package tools_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/edxeth/agent-ios/internal/tools"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil"
)

// Ensure imports are used.
var (
	_ *testutil.MockServer
	_ webkit.Cookie
)

// ---------------------------------------------------------------------------
// Page (page.go)
// ---------------------------------------------------------------------------

func TestTakeScreenshot(t *testing.T) {
	mock, client := setup(t)

	mock.HandleFunc("Runtime.evaluate", map[string]interface{}{
		"result": map[string]interface{}{
			"type":  "string",
			"value": `{"width":1024,"height":768}`,
		},
	})
	mock.HandleFunc("Page.snapshotRect", map[string]interface{}{
		"dataURL": "data:image/png;base64,abc123",
	})

	ctx := context.Background()
	dataURL, err := tools.TakeScreenshot(ctx, client)
	if err != nil {
		t.Fatalf("TakeScreenshot returned error: %v", err)
	}
	if dataURL != "data:image/png;base64,abc123" {
		t.Errorf("expected dataURL %q, got %q", "data:image/png;base64,abc123", dataURL)
	}
}

func TestSnapshotNode(t *testing.T) {
	mock, client := setup(t)

	var receivedNodeID int
	mock.Handle("Page.snapshotNode", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			NodeID int `json:"nodeId"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		receivedNodeID = p.NodeID
		return map[string]interface{}{
			"dataURL": "data:image/png;base64,node123",
		}, nil
	})

	ctx := context.Background()
	dataURL, err := tools.SnapshotNode(ctx, client, 42)
	if err != nil {
		t.Fatalf("SnapshotNode returned error: %v", err)
	}
	if receivedNodeID != 42 {
		t.Errorf("expected nodeId=42, got %d", receivedNodeID)
	}
	if dataURL != "data:image/png;base64,node123" {
		t.Errorf("expected dataURL %q, got %q", "data:image/png;base64,node123", dataURL)
	}
}

func TestSetCookie(t *testing.T) {
	mock, client := setup(t)

	var receivedName, receivedDomain string
	mock.Handle("Page.setCookie", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			Cookie webkit.Cookie `json:"cookie"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		receivedName = p.Cookie.Name
		receivedDomain = p.Cookie.Domain
		return map[string]interface{}{}, nil
	})

	ctx := context.Background()
	cookie := webkit.Cookie{
		Name:   "session",
		Value:  "xyz",
		Domain: ".example.com",
		Path:   "/",
	}
	if err := tools.SetCookie(ctx, client, cookie); err != nil {
		t.Fatalf("SetCookie returned error: %v", err)
	}
	if receivedName != "session" {
		t.Errorf("expected cookie name %q, got %q", "session", receivedName)
	}
	if receivedDomain != ".example.com" {
		t.Errorf("expected cookie domain %q, got %q", ".example.com", receivedDomain)
	}
}

// ---------------------------------------------------------------------------
// Runtime (runtime.go)
// ---------------------------------------------------------------------------

func TestEvaluateScriptWasThrown(t *testing.T) {
	mock, client := setup(t)
	_ = mock

	mock.HandleFunc("Runtime.evaluate", map[string]interface{}{
		"result": map[string]interface{}{
			"type": "object",
		},
		"wasThrown": true,
		"exceptionDetails": map[string]interface{}{
			"text": "ReferenceError: foo is not defined",
			"line": 1,
			"exception": map[string]interface{}{
				"type":        "object",
				"description": "ReferenceError: foo is not defined",
			},
		},
	})

	ctx := context.Background()
	result, err := tools.EvaluateScript(ctx, client, "foo", true)
	if err == nil {
		t.Fatal("expected error for wasThrown=true, got nil")
	}
	if result == nil {
		t.Fatal("expected non-nil result even when wasThrown=true")
	}
	if !result.WasThrown {
		t.Errorf("expected WasThrown=true")
	}
}
