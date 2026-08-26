package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// FetchDevices produces an lshw-style hardware report made up of several
// independent sections. Each section is best-effort: a failure in one
// section (missing endpoint on older firmware, etc.) is reported as a
// warning and does not prevent the remaining sections from running.
func (c *ILOClient) FetchDevices() error {
	ctx := context.Background()

	fmt.Println("##### Firmware Inventory #####")
	if err := c.FetchFirmwareInventory(); err != nil {
		fmt.Printf("Warning: firmware inventory unavailable: %v\n", err)
	}
	fmt.Println()

	fmt.Println("##### Storage #####")
	if drives, err := c.fetchStorageDrives(ctx, false); err != nil {
		fmt.Println(friendlySectionError(err))
	} else {
		printStorageDevices(drives)
	}
	fmt.Println()

	fmt.Println("##### Processor #####")
	if err := c.printProcessors(ctx); err != nil {
		fmt.Println(friendlySectionError(err))
	}
	fmt.Println()

	fmt.Println("##### Memory #####")
	if err := c.printMemory(ctx); err != nil {
		fmt.Println(friendlySectionError(err))
	}
	fmt.Println()

	fmt.Println("##### NIC #####")
	if err := c.printNICs(ctx); err != nil {
		fmt.Println(friendlySectionError(err))
	}
	fmt.Println()

	fmt.Println("##### Power Supply #####")
	if err := c.printPowerSupplies(ctx); err != nil {
		fmt.Println(friendlySectionError(err))
	}

	return nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// fetchCollectionMemberURIs GETs a Redfish collection resource and returns
// the @odata.id of every member.
func (c *ILOClient) fetchCollectionMemberURIs(ctx context.Context, collectionURI string) ([]string, error) {
	body, _, err := c.getJSON(ctx, c.resolveURI(collectionURI))
	if err != nil {
		return nil, err
	}
	var collection struct {
		Members []storageReference `json:"Members"`
	}
	if err := json.Unmarshal(body, &collection); err != nil {
		return nil, fmt.Errorf("parse collection %s: %w", collectionURI, err)
	}
	uris := make([]string, 0, len(collection.Members))
	for _, member := range collection.Members {
		if member.ODataID != "" {
			uris = append(uris, member.ODataID)
		}
	}
	return uris, nil
}

// printDynamicTable renders a boxed table (same visual style as the
// firmware/storage tables) but automatically drops any column for which
// every row is empty, and leaves individual cells blank (never "N/A"/"-")
// when that particular record does not have a value.
func printDynamicTable(headers []string, rows [][]string) {
	if len(rows) == 0 {
		fmt.Println("No data found.")
		return
	}

	visible := make([]bool, len(headers))
	for _, row := range rows {
		for index, cell := range row {
			if index < len(visible) && strings.TrimSpace(cell) != "" {
				visible[index] = true
			}
		}
	}

	var activeHeaders []string
	var activeIndexes []int
	for index, header := range headers {
		if visible[index] {
			activeHeaders = append(activeHeaders, header)
			activeIndexes = append(activeIndexes, index)
		}
	}
	if len(activeHeaders) == 0 {
		fmt.Println("No data found.")
		return
	}

	widths := make([]int, len(activeHeaders))
	for index, header := range activeHeaders {
		widths[index] = len(header)
	}

	filteredRows := make([][]string, len(rows))
	for rowIndex, row := range rows {
		newRow := make([]string, len(activeIndexes))
		for columnIndex, sourceIndex := range activeIndexes {
			if sourceIndex < len(row) {
				newRow[columnIndex] = row[sourceIndex]
			}
			if len(newRow[columnIndex]) > widths[columnIndex] {
				widths[columnIndex] = len(newRow[columnIndex])
			}
		}
		filteredRows[rowIndex] = newRow
	}

	line := func() string {
		parts := make([]string, len(widths))
		for index, width := range widths {
			parts[index] = strings.Repeat("-", width+2)
		}
		return "+" + strings.Join(parts, "+") + "+"
	}
	formatRow := func(row []string) string {
		parts := make([]string, len(row))
		for index, value := range row {
			parts[index] = fmt.Sprintf(" %-*s ", widths[index], value)
		}
		return "|" + strings.Join(parts, "|") + "|"
	}

	fmt.Println(line())
	fmt.Println(formatRow(activeHeaders))
	fmt.Println(line())
	for _, row := range filteredRows {
		fmt.Println(formatRow(row))
	}
	fmt.Println(line())
}

func formatIntIfPositive(value int, suffix string) string {
	if value <= 0 {
		return ""
	}
	return strconv.Itoa(value) + suffix
}

func isAbsent(state string) bool {
	return strings.EqualFold(state, "Absent")
}

// httpStatusPattern extracts the HTTP status code from getJSON's error text
// ("request returned HTTP %d: %s"), without requiring any signature changes
// to getJSON or its callers in storage.go/session_monitor.go.
var httpStatusPattern = regexp.MustCompile(`HTTP (\d{3})`)

// friendlySectionError turns a "this resource is not advertised/supported"
// style HTTP error (400/404/405/501) into a short, user-facing message
// instead of dumping the raw Go/HTTP error text. Any other failure (network,
// timeout, 5xx, etc.) is still reported with detail since those need
// investigation.
func friendlySectionError(err error) string {
	if err == nil {
		return ""
	}
	if match := httpStatusPattern.FindStringSubmatch(err.Error()); match != nil {
		if code, convErr := strconv.Atoi(match[1]); convErr == nil {
			switch code {
			case 400, 404, 405, 501:
				return "目前沒有找到這個資源"
			}
		}
	}
	return fmt.Sprintf("讀取失敗: %v", err)
}

// ---------------------------------------------------------------------------
// Processor
// ---------------------------------------------------------------------------

type redfishProcessor struct {
	Id           string `json:"Id"`
	Socket       string `json:"Socket"`
	Manufacturer string `json:"Manufacturer"`
	Model        string `json:"Model"`
	MaxSpeedMHz  int    `json:"MaxSpeedMHz"`
	TotalCores   int    `json:"TotalCores"`
	TotalThreads int    `json:"TotalThreads"`
	Status       struct {
		Health string `json:"Health"`
		State  string `json:"State"`
	} `json:"Status"`
}

func (c *ILOClient) printProcessors(ctx context.Context) error {
	uris, err := c.fetchCollectionMemberURIs(ctx, "/redfish/v1/Systems/1/Processors")
	if err != nil {
		return err
	}

	headers := []string{"Socket", "Manufacturer", "Model", "Speed", "Cores", "Threads", "Health"}
	var rows [][]string

	for _, uri := range uris {
		body, _, err := c.getJSON(ctx, c.resolveURI(uri))
		if err != nil {
			fmt.Printf("Warning: failed to fetch processor %s: %v\n", uri, err)
			continue
		}
		var processor redfishProcessor
		if err := json.Unmarshal(body, &processor); err != nil {
			fmt.Printf("Warning: failed to parse processor %s: %v\n", uri, err)
			continue
		}
		if isAbsent(processor.Status.State) {
			continue
		}

		socket := processor.Socket
		if socket == "" {
			socket = processor.Id
		}
		speed := ""
		if processor.MaxSpeedMHz > 0 {
			speed = fmt.Sprintf("%.2fGHz", float64(processor.MaxSpeedMHz)/1000)
		}

		rows = append(rows, []string{
			socket,
			processor.Manufacturer,
			processor.Model,
			speed,
			formatIntIfPositive(processor.TotalCores, ""),
			formatIntIfPositive(processor.TotalThreads, ""),
			processor.Status.Health,
		})
	}

	printDynamicTable(headers, rows)
	return nil
}

// ---------------------------------------------------------------------------
// Memory (DIMM)
// ---------------------------------------------------------------------------

type redfishMemory struct {
	Id                string `json:"Id"`
	Name              string `json:"Name"`
	DeviceLocator     string `json:"DeviceLocator"`
	CapacityMiB       int    `json:"CapacityMiB"`
	Manufacturer      string `json:"Manufacturer"`
	PartNumber        string `json:"PartNumber"`
	SerialNumber      string `json:"SerialNumber"`
	MemoryDeviceType  string `json:"MemoryDeviceType"`
	OperatingSpeedMhz int    `json:"OperatingSpeedMhz"`
	RankCount         int    `json:"RankCount"`
	Status            struct {
		Health string `json:"Health"`
		State  string `json:"State"`
	} `json:"Status"`
}

func (c *ILOClient) printMemory(ctx context.Context) error {
	uris, err := c.fetchCollectionMemberURIs(ctx, "/redfish/v1/Systems/1/Memory")
	if err != nil {
		return err
	}

	headers := []string{"DIMM", "Manufacturer", "PartNumber", "SerialNumber", "Capacity", "Type", "Speed", "Rank", "Health"}
	var rows [][]string

	for _, uri := range uris {
		body, _, err := c.getJSON(ctx, c.resolveURI(uri))
		if err != nil {
			fmt.Printf("Warning: failed to fetch memory %s: %v\n", uri, err)
			continue
		}
		var memory redfishMemory
		if err := json.Unmarshal(body, &memory); err != nil {
			fmt.Printf("Warning: failed to parse memory %s: %v\n", uri, err)
			continue
		}
		if isAbsent(memory.Status.State) || memory.CapacityMiB == 0 {
			continue
		}

		location := memory.DeviceLocator
		if location == "" {
			location = memory.Name
		}
		if location == "" {
			location = memory.Id
		}
		capacity := ""
		if memory.CapacityMiB > 0 {
			capacity = fmt.Sprintf("%dGiB", memory.CapacityMiB/1024)
		}
		speed := ""
		if memory.OperatingSpeedMhz > 0 {
			speed = fmt.Sprintf("%dMT/s", memory.OperatingSpeedMhz)
		}

		rows = append(rows, []string{
			location,
			memory.Manufacturer,
			memory.PartNumber,
			memory.SerialNumber,
			capacity,
			memory.MemoryDeviceType,
			speed,
			formatIntIfPositive(memory.RankCount, "R"),
			memory.Status.Health,
		})
	}

	printDynamicTable(headers, rows)
	return nil
}

// ---------------------------------------------------------------------------
// NIC
// ---------------------------------------------------------------------------

type redfishNetworkAdapter struct {
	Id           string `json:"Id"`
	Name         string `json:"Name"`
	Manufacturer string `json:"Manufacturer"`
	Model        string `json:"Model"`
	PartNumber   string `json:"PartNumber"`
	SerialNumber string `json:"SerialNumber"`
	Controllers  []struct {
		FirmwarePackageVersion string `json:"FirmwarePackageVersion"`
	} `json:"Controllers"`
	Status struct {
		Health string `json:"Health"`
	} `json:"Status"`
	NetworkDeviceFunctions storageReference `json:"NetworkDeviceFunctions"`
	Ports                  storageReference `json:"Ports"`
	NetworkPorts           storageReference `json:"NetworkPorts"`
}

type redfishNetworkDeviceFunction struct {
	Id       string `json:"Id"`
	Ethernet struct {
		MACAddress string `json:"MACAddress"`
	} `json:"Ethernet"`
}

// redfishNetworkPort models the current Redfish "Port" schema
// (Port.v1_6_0+), where link speed is reported in Gbps.
type redfishNetworkPort struct {
	Id               string  `json:"Id"`
	LinkStatus       string  `json:"LinkStatus"`
	CurrentSpeedGbps float64 `json:"CurrentSpeedGbps"`
}

// redfishNetworkPortLegacy models the older Redfish "NetworkPort" schema
// (NetworkPort.v1_2_x and earlier), which some HPE NICs still advertise
// alongside/instead of the newer "Port" schema. Speed here is already in
// Mbps, and the MAC address is reported directly on the port instead of via
// NetworkDeviceFunctions.
type redfishNetworkPortLegacy struct {
	Id                         string   `json:"Id"`
	LinkStatus                 string   `json:"LinkStatus"`
	CurrentLinkSpeedMbps       int      `json:"CurrentLinkSpeedMbps"`
	AssociatedNetworkAddresses []string `json:"AssociatedNetworkAddresses"`
}

type redfishEthernetInterface struct {
	Id         string `json:"Id"`
	Name       string `json:"Name"`
	MACAddress string `json:"MACAddress"`
	SpeedMbps  int    `json:"SpeedMbps"`
	LinkStatus string `json:"LinkStatus"`
	FullDuplex *bool  `json:"FullDuplex"`
	Status     struct {
		Health string `json:"Health"`
	} `json:"Status"`
}

func (c *ILOClient) printNICs(ctx context.Context) error {
	adapterURIs, err := c.fetchCollectionMemberURIs(ctx, "/redfish/v1/Chassis/1/NetworkAdapters")
	if err == nil && len(adapterURIs) > 0 {
		return c.printNetworkAdapters(ctx, adapterURIs)
	}
	if err != nil {
		fmt.Printf("Warning: physical NetworkAdapters unavailable, falling back to EthernetInterfaces: %v\n", err)
	}
	return c.printEthernetInterfaces(ctx)
}

func (c *ILOClient) printNetworkAdapters(ctx context.Context, adapterURIs []string) error {
	headers := []string{"Adapter", "Manufacturer", "Model", "PartNumber", "SerialNumber", "Firmware", "Port", "MACAddress", "Speed", "LinkStatus", "Health"}
	var rows [][]string

	for _, adapterURI := range adapterURIs {
		body, _, err := c.getJSON(ctx, c.resolveURI(adapterURI))
		if err != nil {
			fmt.Printf("Warning: failed to fetch network adapter %s: %v\n", adapterURI, err)
			continue
		}
		var adapter redfishNetworkAdapter
		if err := json.Unmarshal(body, &adapter); err != nil {
			fmt.Printf("Warning: failed to parse network adapter %s: %v\n", adapterURI, err)
			continue
		}

		name := adapter.Name
		if name == "" {
			name = adapter.Id
		}
		firmware := ""
		if len(adapter.Controllers) > 0 {
			firmware = adapter.Controllers[0].FirmwarePackageVersion
		}

		var macAddresses []string
		if adapter.NetworkDeviceFunctions.ODataID != "" {
			functionURIs, err := c.fetchCollectionMemberURIs(ctx, adapter.NetworkDeviceFunctions.ODataID)
			if err != nil {
				fmt.Printf("Warning: failed to fetch network device functions for %s: %v\n", adapterURI, err)
			}
			for _, functionURI := range functionURIs {
				functionBody, _, err := c.getJSON(ctx, c.resolveURI(functionURI))
				if err != nil {
					fmt.Printf("Warning: failed to fetch network device function %s: %v\n", functionURI, err)
					macAddresses = append(macAddresses, "")
					continue
				}
				var function redfishNetworkDeviceFunction
				if err := json.Unmarshal(functionBody, &function); err != nil {
					fmt.Printf("Warning: failed to parse network device function %s: %v\n", functionURI, err)
					macAddresses = append(macAddresses, "")
					continue
				}
				macAddresses = append(macAddresses, function.Ethernet.MACAddress)
			}
		}

		var linkStatuses []string
		var speeds []string
		if adapter.Ports.ODataID != "" {
			// Current Redfish "Port" schema: speed is reported in Gbps.
			portURIs, err := c.fetchCollectionMemberURIs(ctx, adapter.Ports.ODataID)
			if err != nil {
				fmt.Printf("Warning: failed to fetch ports for %s: %v\n", adapterURI, err)
			}
			for _, portURI := range portURIs {
				portBody, _, err := c.getJSON(ctx, c.resolveURI(portURI))
				if err != nil {
					fmt.Printf("Warning: failed to fetch port %s: %v\n", portURI, err)
					linkStatuses = append(linkStatuses, "")
					speeds = append(speeds, "")
					continue
				}
				var port redfishNetworkPort
				if err := json.Unmarshal(portBody, &port); err != nil {
					fmt.Printf("Warning: failed to parse port %s: %v\n", portURI, err)
					linkStatuses = append(linkStatuses, "")
					speeds = append(speeds, "")
					continue
				}
				linkStatuses = append(linkStatuses, port.LinkStatus)
				speed := ""
				if port.CurrentSpeedGbps > 0 {
					speed = formatIntIfPositive(int(port.CurrentSpeedGbps*1000+0.5), "Mbps")
				}
				speeds = append(speeds, speed)
			}
		} else if adapter.NetworkPorts.ODataID != "" {
			// Older Redfish "NetworkPort" schema: speed is already in Mbps,
			// and the MAC address is reported directly on the port.
			portURIs, err := c.fetchCollectionMemberURIs(ctx, adapter.NetworkPorts.ODataID)
			if err != nil {
				fmt.Printf("Warning: failed to fetch network ports for %s: %v\n", adapterURI, err)
			}
			for index, portURI := range portURIs {
				portBody, _, err := c.getJSON(ctx, c.resolveURI(portURI))
				if err != nil {
					fmt.Printf("Warning: failed to fetch network port %s: %v\n", portURI, err)
					linkStatuses = append(linkStatuses, "")
					speeds = append(speeds, "")
					continue
				}
				var port redfishNetworkPortLegacy
				if err := json.Unmarshal(portBody, &port); err != nil {
					fmt.Printf("Warning: failed to parse network port %s: %v\n", portURI, err)
					linkStatuses = append(linkStatuses, "")
					speeds = append(speeds, "")
					continue
				}
				linkStatuses = append(linkStatuses, port.LinkStatus)
				speeds = append(speeds, formatIntIfPositive(port.CurrentLinkSpeedMbps, "Mbps"))
				if len(port.AssociatedNetworkAddresses) > 0 {
					for len(macAddresses) <= index {
						macAddresses = append(macAddresses, "")
					}
					if macAddresses[index] == "" {
						macAddresses[index] = port.AssociatedNetworkAddresses[0]
					}
				}
			}
		}

		portCount := len(linkStatuses)
		if len(macAddresses) > portCount {
			portCount = len(macAddresses)
		}
		if portCount == 0 {
			// No per-port data at all; still show the adapter itself.
			rows = append(rows, []string{name, adapter.Manufacturer, adapter.Model, adapter.PartNumber, adapter.SerialNumber, firmware, "", "", "", "", adapter.Status.Health})
			continue
		}

		for index := 0; index < portCount; index++ {
			mac := ""
			if index < len(macAddresses) {
				mac = macAddresses[index]
			}
			linkStatus := ""
			speed := ""
			if index < len(linkStatuses) {
				linkStatus = linkStatuses[index]
			}
			if index < len(speeds) {
				speed = speeds[index]
			}
			portLabel := fmt.Sprintf("%d", index+1)

			// Adapter-level identity columns are only shown on the first
			// port row to avoid needless repetition-driven noise.
			adapterName, manufacturer, model, partNumber, serialNumber, fw, health := "", "", "", "", "", "", ""
			if index == 0 {
				adapterName, manufacturer, model, partNumber, serialNumber, fw, health = name, adapter.Manufacturer, adapter.Model, adapter.PartNumber, adapter.SerialNumber, firmware, adapter.Status.Health
			}

			rows = append(rows, []string{adapterName, manufacturer, model, partNumber, serialNumber, fw, portLabel, mac, speed, linkStatus, health})
		}
	}

	printDynamicTable(headers, rows)
	return nil
}

func (c *ILOClient) printEthernetInterfaces(ctx context.Context) error {
	uris, err := c.fetchCollectionMemberURIs(ctx, "/redfish/v1/Systems/1/EthernetInterfaces")
	if err != nil {
		return err
	}

	headers := []string{"Name", "MACAddress", "Speed", "LinkStatus", "FullDuplex", "Health"}
	var rows [][]string

	for _, uri := range uris {
		body, _, err := c.getJSON(ctx, c.resolveURI(uri))
		if err != nil {
			fmt.Printf("Warning: failed to fetch ethernet interface %s: %v\n", uri, err)
			continue
		}
		var iface redfishEthernetInterface
		if err := json.Unmarshal(body, &iface); err != nil {
			fmt.Printf("Warning: failed to parse ethernet interface %s: %v\n", uri, err)
			continue
		}

		name := iface.Name
		if name == "" {
			name = iface.Id
		}
		fullDuplex := ""
		if iface.FullDuplex != nil {
			fullDuplex = strconv.FormatBool(*iface.FullDuplex)
		}

		rows = append(rows, []string{
			name,
			iface.MACAddress,
			formatIntIfPositive(iface.SpeedMbps, "Mbps"),
			iface.LinkStatus,
			fullDuplex,
			iface.Status.Health,
		})
	}

	printDynamicTable(headers, rows)
	return nil
}

// ---------------------------------------------------------------------------
// Power Supply
// ---------------------------------------------------------------------------

type redfishPowerSupply struct {
	Id                 string `json:"Id"`
	MemberId           string `json:"MemberId"`
	Name               string `json:"Name"`
	Manufacturer       string `json:"Manufacturer"`
	Model              string `json:"Model"`
	PartNumber         string `json:"PartNumber"`
	SparePartNumber    string `json:"SparePartNumber"`
	SerialNumber       string `json:"SerialNumber"`
	FirmwareVersion    string `json:"FirmwareVersion"`
	PowerCapacityWatts int    `json:"PowerCapacityWatts"`
	Status             struct {
		Health string `json:"Health"`
		State  string `json:"State"`
	} `json:"Status"`
}

// printPowerSupplies primarily reads the legacy Redfish "Power" singleton
// (Chassis/1/Power), which is the schema actually advertised by HPE iLO
// across generations (confirmed against the official HPE Gen12 mockup: no
// HPE box advertises the newer PowerSubsystem/PowerSupplies schema). The
// PowerSupplies here are embedded objects, not separate resources to fetch.
// PowerSubsystem is tried as a best-effort secondary source and is silently
// skipped on failure since it is not expected to exist on HPE hardware.
func (c *ILOClient) printPowerSupplies(ctx context.Context) error {
	headers := []string{"Name", "Manufacturer", "Model", "PartNumber", "SerialNumber", "Firmware", "Capacity", "Health"}
	var rows [][]string
	var primaryErr error

	body, _, err := c.getJSON(ctx, c.resolveURI("/redfish/v1/Chassis/1/Power"))
	if err != nil {
		primaryErr = err
	} else {
		var power struct {
			PowerSupplies []redfishPowerSupply `json:"PowerSupplies"`
		}
		if unmarshalErr := json.Unmarshal(body, &power); unmarshalErr != nil {
			primaryErr = fmt.Errorf("parse Chassis/1/Power: %w", unmarshalErr)
		} else {
			rows = powerSupplyRows(power.PowerSupplies)
		}
	}

	if primaryErr != nil {
		if psuRows, subsystemErr := c.fetchPowerSubsystemSupplies(ctx); subsystemErr == nil {
			rows = psuRows
			primaryErr = nil
		}
	}

	if len(rows) == 0 && primaryErr != nil {
		return primaryErr
	}

	printDynamicTable(headers, rows)
	return nil
}

func powerSupplyRows(supplies []redfishPowerSupply) [][]string {
	var rows [][]string
	for _, psu := range supplies {
		if isAbsent(psu.Status.State) {
			continue
		}

		name := psu.Name
		if name == "" {
			name = psu.Id
		}
		if name == "" {
			name = psu.MemberId
		}
		partNumber := psu.PartNumber
		if partNumber == "" {
			partNumber = psu.SparePartNumber
		}

		rows = append(rows, []string{
			name,
			psu.Manufacturer,
			psu.Model,
			partNumber,
			psu.SerialNumber,
			psu.FirmwareVersion,
			formatIntIfPositive(psu.PowerCapacityWatts, "W"),
			psu.Status.Health,
		})
	}
	return rows
}

// fetchPowerSubsystemSupplies is a best-effort fallback for non-HPE Redfish
// targets that only implement the newer PowerSubsystem/PowerSupplies schema.
func (c *ILOClient) fetchPowerSubsystemSupplies(ctx context.Context) ([][]string, error) {
	body, _, err := c.getJSON(ctx, c.resolveURI("/redfish/v1/Chassis/1/PowerSubsystem"))
	if err != nil {
		return nil, fmt.Errorf("fetch PowerSubsystem: %w", err)
	}
	var subsystem struct {
		PowerSupplies storageReference `json:"PowerSupplies"`
	}
	if err := json.Unmarshal(body, &subsystem); err != nil {
		return nil, fmt.Errorf("parse PowerSubsystem: %w", err)
	}
	if subsystem.PowerSupplies.ODataID == "" {
		return nil, fmt.Errorf("PowerSubsystem does not advertise a PowerSupplies collection")
	}

	uris, err := c.fetchCollectionMemberURIs(ctx, subsystem.PowerSupplies.ODataID)
	if err != nil {
		return nil, err
	}

	var supplies []redfishPowerSupply
	for _, uri := range uris {
		psuBody, _, err := c.getJSON(ctx, c.resolveURI(uri))
		if err != nil {
			fmt.Printf("Warning: failed to fetch power supply %s: %v\n", uri, err)
			continue
		}
		var psu redfishPowerSupply
		if err := json.Unmarshal(psuBody, &psu); err != nil {
			fmt.Printf("Warning: failed to parse power supply %s: %v\n", uri, err)
			continue
		}
		supplies = append(supplies, psu)
	}
	return powerSupplyRows(supplies), nil
}
