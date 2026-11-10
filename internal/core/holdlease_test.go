package core

import (
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Design 2.6 heartbeat: while a client of the leasing session keeps an
// event stream open (a CLI command that follows events, a CDP
// attachment), the idle TTL does not expire the lease; after the stream
// closes, the TTL counts from the close.
func TestHeldLeaseDoesNotExpire(t *testing.T) {
	const ttl = 300 * time.Millisecond
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{LeaseTTL: ttl, SweepEvery: 50 * time.Millisecond})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 5*time.Second)
	pg, _, err := r.m.Touch(ctx, r.req, agent)
	if err != nil {
		t.Fatalf("Touch: %v", err)
	}
	other := r.m.HoldLease(pg, AcquireOptions{Session: "other"}) // another session's stream holds nothing
	defer other()
	release := r.m.HoldLease(pg, agent)
	time.Sleep(3 * ttl)
	if st := r.m.Status(); len(st) != 1 || st[0].Lease != "active" || st[0].Session != agent.Session {
		t.Fatalf("status %+v after %v with a held lease; want the lease active", st, 3*ttl)
	}
	release()
	closed := time.Now()
	eventually(t, 3*time.Second, "lease expiry after the hold ends", func() bool { st := r.m.Status(); return len(st) == 1 && st[0].Lease == "none" })
	if d := time.Since(closed); d < ttl {
		t.Fatalf("the lease expired %v after the hold ended, before the TTL %v", d, ttl)
	}
	release() // a second call is harmless
}
