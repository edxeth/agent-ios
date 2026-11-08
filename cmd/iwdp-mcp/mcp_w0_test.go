package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// connectMCP selects a mock page in the global session and returns an MCP
// client session for the registered tools.
func connectMCP(t *testing.T, mock *testutil.MockServer) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	client, err := webkit.NewClient(ctx, mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sess.mu.Lock()
	old := sess.client
	sess.client = client
	sess.mu.Unlock()

	impl := &mcp.Implementation{Name: "iwdp-mcp-test", Version: "0.1.0"}
	server := mcp.NewServer(impl, nil)
	registerTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	cs, err := mcp.NewClient(impl, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = serverSession.Close()
		sess.mu.Lock()
		sess.client = old
		sess.mu.Unlock()
		_ = client.Close()
	})
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return res, text.String()
}

const mcpSecret = "s3cr3t-session-token-1234567890"

func TestGetCookiesRedactsUnlessShowValues(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Page.getCookies", map[string]any{"cookies": []webkit.Cookie{
		{Name: "sid", Value: mcpSecret, Domain: "example.com", Path: "/", SameSite: "Lax"},
	}})
	cs := connectMCP(t, mock)

	res, text := callTool(t, cs, "get_cookies", map[string]any{})
	if res.IsError {
		t.Fatalf("get_cookies error: %s", text)
	}
	structured, _ := json.Marshal(res.StructuredContent)
	if strings.Contains(text+string(structured), mcpSecret) {
		t.Errorf("get_cookies returned the value: %s", structured)
	}
	if !strings.Contains(string(structured), "s3cr...(redacted, 31 chars)") {
		t.Errorf("get_cookies output %s lacks the redacted value", structured)
	}

	res, _ = callTool(t, cs, "get_cookies", map[string]any{"show_values": true})
	structured, _ = json.Marshal(res.StructuredContent)
	if !strings.Contains(string(structured), mcpSecret) {
		t.Errorf("show_values=true output %s lacks the value", structured)
	}
}

func TestSetCookieToolVerifiesAndConvertsExpiry(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	var mu sync.Mutex
	var sent []webkit.Cookie
	mock.Handle("Page.setCookie", func(_ string, params json.RawMessage) (any, error) {
		var p struct {
			Cookie webkit.Cookie `json:"cookie"`
		}
		_ = json.Unmarshal(params, &p)
		mu.Lock()
		sent = append(sent, p.Cookie)
		mu.Unlock()
		return map[string]any{}, nil
	})
	mock.Handle("Page.getCookies", func(string, json.RawMessage) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		return map[string]any{"cookies": sent}, nil
	})
	cs := connectMCP(t, mock)

	res, text := callTool(t, cs, "set_cookie", map[string]any{
		"name": "sid", "value": mcpSecret, "domain": "example.com", "path": "/", "expires": 1893456000,
	})
	if res.IsError {
		t.Fatalf("set_cookie error: %s", text)
	}
	mu.Lock()
	got := sent[0]
	mu.Unlock()
	if got.SameSite != "Lax" || got.Expires != 1893456000*1000 || got.Session {
		t.Errorf("Page.setCookie cookie = %+v; want sameSite Lax and expires in milliseconds", got)
	}
	structured, _ := json.Marshal(res.StructuredContent)
	if strings.Contains(string(structured), mcpSecret) || !strings.Contains(string(structured), `"ok":true`) {
		t.Errorf("set_cookie output = %s", structured)
	}
}

func TestSetCookieToolFailsWhenNotReadBack(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Page.getCookies", map[string]any{"cookies": []webkit.Cookie{}})
	cs := connectMCP(t, mock)
	res, text := callTool(t, cs, "set_cookie", map[string]any{"name": "sid", "value": "v", "domain": "example.com"})
	if !res.IsError || !strings.Contains(text, "not found") {
		t.Errorf("IsError=%v text=%q; want a not-found error", res.IsError, text)
	}
}

func TestEvaluateScriptToolAwaitsPromise(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Runtime.evaluate", map[string]any{
		"result": map[string]any{"type": "object", "className": "Promise", "objectId": "p1"},
	})
	mock.HandleFunc("Runtime.awaitPromise", map[string]any{
		"result": map[string]any{"type": "number", "value": 42},
	})
	cs := connectMCP(t, mock)
	res, text := callTool(t, cs, "evaluate_script", map[string]any{"expression": "Promise.resolve(42)", "return_by_value": true, "await_promise": true})
	if res.IsError {
		t.Fatalf("evaluate_script error: %s", text)
	}
	structured, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(structured), `"value":42`) {
		t.Errorf("evaluate_script output = %s, want the awaited value 42", structured)
	}
}

func TestEvaluateScriptToolReportsException(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Runtime.evaluate", map[string]any{
		"result":    map[string]any{"type": "object", "description": "TypeError: x is not a function", "objectId": "e1"},
		"wasThrown": true,
	})
	mock.HandleFunc("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"type": "string", "value": "f@page.js:1:2"}})
	cs := connectMCP(t, mock)
	res, text := callTool(t, cs, "evaluate_script", map[string]any{"expression": "x()"})
	if !res.IsError || !strings.Contains(text, "TypeError: x is not a function") || !strings.Contains(text, "f@page.js:1:2") {
		t.Errorf("IsError=%v text=%q; want message and stack", res.IsError, text)
	}
}

func TestTakeScreenshotToolDefaultsToViewport(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Runtime.evaluate", map[string]any{
		"result": map[string]any{"type": "string", "value": `{"width":10,"height":20}`},
	})
	var mu sync.Mutex
	var systems []string
	mock.Handle("Page.snapshotRect", func(_ string, params json.RawMessage) (any, error) {
		var p struct {
			CoordinateSystem string `json:"coordinateSystem"`
		}
		_ = json.Unmarshal(params, &p)
		mu.Lock()
		systems = append(systems, p.CoordinateSystem)
		mu.Unlock()
		return map[string]string{"dataURL": "data:image/png;base64,iVBORw0KGgo="}, nil
	})
	t.Setenv("TMPDIR", t.TempDir())
	// take_screenshot is the registry screenshot command (W3b), which runs
	// in the daemon on the page that select_page leased.
	privateDaemonDir(t)
	cs := mcpServerSession(t)
	if res, text := callTool(t, cs, "select_page", map[string]any{"websocket_url": mock.URL}); res.IsError {
		t.Fatalf("select_page: %s", text)
	}

	for _, args := range []map[string]any{{}, {"full_page": true}} {
		if res, text := callTool(t, cs, "take_screenshot", args); res.IsError {
			t.Fatalf("take_screenshot %v: %s", args, text)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(systems, ",") != "Viewport,Page" {
		t.Errorf("coordinate systems = %v, want Viewport then Page", systems)
	}
}
