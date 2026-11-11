package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// core-events r9b (review core-events-r8 F2): a socket write that the
// command's deadline cut off retires the page connection, because
// gorilla/websocket fails every later write of a connection with the first
// write error. The command exits 124; the page is not lost: the background
// recovery connects again while the lease is valid, and the next command,
// with a fresh deadline, succeeds through the new connection. On this path
// (a command's own call, pageConn.send) the code before r9b recovered too,
// by chance: the liveness probe that follows a deadline failed on the
// sticky write error. The test pins that the retired connection keeps exit
// 124 and does not make the page lost. TestSetupWriteCutOffByTheDeadlineReconnects
// covers a write without that probe.

// preemptedCtx is a write context whose Err waits until the context ended
// and then returns its earlier (nil) result: the scheduling of a write
// whose deadline passes after wsConn.Write checked the context and before
// the socket write.
type preemptedCtx struct{ context.Context }

func (c preemptedCtx) Err() error {
	err := c.Context.Err()
	if err == nil {
		<-c.Done()
	}
	return err
}

// preemptOnce runs the first write that contains marker under a
// preemptedCtx.
type preemptOnce struct {
	transport.MessageConn
	marker []byte
	armed  *atomic.Bool
}

func (c preemptOnce) Write(ctx context.Context, msg []byte) error {
	if bytes.Contains(msg, c.marker) && c.armed.CompareAndSwap(true, false) {
		ctx = preemptedCtx{ctx}
	}
	return c.MessageConn.Write(ctx, msg)
}

// preemptTransport is the iwdp transport whose connections preempt the
// first write that contains marker.
type preemptTransport struct {
	transport.Transport
	marker []byte
	armed  *atomic.Bool
}

func (t preemptTransport) Open(ctx context.Context, p transport.Page) (transport.MessageConn, error) {
	c, err := t.Transport.Open(ctx, p)
	if err != nil {
		return nil, err
	}
	return preemptOnce{MessageConn: c, marker: t.marker, armed: t.armed}, nil
}

func TestWriteCutOffByTheDeadlineReconnects(t *testing.T) {
	type setup struct {
		r     *rig
		req   PageRequest
		pageF *fakewebkit.Server // the page's endpoint
		first string             // the method whose write the deadline cuts off
	}
	for name, open := range map[string]func(t *testing.T) setup{
		"session": func(t *testing.T) setup {
			var armed atomic.Bool
			armed.Store(true)
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{Transport: func(_ context.Context, kind, addr string) (transport.Transport, error) {
				return preemptTransport{Transport: iwdp.New(iwdp.Options{ListingAddr: addr}), marker: []byte("r9b-cut-off"), armed: &armed}, nil
			}})
			r.reserve(t, "agent")
			return setup{r: r, req: r.req, pageF: r.f, first: "Runtime.evaluate"}
		},
		"legacy client": func(t *testing.T) setup {
			// The socket stalls every frame of DOM.getDocument until its
			// write deadline; no setup, probe or later command sends it.
			w := &fakewebkit.WriteStall{Method: "DOM.getDocument"}
			r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{OpenURL: func(ctx context.Context, u string) (webkit.Caller, error) {
				return webkit.NewClientWithDialer(ctx, u, w.Dialer())
			}})
			local := fakewebkit.New(fakewebkit.Options{})
			t.Cleanup(local.Close)
			return setup{r: r, req: PageRequest{Transport: "iwdp", Addr: r.l.Addr(), Page: local.URL}, pageF: local, first: "DOM.getDocument"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			mo, err := s.r.m.Begin(ctxFor(t, 5*time.Second), s.req, agent, "attach", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			s.r.m.End(mo.ID)
			pg := mo.Page

			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			mo, err = s.r.m.Begin(ctx, s.req, agent, "first", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			params := json.RawMessage(`{"expression":"'r9b-cut-off'"}`)
			if s.first == "DOM.getDocument" {
				params = nil
			}
			_, err = s.r.m.Raw(ctx, mo.ID, s.first, params)
			s.r.m.End(mo.ID)
			if exitCode(err) != CodeTimeout || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, webkit.ErrNotSent) {
				t.Fatalf("first command = %v (exit %d), want the deadline (exit %d), written", err, exitCode(err), CodeTimeout)
			}
			if st := pg.ConnState(); st != ConnStalled && st != ConnOK {
				t.Fatalf("page state after the failed write = %v, want stalled (reconnecting), not lost", st)
			}
			// The recovery holds attachMu while it reconnects, and the next
			// command waits for it there. A command that comes in the
			// instant between the start of the recovery and its attachMu
			// is told that a reconnect runs (ensureAttached), so the test
			// waits for the reconnect first.
			eventually(t, 5*time.Second, "background reconnect", func() bool {
				return pg.ConnState() == ConnOK && s.pageF.ConnectionCount() == 2
			})

			ctx2 := ctxFor(t, 2*time.Second)
			mo, err = s.r.m.Begin(ctx2, s.req, agent, "next", ClassExclusive)
			if err != nil {
				t.Fatalf("Begin of the next command: %v", err)
			}
			defer s.r.m.End(mo.ID)
			if _, err := s.r.m.Raw(ctx2, mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"1"}`)); err != nil {
				t.Fatalf("next command = %v, want its reply through the new connection", err)
			}
			if n := s.pageF.ConnectionCount(); n != 2 {
				t.Errorf("connections = %d, want 2 (one reconnect)", n)
			}
			pg.conn.mu.Lock()
			kept := len(pg.conn.expected)
			pg.conn.mu.Unlock()
			if kept != 0 {
				// watch has seen the retired connection end; nothing would
				// ever remove such a record.
				t.Errorf("pageConn keeps %d expected-close records after the reconnect, want none", kept)
			}
		})
	}
}

// The same through a write that pageConn.send does not make, so that no
// liveness probe follows the deadline: the reapply of a failed provisional
// setup at Begin (recoverSetup, Session.ReapplySetup). Before, the
// connection stayed installed with gorilla's sticky write error, and every
// later Begin failed at once with the old "i/o timeout".
func TestSetupWriteCutOffByTheDeadlineReconnects(t *testing.T) {
	var armed atomic.Bool
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{Transport: func(_ context.Context, kind, addr string) (transport.Transport, error) {
		return preemptTransport{Transport: iwdp.New(iwdp.Options{ListingAddr: addr}), marker: []byte(`"targetId":"page-2"`), armed: &armed}, nil
	}})
	r.reserve(t, "agent")
	ctx := ctxFor(t, 10*time.Second)
	pg, _, err := r.m.Touch(ctx, r.req, agent)
	if err != nil {
		t.Fatal(err)
	}
	var refusing atomic.Bool
	refusing.Store(true)
	r.f.Handle("Page.setBootstrapScript", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if refusing.Load() && c.Target == "page-2" {
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
	refusing.Store(false)

	// The first write of the reapply (to page-2) waits past its deadline.
	armed.Store(true)
	first, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, err = r.m.Begin(first, r.req, agent, "first", ClassExclusive)
	if exitCode(err) != CodeTimeout || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first command = %v (exit %d), want the deadline (exit %d)", err, exitCode(err), CodeTimeout)
	}
	if armed.Load() {
		t.Fatal("fixture: the reapply wrote nothing to page-2")
	}
	eventually(t, 5*time.Second, "background reconnect", func() bool {
		return pg.ConnState() == ConnOK && r.f.ConnectionCount() == 2
	})
	next := ctxFor(t, 2*time.Second)
	mo, err := r.m.Begin(next, r.req, agent, "next", ClassExclusive)
	if err != nil {
		t.Fatalf("Begin of the next command: %v", err)
	}
	defer r.m.End(mo.ID)
	if _, err := r.m.Raw(next, mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"1"}`)); err != nil {
		t.Fatalf("next command = %v, want its reply through the new connection", err)
	}
}

// slowErrClient is a legacy client whose Err takes a while, as a busy
// scheduler can delay the goroutine of pageConn.watch.
type slowErrClient struct{ *webkit.Client }

func (c slowErrClient) Err() error {
	time.Sleep(300 * time.Millisecond)
	return c.Client.Err()
}

// pageConn.send marks the page stalled before it returns the failed call,
// so the next command takes the recovery path and never reaches the
// retired connection; watch alone would mark it later, from its own
// goroutine.
func TestSendStallsAPageWhoseWriteFailedBeforeItReturns(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	w := &fakewebkit.WriteStall{Method: "DOM.getDocument"}
	c, err := webkit.NewClientWithDialer(ctxFor(t, 5*time.Second), f.URL, w.Dialer())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	var recoveries atomic.Int32
	p := newPageConn(slowErrClient{c}, func() { recoveries.Add(1) }, func(func(context.Context)) bool { return false }, func(string, ...any) {})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := p.Send(ctx, "DOM.getDocument", nil); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, webkit.ErrNotSent) {
		t.Fatalf("failed call = %v, want its deadline, written", err)
	}
	if st := p.State(); st != ConnStalled {
		t.Fatalf("state when the failed call returned = %v, want stalled", st)
	}
	eventually(t, 2*time.Second, "recovery started", func() bool { return recoveries.Load() == 1 })
}

// A failed write that pageConn.send did not make (here a direct call on
// the connection) marks the page stalled through watch and starts the
// recovery once; the page is not lost.
func TestWatchStallsAPageWhoseWriteFailed(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	w := &fakewebkit.WriteStall{Method: "DOM.getDocument"}
	c, err := webkit.NewClientWithDialer(ctxFor(t, 5*time.Second), f.URL, w.Dialer())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	var recoveries atomic.Int32
	p := newPageConn(c, func() { recoveries.Add(1) }, trackedBackground(t), func(string, ...any) {})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := c.Send(ctx, "DOM.getDocument", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fixture: %v, want the deadline", err)
	}
	eventually(t, 2*time.Second, "page stalled", func() bool { return p.State() == ConnStalled })
	time.Sleep(50 * time.Millisecond) // a second verdict would come now
	if n := recoveries.Load(); n != 1 {
		t.Errorf("recoveries started = %d, want 1", n)
	}
	select {
	case <-p.Done():
		t.Error("the page is lost after a failed write of its own connection")
	default:
	}
	if _, err := p.Send(ctxFor(t, time.Second), "Runtime.evaluate", nil); !errors.Is(err, webkit.ErrNotSent) || !errors.Is(err, webkit.ErrWriteFailed) || exitCode(err) != CodeBusy {
		t.Errorf("call on the stalled page = %v (exit %d), want ErrNotSent, the failed write and exit %d", err, exitCode(err), CodeBusy)
	}
}

// A connection that does not say why it ended (no Err; here a client
// behind the plain Caller interface) is lost when it ends unexpectedly, as
// before: only a reported failed write keeps the page.
func TestConnectionWithoutACauseIsStillLost(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	c, err := webkit.NewClient(ctxFor(t, 5*time.Second), f.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	var recoveries atomic.Int32
	p := newPageConn(struct{ webkit.Caller }{c}, func() { recoveries.Add(1) }, trackedBackground(t), func(string, ...any) {})
	f.Close()
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the page is not lost after its connection ended without a cause")
	}
	if st, n := p.State(), recoveries.Load(); st != ConnLost || n != 0 {
		t.Errorf("state %v, %d recoveries; want lost and none", st, n)
	}
}

// endedCaller is a connection that has ended with err (Err).
type endedCaller struct {
	webkit.Caller
	done chan struct{}
	err  error
}

func (c endedCaller) Done() <-chan struct{} { return c.done }
func (c endedCaller) Err() error            { return c.err }

// core-events r9c: the first published cause decides. A page that a peer
// disconnect already made lost stays lost when a connection that ended
// with a write timeout is reported afterwards, and no recovery starts.
func TestTimedOutWriteAfterALossKeepsThePageLost(t *testing.T) {
	done := make(chan struct{})
	close(done)
	timedOut := fmt.Errorf("%w: %w: %w", webkit.ErrSessionClosed, webkit.ErrWriteFailed, webkit.ErrWriteTimeout)
	c := endedCaller{done: done, err: timedOut}
	var recoveries atomic.Int32
	p := newPageConn(c, func() { recoveries.Add(1) }, trackedBackground(t), func(string, ...any) {})
	// The read error was published first: the page is lost.
	p.mu.Lock()
	p.state = ConnLost
	p.mu.Unlock()
	if !p.writeFailed(c) {
		t.Fatal("a connection that ended with a write timeout was not recognized")
	}
	time.Sleep(50 * time.Millisecond) // watch has run too
	if st, n := p.State(), recoveries.Load(); st != ConnLost || n != 0 {
		t.Errorf("state %v, %d recoveries; want lost and none", st, n)
	}
}

// probeStub is a connection whose calls fail with sendErr; ended closes its
// Done, and err is its Err.
type probeStub struct {
	endedCaller
	sendErr error
}

func (c probeStub) Send(context.Context, string, any) (json.RawMessage, error) {
	return nil, c.sendErr
}

// noCauseStub is an ended connection that does not say why (no Err).
type noCauseStub struct {
	webkit.Caller
	done chan struct{}
}

func (c noCauseStub) Done() <-chan struct{} { return c.done }
func (c noCauseStub) Send(context.Context, string, any) (json.RawMessage, error) {
	return nil, webkit.ErrSessionClosed
}

// core-events r9d: a failed liveness probe declares a stall only while its
// connection is open (round 5) or after a write timeout ended it; any other
// end, also one without a known cause, leaves the page to watch's lost
// policy.
func TestProbeStallsOnlyWithoutAPeerEnd(t *testing.T) {
	ended := make(chan struct{})
	close(ended)
	end := func(category error) error {
		return fmt.Errorf("%w: %w: %w", webkit.ErrSessionClosed, webkit.ErrWriteFailed, category)
	}
	timeout := &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}
	for _, tc := range []struct {
		name  string
		c     webkit.Caller
		stall bool
	}{
		{"open connection, no reply", probeStub{endedCaller{done: make(chan struct{})}, timeout}, true},
		{"ended by a write timeout", probeStub{endedCaller{done: ended, err: end(webkit.ErrWriteTimeout)}, timeout}, true},
		{"ended by a broken write", probeStub{endedCaller{done: ended, err: end(webkit.ErrWriteBroken)}, errors.New("broken pipe")}, false},
		{"ended by a read error", probeStub{endedCaller{done: ended, err: webkit.ErrSessionClosed}, webkit.ErrSessionClosed}, false},
		{"ended without a known cause", noCauseStub{done: ended}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var recoveries atomic.Int32
			p := newPageConn(tc.c, func() { recoveries.Add(1) }, func(func(context.Context)) bool { return false }, func(string, ...any) {})
			p.mu.Lock()
			p.expected[tc.c] = true // watch leaves the state to the probe
			p.mu.Unlock()
			err := p.probe(ctxFor(t, time.Second), tc.c)
			stalled := p.State() == ConnStalled
			if err == nil || stalled != tc.stall || recoveries.Load() != map[bool]int32{true: 1, false: 0}[tc.stall] {
				t.Fatalf("probe = %v: stalled %v with %d recoveries; want stalled %v", err, stalled, recoveries.Load(), tc.stall)
			}
			if !tc.stall && !errors.Is(err, ErrLost) {
				t.Errorf("probe = %v, want ErrLost for a connection that a peer ended", err)
			}
		})
	}
}
