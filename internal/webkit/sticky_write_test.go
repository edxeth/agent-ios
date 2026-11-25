package webkit

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// core-events r9b: gorilla/websocket keeps the first write error of a
// connection forever, so a socket write that failed retires the
// connection: it reports itself ended, and later calls are refused before
// they write anything. The two tests are the reviewer's reproductions of
// review core-events-r8 F2, with their assertions unchanged.

// preemptedContext reproduces preemption after wsConn.Write's context
// check returned nil and before it installs the socket deadline and writes
// the frame: Err waits until the context ended and returns its earlier
// (nil) result.
type preemptedContext struct{ context.Context }

func (c preemptedContext) Err() error {
	err := c.Context.Err()
	if err == nil {
		<-c.Done()
	}
	return err
}

// preemptWrite runs its next Write under a preemptedContext.
type preemptWrite struct {
	transport.MessageConn
	preempt atomic.Bool
}

func (c *preemptWrite) Write(ctx context.Context, data []byte) error {
	if c.preempt.Swap(false) {
		ctx = preemptedContext{ctx}
	}
	return c.MessageConn.Write(ctx, data)
}

func TestReviewStickyLegacyWriteRetiresTheClient(t *testing.T) {
	w := &fakewebkit.WriteStall{Method: "Runtime.evaluate"}
	c := stalledClient(t, w)
	_, first := c.Send(commandDeadline(t, 50*time.Millisecond), "Runtime.evaluate", map[string]string{"expression": "1"})
	if !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("fixture: first=%v", first)
	}
	fresh, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, next := c.Send(fresh, "DOM.getDocument", nil)
	t.Logf("first=%v next=%v", first, next)
	if next == nil {
		return
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("legacy Client keeps a permanently unwritable connection open")
	}
}

func TestReviewStickyWriteRetiresTheSession(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	var wire *preemptWrite
	s := newFakeSessionOn(t, func(ctx context.Context) (transport.MessageConn, error) {
		c, err := DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
		wire = &preemptWrite{MessageConn: c}
		return wire, err
	}, SessionOptions{})
	wire.preempt.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, first := s.Send(ctx, "Runtime.evaluate", map[string]string{"expression": "1"})
	if !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("fixture: first=%v, want expired socket write", first)
	}
	fresh, cancelFresh := context.WithTimeout(context.Background(), time.Second)
	defer cancelFresh()
	_, next := s.Send(fresh, "Runtime.evaluate", map[string]string{"expression": "1"})
	t.Logf("first=%v next=%v sessionErr=%v", first, next, s.Err())
	if next == nil {
		return
	}
	if errors.Is(next, context.DeadlineExceeded) {
		t.Fatal("fixture: second command itself expired")
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("all future writes fail with the old timeout, but Session still reports a live connection")
	}
}
