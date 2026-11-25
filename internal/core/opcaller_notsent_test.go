package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// capacityWait is a caller context that reports, through reached, that
// opCaller.Send evaluated Done in its wait for protocol capacity: the send
// passed the check before the wait. With hideDone, Done never fires, so
// only the capacity or the operation's own end can finish the wait; this
// fixes which case of the wait's select is ready.
type capacityWait struct {
	context.Context
	reached  chan struct{}
	once     *sync.Once
	hideDone bool
}

func newCapacityWait(ctx context.Context, hideDone bool) capacityWait {
	return capacityWait{Context: ctx, reached: make(chan struct{}), once: &sync.Once{}, hideDone: hideDone}
}

func (c capacityWait) Done() <-chan struct{} {
	c.once.Do(func() { close(c.reached) })
	if c.hideDone {
		return nil
	}
	return c.Context.Done()
}

// beginNotSentOp begins an operation of testSession at epoch 1 on a
// scheduler with one unit of protocol capacity, on a manual clock so that
// the operation's deadline never fires during the test.
func beginNotSentOp(t *testing.T) (*swapCaller, *Scheduler, *Op) {
	t.Helper()
	c := newSwapCaller()
	s := NewScheduler(c, SchedulerOptions{Clock: NewManualClock(0), MaxInFlight: 1})
	t.Cleanup(s.Close)
	s.SetLease(testSession, 1)
	op, err := s.Begin(context.Background(), OpSpec{Name: "t", Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(op.End)
	return c, s, op
}

// checkNotSent checks the contract of a refusal before the write (webkit
// ErrNotSent, session.go): err wraps webkit.ErrNotSent and cause, its text
// is "<method>: not sent: <cause>" as before the marking, and the page
// connection received nothing.
func checkNotSent(t *testing.T, c *swapCaller, err error, method string, cause error) {
	t.Helper()
	want := method + ": not sent: " + cause.Error()
	if err == nil || !errors.Is(err, webkit.ErrNotSent) || !errors.Is(err, cause) || err.Error() != want {
		t.Fatalf("send: %v; want an error that wraps webkit.ErrNotSent and %v, with the text %q", err, cause, want)
	}
	if got := c.received(); len(got) != 0 {
		t.Fatalf("the page connection received %v", got)
	}
}

// An operation's caller refuses a call before the wait for capacity when
// the operation is not active (ended, or its lease epoch changed). The
// call never reaches the page, so the refusal wraps webkit.ErrNotSent.
func TestOperationCallerRefusalBeforeTheWaitIsNotSent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(s *Scheduler, op *Op)
		cause  error
	}{
		{"the operation ended", func(_ *Scheduler, op *Op) { op.End() }, ErrOpEnded},
		{"the lease moved to another epoch", func(s *Scheduler, _ *Op) { s.SetLease(testSession, 2) }, ErrStaleEpoch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s, op := beginNotSentOp(t)
			tc.change(s, op)
			_, err := op.Caller().Send(context.Background(), "Runtime.evaluate", nil)
			checkNotSent(t, c, err, "Runtime.evaluate", tc.cause)
		})
	}
}

// An operation's caller that waits for protocol capacity sends nothing
// when its own context ends during the wait, when the operation ends
// during the wait, or when the lease moved to another epoch by the time
// the capacity is free. Each refusal wraps webkit.ErrNotSent.
func TestOperationCallerRefusalAroundTheCapacityWaitIsNotSent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hideDone bool // the caller's context cannot end the wait
		change   func(s *Scheduler, op *Op, cancel context.CancelFunc)
		cause    error
	}{
		{"the caller's context ends during the wait", false, func(_ *Scheduler, _ *Op, cancel context.CancelFunc) { cancel() }, context.Canceled},
		{"the operation ends during the wait", true, func(_ *Scheduler, op *Op, _ context.CancelFunc) { op.End() }, ErrOpEnded},
		{"the lease moves while the capacity is in use", true, func(s *Scheduler, _ *Op, _ context.CancelFunc) {
			s.SetLease(testSession, 2)
			<-s.sem // the capacity is free; the operation is not active any more
		}, ErrStaleEpoch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s, op := beginNotSentOp(t)
			s.sem <- struct{}{} // all protocol capacity is in use
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wait := newCapacityWait(ctx, tc.hideDone)
			result := make(chan error, 1)
			go func() {
				_, err := op.Caller().Send(wait, "Runtime.evaluate", nil)
				result <- err
			}()
			<-wait.reached // the send passed the first check and waits for capacity
			tc.change(s, op, cancel)
			var err error
			select {
			case err = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("the send did not finish")
			}
			checkNotSent(t, c, err, "Runtime.evaluate", tc.cause)
		})
	}
}

// Design 2.7: an operation's caller sends nothing to an active target whose
// setup failed after the admission. The refusal wraps webkit.ErrNotSent.
func TestOperationCallerSetupRefusalIsNotSent(t *testing.T) {
	c, _, op := beginNotSentOp(t)
	c.fail(errSwapSetup)
	_, err := op.Caller().Send(context.Background(), "Test.x", nil)
	checkNotSent(t, c, err, "Test.x", errSwapSetup)
}
