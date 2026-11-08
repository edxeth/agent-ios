package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
	// for when it closes (Manager.goBackground). It must not take a lock
	// that is held around swap (Manager.goBackground takes a leaf lock).
	background func(func(ctx context.Context)) bool
	logf       func(string, ...any)

	mu       sync.Mutex
	cur      webkit.Caller
	state    ConnState
	stallErr error
	lostErr  error // why a new connection's peer end made the page lost (markLost); nil: unknown
	subs     map[int]*connSub
	nextSub  int
	probing  bool
	expected map[webkit.Caller]bool // connections the daemon closes itself
	next     *handoff               // the new connection being set up (prepare)
	done     chan struct{}          // closed when the page is lost or closed
	dropped  time.Time              // last transport messageDropped event
	// heldDrops counts the events that prepared connections dropped at
	// handoffLimit (DroppedEvents).
	heldDrops atomic.Int64
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
	// pending removes the subscription on the new connection that is being
	// set up (prepare), nil when none is.
	pending func()
	removed atomic.Bool // unsubscribed: held events are not delivered
}

func newPageConn(c webkit.Caller, onStall func(), background func(func(ctx context.Context)) bool, logf func(string, ...any)) *pageConn {
	p := &pageConn{cur: c, onStall: onStall, background: background, logf: logf, subs: map[int]*connSub{}, expected: map[webkit.Caller]bool{}, done: make(chan struct{})}
	p.stall, p.stallFired = context.WithCancelCause(context.Background())
	p.watch(c)
	return p
}

// watch marks the page lost when c ends without a request from the daemon,
// unless a socket write that timed out ended it (writeFailed). The
// goroutine runs through background, so the page's owner waits for it when
// it closes (Manager.Close); it ends at the end of c or when the owner
// closes, which closes c itself. It reports false when background refuses:
// the owner has closed, and nobody watches c. The manager installs no
// connection after its Close began (Manager.shuttingDown), so a refused
// watch never leaves an installed connection of a live manager unwatched.
func (p *pageConn) watch(c webkit.Caller) bool {
	return p.background(func(ctx context.Context) {
		select {
		case <-c.Done():
			p.ended(c)
		case <-ctx.Done():
		}
	})
}

// ended is watch's verdict on c, which has ended.
func (p *pageConn) ended(c webkit.Caller) {
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

// currentConn returns the current connection in any state; the execution
// contexts that the scheduler looks up belong to it (Scheduler.inventory).
func (p *pageConn) currentConn() webkit.Caller {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
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
// When c was prepared (prepare), the subscriptions keep their
// subscriptions on c, and the events c delivered before the swap reach
// them now, in the order c delivered them: first to the subscriptions that
// run in the read loop, in the caller (their handlers do not block), then
// to each queued subscription on its own goroutine (handoff.openQueued).
// It installs nothing and returns an error when a read-loop subscription
// lost a held event of c (handoff.install); the caller closes c then.
func (p *pageConn) swap(c webkit.Caller) error {
	p.mu.Lock()
	h := p.next
	if h != nil && h.c != c {
		p.discardLocked() // prepared for another connection, which is not installed
		h = nil
	}
	if h != nil {
		if err := h.install(); err != nil {
			p.discardLocked()
			p.mu.Unlock()
			return err
		}
	}
	old := p.cur
	select {
	case <-old.Done(): // retired or gone; watch has seen it end
	default:
		p.expected[old] = true
	}
	p.next = nil
	p.cur = c
	if p.state == ConnLost || p.state == ConnClosed {
		p.done = make(chan struct{})
	}
	p.state, p.stallErr, p.lostErr = ConnOK, nil, nil
	p.stall, p.stallFired = context.WithCancelCause(context.Background())
	for _, s := range p.subs {
		s.unsub()
		if h != nil && s.pending != nil {
			s.unsub, s.pending = s.pending, nil
		} else {
			s.unsub = subscribeOn(c, s)
		}
	}
	p.mu.Unlock()
	if h != nil {
		h.openReadLoop()
	}
	watched := p.watch(c)
	if h != nil {
		if watched {
			h.openQueued()
		} else {
			h.drop() // the manager closes; nobody reads the events any more
		}
	}
	closeCaller(old)
	return nil
}

// prepare subscribes every subscription on c, a new connection that is
// set up before swap installs it (Manager.openConn), so that the events of
// its setup reach the subscribers: for example the replay of Console.enable
// with the messages logged while no connection was installed. Until swap
// installs c, they are held (handoff); discard drops them when c is not
// installed. One connection is prepared at a time (the page's attachMu).
func (p *pageConn) prepare(c webkit.Caller) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.discardLocked()
	h := &handoff{c: c, drops: &p.heldDrops}
	p.next = h
	for _, s := range p.subs {
		s.pending = h.subscribe(s)
	}
}

// discard drops the events of c, a prepared connection that is not
// installed, and its subscriptions; the caller closes c.
func (p *pageConn) discard(c webkit.Caller) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.next != nil && p.next.c == c {
		p.discardLocked()
	}
}

func (p *pageConn) discardLocked() {
	h := p.next
	if h == nil {
		return
	}
	p.next = nil
	for _, s := range p.subs {
		if s.pending != nil {
			s.pending()
			s.pending = nil
		}
	}
	h.drop()
}

// handoffLimit bounds the events that a prepared connection holds for one
// subscription: the capacity of a Session subscriber's queue
// (webkit.DefaultEventQueue), the budget that each subscriber has on an
// installed connection.
const handoffLimit = webkit.DefaultEventQueue

// handoff holds the events of a prepared connection until swap installs
// it or discard drops them (drop). Events for subscriptions that run in
// the read loop and for queued ones are held apart: each kind is delivered
// in the order the connection delivered it, the read-loop kind first, as
// the connection itself delivers them.
//
// Each subscription holds at most handoffLimit events. A queued
// subscription's event beyond it is dropped and counted (drops,
// pageConn.DroppedEvents), as a full Session subscriber queue drops and
// counts it. A read-loop subscription's event is bookkeeping that later
// readers rely on (the scheduler's pending items), so it is never dropped
// silently: an event beyond the limit fails the handoff, and swap does not
// install the connection (install). deliver, which the connection calls,
// never waits, except for a read-loop event that finds its subscription's
// buffer full while swap delivers the held read-loop events.
type handoff struct {
	c     webkit.Caller
	drops *atomic.Int64 // counts the events dropped at handoffLimit; nil: not counted

	mu sync.Mutex
	// syncHeld and asyncHeld are the held events of the read-loop and of
	// the queued subscriptions, each in the order c delivered them; held
	// counts them per subscription.
	syncHeld, asyncHeld []heldEvent
	held                map[*connSub]int
	// replay holds, after openQueued, the events that the replay goroutine
	// of each queued subscription has still to deliver (replayTo).
	replay     map[*connSub][]heldEvent
	syncOpen   bool          // openReadLoop delivered the held read-loop events
	asyncOpen  bool          // openQueued gave the held queued events to replayTo
	installing chan struct{} // set by install, closed when openReadLoop ends
	overflow   error         // a read-loop subscription lost an event
	dropped    bool
}

type heldEvent struct {
	s              *connSub
	target, method string
	params         json.RawMessage
}

// subscribe subscribes s on the prepared connection through h.
func (h *handoff) subscribe(s *connSub) func() {
	return subscribeOn(h.c, &connSub{method: s.method, sync: s.sync, h: func(target, method string, params json.RawMessage) {
		h.deliver(s, target, method, params)
	}})
}

func (h *handoff) deliver(s *connSub, target, method string, params json.RawMessage) {
	e := heldEvent{s: s, target: target, method: method, params: params}
	h.mu.Lock()
	for s.sync && !h.syncOpen && h.installing != nil && h.held[s] >= handoffLimit {
		// swap delivers the held read-loop events: wait for it rather than
		// lose this one (install).
		wait := h.installing
		h.mu.Unlock()
		<-wait
		h.mu.Lock()
	}
	_, replaying := h.replay[s]
	switch {
	case h.dropped:
	case s.sync && !h.syncOpen:
		h.holdLocked(&h.syncHeld, e)
	case !s.sync && !h.asyncOpen:
		h.holdLocked(&h.asyncHeld, e)
	case !s.sync && replaying:
		if len(h.replay[s]) >= handoffLimit {
			h.countDrop()
		} else {
			h.replay[s] = append(h.replay[s], e)
		}
	default:
		h.mu.Unlock()
		if !s.removed.Load() {
			s.h(target, method, params)
		}
		return
	}
	h.mu.Unlock()
}

// holdLocked appends e to held unless its subscription holds handoffLimit
// events already: then e is dropped and counted, and for a read-loop
// subscription the handoff fails (overflow).
func (h *handoff) holdLocked(held *[]heldEvent, e heldEvent) {
	if h.held == nil {
		h.held = map[*connSub]int{}
	}
	if h.held[e.s] >= handoffLimit {
		h.countDrop()
		if e.s.sync && h.overflow == nil {
			h.overflow = fmt.Errorf("the new connection delivered more than %d %s events to a read-loop subscription before it was installed, so their bookkeeping is incomplete", handoffLimit, e.method)
		}
		return
	}
	h.held[e.s]++
	*held = append(*held, e)
}

func (h *handoff) countDrop() {
	if h.drops != nil {
		h.drops.Add(1)
	}
}

// install starts the installation of c (swap). It fails when a read-loop
// subscription lost an event of c (holdLocked). From then on, a read-loop
// event that finds its subscription's buffer full waits until
// openReadLoop has delivered the held ones, instead of being lost.
func (h *handoff) install() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.overflow != nil {
		return h.overflow
	}
	h.installing = make(chan struct{})
	return nil
}

// openReadLoop delivers the held read-loop events in their order, also
// those that arrive meanwhile, and then lets the connection deliver them
// at once. The handlers run without h.mu. As in c's read loop, they must
// not subscribe on c or unsubscribe from it: c's read loop may wait for
// this delivery (deliver), holding its subscriber lock.
func (h *handoff) openReadLoop() {
	for {
		h.mu.Lock()
		batch := h.syncHeld
		h.syncHeld = nil
		for _, e := range batch {
			h.held[e.s]--
		}
		if len(batch) == 0 {
			h.syncOpen = true
			close(h.installing)
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()
		for _, e := range batch {
			if !e.s.removed.Load() {
				e.s.h(e.target, e.method, e.params)
			}
		}
	}
}

// openQueued gives the held events of each queued subscription to a
// goroutine of that subscription (replayTo), so that a slow subscriber
// delays only its own events, as it does on a Session (Session.Subscribe).
// Nobody waits for these goroutines, as nobody waits for a Session's
// subscriber goroutines: Manager.Close never waits for a subscriber's
// handler.
func (h *handoff) openQueued() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asyncOpen = true
	h.replay = map[*connSub][]heldEvent{}
	for _, e := range h.asyncHeld {
		h.replay[e.s] = append(h.replay[e.s], e)
	}
	h.asyncHeld, h.held = nil, nil
	for s := range h.replay {
		go h.replayTo(s)
	}
}

// replayTo delivers s's held events in their order, also those that arrive
// meanwhile (deliver keeps at most handoffLimit of them), and then lets the
// connection deliver to s at once. Like a Session subscriber's goroutine,
// it ends after the running handler when s is unsubscribed or the
// connection ends; the events left are dropped.
func (h *handoff) replayTo(s *connSub) {
	for {
		h.mu.Lock()
		q := h.replay[s]
		if len(q) == 0 || s.removed.Load() || callerEnded(h.c) {
			delete(h.replay, s)
			h.mu.Unlock()
			return
		}
		h.replay[s] = q[1:]
		h.mu.Unlock()
		s.h(q[0].target, q[0].method, q[0].params)
	}
}

func callerEnded(c webkit.Caller) bool {
	select {
	case <-c.Done():
		return true
	default:
		return false
	}
}

// drop ends the delivery of h's events: the held ones and any later one.
func (h *handoff) drop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropped = true
	h.syncHeld, h.asyncHeld, h.held = nil, nil, nil
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

// markLost marks the page lost with cause, which wraps ErrLost, after a
// peer ended the new connection of a reconnect before it was installed
// (Page.newConnLost), as watch does for the installed connection (ended):
// only --takeover connects it again (design 2.5 rule 3). The caller holds
// the page's attachMu.
func (p *pageConn) markLost(cause error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lostErr = cause
	if p.state == ConnLost {
		return
	}
	if p.state != ConnClosed {
		close(p.done)
	}
	p.state = ConnLost
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
	if p.next != nil {
		s.pending = p.next.subscribe(s)
	}
	p.subs[id] = s
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if s := p.subs[id]; s != nil {
				s.removed.Store(true)
				s.unsub()
				if s.pending != nil {
					s.pending()
					s.pending = nil
				}
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

// AgentStates returns what the current connection knows about the agents
// of target (webkit.Session.AgentStates); nil for a connection without
// targets.
func (p *pageConn) AgentStates(target string) map[string]webkit.AgentState {
	p.mu.Lock()
	c := p.cur
	p.mu.Unlock()
	if as, ok := c.(interface {
		AgentStates(string) map[string]webkit.AgentState
	}); ok {
		return as.AgentStates(target)
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
	// A send addressed to an execution context's reporting connection
	// (ExecContext.pin) never goes to the connection that replaced it.
	if pin, ok := pinnedConn(ctx); ok && pin != c {
		return nil, fmt.Errorf("%s: not sent: %w", method, notSentWrap(errConnReplaced))
	}
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
		// call keeps its own error. A probe that found the connection
		// ended by a peer leaves the page lost (probe), so the call says
		// so, with the --takeover guidance of ErrLost: no reconnect runs.
		perr := p.probe(ctx, c)
		switch {
		case errors.Is(perr, ErrLost):
			return nil, &CodedError{Code: CodeBusy, Err: fmt.Errorf("%s: no reply, and the connection ended during the liveness probe: %w (call: %w)", method, perr, err)}
		case perr != nil && !errors.Is(perr, errProbeCanceled):
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
	if err != nil && !errors.Is(err, errProbeCanceled) {
		err = lostOnPeerEnd(c, err)
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

// lostOnPeerEnd wraps ErrLost, with its --takeover guidance, around err, the
// failure of a liveness probe on c, when c ended for a cause other than a
// write timeout (endedByPeer): another inspector may have taken the page, so
// the failure says nothing about a stalled or hidden tab.
func lostOnPeerEnd(c webkit.Caller, err error) error {
	if endedByPeer(c) {
		return fmt.Errorf("%w: %w", ErrLost, err)
	}
	return err
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

// DroppedEvents counts the events that a subscriber lost because its queue
// was full: on the current connection (webkit.Session.DroppedEvents) and in
// the handoffs of new connections (handoffLimit).
func (p *pageConn) DroppedEvents() int64 {
	n := p.heldDrops.Load()
	if d, ok := p.currentConn().(interface{ DroppedEvents() int64 }); ok {
		n += d.DroppedEvents()
	}
	return n
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
