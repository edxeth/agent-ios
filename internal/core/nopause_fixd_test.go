package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// debuggerPauseWindows replays the fake's Debugger calls per connection and
// target and reports every point where WebKit could pause the page (FIX-D
// F1): Debugger enabled, or breakpoints activated or set, while the pause
// on debugger statements or on exceptions may be on. Unknown counts as on
// (JSC::Debugger keeps both across connections).
func debuggerPauseWindows(recs []fakewebkit.Record) []string {
	type st struct{ stmtsOff, excNone, enabled bool }
	state := map[string]*st{}
	var out []string
	for i, r := range recs {
		if !strings.HasPrefix(r.Method, "Debugger.") {
			continue
		}
		k := fmt.Sprint(r.Conn, "/", r.Target)
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
		safe := s.stmtsOff && s.excNone
		if !safe && (s.enabled || r.Method == "Debugger.setBreakpointsActive" || r.Method == "Debugger.setBreakpointByUrl") {
			out = append(out, fmt.Sprintf("#%d %s%s on conn %d target %s", i, r.Method, r.Params, r.Conn, r.Target))
		}
	}
	return out
}

// FIX-D F1: an owner that acquires Debugger without pauses
// (AcquireNoPauseDebugger) keeps the page unable to pause on every path
// that applies the desired state again: a reconnect (new connection) and a
// cross-process navigation (provisional target); and its release never
// turns a pause on.
func TestNoPauseDebuggerOnReconnectAndSwap(t *testing.T) {
	r := breakpointRig(t)
	ctx := ctxFor(t, 5*time.Second)
	owner := SessionOwner("logpoint", agent.Session)
	mo, err := r.m.Begin(ctx, r.req, agent, "logpoint add", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.m.AcquireNoPauseDebugger(ctx, mo, owner); err != nil {
		t.Fatal(err)
	}
	bp := json.RawMessage(`{"url":"https://fixture.example/app.js","lineNumber":3,"options":{"autoContinue":true}}`)
	if _, err := r.m.RawAs(ctx, mo, owner, "Debugger.setBreakpointByUrl", bp); err != nil {
		t.Fatal(err)
	}
	recoverOnce(t, r, mo)
	if got := onConn(r.f, "Debugger.setBreakpointByUrl", 2); len(got) != 1 {
		t.Fatalf("the reconnect did not apply the breakpoint again: %v", got)
	}
	r.f.Swap("page-2", 200*time.Millisecond)
	waitCommitted(t, mo.Page.PS.State)
	if err := mo.Page.PS.Caller.(*pageConn).ActiveSetup(ctxFor(t, 5*time.Second)); err != nil {
		t.Fatalf("page-2's setup: %v", err)
	}
	var onPage2 bool
	for _, rec := range r.f.ReceivedMethod("Debugger.setBreakpointByUrl") {
		onPage2 = onPage2 || rec.Target == "page-2"
	}
	if !onPage2 {
		t.Fatal("page-2 never got the breakpoint")
	}
	if err := r.m.Release(ctxFor(t, 2*time.Second), agent.Session, mo.Page.Key); err != nil {
		t.Fatal(err)
	}
	if w := debuggerPauseWindows(r.f.Received()); len(w) > 0 {
		t.Fatalf("the page could pause:\n%s", strings.Join(w, "\n"))
	}
	for _, rec := range r.f.ReceivedMethod("Debugger.setPauseOnDebuggerStatements") {
		if string(rec.Params) != `{"enabled":false}` {
			t.Fatalf("setPauseOnDebuggerStatements %s was sent; agent-ios never turns a pause on", rec.Params)
		}
	}
}

// recSender records the calls sent to it.
type recSender struct {
	mu    sync.Mutex
	calls []string
}

func (s *recSender) Send(_ context.Context, method string, params any) (json.RawMessage, error) {
	raw, _ := json.Marshal(params)
	s.mu.Lock()
	s.calls = append(s.calls, method+string(raw))
	s.mu.Unlock()
	if method == "Debugger.setBreakpointByUrl" {
		return json.RawMessage(`{"breakpointId":"bp-r"}`), nil
	}
	return json.RawMessage("{}"), nil
}

// FIX-D F1: the reset of an unknown Debugger agent (resetAndApplyDomain,
// after a release without cleanup) sends the owners' pause settings before
// Debugger.enable, and the activation and the breakpoints after it.
func TestResetOfDebuggerSendsPauseSettingsBeforeTheEnable(t *testing.T) {
	r := breakpointRig(t)
	ctx := ctxFor(t, 5*time.Second)
	owner := SessionOwner("logpoint", agent.Session)
	mo, err := r.m.Begin(ctx, r.req, agent, "logpoint add", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(mo.ID)
	if err := r.m.AcquireNoPauseDebugger(ctx, mo, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.RawAs(ctx, mo, owner, "Debugger.setBreakpointByUrl", json.RawMessage(bpByURL)); err != nil {
		t.Fatal(err)
	}
	s := &recSender{}
	if err := resetAndApplyDomain(ctx, mo.Page.PS.State, s, "Debugger"); err != nil {
		t.Fatal(err)
	}
	enable := -1
	for i, c := range s.calls {
		if c == "Debugger.enable null" || c == "Debugger.enablenull" {
			enable = i
		}
	}
	if enable < 0 {
		t.Fatalf("no Debugger.enable in %q", s.calls)
	}
	before := strings.Join(s.calls[:enable], " ")
	after := strings.Join(s.calls[enable:], " ")
	for _, want := range []string{`Debugger.setPauseOnDebuggerStatements{"enabled":false}`, `Debugger.setPauseOnExceptions{"state":"none"}`} {
		if !strings.Contains(before, want) {
			t.Fatalf("%s not sent before Debugger.enable: %q", want, s.calls)
		}
	}
	if strings.Contains(before, "setBreakpointsActive") || strings.Contains(before, "setBreakpointByUrl") || !strings.Contains(after, "setBreakpointsActive") || !strings.Contains(after, "setBreakpointByUrl") {
		t.Fatalf("activation and breakpoint must follow Debugger.enable: %q", s.calls)
	}
}

// FIX-D F2: an owner cannot remove another owner's breakpoint, by the id
// the owner got or by the id a replay gave it; the refusal is never sent.
// BreakpointClientID maps a replayed id back only for its owner.
func TestRemoveBreakpointOfAnotherOwnerIsRefused(t *testing.T) {
	r := breakpointRig(t)
	ctx := ctxFor(t, 5*time.Second)
	a, b := SessionOwner("cdp-a", agent.Session), SessionOwner("cdp-b", agent.Session)
	mo, err := r.m.Begin(ctx, r.req, agent, "cdp", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(mo.ID)
	if _, err := r.m.RawAs(ctx, mo, a, "Debugger.setBreakpointByUrl", json.RawMessage(bpByURL)); err != nil {
		t.Fatal(err)
	}
	st := mo.Page.PS.State
	st.mu.Lock()
	st.bpIDs = map[string]string{"bp-1": "bp-replayed"}
	st.mu.Unlock()
	for _, id := range []string{"bp-1", "bp-replayed"} {
		n := len(r.f.ReceivedMethod("Debugger.removeBreakpoint"))
		if _, err := r.m.RawAs(ctx, mo, b, "Debugger.removeBreakpoint", json.RawMessage(`{"breakpointId":"`+id+`"}`)); err == nil {
			t.Fatalf("owner b removed owner a's breakpoint %s", id)
		}
		if len(r.f.ReceivedMethod("Debugger.removeBreakpoint")) != n {
			t.Fatalf("the refused removal of %s reached WebKit", id)
		}
	}
	if !st.OwnsBreakpoint(a, "bp-1") || st.OwnsBreakpoint(b, "bp-1") {
		t.Fatal("OwnsBreakpoint")
	}
	if id, ok := st.BreakpointClientID(a, "", "bp-replayed"); !ok || id != "bp-1" {
		t.Fatalf("BreakpointClientID(a, bp-replayed) = %q, %v; want bp-1", id, ok)
	}
	if _, ok := st.BreakpointClientID(b, "", "bp-replayed"); ok {
		t.Fatal("BreakpointClientID gave owner b owner a's breakpoint")
	}
	if _, ok := st.BreakpointClientID(a, "", "bp-1"); ok {
		t.Fatal("the replaced id bp-1 of the old target still resolved")
	}
	if _, err := r.m.RawAs(ctx, mo, a, "Debugger.removeBreakpoint", json.RawMessage(`{"breakpointId":"bp-1"}`)); err != nil {
		t.Fatalf("the owner's own removal: %v", err)
	}
}

// FIX-D F1: a connection without an agent record gets no agent reset, and
// its backend may keep a pause setting of an earlier inspector: Apply sends
// the owners' pause settings, baselines included, before Debugger.enable.
func TestApplySendsPauseSettingsBeforeTheEnable(t *testing.T) {
	d := NewDesiredState()
	owner := SessionOwner("logpoint", "s1")
	s := &recSender{}
	for _, x := range noPauseSettings {
		rule := stateRules[x.method]
		d.setShared(s, owner, entryKey(x.method, rule.key(x.params)), x.method, x.method, x.params, rule.clear(x.params))
	}
	d.Acquire(owner, "Debugger")
	if err := d.Apply(ctxFor(t, 5*time.Second), s); err != nil {
		t.Fatal(err)
	}
	var dbg []string
	for _, c := range s.calls {
		if strings.HasPrefix(c, "Debugger.") {
			dbg = append(dbg, c)
		}
	}
	if len(dbg) != 3 || !strings.HasPrefix(dbg[2], "Debugger.enable") || strings.HasPrefix(dbg[0], "Debugger.enable") {
		t.Fatalf("Debugger calls %q; want both pause settings, then Debugger.enable", dbg)
	}
}
