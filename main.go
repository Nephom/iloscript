package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type ILOClient struct {
	BaseURL          string
	Model            string
	Token            string
	Session          *http.Client
	MaxRetries       int
	RetryDelay       time.Duration
	Username         string
	Password         string
	SessionURI       string
	Verbose          bool
	InsecureImageTLS bool
}

type OemHpeData struct {
	State                string `json:"State"`
	FlashProgressPercent int    `json:"FlashProgressPercent"`
}

type UpdateServiceResponse struct {
	Oem struct {
		Hpe OemHpeData `json:"Hpe"`
	} `json:"Oem"`
}

// TaskStatusData 結構更新，添加 TaskState
type TaskStatusData struct {
	TaskState       string `json:"TaskState"`
	TaskStatus      string `json:"TaskStatus"`
	PercentComplete int    `json:"PercentComplete"`
	ODataID         string `json:"@odata.id"`
	TaskMonitor     string `json:"TaskMonitor"`
	Messages        []struct {
		Message string `json:"Message"`
	} `json:"Messages"`
}

func canonicalTaskURI(taskURI string) string {
	if !strings.Contains(taskURI, "/TaskMonitors/") {
		return ""
	}
	return strings.Replace(taskURI, "/TaskMonitors/", "/Tasks/", 1)
}

func isTerminalTaskState(state string) bool {
	return isSuccessfulFirmwareState(state) || isFailedFirmwareState(state)
}

func isSuccessfulFirmwareState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "complete", "completed":
		return true
	default:
		return false
	}
}

func isFailedFirmwareState(state string) bool {
	// Include standard Redfish terminal failures and HPE UpdateService states.
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "error", "failed", "rejected", "exception", "killed", "cancelled", "canceled", "aborted":
		return true
	default:
		return false
	}
}

const (
	maxRetries    = 3
	retryInterval = 2 * time.Second
	updateTimeout = 3600
	httpsAttempts = 5
	httpsTimeout  = 30 * time.Second
)

func isValidURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}

func NewILOClient(iloIP string) (*ILOClient, error) {
	if iloIP == "" {
		return nil, fmt.Errorf("iLO IP address cannot be empty")
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}

	return &ILOClient{
		BaseURL: fmt.Sprintf("https://%s/redfish/v1", iloIP),
		Session: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		MaxRetries: maxRetries,
		RetryDelay: retryInterval,
	}, nil
}

// EnsureHTTPSReachable verifies the iLO HTTPS endpoint before attempting login.
// The root endpoint is used intentionally so Redfish authorization cannot cause
// a reachable iLO to be treated as unavailable.
func (c *ILOClient) EnsureHTTPSReachable() bool {
	baseURL, err := url.Parse(c.BaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		fmt.Println("Unable to determine iLO HTTPS endpoint.")
		return false
	}
	rootURL := fmt.Sprintf("%s://%s/", baseURL.Scheme, baseURL.Host)

	reader := bufio.NewReader(os.Stdin)
	for {
		for attempt := 1; attempt <= httpsAttempts; attempt++ {
			req, requestErr := http.NewRequest(http.MethodGet, rootURL, nil)
			if requestErr == nil {
				ctx, cancel := context.WithTimeout(context.Background(), httpsTimeout)
				req = req.WithContext(ctx)
				resp, requestErr := c.Session.Do(req)
				cancel()
				if requestErr == nil {
					resp.Body.Close()
					return true
				}
				fmt.Printf("HTTPS connection attempt %d/%d failed: %v\n", attempt, httpsAttempts, requestErr)
			} else {
				fmt.Printf("HTTPS connection attempt %d/%d failed: %v\n", attempt, httpsAttempts, requestErr)
			}
		}

		fmt.Print("Unable to connect to iLO over HTTPS. Continue trying? (y/n): ")
		answer, readErr := reader.ReadString('\n')
		if readErr != nil || !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
			return false
		}
	}
}

func (c *ILOClient) Login(username, password string) error {
	if username == "" || password == "" {
		return fmt.Errorf("username and password cannot be empty")
	}

	url := c.BaseURL + "/SessionService/Sessions/"
	payload := map[string]string{
		"UserName": username,
		"Password": password,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal login payload: %v", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Session.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 201 {
		c.Token = resp.Header.Get("X-Auth-Token")
		if c.Token == "" {
			return fmt.Errorf("no auth token in response")
		}
		c.Username = username
		c.Password = password
		c.SessionURI = resp.Header.Get("Location")
		return nil
	}

	body, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("login failed with status %d, couldn't read response: %v", resp.StatusCode, err)
	}
	return fmt.Errorf("login failed: %s", formatRedfishError(resp.StatusCode, body))
}

func (c *ILOClient) Logout() {
	defer func() {
		c.Token = ""
		c.SessionURI = ""
	}()

	if c.Token == "" {
		fmt.Println("No token available for logout.")
		return
	}

	if c.SessionURI != "" {
		deleteURL := c.resolveURI(c.SessionURI)
		deleteReq, err := http.NewRequest("DELETE", deleteURL, nil)
		if err != nil {
			fmt.Printf("Failed to create DELETE request: %v\n", err)
			return
		}
		deleteReq.Header.Set("X-Auth-Token", c.Token)
		deleteResp, err := c.Session.Do(deleteReq)
		if err != nil {
			fmt.Printf("Failed to logout session: %v\n", err)
			return
		}
		defer deleteResp.Body.Close()
		if deleteResp.StatusCode == http.StatusOK || deleteResp.StatusCode == http.StatusNoContent {
			fmt.Println("Logout successful!")
		} else {
			body, _ := io.ReadAll(deleteResp.Body)
			fmt.Printf("Failed to logout: %s\n", formatRedfishError(deleteResp.StatusCode, body))
		}
		return
	}

	url := c.BaseURL + "/SessionService/Sessions/"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		fmt.Printf("Failed to create request: %v\n", err)
		return
	}
	req.Header.Set("X-Auth-Token", c.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	c.debugf("GET task URI %s", url)

	resp, err := c.Session.Do(req)
	if err != nil {
		fmt.Printf("Failed to fetch sessions: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("Failed to fetch session list: %s\n", formatRedfishError(resp.StatusCode, body))
		return
	}

	var responseData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&responseData); err != nil {
		fmt.Printf("Failed to decode response: %v\n", err)
		return
	}

	oemData, ok := responseData["Oem"].(map[string]interface{})
	if !ok {
		fmt.Println("Failed to parse Oem data.")
		return
	}

	hpeData, ok := oemData["Hpe"].(map[string]interface{})
	if !ok {
		fmt.Println("Failed to parse Hpe data.")
		return
	}

	links, ok := hpeData["Links"].(map[string]interface{})
	if !ok {
		fmt.Println("Failed to parse Links data.")
		return
	}

	mySession, ok := links["MySession"].(map[string]interface{})
	if !ok {
		fmt.Println("Failed to parse MySession data.")
		return
	}

	sessionURL, ok := mySession["@odata.id"].(string)
	if !ok {
		fmt.Println("Failed to retrieve session URL.")
		return
	}

	if strings.HasPrefix(sessionURL, "/redfish/v1") {
		sessionURL = sessionURL[len("/redfish/v1"):]
	}

	deleteURL := c.BaseURL + sessionURL
	//fmt.Printf("Logging out session: %s\n", deleteURL) //debug information using

	deleteReq, err := http.NewRequest("DELETE", deleteURL, nil)
	if err != nil {
		fmt.Printf("Failed to create DELETE request: %v\n", err)
		return
	}
	deleteReq.Header.Set("X-Auth-Token", c.Token)

	deleteResp, err := c.Session.Do(deleteReq)
	if err != nil {
		fmt.Printf("Failed to logout session: %v\n", err)
		return
	}
	defer deleteResp.Body.Close()

	if deleteResp.StatusCode == 200 {
		fmt.Println("Logout successful!")
	} else {
		body, _ := io.ReadAll(deleteResp.Body)
		fmt.Printf("Failed to logout: %s\n", formatRedfishError(deleteResp.StatusCode, body))
	}
}

func (c *ILOClient) FetchSystemModel() error {
	url := c.BaseURL + "/Systems/1/"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Auth-Token", c.Token)

	resp, err := c.Session.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fetch system model failed: %s", formatRedfishError(resp.StatusCode, body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var data struct {
		Model string `json:"Model"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return err
	}
	if data.Model == "" {
		return fmt.Errorf("model is missing")
	}

	c.Model = data.Model
	fmt.Printf("System Model: %s\n", data.Model)
	return nil
}

func containsIgnoreCase(s, substr string) bool {
	return bytes.Contains(bytes.ToLower([]byte(s)), bytes.ToLower([]byte(substr)))
}

func (c *ILOClient) UpdateFirmware(firmwareURL string) (string, error) {
	info, err := c.UpdateFirmwareWithTarget(context.Background(), firmwareURL, "auto")
	if err != nil {
		return "", err
	}
	return info.TaskURI, nil
}

// FetchOemHpeData 從 UpdateService 獲取 Oem.Hpe 數據
func (c *ILOClient) FetchOemHpeData() (*OemHpeData, error) {
	// 拼接 URL
	url := c.BaseURL + "/UpdateService/"

	// 建立 HTTP 請求
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("創建請求失敗: %v", err)
	}
	// 設置認證 Token（如果需要）
	req.Header.Set("X-Auth-Token", c.Token)

	// 發送請求
	resp, err := c.Session.Do(req)
	if err != nil {
		return nil, fmt.Errorf("請求失敗: %v", err)
	}
	defer resp.Body.Close()

	// 檢查 HTTP 狀態碼
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("請求失敗: %s", formatRedfishError(resp.StatusCode, body))
	}

	// 讀取回應資料
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("讀取回應資料失敗: %v", err)
	}

	// Debug 信息（模擬 Python 中的 `print` 調試）
	//fmt.Printf("Debug - UpdateService Response JSON: %s\n", string(body))
	/*
			示範data
		- @odata.context: string
		- @odata.etag: string
		- @odata.id: string
		- @odata.type: string
		- Id: string
		- Actions
		  - #UpdateService.SimpleUpdate
		    - TransferProtocol@Redfish.AllowableValues: array [string]
		    - target: string
		- Description: string
		- FirmwareInventory
		  - @odata.id: string
		- HttpPushUri: string
		- Name: string
		- Oem
		  - Hpe
		    - @odata.context: string
		    - @odata.type: string
		    - Accept3rdPartyFirmware: boolean
		    - Actions
		      - #HpeiLOUpdateServiceExt.AddFromUri
		        - target: string
		      - #HpeiLOUpdateServiceExt.DeleteInstallSets
		        - target: string
		      - #HpeiLOUpdateServiceExt.DeleteMaintenanceWindows
		        - target: string
		      - #HpeiLOUpdateServiceExt.DeleteUnlockedComponents
		        - target: string
		      - #HpeiLOUpdateServiceExt.DeleteUpdateTaskQueueItems
		        - target: string
		      - #HpeiLOUpdateServiceExt.RemoveLanguagePack
		        - target: string
		      - #HpeiLOUpdateServiceExt.SetDefaultLanguage
		        - target: string
		      - #HpeiLOUpdateServiceExt.StartFirmwareIntegrityCheck
		        - target: string
		    - BundleUpdateReport
		      - @odata.id: string
		    - Capabilities
		      - BundleDowngradeSupport: boolean
		      - OfflineRuntimeBundleUpdate: string
		      - PLDMFirmwareUpdate: boolean
		      - UpdateFWPKG: boolean
		    - ComponentRepository
		      - @odata.id: string
		    - CurrentTime: string (ISO 8601 format)
		    - DowngradePolicy: string
		    - FirmwareIntegrity
		      - EnableBackgroundScan: boolean
		      - LastScanResult: string
		      - LastScanTime: string (ISO 8601 format)
		      - OnIntegrityFailure: string
		      - ScanEveryDays: integer
		    - FlashProgressPercent: integer <== Structure
		    - InstallSets
		      - @odata.id: string
		    - InvalidImageRepository
		      - @odata.id: string
		    - MaintenanceWindows
		      - @odata.id: string
		    - Result
		      - MessageId: string
		    - State: string <== Structure
		    - UpdateTaskQueue
		      - @odata.id: string
		- ServiceEnabled: boolean
		- SoftwareInventory
		  - @odata.id: string
	*/

	// 解析 JSON
	var data UpdateServiceResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("JSON 解析失敗: %v", err)
	}

	// 提取 Oem.Hpe 數據
	return &data.Oem.Hpe, nil
}

func (c *ILOClient) GetTaskStatus(taskURI string) (string, int, error) {
	// 拼接 /TaskService/ URL
	url := c.resolveURI(taskURI)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", 0, fmt.Errorf("failed to create TaskService request: %v", err)
	}
	req.Header.Set("X-Auth-Token", c.Token)

	// 執行對 /TaskService/ 的請求
	resp, err := c.Session.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("failed to fetch TaskService data: %v", err)
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.debugf("GET task URI %s -> %s", url, formatRedfishError(resp.StatusCode, body))
	} else {
		c.debugf("GET task URI %s -> HTTP %d", url, resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		if strings.Contains(string(body), "iLO.2.44.UpdateBadParameter") {
			if taskURL := canonicalTaskURI(url); taskURL != "" && taskURL != url {
				c.debugf("TaskMonitor rejected with UpdateBadParameter; switching to canonical task URI %s", taskURL)
				return c.GetTaskStatus(taskURL)
			}
		}
		if oemData, oemErr := c.FetchOemHpeData(); oemErr == nil && (oemData.State != "" || oemData.FlashProgressPercent > 0) {
			state := oemData.State
			if isSuccessfulFirmwareState(state) {
				return "Complete", 100, nil
			}
			if isFailedFirmwareState(state) {
				return "Error", oemData.FlashProgressPercent, nil
			}
			return state, oemData.FlashProgressPercent, nil
		}
		return "Error", 0, fmt.Errorf("unexpected TaskService response: %s", formatRedfishError(resp.StatusCode, body))
	}

	// 解析 /TaskService/ 返回數據
	var taskData TaskStatusData
	/* 示範
	- @odata.context: string
	- @odata.etag: string
	- @odata.id: string
	- @odata.type: string
	- Id: string
	- Actions
	  - #UpdateService.SimpleUpdate
	    - TransferProtocol@Redfish.AllowableValues: array [string]
	    - target: string
	- Description: string
	- FirmwareInventory
	  - @odata.id: string
	- HttpPushUri: string
	- Name: string
	- Oem
	  - Hpe
	    - @odata.context: string
	    - @odata.type: string
	    - Accept3rdPartyFirmware: boolean
	    - Actions
	      - #HpeiLOUpdateServiceExt.AddFromUri
	        - target: string
	      - #HpeiLOUpdateServiceExt.DeleteInstallSets
	        - target: string
	      - #HpeiLOUpdateServiceExt.DeleteMaintenanceWindows
	        - target: string
	      - #HpeiLOUpdateServiceExt.DeleteUnlockedComponents
	        - target: string
	      - #HpeiLOUpdateServiceExt.DeleteUpdateTaskQueueItems
	        - target: string
	      - #HpeiLOUpdateServiceExt.RemoveLanguagePack
	        - target: string
	      - #HpeiLOUpdateServiceExt.SetDefaultLanguage
	        - target: string
	      - #HpeiLOUpdateServiceExt.StartFirmwareIntegrityCheck
	        - target: string
	    - BundleUpdateReport
	      - @odata.id: string
	    - Capabilities
	      - BundleDowngradeSupport: boolean
	      - OfflineRuntimeBundleUpdate: string
	      - PLDMFirmwareUpdate: boolean
	      - UpdateFWPKG: boolean
	    - ComponentRepository
	      - @odata.id: string
	    - CurrentTime: string (ISO 8601 format)
	    - DowngradePolicy: string
	    - FirmwareIntegrity
	      - EnableBackgroundScan: boolean
	      - LastScanResult: string
	      - LastScanTime: string (ISO 8601 format)
	      - OnIntegrityFailure: string
	      - ScanEveryDays: integer
	    - InstallSets
	      - @odata.id: string
	    - InvalidImageRepository
	      - @odata.id: string
	    - MaintenanceWindows
	      - @odata.id: string
	    - State: string <== Structure
	    - UpdateTaskQueue
	      - @odata.id: string
	- ServiceEnabled: boolean
	- SoftwareInventory
	  - @odata.id: string
	*/
	if err := json.Unmarshal(body, &taskData); err != nil {
		return "Unknown", 0, fmt.Errorf("failed to parse TaskService JSON: %v", err)
	}

	// 獲取進度數據（/UpdateService/）
	oemData, oemErr := c.FetchOemHpeData()
	//fmt.Printf("Debug - OEM HPE Data JSON: %+v\n", oemData)

	// 判斷 TaskState 和進度
	updateState := taskData.TaskState
	if !isTerminalTaskState(updateState) && oemErr == nil && oemData.State != "" {
		updateState = oemData.State
	}
	if updateState == "" {
		updateState = taskData.TaskState
	}
	if updateState == "" {
		updateState = "Unknown"
	}

	progress := 0
	if oemErr == nil {
		progress = oemData.FlashProgressPercent
	}
	if taskData.PercentComplete > progress {
		progress = taskData.PercentComplete
	}
	if isSuccessfulFirmwareState(updateState) {
		return "Complete", 100, nil
	}
	if isFailedFirmwareState(updateState) {
		return "Error", progress, nil
	}
	if updateState == "" || strings.EqualFold(updateState, "Unknown") {
		if taskData.TaskStatus != "" {
			return taskData.TaskStatus, progress, nil
		}
		return "InProgress", progress, nil
	}
	return updateState, progress, nil
}

// imlEvent represents one IML (Integrated Management Log) entry.
// ID is the entry's actual number on iLO (used to build the /Entries/{ID}
// query URL, and also serves as the unique key for detecting new events).
// It is deliberately excluded from JSON decoding (json:"-") because the
// Redfish response's "Id" field is a string (e.g. "5"), which would fail
// to unmarshal into this int field; ID is instead set manually after decoding.
// Created is the standard Redfish LogEntry timestamp field; some iLO
// firmware versions' IML service may not return it, in which case it will
// be an empty string and the caller must fall back accordingly.
type imlEvent struct {
	ID       int    `json:"-"`
	Message  string `json:"Message"`
	Severity string `json:"Severity"`
	Created  string `json:"Created"`
}

// imlGroup 是給人看的顯示單位：把「連續且 Severity+Message 完全相同」的原始 imlEvent
// 合併成一組，並附上出現次數，模仿 iLO IML 原生介面對重複訊息的呈現方式。
type imlGroup struct {
	Severity  string
	Message   string
	Count     int  // 這組合併了多少筆連續且相同的原始記錄
	Uncertain bool // true 表示這組位於抓取視窗的最舊邊界，且視窗是因抓滿 count 筆而被截斷，
	// 因此真實次數可能比 Count 更多（視窗外還有相同訊息但沒抓到）
	Latest imlEvent // 這組當中最新的一筆原始記錄（用於取得時間等資訊）
}

// groupIMLEvents 將依「新到舊」排序的 members 合併成顯示用的分組列表。
// truncatedAtBoundary 表示 members 中最舊的那一筆，是因為已經抓滿 count 筆而停止抓取
// （而非真的抓到了 log 的最開頭），此時最後一組會標記為 Uncertain。
func groupIMLEvents(members []imlEvent, truncatedAtBoundary bool) []imlGroup {
	var groups []imlGroup
	for _, entry := range members {
		if n := len(groups); n > 0 &&
			groups[n-1].Severity == entry.Severity &&
			groups[n-1].Message == entry.Message {
			groups[n-1].Count++
			continue
		}
		groups = append(groups, imlGroup{
			Severity: entry.Severity,
			Message:  entry.Message,
			Count:    1,
			Latest:   entry,
		})
	}
	if truncatedAtBoundary && len(groups) > 0 {
		groups[len(groups)-1].Uncertain = true
	}
	return groups
}

// imlSeverityColor 依嚴重度回傳對應的 ANSI 顏色碼，供 IML 相關的各種輸出共用。
func imlSeverityColor(severity string) string {
	switch strings.ToLower(severity) {
	case "ok":
		return "\033[32m" // Green
	case "warning":
		return "\033[33m" // Yellow
	case "critical":
		return "\033[31m" // Red
	default:
		return "\033[0m" // Default (No color)
	}
}

// formatIMLGroupLines 把分組後的 IML 事件轉成一行一個字串（含嚴重度上色），
// 供直接列印使用，也供 MonitorIML 快取起來重複重繪畫面時使用。
func formatIMLGroupLines(groups []imlGroup) []string {
	resetColor := "\033[0m"

	var lines []string
	for _, group := range groups {
		colorCode := imlSeverityColor(group.Severity)
		eventTime := formatIMLEventTime(group.Latest)

		switch {
		case group.Uncertain:
			lines = append(lines, fmt.Sprintf("%s- %s %s: %s (Count: %d+...)%s", colorCode, eventTime, group.Severity, group.Message, group.Count, resetColor))
		case group.Count > 1:
			lines = append(lines, fmt.Sprintf("%s- %s %s: %s (Count: %d)%s", colorCode, eventTime, group.Severity, group.Message, group.Count, resetColor))
		default:
			lines = append(lines, fmt.Sprintf("%s- %s %s: %s%s", colorCode, eventTime, group.Severity, group.Message, resetColor))
		}
	}

	if len(lines) == 0 {
		lines = append(lines, "- 未找到相關的事件 -")
	}

	return lines
}

// printIMLGroups 以現有的嚴重度上色規則印出分組後的 IML 事件清單。
func printIMLGroups(header string, groups []imlGroup) {
	fmt.Println(header)
	for _, line := range formatIMLGroupLines(groups) {
		fmt.Println(line)
	}
}

// fetchIMLEntries 負責向 iLO 抓取、過濾並排序 IML 事件（新到舊），
// 不做任何印出動作，供一次性查詢與監控模式共用。
// truncatedAtBoundary 表示回傳結果中最舊的那一筆，是否是因為抓滿 count 筆而被截斷
// （而不是因為已經抓到 log 最開頭），供上層判斷合併顯示時是否需要標記「次數可能不完整」。
func (c *ILOClient) fetchIMLEntries(count int, matchText string, severityFilter string) ([]imlEvent, int, bool, error) {
	// 先取得總事件數
	countURL := fmt.Sprintf("%s/Systems/1/LogServices/IML/Entries", c.BaseURL)
	req, err := http.NewRequest("GET", countURL, nil)
	if err != nil {
		return nil, 0, false, fmt.Errorf("failed to create count request: %v", err)
	}
	req.Header.Set("X-Auth-Token", c.Token)
	resp, err := c.Session.Do(req)
	if err != nil {
		return nil, 0, false, fmt.Errorf("failed to fetch IML events count: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, 0, false, fmt.Errorf("fetching IML events count failed: %s", formatRedfishError(resp.StatusCode, body))
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, false, fmt.Errorf("failed to read count response body: %v", err)
	}

	var countData map[string]interface{}
	if err := json.Unmarshal(body, &countData); err != nil {
		return nil, 0, false, fmt.Errorf("failed to parse count JSON: %v", err)
	}

	// 取得總事件數
	totalCount, ok := countData["Members@odata.count"].(float64)
	if !ok {
		return nil, 0, false, fmt.Errorf("failed to retrieve total event count")
	}

	// 是否指定了過濾條件（matchText 或 severityFilter）
	// matchText 大小寫不拘：空字串或 "none"（不分大小寫）視為未指定
	matchTextLower := strings.ToLower(matchText)
	hasMatchText := matchText != "" && matchTextLower != "none"
	severityFilterLower := strings.ToLower(severityFilter)
	hasSeverityFilter := severityFilter != ""
	hasFilter := hasMatchText || hasSeverityFilter

	matches := func(entry imlEvent) bool {
		if hasSeverityFilter && strings.ToLower(entry.Severity) != severityFilterLower {
			return false
		}
		if hasMatchText && !strings.Contains(strings.ToLower(entry.Message), matchTextLower) {
			return false
		}
		return true
	}

	fetchEvent := func(id int) (imlEvent, bool, error) {
		var eventData imlEvent
		eventURL := fmt.Sprintf("%s/Systems/1/LogServices/IML/Entries/%d", c.BaseURL, id)
		req, err := http.NewRequest("GET", eventURL, nil)
		if err != nil {
			return eventData, false, fmt.Errorf("failed to create event request: %v", err)
		}
		req.Header.Set("X-Auth-Token", c.Token)

		resp, err := c.Session.Do(req)
		if err != nil {
			return eventData, false, fmt.Errorf("failed to fetch IML event %d: %v", id, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return eventData, false, nil // 跳過無法取得的事件
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return eventData, false, fmt.Errorf("failed to read event response body: %v", err)
		}

		if err := json.Unmarshal(body, &eventData); err != nil {
			return eventData, false, fmt.Errorf("failed to parse event JSON: %v", err)
		}
		eventData.ID = id

		return eventData, true, nil
	}

	// 最終要顯示的事件（依「最新事件在前」排序）
	var members []imlEvent
	truncatedAtBoundary := false

	if hasFilter {
		// 有指定 matchText 或 severity 時，不能只在「最新 count 筆」原始事件裡找，
		// 否則符合條件的事件可能因為被更新的事件擠出視窗而找不到。
		// 因此改成從最新事件往舊的方向掃描整個記錄，直到收集到 count 筆符合條件的事件為止。
		for i := int(totalCount); i >= 1; i-- {
			eventData, ok, err := fetchEvent(i)
			if err != nil {
				return nil, 0, false, err
			}
			if !ok {
				continue
			}
			if matches(eventData) {
				members = append(members, eventData)
				if count > 0 && len(members) >= count {
					// 尚未掃描到 id=1 就已收集滿 count 筆，代表視窗外可能還有符合條件但未抓到的舊事件。
					truncatedAtBoundary = i > 1
					break
				}
			}
		}
	} else {
		// 未指定過濾條件時，維持原行為：只抓最新 count 筆事件
		startID := int(totalCount) - count + 1
		truncatedAtBoundary = startID > 1
		if startID < 1 {
			startID = 1
		}

		var rawMembers []imlEvent
		for i := startID; i <= int(totalCount); i++ {
			eventData, ok, err := fetchEvent(i)
			if err != nil {
				return nil, 0, false, err
			}
			if !ok {
				continue
			}
			rawMembers = append(rawMembers, eventData)
		}

		// 反轉 slice 以符合最新事件在前
		for i, j := 0, len(rawMembers)-1; i < j; i, j = i+1, j-1 {
			rawMembers[i], rawMembers[j] = rawMembers[j], rawMembers[i]
		}
		members = rawMembers
	}

	return members, int(totalCount), truncatedAtBoundary, nil
}

// func (c *ILOClient) FetchIMLEvents(count int, matchText string) error {
func (c *ILOClient) FetchIMLEvents(count int, matchText string, severityFilter string) error {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("FetchIMLEvents recovered from panic: %v\n", r)
		}
	}()

	members, _, truncatedAtBoundary, err := c.fetchIMLEntries(count, matchText, severityFilter)
	if err != nil {
		return err
	}

	groups := groupIMLEvents(members, truncatedAtBoundary)
	printIMLGroups("\n最近的 IML 事件:", groups)

	return nil
}

func (c *ILOClient) MonitorUpdate(taskURI, matchText, targetKind string, timeout int) error {
	start := time.Now()

	fmt.Println("\n開始監控更新進度...")
	consecutiveErrors := 0
	maxConsecutiveErrors := 3
	filter := "Firmware flashed" // 預設過濾條件
	if matchText != "" && matchText != "None" {
		filter = matchText // 如果有指定 matchText，則使用該值作為過濾條件
	}

	for time.Since(start).Seconds() < float64(timeout) {
		status, progress, err := c.GetTaskStatus(taskURI)
		if err != nil {
			consecutiveErrors++
			fmt.Printf("\nError fetching task status: %v\n", err)

			if consecutiveErrors >= maxConsecutiveErrors {
				fmt.Println("\n連續多次獲取狀態失敗，嘗試獲取 IML 事件...")
				if !strings.EqualFold(targetKind, "ilo") {
					if fetchErr := c.FetchIMLEvents(10, matchText, ""); fetchErr != nil {
						fmt.Printf("獲取 IML 事件失敗: %v\n", fetchErr)
					}
				}
				return fmt.Errorf("monitoring failed: %v", err)
			}

			time.Sleep(5 * time.Second)
			continue
		}
		consecutiveErrors = 0

		fmt.Printf("\r進度: %d%%, 更新狀態: %s        ", progress, status)

		switch {
		case isSuccessfulFirmwareState(status):
			fmt.Println("\n更新完成!")
			if !strings.EqualFold(targetKind, "ilo") {
				if fetchErr := c.FetchIMLEvents(10, filter, ""); fetchErr != nil {
					fmt.Printf("獲取 IML 事件失敗: %v\n", fetchErr)
				}
			}
			return nil
		case isFailedFirmwareState(status):
			fmt.Println("\n更新失敗!")
			if !strings.EqualFold(targetKind, "ilo") {
				if fetchErr := c.FetchIMLEvents(10, filter, ""); fetchErr != nil {
					fmt.Printf("獲取 IML 事件失敗: %v\n", fetchErr)
				}
			}
			return fmt.Errorf("firmware update failed")
		}

		time.Sleep(5 * time.Second)
	}

	fmt.Println("\n更新超時!")
	if !strings.EqualFold(targetKind, "ilo") {
		if fetchErr := c.FetchIMLEvents(10, matchText, ""); fetchErr != nil {
			fmt.Printf("獲取 IML 事件失敗: %v\n", fetchErr)
		}
	}
	return fmt.Errorf("firmware update timeout")
}

func (c *ILOClient) PowerControl(action string) error {
	url := fmt.Sprintf("%s/Systems/1/Actions/Oem/Hpe/HpeComputerSystemExt.PowerButton/", c.BaseURL)

	var payload map[string]string
	switch action {
	case "on":
		payload = map[string]string{"PushType": "Press"}
	case "off":
		payload = map[string]string{"PushType": "PressAndHold"}
	default:
		return fmt.Errorf("invalid power action. Use 'on' or 'off'")
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %v", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create power control request: %v", err)
	}
	req.Header.Set("X-Auth-Token", c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Session.Do(req)
	if err != nil {
		return fmt.Errorf("power control request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("power control failed: %s", formatRedfishError(resp.StatusCode, body))
	}

	fmt.Printf("Power %s command sent successfully\n", action)
	return nil
}

func (c *ILOClient) FetchFirmwareInventory() error {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("FetchFirmwareInventory recovered from panic: %v\n", r)
		}
	}()

	// First, fetch the total count of firmware inventory items
	countURL := fmt.Sprintf("%s/UpdateService/FirmwareInventory", c.BaseURL)
	req, err := http.NewRequest("GET", countURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create count request: %v", err)
	}
	req.Header.Set("X-Auth-Token", c.Token)

	resp, err := c.Session.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch firmware inventory count: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("fetching firmware inventory count failed: %s", formatRedfishError(resp.StatusCode, body))
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read count response body: %v", err)
	}

	var countData map[string]interface{}
	if err := json.Unmarshal(body, &countData); err != nil {
		return fmt.Errorf("failed to parse count JSON: %v", err)
	}

	// Get total count of firmware inventory items
	totalCount, ok := countData["Members@odata.count"].(float64)
	if !ok {
		return fmt.Errorf("failed to retrieve total firmware inventory count")
	}

	// 先蒐集所有 firmware 資料並找出最長 Name 長度
	var inventoryItems []struct {
		Name    string `json:"Name"`
		Version string `json:"Version"`
	}
	maxNameLength := len("Name") // 初始值從 "Name" 欄位開始

	for i := 1; i <= int(totalCount); i++ {
		itemURL := fmt.Sprintf("%s/UpdateService/FirmwareInventory/%d", c.BaseURL, i)
		req, err := http.NewRequest("GET", itemURL, nil)
		if err != nil {
			fmt.Printf("Failed to create request for firmware inventory item %d: %v\n", i, err)
			continue
		}
		req.Header.Set("X-Auth-Token", c.Token)

		resp, err := c.Session.Do(req)
		if err != nil {
			fmt.Printf("Failed to fetch firmware inventory item %d: %v\n", i, err)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			body, _ := ioutil.ReadAll(resp.Body)
			fmt.Printf("Failed to fetch firmware inventory item %d: %s\n", i, formatRedfishError(resp.StatusCode, body))
			continue
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("Failed to read response body for firmware inventory item %d: %v\n", i, err)
			continue
		}

		var inventoryItem struct {
			Name    string `json:"Name"`
			Version string `json:"Version"`
		}

		if err := json.Unmarshal(body, &inventoryItem); err != nil {
			fmt.Printf("Failed to parse JSON for firmware inventory item %d: %v\n", i, err)
			continue
		}

		// 更新最長 Name 長度
		if len(inventoryItem.Name) > maxNameLength {
			maxNameLength = len(inventoryItem.Name)
		}

		inventoryItems = append(inventoryItems, inventoryItem)
	}

	// 製作表格格線
	tableLine := fmt.Sprintf("+%s+%s+",
		strings.Repeat("-", maxNameLength+2),
		strings.Repeat("-", 30))

	// 印出表頭
	fmt.Println(tableLine)
	fmt.Printf("| %-*s | %-28s |\n", maxNameLength, "Name", "Version")
	fmt.Println(tableLine)

	// 印出每一列
	for _, item := range inventoryItems {
		fmt.Printf("| %-*s | %-28s |\n", maxNameLength, item.Name, item.Version)
	}
	fmt.Println(tableLine)

	return nil
}

func (c *ILOClient) FetchChaissDevices() error {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("FetchFirmwareInventory recovered from panic: %v\n", r)
		}
	}()

	// First, fetch the total count of firmware inventory items
	countURL := fmt.Sprintf("%s/Chassis/1/Devices/", c.BaseURL)
	req, err := http.NewRequest("GET", countURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create count request: %v", err)
	}
	req.Header.Set("X-Auth-Token", c.Token)

	resp, err := c.Session.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch firmware inventory count: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("fetching firmware inventory count failed: %s", formatRedfishError(resp.StatusCode, body))
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read count response body: %v", err)
	}

	var countData map[string]interface{}
	if err := json.Unmarshal(body, &countData); err != nil {
		return fmt.Errorf("failed to parse count JSON: %v", err)
	}

	totalCount, ok := countData["Members@odata.count"].(float64)
	if !ok {
		return fmt.Errorf("failed to retrieve total firmware inventory count")
	}

	var inventoryItems []struct {
		Locat string `json:"Location"`
		Name  string `json:"Name"`
	}
	maxNameLength := len("Location")

	for i := 1; i <= int(totalCount); i++ {
		itemURL := fmt.Sprintf("%s/Chassis/1/Devices/%d", c.BaseURL, i)
		req, err := http.NewRequest("GET", itemURL, nil)
		if err != nil {
			fmt.Printf("Failed to create request for firmware inventory item %d: %v\n", i, err)
			continue
		}
		req.Header.Set("X-Auth-Token", c.Token)

		resp, err := c.Session.Do(req)
		if err != nil {
			fmt.Printf("Failed to fetch firmware inventory item %d: %v\n", i, err)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			body, _ := ioutil.ReadAll(resp.Body)
			fmt.Printf("Failed to fetch firmware inventory item %d: %s\n", i, formatRedfishError(resp.StatusCode, body))
			continue
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("Failed to read response body for firmware inventory item %d: %v\n", i, err)
			continue
		}

		var inventoryItem struct {
			Locat string `json:"Location"`
			Name  string `json:"Name"`
		}

		if err := json.Unmarshal(body, &inventoryItem); err != nil {
			fmt.Printf("Failed to parse JSON for firmware inventory item %d: %v\n", i, err)
			continue
		}

		// 更新最長 Name 長度
		if len(inventoryItem.Locat) > maxNameLength {
			maxNameLength = len(inventoryItem.Locat)
		}

		inventoryItems = append(inventoryItems, inventoryItem)
	}

	// 製作表格格線
	tableLine := fmt.Sprintf("+%s+%s+",
		strings.Repeat("-", maxNameLength+2),
		strings.Repeat("-", 30))

	// 印出表頭
	fmt.Println(tableLine)
	fmt.Printf("| %-*s | %-28s |\n", maxNameLength, "Location", "Name")
	fmt.Println(tableLine)

	// 印出每一列
	for _, item := range inventoryItems {
		fmt.Printf("| %-*s | %-28s |\n", maxNameLength, item.Locat, item.Name)
	}
	fmt.Println(tableLine)

	return nil
}

func (c *ILOClient) FetchEventLogs() error {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("FetchEventLogs recovered from panic: %v\n", r)
		}
	}()

	countURL := fmt.Sprintf("%s/Systems/1/LogServices/Event/Entries", c.BaseURL)
	req, err := http.NewRequest("GET", countURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create count request: %v", err)
	}
	req.Header.Set("X-Auth-Token", c.Token)
	resp, err := c.Session.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch event logs count: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("fetching event logs count failed: %s", formatRedfishError(resp.StatusCode, body))
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read count response body: %v", err)
	}

	var countData map[string]interface{}
	if err := json.Unmarshal(body, &countData); err != nil {
		return fmt.Errorf("failed to parse count JSON: %v", err)
	}

	totalCount, ok := countData["Members@odata.count"].(float64)
	if !ok {
		return fmt.Errorf("failed to retrieve total event count")
	}

	var finalData struct {
		Members []struct {
			MessageId      string `json:"MessageId"`
			Severity       string `json:"Severity"`
			EventTimestamp string `json:"EventTimestamp"`
		} `json:"Members"`
	}

	var members []struct {
		MessageId      string `json:"MessageId"`
		Severity       string `json:"Severity"`
		EventTimestamp string `json:"EventTimestamp"`
	}

	for i := 1; i <= int(totalCount); i++ {
		eventURL := fmt.Sprintf("%s/Systems/1/LogServices/Event/Entries/%d", c.BaseURL, i)
		req, err := http.NewRequest("GET", eventURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create event request: %v", err)
		}
		req.Header.Set("X-Auth-Token", c.Token)

		resp, err := c.Session.Do(req)
		if err != nil {
			return fmt.Errorf("failed to fetch event log %d: %v", i, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			continue
		}

		var eventData struct {
			MessageId      string `json:"MessageId"`
			Severity       string `json:"Severity"`
			EventTimestamp string `json:"EventTimestamp"`
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read event response body: %v", err)
		}

		if err := json.Unmarshal(body, &eventData); err != nil {
			return fmt.Errorf("failed to parse event JSON: %v", err)
		}

		members = append(members, eventData)
	}

	for i, j := 0, len(members)-1; i < j; i, j = i+1, j-1 {
		members[i], members[j] = members[j], members[i]
	}

	finalData.Members = members

	fmt.Println("\n最近的 Event 日誌:")

	maxNameLength := 10 // 預設最小長度
	for _, entry := range finalData.Members {
		if len(entry.MessageId) > maxNameLength {
			maxNameLength = len(entry.MessageId)
		}
	}

	formatHeader := fmt.Sprintf("%%-%ds | %%-%ds | %%s\n", maxNameLength, 13)
	formatRow := fmt.Sprintf("%%-%ds | %%-%ds | %%s\n", maxNameLength, 13)
	border := strings.Repeat("=", maxNameLength+34)

	fmt.Println(border)
	fmt.Printf(formatHeader, "Name", "Severity", "TimeStamp")
	fmt.Println(border)

	serverResetCount := 0
	for _, entry := range finalData.Members {
		messageLower := strings.ToLower(entry.MessageId)
		if !strings.Contains(messageLower, "server") {
			continue
		}

		if strings.Contains(messageLower, "serverreset") {
			serverResetCount++
		}

		colorCode := ""
		resetColor := "\033[0m"
		if strings.ToLower(entry.Severity) == "warning" {
			colorCode = "\033[33m" // Yellow
		}

		// 轉換時區
		timestamp, err := time.Parse(time.RFC3339, entry.EventTimestamp)
		if err != nil {
			return fmt.Errorf("failed to parse timestamp: %v", err)
		}
		loc, _ := time.LoadLocation("Asia/Taipei")
		localTime := timestamp.In(loc).Format("2006-01-02T15:04:05") + " +0800"

		fmt.Printf(colorCode+formatRow+resetColor, entry.MessageId, entry.Severity, localTime)
	}

	fmt.Println(border)
	fmt.Printf("ServerReset total count: %d\n", serverResetCount)

	return nil
}

// New method to get BIOS settings
func (c *ILOClient) GetBIOSSettings() (map[string]interface{}, error) {
	url := fmt.Sprintf("%s/Systems/1/bios/settings", c.BaseURL)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create BIOS settings request: %v", err)
	}

	req.Header.Set("X-Auth-Token", c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Session.Do(req)
	if err != nil {
		return nil, fmt.Errorf("BIOS settings request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("get BIOS settings failed: %s", formatRedfishError(resp.StatusCode, body))
	}

	var biosSettings map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&biosSettings); err != nil {
		return nil, fmt.Errorf("failed to parse BIOS settings: %v", err)
	}

	attributes, ok := biosSettings["Attributes"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("attributes not found or not a valid map")
	}

	// 创建一个切片来存储所有属性的键
	sortedKeys := make([]string, 0, len(attributes))
	for k := range attributes {
		sortedKeys = append(sortedKeys, k)
	}

	// 对所有键进行排序
	sort.Strings(sortedKeys)

	// 打印已排序的属性
	fmt.Println("BIOS Attributes:")
	for _, attr := range sortedKeys {
		fmt.Printf("%s: %v\n", attr, attributes[attr])
	}

	return biosSettings, nil
}

// New method to patch a specific BIOS setting
func (c *ILOClient) PatchBIOSSetting(attrName, attrValue string) error {
	url := fmt.Sprintf("%s/Systems/1/bios/settings", c.BaseURL)

	// Try to convert attrValue to an integer
	var value interface{}
	if intValue, err := strconv.Atoi(attrValue); err == nil {
		value = intValue
	} else {
		value = attrValue
	}

	// Prepare payload with the attribute to modify
	payload := map[string]interface{}{
		"Attributes": map[string]interface{}{
			attrName: value,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %v", err)
	}

	req, err := http.NewRequest("PATCH", url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create BIOS settings patch request: %v", err)
	}

	req.Header.Set("X-Auth-Token", c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Session.Do(req)
	if err != nil {
		return fmt.Errorf("BIOS settings patch request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("patch BIOS settings failed: %s", formatRedfishError(resp.StatusCode, respBody))
	}

	fmt.Printf("BIOS setting %s updated to %s successfully\n", attrName, attrValue)
	return nil
}

func (c *ILOClient) PostBIOSSetting() error {
	// Construct the URL for the BIOS settings
	url := fmt.Sprintf("%s/Systems/1/", c.BaseURL)

	// Prepare payload with fixed attribute values
	payload := map[string]interface{}{
		"Boot": map[string]interface{}{
			"BootSourceOverrideTarget": "BiosSetup",
		},
	}

	// Marshal the payload into JSON format
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %v", err)
	}

	// Create a new POST request
	req, err := http.NewRequest("PATCH", url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create BIOS settings post request: %v", err)
	}

	// Set headers for authentication and content type
	req.Header.Set("X-Auth-Token", c.Token)
	req.Header.Set("Content-Type", "application/json")

	// Execute the request using the session
	resp, err := c.Session.Do(req)
	if err != nil {
		return fmt.Errorf("BIOS settings post request failed: %v", err)
	}
	defer resp.Body.Close()

	// Check for successful response status
	if resp.StatusCode != http.StatusOK {
		respBody, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("post BIOS settings failed: %s", formatRedfishError(resp.StatusCode, respBody))
	}

	fmt.Println("BIOS setting BootSourceOverrideTarget updated to BiosSetup successfully")
	return nil
}

func (c *ILOClient) ResetBIOS() error {
	fmt.Println("Please notice, this method requires that the SUT must be in poweroff status.")

	// Step 1: Reset BIOS to system defaults
	resetURL := fmt.Sprintf("%s/Systems/1/Actions/Oem/Hpe/HpeComputerSystemExt.RestoreSystemDefaults/", c.BaseURL)

	// Create a new POST request for BIOS reset
	resetReq, err := http.NewRequest("POST", resetURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create BIOS reset request: %v", err)
	}

	// Set headers for authentication and content type
	resetReq.Header.Set("X-Auth-Token", c.Token)
	resetReq.Header.Set("Content-Type", "application/json")

	// Execute the request using the session
	resetResp, err := c.Session.Do(resetReq)
	if err != nil {
		return fmt.Errorf("BIOS reset request failed: %v", err)
	}
	defer resetResp.Body.Close()

	// Check for successful response status
	if resetResp.StatusCode != http.StatusOK && resetResp.StatusCode != http.StatusAccepted {
		respBody, _ := ioutil.ReadAll(resetResp.Body)
		return fmt.Errorf("BIOS reset failed: %s", formatRedfishError(resetResp.StatusCode, respBody))
	}

	fmt.Println("Done, check with your IRC to see SUT power-on")

	// Step 2: Power button action
	powerURL := fmt.Sprintf("%s/Systems/1/Actions/Oem/Hpe/HpeComputerSystemExt.PowerButton/", c.BaseURL)

	// Prepare payload for power button press
	powerPayload := map[string]string{
		"PushType": "Press",
	}

	// Marshal the payload into JSON format
	powerBody, err := json.Marshal(powerPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal power button payload: %v", err)
	}

	time.Sleep(5 * time.Second)

	// Create a new POST request for power button
	powerReq, err := http.NewRequest("POST", powerURL, bytes.NewBuffer(powerBody))
	if err != nil {
		return fmt.Errorf("failed to create power button request: %v", err)
	}

	// Set headers for authentication and content type
	powerReq.Header.Set("X-Auth-Token", c.Token)
	powerReq.Header.Set("Content-Type", "application/json")

	// Execute the request using the session
	powerResp, err := c.Session.Do(powerReq)
	if err != nil {
		return fmt.Errorf("power button request failed: %v", err)
	}
	defer powerResp.Body.Close()

	// Check for successful response status
	if powerResp.StatusCode != http.StatusOK && powerResp.StatusCode != http.StatusAccepted {
		respBody, _ := ioutil.ReadAll(powerResp.Body)
		return fmt.Errorf("power button action failed: %s", formatRedfishError(powerResp.StatusCode, respBody))
	}

	fmt.Println("Power button pressed successfully")
	return nil
}

func (c *ILOClient) ClearLogsAndReset() error {
	// Ensure BaseURL starts with https://
	if !strings.HasPrefix(c.BaseURL, "https://") {
		return fmt.Errorf("invalid BaseURL: must start with https://")
	}

	// Define the endpoints for the requests
	ielURL := fmt.Sprintf("%s/Managers/1/LogServices/IEL/Actions/LogService.ClearLog/", c.BaseURL)
	imlURL := fmt.Sprintf("%s/Systems/1/LogServices/IML/Actions/LogService.ClearLog/", c.BaseURL)
	ahsURL := fmt.Sprintf("%s/Managers/1/ActiveHealthSystem/Actions/HpeiLOActiveHealthSystem.ClearLog", c.BaseURL)
	resetURL := fmt.Sprintf("%s/Managers/1/Actions/Manager.Reset", c.BaseURL)

	// Keep request URLs separate from the labels shown to the user.
	requests := []struct {
		label string
		url   string
	}{
		{label: "Clear IEL...", url: ielURL},
		{label: "Clear IML...", url: imlURL},
		{label: "Clear AHS...", url: ahsURL},
	}

	// Set headers for authentication
	headers := map[string]string{
		"X-Auth-Token": c.Token,
		"Content-Type": "application/json",
	}

	// Function to perform POST requests
	postRequest := func(label, url string, payload interface{}) error {
		var requestBody io.Reader
		if payload != nil {
			body, err := json.Marshal(payload)
			if err != nil {
				return fmt.Errorf("failed to marshal request payload for %s: %v", label, err)
			}
			requestBody = bytes.NewReader(body)
		}

		req, err := http.NewRequest("POST", url, requestBody)
		if err != nil {
			return fmt.Errorf("failed to create request for %s: %v", label, err)
		}

		// Set headers
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := c.Session.Do(req)
		if err != nil {
			return fmt.Errorf("%s failed: %v", label, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
			body, _ := ioutil.ReadAll(resp.Body)
			return fmt.Errorf("%s failed: %s", label, formatRedfishError(resp.StatusCode, body))
		}

		fmt.Printf("%s completed.\n", label)
		return nil
	}

	// Clear logs by posting to each URL
	for _, request := range requests {
		if err := postRequest(request.label, request.url, nil); err != nil {
			return err
		}
	}

	// Wait for 60 seconds before iLO Reset
	fmt.Println("Wait for 120 seconds to clear AHS data...")
	time.Sleep(120 * time.Second)

	// Perform iLO Reset
	if err := postRequest("iLO Reset", resetURL, map[string]string{"ResetType": "ForceRestart"}); err != nil {
		return err
	}

	// Wait for 60 seconds before iLO Reset
	fmt.Println("Wait for 120 seconds to launch iLO...")

	fmt.Println("Logs cleared and iLO reset request accepted successfully.")
	return nil
}

func (c *ILOClient) ClearLog(logService string) error {
	var logURL string
	switch strings.ToUpper(logService) {
	case "IML":
		logURL = fmt.Sprintf("%s/Systems/1/LogServices/IML/Actions/LogService.ClearLog/", c.BaseURL)
	case "IEL":
		logURL = fmt.Sprintf("%s/Managers/1/LogServices/IEL/Actions/LogService.ClearLog/", c.BaseURL)
	default:
		return fmt.Errorf("unsupported log service: %s", logService)
	}

	req, err := http.NewRequest("POST", logURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create %s clear request: %v", logService, err)
	}
	req.Header.Set("X-Auth-Token", c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Session.Do(req)
	if err != nil {
		return fmt.Errorf("%s clear request failed: %v", logService, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("%s clear failed: %s", logService, formatRedfishError(resp.StatusCode, body))
	}

	fmt.Printf("%s cleared successfully.\n", strings.ToUpper(logService))
	return nil
}

func (c *ILOClient) FetchSensorData() error {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("FetchSensorData recovered from panic: %v<br/>", r)
		}
	}()

	// Step 1: Fetch the total number of sensors
	sensorsURL := fmt.Sprintf("%s/Chassis/1/Sensors/", c.BaseURL)
	req, err := http.NewRequest("GET", sensorsURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create sensors request: %v", err)
	}
	req.Header.Set("X-Auth-Token", c.Token)
	resp, err := c.Session.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch sensors count: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("fetching sensors count failed: %s", formatRedfishError(resp.StatusCode, body))
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read sensors response body: %v", err)
	}

	var sensorsData map[string]interface{}
	if err := json.Unmarshal(body, &sensorsData); err != nil {
		return fmt.Errorf("failed to parse sensors JSON: %v", err)
	}

	totalCount, ok := sensorsData["Members@odata.count"].(float64)
	if !ok {
		return fmt.Errorf("failed to retrieve total sensors count")
	}

	// Step 2: Fetch details for each sensor
	var sensors []struct {
		Name    string      `json:"Name"`
		Reading interface{} `json:"Reading"`
	}

	for i := 0; i <= int(totalCount); i++ {
		sensorURL := fmt.Sprintf("%s/Chassis/1/Sensors/%d", c.BaseURL, i)
		req, err := http.NewRequest("GET", sensorURL, nil)
		if err != nil {
			return fmt.Errorf("failed to create sensor request: %v", err)
		}
		req.Header.Set("X-Auth-Token", c.Token)

		resp, err := c.Session.Do(req)
		if err != nil {
			return fmt.Errorf("failed to fetch sensor %d: %v", i, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			continue
		}

		var sensorData struct {
			Name    string      `json:"Name"`
			Reading interface{} `json:"Reading"`
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read sensor response body: %v", err)
		}

		if err := json.Unmarshal(body, &sensorData); err != nil {
			return fmt.Errorf("failed to parse sensor JSON: %v", err)
		}

		sensors = append(sensors, sensorData)
	}

	// Step 3: Print results
	fmt.Println("Sensor Data:")

	maxNameLength := 10 // 預設最小長度
	for _, sensor := range sensors {
		if len(sensor.Name) > maxNameLength {
			maxNameLength = len(sensor.Name)
		}
	}

	formatHeader := fmt.Sprintf("%%-%ds | %%s\n", maxNameLength) // 使用 \n 作為換行符
	formatRow := fmt.Sprintf("%%-%ds | %%v\n", maxNameLength)    // 使用 \n 作為換行符
	border := strings.Repeat("=", maxNameLength+20)

	fmt.Println(border)
	fmt.Printf(formatHeader, "Name", "Reading")
	fmt.Println(border)

	for _, sensor := range sensors {
		fmt.Printf(formatRow, sensor.Name, sensor.Reading)
	}

	fmt.Println(border)

	return nil
}

func main() {
	verbose := false
	insecureImageTLS := false
	for _, argument := range os.Args[1:] {
		if argument == "-v" || argument == "--verbose" {
			verbose = true
		}
		if argument == "--i" {
			insecureImageTLS = true
		}
	}
	if len(os.Args) >= 3 && os.Args[1] == "-D" && os.Args[2] == "-firmware" {
		if len(os.Args) != 4 {
			fmt.Println("Usage: ./iloscript -D -firmware <folder>")
			return
		}
		files, scanErr := firmwareFilesInFolder(os.Args[3])
		if scanErr != nil {
			fmt.Printf("Firmware debug error: %v\n", scanErr)
			return
		}
		selected, skipped, selectionErr := selectFirmwareOrder(files, os.Stdin, os.Stdout)
		if selectionErr != nil {
			fmt.Printf("Firmware debug selection: %v\n", selectionErr)
			return
		}
		fmt.Println("\nDebug mode: no firmware was flashed.")
		fmt.Println("Selected flash order:")
		for index, path := range selected {
			fmt.Printf("  %d. %s\n", index+1, path)
		}
		fmt.Printf("Skipped: %d\n", len(skipped))
		return
	}
	if len(os.Args) == 2 && (os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help") {
		printHelp()
		return
	}
	if len(os.Args) < 3 {
		printHelp()
		return
	}
	if len(os.Args) == 3 && verbose {
		printHelp()
		return
	}
	if os.Args[2] == "-h" || os.Args[2] == "--help" || os.Args[2] == "help" {
		printHelp()
		return
	}

	iloIP := os.Args[1]
	client, err := NewILOClient(iloIP)
	if err != nil {
		fmt.Printf("Failed to create client: %v\n", err)
		os.Exit(1)
	}
	client.Verbose = verbose
	client.InsecureImageTLS = insecureImageTLS
	if !client.EnsureHTTPSReachable() {
		return
	}

	// 從 .env 檔案或環境變量獲取認證信息
	// godotenv.Load 找不到 .env 檔案時只會回傳錯誤但不影響流程，
	// 因為正式環境可能是直接注入環境變量而非透過 .env 檔案。
	_ = godotenv.Load()

	username := os.Getenv("ILO_USERNAME")
	password := os.Getenv("ILO_PASSWORD")
	if username == "" || password == "" {
		fmt.Println("Error: ILO_USERNAME and ILO_PASSWORD must be set via environment variables or a .env file.")
		fmt.Println("See .env.example for the expected format.")
		os.Exit(1)
	}

	if err := client.Login(username, password); err != nil {
		fmt.Printf("Login failed: %v\n", err)
		os.Exit(1)
	}
	if err := client.FetchSystemModel(); err != nil {
		fmt.Println("Cannot read SUT model name")
	}
	defer func() {
		if client.Token != "" {
			client.Logout()
		}
	}()

	switch os.Args[2] {
	case "-license":
		if err := client.runLicenseCommand(os.Args[3:]); err != nil {
			fmt.Printf("License operation failed: %v\n", err)
			os.Exit(1)
		}
	case "-iml":
		if len(os.Args) == 4 && strings.EqualFold(os.Args[3], "--clear") {
			if err := client.ClearLog("IML"); err != nil {
				fmt.Printf("IML clear failed: %v\n", err)
				os.Exit(1)
			}
			return
		}
		if len(os.Args) >= 4 && strings.EqualFold(os.Args[3], "monitor") {
			fetchCount := 10
			matchText := ""
			severity := ""
			argumentIndex := 4
			if len(os.Args) >= 5 {
				if count, parseErr := strconv.Atoi(os.Args[argumentIndex]); parseErr == nil && count > 0 {
					fetchCount = count
					argumentIndex++
				} else if strings.EqualFold(os.Args[argumentIndex], "Critical") || strings.EqualFold(os.Args[argumentIndex], "Warning") || strings.EqualFold(os.Args[argumentIndex], "OK") {
					severity = os.Args[argumentIndex]
					argumentIndex++
				} else {
					matchText = os.Args[argumentIndex]
					argumentIndex++
				}
			}
			if len(os.Args) > argumentIndex {
				severity = os.Args[argumentIndex]
			}
			ctx, cancel := monitorContext()
			defer cancel()
			if err := client.MonitorIML(ctx, fetchCount, matchText, severity); err != nil {
				fmt.Printf("IML monitoring failed: %v\n", err)
			}
			return
		}
		fetchCount := 10 // 預設值
		matchText := ""  // 預設值
		severity := ""   // 預設值

		// 根據參數數量處理不同的情況
		switch len(os.Args) {

		case 3:
			// 只有 -iml，保持預設值
		case 4:
			// 有一個額外參數
			arg := os.Args[3]
			if count, err := strconv.Atoi(arg); err == nil && count > 0 {
				// 第一個參數是 fetchCount
				fetchCount = count
			} else {
				// 第一個參數是 severity 或 matchText
				if strings.EqualFold(arg, "Critical") ||
					strings.EqualFold(arg, "Warning") ||
					strings.EqualFold(arg, "OK") {
					severity = arg
				} else {
					matchText = arg
				}
			}
		case 5:
			// 兩個額外參數
			// 第一個參數檢查是否為 fetchCount
			if count, err := strconv.Atoi(os.Args[3]); err == nil && count > 0 {
				fetchCount = count
			} else {
				fmt.Println("Invalid fetch count. Please provide a positive integer.")
				os.Exit(1)
			}

			// 第二個參數可能是 matchText 或 severity
			arg := os.Args[4]
			if strings.EqualFold(arg, "Critical") ||
				strings.EqualFold(arg, "Warning") ||
				strings.EqualFold(arg, "OK") {
				severity = arg
			} else {
				matchText = arg
			}
		case 6:
			// 三個額外參數
			if count, err := strconv.Atoi(os.Args[3]); err == nil && count > 0 {
				fetchCount = count
			} else {
				fmt.Println("Invalid fetch count. Please provide a positive integer.")
				os.Exit(1)
			}

			// 第二個參數是 matchText
			matchText = os.Args[4]

			// 第三個參數是 severity
			severity = os.Args[5]
			if !strings.EqualFold(severity, "Critical") &&
				!strings.EqualFold(severity, "Warning") &&
				!strings.EqualFold(severity, "OK") {
				fmt.Println("Invalid severity. Please use Critical, Warning, or OK.")
				os.Exit(1)
			}
		default:
			fmt.Println("Invalid number of arguments for -iml")
			os.Exit(1)
		}

		// 呼叫 FetchIMLEvents，並傳遞 fetchCount、matchText 和 severity
		if err := client.FetchIMLEvents(fetchCount, matchText, severity); err != nil {
			fmt.Printf("Error fetching IML events: %v\n", err)
			os.Exit(1)
		}

	// 在 switch 語句中添加新的 case
	case "-power":
		if len(os.Args) < 4 {
			fmt.Println("Usage: <ilo_ip> -power [on|off|reset|status|monitor]")
			os.Exit(1)
		}
		powerAction := os.Args[3]
		ctx, cancel := monitorContext()
		defer cancel()
		switch strings.ToLower(powerAction) {
		case "status":
			state, statusErr := client.FetchPowerState(ctx)
			if statusErr != nil {
				fmt.Printf("Power status failed: %v\n", statusErr)
				os.Exit(1)
			}
			fmt.Printf("PowerState: %s\n", state)
		case "monitor":
			if monitorErr := client.MonitorPower(ctx); monitorErr != nil {
				fmt.Printf("Power monitoring failed: %v\n", monitorErr)
			}
		default:
			if powerErr := client.PowerControlDetected(ctx, powerAction); powerErr != nil {
				fmt.Printf("Power control failed: %v\n", powerErr)
				client.Logout()
				os.Exit(1)
			}
		}
	case "-firmware":
		if len(os.Args) < 4 {
			fmt.Println("Usage: <ilo_ip> -firmware <image_path>... [--target auto|bios|ilo|manual]")
			os.Exit(1)
		}
		sources, targetKind, argumentErr := parseFirmwareArguments(os.Args[3:])
		if argumentErr != nil {
			fmt.Printf("Firmware argument error: %v\n", argumentErr)
			os.Exit(1)
		}
		var files []string
		var skippedFiles []string
		if len(sources) == 1 {
			if info, statErr := os.Stat(sources[0]); statErr == nil && info.IsDir() {
				files, argumentErr = firmwareFilesInFolder(sources[0])
				if argumentErr == nil {
					files, skippedFiles, argumentErr = selectFirmwareOrder(files, os.Stdin, os.Stdout)
				}
			} else {
				files = sources
			}
		} else {
			files = sources
		}
		if argumentErr != nil {
			fmt.Printf("Firmware selection error: %v\n", argumentErr)
			os.Exit(1)
		}
		ctx, cancel := monitorContext()
		defer cancel()
		results, flashErr := client.flashFirmwareBatch(ctx, files, targetKind)
		for _, path := range skippedFiles {
			results = append(results, firmwareFlashResult{Path: path, Status: "SKIPPED", Message: "excluded during interactive selection"})
		}
		printFirmwareSummary(results)
		if len(files) == 0 && flashErr == nil {
			fmt.Println("No firmware files selected; nothing to flash.")
			return
		}
		if flashErr != nil {
			fmt.Printf("Firmware batch stopped: %v\n", flashErr)
			os.Exit(1)
		}
	case "-ver":
		if err := client.FetchFirmwareInventory(); err != nil {
			fmt.Printf("Firmware get failed: %v\n", err)
			os.Exit(1)
		}

	case "-devices":
		writeJSON := false
		if len(os.Args) == 4 && strings.EqualFold(os.Args[3], "--output") {
			writeJSON = true
		} else if len(os.Args) != 3 {
			fmt.Println("Usage: <ilo_ip> -devices [--output]")
			os.Exit(1)
		}
		if writeJSON {
			if path, err := client.writeDevicesOutputJSON(""); err != nil {
				fmt.Printf("Warning: failed to write devices JSON: %v\n", err)
			} else {
				fmt.Printf("Generated file: %s\n", path)
			}
		} else if err := client.FetchDevices(); err != nil {
			fmt.Printf("Devices get failed: %v\n", err)
			os.Exit(1)
		}
	case "-storage":
		sortByBay := false
		ledMode := false
		if len(os.Args) == 4 && strings.EqualFold(os.Args[3], "--bay") {
			sortByBay = true
		} else if len(os.Args) == 4 && strings.EqualFold(os.Args[3], "--led") {
			ledMode = true
		} else if len(os.Args) == 5 && strings.EqualFold(os.Args[3], "--bay") && strings.EqualFold(os.Args[4], "--led") {
			sortByBay = true
			ledMode = true
		} else if len(os.Args) != 3 {
			fmt.Println("Usage: <ilo_ip> -storage [--bay] [--led]")
			os.Exit(1)
		}
		ctx, cancel := monitorContext()
		defer cancel()
		var storageErr error
		if ledMode {
			storageErr = client.RunStorageLED(ctx, sortByBay)
		} else {
			storageErr = client.FetchStorageDevices(sortByBay)
		}
		if storageErr != nil {
			fmt.Printf("Storage operation failed: %v\n", storageErr)
			os.Exit(1)
		}
	case "-sensors":
		if err := client.FetchSensorData(); err != nil {
			fmt.Printf("Sensor get failed: %v\n", err)
			os.Exit(1)
		}
	case "-iel":
		if len(os.Args) == 4 && strings.EqualFold(os.Args[3], "--clear") {
			if err := client.ClearLog("IEL"); err != nil {
				fmt.Printf("IEL clear failed: %v\n", err)
				os.Exit(1)
			}
			return
		}
		if err := client.FetchEventLogs(); err != nil {
			fmt.Printf("EventLog get failed: %v\n", err)
			os.Exit(1)
		}
	case "-ahs":
		outputFile := ""
		for i := 3; i < len(os.Args); i++ {
			if os.Args[i] == "-o" || os.Args[i] == "--output" {
				if i+1 < len(os.Args) {
					outputFile = os.Args[i+1]
				}
			}
		}
		ctx, cancel := monitorContext()
		defer cancel()
		if err := client.DownloadAHS(ctx, outputFile); err != nil {
			fmt.Printf("AHS download failed: %v\n", err)
			os.Exit(1)
		}
	case "-reset":
		if err := client.ClearLogsAndReset(); err != nil {
			fmt.Println("Error: ", err)
		} else {
			fmt.Println("Operation completed successfully.")
		}

	case "-bios":
		if len(os.Args) < 4 {
			fmt.Println("Usage: <ilo_ip> -bios [get|setup|patch] [attrName] [attrValue]")
			os.Exit(1)
		}

		switch os.Args[3] {
		case "get":
			settings, err := client.GetBIOSSettings()
			if err != nil {
				fmt.Printf("Failed to retrieve BIOS settings: %v\n", err)
				os.Exit(1)
			}

			// Print all attributes
			fmt.Println("BIOS Attributes:")
			for attr, value := range settings["Attributes"].(map[string]interface{}) {
				fmt.Printf("%s: %v\n", attr, value)
			}

		case "patch":
			if len(os.Args) < 6 {
				fmt.Println("Usage: <ilo_ip> -bios patch <attrName> <attrValue>")
				os.Exit(1)
			}

			attrName := os.Args[4]
			attrValue := os.Args[5]

			if err := client.PatchBIOSSetting(attrName, attrValue); err != nil {
				fmt.Printf("Failed to patch BIOS setting: %v\n", err)
				os.Exit(1)
			}

		case "setup":
			err := client.PostBIOSSetting()
			if err != nil {
				fmt.Printf("Error updating BIOS setting: %v", err)
				os.Exit(1)
			}

		case "reset":
			err := client.ResetBIOS()
			if err != nil {
				fmt.Printf("Error reset BIOS setting: %v", err)
				os.Exit(1)
			}

		default:
			fmt.Println("Usage: <ilo_ip> -bios [get|setup|reset|patch] [attrName] [attrValue]")
			os.Exit(1)
		}

	default:
		if len(os.Args) >= 3 {
			firmwareURL := os.Args[2]

			// 验证 firmwareURL 是否为有效的 URL
			if !isValidURL(firmwareURL) {
				fmt.Println("Error: The provided firmware URL is not valid.")
				fmt.Println("Usage: <ilo_ip> <firmware_url> [match_text] [--target auto|bios|ilo|manual]")
				fmt.Println("[match_text] can be ignored.")
				os.Exit(1)
			}

			sources, targetKind, argumentErr := parseFirmwareArguments(os.Args[2:])
			if argumentErr != nil {
				fmt.Printf("Firmware argument error: %v\n", argumentErr)
				os.Exit(1)
			}
			if len(sources) != 1 || !isValidURL(sources[0]) {
				fmt.Println("Remote firmware mode accepts exactly one HTTPS firmware URL.")
				os.Exit(1)
			}
			firmwareURL = sources[0]

			ctx, cancel := monitorContext()
			defer cancel()
			updateInfo, err := client.UpdateFirmwareWithTarget(ctx, firmwareURL, targetKind)
			if err != nil {
				fmt.Printf("Firmware update failed: %v\n", err)
				os.Exit(1)
			}
			restoreFirmwareSettings := func() {
				if updateInfo.RestoreRemoteServerCertificate != nil {
					if restoreErr := client.RestoreFirmwareUpdateSettings(context.Background(), updateInfo); restoreErr != nil {
						fmt.Printf("Warning: failed to restore remote certificate verification: %v\n", restoreErr)
					}
					updateInfo.RestoreRemoteServerCertificate = nil
				}
			}
			defer restoreFirmwareSettings()

			if updateInfo.TaskURI != "" {
				if err := client.MonitorUpdate(updateInfo.TaskURI, "flash", updateInfo.TargetKind, updateTimeout); err != nil {
					fmt.Printf("Monitoring failed: %v\n", err)
					restoreFirmwareSettings()
					os.Exit(1)
				}
			}
			if waitErr := client.WaitForFirmwareTarget(ctx, updateInfo, time.Duration(updateTimeout)*time.Second); waitErr != nil {
				fmt.Printf("Firmware verification failed: %v\n", waitErr)
				restoreFirmwareSettings()
				os.Exit(1)
			}

		} else {
			fmt.Println("Usage: <ilo_ip> [<firmware_url> [match_text]")
			fmt.Println("[match_text] can be ignored.")
			os.Exit(1)
		}

	}
}
