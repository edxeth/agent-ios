package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Regression tests for the third W1b review (reviewer counterexamples
// R3-1 and R3-2). Every listing and page endpoint is a private fake; no
// test reads or starts the real iwdp.

// useDefaultListing points iwdp's default device-list address at the
// private fake listing addr for the test.
func useDefaultListing(t *testing.T, addr string) {
	t.Helper()
	old := iwdp.DefaultListingAddr
	iwdp.DefaultListingAddr = addr
	t.Cleanup(func() { iwdp.DefaultListingAddr = old })
}

// defaultIWDP is the daemon's transport for a request without transport or
// address when the daemon reads iwdp's default device list.
func defaultIWDP(context.Context, string, string) (transport.Transport, error) {
	return iwdp.New(iwdp.Options{}), nil
}

// R3-1, design 2.5 rule 1 and D11: iwdp's device ports are configurable
// (-c), so the default device-list address does not bound them. An
// explicit URL on a port outside 9222-9322 that the default listing lists
// is that physical page: it needs the reservation before any connection.
func TestDefaultListingCustomDevicePortNeedsReservation(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{Transport: defaultIWDP})
	useDefaultListing(t, r.l.Addr())
	q := PageRequest{Page: r.f.URL}
	if iwdpDevicePort(q.Page) {
		t.Fatalf("fixture URL %s is on a default iwdp port; the test needs a custom port", q.Page)
	}
	mo, err := r.m.Begin(ctxFor(t, 5*time.Second), q, agent, "unreserved", ClassRead)
	if mo != nil {
		r.m.End(mo.ID)
		t.Fatalf("unreserved attach succeeded: key %s, physical %v, %d inspector connections", mo.Page.Key, mo.Page.target.physical, r.f.ConnectionCount())
	}
	if !errors.Is(err, ErrNoReservation) || exitCode(err) != CodeReservation {
		t.Fatalf("Begin without a reservation: %v (code %d), want ErrNoReservation", err, exitCode(err))
	}
	if n := r.f.ConnectionCount(); n != 0 {
		t.Fatalf("%d inspector connections before the reservation, want 0", n)
	}
	r.reserve(t, "agent")
	mo, err = r.m.Begin(ctxFor(t, 5*time.Second), q, agent, "reserved", ClassRead)
	if err != nil {
		t.Fatalf("Begin with the reservation: %v", err)
	}
	r.m.End(mo.ID)
	if !mo.Page.target.physical || mo.Page.Key != "iwdp|"+r.dev.ID+"|"+r.f.URL {
		t.Fatalf("page key %s, physical %v; want the listed physical page", mo.Page.Key, mo.Page.target.physical)
	}
}

// R3-1, design 2.5: the listed busy hint and the iOS version gate also
// apply to an explicit URL on a custom device port of the default listing.
func TestDefaultListingCustomDevicePortKeepsBusyAndVersionGates(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, true, ManagerOptions{Transport: defaultIWDP})
	useDefaultListing(t, r.l.Addr())
	r.reserve(t, "agent")
	r.dev.AddPage("Other", "https://other.example/", "ws://127.0.0.1:1/x")
	r.dev.SetBusy(r.pg, true)
	q := PageRequest{Page: r.f.URL}
	_, err := r.m.Begin(ctxFor(t, 5*time.Second), q, agent, "busy", ClassRead)
	if !errors.Is(err, ErrPageBusy) || exitCode(err) != CodeBusy || r.f.ConnectionCount() != 0 {
		t.Fatalf("Begin on a page listed busy: %v (code %d) after %d connections, want ErrPageBusy and none", err, exitCode(err), r.f.ConnectionCount())
	}
	r.dev.SetBusy(r.pg, false)
	ctx := ctxFor(t, 5*time.Second)
	mo, err := r.m.Begin(ctx, q, agent, "version", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin on the free page: %v", err)
	}
	defer r.m.End(mo.ID)
	// DOM.getMediaStats is not in the iOS 16.7 command list (internal/wip).
	if _, err := r.m.Raw(ctx, mo.ID, "DOM.getMediaStats", nil); err == nil || len(r.f.ReceivedMethod("DOM.getMediaStats")) != 0 {
		t.Fatalf("DOM.getMediaStats on iOS 16.7.10: %v, sent %d times; want it refused unsent", err, len(r.f.ReceivedMethod("DOM.getMediaStats")))
	}
}

// R3-1, design 2.5 rule 2: a URL attempt that was refused leaves a page
// record without a connection; a later attempt by URL takes the busy hint
// from a fresh listing, not from that record.
func TestURLRetryTakesTheBusyHintFromAFreshListing(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, true, ManagerOptions{Transport: defaultIWDP})
	useDefaultListing(t, r.l.Addr())
	r.dev.AddPage("Other", "https://other.example/", "ws://127.0.0.1:1/x")
	q := PageRequest{Page: r.f.URL}
	if _, err := r.m.Begin(ctxFor(t, 5*time.Second), q, agent, "unreserved", ClassRead); !errors.Is(err, ErrNoReservation) {
		t.Fatalf("Begin without a reservation: %v, want ErrNoReservation", err)
	}
	r.dev.SetBusy(r.pg, true) // another inspector client attached meanwhile
	r.reserve(t, "agent")
	mo, err := r.m.Begin(ctxFor(t, 5*time.Second), q, agent, "retry", ClassRead)
	if mo != nil {
		r.m.End(mo.ID)
	}
	if !errors.Is(err, ErrPageBusy) || r.f.ConnectionCount() != 0 {
		t.Fatalf("retry on a page now listed busy: %v after %d connections, want ErrPageBusy and none", err, r.f.ConnectionCount())
	}
}

// R3-1: with the default listing, a URL that the listing does not list
// (or that no listing answers for) is a local endpoint after one listing
// read; a URL on a default iwdp device port that the listing does not
// list is refused.
func TestDefaultListingClassifiesURLsFromTheListing(t *testing.T) {
	reads := 0
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{Transport: func(context.Context, string, string) (transport.Transport, error) {
		return countingIWDP{iwdp.New(iwdp.Options{}), &reads}, nil
	}})
	mo, err := r.m.Begin(ctxFor(t, 5*time.Second), PageRequest{Page: r.f.URL}, agent, "a", ClassRead)
	if err != nil {
		t.Fatalf("Begin on a local URL: %v", err)
	}
	r.m.End(mo.ID)
	if reads != 1 || mo.Page.target.physical {
		t.Fatalf("listing reads = %d, physical %v; want a local page after one listing read", reads, mo.Page.target.physical)
	}
	_, err = r.m.Begin(ctxFor(t, 5*time.Second), PageRequest{Page: "ws://127.0.0.1:9230/devtools/page/7"}, agent, "b", ClassRead)
	if !errors.Is(err, ErrNoPage) || reads != 2 {
		t.Fatalf("iwdp-port URL: %v after %d listing reads; want ErrNoPage after 2 reads", err, reads)
	}
}

// resetsOn counts the CSS.forcePseudoState resets (no classes) sent to
// target.
func resetsOn(f *fakewebkit.Server, target string) int {
	n := 0
	for _, c := range f.ReceivedMethod("CSS.forcePseudoState") {
		var p struct {
			Classes []string `json:"forcedPseudoClasses"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if c.Target == target && len(p.Classes) == 0 {
			n++
		}
	}
	return n
}

// removalsOn returns the breakpoint ids that Debugger.removeBreakpoint
// sent to target.
func removalsOn(f *fakewebkit.Server, target string) []string {
	var out []string
	for _, c := range f.ReceivedMethod("Debugger.removeBreakpoint") {
		var p struct {
			ID string `json:"breakpointId"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if c.Target == target {
			out = append(out, p.ID)
		}
	}
	return out
}

// ownedStateOnPage1 sets a URL breakpoint (bp-1) and a forced pseudo state
// on page-1 and ends the command.
func ownedStateOnPage1(t *testing.T, r *rig) *ManagedOp {
	t.Helper()
	mo := rawSeq(t, r, ctxFor(t, 5*time.Second),
		[2]string{"Debugger.enable", ""},
		[2]string{"Debugger.setBreakpointByUrl", bpByURL},
		[2]string{"CSS.forcePseudoState", `{"nodeId":7,"forcedPseudoClasses":["hover"]}`})
	r.m.End(mo.ID)
	return mo
}

// R3-2, design 2.4 and 2.7: an abandoned provisional target is dropped
// without changing the active target's owned state. Its setup replays the
// URL breakpoint (bp-2 on the provisional target), but page-1 keeps its
// forced pseudo state and its own breakpoint id, so the release resets the
// state on page-1 and removes bp-1 there.
func TestAbandonedProvisionalKeepsActiveCleanup(t *testing.T) {
	r := breakpointRig(t)
	mo := ownedStateOnPage1(t, r)
	// The target is destroyed after its setup replayed the breakpoint, not
	// after a fixed time: a target destroyed earlier gets no replay
	// (docs/notes/w1b.md, "core-events r2: two swap tests read too early").
	// The replay counts as done when the client recorded the provisional
	// target's id (staged), not when the fake received the call: the
	// setup records the reply on its own goroutine, and a destroy event
	// read before that record would be followed by it.
	st := mo.Page.PS.State
	r.f.Provisional("page-abort")
	eventually(t, 5*time.Second, "provisional breakpoint replay", func() bool {
		sent := len(r.f.ReceivedMethod("Debugger.setBreakpointByUrl"))
		st.mu.Lock()
		defer st.mu.Unlock()
		return sent == 2 && st.staged["page-abort"]["bp-1"] == "bp-2"
	})
	r.f.DestroyTarget("page-abort")
	eventually(t, 5*time.Second, "the abort reaches the client", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.syncs["page-abort"] == nil && st.staged["page-abort"] == nil // abandonTarget ran
	})
	if err := r.m.Release(ctxFor(t, 5*time.Second), "s1", mo.Page.Key); err != nil {
		t.Fatal(err)
	}
	if n := resetsOn(r.f, "page-1"); n != 1 {
		t.Errorf("CSS resets on the active page-1 = %d, want 1 after the provisional abort", n)
	}
	if got := removalsOn(r.f, "page-1"); len(got) != 1 || got[0] != "bp-1" {
		t.Errorf("breakpoint removals on page-1 = %v, want [bp-1] (bp-2 belongs to the abandoned target)", got)
	}
}

// R3-2, design 2.7: on commit, the provisional target's breakpoint id
// replaces the old target's, and state scoped to the old document (the
// forced pseudo state's node id) is dropped without a reset.
func TestCommittedProvisionalTakesOverBreakpointIDs(t *testing.T) {
	r := breakpointRig(t)
	mo := ownedStateOnPage1(t, r)
	r.f.Swap("page-2", 200*time.Millisecond)
	eventually(t, time.Second, "provisional breakpoint replay", func() bool {
		return len(r.f.ReceivedMethod("Debugger.setBreakpointByUrl")) == 2
	})
	// Swap returns when the fake sent the commit; the release follows the
	// client's handling of it.
	st := mo.Page.PS.State
	eventually(t, time.Second, "the commit reaches the client", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.active == "page-2"
	})
	if err := r.m.Release(ctxFor(t, 5*time.Second), "s1", mo.Page.Key); err != nil {
		t.Fatal(err)
	}
	if n := resetsOn(r.f, "page-1") + resetsOn(r.f, "page-2"); n != 0 {
		t.Errorf("CSS resets after the commit = %d, want 0 (node 7 belongs to the old document)", n)
	}
	if got := removalsOn(r.f, "page-2"); len(got) != 1 || got[0] != "bp-2" {
		t.Errorf("breakpoint removals on page-2 = %v, want [bp-2]", got)
	}
}

// R3-2, design 2.4 and 2.7, as a property over any sequence of breakpoint
// id records, commits, destroyed targets and removals on one connection:
// a provisional target's ids never change the active target's ids before
// it commits; a destroyed target changes nothing; a commit makes the
// committed target's staged ids current; a removal returns the active id
// (or the client's own id) and every provisional copy with the sender of
// its target, and forgets the id on every target. R4-1: a record for a
// breakpoint whose entry has ended (removed while its replay was in
// flight) is refused and changes nothing.
func TestBreakpointIDsStayWithTheirTarget(t *testing.T) {
	targets := []string{"page-1", "page-2", "page-3"}
	prop := func(ops []uint16) bool {
		d := NewDesiredState()
		d.active = "page-1"
		for i, op := range ops {
			target := targets[int(op>>2)%len(targets)]
			id := fmt.Sprintf("bp-%d", int(op>>4)%3)
			before := maps.Clone(d.bpIDs)
			staged := maps.Clone(d.staged[target])
			var copies []bpCopy
			for _, tg := range targets {
				if c, ok := d.staged[tg][id]; ok {
					copies = append(copies, bpCopy{target: tg, id: c, via: d.stagedVia[tg]})
				}
			}
			active := d.active
			switch op % 4 {
			case 0:
				cur := fmt.Sprintf("%s/%d", target, i)
				if (op>>6)%4 == 0 {
					d.Remove(breakpointKey(id))
					if d.setBreakpointID(target, id, cur, bpTarget{id: target}) || !maps.Equal(before, d.bpIDs) || !maps.Equal(staged, d.staged[target]) {
						t.Logf("op %d: record of the ended %s on %s was kept", i, id, target)
						return false
					}
					continue
				}
				d.Put(Entry{Key: breakpointKey(id)})
				if !d.setBreakpointID(target, id, cur, bpTarget{id: target}) {
					t.Logf("op %d: record of the live %s on %s refused", i, id, target)
					return false
				}
				if target == active {
					if d.bpIDs[id] != cur {
						t.Logf("op %d: record on the active %s: %v", i, target, d.bpIDs)
						return false
					}
				} else if !maps.Equal(before, d.bpIDs) || d.staged[target][id] != cur || d.stagedVia[target] != (bpTarget{id: target}) {
					t.Logf("op %d: record on provisional %s changed the active ids %v -> %v", i, target, before, d.bpIDs)
					return false
				}
			case 1:
				d.commitTarget(target)
				want := map[string]string{}
				maps.Copy(want, before)
				maps.Copy(want, staged)
				if d.active != target || !maps.Equal(want, d.bpIDs) || d.staged[target] != nil {
					t.Logf("op %d: commit of %s: ids %v, want %v", i, target, d.bpIDs, want)
					return false
				}
			case 2:
				d.abandonTarget(target)
				if d.active != active || !maps.Equal(before, d.bpIDs) || d.staged[target] != nil {
					t.Logf("op %d: destroying %s changed the active ids %v -> %v", i, target, before, d.bpIDs)
					return false
				}
			case 3:
				want, ok := before[id]
				if !ok {
					want = id
				}
				got, gotCopies := d.takeBreakpointID(id, "")
				if got != want {
					t.Logf("op %d: removal of %s sent %s, want %s", i, id, got, want)
					return false
				}
				if !slices.Equal(gotCopies, copies) {
					t.Logf("op %d: removal of %s took the copies %v, want %v", i, id, gotCopies, copies)
					return false
				}
				if _, left := d.bpIDs[id]; left {
					return false
				}
				for tg, ids := range d.staged {
					if _, left := ids[id]; left {
						t.Logf("op %d: %s still staged on %s", i, id, tg)
						return false
					}
				}
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}
