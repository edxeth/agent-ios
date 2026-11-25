package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W3b rebase repair round 1, finding F2 (docs/notes/w3b.md; the pending
// cleanup of release_lifecycle.go, core-events r4): a W3b owner's release
// keeps a cleanup send that failed pending after the owner left the model
// (PageSession.ReleaseOwnerChange), and the next W3b use of the domain
// resets it first.

// The reviewer's reproduction (TestReviewTemporaryOwnerFailedDisableStaysPending),
// assertions unchanged: the temporary owner's give-back sends
// Runtime.disable, the page refuses it, and Runtime stays unknown.
func TestTemporaryOwnerFailedDisableStaysPending(t *testing.T) {
	fr := newFrameReleaseRig(t, 17, "")
	fr.f.Handle("Runtime.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		if !fr.ps.State.Needs("Runtime") {
			return nil, &fakewebkit.Error{Code: -32000, Message: "temporary disable refused"}
		}
		return map[string]any{}, nil
	})
	err := fr.ps.ReleaseFrameContexts(ctxFor(t, 5*time.Second), fr.epoch, agent.Session, fr.ps.Caller, func(context.Context, webkit.Caller) error { return nil })
	if err == nil {
		t.Fatal("disable refusal not returned")
	}
	if !fr.ps.State.isUnknown("Runtime") {
		t.Fatalf("failed temporary Runtime.disable lost its pending cleanup: err %v; owned=%v; agent=%v; want Runtime unknown", err, fr.ps.State.Needs("Runtime"), fr.pg.conn.AgentStates(fr.pg.conn.ActiveTarget()))
	}
}

// runtimeCallsSince returns the Runtime.enable and Runtime.disable calls
// that the page received after the first n records, in order.
func runtimeCallsSince(f *fakewebkit.Server, n int) []string {
	var out []string
	for _, rec := range f.Received()[n:] {
		if rec.Method == "Runtime.enable" || rec.Method == "Runtime.disable" {
			out = append(out, rec.Method)
		}
	}
	return out
}

// The temporary owner's acquisition resets a Runtime cleanup that an
// earlier release left pending before its Runtime.enable, in the same
// change, while no owner holds Runtime (ResetPendingRuntime); a reset that
// the page refuses acquires nothing and keeps both the pending cleanup and
// the frame's record.
func TestTemporaryOwnerAcquisitionResetsAPendingRuntimeCleanup(t *testing.T) {
	pending := func(fr *frameReleaseRig) {
		fr.ps.State.mu.Lock()
		fr.ps.State.markUnknownLocked("Runtime")
		fr.ps.State.mu.Unlock()
	}
	t.Run("reset", func(t *testing.T) {
		fr := newFrameReleaseRig(t, 17, "")
		pending(fr)
		n := len(fr.f.Received())
		if err := fr.ps.ReleaseFrameContexts(ctxFor(t, 5*time.Second), fr.epoch, agent.Session, fr.ps.Caller, func(context.Context, webkit.Caller) error { return nil }); err != nil {
			t.Fatalf("ReleaseFrameContexts: %v", err)
		}
		if got, want := runtimeCallsSince(fr.f, n), []string{"Runtime.disable", "Runtime.enable", "Runtime.disable"}; !slices.Equal(got, want) {
			t.Fatalf("Runtime calls %q, want %q (the pending reset, the temporary enable, its give-back)", got, want)
		}
		if fr.ps.State.isUnknown("Runtime") {
			t.Fatal("Runtime still unknown after its reset succeeded")
		}
	})
	t.Run("another owner holds Runtime", func(t *testing.T) {
		// Design 2.4: Runtime.disable goes out only when the last owner
		// leaves (TestFrameContextsProperty). The holder's Runtime.enable
		// came after the failed disable, so nothing is reset, and the mark
		// stays for a reset after the last owner left.
		// The holder's re-apply on the new connection reported no
		// context for the frame (newID 0), so the release takes Runtime
		// as the temporary owner and finds the frame gone.
		fr := newFrameReleaseRig(t, 0, "test-holder")
		pending(fr)
		n := len(fr.f.Received())
		if err := fr.ps.ReleaseFrameContexts(ctxFor(t, 5*time.Second), fr.epoch, agent.Session, fr.ps.Caller, func(context.Context, webkit.Caller) error { return nil }); !errors.Is(err, errFrameGone) {
			t.Fatalf("ReleaseFrameContexts = %v, want the frame gone", err)
		}
		if got := runtimeCallsSince(fr.f, n); slices.Contains(got, "Runtime.disable") {
			t.Fatalf("Runtime calls %q: Runtime.disable sent while another owner holds Runtime", got)
		}
		if !fr.ps.State.isUnknown("Runtime") {
			t.Fatal("the pending Runtime cleanup was dropped without a reset")
		}
	})
	t.Run("refused", func(t *testing.T) {
		fr := newFrameReleaseRig(t, 17, "")
		pending(fr)
		fr.f.Handle("Runtime.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
			return nil, &fakewebkit.Error{Code: -32000, Message: "reset refused"}
		})
		n := len(fr.f.Received())
		err := fr.ps.ReleaseFrameContexts(ctxFor(t, 5*time.Second), fr.epoch, agent.Session, fr.ps.Caller, func(context.Context, webkit.Caller) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "reset refused") {
			t.Fatalf("ReleaseFrameContexts = %v, want the refused reset", err)
		}
		if got := runtimeCallsSince(fr.f, n); slices.Contains(got, "Runtime.enable") {
			t.Fatalf("Runtime calls %q: the temporary owner enabled Runtime after its pending reset failed", got)
		}
		if !fr.ps.State.isUnknown("Runtime") {
			t.Fatal("the refused reset removed the pending Runtime cleanup")
		}
		if got := fr.ps.Sched.unreportedFrames(fr.epoch); !slices.Equal(got, []string{"kid"}) {
			t.Fatalf("recorded frames without a context after the refused reset = %q, want [kid]", got)
		}
	})
}

// ReleaseOwnerChange runs the Network reinitialization barrier for a
// header that lost its last owner while Network stays owned, as a lease
// release does (releaseOwners); a refused reset keeps the Network cleanup
// pending after the owner left.
func TestReleaseOwnerChangeFailedNetworkResetStaysPending(t *testing.T) {
	r, pg, epoch, owner, b := attachmentHeader(t)
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "legacy header", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-Legacy":"1"}}`)); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	b.refuseOn("page-1")
	mo, err = r.m.BeginLeased(ctx, pg, agent.Session, epoch, "release owner", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	ran, err := pg.PS.ReleaseOwnerChange(mo.Op.Context(), mo.Op.Caller(), owner)
	r.m.End(mo.ID)
	if !ran || err == nil || !strings.Contains(err.Error(), "cleanup refused") {
		t.Fatalf("ReleaseOwnerChange = %v, %v; want the change run and the refused Network reset", ran, err)
	}
	if hasOwner(pg, owner) {
		t.Fatalf("owner %s still in the model after its release", owner)
	}
	if !pg.PS.State.isUnknown("Network") {
		t.Fatalf("unknown domains after the refused Network reset = %v, want Network pending", pg.PS.State.unknownDomains())
	}
}

// ReleaseOwnerChange's sends are cleanup sends (asCleanup), as a lease
// release's (core-events round 4, part 2): a forwarded cleanup that a
// provisional target refuses stays pending with that target and marks its
// domain when the target commits.
func TestReleaseOwnerChangeFailedProvisionalCleanupPendsUntilCommit(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, setupFirst)
	r.reserve(t, "agent")
	// The commit's background reset is held: this test pins the mark that
	// the commit sets and its end by the next change (holdCommitReset).
	holdCommitReset(r)
	ctx := ctxFor(t, 10*time.Second)
	b := &headerBackend{hdr: map[string]map[string]string{}}
	b.install(r.f)
	pg, epoch, err := r.m.Touch(ctx, r.req, agent)
	if err != nil {
		t.Fatal(err)
	}
	owner := SessionOwner("w3b-test", agent.Session)
	mo, err := r.m.BeginLeased(ctx, pg, agent.Session, epoch, "header", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.RawAs(ctx, mo, owner, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-Released":"old"}}`)); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	<-r.f.Provisional("page-2")
	if b.header("page-2", "X-Released") != "old" {
		t.Fatal("fixture: the provisional setup did not send the header")
	}
	b.refuseOn("page-2")
	mo, err = r.m.BeginLeased(ctx, pg, agent.Session, epoch, "release owner", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pg.PS.ReleaseOwnerChange(mo.Op.Context(), mo.Op.Caller(), owner)
	r.m.End(mo.ID)
	if err == nil || !strings.Contains(err.Error(), "cleanup refused") {
		t.Fatalf("fixture: ReleaseOwnerChange = %v, want the refused forwarded cleanup", err)
	}
	if b.header("page-1", "X-Released") != "" || pg.PS.State.isUnknown("Network") {
		t.Fatalf("the release did not clean the active target (X-Released %q), or marked it (unknown %v)", b.header("page-1", "X-Released"), pg.PS.State.unknownDomains())
	}
	b.refuseOn("")
	r.f.Commit("page-2")
	eventually(t, 3*time.Second, "commit", func() bool { return pg.PS.State.isActive("page-2") })
	if !pg.PS.State.isUnknown("Network") {
		t.Fatalf("after the commit, unknown domains = %v, want the refused forwarded Network cleanup pending", pg.PS.State.unknownDomains())
	}
}

// The reviewer's reproduction TestReviewScopedNodeChangeDoesNotRetargetTheNode
// (finding F1), with the send through the operation's own caller inside the
// change, as hover --css and highlight send their node ids: a change's
// caller addresses the change's target (desired_sync.go), which a commit
// may have replaced since the node lookup, while the operation's caller
// keeps the frame context's target fence. Assertions unchanged.
func TestScopedNodeSendInsideAChangeKeepsTheFence(t *testing.T) {
	f, sess := targetPage(t)
	ctx := ctxFor(t, 5*time.Second)
	s := NewScheduler(sess, SchedulerOptions{})
	t.Cleanup(s.Close)
	s.SetLease("s1", 1)
	d := NewDesiredState()
	d.active = sess.ActiveTarget()
	d.Acquire(SessionOwner("frame", "s1"), "Runtime")
	d.followTargets(sess)
	op, err := s.Begin(ctx, OpSpec{Session: "s1", Epoch: 1, Name: "hover", Class: ClassMutate})
	if err != nil {
		t.Fatal(err)
	}
	defer op.End()
	ec := s.FrameContext("kid")
	if ec.ID != 17 {
		t.Fatalf("context %v", ec)
	}
	op.SetExecutionContext(ec)
	if err := commitPage2(t, ctx, f, sess); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, "state to follow page-2", func() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.active == "page-2" })
	_, direct := op.Caller().Send(ctx, "CSS.forcePseudoState", map[string]any{"nodeId": 42, "forcedPseudoClasses": []string{"hover"}})
	if !errors.Is(direct, webkit.ErrNotSent) || !errors.Is(direct, webkit.ErrTargetSwapped) {
		t.Fatalf("unwrapped send %v, want refusal", direct)
	}
	err = d.Change(ctx, op.Caller(), func(webkit.Caller) error {
		_, err := op.Caller().Send(ctx, "CSS.forcePseudoState", map[string]any{"nodeId": 42, "forcedPseudoClasses": []string{"hover"}})
		return err
	})
	got := f.ReceivedMethod("CSS.forcePseudoState")
	if len(got) != 0 {
		t.Fatalf("scoped node from page-1 reached %s after Change (err %v); want refusal and no delivery", got[0].Target, err)
	}
	if !errors.Is(err, webkit.ErrTargetSwapped) {
		t.Fatalf("Change returned %v, want ErrTargetSwapped", err)
	}
}
