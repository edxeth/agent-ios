//go:build webkitgtk

package webkitgtk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
)

var labelLine = regexp.MustCompile(`^\[(\d+)\] @(e\d+) (\S+) "(.*)"$`)

// 7.6 item 10: labels match refs, the overlay is gone after the capture
// (DOM check), and --if-changed skips an identical capture.
func TestAnnotatedScreenshotOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "annotate.html", false)
	dir := t.TempDir()
	plain, annotated := filepath.Join(dir, "plain.png"), filepath.Join(dir, "annotated.png")
	e.must(agentA, "screenshot", plain)
	out := e.must(agentA, "screenshot", "--annotate", annotated)
	lines := strings.Split(out, "\n")[1:]
	names := map[string]string{}
	for _, l := range lines {
		m := labelLine.FindStringSubmatch(l)
		if m == nil || "e"+m[1] != m[2] {
			t.Fatalf("label line %q is not [N] @eN role \"name\" with N of eN", l)
		}
		names[m[4]] = m[2]
	}
	if names["Save"] == "" || names["Help"] == "" || names["Far below"] != "" {
		t.Fatalf("labels %v: want Save and Help in the viewport and not the far button", names)
	}
	for name, ref := range names {
		if name == "Search" {
			continue // an input has no text
		}
		if got := strings.TrimSpace(e.must(agentA, "get", "text", "@"+ref)); got != name {
			t.Fatalf("label @%s says %q, the ref's element has text %q", ref, name, got)
		}
	}
	if got := e.js(agentA, "Array.prototype.some.call(document.querySelectorAll('div'), function (d) { return d.style.zIndex === '2147483647'; })"); got != "false" {
		t.Fatal("the overlay is still in the page after the capture")
	}
	a, err1 := os.ReadFile(plain)
	b, err2 := os.ReadFile(annotated)
	if err1 != nil || err2 != nil || string(a) == string(b) {
		t.Fatalf("the annotated capture equals the plain one (%v, %v): no overlay was drawn", err1, err2)
	}
	if out := e.must(agentA, "screenshot", "--if-changed", filepath.Join(dir, "1.png")); !strings.HasPrefix(out, "Screenshot saved") {
		t.Fatalf("first --if-changed: %q", out)
	}
	again := filepath.Join(dir, "2.png")
	if out := e.must(agentA, "screenshot", "--if-changed", again); out != "unchanged" {
		t.Fatalf("identical --if-changed: %q", out)
	}
	if _, err := os.Stat(again); !os.IsNotExist(err) {
		t.Fatal("an unchanged capture wrote a file")
	}
}

// 7.6 item 11 on WebKitGTK: tab list, tab new and tab close (MiniBrowser
// lists the windows that window.open creates), tab selection; frames and
// find with an action.
func TestTabsFramesAndFindOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "dialog.html", false)
	list := e.must(agentA, "tab", "list")
	if !strings.Contains(list, "t1  \"Dialogs\"") {
		t.Fatalf("tab list:\n%s", list)
	}
	out := e.must(agentA, "tab", "new", "--label", "second", e.sites.a+"/second.html")
	if !strings.HasPrefix(out, "opened tab t2") {
		t.Fatalf("tab new: %q", out)
	}
	{
		if title := e.js(agentA, "document.title"); title != "\"Second\"" {
			t.Fatalf("after tab new the session runs on %s, want the new tab", title)
		}
		if out := e.must(agentA, "tab", "t1"); !strings.Contains(out, "tab t1 is now the current page") || e.js(agentA, "document.title") != "\"Dialogs\"" {
			t.Fatalf("tab t1: %q", out)
		}
		if out := e.must(agentA, "tab", "close", "second"); out != "closed tab second" {
			t.Fatalf("tab close: %q", out)
		}
		if list := e.must(agentA, "tab", "list"); strings.Contains(list, "Second") {
			t.Fatalf("closed tab still listed:\n%s", list)
		}
	}

	e.must(agentA, "open", e.sites.a+"/frames.html?other="+e.sites.b)
	// The cross-site document has loaded once the parent cannot reach it.
	e.eventually(agentA, "document.getElementById('other').contentDocument === null", 5e9)
	// Review finding 3: both frames become the session's scope; WebKitGTK
	// runs the cross-origin frame in the page's process, with its own
	// execution context (TestFrameScopesLaterCommandsOnWebKitGTK checks
	// the commands that follow).
	if out := e.must(agentA, "frame", "#same"); !strings.Contains(out, "frame #same (second.html): later commands") {
		t.Fatalf("frame #same: %q", out)
	}
	e.must(agentA, "frame", "main")
	if out := e.must(agentA, "frame", "#other"); !strings.Contains(out, "frame #other ("+e.sites.b+"/child.html): later commands") {
		t.Fatalf("frame #other: %q", out)
	}
	if out := e.must(agentA, "frame", "main"); out != "commands run in the main document" {
		t.Fatalf("frame main: %q", out)
	}

	e.must(agentA, "open", e.sites.a+"/dialog.html")
	e.must(agentA, "dialog", "accept")
	out = e.must(agentA, "find", "role", "button", "click", "--name", "Confirm", "--exact")
	if got := e.js(agentA, "document.getElementById('out').textContent"); got != "\"confirm:true\"" || !strings.HasPrefix(out, "@e") {
		t.Fatalf("find role button click --name Confirm: page %s, output %q", got, out)
	}
	if _, code, msg := e.run(agentA, "find", "role", "button", "click"); code != 1 || !strings.Contains(msg, "matched 3 elements") {
		t.Fatalf("ambiguous find click: exit %d, %q", code, msg)
	}
}

// Design 3.8 (review finding 3): "frame" makes an iframe the session's
// frame scope; later reads and actions run in that frame's document,
// same-origin and cross-origin (one process on WebKitGTK, the frame's own
// execution context), until "frame main" or a navigation of the top
// document.
func TestFrameScopesLaterCommandsOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "nav.html", false)
	e.must(agentA, "open", e.sites.a+"/frames.html?other="+e.sites.b)
	e.eventually(agentA, "document.getElementById('other').contentDocument === null && "+
		"document.getElementById('kid').contentDocument.getElementById('press') !== null && "+
		"document.getElementById('same').contentDocument.querySelector('h1') !== null", 5e9)
	text := func(target string) string { return strings.TrimSpace(e.must(agentA, "get", "text", target)) }

	e.must(agentA, "frame", "#same")
	if got := text("h1"); got != "Second page" {
		t.Fatalf("after frame #same: get text h1 = %q, want the frame's heading \"Second page\"", got)
	}
	if out := e.must(agentA, "frame", "main"); !strings.Contains(out, "main document") {
		t.Fatalf("frame main: %q", out)
	}
	if got := text("h1"); got != "Frames" {
		t.Fatalf("after frame main: get text h1 = %q, want the top document's heading \"Frames\"", got)
	}

	// An action by selector and one by a ref of the frame's snapshot run in
	// the frame; the top document reads the final state.
	e.must(agentA, "frame", "#kid")
	e.must(agentA, "click", "#press")
	if got := e.js(agentA, "document.getElementById('kid').contentDocument.getElementById('press').textContent"); got != `"pressed 1"` {
		t.Fatalf("after click #press in frame #kid the frame's button says %s, want \"pressed 1\"", got)
	}
	snap := e.must(agentA, "snapshot")
	m := regexp.MustCompile(`button "pressed 1" \[ref=(e[0-9]+)`).FindStringSubmatch(snap)
	if m == nil || !strings.Contains(snap, `heading "Child page"`) || strings.Contains(snap, `heading "Frames"`) {
		t.Fatalf("snapshot in frame #kid is not the frame's document:\n%s", snap)
	}
	e.must(agentA, "click", "@"+m[1])
	if got := e.js(agentA, "document.getElementById('kid').contentDocument.getElementById('press').textContent + ' ' + document.querySelector('h1').textContent"); got != `"pressed 2 Frames"` {
		t.Fatalf("after click @%s the page shows %s, want the frame's button pressed twice and the top heading unchanged", m[1], got)
	}

	// A cross-origin frame: the top document cannot read it, so the final
	// state is read in the frame scope.
	e.must(agentA, "frame", "main")
	e.must(agentA, "frame", "#other")
	if got := text("h1"); got != "Child page" {
		t.Fatalf("after frame #other: get text h1 = %q, want \"Child page\"", got)
	}
	e.must(agentA, "click", "#press")
	if got := text("#press"); got != "pressed 1" {
		t.Fatalf("after click #press in the cross-origin frame: %q, want \"pressed 1\"", got)
	}
	if got := e.js(agentA, "document.querySelector('h1').textContent + ' ' + (document.getElementById('other').contentDocument === null)"); got != `"Frames true"` {
		t.Fatalf("top document after the cross-origin click: %s", got)
	}

	// The scope belongs to the document: a navigation of the top
	// document ends it.
	e.must(agentA, "open", e.sites.a+"/nav.html")
	if got := text("h1"); got != "Navigation" {
		t.Fatalf("after open: get text h1 = %q, want the new top document's heading \"Navigation\"", got)
	}
}

// Design 3.9 (review finding 6): an element capture labels only the refs
// whose boxes intersect the element, draws them into the image, and the
// overlay is gone afterwards; also for an element below the viewport.
func TestAnnotatedElementCaptureOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "annotate.html", false)
	dir := t.TempDir()
	plain, annotated := filepath.Join(dir, "plain.png"), filepath.Join(dir, "far.png")
	e.must(agentA, "screenshot", "--selector", "#far", plain)
	out := e.must(agentA, "screenshot", "--annotate", "--selector", "#far", annotated)
	if !strings.Contains(out, `button "Far below"`) || strings.Contains(out, `"Save"`) || strings.Contains(out, `"Help"`) || strings.Contains(out, `"Search"`) {
		t.Fatalf("element capture of #far labels other elements or misses it:\n%s", out)
	}
	a, err1 := os.ReadFile(plain)
	b, err2 := os.ReadFile(annotated)
	if err1 != nil || err2 != nil || string(a) == string(b) {
		t.Fatalf("the annotated element capture equals the plain one (%v, %v): its label is not in the image", err1, err2)
	}
	if got := e.js(agentA, overlayPresent); got != "false" {
		t.Fatal("the overlay is still in the page after the element capture")
	}
}

// overlayPresent is true while the annotation overlay host is in the page.
const overlayPresent = "Array.prototype.some.call(document.querySelectorAll('div'), function (d) { return d.style.zIndex === '2147483647'; })"

// Design 3.9 and 2.6 (review finding 5): when the capture's reply never
// arrives and the command deadline passes, the command ends with 124 and
// the overlay is still removed (by a fenced cleanup operation).
func TestAnnotatedCaptureTimeoutRemovesOverlayOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "annotate.html", false)
	e.tap.set(func(h *tapHooks) {
		h.drop = func(method string, _ json.RawMessage) bool { return method == "Page.snapshotRect" }
	})
	_, code, msg := e.run(agentA, "screenshot", "--annotate", "--timeout", "2s", filepath.Join(t.TempDir(), "timeout.png"))
	if code != 124 {
		t.Fatalf("screenshot with a dropped capture reply: exit %d (%q), want 124", code, msg)
	}
	e.eventually(agentA, "!("+overlayPresent+")", 3*time.Second)
	if got := e.js(agentA, "document.getElementById('save').textContent"); got != `"Save"` {
		t.Fatalf("page after the cleanup: %s", got)
	}
}

// navMCPEnv runs registry MCP tools in the test daemon for agentA, as the
// MCP server's registryEnv (cmd/iwdp-mcp) does.
type navMCPEnv struct{ e *navEnv }

func (navMCPEnv) Page(context.Context) (*core.PageSession, error) {
	return nil, errors.New("registry tools run in the daemon")
}
func (navMCPEnv) Emit(string)                 {}
func (navMCPEnv) ResolvePath(p string) string { return p }
func (m navMCPEnv) RunRemote(ctx context.Context, c *commands.Command, in commands.Input) (commands.Result, error) {
	r, err := m.e.c.Run(ctx, daemon.RunRequest{Argv: commands.Argv(c, in), TimeoutMs: 30000, Owner: daemon.Owner{Session: agentA}}, nil)
	if err != nil {
		return commands.Result{}, err
	}
	return commands.Result{Text: r.Text, Data: r.Data}, nil
}

// Review finding 7: take_screenshot through MCP reaches the page with
// annotate; the result has the labels and the image, and the overlay is
// gone afterwards.
func TestAnnotatedScreenshotThroughMCPOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "annotate.html", false)
	ctx := context.Background()
	impl := &mcp.Implementation{Name: "w3b-nav-test", Version: "0"}
	server := mcp.NewServer(impl, nil)
	commands.RegisterMCP(server, commands.ProfileCore, func(context.Context, commands.Globals) (commands.Env, error) { return navMCPEnv{e}, nil })
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(impl, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "take_screenshot", Arguments: map[string]any{"annotate": true}})
	if err != nil || res.IsError || len(res.Content) != 2 {
		t.Fatalf("take_screenshot annotate: %v %+v", err, res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, `button "Save"`) || strings.Contains(text, `"Far below"`) {
		t.Fatalf("labels:\n%s", text)
	}
	img, err := png.Decode(bytes.NewReader(res.Content[1].(*mcp.ImageContent).Data))
	if err != nil || img.Bounds().Dx() == 0 {
		t.Fatalf("image content is no PNG: %v", err)
	}
	if got := e.js(agentA, overlayPresent); got != "false" {
		t.Fatal("the overlay is still in the page after the MCP capture")
	}
}

// W3b-R2 finding 1 (design 2.4 row "Page-side runtime state", 3.7
// "Release restores the original functions"): the session's dialog
// override installed in a frame context is released with the session: the
// child's confirm is native again after "session release".
func TestFrameDialogOverrideIsReleasedOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "frames.html", false)
	e.eventually(agentA, "document.getElementById('kid').contentDocument.getElementById('press') !== null", 5*time.Second)
	native := "String(document.getElementById('kid').contentWindow.confirm).indexOf('[native code]') >= 0"
	e.must(agentA, "frame", "#kid")
	e.must(agentA, "dialog", "accept")
	if got := e.js(agentA, native); got != "false" {
		t.Fatalf("the dialog override is not installed in the child frame: native=%s", got)
	}
	e.must(agentA, "session", "release")
	if got := e.js(agentA, native); got != "true" {
		t.Fatalf("the child's confirm is still overridden after session release: native=%s", got)
	}
}

// W3b-R2 finding 2 (design 2.4: a domain is disabled only when its last
// owner leaves): with a legacy Runtime owner live, "frame" finds the
// child's context and "frame main" ends the scope without any
// Runtime.disable on the wire.
func TestFrameKeepsAnotherRuntimeOwnerOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "frames.html", false)
	e.eventually(agentA, "document.getElementById('kid').contentDocument.getElementById('press') !== null", 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.m.Do(ctx, core.PageRequest{}, core.AcquireOptions{Session: agentA}, "legacy runtime", core.ClassExclusive, false,
		func(ctx context.Context, mo *core.ManagedOp) error {
			_, err := e.m.Raw(ctx, mo.ID, "Runtime.enable", nil)
			return err
		}); err != nil {
		t.Fatalf("legacy Runtime.enable: %v", err)
	}
	e.must(agentA, "frame", "#kid")
	if got := strings.TrimSpace(e.must(agentA, "get", "text", "h1")); got != "Child page" {
		t.Fatalf("after frame #kid: get text h1 = %q, want the child's heading", got)
	}
	e.must(agentA, "frame", "main")
	for _, r := range withoutAgentResets(e.tap.records()) {
		if r.Out && r.Method == "Runtime.disable" {
			t.Fatalf("Runtime.disable was sent while the legacy owner holds Runtime (record %d)", r.Seq)
		}
	}
}

// W3b-R2 finding 3 (design 2.10, 3.9): the overlay of a capture whose
// reply was lost is removed before a read that queued behind the capture
// runs.
func TestAnnotatedCaptureCleanupPrecedesQueuedSuccessorOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "annotate.html", false)
	e.tap.set(func(h *tapHooks) {
		h.drop = func(method string, _ json.RawMessage) bool { return method == "Page.snapshotRect" }
	})
	done := make(chan int, 1)
	go func() {
		_, code, _, _ := e.runAs(daemon.Owner{Session: agentA}, "screenshot", "--annotate", "--timeout", "2s", filepath.Join(t.TempDir(), "first.png"))
		done <- code
	}()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		seen := false
		for _, r := range e.tap.records() {
			if r.Out && r.Method == "Page.snapshotRect" {
				seen = true
			}
		}
		if seen {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the capture was never sent")
		}
	}
	// Queued behind the capture, before its deadline.
	got := e.js(agentA, overlayPresent)
	if code := <-done; code != 124 {
		t.Fatalf("the capture with a lost reply exited %d, want 124", code)
	}
	if got != "false" {
		t.Fatal("the read queued behind the failed capture saw its overlay")
	}
}
