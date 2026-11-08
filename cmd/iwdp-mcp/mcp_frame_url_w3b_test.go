package main

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// W3b final repair round 1, finding 2: a page selected by an explicit
// WebSocket URL that iwdp does not list is opened with webkit.Client
// (core ManagerOptions.OpenURL). select_frame works there: the client's
// inventory has the frame's context, and the session's later runtime
// calls go to that context. Before the repair, the frame had no context
// on such a connection and select_frame reported it as unsupported.
func TestSelectFrameOnAnExplicitURLPage(t *testing.T) {
	privateDaemonDir(t)
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	var mu sync.Mutex
	var pings []any // contextId of each runtime ping
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p map[string]any
		_ = json.Unmarshal(c.Params, &p)
		expr, _ := p["expression"].(string)
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(expr); m != nil {
			if m[1] == "ping" {
				mu.Lock()
				pings = append(pings, p["contextId"])
				mu.Unlock()
			}
			v := map[string]any{"ok": true, "value": true}
			if m[1] == "clock" {
				v["value"] = map[string]any{"now": 1000, "doc": "doc-1"}
			}
			return map[string]any{"result": map[string]any{"type": "object", "value": v}}, nil
		}
		if strings.Contains(expr, `window.__iwdp.call("pick"`) {
			return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"tag": "iframe", "src": "/child.html", "index": 0, "frames": 1}}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 0}}, nil
	})
	f.Handle("Runtime.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		f.EmitFrom(c.Target, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 1, "type": "normal", "frameId": "main"}})
		f.EmitFrom(c.Target, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 7, "type": "normal", "frameId": "f-kid"}})
		return map[string]any{}, nil
	})
	f.Result("Page.getResourceTree", map[string]any{"frameTree": map[string]any{
		"frame":       map[string]any{"id": "main"},
		"childFrames": []any{map[string]any{"frame": map[string]any{"id": "f-kid"}}},
	}})
	cs := mcpServerSession(t)
	if res, text := callTool(t, cs, "select_page", map[string]any{"websocket_url": f.URL}); res.IsError {
		t.Fatalf("select_page: %s", text)
	}
	res, text := callTool(t, cs, "select_frame", map[string]any{"target": "#kid"})
	if res.IsError || !strings.Contains(text, `"frameId":"f-kid"`) {
		t.Fatalf("select_frame on an explicit-URL page: error %v, %q", res.IsError, text)
	}
	mu.Lock()
	got := append([]any(nil), pings...)
	mu.Unlock()
	if len(got) == 0 || got[len(got)-1] != float64(7) {
		t.Fatalf("contextId of the runtime pings = %v, want the last one in the frame's context 7", got)
	}
}
