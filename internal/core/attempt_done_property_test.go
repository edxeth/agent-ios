package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"testing/quick"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// Review2 F18 (FIX-F F1, Action.Done): for a random step count and a
// random step from which Done accepts, the action ends after the first
// dispatched step that Done accepts: no later step is sent, the
// acknowledged count is that step, the values are those of the sent steps
// in order, and Done saw each dispatched step's own value, in order, and
// nothing after it accepted one. doneFrom 0 is a Done that never accepts:
// every step runs.
func TestDoneEndsTheActionAtTheAcceptedStepProperty(t *testing.T) {
	prop := func(stepsSeed, doneSeed uint8) bool {
		steps := 1 + int(stepsSeed)%12
		doneFrom := int(doneSeed) % (steps + 2) // 0: never; steps+1: past the end
		var sent []int
		rt := func(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
			if _, err := c.Send(ctx, "Runtime.evaluate", nil); err != nil {
				return nil, err
			}
			if fn == ClockFn {
				return json.RawMessage(`{"now":1,"doc":"d1"}`), nil
			}
			a := args.(AttemptArgs)
			sent = append(sent, a.Step)
			return json.Marshal(AttemptReply{Status: StatusDispatched, Now: 2, Doc: a.Doc, Value: json.RawMessage(fmt.Sprintf(`{"step":%d}`, a.Step))})
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
		accepted := false
		res, err := op.RunSteps(Action{Name: "select", Fn: "act", Steps: steps, Done: func(step int, v json.RawMessage) bool {
			if accepted {
				seen = append(seen, "called after accepting")
			}
			seen = append(seen, fmt.Sprintf(`%d {"step":%d}`, step, step))
			accepted = doneFrom != 0 && step >= doneFrom
			return accepted
		}})
		// last: the step after which the action ends.
		last := steps
		if doneFrom != 0 && doneFrom < steps {
			last = doneFrom
		}
		var wantSent []int
		var wantValues, wantSeen []string
		for i := 1; i <= last; i++ {
			wantSent = append(wantSent, i)
			wantValues = append(wantValues, fmt.Sprintf(`{"step":%d}`, i))
			wantSeen = append(wantSeen, fmt.Sprintf(`%d {"step":%d}`, i, i))
		}
		var values []string
		for _, v := range res.Values {
			values = append(values, string(v))
		}
		// Done may or may not be asked about the last step of the
		// action: nothing follows it.
		if last == steps && len(seen) == len(wantSeen)-1 {
			wantSeen = wantSeen[:len(seen)]
		}
		if err != nil || res.Acked != last || !slices.Equal(sent, wantSent) || !slices.Equal(values, wantValues) || !slices.Equal(seen, wantSeen) {
			t.Logf("steps %d, Done from %d: err %v, acked %d, sent %v, values %v, Done saw %q; want acked %d, sent %v, Done saw %q",
				steps, doneFrom, err, res.Acked, sent, values, seen, last, wantSent, wantSeen)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}
