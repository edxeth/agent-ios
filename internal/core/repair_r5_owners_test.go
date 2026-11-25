package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// Focused tests of the domain ownership model (domain_owners.go; design
// 2.4, orchestrator decision for W1b round 5), over the WebKit-like fake of
// repair_r5_test.go.

func ownersPage(t *testing.T) (*wkPage, wkConn, *PageSession) {
	t.Helper()
	p := &wkPage{active: "page-0", live: map[string]*wkTarget{"page-0": {bps: map[string]string{}}}, gone: map[string]error{}}
	conn := wkConn{p}
	d := NewDesiredState()
	if err := d.Apply(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	// The scheduler runs the Network reinitialization barrier's step 2.
	return p, conn, &PageSession{State: d, Sched: NewScheduler(conn, SchedulerOptions{})}
}

func ownerCall(t *testing.T, ps *PageSession, conn webkit.Caller, owner, method, params string) json.RawMessage {
	t.Helper()
	res, err := legacyCall(context.Background(), ps, SessionOwner("legacy", owner), conn, method, json.RawMessage(params))
	if err != nil {
		t.Fatalf("%s %s%s: %v", owner, method, params, err)
	}
	return res
}

func sentSince(p *wkPage, from int, method string) []string {
	var out []string
	for _, w := range p.wire[from:] {
		if _, call, _ := strings.Cut(w, " "); strings.HasPrefix(call, method) {
			out = append(out, w)
		}
	}
	return out
}

// Rules 1 to 3: a breakpoint acquires Debugger for its owner (the first
// acquisition enables it); another owner's own Debugger.disable releases
// only that owner's state, the shared activation falls back to the most
// recent value of the remaining owner, and Debugger.disable reaches the
// page only with the last owner, after that owner's state was reset.
func TestExplicitDisableKeepsTheDomainForOtherOwners(t *testing.T) {
	p, conn, ps := ownersPage(t)
	tg := p.live["page-0"]
	ownerCall(t, ps, conn, "s1", "Debugger.setBreakpointByUrl", bpByURL)
	if !tg.enabled || len(tg.bps) != 1 {
		t.Fatalf("after an implicit acquisition: enabled %v, breakpoints %v; want enabled with one breakpoint", tg.enabled, tg.bps)
	}
	ownerCall(t, ps, conn, "s1", "Debugger.setBreakpointsActive", `{"active":true}`)
	ownerCall(t, ps, conn, "s2", "Debugger.enable", "")
	ownerCall(t, ps, conn, "s2", "Debugger.setBreakpointsActive", `{"active":false}`)
	if tg.active {
		t.Fatal("s2's later active:false is not effective")
	}
	from := len(p.wire)
	ownerCall(t, ps, conn, "s2", "Debugger.disable", "")
	if got := sentSince(p, from, "Debugger.disable"); len(got) != 0 {
		t.Fatalf("s2's disable reached the page while s1 holds Debugger: %q", got)
	}
	if !tg.enabled || !tg.active || len(tg.bps) != 1 {
		t.Fatalf("after s2's disable: enabled %v, active %v, breakpoints %v; want s1's state (enabled, active, one breakpoint)", tg.enabled, tg.active, tg.bps)
	}
	from = len(p.wire)
	ownerCall(t, ps, conn, "s1", "Debugger.disable", "")
	want := []string{"page-0 Debugger.removeBreakpoint", `page-0 Debugger.setBreakpointsActive{"active":false}`, "page-0 Debugger.disable"}
	var got []string
	for _, w := range p.wire[from:] {
		got = append(got, strings.TrimSuffix(strings.TrimSuffix(w, `{"breakpointId":"https://fixture.example/app.js:1:0"}`), "null"))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the last owner's disable sent %q, want %q", got, want)
	}
	if left := ps.State.AllOwners(); len(left) != 0 {
		t.Fatalf("owners left: %v", left)
	}
}

// Rule 2 for any shared value, and rule 4: a released owner's value falls
// back to the most recent value of a remaining owner, a new target gets the
// effective value, and the last release restores WebKit's baseline.
func TestReleasedSharedValueFallsBack(t *testing.T) {
	p, conn, ps := ownersPage(t)
	ctx := context.Background()
	ownerCall(t, ps, conn, "s1", "Page.overrideUserAgent", `{"value":"A"}`)
	ownerCall(t, ps, conn, "s2", "Page.overrideUserAgent", `{"value":"B"}`)
	from := len(p.wire)
	if _, err := releaseOwners(ctx, ps, "s2", conn); err != nil {
		t.Fatal(err)
	}
	if got := p.wire[from:]; !slices.Equal(got, []string{`page-0 Page.overrideUserAgent{"value":"A"}`}) {
		t.Fatalf("release of s2 sent %q, want s1's value A again", got)
	}
	p.live["page-1"] = &wkTarget{bps: map[string]string{}}
	from = len(p.wire)
	if err := ps.State.Apply(ctx, wkTargetCaller{p, "page-1"}); err != nil {
		t.Fatal(err)
	}
	if got := sentSince(p, from, "Page.overrideUserAgent"); !slices.Equal(got, []string{`page-1 Page.overrideUserAgent{"value":"A"}`}) {
		t.Fatalf("a new target got %q, want the effective value A", got)
	}
	from = len(p.wire)
	if _, err := releaseOwners(ctx, ps, "s1", conn); err != nil {
		t.Fatal(err)
	}
	if got := sentSince(p, from, "Page.overrideUserAgent"); !slices.Equal(got, []string{"page-0 Page.overrideUserAgent{}", "page-1 Page.overrideUserAgent{}"}) {
		t.Fatalf("the last release sent %q, want the baseline (no value) on both targets", got)
	}
}

// A breakpoint set whose change began on a target that a commit replaced
// before the set was written goes to the committed target, once, after the
// Debugger.enable that the change queued for it.
func TestBreakpointSetCrossingACommitGoesToTheNewTarget(t *testing.T) {
	p, conn, ps := ownersPage(t)
	ctx := context.Background()
	p.live["page-1"] = &wkTarget{bps: map[string]string{}}
	if err := ps.State.Apply(ctx, wkTargetCaller{p, "page-1"}); err != nil {
		t.Fatal(err)
	}
	p.hookAt, p.hook = p.sends+1, func() { // after the implicit Debugger.enable on page-0
		delete(p.live, "page-0")
		p.gone["page-0"] = webkit.ErrTargetSwapped
		p.active = "page-1"
		ps.State.commitTarget("page-1")
	}
	from := len(p.wire)
	res := ownerCall(t, ps, conn, "s1", "Debugger.setBreakpointByUrl", bpByURL)
	want := []string{"page-0 Debugger.enable", "page-1 Debugger.enable", "page-1 Debugger.setBreakpointByUrl"}
	var got []string
	for _, w := range p.wire[from:] {
		got = append(got, strings.TrimSuffix(strings.TrimSuffix(w, bpByURL), "null"))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
	var v struct {
		ID string `json:"breakpointId"`
	}
	if json.Unmarshal(res, &v) != nil || v.ID == "" {
		t.Fatalf("reply %s has no breakpoint id", res)
	}
	ownerCall(t, ps, conn, "s1", "Debugger.removeBreakpoint", `{"breakpointId":"`+v.ID+`"}`)
	if n := len(p.live["page-1"].bps); n != 0 {
		t.Fatalf("page-1 keeps %d breakpoints after the removal", n)
	}
}
