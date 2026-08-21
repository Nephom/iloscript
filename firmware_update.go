package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

type FirmwareCapabilities struct {
	MultipartHTTPPushURI          string
	HTTPPushURI                   string
	SimpleUpdateTarget            string
	TransferProtocols             []string
	VerifyRemoteServerCertificate *bool
}

type FirmwareInventoryTarget struct {
	ID          string `json:"Id"`
	Name        string `json:"Name"`
	Description string `json:"Description"`
	Version     string `json:"Version"`
	Updateable  bool   `json:"Updateable"`
	ODataID     string `json:"@odata.id"`
}

type FirmwareUpdateInfo struct {
	TaskURI                        string
	TargetURI                      string
	TargetKind                     string
	BeforeVersion                  string
	RestoreRemoteServerCertificate *bool
}

func parseFirmwareArguments(arguments []string) (string, string, string, error) {
	if len(arguments) == 0 {
		return "", "", "", fmt.Errorf("firmware source is required")
	}
	source := arguments[0]
	matchText := "None"
	targetKind := "bios"
	for index := 1; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "-v" || argument == "--verbose" || argument == "--i" {
			continue
		}
		if argument == "--target" {
			if index+1 >= len(arguments) {
				return "", "", "", fmt.Errorf("--target requires auto, bios, ilo, or manual")
			}
			targetKind = strings.ToLower(arguments[index+1])
			index++
			continue
		}
		if strings.HasPrefix(argument, "--target=") {
			targetKind = strings.ToLower(strings.TrimPrefix(argument, "--target="))
			continue
		}
		if matchText == "None" {
			matchText = argument
			continue
		}
		return "", "", "", fmt.Errorf("unexpected firmware argument %q", argument)
	}
	if targetKind != "auto" && targetKind != "bios" && targetKind != "ilo" && targetKind != "manual" {
		return "", "", "", fmt.Errorf("invalid firmware target %q; use auto, bios, ilo, or manual", targetKind)
	}
	return source, matchText, targetKind, nil
}

func (c *ILOClient) resolveURI(resourceURI string) string {
	if strings.HasPrefix(resourceURI, "http://") || strings.HasPrefix(resourceURI, "https://") {
		return resourceURI
	}
	if strings.HasPrefix(resourceURI, "/redfish/v1") {
		return c.BaseURL + strings.TrimPrefix(resourceURI, "/redfish/v1")
	}
	if strings.HasPrefix(resourceURI, "/") {
		base := strings.TrimSuffix(c.BaseURL, "/redfish/v1")
		return base + resourceURI
	}
	return c.BaseURL + "/" + resourceURI
}

func (c *ILOClient) DiscoverFirmwareCapabilities(ctx context.Context) (FirmwareCapabilities, error) {
	body, _, err := c.getJSON(ctx, c.BaseURL+"/UpdateService/")
	if err != nil {
		return FirmwareCapabilities{}, err
	}
	var service struct {
		Actions struct {
			SimpleUpdate struct {
				Target    string   `json:"target"`
				Protocols []string `json:"TransferProtocol@Redfish.AllowableValues"`
			} `json:"#UpdateService.SimpleUpdate"`
		} `json:"Actions"`
		HttpPushURI                   string `json:"HttpPushUri"`
		MultipartHTTPPushURI          string `json:"MultipartHttpPushUri"`
		VerifyRemoteServerCertificate *bool  `json:"VerifyRemoteServerCertificate"`
	}
	if err := json.Unmarshal(body, &service); err != nil {
		return FirmwareCapabilities{}, fmt.Errorf("parse UpdateService: %w", err)
	}
	return FirmwareCapabilities{
		MultipartHTTPPushURI:          service.MultipartHTTPPushURI,
		HTTPPushURI:                   service.HttpPushURI,
		SimpleUpdateTarget:            service.Actions.SimpleUpdate.Target,
		TransferProtocols:             service.Actions.SimpleUpdate.Protocols,
		VerifyRemoteServerCertificate: service.VerifyRemoteServerCertificate,
	}, nil
}

func (c *ILOClient) SetRemoteServerCertificateVerification(ctx context.Context, enabled bool) error {
	body, err := json.Marshal(map[string]bool{"VerifyRemoteServerCertificate": enabled})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.BaseURL+"/UpdateService/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Auth-Token", c.Token)
	req.Header.Set("Content-Type", "application/json")
	c.debugf("PATCH UpdateService VerifyRemoteServerCertificate=%t", enabled)
	resp, err := c.Session.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	c.debugf("PATCH UpdateService -> HTTP %d; response=%s", resp.StatusCode, truncateDebugBody(responseBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("set VerifyRemoteServerCertificate=%t failed with HTTP %d: %s", enabled, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

func (c *ILOClient) UpdateFirmwareWithTarget(ctx context.Context, firmwareURL, requestedKind string) (FirmwareUpdateInfo, error) {
	target, err := c.SelectFirmwareTarget(ctx, firmwareURL, requestedKind)
	if err != nil {
		return FirmwareUpdateInfo{}, err
	}
	c.debugf("firmware target selected: kind=%s name=%q uri=%s version=%q", requestedKind, target.Name, target.ODataID, target.Version)
	capabilities, err := c.DiscoverFirmwareCapabilities(ctx)
	if err != nil {
		return FirmwareUpdateInfo{}, err
	}
	if capabilities.SimpleUpdateTarget == "" {
		return FirmwareUpdateInfo{}, fmt.Errorf("iLO does not advertise UpdateService.SimpleUpdate")
	}
	parsedURL, err := url.Parse(firmwareURL)
	if err != nil || !strings.EqualFold(parsedURL.Scheme, "https") {
		return FirmwareUpdateInfo{}, fmt.Errorf("remote firmware source must use HTTPS; received %q", firmwareURL)
	}
	var beforeVersion string
	if target.ODataID != "" {
		beforeVersion, err = c.FirmwareTargetVersion(ctx, target.ODataID)
		if err != nil {
			return FirmwareUpdateInfo{}, fmt.Errorf("read firmware target version: %w", err)
		}
	}
	var restoreRemoteCertificate *bool
	keepRemoteCertificateDisabled := false
	if c.InsecureImageTLS {
		if capabilities.VerifyRemoteServerCertificate == nil {
			return FirmwareUpdateInfo{}, fmt.Errorf("iLO does not expose VerifyRemoteServerCertificate; cannot use --i safely")
		}
		original := *capabilities.VerifyRemoteServerCertificate
		if original {
			if err := c.SetRemoteServerCertificateVerification(ctx, false); err != nil {
				return FirmwareUpdateInfo{}, err
			}
			restoreRemoteCertificate = &original
			defer func() {
				if !keepRemoteCertificateDisabled {
					_ = c.SetRemoteServerCertificateVerification(context.Background(), original)
				}
			}()
		}
	}
	payload := map[string]interface{}{
		"ImageURI":         firmwareURL,
		"TransferProtocol": "HTTPS",
	}
	if target.ODataID != "" {
		payload["Targets"] = []string{target.ODataID}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return FirmwareUpdateInfo{}, fmt.Errorf("marshal firmware update payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolveURI(capabilities.SimpleUpdateTarget), bytes.NewReader(body))
	if err != nil {
		return FirmwareUpdateInfo{}, fmt.Errorf("create firmware update request: %w", err)
	}
	req.Header.Set("X-Auth-Token", c.Token)
	req.Header.Set("Content-Type", "application/json")
	c.debugf("POST SimpleUpdate url=%s image=%s target=%s payload=%s", req.URL.String(), firmwareURL, target.ODataID, string(body))
	resp, err := c.Session.Do(req)
	if err != nil {
		return FirmwareUpdateInfo{}, err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	c.debugf("POST SimpleUpdate -> HTTP %d Location=%q Content-Location=%q response=%s", resp.StatusCode, resp.Header.Get("Location"), resp.Header.Get("Content-Location"), truncateDebugBody(responseBody))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return FirmwareUpdateInfo{}, fmt.Errorf("firmware update failed with HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	taskURI := taskURIFromResponse(resp)
	if taskURI == "" && len(responseBody) > 0 {
		var response map[string]interface{}
		if json.Unmarshal(responseBody, &response) == nil {
			for _, key := range []string{"TaskURI", "TaskMonitorURI", "TaskMonitor", "@odata.id"} {
				if value, ok := response[key].(string); ok && value != "" {
					taskURI = value
					break
				}
			}
		}
	}
	fmt.Printf("Update initiated for %s (%s). Task URI: %s\n", target.Name, target.ODataID, taskURI)
	updateInfo := FirmwareUpdateInfo{TaskURI: taskURI, TargetURI: target.ODataID, BeforeVersion: beforeVersion}
	updateInfo.TargetKind = resolvedFirmwareTargetKind(firmwareURL, requestedKind)
	updateInfo.RestoreRemoteServerCertificate = restoreRemoteCertificate
	keepRemoteCertificateDisabled = true
	return updateInfo, nil
}

func (c *ILOClient) VerifyFirmwareTarget(ctx context.Context, info FirmwareUpdateInfo) error {
	if info.TargetURI == "" {
		return fmt.Errorf("firmware target URI is missing")
	}
	version, err := c.FirmwareTargetVersion(ctx, info.TargetURI)
	if err != nil {
		return fmt.Errorf("verify firmware target version: %w", err)
	}
	if version == "" || version == info.BeforeVersion {
		return fmt.Errorf("firmware task completed, but target version did not change (before=%q after=%q)", info.BeforeVersion, version)
	}
	fmt.Printf("Firmware target verified: %s -> %s\n", info.BeforeVersion, version)
	return nil
}

func (c *ILOClient) RestoreFirmwareUpdateSettings(ctx context.Context, info FirmwareUpdateInfo) error {
	if info.RestoreRemoteServerCertificate == nil {
		return nil
	}
	return c.SetRemoteServerCertificateVerification(ctx, *info.RestoreRemoteServerCertificate)
}

func (c *ILOClient) WaitForFirmwareTarget(ctx context.Context, info FirmwareUpdateInfo, timeout time.Duration) error {
	if info.TargetURI == "" {
		return fmt.Errorf("firmware target URI is missing")
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	for {
		if err := c.VerifyFirmwareTarget(ctx, info); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("firmware target version did not change before timeout")
		case <-ticker.C:
		}
	}
}

func (c *ILOClient) MonitorUpdateService(ctx context.Context, timeout int) error {
	start := time.Now()
	fmt.Println("\n開始監控 UpdateService firmware progress...")
	for time.Since(start) < time.Duration(timeout)*time.Second {
		data, err := c.FetchOemHpeData()
		if err != nil {
			return fmt.Errorf("failed to fetch UpdateService progress: %w", err)
		}
		state := data.State
		progress := data.FlashProgressPercent
		if state == "" {
			state = "Unknown"
		}
		fmt.Printf("\r進度: %d%%, 更新狀態: %s        ", progress, state)
		c.debugf("UpdateService progress state=%s percent=%d", state, progress)
		if strings.EqualFold(state, "Complete") || strings.EqualFold(state, "Completed") {
			fmt.Println("\n更新完成!")
			return nil
		}
		if strings.EqualFold(state, "Failed") || strings.EqualFold(state, "Exception") || strings.EqualFold(state, "Killed") || strings.EqualFold(state, "Cancelled") {
			fmt.Println("\n更新失敗!")
			return fmt.Errorf("firmware update failed with UpdateService state %s", state)
		}
		if err := sleepWithContext(ctx, monitorInterval); err != nil {
			return err
		}
	}
	return fmt.Errorf("firmware update monitoring timeout")
}

func (c *ILOClient) DiscoverFirmwareTargets(ctx context.Context) ([]FirmwareInventoryTarget, error) {
	body, _, err := c.getJSON(ctx, c.BaseURL+"/UpdateService/FirmwareInventory/")
	if err != nil {
		return nil, err
	}
	var collection struct {
		Members []struct {
			ODataID string `json:"@odata.id"`
		} `json:"Members"`
	}
	if err := json.Unmarshal(body, &collection); err != nil {
		return nil, fmt.Errorf("parse firmware inventory collection: %w", err)
	}
	targets := make([]FirmwareInventoryTarget, 0, len(collection.Members))
	for _, member := range collection.Members {
		if member.ODataID == "" {
			continue
		}
		itemBody, _, itemErr := c.getJSON(ctx, c.resolveURI(member.ODataID))
		if itemErr != nil {
			return nil, fmt.Errorf("read firmware inventory %s: %w", member.ODataID, itemErr)
		}
		var target FirmwareInventoryTarget
		if err := json.Unmarshal(itemBody, &target); err != nil {
			return nil, fmt.Errorf("parse firmware inventory %s: %w", member.ODataID, err)
		}
		if target.ODataID == "" {
			target.ODataID = member.ODataID
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func resolvedFirmwareTargetKind(source, requestedKind string) string {
	kind := strings.ToLower(strings.TrimSpace(requestedKind))
	if kind == "" {
		return "bios"
	}
	return kind
}

func firmwareTargetMatches(target FirmwareInventoryTarget, kind string) bool {
	text := strings.ToLower(target.Name + " " + target.Description)
	switch kind {
	case "bios":
		return strings.Contains(text, "system rom") || strings.Contains(text, "bios") || strings.Contains(text, "systemrom")
	case "ilo":
		return strings.Contains(strings.ToLower(target.Name), "ilo") || strings.Contains(strings.ToLower(target.Description), "bmc")
	default:
		return false
	}
}

func (c *ILOClient) SelectFirmwareTarget(ctx context.Context, source, requestedKind string) (FirmwareInventoryTarget, error) {
	kind := strings.ToLower(strings.TrimSpace(requestedKind))
	if kind == "" {
		kind = "bios"
	}
	if kind == "auto" {
		return FirmwareInventoryTarget{}, nil
	}
	targets, err := c.DiscoverFirmwareTargets(ctx)
	if err != nil {
		return FirmwareInventoryTarget{}, err
	}
	if kind == "manual" {
		return selectManualFirmwareTarget(targets, os.Stdin, os.Stdout)
	}
	if kind != "bios" && kind != "ilo" {
		return FirmwareInventoryTarget{}, fmt.Errorf("invalid firmware target %q; use auto, bios, ilo, or manual", kind)
	}
	var matches []FirmwareInventoryTarget
	for _, target := range targets {
		if target.Updateable && target.ODataID != "" && firmwareTargetMatches(target, kind) {
			matches = append(matches, target)
		}
	}
	if len(matches) != 1 {
		return FirmwareInventoryTarget{}, fmt.Errorf("expected one updateable %s target, found %d", kind, len(matches))
	}
	return matches[0], nil
}

func selectManualFirmwareTarget(targets []FirmwareInventoryTarget, input io.Reader, output io.Writer) (FirmwareInventoryTarget, error) {
	if len(targets) == 0 {
		return FirmwareInventoryTarget{}, fmt.Errorf("firmware inventory is empty")
	}

	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tName\tVersion\tDescription")
	for _, target := range targets {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", target.ID, target.Name, target.Version, target.Description)
	}
	if err := writer.Flush(); err != nil {
		return FirmwareInventoryTarget{}, fmt.Errorf("display firmware inventory: %w", err)
	}

	fmt.Fprint(output, "Enter firmware inventory ID: ")
	reader := bufio.NewReader(input)
	var selectedID string
	if _, err := fmt.Fscanln(reader, &selectedID); err != nil {
		return FirmwareInventoryTarget{}, fmt.Errorf("read firmware inventory ID: %w", err)
	}
	for _, target := range targets {
		if target.ID != selectedID {
			continue
		}
		if !target.Updateable {
			return FirmwareInventoryTarget{}, fmt.Errorf("firmware inventory ID %q is not updateable", selectedID)
		}
		if target.ODataID == "" {
			return FirmwareInventoryTarget{}, fmt.Errorf("firmware inventory ID %q has no @odata.id", selectedID)
		}
		return target, nil
	}
	return FirmwareInventoryTarget{}, fmt.Errorf("firmware inventory ID %q was not found", selectedID)
}

func (c *ILOClient) FirmwareTargetVersion(ctx context.Context, targetURI string) (string, error) {
	body, _, err := c.getJSON(ctx, c.resolveURI(targetURI))
	if err != nil {
		return "", err
	}
	var target FirmwareInventoryTarget
	if err := json.Unmarshal(body, &target); err != nil {
		return "", fmt.Errorf("parse firmware target: %w", err)
	}
	return target.Version, nil
}

func taskURIFromResponse(resp *http.Response) string {
	if location := resp.Header.Get("Location"); location != "" {
		return location
	}
	return resp.Header.Get("Content-Location")
}

func (c *ILOClient) UploadFirmwareMultipart(ctx context.Context, imagePath, endpoint string) (string, error) {
	return c.UploadFirmwareMultipartWithTarget(ctx, imagePath, endpoint, "")
}

func (c *ILOClient) UploadFirmwareMultipartWithTarget(ctx context.Context, imagePath, endpoint, targetURI string) (string, error) {
	image, err := os.Open(imagePath)
	if err != nil {
		return "", fmt.Errorf("open firmware image: %w", err)
	}
	defer image.Close()

	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	parametersHeader := make(textproto.MIMEHeader)
	parametersHeader.Set("Content-Disposition", `form-data; name="UpdateParameters"`)
	parametersHeader.Set("Content-Type", "application/json")
	parameters, err := writer.CreatePart(parametersHeader)
	if err != nil {
		return "", fmt.Errorf("create update parameters part: %w", err)
	}
	parametersPayload := map[string]interface{}{"ForceUpdate": false}
	if targetURI != "" {
		parametersPayload["Targets"] = []string{targetURI}
	}
	parametersBody, err := json.Marshal(parametersPayload)
	if err != nil {
		return "", fmt.Errorf("marshal update parameters: %w", err)
	}
	if _, err := parameters.Write(parametersBody); err != nil {
		return "", fmt.Errorf("write update parameters: %w", err)
	}
	fileHeader := make(textproto.MIMEHeader)
	fileHeader.Set("Content-Disposition", `form-data; name="UpdateFile"; filename="firmware.bin"`)
	fileHeader.Set("Content-Type", "application/octet-stream")
	filePart, err := writer.CreatePart(fileHeader)
	if err != nil {
		return "", fmt.Errorf("create firmware part: %w", err)
	}
	if _, err := io.Copy(filePart, image); err != nil {
		return "", fmt.Errorf("read firmware image: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolveURI(endpoint), &payload)
	if err != nil {
		return "", fmt.Errorf("create multipart request: %w", err)
	}
	req.Header.Set("X-Auth-Token", c.Token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c.debugf("POST multipart firmware url=%s image=%s target=%s content-length=%d", req.URL.String(), imagePath, targetURI, payload.Len())
	resp, err := c.Session.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	c.debugf("POST multipart firmware -> HTTP %d Location=%q Content-Location=%q response=%s", resp.StatusCode, resp.Header.Get("Location"), resp.Header.Get("Content-Location"), truncateDebugBody(responseBody))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("multipart firmware update failed with HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	if taskURI := taskURIFromResponse(resp); taskURI != "" {
		return taskURI, nil
	}
	var response struct {
		TaskURI string `json:"TaskURI"`
		Task    struct {
			ID string `json:"@odata.id"`
		} `json:"Task"`
	}
	if len(responseBody) > 0 && json.Unmarshal(responseBody, &response) == nil {
		if response.TaskURI != "" {
			return response.TaskURI, nil
		}
		if response.Task.ID != "" {
			return response.Task.ID, nil
		}
	}
	return "", nil
}

func (c *ILOClient) UpdateFirmwareImage(ctx context.Context, imagePath string) (string, error) {
	info, err := c.UpdateFirmwareImageWithTarget(ctx, imagePath, "auto")
	if err != nil {
		return "", err
	}
	return info.TaskURI, nil
}

func (c *ILOClient) UpdateFirmwareImageWithTarget(ctx context.Context, imagePath, requestedKind string) (FirmwareUpdateInfo, error) {
	target, err := c.SelectFirmwareTarget(ctx, imagePath, requestedKind)
	if err != nil {
		return FirmwareUpdateInfo{}, err
	}
	var beforeVersion string
	if target.ODataID != "" {
		beforeVersion, err = c.FirmwareTargetVersion(ctx, target.ODataID)
		if err != nil {
			return FirmwareUpdateInfo{}, fmt.Errorf("read firmware target version: %w", err)
		}
	}
	capabilities, err := c.DiscoverFirmwareCapabilities(ctx)
	if err != nil {
		return FirmwareUpdateInfo{}, err
	}
	if capabilities.MultipartHTTPPushURI != "" {
		taskURI, uploadErr := c.UploadFirmwareMultipartWithTarget(ctx, imagePath, capabilities.MultipartHTTPPushURI, target.ODataID)
		return FirmwareUpdateInfo{TaskURI: taskURI, TargetURI: target.ODataID, TargetKind: resolvedFirmwareTargetKind(imagePath, requestedKind), BeforeVersion: beforeVersion}, uploadErr
	}
	if capabilities.HTTPPushURI != "" {
		taskURI, uploadErr := c.UploadFirmwareMultipartWithTarget(ctx, imagePath, capabilities.HTTPPushURI, target.ODataID)
		return FirmwareUpdateInfo{TaskURI: taskURI, TargetURI: target.ODataID, TargetKind: resolvedFirmwareTargetKind(imagePath, requestedKind), BeforeVersion: beforeVersion}, uploadErr
	}
	return FirmwareUpdateInfo{}, fmt.Errorf("iLO does not advertise a local firmware upload endpoint; use a remote firmware URL")
}
