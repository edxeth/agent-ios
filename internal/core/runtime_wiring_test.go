package core

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/runtime"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Design 3.1 (owner epoch): lease epochs of a page only grow within one
// daemon, because the page refuses an owner of the same daemon with an
// older epoch (stale_owner). The page keeps its runtime state when the
// daemon closes an idle page record, so a record created again for the
// same page must not count from 1 again.
func TestRecreatedPageRecordNeverReusesAnEpoch(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{DaemonID: "d-test"})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	pg, first, err := r.m.Touch(ctx, r.req, agent)
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	key := pg.Key
	if err := r.m.Release(ctx, agent.Session, key); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// The sweeper closes a page without a lease after IdleClose.
	r.m.Sweep(time.Now().Add(DefaultIdleClose + time.Minute))
	if r.m.LookupPage(key) != nil {
		t.Fatal("the idle page record was not closed")
	}
	for _, ao := range []AcquireOptions{agent, {Session: "s2", Reservation: "agent"}} {
		pg2, epoch, err := r.m.Touch(ctx, r.req, ao)
		if err != nil {
			t.Fatalf("lease of %s on the new record: %v", ao.Session, err)
		}
		if pg2 == pg {
			t.Fatal("Touch returned the closed page record")
		}
		if epoch <= first {
			t.Fatalf("session %s got epoch %d on the new record, want more than %d (the page's latest owner)", ao.Session, epoch, first)
		}
		first = epoch
		if err := r.m.Release(ctx, ao.Session, pg2.Key); err != nil {
			t.Fatalf("Release %s: %v", ao.Session, err)
		}
	}
}

// Property of design 3.1 (lease epochs only grow within one daemon): for
// any sequence of leases by two sessions, releases and idle closes of the
// page record, a new grant gets an epoch above every earlier one, and a
// renewal keeps the current one.
func TestLeaseEpochsOnlyGrowProperty(t *testing.T) {
	prop := func(steps []uint8) bool {
		if len(steps) > 8 {
			steps = steps[:8]
		}
		r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{DaemonID: "d-prop"})
		r.reserve(t, "agent")
		ctx := ctxFor(t, 20*time.Second)
		sessions := []AcquireOptions{agent, {Session: "s2", Reservation: "agent"}}
		var last uint64
		holder := -1 // index of the session that holds the lease
		for _, s := range steps {
			switch op := s % 4; op {
			case 0, 1:
				_, epoch, err := r.m.Touch(ctx, r.req, sessions[op])
				if holder >= 0 && holder != int(op) {
					if exitCode(err) != CodeBusy {
						t.Logf("steps %v: Touch by %s while %s holds the lease: %v", steps, sessions[op].Session, sessions[holder].Session, err)
						return false
					}
					continue
				}
				if err != nil {
					t.Logf("steps %v: Touch: %v", steps, err)
					return false
				}
				renewal := holder == int(op)
				if (renewal && epoch != last) || (!renewal && epoch <= last) {
					t.Logf("steps %v: epoch %d after %d (renewal %v)", steps, epoch, last, renewal)
					return false
				}
				last, holder = epoch, int(op)
			case 2:
				if holder >= 0 {
					if _, err := r.m.ReleaseSession(ctx, sessions[holder].Session); err != nil {
						t.Logf("steps %v: release: %v", steps, err)
						return false
					}
					holder = -1
				}
			case 3:
				// Past the lease TTL and IdleClose: an idle lease expires
				// in one sweep, and the record without a lease closes in
				// the next.
				r.m.Sweep(time.Now().Add(DefaultIdleClose + time.Minute))
				r.m.Sweep(time.Now().Add(DefaultIdleClose + time.Minute))
				holder = -1
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 40}); err != nil {
		t.Fatal(err)
	}
}

// W3a hooks (docs/notes/w1b.md "Hooks for W3a", design 2.9, 3.1, 3.3): an
// attached page has the agent runtime and a ref table, the runtime carries
// the daemon id, scheduler attempts call the runtime with its owner, and
// the runtime is a bootstrap part that a release keeps.
func TestAttachWiresThePageRuntime(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{DaemonID: "d-test"})
	r.reserve(t, "agent")
	var mu sync.Mutex
	var exprs []string
	r.f.SetRuntime(func(c fakewebkit.Call, fn string, _ json.RawMessage) any {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		mu.Lock()
		exprs = append(exprs, p.Expression)
		mu.Unlock()
		return map[string]any{"ok": false, "error": map[string]string{"code": "unsupported", "message": "fake page has no " + fn}}
	})
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "snapshot", ClassMutate)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// Mark the end of the attach: its setup may reset the Page agent with
	// an empty bootstrap script (resetCalls, agentResetCalls) only before
	// this record.
	attached := lastSeq(r.f)
	ps := mo.Page.PS
	if ps.Runtime == nil || ps.Refs == nil {
		t.Fatalf("page session without the W3a parts: Runtime %v, Refs %v", ps.Runtime, ps.Refs)
	}
	if ps.DaemonID != "d-test" {
		t.Fatalf("PageSession.DaemonID = %q, want the manager's %q", ps.DaemonID, "d-test")
	}
	if got := ps.Boot.Names(); len(got) != 1 || got[0] != RuntimePart {
		t.Fatalf("bootstrap parts = %q, want [%s]", got, RuntimePart)
	}
	if len(r.f.ReceivedMethod("Page.enable")) == 0 {
		t.Error("attach did not enable the Page domain that the bootstrap script needs")
	}
	boots := attachBootstrapScripts(r.f, attached)
	if len(boots) != 1 || !strings.Contains(string(boots[0].Params), "iwdp bootstrap part: runtime") {
		t.Fatalf("attach sent %d bootstrap scripts, want one with the runtime part", len(boots))
	}
	var bs struct{ Source string }
	_ = json.Unmarshal(boots[0].Params, &bs)
	if !strings.Contains(bs.Source, runtime.Source()) {
		t.Fatal("the bootstrap script does not contain runtime.Source()")
	}

	// Scheduler attempts go through the page runtime and carry its owner.
	ps.Runtime.SetOwner(runtime.Owner{Daemon: ps.DaemonID, Session: agent.Session, Epoch: mo.Epoch})
	if _, err := mo.Op.RunSteps(Action{Name: "click", Fn: "click", Steps: 1}); err == nil {
		t.Fatal("RunSteps succeeded against a page without the clock function")
	}
	mu.Lock()
	got := append([]string(nil), exprs...)
	mu.Unlock()
	if len(got) == 0 || !strings.Contains(got[0], `window.__iwdp.own({"daemon":"d-test","session":"s1"`) {
		t.Fatalf("the attempt's clock call does not carry the runtime owner: %q", got)
	}
	r.m.End(mo.ID)

	// The runtime part is the page's, not the session's: a release keeps
	// it, on the page too. Nothing after the attach, the attempt or the
	// release, sends a bootstrap script: neither the runtime part again
	// nor an empty script that clears it.
	if err := r.m.Release(ctx, agent.Session, mo.Page.Key); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := ps.Boot.Names(); len(got) != 1 || got[0] != RuntimePart {
		t.Fatalf("bootstrap parts after the release = %q, want [%s]", got, RuntimePart)
	}
	if n := receivedAfter(r.f, "Page.setBootstrapScript", attached); n != 0 {
		t.Fatalf("the attempt or the release sent %d Page.setBootstrapScript (any source) after the attach, want 0", n)
	}
}

// attachBootstrapScripts returns the Page.setBootstrapScript calls the
// fake received up to the log record attached (lastSeq after the attach),
// without the empty script of a Page agent reset (resetCalls,
// agentResetCalls), which a setup sends before Page.enable.
func attachBootstrapScripts(f *fakewebkit.Server, attached int) []fakewebkit.Record {
	var out []fakewebkit.Record
	for _, rec := range f.ReceivedMethod("Page.setBootstrapScript") {
		if rec.Seq <= attached && string(rec.Params) != `{"source":""}` {
			out = append(out, rec)
		}
	}
	return out
}

// Design 2.7 and 3.1 install path 2: a provisional target gets the page
// runtime before page scripts run. A desired-state registration outside a
// change does not reach a provisional target whose setup snapshot is older
// (DesiredState.beginChange, docs/notes/w1b.md round-5 residual). The
// runtime part is set before the first connection opens, so a provisional
// target created while that connection applies the desired state gets
// Page.enable and the runtime part in its own setup, before it commits.
func TestProvisionalTargetDuringAttachGetsThePageRuntime(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{DaemonID: "d-test"})
	r.reserve(t, "agent")
	var once sync.Once
	swapped := make(chan struct{})
	// The first DOM.hideHighlight is the first call of the attach's apply
	// on page-1: page-2 is announced then. Each apply waits 300 ms for
	// its DOM.setInspectModeEnabled, so page-2's setup takes its snapshot
	// while the attach still runs, and page-2 commits a second after its
	// setup, when the attach has its replies.
	r.f.Handle("DOM.hideHighlight", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		once.Do(func() { go func() { r.f.Swap("page-2", time.Second); close(swapped) }() })
		return nil, nil
	})
	r.f.DelayReplies("DOM.setInspectModeEnabled", 300*time.Millisecond)
	mo, err := r.m.Begin(ctxFor(t, 10*time.Second), r.req, agent, "get", ClassRead)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	attached := lastSeq(r.f)
	r.m.End(mo.ID)
	select {
	case <-swapped:
	case <-time.After(5 * time.Second):
		t.Fatal("the swap to page-2 did not end")
	}
	if setup := firstCallTo(r.f, "page-2"); setup == 0 || setup > attached {
		t.Fatalf("page-2's setup began at log record %d, want before the attach returned (record %d)", setup, attached)
	}
	var enabled, booted bool
	commit := 0
	for _, rec := range r.f.Log() {
		if rec.Method == "Target.didCommitProvisionalTarget" {
			commit = rec.Seq
			break
		}
		if !rec.In || rec.Target != "page-2" {
			continue
		}
		switch {
		case rec.Method == "Page.enable":
			enabled = true
		case rec.Method == "Page.setBootstrapScript" && strings.Contains(string(rec.Params), "iwdp bootstrap part: runtime"):
			booted = true
		}
	}
	if commit == 0 {
		t.Fatal("page-2 never committed")
	}
	if !enabled || !booted {
		t.Fatalf("page-2 before its commit: Page.enable %v, runtime bootstrap part %v; want both", enabled, booted)
	}
}

// firstCallTo is the log record of the first call the fake received for
// target, or 0.
func firstCallTo(f *fakewebkit.Server, target string) int {
	for _, rec := range f.Log() {
		if rec.In && rec.Target == target {
			return rec.Seq
		}
	}
	return 0
}
