package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/edxeth/agent-ios/internal/webkit"
)

func TestDevicePort(t *testing.T) {
	tests := []struct {
		name    string
		entry   webkit.DeviceEntry
		want    int
		wantErr bool
	}{
		{
			name:  "standard URL",
			entry: webkit.DeviceEntry{DeviceName: "iPhone", URL: "localhost:9222"},
			want:  9222,
		},
		{
			name:  "with scheme",
			entry: webkit.DeviceEntry{DeviceName: "iPhone", URL: "http://localhost:9222"},
			want:  9222,
		},
		{
			name:  "second device",
			entry: webkit.DeviceEntry{DeviceName: "iPad", URL: "localhost:9223"},
			want:  9223,
		},
		{
			name:    "empty URL",
			entry:   webkit.DeviceEntry{DeviceName: "Bad"},
			wantErr: true,
		},
		{
			name:    "URL without port",
			entry:   webkit.DeviceEntry{DeviceName: "iPhone", URL: "localhost"},
			wantErr: true,
		},
		{
			name:    "URL with scheme but no port",
			entry:   webkit.DeviceEntry{DeviceName: "iPhone", URL: "http://localhost"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DevicePort(tt.entry)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got port %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("DevicePort() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestListDevicesFromURL(t *testing.T) {
	devices := []webkit.DeviceEntry{
		{DeviceID: "abc123", DeviceName: "iPhone 15", URL: "localhost:9222"},
		{DeviceID: "def456", DeviceName: "iPad Pro", URL: "localhost:9223"},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(devices)
	}))
	defer ts.Close()

	got, err := listDevicesFromURL(context.Background(), ts.URL+"/json")
	if err != nil {
		t.Fatalf("listDevicesFromURL: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(got))
	}
	if got[0].DeviceName != "iPhone 15" {
		t.Errorf("device[0].DeviceName = %q, want %q", got[0].DeviceName, "iPhone 15")
	}
	if got[0].URL != "localhost:9222" {
		t.Errorf("device[0].URL = %q, want %q", got[0].URL, "localhost:9222")
	}
	if got[1].DeviceID != "def456" {
		t.Errorf("device[1].DeviceID = %q, want %q", got[1].DeviceID, "def456")
	}
}

func TestListDevicesFromURL_Empty(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]webkit.DeviceEntry{})
	}))
	defer ts.Close()

	got, err := listDevicesFromURL(context.Background(), ts.URL+"/json")
	if err != nil {
		t.Fatalf("listDevicesFromURL: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 devices, got %d", len(got))
	}
}

func TestListDevicesFromURL_ConnectionRefused(t *testing.T) {
	_, err := listDevicesFromURL(context.Background(), "http://localhost:1/json")
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestListDevicesFromURL_NonOKStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	_, err := listDevicesFromURL(context.Background(), ts.URL+"/json")
	if err == nil {
		t.Fatal("expected error for non-200 status")
	}
}

func TestListDevicesFromURL_InvalidJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json"))
	}))
	defer ts.Close()

	_, err := listDevicesFromURL(context.Background(), ts.URL+"/json")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestHasScheme(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"http://localhost", true},
		{"https://example.com", true},
		{"localhost:9222", false},
		{"127.0.0.1:9222", false},
		{"ftp://files", true},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := hasScheme(tt.input); got != tt.want {
				t.Errorf("hasScheme(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
