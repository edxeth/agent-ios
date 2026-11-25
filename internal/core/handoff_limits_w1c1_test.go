package core

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W1c-1 repair round 1 (review w1c-core-20261108-064740, F4 to F7): the
// limits of the event handoff of a new connection, the shutdown fence of
// a reconnect and the takeover guard after a new connection's peer end.
// The reviewer's reproductions are in review_handoff_limits_test.go.

// numbered returns the JSON strings that emit sends for from..to-1.
func numbered(from, to int) []string {
	out := make([]string, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, `"`+strconv.Itoa(i)+`"`)
	}
	return out
}

// F5. Source: Session.Subscribe and SessionOptions.EventQueue: each
// subscriber holds at most webkit.DefaultEventQueue events, a full queue
// drops the new event and counts it (DroppedEvents). A queued subscriber
// of a prepared connection has the same budget and the same accounting.
func TestPreparedQueuedEventsBeyondTheLimitAreDroppedAndCounted(t *testing.T) {
	h := newHandoffRig(t, trackedBackground(t))
	h.unsubs[0]() // only the queued subscriber
	next := newEmitCallerFor(t)
	h.p.prepare(next)
	const extra = 5
	for i := 0; i < webkit.DefaultEventQueue+extra; i++ {
		next.emit(strconv.Itoa(i))
	}
	if n := h.p.DroppedEvents(); n != extra {
		t.Errorf("DroppedEvents before the swap = %d, want %d (the events beyond the %d-event budget)", n, extra, webkit.DefaultEventQueue)
	}
	mustSwap(t, h.p, next)
	h.await(t, "async", webkit.DefaultEventQueue)
	time.Sleep(50 * time.Millisecond) // a dropped event would arrive now
	if got, want := h.events("async"), numbered(0, webkit.DefaultEventQueue); !equalStrings(got, want) {
		t.Errorf("queued subscriber got %d events (first %v), want the first %d in order", len(got), got[:min(3, len(got))], len(want))
	}
}

// F5. Source: the review's rule for read-loop bookkeeping: it is never
// dropped silently; when it cannot be kept, the new connection is not
// installed. The page keeps its connection, and the new one keeps no
// subscription.
func TestPreparedReadLoopOverflowRefusesTheSwap(t *testing.T) {
	h := newHandoffRig(t, trackedBackground(t))
	next := newEmitCallerFor(t)
	h.p.prepare(next)
	for i := 0; i <= webkit.DefaultEventQueue; i++ {
		next.emit(strconv.Itoa(i))
	}
	err := h.p.swap(next)
	if err == nil {
		t.Fatal("swap installed a connection whose read-loop subscriber lost an event")
	}
	if h.p.currentConn() != h.first {
		t.Error("swap refused the connection but installed it")
	}
	if n := next.subscribers(); n != 0 {
		t.Errorf("subscriptions left on the refused connection = %d, want 0", n)
	}
	// One event beyond the budget for each of the two subscribers.
	if n := h.p.DroppedEvents(); n != 2 {
		t.Errorf("DroppedEvents = %d, want 2", n)
	}
	h.first.emit("old")
	h.await(t, "async", 1)
	for _, kind := range []string{"sync", "async"} {
		if got := h.events(kind); !equalStrings(got, []string{`"old"`}) {
			t.Errorf("%s subscriber got %d events (first %v), want only the kept connection's [\"old\"]", kind, len(got), got[:min(3, len(got))])
		}
	}
}

// F5. A read-loop event that finds its subscriber's buffer full while swap
// delivers the held read-loop events waits for that delivery, in its
// order; it is neither dropped nor does it fail the installed connection.
func TestReadLoopEventWaitsWhileSwapDelivers(t *testing.T) {
	first, next := newEmitCallerFor(t), newEmitCallerFor(t)
	p := newPageConn(first, nil, trackedBackground(t), t.Logf)
	var mu sync.Mutex
	var got []string
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p.SubscribeTargetSync("Console.messageAdded", func(_, _ string, params json.RawMessage) {
		mu.Lock()
		got = append(got, string(params))
		mu.Unlock()
		once.Do(func() {
			close(entered)
			<-gate
		})
	})
	p.prepare(next)
	n := webkit.DefaultEventQueue
	for i := 0; i < n; i++ { // exactly the budget: no loss
		next.emit(strconv.Itoa(i))
	}
	swapped := make(chan error, 1)
	go func() { swapped <- p.swap(next) }()
	<-entered // swap delivers the first held event
	for i := n; i < 2*n; i++ {
		next.emit(strconv.Itoa(i)) // the held batch left the buffer: these fit
	}
	late := make(chan struct{})
	go func() {
		next.emit(strconv.Itoa(2 * n))
		close(late)
	}()
	select {
	case <-late:
		t.Error("an event beyond the budget did not wait while swap delivered the held events")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	if err := <-swapped; err != nil {
		t.Fatalf("swap = %v, want the connection installed", err)
	}
	<-late
	mu.Lock()
	defer mu.Unlock()
	if want := numbered(0, 2*n+1); !equalStrings(got, want) {
		t.Errorf("read-loop subscriber got %d events, want %d in order", len(got), len(want))
	}
	if d := p.DroppedEvents(); d != 0 {
		t.Errorf("DroppedEvents = %d, want 0", d)
	}
}

// F4. An event that arrives while a queued subscriber's held events are
// still being replayed comes after them, as on its own queue.
func TestReplayKeepsLaterEventsBehindHeldOnes(t *testing.T) {
	h := newHandoffRig(t, trackedBackground(t))
	h.unsubs[0]()
	next := newEmitCallerFor(t)
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var got []string
	defer h.p.Subscribe("Console.messageAdded", func(_ string, params json.RawMessage) {
		once.Do(func() {
			close(entered)
			<-gate
		})
		mu.Lock()
		defer mu.Unlock()
		got = append(got, string(params))
	})()
	h.p.prepare(next)
	next.emit("a")
	next.emit("b")
	mustSwap(t, h.p, next)
	<-entered
	next.emit("c")
	close(gate)
	eventually(t, 2*time.Second, "the replay and the later event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 3
	})
	mu.Lock()
	defer mu.Unlock()
	if want := []string{`"a"`, `"b"`, `"c"`}; !equalStrings(got, want) {
		t.Errorf("slow subscriber got %v, want %v", got, want)
	}
}

// F6. Source: C11 and the review's rule: after Close began, no reconnect
// installs a connection. A reopen whose setup runs while Close runs closes
// its new connection and fails.
func TestReopenRacingCloseDoesNotInstallAfterShutdown(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	if err := evalOnce(ctxFor(t, 5*time.Second), r, agent); err != nil {
		t.Fatal(err)
	}
	pg := r.m.FindAttached(r.f.URL)
	pg.conn.mu.Lock()
	pg.conn.state, pg.conn.stallErr = ConnStalled, webkit.ErrSessionStalled
	pg.conn.mu.Unlock()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.f.Handle("Page.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if c.Conn == 2 {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil, nil
	})
	reopened := make(chan error, 1)
	go func() { reopened <- evalOnce(ctxFor(t, 5*time.Second), r, agent) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("fixture: the reopen did not set up a new connection")
	}
	closed := make(chan error, 1)
	go func() { closed <- r.m.Close(ctxFor(t, 5*time.Second)) }()
	eventually(t, 2*time.Second, "Close began", r.m.shuttingDown)
	close(release)
	err := <-reopened
	if cerr := <-closed; cerr != nil {
		t.Errorf("Close = %v", cerr)
	}
	conns := r.f.OpenConnections()
	for end := time.Now().Add(time.Second); conns != 0 && time.Now().Before(end); conns = r.f.OpenConnections() {
		time.Sleep(10 * time.Millisecond)
	}
	st := pg.ConnState()
	pg.conn.close() // Close is idempotent: close an escaped connection here
	if err == nil || conns != 0 || st != ConnClosed {
		t.Fatalf("reopen during Close = %v; page %s with %d live connections, want an error, closed and none", err, st, conns)
	}
}

// F7. Source: design 2.5 rule 3: a connection that a peer ended is lost,
// and only --takeover connects the page again. This holds for the new
// connection of a reopen that a peer ends during the re-apply of the page
// state, as during its liveness probe (review_handoff_limits_test.go).
func TestReapplyPeerEndRequiresTakeoverOnNextCommand(t *testing.T) {
	r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{})
	r.reserve(t, "agent")
	if err := evalOnce(ctxFor(t, 5*time.Second), r, agent); err != nil {
		t.Fatal(err)
	}
	pg := r.m.FindAttached(r.f.URL)
	pg.conn.mu.Lock()
	pg.conn.state, pg.conn.stallErr = ConnStalled, webkit.ErrSessionStalled
	pg.conn.mu.Unlock()
	peers := make(chan transport.MessageConn, 1)
	var once sync.Once
	r.f.Handle("Page.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if c.Conn == 2 {
			once.Do(func() {
				// Another inspector connects while the reopen sets up.
				s, err := webkit.DialWebSocket(ctxFor(t, 5*time.Second), r.f.URL, transport.Limits{Routed: true})
				if err != nil {
					t.Errorf("peer inspector: %v", err)
				}
				peers <- s
			})
		}
		return nil, nil
	})
	err := evalOnce(ctxFor(t, 5*time.Second), r, agent)
	select {
	case peer := <-peers:
		if peer != nil {
			defer func() { _ = peer.Close() }()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fixture: no peer inspector connected during the reopen's setup")
	}
	if !errors.Is(err, ErrLost) || exitCode(err) != CodeBusy {
		t.Fatalf("reopen whose new connection a peer ended during its setup = %v (exit %d), want ErrLost (exit %d)", err, exitCode(err), CodeBusy)
	}
	before := r.f.ConnectionCount()
	err = evalOnce(ctxFor(t, 5*time.Second), r, agent)
	if !errors.Is(err, ErrLost) || r.f.ConnectionCount() != before {
		t.Fatalf("next command without --takeover = %v with %d -> %d connections, want ErrLost and no new connection", err, before, r.f.ConnectionCount())
	}
	takeover := agent
	takeover.Takeover = true
	if err := evalOnce(ctxFor(t, 5*time.Second), r, takeover); err != nil {
		t.Fatalf("command with --takeover = %v, want the page connected again", err)
	}
}

// F7. The same rule for the background recovery: when a peer ends its new
// connection during the liveness probe, the page is lost, and a later
// command without --takeover dials nothing.
func TestRecoveryPeerEndRequiresTakeoverOnNextCommand(t *testing.T) {
	r, pg, release := stalledWithHeldRecovery(t, ManagerOptions{})
	closeOnProbe(r.f, 2)
	release()
	eventually(t, 10*time.Second, "the recovery ended", func() bool {
		pg.mu.Lock()
		defer pg.mu.Unlock()
		return !pg.recovering
	})
	if st := pg.ConnState(); st != ConnLost {
		t.Fatalf("page after a recovery whose new connection a peer ended = %s, want lost", st)
	}
	before := r.f.ConnectionCount()
	err := evalOnce(ctxFor(t, 5*time.Second), r, agent)
	if !errors.Is(err, ErrLost) || exitCode(err) != CodeBusy || r.f.ConnectionCount() != before {
		t.Fatalf("next command without --takeover = %v (exit %d) with %d -> %d connections, want ErrLost (exit %d) and no new connection", err, exitCode(err), before, r.f.ConnectionCount(), CodeBusy)
	}
	takeover := agent
	takeover.Takeover = true
	if err := evalOnce(ctxFor(t, 5*time.Second), r, takeover); err != nil {
		t.Fatalf("command with --takeover = %v, want the page connected again", err)
	}
}

// countingCaller is an emitCaller that reports dropped events as a
// webkit.Session does (DroppedEvents).
type countingCaller struct {
	*emitCaller
	dropped int64
}

func (c countingCaller) DroppedEvents() int64 { return c.dropped }

// F5. A queued subscriber whose replay runs keeps the same budget for the
// events that arrive meanwhile; DroppedEvents adds the current
// connection's own count (webkit.Session.DroppedEvents).
func TestReplayingSubscriberKeepsTheLimit(t *testing.T) {
	first := newEmitCallerFor(t)
	next := countingCaller{emitCaller: newEmitCallerFor(t), dropped: 7}
	p := newPageConn(first, nil, trackedBackground(t), t.Logf)
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var got []string
	defer p.Subscribe("Console.messageAdded", func(_ string, params json.RawMessage) {
		once.Do(func() {
			close(entered)
			<-gate
		})
		mu.Lock()
		defer mu.Unlock()
		got = append(got, string(params))
	})()
	p.prepare(next)
	next.emit("held")
	mustSwap(t, p, next)
	<-entered // the replay delivers "held" and waits
	n := webkit.DefaultEventQueue
	for i := 0; i <= n; i++ {
		next.emit(strconv.Itoa(i))
	}
	if d := p.DroppedEvents(); d != 1+next.dropped {
		t.Errorf("DroppedEvents = %d, want 1 dropped by the replay's budget + %d of the connection", d, next.dropped)
	}
	close(gate)
	eventually(t, 2*time.Second, "the replay", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= n+1
	})
	time.Sleep(50 * time.Millisecond) // a dropped event would arrive now
	mu.Lock()
	defer mu.Unlock()
	if want := append([]string{`"held"`}, numbered(0, n)...); !equalStrings(got, want) {
		t.Errorf("subscriber got %d events, want %d: the held one and the first %d later ones in order", len(got), len(want), n)
	}
}

// emitDuringSetup makes the fake send n Debugger.resumed events, which
// the scheduler counts in the read loop (Scheduler: setPaused), while the
// setup of connection conn enables Page.
func emitDuringSetup(r *rig, conn, n int) {
	var once sync.Once
	r.f.Handle("Page.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if c.Conn == conn {
			once.Do(func() {
				for i := 0; i < n; i++ {
					r.f.Emit("Debugger.resumed", map[string]any{})
				}
			})
		}
		return nil, nil
	})
}

// F5. A reopen whose new connection delivered more read-loop events than
// the budget before its install is not installed: it fails (exit 4), and
// the next command reopens the page.
func TestReopenWithReadLoopOverflowIsNotInstalled(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	if err := evalOnce(ctxFor(t, 5*time.Second), r, agent); err != nil {
		t.Fatal(err)
	}
	pg := r.m.FindAttached(r.f.URL)
	pg.conn.mu.Lock()
	pg.conn.state, pg.conn.stallErr = ConnStalled, webkit.ErrSessionStalled
	pg.conn.mu.Unlock()
	emitDuringSetup(r, 2, webkit.DefaultEventQueue+1)
	err := evalOnce(ctxFor(t, 5*time.Second), r, agent)
	if err == nil || exitCode(err) != CodeBusy || !strings.Contains(err.Error(), "read-loop subscription") {
		t.Fatalf("reopen with an overflow = %v (exit %d), want the overflow (exit %d)", err, exitCode(err), CodeBusy)
	}
	if st := pg.ConnState(); st == ConnOK {
		t.Fatal("the page is attached on the refused connection")
	}
	if err := evalOnce(ctxFor(t, 5*time.Second), r, agent); err != nil {
		t.Fatalf("next command = %v, want the page reopened", err)
	}
	if n := r.f.ConnectionCount(); n != 3 {
		t.Errorf("connections = %d, want 3 (the refused one and the reopen)", n)
	}
}

// F5. The same for the background recovery: it fails with the overflow,
// and the page stays stalled with the recovery's failure.
func TestRecoveryWithReadLoopOverflowIsNotInstalled(t *testing.T) {
	r, pg, release := stalledWithHeldRecovery(t, ManagerOptions{})
	emitDuringSetup(r, 2, webkit.DefaultEventQueue+1)
	release()
	eventually(t, 10*time.Second, "the recovery ended", func() bool {
		pg.mu.Lock()
		defer pg.mu.Unlock()
		return !pg.recovering
	})
	pg.conn.mu.Lock()
	st, serr := pg.conn.state, pg.conn.stallErr
	pg.conn.mu.Unlock()
	if st != ConnStalled || !errors.Is(serr, ErrRecoveryFailed) || !strings.Contains(serr.Error(), "read-loop subscription") {
		t.Fatalf("page after a recovery with an overflow = %s (%v), want stalled with the recovery's overflow failure", st, serr)
	}
}

// F6. A command after Close began gets the shutdown error and connects
// nothing.
func TestCommandAfterCloseIsRefused(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	if err := r.m.Close(ctxFor(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := evalOnce(ctxFor(t, 5*time.Second), r, agent); !errors.Is(err, errShuttingDown) || r.f.ConnectionCount() != 0 {
		t.Fatalf("command after Close = %v with %d connections, want errShuttingDown and none", err, r.f.ConnectionCount())
	}
}
