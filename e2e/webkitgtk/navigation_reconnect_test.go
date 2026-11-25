//go:build webkitgtk

package webkitgtk

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/daemon"
)

// W3b-R3 finding 3 (design 2.4, 3.8; reconnect of design 2.6): the frame
// execution contexts that WebKit reports while a reconnect re-applies the
// desired state (Runtime.enable of the session's frame owner) reach the
// page's context inventory. Here the page connection closes, and a
// takeover reconnects: the new lease's first runtime command releases the
// dialog override that the earlier lease left in the child frame's
// context, and "frame #kid" selects the child again, in its context on the
// new connection.
func TestFrameContextsAcrossReconnectOnWebKitGTK(t *testing.T) {
	frameContextsAcrossReconnect(t, false)
}

// W3b-R4 (navigation review R3 P1; design 2.4 reference-counted domains,
// 3.7 "Release restores the original functions"): the session returns to
// the top document with "frame main" before the connection closes, so no
// owner holds Runtime, and the reconnect's re-apply makes WebKit report no
// frame context on the new connection. The new lease's first runtime
// command still releases the earlier lease's dialog override in the child
// frame: it holds Runtime for the release only, as a temporary owner, to
// get the frame's context on the new connection.
func TestFrameContextsAcrossReconnectAfterFrameMainOnWebKitGTK(t *testing.T) {
	frameContextsAcrossReconnect(t, true)
}

// frameContextsAcrossReconnect runs the reconnect scenario; with
// frameMain, the session runs "frame main" before the connection closes.
func frameContextsAcrossReconnect(t *testing.T, frameMain bool) {
	e := newNavEnv(t, "frames.html", false)
	e.eventually(agentA, "document.getElementById('kid').contentDocument.getElementById('press') !== null", 5*time.Second)
	native := "String(document.getElementById('kid').contentWindow.confirm).indexOf('[native code]') >= 0"
	e.must(agentA, "frame", "#kid")
	e.must(agentA, "dialog", "accept")
	if got := e.js(agentA, native); got != "false" {
		t.Fatalf("the dialog override is not installed in the child frame: native=%s", got)
	}
	if frameMain {
		e.must(agentA, "frame", "main")
	}
	before := len(e.tap.records())
	e.tap.closeConns()
	out, code, msg, err := e.runAs(daemon.Owner{Session: agentA, Takeover: true}, "get", "text", "h1")
	if err != nil || code != 0 {
		t.Fatalf("get text h1 --takeover: exit %d, %q, %v", code, msg, err)
	}
	if frameMain {
		// No owner held Runtime at the reconnect: the release held it for
		// itself and gave it back (design 2.4).
		if got := runtimeSwitches(withoutAgentResets(e.tap.records()[before:])); !slices.Equal(got, []string{"Runtime.enable", "Runtime.disable"}) {
			t.Fatalf("Runtime enable and disable calls of the takeover's command = %q, want one temporary Runtime.enable, then Runtime.disable", got)
		}
	}
	// The takeover is a new lease epoch, which ends the earlier frame scope.
	if got := strings.TrimSpace(out); got != "Frames" {
		t.Fatalf("get text h1 after the takeover = %q, want the top document's heading", got)
	}
	t.Logf("frame execution contexts that the daemon called before the reconnect: %v; after: %v", contextIDs(e.tap.records()[:before]), contextIDs(e.tap.records()[before:]))
	if got := e.js(agentA, native); got != "true" {
		t.Fatalf("the child's confirm is still overridden after the new lease's first runtime command: native=%s", got)
	}
	e.must(agentA, "frame", "#kid")
	if got := strings.TrimSpace(e.must(agentA, "get", "text", "h1")); got != "Child page" {
		t.Fatalf("after frame #kid on the new connection: get text h1 = %q, want the child's heading", got)
	}
	e.must(agentA, "frame", "main")
}

// runtimeSwitches lists the Runtime.enable and Runtime.disable calls that
// the daemon sent in rs, in order.
func runtimeSwitches(rs []tapRecord) []string {
	var out []string
	for _, r := range rs {
		if r.Out && (r.Method == "Runtime.enable" || r.Method == "Runtime.disable") {
			out = append(out, r.Method)
		}
	}
	return out
}

// contextIDs lists the distinct execution context ids that the daemon's
// Runtime.evaluate calls in rs named, in order.
func contextIDs(rs []tapRecord) []int64 {
	var out []int64
	for _, r := range rs {
		if !r.Out || r.Method != "Runtime.evaluate" {
			continue
		}
		var p struct {
			ContextID int64 `json:"contextId"`
		}
		if json.Unmarshal([]byte(r.Params), &p) == nil && p.ContextID != 0 && !slices.Contains(out, p.ContextID) {
			out = append(out, p.ContextID)
		}
	}
	return out
}
