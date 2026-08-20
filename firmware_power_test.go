package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testClient(serverURL string) *ILOClient {
	return &ILOClient{
		BaseURL: serverURL + "/redfish/v1",
		Session: &http.Client{},
		Token:   "test-token",
	}
}

func TestUpdateFirmwareImageMultipart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/redfish/v1/UpdateService/":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"MultipartHttpPushUri":"/redfish/v1/UpdateService/MultipartHttpPush","Actions":{"#UpdateService.SimpleUpdate":{"target":"/redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate"}}}`))
		case "/redfish/v1/UpdateService/MultipartHttpPush":
			if !strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/") {
				http.Error(writer, "multipart content type required", http.StatusBadRequest)
				return
			}
			if err := request.ParseMultipartForm(1024 * 1024); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			if request.MultipartForm.Value["UpdateParameters"][0] == "" || len(request.MultipartForm.File["UpdateFile"]) != 1 {
				http.Error(writer, "missing multipart fields", http.StatusBadRequest)
				return
			}
			writer.Header().Set("Location", "/redfish/v1/TaskService/Tasks/1")
			writer.WriteHeader(http.StatusAccepted)
		default:
			http.Error(writer, "unexpected path: "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	imagePath := filepath.Join(t.TempDir(), "firmware.bin")
	if err := os.WriteFile(imagePath, []byte("firmware-test-image"), 0600); err != nil {
		t.Fatal(err)
	}

	taskURI, err := testClient(server.URL).UpdateFirmwareImage(context.Background(), imagePath)
	if err != nil {
		t.Fatalf("UpdateFirmwareImage returned error: %v", err)
	}
	if taskURI != "/redfish/v1/TaskService/Tasks/1" {
		t.Fatalf("unexpected task URI: %q", taskURI)
	}
}

func TestPowerStateAndStandardResetDiscovery(t *testing.T) {
	var receivedPayload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/redfish/v1/Systems/1/":
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{
				"PowerState": "On",
				"Actions": map[string]interface{}{
					"#ComputerSystem.Reset": map[string]interface{}{
						"target":                            "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
						"ResetType@Redfish.AllowableValues": []string{"On", "ForceRestart"},
					},
				},
			})
		case "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset":
			body, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(body, &receivedPayload)
			writer.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := testClient(server.URL)
	state, err := client.FetchPowerState(context.Background())
	if err != nil || state != "On" {
		t.Fatalf("FetchPowerState returned %q, %v", state, err)
	}
	if err := client.PowerControlDetected(context.Background(), "reset"); err != nil {
		t.Fatalf("PowerControlDetected returned error: %v", err)
	}
	if receivedPayload["ResetType"] != "ForceRestart" {
		t.Fatalf("unexpected reset payload: %#v", receivedPayload)
	}
}

func TestTaskMonitor400FallsBackToUpdateServiceState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/redfish/v1/TaskService/TaskMonitors/1":
			http.Error(writer, `{"error":"task monitor temporarily unavailable"}`, http.StatusBadRequest)
		case "/redfish/v1/UpdateService/":
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{
				"Oem": map[string]interface{}{"Hpe": map[string]interface{}{
					"State":                "InProgress",
					"FlashProgressPercent": 42,
				}},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	status, progress, err := testClient(server.URL).GetTaskStatus("/redfish/v1/TaskService/TaskMonitors/1")
	if err != nil {
		t.Fatalf("GetTaskStatus returned error: %v", err)
	}
	if status != "InProgress" || progress != 42 {
		t.Fatalf("unexpected fallback status: %s %d", status, progress)
	}
}
