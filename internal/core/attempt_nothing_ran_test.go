package core

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/runtime"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// A step whose runtime call the page answered with the missing sentinel
// ran nothing in the page, also when the install that follows fails or
// the retry answers the sentinel again (a navigation to a new document
// between the install and the retry): the step error says the step did
// not run, not that it may have run (review-split F1).
func TestMissingRuntimeAfterInstallIsAStepThatDidNotRun(t *testing.T) {
	for _, tc := range []struct {
		name         string
		installThrow bool
	}{
		{"the install fails", true},
		{"the retry answers missing again", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, SchedulerOptions{})
			acts, installs := 0, 0
			h.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
				var p struct {
					Expression string `json:"expression"`
				}
				_ = json.Unmarshal(c.Params, &p)
				m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression)
				switch {
				case m == nil: // an install of the runtime
					installs++
					if tc.installThrow {
						return map[string]any{"result": map[string]any{"type": "object", "description": "SyntaxError"}, "wasThrown": true}, nil
					}
					return map[string]any{"result": map[string]any{"type": "undefined"}}, nil
				case m[1] == "act":
					acts++
					return map[string]any{"result": map[string]any{"type": "string", "value": runtime.MissingSentinel}}, nil
				default:
					v := h.rt.Handler()(c, m[1], json.RawMessage(m[2]))
					return map[string]any{"result": map[string]any{"type": "object", "value": v}}, nil
				}
			})
			ctx, _ := timeoutCtx(t, 5*time.Second)
			op := h.begin(t, ctx, ClassMutate)
			_, err := op.RunSteps(Action{Name: "fill", Fn: "act", Steps: 1})
			op.End()
			var se *StepError
			if !errors.As(err, &se) || se.MayHaveRun || se.Step != 1 {
				t.Fatalf("err = %v, want a StepError for step 1 that did not run", err)
			}
			if !errors.Is(err, runtime.ErrNothingRan) {
				t.Fatalf("err = %v, want runtime.ErrNothingRan", err)
			}
			wantActs := 2
			if tc.installThrow {
				wantActs = 1
			}
			if acts != wantActs || installs != 1 {
				t.Fatalf("act calls %d, installs %d; want %d and 1", acts, installs, wantActs)
			}
		})
	}
}
