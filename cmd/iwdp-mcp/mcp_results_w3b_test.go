package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// W3b final brief, leftover 4e: the two result forms of registry tools
// work side by side on one MCP server (registerTools, which calls
// commands.RegisterMCP), as a client reads them from the wire:
//   - a capture (take_screenshot, W3b review finding 7) is its text plus
//     the PNG as image content, with the legacy file_path in the
//     structured content;
//   - a JSON object result (set_cookie, W4: the legacy {ok, cookie}) is
//     that JSON as text and the same object as structured content.
func TestRegistryImageAndObjectResultsThroughMCP(t *testing.T) {
	privateDaemonDir(t)
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	shot := buf.Bytes()
	mock := testutil.NewMockServer()
	defer mock.Close()
	// The capture asks for the viewport size; set_cookie may ask for the
	// page's host.
	mock.Handle("Runtime.evaluate", func(_ string, params json.RawMessage) (any, error) {
		value := "app.test"
		if strings.Contains(string(params), "innerWidth") {
			value = `{"width":3,"height":2}`
		}
		return map[string]any{"result": map[string]any{"type": "string", "value": value}}, nil
	})
	mock.HandleFunc("Page.snapshotRect", map[string]string{"dataURL": "data:image/png;base64," + base64.StdEncoding.EncodeToString(shot)})
	var mu sync.Mutex
	var jar []webkit.Cookie
	mock.Handle("Page.setCookie", func(_ string, params json.RawMessage) (any, error) {
		var p struct {
			Cookie webkit.Cookie `json:"cookie"`
		}
		_ = json.Unmarshal(params, &p)
		mu.Lock()
		jar = append(jar, p.Cookie)
		mu.Unlock()
		return map[string]any{}, nil
	})
	mock.Handle("Page.getCookies", func(string, json.RawMessage) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		return map[string]any{"cookies": jar}, nil
	})
	cs := mcpServerSession(t)
	if res, text := callTool(t, cs, "select_page", map[string]any{"websocket_url": mock.URL}); res.IsError {
		t.Fatalf("select_page: %s", text)
	}

	res, text := callTool(t, cs, "take_screenshot", map[string]any{})
	if res.IsError || len(res.Content) != 2 {
		t.Fatalf("take_screenshot: error %v, content %+v; want text and image", res.IsError, res.Content)
	}
	ic, ok := res.Content[1].(*mcp.ImageContent)
	if _, isText := res.Content[0].(*mcp.TextContent); !isText || !ok || ic.MIMEType != "image/png" || !bytes.Equal(ic.Data, shot) {
		t.Fatalf("take_screenshot content %+v; want the text, then the captured PNG as image/png", res.Content)
	}
	var capture map[string]any
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &capture); err != nil || capture["file_path"] == nil || !strings.Contains(text, capture["file_path"].(string)) {
		t.Fatalf("take_screenshot structured content %s; want the legacy file_path, also named in the text %q", raw, text)
	}

	res, text = callTool(t, cs, "set_cookie", map[string]any{"name": "sid", "value": mcpSecret, "domain": "app.test"})
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("set_cookie: error %v, content %+v; want one text item", res.IsError, res.Content)
	}
	var fromText, structured map[string]any
	raw, _ = json.Marshal(res.StructuredContent)
	if err := json.Unmarshal([]byte(text), &fromText); err != nil {
		t.Fatalf("set_cookie text %q is not a JSON object: %v", text, err)
	}
	if err := json.Unmarshal(raw, &structured); err != nil || !reflect.DeepEqual(structured, fromText) || structured["ok"] != true {
		t.Fatalf("set_cookie structured content %s, text %s; want the same {ok, cookie} object in both", raw, text)
	}
	if strings.Contains(text+string(raw), mcpSecret) {
		t.Fatalf("set_cookie result %s shows the cookie value", raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(jar) != 1 || jar[0].Name != "sid" {
		t.Fatalf("cookie jar %+v; want sid", jar)
	}
}
