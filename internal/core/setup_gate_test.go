package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// swapCaller is a page connection whose active target can change: the
// setup result of the active target is settable, and a pending setup is
// modeled by ActiveSetup waiting until pending is closed. It records the
// method of every Send that reaches it.
type swapCaller struct {
	nopCaller
	mu      sync.Mutex
	setup   error
	pending chan struct{} // nil: no setup in progress
	waiting chan struct{} // receives a value each time ActiveSetup starts to wait
	sent    []string
}

func newSwapCaller() *swapCaller { return &swapCaller{waiting: make(chan struct{}, 16)} }

func (c *swapCaller) fail(err error) {
	c.mu.Lock()
	c.setup = err
	c.mu.Unlock()
}

func (c *swapCaller) ActiveSetup(ctx context.Context) error {
	c.mu.Lock()
	pending := c.pending
	c.mu.Unlock()
	if pending != nil {
		c.waiting <- struct{}{}
		select {
		case <-pending:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setup
}

func (c *swapCaller) received() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sent)
}

func (c *swapCaller) Send(_ context.Context, method string, _ any) (json.RawMessage, error) {
	c.mu.Lock()
	c.sent = append(c.sent, method)
	c.mu.Unlock()
	return json.RawMessage(`{}`), nil
}

var errSwapSetup = errors.New("Page.setBootstrapScript: 'Page.setBootstrapScript' was not found")

// Design 2.7 and 2.10 (review round 2, finding 1): the setup gate holds when
// the operation can use the target, not only when it queues. An operation
// that queued while the active target was healthy is refused at admission
// when a target swap with a failed setup happened while it waited, and
// sends nothing.
func TestQueuedOpRefusedAfterFailedSwap(t *testing.T) {
	c := newSwapCaller()
	s := NewScheduler(c, SchedulerOptions{Clock: NewManualClock(0)})
	defer s.Close()
	s.SetLease(testSession, 1)
	spec := OpSpec{Name: "click", Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: time.Hour}
	a, err := s.Begin(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	bErr := make(chan error, 1)
	go func() {
		op, err := s.Begin(context.Background(), spec)
		if err == nil {
			_, sendErr := op.Caller().Send(context.Background(), "Test.b", nil)
			op.End()
			err = fmt.Errorf("Begin admitted the operation onto the failed target (its send: %v)", sendErr)
		}
		bErr <- err
	}()
	waitUntil(t, "b to queue", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.queue) == 1
	})
	c.fail(errSwapSetup) // a provisional target commits with a failed setup
	a.End()
	select {
	case err := <-bErr:
		if !errors.Is(err, errSwapSetup) {
			t.Fatalf("queued operation err = %v, want the setup failure of the new target", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued operation did not finish")
	}
	if got := c.received(); len(got) != 0 {
		t.Fatalf("page connection received %v after the failed swap", got)
	}
}

// Design 2.7 (review round 2, finding 1): an admitted operation sends
// nothing to an active target whose setup failed after the admission, both
// through its Caller and through an attempt of RunSteps. The refusal is a
// not-sent error, so the attempt is not reported as possibly run.
func TestSendRefusedAfterFailedSwap(t *testing.T) {
	t.Run("caller", func(t *testing.T) {
		c := newSwapCaller()
		s := NewScheduler(c, SchedulerOptions{Clock: NewManualClock(0)})
		defer s.Close()
		s.SetLease(testSession, 1)
		op, err := s.Begin(context.Background(), OpSpec{Name: "click", Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		defer op.End()
		c.fail(errSwapSetup)
		if _, err := op.Caller().Send(context.Background(), "Test.x", nil); !errors.Is(err, errSwapSetup) {
			t.Fatalf("Send err = %v, want the setup failure", err)
		}
		if got := c.received(); len(got) != 0 {
			t.Fatalf("page connection received %v", got)
		}
	})
	t.Run("attempt", func(t *testing.T) {
		c := newSwapCaller()
		rt := func(ctx context.Context, cl webkit.Caller, fn string, args any) (json.RawMessage, error) {
			if fn == "act" {
				c.fail(errSwapSetup) // the swap lands between the measurement and the attempt
			}
			if _, err := cl.Send(ctx, "Runtime.evaluate:"+fn, nil); err != nil {
				return nil, err
			}
			if fn == ClockFn {
				return json.RawMessage(`{"now":1,"doc":"d1"}`), nil
			}
			a := args.(AttemptArgs)
			return json.Marshal(AttemptReply{Status: StatusDispatched, Now: 2, Doc: a.Doc})
		}
		s := NewScheduler(c, SchedulerOptions{Clock: NewManualClock(0), Runtime: rt})
		defer s.Close()
		s.SetLease(testSession, 1)
		op, err := s.Begin(context.Background(), OpSpec{Name: "click", Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		defer op.End()
		_, err = op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1})
		var se *StepError
		if !errors.As(err, &se) || !errors.Is(err, errSwapSetup) || se.MayHaveRun {
			t.Fatalf("RunSteps err = %v, want a not-run StepError with the setup failure", err)
		}
		if got := c.received(); !slices.Equal(got, []string{"Runtime.evaluate:" + ClockFn}) {
			t.Fatalf("page connection received %v, want only the clock measurement", got)
		}
	})
}

// Design 2.6 (review round 2, finding 2): one absolute deadline covers all
// waiting of an operation, including the wait for a setup in progress. An
// explicit Deadline earlier than the parent context ends that wait at the
// deadline, and time spent waiting for the setup does not extend the
// deadline of the admitted operation.
func TestSetupWaitBoundByOperationDeadline(t *testing.T) {
	const deadline = time.Second
	t.Run("setup still pending at the deadline", func(t *testing.T) {
		sc := newSwapCaller()
		sc.pending = make(chan struct{}) // never ends within the test
		clk := NewManualClock(0)
		s := NewScheduler(sc, SchedulerOptions{Clock: clk})
		defer s.Close()
		s.SetLease(testSession, 1)
		errc := make(chan error, 1)
		go func() {
			op, err := s.Begin(context.Background(), OpSpec{Name: "click", Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: deadline})
			if err == nil {
				op.End()
			}
			errc <- err
		}()
		<-sc.waiting
		clk.Set(deadline)
		select {
		case err := <-errc:
			if !errors.Is(err, ErrDeadline) {
				t.Fatalf("Begin err = %v, want the operation deadline", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Begin still waits for the setup after the operation deadline passed")
		}
	})
	t.Run("deadline passes while queued", func(t *testing.T) {
		c := newSwapCaller()
		clk := NewManualClock(0)
		s := NewScheduler(c, SchedulerOptions{Clock: clk})
		defer s.Close()
		s.SetLease(testSession, 1)
		a, err := s.Begin(context.Background(), OpSpec{Name: "hold", Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		defer a.End()
		errc := make(chan error, 1)
		go func() {
			op, err := s.Begin(context.Background(), OpSpec{Name: "click", Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: deadline})
			if err == nil {
				op.End()
			}
			errc <- err
		}()
		waitUntil(t, "click to queue", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return len(s.queue) == 1
		})
		clk.Set(deadline)
		select {
		case err := <-errc:
			if !errors.Is(err, ErrDeadline) || !strings.Contains(err.Error(), "while queued") {
				t.Fatalf("Begin err = %v, want the deadline while queued", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Begin still queued after the operation deadline passed")
		}
		s.mu.Lock()
		queued := len(s.queue)
		s.mu.Unlock()
		if queued != 0 {
			t.Fatalf("queue holds %d operations after the expiry, want 0", queued)
		}
	})
	t.Run("setup wait does not extend the deadline", func(t *testing.T) {
		sc := newSwapCaller()
		pending := make(chan struct{})
		sc.pending = pending
		clk := NewManualClock(0)
		s := NewScheduler(sc, SchedulerOptions{Clock: clk})
		defer s.Close()
		s.SetLease(testSession, 1)
		type begun struct {
			op  *Op
			err error
		}
		bc := make(chan begun, 1)
		go func() {
			op, err := s.Begin(context.Background(), OpSpec{Name: "click", Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: deadline})
			bc <- begun{op, err}
		}()
		<-sc.waiting
		clk.Set(600 * time.Millisecond)
		sc.mu.Lock()
		sc.pending = nil
		sc.mu.Unlock()
		close(pending) // the setup ends successfully at 600 ms
		b := <-bc
		if b.err != nil {
			t.Fatalf("Begin: %v", b.err)
		}
		defer b.op.End()
		clk.Set(deadline)
		select {
		case <-b.op.Context().Done():
			if cause := context.Cause(b.op.Context()); !errors.Is(cause, ErrDeadline) {
				t.Fatalf("operation ended with %v, want the deadline", cause)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("operation still active at its deadline; the setup wait extended it")
		}
	})
}

// Design 2.6, cancellation rule 3 (review round 2, finding 3): once RunSteps
// stopped waiting for an attempt before its first send, a delayed send of
// that attempt is refused as not sent (ErrAttemptClosed) and never reaches
// the page connection. This holds when the reply-loss timer closed the
// attempt while the operation is still active, and when a cancel closed it.
func TestClosedAttemptRefusesDelayedSend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(clk *ManualClock, op *Op, errc <-chan error) error // closes the attempt; returns the RunSteps error
		want  error                                                   // RunSteps error and cause of the refusal
	}{
		{"reply lost while the operation is active", func(clk *ManualClock, op *Op, errc <-chan error) error {
			// Move the clock past the attempt boundary plus the reply grace
			// until RunSteps gives up; the operation deadline (1 h) is far away.
			for {
				clk.Set(clk.Now() + DefaultAttemptBudget + DefaultReplyGrace)
				select {
				case err := <-errc:
					if err := op.active(); err != nil {
						t.Fatalf("operation not active (%v); the case needs an active operation", err)
					}
					return err
				case <-time.After(10 * time.Millisecond):
				}
			}
		}, ErrReplyLost},
		{"canceled", func(_ *ManualClock, op *Op, errc <-chan error) error {
			op.Cancel(errClientGone)
			return <-errc
		}, errClientGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := NewManualClock(0)
			c := newSwapCaller()
			inAttempt := make(chan struct{})
			proceed := make(chan struct{})
			lateErr := make(chan error, 1)
			rt := func(ctx context.Context, cl webkit.Caller, fn string, args any) (json.RawMessage, error) {
				if fn == ClockFn {
					if _, err := cl.Send(ctx, "Runtime.evaluate:"+fn, nil); err != nil {
						return nil, err
					}
					return json.RawMessage(`{"now":1,"doc":"d1"}`), nil
				}
				if fn == "cancel" { // the page cancel that watch sends after the cancel
					return json.RawMessage(`null`), nil
				}
				close(inAttempt)
				<-proceed // the send is delayed until RunSteps stopped waiting
				_, err := cl.Send(ctx, "Runtime.evaluate:"+fn, nil)
				lateErr <- err
				return nil, err
			}
			s := NewScheduler(c, SchedulerOptions{Clock: clk, Runtime: rt})
			defer s.Close()
			s.SetLease(testSession, 1)
			op, err := s.Begin(context.Background(), OpSpec{Name: "click", Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			defer op.End()
			errc := make(chan error, 1)
			go func() { _, err := op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1}); errc <- err }()
			<-inAttempt
			runErr := tc.close(clk, op, errc)
			var se *StepError
			if !errors.As(runErr, &se) || !errors.Is(runErr, tc.want) || se.MayHaveRun {
				t.Fatalf("RunSteps err = %v, want a not-run StepError wrapping %v", runErr, tc.want)
			}
			close(proceed)
			if err := <-lateErr; !errors.Is(err, webkit.ErrNotSent) || !errors.Is(err, ErrAttemptClosed) {
				t.Fatalf("delayed send err = %v, want not sent because the attempt is closed", err)
			}
			if got := c.received(); !slices.Equal(got, []string{"Runtime.evaluate:" + ClockFn}) {
				t.Fatalf("page connection received %v, want only the clock measurement", got)
			}
		})
	}
}

// marshalHook runs fn while the page connection marshals the command's
// params: after the scheduler's checks and before the Session chooses the
// target.
type marshalHook struct{ fn func() }

func (h marshalHook) MarshalJSON() ([]byte, error) {
	h.fn()
	return []byte(`{}`), nil
}

// Design 2.7 and 2.10 (review round 3, finding 1): with a real Session, a
// swap to a target whose setup failed that commits after the operation's
// setup check and before the Session chose the target sends nothing to the
// new target, and the send is refused as not sent. The control lane still
// completes a held request on that target.
func TestOrdinarySendNeverReachesFailedSwapTarget(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Unsupported("Page.setBootstrapScript")
	ctx, cancel := timeoutCtx(t, 5*time.Second)
	sess, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	}, webkit.SessionOptions{PauseOnStart: true, Setup: func(ctx context.Context, tc webkit.TargetCaller) error {
		_, err := tc.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": "x"})
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	s := NewScheduler(sess, SchedulerOptions{})
	t.Cleanup(s.Close)
	s.SetLease(testSession, 1)
	op, err := s.Begin(ctx, OpSpec{Name: "click", Class: ClassMutate, Session: testSession, Epoch: 1})
	if err != nil {
		t.Fatalf("Begin before the swap: %v", err)
	}
	defer op.End()
	swap := func() {
		f.Swap("page-2", 0)
		waitUntil(t, "page-2 to commit", func() bool { return sess.ActiveTarget() == "page-2" })
	}
	_, err = op.Caller().Send(ctx, "Test.x", marshalHook{swap})
	var se *webkit.SetupError
	if !errors.Is(err, webkit.ErrNotSent) || !errors.As(err, &se) || se.Target != "page-2" {
		t.Fatalf("Send err = %v, want not sent with the setup failure of page-2", err)
	}
	if got := f.ReceivedMethod("Test.x"); len(got) != 0 {
		t.Fatalf("fake received %+v, want nothing on the failed target", got)
	}

	f.EmitFrom("page-2", "Network.requestIntercepted", map[string]string{"requestId": "r1"})
	waitUntil(t, "the held request", func() bool { return slices.Contains(s.PendingRequests(), "r1") })
	if _, err := s.Control(ctx, ControlMsg{Kind: ControlCompleteRequest, Session: testSession, Epoch: 1, RequestID: "r1", Method: "Network.interceptContinue"}); err != nil {
		t.Fatalf("control lane on the failed target: %v", err)
	}
	if got := f.ReceivedMethod("Network.interceptContinue"); len(got) != 1 || got[0].Target != "page-2" {
		t.Fatalf("interceptContinue records = %+v, want one on page-2", got)
	}
	cancel()
}

// Design 2.10: a page connection without a separate control path (no
// SendControl, so no setup check on its Send) receives control-lane
// traffic, here the release barrier's cleanup, through its Send.
func TestControlLaneFallsBackToSend(t *testing.T) {
	c := newSwapCaller()
	s := NewScheduler(c, SchedulerOptions{Clock: NewManualClock(0)})
	defer s.Close()
	s.SetLease(testSession, 1)
	ctx, _ := timeoutCtx(t, 5*time.Second)
	if err := s.Release(ctx, func(ctx context.Context, cl webkit.Caller) error {
		_, err := cl.Send(ctx, "Test.cleanup", nil)
		return err
	}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := c.received(); !slices.Equal(got, []string{"Test.cleanup"}) {
		t.Fatalf("page connection received %v, want the cleanup", got)
	}
}
