package core

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/runtime"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// FIX-K: an attempt whose call ran and answered a promise, whose document
// was then destroyed before the promise settled, may have run: the step
// error says so, as before the armed await. An attempt whose reply could
// not be armed ran nothing: the step error says it did not run, and the
// call is not sent.
func TestArmedAttemptOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		armFails   bool
		mayHaveRun bool
		acts       int
		want       error
	}{
		{"the promise's document was destroyed", false, true, 1, runtime.ErrResultLost},
		{"the reply could not be armed", true, false, 0, runtime.ErrNothingRan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, SchedulerOptions{Runtime: runtime.New(nil, nil).RuntimeCaller()})
			acts := 0
			h.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
				var p struct {
					Expression string `json:"expression"`
				}
				_ = json.Unmarshal(c.Params, &p)
				if res, ok := fakewebkit.ArmedPromise(p.Expression); ok {
					if tc.armFails {
						return map[string]any{"result": map[string]any{"type": "object", "description": "TypeError"}, "wasThrown": true}, nil
					}
					return res, nil
				}
				m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression)
				if m == nil {
					t.Errorf("unexpected expression %.80s", p.Expression)
					return map[string]any{"result": map[string]any{"type": "undefined"}}, nil
				}
				if m[1] == "act" {
					acts++
					return map[string]any{"result": map[string]any{"type": "string", "value": runtime.PendingSentinel}}, nil
				}
				v := h.rt.Handler()(c, m[1], json.RawMessage(m[2]))
				return map[string]any{"result": map[string]any{"type": "object", "value": v}}, nil
			})
			h.f.Handle("Runtime.awaitPromise", func(fakewebkit.Call) (any, *fakewebkit.Error) {
				return nil, &fakewebkit.Error{Code: -32000, Message: "Missing injected script for given promiseObjectId"}
			})
			ctx, _ := timeoutCtx(t, 5*time.Second)
			op := h.begin(t, ctx, ClassMutate)
			_, err := op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1})
			op.End()
			var se *StepError
			if !errors.As(err, &se) || se.MayHaveRun != tc.mayHaveRun || se.Step != 1 || !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want a StepError for step 1 with MayHaveRun %v wrapping %v", err, tc.mayHaveRun, tc.want)
			}
			if acts != tc.acts {
				t.Fatalf("act sent %d times, want %d", acts, tc.acts)
			}
		})
	}
}
