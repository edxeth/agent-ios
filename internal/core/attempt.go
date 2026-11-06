package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// ClockFn is the runtime function that returns the page clock and the
// document identity: {"now": performance.now() in milliseconds, "doc": id}.
// The runtime picks a random doc id at install, so every document has its
// own (design 2.6: an attempt addressed to another document does nothing).
const ClockFn = "clock"

// Attempt reply statuses (the page side of design 2.6 and 3.5).
const (
	StatusDispatched  = "dispatched"  // every check passed; the step's side effects ran
	StatusRetry       = "retry"       // a check failed; nothing changed
	StatusExpired     = "expired"     // the attempt end or the deadline had passed; nothing changed
	StatusInterrupted = "interrupted" // the attempt end stopped the step after some of its events
	StatusRefused     = "refused"     // canceled operation, other epoch or not the next step; nothing changed
)

// Errors of the attempt loop.
var (
	ErrOutcomeUnknown = errors.New("canceled; outcome unknown")
	ErrReplyLost      = errors.New("the reply of the attempt was lost")
	ErrClockUncertain = errors.New("the page is too slow to fence actions safely")
	// ErrDocumentChanged: the page moved to another document between the
	// clock measurement and the attempt; nothing was sent to the new one.
	ErrDocumentChanged = errors.New("the page navigated to another document before the attempt; nothing ran in the new document")
)

// Action is a side-effecting action as an ordered list of steps (design 2.6).
type Action struct {
	Name  string             // for messages, for example "type"
	Fn    string             // runtime function that runs one attempt
	Steps int                // number of steps; at least 1
	Delay time.Duration      // pause between steps (type --delay)
	Args  func(step int) any // per-step arguments (nil: none)
}

// AttemptArgs fence one attempt on the page (design 3.1). AttemptEnd and
// Deadline are on the document's performance.now() clock, in milliseconds.
// Doc is the document id of that clock; the page refuses an attempt for
// another document.
type AttemptArgs struct {
	OpID       string  `json:"opId"`
	Epoch      uint64  `json:"epoch"`
	Doc        string  `json:"doc"`
	Step       int     `json:"step"`
	Steps      int     `json:"steps"`
	AttemptEnd float64 `json:"attemptEnd"`
	Deadline   float64 `json:"deadline"`
	Args       any     `json:"args,omitempty"`
}

// AttemptReply is the value of an attempt's result envelope.
type AttemptReply struct {
	Status string          `json:"status"`
	Reason string          `json:"reason,omitempty"`
	Now    float64         `json:"now"` // performance.now() at reply
	Doc    string          `json:"doc"` // the answering document's id
	Value  json.RawMessage `json:"value,omitempty"`
}

// StepsResult lists the acknowledged steps.
type StepsResult struct {
	Acked  int
	Values []json.RawMessage
}

// StepError says where a multi-step action stopped and whether the page may
// have run the step in question.
type StepError struct {
	Action      string
	Step, Steps int
	Acked       int
	MayHaveRun  bool // the step was in flight; its outcome is unknown
	Interrupted bool // the page ran part of the step's events
	Reason      string
	Err         error
}

func (e *StepError) Error() string {
	var msg string
	switch {
	case e.Interrupted:
		msg = fmt.Sprintf("step %d of %d of %s was interrupted after some of its events", e.Step, e.Steps, e.Action)
	case errors.Is(e.Err, ErrOutcomeUnknown):
		msg = fmt.Sprintf("canceled; outcome unknown: the page may have run step %d of %d of %s", e.Step, e.Steps, e.Action)
	case e.MayHaveRun:
		msg = fmt.Sprintf("the page may have run step %d of %d of %s before the reply was lost", e.Step, e.Steps, e.Action)
	default:
		msg = fmt.Sprintf("%s stopped before step %d of %d", e.Action, e.Step, e.Steps)
	}
	msg += fmt.Sprintf(" (%d acknowledged)", e.Acked)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	if e.Err != nil && !errors.Is(e.Err, ErrOutcomeUnknown) {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *StepError) Unwrap() error { return e.Err }

// ClockUncertaintyError is returned when the clock mapping stays too wide.
type ClockUncertaintyError struct{ Uncertainty time.Duration }

func (e *ClockUncertaintyError) Error() string {
	return fmt.Sprintf("page clock uncertainty is %d ms; the page is too slow to fence actions safely", e.Uncertainty.Milliseconds())
}

func (e *ClockUncertaintyError) Unwrap() error { return ErrClockUncertain }

func toMS(d time.Duration) float64 { return float64(d) / 1e6 }

func fromMS(ms float64) time.Duration { return time.Duration(math.Round(ms * 1e6)) }

// measureClock adds one clock sample of document generation gen to cm
// through a runtime call. A sample is kept only when it comes from the
// map's document: a new generation, or another document id, during the
// measurement gives ErrDocumentChanged.
func (op *Op) measureClock(cm *ClockMap, gen uint64) error {
	s := op.s
	h0 := s.o.Clock.Now()
	raw, err := s.o.Runtime(op.ctx, op.Caller(), ClockFn, nil)
	h1 := s.o.Clock.Now()
	if err != nil {
		return fmt.Errorf("measuring the page clock: %w", err)
	}
	var v struct {
		Now *float64 `json:"now"`
		Doc string   `json:"doc"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Now == nil || v.Doc == "" {
		return fmt.Errorf("measuring the page clock: reply %s has no now or doc", raw)
	}
	if s.o.Generation() != gen {
		return ErrDocumentChanged
	}
	if !cm.BindDoc(v.Doc) {
		s.dropClock(gen, cm)
		return ErrDocumentChanged
	}
	cm.Observe(ClockSample{H0: h0, H1: h1, P: fromMS(*v.Now)})
	return nil
}

// RunSteps runs a in the daemon-owned attempt loop of design 2.6. Each
// attempt is one runtime call for the current step that dispatches only
// when every readiness check passes. Attempts are sent only while the
// operation is active and the page shows the document whose clock mapping
// fenced them; the loop advances to the next step only after the reply that
// acknowledges the current one; a missing reply, a cancel, the deadline or a
// document change stops the sequence, and the ambiguous step is never sent
// again.
func (op *Op) RunSteps(a Action) (StepsResult, error) {
	op.pageWork.Store(true)
	s := op.s
	var res StepsResult
	stop := func(step int, err error) (StepsResult, error) {
		return res, &StepError{Action: a.Name, Step: step, Steps: a.Steps, Acked: res.Acked, Err: err}
	}
	ambiguous := func(step int, err error) (StepsResult, error) {
		return res, &StepError{Action: a.Name, Step: step, Steps: a.Steps, Acked: res.Acked, MayHaveRun: true, Err: err}
	}
	for step := 1; step <= a.Steps; step++ {
		if step > 1 && a.Delay > 0 {
			select {
			case <-s.o.Clock.After(a.Delay):
			case <-op.ctx.Done():
				return stop(step, context.Cause(op.ctx))
			}
		}
		for {
			if err := op.active(); err != nil {
				return stop(step, err)
			}
			gen := s.o.Generation()
			cm := s.clockMap(gen)
			if !cm.Measured() {
				if err := op.measureClock(cm, gen); err != nil {
					return stop(step, err)
				}
			}
			sent := s.o.Clock.Now()
			boundary := min(sent+s.o.AttemptBudget, op.deadline)
			if u := cm.Uncertainty(boundary); u > s.o.MaxUncertainty {
				if err := op.measureClock(cm, gen); err != nil {
					return stop(step, err)
				}
				sent = s.o.Clock.Now()
				boundary = min(sent+s.o.AttemptBudget, op.deadline)
				if u := cm.Uncertainty(boundary); u > s.o.MaxUncertainty {
					return stop(step, &ClockUncertaintyError{Uncertainty: u})
				}
			}
			var stepArgs any
			if a.Args != nil {
				stepArgs = a.Args(step)
			}
			args := AttemptArgs{
				OpID: op.ID, Epoch: op.Spec.Epoch, Doc: cm.Doc(), Step: step, Steps: a.Steps,
				AttemptEnd: toMS(cm.PageMin(boundary)), Deadline: toMS(cm.PageMin(op.deadline)),
				Args: stepArgs,
			}
			inf := &inflightAttempt{boundary: boundary, replied: make(chan struct{}), gen: gen}
			op.mu.Lock()
			op.inflight = inf
			op.mu.Unlock()
			ev := AttemptEvent{OpID: op.ID, Step: step, SentAt: sent, Args: args}
			caller := attemptCaller{op: op, inf: inf, commit: func() {
				if s.o.OnAttempt != nil {
					s.o.OnAttempt(ev)
				}
			}}
			type result struct {
				raw json.RawMessage
				err error
			}
			done := make(chan result, 1)
			// Every send of the call passes the operation's fence (caller);
			// the reply wait is bound to the boundary plus the reply grace,
			// not to the operation's context.
			callCtx, cancelCall := context.WithTimeout(context.Background(), boundary-sent+s.o.ReplyGrace)
			go func() {
				defer cancelCall()
				raw, err := s.o.Runtime(callCtx, caller, a.Fn, args)
				done <- result{raw, err}
				close(inf.replied)
			}()
			lost := s.o.Clock.After(boundary + s.o.ReplyGrace - sent)
			var r result
			select {
			case r = <-done:
			case <-op.ctx.Done():
				if !inf.close() {
					return stop(step, context.Cause(op.ctx)) // nothing was sent, and nothing will be
				}
				// The slot stays held until the reply or the boundary (End).
				return ambiguous(step, ErrOutcomeUnknown)
			case <-lost:
				if !inf.close() {
					return stop(step, ErrReplyLost)
				}
				return ambiguous(step, ErrReplyLost)
			}
			op.mu.Lock()
			op.inflight = nil
			op.mu.Unlock()
			replyAt := s.o.Clock.Now()
			if r.err != nil {
				if !inf.isCommitted() || errors.Is(r.err, webkit.ErrNotSent) {
					return stop(step, r.err)
				}
				return ambiguous(step, r.err)
			}
			var rep AttemptReply
			if err := json.Unmarshal(r.raw, &rep); err != nil {
				return ambiguous(step, fmt.Errorf("decoding attempt reply: %w", err))
			}
			if rep.Doc != args.Doc {
				// The page refuses an attempt for another document, so the
				// step did not run; that document needs a new mapping.
				s.dropClock(gen, cm)
				return stop(step, ErrDocumentChanged)
			}
			if rep.Now > 0 {
				cm.Observe(ClockSample{H0: sent, H1: replyAt, P: fromMS(rep.Now)})
			}
			switch rep.Status {
			case StatusDispatched:
				res.Acked = step
				res.Values = append(res.Values, rep.Value)
			case StatusRetry, StatusExpired:
				select {
				case <-s.o.Clock.After(s.o.RetryBackoff):
				case <-op.ctx.Done():
					return stop(step, context.Cause(op.ctx))
				}
				continue
			case StatusInterrupted:
				return res, &StepError{Action: a.Name, Step: step, Steps: a.Steps, Acked: res.Acked, Interrupted: true, Reason: rep.Reason}
			default:
				return res, &StepError{Action: a.Name, Step: step, Steps: a.Steps, Acked: res.Acked, Reason: fmt.Sprintf("page answered %q %s", rep.Status, rep.Reason)}
			}
			break
		}
	}
	return res, nil
}
