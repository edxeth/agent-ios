package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Connection states of a page (design 2.5 and 2.6).
type ConnState int

const (
	ConnOK      ConnState = iota
	ConnStalled           // call and probe got no reply; background recovery may run
	ConnLost              // closed without a request from the daemon; ownership unknown
	ConnClosed            // closed by the daemon
)

func (s ConnState) String() string {
	return [...]string{"attached", "stalled", "lost", "closed"}[s]
}

// pageConn is the page connection that a PageSession's scheduler sends
// through. It hides reconnects: background recovery and --takeover swap the
// underlying connection, and subscriptions move with it. It adds hang
// detection (design 2.6): a call that gets no reply is followed by the
// liveness probe; a failed probe marks the page stalled.
type pageConn struct {
	onStall func() // starts background recovery; runs once per stall
	// background runs a goroutine that the page's owner cancels and waits
	// for when it closes (Manager.goBackground).
	background func(func(ctx context.Context)) bool
	logf       func(string, ...any)

	mu       sync.Mutex
	cur      webkit.Caller
	state    ConnState
	stallErr error
	subs     map[int]*connSub
	nextSub  int
	probing  bool
	expected map[webkit.Caller]bool // connections the daemon closes itself
	done     chan struct{}          // closed when the page is lost or closed
	dropped  time.Time              // last transport messageDropped event
	// stall ends, with the stall error as its cause, when the liveness
	// probe declares cur stalled; swap gives the new connection its own.
	// Every call that waits on cur is bounded by it (bound), so the probe's
	// verdict ends them at once (core-events r5).
	stall      context.Context
	stallFired context.CancelCauseFunc
}

type connSub struct {
	method string
	h      webkit.TargetEventHandler
	sync   bool
	unsub  func()
}

func newPageConn(c webkit.Caller, onStall func(), background func(func(ctx context.Context)) bool, logf func(string, ...any)) *pageConn {
	p := &pageConn{cur: c, onStall: onStall, background: background, logf: logf, subs: map[int]*connSub{}, expected: map[webkit.Caller]bool{}, done: make(chan struct{})}
	p.stall, p.stallFired = context.WithCancelCause(context.Background())
	p.watch(c)
	return p
}

// watch marks the page lost when c ends without a request from the daemon,
// unless a socket write that timed out ended it (writeFailed).
func (p *pageConn) watch(c webkit.Caller) {
	go func() {
		<-c.Done()
		p.mu.Lock()
		if p.expected[c] {
			delete(p.expected, c)
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		if p.writeFailed(c) {
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.cur != c || p.state == ConnClosed || p.state == ConnLost {
			return
		}
		p.state = ConnLost
		p.logf("page connection closed unexpectedly; the lease is lost")
		close(p.done)
	}()
}

// writeFailed reports whether c ended because one of its socket writes
// timed out (webkit.ErrWriteTimeout in the chain of c's Err): the write's
// deadline or call budget ended it, so the far side was still connected
// but did not take the data in time, the case of a failed liveness probe
// (design 2.6). A WebSocket connection cannot be written after a failed
// write, so c closed itself. When c is the attached connection and the
// page is still attached, the page becomes stalled with c's error, and the
// background recovery connects again while the lease is valid (recover);
// without a lease the next command reopens the page (ensureAttached). The
// stall context does not end: the calls on c already ended with c, and a
// call whose deadline cut off the write keeps its timeout (exit 124,
// decision D9). Any other end is not handled here: a write that the peer or
// the network broke (webkit.ErrWriteBroken: another inspector may have
// taken the page) is an unexpected disconnect, and watch marks the page
// lost; only --takeover connects it again (design 2.5 rule 3). The first
// published cause of c's end decides (Session and Client keep it), and a
// page already lost or stalled is never changed here. send calls it as
// soon as a call fails, so the next command finds the page stalled; watch
// calls it for the writes that send does not make.
func (p *pageConn) writeFailed(c webkit.Caller) bool {
	select {
	case <-c.Done():
	default:
		return false
	}
	ce, ok := c.(interface{ Err() error })
	if !ok {
		return false
	}
	err := ce.Err()
	if !errors.Is(err, webkit.ErrWriteTimeout) {
		return false
	}
	p.mu.Lock()
	stall := p.cur == c && p.state == ConnOK && !p.expected[c]
	if stall {
		p.state, p.stallErr = ConnStalled, err
	}
	p.mu.Unlock()
	if stall {
		p.logf("page connection closed after a write timed out: %v; reconnecting", err)
		if p.onStall != nil {
			p.onStall() // starts the recovery in the background
		}
	}
	return true
}

// State returns the connection state.
func (p *pageConn) State() ConnState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *pageConn) current() (webkit.Caller, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentLocked()
}

func (p *pageConn) currentLocked() (webkit.Caller, error) {
	switch p.state {
	case ConnLost:
		return nil, &CodedError{Code: CodeBusy, Err: ErrLost}
	case ConnStalled:
		return nil, &CodedError{Code: CodeBusy, Err: p.stallErr}
	case ConnClosed:
		return nil, &CodedError{Code: CodeBusy, Err: webkit.ErrSessionClosed}
	}
	return p.cur, nil
}

// installed reports whether c is the current connection, whatever its
// state (Scheduler.disabledOn).
func (p *pageConn) installed(c webkit.Caller) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur == c
}

// swap replaces the connection (recovery or takeover); the old one is
// closed as expected, subscriptions move, and the state is attached again.
func (p *pageConn) swap(c webkit.Caller) {
	p.mu.Lock()
	old := p.cur
	select {
	case <-old.Done(): // retired or gone; watch has seen it end
	default:
		p.expected[old] = true
	}
	p.cur = c
	if p.state == ConnLost || p.state == ConnClosed {
		p.done = make(chan struct{})
	}
	p.state, p.stallErr = ConnOK, nil
	p.stall, p.stallFired = context.WithCancelCause(context.Background())
	for _, s := range p.subs {
		s.unsub()
		s.unsub = subscribeOn(c, s)
	}
	p.mu.Unlock()
	p.watch(c)
	closeCaller(old)
}

// retire closes the current connection as expected without changing the
// page state. Recovery and reopen call it before they dial the replacement:
// on a single-inspector endpoint the new connection would close the old one,
// which would otherwise look like a loss (design 2.5 rule 3).
func (p *pageConn) retire() {
	p.mu.Lock()
	c := p.cur
	select {
	case <-c.Done(): // gone already (writeFailed); watch has seen it end
	default:
		p.expected[c] = true
	}
	p.mu.Unlock()
	closeCaller(c)
}

// close ends the connection on the daemon's request.
func (p *pageConn) close() {
	p.mu.Lock()
	c := p.cur
	p.expected[c] = true
	wasOpen := p.state != ConnLost && p.state != ConnClosed
	p.state = ConnClosed
	if wasOpen {
		close(p.done)
	}
	p.mu.Unlock()
	closeCaller(c)
}

func closeCaller(c webkit.Caller) {
	if cl, ok := c.(interface{ Close() error }); ok {
		_ = cl.Close()
	}
}

func subscribeOn(c webkit.Caller, s *connSub) func() {
	if s.sync {
		return subscribeItems(c, s.method, s.h)
	}
	return webkit.SubscribeTarget(c, s.method, s.h)
}

func (p *pageConn) subscribe(method string, h webkit.TargetEventHandler, inLoop bool) func() {
	p.mu.Lock()
	id := p.nextSub
	p.nextSub++
	s := &connSub{method: method, h: h, sync: inLoop}
	s.unsub = subscribeOn(p.cur, s)
	p.subs[id] = s
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if s := p.subs[id]; s != nil {
				s.unsub()
				delete(p.subs, id)
			}
			p.mu.Unlock()
		})
	}
}

func (p *pageConn) Subscribe(method string, h webkit.EventHandler) func() {
	return p.subscribe(method, func(_, m string, params json.RawMessage) { h(m, params) }, false)
}

// SubscribeTarget is Subscribe with each event's source target
// (webkit.TargetEventHandler).
func (p *pageConn) SubscribeTarget(method string, h webkit.TargetEventHandler) func() {
	return p.subscribe(method, h, false)
}

// SubscribeSync keeps the scheduler's bookkeeping inside the read loop.
func (p *pageConn) SubscribeSync(method string, h webkit.EventHandler) func() {
	return p.subscribe(method, func(_, m string, params json.RawMessage) { h(m, params) }, true)
}

// SubscribeTargetSync is SubscribeSync with each event's source target.
func (p *pageConn) SubscribeTargetSync(method string, h webkit.TargetEventHandler) func() {
	return p.subscribe(method, h, true)
}

func (p *pageConn) OnEvent(method string, h webkit.EventHandler) { p.Subscribe(method, h) }

func (p *pageConn) IsTargetRouted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur.IsTargetRouted()
}

// Done is closed when the page is lost or closed (not on a recovery swap).
func (p *pageConn) Done() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// Generation is the document generation of the current connection.
func (p *pageConn) Generation() uint64 {
	p.mu.Lock()
	c := p.cur
	p.mu.Unlock()
	if g, ok := c.(interface{ Generation() uint64 }); ok {
		return g.Generation()
	}
	return 1
}

// ActiveTarget is the active target of the current connection, or "" for
// a connection without targets.
func (p *pageConn) ActiveTarget() string {
	p.mu.Lock()
	c := p.cur
	p.mu.Unlock()
	if at, ok := c.(interface{ ActiveTarget() string }); ok {
		return at.ActiveTarget()
	}
	return ""
}

// ExecutionContexts returns the execution contexts of target on the
// current connection (webkit.Session.ExecutionContexts); nil for a
// connection without targets.
func (p *pageConn) ExecutionContexts(target string) []webkit.ExecutionContext {
	p.mu.Lock()
	c := p.cur
	p.mu.Unlock()
	if ec, ok := c.(interface {
		ExecutionContexts(string) []webkit.ExecutionContext
	}); ok {
		return ec.ExecutionContexts(target)
	}
	return nil
}

func (p *pageConn) ActiveSetup(ctx context.Context) error {
	b, err := p.bound(ctx)
	if err != nil {
		return nil // the send itself reports the state
	}
	defer b.stop()
	if as, ok := b.c.(interface{ ActiveSetup(context.Context) error }); ok {
		return b.cut("waiting for the setup of the active page target", as.ActiveSetup(b.ctx))
	}
	return nil
}

// ReapplySetup runs the setup of the active target again
// (webkit.Session.ReapplySetup, Manager.recoverSetup). A connection
// without provisional setups has nothing to apply.
func (p *pageConn) ReapplySetup(ctx context.Context) error {
	b, err := p.bound(ctx)
	if err != nil {
		return err
	}
	defer b.stop()
	if rs, ok := b.c.(interface{ ReapplySetup(context.Context) error }); ok {
		return b.cut("applying the page state again", rs.ReapplySetup(b.ctx))
	}
	return nil
}

func (p *pageConn) PausedTargets() []string {
	c, err := p.current()
	if err != nil {
		return nil
	}
	if pt, ok := c.(interface{ PausedTargets() []string }); ok {
		return pt.PausedTargets()
	}
	return nil
}

func (p *pageConn) ResumeTarget(ctx context.Context, id string) error {
	b, err := p.bound(ctx)
	if err != nil {
		return err
	}
	defer b.stop()
	if rt, ok := b.c.(interface {
		ResumeTarget(context.Context, string) error
	}); ok {
		return b.cut("resuming page target "+id, rt.ResumeTarget(b.ctx, id))
	}
	return webkit.ErrTargetNotPaused
}

// Send sends through the current connection with hang detection.
func (p *pageConn) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return p.send(ctx, false, method, params)
}

// SendControl is the control lane (design 2.10); it skips the setup gate.
func (p *pageConn) SendControl(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return p.send(ctx, true, method, params)
}

func (p *pageConn) send(ctx context.Context, control bool, method string, params any) (json.RawMessage, error) {
	b, err := p.bound(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: not sent: %w", method, notSentWrap(err))
	}
	defer b.stop()
	c := b.c
	var res json.RawMessage
	if cs, ok := c.(interface {
		SendControl(context.Context, string, any) (json.RawMessage, error)
	}); ok && control {
		res, err = cs.SendControl(b.ctx, method, params)
	} else {
		res, err = c.Send(b.ctx, method, params)
	}
	if err == nil {
		return res, nil
	}
	p.writeFailed(c)
	if cut := b.cut(method, err); cut != err {
		return nil, cut
	}
	p.noteDropped(&err)
	switch {
	case errors.Is(err, webkit.ErrCallTimeout):
		// Design 2.6 step 2: at the call budget, probe within the time left.
		// A probe that the operation's owner canceled is no stall; the
		// call keeps its own error.
		if perr := p.probe(ctx, c); perr != nil && !errors.Is(perr, errProbeCanceled) {
			return nil, &CodedError{Code: CodeBusy, Err: fmt.Errorf("%w: %w", webkit.ErrSessionStalled, err)}
		}
		return nil, err
	case !errors.Is(err, webkit.ErrNotSent) && ctx.Err() != nil:
		// The command deadline ended the call without a reply. The caller
		// gets its error at once; the probe runs in the background so that
		// nothing after the deadline is charged to the caller.
		p.background(func(bg context.Context) {
			pctx, cancel := context.WithTimeout(bg, webkit.DefaultProbeBudget)
			defer cancel()
			_ = p.probe(pctx, c)
		})
	}
	return nil, err
}

// boundCall is one call on the connection c, bounded by c's stall (bound).
type boundCall struct {
	c     webkit.Caller
	ctx   context.Context // the caller's ctx, also ended by c's stall
	stall context.Context // c's stall (pageConn.stall)
	stop  func()          // releases ctx
}

// cut is stallCut for this call.
func (b boundCall) cut(what string, err error) error { return stallCut(b.ctx, b.stall, what, err) }

// bound returns the current connection and ctx bounded by that
// connection's stall: when the liveness probe declares it stalled, the
// returned context ends with the stall error as its cause, so a call that
// waits on it (a reply, the write slot, the setup gate, a setup) ends at
// once instead of at its deadline (core-events r5; stallCut), and probe
// interrupts a write in progress. It fails like current for a page that is
// not attached.
func (p *pageConn) bound(ctx context.Context) (boundCall, error) {
	p.mu.Lock()
	c, err := p.currentLocked()
	stall := p.stall
	p.mu.Unlock()
	if err != nil {
		return boundCall{}, err
	}
	sctx, cancel := context.WithCancelCause(ctx)
	unhook := context.AfterFunc(stall, func() { cancel(context.Cause(stall)) })
	return boundCall{c: c, ctx: sctx, stall: stall, stop: func() { unhook(); cancel(nil) }}, nil
}

// stallCut returns err, or, when the liveness probe declared the connection
// stalled before ctx (from bound) ended otherwise, the stall classification
// (exit 4, ErrSessionStalled) for what. A deadline that ended first keeps
// its own error (exit 124, design 2.6). A call that was never written stays
// ErrNotSent.
func stallCut(ctx, stall context.Context, what string, err error) error {
	if err == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if cause == nil {
		// The stall can end a call before bound's AfterFunc cancels ctx:
		// probe interrupts a write in progress right after it declares the
		// stall (InterruptWrites). ctx has not ended, so the stall came
		// first.
		cause = context.Cause(stall)
	}
	if !errors.Is(cause, webkit.ErrSessionStalled) {
		return err
	}
	if errors.Is(err, webkit.ErrNotSent) {
		return fmt.Errorf("%s: not sent: %w", what, notSentWrap(&CodedError{Code: CodeBusy, Err: cause}))
	}
	return &CodedError{Code: CodeBusy, Err: fmt.Errorf("%s: no reply before the liveness probe declared the page stalled: %w", what, cause)}
}

// notSentWrap marks err as never sent for callers that check ErrNotSent.
func notSentWrap(err error) error { return fmt.Errorf("%w: %w", webkit.ErrNotSent, err) }

// errProbeCanceled marks a probe that its owner canceled (canceledByOwner).
var errProbeCanceled = errors.New("the liveness probe was canceled")

// canceledByOwner reports whether err is the cancellation of ctx by its
// owner: Manager.Close, the end or cancel of the operation, or the client.
// Such an end says nothing about the page. A deadline is no such
// cancellation (the probe budget, the command deadline, or the operation
// deadline, whose cancel cause is ErrDeadline): a probe without a reply
// within its time is a liveness failure.
func canceledByOwner(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) && ctx.Err() != nil && !errors.Is(context.Cause(ctx), context.DeadlineExceeded)
}

// probeNow probes the current connection.
func (p *pageConn) probeNow(ctx context.Context) error {
	p.mu.Lock()
	c := p.cur
	p.mu.Unlock()
	return p.probe(ctx, c)
}

// probe runs the liveness probe on c, one at a time. A failure on a
// connection that is still open, or that a write timeout ended, marks the
// page stalled and starts background recovery once. A probe that its
// owner canceled marks nothing and returns an error wrapping
// errProbeCanceled. A failure on a connection that ended for any other
// cause (endedByPeer: another inspector may have taken the page) marks
// nothing either: watch marks the page lost, and only --takeover connects
// it again (design 2.5 rule 3); the error wraps ErrLost.
func (p *pageConn) probe(ctx context.Context, c webkit.Caller) error {
	p.mu.Lock()
	if p.probing || p.cur != c || p.state != ConnOK {
		p.mu.Unlock()
		return nil
	}
	p.probing = true
	p.mu.Unlock()
	err := probeCaller(ctx, c)
	if err != nil && canceledByOwner(ctx, err) {
		err = fmt.Errorf("%w: %w", errProbeCanceled, err)
	}
	if err != nil && !errors.Is(err, errProbeCanceled) && endedByPeer(c) {
		err = fmt.Errorf("%w: %w", ErrLost, err)
	}
	p.mu.Lock()
	p.probing = false
	stall := err != nil && !errors.Is(err, errProbeCanceled) && !errors.Is(err, ErrLost) && p.cur == c && p.state == ConnOK
	if stall {
		p.state = ConnStalled
		p.stallErr = fmt.Errorf("%w (%v)", webkit.ErrSessionStalled, err)
		p.stallFired(p.stallErr) // ends every call that waits on c (bound)
	}
	p.mu.Unlock()
	if stall {
		// The probe's verdict ends the waits through the stall context; a
		// socket write already in progress ignores its context, so it is
		// interrupted too. No call is sent on the stalled connection again
		// (currentLocked refuses it): a stalled page gets a new connection
		// (recover, reattach) or none.
		if iw, ok := c.(writeInterrupter); ok {
			iw.InterruptWrites()
		}
		p.logf("liveness probe failed: %v; the page is stalled", err)
		if p.onStall != nil {
			p.onStall() // starts the recovery in the background
		}
	}
	return err
}

// endedByPeer reports whether c has ended for a cause other than a write
// timeout (webkit.ErrWriteTimeout): a peer or network failure, a read
// error, or a cause that c does not report (no Err). When in doubt, the
// page is lost, never stalled (core-events r9c, r9d).
func endedByPeer(c webkit.Caller) bool {
	select {
	case <-c.Done():
	default:
		return false
	}
	ce, ok := c.(interface{ Err() error })
	return !ok || !errors.Is(ce.Err(), webkit.ErrWriteTimeout)
}

// writeInterrupter is a connection whose socket writes in progress can be
// interrupted (webkit.Session.InterruptWrites, webkit.Client.InterruptWrites).
type writeInterrupter interface{ InterruptWrites() }

// Both connection kinds that the daemon installs interrupt their writes.
var (
	_ writeInterrupter = (*webkit.Session)(nil)
	_ writeInterrupter = (*webkit.Client)(nil)
)

// probeCaller is Session.Probe, or the same evaluation for a legacy client.
func probeCaller(ctx context.Context, c webkit.Caller) error {
	if pr, ok := c.(interface{ Probe(context.Context) error }); ok {
		return pr.Probe(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, webkit.DefaultProbeBudget)
	defer cancel()
	if _, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "1", "returnByValue": true}); err != nil {
		return fmt.Errorf("liveness probe: %w", err)
	}
	return nil
}

// markDropped records a transport messageDropped event (direct transport).
func (p *pageConn) markDropped() {
	p.mu.Lock()
	p.dropped = time.Now()
	p.mu.Unlock()
}

// noteDropped adds the hint of design section 6 to an error that follows a
// message the device dropped above the frame cap.
func (p *pageConn) noteDropped(err *error) {
	p.mu.Lock()
	d := p.dropped
	p.mu.Unlock()
	if !d.IsZero() && time.Since(d) < time.Minute {
		*err = fmt.Errorf("%w; the device dropped a message above the frame cap, so a large reply (for example a heap snapshot) may be lost; use --transport iwdp", *err)
	}
}
