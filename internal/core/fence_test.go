package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
)

var errClientGone = errors.New("client went away")

// interferingRuntime runs interfere just before the attempt function fn is
// handed to the real runtime call, that is, between the daemon's
// preparation of an attempt and its send.
func interferingRuntime(fn string, interfere func()) RuntimeCaller {
	return func(ctx context.Context, c webkit.Caller, f string, args any) (json.RawMessage, error) {
		if f == fn {
			interfere()
		}
		return CallRuntime(ctx, c, f, args)
	}
}

// requireNotSent checks that RunSteps stopped before step 1 without
// sending it: the error is not ambiguous and the page received no attempt.
func requireNotSent(t *testing.T, h *harness, err error, want error) {
	t.Helper()
	var se *StepError
	if !errors.As(err, &se) || !errors.Is(err, want) {
		t.Fatalf("err = %v, want a StepError wrapping %v", err, want)
	}
	if se.MayHaveRun {
		t.Fatalf("err = %v: an attempt that was never sent is reported as possibly run", err)
	}
	time.Sleep(100 * time.Millisecond) // a send in flight would reach the fake by now
	if n := attemptFn(h); n != 0 {
		t.Fatalf("fake received %d attempts after the operation stopped being active", n)
	}
	if d := h.rt.Dispatched(); len(d) != 0 {
		t.Fatalf("page dispatched %+v", d)
	}
}

// Design 2.6, cancellation rule 3: an attempt is sent only while the
// operation is active. A cancel or a lease change between the preparation
// of an attempt and its send stops it before anything reaches the page.
func TestAttemptNotSentAfterCancelOrEpochChangeBeforeSend(t *testing.T) {
	for _, tc := range []struct {
		name      string
		interfere func(h *harness, op *Op)
		want      error
	}{
		{"cancel", func(_ *harness, op *Op) { op.Cancel(errClientGone) }, errClientGone},
		{"lease epoch", func(h *harness, _ *Op) { h.s.SetLease(testSession, 2) }, ErrStaleEpoch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h *harness
			var op *Op
			h = newHarness(t, SchedulerOptions{Runtime: interferingRuntime("act", func() { tc.interfere(h, op) })})
			ctx, _ := timeoutCtx(t, 5*time.Second)
			op = h.begin(t, ctx, ClassMutate)
			_, err := op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1})
			op.End()
			requireNotSent(t, h, err, tc.want)
		})
	}
}

// Design 2.6, clock bounds: the mapping is measured per document, and an
// attempt addressed to another document does nothing. A new generation
// seen by the daemon between the measurement and the send stops the
// attempt before it is sent.
func TestAttemptNotSentAfterDocumentSwapBeforeSend(t *testing.T) {
	var gen atomic.Uint64
	gen.Store(1)
	h := newHarness(t, SchedulerOptions{Generation: gen.Load, Runtime: interferingRuntime("act", func() { gen.Add(1) })})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	op := h.begin(t, ctx, ClassMutate)
	_, err := op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1})
	op.End()
	requireNotSent(t, h, err, ErrDocumentChanged)
	if !strings.Contains(err.Error(), "another document") {
		t.Fatalf("err = %q, want it to name the document change", err)
	}
}

// Design 2.6, clock bounds: a same-process navigation that the daemon has
// not seen yet still cannot run an attempt in the new document: the page
// refuses an attempt carrying another document's id, the action stops
// without a possible run, and the next action measures the new document.
func TestAttemptForOldDocumentIsRefusedByPage(t *testing.T) {
	var h *harness
	navigated := false
	h = newHarness(t, SchedulerOptions{Runtime: interferingRuntime("act", func() {
		if !navigated {
			navigated = true
			h.rt.Navigate()
		}
	})})
	ctx, _ := timeoutCtx(t, 5*time.Second)
	op := h.begin(t, ctx, ClassMutate)
	_, err := op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1})
	op.End()
	var se *StepError
	if !errors.As(err, &se) || !errors.Is(err, ErrDocumentChanged) || se.MayHaveRun {
		t.Fatalf("err = %v, want a StepError for a document change that did not run", err)
	}
	if d := h.rt.Dispatched(); len(d) != 0 {
		t.Fatalf("page dispatched %+v in the new document", d)
	}
	op = h.begin(t, ctx, ClassMutate)
	defer op.End()
	if res, err := op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1}); err != nil || res.Acked != 1 {
		t.Fatalf("next action = %+v, %v; want step 1 acknowledged", res, err)
	}
	if n := len(h.rt.CallsOf(ClockFn)); n != 2 {
		t.Fatalf("clock measurements = %d, want 2 (the new document is measured)", n)
	}
	if d := h.rt.Dispatched(); len(d) != 1 {
		t.Fatalf("dispatches = %+v, want one in the new document", d)
	}
}

type setupCaller struct {
	nopCaller
	err error
}

func (c setupCaller) ActiveSetup(context.Context) error { return c.err }

// Design 2.7: a page whose provisional setup failed lacks required state
// (bootstrap parts, interception, overrides). Admission refuses operations
// with the setup error until the desired state is applied again.
func TestBeginRefusesWhileActiveSetupFailed(t *testing.T) {
	failed := errors.New("Page.setBootstrapScript: 'Page.setBootstrapScript' was not found")
	for _, tc := range []struct {
		name string
		err  error
	}{{"failed", failed}, {"ok", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewScheduler(setupCaller{err: tc.err}, SchedulerOptions{Clock: NewManualClock(0)})
			defer s.Close()
			s.SetLease(testSession, 1)
			op, err := s.Begin(context.Background(), OpSpec{Name: "click", Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: time.Hour})
			if tc.err == nil {
				if err != nil {
					t.Fatalf("Begin: %v", err)
				}
				op.End()
				return
			}
			if !errors.Is(err, failed) || !strings.Contains(err.Error(), "operation click") {
				t.Fatalf("Begin err = %v, want the setup failure", err)
			}
		})
	}
}

// Design 2.6, cancellation rule 4: a step that its attempt end interrupts
// after some of its events is reported as interrupted and stops the
// sequence; no later step is sent.
func TestInterruptedStepStopsSequence(t *testing.T) {
	var steps []int
	rt := func(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
		if _, err := c.Send(ctx, "Runtime.evaluate", nil); err != nil {
			return nil, err
		}
		if fn == ClockFn {
			return json.RawMessage(`{"now":1,"doc":"d1"}`), nil
		}
		a := args.(AttemptArgs)
		steps = append(steps, a.Step)
		status := StatusDispatched
		if a.Step == 2 {
			status = StatusInterrupted
		}
		return json.Marshal(AttemptReply{Status: status, Reason: "attempt end", Now: 2, Doc: a.Doc})
	}
	s := NewScheduler(nopCaller{}, SchedulerOptions{Clock: NewManualClock(0), Runtime: rt})
	defer s.Close()
	s.SetLease(testSession, 1)
	op, err := s.Begin(context.Background(), OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer op.End()
	res, err := op.RunSteps(Action{Name: "type", Fn: "act", Steps: 3})
	var se *StepError
	if !errors.As(err, &se) || !se.Interrupted || se.Step != 2 || se.MayHaveRun {
		t.Fatalf("err = %v, want step 2 reported as interrupted", err)
	}
	if want := "step 2 of 3 of type was interrupted after some of its events (1 acknowledged): attempt end"; err.Error() != want {
		t.Fatalf("message = %q, want %q", err, want)
	}
	if res.Acked != 1 || len(steps) != 2 {
		t.Fatalf("acked %d, attempts for steps %v; want step 1 acknowledged and no attempt for step 3", res.Acked, steps)
	}
}

// FIX-F F1: Action.Done ends the action after a dispatched step whose
// value it accepts; no later step is sent (select on a native <select>).
func TestDoneEndsTheActionEarly(t *testing.T) {
	var steps []int
	rt := func(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
		if _, err := c.Send(ctx, "Runtime.evaluate", nil); err != nil {
			return nil, err
		}
		if fn == ClockFn {
			return json.RawMessage(`{"now":1,"doc":"d1"}`), nil
		}
		a := args.(AttemptArgs)
		steps = append(steps, a.Step)
		return json.Marshal(AttemptReply{Status: StatusDispatched, Now: 2, Doc: a.Doc, Value: json.RawMessage(`{"fin":true}`)})
	}
	s := NewScheduler(nopCaller{}, SchedulerOptions{Clock: NewManualClock(0), Runtime: rt})
	defer s.Close()
	s.SetLease(testSession, 1)
	op, err := s.Begin(context.Background(), OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1, Deadline: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer op.End()
	var seen []string
	res, err := op.RunSteps(Action{Name: "select", Fn: "act", Steps: 2, Done: func(step int, v json.RawMessage) bool {
		seen = append(seen, fmt.Sprintf("%d %s", step, v))
		return true
	}})
	if err != nil || res.Acked != 1 || len(res.Values) != 1 || len(steps) != 1 || len(seen) != 1 || seen[0] != `1 {"fin":true}` {
		t.Fatalf("err %v, acked %d, values %d, attempts for steps %v, Done saw %v; want step 1 only", err, res.Acked, len(res.Values), steps, seen)
	}
}
