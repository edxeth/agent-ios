package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Core-events round 8: a provisional target's cleanup that is still on its
// way to it when it commits. A provisional target holds the state of its
// setup snapshot. The cleanup of that state reaches it only as a forwarded
// send of a release or of a reset (desired_sync.go). Until that send
// succeeded, the cleanup is the target's obligation; when the target
// commits first, the obligation must be pending on the active target
// (design 2.4: the released state is gone from the active backend, or a
// cleanup is pending), and it must end when the send succeeds or the
// target goes away, so that no later change resets the agent again (a
// reset drops other owners' scoped state, such as their breakpoints).

// provisionalWithReleasedHeader makes s1 set the header X-Released, which
// the active target page-1 and, through their setups, the provisional
// targets (page-2 when none is named) then hold.
func provisionalWithReleasedHeader(t *testing.T, targets ...string) (*rig, *Page, *headerBackend) {
	r := newRig(t, fakewebkit.Options{}, false, setupFirst)
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	b := &headerBackend{hdr: map[string]map[string]string{}}
	b.install(r.f)
	mo, err := r.m.Begin(ctx, r.req, agent, "headers", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-Released":"old"}}`)); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	if len(targets) == 0 {
		targets = []string{"page-2"}
	}
	for _, id := range targets {
		<-r.f.Provisional(id)
		if b.header(id, "X-Released") != "old" {
			t.Fatalf("fixture: the setup of %s did not send the header", id)
		}
	}
	return r, mo.Page, b
}

// disablesOn counts the Network.disable calls that target received.
func disablesOn(f *fakewebkit.Server, target string) int {
	n := 0
	for _, rec := range f.ReceivedMethod("Network.disable") {
		if rec.Target == target {
			n++
		}
	}
	return n
}

// receivedOn reports whether target received method with a parameter
// text that contains sub.
func receivedOn(f *fakewebkit.Server, target, method, sub string) bool {
	for _, rec := range f.ReceivedMethod(method) {
		if rec.Target == target && strings.Contains(string(rec.Params), sub) {
			return true
		}
	}
	return false
}

// queuedReleaseCleanup builds the cause of generated plan #11 (the fixed
// plan "commit with a queued cleanup") for the provisional targets: the
// release's Network.disable for each of them stays queued. Another
// change's send to the first target holds that target's sending slot past
// the release deadline, so the release returns before any of its
// forwarded sends went out; that change then sends each queue only up to
// its own item.
func queuedReleaseCleanup(t *testing.T, targets ...string) (*rig, *Page, *headerBackend) {
	r, pg, b := provisionalWithReleasedHeader(t, targets...)
	// These tests pin the marks of a commit and their end by the next
	// change, so the commit's background reset is held (holdCommitReset).
	holdCommitReset(r)
	d := pg.PS.State
	ctx := ctxFor(t, 10*time.Second)
	// Another header change of s1, made as legacyCall makes it: its send to
	// page-1 is answered, its forwarded send to the first target only when
	// the test sends releaseHold.
	const releaseHold = "Runtime.releaseObjectGroup"
	cs, end, err := d.beginChange(ctx, controlCaller{pg.conn})
	if err != nil {
		t.Fatal(err)
	}
	ch := d.SetHeaders(SessionOwner("legacy", "s1"), map[string]string{"X-Released": "old", "X-Other": "1"})
	if ch.NeedsNetworkReset || ch.Send == nil {
		t.Fatalf("fixture: header change %+v, want a plain send", ch)
	}
	if _, err := cs.Send(ctx, "Network.setExtraHTTPHeaders", map[string]any{"headers": ch.Send}); err != nil {
		t.Fatal(err)
	}
	r.f.HoldUntil("Network.setExtraHTTPHeaders", releaseHold)
	ended := make(chan error, 1)
	go func() { ended <- end(nil) }()
	eventually(t, 3*time.Second, "the held forwarded send", func() bool {
		return receivedOn(r.f, targets[0], "Network.setExtraHTTPHeaders", "X-Other")
	})
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	err = r.m.Break(short, pg.Key)
	cancel()
	t.Logf("release while %s's sending slot is held: %v", targets[0], err)
	if _, err := (controlCaller{pg.conn}).Send(ctx, releaseHold, map[string]string{"objectGroup": "r8"}); err != nil {
		t.Fatal(err)
	}
	if err := <-ended; err != nil {
		t.Fatalf("the change that held the slot: %v", err)
	}
	if b.header("page-1", "X-Released") != "" || d.isUnknown("Network") {
		t.Fatalf("fixture: want the release's cleanup done on page-1 and nothing pending (unknown %v)", d.unknownDomains())
	}
	for _, id := range targets {
		d.mu.Lock()
		queued := len(d.syncs[id].queue)
		d.mu.Unlock()
		if queued == 0 || b.header(id, "X-Released") != "old" {
			t.Fatalf("fixture: want the release's cleanup queued for %s (queued %d, X-Released %q)", id, queued, b.header(id, "X-Released"))
		}
	}
	return r, pg, b
}

// commit commits target and waits until the model made it active.
func commit(t *testing.T, r *rig, d *DesiredState, target string) {
	t.Helper()
	r.f.Commit(target)
	eventually(t, 3*time.Second, "commit of "+target, func() bool { return d.isActive(target) })
}

// nextHeaderChange makes a new session s2 set the header X-New and checks
// that target then has it and not the released header, that target got
// wantDisables Network.disable calls meanwhile, and that no agent stays
// unknown.
func nextHeaderChange(t *testing.T, r *rig, pg *Page, b *headerBackend, target string, wantDisables int) {
	t.Helper()
	ctx := ctxFor(t, 10*time.Second)
	disables := disablesOn(r.f, target)
	next, err := r.m.Begin(ctx, r.req, AcquireOptions{Session: "s2", Reservation: "agent"}, "next headers", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(next.ID)
	if _, err := r.m.Raw(ctx, next.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-New":"new"}}`)); err != nil {
		t.Fatal(err)
	}
	if v := b.header(target, "X-Released"); v != "" {
		t.Fatalf("the committed target %s keeps the released header X-Released: %s after the next Network change", target, v)
	}
	if v := b.header(target, "X-New"); v != "new" {
		t.Fatalf("committed target %s: header X-New = %q, want new", target, v)
	}
	if n := disablesOn(r.f, target) - disables; n != wantDisables {
		t.Fatalf("the next change sent %d Network.disable to %s, want %d", n, target, wantDisables)
	}
	if got := pg.PS.State.unknownDomains(); len(got) > 0 {
		t.Fatalf("unknown domains after the next change = %v, want none", got)
	}
}

// The commit makes the queued cleanup pending on the new active target.
// The next change sends the queued cleanup first, which ends the pending
// mark, so it does not reset the agent again: one Network.disable.
func TestQueuedProvisionalCleanupPendsAtCommit(t *testing.T) {
	r, pg, b := queuedReleaseCleanup(t, "page-2")
	d := pg.PS.State
	commit(t, r, d, "page-2")
	if !d.isUnknown("Network") {
		t.Fatalf("page-2 committed with its cleanup still queued and the released header X-Released: %s, but no cleanup is pending (unknown %v)",
			b.header("page-2", "X-Released"), d.unknownDomains())
	}
	nextHeaderChange(t, r, pg, b, "page-2", 1)
}

// A second commit replaces page-2 with page-3, while both still have the
// release's cleanup queued. The cleanup that page-3 is owed must stay
// pending; the mark of page-2's cleanup ends with page-2 and must not hide
// it.
func TestNextCommitKeepsItsOwnCleanupPending(t *testing.T) {
	r, pg, b := queuedReleaseCleanup(t, "page-2", "page-3")
	d := pg.PS.State
	commit(t, r, d, "page-2")
	if !d.isUnknown("Network") {
		t.Fatalf("fixture: want Network pending after the commit of page-2, unknown %v", d.unknownDomains())
	}
	commit(t, r, d, "page-3")
	eventually(t, 3*time.Second, "page-2's record dropped", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.syncs["page-2"] == nil
	})
	if !d.isUnknown("Network") {
		t.Fatalf("page-3 committed with its cleanup still queued and the released header X-Released: %s, but no cleanup is pending (unknown %v)",
			b.header("page-3", "X-Released"), d.unknownDomains())
	}
	nextHeaderChange(t, r, pg, b, "page-3", 1)
}

// A committed target that a later commit replaces takes the released state
// with it, and with it the pending mark of its queued cleanup: the next
// active target's setup snapshot is newer than the release, so nothing is
// pending for it and the next change does not reset its Network agent.
func TestReplacedTargetTakesItsQueuedCleanupMark(t *testing.T) {
	r, pg, b := queuedReleaseCleanup(t, "page-2")
	d := pg.PS.State
	commit(t, r, d, "page-2")
	if !d.isUnknown("Network") {
		t.Fatalf("fixture: want Network pending after the commit, unknown %v", d.unknownDomains())
	}
	<-r.f.Provisional("page-3")
	commit(t, r, d, "page-3")
	if got := d.unknownDomains(); len(got) > 0 {
		t.Fatalf("unknown domains after page-2 went with its queued cleanup = %v, want none", got)
	}
	nextHeaderChange(t, r, pg, b, "page-3", 0)
}

// The same as TestQueuedProvisionalCleanupPendsAtCommit with the cleanup
// in flight: page-2 received the release's Network.disable but has not run
// it when it commits.
func TestInFlightProvisionalCleanupPendsAtCommit(t *testing.T) {
	r, pg, b := provisionalWithReleasedHeader(t)
	d := pg.PS.State
	ctx := ctxFor(t, 10*time.Second)
	reached, committed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.f.Handle("Network.disable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if c.Target == "page-2" {
			once.Do(func() { close(reached) })
			<-committed
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.hdr, c.Target)
		return nil, nil
	})
	released := make(chan error, 1)
	go func() { released <- r.m.Break(ctx, pg.Key) }()
	<-reached
	commit(t, r, d, "page-2")
	pending := d.isUnknown("Network")
	close(committed)
	if err := <-released; err != nil {
		t.Fatalf("release = %v", err)
	}
	if !pending {
		t.Fatal("page-2 committed with the release's cleanup in flight and the released header, but no cleanup was pending")
	}
	if v := b.header("page-2", "X-Released"); v != "" {
		t.Fatalf("the committed target keeps the released header X-Released: %s", v)
	}
	if got := d.unknownDomains(); len(got) > 0 {
		t.Fatalf("unknown domains after the forwarded cleanup succeeded = %v, want none", got)
	}
}

// A target whose setup still runs when it commits, with a cleanup queued
// behind the setup: the commit makes the cleanup pending. When the setup
// fails, its queue is dropped unsent, and the failed setup is the pending
// state instead (ReapplySetup resets every agent), so the mark ends with
// the dropped item.
func TestFailedSetupEndsTheMarkOfItsQueuedCleanup(t *testing.T) {
	d := NewDesiredState()
	d.mu.Lock()
	d.active = "page-1"
	ts := d.registerLocked("page-2", nil) // its setup runs (busy)
	q := &queued{send: forwardCall("Network.disable", nil), cleanup: "Network", done: make(chan struct{})}
	ts.queue = append(ts.queue, q)
	d.mu.Unlock()
	d.commitTarget("page-2")
	if !d.isUnknown("Network") {
		t.Fatal("page-2 committed with a cleanup queued behind its setup, but Network is not pending")
	}
	setupErr := errors.New("setup refused")
	if err := d.finishSetup(context.Background(), "page-2", ts, setupErr); !errors.Is(err, setupErr) {
		t.Fatalf("finishSetup = %v, want the setup's error", err)
	}
	if !q.completed || q.err != nil {
		t.Fatalf("the dropped cleanup item: completed %v, err %v; want completed without error", q.completed, q.err)
	}
	if got := d.unknownDomains(); len(got) > 0 {
		t.Fatalf("unknown domains after the failed setup dropped the cleanup = %v, want none", got)
	}
}

// A cleanup send that the active target refuses because its agent is not
// enabled is in the state the cleanup wants there, and the release counts
// it as done (sendDisable). The provisional target got the agent enabled
// with the released state from its setup, so the send must still reach
// it.
func TestNotEnabledCleanupStillReachesProvisionalTargets(t *testing.T) {
	r, pg, b := provisionalWithReleasedHeader(t)
	d := pg.PS.State
	ctx := ctxFor(t, 10*time.Second)
	r.f.Handle("Network.disable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if c.Target == "page-1" {
			return nil, &fakewebkit.Error{Code: -32000, Message: "Network domain must be enabled"}
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.hdr, c.Target)
		return nil, nil
	})
	if err := r.m.Break(ctx, pg.Key); err != nil {
		t.Fatalf("release = %v, want the refusal as not enabled counted as done", err)
	}
	if v := b.header("page-2", "X-Released"); v != "" && !d.isUnknown("Network") {
		t.Fatalf("the provisional target keeps the released header X-Released: %s, and no cleanup is pending: the release's Network.disable did not reach it", v)
	}
	commit(t, r, d, "page-2")
	if v := b.header("page-2", "X-Released"); v != "" && !d.isUnknown("Network") {
		t.Fatalf("the committed target keeps the released header X-Released: %s, and no cleanup is pending", v)
	}
}
