package core

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// idleStep is one step of a generated idle-close lifecycle
// (TestIdleCloseLifecycleProperty).
type idleStep uint8

const (
	idleTouch            idleStep = iota // Touch of s1
	idleTouchOther                       // Touch of s2, refused while s1 holds the lease
	idleSweepEarly                       // a sweep before any idle limit
	idleSweepIdle                        // a sweep past LeaseTTL and IdleClose
	idleBreak                            // break the lease of the listed record
	idleRelease                          // s1 releases the listed record
	idleSweepInAttach                    // a sweep while a Touch waits for the record's attach lock
	idleSweepInAcquire                   // a sweep after a Touch attached the record, before its lease
	idleSweepInOperation                 // a sweep while an operation of s1 runs
	idleSteps
)

var idleStepNames = [...]string{
	"touch", "touch-other", "sweep-early", "sweep-idle", "break", "release",
	"sweep-in-attach", "sweep-in-acquire", "sweep-in-operation",
}

func (s idleStep) String() string { return idleStepNames[s] }

// Review devfix-R2 F6: a generated property of the idle close (Sweep,
// closeIdle) against the real Manager, its attach and lease paths, and the
// fake WebKit and listing. The steps are Touch, sweeps with idle times
// before and past the limits, lease break and release, and three
// interleavings in which the sweep runs while a Touch is inside attach,
// inside acquire, or while an operation runs. The interleavings are
// ordered by events (the attach lock, the release flag, a goroutine
// waiting in a named function), not by time windows. After each step:
//   - a successful Touch returns the record that the manager holds for its
//     key (Sweep contract: a command gets the current record);
//   - a record that the manager no longer lists has no lease and its
//     connection is closed (no removed record gets a connection or lease);
//   - the fake WebKit has exactly one open connection per listed record
//     with a live connection (no page connection outside the records), so
//     every connection is reachable by Sweep, Status and release;
//   - Status lists every record whose connection is live;
//
// and after Close, no connection is open (shutdown reaches every one).
func TestIdleCloseLifecycleProperty(t *testing.T) {
	const maxSteps = 14
	run := 0
	var cov idleCoverage
	prop := func(raw []uint8) bool {
		if len(raw) > maxSteps {
			raw = raw[:maxSteps]
		}
		steps := make([]idleStep, len(raw))
		for i, b := range raw {
			steps[i] = idleStep(b) % idleSteps
		}
		run++
		ok := true
		t.Run(fmt.Sprintf("plan%d", run), func(t *testing.T) {
			if why := runIdleLifecycle(t, steps, &cov); why != "" {
				t.Errorf("steps %v: %s", steps, why)
				ok = false
			}
		})
		return ok
	}
	cfg := &quick.Config{MaxCount: 40, Rand: rand.New(rand.NewSource(20261109))}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
	// The generated plans reach the cases of the review: each interleaving
	// with a listed record, and idle closes of a record.
	t.Logf("coverage %+v", cov)
	if cov.inAttach == 0 || cov.inAcquire == 0 || cov.inOperation == 0 || cov.closed == 0 {
		t.Fatalf("the generated plans miss a case: %+v", cov)
	}
}

// idleCoverage counts the cases that the generated plans reached.
type idleCoverage struct {
	inAttach, inAcquire, inOperation int // interleavings run on a listed record
	closed                           int // records that a sweep removed
}

var otherSession = AcquireOptions{Session: "s2", Reservation: "agent"}

// runIdleLifecycle runs steps on a new manager and returns the first broken
// invariant, or "".
func runIdleLifecycle(t *testing.T, steps []idleStep, cov *idleCoverage) string {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{SweepEvery: time.Hour})
	r.reserve(t, "agent")
	seen := map[*Page]bool{}
	var current *Page // the record that the manager lists, if any
	touched := func(pg *Page, err error, what string) string {
		if err != nil {
			return ""
		}
		seen[pg] = true
		if held := r.m.LookupPage(pg.Key); held != pg {
			return fmt.Sprintf("%s returned record %p (connection %v); the manager holds %p for its key", what, pg, pg.ConnState(), held)
		}
		return ""
	}
	ctx := func() context.Context { return ctxFor(t, 5*time.Second) }
	touch := func(ao AcquireOptions) <-chan string {
		out := make(chan string, 1)
		go func() {
			pg, _, err := r.m.Touch(ctx(), r.req, ao)
			out <- touched(pg, err, "Touch of "+ao.Session)
		}()
		return out
	}
	idle := func() time.Time { return time.Now().Add(2 * max(DefaultIdleClose, DefaultLeaseTTL)) }
	for i, step := range steps {
		current = nil
		for pg := range seen {
			if r.m.LookupPage(pg.Key) == pg {
				current = pg
			}
		}
		var why string
		switch step {
		case idleTouch:
			why = <-touch(agent)
		case idleTouchOther:
			why = <-touch(otherSession)
		case idleSweepEarly:
			r.m.Sweep(time.Now())
		case idleSweepIdle:
			r.m.Sweep(idle())
		case idleBreak:
			if current != nil {
				if err := r.m.Break(ctx(), current.Key); err != nil {
					return fmt.Sprintf("step %d: Break: %v", i, err)
				}
			}
		case idleRelease:
			if current != nil {
				// A release of a page that s1 does not hold is refused;
				// the lifecycle goes on either way.
				_ = r.m.Release(ctx(), agent.Session, current.Key)
			}
		case idleSweepInAttach:
			if current == nil {
				why = <-touch(agent)
				break
			}
			cov.inAttach++
			if err := current.attachMu.Lock(ctx()); err != nil {
				return fmt.Sprintf("step %d: attach lock: %v", i, err)
			}
			res := touch(agent)
			waitBlockedIn(t, "core.(*Manager).attach")
			r.m.Sweep(idle())
			current.attachMu.Unlock()
			why = <-res
		case idleSweepInAcquire:
			if current == nil {
				why = <-touch(agent)
				break
			}
			cov.inAcquire++
			current.mu.Lock()
			current.releasing = true
			current.mu.Unlock()
			res := touch(agent)
			waitBlockedIn(t, "core.(*Manager).acquire")
			current.mu.Lock()
			current.releasing = false
			current.mu.Unlock()
			r.m.Sweep(idle())
			current.mu.Lock()
			close(current.freed)
			current.freed = make(chan struct{})
			current.mu.Unlock()
			why = <-res
		case idleSweepInOperation:
			mo, err := r.m.Begin(ctx(), r.req, agent, "eval", ClassRead)
			if errors.Is(err, ErrLeased) {
				break // s2 holds the lease
			}
			if err != nil {
				return fmt.Sprintf("step %d: Begin: %v", i, err)
			}
			cov.inOperation++
			why = touched(mo.Page, nil, "Begin")
			r.m.Sweep(idle())
			if r.m.LookupPage(mo.Page.Key) != mo.Page || mo.Page.ConnState() != ConnOK {
				why = "the sweep closed a page while an operation ran on it"
			}
			r.m.End(mo.ID)
		}
		if why == "" {
			why = idleInvariants(t, r, seen)
		}
		if current != nil && r.m.LookupPage(current.Key) != current {
			cov.closed++
		}
		if why != "" {
			return fmt.Sprintf("after step %d (%v): %s", i, step, why)
		}
	}
	if err := r.m.Close(ctx()); err != nil {
		return fmt.Sprintf("Close: %v", err)
	}
	if !settles(func() bool { return r.f.OpenConnections() == 0 }) {
		return fmt.Sprintf("after Close, %d connections are open", r.f.OpenConnections())
	}
	return ""
}

// idleInvariants checks the records that the steps saw against the
// manager and the fake WebKit.
func idleInvariants(t *testing.T, r *rig, seen map[*Page]bool) string {
	t.Helper()
	live := 0
	for pg := range seen {
		pg.mu.Lock()
		leased := pg.lease != nil
		pg.mu.Unlock()
		st := pg.ConnState()
		if r.m.LookupPage(pg.Key) != pg {
			if leased || st != ConnClosed {
				return fmt.Sprintf("record %p, which the manager no longer lists, has lease %v and connection %v", pg, leased, st)
			}
			continue
		}
		if st == ConnOK || st == ConnStalled {
			live++
		}
	}
	listed := 0
	for _, s := range r.m.Status() {
		if s.State == ConnOK.String() || s.State == ConnStalled.String() {
			listed++
		}
	}
	if listed != live {
		return fmt.Sprintf("Status lists %d live records, the steps hold %d", listed, live)
	}
	if !settles(func() bool { return r.f.OpenConnections() == live }) {
		return fmt.Sprintf("%d connections are open, %d listed records are live: a connection outside the records", r.f.OpenConnections(), live)
	}
	return ""
}

// settles reports whether cond holds within a bound: the fake WebKit sees
// the close of a connection after the client closed it.
func settles(cond func() bool) bool {
	end := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(end) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}
