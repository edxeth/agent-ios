package core

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Tests of LeaseClaim and Manager.ReleaseClaimed: a caller ends a lease
// that its own acquisition granted only while nothing else of the session
// used it since the grant (review of 2026-11-13, F37: an epoch identifies
// a lease, not exclusive use of it).

// claimedRead runs a read with claim for agent's session and returns the
// key of its page and the epoch of the lease that it ran under.
func claimedRead(t *testing.T, r *rig, claim *LeaseClaim) (string, uint64) {
	t.Helper()
	ao := agent
	ao.Claim = claim
	var key string
	var epoch uint64
	err := r.m.Do(ctxFor(t, 5*time.Second), r.req, ao, "claimed read", ClassRead, false, func(_ context.Context, mo *ManagedOp) error {
		key, epoch = mo.Page.Key, mo.Epoch
		return nil
	})
	if err != nil {
		t.Fatalf("the claimed read: %v", err)
	}
	return key, epoch
}

// leaseEpoch is the epoch of agent's session's lease on key, or 0.
func leaseEpoch(r *rig, key string) uint64 {
	s, e, ok := r.m.LeaseOf(key)
	if !ok || s != agent.Session {
		return 0
	}
	return e
}

func TestReleaseClaimedEndsALeaseThatOnlyTheClaimUsed(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	var claim LeaseClaim
	key, epoch := claimedRead(t, r, &claim)
	if got := claim.Keys(); !slices.Equal(got, []string{key}) {
		t.Fatalf("claim keys %v; want [%s]", got, key)
	}
	// A second read with the same claim is the same caller.
	if k2, e2 := claimedRead(t, r, &claim); k2 != key || e2 != epoch {
		t.Fatalf("the second claimed read ran on %s epoch %d; want %s epoch %d", k2, e2, key, epoch)
	}
	if err := r.m.ReleaseClaimed(ctxFor(t, 5*time.Second), &claim, key); err != nil {
		t.Fatal(err)
	}
	if e := leaseEpoch(r, key); e != 0 {
		t.Fatalf("after ReleaseClaimed the session holds epoch %d; want no lease", e)
	}
}

// A lease that the session held before the claimed read, a lease granted
// again after the claimed one ended, and a key without a grant or without
// a page stay as they are, and nothing fails.
func TestReleaseClaimedEndsNoLeaseOfAnotherGrant(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 5*time.Second)
	pg, held, err := r.m.Touch(ctx, r.req, agent)
	if err != nil {
		t.Fatal(err)
	}
	var claim LeaseClaim
	if key, epoch := claimedRead(t, r, &claim); key != pg.Key || epoch != held || len(claim.Keys()) != 0 {
		t.Fatalf("a claimed read of a held lease: %s epoch %d, claim %v; want epoch %d and no claimed key", key, epoch, claim.Keys(), held)
	}
	if err := r.m.ReleaseClaimed(ctx, &claim, pg.Key); err != nil || leaseEpoch(r, pg.Key) != held {
		t.Fatalf("ReleaseClaimed of a lease held before: %v, epoch %d; want epoch %d kept", err, leaseEpoch(r, pg.Key), held)
	}
	if err := r.m.Release(ctx, agent.Session, pg.Key); err != nil {
		t.Fatal(err)
	}
	key, granted := claimedRead(t, r, &claim)
	if err := r.m.Release(ctx, agent.Session, key); err != nil {
		t.Fatal(err)
	}
	_, again, err := r.m.Touch(ctx, r.req, agent)
	if err != nil || again == granted {
		t.Fatalf("Touch after the release: epoch %d, %v; want a new lease", again, err)
	}
	if err := r.m.ReleaseClaimed(ctx, &claim, key); err != nil || leaseEpoch(r, key) != again {
		t.Fatalf("ReleaseClaimed of a lease granted again: %v, epoch %d; want epoch %d kept", err, leaseEpoch(r, key), again)
	}
	if err := r.m.ReleaseClaimed(ctx, &claim, "no-such-key"); err != nil {
		t.Fatalf("ReleaseClaimed of a key without a page: %v", err)
	}
	var none LeaseClaim
	if err := r.m.ReleaseClaimed(ctx, &none, key); err != nil || leaseEpoch(r, key) != again {
		t.Fatalf("ReleaseClaimed without a grant: %v, epoch %d; want epoch %d kept", err, leaseEpoch(r, key), again)
	}
}

// Order 1: another use of the lease by the same session after the claimed
// grant, finished or still running, keeps the lease, and a running
// operation is not ended.
func TestReleaseClaimedKeepsALeaseThatAnotherUseOfTheSessionReached(t *testing.T) {
	for _, tc := range []struct {
		name string
		// use uses the lease (epoch) on pg and returns a function that
		// ends the use, or nil when the use ended already.
		use func(t *testing.T, r *rig, pg *Page, epoch uint64) func()
	}{
		{"a finished operation", func(t *testing.T, r *rig, _ *Page, _ uint64) func() {
			if err := r.m.Do(ctxFor(t, 5*time.Second), r.req, agent, "other", ClassRead, false, func(context.Context, *ManagedOp) error { return nil }); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{"a running operation", func(t *testing.T, r *rig, _ *Page, _ uint64) func() {
			mo, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, agent, "other", ClassRead)
			if err != nil {
				t.Fatal(err)
			}
			return func() {
				if err := mo.Op.Context().Err(); err != nil {
					t.Errorf("the other operation was ended: %v", err)
				}
				r.m.End(mo.ID)
			}
		}},
		{"a touch", func(t *testing.T, r *rig, _ *Page, _ uint64) func() {
			if _, _, err := r.m.Touch(ctxFor(t, 5*time.Second), r.req, agent); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{"an operation begun under the lease", func(t *testing.T, r *rig, pg *Page, epoch uint64) func() {
			mo, err := r.m.BeginLeased(ctxFor(t, 5*time.Second), pg, agent.Session, epoch, "other", ClassRead)
			if err != nil {
				t.Fatal(err)
			}
			r.m.End(mo.ID)
			return nil
		}},
		{"an open event stream", func(t *testing.T, r *rig, pg *Page, _ uint64) func() {
			return r.m.HoldLease(pg, agent)
		}},
		{"a closed event stream", func(t *testing.T, r *rig, pg *Page, _ uint64) func() {
			r.m.HoldLease(pg, agent)()
			return nil
		}},
		{"a stream bound to the lease", func(t *testing.T, r *rig, pg *Page, epoch uint64) func() {
			if _, err := r.m.LeaseEnded(pg, agent.Session, epoch); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
			r.reserve(t, "agent")
			var claim LeaseClaim
			key, epoch := claimedRead(t, r, &claim)
			end := tc.use(t, r, r.m.lookup(key), epoch)
			if err := r.m.ReleaseClaimed(ctxFor(t, 5*time.Second), &claim, key); err != nil {
				t.Fatal(err)
			}
			if end != nil {
				end()
			}
			if e := leaseEpoch(r, key); e != epoch {
				t.Fatalf("after ReleaseClaimed the session holds epoch %d; want the used lease, epoch %d", e, epoch)
			}
		})
	}
}

// An operation that runs or waits on the page keeps the lease, also one
// of the claim itself, and is not ended; after it the lease ends.
func TestReleaseClaimedKeepsALeaseWhileItsOwnOperationRuns(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	var claim LeaseClaim
	ao := agent
	ao.Claim = &claim
	mo, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, ao, "claimed", ClassRead)
	if err != nil {
		t.Fatal(err)
	}
	key := mo.Page.Key
	if err := r.m.ReleaseClaimed(ctxFor(t, 5*time.Second), &claim, key); err != nil {
		t.Fatal(err)
	}
	if e := leaseEpoch(r, key); e != mo.Epoch || mo.Op.Context().Err() != nil {
		t.Fatalf("ReleaseClaimed while the operation runs: epoch %d, operation %v; want epoch %d kept and the operation running", e, mo.Op.Context().Err(), mo.Epoch)
	}
	r.m.End(mo.ID)
	if err := r.m.ReleaseClaimed(ctxFor(t, 5*time.Second), &claim, key); err != nil || leaseEpoch(r, key) != 0 {
		t.Fatalf("ReleaseClaimed after the operation: %v, epoch %d; want no lease", err, leaseEpoch(r, key))
	}
}

// An event stream of the session that was opened before the claimed grant
// holds the lease that the session gets: the lease stays while the stream
// is open and after it closed.
func TestReleaseClaimedKeepsALeaseThatAnEarlierStreamHolds(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	var first LeaseClaim
	key, _ := claimedRead(t, r, &first)
	if err := r.m.ReleaseClaimed(ctxFor(t, 5*time.Second), &first, key); err != nil || leaseEpoch(r, key) != 0 {
		t.Fatalf("setup: %v, epoch %d; want the page without a lease", err, leaseEpoch(r, key))
	}
	unhold := r.m.HoldLease(r.m.lookup(key), agent)
	var claim LeaseClaim
	_, epoch := claimedRead(t, r, &claim)
	if err := r.m.ReleaseClaimed(ctxFor(t, 5*time.Second), &claim, key); err != nil || leaseEpoch(r, key) != epoch {
		t.Fatalf("ReleaseClaimed with an open stream: %v, epoch %d; want epoch %d kept", err, leaseEpoch(r, key), epoch)
	}
	unhold()
	if err := r.m.ReleaseClaimed(ctxFor(t, 5*time.Second), &claim, key); err != nil || leaseEpoch(r, key) != epoch {
		t.Fatalf("ReleaseClaimed after the stream closed: %v, epoch %d; want epoch %d kept", err, leaseEpoch(r, key), epoch)
	}
}

// Order 2: a command of the session that starts while ReleaseClaimed ends
// the claimed lease waits for the release, runs under a new lease, and
// keeps it.
func TestReleaseClaimedLetsACommandThatComesDuringTheReleaseKeepItsLease(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	var claim LeaseClaim
	key, epoch := claimedRead(t, r, &claim)
	// The next Runtime.evaluate is the page-side runtime release of the
	// claimed lease: it signals releasing and waits until the other
	// command has resolved the page.
	ctx := ctxFor(t, 10*time.Second)
	var armed atomic.Bool
	armed.Store(true)
	releasing, resolved := make(chan struct{}), make(chan struct{})
	var resolvedOnce sync.Once
	r.f.Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		if armed.CompareAndSwap(true, false) {
			close(releasing)
			select {
			case <-resolved:
			case <-ctx.Done():
			}
		}
		return map[string]any{"result": map[string]any{"type": "undefined"}}, nil
	})
	released := make(chan error, 1)
	go func() { released <- r.m.ReleaseClaimed(ctx, &claim, key) }()
	select {
	case <-releasing:
	case <-ctx.Done():
		t.Fatal("the release of the claimed lease sent nothing to the page")
	}
	observed := WithPageObserver(ctx, func(string, string) { resolvedOnce.Do(func() { close(resolved) }) })
	var ran uint64
	err := r.m.Do(observed, r.req, agent, "other", ClassRead, false, func(ctx context.Context, mo *ManagedOp) error {
		ran = mo.Epoch
		return ctx.Err()
	})
	if err != nil {
		t.Fatalf("the command that came during the release: %v", err)
	}
	select {
	case err := <-released:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("ReleaseClaimed did not return")
	}
	if ran == 0 || ran == epoch {
		t.Fatalf("the command ran under epoch %d; want a new lease after the claimed epoch %d", ran, epoch)
	}
	if e := leaseEpoch(r, key); e != ran {
		t.Fatalf("after both the session holds epoch %d; want the new lease %d", e, ran)
	}
}
