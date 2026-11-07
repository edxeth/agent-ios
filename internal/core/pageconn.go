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
}

type connSub struct {
	method string
	h      webkit.EventHandler
	sync   bool
	unsub  func()
}

func newPageConn(c webkit.Caller, onStall func(), background func(func(ctx context.Context)) bool, logf func(string, ...any)) *pageConn {
	p := &pageConn{cur: c, onStall: onStall, background: background, logf: logf, subs: map[int]*connSub{}, expected: map[webkit.Caller]bool{}, done: make(chan struct{})}
	p.watch(c)
	return p
}

// watch marks the page lost when c ends without a request from the daemon.
func (p *pageConn) watch(c webkit.Caller) {
	go func() {
		<-c.Done()
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.expected[c] {
			delete(p.expected, c)
			return
		}
		if p.cur != c || p.state == ConnClosed || p.state == ConnLost {
			return
		}
		p.state = ConnLost
		p.logf("page connection closed unexpectedly; the lease is lost")
		close(p.done)
	}()
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
	p.expected[c] = true
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
		if ss, ok := c.(interface {
			SubscribeSync(string, webkit.EventHandler) func()
		}); ok {
			return ss.SubscribeSync(s.method, s.h)
		}
	}
	return c.Subscribe(s.method, s.h)
}

func (p *pageConn) subscribe(method string, h webkit.EventHandler, inLoop bool) func() {
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
	return p.subscribe(method, h, false)
}

// SubscribeSync keeps the scheduler's bookkeeping inside the read loop.
func (p *pageConn) SubscribeSync(method string, h webkit.EventHandler) func() {
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

func (p *pageConn) ActiveSetup(ctx context.Context) error {
	c, err := p.current()
	if err != nil {
		return nil // the send itself reports the state
	}
	if as, ok := c.(interface{ ActiveSetup(context.Context) error }); ok {
		return as.ActiveSetup(ctx)
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
	c, err := p.current()
	if err != nil {
		return err
	}
	if rt, ok := c.(interface {
		ResumeTarget(context.Context, string) error
	}); ok {
		return rt.ResumeTarget(ctx, id)
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
	c, err := p.current()
	if err != nil {
		return nil, fmt.Errorf("%s: not sent: %w", method, notSentWrap(err))
	}
	var res json.RawMessage
	if cs, ok := c.(interface {
		SendControl(context.Context, string, any) (json.RawMessage, error)
	}); ok && control {
		res, err = cs.SendControl(ctx, method, params)
	} else {
		res, err = c.Send(ctx, method, params)
	}
	if err == nil {
		return res, nil
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

// probe runs the liveness probe on c, one at a time. A failure marks the
// page stalled and starts background recovery once. A probe that its
// owner canceled marks nothing and returns an error wrapping
// errProbeCanceled.
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
	p.mu.Lock()
	p.probing = false
	stall := err != nil && !errors.Is(err, errProbeCanceled) && p.cur == c && p.state == ConnOK
	if stall {
		p.state = ConnStalled
		p.stallErr = fmt.Errorf("%w (%v)", webkit.ErrSessionStalled, err)
	}
	p.mu.Unlock()
	if stall {
		p.logf("liveness probe failed: %v; the page is stalled", err)
		if p.onStall != nil {
			p.onStall() // starts the recovery in the background
		}
	}
	return err
}

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
