package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type storageReference struct {
	ODataID string `json:"@odata.id"`
}

type storageDrive struct {
	ODataID                 string `json:"@odata.id"`
	Model                   string `json:"Model"`
	SerialNumber            string `json:"SerialNumber"`
	CapacityBytes           int64  `json:"CapacityBytes"`
	LocationIndicatorActive *bool  `json:"LocationIndicatorActive"`
	PhysicalLocation        struct {
		PartLocation struct {
			ServiceLabel string `json:"ServiceLabel"`
		} `json:"PartLocation"`
	} `json:"PhysicalLocation"`
	Location []struct {
		Info string `json:"Info"`
	} `json:"Location"`
}

type storageDriveListItem struct {
	Drive      storageDrive
	Location   string
	Port       int
	PortSuffix string
	Box        int
	Bay        int
	HasPort    bool
	HasBox     bool
	HasBay     bool
	Capacity   string
}

func storageLocation(drive storageDrive) string {
	if label := drive.PhysicalLocation.PartLocation.ServiceLabel; label != "" {
		return label
	}
	if len(drive.Location) > 0 {
		return drive.Location[0].Info
	}
	return "Unknown"
}

func storageLocationSortKeys(location string) (port int, portSuffix string, box, bay int, hasPort, hasBox, hasBay bool) {
	parts := strings.Split(location, ":")
	for _, part := range parts {
		pair := strings.SplitN(part, "=", 2)
		if len(pair) != 2 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(pair[0])) {
		case "port":
			portText := strings.TrimSpace(pair[1])
			portDigits := strings.TrimRight(portText, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz")
			value, err := strconv.Atoi(portDigits)
			if err != nil {
				continue
			}
			port, portSuffix, hasPort = value, strings.ToLower(strings.TrimPrefix(portText, portDigits)), true
		case "box":
			value, err := strconv.Atoi(strings.TrimSpace(pair[1]))
			if err != nil {
				continue
			}
			box, hasBox = value, true
		case "bay":
			value, err := strconv.Atoi(strings.TrimSpace(pair[1]))
			if err != nil {
				continue
			}
			bay, hasBay = value, true
		}
	}
	return
}

func compareStorageLocation(left, right storageDriveListItem, sortByBay bool) bool {
	compare := func(leftValue, rightValue int, leftSet, rightSet bool) int {
		if leftSet != rightSet {
			if leftSet {
				return -1
			}
			return 1
		}
		if !leftSet {
			return 0
		}
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
		return 0
	}

	comparePort := func() int {
		return compare(left.Port, right.Port, left.HasPort, right.HasPort)
	}
	compareBoxAndBay := func() int {
		for _, values := range [][4]interface{}{
			{left.Box, right.Box, left.HasBox, right.HasBox},
			{left.Bay, right.Bay, left.HasBay, right.HasBay},
		} {
			if result := compare(values[0].(int), values[1].(int), values[2].(bool), values[3].(bool)); result != 0 {
				return result
			}
		}
		return 0
	}

	if sortByBay {
		if result := compareBoxAndBay(); result != 0 {
			return result < 0
		}
	} else if result := comparePort(); result != 0 {
		return result < 0
	} else if left.PortSuffix != right.PortSuffix {
		return left.PortSuffix < right.PortSuffix
	} else if result := compareBoxAndBay(); result != 0 {
		return result < 0
	}
	if left.Location != right.Location {
		return left.Location < right.Location
	}
	return left.Drive.ODataID < right.Drive.ODataID
}

func formatStorageCapacity(capacityBytes int64) string {
	if capacityBytes <= 0 {
		return "Unknown"
	}
	const terabyte = float64(1000 * 1000 * 1000 * 1000)
	const gigabyte = float64(1000 * 1000 * 1000)
	if float64(capacityBytes) >= terabyte {
		return fmt.Sprintf("%.2f TB", float64(capacityBytes)/terabyte)
	}
	return fmt.Sprintf("%.2f GB", float64(capacityBytes)/gigabyte)
}

func (c *ILOClient) fetchStorageDrives(ctx context.Context, sortByBay bool) ([]storageDriveListItem, error) {
	body, _, err := c.getJSON(ctx, c.BaseURL+"/Systems/1/Storage")
	if err != nil {
		return nil, fmt.Errorf("fetch storage collection: %w", err)
	}

	var collection struct {
		Members []storageReference `json:"Members"`
	}
	if err := json.Unmarshal(body, &collection); err != nil {
		return nil, fmt.Errorf("parse storage collection: %w", err)
	}

	var drives []storageDriveListItem
	for _, storage := range collection.Members {
		if storage.ODataID == "" {
			continue
		}
		storageBody, _, err := c.getJSON(ctx, c.resolveURI(storage.ODataID))
		if err != nil {
			return nil, fmt.Errorf("fetch storage %s: %w", storage.ODataID, err)
		}
		var storageData struct {
			Drives []storageReference `json:"Drives"`
		}
		if err := json.Unmarshal(storageBody, &storageData); err != nil {
			return nil, fmt.Errorf("parse storage %s: %w", storage.ODataID, err)
		}

		for _, driveReference := range storageData.Drives {
			if driveReference.ODataID == "" {
				continue
			}
			driveBody, _, err := c.getJSON(ctx, c.resolveURI(driveReference.ODataID))
			if err != nil {
				return nil, fmt.Errorf("fetch drive %s: %w", driveReference.ODataID, err)
			}
			var drive storageDrive
			if err := json.Unmarshal(driveBody, &drive); err != nil {
				return nil, fmt.Errorf("parse drive %s: %w", driveReference.ODataID, err)
			}
			drive.ODataID = driveReference.ODataID
			location := storageLocation(drive)
			port, portSuffix, box, bay, hasPort, hasBox, hasBay := storageLocationSortKeys(location)
			drives = append(drives, storageDriveListItem{
				Drive: drive, Location: location,
				Port: port, PortSuffix: portSuffix, Box: box, Bay: bay,
				HasPort: hasPort, HasBox: hasBox, HasBay: hasBay,
				Capacity: formatStorageCapacity(drive.CapacityBytes),
			})
		}
	}

	sort.SliceStable(drives, func(i, j int) bool {
		return compareStorageLocation(drives[i], drives[j], sortByBay)
	})
	return drives, nil
}

func (c *ILOClient) FetchStorageDevices(sortByBay bool) error {
	drives, err := c.fetchStorageDrives(context.Background(), sortByBay)
	if err != nil {
		return err
	}
	printStorageDevices(drives)
	return nil
}

func printStorageDevices(drives []storageDriveListItem) {
	if len(drives) == 0 {
		fmt.Println("No storage devices found.")
		return
	}

	modelWidth, serialWidth, locationWidth := len("Model"), len("SerialNumber"), len("Location")
	for _, drive := range drives {
		modelWidth = maxStorageColumnWidth(modelWidth, drive.Drive.Model)
		serialWidth = maxStorageColumnWidth(serialWidth, drive.Drive.SerialNumber)
		locationWidth = maxStorageColumnWidth(locationWidth, drive.Location)
	}
	line := fmt.Sprintf("+%s+%s+%s+%s+", strings.Repeat("-", modelWidth+2), strings.Repeat("-", serialWidth+2), strings.Repeat("-", locationWidth+2), strings.Repeat("-", 14))
	fmt.Println(line)
	fmt.Printf("| %-*s | %-*s | %-*s | %-12s |\n", modelWidth, "Model", serialWidth, "SerialNumber", locationWidth, "Location", "Capacity")
	fmt.Println(line)
	for _, drive := range drives {
		fmt.Printf("| %-*s | %-*s | %-*s | %-12s |\n", modelWidth, drive.Drive.Model, serialWidth, drive.Drive.SerialNumber, locationWidth, drive.Location, drive.Capacity)
	}
	fmt.Println(line)
}

func (c *ILOClient) setDriveLocationIndicator(ctx context.Context, drive storageDriveListItem, active bool) error {
	payload, err := json.Marshal(map[string]bool{"LocationIndicatorActive": active})
	if err != nil {
		return fmt.Errorf("marshal location indicator request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.resolveURI(drive.Drive.ODataID), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create location indicator request: %w", err)
	}
	req.Header.Set("X-Auth-Token", c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Session.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("set LocationIndicatorActive=%t failed with HTTP %d: %s", active, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *ILOClient) RunStorageLED(ctx context.Context, sortByBay bool) error {
	drives, err := c.fetchStorageDrives(ctx, sortByBay)
	if err != nil {
		return err
	}
	if len(drives) == 0 {
		fmt.Println("No storage devices found.")
		return nil
	}

	fmt.Println("Storage drive LED sequence started. Each drive will be active for 20 seconds.")
	for index, drive := range drives {
		restoreState := false
		if drive.Drive.LocationIndicatorActive != nil {
			restoreState = *drive.Drive.LocationIndicatorActive
		}
		fmt.Printf("[%d/%d] %s - %s\n", index+1, len(drives), drive.Location, drive.Drive.SerialNumber)
		if err := c.setDriveLocationIndicator(ctx, drive, true); err != nil {
			return fmt.Errorf("activate LED for %s: %w", drive.Location, err)
		}

		waitErr := sleepWithContext(ctx, 20*time.Second)
		cleanupCtx := ctx
		if waitErr != nil {
			cleanupCtx = context.Background()
		}
		if err := c.setDriveLocationIndicator(cleanupCtx, drive, restoreState); err != nil {
			return fmt.Errorf("restore LED for %s: %w", drive.Location, err)
		}
		if waitErr != nil {
			fmt.Println("Storage drive LED sequence interrupted.")
			return nil
		}
	}
	fmt.Println("Storage drive LED sequence completed.")
	return nil
}

func maxStorageColumnWidth(current int, value string) int {
	if len(value) > current {
		return len(value)
	}
	return current
}
