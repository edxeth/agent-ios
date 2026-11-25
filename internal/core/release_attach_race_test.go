package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/transport/iwdp"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// raceRepeats is how often the release/attach interleaving tests repeat:
// the race detector sees a race only when the two goroutines interleave,
// so one run proves little.
const raceRepeats = 50

// A lease break of session s1 runs while session s2 attaches the same
// page record (W5a's CDP lifetime property found it: a release read the
// page record's target while an attach wrote it). Under -race the test
// fails on any unsynchronized access of the two paths. Each run also
// checks the contracts of both sides: the release's cleanup reached the
// page's connection, and the attach then holds the lease with a newer
// epoch (design 2.5).
func TestBreakConcurrentWithTouchOfAnotherSession(t *testing.T) {
	for i := range raceRepeats {
		t.Run(fmt.Sprintf("run%d", i), func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
			r.reserve(t, "agent")
			ctx := ctxFor(t, 10*time.Second)
			mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin s1: %v", err)
			}
			if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
				t.Fatalf("Network.enable: %v", err)
			}
			r.m.End(mo.ID)
			pg := mo.Page

			s2 := AcquireOptions{Session: "s2", Reservation: "agent", WaitLease: 5 * time.Second}
			start := make(chan struct{})
			breakErr := make(chan error, 1)
			type touched struct {
				pg    *Page
				epoch uint64
				err   error
			}
			touch := make(chan touched, 1)
			go func() {
				<-start
				breakErr <- r.m.Break(ctx, pg.Key)
			}()
			go func() {
				<-start
				p, e, err := r.m.Touch(ctx, r.req, s2)
				touch <- touched{p, e, err}
			}()
			close(start)
			if err := <-breakErr; err != nil {
				t.Fatalf("Break: %v", err)
			}
			got := <-touch
			if got.err != nil {
				t.Fatalf("Touch s2: %v", got.err)
			}
			if got.pg != pg || got.epoch <= mo.Epoch {
				t.Fatalf("Touch s2 = page %p epoch %d, want page %p with an epoch above %d", got.pg, got.epoch, pg, mo.Epoch)
			}
			if st := r.m.Status(); len(st) != 1 || st[0].Session != "s2" || st[0].Epoch != got.epoch || st[0].Lease != "active" {
				t.Fatalf("status = %+v, want s2's active lease with epoch %d", st, got.epoch)
			}
			// The attach's setup sends a Network.disable before s1's enable
			// (the Network reset of a new connection); the release's comes after.
			enable := r.f.ReceivedMethod("Network.enable")
			disable := methodsAfter(r.f, "Network.enable", "Network.disable")
			if len(enable) != 1 || len(disable) != 1 || disable[0].Conn != enable[0].Conn {
				t.Fatalf("Network.enable %+v, Network.disable after it %+v: want the release's one Network.disable on the connection of the enable", enable, disable)
			}
			if n := r.f.ConnectionCount(); n != 1 {
				t.Fatalf("connections = %d, want 1: an attached page is not dialed again", n)
			}
		})
	}
}

// A takeover reattach must not grant its lease while a release of the
// old lease runs: the release ends with the scheduler lease cleared and
// the lease channel closed, which would end the new holder's lease too.
// The held scheduler operation keeps the release running (Scheduler.Release
// waits for running operations) until the test ends it.
//
// "claimed before the takeover": the takeover waits for the release before
// it reattaches. "claimed during the connect": the release starts while
// the takeover's new connection is set up (the delayed Network.enable of
// the re-applied desired state), so the takeover leaves the grant to
// acquire, which waits for the release.
func TestTakeoverDuringReleaseKeepsTheNewLease(t *testing.T) {
	for _, during := range []bool{false, true} {
		name := "claimed before the takeover"
		if during {
			name = "claimed during the connect"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{})
			r.reserve(t, "agent")
			ctx := ctxFor(t, 10*time.Second)
			mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin s1: %v", err)
			}
			if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
				t.Fatalf("Network.enable: %v", err)
			}
			r.m.End(mo.ID)
			pg := mo.Page
			held, err := pg.PS.Sched.Begin(ctx, OpSpec{Name: "held", Class: ClassRead, Session: "s1", Epoch: mo.Epoch})
			if err != nil {
				t.Fatalf("held operation: %v", err)
			}
			stealPage(t, r.f)
			eventually(t, 3*time.Second, "lease lost", func() bool { return pg.ConnState() == ConnLost })

			releasing := func() bool {
				pg.mu.Lock()
				defer pg.mu.Unlock()
				return pg.releasing
			}
			breakErr := make(chan error, 1)
			startBreak := func() {
				go func() { breakErr <- r.m.Break(ctx, pg.Key) }()
				eventually(t, 3*time.Second, "release claimed", releasing)
			}
			const connectDelay = 500 * time.Millisecond
			if during {
				r.f.DelayReplies("Network.enable", connectDelay)
			} else {
				startBreak()
			}
			take := AcquireOptions{Session: "s2", Reservation: "agent", Takeover: true}
			type touched struct {
				epoch uint64
				err   error
			}
			touch := make(chan touched, 1)
			go func() {
				_, e, err := r.m.Touch(ctx, r.req, take)
				touch <- touched{e, err}
			}()
			if during {
				// The takeover's connection (the third: s1's, the stealer's,
				// the takeover's) received the re-applied Network.enable.
				eventually(t, 3*time.Second, "takeover connecting", func() bool {
					for _, rec := range r.f.ReceivedMethod("Network.enable") {
						if rec.Conn == 3 {
							return true
						}
					}
					return false
				})
				startBreak()
			}
			// Let the takeover finish its connect while the release runs.
			time.Sleep(connectDelay + 200*time.Millisecond)
			if !releasing() {
				t.Fatal("the release ended before the held operation ended")
			}
			if during && pg.ConnState() == ConnOK {
				t.Error("takeover installed its connection while the old release is still running")
			}
			held.End()
			if err := <-breakErr; err != nil {
				t.Fatalf("Break: %v", err)
			}
			got := <-touch
			if got.err != nil {
				t.Fatalf("takeover: %v", got.err)
			}
			if pg.PS.State.Needs("Network") {
				t.Errorf("released s1 Network ownership remains after s2 takeover: owners %v", pg.PS.State.AllOwners())
			}
			if got.epoch <= mo.Epoch {
				t.Fatalf("takeover epoch = %d, want above %d", got.epoch, mo.Epoch)
			}
			if holder, epoch := pg.PS.Sched.Lease(); holder != "s2" || epoch != got.epoch {
				t.Fatalf("scheduler lease = %q epoch %d, want s2 with epoch %d", holder, epoch, got.epoch)
			}
			ended, err := r.m.LeaseEnded(pg, "s2", got.epoch)
			if err != nil {
				t.Fatalf("LeaseEnded s2: %v", err)
			}
			mo2, err := r.m.Begin(ctx, r.req, AcquireOptions{Session: "s2", Reservation: "agent"}, "eval", ClassExclusive)
			if err != nil {
				t.Fatalf("s2's command after the takeover: %v", err)
			}
			if _, err := r.m.Raw(ctx, mo2.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"1"}`)); err != nil {
				t.Fatalf("s2's call after the takeover: %v", err)
			}
			r.m.End(mo2.ID)
			select {
			case <-ended:
				t.Fatal("s2's lease channel closed while s2 holds the lease")
			default:
			}
		})
	}
}

// A reopen of a page whose connection stalled during a release waits
// until the release ended: the release removes the session's desired
// state, and the new connection is set up from what is left, so nothing of
// the released session reaches it (design 2.5: nothing of a session
// outlives its lease). Without the wait the reopen dials while the release
// still runs and its setup re-applies the released session's Network.enable.
// The held scheduler operation keeps the release running until the test
// ends it.
func TestReopenWaitsForTheRelease(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	mo, err := r.m.Begin(ctx, r.req, agent, "network", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin s1: %v", err)
	}
	if _, err := r.m.Raw(ctx, mo.ID, "Network.enable", nil); err != nil {
		t.Fatalf("Network.enable: %v", err)
	}
	r.m.End(mo.ID)
	pg := mo.Page
	held, err := pg.PS.Sched.Begin(ctx, OpSpec{Name: "held", Class: ClassRead, Session: "s1", Epoch: mo.Epoch})
	if err != nil {
		t.Fatalf("held operation: %v", err)
	}
	breakErr := make(chan error, 1)
	go func() { breakErr <- r.m.Break(ctx, pg.Key) }()
	eventually(t, 3*time.Second, "release claimed", func() bool {
		pg.mu.Lock()
		defer pg.mu.Unlock()
		return pg.releasing
	})
	// The page stops answering while the release waits for the held
	// operation: the probe marks it stalled, and no recovery runs during a
	// release.
	r.f.StallConnection(1)
	if err := pg.conn.probeNow(ctx); err == nil {
		t.Fatal("the probe of the stalled connection succeeded")
	}
	if st := pg.ConnState(); st != ConnStalled {
		t.Fatalf("state = %v, want stalled", st)
	}
	// A reopen whose deadline ends during the wait fails with exit 124
	// (decision D9) and names the wait.
	short := AcquireOptions{Session: "s2", Reservation: "agent"}
	if _, _, err := r.m.Touch(ctxFor(t, 300*time.Millisecond), r.req, short); exitCode(err) != CodeTimeout || !strings.Contains(err.Error(), "waiting for the release of the lease") {
		t.Fatalf("Touch s2 with a deadline inside the release = %v (exit %d), want exit 124 waiting for the release", err, exitCode(err))
	}
	type touched struct {
		epoch uint64
		err   error
	}
	touch := make(chan touched, 1)
	go func() {
		_, e, err := r.m.Touch(ctx, r.req, AcquireOptions{Session: "s2", Reservation: "agent"})
		touch <- touched{e, err}
	}()
	time.Sleep(500 * time.Millisecond)
	if n := r.f.ConnectionCount(); n != 1 {
		t.Fatalf("connections = %d while the release runs, want 1: the reopen waits for the release", n)
	}
	held.End()
	// The cleanup is not sent on the stalled connection (design 2.4: no
	// reconnect just to clean up), so the release reports it; the lease
	// still ends.
	if err := <-breakErr; err == nil {
		t.Fatal("Break: the cleanup on the stalled connection succeeded")
	}
	got := <-touch
	if got.err != nil {
		t.Fatalf("Touch s2 after the release: %v", got.err)
	}
	if n := r.f.ConnectionCount(); n != 2 {
		t.Fatalf("connections = %d, want 2: the reopen dials after the release", n)
	}
	if holder, epoch := pg.PS.Sched.Lease(); holder != "s2" || epoch != got.epoch || epoch <= mo.Epoch {
		t.Fatalf("scheduler lease = %q epoch %d, want s2 with epoch %d above %d", holder, epoch, got.epoch, mo.Epoch)
	}
	for _, rec := range r.f.ReceivedMethod("Network.enable") {
		if rec.Conn != 1 {
			t.Fatalf("Network.enable reached connection %d: the released session's state was re-applied", rec.Conn)
		}
	}
}

// lockedTarget reads pg's target under pg.mu.
func lockedTarget(pg *Page) pageTarget {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	return pg.target
}

// attachLoop calls Touch of ao in a loop until the returned stop runs.
// Every Touch writes the page record's target (title, URL, busy hint)
// under pg.mu in ensureAttached, as a CDP attachToTarget does. Its results
// do not matter here: a refused Touch (another session's lease, a lost
// page, a release that outlasts its short deadline) has written the
// target before it is refused.
func attachLoop(r *rig, ao AcquireOptions) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			tctx, tcancel := context.WithTimeout(ctx, 50*time.Millisecond)
			_, _, _ = r.m.Touch(tctx, r.req, ao)
			tcancel()
		}
	}()
	return func() { cancel(); <-done }
}

// Every path that reads the page record's target (or the lease's
// reservation owner) outside pg.mu runs while attaches write them
// (attachLoop): the race detector fails the subtest of a path that reads
// them without the lock. Each subtest names the reads it reaches in
// Manager; each repeats raceRepeats times.
func TestTargetReadsRaceWithAttach(t *testing.T) {
	// waitDeadline ends a lease wait; it must leave time for resolving
	// the page (the fake listing), which a shorter deadline cuts off.
	const waitDeadline = 100 * time.Millisecond
	// logPause holds each log call of the manager: a log's arguments are
	// read before the call, and the pause lets the attaches write the
	// target after that read and before the logging path takes a lock
	// that would order the two for the race detector.
	const logPause = 5 * time.Millisecond
	// attachedRig attaches s1 (agent) and returns its page.
	attachedRig := func(t *testing.T, fo fakewebkit.Options, mo ManagerOptions) (*rig, *Page) {
		t.Helper()
		mo.Logf = func(string, ...any) { time.Sleep(logPause) }
		r := newRig(t, fo, false, mo)
		r.reserve(t, "agent")
		op, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, agent, "attach", ClassExclusive)
		if err != nil {
			t.Fatalf("Begin s1: %v", err)
		}
		r.m.End(op.ID)
		return r, op.Page
	}
	// leased makes s1 hold the page's lease again after a release.
	leased := func(t *testing.T, r *rig) {
		t.Helper()
		if _, _, err := r.m.Touch(ctxFor(t, 5*time.Second), r.req, agent); err != nil {
			t.Fatalf("Touch s1: %v", err)
		}
	}
	t.Run("URL record selected from the listing", func(t *testing.T) {
		// A page attached by its URL while no listing answered is a local
		// endpoint (key url|...). Selected later from the listing, the same
		// record takes the listed target as a whole (Manager.page): device,
		// OS version and physical flag change. Operations (Begin's idle
		// probe, Raw's command list) and the sweeper's renewal read those
		// meanwhile.
		for range raceRepeats {
			noBareListing := func(_ context.Context, kind, addr string) (transport.Transport, error) {
				if addr == "" {
					return nil, fmt.Errorf("transport %s: this test lists pages only at the fake listing's address", kind)
				}
				return iwdp.New(iwdp.Options{ListingAddr: addr}), nil
			}
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{
				Transport: noBareListing, IdleProbe: time.Nanosecond, SweepEvery: time.Hour,
				Logf: func(string, ...any) { time.Sleep(logPause) },
			})
			r.reserve(t, "agent")
			byURL := PageRequest{Page: r.f.URL}
			ctx := ctxFor(t, 10*time.Second)
			first, err := r.m.Begin(ctx, byURL, agent, "attach", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin by URL: %v", err)
			}
			r.m.End(first.ID)
			pg := first.Page
			if tg := lockedTarget(pg); !strings.HasPrefix(tg.key, "url|") || tg.physical {
				t.Fatalf("target by URL = key %q physical %v, want a local url| record", tg.key, tg.physical)
			}
			stop := make(chan struct{})
			readerDone := make(chan error, 1)
			go func() {
				for {
					select {
					case <-stop:
						readerDone <- nil
						return
					default:
					}
					mo, err := r.m.Begin(ctx, byURL, agent, "eval", ClassExclusive)
					if err != nil {
						readerDone <- fmt.Errorf("Begin: %w", err)
						return
					}
					_, err = r.m.Raw(ctx, mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"1"}`))
					r.m.End(mo.ID)
					if err != nil {
						readerDone <- fmt.Errorf("Raw: %w", err)
						return
					}
					r.m.Sweep(time.Now())
				}
			}()
			time.Sleep(10 * time.Millisecond)
			listed, _, err := r.m.Touch(ctx, r.req, agent)
			time.Sleep(10 * time.Millisecond)
			close(stop)
			if rerr := <-readerDone; rerr != nil {
				t.Fatal(rerr)
			}
			if err != nil {
				t.Fatalf("Touch from the listing: %v", err)
			}
			if tg := lockedTarget(pg); listed != pg || !tg.physical || tg.device.OSVersion != "16.7.10" {
				t.Fatalf("listed selection = page %p (want %p), physical %v, iOS %q: want the record with the listed target", listed, pg, tg.physical, tg.device.OSVersion)
			}
		}
	})
	t.Run("Sweep renewal and idle expiry", func(t *testing.T) {
		// The renewal reads the physical flag; the expiry releases the
		// lease (releaseRuntime logs the title when the fake fails the
		// page-side release) and logs the title.
		r, _ := attachedRig(t, fakewebkit.Options{}, ManagerOptions{SweepEvery: time.Hour})
		stop := attachLoop(r, agent)
		defer stop()
		for range raceRepeats {
			leased(t, r)
			r.m.Sweep(time.Now())
			r.m.Sweep(time.Now().Add(2 * DefaultLeaseTTL))
		}
	})
	t.Run("acquire waits that end at the deadline", func(t *testing.T) {
		// s2 waits for s1's lease until its deadline: the error names the page.
		r, _ := attachedRig(t, fakewebkit.Options{}, ManagerOptions{})
		stop := attachLoop(r, agent)
		defer stop()
		s2 := AcquireOptions{Session: "s2", Reservation: "agent", WaitLease: time.Minute}
		for range raceRepeats {
			if _, _, err := r.m.Touch(ctxFor(t, waitDeadline), r.req, s2); exitCode(err) != CodeTimeout {
				t.Fatalf("Touch s2 = %v (exit %d), want exit 124 at the deadline", err, exitCode(err))
			}
		}
	})
	t.Run("acquire waits for a release past the deadline", func(t *testing.T) {
		// A held scheduler operation keeps s1's release running; s2's
		// Touch waits for it until its deadline: the error names the page.
		r, pg := attachedRig(t, fakewebkit.Options{}, ManagerOptions{})
		_, epoch := pg.PS.Sched.Lease()
		held, err := pg.PS.Sched.Begin(ctxFor(t, 5*time.Second), OpSpec{Name: "held", Class: ClassRead, Session: "s1", Epoch: epoch})
		if err != nil {
			t.Fatalf("held operation: %v", err)
		}
		breakErr := make(chan error, 1)
		go func() { breakErr <- r.m.Break(ctxFor(t, 10*time.Second), pg.Key) }()
		eventually(t, 3*time.Second, "release claimed", func() bool {
			pg.mu.Lock()
			defer pg.mu.Unlock()
			return pg.releasing
		})
		stop := attachLoop(r, agent)
		s2 := AcquireOptions{Session: "s2", Reservation: "agent"}
		for range raceRepeats {
			if _, _, err := r.m.Touch(ctxFor(t, waitDeadline), r.req, s2); exitCode(err) != CodeTimeout {
				t.Fatalf("Touch s2 = %v (exit %d), want exit 124 while the release runs", err, exitCode(err))
			}
		}
		stop()
		held.End()
		if err := <-breakErr; err != nil {
			t.Fatalf("Break: %v", err)
		}
	})
	t.Run("runtime release skipped at the deadline", func(t *testing.T) {
		// A Break with less than runtimeReleaseReserve+runtimeReleaseMin
		// left skips the page-side release and logs the title.
		r, pg := attachedRig(t, fakewebkit.Options{}, ManagerOptions{})
		stop := attachLoop(r, agent)
		defer stop()
		for range raceRepeats {
			leased(t, r)
			_ = r.m.Break(ctxFor(t, runtimeReleaseReserve), pg.Key)
		}
	})
	t.Run("release without cleanup and idle close of a lost page", func(t *testing.T) {
		// A lost page's release logs the title, and so does the sweeper
		// when it closes the page without a lease. Touch refuses the lost
		// page after writing the target, so it takes no lease. The first
		// sweep runs while the attaches write the target; it leaves the
		// page to the next sweep when it meets an attach of the page
		// (closeIdle), so a sweep after the attaches ended closes it
		// (review devfix-R1 F4; TestIdleCloseAndAConcurrentTouch pins the
		// interleavings).
		for range raceRepeats {
			r, pg := attachedRig(t, fakewebkit.Options{Steal: true}, ManagerOptions{SweepEvery: time.Hour})
			stealPage(t, r.f)
			eventually(t, 3*time.Second, "lease lost", func() bool { return pg.ConnState() == ConnLost })
			stop := attachLoop(r, agent)
			err := r.m.Break(ctxFor(t, 5*time.Second), pg.Key)
			r.m.Sweep(time.Now().Add(2 * DefaultIdleClose))
			stop()
			r.m.Sweep(time.Now().Add(2 * DefaultIdleClose))
			if err != nil {
				t.Fatalf("Break: %v", err)
			}
			if st := pg.ConnState(); st != ConnClosed {
				t.Fatalf("state after the sweep = %v, want closed", st)
			}
		}
	})
}
