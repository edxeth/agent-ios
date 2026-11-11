package core

import (
	"errors"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// waitBlockedIn waits until a goroutine waits in a select with fn on its
// stack (fn is a function name of the goroutine dump, for example
// "core.(*Manager).acquire"): the event that orders a step of the test
// after a step of the code under test.
func waitBlockedIn(t *testing.T, fn string) {
	t.Helper()
	eventually(t, 5*time.Second, "a goroutine that waits in "+fn, func() bool {
		buf := make([]byte, 1<<22)
		buf = buf[:goruntime.Stack(buf, true)]
		for _, g := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(g, "[select") && strings.Contains(g, fn+"(") {
				return true
			}
		}
		return false
	})
}

// Review devfix-R1 F4: TestTargetReadsRaceWithAttach failed once with
// "state after the sweep = attached, want closed". A Touch had found the
// page record (Manager.page) before the sweeper's idle close removed it
// and closed its connection; the Touch then attached the removed record
// again and took a lease on it: a connection that the manager no longer
// lists, which no sweep, Status or release reaches, and which keeps the
// page's one inspector connection. Contract (Sweep): a page record that a
// command gets is the one the manager holds for its key; the idle close
// never closes a page while an attach of it runs, and a command that finds
// a record after the close looks the page up again.
func TestIdleCloseAndAConcurrentTouch(t *testing.T) {
	// attachedIdle attaches the page for s1 and ends s1's lease: the
	// page is idle without a lease, so a sweep IdleClose later closes it.
	attachedIdle := func(t *testing.T) (*rig, *Page) {
		t.Helper()
		r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{SweepEvery: time.Hour})
		r.reserve(t, "agent")
		pg, _, err := r.m.Touch(ctxFor(t, 5*time.Second), r.req, agent)
		if err != nil {
			t.Fatalf("Touch: %v", err)
		}
		if err := r.m.Break(ctxFor(t, 5*time.Second), pg.Key); err != nil {
			t.Fatalf("Break: %v", err)
		}
		return r, pg
	}
	// touch runs a Touch of s1 and returns its page and error.
	type touched struct {
		pg  *Page
		err error
	}
	touch := func(t *testing.T, r *rig) <-chan touched {
		out := make(chan touched, 1)
		go func() {
			pg, _, err := r.m.Touch(ctxFor(t, 5*time.Second), r.req, agent)
			out <- touched{pg, err}
		}()
		return out
	}
	// listed checks that the page of a Touch is the record that the
	// manager holds for its key.
	listed := func(t *testing.T, r *rig, got touched) {
		t.Helper()
		if got.err != nil {
			t.Fatalf("Touch: %v", got.err)
		}
		if held := r.m.LookupPage(got.pg.Key); held != got.pg {
			t.Fatalf("Touch returned page record %p (connection %v); the manager holds %p for its key: an attached record that the manager no longer lists", got.pg, got.pg.ConnState(), held)
		}
		if st := got.pg.ConnState(); st != ConnOK {
			t.Fatalf("state of the Touch's page = %v, want attached", st)
		}
	}

	t.Run("sweep while a Touch waits to attach the record", func(t *testing.T) {
		r, pg := attachedIdle(t)
		// The Touch finds the record and waits for its attach lock.
		if err := pg.attachMu.Lock(ctxFor(t, 5*time.Second)); err != nil {
			t.Fatal(err)
		}
		res := touch(t, r)
		waitBlockedIn(t, "core.(*Manager).attach")
		// The lock stands for an attach of the page that runs (a reopen
		// that installs a new connection): the sweep leaves the page to
		// a later sweep instead of closing it under the attach.
		r.m.Sweep(time.Now().Add(2 * DefaultIdleClose))
		if held := r.m.LookupPage(pg.Key); held != pg {
			t.Fatalf("the sweep removed the record while an attach of it ran (the manager holds %p)", held)
		}
		pg.attachMu.Unlock()
		got := <-res
		listed(t, r, got)
		if got.pg != pg {
			t.Fatal("the Touch got a new record, although the sweep kept the page")
		}
	})
	t.Run("sweep after a Touch attached the record, before its lease", func(t *testing.T) {
		r, pg := attachedIdle(t)
		// The Touch attaches the record and then waits for a release in
		// acquire; the sweep runs when no release runs any more, before
		// the Touch looks at the lease again.
		pg.mu.Lock()
		pg.releasing = true
		pg.mu.Unlock()
		res := touch(t, r)
		waitBlockedIn(t, "core.(*Manager).acquire")
		pg.mu.Lock()
		pg.releasing = false
		pg.mu.Unlock()
		r.m.Sweep(time.Now().Add(2 * DefaultIdleClose))
		pg.mu.Lock()
		close(pg.freed)
		pg.freed = make(chan struct{})
		pg.mu.Unlock()
		got := <-res
		listed(t, r, got)
		if got.pg == pg {
			t.Fatal("the Touch got the record that the sweep closed")
		}
		if st := pg.ConnState(); st != ConnClosed {
			t.Fatalf("state of the swept record = %v, want closed", st)
		}
	})
	t.Run("attach of a record that the sweep closed", func(t *testing.T) {
		// A command that found the record before the close and attaches
		// after it gets errPageRemoved (touch looks the page up again).
		r, pg := attachedIdle(t)
		r.m.Sweep(time.Now().Add(2 * DefaultIdleClose))
		if st := pg.ConnState(); st != ConnClosed {
			t.Fatalf("state of the swept record = %v, want closed", st)
		}
		if err := r.m.ensureAttached(ctxFor(t, 5*time.Second), pg, lockedTarget(pg), agent); !errors.Is(err, errPageRemoved) {
			t.Fatalf("attach of the swept record = %v, want errPageRemoved", err)
		}
		if st := pg.ConnState(); st != ConnClosed {
			t.Fatalf("state of the swept record after the refused attach = %v, want closed", st)
		}
		got := <-touch(t, r)
		listed(t, r, got)
		if got.pg == pg {
			t.Fatal("the Touch got the record that the sweep closed")
		}
	})
	t.Run("an idle close that meets a lease taken since the sweep's first look", func(t *testing.T) {
		// closeIdle looks at the lease again under mu: a lease granted
		// after the sweep read the page keeps it open.
		r, pg := attachedIdle(t)
		if _, _, err := r.m.Touch(ctxFor(t, 5*time.Second), r.req, agent); err != nil {
			t.Fatalf("Touch: %v", err)
		}
		if r.m.closeIdle(pg, time.Now().Add(2*DefaultIdleClose)) {
			t.Fatal("closeIdle closed a page with a lease")
		}
		if st := pg.ConnState(); st != ConnOK || r.m.LookupPage(pg.Key) != pg {
			t.Fatalf("state = %v, listed %v: want the leased page attached and listed", st, r.m.LookupPage(pg.Key) == pg)
		}
	})
}
