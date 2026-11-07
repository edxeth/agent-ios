package webkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
)

// Errors of a Session (design docs/design/agentic-parity.md, sections 2.6 and 2.7).
var (
	// ErrNoTarget: the connection closed or stayed silent without
	// Target.targetCreated. There is no direct-mode fallback.
	ErrNoTarget = errors.New("page did not announce its target; it may be held by another inspector or not visible")
	// ErrTargetSwapped: the call was addressed to a target that a
	// cross-origin navigation replaced before the reply arrived.
	ErrTargetSwapped = errors.New("the page swapped to a new process target before the reply arrived; the page may have executed the command")
	// ErrTargetAbandoned: the call was addressed to a provisional target
	// that was destroyed without commit.
	ErrTargetAbandoned = errors.New("the provisional target was destroyed before commit")
	// ErrPageClosed: the active target was destroyed without a swap.
	ErrPageClosed = errors.New("page closed")
	// ErrSessionClosed: the page connection ended.
	ErrSessionClosed = errors.New("connection to page closed")
	// ErrCallTimeout: the call budget passed without a reply.
	ErrCallTimeout = errors.New("WebKit accepted the command and did not answer")
	// ErrSessionStalled: the call and the liveness probe got no reply. The
	// daemon's background recovery (W1b) returns it.
	ErrSessionStalled = errors.New("page stopped answering, including the liveness probe")
	// ErrNotSent marks a failure before anything of the message was written:
	// the page never saw it, so the caller may treat the command as not run.
	ErrNotSent = errors.New("not sent")
	// ErrTargetNotPaused: Target.resume was asked for a target that is not
	// paused on start, or whose resume is already being written.
	ErrTargetNotPaused = errors.New("target is not paused on start")
)

// notSentError marks err as a failure before the write (errors.Is(err,
// ErrNotSent)) and keeps err's text and chain.
type notSentError struct{ err error }

func (e notSentError) Error() string        { return e.err.Error() }
func (e notSentError) Unwrap() error        { return e.err }
func (e notSentError) Is(target error) bool { return target == ErrNotSent }

// notSent wraps err as a failure before the write; a write refused by the
// connection (transport.ErrNotWritten) counts as one.
func notSent(err error) error { return notSentError{err} }

// SetupError reports that the desired-state setup of a provisional target
// failed (design 2.7). The target was still resumed and may be active; it
// lacks part of its desired state until ReapplySetup succeeds.
type SetupError struct {
	Target string
	Err    error
}

func (e *SetupError) Error() string {
	return fmt.Sprintf("setup of page target %s failed: %v; the page lacks part of its desired state (bootstrap script, interception, overrides) until it is applied again", e.Target, e.Err)
}

func (e *SetupError) Unwrap() error { return e.Err }

// WireLimitError reports a message larger than the transport accepts. It is
// returned before anything is written.
type WireLimitError struct {
	What  string // the method or payload, for example "Page.setBootstrapScript"
	Size  int    // serialized outer message, in bytes
	Limit int    // transport.Limits.MaxOutgoing
}

func (e *WireLimitError) Error() string {
	return fmt.Sprintf("%s is %d bytes on the wire; the limit for this transport is %d", e.What, e.Size, e.Limit)
}

// Step budgets of design 2.6.
const (
	DefaultTargetWait  = 3 * time.Second
	DefaultCallTimeout = 10 * time.Second
	DefaultNudgeAfter  = 250 * time.Millisecond
	DefaultResumeAfter = 2 * time.Second
	DefaultProbeBudget = 2 * time.Second
	DefaultEventQueue  = 1024
	// setupBudget bounds the desired-state setup of one provisional target.
	setupBudget = 10 * time.Second
	// closeResumeBudget bounds the Target.resume calls that Close sends.
	closeResumeBudget = 500 * time.Millisecond
)

// SessionOptions configures a Session. Zero fields take the defaults above.
type SessionOptions struct {
	TargetWait  time.Duration // wait for Target.targetCreated
	CallTimeout time.Duration // budget of one protocol call
	// Nudge sends one Runtime.evaluate "0" when a call has no reply after
	// NudgeAfter (iOS 26 holds replies until the next message, MAP fact 7).
	Nudge      bool
	NudgeAfter time.Duration
	// PauseOnStart sends Target.setPauseOnStart(true) at connect; each paused
	// target is resumed after its Setup or after ResumeAfter, whichever comes
	// first, and on Close.
	PauseOnStart bool
	ResumeAfter  time.Duration
	// Setup sends the desired state to a new provisional target before commit
	// (design 2.7). It runs in its own goroutine; its ctx ends when the target
	// is destroyed or the session closes.
	Setup func(ctx context.Context, t TargetCaller) error
	// AgentReset returns the calls that bring the agent of a domain to
	// WebKit's baseline. Before the first <Domain>.enable that the session
	// sends to a target whose agent state it does not know (AgentState),
	// it writes these calls and then the enable in one round trip
	// (sendWithReset). nil: <Domain>.disable alone.
	AgentReset func(domain string) []Call
	// EventQueue is the capacity of each subscriber's queue. A full queue
	// drops the event (DroppedEvents counts them); it never blocks the read loop.
	EventQueue int
}

func (o SessionOptions) withDefaults() SessionOptions {
	if o.TargetWait <= 0 {
		o.TargetWait = DefaultTargetWait
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.NudgeAfter <= 0 {
		o.NudgeAfter = DefaultNudgeAfter
	}
	if o.ResumeAfter <= 0 {
		o.ResumeAfter = DefaultResumeAfter
	}
	if o.EventQueue <= 0 {
		o.EventQueue = DefaultEventQueue
	}
	return o
}

type callBudgetKey struct{}

// WithCallBudget returns ctx with a per-call budget that replaces
// SessionOptions.CallTimeout for Session sends made with it. The daemon
// uses it for legacy raw calls, which W0 bounds only by the command
// deadline (a heap snapshot can take minutes).
func WithCallBudget(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, callBudgetKey{}, d)
}

// HangsThroughTarget reports whether method gets no reply through Target
// routing (CSS.enable, CSS.getAllStyleSheets, CSS.getStyleSheetText).
func HangsThroughTarget(method string) bool { return hangsThroughTarget[method] }

// TargetCaller sends commands to one specific target, for example a
// provisional target during setup.
type TargetCaller interface {
	TargetID() string
	Send(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// Call is one protocol command of a pipelined batch (SendPipelined,
// SessionOptions.AgentReset).
type Call struct {
	Method string
	Params any
}

type reply struct {
	result json.RawMessage
	err    error
}

type pendingCall struct {
	ch     chan reply
	target string // "" for a call on the outer connection
	method string
}

type targetState struct {
	id          string
	provisional bool
	paused      bool // paused on start and not resumed yet
	resuming    bool // a Target.resume is being written
	destroyed   bool
	cancelSetup context.CancelFunc
	// setupDone is closed when the provisional setup ends (nil: no setup
	// ran); setupErr is its result, or that of the last ReapplySetup.
	setupDone chan struct{}
	setupErr  error
}

// AgentState is what a Session knows about the agent of one domain on a
// target's backend, from the replies to the <Domain>.enable and
// <Domain>.disable calls it sent to that target (design 2.4 and 2.7). A
// target that a commit replaced keeps its record: when a back or forward
// navigation restores a page from the back/forward cache, WebKit announces
// its old target id again, and its backend kept every agent and its state,
// because the commit away never disconnected it (WebKitGTK 2.54 and iOS
// 16.x; docs/notes/w1b.md, "Back and forward across sites").
type AgentState uint8

const (
	// AgentEnabled: an enable succeeded, or WebKit refused it as already
	// enabled, and no disable was sent after it.
	AgentEnabled AgentState = iota + 1
	// AgentMaybeEnabled: an enable or a disable was written and its reply
	// was lost (call timeout, cancellation, target swap), so the agent can
	// be enabled.
	AgentMaybeEnabled
	// AgentDisabled: a disable succeeded, or WebKit refused it because the
	// agent was not enabled (NotEnabled), and no enable was sent after it.
	AgentDisabled
)

// NotEnabled reports a refusal because the domain's agent is not enabled
// ("not enabled", "must be enabled", "already disabled"): the agent is in
// the state that a disable wants.
func NotEnabled(err error) bool {
	var pe *ErrorData
	if !errors.As(err, &pe) {
		return false
	}
	m := strings.ToLower(pe.Message)
	return strings.Contains(m, "not enabled") || strings.Contains(m, "must be enabled") || strings.Contains(m, "already disabled")
}

type subscriber struct {
	h     EventHandler
	queue chan event
	stop  chan struct{}
}

type event struct {
	method string
	params json.RawMessage
}

// Session is one Target-routed page connection on a transport.MessageConn:
// Target routing with no direct fallback, active and setup targets,
// provisional swaps, document generations, call deadlines, the reply nudge,
// wire-byte limits and bounded per-subscriber event queues.
type Session struct {
	conn transport.MessageConn
	opts SessionOptions

	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[int64]*pendingCall
	wrapOf   map[int64]int64 // outer Target.sendMessageToTarget id -> inner id
	active   string
	targets  map[string]*targetState
	closeErr error
	// Document generation (design 2.7): a document is (target, main-frame loader).
	gen           uint64
	docTarget     string
	docLoader     string
	docFresh      bool              // committed; its frameNavigated is not a new document
	pendingLoader map[string]string // provisional target -> loader seen before commit

	ready     chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	subMu    sync.RWMutex
	subs     map[string]map[int]*subscriber
	syncSubs map[string]map[int]EventHandler
	nextSub  int

	// agents is the AgentState record per target id and domain. It
	// outlives the target's destruction (see AgentState).
	agents map[string]map[string]AgentState

	dropped   atomic.Int64
	malformed atomic.Int64
	workers   atomic.Int64 // running subscriber goroutines
}

// Dial opens a connection with open and starts a Session on it. A connection
// that closes or stays silent without announcing a target is a failed
// attempt; one more attempt is made if ctx has time left (design 2.6).
func Dial(ctx context.Context, open func(context.Context) (transport.MessageConn, error), o SessionOptions) (*Session, error) {
	var firstErr error
	for attempt := 1; attempt <= 2; attempt++ {
		conn, err := open(ctx)
		if err != nil {
			return nil, err
		}
		s, err := NewSession(ctx, conn, o)
		if err == nil {
			return s, nil
		}
		if !errors.Is(err, ErrNoTarget) || ctx.Err() != nil {
			return nil, err
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, fmt.Errorf("2 connection attempts: %w", firstErr)
}

// NewSession starts a Session on conn and waits for Target.targetCreated, at
// most TargetWait and never past ctx. On failure conn is closed.
func NewSession(ctx context.Context, conn transport.MessageConn, o SessionOptions) (*Session, error) {
	s := &Session{
		conn:          conn,
		opts:          o.withDefaults(),
		pending:       make(map[int64]*pendingCall),
		wrapOf:        make(map[int64]int64),
		targets:       make(map[string]*targetState),
		pendingLoader: make(map[string]string),
		agents:        make(map[string]map[string]AgentState),
		ready:         make(chan struct{}),
		done:          make(chan struct{}),
		subs:          make(map[string]map[int]*subscriber),
		syncSubs:      make(map[string]map[int]EventHandler),
	}
	go s.readLoop()

	start := time.Now()
	wait := time.NewTimer(s.opts.TargetWait)
	defer wait.Stop()
	select {
	case <-s.ready:
	case <-wait.C:
		s.shutdown(ErrNoTarget)
		return nil, fmt.Errorf("no Target.targetCreated after %s: %w", time.Since(start).Round(time.Millisecond), ErrNoTarget)
	case <-s.done:
		return nil, fmt.Errorf("connection closed after %s before a target was announced: %w", time.Since(start).Round(time.Millisecond), ErrNoTarget)
	case <-ctx.Done():
		s.shutdown(ctx.Err())
		return nil, fmt.Errorf("waiting for the page target: %w", ctxErr(ctx))
	}
	if s.opts.PauseOnStart {
		if _, err := s.sendOuter(ctx, "Target.setPauseOnStart", map[string]bool{"pauseOnStart": true}); err != nil {
			s.shutdown(err)
			return nil, fmt.Errorf("enabling pause on start: %w", err)
		}
	}
	return s, nil
}

// Send sends an ordinary command to the active target and waits for its
// reply, at most CallTimeout and never past ctx. It never sends to a target
// whose provisional setup failed or still runs (design 2.7): it waits for a
// running setup, within the same budget, and fails with ErrNotSent and a
// *SetupError when the setup failed. The target is chosen and its setup
// checked in one step, so a swap cannot commit between them.
func (s *Session) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	target, _ := TargetOf(ctx)
	return s.sendTo(ctx, target, true, method, params)
}

type targetKey struct{}

// WithTarget makes Session.Send address the target id instead of the active
// target at send time, with the same setup gate. A target that a commit
// replaced fails the call with ErrTargetSwapped (with ErrNotSent when it was
// gone before the write). Other callers ignore it.
func WithTarget(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, targetKey{}, id)
}

// TargetOf returns the target that WithTarget set on ctx.
func TargetOf(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(targetKey{}).(string)
	return id, ok && id != ""
}

// SendControl sends method to the active target like Send, without the
// setup check: control-lane messages (design 2.10) and the liveness probe
// must reach a target whose setup failed, so that held requests, pauses and
// recovery can complete.
func (s *Session) SendControl(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return s.sendTo(ctx, "", false, method, params)
}

// IsTargetRouted is always true: a Session has no direct mode.
func (s *Session) IsTargetRouted() bool { return true }

// Done is closed when the connection ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns why the session ended, or nil while it runs.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}

// ActiveTarget returns the id of the target that receives command traffic.
func (s *Session) ActiveTarget() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Generation identifies the current document. It increases once per new
// document (commit of a swap, or a main-frame navigation to a new loader),
// never for same-document navigations.
func (s *Session) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// Limits returns the limits of the underlying connection.
func (s *Session) Limits() transport.Limits { return s.conn.Limits() }

// DroppedEvents counts events dropped because a subscriber queue was full.
func (s *Session) DroppedEvents() int64 { return s.dropped.Load() }

// MalformedMessages counts incoming messages that could not be parsed.
func (s *Session) MalformedMessages() int64 { return s.malformed.Load() }

// Probe checks that the page answers: Runtime.evaluate "1" within
// DefaultProbeBudget and ctx.
func (s *Session) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, DefaultProbeBudget)
	defer cancel()
	_, err := s.SendControl(ctx, "Runtime.evaluate", map[string]any{"expression": "1", "returnByValue": true})
	if err != nil {
		return fmt.Errorf("liveness probe: %w", err)
	}
	return nil
}

// PausedTargets lists targets that are paused on start and not resumed yet.
func (s *Session) PausedTargets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, t := range s.targets {
		if pausedLocked(t) {
			out = append(out, id)
		}
	}
	return out
}

// pausedLocked reports whether t waits for a Target.resume that nobody is
// writing yet.
func pausedLocked(t *targetState) bool {
	return t != nil && t.paused && !t.resuming && !t.destroyed
}

// IsPaused reports whether target id is paused on start and not resumed.
func (s *Session) IsPaused(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return pausedLocked(s.targets[id])
}

// ResumeTarget sends Target.resume for a target paused on start, at most
// once per target. It returns ErrTargetNotPaused for a target that is not
// paused or whose resume is being written. A resume that was never written
// (an error wrapping ErrNotSent) leaves the target paused, so Close and the
// release barrier resume it later; any other outcome ends the pause.
func (s *Session) ResumeTarget(ctx context.Context, id string) error {
	s.mu.Lock()
	t := s.targets[id]
	if !pausedLocked(t) {
		s.mu.Unlock()
		return fmt.Errorf("Target.resume for %s: %w", id, ErrTargetNotPaused)
	}
	t.resuming = true
	s.mu.Unlock()
	_, err := s.sendOuter(ctx, "Target.resume", map[string]string{"targetId": id})
	s.mu.Lock()
	t.resuming = false
	if !errors.Is(err, ErrNotSent) {
		t.paused = false
	}
	s.mu.Unlock()
	return err
}

// AgentStates returns what the session knows about the agents of target's
// backend, from the enables and disables it sent to target (AgentState).
// The session knows nothing about a domain without an entry: an empty
// record does not mean a fresh backend (needsResetLocked).
func (s *Session) AgentStates(target string) map[string]AgentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.agents[target])
}

// needsResetLocked reports whether method is an enable whose agent on
// target the session must reset first: it never sent that agent a call
// with a known outcome (no record entry), or the last outcome was lost
// (AgentMaybeEnabled). A connection cannot tell a fresh backend from one
// that kept its agents: a page restored from the back/forward cache after
// the connection that enabled them closed, or a target that existed
// before the connection, arrive like any new target (docs/notes/w1b.md,
// "unknown backends"). An empty record never means a fresh backend.
func (s *Session) needsResetLocked(target, method string) bool {
	dom, verb, _ := strings.Cut(method, ".")
	if verb != "enable" {
		return false
	}
	st, ok := s.agents[target][dom]
	return !ok || st == AgentMaybeEnabled
}

// noteAgent updates target's AgentState record after a call of method was
// written to it, or its write failed, and ended with err.
func (s *Session) noteAgent(target, method string, err error) {
	dom, verb, _ := strings.Cut(method, ".")
	if verb != "enable" && verb != "disable" {
		return
	}
	var pe *ErrorData
	refused := errors.As(err, &pe)
	already := refused && verb == "enable" && strings.Contains(strings.ToLower(pe.Message), "already enabled")
	off := verb == "disable" && (err == nil || NotEnabled(err))
	if errors.Is(err, ErrNotSent) || refused && !already && !off {
		return // the backend did not change
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.agents[target]
	if rec == nil {
		rec = make(map[string]AgentState)
		s.agents[target] = rec
	}
	switch {
	case verb == "enable" && (err == nil || already):
		rec[dom] = AgentEnabled
	case off:
		rec[dom] = AgentDisabled
	case verb == "disable" || rec[dom] != AgentEnabled:
		rec[dom] = AgentMaybeEnabled
	}
}

// ActiveSetup waits until the provisional setup of the active target has
// ended, at most until ctx ends, and returns its failure as a *SetupError.
// It returns nil when the setup succeeded or the active target had none
// (the first target, or no SessionOptions.Setup).
func (s *Session) ActiveSetup(ctx context.Context) error {
	s.mu.Lock()
	t := s.targets[s.active]
	var done chan struct{}
	if t != nil {
		done = t.setupDone
	}
	s.mu.Unlock()
	if t == nil {
		return nil
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("waiting for the setup of page target %s: %w", t.id, ctxErr(ctx))
		}
	}
	s.mu.Lock()
	err := t.setupErr
	s.mu.Unlock()
	if err != nil {
		return &SetupError{Target: t.id, Err: err}
	}
	return nil
}

// ReapplySetup runs SessionOptions.Setup again on the active target, after
// its provisional setup has ended, and records the result: success clears
// the failure that ActiveSetup reports. The page state it sends arrives
// after the document started, so it cannot run before page scripts.
func (s *Session) ReapplySetup(ctx context.Context) error {
	if s.opts.Setup == nil {
		return nil
	}
	s.mu.Lock()
	t := s.targets[s.active]
	s.mu.Unlock()
	if t == nil {
		return fmt.Errorf("reapplying the page setup: %w", ErrNoTarget)
	}
	if err := s.ActiveSetup(ctx); err != nil && !errors.As(err, new(*SetupError)) {
		return err
	}
	err := s.opts.Setup(ctx, targetCaller{s: s, id: t.id})
	s.mu.Lock()
	t.setupErr = err
	s.mu.Unlock()
	if err != nil {
		return &SetupError{Target: t.id, Err: err}
	}
	return nil
}

// Close resumes every target still paused on start, then closes the
// connection. Pending calls fail with ErrSessionClosed.
func (s *Session) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeResumeBudget)
	for _, id := range s.PausedTargets() {
		_ = s.ResumeTarget(ctx, id)
	}
	cancel()
	s.shutdown(ErrSessionClosed)
	return nil
}

// OnEvent registers a handler for the life of the session.
func (s *Session) OnEvent(method string, h EventHandler) { s.Subscribe(method, h) }

// Subscribe registers h for method (AllEvents for every event). Each
// subscriber has its own bounded queue and goroutine, so a slow handler
// never delays replies or other subscribers. An event that arrived before a
// reply can be handled after Send returns.
func (s *Session) Subscribe(method string, h EventHandler) func() {
	select {
	case <-s.done:
		return func() {} // a closed session delivers no events
	default:
	}
	sub := &subscriber{h: h, queue: make(chan event, s.opts.EventQueue), stop: make(chan struct{})}
	s.subMu.Lock()
	id := s.nextSub
	s.nextSub++
	if s.subs[method] == nil {
		s.subs[method] = make(map[int]*subscriber)
	}
	s.subs[method][id] = sub
	s.subMu.Unlock()
	s.workers.Add(1)
	go func() {
		defer s.workers.Add(-1)
		for {
			select {
			case ev := <-sub.queue:
				sub.h(ev.method, ev.params)
			case <-sub.stop:
				return
			case <-s.done:
				return // queued events of a closed session are dropped
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.subMu.Lock()
			delete(s.subs[method], id)
			s.subMu.Unlock()
			close(sub.stop)
		})
	}
}

// SubscribeSync registers h to run inside the read loop, before the event
// reaches queued subscribers. h must not block and must not call Send; it is
// for bookkeeping that later readers of the same event rely on (for
// example the scheduler's pending control items).
func (s *Session) SubscribeSync(method string, h EventHandler) func() {
	s.subMu.Lock()
	id := s.nextSub
	s.nextSub++
	if s.syncSubs[method] == nil {
		s.syncSubs[method] = make(map[int]EventHandler)
	}
	s.syncSubs[method][id] = h
	s.subMu.Unlock()
	return func() {
		s.subMu.Lock()
		delete(s.syncSubs[method], id)
		s.subMu.Unlock()
	}
}

func (s *Session) dispatch(method string, params json.RawMessage) {
	s.subMu.RLock()
	defer s.subMu.RUnlock()
	for _, key := range [2]string{method, AllEvents} {
		for _, h := range s.syncSubs[key] {
			h(method, params)
		}
	}
	for _, key := range [2]string{method, AllEvents} {
		for _, sub := range s.subs[key] {
			select {
			case sub.queue <- event{method, params}:
			default:
				s.dropped.Add(1)
			}
		}
	}
}

// sendTo sends method to target ("" = the active target at send time).
// With gate, the target must have a finished, successful setup (Send).
// The first <Domain>.enable to a target whose agent state the session does
// not know goes out after that agent's reset, in one round trip
// (sendWithReset).
func (s *Session) sendTo(ctx context.Context, target string, gate bool, method string, params any) (json.RawMessage, error) {
	if hangsThroughTarget[method] {
		return nil, notSent(fmt.Errorf("%s: %w", method, ErrHangsThroughTarget))
	}
	callCtx, cancel := s.callContext(ctx)
	defer cancel()
	start := time.Now()
	rawParams, err := marshalParams(method, params)
	if err != nil {
		return nil, err
	}
	target, reset, err := s.resolve(ctx, callCtx, target, gate, method)
	if err != nil {
		return nil, err
	}
	if reset {
		return s.sendWithReset(ctx, callCtx, start, target, method, rawParams)
	}
	in, err := s.write(callCtx, target, method, rawParams)
	if err != nil {
		return nil, err
	}
	return s.await(ctx, callCtx, start, in)
}

// callContext bounds one call, or one pipelined batch, by the call budget.
func (s *Session) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := s.opts.CallTimeout
	if d, ok := ctx.Value(callBudgetKey{}).(time.Duration); ok && d > 0 {
		budget = d
	}
	return context.WithTimeout(ctx, budget)
}

func marshalParams(method string, params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("%s: marshaling params: %w", method, err)
	}
	return raw, nil
}

// resolve picks the target of a call ("" = the active target), waits for
// its running setup with gate, and reports whether method is an enable
// whose agent must be reset first (needsResetLocked).
func (s *Session) resolve(ctx, callCtx context.Context, target string, gate bool, method string) (dest string, reset bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.closeErr != nil {
			return "", false, notSent(fmt.Errorf("%s: %w", method, s.closeErr))
		}
		dest = target
		if dest == "" {
			dest = s.active
		}
		t := s.targets[dest]
		if err := targetErr(t); err != nil {
			return "", false, notSent(fmt.Errorf("%s to %s: %w", method, dest, err))
		}
		if gate {
			running, err := setupState(t)
			if err != nil {
				return "", false, notSent(fmt.Errorf("%s to %s: %w", method, dest, err))
			}
			if running {
				done := t.setupDone
				s.mu.Unlock()
				var werr error
				select {
				case <-done:
				case <-callCtx.Done():
					werr = notSent(fmt.Errorf("%s: waiting for the setup of page target %s: %w", method, dest, callCtx.Err()))
				case <-s.done:
					werr = notSent(fmt.Errorf("%s: %w", method, s.Err()))
				}
				s.mu.Lock()
				if werr != nil {
					return "", false, werr
				}
				continue // the active target may have changed meanwhile
			}
		}
		return dest, s.needsResetLocked(dest, method), nil
	}
}

// inflight is a call written to a target whose reply is awaited.
type inflight struct {
	call             *pendingCall
	innerID, outerID int64
	target, method   string
}

// write registers and writes one call to target without waiting for its
// reply. A write that fails is recorded (noteAgent) and its registration
// removed.
func (s *Session) write(callCtx context.Context, target, method string, rawParams json.RawMessage) (*inflight, error) {
	innerID := s.nextID.Add(1)
	inner, err := json.Marshal(Message{ID: innerID, Method: method, Params: rawParams})
	if err != nil {
		return nil, fmt.Errorf("%s: marshaling message: %w", method, err)
	}
	outerID := s.nextID.Add(1)
	outerParams, err := json.Marshal(map[string]string{"targetId": target, "message": string(inner)})
	if err != nil {
		return nil, fmt.Errorf("%s: marshaling outer params: %w", method, err)
	}
	wire, err := json.Marshal(Message{ID: outerID, Method: "Target.sendMessageToTarget", Params: outerParams})
	if err != nil {
		return nil, fmt.Errorf("%s: marshaling outer message: %w", method, err)
	}
	if lim := s.conn.Limits().MaxOutgoing; lim > 0 && len(wire) > lim {
		return nil, notSent(&WireLimitError{What: method, Size: len(wire), Limit: lim})
	}

	in := &inflight{call: &pendingCall{ch: make(chan reply, 1), target: target, method: method}, innerID: innerID, outerID: outerID, target: target, method: method}
	s.mu.Lock()
	// Re-check under the lock: the target may have been destroyed or
	// swapped while the message was built. Nothing is written to it then.
	if err := targetErr(s.targets[target]); err != nil {
		s.mu.Unlock()
		return nil, notSent(fmt.Errorf("%s to %s: %w", method, target, err))
	}
	s.pending[innerID] = in.call
	s.wrapOf[outerID] = innerID
	s.mu.Unlock()

	if err := s.conn.Write(callCtx, wire); err != nil {
		err = writeErr(method, err)
		s.finish(in, err)
		return nil, err
	}
	return in, nil
}

// finish removes a call's registration and records its outcome.
func (s *Session) finish(in *inflight, err error) {
	s.mu.Lock()
	delete(s.pending, in.innerID)
	delete(s.wrapOf, in.outerID)
	s.mu.Unlock()
	s.noteAgent(in.target, in.method, err)
}

// await waits for the reply of a written call, at most until callCtx ends.
func (s *Session) await(ctx, callCtx context.Context, start time.Time, in *inflight) (_ json.RawMessage, err error) {
	defer func() { s.finish(in, err) }()
	var nudge <-chan time.Time
	if s.opts.Nudge {
		t := time.NewTimer(s.opts.NudgeAfter)
		defer t.Stop()
		nudge = t.C
	}
	for {
		select {
		case r := <-in.call.ch:
			if r.err != nil {
				return nil, fmt.Errorf("%s: %w", in.method, r.err)
			}
			return r.result, nil
		case <-nudge:
			nudge = nil
			s.sendNudge(callCtx, in.target)
		case <-callCtx.Done():
			elapsed := time.Since(start).Round(time.Millisecond)
			if ctx.Err() == nil {
				return nil, fmt.Errorf("%s: no reply after %s: %w", in.method, elapsed, ErrCallTimeout)
			}
			return nil, fmt.Errorf("%s: no response after %s: %w", in.method, elapsed, ctxErr(ctx))
		case <-s.done:
			return nil, fmt.Errorf("%s: %w", in.method, s.Err())
		}
	}
}

// pipeline writes calls to target in order without waiting between them,
// then collects every reply (one round trip; WebKit runs a target's
// commands in the order they arrive). A write that fails ends the batch:
// the later calls are not written and fail with ErrNotSent.
func (s *Session) pipeline(ctx, callCtx context.Context, start time.Time, target string, calls []Call) ([]json.RawMessage, []error) {
	res := make([]json.RawMessage, len(calls))
	errs := make([]error, len(calls))
	ins := make([]*inflight, len(calls))
	for i, c := range calls {
		raw, err := marshalParams(c.Method, c.Params)
		if err == nil {
			ins[i], err = s.write(callCtx, target, c.Method, raw)
		}
		if err != nil {
			errs[i] = err
			for j := i + 1; j < len(calls); j++ {
				errs[j] = notSent(fmt.Errorf("%s: not written after the failure of %s: %w", calls[j].Method, c.Method, err))
			}
			break
		}
	}
	for i, in := range ins {
		if in != nil {
			res[i], errs[i] = s.await(ctx, callCtx, start, in)
		}
	}
	return res, errs
}

// SendPipelined sends calls to the active target, or the one WithTarget
// names, like Send, but writes them in order without waiting between them
// and then collects the replies, so the batch costs one round trip. It
// returns each call's error; a write that fails ends the batch (pipeline).
// It sends no agent reset (sendWithReset): a caller that puts an enable in
// the batch resets that agent in the batch first, as core's Apply does.
func (s *Session) SendPipelined(ctx context.Context, calls []Call) []error {
	target, _ := TargetOf(ctx)
	return s.sendPipelined(ctx, target, true, calls)
}

func (s *Session) sendPipelined(ctx context.Context, target string, gate bool, calls []Call) []error {
	if len(calls) == 0 {
		return nil
	}
	callCtx, cancel := s.callContext(ctx)
	defer cancel()
	start := time.Now()
	errs := make([]error, len(calls))
	for _, c := range calls {
		if hangsThroughTarget[c.Method] {
			for j := range errs {
				errs[j] = notSent(fmt.Errorf("%s: batch with %s: %w", calls[j].Method, c.Method, ErrHangsThroughTarget))
			}
			return errs
		}
	}
	dest, _, err := s.resolve(ctx, callCtx, target, gate, calls[0].Method)
	if err != nil {
		for i := range errs {
			errs[i] = err
		}
		return errs
	}
	_, errs = s.pipeline(ctx, callCtx, start, dest, calls)
	return errs
}

// sendWithReset sends the enable method to target after the reset of its
// agent (SessionOptions.AgentReset), pipelined: the reset calls and the
// enable are written in order and their replies collected together. A
// reset call refused because the agent is not enabled is no failure
// (NotEnabled). Any other failure of a reset call fails the enable; it is
// reported without its protocol error type, because the reset changed the
// backend whatever the enable's own outcome. A reset call that ended
// without WebKit's answer (ErrCallTimeout, the caller's context, a closed
// session) keeps that error in the chain, because callers read it: hang
// detection probes after ErrCallTimeout, and an owner's cancellation is no
// stall. ErrNotSent is kept only when nothing of the batch was written.
func (s *Session) sendWithReset(ctx, callCtx context.Context, start time.Time, target, method string, rawParams json.RawMessage) (json.RawMessage, error) {
	dom, _, _ := strings.Cut(method, ".")
	calls := s.agentReset(dom)
	enable := Call{Method: method}
	if rawParams != nil {
		enable.Params = rawParams // a nil RawMessage would be sent as null
	}
	calls = append(calls[:len(calls):len(calls)], enable)
	res, errs := s.pipeline(ctx, callCtx, start, target, calls)
	last := len(calls) - 1
	var failed []string
	var cause error // the first failure without WebKit's answer
	for _, err := range errs[:last] {
		if err != nil && !NotEnabled(err) {
			var pe *ErrorData
			if cause == nil && !errors.As(err, &pe) && !errors.Is(err, ErrNotSent) {
				cause = err
				continue
			}
			failed = append(failed, err.Error())
		}
	}
	switch {
	case cause != nil:
		others := ""
		if len(failed) > 0 {
			others = "; " + strings.Join(failed, "; ")
		}
		return nil, fmt.Errorf("%s: resetting the %s agent, whose state this session did not know, failed: %w%s (the enable: %v)", method, dom, cause, others, errs[last])
	case len(failed) == 0:
		return res[last], errs[last]
	case errors.Is(errs[0], ErrNotSent):
		// Nothing of the batch reached the page.
		return nil, fmt.Errorf("%s: resetting the %s agent first: %w", method, dom, errs[0])
	default:
		return nil, fmt.Errorf("%s: resetting the %s agent, whose state this session did not know, failed: %s (the enable: %v)", method, dom, strings.Join(failed, "; "), errs[last])
	}
}

// agentReset returns the reset calls of dom's agent.
func (s *Session) agentReset(dom string) []Call {
	if s.opts.AgentReset != nil {
		return s.opts.AgentReset(dom)
	}
	return []Call{{Method: dom + ".disable"}}
}

// writeErr reports a failed write of method; a write that the connection
// refused before writing anything is a not-sent failure.
func writeErr(method string, err error) error {
	err = fmt.Errorf("%s: writing message: %w", method, err)
	if errors.Is(err, transport.ErrNotWritten) {
		return notSent(err)
	}
	return err
}

// targetErr says why a target cannot receive commands, or nil when it can.
// Destroyed targets stay in the map so that late sends get the right error.
func targetErr(t *targetState) error {
	switch {
	case t == nil:
		return ErrTargetSwapped
	case t.destroyed && t.provisional:
		return ErrTargetAbandoned
	case t.destroyed:
		return ErrTargetSwapped
	}
	return nil
}

// setupState reports, for a live target and with s.mu held, whether its
// provisional setup still runs (wait on t.setupDone), or its setup failure
// (the provisional setup or the last ReapplySetup) as a *SetupError.
func setupState(t *targetState) (running bool, err error) {
	if t.setupDone != nil {
		select {
		case <-t.setupDone:
		default:
			return true, nil
		}
	}
	if t.setupErr != nil {
		return false, &SetupError{Target: t.id, Err: t.setupErr}
	}
	return false, nil
}

// sendNudge writes Runtime.evaluate "0" to target without waiting; its reply
// has no pending entry and is dropped.
func (s *Session) sendNudge(ctx context.Context, target string) {
	inner, _ := json.Marshal(Message{ID: s.nextID.Add(1), Method: "Runtime.evaluate", Params: json.RawMessage(`{"expression":"0"}`)})
	outerParams, _ := json.Marshal(map[string]string{"targetId": target, "message": string(inner)})
	wire, _ := json.Marshal(Message{ID: s.nextID.Add(1), Method: "Target.sendMessageToTarget", Params: outerParams})
	_ = s.conn.Write(ctx, wire)
}

// sendOuter sends an unwrapped command on the outer connection (Target domain).
func (s *Session) sendOuter(ctx context.Context, method string, params any) (json.RawMessage, error) {
	callCtx, cancel := context.WithTimeout(ctx, s.opts.CallTimeout)
	defer cancel()
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("%s: marshaling params: %w", method, err)
	}
	id := s.nextID.Add(1)
	wire, err := json.Marshal(Message{ID: id, Method: method, Params: raw})
	if err != nil {
		return nil, fmt.Errorf("%s: marshaling message: %w", method, err)
	}
	if lim := s.conn.Limits().MaxOutgoing; lim > 0 && len(wire) > lim {
		return nil, notSent(&WireLimitError{What: method, Size: len(wire), Limit: lim})
	}
	call := &pendingCall{ch: make(chan reply, 1), method: method}
	s.mu.Lock()
	if s.closeErr != nil {
		err := s.closeErr
		s.mu.Unlock()
		return nil, notSent(fmt.Errorf("%s: %w", method, err))
	}
	s.pending[id] = call
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	if err := s.conn.Write(callCtx, wire); err != nil {
		return nil, writeErr(method, err)
	}
	select {
	case r := <-call.ch:
		if r.err != nil {
			return nil, fmt.Errorf("%s: %w", method, r.err)
		}
		return r.result, nil
	case <-callCtx.Done():
		return nil, fmt.Errorf("%s: no reply: %w", method, callCtx.Err())
	case <-s.done:
		return nil, fmt.Errorf("%s: %w", method, s.Err())
	}
}

// shutdown ends the session once: pending calls fail with err.
func (s *Session) shutdown(err error) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.closeErr == nil { // handleTargetDestroyed may have published ErrPageClosed
			s.closeErr = err
		}
		for _, t := range s.targets {
			if t.cancelSetup != nil {
				t.cancelSetup()
			}
		}
		s.mu.Unlock()
		close(s.done)
		_ = s.conn.Close()
	})
}

func (s *Session) readLoop() {
	for {
		data, err := s.conn.Read(context.Background())
		if err != nil {
			// Both errors stay in the chain: a transport that reports
			// ErrPageClosed (the direct transport) keeps that identity.
			s.shutdown(fmt.Errorf("%w: %w", ErrSessionClosed, err))
			return
		}
		s.handle(data)
	}
}

// wireMessage is an incoming message. Fields stay raw so that a reply with a
// wrong shape is reported to its caller instead of being dropped.
type wireMessage struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (s *Session) handle(data []byte) {
	var m wireMessage
	if err := json.Unmarshal(data, &m); err != nil {
		s.malformed.Add(1)
		return
	}
	if m.ID != nil {
		s.handleOuterReply(*m.ID, m)
		return
	}
	switch m.Method {
	case "Target.targetCreated":
		s.handleTargetCreated(m.Params)
	case "Target.didCommitProvisionalTarget":
		s.handleCommit(m.Params)
	case "Target.targetDestroyed":
		s.handleTargetDestroyed(m.Params)
	case "Target.dispatchMessageFromTarget":
		s.handleFromTarget(m.Params)
		return
	case "":
		s.malformed.Add(1)
		return
	}
	s.dispatch(m.Method, m.Params)
}

func (s *Session) handleOuterReply(id int64, m wireMessage) {
	s.mu.Lock()
	if innerID, wrapped := s.wrapOf[id]; wrapped {
		// The ack of Target.sendMessageToTarget is ignored unless it is an
		// error: then the inner command never reaches the page.
		call := s.pending[innerID]
		s.mu.Unlock()
		if call != nil && len(m.Error) > 0 && string(m.Error) != "null" {
			deliver(call, reply{err: replyError(m.Error)})
		}
		return
	}
	call := s.pending[id]
	s.mu.Unlock()
	if call != nil {
		deliver(call, decodeReply(m))
	}
}

func (s *Session) handleFromTarget(params json.RawMessage) {
	var p struct {
		TargetID string `json:"targetId"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		s.malformed.Add(1)
		return
	}
	var m wireMessage
	if err := json.Unmarshal([]byte(p.Message), &m); err != nil {
		s.malformed.Add(1)
		return
	}
	if m.ID != nil {
		s.mu.Lock()
		call := s.pending[*m.ID]
		s.mu.Unlock()
		if call != nil {
			deliver(call, decodeReply(m))
		}
		return
	}
	if m.Method == "" {
		s.malformed.Add(1)
		return
	}
	s.mu.Lock()
	t := s.targets[p.TargetID]
	live := t != nil && !t.destroyed
	s.mu.Unlock()
	if !live {
		return // events of a swapped-out or abandoned target
	}
	if m.Method == "Page.frameNavigated" {
		s.trackNavigation(p.TargetID, m.Params)
	}
	s.dispatch(m.Method, m.Params)
}

func deliver(call *pendingCall, r reply) {
	select {
	case call.ch <- r:
	default: // already answered (for example by an error ack)
	}
}

// decodeReply checks the shape of a reply: an error object, or a result
// object. Anything else is a malformed reply and an error for its caller.
func decodeReply(m wireMessage) reply {
	if len(m.Error) > 0 && string(m.Error) != "null" {
		return reply{err: replyError(m.Error)}
	}
	if len(m.Result) == 0 || m.Result[0] != '{' {
		return reply{err: fmt.Errorf("malformed reply: want a result object, got %.80q", string(m.Result))}
	}
	return reply{result: m.Result}
}

func replyError(raw json.RawMessage) error {
	var e ErrorData
	if err := json.Unmarshal(raw, &e); err != nil || e.Message == "" {
		return fmt.Errorf("malformed error reply: %.120q", string(raw))
	}
	return &e
}

type targetInfo struct {
	TargetID      string `json:"targetId"`
	Type          string `json:"type"`
	IsProvisional bool   `json:"isProvisional"`
	IsPaused      bool   `json:"isPaused"`
}

func (s *Session) handleTargetCreated(params json.RawMessage) {
	var p struct {
		TargetInfo targetInfo `json:"targetInfo"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.TargetInfo.TargetID == "" {
		s.malformed.Add(1)
		return
	}
	info := p.TargetInfo
	if info.Type != "" && info.Type != "page" {
		return // workers and other target kinds are not page targets
	}
	s.mu.Lock()
	if s.closeErr != nil {
		s.mu.Unlock()
		return
	}
	t := &targetState{id: info.TargetID, provisional: info.IsProvisional, paused: info.IsPaused}
	s.targets[info.TargetID] = t
	if !info.IsProvisional {
		if s.active == "" {
			s.active = info.TargetID
			s.gen = 1
			s.docTarget = info.TargetID
			close(s.ready)
		}
		s.mu.Unlock()
		if info.IsPaused {
			go s.resumeLater(info.TargetID, nil)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupBudget)
	t.cancelSetup = cancel
	t.setupDone = make(chan struct{})
	s.mu.Unlock()
	go s.setupTarget(ctx, cancel, t, info.IsPaused)
}

// setupTarget applies the desired state to a provisional target, records
// the result for ActiveSetup, and resumes the target when it was paused on
// start, whatever the result.
func (s *Session) setupTarget(ctx context.Context, cancel context.CancelFunc, t *targetState, paused bool) {
	defer cancel()
	if paused {
		go s.resumeLater(t.id, t.setupDone)
	}
	var err error
	if s.opts.Setup != nil {
		err = s.opts.Setup(ctx, targetCaller{s: s, id: t.id})
	}
	s.mu.Lock()
	t.setupErr = err
	s.mu.Unlock()
	close(t.setupDone)
}

// resumeLater resumes a paused target when setupDone closes (nil: no setup)
// or after ResumeAfter, whichever comes first.
func (s *Session) resumeLater(id string, setupDone <-chan struct{}) {
	t := time.NewTimer(s.opts.ResumeAfter)
	defer t.Stop()
	select {
	case <-setupDone:
	case <-t.C:
	case <-s.done:
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.CallTimeout)
	defer cancel()
	_ = s.ResumeTarget(ctx, id)
}

func (s *Session) handleCommit(params json.RawMessage) {
	var p struct {
		OldTargetID string `json:"oldTargetId"`
		NewTargetID string `json:"newTargetId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.NewTargetID == "" {
		s.malformed.Add(1)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	nt := s.targets[p.NewTargetID]
	if nt == nil {
		nt = &targetState{id: p.NewTargetID}
		s.targets[p.NewTargetID] = nt
	}
	nt.provisional = false
	if old := s.targets[p.OldTargetID]; old != nil {
		old.destroyed = true
		if old.cancelSetup != nil {
			old.cancelSetup()
		}
	}
	s.active = p.NewTargetID
	for _, call := range s.pending {
		if call.target == p.OldTargetID {
			deliver(call, reply{err: ErrTargetSwapped})
		}
	}
	if s.docTarget != p.NewTargetID {
		s.docTarget = p.NewTargetID
		s.docLoader = s.pendingLoader[p.NewTargetID]
		s.docFresh = s.docLoader == ""
		s.gen++
	}
	delete(s.pendingLoader, p.NewTargetID)
}

func (s *Session) handleTargetDestroyed(params json.RawMessage) {
	var p struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.TargetID == "" {
		s.malformed.Add(1)
		return
	}
	s.mu.Lock()
	t := s.targets[p.TargetID]
	if t == nil {
		s.mu.Unlock()
		return
	}
	wasActive := p.TargetID == s.active
	t.destroyed = true
	t.paused = false
	if t.cancelSetup != nil {
		t.cancelSetup()
	}
	delete(s.pendingLoader, p.TargetID)
	for _, call := range s.pending {
		if call.target == p.TargetID {
			switch {
			case wasActive:
				deliver(call, reply{err: ErrPageClosed})
			case t.provisional:
				deliver(call, reply{err: ErrTargetAbandoned})
			default:
				deliver(call, reply{err: ErrTargetSwapped})
			}
		}
	}
	if !wasActive {
		// An abandoned provisional target, or the old target after a commit.
		s.mu.Unlock()
		return
	}
	// Publish the close in the same critical section that failed the
	// pending calls: a Send that runs after a caller saw ErrPageClosed must
	// not find the destroyed target and report ErrTargetSwapped.
	if s.closeErr == nil {
		s.closeErr = ErrPageClosed
	}
	s.mu.Unlock()
	s.shutdown(ErrPageClosed)
}

// trackNavigation applies a main-frame Page.frameNavigated to the document
// generation. A commit and the frameNavigated of the same document count once.
func (s *Session) trackNavigation(target string, params json.RawMessage) {
	var p struct {
		Frame struct {
			ParentID string `json:"parentId"`
			LoaderID string `json:"loaderId"`
		} `json:"frame"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Frame.ParentID != "" || p.Frame.LoaderID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if target != s.active {
		s.pendingLoader[target] = p.Frame.LoaderID // provisional, before commit
		return
	}
	switch {
	case s.docFresh:
		s.docLoader = p.Frame.LoaderID // the document that the commit counted
		s.docFresh = false
	case s.docLoader != p.Frame.LoaderID:
		s.docTarget, s.docLoader = target, p.Frame.LoaderID
		s.gen++
	}
}

type targetCaller struct {
	s  *Session
	id string
}

func (t targetCaller) TargetID() string { return t.id }

// AgentStates returns what the session knows about the agents of the
// target's backend (Session.AgentStates).
func (t targetCaller) AgentStates() map[string]AgentState { return t.s.AgentStates(t.id) }

// SendPipelined sends calls to the target in one round trip
// (Session.SendPipelined), without the setup check.
func (t targetCaller) SendPipelined(ctx context.Context, calls []Call) []error {
	return t.s.sendPipelined(ctx, t.id, false, calls)
}

func (t targetCaller) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return t.s.sendTo(ctx, t.id, false, method, params)
}
