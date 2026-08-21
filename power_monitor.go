package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type redfishPowerAction struct {
	Target     string   `json:"target"`
	ResetTypes []string `json:"ResetType@Redfish.AllowableValues"`
	PushTypes  []string `json:"PushType@Redfish.AllowableValues"`
}

type computerSystemActions struct {
	Reset redfishPowerAction `json:"#ComputerSystem.Reset"`
	Oem   struct {
		PowerButton redfishPowerAction `json:"#HpeComputerSystemExt.PowerButton"`
		SystemReset redfishPowerAction `json:"#HpeComputerSystemExt.SystemReset"`
		// Some older iLO versions wrap the OEM actions in Oem.Hpe.Actions.
		HPE struct {
			Actions struct {
				PowerButton redfishPowerAction `json:"#HpeComputerSystemExt.PowerButton"`
				SystemReset redfishPowerAction `json:"#HpeComputerSystemExt.SystemReset"`
			} `json:"Actions"`
		} `json:"Hpe"`
	} `json:"Oem"`
}

func (c *ILOClient) FetchPowerState(ctx context.Context) (string, error) {
	body, _, err := c.getJSON(ctx, c.BaseURL+"/Systems/1/")
	if err != nil {
		return "", err
	}
	var system struct {
		PowerState string `json:"PowerState"`
	}
	if err := json.Unmarshal(body, &system); err != nil {
		return "", fmt.Errorf("parse power state: %w", err)
	}
	if system.PowerState == "" {
		return "", fmt.Errorf("PowerState is missing from ComputerSystem response")
	}
	return system.PowerState, nil
}

func containsValue(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(value, wanted) {
			return true
		}
	}
	return false
}

func firstAdvertised(values []string, candidates ...string) string {
	for _, candidate := range candidates {
		if containsValue(values, candidate) {
			return candidate
		}
	}
	return ""
}

func (actions computerSystemActions) powerButton() redfishPowerAction {
	if actions.Oem.PowerButton.Target != "" {
		return actions.Oem.PowerButton
	}
	return actions.Oem.HPE.Actions.PowerButton
}

func (actions computerSystemActions) systemReset() redfishPowerAction {
	if actions.Oem.SystemReset.Target != "" {
		return actions.Oem.SystemReset
	}
	return actions.Oem.HPE.Actions.SystemReset
}

func describePowerActions(actions computerSystemActions) string {
	powerButton := actions.powerButton()
	systemReset := actions.systemReset()
	return fmt.Sprintf("standard reset target=%q types=%v; HPE power button target=%q types=%v; HPE system reset target=%q types=%v",
		actions.Reset.Target, actions.Reset.ResetTypes,
		powerButton.Target, powerButton.PushTypes,
		systemReset.Target, systemReset.ResetTypes)
}

func (c *ILOClient) fetchComputerSystemActions(ctx context.Context) (computerSystemActions, error) {
	body, _, err := c.getJSON(ctx, c.BaseURL+"/Systems/1/")
	if err != nil {
		return computerSystemActions{}, err
	}
	var system struct {
		Actions computerSystemActions `json:"Actions"`
	}
	if err := json.Unmarshal(body, &system); err != nil {
		return computerSystemActions{}, fmt.Errorf("parse ComputerSystem actions: %w", err)
	}
	actions := system.Actions
	return actions, nil
}

func (c *ILOClient) postPowerAction(ctx context.Context, target string, payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal power action: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolveURI(target), strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("create power action request: %w", err)
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
		return fmt.Errorf("power action failed with HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *ILOClient) PowerControlDetected(ctx context.Context, action string) error {
	actions, err := c.fetchComputerSystemActions(ctx)
	if err != nil {
		return err
	}

	var target string
	var payload interface{}
	powerButton := actions.powerButton()
	systemReset := actions.systemReset()
	switch strings.ToLower(action) {
	case "on", "off":
		if strings.EqualFold(action, "on") {
			if resetType := firstAdvertised(actions.Reset.ResetTypes, "On"); actions.Reset.Target != "" && resetType != "" {
				target = actions.Reset.Target
				payload = map[string]string{"ResetType": resetType}
			} else if powerButton.Target != "" {
				pushType := firstAdvertised(powerButton.PushTypes, "Press")
				if pushType == "" && len(powerButton.PushTypes) == 0 {
					pushType = "Press"
				}
				if pushType != "" {
					target = powerButton.Target
					payload = map[string]string{"PushType": pushType}
				}
			}
		} else {
			if resetType := firstAdvertised(actions.Reset.ResetTypes, "ForceOff", "GracefulShutdown", "PushPowerButton", "Off"); actions.Reset.Target != "" && resetType != "" {
				target = actions.Reset.Target
				payload = map[string]string{"ResetType": resetType}
			} else if powerButton.Target != "" {
				pushType := firstAdvertised(powerButton.PushTypes, "PressAndHold")
				if pushType == "" && len(powerButton.PushTypes) == 0 {
					pushType = "PressAndHold"
				}
				if pushType != "" {
					target = powerButton.Target
					payload = map[string]string{"PushType": pushType}
				}
			}
		}
	case "reset":
		if actions.Reset.Target != "" && containsValue(actions.Reset.ResetTypes, "ForceRestart") {
			target = actions.Reset.Target
			payload = map[string]string{"ResetType": "ForceRestart"}
		} else if systemReset.Target != "" && containsValue(systemReset.ResetTypes, "ColdBoot") {
			target = systemReset.Target
			payload = map[string]string{"ResetType": "ColdBoot"}
		}
	default:
		return fmt.Errorf("invalid power action %q; use on, off, reset, status, or monitor", action)
	}
	if target == "" {
		return fmt.Errorf("iLO does not advertise a compatible %s power action (%s)", action, describePowerActions(actions))
	}
	if err := c.postPowerAction(ctx, target, payload); err != nil {
		return err
	}
	fmt.Printf("Power %s command sent successfully\n", action)
	return nil
}

func (c *ILOClient) MonitorPower(ctx context.Context) error {
	fmt.Println("Monitoring power status every 5 seconds. Press Ctrl+C to stop.")
	for {
		state, err := c.FetchPowerState(ctx)
		if err != nil && isSessionError(err) {
			fmt.Printf("Power status connection lost: %v; reconnecting...\n", err)
			if reconnectErr := c.Reconnect(); reconnectErr != nil {
				fmt.Printf("Reconnect failed: %v\n", reconnectErr)
			}
		} else if err != nil {
			fmt.Printf("Power status error: %v\n", err)
		} else {
			fmt.Printf("\rPowerState: %-12s", state)
		}
		if err := sleepWithContext(ctx, monitorInterval); err != nil {
			fmt.Println()
			return nil
		}
	}
}
