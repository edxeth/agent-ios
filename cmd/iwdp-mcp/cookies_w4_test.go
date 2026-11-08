package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// Design 4.2 row "cookies set": the server lists one set_cookie, the
// registry command, with the W0 parameters plus curl, and no
// import_cookies; a call with curl imports the file's cookies.
func TestSetCookieToolIsTheRegistryCommand(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
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
	mock.HandleFunc("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "string", "value": "app.test"}})
	cs := connectMCP(t, mock)

	n := 0
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		switch tool.Name {
		case "import_cookies":
			t.Error("the server still lists import_cookies")
		case "set_cookie":
			n++
			schema, _ := json.Marshal(tool.InputSchema)
			for _, p := range []string{"name", "value", "domain", "path", "expires", "secure", "http_only", "same_site", "curl"} {
				if !strings.Contains(string(schema), `"`+p+`"`) {
					t.Errorf("set_cookie schema lacks %q: %s", p, schema)
				}
			}
		}
	}
	if n != 1 {
		t.Fatalf("the server lists set_cookie %d times, want 1", n)
	}

	file := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(file, []byte("sid="+mcpSecret+"; theme=dark"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, text := callTool(t, cs, "set_cookie", map[string]any{"curl": file})
	if res.IsError {
		t.Fatalf("set_cookie curl: %s", text)
	}
	mu.Lock()
	got := len(jar)
	mu.Unlock()
	structured, _ := json.Marshal(res.StructuredContent)
	if got != 2 || !strings.Contains(string(structured), `"imported":2`) || strings.Contains(text+string(structured), mcpSecret) {
		t.Fatalf("jar has %d cookies, result %s; want 2 imported without values", got, structured)
	}
}
