package core

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// The pending-cleanup transaction (core-events round 4): the reviewer's
// round-3 reproductions, kept as regression tests.

// Design 2.4: a cleanup send that WebKit refuses keeps its obligation, so
// no successor gets the released header (WIP merges header maps).
func TestReviewFailedCleanupStaysPending(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "headers", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-Released":"old"}}`)); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	var backendMu sync.Mutex
	backend := map[string]string{"X-Released": "old"}
	r.f.Handle("Network.setExtraHTTPHeaders", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Headers map[string]string `json:"headers"`
		}
		_ = json.Unmarshal(c.Params, &p)
		backendMu.Lock()
		defer backendMu.Unlock()
		for k, v := range p.Headers {
			backend[k] = v
		}
		return nil, nil
	})
	r.f.Handle("Network.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "cleanup refused"}
	})
	if err := r.m.Break(ctx, mo.Page.Key); err == nil {
		t.Fatal("fixture: expected cleanup failure")
	}
	t.Logf("unknown domains after failed cleanup: %v", mo.Page.PS.State.unknownDomains())
	r.f.Handle("Network.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		backendMu.Lock()
		clear(backend)
		backendMu.Unlock()
		return nil, nil
	})
	next, err := r.m.Begin(ctx, r.req, AcquireOptions{Session: "s2", Reservation: "agent"}, "next headers", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(next.ID)
	if _, err := r.m.Raw(ctx, next.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-New":"new"}}`)); err != nil {
		t.Fatal(err)
	}
	backendMu.Lock()
	leaked := backend["X-Released"]
	backendMu.Unlock()
	if leaked != "" {
		t.Fatal("released header survives failed cleanup and the successor's Network change; no pending reset was recorded")
	}
}

// Design 2.4 and 2.10: a setup that runs again after its target committed
// is an active Network reset, so it accounts for the pending holds.
func TestReviewCommittedSetupResetAccountsForHolds(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{Session: webkit.SessionOptions{PauseOnStart: true}})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 15*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "headers", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-Released":"old"}}`)); err != nil {
		t.Fatal(err)
	}
	for _, call := range [][2]string{
		{"Network.setInterceptionEnabled", `{"enabled":true}`},
		{"Network.addInterception", `{"url":"https://fixture.example/api","stage":"request"}`},
	} {
		if _, err := r.m.Raw(ctx, mo.ID, call[0], json.RawMessage(call[1])); err != nil {
			t.Fatal(err)
		}
	}
	r.m.End(mo.ID)
	pg := mo.Page
	_, end, err := pg.PS.State.beginChange(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = end(nil) }()
	r.f.DelayReplies("Network.addInterception", 3*time.Second)
	r.f.Provisional("page-2")
	eventually(t, 3*time.Second, "provisional interception configured", func() bool {
		for _, rec := range r.f.ReceivedMethod("Network.addInterception") {
			if rec.Target == "page-2" {
				return true
			}
		}
		return false
	})
	// WebKit resumes a target after ResumeAfter even when its setup is still pending.
	eventually(t, 4*time.Second, "provisional resumed before setup", func() bool {
		for _, rec := range r.f.ReceivedMethod("Target.resume") {
			if strings.Contains(string(rec.Params), "page-2") {
				return true
			}
		}
		return false
	})
	r.f.Commit("page-2")
	eventually(t, 3*time.Second, "committed", func() bool { return pg.conn.ActiveTarget() == "page-2" })
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_ = r.m.Break(short, pg.Key)
	cancel()
	// New page activity after the release's barrier snapshot but before setup retries.
	r.f.Intercept("page-2", "post-release", "request")
	eventually(t, 3*time.Second, "post-release hold", func() bool { return len(pg.PS.Sched.PendingRequests()) == 1 })
	r.f.DelayReplies("Network.addInterception", 0)
	eventually(t, 3*time.Second, "setup retry disabled Network", func() bool {
		return len(r.f.HeldRequests("page-2")) == 0
	})
	// The fake drops the hold when it runs the reset's continue, before the
	// scheduler gets the reply and records the reset (resetTargetHolds), so
	// wait until the setup retry, which records it before its Network
	// reset, has ended.
	setupCtx, setupCancel := context.WithTimeout(ctx, 3*time.Second)
	defer setupCancel()
	if err := pg.conn.ActiveSetup(setupCtx); setupCtx.Err() != nil {
		t.Fatalf("the setup retry of page-2 did not end: %v", err)
	}
	pending := pg.PS.Sched.PendingRequests()
	_, err = pg.PS.Sched.control(ctx, ControlMsg{Kind: ControlCompleteRequest, TargetID: "page-2", RequestID: "post-release", Method: "Network.interceptContinue"})
	t.Logf("pending=%v completion error=%v", pending, err)
	if !errors.Is(err, ErrRequestReset) {
		t.Fatal("setup continued a committed target's request without recording its Network reset; completion should be refused as ErrRequestReset")
	}
	// Design 2.4 step 2: the reset continues the held request through the
	// control lane before its Network.disable.
	if !slices.Contains(r.f.Answers(), fakewebkit.Answer{Target: "page-2", RequestID: "post-release", Stage: "request", Method: "Network.interceptContinue"}) {
		t.Fatalf("answers %+v, want the reset's continue of post-release on page-2", r.f.Answers())
	}
}

// Design 2.4 and 2.10: every successful Network.disable on the installed
// connection, here the legacy disable of the last Network owner without
// any barrier, records the requests it continued as reset
// (webkit.SessionOptions.NetworkDisabled), so a later completion fails with
// ErrRequestReset instead of WebKit's "Missing pending intercept ...".
func TestNetworkDisableRecordsHeldRequestsAsReset(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "monitor", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(mo.ID)
	if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
		t.Fatal(err)
	}
	pg := mo.Page
	r.f.Intercept("page-1", "r1", "request")
	eventually(t, 3*time.Second, "held request", func() bool { return len(pg.PS.Sched.PendingRequests()) == 1 })
	if _, err := r.m.Raw(ctx, mo.ID, "Network.disable", nil); err != nil {
		t.Fatal(err)
	}
	if held := r.f.HeldRequests("page-1"); len(held) != 0 {
		t.Fatalf("fixture: the backend still holds %v after Network.disable", held)
	}
	if p := pg.PS.Sched.PendingRequests(); len(p) != 0 {
		t.Fatalf("pending requests when Network.disable returned = %v, want none", p)
	}
	_, err = pg.PS.Sched.control(ctx, ControlMsg{Kind: ControlCompleteRequest, TargetID: "page-1", RequestID: "r1", Method: "Network.interceptContinue"})
	if !errors.Is(err, ErrRequestReset) {
		t.Fatalf("completion after Network.disable = %v, want ErrRequestReset", err)
	}
}

// blockingTarget is a provisional target that never answers.
type blockingTarget struct{ id string }

func (b blockingTarget) TargetID() string { return b.id }

func (b blockingTarget) Send(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// Design 2.6: a change waits for its sends to provisional targets at most
// until its command's deadline, not forwardBudget past it. Found by the
// lifecycle property (a release with a provisional target on a stalled
// connection returned 9.9 s after its 1 s deadline).
func TestChangeForwardingEndsAtTheDeadline(t *testing.T) {
	d := NewDesiredState()
	d.mu.Lock()
	ts := d.registerLocked("p1", blockingTarget{"p1"})
	ts.busy = false // the setup ended
	close(ts.idle)
	d.mu.Unlock()
	const deadline = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	cs, end, err := d.beginChange(ctx, newSwapCaller())
	if err != nil {
		t.Fatal(err)
	}
	d.SetHeaders("legacy:s1", map[string]string{"X-A": "1"})
	if _, err := cs.Send(ctx, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"X-A": "1"}}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = end(nil)
	if took := time.Since(start); took > deadline+time.Second {
		t.Fatalf("end returned after %v, want by the %v deadline", took, deadline)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("end = %v, want the deadline of the forwarded send", err)
	}
}

// An unknown-agent reset can outlive another release. It must not clear
// that release's pending mark.
func TestReviewResetOverlappingReleaseKeepsPendingCleanup(t *testing.T) {
	r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "bootstrap", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.m.Raw(ctx, mo.ID, "Page.setBootstrapScript", json.RawMessage(`{"source":"window.releasedMarker = 1"}`)); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	pg := mo.Page
	// Takeover retains s1's desired state. s2 adds a second bootstrap part.
	stealPage(t, r.f)
	eventually(t, 3*time.Second, "lost", func() bool { return pg.ConnState() == ConnLost })
	other, err := r.m.Begin(ctx, r.req, AcquireOptions{Session: "s2", Reservation: "agent", Takeover: true}, "other bootstrap", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.m.Raw(ctx, other.ID, "Page.setBootstrapScript", json.RawMessage(`{"source":"window.otherMarker = 1"}`)); err != nil {
		t.Fatal(err)
	}
	r.m.End(other.ID)
	_, unlock, err := pg.PS.State.beginChange(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	short0, cancel0 := context.WithTimeout(ctx, 100*time.Millisecond)
	_ = r.m.Break(short0, pg.Key)
	cancel0()
	if !pg.PS.State.isUnknown("Page") {
		t.Fatal("fixture: s2 release did not leave Page pending")
	}
	// Grant s1 again without sending ordinary work. The background barrier need not block lease grants.
	if _, _, err = r.m.Touch(ctx, r.req, agent); err != nil {
		t.Fatal(err)
	}
	started, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.f.Handle("Page.setBootstrapScript", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if strings.Contains(string(c.Params), "releasedMarker") {
			once.Do(func() { close(started); <-proceed })
		}
		return nil, nil
	})
	defer func() {
		select {
		case <-proceed:
		default:
			close(proceed)
		}
	}()
	if err := unlock(nil); err != nil {
		t.Fatal(err)
	}
	<-started
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_ = r.m.Break(short, pg.Key)
	cancel()
	if !pg.PS.State.isUnknown("Page") {
		t.Fatal("fixture: release should have marked Page unknown")
	}
	close(proceed)
	// Wait behind all resets that might already have been launched by the release.
	if err := resetUnknown(ctx, pg); err != nil {
		t.Fatal(err)
	}
	var last json.RawMessage
	for _, rec := range r.f.ReceivedMethod("Page.setBootstrapScript") {
		last = rec.Params
	}
	t.Logf("owners=%v unknown=%v", pg.PS.State.AllOwners(), pg.PS.State.unknownDomains())
	if strings.Contains(string(last), "releasedMarker") {
		t.Fatal("in-flight background reset reapplied released bootstrap state, cleared the newer unknown mark, and left no pending cleanup")
	}
}

// Design 2.7 (W4 round 7 watchout): a provisional setup that WebKit refused
// is applied again with the current desired state at the next Begin and at
// a lease release, before gated sends on its target resume. While the
// refusal lasts, Begin reports the failure and does not loop.
func TestReviewFailedSetupIsRecovered(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	pg, _, err := r.m.Touch(ctx, r.req, agent)
	if err != nil {
		t.Fatal(err)
	}
	var refuse sync.Mutex
	refusing := true
	r.f.Handle("Page.setBootstrapScript", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		refuse.Lock()
		defer refuse.Unlock()
		if refusing && c.Target == "page-2" {
			return nil, &fakewebkit.Error{Code: -32000, Message: "transient setup refusal"}
		}
		return nil, nil
	})
	resumed := r.f.Provisional("page-2")
	<-resumed
	eventually(t, 3*time.Second, "provisional setup sends", func() bool {
		for _, rec := range r.f.ReceivedMethod("Page.setBootstrapScript") {
			if rec.Target == "page-2" {
				return true
			}
		}
		return false
	})
	r.f.Commit("page-2")
	eventually(t, 3*time.Second, "commit", func() bool { return pg.conn.ActiveTarget() == "page-2" })
	// The provisional setup has ended (failed) before the count starts.
	if err := pg.conn.ActiveSetup(ctx); !errors.As(err, new(*webkit.SetupError)) {
		t.Fatalf("fixture: ActiveSetup after the commit = %v, want the SetupError", err)
	}
	attempts := func() int { return len(r.f.ReceivedMethod("Page.setBootstrapScript")) }
	before := attempts()
	_, err = r.m.Begin(ctx, r.req, agent, "while refused", ClassExclusive)
	var se *webkit.SetupError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "applying the page state again") {
		t.Fatalf("Begin while the refusal lasts = %v, want the failed recovery with its SetupError", err)
	}
	if got := attempts() - before; got < 1 || got > 2 {
		t.Fatalf("Begin sent %d bootstrap scripts, want one recovery attempt (one reset and one apply at most)", got)
	}
	refuse.Lock()
	refusing = false
	refuse.Unlock()
	if err := r.m.Break(ctx, pg.Key); err != nil {
		t.Fatal(err)
	}
	// The release starts the recovery; the next Begin waits behind it.
	next, err := r.m.Begin(ctx, r.req, AcquireOptions{Session: "s2", Reservation: "agent"}, "after release", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin after the refusal ended: %v", err)
	}
	defer r.m.End(next.ID)
	if err := pg.conn.ActiveSetup(ctx); err != nil {
		t.Fatalf("ActiveSetup after recovery: %v", err)
	}
	if _, err := r.m.Raw(ctx, next.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"1"}`)); err != nil {
		t.Fatalf("gated send after recovery: %v", err)
	}
	var lastScript json.RawMessage
	for _, rec := range r.f.ReceivedMethod("Page.setBootstrapScript") {
		if rec.Target == "page-2" {
			lastScript = rec.Params
		}
	}
	if !strings.Contains(string(lastScript), "__iwdp") {
		t.Fatalf("last bootstrap script on page-2 = %s, want the desired runtime part", lastScript)
	}
}

// headerBackend models the extra headers of each target's backend:
// WebKit merges a Network.setExtraHTTPHeaders map into it, and
// Network.disable clears it. The target refuse refuses Network.disable.
type headerBackend struct {
	mu     sync.Mutex
	hdr    map[string]map[string]string
	refuse string
}

func (b *headerBackend) install(f *fakewebkit.Server) {
	f.Handle("Network.setExtraHTTPHeaders", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Headers map[string]string `json:"headers"`
		}
		_ = json.Unmarshal(c.Params, &p)
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.hdr[c.Target] == nil {
			b.hdr[c.Target] = map[string]string{}
		}
		maps.Copy(b.hdr[c.Target], p.Headers)
		return nil, nil
	})
	f.Handle("Network.disable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c.Target == b.refuse {
			return nil, &fakewebkit.Error{Code: -32000, Message: "cleanup refused"}
		}
		delete(b.hdr, c.Target)
		return nil, nil
	})
}

func (b *headerBackend) header(target, name string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hdr[target][name]
}

func (b *headerBackend) refuseOn(target string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refuse = target
}

// setupFirst makes the session pause every provisional target until its
// setup ended (Session.resumeLater), so fakewebkit.Provisional returns only
// then: after Apply's check for a release after its snapshot, which would
// set the target up again. ResumeAfter is far beyond any setup.
var setupFirst = ManagerOptions{Session: webkit.SessionOptions{PauseOnStart: true, ResumeAfter: time.Minute}}

// failedProvisionalCleanup releases s1, whose header X-Released the active
// target page-1 and the provisional target page-2 hold, while page-2
// refuses the release's forwarded Network.disable.
func failedProvisionalCleanup(t *testing.T) (*rig, *Page, *headerBackend) {
	r := newRig(t, fakewebkit.Options{}, false, setupFirst)
	r.reserve(t, "agent")
	// The commit's background reset is held: the tests of this fixture pin
	// the mark that a commit sets and its end by the next change
	// (holdCommitReset).
	holdCommitReset(r)
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
	pg := mo.Page
	<-r.f.Provisional("page-2")
	if b.header("page-2", "X-Released") != "old" {
		t.Fatal("fixture: the provisional setup did not send the header")
	}
	b.refuseOn("page-2")
	if err := r.m.Break(ctx, pg.Key); err == nil || !strings.Contains(err.Error(), "cleanup refused") {
		t.Fatalf("fixture: release = %v, want the refused forwarded cleanup", err)
	}
	if b.header("page-1", "X-Released") != "" {
		t.Fatal("fixture: the release did not clean the active target")
	}
	return r, pg, b
}

// Design 2.4 and 2.7 (core-events round 4, part 2): a release's cleanup
// that a provisional target refused stays pending with that target. A
// reset of the old active target does not complete it; the commit makes
// it pending on the new active target, so the next Network change resets
// that agent and the released header is gone.
func TestFailedProvisionalCleanupPendsUntilCommit(t *testing.T) {
	r, pg, b := failedProvisionalCleanup(t)
	ctx := ctxFor(t, 10*time.Second)
	if err := resetUnknown(ctx, pg); err != nil { // the old active target's reset
		t.Fatal(err)
	}
	b.refuseOn("")
	r.f.Commit("page-2")
	eventually(t, 3*time.Second, "commit", func() bool { return pg.PS.State.isActive("page-2") })
	if !pg.PS.State.isUnknown("Network") {
		t.Fatalf("after the commit, unknown domains = %v, want the refused Network cleanup pending", pg.PS.State.unknownDomains())
	}
	next, err := r.m.Begin(ctx, r.req, AcquireOptions{Session: "s2", Reservation: "agent"}, "next headers", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(next.ID)
	if _, err := r.m.Raw(ctx, next.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-New":"new"}}`)); err != nil {
		t.Fatal(err)
	}
	if v := b.header("page-2", "X-Released"); v != "" {
		t.Fatalf("the committed target keeps the released header X-Released: %s after the next Network change", v)
	}
	if v := b.header("page-2", "X-New"); v != "new" {
		t.Fatalf("committed target header X-New = %q, want new", v)
	}
}

// Design 2.7: a provisional target destroyed before commit takes the state
// of its failed cleanup with it, so nothing stays pending on the active
// target, whose cleanup succeeded.
func TestFailedProvisionalCleanupGoesWithDestroyedTarget(t *testing.T) {
	r, pg, _ := failedProvisionalCleanup(t)
	d := pg.PS.State
	r.f.DestroyTarget("page-2")
	eventually(t, 3*time.Second, "destroy", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.syncs["page-2"] == nil
	})
	if got := d.unknownDomains(); len(got) > 0 {
		t.Fatalf("unknown domains after the destroy = %v, want none: the active target's cleanup succeeded", got)
	}
}

// Design 2.4: the reset of an unknown agent is a cleanup too. A release
// that could not send its cleanup (the change lock stayed taken past its
// deadline) leaves Network unknown; its reset reaches the active target,
// but the provisional target refuses the forwarded Network.disable, so the
// cleanup stays pending with that target until a reset succeeds there
// after its commit.
func TestFailedProvisionalResetPendsUntilCommit(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, setupFirst)
	r.reserve(t, "agent")
	// The commit's background reset is held: this test pins the mark that
	// the commit sets and its end by the next change (holdCommitReset).
	holdCommitReset(r)
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
	pg, d := mo.Page, mo.Page.PS.State
	<-r.f.Provisional("page-2")
	if b.header("page-2", "X-Released") != "old" {
		t.Fatal("fixture: the provisional setup did not send the header")
	}
	b.refuseOn("page-2")
	_, unlock, err := d.beginChange(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_ = r.m.Break(short, pg.Key)
	cancel()
	if !d.isUnknown("Network") {
		t.Fatal("fixture: the release without cleanup did not leave Network unknown")
	}
	if err := unlock(nil); err != nil {
		t.Fatal(err)
	}
	// The release's background reset runs once the lock is free. Wait until
	// its Network.disable, forwarded to page-2, was sent and page-2's queue
	// is idle again (the refusal is recorded): the change lock alone does
	// not order this, because a change waits for its forwarded sends after
	// it frees the lock.
	eventually(t, 3*time.Second, "the reset's forwarded Network.disable", func() bool {
		n := 0
		for _, rec := range r.f.ReceivedMethod("Network.disable") {
			if rec.Target == "page-2" {
				n++ // the setup's reset, then the forwarded reset
			}
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		ts := d.syncs["page-2"]
		return n >= 2 && ts != nil && !ts.busy && len(ts.queue) == 0
	})
	if b.header("page-1", "X-Released") != "" || d.isUnknown("Network") {
		t.Fatalf("fixture: the reset of the active target did not complete (unknown %v)", d.unknownDomains())
	}
	b.refuseOn("")
	r.f.Commit("page-2")
	eventually(t, 3*time.Second, "commit", func() bool { return d.isActive("page-2") })
	if !d.isUnknown("Network") {
		t.Fatalf("after the commit, unknown domains = %v, want the refused Network reset pending", d.unknownDomains())
	}
	next, err := r.m.Begin(ctx, r.req, AcquireOptions{Session: "s2", Reservation: "agent"}, "next headers", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(next.ID)
	if _, err := r.m.Raw(ctx, next.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-New":"new"}}`)); err != nil {
		t.Fatal(err)
	}
	if v := b.header("page-2", "X-Released"); v != "" {
		t.Fatalf("the committed target keeps the released header X-Released: %s after the next Network change", v)
	}
}

// Design 2.4 and 2.7: a release's cleanup send that a commit cuts off
// (ErrTargetSwapped) is pending until its forwarded send reaches the
// committed target, and no longer: when that send succeeds, nothing stays
// unknown, so no later change resets the agent again (a reset drops other
// owners' scoped state, such as their breakpoints).
func TestCutOffCleanupEndsWithItsForwardedSend(t *testing.T) {
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
	pg, d := mo.Page, mo.Page.PS.State
	<-r.f.Provisional("page-2")
	if b.header("page-2", "X-Released") != "old" {
		t.Fatal("fixture: the provisional setup did not send the header")
	}
	// page-1 answers the release's Network.disable only after page-2
	// committed, so the session ends that call with ErrTargetSwapped.
	reached, committed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.f.Handle("Network.disable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if c.Target == "page-1" {
			once.Do(func() { close(reached) })
			<-committed
			return nil, nil
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.hdr, c.Target)
		return nil, nil
	})
	released := make(chan error, 1)
	go func() { released <- r.m.Break(ctx, pg.Key) }()
	<-reached
	r.f.Commit("page-2")
	eventually(t, 3*time.Second, "commit", func() bool { return d.isActive("page-2") })
	close(committed)
	if err := <-released; err != nil {
		t.Fatalf("release = %v, want the cut-off cleanup done by its forwarded send", err)
	}
	if v := b.header("page-2", "X-Released"); v != "" {
		t.Fatalf("the committed target keeps the released header X-Released: %s", v)
	}
	if got := d.unknownDomains(); len(got) > 0 {
		t.Fatalf("unknown domains after the forwarded cleanup succeeded = %v, want none", got)
	}
}
