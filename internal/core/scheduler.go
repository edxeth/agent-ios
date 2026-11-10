package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/runtime"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Class is the admission class of an operation (design 2.5 and 2.10).
type Class int

const (
	// ClassRead runs in parallel with other reads.
	ClassRead Class = iota
	// ClassMutate runs alone, in FIFO order; ref allocation counts as a mutation.
	ClassMutate
	// ClassExclusive runs alone, like a mutation: legacy multi-send
	// operations and barriers.
	ClassExclusive
)

func (c Class) String() string {
	switch c {
	case ClassRead:
		return "read"
	case ClassMutate:
		return "mutate"
	case ClassExclusive:
		return "exclusive"
	}
	return fmt.Sprintf("Class(%d)", int(c))
}

// deadlineError is ErrDeadline.
type deadlineError struct{}

func (deadlineError) Error() string { return "the operation deadline passed" }
func (deadlineError) Unwrap() error { return context.DeadlineExceeded }

// Errors of the scheduler.
var (
	ErrStaleEpoch = errors.New("the lease epoch of the operation is no longer current; the page lease changed")
	ErrLeaseEnded = errors.New("the page lease ended")
	ErrOpEnded    = errors.New("the operation has ended")
	// ErrDeadline is a deadline error: errors.Is(err, context.DeadlineExceeded)
	// holds for it, so an operation that its own deadline ended fails as a
	// deadline wherever it surfaces (decision D9: exit 124).
	ErrDeadline       error = deadlineError{}
	ErrNoDeadline           = errors.New("an operation needs a deadline")
	ErrControlRefused       = errors.New("control message refused")
	// ErrAttemptClosed: RunSteps stopped waiting for the attempt (its reply
	// was lost or the operation stopped), so no send of it is admitted.
	ErrAttemptClosed = errors.New("the attempt is closed; the daemon no longer waits for its reply")
)

// Defaults of SchedulerOptions (design 2.5, 2.6 and 2.10).
const (
	DefaultMaxInFlight    = 8 // protocol calls in flight per page (MAP fact 8)
	DefaultAttemptBudget  = 250 * time.Millisecond
	DefaultMaxUncertainty = 100 * time.Millisecond
	DefaultReplyGrace     = 2 * time.Second
	DefaultRetryBackoff   = 20 * time.Millisecond
	DefaultCancelBudget   = time.Second
	DefaultControlBudget  = 10 * time.Second
)

// RuntimeCaller runs one runtime function through c and returns the value
// of its result envelope.
type RuntimeCaller func(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error)

// CallRuntime is the default RuntimeCaller: runtime.Runtime.Call.
func CallRuntime(ctx context.Context, c webkit.Caller, fn string, args any) (json.RawMessage, error) {
	var v json.RawMessage
	if err := runtime.New(c, nil).Call(ctx, fn, args, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// AttemptEvent describes one attempt as the daemon sent it (log and test
// hook). OnAttempt runs once per attempt, when its first send is admitted.
type AttemptEvent struct {
	OpID   string
	Step   int
	SentAt time.Duration // scheduler clock
	Args   AttemptArgs
}

// SchedulerOptions configure a Scheduler. Zero fields take the defaults.
type SchedulerOptions struct {
	Clock          Clock
	Runtime        RuntimeCaller
	Generation     func() uint64 // document generation; nil: one document
	MaxInFlight    int
	AttemptBudget  time.Duration
	MaxUncertainty time.Duration
	ReplyGrace     time.Duration // wait for an attempt reply after its boundary
	RetryBackoff   time.Duration
	CancelBudget   time.Duration
	OnAttempt      func(AttemptEvent)
}

// Scheduler is the per-page operation scheduler of design 2.10. Every entry
// point (registry command, CDP message) is an operation.
// Mutations and exclusive operations run one at a time in FIFO order and
// exclude reads; reads run in parallel with at most MaxInFlight protocol
// calls. Control-lane messages that let a running operation finish never
// queue behind it, and barriers close admission.
type Scheduler struct {
	c webkit.Caller
	o SchedulerOptions

	mu       sync.Mutex
	holder   string // logical session that holds the lease
	epoch    uint64
	released bool // the lease ended; operations of every epoch stop
	barriers int  // open barriers; admission is closed while any is open
	queue    []*Op
	running  map[*Op]bool
	changed  chan struct{} // closed and replaced when running shrinks or a completion ends
	nextOp   int
	clocks   map[clockKey]*ClockMap // by document generation and execution context
	ctxs     contextBook            // execution contexts that operations selected (exec_context_w3b.go)

	// Pending control items (design 2.10). Each is completed at most once:
	// an item stays pending while its completion is being written. A held
	// request closes when a completion succeeded or WebKit's reply shows
	// that it no longer holds the request (requestGone); after any other
	// failure it is open again for a later completion or a barrier. Holds
	// are kept per target, because WebKit holds a request in the backend of
	// the target that reported it: a provisional target's hold is answered
	// on that target, before and after its commit (the committed target
	// keeps its id). A destroyed target's holds are dropped: WebKit
	// destroyed them with the target.
	intercepted map[holdKey]*heldRequest
	holdSeq     uint64 // counts interception events
	// Debugger pauses are kept per target for the same reason: WebKit
	// pauses the JavaScript of the backend that sent Debugger.paused, and
	// only a Debugger.resume sent to that backend resumes it. A destroyed
	// target's pause is dropped like its holds.
	pauses   map[string]*debuggerPause
	pauseSeq uint64 // counts Debugger.paused events
	// resetReqs are held requests that a Network reinitialization barrier
	// continued (design 2.4); a later completion for one fails with
	// ErrRequestReset until the id is held again on that target.
	resetReqs map[holdKey]bool
	// holdClosed, when set (tests only), is called with the key of a hold
	// that a completion closed, after the critical section that closed it.
	holdClosed func(holdKey)
	// holdForgotten, when set (tests only), is called with the key of a
	// hold that the scheduler forgot without a completion (forgetHolds,
	// dropHolds), after the critical section that forgot it.
	holdForgotten func(holdKey)

	sem   chan struct{}
	unsub []func()
}

// NewScheduler returns the scheduler of the page reached through c.
func NewScheduler(c webkit.Caller, o SchedulerOptions) *Scheduler {
	if o.Clock == nil {
		o.Clock = NewRealClock()
	}
	if o.Runtime == nil {
		o.Runtime = CallRuntime
	}
	if o.Generation == nil {
		o.Generation = func() uint64 { return 1 }
	}
	if o.MaxInFlight <= 0 {
		o.MaxInFlight = DefaultMaxInFlight
	}
	if o.AttemptBudget <= 0 {
		o.AttemptBudget = DefaultAttemptBudget
	}
	if o.MaxUncertainty <= 0 {
		o.MaxUncertainty = DefaultMaxUncertainty
	}
	if o.ReplyGrace <= 0 {
		o.ReplyGrace = DefaultReplyGrace
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = DefaultRetryBackoff
	}
	if o.CancelBudget <= 0 {
		o.CancelBudget = DefaultCancelBudget
	}
	s := &Scheduler{
		c:           c,
		o:           o,
		running:     make(map[*Op]bool),
		changed:     make(chan struct{}),
		clocks:      make(map[clockKey]*ClockMap),
		intercepted: make(map[holdKey]*heldRequest),
		pauses:      make(map[string]*debuggerPause),
		sem:         make(chan struct{}, o.MaxInFlight),
	}
	sub := func(method string, h webkit.TargetEventHandler) func() { return subscribeItems(c, method, h) }
	addRequest := func(stage string) webkit.TargetEventHandler {
		return func(target, _ string, p json.RawMessage) {
			var v struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(p, &v) == nil && v.RequestID != "" {
				key := holdKey{target, v.RequestID}
				s.mu.Lock()
				s.holdSeq++
				h := s.intercepted[key]
				if h == nil {
					h = &heldRequest{}
					s.intercepted[key] = h
				}
				// A new hold of the same id (the response stage) is open
				// even while a completion of the earlier hold is in flight.
				h.seq, h.stage, h.inflight = s.holdSeq, stage, false
				delete(s.resetReqs, key)
				s.mu.Unlock()
			}
		}
	}
	// dropTarget forgets the holds and the pause of a target that WebKit
	// destroyed: an abandoned provisional target, the target that a commit
	// replaced (its frontend is disconnected, which continues its pending
	// intercepts), or a closed page.
	dropTarget := func(field string) webkit.TargetEventHandler {
		return func(_, _ string, p json.RawMessage) {
			var v map[string]string
			if json.Unmarshal(p, &v) == nil && v[field] != "" {
				s.dropHolds(v[field])
			}
		}
	}
	setPaused := func(paused bool) webkit.TargetEventHandler {
		return func(target, _ string, _ json.RawMessage) {
			s.mu.Lock()
			defer s.mu.Unlock()
			p := s.pauses[target]
			if p == nil {
				p = &debuggerPause{}
				s.pauses[target] = p
			}
			p.paused = paused
			if paused {
				s.pauseSeq++
				p.seq = s.pauseSeq
				close(s.changed) // a release barrier drains the new pause
				s.changed = make(chan struct{})
			}
		}
	}
	s.unsub = append(s.unsub,
		sub("Network.requestIntercepted", addRequest(stageRequest)),
		sub("Network.responseIntercepted", addRequest(stageResponse)),
		sub("Debugger.paused", setPaused(true)),
		sub("Debugger.resumed", setPaused(false)),
		sub("Target.targetDestroyed", dropTarget("targetId")),
		sub("Target.didCommitProvisionalTarget", dropTarget("oldTargetId")),
	)
	return s
}

// subscribeItems registers h for the scheduler's pending items: inside the
// read loop when c allows it, so that whoever sees the event later finds
// the item, and with each event's source target when c knows it
// (webkit.TargetEventHandler; "" otherwise).
func subscribeItems(c webkit.Caller, method string, h webkit.TargetEventHandler) func() {
	if ss, ok := c.(interface {
		SubscribeTargetSync(string, webkit.TargetEventHandler) func()
	}); ok {
		return ss.SubscribeTargetSync(method, h)
	}
	if ss, ok := c.(interface {
		SubscribeSync(string, webkit.EventHandler) func()
	}); ok {
		return ss.SubscribeSync(method, func(m string, p json.RawMessage) { h("", m, p) })
	}
	return webkit.SubscribeTarget(c, method, h)
}

// dropHolds forgets every hold of target, and its pause.
func (s *Scheduler) dropHolds(target string) {
	s.mu.Lock()
	var forgot []holdKey
	for k := range s.intercepted {
		if k.target == target {
			delete(s.intercepted, k)
			forgot = append(forgot, k)
		}
	}
	delete(s.pauses, target)
	close(s.changed) // a barrier may wait for one of them
	s.changed = make(chan struct{})
	s.mu.Unlock()
	s.reportForgotten(forgot)
}

// reportForgotten calls holdForgotten (tests only) with each forgotten
// hold.
func (s *Scheduler) reportForgotten(forgot []holdKey) {
	if s.holdForgotten != nil {
		for _, k := range forgot {
			s.holdForgotten(k)
		}
	}
}

// holdTarget is the target that a control message names: target, or the
// page connection's active target for "" (the connection's own target
// when it has no targets).
func (s *Scheduler) holdTarget(target string) string {
	if target != "" {
		return target
	}
	if at, ok := s.c.(interface{ ActiveTarget() string }); ok {
		return at.ActiveTarget()
	}
	return ""
}

// Close removes the scheduler's event subscriptions.
func (s *Scheduler) Close() {
	for _, u := range s.unsub {
		u()
	}
}

// SetLease records the session that holds the page lease and its epoch.
// Queued operations of another epoch fail with ErrStaleEpoch; running ones
// can send nothing more.
func (s *Scheduler) SetLease(session string, epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holder, s.epoch, s.released = session, epoch, false
	s.pumpLocked()
}

// Lease returns the current holder and epoch.
func (s *Scheduler) Lease() (string, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holder, s.epoch
}

// OpSpec describes an operation.
type OpSpec struct {
	Name    string
	Class   Class
	Session string // logical session that runs the operation
	Epoch   uint64 // lease epoch the operation was admitted under
	// PageWork: the operation runs page-side work fenced by its op id, so a
	// cancel also sends __iwdp.cancel(opId) on the control lane.
	PageWork bool
	// Deadline is absolute on the scheduler clock. Zero: taken from the
	// ctx deadline at Begin.
	Deadline time.Duration
}

// Op is an admitted operation. End must be called exactly once.
type Op struct {
	ID         string
	Spec       OpSpec
	AdmittedAt time.Duration // scheduler clock

	s        *Scheduler
	ctx      context.Context
	cancel   context.CancelCauseFunc
	deadline time.Duration
	admitted chan error
	pageWork atomic.Bool

	mu       sync.Mutex
	ended    bool
	inflight *inflightAttempt
	exec     atomic.Pointer[ExecContext] // execution context it works in; nil: main (exec_context_w3b.go)
	finally  []opFinally                 // cleanups that End runs before the slot is released (op_finally_w3b.go)
}

type inflightAttempt struct {
	boundary time.Duration // slot-release boundary H
	replied  chan struct{}
	gen      uint64 // document generation the attempt's clock mapping belongs to

	mu        sync.Mutex
	committed bool // a Send of the attempt was admitted; the page may run it
	closed    bool // RunSteps stopped waiting; no Send is admitted any more
}

// admit admits one Send of the attempt's runtime call (design 2.6 rule 3):
// only while RunSteps still waits for it, the operation is active (not
// ended, canceled or expired, lease epoch current) and the page shows the
// document the clock mapping was measured on. The first admitted Send
// commits the attempt; commit runs once, after the admission.
func (inf *inflightAttempt) admit(op *Op, commit func()) error {
	inf.mu.Lock()
	if inf.closed {
		inf.mu.Unlock()
		// The operation can still be active here (a lost reply closes the
		// attempt), so the refusal never depends on its context.
		if cause := context.Cause(op.ctx); cause != nil {
			return fmt.Errorf("%w: %w", ErrAttemptClosed, cause)
		}
		return ErrAttemptClosed
	}
	if err := op.active(); err != nil {
		inf.mu.Unlock()
		return err
	}
	if op.s.o.Generation() != inf.gen {
		inf.mu.Unlock()
		return ErrDocumentChanged
	}
	first := !inf.committed
	inf.committed = true
	inf.mu.Unlock()
	if first && commit != nil {
		commit()
	}
	return nil
}

// close stops further admission and reports whether the attempt was
// committed, that is, whether the page may have run it.
func (inf *inflightAttempt) close() bool {
	inf.mu.Lock()
	defer inf.mu.Unlock()
	inf.closed = true
	return inf.committed
}

func (inf *inflightAttempt) isCommitted() bool {
	inf.mu.Lock()
	defer inf.mu.Unlock()
	return inf.committed
}

// attemptCaller sends the runtime call of one attempt. Each Send passes
// inflightAttempt.admit first and sends nothing when it fails. The reply
// wait is not bound to the operation's context: after a cancel the daemon
// still waits for the reply, so that the page is released early.
type attemptCaller struct {
	op     *Op
	inf    *inflightAttempt
	commit func()
}

func (c attemptCaller) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := c.op.setupReady(ctx); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, err)
	}
	if err := c.inf.admit(c.op, c.commit); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, err)
	}
	ctx, params = c.op.route(ctx, method, params)
	return c.op.s.c.Send(ctx, method, params)
}

func (c attemptCaller) Subscribe(method string, h webkit.EventHandler) func() {
	return c.op.s.c.Subscribe(method, h)
}

func (c attemptCaller) SubscribeTarget(method string, h webkit.TargetEventHandler) func() {
	return webkit.SubscribeTarget(c.op.s.c, method, h)
}
func (c attemptCaller) IsTargetRouted() bool  { return c.op.s.c.IsTargetRouted() }
func (c attemptCaller) Done() <-chan struct{} { return c.op.s.c.Done() }

// Begin queues an operation and waits for admission. The operation's
// deadline covers the wait. Canceling ctx while queued removes it from the
// queue without sending anything. At admission, when the page connection
// reports a failed provisional setup of its active target (webkit.Session
// ActiveSetup, design 2.7), the operation is refused with that error: the
// page lacks required desired state until it is applied again. A setup in
// progress is waited for within the operation's deadline.
func (s *Scheduler) Begin(ctx context.Context, spec OpSpec) (*Op, error) {
	now := s.o.Clock.Now()
	d := spec.Deadline
	if d == 0 {
		dl, ok := ctx.Deadline()
		if !ok {
			return nil, fmt.Errorf("operation %s: %w", spec.Name, ErrNoDeadline)
		}
		d = now + time.Until(dl)
	}
	opCtx, cancel := context.WithCancelCause(ctx)
	op := &Op{Spec: spec, s: s, ctx: opCtx, cancel: cancel, deadline: d, admitted: make(chan error, 1)}
	// One absolute deadline covers the queue, the setup wait and the run
	// (design 2.6): the expiry starts now, whatever ctx allows.
	go op.watch(s.o.Clock.After(d - now))
	s.mu.Lock()
	if s.released || spec.Epoch != s.epoch {
		err := ErrStaleEpoch
		if s.released {
			err = ErrLeaseEnded
		}
		s.mu.Unlock()
		cancel(err)
		return nil, fmt.Errorf("operation %s: %w", spec.Name, err)
	}
	s.nextOp++
	op.ID = fmt.Sprintf("op-%d", s.nextOp)
	s.queue = append(s.queue, op)
	s.pumpLocked()
	s.mu.Unlock()

	var cause error
	select {
	case err := <-op.admitted:
		if err != nil {
			cancel(err)
			return nil, fmt.Errorf("operation %s: %w", spec.Name, err)
		}
		// The setup gate holds when the operation can use the target: the
		// active target may have changed while the operation was queued.
		if err := op.setupReady(context.Background()); err != nil {
			op.End()
			return nil, fmt.Errorf("operation %s: %w", spec.Name, err)
		}
		// Page work starts only after admission; watch sends no page
		// cancel for an operation that never ran.
		op.pageWork.Store(spec.PageWork)
		return op, nil
	case <-opCtx.Done():
		cause = context.Cause(opCtx)
		if errors.Is(cause, ErrDeadline) {
			cause = fmt.Errorf("%w while queued", ErrDeadline)
		}
	}
	s.mu.Lock()
	removed := s.removeLocked(op)
	s.mu.Unlock()
	if !removed {
		// Admitted at the same moment: give the slot back.
		if err := <-op.admitted; err == nil {
			op.End()
		}
	}
	cancel(cause)
	return nil, fmt.Errorf("operation %s: %w", spec.Name, cause)
}

func (s *Scheduler) removeLocked(op *Op) bool {
	for i, q := range s.queue {
		if q == op {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			s.pumpLocked()
			return true
		}
	}
	return false
}

// pumpLocked admits queued operations in FIFO order.
func (s *Scheduler) pumpLocked() {
	if s.barriers > 0 {
		return
	}
	writer := false
	for op := range s.running {
		if op.Spec.Class != ClassRead {
			writer = true
		}
	}
	for len(s.queue) > 0 {
		op := s.queue[0]
		if s.released || op.Spec.Epoch != s.epoch {
			s.queue = s.queue[1:]
			if s.released {
				op.admitted <- ErrLeaseEnded
			} else {
				op.admitted <- ErrStaleEpoch
			}
			continue
		}
		if writer || (op.Spec.Class != ClassRead && len(s.running) > 0) {
			return
		}
		s.queue = s.queue[1:]
		s.running[op] = true
		op.AdmittedAt = s.o.Clock.Now()
		op.admitted <- nil
		if op.Spec.Class != ClassRead {
			return
		}
	}
}

// setupReady waits, at most until ctx or the operation ends, for the
// provisional setup of the page's active target and returns its failure
// (webkit.Session ActiveSetup, design 2.7). It returns nil when the page
// connection has no provisional setup.
func (op *Op) setupReady(ctx context.Context) error {
	as, ok := op.s.c.(interface{ ActiveSetup(context.Context) error })
	if !ok {
		return nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := context.AfterFunc(op.ctx, func() { cancel(context.Cause(op.ctx)) })
	defer stop()
	err := as.ActiveSetup(ctx)
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("%w: %w", err, context.Cause(ctx))
	}
	return err
}

// watch cancels the operation at its deadline and sends the page cancel
// for page work when the operation is canceled before it ends.
func (op *Op) watch(expiry <-chan struct{}) {
	select {
	case <-op.ctx.Done():
	case <-expiry:
		op.cancel(ErrDeadline)
	}
	// A normal End cancels with ErrOpEnded; any other cause is a cancel or
	// an expiry while page work may run, even when End follows at once.
	if !errors.Is(context.Cause(op.ctx), ErrOpEnded) && op.pageWork.Load() {
		op.s.sendCancel(op)
	}
}

// Context ends when the operation is canceled, expires or ends.
func (op *Op) Context() context.Context { return op.ctx }

// Deadline is the absolute deadline on the scheduler clock.
func (op *Op) Deadline() time.Duration { return op.deadline }

// Cancel cancels the operation with cause (the client went away).
func (op *Op) Cancel(cause error) { op.cancel(cause) }

// active returns nil while the operation may still send.
func (op *Op) active() error {
	op.mu.Lock()
	ended := op.ended
	op.mu.Unlock()
	if ended {
		return ErrOpEnded
	}
	if err := op.ctx.Err(); err != nil {
		return context.Cause(op.ctx)
	}
	s := op.s
	s.mu.Lock()
	released, epoch := s.released, s.epoch
	s.mu.Unlock()
	if released {
		return ErrLeaseEnded
	}
	if epoch != op.Spec.Epoch {
		return ErrStaleEpoch
	}
	if s.o.Clock.Now() >= op.deadline {
		return ErrDeadline
	}
	return nil
}

// End ends the operation. When an attempt is still in flight, the slot is
// released only at its reply or its slot-release boundary (design 2.6), so a
// conflicting successor never overlaps it. Then the cleanups registered
// with Finally run, and only after them is the slot released. End does
// not wait for any of that.
func (op *Op) End() {
	op.mu.Lock()
	if op.ended {
		op.mu.Unlock()
		return
	}
	op.ended = true
	inf, fin := op.inflight, op.finally
	op.mu.Unlock()
	op.cancel(ErrOpEnded)
	if inf == nil && len(fin) == 0 {
		op.s.release(op)
		return
	}
	go func() {
		if inf != nil {
			select {
			case <-inf.replied:
			case <-op.s.o.Clock.After(inf.boundary - op.s.o.Clock.Now()):
			}
		}
		op.runFinally(fin)
		op.s.release(op)
	}()
}

func (s *Scheduler) release(op *Op) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running[op] {
		return
	}
	delete(s.running, op)
	close(s.changed)
	s.changed = make(chan struct{})
	s.pumpLocked()
}

// Caller returns a Caller fenced by the operation: every Send first checks
// that the operation is active (not ended, canceled or expired, lease epoch
// current) and sends nothing otherwise.
func (op *Op) Caller() webkit.Caller { return opCaller{op} }

type opCaller struct{ op *Op }

// opEnded reports that the operation ended or was canceled: its caller
// sends nothing more, and a desired-state change made through it records
// no more additions (DesiredState.refusedLocked).
func (c opCaller) opEnded() bool { return c.op.ctx.Err() != nil }

func (c opCaller) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	op := c.op
	// Each refusal here is before the write: webkit.ErrNotSent, whose text
	// is "not sent".
	if err := op.active(); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, err)
	}
	select {
	case op.s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, ctx.Err())
	case <-op.ctx.Done():
		return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, context.Cause(op.ctx))
	}
	defer func() { <-op.s.sem }()
	if err := op.active(); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, err)
	}
	// The active target may have changed since admission (design 2.7).
	if err := op.setupReady(ctx); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, err)
	}
	callCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := context.AfterFunc(op.ctx, func() { cancel(context.Cause(op.ctx)) })
	defer stop()
	callCtx, params = op.route(callCtx, method, params)
	return op.s.c.Send(callCtx, method, params)
}

func (c opCaller) Subscribe(method string, h webkit.EventHandler) func() {
	return c.op.s.c.Subscribe(method, h)
}

func (c opCaller) SubscribeTarget(method string, h webkit.TargetEventHandler) func() {
	return webkit.SubscribeTarget(c.op.s.c, method, h)
}
func (c opCaller) IsTargetRouted() bool  { return c.op.s.c.IsTargetRouted() }
func (c opCaller) Done() <-chan struct{} { return c.op.s.c.Done() }

// ControlKind is the kind of a control-lane message (design 2.10).
type ControlKind int

const (
	// ControlCancel: __iwdp.cancel(opId) for a running operation.
	ControlCancel ControlKind = iota + 1
	// ControlCompleteRequest: continue, fulfill or fail a held request.
	ControlCompleteRequest
	// ControlDebuggerResume: Debugger.resume while the page is paused.
	ControlDebuggerResume
	// ControlTargetResume: Target.resume for a target paused on start.
	ControlTargetResume
)

// Interception stages: the WIP NetworkStage values. A hold opened by
// Network.requestIntercepted is at the request stage, one opened by
// Network.responseIntercepted at the response stage.
const (
	stageRequest  = "request"
	stageResponse = "response"
)

// interceptCompletions are the WIP methods that complete a held request,
// each with the stage of the holds it completes: WebKit's
// InspectorNetworkAgent takes the hold from its pending requests or its
// pending responses and answers "Missing pending intercept ..." for a hold
// at the other stage. Network.interceptContinue completes either stage; its
// required stage parameter (Network.json) names which ("" here).
var interceptCompletions = map[string]string{
	"Network.interceptContinue":            "",
	"Network.interceptWithRequest":         stageRequest,
	"Network.interceptWithResponse":        stageResponse,
	"Network.interceptRequestWithResponse": stageRequest,
	"Network.interceptRequestWithError":    stageRequest,
}

// isInterceptCompletion reports whether method completes a held request.
func isInterceptCompletion(method string) bool {
	_, ok := interceptCompletions[method]
	return ok
}

// ControlMsg is a control-lane message.
type ControlMsg struct {
	Kind      ControlKind
	Session   string // logical session of the sender
	Epoch     uint64 // lease epoch the sender acts under
	OpID      string // ControlCancel
	RequestID string // ControlCompleteRequest
	// TargetID is the target paused on start (ControlTargetResume), the
	// target that holds the request (ControlCompleteRequest): the source
	// target of its interception event (webkit.TargetEventHandler), or the
	// paused target (ControlDebuggerResume): the source target of its
	// Debugger.paused event. "" names the page's active target; for
	// ControlDebuggerResume it names the active target when that target is
	// paused and otherwise the most recently paused target (pauseTarget).
	TargetID string
	Method   string // ControlCompleteRequest: one of the five completion methods
	// Params are the method's parameters besides requestId. For
	// Network.interceptContinue a missing stage is the stage of the hold.
	Params map[string]any
}

// Control sends a control-lane message without waiting behind ordinary
// operations. It is validated first: the epoch is current, the sender acts
// for the session that holds the lease, and the message names an open
// pending item, which it completes at most once. A refused message is
// never sent.
func (s *Scheduler) Control(ctx context.Context, m ControlMsg) (json.RawMessage, error) {
	s.mu.Lock()
	holder, epoch, released := s.holder, s.epoch, s.released
	s.mu.Unlock()
	switch {
	case released:
		return nil, fmt.Errorf("%w: %w", ErrControlRefused, ErrLeaseEnded)
	case m.Epoch != epoch:
		return nil, fmt.Errorf("%w: epoch %d is not the current lease epoch %d", ErrControlRefused, m.Epoch, epoch)
	case m.Session != holder:
		return nil, fmt.Errorf("%w: sent for session %q; the page is leased by session %q", ErrControlRefused, m.Session, holder)
	}
	return s.control(ctx, m)
}

// interceptGoneErrors are the errors of WebKit's InspectorNetworkAgent for
// the five completion methods (webkitgtk-2.54.2 InspectorNetworkAgent.cpp
// :1180-1353; the same texts in iOS 16 and WebKit main). Each method takes
// the hold from m_pendingInterceptRequests or m_pendingInterceptResponses
// first, so every one of these errors means that WebKit no longer holds the
// request: it had no such hold ("Missing pending ..."), or it took the hold
// and then refused the completion (a decode error, or a loader that had
// already finished). Errors of the generated dispatcher (InvalidParams
// -32602 and the like) come before the agent runs and leave the hold.
var interceptGoneErrors = map[string]bool{
	"Missing pending intercept request for given requestId":      true,
	"Missing pending intercept response for given requestId":     true,
	"Unable to intercept request, it has already been processed": true,
	"Unable to fulfill request, it has already been processed":   true,
	"Unable to abort request, it has already been processed":     true,
	"Unable to decode given postData":                            true,
	"Unable to decode given content":                             true,
}

// requestGone reports whether err is WebKit's reply to a completion that
// proves that WebKit no longer holds the request.
func requestGone(err error) bool {
	var pe *webkit.ErrorData
	return errors.As(err, &pe) && interceptGoneErrors[pe.Message]
}

// holdKey names a held request: WebKit holds it in the backend of target.
type holdKey struct{ target, id string }

// heldRequest is a request held by interception (design 2.10).
type heldRequest struct {
	seq      uint64 // interception event that opened this hold
	stage    string // stageRequest or stageResponse: the event that opened this hold
	inflight bool   // a completion is being written
}

// control validates the pending item and sends; barriers call it directly.
func (s *Scheduler) control(ctx context.Context, m ControlMsg) (json.RawMessage, error) {
	switch m.Kind {
	case ControlCompleteRequest:
		return s.completeHold(ctx, holdKey{s.holdTarget(m.TargetID), m.RequestID}, m, false)
	case ControlDebuggerResume:
		return s.resumePause(ctx, s.pauseTarget(m.TargetID))
	case ControlTargetResume:
		pt, ok := s.c.(interface {
			ResumeTarget(context.Context, string) error
		})
		if !ok {
			return nil, fmt.Errorf("%w: the page connection has no targets paused on start", ErrControlRefused)
		}
		err := pt.ResumeTarget(ctx, m.TargetID)
		if errors.Is(err, webkit.ErrTargetNotPaused) {
			return nil, fmt.Errorf("%w: target %q is not paused on start", ErrControlRefused, m.TargetID)
		}
		return nil, err
	case ControlCancel:
		s.mu.Lock()
		var op *Op
		for r := range s.running {
			if r.ID == m.OpID {
				op = r
			}
		}
		s.mu.Unlock()
		if op == nil {
			return nil, fmt.Errorf("%w: no running operation %q", ErrControlRefused, m.OpID)
		}
		op.Cancel(fmt.Errorf("canceled through the control lane"))
		return nil, nil
	}
	return nil, fmt.Errorf("%w: unknown kind %d", ErrControlRefused, m.Kind)
}

// completeHold validates and sends m, a completion of the held request
// key; key names the target exactly (the barriers pass their own keys).
// With reset (the Network reinitialization barrier), a completion that
// closes the hold records it as reset in the same critical section, so a
// later completion fails with ErrRequestReset at once.
func (s *Scheduler) completeHold(ctx context.Context, key holdKey, m ControlMsg, reset bool) (json.RawMessage, error) {
	if !isInterceptCompletion(m.Method) {
		return nil, fmt.Errorf("%w: %q does not complete a held request", ErrControlRefused, m.Method)
	}
	s.mu.Lock()
	h := s.intercepted[key]
	if h == nil && s.resetReqs[key] {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: intercepted request %q: %w", ErrControlRefused, m.RequestID, ErrRequestReset)
	}
	if h == nil || h.inflight {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: no open intercepted request %q on page target %q", ErrControlRefused, m.RequestID, key.target)
	}
	// WebKit refuses a completion for the other stage and keeps the
	// request held, so such a completion is never sent.
	stage := interceptCompletions[m.Method]
	if m.Method == "Network.interceptContinue" {
		stage = h.stage
		if v, ok := m.Params["stage"]; ok {
			stage = fmt.Sprint(v) // a value that is not a NetworkStage string never matches
		}
	}
	if stage != h.stage {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s completes a hold at the %q stage; intercepted request %q is held at the %q stage", ErrControlRefused, m.Method, stage, m.RequestID, h.stage)
	}
	h.inflight = true
	seq := h.seq
	s.mu.Unlock()
	params := map[string]any{}
	for k, v := range m.Params {
		params[k] = v
	}
	params["requestId"] = m.RequestID
	if m.Method == "Network.interceptContinue" {
		params["stage"] = stage // required by WIP (Network.json)
	}
	// The completion goes to the target that holds the request, which
	// is not the active target while a provisional target loads.
	res, err := s.controlSend(webkit.WithTarget(ctx, key.target), DefaultControlBudget, m.Method, params)
	closed := false
	s.mu.Lock()
	if h.seq == seq { // no newer hold of this id arrived meanwhile
		if err == nil || requestGone(err) {
			if s.intercepted[key] == h {
				delete(s.intercepted, key)
				closed = true
			}
			if reset {
				s.markResetLocked(key)
			}
		} else {
			// Not sent, refused by WebKit's dispatcher before the
			// agent took the hold, or the outcome is unknown: the
			// request may still be paused. A later completion or a
			// barrier's continue answers it; if WebKit already took
			// it, that completion gets "Missing pending intercept ...".
			h.inflight = false
		}
	}
	close(s.changed) // a barrier may wait for this completion
	s.changed = make(chan struct{})
	s.mu.Unlock()
	if closed && s.holdClosed != nil {
		s.holdClosed(key)
	}
	return res, err
}

// debuggerPause is the Debugger pause of one target (design 2.10).
type debuggerPause struct {
	paused     bool
	seq        uint64 // the Debugger.paused event of the current pause
	resumedSeq uint64 // the pause that a Debugger.resume completed or is completing
	resuming   bool   // a Debugger.resume is being written
}

// open reports whether p is an open pending item: paused, not resumed
// yet, and no resume being written.
func (p *debuggerPause) open() bool {
	return p != nil && p.paused && !p.resuming && p.resumedSeq != p.seq
}

// pauseTarget is the target whose pause a Debugger.resume for target
// answers: target itself, or for "" the active target when its pause is
// open and otherwise the target of the most recent open pause. Callers of
// the legacy tools cannot name a target, and the events they get do not
// name their source, so "" must also reach a pause that a provisional
// target reported.
func (s *Scheduler) pauseTarget(target string) string {
	if target != "" {
		return target
	}
	active := s.holdTarget("") // outside s.mu: it asks the connection
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pauses[active].open() {
		return active
	}
	var last *debuggerPause
	for t, p := range s.pauses {
		if p.open() && (last == nil || p.seq > last.seq) {
			target, last = t, p
		}
	}
	if last == nil {
		return active
	}
	return target
}

// resumePause sends Debugger.resume to target, the key of its pause, when
// that pause is an open pending item, and completes the pause at most once.
func (s *Scheduler) resumePause(ctx context.Context, target string) (json.RawMessage, error) {
	s.mu.Lock()
	p := s.pauses[target]
	if !p.open() {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: the debugger is not paused on page target %q", ErrControlRefused, target)
	}
	seq := p.seq
	p.resuming = true
	s.mu.Unlock()
	// The resume goes to the target that paused, which is not the active
	// target while a provisional target loads.
	res, err := s.controlSend(webkit.WithTarget(ctx, target), DefaultControlBudget, "Debugger.resume", nil)
	s.mu.Lock()
	p.resuming = false
	if !errors.Is(err, webkit.ErrNotSent) {
		p.resumedSeq = seq // completed, or the outcome is unknown: never resume this pause again
		if err == nil && p.seq == seq {
			p.paused = false
		}
	}
	close(s.changed) // a release barrier may wait for this resume
	s.changed = make(chan struct{})
	s.mu.Unlock()
	return res, err
}

// pauseGen names one pause of a target: its Debugger.paused event.
type pauseGen struct {
	target string
	seq    uint64
}

// errResumeUnknown: a Debugger.resume of the pause has an unknown outcome
// (no reply, or an error reply), so the pause is never resumed again.
var errResumeUnknown = errors.New("the outcome of its Debugger.resume is unknown")

// pauseDrain is the release barrier's answer to the Debugger pauses
// (design 2.10). It resumes every open pause on its own target and waits
// for the resumes in flight, until no pause is open or being resumed:
// pauses open at the start, pauses that a client resume in flight leaves
// open (not sent), and pauses that arrive meanwhile (a newer pause that
// WebKit reports before the reply to the resume of the older one). A
// resume is sent again only when it was not sent (webkit.ErrNotSent), at
// most maxBarrierContinues times per pause; a resume with an unknown
// outcome is never repeated, because a repeated one could resume a later
// pause that nobody has seen. The release's context bounds the drain.
type pauseDrain struct {
	s      *Scheduler
	unsent map[pauseGen]int  // resumes of a pause that were not sent
	gaveUp map[pauseGen]bool // pauses whose resume was not sent maxBarrierContinues times
	err    error             // the first failure of the barrier's own resumes
}

func newPauseDrain(s *Scheduler) *pauseDrain {
	return &pauseDrain{s: s, unsent: map[pauseGen]int{}, gaveUp: map[pauseGen]bool{}}
}

// nextLocked returns an open pause that the drain has not given up, and
// whether a resume is in flight. The caller holds s.mu.
func (d *pauseDrain) nextLocked() (gen pauseGen, ok, inflight bool) {
	for t, p := range d.s.pauses {
		switch {
		case p.resuming:
			inflight = true
		case p.open() && !d.gaveUp[pauseGen{t, p.seq}]:
			gen, ok = pauseGen{t, p.seq}, true
		}
	}
	return gen, ok, inflight
}

// run drains the pauses (pauseDrain). It returns nil when no target is
// paused at the end, and otherwise an error that names every target that
// is still paused, with the first failure, the end of ctx, or
// errResumeUnknown as its cause.
func (d *pauseDrain) run(ctx context.Context) error {
	s := d.s
	for {
		s.mu.Lock()
		gen, ok, inflight := d.nextLocked()
		if !ok && inflight {
			changed := s.changed
			s.mu.Unlock()
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return d.remaining(ctx.Err())
			}
		}
		s.mu.Unlock()
		if !ok {
			return d.remaining(nil)
		}
		_, err := s.resumePause(ctx, gen.target)
		switch {
		case err == nil, errors.Is(err, ErrControlRefused):
			// Answered, or another sender answered or is answering it:
			// look again.
		case errors.Is(err, webkit.ErrNotSent):
			if d.unsent[gen]++; d.unsent[gen] >= maxBarrierContinues || ctx.Err() != nil {
				d.gaveUp[gen] = true
				if d.err == nil {
					d.err = err
				}
			}
		default:
			if d.err == nil {
				d.err = err
			}
		}
	}
}

// remaining returns nil when no target is paused, and otherwise an error
// naming every paused target, with the drain's first failure and cause
// (the end of the release's context) as its causes, or errResumeUnknown
// when there is neither.
func (d *pauseDrain) remaining(cause error) error {
	d.s.mu.Lock()
	var targets []string
	for t, p := range d.s.pauses {
		if p.paused {
			targets = append(targets, t)
		}
	}
	d.s.mu.Unlock()
	if len(targets) == 0 {
		return nil
	}
	slices.Sort(targets)
	causes := errors.Join(d.err, cause)
	if causes == nil {
		causes = errResumeUnknown
	}
	return fmt.Errorf("release: the debugger is still paused on page targets %q: %w", targets, causes)
}

// markResetLocked records the hold k as continued by a Network reset
// (resetReqs). The caller holds s.mu.
func (s *Scheduler) markResetLocked(k holdKey) {
	if s.resetReqs == nil {
		s.resetReqs = map[holdKey]bool{}
	}
	s.resetReqs[k] = true
}

// controlCaller sends on the control lane of the page connection. A
// webkit.Session refuses ordinary commands to a target whose setup failed or
// still runs (design 2.7); control-lane messages and the release barrier's
// cleanup pass that check through its SendControl (design 2.10), so that
// held items and recovery can complete. The page cancel stays ordinary: it
// matters only for page work on a target that passed the check.
type controlCaller struct{ webkit.Caller }

func (c controlCaller) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if cs, ok := c.Caller.(interface {
		SendControl(context.Context, string, any) (json.RawMessage, error)
	}); ok {
		return cs.SendControl(ctx, method, params)
	}
	return c.Caller.Send(ctx, method, params)
}

func (s *Scheduler) controlSend(ctx context.Context, budget time.Duration, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return controlCaller{s.c}.Send(ctx, method, params)
}

// sendCancel sends __iwdp.cancel(opId) on the control lane with the cancel
// budget. Losing it changes only how soon page work ends (design 2.6).
func (s *Scheduler) sendCancel(op *Op) {
	ctx, cancel := context.WithTimeout(context.Background(), s.o.CancelBudget)
	defer cancel()
	_, _ = s.o.Runtime(ctx, s.c, "cancel", map[string]string{"opId": op.ID})
}

// PendingRequests returns the ids of the intercepted requests that are not
// completed, on every target (PendingHolds names the targets).
func (s *Scheduler) PendingRequests() []string {
	var out []string
	for _, h := range s.PendingHolds() {
		out = append(out, h.RequestID)
	}
	return out
}

// OpenHold returns the stage and the generation of the hold of request id
// on target while it is open: not completed and no completion in flight.
// Each interception event of the request opens a new generation, which
// the read loop records before the AllEvents subscriptions get the event
// (webkit.Session dispatch order), so a subscriber that asks while it
// handles the event learns the identity of the hold that the event
// announced, and later asks whether exactly that hold is still open.
func (s *Scheduler) OpenHold(target, id string) (stage string, gen uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.intercepted[holdKey{target, id}]
	if h == nil || h.inflight {
		return "", 0, false
	}
	return h.stage, h.seq, true
}

// HeldRequest is a request that interception holds on a page target.
type HeldRequest struct {
	Target    string // source target of the interception event ("" on a connection without targets)
	RequestID string
}

// PendingHolds returns the intercepted requests that are not completed,
// with the target that holds each, ordered by target and id.
func (s *Scheduler) PendingHolds() []HeldRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []HeldRequest
	for k, h := range s.intercepted {
		if !h.inflight {
			out = append(out, HeldRequest{Target: k.target, RequestID: k.id})
		}
	}
	slices.SortFunc(out, func(a, b HeldRequest) int {
		return cmp.Or(strings.Compare(a.Target, b.Target), strings.Compare(a.RequestID, b.RequestID))
	})
	return out
}

// Release is the release barrier (design 2.10): it closes admission,
// cancels the running operations, completes every pending control item
// through the control lane (continues held requests at the stage of each
// hold, resumes the debugger of each paused target and targets paused on
// start), waits until the running operations have released the page, runs
// cleanup on the control lane, and ends the lease.
// Queued operations fail with ErrLeaseEnded.
//
// Debugger pauses are drained over the whole release window (pauseDrain):
// a pause that arrives while the release waits for running operations or
// while its cleanup runs is resumed too, and the lease ends in the critical
// section that finds no pause to resume and no resume in flight. A failed
// release returns every failure joined, with the targets that are still
// paused named.
func (s *Scheduler) Release(ctx context.Context, cleanup func(ctx context.Context, c webkit.Caller) error) error {
	s.mu.Lock()
	s.barriers++
	var running []*Op
	for op := range s.running {
		running = append(running, op)
	}
	// Holds with a completion in flight too: that completion can fail and
	// leave the request paused.
	requests := make([]holdKey, 0, len(s.intercepted))
	for k := range s.intercepted {
		requests = append(requests, k)
	}
	s.mu.Unlock()

	for _, op := range running {
		op.Cancel(fmt.Errorf("%w: released", ErrLeaseEnded))
	}
	var errs []error
	// A refusal means that another sender completed the item after the
	// snapshot above; the item no longer needs the barrier.
	note := func(err error) {
		if err != nil && !errors.Is(err, ErrControlRefused) {
			errs = append(errs, err)
		}
	}
	pauses := newPauseDrain(s)
	// failed joins the failures with the paused targets (pauseDrain.remaining;
	// cause is the end of ctx while a resume was in flight, or nil).
	failed := func(cause error) error {
		return errors.Join(append(errs, pauses.remaining(cause))...)
	}
	for _, k := range requests {
		note(s.answerHold(ctx, k, false))
	}
	_ = pauses.run(ctx) // failed reports what stays paused
	if pt, ok := s.c.(interface{ PausedTargets() []string }); ok {
		for _, id := range pt.PausedTargets() {
			_, err := s.control(ctx, ControlMsg{Kind: ControlTargetResume, TargetID: id})
			note(err)
		}
	}
	for {
		// The pause state, the running operations and the channel to wait
		// on are read in one critical section, so a pause or the end of an
		// operation after the check closes the channel captured here.
		s.mu.Lock()
		n, changed := len(s.running), s.changed
		_, ok, inflight := pauses.nextLocked()
		s.mu.Unlock()
		if (ok || inflight) && ctx.Err() == nil {
			// A pause that arrived meanwhile (a running evaluation that
			// hit a breakpoint) is resumed, so the operation can end.
			_ = pauses.run(ctx)
			continue
		}
		if afterPausePending != nil {
			afterPausePending()
		}
		if n == 0 {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			note(fmt.Errorf("release: waiting for running operations: %w", ctx.Err()))
			return failed(nil)
		}
	}
	if cleanup != nil {
		note(cleanup(ctx, controlCaller{s.c}))
	}
	// A pause that arrived during the cleanup is drained before the lease
	// ends; the lease ends in the critical section that finds nothing left
	// to resume or to wait for (or when ctx ended).
	for {
		s.mu.Lock()
		_, ok, inflight := pauses.nextLocked()
		if (!ok && !inflight) || ctx.Err() != nil {
			s.released = true
			s.holder = ""
			s.barriers--
			s.pumpLocked() // fails the queued operations with ErrLeaseEnded
			s.mu.Unlock()
			break
		}
		s.mu.Unlock()
		_ = pauses.run(ctx)
	}
	return failed(ctx.Err())
}

// afterPausePending, when set (tests only), is called by Release after it
// found no pause to drain and before it waits for the running operations.
var afterPausePending func()

// maxBarrierContinues bounds the continues that a barrier sends for one
// held request whose continue fails without closing it (not sent, or no
// reply). A repeated continue is safe: WebKit takes a hold once and answers
// "Missing pending intercept ..." afterwards (requestGone).
const maxBarrierContinues = 3

// answerHold is a barrier's answer to the held request k: it waits until
// no completion of k is in flight, then continues the hold unmodified at
// its stage on its own target, and repeats until the target holds no
// request k.id. A hold that a completion's failure left open, and a newer
// hold of the same id (the response stage after a request-stage
// continue), get the continue too; a hold that its target's destruction
// dropped needs none. With reset (the Network reinitialization barrier) the
// hold is recorded as reset in the critical section that finds it closed,
// and in the one in which its continue closes it (completeHold). It
// returns the last error after maxBarrierContinues failed continues of one
// hold, or when ctx ends while it waits.
func (s *Scheduler) answerHold(ctx context.Context, k holdKey, reset bool) error {
	failed := 0
	var seq uint64
	for {
		s.mu.Lock()
		h := s.intercepted[k]
		for h != nil && h.inflight {
			changed := s.changed
			s.mu.Unlock()
			select {
			case <-changed:
			case <-ctx.Done():
				return fmt.Errorf("waiting for the completion of intercepted request %q on page target %q: %w", k.id, k.target, ctx.Err())
			}
			s.mu.Lock()
			h = s.intercepted[k]
		}
		if h == nil {
			if reset {
				s.markResetLocked(k)
			}
			s.mu.Unlock()
			return nil
		}
		if h.seq != seq {
			seq, failed = h.seq, 0 // another hold of the id: its own budget
		}
		s.mu.Unlock()
		_, err := s.completeHold(ctx, k, ControlMsg{Kind: ControlCompleteRequest, TargetID: k.target, RequestID: k.id, Method: "Network.interceptContinue"}, reset)
		if err == nil || requestGone(err) || errors.Is(err, ErrControlRefused) {
			continue // answered, or another sender started a completion: look again
		}
		if failed++; failed >= maxBarrierContinues {
			return fmt.Errorf("continuing intercepted request %q on page target %q: %w", k.id, k.target, err)
		}
	}
}

// Barrier closes admission and waits, at most until ctx ends, until no
// operation runs (design 2.10: background recovery waits for the running
// operation). Queued operations stay queued. reopen opens admission again;
// it is also called when the wait fails.
func (s *Scheduler) Barrier(ctx context.Context) (reopen func(), err error) {
	s.mu.Lock()
	s.barriers++
	s.mu.Unlock()
	var once sync.Once
	reopen = func() {
		once.Do(func() {
			s.mu.Lock()
			s.barriers--
			s.pumpLocked()
			s.mu.Unlock()
		})
	}
	for {
		s.mu.Lock()
		n, changed := len(s.running), s.changed
		s.mu.Unlock()
		if n == 0 {
			return reopen, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			reopen()
			return nil, fmt.Errorf("barrier: waiting for %d running operations: %w", n, ctx.Err())
		}
	}
}

// clockMap returns the clock mapping of k (a document generation and an
// execution context).
func (s *Scheduler) clockMap(k clockKey) *ClockMap {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.clocks[k]
	if m == nil {
		m = &ClockMap{}
		s.clocks[k] = m
	}
	return m
}

// dropClock forgets m, the mapping of k, after the page showed another
// document under the same generation; the next attempt measures the page
// again.
func (s *Scheduler) dropClock(k clockKey, m *ClockMap) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clocks[k] == m {
		delete(s.clocks, k)
	}
}
