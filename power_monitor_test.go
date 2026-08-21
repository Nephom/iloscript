package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPowerControlDetectedUsesHPEPowerButton(t *testing.T) {
	var payload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/redfish/v1/Systems/1/":
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{
				"Actions": map[string]interface{}{
					"Oem": map[string]interface{}{
						"#HpeComputerSystemExt.PowerButton": map[string]interface{}{
							"target":                           "/redfish/v1/Systems/1/Actions/Oem/Hpe/PowerButton",
							"PushType@Redfish.AllowableValues": []string{"Press", "PressAndHold"},
						},
					},
				},
			})
		case "/redfish/v1/Systems/1/Actions/Oem/Hpe/PowerButton":
			body, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(body, &payload)
			writer.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := testPowerClient(server.URL)
	if err := client.PowerControlDetected(context.Background(), "off"); err != nil {
		t.Fatalf("PowerControlDetected returned error: %v", err)
	}
	if payload["PushType"] != "PressAndHold" {
		t.Fatalf("payload PushType = %q, want PressAndHold", payload["PushType"])
	}
}

func TestPowerControlDetectedUsesStandardForceOff(t *testing.T) {
	var payload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/redfish/v1/Systems/1/":
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{
				"Actions": map[string]interface{}{
					"#ComputerSystem.Reset": map[string]interface{}{
						"target":                            "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
						"ResetType@Redfish.AllowableValues": []string{"On", "ForceOff", "ForceRestart"},
					},
				},
			})
		case "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset":
			body, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(body, &payload)
			writer.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	if err := testPowerClient(server.URL).PowerControlDetected(context.Background(), "off"); err != nil {
		t.Fatalf("PowerControlDetected returned error: %v", err)
	}
	if payload["ResetType"] != "ForceOff" {
		t.Fatalf("payload ResetType = %q, want ForceOff", payload["ResetType"])
	}
}

func TestPowerControlDetectedReportsDiscoveredCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]interface{}{
			"Actions": map[string]interface{}{
				"#ComputerSystem.Reset": map[string]interface{}{
					"target":                            "/reset",
					"ResetType@Redfish.AllowableValues": []string{"On", "ForceRestart"},
				},
			},
		})
	}))
	defer server.Close()

	err := testPowerClient(server.URL).PowerControlDetected(context.Background(), "off")
	if err == nil || !strings.Contains(err.Error(), `types=[On ForceRestart]`) {
		t.Fatalf("error = %v, want discovered allowable values", err)
	}
}

func testPowerClient(serverURL string) *ILOClient {
	return &ILOClient{
		BaseURL: serverURL + "/redfish/v1",
		Session: &http.Client{},
		Token:   "test-token",
	}
}
