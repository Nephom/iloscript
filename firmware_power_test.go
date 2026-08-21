package main

import (
	"bytes"
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

func TestParseFirmwareArgumentsTargetModes(t *testing.T) {
	tests := []struct {
		name       string
		arguments  []string
		targetKind string
	}{
		{name: "default bios", arguments: []string{"image.bin"}, targetKind: "bios"},
		{name: "auto", arguments: []string{"image.bin", "--target", "auto"}, targetKind: "auto"},
		{name: "manual", arguments: []string{"image.bin", "--target=manual"}, targetKind: "manual"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, targetKind, err := parseFirmwareArguments(test.arguments)
			if err != nil {
				t.Fatal(err)
			}
			if targetKind != test.targetKind {
				t.Fatalf("target kind = %q, want %q", targetKind, test.targetKind)
			}
		})
	}
}

func TestSelectManualFirmwareTargetUsesReturnedIDAndODataID(t *testing.T) {
	targets := []FirmwareInventoryTarget{{
		ID:          "returned-id-7",
		Name:        "Network Controller",
		Version:     "1.2.3",
		Description: "Adapter firmware",
		Updateable:  true,
		ODataID:     "/redfish/v1/UpdateService/FirmwareInventory/99",
	}}
	var output bytes.Buffer
	target, err := selectManualFirmwareTarget(targets, strings.NewReader("returned-id-7\n"), &output)
	if err != nil {
		t.Fatal(err)
	}
	if target.ODataID != targets[0].ODataID {
		t.Fatalf("odata id = %q, want %q", target.ODataID, targets[0].ODataID)
	}
	table := output.String()
	if strings.Index(table, "ID") > strings.Index(table, "Name") ||
		strings.Index(table, "Name") > strings.Index(table, "Version") ||
		strings.Index(table, "Version") > strings.Index(table, "Description") {
		t.Fatalf("manual table columns are out of order: %q", table)
	}
	if !strings.Contains(table, "returned-id-7") || !strings.Contains(table, "Adapter firmware") {
		t.Fatalf("manual table does not contain returned values: %q", output.String())
	}
}

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
		case "/redfish/v1/UpdateService/FirmwareInventory/":
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{"Members": []map[string]string{{"@odata.id": "/redfish/v1/UpdateService/FirmwareInventory/7"}}})
		case "/redfish/v1/UpdateService/FirmwareInventory/7":
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{"@odata.id": "/redfish/v1/UpdateService/FirmwareInventory/7", "Name": "System ROM", "Version": "old", "Updateable": true})
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
			if !strings.Contains(request.MultipartForm.Value["UpdateParameters"][0], "/FirmwareInventory/7") || len(request.MultipartForm.File["UpdateFile"]) != 1 {
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

	taskInfo, err := testClient(server.URL).UpdateFirmwareImageWithTarget(context.Background(), imagePath, "bios")
	if err != nil {
		t.Fatalf("UpdateFirmwareImage returned error: %v", err)
	}
	if taskInfo.TaskURI != "/redfish/v1/TaskService/Tasks/1" || taskInfo.TargetURI != "/redfish/v1/UpdateService/FirmwareInventory/7" {
		t.Fatalf("unexpected update info: %#v", taskInfo)
	}
}

func TestUpdateFirmwareImageMultipartAutoOmitsTargets(t *testing.T) {
	var updateParameters string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/redfish/v1/UpdateService/":
			_ = json.NewEncoder(writer).Encode(map[string]string{"MultipartHttpPushUri": "/redfish/v1/UpdateService/MultipartHttpPush"})
		case "/redfish/v1/UpdateService/MultipartHttpPush":
			if err := request.ParseMultipartForm(1024 * 1024); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			updateParameters = request.MultipartForm.Value["UpdateParameters"][0]
			writer.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	imagePath := filepath.Join(t.TempDir(), "firmware.bin")
	if err := os.WriteFile(imagePath, []byte("firmware-test-image"), 0600); err != nil {
		t.Fatal(err)
	}

	info, err := testClient(server.URL).UpdateFirmwareImageWithTarget(context.Background(), imagePath, "auto")
	if err != nil {
		t.Fatalf("UpdateFirmwareImageWithTarget returned error: %v", err)
	}
	if info.TargetURI != "" {
		t.Fatalf("auto target URI = %q, want empty", info.TargetURI)
	}
	var parameters map[string]interface{}
	if err := json.Unmarshal([]byte(updateParameters), &parameters); err != nil {
		t.Fatal(err)
	}
	if _, exists := parameters["Targets"]; exists {
		t.Fatalf("auto multipart parameters unexpectedly contain Targets: %s", updateParameters)
	}
}

func TestUpdateFirmwareWithTargetAutoOmitsTargets(t *testing.T) {
	var requestPayload map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/redfish/v1/UpdateService/":
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{
				"Actions": map[string]interface{}{
					"#UpdateService.SimpleUpdate": map[string]string{"target": "/redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate"},
				},
			})
		case "/redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate":
			body, _ := io.ReadAll(request.Body)
			if err := json.Unmarshal(body, &requestPayload); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			writer.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	info, err := testClient(server.URL).UpdateFirmwareWithTarget(context.Background(), "https://example.test/firmware.bin", "auto")
	if err != nil {
		t.Fatalf("UpdateFirmwareWithTarget returned error: %v", err)
	}
	if info.TargetURI != "" {
		t.Fatalf("auto target URI = %q, want empty", info.TargetURI)
	}
	if _, exists := requestPayload["Targets"]; exists {
		t.Fatalf("auto request unexpectedly contains Targets: %#v", requestPayload)
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

func TestTaskMonitorUpdateBadParameterUsesCanonicalTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/redfish/v1/TaskService/TaskMonitors/6/":
			http.Error(writer, `{"error":{"@Message.ExtendedInfo":[{"MessageId":"iLO.2.44.UpdateBadParameter"}]}}`, http.StatusBadRequest)
		case "/redfish/v1/TaskService/Tasks/6/":
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{
				"@odata.id":       "/redfish/v1/TaskService/Tasks/6/",
				"TaskState":       "Completed",
				"PercentComplete": 100,
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	status, progress, err := testClient(server.URL).GetTaskStatus("/redfish/v1/TaskService/TaskMonitors/6/")
	if err != nil {
		t.Fatalf("GetTaskStatus returned error: %v", err)
	}
	if status != "Complete" || progress != 100 {
		t.Fatalf("unexpected canonical task status: %s %d", status, progress)
	}
}

func TestSetRemoteServerCertificateVerification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPatch || request.URL.Path != "/redfish/v1/UpdateService/" {
			http.NotFound(writer, request)
			return
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != `{"VerifyRemoteServerCertificate":false}` {
			http.Error(writer, "unexpected payload", http.StatusBadRequest)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := testClient(server.URL).SetRemoteServerCertificateVerification(context.Background(), false); err != nil {
		t.Fatalf("SetRemoteServerCertificateVerification returned error: %v", err)
	}
}

func TestMonitorUpdateServiceCompletesWithoutTaskURI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/redfish/v1/UpdateService/" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]interface{}{
			"Oem": map[string]interface{}{"Hpe": map[string]interface{}{
				"State":                "Complete",
				"FlashProgressPercent": 100,
			}},
		})
	}))
	defer server.Close()

	if err := testClient(server.URL).MonitorUpdateService(context.Background(), 1); err != nil {
		t.Fatalf("MonitorUpdateService returned error: %v", err)
	}
}
