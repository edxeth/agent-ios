package cdp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Tests of the Page methods that the acceptance tests (worlds_test.go,
// navigation_test.go, log_domain_test.go) do not reach. Expected results
// come from the CDP protocol (Page domain) and design 5.3's Page row.

// Page.getFrameTree lists the child frames of the current document under
// their parent (CDP FrameTree: frame, childFrames; no resources), with the
// page target id as the main frame's id and parentId; getResourceTree adds
// each frame's resources (CDP FrameResourceTree), keeping the failed and
// canceled flags.
func TestFrameAndResourceTree(t *testing.T) {
	_, fp, c, sid, target := attachPage(t)
	child := func(id, parent string, kids []wipTree, res ...string) wipTree {
		var rs []json.RawMessage
		for _, r := range res {
			rs = append(rs, json.RawMessage(r))
		}
		return wipTree{
			Frame:       wipFrame{ID: id, ParentID: parent, LoaderID: "L-" + id, Name: id + "-name", URL: "https://frames.example/" + id, SecurityOrigin: "https://frames.example", MimeType: "text/html"},
			ChildFrames: kids, Resources: rs,
		}
	}
	fp.mu.Lock()
	fp.children = []wipTree{
		child("c1", "main", []wipTree{child("g1", "c1", nil)}, `{"url":"https://frames.example/c1.css","type":"Stylesheet","mimeType":"text/css","failed":true}`),
		child("c2", "main", nil, `{"url":"https://frames.example/c2.png","type":"Image","mimeType":"image/png","canceled":true}`),
	}
	fp.mu.Unlock()

	type frame struct {
		ID, ParentID, LoaderID, URL, Name, SecurityOrigin, MimeType string
	}
	type resource struct {
		URL, Type, MimeType string
		Failed, Canceled    bool
	}
	type node struct {
		Frame       frame
		ChildFrames []node
		Resources   []resource
	}
	raw := call(t, c, sid, "Page.getFrameTree", nil)
	if strings.Contains(string(raw), `"resources"`) {
		t.Fatalf("getFrameTree carries resources: %s", raw)
	}
	tree := decode[struct{ FrameTree node }](t, raw).FrameTree
	if tree.Frame.ID != target || tree.Frame.ParentID != "" || tree.Frame.URL != "https://one.example/" || len(tree.ChildFrames) != 2 {
		t.Fatalf("frame tree %+v, want main frame %s with two children", tree, target)
	}
	c1, c2 := tree.ChildFrames[0], tree.ChildFrames[1]
	if c1.Frame.ID != "c1" || c1.Frame.ParentID != target || c1.Frame.Name != "c1-name" || c1.Frame.URL != "https://frames.example/c1" || c1.Frame.SecurityOrigin != "https://frames.example" || c1.Frame.MimeType != "text/html" {
		t.Fatalf("child frame c1 %+v", c1.Frame)
	}
	if c2.Frame.ID != "c2" || c2.Frame.ParentID != target || len(c2.ChildFrames) != 0 {
		t.Fatalf("child frame c2 %+v", c2)
	}
	if len(c1.ChildFrames) != 1 || c1.ChildFrames[0].Frame.ID != "g1" || c1.ChildFrames[0].Frame.ParentID != "c1" {
		t.Fatalf("grandchild of c1 %+v, want g1 under c1", c1.ChildFrames)
	}
	loaders := map[string]bool{tree.Frame.LoaderID: true, c1.Frame.LoaderID: true, c2.Frame.LoaderID: true, c1.ChildFrames[0].Frame.LoaderID: true}
	if len(loaders) != 4 || loaders[""] {
		t.Fatalf("loader ids %v, want one non-empty id per frame document", loaders)
	}

	rt := decode[struct{ FrameTree node }](t, call(t, c, sid, "Page.getResourceTree", nil)).FrameTree
	if rt.Frame.ID != target || rt.Frame.LoaderID != tree.Frame.LoaderID {
		t.Fatalf("resource tree main frame %+v, want the frame tree's %+v", rt.Frame, tree.Frame)
	}
	if want := []resource{{URL: "https://one.example/app.js", Type: "Script", MimeType: "text/javascript"}}; !slices.Equal(rt.Resources, want) {
		t.Fatalf("main frame resources %+v, want %+v", rt.Resources, want)
	}
	if len(rt.ChildFrames) != 2 {
		t.Fatalf("resource tree children %+v", rt.ChildFrames)
	}
	if want := []resource{{URL: "https://frames.example/c1.css", Type: "Stylesheet", MimeType: "text/css", Failed: true}}; !slices.Equal(rt.ChildFrames[0].Resources, want) {
		t.Fatalf("c1 resources %+v, want %+v", rt.ChildFrames[0].Resources, want)
	}
	if want := []resource{{URL: "https://frames.example/c2.png", Type: "Image", MimeType: "image/png", Canceled: true}}; !slices.Equal(rt.ChildFrames[1].Resources, want) {
		t.Fatalf("c2 resources %+v, want %+v", rt.ChildFrames[1].Resources, want)
	}
	if g := rt.ChildFrames[0].ChildFrames; len(g) != 1 || g[0].Resources == nil || len(g[0].Resources) != 0 {
		t.Fatalf("g1 in the resource tree %+v, want an empty resource list", g)
	}
}

// screenshotPage answers the viewport read with a 390x844 viewport
// scrolled to (0, 300) and Page.snapshotRect with a 4x3 PNG; it records
// the snapshotRect parameters.
func screenshotPage(t *testing.T, fp *fakePage, f *fakewebkit.Server) (pngB64 string, rects func() []map[string]any) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	for x := range 4 {
		for y := range 3 {
			img.Set(x, y, color.RGBA{R: uint8(60 * x), G: uint8(80 * y), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	pngB64 = base64.StdEncoding.EncodeToString(buf.Bytes())
	fp.mu.Lock()
	fp.eval = func(expr string) (any, bool) {
		if expr != viewportExpr {
			return nil, false
		}
		return value(map[string]any{
			"iw": 390, "ih": 844, "sx": 0, "sy": 300, "cw": 375, "ch": 844, "w": 375, "h": 3000,
			"vx": 0, "vy": 0, "vpx": 0, "vpy": 300, "vw": 390, "vh": 844, "sc": 1,
		}), true
	}
	fp.mu.Unlock()
	var mu sync.Mutex
	var got []map[string]any
	f.Handle("Page.snapshotRect", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p map[string]any
		_ = json.Unmarshal(c.Params, &p)
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
		return map[string]any{"dataURL": "data:image/png;base64," + pngB64}, nil
	})
	return pngB64, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), got...)
	}
}

// Page.captureScreenshot (design 5.3): WIP Page.snapshotRect of the
// viewport without a clip; a clip is in CDP page coordinates, sent in
// WebKit's Page coordinates with captureBeyondViewport and else in
// Viewport coordinates (minus the scroll offset); jpeg is encoded on the
// host; other formats and an empty area are refused.
func TestCaptureScreenshot(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	pngB64, rects := screenshotPage(t, fp, r.fake["One"])
	type shot struct{ Data string }

	got := decode[shot](t, call(t, c, sid, "Page.captureScreenshot", nil))
	if got.Data != pngB64 {
		t.Fatalf("png screenshot %q, want the snapshot's PNG", got.Data)
	}
	viewport := map[string]any{"x": 0.0, "y": 0.0, "width": 390.0, "height": 844.0, "coordinateSystem": "Viewport"}
	want := []map[string]any{viewport}

	got = decode[shot](t, call(t, c, sid, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 50}))
	raw, err := base64.StdEncoding.DecodeString(got.Data)
	if err != nil {
		t.Fatalf("jpeg screenshot is not base64: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("jpeg screenshot does not decode as JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 4 || b.Dy() != 3 {
		t.Fatalf("jpeg screenshot is %v, want the snapshot's 4x3", b)
	}
	want = append(want, viewport)

	call(t, c, sid, "Page.captureScreenshot", map[string]any{"clip": map[string]any{"x": 10.5, "y": 400, "width": 100.2, "height": 50, "scale": 1}})
	want = append(want, map[string]any{"x": 10.0, "y": 100.0, "width": 101.0, "height": 50.0, "coordinateSystem": "Viewport"})
	call(t, c, sid, "Page.captureScreenshot", map[string]any{"clip": map[string]any{"x": 10, "y": 400, "width": 100, "height": 50, "scale": 1}, "captureBeyondViewport": true})
	want = append(want, map[string]any{"x": 10.0, "y": 400.0, "width": 100.0, "height": 50.0, "coordinateSystem": "Page"})

	for _, p := range []map[string]any{
		{"format": "webp"},
		{"clip": map[string]any{"x": 0, "y": 300, "width": 0, "height": 10, "scale": 1}},
	} {
		if _, err := c.Call(ctxFor(t, 5*time.Second), sid, "Page.captureScreenshot", p); err == nil {
			t.Fatalf("captureScreenshot %v succeeded", p)
		}
	}
	gotRects := rects()
	if len(gotRects) != len(want) {
		t.Fatalf("snapshotRect calls %v, want %v", gotRects, want)
	}
	for i := range want {
		for k, v := range want[i] {
			if gotRects[i][k] != v {
				t.Fatalf("snapshotRect call %d = %v, want %v", i, gotRects[i], want[i])
			}
		}
	}
}

// Page.getLayoutMetrics reports the page's viewports and content size in
// CSS pixels, also as the css* fields.
func TestGetLayoutMetrics(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	screenshotPage(t, fp, r.fake["One"])
	type rect struct {
		PageX, PageY, OffsetX, OffsetY, ClientWidth, ClientHeight, Scale, Zoom, X, Y, Width, Height float64
	}
	m := decode[map[string]rect](t, call(t, c, sid, "Page.getLayoutMetrics", nil))
	layout := rect{PageX: 0, PageY: 300, ClientWidth: 375, ClientHeight: 844}
	visual := rect{PageY: 300, ClientWidth: 390, ClientHeight: 844, Scale: 1, Zoom: 1}
	content := rect{Width: 375, Height: 3000}
	for name, want := range map[string]rect{
		"layoutViewport": layout, "cssLayoutViewport": layout, "visualViewport": visual,
		"cssVisualViewport": visual, "contentSize": content, "cssContentSize": content,
	} {
		if got, ok := m[name]; !ok || got != want {
			t.Fatalf("%s = %+v (present %v), want %+v", name, got, ok, want)
		}
	}
}

// Page.getNavigationHistory lists the current entry only (id 0), and
// Page.navigateToHistoryEntry runs history.go with the entry id as the
// offset from it (design 5.3).
func TestNavigationHistory(t *testing.T) {
	_, fp, c, sid, _ := attachPage(t)
	fp.mu.Lock()
	fp.eval = func(expr string) (any, bool) {
		if expr == "[location.href, document.title]" {
			return value([]any{"https://one.example/a", "Page A"}), true
		}
		return nil, false
	}
	fp.mu.Unlock()
	h := decode[struct {
		CurrentIndex int
		Entries      []struct {
			ID                                       int
			URL, UserTypedURL, Title, TransitionType string
		}
	}](t, call(t, c, sid, "Page.getNavigationHistory", nil))
	if h.CurrentIndex != 0 || len(h.Entries) != 1 {
		t.Fatalf("history %+v, want the current entry only", h)
	}
	if e := h.Entries[0]; e.ID != 0 || e.URL != "https://one.example/a" || e.UserTypedURL != e.URL || e.Title != "Page A" || e.TransitionType == "" {
		t.Fatalf("current entry %+v", e)
	}
	call(t, c, sid, "Page.navigateToHistoryEntry", map[string]any{"entryId": -1})
	if !slices.Contains(fp.expressions(), "history.go(-1)") {
		t.Fatalf("navigateToHistoryEntry{-1} ran %v, want history.go(-1)", fp.expressions())
	}
}

// Page.reload goes to WIP Page.reload with ignoreCache;
// scriptToEvaluateOnLoad is refused without reloading. Page.stopLoading
// runs window.stop() (design 5.3).
func TestReloadAndStopLoading(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	if _, err := c.Call(ctxFor(t, 5*time.Second), sid, "Page.reload", map[string]any{"scriptToEvaluateOnLoad": "1"}); err == nil || !strings.Contains(err.Error(), "scriptToEvaluateOnLoad is not supported") {
		t.Fatalf("reload with scriptToEvaluateOnLoad: %v", err)
	}
	if n := len(f.ReceivedMethod("Page.reload")); n != 0 {
		t.Fatalf("a refused reload sent %d WIP Page.reload", n)
	}
	call(t, c, sid, "Page.reload", map[string]any{"ignoreCache": true})
	call(t, c, sid, "Page.reload", nil)
	rl := f.ReceivedMethod("Page.reload")
	if len(rl) != 2 || !strings.Contains(string(rl[0].Params), `"ignoreCache":true`) || !strings.Contains(string(rl[1].Params), `"ignoreCache":false`) {
		t.Fatalf("WIP Page.reload calls %v, want ignoreCache true then false", rl)
	}
	call(t, c, sid, "Page.stopLoading", nil)
	if !slices.Contains(fp.expressions(), "window.stop()") {
		t.Fatalf("stopLoading ran %v, want window.stop()", fp.expressions())
	}
}

// lastBoot is the source of the last Page.setBootstrapScript that the
// page received.
func lastBoot(t *testing.T, f *fakewebkit.Server) string {
	t.Helper()
	boots := f.ReceivedMethod("Page.setBootstrapScript")
	if len(boots) == 0 {
		t.Fatal("no Page.setBootstrapScript")
	}
	return decode[struct{ Source string }](t, boots[len(boots)-1].Params).Source
}

// Page.addScriptToEvaluateOnNewDocument puts each script in the
// attachment's bootstrap part (WIP runs the bootstrap script in every new
// document), each by indirect eval in its own try block, in order;
// runImmediately also runs it in the current document.
// removeScriptToEvaluateOnNewDocument takes it out of the part, and the
// detach removes the attachment's part. (WebKitGTK runs the scripts:
// e2e/webkitgtk/cdp_w5b1_test.go.)
func TestScriptsOnNewDocument(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	first := decode[struct{ Identifier string }](t, call(t, c, sid, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.A = 1"})).Identifier
	second := decode[struct{ Identifier string }](t, call(t, c, sid, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.B = 2", "runImmediately": true})).Identifier
	if first == "" || first == second {
		t.Fatalf("identifiers %q and %q, want two distinct ones", first, second)
	}
	a, b := `try { (0, eval)("window.A = 1"); } catch (e) {}`, `try { (0, eval)("window.B = 2"); } catch (e) {}`
	boot := lastBoot(t, f)
	if ia, ib := strings.Index(boot, a), strings.Index(boot, b); ia < 0 || ib < ia {
		t.Fatalf("bootstrap script %q, want A then B", boot)
	}
	if !slices.Contains(fp.expressions(), "window.B = 2") || slices.Contains(fp.expressions(), "window.A = 1") {
		t.Fatalf("expressions %v, want only the runImmediately script run now", fp.expressions())
	}

	n := len(f.ReceivedMethod("Page.setBootstrapScript"))
	call(t, c, sid, "Page.removeScriptToEvaluateOnNewDocument", map[string]any{"identifier": "no-such-script"})
	if got := len(f.ReceivedMethod("Page.setBootstrapScript")); got != n {
		t.Fatalf("removing an unknown script sent %d bootstrap scripts", got-n)
	}
	call(t, c, sid, "Page.removeScriptToEvaluateOnNewDocument", map[string]any{"identifier": first})
	if boot := lastBoot(t, f); strings.Contains(boot, a) || !strings.Contains(boot, b) {
		t.Fatalf("bootstrap script after removing A: %q", boot)
	}

	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
	eventually(t, 5*time.Second, "the detach's bootstrap script", func() bool {
		return !strings.Contains(lastBoot(t, f), "window.B")
	})
}

// Page.bringToFront is a probe: it succeeds when the tab answers and
// names the cause when it does not (design 3.8: WIP cannot raise a tab).
// Page.setBypassCSP and Page.handleJavaScriptDialog return the design's
// unsupported errors (5.2, 5.3) and send nothing to the page.
func TestBringToFrontAndRefusals(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Page.bringToFront", nil)
	before := len(f.Received())
	for method, msg := range map[string]string{
		"Page.setBypassCSP":           "bypassing the Content Security Policy is not supported on iOS WebKit",
		"Page.handleJavaScriptDialog": "dynamic dialog handling is not supported on iOS WebKit",
	} {
		_, err := c.Call(ctxFor(t, 5*time.Second), sid, method, map[string]any{"enabled": true, "accept": true})
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Fatalf("%s: %v, want %q", method, err, msg)
		}
	}
	if got := received(f)[before:]; len(got) != 0 {
		t.Fatalf("the refused methods sent %v to the page", got)
	}
	f.Stall("Runtime.evaluate")
	start := time.Now()
	_, err := c.Call(ctxFor(t, 10*time.Second), sid, "Page.bringToFront", nil)
	if err == nil || !strings.Contains(err.Error(), "is not answering; it is probably not the visible Safari tab") {
		t.Fatalf("bringToFront of a stalled tab: %v", err)
	}
	if d := time.Since(start); d > probeWait+3*time.Second {
		t.Fatalf("bringToFront of a stalled tab took %v, want about %v", d, probeWait)
	}
}
