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
	"os"
	"strings"
)

type FirmwareCapabilities struct {
	MultipartHTTPPushURI string
	HTTPPushURI          string
	SimpleUpdateTarget   string
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

func taskURIFromResponse(resp *http.Response) string {
	if location := resp.Header.Get("Location"); location != "" {
		return location
	}
	return resp.Header.Get("Content-Location")
}

func (c *ILOClient) UploadFirmwareMultipart(ctx context.Context, imagePath, endpoint string) (string, error) {
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
	if _, err := parameters.Write([]byte(`{"Targets":[],"ForceUpdate":false}`)); err != nil {
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
	capabilities, err := c.DiscoverFirmwareCapabilities(ctx)
	if err != nil {
		return "", err
	}
	if capabilities.MultipartHTTPPushURI != "" {
		return c.UploadFirmwareMultipart(ctx, imagePath, capabilities.MultipartHTTPPushURI)
	}
	if capabilities.HTTPPushURI != "" {
		return c.UploadFirmwareMultipart(ctx, imagePath, capabilities.HTTPPushURI)
	}
	return "", fmt.Errorf("iLO does not advertise a local firmware upload endpoint; use a remote firmware URL")
}
