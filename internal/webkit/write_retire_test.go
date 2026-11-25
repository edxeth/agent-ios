package webkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
	"github.com/gorilla/websocket"
)

// core-events r9b (review core-events-r8 F2): gorilla/websocket keeps the
// first write error of a connection and fails every later write with it.
// A failed socket write therefore retires the connection: the failed call
// keeps its own classification (a passed deadline stays the deadline,
// decision D9; a wire failure stays a wire failure), the connection ends
// (Done, Err with ErrWriteFailed), calls written before it fail as on a
// lost connection, and every later call is refused before it writes
// anything (ErrNotSent). The fake socket (fakewebkit.WriteStall.Dialer)
// fails the frame under gorilla/websocket, so its write error is sticky as
// on a real socket.

// promptly bounds the calls that a retired connection must end at once.
const promptly = time.Second

var errReset = &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}

// socketStalledSession is a Session over a WebSocket whose socket stalls
// or fails the frames of w.Method.
func socketStalledSession(t *testing.T, f *fakewebkit.Server, w *fakewebkit.WriteStall, o SessionOptions) *Session {
	t.Helper()
	return newFakeSessionOn(t, func(ctx context.Context) (transport.MessageConn, error) {
		return dialWebSocket(ctx, w.Dialer(), f.URL, transport.Limits{Routed: true})
	}, o)
}

// firstFailures are the two kinds of failed write and the check of the
// failed call's own error.
var firstFailures = []struct {
	name     string
	fail     error
	timeout  time.Duration
	category error // the category of the connection's end (r9c)
	check    func(t *testing.T, err error)
}{
	{"deadline", nil, 300 * time.Millisecond, ErrWriteTimeout, func(t *testing.T, err error) {
		wantDeadline(t, "the call whose write the deadline cut off", err)
	}},
	{"wire failure", errReset, 5 * time.Second, ErrWriteBroken, func(t *testing.T, err error) {
		if !errors.Is(err, syscall.ECONNRESET) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNotSent) {
			t.Fatalf("the call whose write failed = %v, want its wire failure, without the deadline or ErrNotSent", err)
		}
	}},
}

// wantRetired checks that err, the reason why a connection ended, is a
// failed write: ErrSessionClosed and ErrWriteFailed, without the failed
// call's deadline or socket timeout in its chain (the other calls did not
// time out; deadlineCaused reads a socket timeout as the deadline).
func wantRetired(t *testing.T, what string, err error) {
	t.Helper()
	var ne net.Error
	if !errors.Is(err, ErrWriteFailed) || !errors.Is(err, ErrSessionClosed) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) {
		t.Fatalf("%s = %v, want ErrSessionClosed and ErrWriteFailed without the deadline or the socket's error", what, err)
	}
}

// wantEnded waits at most promptly for done.
func wantEnded(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(promptly):
		t.Fatalf("%s: Done is still open after a failed write", what)
	}
}

func TestFailedSocketWriteRetiresTheSession(t *testing.T) {
	for _, tc := range firstFailures {
		t.Run(tc.name, func(t *testing.T) {
			f := fakewebkit.New(fakewebkit.Options{})
			t.Cleanup(f.Close)
			f.Stall("DOM.getDocument")
			w := &fakewebkit.WriteStall{Method: "Runtime.evaluate", Fail: tc.fail}
			s := socketStalledSession(t, f, w, SessionOptions{CallTimeout: 10 * time.Second})

			// A call written before the failure, still waiting for its reply.
			written := make(chan error, 1)
			go func() {
				_, err := s.Send(commandDeadline(t, 10*time.Second), "DOM.getDocument", nil)
				written <- err
			}()
			waitFor(t, func() bool { return len(f.ReceivedMethod("DOM.getDocument")) == 1 })

			_, err := s.Send(commandDeadline(t, tc.timeout), "Runtime.evaluate", map[string]string{"expression": "1"})
			tc.check(t, err)
			wantEnded(t, "Session", s.Done())
			wantRetired(t, "Session.Err", s.Err())
			wantCategory(t, "Session.Err", s.Err(), tc.category)

			select {
			case err := <-written:
				if errors.Is(err, ErrNotSent) || !errors.Is(err, ErrWriteFailed) {
					t.Errorf("call written before the failure = %v, want the end of the connection without ErrNotSent", err)
				}
			case <-time.After(promptly):
				t.Error("the call written before the failure still waits after the connection ended")
			}

			start := time.Now()
			_, err = s.Send(commandDeadline(t, 5*time.Second), "Page.reload", nil)
			if !errors.Is(err, ErrNotSent) || !errors.Is(err, ErrWriteFailed) {
				t.Errorf("call after the failure = %v, want ErrNotSent with the failed write as cause", err)
			}
			if took := time.Since(start); took > promptly {
				t.Errorf("call after the failure returned after %v, want at once", took.Round(time.Millisecond))
			}
			if n := len(f.ReceivedMethod("Page.reload")); n != 0 {
				t.Errorf("the page received %d calls after the failed write, want none", n)
			}
			if w.Stalled() != 1 {
				t.Errorf("%d failed socket writes, want 1", w.Stalled())
			}
		})
	}
}

// Session.sendOuter (Target.resume) retires the session the same way, and
// the failed resume keeps its deadline.
func TestFailedResumeWriteRetiresTheSession(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	w := &fakewebkit.WriteStall{Method: "Target.resume"}
	s := socketStalledSession(t, f, w, SessionOptions{PauseOnStart: true, ResumeAfter: time.Minute, Setup: func(ctx context.Context, tc TargetCaller) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	go f.Swap("page-2", 0)
	waitFor(t, func() bool { return s.IsPaused("page-2") })
	err := s.ResumeTarget(commandDeadline(t, 300*time.Millisecond), "page-2")
	wantDeadline(t, "Target.resume", err)
	wantEnded(t, "Session", s.Done())
	wantRetired(t, "Session.Err", s.Err())
	if _, err := s.Send(commandDeadline(t, 5*time.Second), "Runtime.evaluate", map[string]string{"expression": "1"}); !errors.Is(err, ErrNotSent) {
		t.Errorf("call after the failed resume = %v, want ErrNotSent", err)
	}
}

// Close writes nothing to a retired connection and does not block on it:
// a target still paused on start gets no Target.resume, because the socket
// takes no more data (design 2.7).
func TestCloseOfARetiredSessionWritesNothing(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	w := &fakewebkit.WriteStall{Method: "Runtime.evaluate"}
	s := socketStalledSession(t, f, w, SessionOptions{PauseOnStart: true, ResumeAfter: time.Minute, Setup: func(ctx context.Context, tc TargetCaller) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	go f.Swap("page-2", 0)
	waitFor(t, func() bool { return s.IsPaused("page-2") })
	_, err := s.Send(commandDeadline(t, 300*time.Millisecond), "Runtime.evaluate", map[string]string{"expression": "1"})
	wantDeadline(t, "Runtime.evaluate", err)
	wantEnded(t, "Session", s.Done())
	start := time.Now()
	_ = s.Close()
	if took := time.Since(start); took > promptly {
		t.Errorf("Close of a retired session took %v, want at once", took.Round(time.Millisecond))
	}
	if n := len(f.ReceivedMethod("Target.resume")); n != 0 {
		t.Errorf("Close wrote %d Target.resume to a retired connection, want none", n)
	}
}

// The nudge (SessionOptions.Nudge) whose write the deadline cut off
// retires the session too, and the call that it nudged ends with its
// deadline, not with the end of the session.
func TestFailedNudgeWriteRetiresTheSession(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Stall("DOM.getDocument")
	w := &fakewebkit.WriteStall{Method: "Runtime.evaluate"}
	s := socketStalledSession(t, f, w, SessionOptions{Nudge: true, NudgeAfter: 50 * time.Millisecond, CallTimeout: 10 * time.Second})
	_, err := s.Send(commandDeadline(t, 300*time.Millisecond), "DOM.getDocument", nil)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errCommandDeadline) || errors.Is(err, ErrWriteFailed) {
		t.Fatalf("nudged call = %v, want its deadline with its cause, not the end of the session", err)
	}
	if w.Stalled() != 1 {
		t.Fatalf("fixture: %d stalled nudge writes, want 1", w.Stalled())
	}
	wantEnded(t, "Session", s.Done())
	wantRetired(t, "Session.Err", s.Err())
}

func TestFailedSocketWriteRetiresTheClient(t *testing.T) {
	for _, tc := range firstFailures {
		t.Run(tc.name, func(t *testing.T) {
			w := &fakewebkit.WriteStall{Method: "Runtime.evaluate", Fail: tc.fail}
			f := fakewebkit.New(fakewebkit.Options{})
			t.Cleanup(f.Close)
			f.Stall("DOM.getDocument")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := NewClientWithDialer(ctx, f.URL, w.Dialer())
			if err != nil {
				t.Fatalf("NewClientWithDialer: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })
			if err := c.Err(); err != nil {
				t.Fatalf("Err of an open client = %v, want nil", err)
			}

			written := make(chan error, 1)
			go func() {
				_, err := c.Send(commandDeadline(t, 10*time.Second), "DOM.getDocument", nil)
				written <- err
			}()
			waitFor(t, func() bool { return len(f.ReceivedMethod("DOM.getDocument")) == 1 })

			_, err = c.Send(commandDeadline(t, tc.timeout), "Runtime.evaluate", map[string]string{"expression": "1"})
			tc.check(t, err)
			wantEnded(t, "Client", c.Done())
			wantRetired(t, "Client.Err", c.Err())
			wantCategory(t, "Client.Err", c.Err(), tc.category)

			select {
			case err := <-written:
				if err == nil || errors.Is(err, ErrNotSent) {
					t.Errorf("call written before the failure = %v, want the end of the connection without ErrNotSent", err)
				}
			case <-time.After(promptly):
				t.Error("the call written before the failure still waits after the connection ended")
			}

			_, err = c.Send(commandDeadline(t, 5*time.Second), "Page.reload", nil)
			if !errors.Is(err, ErrNotSent) || !errors.Is(err, ErrWriteFailed) {
				t.Errorf("call after the failure = %v, want ErrNotSent with the failed write as cause", err)
			}
			if n := len(f.ReceivedMethod("Page.reload")); n != 0 {
				t.Errorf("the page received %d calls after the failed write, want none", n)
			}
		})
	}
}

// A client that Close or the far side ended reports ErrSessionClosed, not
// a failed write.
func TestClientErrAfterAnOrdinaryEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	closed, err := NewClient(ctx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	if err := closed.Err(); err != ErrSessionClosed {
		t.Errorf("Err after Close = %v, want ErrSessionClosed", err)
	}
	far := fakewebkit.New(fakewebkit.Options{})
	gone, err := NewClient(ctx, far.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gone.Close() })
	far.Close()
	select {
	case <-gone.Done():
	case <-ctx.Done():
		t.Fatal("the client did not end when the far side closed")
	}
	if err := gone.Err(); err != ErrSessionClosed {
		t.Errorf("Err after the far side closed = %v, want ErrSessionClosed", err)
	}
}

// core-events r9c (review core-events-r9b F5): the end of a retired
// connection keeps the category of its failed write. Only a write that its
// deadline or call budget ended (a net.Error with Timeout) is local; every
// other failure is a peer or network failure, and the daemon loses the
// page (design 2.5 rule 3).

// wantCategory checks that err holds category and not the other one.
func wantCategory(t *testing.T, what string, err, category error) {
	t.Helper()
	other := ErrWriteTimeout
	if category == ErrWriteTimeout {
		other = ErrWriteBroken
	}
	if !errors.Is(err, category) || errors.Is(err, other) {
		t.Fatalf("%s = %v, want the category %q only", what, err, category)
	}
}

func TestFailedWriteCategory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		category error
	}{
		{"socket deadline", &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}, ErrWriteTimeout},
		{"context deadline", fmt.Errorf("writing: %w", context.DeadlineExceeded), ErrWriteTimeout},
		{"broken pipe", &net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE}, ErrWriteBroken},
		{"connection reset", errReset, ErrWriteBroken},
		{"closed connection", &net.OpError{Op: "write", Net: "tcp", Err: net.ErrClosed}, ErrWriteBroken},
		{"end of stream", io.EOF, ErrWriteBroken},
		{"close frame sent", websocket.ErrCloseSent, ErrWriteBroken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := &cutConn{}
			cause := retiredBy(wire.failed(tc.err))
			wantRetired(t, "end cause", cause)
			wantCategory(t, "end cause", cause, tc.category)
		})
	}
}

// The first published cause of a connection's end stays: a peer close
// after a write timed out does not turn the timeout into a disconnect.
func TestRetiredClientKeepsItsFirstCause(t *testing.T) {
	w := &fakewebkit.WriteStall{Method: "Runtime.evaluate"}
	f := fakewebkit.New(fakewebkit.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := NewClientWithDialer(ctx, f.URL, w.Dialer())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_, err = c.Send(commandDeadline(t, 200*time.Millisecond), "Runtime.evaluate", map[string]string{"expression": "1"})
	wantDeadline(t, "Runtime.evaluate", err)
	f.Close() // the peer closes after the timeout ended the connection
	wantCategory(t, "Client.Err after the peer closed", c.Err(), ErrWriteTimeout)
}
