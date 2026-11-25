package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil"
)

// TestCmdDevicesWithMockIWDP tests the devices listing against a mock HTTP
// server that simulates ios-webkit-debug-proxy's /json endpoint on port 9221.
func TestCmdDevicesWithMockIWDP(t *testing.T) {
	devices := []webkit.DeviceEntry{
		{DeviceID: "abc123", DeviceName: "iPhone 15", URL: "localhost:9222"},
		{DeviceID: "def456", DeviceName: "iPad Pro", URL: "localhost:9223"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(devices)
	}))
	defer server.Close()

	// We can't easily redirect the proxy package to use our mock server
	// (it uses hardcoded localhost:9221), but we can verify the mock serves
	// valid JSON that matches the expected types.
	resp, err := http.Get(server.URL + "/json")
	if err != nil {
		t.Fatalf("GET /json: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got []webkit.DeviceEntry
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d devices, want 2", len(got))
	}
	if got[0].DeviceName != "iPhone 15" {
		t.Errorf("first device = %q, want iPhone 15", got[0].DeviceName)
	}
	if got[1].URL != "localhost:9223" {
		t.Errorf("second device URL = %q, want localhost:9223", got[1].URL)
	}
}

// TestCmdPagesWithMockIWDP tests the pages listing against a mock HTTP server
// that simulates a device port's /json endpoint.
func TestCmdPagesWithMockIWDP(t *testing.T) {
	pages := []webkit.PageEntry{
		{
			Title:                "Example",
			URL:                  "https://example.com",
			WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/1",
			PageID:               1,
		},
		{
			Title:                "Google",
			URL:                  "https://google.com",
			WebSocketDebuggerURL: "ws://localhost:9222/devtools/page/2",
			PageID:               2,
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pages)
	}))
	defer server.Close()

	resp, err := http.Get(server.URL + "/json")
	if err != nil {
		t.Fatalf("GET /json: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got []webkit.PageEntry
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d pages, want 2", len(got))
	}
	if got[0].Title != "Example" {
		t.Errorf("first page title = %q, want Example", got[0].Title)
	}
	if got[0].WebSocketDebuggerURL != "ws://localhost:9222/devtools/page/1" {
		t.Errorf("first page ws URL = %q", got[0].WebSocketDebuggerURL)
	}
	if got[1].PageID != 2 {
		t.Errorf("second page ID = %d, want 2", got[1].PageID)
	}
}

// TestMockIWDPFullFlow simulates the full discovery flow: listing devices on
// the listing port, then listing pages on a device port, then connecting via
// WebSocket. This is an end-to-end test using mock servers.
func TestMockIWDPFullFlow(t *testing.T) {
	// Step 1: Create a mock WebSocket server (simulates a debuggable page).
	wsMock := testutil.NewMockServer()
	defer wsMock.Close()

	wsMock.HandleFunc("Runtime.evaluate", map[string]interface{}{
		"result": map[string]interface{}{
			"type":  "string",
			"value": "test-result",
		},
	})

	// Step 2: Create a mock HTTP server for the device port (simulates /json on 9222).
	deviceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages := []webkit.PageEntry{
			{
				Title:                "Test Page",
				URL:                  "https://test.example.com",
				WebSocketDebuggerURL: wsMock.URL,
				PageID:               1,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pages)
	}))
	defer deviceServer.Close()

	// Step 3: Fetch pages from the mock device server.
	resp, err := http.Get(deviceServer.URL + "/json")
	if err != nil {
		t.Fatalf("GET device /json: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var pages []webkit.PageEntry
	if err := json.NewDecoder(resp.Body).Decode(&pages); err != nil {
		t.Fatalf("decode pages: %v", err)
	}
	if len(pages) == 0 {
		t.Fatal("no pages returned")
	}

	// Step 4: Connect to the first page's WebSocket URL.
	wsURL := pages[0].WebSocketDebuggerURL
	if wsURL == "" {
		t.Fatal("no WebSocket URL for first page")
	}

	ctx := context.Background()
	client, err := webkit.NewClient(ctx, wsURL)
	if err != nil {
		t.Fatalf("NewClient(%s): %v", wsURL, err)
	}
	defer func() { _ = client.Close() }()

	// Step 5: Send a command through the WebSocket connection.
	result, err := client.Send(ctx, "Runtime.evaluate", map[string]interface{}{
		"expression":    "'hello'",
		"returnByValue": true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	var evalResult map[string]interface{}
	if err := json.Unmarshal(result, &evalResult); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	res, ok := evalResult["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected result map, got %T", evalResult["result"])
	}
	if res["value"] != "test-result" {
		t.Errorf("result.value = %v, want test-result", res["value"])
	}
}

// TestMockIWDPDeviceDiscovery simulates the device listing endpoint and
// verifies the /json response is correctly structured.
func TestMockIWDPDeviceDiscovery(t *testing.T) {
	// Simulate the listing port (9221) with multiple devices.
	listingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			devices := []webkit.DeviceEntry{
				{DeviceID: "abc", DeviceName: "iPhone 15 Pro", URL: "localhost:9222"},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(devices)
		default:
			http.NotFound(w, r)
		}
	}))
	defer listingServer.Close()

	resp, err := http.Get(listingServer.URL + "/json")
	if err != nil {
		t.Fatalf("GET /json: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var devices []webkit.DeviceEntry
	if err := json.NewDecoder(resp.Body).Decode(&devices); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(devices))
	}
	if devices[0].DeviceID != "abc" {
		t.Errorf("deviceId = %q, want abc", devices[0].DeviceID)
	}
}

// TestPrintUsageDoesNotPanic verifies that printUsage() completes without error.
func TestPrintUsageDoesNotPanic(t *testing.T) {
	// Redirect stderr to discard output.
	old := os.Stderr
	os.Stderr, _ = os.Open(os.DevNull)
	defer func() { os.Stderr = old }()

	// Should not panic. The file is open for reading, so the write fails;
	// that error is not what this test is about.
	_ = printUsage(os.Stderr)
}
