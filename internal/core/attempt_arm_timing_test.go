package core

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/runtime"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// armedPage answers the runtime calls of an armed attempt (FIX-K): onArm
// runs when the arm reaches the page, before its reply; the call of a
// runtime function goes to onCall when it is set and answers, else to the
// fake page runtime. It returns the counter of the calls of each function.
func armedPage(t *testing.T, h *harness, onArm func(), onCall func(c fakewebkit.Call, fn string, args json.RawMessage) (any, bool)) func(fn string) int {
	var mu sync.Mutex
	calls := map[string]int{}
	h.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if res, ok := fakewebkit.ArmedPromise(p.Expression); ok {
			if onArm != nil {
				onArm()
			}
			return res, nil
		}
		m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression)
		if m == nil {
			return map[string]any{"result": map[string]any{"type": "undefined"}}, nil
		}
		mu.Lock()
		calls[m[1]]++
		mu.Unlock()
		v, ok := any(nil), false
		if onCall != nil {
			v, ok = onCall(c, m[1], json.RawMessage(m[2]))
		}
		if !ok {
			v = h.rt.Handler()(c, m[1], json.RawMessage(m[2]))
		}
		return map[string]any{"result": map[string]any{"type": "object", "value": v}}, nil
	})
	return func(fn string) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[fn]
	}
}

// FIX-M F3 (design 2.6 rule 3): a cancel that comes after the arm reached
// the page and before the call is sent: the call is not sent, and the
// step error says that the step did not run, not "outcome unknown".
func TestCancelBetweenArmAndCallIsNotRun(t *testing.T) {
	h := newHarness(t, SchedulerOptions{Runtime: runtime.New(nil, nil).RuntimeCaller()})
	var op *Op
	calls := armedPage(t, h, func() { op.Cancel(errors.New("client went away")) }, nil)
	h.f.Stall("Runtime.awaitPromise")
	op = h.begin(t, testDeadlineCtx(t), ClassMutate)
	_, err := op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1})
	op.End()
	if calls("act") != 0 {
		t.Fatalf("the act call was sent %d times after the cancel", calls("act"))
	}
	var se *StepError
	if !errors.As(err, &se) || se.MayHaveRun || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("err = %v, want a StepError that says step 1 did not run", err)
	}
	if sends, _ := h.attempts(); len(sends) != 0 {
		t.Fatalf("OnAttempt ran %d times, want none: no attempt call was sent", len(sends))
	}
}

// FIX-M F3: the page moves to a newer document between the arm and the
// call. The daemon sees it first (a new generation): the call is refused
// before its write. Or the page sees it first: the call reaches the new
// document, whose runtime refuses an attempt for another document. Either
// way the step did not run and the error says the document changed.
func TestDocumentChangeBetweenArmAndCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		daemon  bool // the daemon's generation moves; else only the page's document
		wantAct int
	}{
		{"the daemon sees it first", true, 0},
		{"the page sees it first", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gen atomic.Uint64
			h := newHarness(t, SchedulerOptions{Generation: gen.Load, Runtime: runtime.New(nil, nil).RuntimeCaller()})
			armed := 0
			calls := armedPage(t, h, func() {
				if armed++; armed == 1 {
					if tc.daemon {
						gen.Add(1)
					}
					h.rt.Navigate()
				}
			}, nil)
			op := h.begin(t, testDeadlineCtx(t), ClassMutate)
			_, err := op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1})
			op.End()
			var se *StepError
			if !errors.As(err, &se) || se.MayHaveRun || !errors.Is(err, ErrDocumentChanged) {
				t.Fatalf("err = %v, want a StepError that says step 1 did not run because the document changed", err)
			}
			if calls("act") != tc.wantAct {
				t.Fatalf("act sent %d times, want %d", calls("act"), tc.wantAct)
			}
			if d := h.rt.Dispatched(); len(d) != 0 {
				t.Fatalf("the page dispatched %v", d)
			}
		})
	}
}

// FIX-M F3: the attempt window starts at the call, not at the arm: an arm
// whose reply takes longer than the whole attempt window (a held reply,
// iOS 26's nudge) leaves the call its full window, and the step dispatches
// at once. Before, every attempt's call reached the page after its
// attempt end and expired.
func TestSlowArmTakesNothingFromTheAttemptWindow(t *testing.T) {
	h := newHarness(t, SchedulerOptions{Runtime: runtime.New(nil, nil).RuntimeCaller()})
	calls := armedPage(t, h, func() { time.Sleep(2 * DefaultAttemptBudget) }, nil)
	op := h.begin(t, testDeadlineCtx(t), ClassMutate)
	res, err := op.RunSteps(Action{Name: "click", Fn: "act", Steps: 1})
	op.End()
	if err != nil || res.Acked != 1 {
		t.Fatalf("RunSteps = %+v, %v; want step 1 acknowledged", res, err)
	}
	if calls("act") != 1 {
		t.Fatalf("act sent %d times, want 1", calls("act"))
	}
}

// FIX-M F3 (design 3.5): the window of a probed call (click --new-tab,
// sent with emulateUserGesture) is checked again right before the call
// is sent, after its arm: when a slow arm leaves too little time for the
// window, the call is not sent (WebKit would grant the activation on
// receipt although the page refuses the late click).
func TestProbedCallIsNotSentLateAfterASlowArm(t *testing.T) {
	h := newHarness(t, SchedulerOptions{Runtime: runtime.New(nil, nil).RuntimeCaller()})
	const deadline = 4 * DefaultAttemptBudget
	calls := armedPage(t, h, func() { time.Sleep(deadline - DefaultAttemptBudget/2) }, func(c fakewebkit.Call, fn string, _ json.RawMessage) (any, bool) {
		if fn != "actProbe" {
			return nil, false
		}
		var clk struct {
			Value struct {
				Now float64 `json:"now"`
				Doc string  `json:"doc"`
			} `json:"value"`
		}
		raw, _ := json.Marshal(h.rt.Handler()(c, ClockFn, nil))
		_ = json.Unmarshal(raw, &clk)
		return map[string]any{"ok": true, "value": map[string]any{"status": StatusRetry, "reason": ProbeReady, "now": clk.Value.Now, "doc": clk.Value.Doc}}, true
	})
	ctx, _ := timeoutCtx(t, deadline) // the time window under test
	op := h.begin(t, ctx, ClassMutate)
	_, err := op.RunSteps(Action{Name: "click", Fn: "actGesture", Probe: "actProbe", Steps: 1})
	op.End()
	if calls("actGesture") != 0 {
		t.Fatalf("the gesture call was sent %d times after its window no longer fit", calls("actGesture"))
	}
	var pw *ProbedWindowError
	var se *StepError
	if !errors.As(err, &pw) || !errors.As(err, &se) || se.MayHaveRun {
		t.Fatalf("err = %v, want a ProbedWindowError and a step that did not run", err)
	}
}
