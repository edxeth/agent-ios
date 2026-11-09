package core

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// core-events r9c (review core-events-r9b F5): the reviewer's reproduction,
// with its assertion unchanged.

// Delay only delivery of the first real read error. This represents the
// Session read goroutine being descheduled after the peer closed, so a
// real socket write can report the disconnect first.
type peerReadGate struct {
	transport.MessageConn
	armed            *atomic.Bool
	reached, release chan struct{}
}

func (c peerReadGate) Read(ctx context.Context) ([]byte, error) {
	b, err := c.MessageConn.Read(ctx)
	if err != nil && c.armed.CompareAndSwap(true, false) {
		close(c.reached)
		<-c.release
	}
	return b, err
}

type peerGateTransport struct {
	transport.Transport
	armed            *atomic.Bool
	reached, release chan struct{}
}

func (t peerGateTransport) Open(ctx context.Context, p transport.Page) (transport.MessageConn, error) {
	c, err := t.Transport.Open(ctx, p)
	if err != nil {
		return nil, err
	}
	return peerReadGate{c, t.armed, t.reached, t.release}, nil
}

// Design 2.5 rule 3 / acceptance 7.3.7(d): another inspector taking the
// page makes the lease lost and never triggers automatic reconnect.
func TestReviewPeerStealBeforeWriteFailureStaysLost(t *testing.T) {
	var armed atomic.Bool
	reached, release := make(chan struct{}), make(chan struct{})
	r := newRig(t, fakewebkit.Options{Steal: true}, false, ManagerOptions{
		Transport: func(_ context.Context, kind, addr string) (transport.Transport, error) {
			return peerGateTransport{iwdp.New(iwdp.Options{ListingAddr: addr}), &armed, reached, release}, nil
		},
	})
	var once sync.Once
	releaseRead := func() { once.Do(func() { close(release) }) }
	defer releaseRead()
	r.reserve(t, "agent")
	pg, _, err := r.m.Touch(ctxFor(t, 5*time.Second), r.req, agent)
	if err != nil {
		t.Fatal(err)
	}
	c, err := pg.conn.current()
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	stealPage(t, r.f)
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture: stolen connection did not receive a read error")
	}
	// Only the test's own deadline bounds the send: building the 4 MiB
	// message took more than 3 s under -race and a host load of 70 (devfix
	// R3), and its write fails at once on the closed socket.
	sendCtx := context.Background()
	if d, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		sendCtx, cancel = context.WithDeadline(sendCtx, d)
		defer cancel()
	}
	_, err = c.Send(sendCtx, "Runtime.evaluate", map[string]string{"expression": strings.Repeat("x", 4<<20)})
	releaseRead() // reader resumes now; retirement has already fixed the close cause.
	var ne net.Error
	if !errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fixture: expected a real socket write failure, got %v", err)
	}
	t.Logf("old send=%v; state=%v; retired=%v", err, pg.ConnState(), errors.Is(c.(interface{ Err() error }).Err(), webkit.ErrWriteFailed))
	eventually(t, 5*time.Second, "lost state or unauthorized reconnect", func() bool {
		return pg.ConnState() == ConnLost || r.f.ConnectionCount() > 2
	})
	if n := r.f.ConnectionCount(); pg.ConnState() != ConnLost || n != 2 {
		t.Fatalf("another inspector stole the page, but daemon state=%v connections=%d; want lost and 2 connections, with no automatic reconnect", pg.ConnState(), n)
	}
}

// slowErrSession is a Session whose Err takes a while, as a busy scheduler
// can delay the goroutine of pageConn.watch after the connection ended.
type slowErrSession struct{ *webkit.Session }

func (s slowErrSession) Err() error {
	time.Sleep(300 * time.Millisecond)
	return s.Session.Err()
}

// core-events r9d: the liveness probe is the same F5 class. Another
// inspector takes the page, and the probe's own write fails with a real
// broken pipe before the loss is published. The probe must not declare a
// stall: the page is lost, and no recovery opens a third connection
// (design 2.5 rule 3, acceptance 7.3.7(d)). The read error waits until the
// probe has decided (as in TestReviewPeerStealBeforeWriteFailureStaysLost),
// and watch's verdict comes late (slowErrSession).
func TestPeerStealBeforeProbeWriteFailureStaysLost(t *testing.T) {
	var armed atomic.Bool
	reached, release := make(chan struct{}), make(chan struct{})
	local := fakewebkit.New(fakewebkit.Options{Steal: true})
	t.Cleanup(local.Close)
	var conns atomic.Int32
	var first atomic.Pointer[webkit.Session]
	r := newRig(t, fakewebkit.Options{}, false, ManagerOptions{OpenURL: func(ctx context.Context, u string) (webkit.Caller, error) {
		conns.Add(1)
		s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
			c, err := webkit.DialWebSocket(ctx, u, transport.Limits{Routed: true})
			if err != nil {
				return nil, err
			}
			return peerReadGate{c, &armed, reached, release}, nil
		}, webkit.SessionOptions{TargetWait: time.Second})
		if err != nil {
			return nil, err
		}
		first.CompareAndSwap(nil, s)
		return slowErrSession{s}, nil
	}})
	var once sync.Once
	releaseRead := func() { once.Do(func() { close(release) }) }
	defer releaseRead()
	req := PageRequest{Transport: "iwdp", Addr: r.l.Addr(), Page: local.URL}
	pg, _, err := r.m.Touch(ctxFor(t, 5*time.Second), req, agent)
	if err != nil {
		t.Fatal(err)
	}
	s := first.Load()
	armed.Store(true)
	stealPage(t, local)
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture: stolen connection did not receive a read error")
	}
	// A small write to the closed socket still goes out; the far side
	// answers it with a reset, so the probe's write below gets the broken
	// pipe. The call itself ends at its budget without a reply.
	prime, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, _ = s.Send(prime, "Runtime.evaluate", map[string]string{"expression": "1"})
	cancel()
	select {
	case <-s.Done():
		t.Fatal("fixture: the priming write already ended the connection")
	default:
	}
	perr := pg.conn.probeNow(ctxFor(t, 3*time.Second))
	releaseRead() // the reader resumes; the probe has already decided
	var ne net.Error
	if !errors.As(perr, &ne) || ne.Timeout() || !errors.Is(s.Err(), webkit.ErrWriteBroken) {
		t.Fatalf("fixture: probe = %v, connection end = %v; want the probe's real socket write failure", perr, s.Err())
	}
	eventually(t, 5*time.Second, "lost state or unauthorized reconnect", func() bool {
		return pg.ConnState() == ConnLost || conns.Load() > 1
	})
	time.Sleep(500 * time.Millisecond) // a recovery started by the probe would connect now
	if n := local.ConnectionCount(); pg.ConnState() != ConnLost || n != 2 {
		t.Fatalf("another inspector stole the page, but daemon state=%v connections=%d; want lost and 2 connections, with no automatic reconnect", pg.ConnState(), n)
	}
}
