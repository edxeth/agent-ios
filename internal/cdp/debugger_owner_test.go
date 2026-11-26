package cdp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// pauseWindows replays the fake's Debugger calls per connection and target
// and reports every point where WebKit could pause the page (FIX-D F1):
// Debugger enabled while the pause on debugger statements or on exceptions
// may be on, or a breakpoint activated or set then. An agent's state is
// unknown until a call sets it, and unknown counts as pausing (JSC keeps
// both settings across connections, and Web Inspector turns the debugger
// statements on).
func pauseWindows(recs []fakewebkit.Record) []string {
	type place struct {
		conn   int
		target string
	}
	type st struct {
		stmtsOff, excNone, enabled bool
	}
	state := map[place]*st{}
	var out []string
	for i, r := range recs {
		if !strings.HasPrefix(r.Method, "Debugger.") {
			continue
		}
		k := place{r.Conn, r.Target}
		s := state[k]
		if s == nil {
			s = &st{}
			state[k] = s
		}
		var p map[string]any
		_ = json.Unmarshal(r.Params, &p)
		switch r.Method {
		case "Debugger.setPauseOnDebuggerStatements":
			s.stmtsOff = p["enabled"] == false
		case "Debugger.setPauseOnExceptions":
			s.excNone = p["state"] == "none"
		case "Debugger.enable":
			s.enabled = true
		case "Debugger.disable":
			s.enabled = false
		}
		if s.enabled && (!s.stmtsOff || !s.excNone) || (r.Method == "Debugger.setBreakpointsActive" || r.Method == "Debugger.setBreakpointByUrl") && (!s.stmtsOff || !s.excNone) {
			out = append(out, fmt.Sprintf("#%d %s%s on %s (conn %d): debugger statements off %v, exceptions none %v", i, r.Method, r.Params, r.Target, r.Conn, s.stmtsOff, s.excNone))
		}
	}
	return out
}

// waitReceived waits, within the test's deadline, until target got a call
// of method (a provisional target's setup runs on its own goroutine).
func waitReceived(t *testing.T, f *fakewebkit.Server, target, method string) {
	t.Helper()
	ctx := testCtx(t)
	for {
		for _, r := range f.ReceivedMethod(method) {
			if r.Target == target {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s never got %s", target, method)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

const devtoolsLogpoint = "/** DEVTOOLS_LOGPOINT */ console.log('n', n)"

// FIX-D F1: a client that sets a logpoint without Debugger.enable
// (implicit acquisition) gets WIP Debugger only after the pauses are off,
// in that order; so does an explicit enable; and the release at detach
// never turns a pause on.
func TestDebuggerNeverLeavesAPauseWindow(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit enable %v", explicit), func(t *testing.T) {
			r, fp, c, sid, _ := attachPage(t)
			f := fp.f
			f.Handle("Debugger.setBreakpointByUrl", func(fakewebkit.Call) (any, *fakewebkit.Error) {
				return map[string]any{"breakpointId": "bp1"}, nil
			})
			if explicit {
				call(t, c, sid, "Debugger.enable", nil)
			}
			call(t, c, sid, "Debugger.setBreakpointByUrl", map[string]any{"url": "https://one.example/a.js", "lineNumber": 1, "condition": devtoolsLogpoint})
			if n := len(f.ReceivedMethod("Debugger.enable")); n == 0 {
				t.Fatal("WIP Debugger was never enabled")
			}
			if w := pauseWindows(f.Received()); len(w) > 0 {
				t.Fatalf("the page could pause:\n%s", strings.Join(w, "\n"))
			}
			call(t, c, sid, "Debugger.removeBreakpoint", map[string]any{"breakpointId": "bp1"})
			call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
			_ = r
			for _, p := range wipParams(t, f, "Debugger.setPauseOnDebuggerStatements") {
				if p["enabled"] != false {
					t.Fatalf("setPauseOnDebuggerStatements %v was sent; agent-ios never turns a pause on", p)
				}
			}
		})
	}
}

// FIX-D F1: a provisional target (a cross-process navigation) gets the
// pause settings before its Debugger.enable, then the activation and the
// breakpoint.
func TestDebuggerReplayOnANewTargetHasNoPauseWindow(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := fp.f
	f.Handle("Debugger.setBreakpointByUrl", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"breakpointId": "bp1"}, nil
	})
	call(t, c, sid, "Debugger.enable", nil)
	call(t, c, sid, "Debugger.setBreakpointByUrl", map[string]any{"url": "https://one.example/a.js", "lineNumber": 1, "condition": devtoolsLogpoint})
	waitResumed(t, f.Provisional("next"))
	waitReceived(t, f, "next", "Debugger.setBreakpointByUrl")
	_ = r
	if w := pauseWindows(f.Received()); len(w) > 0 {
		t.Fatalf("the page could pause:\n%s", strings.Join(w, "\n"))
	}
}

// FIX-D F2: a breakpoint belongs to the attachment that set it: another
// attachment of the same page neither sees its breakpointResolved nor can
// remove it, in the bridge or in WebKit.
func TestDebuggerBreakpointsAreOwnedByTheirAttachment(t *testing.T) {
	_, fp, c, sid, target := attachPage(t)
	f := fp.f
	f.Handle("Debugger.setBreakpointByUrl", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"breakpointId": "bp-owned"}, nil
	})
	other := decode[struct{ SessionID string }](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true})).SessionID
	call(t, c, sid, "Debugger.enable", nil)
	call(t, c, other, "Debugger.enable", nil)
	call(t, c, sid, "Debugger.setBreakpointByUrl", map[string]any{"url": "https://one.example/a.js", "lineNumber": 1, "condition": devtoolsLogpoint})

	seq := c.Seq()
	f.Emit("Debugger.breakpointResolved", map[string]any{"breakpointId": "bp-owned", "location": map[string]any{"scriptId": "s", "lineNumber": 1, "columnNumber": 0}})
	waitEvent(t, c, seq, sid, "Debugger.breakpointResolved", nil)
	call(t, c, other, "Debugger.setSkipAllPauses", map[string]any{"skip": true}) // a later reply: the event was translated
	if n := len(sessionEvents(c, seq, other, "Debugger.breakpointResolved")); n != 0 {
		t.Fatalf("the other attachment got %d breakpointResolved of a breakpoint it did not set", n)
	}

	n := len(f.ReceivedMethod("Debugger.removeBreakpoint"))
	if e := callErr(t, c, other, "Debugger.removeBreakpoint", map[string]any{"breakpointId": "bp-owned"}); e.Code != codeServer {
		t.Fatalf("the other attachment's removeBreakpoint: %+v, want a refusal", e)
	}
	if len(f.ReceivedMethod("Debugger.removeBreakpoint")) != n {
		t.Fatal("the other attachment removed the owner's breakpoint in WebKit")
	}
	call(t, c, sid, "Debugger.removeBreakpoint", map[string]any{"breakpointId": "bp-owned"})
	if len(f.ReceivedMethod("Debugger.removeBreakpoint")) != n+1 {
		t.Fatal("the owner's removeBreakpoint did not reach WebKit")
	}
}

// FIX-D F3: after a replay on a new target gave the breakpoint another id,
// its breakpointResolved names the id the client got.
func TestDebuggerResolvedKeepsTheClientIDAfterASwap(t *testing.T) {
	_, fp, c, sid, _ := attachPage(t)
	var next int
	fp.f.Handle("Debugger.setBreakpointByUrl", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		next++
		if next == 1 {
			return map[string]any{"breakpointId": "old"}, nil
		}
		return map[string]any{"breakpointId": "new"}, nil
	})
	call(t, c, sid, "Debugger.enable", nil)
	call(t, c, sid, "Debugger.setBreakpointByUrl", map[string]any{"url": "https://one.example/a.js", "lineNumber": 1, "condition": devtoolsLogpoint})
	waitResumed(t, fp.f.Provisional("next"))
	waitReceived(t, fp.f, "next", "Debugger.setBreakpointByUrl")
	seq := c.Seq()
	fp.f.EmitFrom("next", "Debugger.breakpointResolved", map[string]any{"breakpointId": "new", "location": map[string]any{"scriptId": "p", "lineNumber": 1, "columnNumber": 0}})
	fp.f.Commit("next")
	fp.f.EmitFrom("next", "Debugger.breakpointResolved", map[string]any{"breakpointId": "new", "location": map[string]any{"scriptId": "s", "lineNumber": 1, "columnNumber": 0}})
	for _, script := range []string{"p", "s"} {
		ev := waitEvent(t, c, seq, sid, "Debugger.breakpointResolved", func(p json.RawMessage) bool { return strings.Contains(string(p), `"scriptId":"`+script+`"`) })
		if got := decode[struct{ BreakpointID string }](t, ev.Params).BreakpointID; got != "old" {
			t.Fatalf("breakpointResolved (script %s) named %q; the client got old", script, got)
		}
	}
}
