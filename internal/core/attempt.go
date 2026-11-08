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
	// ErrActionTimeout: the attempts of an Action with a Reserve ended
	// before the step dispatched; it wraps context.DeadlineExceeded.
	ErrActionTimeout = fmt.Errorf("action timeout: %w", context.DeadlineExceeded)
)

// Action is a side-effecting action as an ordered list of steps (design 2.6).
type Action struct {
	Name  string             // for messages, for example "type"
	Fn    string             // runtime function that runs one attempt
	Steps int                // number of steps; at least 1
	Delay time.Duration      // pause between steps (type --delay)
	Args  func(step int) any // per-step arguments (nil: none)
	// Reserve, when positive, ends the attempts this long before the
	// operation deadline: no attempt is sent, and no attempt window
	// reaches, past that instant, so the reply of the last attempt arrives
	// inside the deadline. A step that never dispatched then fails with
	// ErrActionTimeout and its last retry reason instead of an ambiguous
	// outcome (W3b; design 3.5: the action timeout lies inside the command
	// deadline).
	Reserve time.Duration
	// Probe, when set, is a runtime function that runs an attempt's checks
	// without its effects. Each attempt of Fn is then a probe attempt
	// first, and Fn is sent only after the probe answered retry with
	// ProbeReady, in an attempt window sized from the measured round trip
	// (sendAttempt). It is for an Fn whose call has an effect that WebKit
	// applies when it receives the call, before the page's fence can
	// refuse it: click --new-tab sends Fn with emulateUserGesture, which
	// grants the page a user activation (W3b, design 3.5).
	Probe string
}

// ProbeReady is the retry reason of a probe whose attempt would dispatch
// now (READY in internal/runtime/js/50-actionability.js).
const ProbeReady = "ready to dispatch"

// ProbedWindowError: the time left before the end of an action's attempts
// cannot cover the attempt window of the call after a ready probe
// (Action.Probe), so the call was not sent. It wraps ErrActionTimeout.
type ProbedWindowError struct {
	Fn        string
	Window    time.Duration // the call's attempt window
	RoundTrip time.Duration // the measured round trip it was sized from
	Left      time.Duration // the time left when the call would have been sent
}

func (e *ProbedWindowError) Error() string {
	return fmt.Sprintf("%v left cannot cover the %v attempt window of %s (at least 3 times the measured round trip of %v); it was not sent",
		e.Left.Round(time.Millisecond), e.Window.Round(time.Millisecond), e.Fn, e.RoundTrip.Round(time.Millisecond))
}

func (e *ProbedWindowError) Unwrap() error { return ErrActionTimeout }

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
		s.dropClock(op.clockKey(gen), cm)
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
	// lastReason is why the page last answered retry for the current step;
	// a stop before its dispatch names it (W3b: "covered by <div ...>").
	var lastReason string
	stop := func(step int, err error) (StepsResult, error) {
		return res, &StepError{Action: a.Name, Step: step, Steps: a.Steps, Acked: res.Acked, Reason: lastReason, Err: err}
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
			fn := a.Fn
			if a.Probe != "" {
				fn = a.Probe
			}
			rep, out, err := op.sendAttempt(a, fn, step, false)
			if out == attemptReplied && a.Probe != "" && rep.Status == StatusRetry && rep.Reason == ProbeReady {
				rep, out, err = op.sendAttempt(a, a.Fn, step, true)
			}
			switch out {
			case attemptStopped:
				return stop(step, err)
			case attemptAmbiguous:
				return ambiguous(step, err)
			}
			switch rep.Status {
			case StatusDispatched:
				res.Acked = step
				res.Values = append(res.Values, rep.Value)
				lastReason = ""
			case StatusRetry, StatusExpired:
				if rep.Reason != "" {
					lastReason = rep.Reason
				}
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

// attemptOutcome is how one attempt of sendAttempt ended.
type attemptOutcome int

const (
	attemptReplied   attemptOutcome = iota // the page's reply is in rep
	attemptStopped                         // nothing ran, and nothing will: stop with the error
	attemptAmbiguous                       // the attempt may have run: stop with the error, never send it again
)

// probedWindowFactor sizes the attempt window of a call whose effect the
// engine applies on receipt (Action.Probe): at least this many measured
// round trips, so that a call delivered as slowly as the last measured
// call still arrives well inside its window.
const probedWindowFactor = 3

// sendAttempt sends one attempt of fn for step and waits for its reply
// (design 2.6). The attempt window is the scheduler's AttemptBudget; with
// probed (the call after a ready probe, Action.Probe) it is at least
// probedWindowFactor times the last round trip of the clock mapping, and
// the call is sent only when that whole window lies before the end of
// the attempts (the deadline, less Action.Reserve). A clock refresh
// measures a new round trip, so the window is sized again after it, and
// the fit and the projected uncertainty are checked for the new window.
// So a call that WebKit can act on before the page's fence refuses it is
// never sent with a window that a delivery as slow as the measured one
// could miss.
func (op *Op) sendAttempt(a Action, fn string, step int, probed bool) (AttemptReply, attemptOutcome, error) {
	s := op.s
	gen := s.o.Generation()
	cm := s.clockMap(op.clockKey(gen))
	if !cm.Measured() {
		if err := op.measureClock(cm, gen); err != nil {
			return AttemptReply{}, attemptStopped, err
		}
	}
	sizeWindow := func() time.Duration {
		if probed {
			return max(s.o.AttemptBudget, probedWindowFactor*cm.RoundTrip())
		}
		return s.o.AttemptBudget
	}
	window := sizeWindow()
	end := op.deadline
	if a.Reserve > 0 {
		end -= a.Reserve
	}
	// fits reports whether an attempt sent at sent may start; it
	// returns the reason when it may not.
	fits := func(sent time.Duration) error {
		switch {
		case probed && sent+window > end:
			return &ProbedWindowError{Fn: fn, Window: window, RoundTrip: cm.RoundTrip(), Left: max(end-sent, 0)}
		case a.Reserve > 0 && sent >= end:
			return ErrActionTimeout
		}
		return nil
	}
	sent := s.o.Clock.Now()
	if err := fits(sent); err != nil {
		return AttemptReply{}, attemptStopped, err
	}
	boundary := min(sent+window, end)
	if u := cm.Uncertainty(boundary); u > s.o.MaxUncertainty {
		if err := op.measureClock(cm, gen); err != nil {
			return AttemptReply{}, attemptStopped, err
		}
		window = sizeWindow()
		sent = s.o.Clock.Now()
		if err := fits(sent); err != nil {
			return AttemptReply{}, attemptStopped, err
		}
		boundary = min(sent+window, end)
		if u := cm.Uncertainty(boundary); u > s.o.MaxUncertainty {
			return AttemptReply{}, attemptStopped, &ClockUncertaintyError{Uncertainty: u}
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
		raw, err := s.o.Runtime(callCtx, caller, fn, args)
		done <- result{raw, err}
		close(inf.replied)
	}()
	lost := s.o.Clock.After(boundary + s.o.ReplyGrace - sent)
	var r result
	select {
	case r = <-done:
	case <-op.ctx.Done():
		if !inf.close() {
			return AttemptReply{}, attemptStopped, context.Cause(op.ctx) // nothing was sent, and nothing will be
		}
		// The slot stays held until the reply or the boundary (End).
		return AttemptReply{}, attemptAmbiguous, ErrOutcomeUnknown
	case <-lost:
		if !inf.close() {
			return AttemptReply{}, attemptStopped, ErrReplyLost
		}
		return AttemptReply{}, attemptAmbiguous, ErrReplyLost
	}
	op.mu.Lock()
	op.inflight = nil
	op.mu.Unlock()
	replyAt := s.o.Clock.Now()
	if r.err != nil {
		if !inf.isCommitted() || errors.Is(r.err, webkit.ErrNotSent) {
			return AttemptReply{}, attemptStopped, r.err
		}
		return AttemptReply{}, attemptAmbiguous, r.err
	}
	var rep AttemptReply
	if err := json.Unmarshal(r.raw, &rep); err != nil {
		return AttemptReply{}, attemptAmbiguous, fmt.Errorf("decoding attempt reply: %w", err)
	}
	if rep.Doc != args.Doc {
		// The page refuses an attempt for another document, so the
		// step did not run; that document needs a new mapping.
		s.dropClock(op.clockKey(gen), cm)
		return AttemptReply{}, attemptStopped, ErrDocumentChanged
	}
	if rep.Now > 0 {
		cm.Observe(ClockSample{H0: sent, H1: replyAt, P: fromMS(rep.Now)})
	}
	return rep, attemptReplied, nil
}
