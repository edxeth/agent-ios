package tools_test

import (
	"context"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// helper creates a mock server and connected client, returning both and a cleanup function.
func setup(t *testing.T) (*testutil.MockServer, *webkit.Client) {
	t.Helper()
	mock := testutil.NewMockServer()

	ctx := context.Background()
	client, err := webkit.NewClient(ctx, mock.URL)
	if err != nil {
		mock.Close()
		t.Fatalf("NewClient: %v", err)
	}

	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Logf("warning: closing client: %v", err)
		}
		mock.Close()
	})

	return mock, client
}

func TestEvaluateScript(t *testing.T) {
	mock, client := setup(t)

	mock.HandleFunc("Runtime.evaluate", map[string]interface{}{
		"result": map[string]interface{}{
			"type":  "string",
			"value": `"hello"`,
		},
		"wasThrown": false,
	})

	ctx := context.Background()
	result, err := tools.EvaluateScript(ctx, client, `"hello"`, true)
	if err != nil {
		t.Fatalf("EvaluateScript returned error: %v", err)
	}
	if result.Result.Type != "string" {
		t.Errorf("expected result type %q, got %q", "string", result.Result.Type)
	}
	if result.WasThrown {
		t.Errorf("expected wasThrown to be false")
	}
}

func TestGetDocument(t *testing.T) {
	mock, client := setup(t)

	mock.HandleFunc("DOM.getDocument", map[string]interface{}{
		"root": map[string]interface{}{
			"nodeId":   1,
			"nodeType": 9,
			"nodeName": "#document",
		},
	})

	ctx := context.Background()
	root, err := tools.GetDocument(ctx, client, 0)
	if err != nil {
		t.Fatalf("GetDocument returned error: %v", err)
	}
	if root.NodeID != 1 {
		t.Errorf("expected root.NodeID = 1, got %d", root.NodeID)
	}
	if root.NodeType != 9 {
		t.Errorf("expected root.NodeType = 9, got %d", root.NodeType)
	}
	if root.NodeName != "#document" {
		t.Errorf("expected root.NodeName = %q, got %q", "#document", root.NodeName)
	}
}

func TestQuerySelector(t *testing.T) {
	mock, client := setup(t)

	mock.HandleFunc("DOM.querySelector", map[string]interface{}{
		"nodeId": 42,
	})

	ctx := context.Background()
	nodeID, err := tools.QuerySelector(ctx, client, 1, "div.main")
	if err != nil {
		t.Fatalf("QuerySelector returned error: %v", err)
	}
	if nodeID != 42 {
		t.Errorf("expected nodeID = 42, got %d", nodeID)
	}
}

func TestQuerySelectorNotFound(t *testing.T) {
	mock, client := setup(t)

	mock.HandleFunc("DOM.querySelector", map[string]interface{}{
		"nodeId": 0,
	})

	ctx := context.Background()
	_, err := tools.QuerySelector(ctx, client, 1, "div.nonexistent")
	if err == nil {
		t.Fatal("expected error for not-found selector, got nil")
	}
}

func TestGetCookies(t *testing.T) {
	mock, client := setup(t)

	mock.HandleFunc("Page.getCookies", map[string]interface{}{
		"cookies": []map[string]interface{}{
			{
				"name":     "session",
				"value":    "abc123",
				"domain":   ".example.com",
				"path":     "/",
				"httpOnly": true,
				"secure":   true,
				"session":  false,
				"expires":  1700000000.0,
			},
		},
	})

	ctx := context.Background()
	cookies, err := tools.GetCookies(ctx, client)
	if err != nil {
		t.Fatalf("GetCookies returned error: %v", err)
	}
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}

	c := cookies[0]
	if c.Name != "session" {
		t.Errorf("expected cookie name %q, got %q", "session", c.Name)
	}
	if c.Value != "abc123" {
		t.Errorf("expected cookie value %q, got %q", "abc123", c.Value)
	}
	if c.Domain != ".example.com" {
		t.Errorf("expected cookie domain %q, got %q", ".example.com", c.Domain)
	}
	if !c.HTTPOnly {
		t.Errorf("expected cookie to be httpOnly")
	}
	if !c.Secure {
		t.Errorf("expected cookie to be secure")
	}
}
