package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// chassisInventory stores raw resources internally. Output is always built
// through the compact report functions below, never by dumping Redfish JSON.
type chassisInventory struct {
	Chassis        []map[string]interface{}
	PCIeDevices    []map[string]interface{}
	PCIeSlots      []map[string]interface{}
	BackplaneSlots []map[string]interface{}
	Drives         []map[string]interface{}
}

func (c *ILOClient) fetchChassisInventory(ctx context.Context) (*chassisInventory, error) {
	chassisURIs, err := c.fetchCollectionMemberURIs(ctx, "/redfish/v1/Chassis")
	if err != nil {
		return nil, err
	}

	result := &chassisInventory{}
	for _, chassisURI := range chassisURIs {
		chassis, err := c.fetchJSONObject(ctx, chassisURI)
		if err != nil {
			fmt.Printf("Warning: failed to fetch chassis %s: %v\n", chassisURI, err)
			continue
		}
		chassisID := stringValue(chassis["Id"])
		if chassisID == "" {
			chassisID = lastURIComponent(chassisURI)
		}
		chassis["Chassis"] = chassisID
		result.Chassis = append(result.Chassis, chassis)

		for _, resource := range []struct {
			name  string
			items *[]map[string]interface{}
		}{
			{"PCIeDevices", &result.PCIeDevices},
			{"PCIeSlots", &result.PCIeSlots},
			{"Drives", &result.Drives},
		} {
			collectionURI := strings.TrimRight(chassisURI, "/") + "/" + resource.name
			members, collectionErr := c.fetchCollectionMemberURIs(ctx, collectionURI)
			if collectionErr != nil {
				fmt.Printf("Warning: %s unavailable for chassis %s: %v\n", resource.name, chassisID, collectionErr)
				continue
			}
			for _, memberURI := range members {
				item, itemErr := c.fetchJSONObject(ctx, memberURI)
				if itemErr != nil {
					fmt.Printf("Warning: failed to fetch %s member %s: %v\n", resource.name, memberURI, itemErr)
					continue
				}
				item["Chassis"] = chassisID
				*resource.items = append(*resource.items, item)
				if resource.name == "PCIeSlots" && strings.EqualFold(partLocationType(item), "Backplane") {
					result.BackplaneSlots = append(result.BackplaneSlots, item)
				}
			}
		}
	}
	return result, nil
}

func (c *ILOClient) fetchJSONObject(ctx context.Context, resourceURI string) (map[string]interface{}, error) {
	body, _, err := c.getJSON(ctx, c.resolveURI(resourceURI))
	if err != nil {
		return nil, err
	}
	var object map[string]interface{}
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, fmt.Errorf("parse resource %s: %w", resourceURI, err)
	}
	return object, nil
}

func stringValue(value interface{}) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func intValue(value interface{}) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	}
	return 0
}

func lastURIComponent(uri string) string {
	parts := strings.Split(strings.Trim(uri, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func locationParts(object map[string]interface{}) map[string]interface{} {
	location, _ := object["Location"].(map[string]interface{})
	if location == nil {
		location, _ = object["PhysicalLocation"].(map[string]interface{})
	}
	part, _ := location["PartLocation"].(map[string]interface{})
	return part
}

func partLocationType(object map[string]interface{}) string {
	return stringValue(locationParts(object)["LocationType"])
}

func locationLabel(object map[string]interface{}) string {
	return stringValue(locationParts(object)["ServiceLabel"])
}

func locationOrdinal(object map[string]interface{}) int {
	return intValue(locationParts(object)["LocationOrdinalValue"])
}

func statusValue(object map[string]interface{}, field string) string {
	status, _ := object["Status"].(map[string]interface{})
	return stringValue(status[field])
}

func locationKey(object map[string]interface{}) string {
	if ordinal := locationOrdinal(object); ordinal > 0 {
		return fmt.Sprintf("ordinal:%d", ordinal)
	}
	if label := locationLabel(object); label != "" {
		return "label:" + label
	}
	return ""
}

func deviceForSlot(slot map[string]interface{}, devices []map[string]interface{}) map[string]interface{} {
	slotKey := locationKey(slot)
	if slotKey == "" {
		return nil
	}
	for _, device := range devices {
		if locationKey(device) == slotKey {
			return device
		}
	}
	return nil
}

func compactChassis(object map[string]interface{}) map[string]interface{} {
	result := compactFields(object, []string{
		"Chassis", "LocationType", "ServiceLabel", "Model", "Name",
		"PartNumber", "SerialNumber", "Health",
	})
	result["LocationType"] = partLocationType(object)
	result["ServiceLabel"] = locationLabel(object)
	result["Health"] = statusValue(object, "Health")
	return result
}

func compactPCIeDevice(device map[string]interface{}) map[string]interface{} {
	result := compactFields(device, []string{
		"Chassis", "Slot", "ServiceLabel", "Model", "Name", "Manufacturer",
		"PartNumber", "SerialNumber", "Health",
	})
	if _, ok := result["Slot"]; !ok {
		if ordinal := locationOrdinal(device); ordinal > 0 {
			result["Slot"] = ordinal
		} else if id := stringValue(device["Id"]); id != "" {
			result["Slot"] = id
		}
	}
	if _, ok := result["ServiceLabel"]; !ok {
		result["ServiceLabel"] = locationLabel(device)
	}
	result["Health"] = statusValue(device, "Health")
	return result
}

func compactSlot(slot map[string]interface{}, devices []map[string]interface{}) map[string]interface{} {
	result := compactFields(slot, []string{
		"Chassis", "Slot", "ServiceLabel", "SlotType", "Lanes", "HotPluggable",
		"Health", "State",
	})
	if _, ok := result["Slot"]; !ok {
		if ordinal := locationOrdinal(slot); ordinal > 0 {
			result["Slot"] = ordinal
		} else {
			result["Slot"] = stringValue(slot["Id"])
		}
	}
	if _, ok := result["ServiceLabel"]; !ok {
		result["ServiceLabel"] = locationLabel(slot)
	}
	result["LocationType"] = partLocationType(slot)
	result["Lanes"] = intValue(slot["Lanes"])
	result["HotPluggable"] = slot["HotPluggable"]
	result["Health"] = statusValue(slot, "Health")
	result["State"] = statusValue(slot, "State")
	if device := deviceForSlot(slot, devices); device != nil {
		result["Model"] = stringValue(device["Model"])
		result["Name"] = stringValue(device["Name"])
	}
	return result
}

func compactDrive(drive map[string]interface{}) map[string]interface{} {
	result := compactFields(drive, []string{
		"Chassis", "Bay", "ServiceLabel", "Manufacturer", "Model", "Name",
		"SerialNumber", "Protocol", "MediaType", "CapacityBytes", "Health", "State",
	})
	if _, ok := result["Bay"]; !ok {
		if ordinal := locationOrdinal(drive); ordinal > 0 {
			result["Bay"] = ordinal
		}
	}
	if _, ok := result["ServiceLabel"]; !ok {
		result["ServiceLabel"] = locationLabel(drive)
	}
	result["Health"] = statusValue(drive, "Health")
	result["State"] = statusValue(drive, "State")
	return result
}

func compactFields(object map[string]interface{}, fields []string) map[string]interface{} {
	result := make(map[string]interface{}, len(fields))
	for _, field := range fields {
		if value, ok := object[field]; ok && value != nil {
			if text, isText := value.(string); isText && text == "" {
				continue
			}
			result[field] = value
		}
	}
	return result
}

func compactChassisObjects(inventory *chassisInventory) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(inventory.Chassis))
	for _, item := range inventory.Chassis {
		result = append(result, compactChassis(item))
	}
	return result
}

func compactPCIeDeviceObjects(inventory *chassisInventory) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(inventory.PCIeDevices))
	for _, item := range inventory.PCIeDevices {
		result = append(result, compactPCIeDevice(item))
	}
	return result
}

func compactSlotObjects(slots, devices []map[string]interface{}) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(slots))
	for _, item := range slots {
		result = append(result, compactSlot(item, devices))
	}
	return result
}

func compactDriveObjects(inventory *chassisInventory) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(inventory.Drives))
	for _, item := range inventory.Drives {
		result = append(result, compactDrive(item))
	}
	return result
}

func compactBackplaneRows(inventory *chassisInventory) [][]string {
	if inventory == nil {
		return nil
	}
	rows := make([][]string, 0, len(inventory.Chassis)+len(inventory.BackplaneSlots))
	seen := make(map[string]struct{})
	appendRow := func(values []string) {
		value := joinHardwareValues(values)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		rows = append(rows, []string{"", "BP", value})
	}

	// Backplane is commonly reported on the Chassis resource itself. Do not
	// assume a chassis number; retain every discovered Backplane location.
	for _, chassis := range inventory.Chassis {
		if !strings.EqualFold(partLocationType(chassis), "Backplane") {
			continue
		}
		appendRow([]string{
			locationLabel(chassis),
			stringValue(chassis["Name"]),
			stringValue(chassis["PartNumber"]),
			stringValue(chassis["SerialNumber"]),
		})
	}

	// Some firmware exposes additional Backplane locations under PCIeSlots.
	for _, slot := range inventory.BackplaneSlots {
		device := deviceForSlot(slot, inventory.PCIeDevices)
		values := []string{locationLabel(slot)}
		if device != nil {
			values = append(values,
				stringValue(device["Name"]),
				stringValue(device["PartNumber"]),
				stringValue(device["SerialNumber"]),
			)
		} else {
			values = append(values,
				stringValue(slot["Name"]),
				stringValue(slot["PartNumber"]),
				stringValue(slot["SerialNumber"]),
			)
		}
		appendRow(values)
	}
	return rows
}

func compactDriveRows(inventory *chassisInventory) [][]string {
	if inventory == nil {
		return nil
	}
	rows := make([][]string, 0, len(inventory.Drives))
	for _, drive := range inventory.Drives {
		rows = append(rows, []string{"", "Drives", joinHardwareValues([]string{
			stringValue(drive["Manufacturer"]),
			stringValue(drive["Model"]),
			stringValue(drive["Name"]),
			stringValue(drive["SerialNumber"]),
		})})
	}
	return rows
}

func joinHardwareValues(values []string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			parts = append(parts, replaceCommas(value))
		}
	}
	return strings.Join(parts, " / ")
}

func printCompactTable(title string, headers []string, objects []map[string]interface{}) {
	if len(objects) == 0 {
		return
	}
	rows := make([][]string, 0, len(objects))
	for _, object := range objects {
		row := make([]string, len(headers))
		for index, header := range headers {
			value, ok := object[header]
			if !ok || value == nil {
				continue
			}
			if text, isText := value.(string); isText {
				row[index] = text
			} else {
				row[index] = fmt.Sprint(value)
			}
		}
		rows = append(rows, row)
	}
	fmt.Printf("##### %s #####\n", title)
	printDynamicTable(headers, rows)
}

func printChassisInventory(inventory *chassisInventory) {
	printCompactTable("Chassis", []string{"Chassis", "LocationType", "ServiceLabel", "Model", "Name", "PartNumber", "SerialNumber", "Health"}, compactChassisObjects(inventory))
	printCompactTable("PCIeDevices", []string{"Chassis", "Slot", "ServiceLabel", "Model", "Name", "Manufacturer", "PartNumber", "SerialNumber", "Health"}, compactPCIeDeviceObjects(inventory))
	printCompactTable("PCIeSlots", []string{"Chassis", "Slot", "ServiceLabel", "SlotType", "Lanes", "HotPluggable", "Model", "Name", "Health", "State"}, compactSlotObjects(inventory.PCIeSlots, inventory.PCIeDevices))
	printCompactTable("Backplane Drives", []string{"Chassis", "Bay", "ServiceLabel", "Manufacturer", "Model", "Name", "SerialNumber", "Protocol", "MediaType", "CapacityBytes", "Health", "State"}, compactDriveObjects(inventory))
}
