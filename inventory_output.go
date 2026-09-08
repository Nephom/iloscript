package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// devicesOutput preserves the existing header/footer/body report shape.
type devicesOutput struct {
	Header []string    `json:"header"`
	Footer interface{} `json:"footer"`
	Body   [][]string  `json:"body"`
}

// devicesOutputLabels lists every label, in exact order, exactly as it appears
// in 2026818.json. A label is filled only when -devices actually reports a
// value for it; otherwise the cell is "N/A".
var devicesOutputLabels = []string{
	"Board",
	"Processor/CPU",
	"DIMM (NVDIMM)",
	"Options",
	"BP",
	"Drives",
	"TPM",
	"PSU",
	"T-Bird",
	"BIOS",
	"iLO/BMC/ServerManagement",
	"ME/IE",
	"Power PIC",
	"OS",
	"Test Kit",
	"SPP/GIAUS",
	"CPLD",
	"Others",
}

const na = "N/A"

type firmwareEntry struct {
	Name    string `json:"Name"`
	Version string `json:"Version"`
}

var illegalFilenameChars = regexp.MustCompile(`[\\/:*?"<>|]`)

// sanitizeFileName keeps the model name readable (spaces preserved) while
// stripping characters that are not allowed in file names.
func sanitizeFileName(name string) string {
	name = illegalFilenameChars.ReplaceAllString(name, "")
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		name = "unknown"
	}
	return name
}

// replaceCommas converts every comma to a semicolon, since the Info column must
// never contain a comma.
func replaceCommas(s string) string {
	return strings.ReplaceAll(s, ",", ";")
}

// withCount appends "x<n>" when the same value appears more than once.
func withCount(value string, count int) string {
	if count > 1 {
		return fmt.Sprintf("%s x%d", value, count)
	}
	return value
}

// countValues tallies identical values in document order so that duplicate
// items can be collapsed into a single "value x<n>" cell.
func countValues(values []string) []string {
	seen := make(map[string]int)
	order := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if seen[v] == 0 {
			order = append(order, v)
		}
		seen[v]++
	}
	out := make([]string, 0, len(order))
	for _, v := range order {
		out = append(out, withCount(v, seen[v]))
	}
	return out
}

// joinValues combines the (already collapsed) distinct values with "; ".
func joinValues(values []string) string {
	if len(values) == 0 {
		return na
	}
	return replaceCommas(strings.Join(values, "; "))
}

// findFirmware returns the version for the first firmware name (case-insensitive
// prefix match) matching any of the given prefixes, or "" when none match.
func findFirmware(items []firmwareEntry, prefixes ...string) string {
	for _, item := range items {
		for _, prefix := range prefixes {
			if strings.EqualFold(item.Name, prefix) || strings.HasPrefix(strings.ToLower(item.Name), strings.ToLower(prefix)) {
				if item.Version != "" {
					return item.Version
				}
			}
		}
	}
	return ""
}

// collectFirmwareForOutput mirrors the original firmware inventory reader
// without changing the terminal inventory command. The JSON report uses the
// same numeric member URLs as the original iLO implementation.
func (c *ILOClient) collectFirmwareForOutput(ctx context.Context) ([]firmwareEntry, error) {
	body, _, err := c.getJSON(ctx, c.BaseURL+"/UpdateService/FirmwareInventory")
	if err != nil {
		return nil, err
	}
	var collection struct {
		MemberCount int `json:"Members@odata.count"`
	}
	if err := json.Unmarshal(body, &collection); err != nil {
		return nil, fmt.Errorf("parse FirmwareInventory collection: %w", err)
	}

	items := make([]firmwareEntry, 0, collection.MemberCount)
	for index := 1; index <= collection.MemberCount; index++ {
		itemBody, _, err := c.getJSON(ctx, fmt.Sprintf("%s/UpdateService/FirmwareInventory/%d", c.BaseURL, index))
		if err != nil {
			continue
		}
		var item firmwareEntry
		if err := json.Unmarshal(itemBody, &item); err != nil || item.Name == "" {
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

// collectProcessors returns the distinct processor descriptions in socket order.
func (c *ILOClient) collectProcessors(ctx context.Context) ([]string, error) {
	uris, err := c.fetchCollectionMemberURIs(ctx, "/redfish/v1/Systems/1/Processors")
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(uris))
	for _, uri := range uris {
		body, _, err := c.getJSON(ctx, c.resolveURI(uri))
		if err != nil {
			continue
		}
		var p redfishProcessor
		if err := json.Unmarshal(body, &p); err != nil || isAbsent(p.Status.State) {
			continue
		}
		model := p.Model
		if model == "" {
			model = p.Id
		}
		if model == "" {
			continue
		}
		if p.MaxSpeedMHz > 0 {
			model = fmt.Sprintf("%s %dMHz", model, p.MaxSpeedMHz)
		}
		values = append(values, model)
	}
	return values, nil
}

// collectMemory returns the distinct DIMM descriptions (part number preferred,
// falling back to serial / part-number-with-manufacturer) in device order.
func (c *ILOClient) collectMemory(ctx context.Context) ([]string, error) {
	uris, err := c.fetchCollectionMemberURIs(ctx, "/redfish/v1/Systems/1/Memory")
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(uris))
	for _, uri := range uris {
		body, _, err := c.getJSON(ctx, c.resolveURI(uri))
		if err != nil {
			continue
		}
		var m redfishMemory
		if err := json.Unmarshal(body, &m); err != nil || isAbsent(m.Status.State) {
			continue
		}
		desc := m.PartNumber
		if desc == "" {
			desc = m.SerialNumber
		}
		if desc == "" {
			desc = m.Name
		}
		if desc == "" {
			continue
		}
		if m.CapacityMiB > 0 {
			desc = fmt.Sprintf("%s %dGB", desc, m.CapacityMiB/1024)
		}
		if m.Manufacturer != "" {
			desc = fmt.Sprintf("%s %s", m.Manufacturer, desc)
		}
		values = append(values, desc)
	}
	return values, nil
}

// collectDrives returns the distinct drive descriptions in bay order.
func (c *ILOClient) collectDrives(ctx context.Context) ([]string, error) {
	drives, err := c.fetchStorageDrives(ctx, true)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(drives))
	for _, d := range drives {
		desc := d.Drive.Model
		if desc == "" {
			desc = d.Drive.SerialNumber
		}
		if desc == "" {
			continue
		}
		if d.Capacity != "" {
			desc = fmt.Sprintf("%s %s", desc, d.Capacity)
		}
		values = append(values, desc)
	}
	return values, nil
}

// collectPowerSupplies returns the distinct PSU descriptions (model preferred,
// part number as fallback) in physical order.
func (c *ILOClient) collectPowerSupplies(ctx context.Context) ([]string, error) {
	body, _, err := c.getJSON(ctx, c.resolveURI("/redfish/v1/Chassis/1/Power"))
	var primary []redfishPowerSupply
	if err == nil {
		var power struct {
			PowerSupplies []redfishPowerSupply `json:"PowerSupplies"`
		}
		if unmarshalErr := json.Unmarshal(body, &power); unmarshalErr == nil {
			primary = power.PowerSupplies
		}
	}
	values := make([]string, 0, len(primary))
	for _, psu := range primary {
		if isAbsent(psu.Status.State) {
			continue
		}
		desc := psu.Model
		if desc == "" {
			desc = psu.PartNumber
		}
		if desc == "" {
			desc = psu.Name
		}
		if desc == "" {
			continue
		}
		if psu.Manufacturer != "" {
			desc = fmt.Sprintf("%s %s", psu.Manufacturer, desc)
		}
		values = append(values, desc)
	}
	if len(values) == 0 {
		rows, fallbackErr := c.fetchPowerSubsystemSupplies(ctx)
		if fallbackErr != nil {
			if err != nil {
				return nil, err
			}
			return nil, fallbackErr
		}
		for _, row := range rows {
			if len(row) > 2 && row[2] != "" {
				values = append(values, row[2])
			} else if len(row) > 3 {
				values = append(values, row[3])
			}
		}
	}
	return values, nil
}

// collectNICs returns the distinct NIC descriptions (manufacturer + model) in
// adapter order.
func (c *ILOClient) collectNICs(ctx context.Context) ([]string, error) {
	adapterURIs, err := c.fetchCollectionMemberURIs(ctx, "/redfish/v1/Chassis/1/NetworkAdapters")
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(adapterURIs))
	for _, uri := range adapterURIs {
		body, _, err := c.getJSON(ctx, c.resolveURI(uri))
		if err != nil {
			continue
		}
		var adapter redfishNetworkAdapter
		if err := json.Unmarshal(body, &adapter); err != nil {
			continue
		}
		desc := adapter.Model
		if desc == "" {
			desc = adapter.Name
		}
		if desc == "" {
			continue
		}
		if adapter.Manufacturer != "" {
			desc = fmt.Sprintf("%s %s", adapter.Manufacturer, desc)
		}
		values = append(values, desc)
	}
	return values, nil
}

// collectDevicesOutput assembles the 18-label body (plus SecondaryCPLD after
// CPLD when present). It reuses the same Redfish endpoints the -devices print
// sections use and does not add any new HTTP calls.
func (c *ILOClient) collectDevicesOutput() (*devicesOutput, error) {
	ctx := context.Background()

	out := &devicesOutput{
		Header: []string{"", "Tags", `Info (DO NOT use comma "," in this column)`},
		Footer: nil,
	}

	board := c.Model
	if board == "" {
		board = na
	}
	firmware, _ := c.collectFirmwareForOutput(ctx)
	processors, _ := c.collectProcessors(ctx)
	memory, _ := c.collectMemory(ctx)
	drives, _ := c.collectDrives(ctx)
	psus, _ := c.collectPowerSupplies(ctx)
	nics, _ := c.collectNICs(ctx)
	chassis, _ := c.fetchChassisInventory(ctx)

	values := make(map[string]string, len(devicesOutputLabels)+2)
	values["Board"] = board
	values["Processor/CPU"] = joinValues(countValues(processors))
	values["DIMM (NVDIMM)"] = joinValues(countValues(memory))
	values["Options"] = na
	values["BP"] = na
	values["Drives"] = joinValues(countValues(drives))
	values["TPM"] = na
	values["PSU"] = joinValues(countValues(psus))
	values["T-Bird"] = na
	values["BIOS"] = joinValues(countValues([]string{findFirmware(firmware, "System ROM")}))
	values["iLO/BMC/ServerManagement"] = joinValues(countValues([]string{findFirmware(firmware, "iLO")}))
	values["ME/IE"] = joinValues(countValues([]string{findFirmware(firmware, "ME/IE", "ME", "IE")}))
	values["Power PIC"] = na
	values["OS"] = na
	values["Test Kit"] = na
	values["SPP/GIAUS"] = na
	values["CPLD"] = joinValues(countValues([]string{findFirmware(firmware, "System Programmable Logic Device")}))
	values["Others"] = joinValues(countValues(nics))

	// Dynamic firmware fields are inserted after CPLD and before the remaining
	// fixed fields. They therefore stay before Others without changing the
	// order of any fixed field.
	var secondaryVersions []string
	for _, item := range firmware {
		if strings.HasPrefix(strings.ToLower(item.Name), "secondary") {
			secondaryVersions = append(secondaryVersions, item.Version)
		}
	}
	for _, label := range devicesOutputLabels {
		if label == "Power PIC" && len(secondaryVersions) > 0 {
			out.Body = append(out.Body, []string{"", "SecondaryCPLD", joinValues(countValues(secondaryVersions))})
		}
		if label == "BP" {
			rows := compactBackplaneRows(chassis)
			if len(rows) > 0 {
				out.Body = append(out.Body, rows...)
				continue
			}
		}
		if label == "Drives" {
			rows := compactDriveRows(chassis)
			if len(rows) > 0 {
				out.Body = append(out.Body, rows...)
				continue
			}
		}
		out.Body = append(out.Body, []string{"", label, values[label]})
	}

	return out, nil
}

// writeDevicesOutputJSON writes the hardware inventory to
// <dir>/<model>_<yyyymmdd>.json. A nil/empty dir means the current directory.
func (c *ILOClient) writeDevicesOutputJSON(outDir string) (string, error) {
	out, err := c.collectDevicesOutput()
	if err != nil {
		return "", err
	}

	if outDir == "" {
		outDir = "."
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}

	name := fmt.Sprintf("%s_%s.json",
		sanitizeFileName(c.Model),
		time.Now().Format("20060102"))
	full := filepath.Join(outDir, name)

	data, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encode inventory json: %w", err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return "", fmt.Errorf("write inventory file: %w", err)
	}
	return full, nil
}

// sortStringSlice is a small helper kept for any future sorted joins.
func sortStringSlice(s []string) []string {
	sort.Strings(s)
	return s
}
