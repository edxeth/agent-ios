// Package devtools holds the DevTools extras of the daemon (design
// docs/design/agentic-parity.md, section 7.7): the network log with its
// idle tracker and HAR writer, request routes over WIP interception and the
// JavaScript fallback, the console and error buffer, media, user-agent and
// header overrides, origin-scoped credentials, the geolocation shim, init
// scripts, and the Timeline and ScriptProfiler converters.
//
// Every piece of page state is registered in the page's core.DesiredState
// under an owner named "<kind>:<session>" (design 2.4), so that a reconnect
// or a provisional target gets it again (2.7) and the lease release of the
// session removes it (core.Manager releases every owner that ends in
// ":<session>"). In-memory captures register an Entry whose Release stops
// them, so nothing of a session outlives its lease.
package devtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// ErrNoDaemonState: the page session has no scheduler or desired state, so
// state could neither be owned, re-applied nor released.
var ErrNoDaemonState = errors.New("this page session has no desired state; DevTools commands run in the agent-ios daemon")

// Page is the DevTools state of one attached page. It lives as long as the
// page session (core.PageSession) of the daemon.
type Page struct {
	ps *core.PageSession

	mu       sync.Mutex
	nets     map[string]*Netlog  // by session
	consoles map[string]*Console // by session
	icpt     *interceptor
	traces   map[string]*traceRec
	profiles map[string]*profileRec
	inits    map[string]int // next init script number by session
}

var pages sync.Map // *core.PageSession -> *Page

// For returns the DevTools state of ps. It fails for a page session that
// has no desired state (built outside the daemon).
func For(ps *core.PageSession) (*Page, error) {
	if ps == nil || ps.State == nil || ps.Sched == nil || ps.Caller == nil {
		return nil, ErrNoDaemonState
	}
	if p, ok := pages.Load(ps); ok {
		return p.(*Page), nil
	}
	p := &Page{
		ps: ps, nets: map[string]*Netlog{}, consoles: map[string]*Console{},
		traces: map[string]*traceRec{}, profiles: map[string]*profileRec{}, inits: map[string]int{},
	}
	actual, loaded := pages.LoadOrStore(ps, p)
	if !loaded {
		// The interceptor runs for the life of the page state, not only
		// while a rule of this package exists: its safety net answers
		// every intercepted request that no handler claims (intercepted).
		p.mu.Lock()
		p.interceptorLocked()
		p.mu.Unlock()
		go forgetWhenClosed(ps)
	}
	return actual.(*Page), nil
}

// connState is the state view of the daemon's page connection
// (core's pageConn, the Caller of a daemon PageSession).
type connState interface {
	State() core.ConnState
}

// lostPoll is how often forgetWhenClosed checks a lost page connection
// for a takeover or the close of the page record.
const lostPoll = 500 * time.Millisecond

// forgetWhenClosed drops the DevTools state of ps when its page connection
// is closed for good. A lost connection is not the end: a takeover
// (core.Manager reattach) gives the same page session a new connection
// and re-applies its desired state, and the captures that the desired
// state owns must keep their contents (7.7 tests 1 and 4). Done does not
// close again when a lost page is closed later, so a lost page is polled
// until it is taken over (Done is open again) or closed.
func forgetWhenClosed(ps *core.PageSession) {
	cs, ok := ps.Caller.(connState)
	for {
		<-ps.Caller.Done()
		if !ok || cs.State() != core.ConnLost {
			pages.Delete(ps)
			return
		}
		for cs.State() == core.ConnLost {
			time.Sleep(lostPoll)
		}
	}
}

// Session returns the session that holds the page lease, or an error when
// nobody holds it: DevTools state always belongs to a session.
func (p *Page) Session() (string, error) {
	s, _ := p.ps.Sched.Lease()
	if s == "" {
		return "", fmt.Errorf("the page has no lease holder; DevTools state belongs to a session")
	}
	return s, nil
}

// leaseHookTimeout bounds the enable calls of the captures that a lease
// start begins; a page that does not answer delays the lease's first
// command by at most this long, and that command's own deadline applies.
const leaseHookTimeout = 5 * time.Second

func init() { core.OnLeaseStart(startLeaseCaptures) }

// startLeaseCaptures starts the network log and the console capture of
// session when its lease starts. WebKit replays no network events and only
// its last 100 console messages, so both captures start with the lease
// (design 4.2 "network requests" and "console", MAP 1.7 and 1.8). They run
// as one operation under the new lease; the release of the lease stops
// them.
func startLeaseCaptures(ctx context.Context, ps *core.PageSession, session string, epoch uint64) error {
	p, err := For(ps)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, leaseHookTimeout)
	defer cancel()
	op, err := ps.Sched.Begin(ctx, core.OpSpec{Name: "lease captures", Class: core.ClassMutate, Session: session, Epoch: epoch})
	if err != nil {
		return fmt.Errorf("starting the network and console captures: %w", err)
	}
	defer op.End()
	if err := ps.State.Change(op.Context(), op.Caller(), func(webkit.Caller) error {
		p.dropReleased(session)
		return nil
	}); err != nil {
		return fmt.Errorf("dropping the DevTools state of the session's earlier lease: %w", err)
	}
	if _, _, err := p.StartNetlog(op.Context(), op.Caller(), session); err != nil {
		return fmt.Errorf("starting the network log: %w", err)
	}
	if _, err := p.StartConsole(op.Context(), op.Caller(), session); err != nil {
		return fmt.Errorf("starting the console capture: %w", err)
	}
	return nil
}

// dropReleased drops the DevTools state of session in memory whose
// desired-state entry is gone although its Release did not run. A lease
// release removes the session's entries also when it cannot run their
// Release (core-events r3, forgetSession): the page connection is not
// attached, or a change held the change lock past the release deadline.
// The next lease of the same session must not find that state: a capture
// that no longer owns its domain, a route, offline mode or credentials
// that would claim requests, a recording that blocks a new start, or a
// tombstone. Each piece is dropped as its entry's Release would drop it;
// nothing is sent, because the page-side cleanup is core's (the agents the
// owners touched are reset later, or by the next connection's setup). A
// recording's run goes on in the page until then, also across a
// same-target navigation: its entries belong to the ScriptProfiler and
// Timeline agents, so the release marked them, and every later start
// resets a marked agent before it starts (scriptProfilerFree, StartTrace).
//
// It runs at the start of a lease (startLeaseCaptures), inside a change:
// every devtools function records an entry and its state in memory inside
// a change, or before its change and for an operation of the session that
// holds the lease, so none is half done here.
func (p *Page) dropReleased(session string) {
	has := p.ps.State.HasEntry
	p.mu.Lock()
	n, k := p.nets[session], p.consoles[session]
	tr, pr := p.traces[session], p.profiles[session]
	ic := p.icpt
	p.mu.Unlock()
	if n != nil && !has("devtools:netlog:"+session) {
		p.dropNetlog(session, n)
	}
	if k != nil && !has("devtools:console:"+session) {
		p.dropConsole(session, k)
	}
	if tr != nil && !has("devtools:trace:"+session) {
		p.mu.Lock()
		if p.traces[session] == tr {
			delete(p.traces, session)
		}
		p.mu.Unlock()
		tr.stop()
	}
	if pr != nil && !has("devtools:profiler:"+session) {
		p.mu.Lock()
		if p.profiles[session] == pr {
			delete(p.profiles, session)
		}
		p.mu.Unlock()
		pr.stop()
	}
	if ic != nil {
		ic.dropReleased(session, has)
	}
}

// owner is the desired-state owner of kind for session.
func owner(kind, session string) core.Owner { return core.SessionOwner(kind, session) }

// acquireDomain makes o an owner of domain and sends domain.enable when o
// is the first owner. A failed enable removes the ownership again only
// when it provably changed nothing (noEffect). When the enable may have
// reached the page (its reply lost to a cancellation, a deadline or a
// lost connection), o keeps the domain, so that the release of o sends
// domain.disable, as core's releaseFailedAcquire does (review finding
// R6-6).
func acquireDomain(ctx context.Context, ps *core.PageSession, c webkit.Caller, o core.Owner, domain string) error {
	if !ps.State.Acquire(o, domain) {
		return nil
	}
	if _, err := c.Send(ctx, domain+".enable", nil); err != nil {
		if noEffect(err) {
			ps.State.ReleaseDomain(o, domain)
		}
		return fmt.Errorf("%s.enable: %w", domain, err)
	}
	return nil
}

// releaseDomain removes o from domain's owners and sends domain.disable
// when o was the last one (design 2.4 reference count). When the disable
// fails, the domain stays enabled in the page, so o keeps it: a later
// release of o (a retry, or the lease release) sends the disable again.
func releaseDomain(ctx context.Context, ps *core.PageSession, c webkit.Caller, o core.Owner, domain string) error {
	if ps.State.ReleaseDomain(o, domain) {
		if _, err := c.Send(ctx, domain+".disable", nil); err != nil {
			ps.State.Acquire(o, domain)
			return fmt.Errorf("%s.disable: %w", domain, err)
		}
	}
	return nil
}

// noop is the Apply of an entry that only ties an in-memory capture to its
// owner's lifetime: there is nothing to send to a new target.
func noop(context.Context, core.Sender) error { return nil }

// sendJSON sends method and decodes the result into out (when non-nil).
func sendJSON(ctx context.Context, c core.Sender, method string, params, out any) error {
	raw, err := c.Send(ctx, method, params)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s: bad reply: %w", method, err)
		}
	}
	return nil
}
