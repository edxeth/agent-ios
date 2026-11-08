package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// W3b, design 3.5: an action whose checks never pass fails before
// dispatch and names the reason of the page's last retry ("covered by
// <div class=modal>"), so the agent sees what blocks it. The step was
// never dispatched, so the error is not ambiguous.
func TestStopBeforeDispatchNamesTheLastRetryReason(t *testing.T) {
	attempts := 0
	rt := func(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
		if fn == ClockFn {
			return json.RawMessage(`{"now":1,"doc":"d1"}`), nil
		}
		a, ok := args.(AttemptArgs)
		if !ok {
			return json.RawMessage("null"), nil // __iwdp.cancel at the deadline
		}
		attempts++
		reason := "waiting for the element to be stable"
		if attempts > 1 {
			reason = "covered by <div class=\"modal\">"
		}
		return json.Marshal(AttemptReply{Status: StatusRetry, Reason: reason, Now: 2, Doc: a.Doc})
	}
	s := NewScheduler(nopCaller{}, SchedulerOptions{Runtime: rt})
	defer s.Close()
	s.SetLease(testSession, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	op, err := s.Begin(ctx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer op.End()
	_, err = op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1})
	var se *StepError
	if !errors.As(err, &se) || se.MayHaveRun || se.Acked != 0 {
		t.Fatalf("err = %v, want a StepError before dispatch", err)
	}
	if attempts < 2 || !strings.Contains(err.Error(), "covered by <div class=\"modal\">") {
		t.Fatalf("after %d attempts err = %q, want it to name the last retry reason", attempts, err)
	}
}

// W3b race fix (design 3.5: the action timeout lies inside the command
// deadline): a slow page answers each attempt "covered" after 150 ms. With
// a Reserve the attempts end before the operation deadline, so the last
// reply arrives in time and the action fails before dispatch, naming the
// cover. Without it the deadline at 400 ms falls while the third attempt
// is in flight (sent at about 340 ms), and the outcome is unknown.
func TestReserveEndsAttemptsBeforeTheDeadline(t *testing.T) {
	run := func(reserve time.Duration) (time.Duration, error) {
		rt := func(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
			if fn == ClockFn {
				return json.RawMessage(`{"now":1,"doc":"d1"}`), nil
			}
			a, ok := args.(AttemptArgs)
			if !ok {
				return json.RawMessage("null"), nil // __iwdp.cancel at the deadline
			}
			// The attempt goes on the wire (it commits), then the page
			// answers after 150 ms.
			if _, err := c.Send(ctx, "Runtime.evaluate", nil); err != nil {
				return nil, err
			}
			time.Sleep(150 * time.Millisecond)
			return json.Marshal(AttemptReply{Status: StatusRetry, Reason: "covered by <div class=\"modal\">", Now: 2, Doc: a.Doc})
		}
		s := NewScheduler(nopCaller{}, SchedulerOptions{Runtime: rt})
		defer s.Close()
		s.SetLease(testSession, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		op, err := s.Begin(ctx, OpSpec{Class: ClassMutate, Session: testSession, Epoch: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer op.End()
		start := time.Now()
		_, err = op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1, Reserve: reserve})
		return time.Since(start), err
	}
	took, err := run(250 * time.Millisecond)
	var se *StepError
	if !errors.As(err, &se) || se.MayHaveRun || !errors.Is(err, ErrActionTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("with a reserve: err = %v, want ErrActionTimeout before dispatch", err)
	}
	if !strings.Contains(err.Error(), "covered by <div class=\"modal\">") || took > 400*time.Millisecond {
		t.Fatalf("with a reserve: err = %q after %s, want the cover named inside the deadline", err, took)
	}
	// Without a reserve the same page gives the ambiguous outcome of design
	// 2.6 item 5: this is the case the reserve exists for.
	if _, err := run(0); !errors.As(err, &se) || !se.MayHaveRun {
		t.Fatalf("without a reserve: err = %v, want an attempt in flight at the deadline", err)
	}
}
