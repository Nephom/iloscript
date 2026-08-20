package main

import (
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
	"path"
	"strings"
	"time"
)

type FirmwareCapabilities struct {
	MultipartHTTPPushURI string
	HTTPPushURI          string
	SimpleUpdateTarget   string
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
	TaskURI       string
	TargetURI     string
	BeforeVersion string
}

func parseFirmwareArguments(arguments []string) (string, string, string, error) {
	if len(arguments) == 0 {
		return "", "", "", fmt.Errorf("firmware source is required")
	}
	source := arguments[0]
	matchText := "None"
	targetKind := "auto"
	for index := 1; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--target" {
			if index+1 >= len(arguments) {
				return "", "", "", fmt.Errorf("--target requires bios or ilo")
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
	if targetKind != "auto" && targetKind != "bios" && targetKind != "ilo" {
		return "", "", "", fmt.Errorf("invalid firmware target %q; use auto, bios, or ilo", targetKind)
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
				Target string `json:"target"`
			} `json:"#UpdateService.SimpleUpdate"`
		} `json:"Actions"`
		HttpPushURI          string `json:"HttpPushUri"`
		MultipartHTTPPushURI string `json:"MultipartHttpPushUri"`
	}
	if err := json.Unmarshal(body, &service); err != nil {
		return FirmwareCapabilities{}, fmt.Errorf("parse UpdateService: %w", err)
	}
	return FirmwareCapabilities{
		MultipartHTTPPushURI: service.MultipartHTTPPushURI,
		HTTPPushURI:          service.HttpPushURI,
		SimpleUpdateTarget:   service.Actions.SimpleUpdate.Target,
	}, nil
}

func (c *ILOClient) UpdateFirmwareWithTarget(ctx context.Context, firmwareURL, requestedKind string) (FirmwareUpdateInfo, error) {
	target, err := c.SelectFirmwareTarget(ctx, firmwareURL, requestedKind)
	if err != nil {
		return FirmwareUpdateInfo{}, err
	}
	capabilities, err := c.DiscoverFirmwareCapabilities(ctx)
	if err != nil {
		return FirmwareUpdateInfo{}, err
	}
	if capabilities.SimpleUpdateTarget == "" {
		return FirmwareUpdateInfo{}, fmt.Errorf("iLO does not advertise UpdateService.SimpleUpdate")
	}
	beforeVersion, err := c.FirmwareTargetVersion(ctx, target.ODataID)
	if err != nil {
		return FirmwareUpdateInfo{}, fmt.Errorf("read firmware target version: %w", err)
	}
	payload := map[string]interface{}{
		"ImageURI":         firmwareURL,
		"TransferProtocol": "HTTP",
		"Targets":          []string{target.ODataID},
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
	resp, err := c.Session.Do(req)
	if err != nil {
		return FirmwareUpdateInfo{}, err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
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
	return FirmwareUpdateInfo{TaskURI: taskURI, TargetURI: target.ODataID, BeforeVersion: beforeVersion}, nil
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

func inferFirmwareTarget(source string) string {
	parsed, err := url.Parse(source)
	if err != nil {
		return ""
	}
	name := strings.ToLower(path.Base(parsed.Path))
	if strings.Contains(name, "bios") || strings.Contains(name, "systemrom") || strings.Contains(name, "system-rom") || isHPESystemROMName(name) {
		return "bios"
	}
	if strings.Contains(name, "ilo") || strings.Contains(name, "bmc") {
		return "ilo"
	}
	return ""
}

func isHPESystemROMName(name string) bool {
	if len(name) < 3 || name[0] != 'a' {
		return false
	}
	return name[1] >= '0' && name[1] <= '9' && name[2] >= '0' && name[2] <= '9'
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
	if kind == "" || kind == "auto" {
		kind = inferFirmwareTarget(source)
	}
	if kind != "bios" && kind != "ilo" {
		return FirmwareInventoryTarget{}, fmt.Errorf("cannot determine firmware target from %q; specify --target bios or --target ilo", source)
	}
	targets, err := c.DiscoverFirmwareTargets(ctx)
	if err != nil {
		return FirmwareInventoryTarget{}, err
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
	parametersPayload := map[string]interface{}{"Targets": []string{}, "ForceUpdate": false}
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
	resp, err := c.Session.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
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
	beforeVersion, err := c.FirmwareTargetVersion(ctx, target.ODataID)
	if err != nil {
		return FirmwareUpdateInfo{}, fmt.Errorf("read firmware target version: %w", err)
	}
	capabilities, err := c.DiscoverFirmwareCapabilities(ctx)
	if err != nil {
		return FirmwareUpdateInfo{}, err
	}
	if capabilities.MultipartHTTPPushURI != "" {
		taskURI, uploadErr := c.UploadFirmwareMultipartWithTarget(ctx, imagePath, capabilities.MultipartHTTPPushURI, target.ODataID)
		return FirmwareUpdateInfo{TaskURI: taskURI, TargetURI: target.ODataID, BeforeVersion: beforeVersion}, uploadErr
	}
	if capabilities.HTTPPushURI != "" {
		taskURI, uploadErr := c.UploadFirmwareMultipartWithTarget(ctx, imagePath, capabilities.HTTPPushURI, target.ODataID)
		return FirmwareUpdateInfo{TaskURI: taskURI, TargetURI: target.ODataID, BeforeVersion: beforeVersion}, uploadErr
	}
	return FirmwareUpdateInfo{}, fmt.Errorf("iLO does not advertise a local firmware upload endpoint; use a remote firmware URL")
}
