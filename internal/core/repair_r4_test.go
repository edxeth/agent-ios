package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// Regression tests for the fourth W1b review (reviewer counterexample
// R4-1): a URL breakpoint that a provisional target's setup replayed is
// removed from that target when the client removes the breakpoint or its
// owner releases it, before or after the target commits. Every page
// endpoint is a private fake.

// R4-1 (the reviewer's counterexample), design 2.4 and 2.7: page-1 has
// bp-1; the provisional page-2 has replayed it as bp-2. Removing bp-1
// while page-1 is still active also removes bp-2 from page-2, so the
// commit of page-2 cannot bring the breakpoint back.
func TestRemoveBreakpointDuringProvisionalSetup(t *testing.T) {
	r := breakpointRig(t)
	mo := ownedStateOnPage1(t, r)
	swapped := make(chan struct{})
	go func() { r.f.Swap("page-2", 700*time.Millisecond); close(swapped) }()
	defer func() { <-swapped }()
	st := mo.Page.PS.State
	eventually(t, time.Second, "provisional replay to be staged", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.staged["page-2"]["bp-1"] == "bp-2"
	})
	rm := rawSeq(t, r, ctxFor(t, 2*time.Second), [2]string{"Debugger.removeBreakpoint", `{"breakpointId":"bp-1"}`})
	r.m.End(rm.ID)
	st.mu.Lock()
	active := st.active
	st.mu.Unlock()
	if active != "page-1" {
		t.Fatalf("test missed the precommit removal boundary: active=%s", active)
	}
	if got := removalsOn(r.f, "page-1"); len(got) != 1 || got[0] != "bp-1" {
		t.Fatalf("active removal %v", got)
	}
	<-swapped
	eventually(t, time.Second, "commit reaches desired state", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.active == "page-2"
	})
	if err := r.m.Release(ctxFor(t, 2*time.Second), "s1", mo.Page.Key); err != nil {
		t.Fatal(err)
	}
	if got := removalsOn(r.f, "page-2"); len(got) != 1 || got[0] != "bp-2" {
		t.Fatalf("provisional target installed bp-2, committed after successful remove, but removals on page-2=%v even after release; want bp-2 removed", got)
	}
}

// R4-1, design 2.4 and 2.7: the removal arrives while page-2's replay of
// the breakpoint is in flight (page-2 installed bp-2, its reply has not
// arrived). When the reply arrives, the setup removes bp-2 from page-2
// itself, because the client's breakpoint no longer exists.
func TestRemoveBreakpointWhileItsReplayIsInFlight(t *testing.T) {
	r := breakpointRig(t)
	mo := ownedStateOnPage1(t, r)
	r.f.DelayReplies("Debugger.setBreakpointByUrl", 400*time.Millisecond)
	swapped := make(chan struct{})
	go func() { r.f.Swap("page-2", 800*time.Millisecond); close(swapped) }()
	defer func() { <-swapped }()
	eventually(t, time.Second, "page-2 to install the replayed breakpoint", func() bool {
		return len(r.f.ReceivedMethod("Debugger.setBreakpointByUrl")) == 2
	})
	rm := rawSeq(t, r, ctxFor(t, 2*time.Second), [2]string{"Debugger.removeBreakpoint", `{"breakpointId":"bp-1"}`})
	r.m.End(rm.ID)
	st := mo.Page.PS.State
	st.mu.Lock()
	inFlight := st.staged["page-2"]["bp-1"] == "" && st.active == "page-1"
	st.mu.Unlock()
	if !inFlight {
		t.Fatal("test missed the in-flight boundary: the replay was recorded or page-2 committed before the removal")
	}
	eventually(t, 2*time.Second, "the late replay to remove bp-2 from page-2", func() bool {
		return len(removalsOn(r.f, "page-2")) == 1
	})
	<-swapped
	eventually(t, time.Second, "commit reaches desired state", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.active == "page-2"
	})
	if err := r.m.Release(ctxFor(t, 2*time.Second), "s1", mo.Page.Key); err != nil {
		t.Fatal(err)
	}
	if got := removalsOn(r.f, "page-2"); len(got) != 1 || got[0] != "bp-2" {
		t.Fatalf("removals on page-2 = %v, want [bp-2] once", got)
	}
	if got := removalsOn(r.f, "page-1"); len(got) != 1 || got[0] != "bp-1" {
		t.Fatalf("removals on page-1 = %v, want [bp-1]", got)
	}
}

// R4-1, design 2.4 (release cleanup) and 2.7: releasing the lease while
// page-2 is provisional removes the session's breakpoint from page-1 and
// from page-2, which then commits without it.
func TestReleaseDuringProvisionalSetupRemovesTheReplayedBreakpoint(t *testing.T) {
	r := breakpointRig(t)
	mo := ownedStateOnPage1(t, r)
	swapped := make(chan struct{})
	go func() { r.f.Swap("page-2", 700*time.Millisecond); close(swapped) }()
	defer func() { <-swapped }()
	st := mo.Page.PS.State
	eventually(t, time.Second, "provisional replay to be staged", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.staged["page-2"]["bp-1"] == "bp-2"
	})
	if err := r.m.Release(ctxFor(t, 2*time.Second), "s1", mo.Page.Key); err != nil {
		t.Fatal(err)
	}
	if got := removalsOn(r.f, "page-1"); len(got) != 1 || got[0] != "bp-1" {
		t.Fatalf("removals on page-1 = %v, want [bp-1]", got)
	}
	if got := removalsOn(r.f, "page-2"); len(got) != 1 || got[0] != "bp-2" {
		t.Fatalf("removals on the provisional page-2 = %v, want [bp-2] at release", got)
	}
	<-swapped
}

// bpBackend models a page whose targets each have their own JavaScript
// breakpoints, as separate WebKit backends (web processes) do: a target
// starts with none, a commit replaces the active target, and a destroyed
// target takes its breakpoints with it. Debugger.removeBreakpoint of an id
// the target does not have succeeds without effect, as in WebKit
// (InspectorDebuggerAgent::removeBreakpoint).
type bpBackend struct {
	active string
	live   map[string]map[string]string // target -> installed breakpoint id -> url
	n      int
	// desired reports whether a client breakpoint for url still exists;
	// wrong collects removals of such breakpoints.
	desired func(url string) bool
	wrong   []string
	// failRemove makes Debugger.removeBreakpoint on a target fail.
	failRemove map[string]error
	// onReplay runs once, inside the next Debugger.setBreakpointByUrl that
	// a target other than the active one receives, after the target
	// installed the breakpoint and before its reply: the window in which
	// the replay is in flight.
	onReplay func(url string)
	trace    []string
}

// bpTarget sends to one target, as webkit.TargetCaller does.
type bpTarget struct {
	b  *bpBackend
	id string
}

func (t bpTarget) TargetID() string { return t.id }

func (t bpTarget) Send(_ context.Context, method string, params any) (json.RawMessage, error) {
	b := t.b
	bps, ok := b.live[t.id]
	if !ok {
		return nil, fmt.Errorf("%s on %s: %w", method, t.id, webkit.ErrTargetAbandoned)
	}
	raw, _ := json.Marshal(params)
	var p struct {
		URL string `json:"url"`
		ID  string `json:"breakpointId"`
	}
	_ = json.Unmarshal(raw, &p)
	switch method {
	case "Debugger.setBreakpointByUrl":
		b.n++
		id := fmt.Sprintf("%s/bp-%d", t.id, b.n)
		bps[id] = p.URL
		if h := b.onReplay; h != nil && t.id != b.active {
			b.onReplay = nil
			h(p.URL)
		}
		return json.Marshal(map[string]any{"breakpointId": id, "locations": []any{}})
	case "Debugger.removeBreakpoint":
		if err := b.failRemove[t.id]; err != nil {
			return nil, err
		}
		if url, ok := bps[p.ID]; ok && b.desired != nil && b.desired(url) {
			b.wrong = append(b.wrong, fmt.Sprintf("%s on %s (%s)", p.ID, t.id, url))
		}
		delete(bps, p.ID)
	}
	return json.RawMessage("{}"), nil
}

// bpPage is the connection: it sends to the active target.
type bpPage struct{ b *bpBackend }

func (c bpPage) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return bpTarget{c.b, c.b.active}.Send(ctx, method, params)
}
func (c bpPage) ActiveTarget() string                       { return c.b.active }
func (bpPage) Subscribe(string, webkit.EventHandler) func() { return func() {} }
func (bpPage) IsTargetRouted() bool                         { return true }
func (bpPage) Done() <-chan struct{}                        { return nil }

func (c bpPage) provisional() []string {
	var out []string
	for id := range c.b.live {
		if id != c.b.active {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// R4-1 and the global rule against hidden failures: a provisional
// target's copy that cannot be removed (the target is alive but the send
// fails) makes the removal fail with an error that names the target, also
// when the active target's removal succeeded; a late replay whose own
// removal fails makes that target's setup fail. A destroyed target's copy
// is no failure (TestNoBreakpointSurvivesItsOwner).
func TestBreakpointCopyRemovalFailuresAreReported(t *testing.T) {
	ctx := context.Background()
	stall := errors.New("page did not answer")
	b := &bpBackend{active: "page-0", live: map[string]map[string]string{"page-0": {}, "page-1": {}}}
	page := bpPage{b}
	d := NewDesiredState()
	d.active = "page-0"
	ps := &PageSession{State: d}
	set := func(url string) string {
		t.Helper()
		res, _, err := legacyStateCall(ctx, ps, "legacy:a:s1", page, "Debugger.setBreakpointByUrl", json.RawMessage(fmt.Sprintf(`{"url":%q,"lineNumber":1}`, url)))
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			ID string `json:"breakpointId"`
		}
		_ = json.Unmarshal(res, &v)
		return v.ID
	}
	id := set("https://fixture.example/a.js")
	if err := d.Apply(ctx, bpTarget{b, "page-1"}); err != nil {
		t.Fatal(err)
	}
	b.failRemove = map[string]error{"page-1": stall}
	_, _, err := legacyStateCall(ctx, ps, "legacy:a:s1", page, "Debugger.removeBreakpoint", json.RawMessage(fmt.Sprintf(`{"breakpointId":%q}`, id)))
	if !errors.Is(err, stall) || !strings.Contains(err.Error(), "page target page-1") {
		t.Fatalf("removal with a failing provisional copy: %v; want the failure, naming page-1", err)
	}
	if len(b.live["page-0"]) != 0 {
		t.Fatalf("active page-0 still has %v", b.live["page-0"])
	}

	id = set("https://fixture.example/b.js")
	b.live["page-2"] = map[string]string{}
	b.failRemove = map[string]error{"page-2": stall}
	b.onReplay = func(string) {
		if _, _, err := legacyStateCall(ctx, ps, "legacy:a:s1", page, "Debugger.removeBreakpoint", json.RawMessage(fmt.Sprintf(`{"breakpointId":%q}`, id))); err != nil {
			t.Errorf("removal on the active target: %v", err)
		}
	}
	err = d.Apply(ctx, bpTarget{b, "page-2"})
	if !errors.Is(err, stall) || !strings.Contains(err.Error(), "ended while it was replayed") {
		t.Fatalf("setup whose late removal fails: %v; want the failure", err)
	}
}

// R4-1, design 2.4 and 2.7, as a property over any sequence of client
// breakpoint sets and removals, provisional targets (whose setup can see a
// removal, a release, its own commit or its own destruction while a
// breakpoint replay is in flight), commits, destroyed provisional targets,
// lease releases and reconnects: after every step, every breakpoint
// installed on any live target belongs to a client breakpoint that still
// exists, no removal hits a breakpoint that still exists, and removals and
// releases succeed. Releasing every owner at the end leaves no breakpoint
// on any target.
func TestNoBreakpointSurvivesItsOwner(t *testing.T) {
	owners := []Owner{"legacy:a:s1", "legacy:b:s2"}
	prop := func(ops []uint16) bool {
		ctx := context.Background()
		b := &bpBackend{active: "page-0", live: map[string]map[string]string{"page-0": {}}}
		page := bpPage{b}
		d := NewDesiredState()
		d.active = "page-0"
		ps := &PageSession{State: d}
		type clientBP struct {
			id    string
			owner Owner
		}
		desired := map[string]clientBP{} // url -> the client's breakpoint
		b.desired = func(url string) bool { _, ok := desired[url]; return ok }
		fail := func(f string, a ...any) bool {
			t.Logf("after %s: "+f, append([]any{strings.Join(b.trace, "; ")}, a...)...)
			return false
		}
		remove := func(url string) error {
			bp := desired[url]
			delete(desired, url)
			b.trace = append(b.trace, "remove "+bp.id)
			_, _, err := legacyStateCall(ctx, ps, bp.owner, page, "Debugger.removeBreakpoint", json.RawMessage(fmt.Sprintf(`{"breakpointId":%q}`, bp.id)))
			return err
		}
		release := func(o Owner) error {
			for u, bp := range desired {
				if bp.owner == o {
					delete(desired, u)
				}
			}
			b.trace = append(b.trace, "release "+string(o))
			_, err := d.Release(ctx, o, page)
			return err
		}
		commit := func(target string) {
			b.trace = append(b.trace, "commit "+target)
			delete(b.live, b.active)
			b.active = target
			d.commitTarget(target)
		}
		abandon := func(target string) {
			b.trace = append(b.trace, "destroy "+target)
			delete(b.live, target)
			d.abandonTarget(target)
		}
		var hookErr error
		for i, op := range ops {
			arg := int(op >> 3)
			switch op % 8 {
			case 0, 1: // the client sets a URL breakpoint on the active target
				o := owners[arg%2]
				url := fmt.Sprintf("https://fixture.example/%d.js", i)
				b.trace = append(b.trace, "set "+url)
				res, _, err := legacyStateCall(ctx, ps, o, page, "Debugger.setBreakpointByUrl", json.RawMessage(fmt.Sprintf(`{"url":%q,"lineNumber":1}`, url)))
				if err != nil {
					return fail("set: %v", err)
				}
				var v struct {
					ID string `json:"breakpointId"`
				}
				_ = json.Unmarshal(res, &v)
				desired[url] = clientBP{id: v.ID, owner: o}
			case 2:
				if us := slices.Sorted(maps.Keys(desired)); len(us) > 0 {
					if err := remove(us[arg%len(us)]); err != nil {
						return fail("remove: %v", err)
					}
				}
			case 3: // a provisional target and its setup
				target := fmt.Sprintf("page-%d", i+1)
				b.live[target] = map[string]string{}
				b.trace = append(b.trace, "provisional "+target)
				switch arg % 5 {
				case 1:
					b.onReplay = func(url string) { hookErr = remove(url) }
				case 2:
					b.onReplay = func(url string) { hookErr = release(desired[url].owner) }
				case 3:
					b.onReplay = func(string) { commit(target) }
				case 4:
					b.onReplay = func(string) { abandon(target) }
				}
				_ = d.Apply(ctx, bpTarget{b, target}) // fails when the target is destroyed during its setup
				b.onReplay = nil
				if hookErr != nil {
					return fail("during the replay: %v", hookErr)
				}
			case 4:
				if ts := page.provisional(); len(ts) > 0 {
					commit(ts[arg%len(ts)])
				}
			case 5:
				if ts := page.provisional(); len(ts) > 0 {
					abandon(ts[arg%len(ts)])
				}
			case 6:
				if err := release(owners[arg%2]); err != nil {
					return fail("release: %v", err)
				}
			case 7: // a reconnect: a new connection to a new backend
				target := fmt.Sprintf("page-%d", i+1)
				b.trace = append(b.trace, "reconnect "+target)
				b.live = map[string]map[string]string{target: {}}
				b.active = target
				if err := d.Apply(ctx, page); err != nil {
					return fail("reconnect: %v", err)
				}
			}
			if len(b.wrong) > 0 {
				return fail("removed breakpoints that still exist: %v", b.wrong)
			}
			for target, bps := range b.live {
				for id, url := range bps {
					if _, ok := desired[url]; !ok {
						return fail("%s still has breakpoint %s (%s), which no client breakpoint owns", target, id, url)
					}
				}
			}
		}
		for _, o := range owners {
			if err := release(o); err != nil {
				return fail("final release: %v", err)
			}
		}
		for target, bps := range b.live {
			if len(bps) > 0 {
				return fail("%s has %v after every owner released", target, slices.Sorted(maps.Keys(bps)))
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 3000}); err != nil {
		t.Fatal(err)
	}
}
