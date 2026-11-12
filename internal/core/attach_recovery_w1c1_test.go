package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// W1c-1: attach and recovery in the core (w1c-backlog.md C03, C05, C06,
// C07, C10, C11 and the core part of L07). These tests use only behavior
// that the base had too, so each fails on the base for its item's cause;
// the tests of the new internal pieces are in handoff_w1c1_test.go.

// stalledWithHeldRecovery attaches the rig's page, stalls its connection
// (the call and the liveness probe get no reply) and returns once the
// background recovery runs, held after it took attachMu and retired the
// old connection, until release is called.
func stalledWithHeldRecovery(t *testing.T, mo ManagerOptions) (r *rig, pg *Page, release func()) {
	t.Helper()
	r = newRig(t, fakewebkit.Options{}, false, mo)
	r.reserve(t, "agent")
	held, released := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(released) }) }
	t.Cleanup(release)
	r.m.recoveryHold = func(ctx context.Context) {
		close(held)
		select {
		case <-ctx.Done():
		case <-released:
		}
	}
	mo1, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, agent, "eval", ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	r.f.StallConnection(1)
	_, _ = r.m.Raw(ctxFor(t, 500*time.Millisecond), mo1.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"1"}`))
	r.m.End(mo1.ID)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("fixture: no background recovery started after the failed probe")
	}
	return r, mo1.Page, release
}

// evalOnce runs one command (Begin, one Runtime.evaluate, End) for agent.
func evalOnce(ctx context.Context, r *rig, ao AcquireOptions) error {
	mo, err := r.m.Begin(ctx, r.req, ao, "eval", ClassExclusive)
	if err != nil {
		return err
	}
	defer r.m.End(mo.ID)
	_, err = r.m.Raw(ctx, mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"2"}`))
	return err
}

// C05 (orchestrator decision): a command that comes while a background
// recovery runs waits for it within its own deadline. Before, it waited on
// the recovery's attach lock without its deadline (up to the 15 s recovery
// budget), and in the instant before the recovery took that lock it got
// exit 4 at once.
func TestCommandWaitsForTheRecoveryWithinItsDeadline(t *testing.T) {
	t.Run("deadline first", func(t *testing.T) {
		r, _, _ := stalledWithHeldRecovery(t, ManagerOptions{})
		start := time.Now()
		err := evalOnce(ctxFor(t, 300*time.Millisecond), r, agent)
		if took := time.Since(start); exitCode(err) != CodeTimeout || took > 2*time.Second {
			t.Fatalf("command during the recovery = %v (exit %d) after %v; want a timeout (exit %d) at its 300 ms deadline", err, exitCode(err), took.Round(time.Millisecond), CodeTimeout)
		}
	})
	t.Run("recovery succeeds", func(t *testing.T) {
		r, _, release := stalledWithHeldRecovery(t, ManagerOptions{})
		got := make(chan error, 1)
		go func() { got <- evalOnce(ctxFor(t, 10*time.Second), r, agent) }()
		select {
		case err := <-got:
			t.Fatalf("command returned while the recovery runs: %v (exit %d), want it to wait", err, exitCode(err))
		case <-time.After(300 * time.Millisecond):
		}
		release()
		select {
		case err := <-got:
			if err != nil {
				t.Fatalf("command after the recovery = %v, want its reply through the new connection", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("command did not end after the recovery")
		}
		if n := r.f.ConnectionCount(); n != 2 {
			t.Errorf("connections = %d, want 2 (the recovery's)", n)
		}
	})
	t.Run("recovery fails", func(t *testing.T) {
		r, _, release := stalledWithHeldRecovery(t, ManagerOptions{RecoveryBudget: 2 * time.Second})
		r.f.StallConnection(2) // the recovery's connection does not answer its setup
		got := make(chan error, 1)
		go func() { got <- evalOnce(ctxFor(t, 10*time.Second), r, agent) }()
		release()
		var err error
		select {
		case err = <-got:
		case <-time.After(10 * time.Second):
			t.Fatal("command did not end after the recovery failed")
		}
		if exitCode(err) != CodeBusy || !errors.Is(err, ErrRecoveryFailed) {
			t.Fatalf("command after a failed recovery = %v (exit %d), want the recovery's failure (exit %d)", err, exitCode(err), CodeBusy)
		}
		if n := r.f.ConnectionCount(); n != 2 {
			t.Errorf("connections = %d, want 2: the waiting command must not reopen the page", n)
		}
	})
	t.Run("new connection ended by a peer", func(t *testing.T) {
		// C06 in the recovery: the failure names the loss and --takeover,
		// not a page that does not answer.
		r, _, release := stalledWithHeldRecovery(t, ManagerOptions{})
		closeOnProbe(r.f, 2)
		got := make(chan error, 1)
		go func() { got <- evalOnce(ctxFor(t, 10*time.Second), r, agent) }()
		release()
		var err error
		select {
		case err = <-got:
		case <-time.After(10 * time.Second):
			t.Fatal("command did not end after the recovery failed")
		}
		if exitCode(err) != CodeBusy || !errors.Is(err, ErrRecoveryFailed) || !errors.Is(err, ErrLost) || strings.Contains(err.Error(), "does not answer") {
			t.Fatalf("command after a recovery whose connection a peer ended = %v (exit %d), want the recovery's failure with ErrLost (exit %d)", err, exitCode(err), CodeBusy)
		}
	})
}

// C05, the instant between the start of a recovery and its attach lock:
// the command waits for the recovery instead of failing with exit 4.
func TestCommandWaitsForARecoveryBeforeItsAttachLock(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fail     bool
		deadline bool // the recovery does not end within the command's deadline
		canceled bool // the recovery ends without a failure and leaves the page stalled
	}{{name: "recovered"}, {name: "failed", fail: true}, {name: "deadline first", deadline: true}, {name: "ended without a failure", canceled: true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
			r.reserve(t, "agent")
			if err := evalOnce(ctxFor(t, 5*time.Second), r, agent); err != nil {
				t.Fatal(err)
			}
			pg := r.m.FindAttached(r.f.URL)
			if pg == nil {
				t.Fatal("fixture: no attached page")
			}
			finish := fakeRecoveryStart(pg)
			if tc.deadline {
				start := time.Now()
				err := evalOnce(ctxFor(t, 300*time.Millisecond), r, agent)
				if took := time.Since(start); exitCode(err) != CodeTimeout || took > 2*time.Second {
					t.Fatalf("command while a recovery runs past its deadline = %v (exit %d) after %v, want a timeout (exit %d) at its deadline", err, exitCode(err), took.Round(time.Millisecond), CodeTimeout)
				}
				finish(nil)
				return
			}
			got := make(chan error, 1)
			go func() { got <- evalOnce(ctxFor(t, 10*time.Second), r, agent) }()
			select {
			case err := <-got:
				t.Fatalf("command while a recovery starts = %v (exit %d), want it to wait", err, exitCode(err))
			case <-time.After(300 * time.Millisecond):
			}
			var failure error
			switch {
			case tc.fail:
				failure = fmt.Errorf("%w: the new connection does not answer either", ErrRecoveryFailed)
			case tc.canceled:
				failure = webkit.ErrSessionStalled // as after a recovery that Close canceled
			}
			finish(failure)
			err := <-got
			switch {
			case tc.fail && (exitCode(err) != CodeBusy || !errors.Is(err, ErrRecoveryFailed)):
				t.Fatalf("command after a failed recovery = %v (exit %d), want its failure (exit %d)", err, exitCode(err), CodeBusy)
			case !tc.fail && err != nil:
				t.Fatalf("command after the recovery = %v, want success", err)
			}
			// A recovery that ended without a failure leaves the stalled page
			// to the command, which reopens it.
			want := 1
			if tc.canceled {
				want = 2
			}
			if n := r.f.ConnectionCount(); n != want {
				t.Errorf("connections = %d, want %d", n, want)
			}
		})
	}
}

// peerEndCaller is a connection that a peer ends during a call: the call
// gets no reply within its budget, and the liveness probe that follows
// fails on the ended connection.
type peerEndCaller struct {
	endedCaller
	once *sync.Once
}

func (c peerEndCaller) Send(_ context.Context, method string, _ any) (json.RawMessage, error) {
	if method == "Runtime.evaluate" {
		return nil, errors.New("liveness probe: write: broken pipe")
	}
	c.once.Do(func() { close(c.done) })
	return nil, fmt.Errorf("%s: %w", method, webkit.ErrCallTimeout)
}

// C06: a call whose liveness probe found the connection ended by a peer
// keeps ErrLost and its --takeover guidance (exit 4); it does not say that
// the page is stalled and a reconnect runs.
func TestCallKeepsTheLossWhenTheProbeFindsAPeerEnd(t *testing.T) {
	c := peerEndCaller{endedCaller{done: make(chan struct{}), err: fmt.Errorf("%w: %w", webkit.ErrSessionClosed, webkit.ErrWriteBroken)}, &sync.Once{}}
	recoveries := 0
	p := newPageConn(c, func() { recoveries++ }, func(func(context.Context)) bool { return false }, func(string, ...any) {})
	p.mu.Lock()
	p.expected[c] = true // watch leaves the state to the probe
	p.mu.Unlock()
	_, err := p.Send(ctxFor(t, 2*time.Second), "DOM.getDocument", nil)
	if !errors.Is(err, ErrLost) || errors.Is(err, webkit.ErrSessionStalled) || exitCode(err) != CodeBusy || !strings.Contains(err.Error(), "--takeover") {
		t.Fatalf("call = %v (exit %d), want ErrLost with the --takeover guidance (exit %d), not stalled", err, exitCode(err), CodeBusy)
	}
	if st := p.State(); st == ConnStalled || recoveries != 0 {
		t.Errorf("state %v with %d recoveries, want no stall and no recovery", st, recoveries)
	}
}

// closeOnProbe makes the fake close every connection when connection conn
// gets a liveness probe (Runtime.evaluate of "1"), as when another
// inspector takes the page during the probe.
func closeOnProbe(f *fakewebkit.Server, conn int) {
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		if p.Expression == "1" && (conn == 0 || c.Conn == conn) {
			f.CloseConnections()
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
	})
}

// C06, the idle probe of Manager.Begin: a peer end during the probe is a
// loss with the --takeover guidance, not "did not answer the liveness
// probe".
func TestIdleProbeKeepsTheLossOfAPeerEnd(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{IdleProbe: 100 * time.Millisecond})
	r.reserve(t, "agent")
	if err := evalOnce(ctxFor(t, 5*time.Second), r, agent); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // idle past IdleProbe
	closeOnProbe(r.f, 0)
	_, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, agent, "eval", ClassExclusive)
	if !errors.Is(err, ErrLost) || errors.Is(err, webkit.ErrSessionStalled) || exitCode(err) != CodeBusy || !strings.Contains(err.Error(), "--takeover") {
		t.Fatalf("Begin after idle = %v (exit %d), want ErrLost with the --takeover guidance (exit %d), not stalled", err, exitCode(err), CodeBusy)
	}
}

// L07, core part: when the fresh connection of a takeover or reopen ends
// during its liveness probe, the error is the loss with the --takeover
// guidance, not "not the visible Safari tab".
func TestFreshProbePeerEndIsNoHiddenTab(t *testing.T) {
	for _, why := range []string{"takeover", "reopen"} {
		t.Run(why, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
			r.reserve(t, "agent")
			if err := evalOnce(ctxFor(t, 5*time.Second), r, agent); err != nil {
				t.Fatal(err)
			}
			pg := r.m.FindAttached(r.f.URL)
			ao := agent
			if why == "takeover" {
				r.f.CloseConnections()
				eventually(t, 5*time.Second, "lost page", func() bool { return pg.ConnState() == ConnLost })
				ao.Takeover = true
			} else {
				// A stalled page without a recovery: the next command reopens it.
				pg.conn.mu.Lock()
				pg.conn.state, pg.conn.stallErr = ConnStalled, webkit.ErrSessionStalled
				pg.conn.mu.Unlock()
			}
			closeOnProbe(r.f, 2)
			err := evalOnce(ctxFor(t, 5*time.Second), r, ao)
			if !errors.Is(err, ErrLost) || exitCode(err) != CodeBusy || strings.Contains(err.Error(), "visible Safari tab") || !strings.Contains(err.Error(), "--takeover") {
				t.Fatalf("%s whose new connection ended during the probe = %v (exit %d), want ErrLost with the --takeover guidance (exit %d), no hidden-tab text", why, err, exitCode(err), CodeBusy)
			}
		})
	}
}

// Device check of 2026-11-12: iwdp ended the first attach to a page that
// no other client held, and the error told to run --takeover. A first
// attach whose new connection a peer ends has nothing to take over: its
// error is not ErrLost, names no --takeover and tells to run the command
// again, and the next command attaches without --takeover.
func TestFirstAttachPeerEndTellsToRunAgain(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	closeOnProbe(r.f, 1)
	err := evalOnce(ctxFor(t, 5*time.Second), r, agent)
	if err == nil || errors.Is(err, ErrLost) || exitCode(err) != CodeBusy || strings.Contains(err.Error(), "--takeover") || !strings.Contains(err.Error(), "run the command again") {
		t.Fatalf("first attach whose new connection a peer ended = %v (exit %d); want exit %d, no ErrLost, no --takeover, and the advice to run it again", err, exitCode(err), CodeBusy)
	}
	if err := evalOnce(ctxFor(t, 5*time.Second), r, agent); err != nil {
		t.Fatalf("the next command without --takeover = %v, want it to attach", err)
	}
}

// C07: the events of the re-apply on a new connection reach the page's
// subscribers, here the Console.enable replay, through a takeover and
// through a background recovery; each subscriber gets each event once.
func TestReapplyEventsReachSubscribers(t *testing.T) {
	for _, why := range []string{"takeover", "recovery"} {
		t.Run(why, func(t *testing.T) {
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
			r.reserve(t, "agent")
			pg := consoleOwner(t, r)
			const marker = "replayed-w1c1"
			r.f.Handle("Console.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
				if c.Conn >= 2 {
					r.f.EmitFrom(c.Target, "Console.messageAdded", map[string]any{"message": map[string]any{"source": "console-api", "level": "log", "text": marker}})
				}
				return nil, nil
			})
			var mu sync.Mutex
			got := map[string]int{}
			count := func(kind string) func(string, json.RawMessage) {
				return func(_ string, params json.RawMessage) {
					if strings.Contains(string(params), marker) {
						mu.Lock()
						got[kind]++
						mu.Unlock()
					}
				}
			}
			n := func(kind string) int {
				mu.Lock()
				defer mu.Unlock()
				return got[kind]
			}
			defer pg.conn.Subscribe("Console.messageAdded", count("queued"))()
			defer pg.conn.SubscribeSync("Console.messageAdded", count("read loop"))()
			if why == "takeover" {
				r.f.CloseConnections()
				eventually(t, 5*time.Second, "lost page", func() bool { return pg.ConnState() == ConnLost })
				ao := agent
				ao.Takeover = true
				if err := evalOnce(ctxFor(t, 5*time.Second), r, ao); err != nil {
					t.Fatal(err)
				}
				// The read-loop subscribers got the held events before the
				// swap returned.
				if c := n("read loop"); c != 1 {
					t.Errorf("read-loop subscriber got the replayed message %d times when the takeover returned, want 1", c)
				}
			} else {
				r.f.StallConnection(1)
				mo, err := r.m.Begin(ctxFor(t, 5*time.Second), r.req, agent, "eval", ClassExclusive)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = r.m.Raw(ctxFor(t, 500*time.Millisecond), mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"3"}`))
				r.m.End(mo.ID)
				eventually(t, 10*time.Second, "background reconnect", func() bool {
					return pg.ConnState() == ConnOK && r.f.ConnectionCount() == 2
				})
			}
			eventually(t, 5*time.Second, "the replayed message at every subscriber", func() bool { return n("queued") >= 1 && n("read loop") >= 1 })
			time.Sleep(100 * time.Millisecond) // a duplicate would arrive now
			if q, s := n("queued"), n("read loop"); q != 1 || s != 1 {
				t.Errorf("replayed message delivered %d times (queued) and %d times (read loop), want once each", q, s)
			}
		})
	}
}

// C03: a commit that leaves a cleanup pending starts a bounded background
// reset, so the committed target loses the released state without waiting
// for a next change, and gets the cleanup once (the reset sends the queued
// cleanup, which ends the mark; it does not reset the agent a second time).
func TestCommitLeavingCleanupResetsInTheBackground(t *testing.T) {
	r, pg, b := queuedReleaseCleanup(t, "page-2")
	allowCommitReset(r)
	d := pg.PS.State
	before := disablesOn(r.f, "page-2")
	commit(t, r, d, "page-2")
	eventually(t, 5*time.Second, "the released header gone from the committed page-2 without a next change", func() bool {
		return b.header("page-2", "X-Released") == "" && len(d.unknownDomains()) == 0
	})
	time.Sleep(200 * time.Millisecond) // a second reset would come now
	if n := disablesOn(r.f, "page-2") - before; n != 1 {
		t.Errorf("Network.disable sent to page-2 %d times after the commit, want 1", n)
	}
}

// C10: a cancellation by the client during a new connection's setup is
// that cancellation, not "busy" (exit 4).
func TestCancelDuringSetupIsNoBusy(t *testing.T) {
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{})
	r.reserve(t, "agent")
	r.f.Stall("Page.enable") // the first connection's setup waits
	ctx, cancel := context.WithCancel(ctxFor(t, 5*time.Second))
	go func() {
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && len(r.f.ReceivedMethod("Page.enable")) == 0; {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	_, err := r.m.Begin(ctx, r.req, agent, "eval", ClassExclusive)
	if !errors.Is(err, context.Canceled) || exitCode(err) == CodeBusy || exitCode(err) == CodeTimeout {
		t.Fatalf("Begin canceled during the setup = %v (exit %d), want the cancellation, not busy or a timeout", err, exitCode(err))
	}
}

// C11: Manager.Close waits for the watch goroutine of every page
// connection; none is left after Close returns, also for a connection that
// has not ended.
func TestCloseEndsTheConnectionWatch(t *testing.T) {
	// Other tests' page connections without a manager may leave watches
	// of connections that never end; only this one's is counted.
	watches := func() int {
		buf := make([]byte, 1<<22)
		return strings.Count(string(buf[:runtime.Stack(buf, true)]), "(*pageConn).watch")
	}
	before := watches()
	m := NewManager(ManagerOptions{})
	c := endedCaller{done: make(chan struct{})} // never ends by itself
	newPageConn(c, nil, m.goBackground, func(string, ...any) {})
	if err := m.Close(ctxFor(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if after := watches(); after > before {
		t.Fatalf("connection watches: %d before the page connection, %d after Close returned; want its watch ended", before, after)
	}
}
