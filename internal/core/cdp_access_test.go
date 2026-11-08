package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// The CDP endpoint's core API (cdp_access.go) under the release and
// attach lifecycle of release_lifecycle.go (W5a rebase onto core-events).

// attachmentHeader leases the rig's page for agent's session and sets the
// header X-Released through RawAs with the attachment owner cdp-A, as a CDP
// page session's Network.setExtraHTTPHeaders does. The header backend
// models what page-1 holds.
func attachmentHeader(t *testing.T) (*rig, *Page, uint64, Owner, *headerBackend) {
	t.Helper()
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	b := &headerBackend{hdr: map[string]map[string]string{}}
	b.install(r.f)
	ctx := ctxFor(t, 10*time.Second)
	pg, epoch, err := r.m.Touch(ctx, r.req, agent)
	if err != nil {
		t.Fatal(err)
	}
	owner := SessionOwner("cdp-A", agent.Session)
	mo, err := r.m.BeginLeased(ctx, pg, agent.Session, epoch, "cdp Network.setExtraHTTPHeaders", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.RawAs(ctx, mo, owner, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-Released":"old"}}`)); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	if b.header("page-1", "X-Released") != "old" {
		t.Fatal("fixture: the attachment's header did not reach page-1")
	}
	return r, pg, epoch, owner, b
}

func hasOwner(pg *Page, o Owner) bool { return slices.Contains(pg.PS.State.AllOwners(), o) }

// Design 2.4 and 5.2 (release_lifecycle.go, "a cleanup send that fails
// marks its domain"): a detach whose cleanup send WebKit refuses keeps
// that cleanup pending, so the session's next Network change does not
// leave the detached attachment's header on the page.
func TestReleaseOwnerFailedCleanupStaysPending(t *testing.T) {
	r, pg, epoch, owner, b := attachmentHeader(t)
	ctx := ctxFor(t, 10*time.Second)
	b.refuseOn("page-1")
	mo, err := r.m.BeginLeased(ctx, pg, agent.Session, epoch, "cdp detach", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	err = r.m.ReleaseOwner(mo.Op.Context(), mo, owner)
	r.m.End(mo.ID)
	if err == nil || !strings.Contains(err.Error(), "cleanup refused") {
		t.Fatalf("fixture: ReleaseOwner = %v, want the refused Network.disable", err)
	}
	if hasOwner(pg, owner) {
		t.Fatalf("owner %s still in the model after its release: %v", owner, pg.PS.State.AllOwners())
	}
	b.refuseOn("")
	next, err := r.m.Begin(ctx, r.req, agent, "next headers", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(next.ID)
	if _, err := r.m.Raw(ctx, next.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-New":"new"}}`)); err != nil {
		t.Fatal(err)
	}
	if v := b.header("page-1", "X-Released"); v != "" {
		t.Fatalf("page-1 still sends X-Released=%q after the detached attachment's failed cleanup and the next Network change; no pending reset was recorded", v)
	}
	if b.header("page-1", "X-New") != "new" {
		t.Fatal("the next Network change did not set X-New")
	}
}

// Design 5.2 with release_lifecycle.go ("the removal never waits for the
// change lock"): a detach whose release cannot take the change lock before
// its deadline still removes the attachment's owner, in the same critical
// section as a new release generation, so no later setup applies its
// state again; the page-side cleanup stays pending and the background
// reset removes the header once the lock is free.
func TestReleaseOwnerWithoutTheChangeLockForgetsTheOwner(t *testing.T) {
	r, pg, epoch, owner, b := attachmentHeader(t)
	ctx := ctxFor(t, 10*time.Second)
	_, end, err := pg.PS.State.beginChange(ctx, nil) // another change holds the lock
	if err != nil {
		t.Fatal(err)
	}
	mo, err := r.m.BeginLeased(ctx, pg, agent.Session, epoch, "cdp detach", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	gen := pg.PS.State.ReleaseGen()
	const deadline = 80 * time.Millisecond
	short, cancel := context.WithTimeout(mo.Op.Context(), deadline)
	start := time.Now()
	err = r.m.ReleaseOwner(short, mo, owner)
	took := time.Since(start)
	cancel()
	r.m.End(mo.ID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReleaseOwner while the change lock is held = %v, want its deadline", err)
	}
	if took > deadline+time.Second {
		t.Fatalf("ReleaseOwner took %v with a deadline of %v", took, deadline)
	}
	if hasOwner(pg, owner) {
		t.Fatalf("owner %s still in the model after a release that missed the change lock: %v", owner, pg.PS.State.AllOwners())
	}
	if got := pg.PS.State.ReleaseGen(); got != gen+1 {
		t.Fatalf("release generation %d after the owner's removal, want %d: a setup that snapshotted the owner could be installed", got, gen+1)
	}
	if !pg.PS.State.isUnknown("Network") {
		t.Fatal("Network is not marked unknown although the removed owner's cleanup was never sent on the live connection")
	}
	if err := end(nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the background reset removes the released header", func() bool {
		return b.header("page-1", "X-Released") == ""
	})
}

// ForgetOwner is the removal when the detach cannot open its operation
// (BeginLeased failed before its deadline): the owner leaves the model,
// and the cleanup stays pending on the live connection.
func TestForgetOwnerWithoutAnOperation(t *testing.T) {
	r, pg, _, owner, b := attachmentHeader(t)
	gen := pg.PS.State.ReleaseGen()
	r.m.ForgetOwner(pg, owner)
	if hasOwner(pg, owner) {
		t.Fatalf("owner %s still in the model after ForgetOwner: %v", owner, pg.PS.State.AllOwners())
	}
	if got := pg.PS.State.ReleaseGen(); got != gen+1 {
		t.Fatalf("release generation %d after ForgetOwner, want %d", got, gen+1)
	}
	eventually(t, 5*time.Second, "the background reset removes the released header", func() bool {
		return b.header("page-1", "X-Released") == ""
	})
}

// Design 2.7 (core-events round 4, recoverSetup at every Begin): an
// operation under a held lease also applies the page state again after a
// failed provisional setup, before its sends to the committed target,
// which the session refuses until a setup succeeds.
func TestBeginLeasedRecoversAFailedSetup(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	pg, epoch, err := r.m.Touch(ctx, r.req, agent)
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
	<-r.f.Provisional("page-2")
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
	if err := pg.conn.ActiveSetup(ctx); !errors.As(err, new(*webkit.SetupError)) {
		t.Fatalf("fixture: ActiveSetup after the commit = %v, want the SetupError", err)
	}
	_, err = r.m.BeginLeased(ctx, pg, agent.Session, epoch, "cdp while refused", ClassMutate)
	if !errors.As(err, new(*webkit.SetupError)) || !strings.Contains(err.Error(), "applying the page state again") {
		t.Fatalf("BeginLeased while the refusal lasts = %v, want the failed recovery with its SetupError", err)
	}
	refuse.Lock()
	refusing = false
	refuse.Unlock()
	mo, err := r.m.BeginLeased(ctx, pg, agent.Session, epoch, "cdp after refusal", ClassMutate)
	if err != nil {
		t.Fatalf("BeginLeased after the refusal ended: %v", err)
	}
	defer r.m.End(mo.ID)
	if _, err := mo.Op.Caller().Send(mo.Op.Context(), "Runtime.evaluate", map[string]any{"expression": "1"}); err != nil {
		t.Fatalf("gated send after the recovery: %v", err)
	}
}

// Design 2.4 and 2.7 (core-events round 4, part 2: forwarded cleanup
// sends): the detach of an attachment whose header a provisional target
// holds, while that target refuses the forwarded Network.disable, keeps
// the cleanup pending with that target; after the commit, the next Network
// change resets the agent and the detached attachment's header is gone.
func TestReleaseOwnerFailedProvisionalCleanupPendsUntilCommit(t *testing.T) {
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
	owner := SessionOwner("cdp-A", agent.Session)
	mo, err := r.m.BeginLeased(ctx, pg, agent.Session, epoch, "cdp Network.setExtraHTTPHeaders", ClassExclusive)
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
	mo, err = r.m.BeginLeased(ctx, pg, agent.Session, epoch, "cdp detach", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	err = r.m.ReleaseOwner(mo.Op.Context(), mo, owner)
	r.m.End(mo.ID)
	if err == nil || !strings.Contains(err.Error(), "cleanup refused") {
		t.Fatalf("fixture: ReleaseOwner = %v, want the refused forwarded cleanup", err)
	}
	if b.header("page-1", "X-Released") != "" {
		t.Fatal("fixture: the release did not clean the active target")
	}
	b.refuseOn("")
	r.f.Commit("page-2")
	eventually(t, 3*time.Second, "commit", func() bool { return pg.PS.State.isActive("page-2") })
	if !pg.PS.State.isUnknown("Network") {
		t.Fatalf("after the commit, unknown domains = %v, want the refused Network cleanup pending", pg.PS.State.unknownDomains())
	}
	next, err := r.m.Begin(ctx, r.req, agent, "next headers", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(next.ID)
	if _, err := r.m.Raw(ctx, next.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-New":"new"}}`)); err != nil {
		t.Fatal(err)
	}
	if v := b.header("page-2", "X-Released"); v != "" {
		t.Fatalf("the committed target keeps the detached attachment's header X-Released: %s", v)
	}
}

// Design 2.4 (releaseOwners: "Network for a failed Network reset"): when
// the attachment's header name loses its last owner while the session's
// legacy owner keeps Network, the release runs the Network
// reinitialization barrier; a refused reset keeps the Network cleanup
// pending, and the background reset removes the header once WebKit
// accepts it.
func TestReleaseOwnerFailedNetworkResetStaysPending(t *testing.T) {
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
	mo, err = r.m.BeginLeased(ctx, pg, agent.Session, epoch, "cdp detach", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	err = r.m.ReleaseOwner(mo.Op.Context(), mo, owner)
	r.m.End(mo.ID)
	if err == nil || !strings.Contains(err.Error(), "cleanup refused") {
		t.Fatalf("fixture: ReleaseOwner = %v, want the refused Network reset", err)
	}
	if !pg.PS.State.isUnknown("Network") {
		t.Fatalf("unknown domains after the refused Network reset = %v, want Network pending", pg.PS.State.unknownDomains())
	}
	b.refuseOn("")
	// A change of the session resets the pending agent first, if the
	// background reset has not yet. The legacy owner's map replaces its
	// earlier one, so it names X-Legacy again.
	next, err := r.m.Begin(ctx, r.req, agent, "next headers", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.m.End(next.ID)
	if _, err := r.m.Raw(ctx, next.ID, "Network.setExtraHTTPHeaders", json.RawMessage(`{"headers":{"X-Legacy":"1","X-New":"new"}}`)); err != nil {
		t.Fatal(err)
	}
	if v := b.header("page-1", "X-Released"); v != "" {
		t.Fatalf("page-1 keeps the detached attachment's header X-Released=%q after a refused Network reset", v)
	}
	if b.header("page-1", "X-Legacy") != "1" {
		t.Fatal("the reset dropped the legacy owner's header X-Legacy")
	}
}

// Review W5a rebase F1 (TestReviewForgetOwnerCannotCrossTheInstallGuard,
// release_lifecycle.go install rule): an owner removal and a connection
// install serialize under pg.mu. A ForgetOwner that starts after the
// recovery's install check either waits for the swap and then sees the
// live connection, so the cleanup is pending and the background reset
// removes the header, or comes before the check, so the recovery sets up
// again. It never leaves the detached attachment's header on the
// installed connection without an owner or a pending reset.
func TestForgetOwnerCannotCrossTheInstallGuard(t *testing.T) {
	r, pg, _, owner, b := attachmentHeader(t)
	checked := make(chan struct{})
	forgotten := make(chan struct{})
	installed := make(chan struct{})
	var once sync.Once
	r.m.installChecked = func(*Page) {
		once.Do(func() {
			close(checked)
			// Give a ForgetOwner that does not wait for pg.mu the time
			// to finish inside the check-to-swap interval.
			select {
			case <-forgotten:
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
	watchLifecycle(r.m, func(ev lifecycleEvent) {
		if ev.kind == "install" {
			close(installed)
		}
	})
	go func() {
		<-checked
		r.m.ForgetOwner(pg, owner)
		close(forgotten)
	}()
	r.f.StallConnection(1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := pg.conn.probeNow(ctx) // its failure starts the background recovery
	cancel()
	if err == nil {
		t.Fatal("fixture: the liveness probe of connection 1 did not fail")
	}
	for what, ch := range map[string]chan struct{}{"the recovery installs": installed, "ForgetOwner returns": forgotten} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("fixture: %s did not happen", what)
		}
	}
	if hasOwner(pg, owner) {
		t.Fatal("fixture: the removed owner remains in the model")
	}
	eventually(t, 5*time.Second, "the detached attachment's header leaves the installed connection", func() bool {
		return b.header("page-1", "X-Released") == ""
	})
}
