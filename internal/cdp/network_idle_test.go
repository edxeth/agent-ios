package cdp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
	"github.com/edxeth/agent-ios/internal/devtools"
)

// quietCheck is how long a test waits to show that networkIdle does not
// come: well past the idle quiet time.
const quietCheck = 3 * devtools.DefaultIdleQuiet

// Design 5.3 (Page row: "lifecycleEvent (init, DOMContentLoaded, load,
// networkIdle synthesized)") with Chrome's meaning of networkIdle (no
// request in flight for 500 ms): the bridge reports networkAlmostIdle and
// networkIdle of a loaded document from the session's network log (W4's
// tracker of "wait --load networkidle"), which
// Page.setLifecycleEventsEnabled starts. A request in flight holds them
// back; a document replaced before it is idle gets none; a replay after
// idleness reports them at once.
func TestNetworkIdleLifecycle(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	isIdle := func(p json.RawMessage) bool { return strings.Contains(string(p), `"name":"networkIdle"`) }
	call(t, c, sid, "Page.enable", nil)
	seq := c.Seq()
	call(t, c, sid, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": true})
	if n := len(f.ReceivedMethod("Network.enable")); n != 1 {
		t.Fatalf("setLifecycleEventsEnabled sent %d Network.enable, want the network log started", n)
	}
	waitEvent(t, c, seq, sid, "Page.lifecycleEvent", isIdle)

	// A request in flight at load holds networkIdle back until it ends.
	seq = c.Seq()
	nav := decode[struct{ LoaderID string }](t, call(t, c, sid, "Page.navigate", map[string]any{"url": "https://one.example/two"})).LoaderID
	f.Emit("Network.requestWillBeSent", map[string]any{"requestId": "r1", "timestamp": 1.0, "request": map[string]any{"url": "https://one.example/slow", "method": "GET"}})
	f.Emit("Page.loadEventFired", map[string]any{"timestamp": 2.0})
	waitEvent(t, c, seq, sid, "Page.lifecycleEvent", func(p json.RawMessage) bool { return strings.Contains(string(p), `"name":"load"`) })
	time.Sleep(quietCheck)
	if ev := sessionEvents(c, seq, sid, "Page.lifecycleEvent"); hasLifecycle(t, ev, "networkIdle") || hasLifecycle(t, ev, "networkAlmostIdle") {
		t.Fatalf("network idle reported while a request is in flight: %v", lifecycleOf(t, ev))
	}
	finished := time.Now()
	f.Emit("Network.loadingFinished", map[string]any{"requestId": "r1", "timestamp": 3.0})
	waitEvent(t, c, seq, sid, "Page.lifecycleEvent", isIdle)
	if d := time.Since(finished); d < devtools.DefaultIdleQuiet {
		t.Fatalf("networkIdle %v after the last request ended, want at least the quiet time %v", d, devtools.DefaultIdleQuiet)
	}
	if got := strings.Join(lifecycleOf(t, sessionEvents(c, seq, sid, "Page.lifecycleEvent")), ","); got != "init@"+nav+",load@"+nav+",networkAlmostIdle@"+nav+",networkIdle@"+nav {
		t.Fatalf("lifecycle of the new document %s, want init, load, networkAlmostIdle, networkIdle with loader %s", got, nav)
	}

	// A replay reports the reached idleness at once.
	call(t, c, sid, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": false})
	seq = c.Seq()
	call(t, c, sid, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": true})
	if got := strings.Join(lifecycleOf(t, sessionEvents(c, seq, sid, "Page.lifecycleEvent")), ","); got != strings.Join([]string{"init@" + nav, "DOMContentLoaded@" + nav, "load@" + nav, "networkAlmostIdle@" + nav, "networkIdle@" + nav}, ",") {
		t.Fatalf("lifecycle replay of an idle document %s", got)
	}
	if n := len(f.ReceivedMethod("Network.enable")); n != 1 {
		t.Fatalf("a second setLifecycleEventsEnabled sent Network.enable again (%d)", n)
	}

	// A document replaced before it is idle gets no networkIdle; the next
	// one gets its own after its load.
	seq = c.Seq()
	call(t, c, sid, "Page.navigate", map[string]any{"url": "https://one.example/three"})
	f.Emit("Network.requestWillBeSent", map[string]any{"requestId": "r2", "timestamp": 4.0, "request": map[string]any{"url": "https://one.example/slow2", "method": "GET"}})
	f.Emit("Page.loadEventFired", map[string]any{"timestamp": 5.0})
	four := decode[struct{ LoaderID string }](t, call(t, c, sid, "Page.navigate", map[string]any{"url": "https://one.example/four"})).LoaderID
	f.Emit("Network.loadingFinished", map[string]any{"requestId": "r2", "timestamp": 6.0})
	time.Sleep(quietCheck)
	if ev := sessionEvents(c, seq, sid, "Page.lifecycleEvent"); hasLifecycle(t, ev, "networkIdle") {
		t.Fatalf("networkIdle of a replaced or unloaded document: %v", lifecycleOf(t, ev))
	}
	f.Emit("Page.loadEventFired", map[string]any{"timestamp": 7.0})
	idle := decode[lifecycle](t, waitEvent(t, c, seq, sid, "Page.lifecycleEvent", isIdle).Params)
	if idle.LoaderID != four {
		t.Fatalf("networkIdle with loader %s, want the last document's %s", idle.LoaderID, four)
	}
}

func lifecycleOf(t *testing.T, ev []cdptest.Event) []string {
	t.Helper()
	var out []string
	for _, e := range ev {
		l := decode[lifecycle](t, e.Params)
		out = append(out, l.Name+"@"+l.LoaderID)
	}
	return out
}

func hasLifecycle(t *testing.T, ev []cdptest.Event, name string) bool {
	t.Helper()
	for _, s := range lifecycleOf(t, ev) {
		if strings.HasPrefix(s, name+"@") {
			return true
		}
	}
	return false
}
