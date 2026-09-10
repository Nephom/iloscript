package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// FetchAHSLocation queries the ActiveHealthSystem resource and returns the
// Links map (name -> URL) that iLO advertises for AHS log downloads.
func (c *ILOClient) FetchAHSLocation(ctx context.Context) (map[string]string, error) {
	url := c.BaseURL + "/Managers/1/ActiveHealthSystem"
	body, _, err := c.getJSON(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch AHS location: %w", err)
	}

	var data struct {
		Links struct {
			AHSLocation   map[string]string `json:"AHSLocation"`
			RecentWeek    map[string]string `json:"RecentWeek"`
			InfoSight     map[string]string `json:"InfoSight"`
			OneDaySlimAHS map[string]string `json:"OneDaySlimAHS"`
		} `json:"Links"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("failed to parse AHS response: %w", err)
	}

	links := make(map[string]string)
	if ref, ok := data.Links.AHSLocation["extref"]; ok {
		links["AHSLocation"] = ref
	}
	if ref, ok := data.Links.RecentWeek["extref"]; ok {
		links["RecentWeek"] = ref
	}
	if ref, ok := data.Links.InfoSight["extref"]; ok {
		links["InfoSight"] = ref
	}
	if ref, ok := data.Links.OneDaySlimAHS["extref"]; ok {
		links["OneDaySlimAHS"] = ref
	}

	if len(links) == 0 {
		return nil, fmt.Errorf("no AHS location links found in response")
	}
	return links, nil
}

// DownloadAHS downloads the full AHS log (downloadAll=1) and writes it to
// outputFile. If outputFile is empty, a default name is derived from the
// AHS location URL.
func (c *ILOClient) DownloadAHS(ctx context.Context, outputFile string) error {
	links, err := c.FetchAHSLocation(ctx)
	if err != nil {
		return err
	}

	ahsURL, ok := links["AHSLocation"]
	if !ok {
		return fmt.Errorf("AHSLocation link not found")
	}

	// Build the download URL with downloadAll=1
	downloadURL := strings.Replace(ahsURL, "days=7", "downloadAll=1", 1)
	downloadURL = strings.Replace(downloadURL, "minimalDL=1&&days=1", "downloadAll=1", 1)
	if !strings.Contains(downloadURL, "downloadAll=1") {
		if strings.Contains(downloadURL, "?") {
			downloadURL = downloadURL + "&downloadAll=1"
		} else {
			downloadURL = downloadURL + "?downloadAll=1"
		}
	}
	downloadURL = strings.ReplaceAll(downloadURL, " ", "%20")

	fullURL := c.downloadURLFromExtref(downloadURL)
	c.debugf("Downloading AHS log from %s", fullURL)

	if outputFile == "" {
		outputFile = deriveAHSFilename(ahsURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create AHS download request: %w", err)
	}
	req.Header.Set("X-Auth-Token", c.Token)

	resp, err := c.Session.Do(req)
	if err != nil {
		return fmt.Errorf("AHS download request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("AHS download failed: %s", formatRedfishError(resp.StatusCode, body))
	}

	out, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("failed to create output file %s: %w", outputFile, err)
	}
	defer out.Close()

	written, err := io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write AHS log to %s: %w", outputFile, err)
	}

	fmt.Printf("AHS log downloaded: %s (%s)\n", outputFile, formatFileSize(written))
	return nil
}

// downloadURLFromExtref converts an extref path (e.g. /ahsdata/file.ahs?downloadAll=1)
// into a full HTTPS URL using the client's base host.
func (c *ILOClient) downloadURLFromExtref(extref string) string {
	if strings.HasPrefix(extref, "http://") || strings.HasPrefix(extref, "https://") {
		return extref
	}
	base := c.BaseURL
	if idx := strings.Index(base, "/redfish/"); idx > 0 {
		base = base[:idx]
	}
	base = strings.TrimSuffix(base, "/")
	return base + extref
}

// deriveAHSFilename extracts a usable filename from an AHS extref URL.
// e.g. "/ahsdata/HPE_iLO8 DCSCM_20260903.ahs?downloadAll=1" -> "HPE_iLO8 DCSCM_20260903.ahs"
func deriveAHSFilename(extref string) string {
	name := extref
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if idx := strings.Index(name, "?"); idx >= 0 {
		name = name[:idx]
	}
	name = strings.ReplaceAll(name, " ", "_")
	dir := filepath.Dir(name)
	if dir == "." {
		return name
	}
	return name
}

// formatFileSize returns a human-readable file size string.
func formatFileSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), "kMGTPE"[exp])
}
