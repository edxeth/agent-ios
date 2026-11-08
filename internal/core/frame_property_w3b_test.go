package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// W3b final round, finding (c) of the round-4 review: a testing/quick
// property over random interleavings of frame selection, "frame main",
// another Runtime owner, reconnects (with the new connection's reports
// before or after the reconnect's step, answered, late or missing), target
// commits (a cross-site navigation: a new target on the same connection),
// frame navigations and deaths, late report delivery, and frame releases
// with a live or an expired deadline. A frame selection and a release can
// meet a commit or a reconnect between the lookup of a frame's context and
// the send of its id (W3b final repair round 1, finding 1). The page and
// its connections are a model (propPage, propConn); the scheduler, the
// page connection (pageConn), operations, the desired state and
// PageSession.ReleaseFrameContexts are the real code. Invariants (design
// 2.4, 2.6, 3.8):
//   - an old context id is never sent: every Runtime.evaluate names a
//     context that the target it goes to reported, on that connection;
//   - page-side state of a live frame stays recorded until a release
//     reached it: no cleanup turns unknown liveness into "gone", and an
//     expired or canceled cleanup changes no record;
//   - a release whose send found its target or connection replaced
//     returns an error and keeps the frame recorded;
//   - the temporary owner is gone after every release, and the other
//     owners are unchanged;
//   - Runtime.disable is never sent while an owner holds Runtime;
//   - a release waits for reports only for its share of the time left.
//
// The model keeps what the connection observed (its reported contexts)
// apart from what the backend has enabled (the agent state), as
// webkit.Session does.

// propConn is one inspector connection of the model page, with one active
// target at a time; a commit replaces it (propPage.commit).
type propConn struct {
	nopCaller
	p       *propPage
	n       int
	done    chan struct{}
	target  string                    // active target
	targets int                       // targets this connection had
	mode    int                       // reply of a Runtime.enable: enableAnswered, enableLate or enableMissing
	runtime webkit.AgentState         // the session's agent state of Runtime on the active target
	pending bool                      // a late enable's reports are still to come
	ids     map[string]int            // frame: its context reported by the active target
	issued  map[string]map[int]string // target: every context id it reported: its frame
}

const (
	enableAnswered = iota // the reply comes, after the reports of every frame
	enableLate            // the reply is lost; the reports come later (propPage.deliver)
	enableMissing         // the reply and the reports are lost
)

// propPage is the model page: its frames, their page-side state, and the
// current connection.
type propPage struct {
	state *DesiredState

	mu     sync.Mutex
	alive  map[string]bool
	holds  map[string]bool // frames whose document holds the epoch's page-side state
	cur    *propConn
	nextID int // context ids are unique over every target and connection
	conns  int
	broken []string // invariant violations seen in Send
	trace  *propTrace
	// closing: the connection closes at the next call that reaches it
	// (closeBeforeMarked, closeBeforeUnmarked, closeAfter), or closeNone.
	closing int
	// failedAt: per frame, the target of the current release's call in its
	// context that the page received and did not answer; enableFailAt: the
	// target of the current release's Runtime.enable that the page received
	// and that failed or got no answer (startTrace).
	failedAt     map[string]propAt
	enableFailAt propAt
}

// Closes of the connection at one call (propPage.closing).
const (
	closeNone           = iota
	closeBeforeMarked   // before the write; the error is marked webkit.ErrNotSent
	closeBeforeUnmarked // before the write; the error has no marker
	closeAfter          // after the write, before the reply
)

// propTrace is what the page received during one release: the frames
// whose context a Runtime.evaluate reached. While a trace is set, the page
// also keeps the release's attempts that reached it and failed or got no
// answer (propPage.failedAt, enableFailAt).
type propTrace struct {
	evals map[string]bool
}

// startTrace starts the trace of one release, with p.mu held: the release
// attempts that reached the page are this release's only.
func (p *propPage) startTrace() *propTrace {
	p.trace = &propTrace{evals: map[string]bool{}}
	clear(p.failedAt)
	p.enableFailAt = propAt{}
	return p.trace
}

// propAt is a connection of the model page and one of its targets.
type propAt struct {
	conn   int
	target string
}

func (p *propPage) newConn(mode int) *propConn {
	p.conns++
	c := &propConn{p: p, n: p.conns, mode: mode, done: make(chan struct{}), issued: map[string]map[int]string{}}
	c.newTarget()
	return c
}

// newTarget makes a new active target of c, without contexts and with an
// unknown Runtime state. Target ids repeat on every connection, as a new
// inspector connection can name the same page with the same id; only the
// connection tells them apart.
func (c *propConn) newTarget() {
	c.targets++
	c.target = fmt.Sprintf("page-%d", c.targets)
	c.ids, c.runtime, c.pending = map[string]int{}, 0, false
	c.issued[c.target] = map[int]string{}
}

func (p *propPage) report(c *propConn) {
	for _, f := range slices.Sorted(maps.Keys(p.alive)) {
		if p.alive[f] && c.ids[f] == 0 {
			p.nextID++
			c.ids[f] = p.nextID
			c.issued[c.target][p.nextID] = f
		}
	}
}

// enable is a Runtime.enable on c's active target.
func (p *propPage) enable(c *propConn) error {
	switch c.mode {
	case enableAnswered:
		p.report(c)
		c.runtime, c.pending = webkit.AgentEnabled, false
		return nil
	case enableLate:
		c.runtime, c.pending = webkit.AgentMaybeEnabled, true
	default:
		c.runtime = webkit.AgentMaybeEnabled
	}
	return fmt.Errorf("Runtime.enable: %w", webkit.ErrCallTimeout)
}

// deliver delivers the reports and the agent state of a late enable.
func (p *propPage) deliver() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.cur; c.pending {
		p.report(c)
		c.runtime, c.pending = webkit.AgentEnabled, false
	}
}

// commit is a cross-site navigation of the top document: a new target
// replaces the active one on the current connection, as
// Target.didCommitProvisionalTarget does. The new document holds no
// page-side state; its frames are the same and report new contexts when
// the provisional setup enables Runtime (an owner holds it).
func (p *propPage) commit() {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.cur
	c.newTarget()
	for f := range p.holds {
		p.holds[f] = false
	}
	if p.state.Needs("Runtime") {
		_ = p.enable(c)
	}
}

func (c *propConn) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: %w", method, webkit.ErrNotSent)
	}
	p := c.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur != c {
		return nil, fmt.Errorf("%s: %w: %w", method, webkit.ErrNotSent, webkit.ErrSessionClosed)
	}
	// webkit.Session: a call addressed to a target that a commit replaced
	// is not sent.
	if target, ok := webkit.TargetOf(ctx); ok && target != c.target {
		return nil, fmt.Errorf("%s to %s: %w: %w", method, target, webkit.ErrNotSent, webkit.ErrTargetSwapped)
	}
	// A close of the connection at this call (propPage.closing), once.
	closing := p.closing
	p.closing = closeNone
	switch closing {
	case closeBeforeMarked: // webkit.Client and Session: closed before the write
		return nil, fmt.Errorf("%s: %w: connection closed", method, webkit.ErrNotSent)
	case closeBeforeUnmarked: // a connection that fails before the write without the marker
		return nil, fmt.Errorf("%s: connection closed", method)
	}
	webkit.MarkWritten(ctx)
	res, err := c.receive(method, params)
	if closing == closeAfter { // the page got the call; the connection closed before the reply
		if p.trace != nil {
			at := propAt{c.n, c.target}
			if method == "Runtime.enable" {
				p.enableFailAt = at
			} else if m, ok := params.(map[string]any); ok { // a reset's Runtime.disable has none
				if id, ok := m["contextId"].(int64); ok {
					if f, ok := c.issued[c.target][int(id)]; ok {
						p.failedAt[f] = at
					}
				}
			}
		}
		return nil, fmt.Errorf("%s: connection closed", method)
	}
	return res, err
}

// receive is the page's handling of a call that c delivered, with p.mu held.
func (c *propConn) receive(method string, params any) (json.RawMessage, error) {
	p := c.p
	switch method {
	case "Runtime.enable":
		err := p.enable(c)
		if err != nil && p.trace != nil {
			p.enableFailAt = propAt{c.n, c.target}
		}
		return nil, err
	case "Runtime.disable":
		if p.state.Needs("Runtime") {
			p.broken = append(p.broken, "Runtime.disable sent while an owner holds Runtime")
		}
		c.runtime = webkit.AgentDisabled
		return json.RawMessage(`{}`), nil
	case "Runtime.evaluate":
		m := params.(map[string]any)
		id, _ := m["contextId"].(int64)
		if id == 0 {
			return json.RawMessage(`{"result":{"type":"boolean","value":true}}`), nil // the main context
		}
		frame, ok := c.issued[c.target][int(id)]
		if ok && p.trace != nil {
			p.trace.evals[frame] = true
		}
		if !ok {
			p.broken = append(p.broken, fmt.Sprintf("context %d sent to target %s of connection %d, which never reported it", id, c.target, c.n))
			return nil, &webkit.ErrorData{Code: -32000, Message: errNoContext}
		}
		if !p.alive[frame] || c.ids[frame] != int(id) {
			return nil, &webkit.ErrorData{Code: -32000, Message: errNoContext}
		}
		p.holds[frame] = m["expression"] == "install"
		return json.RawMessage(`{"result":{"type":"boolean","value":true}}`), nil
	}
	return json.RawMessage(`{}`), nil
}

func (c *propConn) Done() <-chan struct{} { return c.done }

func (c *propConn) Close() error {
	close(c.done)
	return nil
}

// Probe answers pageConn's liveness probe.
func (c *propConn) Probe(context.Context) error { return nil }

func (c *propConn) ActiveTarget() string {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	return c.target
}

func (c *propConn) ExecutionContexts(target string) []webkit.ExecutionContext {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	if target != c.target {
		return nil // a replaced target has no contexts
	}
	var out []webkit.ExecutionContext
	for f, id := range c.ids {
		out = append(out, normalCtx(id, f))
	}
	return out
}

func (c *propConn) AgentStates(target string) map[string]webkit.AgentState {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	if target != c.target || c.runtime == 0 {
		return nil
	}
	return map[string]webkit.AgentState{"Runtime": c.runtime}
}

// reconnect opens connection n+1 with the enable mode, re-applies the
// desired state there (a Runtime.enable when an owner holds Runtime) and
// makes it the page connection's current one (pageConn.swap). early: the
// re-apply's reports come before the scheduler's reconnect step (a
// provisional setup during the open).
func (p *propPage) reconnect(s *Scheduler, pc *pageConn, mode int, early bool) {
	p.mu.Lock()
	c := p.newConn(mode)
	p.mu.Unlock()
	reapply := func() {
		if p.state.Needs("Runtime") {
			p.mu.Lock()
			_ = p.enable(c) // a lost reply leaves the owners as they are
			p.mu.Unlock()
		}
	}
	if early {
		reapply()
		s.contextsMoved()
	} else {
		s.contextsMoved()
		reapply()
	}
	p.mu.Lock()
	p.cur = c
	p.mu.Unlock()
	pc.swap(c)
}

// recorded reports whether the book keeps frame for epoch 1.
func recorded(s *Scheduler, frame string) bool {
	s.ctxs.mu.Lock()
	defer s.ctxs.mu.Unlock()
	ec := s.ctxs.installed[1]
	if ec == nil {
		return false
	}
	return ec.stale[frame] || slices.ContainsFunc(slices.Collect(maps.Keys(ec.ids)), func(r ExecContext) bool { return r.Frame == frame })
}

// recordedFrames returns the frames that epoch 1 records, in a context
// or as a frame of an earlier connection or target.
func recordedFrames(s *Scheduler) map[string]bool {
	s.ctxs.mu.Lock()
	defer s.ctxs.mu.Unlock()
	out := map[string]bool{}
	if ec := s.ctxs.installed[1]; ec != nil {
		maps.Copy(out, ec.stale)
		for r := range ec.ids {
			out[r.Frame] = true
		}
	}
	return out
}

// staleCount is the number of frames that epoch 1 recorded on an earlier
// connection or target.
func staleCount(s *Scheduler) int {
	s.ctxs.mu.Lock()
	defer s.ctxs.mu.Unlock()
	if ec := s.ctxs.installed[1]; ec != nil {
		return len(ec.stale)
	}
	return 0
}

func bookOf(s *Scheduler) string {
	s.ctxs.mu.Lock()
	defer s.ctxs.mu.Unlock()
	if ec := s.ctxs.installed[1]; ec != nil {
		var ids []string
		for r := range ec.ids {
			ids = append(ids, fmt.Sprintf("%d@%s(%s)", r.ID, r.target, r.Frame))
		}
		slices.Sort(ids)
		return fmt.Sprint(ids, ec.stale)
	}
	return "none"
}

// propOps is the number of step kinds of TestFrameContextsProperty.
const propOps = 18

// Kinds of an interruption between the lookup of a frame's context and
// the send of its id.
const (
	betweenCommit    = iota // a commit replaces the target
	betweenReconnect        // a reconnect replaces the connection
)

func TestFrameContextsProperty(t *testing.T) {
	const (
		frameOwner = Owner("frame:s1")
		otherOwner = Owner("legacy:s2")
		budget     = 200 * time.Millisecond // half of it is below frameReportWait
		slack      = 60 * time.Millisecond  // the release's own sends
	)
	var reach struct {
		gone, waited, expired, early, late, resolved, commits int
		// selections and releases whose send met a replaced target or
		// connection, by kind
		selCommit, selReconnect, relCommit, relReconnect int
		// owner-change cleanups while a refused frame is recorded, with an
		// ended budget, an incomplete and a complete inventory; frames
		// forgotten after a release call or a temporary Runtime.enable that
		// reached the page and failed or got no answer; refused frames kept
		// by a cleanup
		retryEnded, retryIncomplete, retryComplete, forgotAfterReach, keptRefused int
		// owner-change cleanups whose connection closed before the write
		// of a call, or after it
		closedBefore, closedAfter int
	}
	prop := func(ops []uint16) bool {
		state := NewDesiredState()
		p := &propPage{state: state, alive: map[string]bool{"f1": true, "f2": true, "f3": true}, holds: map[string]bool{}, failedAt: map[string]propAt{}}
		p.cur = p.newConn(enableAnswered)
		pc := newPageConn(p.cur, func() {}, func(func(context.Context)) bool { return false }, func(string, ...any) {})
		defer pc.close()
		s := NewScheduler(pc, SchedulerOptions{})
		defer s.Close()
		s.SetLease("s1", 1)
		ps := &PageSession{State: state, Sched: s}
		frames := []string{"f1", "f2", "f3"}
		// refusedOnce: frames whose release send was refused for a
		// replaced target or connection. abandoned: frames that an
		// owner-change cleanup forgot after a Runtime.enable that reached
		// the page and failed: their page-side state is given up by rule.
		refusedOnce, abandoned := map[string]bool{}, map[string]bool{}
		live := func() (context.Context, context.CancelFunc) { return context.WithTimeout(context.Background(), budget) }
		// interrupt runs the interruption kind between a lookup and its
		// send; it reports the kind, or -1 for none.
		interrupt := func(kind, mode int) int {
			switch kind {
			case betweenCommit:
				p.commit()
			case betweenReconnect:
				p.reconnect(s, pc, mode, true)
			default:
				return -1
			}
			return kind
		}
		for i, op := range ops {
			arg := int(op / propOps)
			frame := frames[arg%len(frames)]
			between := (arg / 3) % 6 // betweenCommit, betweenReconnect, or none
			switch op % propOps {
			case 0, 1, 2: // frame <f>: hold Runtime, find the frame's context, install it in an operation
				p.mu.Lock()
				p.cur.mode = arg % 3
				p.mu.Unlock()
				ctx, cancel := live()
				if acquireFor(ctx, state, frameOwner, pc, "Runtime") == nil {
					if ec := s.FrameContext(frame); ec.ID != 0 {
						kind := interrupt(between, arg%3)
						o, err := s.Begin(ctx, OpSpec{Session: "s1", Epoch: 1, Name: "frame", Class: ClassRead})
						if err != nil {
							t.Logf("step %d: Begin: %v", i, err)
							cancel()
							return false
						}
						o.SetExecutionContext(ec)
						_, err = o.Caller().Send(ctx, "Runtime.evaluate", map[string]any{"expression": "install"})
						o.End()
						if err == nil {
							delete(abandoned, frame)
						}
						if kind >= 0 && !targetReplaced(err) {
							t.Logf("step %d: the install met a replaced target or connection (kind %d) and got %v, want ErrTargetSwapped", i, kind, err)
							cancel()
							return false
						}
						switch kind {
						case betweenCommit:
							reach.selCommit++
						case betweenReconnect:
							reach.selReconnect++
						}
					}
				}
				cancel()
			case 3: // frame main
				_, _ = state.Release(context.Background(), frameOwner, pc)
			case 4: // another owner takes Runtime
				_ = acquireFor(context.Background(), state, otherOwner, pc, "Runtime")
			case 5: // and gives it back
				_, _ = state.Release(context.Background(), otherOwner, pc)
			case 6, 7: // reconnect
				early := op%propOps == 6
				if early {
					reach.early++
				} else {
					reach.late++
				}
				p.reconnect(s, pc, arg%3, early)
			case 8: // the frame navigates: its state and context end, a new one is reported when Runtime is enabled
				p.mu.Lock()
				if p.alive[frame] {
					p.holds[frame] = false
					delete(p.cur.ids, frame)
					if p.cur.runtime == webkit.AgentEnabled {
						p.report(p.cur)
					}
				}
				p.mu.Unlock()
			case 9: // the frame is removed
				p.mu.Lock()
				p.alive[frame], p.holds[frame] = false, false
				delete(p.cur.ids, frame)
				p.mu.Unlock()
			case 10: // a late enable's reports arrive, or a commit
				if arg%2 == 0 {
					p.deliver()
				} else {
					reach.commits++
					p.commit()
				}
			case 11, 12, 13: // release with a live deadline, maybe interrupted before its first send
				owners := state.AllOwners()
				stale := staleCount(s)
				kind := -1
				var refused []string // frames of sends refused for a replaced target or connection
				release := func(ctx context.Context, c webkit.Caller) error {
					if kind < 0 {
						kind = interrupt(between, arg%3)
						between = -1 // once per release
					}
					_, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "release"})
					if targetReplaced(err) {
						refused = append(refused, c.(contextCaller).ec.Frame)
					}
					return err
				}
				ctx, cancel := live()
				start := time.Now()
				p.mu.Lock()
				p.startTrace()
				p.mu.Unlock()
				err := ps.ReleaseFrameContexts(ctx, 1, "s1", pc, release)
				took := time.Since(start)
				cancel()
				p.mu.Lock()
				p.trace = nil
				p.mu.Unlock()
				if errors.Is(err, errFrameGone) {
					reach.gone++
				}
				if staleCount(s) < stale && !errors.Is(err, errFrameGone) {
					reach.resolved++ // a frame of an earlier connection released in its new context
				}
				if len(refused) > 0 {
					switch kind {
					case betweenCommit:
						reach.relCommit++
					case betweenReconnect:
						reach.relReconnect++
					}
					if err == nil {
						t.Logf("step %d: a release whose send found its target replaced returned nil", i)
						return false
					}
					for _, f := range refused {
						refusedOnce[f] = true
						p.mu.Lock()
						alive := p.alive[f]
						p.mu.Unlock()
						if f != "" && alive && !recorded(s, f) {
							t.Logf("step %d: frame %s was forgotten after its release was refused (%v)", i, f, err)
							return false
						}
					}
				}
				if took > budget/2+slack {
					t.Logf("step %d: the release took %v with a %v budget", i, took, budget)
					return false
				}
				if took >= budget/2 {
					reach.waited++
				}
				if got := state.AllOwners(); !slices.Equal(got, owners) {
					t.Logf("step %d: owners %q after the release, want %q", i, got, owners)
					return false
				}
			case 14, 15: // release with an expired or canceled context
				reach.expired++
				owners, book := state.AllOwners(), bookOf(s)
				p.mu.Lock()
				holds := maps.Clone(p.holds)
				p.mu.Unlock()
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
				if op%propOps == 15 {
					ctx, cancel = context.WithCancel(context.Background())
					cancel()
				}
				_ = ps.ReleaseFrameContexts(ctx, 1, "s1", pc, func(ctx context.Context, c webkit.Caller) error {
					_, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "release"})
					return err
				})
				cancel()
				p.mu.Lock()
				same := maps.Equal(holds, p.holds)
				p.mu.Unlock()
				if got := bookOf(s); got != book || !same || !slices.Equal(state.AllOwners(), owners) {
					t.Logf("step %d: an ended release changed the book %s -> %s, page state %v, owners %q", i, book, got, same, state.AllOwners())
					return false
				}
			case 16, 17: // owner-change cleanup through an operation (commands releaseEarlierFrames): live, or ended budget
				before, book := recordedFrames(s), bookOf(s)
				p.mu.Lock()
				complete := p.cur.runtime == webkit.AgentEnabled
				tr := p.startTrace()
				p.mu.Unlock()
				if slices.ContainsFunc(slices.Collect(maps.Keys(refusedOnce)), func(f string) bool { return before[f] }) {
					switch {
					case op%propOps == 17:
						reach.retryEnded++
					case complete:
						reach.retryComplete++
					default:
						reach.retryIncomplete++
					}
				}
				octx, ocancel := context.WithTimeout(context.Background(), 5*time.Second)
				o, err := s.Begin(octx, OpSpec{Session: "s1", Epoch: 1, Name: "cleanup", Class: ClassRead})
				if err != nil {
					ocancel()
					t.Logf("step %d: Begin: %v", i, err)
					return false
				}
				ctx, cancel := live()
				closing := closeNone
				if op%propOps == 17 {
					cancel()
				} else {
					closing = (arg / 18) % 4
				}
				p.mu.Lock()
				p.closing = closing
				p.mu.Unlock()
				kind := -1
				err = ps.ReleaseFrameContexts(ctx, 1, "s1", o.Caller(), func(ctx context.Context, c webkit.Caller) error {
					if kind < 0 && op%propOps == 16 {
						kind = interrupt(between, arg%3)
						between = -1
					}
					_, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "release"})
					if targetReplaced(err) {
						refusedOnce[c.(contextCaller).ec.Frame] = true
					}
					return err
				})
				cancel()
				if err != nil {
					s.ForgetFailedExecutionContexts(1)
				}
				o.End()
				ocancel()
				p.mu.Lock()
				if closing != closeNone && p.closing == closeNone {
					if closing == closeAfter {
						reach.closedAfter++
					} else {
						reach.closedBefore++
					}
				}
				p.closing = closeNone
				p.trace = nil
				cur := propAt{p.cur.n, p.cur.target}
				enableCurrent := p.enableFailAt == cur
				failedAt := maps.Clone(p.failedAt)
				holds := maps.Clone(p.holds)
				p.mu.Unlock()
				after := recordedFrames(s)
				if op%propOps == 17 && bookOf(s) != book {
					t.Logf("step %d: an owner-change cleanup with an ended budget changed the book %s -> %s", i, book, bookOf(s))
					return false
				}
				for f := range before {
					if after[f] {
						if refusedOnce[f] {
							reach.keptRefused++
						}
						continue
					}
					gone := err != nil && strings.Contains(err.Error(), fmt.Sprintf("frame %s: %v", f, errFrameGone))
					if !tr.evals[f] && failedAt[f] != cur && !enableCurrent && !gone {
						t.Logf("step %d (op %d): frame %s forgotten without a send that reached it, a release attempt that reached the current target and failed, or a gone decision (err %v)", i, op%propOps, f, err)
						return false
					}
					if !gone && (failedAt[f] == cur || !tr.evals[f]) {
						reach.forgotAfterReach++
						if holds[f] {
							abandoned[f] = true
						}
					}
				}
				for f := range refusedOnce {
					if !after[f] {
						delete(refusedOnce, f)
					}
				}
			}
			p.mu.Lock()
			broken := slices.Clone(p.broken)
			var lost []string
			for f, h := range p.holds {
				if h && !recorded(s, f) && !abandoned[f] {
					lost = append(lost, f)
				}
			}
			p.mu.Unlock()
			if len(broken) > 0 || len(lost) > 0 {
				t.Logf("step %d (op %d): violations %q; live frames with page-side state that the book lost: %q", i, op%propOps, broken, lost)
				return false
			}
			if o := state.AllOwners(); slices.ContainsFunc(o, func(o Owner) bool { return strings.HasPrefix(string(o), frameReleaseKind+":") }) {
				t.Logf("step %d: temporary owner left: %q", i, o)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
	if reach.gone == 0 || reach.waited == 0 || reach.expired == 0 || reach.early == 0 || reach.late == 0 || reach.resolved == 0 ||
		reach.commits == 0 || reach.selCommit == 0 || reach.selReconnect == 0 || reach.relCommit == 0 || reach.relReconnect == 0 ||
		reach.retryEnded == 0 || reach.retryIncomplete == 0 || reach.retryComplete == 0 || reach.forgotAfterReach == 0 || reach.keptRefused == 0 ||
		reach.closedBefore == 0 || reach.closedAfter == 0 {
		t.Fatalf("generator reach %+v: want each case", reach)
	}
	t.Logf("reach %+v", reach)
}
