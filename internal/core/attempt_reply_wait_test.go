package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A step whose reply misses its wait (the attempt boundary plus
// ReplyGrace) is a lost reply that may have run, also when the runtime
// call's own timeout ends the call before the daemon's lost timer fires:
// that timeout is no operation deadline (exit 124 says the command
// deadline passed, and a click whose handler blocks the page longer than
// the grace showed "timed out after 3.1s (deadline 20s)").
func TestReplyWaitTimeoutIsALostReplyNotADeadline(t *testing.T) {
	// The scheduler clock never advances, so its lost timer never fires
	// and only the call's own reply wait ends the call.
	clock := NewManualClock(time.Second)
	h := newHarness(t, SchedulerOptions{Clock: clock, ReplyGrace: 100 * time.Millisecond})
	h.rt.DropReplyOfStep = 1
	ctx, _ := timeoutCtx(t, 5*time.Second)
	op := h.begin(t, ctx, ClassMutate)
	defer op.End()
	_, err := op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1})
	var se *StepError
	if !errors.As(err, &se) || !se.MayHaveRun || se.Step != 1 {
		t.Fatalf("err = %v, want a StepError for step 1 that may have run", err)
	}
	if !errors.Is(err, ErrReplyLost) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v (is ErrReplyLost %v, is a deadline %v); want a lost reply, not a deadline", err,
			errors.Is(err, ErrReplyLost), errors.Is(err, context.DeadlineExceeded))
	}
}
