package cdp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type lifecycle struct {
	FrameID  string `json:"frameId"`
	LoaderID string `json:"loaderId"`
	Name     string `json:"name"`
}

type navFrame struct {
	Frame struct {
		ID       string `json:"id"`
		LoaderID string `json:"loaderId"`
		URL      string `json:"url"`
		ParentID string `json:"parentId"`
	} `json:"frame"`
}

// Design 7.8 item 5 (5.2 "Navigation identity"): the loaderId returned by
// Page.navigate equals the one in frameNavigated and in the lifecycle
// events of the new document; a same-document navigation returns no
// loader id and emits navigatedWithinDocument; lifecycle events carry the
// page session id. (requestId == loaderId of the main-frame document
// request is W5c's: Network events; events.go names the hook.)
func TestNavigationIdentity(t *testing.T) {
	r, fp, c, sid, target := attachPage(t)
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	seq := c.Seq()
	call(t, c, sid, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": true})
	// networkIdle follows once the network log is quiet (network_idle_test.go).
	waitEvent(t, c, seq, sid, "Page.lifecycleEvent", func(p json.RawMessage) bool { return strings.Contains(string(p), `"networkIdle"`) })
	var names []string
	var first string
	for _, e := range sessionEvents(c, seq, sid, "Page.lifecycleEvent") {
		l := decode[lifecycle](t, e.Params)
		names = append(names, l.Name)
		first = l.LoaderID
		if l.FrameID != target {
			t.Fatalf("lifecycle event of frame %q, want the main frame %q", l.FrameID, target)
		}
	}
	if strings.Join(names, ",") != "init,DOMContentLoaded,load,networkAlmostIdle,networkIdle" {
		t.Fatalf("lifecycle replay of a complete document: %v", names)
	}
	tree := decode[struct{ FrameTree navFrame }](t, call(t, c, sid, "Page.getFrameTree", nil)).FrameTree
	if tree.Frame.ID != target || tree.Frame.LoaderID != first || first == "" {
		t.Fatalf("frame tree %+v, want main frame %s with the lifecycle's loader %s", tree, target, first)
	}

	seq = c.Seq()
	nav := decode[struct{ FrameID, LoaderID string }](t, call(t, c, sid, "Page.navigate", map[string]any{"url": "https://one.example/two"}))
	if nav.FrameID != target || nav.LoaderID == "" || nav.LoaderID == first {
		t.Fatalf("Page.navigate = %+v, want frame %s and a new loader id (old %s)", nav, target, first)
	}
	fn := decode[navFrame](t, waitEvent(t, c, seq, sid, "Page.frameNavigated", nil).Params)
	if fn.Frame.ID != target || fn.Frame.LoaderID != nav.LoaderID || fn.Frame.URL != "https://one.example/two" {
		t.Fatalf("frameNavigated %+v, want frame %s with loader %s", fn, target, nav.LoaderID)
	}
	fp.f.Emit("Page.domContentEventFired", map[string]any{"timestamp": 1.5})
	fp.f.Emit("Page.loadEventFired", map[string]any{"timestamp": 2.5})
	waitEvent(t, c, seq, sid, "Page.lifecycleEvent", func(p json.RawMessage) bool { return strings.Contains(string(p), `"networkIdle"`) })
	names = nil
	for _, e := range sessionEvents(c, seq, sid, "Page.lifecycleEvent") {
		l := decode[lifecycle](t, e.Params)
		names = append(names, l.Name)
		if l.LoaderID != nav.LoaderID {
			t.Fatalf("lifecycle %s carries loader %s, want the navigation's %s", l.Name, l.LoaderID, nav.LoaderID)
		}
	}
	if strings.Join(names, ",") != "init,DOMContentLoaded,load,networkAlmostIdle,networkIdle" {
		t.Fatalf("lifecycle of the new document: %v", names)
	}
	if ev := sessionEvents(c, seq, sid, "Page.loadEventFired"); len(ev) != 1 || decode[struct{ Timestamp float64 }](t, ev[0].Params).Timestamp != 2.5 {
		t.Fatalf("loadEventFired %v", ev)
	}
	// Lifecycle events carry the page session id, and only it.
	for _, e := range c.Events() {
		if e.Method == "Page.lifecycleEvent" && e.SessionID != sid {
			t.Fatalf("a lifecycle event on session %q, want only the page session %q", e.SessionID, sid)
		}
	}

	// A same-document navigation: no loader id, navigatedWithinDocument,
	// no new document.
	seq = c.Seq()
	same := decode[map[string]any](t, call(t, c, sid, "Page.navigate", map[string]any{"url": "https://one.example/two#part"}))
	if same["frameId"] != target || same["loaderId"] != nil {
		t.Fatalf("same-document Page.navigate = %v, want the frame id and no loader id", same)
	}
	within := decode[struct{ FrameID, URL, NavigationType string }](t, waitEvent(t, c, seq, sid, "Page.navigatedWithinDocument", nil).Params)
	if within.FrameID != target || within.URL != "https://one.example/two#part" || within.NavigationType != "fragment" {
		t.Fatalf("navigatedWithinDocument %+v", within)
	}
	fp.mu.Lock()
	fp.url = "https://one.example/three" // history.pushState changes the document's URL
	fp.mu.Unlock()
	fp.consoleDebug(navMarker + `{"u":"https://one.example/three","t":"historyApi"}`)
	waitEvent(t, c, seq, sid, "Page.navigatedWithinDocument", func(p json.RawMessage) bool { return strings.Contains(string(p), "historyApi") })
	for _, m := range []string{"Page.frameNavigated", "Runtime.executionContextsCleared", "Runtime.consoleAPICalled"} {
		if ev := sessionEvents(c, seq, sid, m); len(ev) != 0 {
			t.Fatalf("a same-document navigation sent %s: %v", m, ev)
		}
	}
	tree = decode[struct{ FrameTree navFrame }](t, call(t, c, sid, "Page.getFrameTree", nil)).FrameTree
	if tree.Frame.LoaderID != nav.LoaderID || tree.Frame.URL != "https://one.example/three" {
		t.Fatalf("frame tree after same-document navigations %+v, want loader %s and the new URL", tree, nav.LoaderID)
	}

	// A provisional swap: the new target reports the same WIP loader id
	// string, which is another document (2.7: target and loader).
	f := r.fake["One"]
	seq = c.Seq()
	f.Swap("wk-One-b", 20*time.Millisecond)
	eventually(t, 5*time.Second, "the swap's commit", func() bool { return f.ActiveTarget() == "wk-One-b" })
	f.EmitFrom("wk-One-b", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main", "loaderId": "0.2", "url": "https://two.example/", "securityOrigin": "https://two.example", "mimeType": "text/html"}})
	swapped := decode[navFrame](t, waitEvent(t, c, seq, sid, "Page.frameNavigated", nil).Params)
	if swapped.Frame.LoaderID == nav.LoaderID || swapped.Frame.ID != target {
		t.Fatalf("frameNavigated of the swapped-in document %+v: want frame %s with a loader other than %s", swapped, target, nav.LoaderID)
	}
	if ev := sessionEvents(c, seq, sid, "Runtime.executionContextsCleared"); len(ev) != 1 {
		t.Fatalf("executionContextsCleared after the swap: %v", ev)
	}
}

// Page.navigate refuses a child frame and an invalid URL without
// navigating.
func TestNavigateRefusals(t *testing.T) {
	_, fp, c, sid, _ := attachPage(t)
	for _, p := range []map[string]any{{"url": "https://x.example/", "frameId": "child"}, {"url": "not a url"}} {
		if _, err := c.Call(ctxFor(t, 5*time.Second), sid, "Page.navigate", p); err == nil {
			t.Fatalf("Page.navigate %v succeeded", p)
		}
	}
	for _, e := range fp.expressions() {
		if strings.Contains(e, "location.href =") {
			t.Fatalf("a refused navigation ran %q", e)
		}
	}
}
