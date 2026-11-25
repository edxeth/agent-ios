package webkit

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Decision D9 (docs/notes/w1b.md, "core-events r1: a write cut off by the
// deadline"): a socket write or read whose deadline came from the
// command's context and that the deadline cut off fails as that deadline,
// context.DeadlineExceeded with its cause, so that the command exits 124.
// Before, it was a wire failure ("writing message: write tcp ...: i/o
// timeout"), and the final check's legacy call exited 4. The fake's write
// returns the socket's timeout at the deadline, before the context's own
// timer has necessarily fired, as a real socket does.

var errCommandDeadline = errors.New("the command deadline passed (test cause)")

// commandDeadline is a context whose deadline, d from now, carries
// errCommandDeadline as its cause.
func commandDeadline(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadlineCause(context.Background(), time.Now().Add(d), errCommandDeadline)
	t.Cleanup(cancel)
	return ctx
}

func wantDeadline(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errCommandDeadline) || !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("%s: %v; want context.DeadlineExceeded with its cause and the socket's timeout", what, err)
	}
}

func stalledSession(t *testing.T, w *fakewebkit.WriteStall, o SessionOptions) *Session {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	return newFakeSessionOn(t, func(ctx context.Context) (transport.MessageConn, error) {
		c, err := DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
		if err != nil {
			return nil, err
		}
		return w.Conn(c), nil
	}, o)
}

func stalledClient(t *testing.T, w *fakewebkit.WriteStall) *Client {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewClientWithDialer(ctx, f.URL, w.Dialer())
	if err != nil {
		t.Fatalf("NewClientWithDialer: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestWriteCutOffByTheDeadlineIsTheDeadline(t *testing.T) {
	const deadline = 300 * time.Millisecond
	t.Run("session call", func(t *testing.T) {
		w := &fakewebkit.WriteStall{Method: "Runtime.evaluate"}
		s := stalledSession(t, w, SessionOptions{CallTimeout: 5 * time.Second})
		_, err := s.Send(commandDeadline(t, deadline), "Runtime.evaluate", map[string]string{"expression": "1"})
		wantDeadline(t, "Runtime.evaluate", err)
		if w.Stalled() != 1 {
			t.Fatalf("%d stalled writes, want 1", w.Stalled())
		}
	})
	t.Run("pipelined batch", func(t *testing.T) {
		// core's Apply batch: the bootstrap script follows Page.enable.
		w := &fakewebkit.WriteStall{Method: "Page.setBootstrapScript"}
		s := stalledSession(t, w, SessionOptions{CallTimeout: 5 * time.Second})
		errs := s.SendPipelined(commandDeadline(t, deadline), []Call{
			{Method: "Page.enable"},
			{Method: "Page.setBootstrapScript", Params: map[string]string{"source": "0"}},
			{Method: "Runtime.enable"},
		})
		// Page.enable was answered during the stall; its reply is collected
		// after the batch's writes, when the deadline may have won the race.
		if errs[0] != nil && !errors.Is(errs[0], errCommandDeadline) {
			t.Fatalf("Page.enable: %v, want success or the deadline", errs[0])
		}
		wantDeadline(t, "Page.setBootstrapScript", errs[1])
		if !errors.Is(errs[2], ErrNotSent) {
			t.Fatalf("Runtime.enable after the cut-off write: %v, want ErrNotSent", errs[2])
		}
	})
	t.Run("reset batch of a first enable", func(t *testing.T) {
		// The session resets an agent it does not know in the batch of its
		// first enable (sendWithReset); the reset's write is cut off.
		w := &fakewebkit.WriteStall{Method: "Memory.disable"}
		s := stalledSession(t, w, SessionOptions{CallTimeout: 5 * time.Second})
		_, err := s.Send(commandDeadline(t, deadline), "Memory.enable", nil)
		wantDeadline(t, "Memory.enable", err)
	})
	t.Run("legacy client", func(t *testing.T) {
		w := &fakewebkit.WriteStall{Method: "Runtime.evaluate"}
		c := stalledClient(t, w)
		_, err := c.Send(commandDeadline(t, deadline), "Runtime.evaluate", map[string]string{"expression": "1"})
		wantDeadline(t, "Runtime.evaluate", err)
		if w.Stalled() != 1 {
			t.Fatalf("%d stalled writes, want 1", w.Stalled())
		}
	})
}

// A write failure while the command's deadline is still ahead keeps its
// classification: a wire failure, and a write that a shorter call budget
// cut off, are no deadline of the command.
func TestWriteFailureBeforeTheDeadlineIsNoDeadline(t *testing.T) {
	reset := &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}
	check := func(t *testing.T, err error, want error) {
		t.Helper()
		if err == nil || errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, want) {
			t.Fatalf("Runtime.evaluate: %v; want %v and no deadline error", err, want)
		}
	}
	t.Run("session wire failure", func(t *testing.T) {
		w := &fakewebkit.WriteStall{Method: "Runtime.evaluate", Fail: reset}
		s := stalledSession(t, w, SessionOptions{CallTimeout: 5 * time.Second})
		_, err := s.Send(commandDeadline(t, 5*time.Second), "Runtime.evaluate", nil)
		check(t, err, syscall.ECONNRESET)
	})
	t.Run("session call budget", func(t *testing.T) {
		w := &fakewebkit.WriteStall{Method: "Runtime.evaluate"}
		s := stalledSession(t, w, SessionOptions{CallTimeout: 200 * time.Millisecond})
		_, err := s.Send(commandDeadline(t, 5*time.Second), "Runtime.evaluate", nil)
		check(t, err, os.ErrDeadlineExceeded)
	})
	t.Run("legacy client wire failure", func(t *testing.T) {
		w := &fakewebkit.WriteStall{Method: "Runtime.evaluate", Fail: reset}
		c := stalledClient(t, w)
		_, err := c.Send(commandDeadline(t, 5*time.Second), "Runtime.evaluate", nil)
		check(t, err, syscall.ECONNRESET)
	})
}

// A WebSocket handshake that the command's deadline cuts off is that
// deadline too (the attach of a page that does not answer).
func TestHandshakeCutOffByTheDeadlineIsTheDeadline(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 2) // accepted, never answered
	defer func() {
		_ = l.Close() // ends the acceptor, which closes accepted
		for c := range accepted {
			_ = c.Close()
		}
	}()
	go func() {
		defer close(accepted)
		for range 2 {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	url := "ws://" + l.Addr().String() + "/devtools/page/1"
	_, err = DialWebSocket(commandDeadline(t, 200*time.Millisecond), url, transport.Limits{Routed: true})
	wantDeadline(t, "DialWebSocket", err)
	_, err = NewClient(commandDeadline(t, 200*time.Millisecond), url)
	wantDeadline(t, "NewClient", err)
}
