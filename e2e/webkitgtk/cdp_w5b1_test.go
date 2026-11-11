//go:build webkitgtk

package webkitgtk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp/cdptest"
)

// w5b1Wait bounds each wait for a CDP event of the W5b-1 test.
const w5b1Wait = 15 * time.Second

// gtkValue evaluates expr in the page session (in contextId when it is
// not 0) and returns its value.
func gtkValue(t *testing.T, c *cdptest.Client, session, expr string, contextID int) any {
	t.Helper()
	p := map[string]any{"expression": expr, "returnByValue": true}
	if contextID != 0 {
		p["contextId"] = contextID
	}
	var r struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails any `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(gtkCall(t, c, session, "Runtime.evaluate", p), &r); err != nil || r.ExceptionDetails != nil {
		t.Fatalf("Runtime.evaluate %q: %v, exception %v", expr, err, r.ExceptionDetails)
	}
	return r.Result.Value
}

func gtkEvent(t *testing.T, c *cdptest.Client, after int, session, method string, match func(json.RawMessage) bool) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), w5b1Wait)
	defer cancel()
	e, err := c.WaitEvent(ctx, after, method, func(e cdptest.Event) bool {
		return e.SessionID == session && (match == nil || match(e.Params))
	})
	if err != nil {
		var seen []string
		for _, e := range c.Events() {
			if e.Seq > after {
				seen = append(seen, e.Method)
			}
		}
		t.Fatalf("%s on session %s: %v; events since: %v", method, session, err, seen)
	}
	return e.Params
}

// W5b-1 on WebKitGTK (design 5.2, 5.3; acceptance 7.8 items 4 to 7, and
// the attach of item 1): a CDP client attaches through "get cdp-url" as
// Puppeteer does, and in the real page
//   - Page.navigate returns the loader id that frameNavigated and the
//     lifecycle events (load, and networkIdle from the network log)
//     carry;
//   - a script of addScriptToEvaluateOnNewDocument runs in the new
//     document;
//   - an evaluate in a createIsolatedWorld context runs in the page;
//   - a fragment navigation returns no loader id and emits
//     navigatedWithinDocument, and a Runtime.addBinding function emits
//     bindingCalled (the hooks of js/66-cdp-page.js);
//   - captureScreenshot returns a PNG of the viewport;
//   - a confirm() is answered by the session's dialog policy (dismiss:
//     false) and reported as a Log.entryAdded warning;
//   - Performance.getMetrics reports the document's node count and
//     navigation timing.
func TestCDPPageRuntimeLogPerformance(t *testing.T) {
	addr := runtimeBrowser(t, "forms.html")
	if r := cli(t, "daemon", "start", "--transport", "webkitgtk", "--addr", addr); r.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", r.code, r.stderr)
	}
	t.Cleanup(func() {
		if r := cli(t, "daemon", "stop"); r.code != 0 {
			t.Errorf("daemon stop: exit %d, stderr %q", r.code, r.stderr)
		}
	})
	port := strings.TrimPrefix(freeAddr(t), "127.0.0.1:")
	if r := cli(t, "cdp", "start", "--port", port); r.code != 0 {
		t.Fatalf("cdp start --port %s: exit %d, stdout %q, stderr %q", port, r.code, r.stdout, r.stderr)
	}
	snapshotRef(t, "s1")
	r := cli(t, "--session", "s1", "get", "cdp-url")
	ws := strings.TrimSpace(r.stdout)
	if r.code != 0 || !strings.HasPrefix(ws, "ws://127.0.0.1:"+port+"/t/") {
		t.Fatalf("get cdp-url: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), w5b1Wait)
	defer cancel()
	c, _, err := cdptest.Dial(ctx, ws, nil)
	if err != nil {
		t.Fatalf("dialing the printed URL: %v", err)
	}
	defer c.Close()
	gtkCall(t, c, "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}},
	})
	tab, _ := gtkLastAttach(t, c)
	gtkCall(t, c, tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true})
	page, kind := gtkLastAttach(t, c)
	if kind != "page" {
		t.Fatalf("tab auto-attach attached a %q, want the page", kind)
	}
	for _, m := range []string{"Page.enable", "Runtime.enable", "Log.enable", "Performance.enable"} {
		gtkCall(t, c, page, m, nil)
	}
	gtkCall(t, c, page, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": true})
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID, LoaderID, URL string
			}
		}
	}
	if err := json.Unmarshal(gtkCall(t, c, page, "Page.getFrameTree", nil), &tree); err != nil || tree.FrameTree.Frame.ID == "" || !strings.HasSuffix(tree.FrameTree.Frame.URL, "/forms.html") {
		t.Fatalf("frame tree %+v (%v), want the forms page", tree, err)
	}
	mainFrame := tree.FrameTree.Frame.ID

	// Navigation identity, and a script for new documents.
	gtkCall(t, c, page, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.__w5b1 = 'boot:' + location.pathname"})
	next := strings.TrimSuffix(tree.FrameTree.Frame.URL, "forms.html") + "aria.html"
	seq := c.Seq()
	var nav struct{ FrameID, LoaderID string }
	if err := json.Unmarshal(gtkCall(t, c, page, "Page.navigate", map[string]any{"url": next}), &nav); err != nil || nav.FrameID != mainFrame || nav.LoaderID == "" || nav.LoaderID == tree.FrameTree.Frame.LoaderID {
		t.Fatalf("Page.navigate = %+v (%v), want frame %s and a new loader id (old %s)", nav, err, mainFrame, tree.FrameTree.Frame.LoaderID)
	}
	var fn struct {
		Frame struct{ ID, LoaderID, URL, ParentID string }
	}
	if err := json.Unmarshal(gtkEvent(t, c, seq, page, "Page.frameNavigated", nil), &fn); err != nil || fn.Frame.ID != mainFrame || fn.Frame.LoaderID != nav.LoaderID || fn.Frame.URL != next {
		t.Fatalf("frameNavigated %+v (%v), want frame %s at %s with Page.navigate's loader %s", fn, err, mainFrame, next, nav.LoaderID)
	}
	for _, name := range []string{"load", "networkIdle"} {
		var l struct{ FrameID, LoaderID string }
		if err := json.Unmarshal(gtkEvent(t, c, seq, page, "Page.lifecycleEvent", func(p json.RawMessage) bool {
			return strings.Contains(string(p), `"name":"`+name+`"`)
		}), &l); err != nil || l.FrameID != mainFrame || l.LoaderID != nav.LoaderID {
			t.Fatalf("lifecycle %s %+v (%v), want frame %s with loader %s", name, l, err, mainFrame, nav.LoaderID)
		}
	}
	if v := gtkValue(t, c, page, "window.__w5b1", 0); v != "boot:/aria.html" {
		t.Fatalf("the new-document script left %v, want boot:/aria.html", v)
	}

	// A utility world: its context runs in the page.
	var world struct{ ExecutionContextID int }
	if err := json.Unmarshal(gtkCall(t, c, page, "Page.createIsolatedWorld", map[string]any{"frameId": mainFrame, "worldName": "w5b1-util"}), &world); err != nil || world.ExecutionContextID == 0 {
		t.Fatalf("createIsolatedWorld = %+v (%v)", world, err)
	}
	if v := gtkValue(t, c, page, "document.title + ':' + (6*7)", world.ExecutionContextID); v != "ARIA:42" {
		t.Fatalf("evaluate in the utility world = %v, want ARIA:42", v)
	}

	// A same-document navigation (the runtime's navigation hook): no
	// loader id, navigatedWithinDocument.
	seq = c.Seq()
	var same map[string]any
	if err := json.Unmarshal(gtkCall(t, c, page, "Page.navigate", map[string]any{"url": next + "#part"}), &same); err != nil || same["frameId"] != mainFrame || same["loaderId"] != nil {
		t.Fatalf("same-document Page.navigate = %v (%v), want frame %s and no loader id", same, err, mainFrame)
	}
	var within struct{ FrameID, URL, NavigationType string }
	if err := json.Unmarshal(gtkEvent(t, c, seq, page, "Page.navigatedWithinDocument", nil), &within); err != nil || within.FrameID != mainFrame || within.URL != next+"#part" || within.NavigationType != "fragment" {
		t.Fatalf("navigatedWithinDocument %+v (%v)", within, err)
	}

	// A binding (the runtime's marked console message): bindingCalled.
	gtkCall(t, c, page, "Runtime.addBinding", map[string]any{"name": "w5b1Bind"})
	seq = c.Seq()
	gtkValue(t, c, page, "w5b1Bind('hello'), 1", 0)
	var bound struct {
		Name, Payload      string
		ExecutionContextID int
	}
	if err := json.Unmarshal(gtkEvent(t, c, seq, page, "Runtime.bindingCalled", nil), &bound); err != nil || bound.Name != "w5b1Bind" || bound.Payload != "hello" || bound.ExecutionContextID == 0 {
		t.Fatalf("bindingCalled %+v (%v)", bound, err)
	}
	for _, e := range c.Events() {
		if e.Seq > seq && e.Method == "Runtime.consoleAPICalled" && strings.Contains(string(e.Params), "__iwdp_") {
			t.Fatalf("the binding's marked message reached the client: %s", e.Params)
		}
	}

	// A screenshot of the viewport.
	var shot struct{ Data string }
	if err := json.Unmarshal(gtkCall(t, c, page, "Page.captureScreenshot", map[string]any{"format": "png"}), &shot); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		t.Fatalf("screenshot data is not base64: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("screenshot is not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() < 100 || b.Dy() < 100 {
		t.Fatalf("screenshot is %v, want the viewport", b)
	}

	// A dialog answered by the session's policy, as a Log warning.
	seq = c.Seq()
	if v := gtkValue(t, c, page, "confirm('Delete?')", 0); v != false {
		t.Fatalf("confirm() returned %v, want false from the session's dismiss policy", v)
	}
	var entry struct {
		Entry struct{ Source, Level, Text string }
	}
	if err := json.Unmarshal(gtkEvent(t, c, seq, page, "Log.entryAdded", func(p json.RawMessage) bool { return strings.Contains(string(p), "Delete?") }), &entry); err != nil ||
		entry.Entry.Level != "warning" || !strings.HasPrefix(entry.Entry.Text, `dialog confirm "Delete?" answered false by the session's dialog policy`) {
		t.Fatalf("the dialog's Log entry %+v (%v)", entry, err)
	}
	for _, e := range c.Events() {
		if strings.HasPrefix(e.Method, "Page.javascriptDialog") {
			t.Fatalf("the bridge sent %s", e.Method)
		}
	}

	// Performance metrics of the document.
	var perf struct {
		Metrics []struct {
			Name  string
			Value float64
		}
	}
	if err := json.Unmarshal(gtkCall(t, c, page, "Performance.getMetrics", nil), &perf); err != nil {
		t.Fatal(err)
	}
	metrics := map[string]float64{}
	for _, m := range perf.Metrics {
		metrics[m.Name] = m.Value
	}
	if metrics["Nodes"] < 5 || metrics["Documents"] < 1 || metrics["NavigationStart"] <= 0 || metrics["Timestamp"] <= 0 {
		t.Fatalf("Performance.getMetrics = %v, want the document's nodes and navigation timing", metrics)
	}
}
