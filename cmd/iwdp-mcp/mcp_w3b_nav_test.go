package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// Review finding 2 (W3b navigation): every MCP tool that a navigation
// registry command took over accepts each parameter of its legacy handler
// with the same JSON type. The legacy input types below are the ones the
// handlers in main.go decode.
func TestNavigationRegistryToolsKeepLegacyInputs(t *testing.T) {
	jsonType := map[reflect.Kind]string{reflect.String: "string", reflect.Bool: "boolean", reflect.Int: "integer"}
	for name, in := range map[string]any{
		"navigate":        NavigateInput{},
		"reload":          ReloadInput{},
		"take_screenshot": TakeScreenshotInput{},
	} {
		if !commands.ProvidesMCPTool(commands.ProfileCore, name) {
			t.Fatalf("%s: no registry command provides it in the core profile", name)
		}
		var cmd *commands.Command
		for _, c := range commands.All() {
			if c.MCPName == name {
				cmd = c
			}
		}
		props := commands.InputSchema(cmd)["properties"].(map[string]any)
		rt := reflect.TypeOf(in)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			param, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			p, ok := props[param].(map[string]any)
			if !ok {
				t.Fatalf("%s: the registry tool lacks the legacy parameter %q", name, param)
			}
			if want := jsonType[f.Type.Kind()]; p["type"] != want {
				t.Fatalf("%s: parameter %q has type %v, the legacy one %s", name, param, p["type"], want)
			}
		}
	}
}

// Review finding 7: take_screenshot is the registry screenshot command.
// Through MCP it returns the PNG as image content next to the legacy text
// and file_path, takes the legacy full_page, and if_changed skips an
// identical capture.
func TestTakeScreenshotReturnsTheImageThroughMCP(t *testing.T) {
	privateDaemonDir(t)
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	shot := buf.Bytes()
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "string", "value": `{"width":3,"height":2}`}})
	mock.HandleFunc("Page.snapshotRect", map[string]string{"dataURL": "data:image/png;base64," + base64.StdEncoding.EncodeToString(shot)})
	cs := mcpServerSession(t)
	if res, text := callTool(t, cs, "select_page", map[string]any{"websocket_url": mock.URL}); res.IsError {
		t.Fatalf("select_page: %s", text)
	}

	res, text := callTool(t, cs, "take_screenshot", map[string]any{"full_page": true, "if_changed": true})
	if res.IsError || !strings.HasPrefix(text, "Screenshot saved to ") {
		t.Fatalf("take_screenshot: error %v, %q", res.IsError, text)
	}
	var images []*mcp.ImageContent
	for _, c := range res.Content {
		if ic, ok := c.(*mcp.ImageContent); ok {
			images = append(images, ic)
		}
	}
	if len(images) != 1 || images[0].MIMEType != "image/png" || !bytes.Equal(images[0].Data, shot) {
		t.Fatalf("take_screenshot content %+v: want one image/png with the captured PNG", res.Content)
	}
	var out struct {
		FilePath string `json:"file_path"`
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &out); err != nil || out.FilePath == "" || !strings.Contains(text, out.FilePath) {
		t.Fatalf("structured content %s: want the legacy file_path, also named in the text %q", raw, text)
	}
	if b, err := os.ReadFile(out.FilePath); err != nil || !bytes.Equal(b, shot) {
		t.Fatalf("file %s: %v", out.FilePath, err)
	}
	if !strings.Contains(strings.Join(mock.ReceivedMethods(), ","), "Page.snapshotRect") {
		t.Fatal("the page received no capture")
	}

	res, text = callTool(t, cs, "take_screenshot", map[string]any{"full_page": true, "if_changed": true})
	if res.IsError || !strings.Contains(text, `"unchanged":true`) || len(res.Content) != 1 {
		t.Fatalf("identical take_screenshot with if_changed: error %v, content %+v", res.IsError, res.Content)
	}
}

// W3b-R1 merge brief item 3: through the MCP server, navigate keeps the
// legacy rule. A javascript: URL is a tool error with the legacy wording,
// and the page receives nothing.
func TestNavigateThroughMCPRefusesNonWebSchemes(t *testing.T) {
	privateDaemonDir(t)
	mock := testutil.NewMockServer()
	defer mock.Close()
	cs := mcpServerSession(t)
	if res, text := callTool(t, cs, "select_page", map[string]any{"websocket_url": mock.URL}); res.IsError {
		t.Fatalf("select_page: %s", text)
	}
	before := len(mock.ReceivedMethods())
	for _, u := range []string{"javascript:alert(document.cookie)", "file:///etc/passwd", "example.com"} {
		res, text := callTool(t, cs, "navigate", map[string]any{"url": u})
		if !res.IsError || !strings.Contains(text, "only http and https are allowed") {
			t.Fatalf("navigate %s: error %v, %q; want the legacy scheme error", u, res.IsError, text)
		}
	}
	if got := mock.ReceivedMethods()[before:]; len(got) != 0 {
		t.Fatalf("refused navigate calls reached the page: %v", got)
	}
}
