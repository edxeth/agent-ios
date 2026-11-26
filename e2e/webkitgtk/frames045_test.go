//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/daemon"
)

// Iframe parity with agent-browser 0.45 (contract section 9): frame
// selection of nested, unnamed and cross-origin frames, eval in the
// selected frame with its origin, find, wait and check inside a frame,
// console and errors of cross-origin frames, and annotation boxes of refs
// inside scrolled and scaled frames in page coordinates. WebKitGTK runs a
// cross-origin frame in the page's process with its own execution context,
// as iOS Safari without site isolation does.

// frames045Ready: every frame of frames045.html has loaded; a cross-origin
// frame has loaded once the parent cannot reach its document.
const frames045Ready = "document.querySelector('.xo').contentDocument === null && " +
	"document.querySelector('iframe[title=Outer]').contentDocument.querySelector('iframe') !== null && " +
	"document.querySelector('iframe[title=Outer]').contentDocument.querySelector('iframe').contentDocument === null && " +
	"document.getElementById('scrolled').contentDocument.getElementById('inside') !== null && " +
	"document.getElementById('scaled').contentDocument.getElementById('inside') !== null"

func newFrames045(t *testing.T) *navEnv {
	t.Helper()
	e := newNavEnv(t, "nav.html", false)
	e.must(agentA, "open", e.sites.a+"/frames045.html?other="+e.sites.b)
	e.eventually(agentA, frames045Ready, 10*time.Second)
	return e
}

// data runs a command line for session and returns its JSON data.
func (e *navEnv) data(session string, argv ...string) map[string]any {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	// JSON: the request of a client run with --json, whose data this reads
	// (eval reads the origin only then).
	r, err := e.c.Run(ctx, daemon.RunRequest{Argv: argv, Deadline: time.Now().Add(30 * time.Second), JSON: true, Owner: daemon.Owner{Session: session}}, nil)
	if err != nil {
		var re *daemon.RemoteError
		if errors.As(err, &re) {
			e.t.Fatalf("%s: exit %d: %s", strings.Join(argv, " "), re.E.Code, re.E.Message)
		}
		e.t.Fatalf("%s: %v", strings.Join(argv, " "), err)
	}
	var m map[string]any
	if err := json.Unmarshal(r.Data, &m); err != nil {
		e.t.Fatalf("%s: data %s: %v", strings.Join(argv, " "), r.Data, err)
	}
	return m
}

// frame waits until the selected frame's document has a <h1> and returns
// its text.
func (e *navEnv) frameHeading() string {
	e.t.Helper()
	e.must(agentA, "wait", "h1", "--state", "attached")
	return strings.TrimSpace(e.must(agentA, "get", "text", "h1"))
}

// Contract 9: frame resolves an unnamed same-origin iframe by CSS, a
// cross-origin iframe nested in it by CSS in the selected frame, an
// unnamed cross-origin iframe of the top document, and an iframe by a ref
// of the top snapshot, also one nested in a same-origin frame.
func TestFrame045SelectsNestedUnnamedAndCrossOriginFrames(t *testing.T) {
	e := newFrames045(t)

	e.must(agentA, "frame", "iframe[title=Outer]")
	if got := e.frameHeading(); got != "Outer 045" {
		t.Fatalf("frame iframe[title=Outer]: h1 %q, want the unnamed same-origin frame's \"Outer 045\"", got)
	}
	// Nested: the selector is resolved in the selected frame.
	e.must(agentA, "frame", "iframe")
	if got := e.frameHeading(); got != "Child 045" {
		t.Fatalf("nested frame: h1 %q, want \"Child 045\"", got)
	}
	if got := e.must(agentA, "eval", "location.search + ' ' + location.origin"); got != "?nested=1 "+e.sites.b {
		t.Fatalf("nested frame: eval says %q, want the nested cross-origin child", got)
	}
	e.must(agentA, "frame", "main")

	e.must(agentA, "frame", ".xo")
	if got := e.must(agentA, "eval", "location.search + ' ' + location.origin"); got != " "+e.sites.b {
		t.Fatalf("frame .xo: eval says %q, want the unnamed cross-origin child", got)
	}
	e.must(agentA, "frame", "main")

	// By ref: the top snapshot walks the same-origin Outer frame, whose
	// nested cross-origin iframe gets a ref too.
	snap := e.must(agentA, "snapshot")
	outer := regexp.MustCompile(`Iframe "Outer" \[ref=(e\d+)\]`).FindStringSubmatch(snap)
	if outer == nil {
		t.Fatalf("top snapshot has no ref for the Outer frame:\n%s", snap)
	}
	e.must(agentA, "frame", "@"+outer[1])
	if got := e.frameHeading(); got != "Outer 045" {
		t.Fatalf("frame @%s: h1 %q, want \"Outer 045\"", outer[1], got)
	}
	e.must(agentA, "frame", "main")
	nested := regexp.MustCompile(`(?m)^ +- Iframe \[ref=(e\d+)\]`).FindStringSubmatch(snap)
	if nested == nil {
		t.Fatalf("top snapshot has no ref for the iframe nested in Outer:\n%s", snap)
	}
	e.must(agentA, "frame", "@"+nested[1])
	if got := e.must(agentA, "eval", "location.search"); got != "?nested=1" {
		t.Fatalf("frame @%s (nested in Outer): eval says %q, want the nested child", nested[1], got)
	}
}

// Contract 9: eval after frame runs in the selected frame, and its JSON
// data names that frame's URL as origin (agent-browser data.origin); the
// top document's eval names the page URL.
func TestEval045RunsInTheSelectedFrameWithItsOrigin(t *testing.T) {
	e := newFrames045(t)
	top := e.data(agentA, "eval", "document.title")
	if top["value"] != "Frames 045" || top["origin"] != e.sites.a+"/frames045.html?other="+e.sites.b {
		t.Fatalf("top eval data %v: want value \"Frames 045\" and the page URL as origin", top)
	}
	e.must(agentA, "frame", ".xo")
	in := e.data(agentA, "eval", "document.title")
	if in["value"] != "Child 045" || in["origin"] != e.sites.b+"/child045.html" {
		t.Fatalf("eval in frame .xo: data %v, want value \"Child 045\" and origin %s/child045.html", in, e.sites.b)
	}
	// A Promise in the frame is awaited there.
	if got := e.must(agentA, "eval", "Promise.resolve(document.querySelector('h1').textContent)"); got != "Child 045" {
		t.Fatalf("awaited eval in frame: %q", got)
	}
	e.must(agentA, "frame", "main")
	if got := e.must(agentA, "eval", "document.title"); got != "Frames 045" {
		t.Fatalf("eval after frame main: %q, want the top document's title", got)
	}
}

// Contract 9: find, wait and check/uncheck act inside the selected
// cross-origin frame.
func TestFind045WaitAndCheckActInsideAFrame(t *testing.T) {
	e := newFrames045(t)
	e.must(agentA, "frame", ".xo")
	e.must(agentA, "check", "#agree")
	if got := e.must(agentA, "is", "checked", "#agree"); got != "true" {
		t.Fatalf("is checked #agree after check in the frame: %q", got)
	}
	if got := e.must(agentA, "eval", "document.getElementById('agree').checked"); got != "true" {
		t.Fatalf("the frame's checkbox after check: %q", got)
	}
	e.must(agentA, "uncheck", "#agree")
	if got := e.must(agentA, "eval", "document.getElementById('agree').checked"); got != "false" {
		t.Fatalf("the frame's checkbox after uncheck: %q", got)
	}
	out := e.must(agentA, "find", "role", "checkbox", "check", "--name", "Agree")
	if !strings.HasPrefix(out, "@e") || e.must(agentA, "eval", "document.getElementById('agree').checked") != "true" {
		t.Fatalf("find role checkbox check in the frame: %q", out)
	}
	if out := e.must(agentA, "find", "text", "Later"); !strings.Contains(out, `button "Later"`) {
		t.Fatalf("find text Later in the frame: %q", out)
	}
	e.must(agentA, "click", "#later")
	e.must(agentA, "wait", "#late")
	e.must(agentA, "wait", "--text", "Late text")
	if got := strings.TrimSpace(e.must(agentA, "get", "text", "#late")); got != "Late text" {
		t.Fatalf("get text #late in the frame: %q", got)
	}
}

// Contract 9: console and errors capture the messages and uncaught errors
// of cross-origin frames, also of a frame nested in another frame.
func TestConsole045AndErrorsCaptureCrossOriginFrames(t *testing.T) {
	e := newFrames045(t)
	e.must(agentA, "console")
	e.must(agentA, "errors")
	e.must(agentA, "frame", ".xo")
	e.must(agentA, "click", "#log")
	e.must(agentA, "frame", "main")
	e.must(agentA, "frame", "iframe[title=Outer]")
	e.must(agentA, "frame", "iframe")
	e.must(agentA, "click", "#log")
	e.must(agentA, "frame", "main")
	want := []string{"hello from child " + e.sites.b, "hello from nested " + e.sites.b}
	var out string
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		out = e.must(agentA, "console")
		if strings.Contains(out, want[0]) && strings.Contains(out, want[1]) {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("console does not show the frames' messages %q:\n%s", want, out)
		}
	}
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		out = e.must(agentA, "errors")
		if strings.Contains(out, "boom in child") && strings.Contains(out, "boom in nested") {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("errors does not show the frames' uncaught errors:\n%s", out)
		}
	}
}

// Contract 9: screenshot --annotate places the boxes of refs inside
// iframes in page coordinates, through the frame's offset, border,
// padding, scroll position and CSS scale. The expected boxes are computed
// by hand from frames045.html and scroll045.html (README.md there).
func TestAnnotate045BoxesInsideScrolledAndScaledFrames(t *testing.T) {
	e := newFrames045(t)
	e.js(agentA, "document.getElementById('scrolled').contentWindow.scrollTo(0, 350); document.getElementById('scaled').contentWindow.scrollTo(0, 350); window.scrollTo(0, 0); 1")
	d := e.data(agentA, "screenshot", "--annotate", filepath.Join(t.TempDir(), "a.png"))
	labels, _ := d["labels"].([]any)
	var boxes []map[string]any
	for _, l := range labels {
		m, _ := l.(map[string]any)
		if m["name"] == "Inside" {
			b, _ := m["box"].(map[string]any)
			boxes = append(boxes, b)
		}
	}
	// #scrolled: 37+5+7+20 = 69, 253+5+7+(400-350) = 315.
	// #scaled: 400+(3+2+20)*2 = 450, 253+(3+2+50)*2 = 363, size doubled.
	want := []map[string]any{
		{"x": 69.0, "y": 315.0, "width": 100.0, "height": 30.0},
		{"x": 450.0, "y": 363.0, "width": 200.0, "height": 60.0},
	}
	if len(boxes) != len(want) {
		t.Fatalf("annotation labels %v: want two \"Inside\" buttons with boxes", labels)
	}
	for i := range want {
		for k, v := range want[i] {
			if boxes[i][k] != v {
				t.Fatalf("box %d of \"Inside\" = %v, want %v (labels %v)", i, boxes[i], want[i], labels)
			}
		}
	}
	// An element capture gives the boxes relative to the element, as
	// agent-browser does: the scaled frame's box starts at 400, 253.
	d = e.data(agentA, "screenshot", "--annotate", "--selector", "#scaled", filepath.Join(t.TempDir(), "el.png"))
	var rel map[string]any
	for _, l := range d["labels"].([]any) {
		if m := l.(map[string]any); m["name"] == "Inside" {
			rel, _ = m["box"].(map[string]any)
		}
	}
	if rel == nil || rel["x"] != 50.0 || rel["y"] != 110.0 || rel["width"] != 200.0 || rel["height"] != 60.0 {
		t.Fatalf("element capture of #scaled: box of \"Inside\" %v, want x 50, y 110, 200x60 (labels %v)", rel, d["labels"])
	}
	// Scrolled out of the frame's viewport: no label.
	e.js(agentA, "document.getElementById('scrolled').contentWindow.scrollTo(0, 0); 1")
	d = e.data(agentA, "screenshot", "--annotate", filepath.Join(t.TempDir(), "b.png"))
	n := 0
	for _, l := range d["labels"].([]any) {
		if l.(map[string]any)["name"] == "Inside" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("with #scrolled at the top its button is outside the frame's viewport; labels %v, want only the scaled frame's", d["labels"])
	}
}

// Review F1 and F2 (frames-heap): annotation boxes are in the coordinates
// of the captured image, as agent-browser 0.45 (e2e_tests.rs: after a
// 120 px scroll the viewport boxes shift by -120, full-page boxes stay in
// document coordinates); an element capture of a ref inside a same-origin
// frame captures that element where the top page shows it, so its own
// label is at 0, 0 of the image.
func TestAnnotate045BoxesInImageCoordinates(t *testing.T) {
	e := newFrames045(t)
	e.js(agentA, "document.body.style.height = '3000px'; document.getElementById('scrolled').contentWindow.scrollTo(0, 350); window.scrollTo(0, 120); 1")
	inside := func(d map[string]any) map[string]any {
		for _, l := range d["labels"].([]any) {
			if m := l.(map[string]any); m["name"] == "Inside" {
				b, _ := m["box"].(map[string]any)
				return b
			}
		}
		t.Fatalf("no Inside label: %v", d["labels"])
		return nil
	}
	// #scrolled's Inside button is at 69, 315 of the document (TestAnnotate045BoxesInsideScrolledAndScaledFrames).
	if b := inside(e.data(agentA, "screenshot", "--annotate", filepath.Join(t.TempDir(), "v.png"))); b["x"] != 69.0 || b["y"] != 195.0 {
		t.Fatalf("viewport capture scrolled by 120: Inside box %v, want x 69, y 195", b)
	}
	if b := inside(e.data(agentA, "screenshot", "--annotate", "--full", filepath.Join(t.TempDir(), "f.png"))); b["x"] != 69.0 || b["y"] != 315.0 {
		t.Fatalf("full-page capture scrolled by 120: Inside box %v, want x 69, y 315", b)
	}
	m := regexp.MustCompile(`button "Inside" \[ref=(e\d+)\]`).FindStringSubmatch(e.must(agentA, "snapshot"))
	if m == nil {
		t.Fatal("no Inside ref in the snapshot")
	}
	b := inside(e.data(agentA, "screenshot", "--annotate", "--selector", "@"+m[1], filepath.Join(t.TempDir(), "el.png")))
	if b["x"] != 0.0 || b["y"] != 0.0 || b["width"] != 100.0 || b["height"] != 30.0 {
		t.Fatalf("element capture of @%s inside a frame: Inside box %v, want 0, 0, 100x30", m[1], b)
	}
}

// Closure review F13: when focus is inside a cross-origin frame, which the
// top document's runtime cannot reach, press refuses and sends nothing;
// with that frame selected, press goes to its focused element.
func TestPress045FocusInCrossOriginFrame(t *testing.T) {
	e := newFrames045(t)
	// WebKit lets no cross-origin frame take focus by script; the top
	// document focuses the frame itself, so its body has the focus.
	e.must(agentA, "frame", ".xo")
	e.must(agentA, "eval", "window.keys = []; document.addEventListener('keydown', function (k) { keys.push(k.key); }); 1")
	e.must(agentA, "frame", "main")
	e.must(agentA, "eval", "window.keys = []; document.addEventListener('keydown', function (k) { keys.push(k.key); }, true); document.querySelector('.xo').focus(); 1")
	_, code, msg := e.run(agentA, "--timeout", "3s", "press", "x")
	if code == 0 || !strings.Contains(msg, "cross-origin frame") || e.must(agentA, "eval", "'[' + keys.join() + ']'") != "[]" {
		t.Fatalf("press x with focus in a cross-origin frame: exit %d, %q, top keys %q; want a refusal naming the cross-origin frame and no key event",
			code, msg, e.must(agentA, "eval", "'[' + keys.join() + ']'"))
	}
	e.must(agentA, "frame", ".xo")
	if got := e.must(agentA, "eval", "'[' + keys.join() + ']'"); got != "[]" {
		t.Fatalf("the refused press reached the frame: keys %q", got)
	}
	e.must(agentA, "press", "x")
	if got := e.must(agentA, "eval", "'[' + keys.join() + ']'"); got != "[x]" {
		t.Fatalf("press x in the selected cross-origin frame: keydown keys %q, want x", got)
	}
}

// Closure review F14: a ref that crosses its frame's viewport edge is
// annotated with its visible part only: the reported box is cut at the
// frame's edge, and nothing is painted on the top page outside the
// frame; an element capture of that ref is refused, since its box shows
// pixels outside the frame.
func TestAnnotate045RefClippedByFrame(t *testing.T) {
	decodePNG := func(t *testing.T, path string) image.Image {
		t.Helper()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		img, err := png.Decode(f)
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return img
	}
	e := newFrames045(t)
	// #scrolled's Inside button is at y 400..430 of its frame document, whose
	// viewport starts at 253+5+7 = 265 of the page: scrolled by 415, 15 px
	// of it show at the top of the frame.
	e.js(agentA, "document.getElementById('scrolled').contentWindow.scrollTo(0, 415); document.getElementById('scaled').contentWindow.scrollTo(0, 0); window.scrollTo(0, 0); 1")
	dir := t.TempDir()
	plain, annotated := filepath.Join(dir, "plain.png"), filepath.Join(dir, "annotated.png")
	e.must(agentA, "screenshot", plain)
	d := e.data(agentA, "screenshot", "--annotate", annotated)
	var box map[string]any
	for _, l := range d["labels"].([]any) {
		if m := l.(map[string]any); m["name"] == "Inside" {
			box, _ = m["box"].(map[string]any)
		}
	}
	if box == nil || box["x"] != 69.0 || box["y"] != 265.0 || box["width"] != 100.0 || box["height"] != 15.0 {
		t.Fatalf("Inside button cut by its frame's top edge: box %v, want x 69, y 265, 100x15 (labels %v)", box, d["labels"])
	}
	// Between the Outer frame's annotations (to y 236) and the frame's
	// viewport (from y 265), right of #scrolled's own label: the page and
	// the frame's border and padding, where the uncut box's top edge and
	// a label above the cut box would be. Both captures are equal there,
	// but for #scrolled's own box edge (y 253 and 254).
	a, b := decodePNG(t, plain), decodePNG(t, annotated)
	iw, err := strconv.ParseFloat(e.js(agentA, "innerWidth"), 64)
	if err != nil || iw <= 0 {
		t.Fatalf("innerWidth: %v", err)
	}
	s := float64(a.Bounds().Dx()) / iw // image pixels per CSS pixel
	for y := int(237 * s); y < int(265*s); y++ {
		if y >= int(253*s) && y < int(255*s) {
			continue
		}
		for x := int(65 * s); x < int(200*s); x++ {
			if a.At(x, y) != b.At(x, y) {
				t.Fatalf("the annotation of a frame-clipped ref painted at image pixel %d, %d, outside the frame's viewport", x, y)
			}
		}
	}
	m := regexp.MustCompile(`button "Inside" \[ref=(e\d+)\]`).FindStringSubmatch(e.must(agentA, "snapshot", "-s", "#scrolled"))
	if m == nil {
		t.Fatal("no Inside ref in the snapshot of #scrolled")
	}
	_, code, msg := e.run(agentA, "screenshot", "--annotate", "--selector", "@"+m[1], filepath.Join(dir, "el.png"))
	if code == 0 || !strings.Contains(msg, "outside its frame") {
		t.Fatalf("element capture of a ref cut by its frame: exit %d, %q; want refused", code, msg)
	}
}
