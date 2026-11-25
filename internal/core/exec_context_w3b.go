package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edxeth/agent-ios/internal/logsafe"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// Execution contexts of operations (design docs/design/agentic-parity.md,
// section 3.8): a session that selected a frame runs its page work in the
// frame's execution context. The operation carries the WIP context id;
// every Runtime.evaluate it sends gets that contextId, so the runtime is
// installed and called in the frame's global object, and its attempts
// use a clock mapping of that context (the frame's performance.now() has
// its own origin and document id).

// ExecContext is a WIP execution context id together with the connection
// and the target that reported it (Scheduler.FrameContext). A context id
// is valid only on its reporting target: another target, also the target
// that a commit put in its place on the same connection, and every target
// of another connection number their contexts on their own, so the same
// id can name another frame's context there, or none. Every send that
// carries the id therefore goes to the reporting target (pin) and fails
// when that target or its connection is gone or replaced
// (webkit.ErrTargetSwapped). The zero value is the page's main context.
type ExecContext struct {
	ID    int64
	Frame string // the frame whose main world it is; "" when unknown

	target string        // reporting target; "" on a connection without targets
	conn   webkit.Caller // reporting connection; nil when unknown
}

// Source returns the target and the connection that reported the
// context; a CDP attachment compares them with the document that it
// knows before it uses the context (design 5.3).
func (ec ExecContext) Source() (target string, conn webkit.Caller) { return ec.target, ec.conn }

// connPinKey is the context key of the connection that a send must reach
// (ExecContext.pin, pageConn.send).
type connPinKey struct{}

// errConnReplaced: the connection that reported an execution context is no
// longer the page's connection (a reconnect), so its targets are gone for
// the daemon.
var errConnReplaced = fmt.Errorf("the connection that reported the execution context was replaced: %w", webkit.ErrTargetSwapped)

// pin addresses ctx's sends to the context's reporting connection and
// target: webkit.WithTarget makes the session send to that target or fail
// with ErrTargetSwapped when a commit replaced it, and pageConn.send
// refuses a send when the page's connection is another one.
func (ec ExecContext) pin(ctx context.Context) context.Context {
	if ec.target != "" {
		ctx = webkit.WithTarget(ctx, ec.target)
	}
	if ec.conn != nil {
		ctx = context.WithValue(ctx, connPinKey{}, ec.conn)
	}
	return ctx
}

// pinnedConn returns the connection that ctx's sends must reach, if any.
func pinnedConn(ctx context.Context) (webkit.Caller, bool) {
	c, ok := ctx.Value(connPinKey{}).(webkit.Caller)
	return c, ok
}

// addressed reports whether ctx names the target or the connection of its
// sends (a desired-state change to a provisional target, a frame release).
func addressed(ctx context.Context) bool {
	_, target := webkit.TargetOf(ctx)
	_, conn := pinnedConn(ctx)
	return target || conn
}

// SetExecutionContext makes the operation work in the execution context
// ec; the zero ExecContext is the page's main context. The scheduler
// records a non-main context for the operation's lease epoch, because the
// runtime may be installed there (ReleaseExecutionContexts).
func (op *Op) SetExecutionContext(ec ExecContext) {
	op.exec.Store(&ec)
	if ec.ID != 0 {
		op.s.installContext(op.Spec.Epoch, ec)
	}
}

// ExecutionContext returns the context set by SetExecutionContext.
func (op *Op) ExecutionContext() ExecContext {
	if ec := op.exec.Load(); ec != nil {
		return *ec
	}
	return ExecContext{}
}

// route returns the context and the parameters of one send of the
// operation. With a non-main execution context, the send goes to the
// context's reporting connection and target (ExecContext.pin), and a
// Runtime.evaluate that names no context gets the context's id. Every
// command of such an operation goes there, because its object ids embed
// the context's id too (WebKit's injectedScriptId). A send whose ctx names
// its own target or connection (addressed) is left as it is and gets no
// id, which is valid only on its reporting target. Every Runtime.evaluate
// of the daemon passes its parameters as a map.
func (op *Op) route(ctx context.Context, method string, params any) (context.Context, any) {
	ec := op.exec.Load()
	if ec == nil || ec.ID == 0 || addressed(ctx) {
		return ctx, params
	}
	return ec.pin(ctx), withContextID(method, params, ec.ID)
}

// withContextID returns params with contextId id for a Runtime.evaluate
// whose parameter map names no context.
func withContextID(method string, params any, id int64) any {
	p, ok := params.(map[string]any)
	if method != "Runtime.evaluate" || !ok {
		return params
	}
	if _, set := p["contextId"]; set {
		return params
	}
	q := maps.Clone(p)
	q["contextId"] = id
	return q
}

// clockKey names one clock mapping: a document generation and the
// execution context whose clock it maps.
type clockKey struct {
	gen uint64
	ctx int64
}

func (op *Op) clockKey(gen uint64) clockKey { return clockKey{gen: gen, ctx: op.ExecutionContext().ID} }

// Execution contexts of the page (design 2.4, 3.8). WIP names a frame's
// execution context only in Runtime.executionContextCreated, which WebKit
// sends for every existing context only when Runtime is enabled (a second
// Runtime.enable reports nothing; PageRuntimeAgent::enable). The page's
// connection keeps that inventory per target from its start, in its read
// loop (webkit.Session.ExecutionContexts, W1c), also for the reports of a
// provisional target's setup, which runs before any subscriber of the
// daemon exists. The scheduler reads it from its connection (inventory).
// A connection opened by an explicit WebSocket URL is a webkit.Client,
// which keeps the same inventory for its one target (ContextInventory)
// but no agent states.
//
// The scheduler records, per lease epoch, the non-main contexts that
// operations selected, each with its reporting target and connection
// (ExecContext): the runtime may hold page-side state there (dialog
// overrides, ref maps), which the release of that epoch must undo in each
// of them.
//
// A reconnect (recovery, takeover, reopen) opens a new inspector
// connection, on which the old context ids are void: the new connection
// reports the same contexts under new ids (seen on WebKitGTK, where a
// frame's context 4 became 8; TestFrameContextsAcrossReconnectOnWebKitGTK),
// and an old id may name another context there. The reconnect therefore
// turns each recorded context into its frame id (contextsMoved), and a
// release finds the frame's context on the connection it sends through,
// at the moment it sends. When no owner holds Runtime on the new
// connection ("frame main" released the last one before the reconnect),
// the inventory has no context of the frame; the release then holds
// Runtime for itself (PageSession.ReleaseFrameContexts). A commit that
// replaces the reporting target on the same connection voids its ids the
// same way: a release whose send finds the target replaced keeps the
// context's frame for the next release (ReleaseExecutionContexts).
type contextBook struct {
	mu        sync.Mutex
	installed map[uint64]*epochContexts // lease epoch: contexts selected under it
}

// epochContexts are the contexts that operations of one lease epoch
// selected.
type epochContexts struct {
	ids   map[ExecContext]bool // contexts with their reporting target and connection
	stale map[string]bool      // frame ids whose recorded context is void: its target or connection was replaced
	// reached: frames of stale whose attempt in the latest release
	// (ReleaseFrameContexts, which clears the marks first) reached the page
	// and failed or got no answer, with the target and connection it
	// reached: a release send, or a temporary Runtime.enable for the
	// frame. reachedIDs: the same for ids. A record without such an
	// attempt on the current target is never forgotten
	// (ForgetFailedExecutionContexts).
	reached    map[string]ExecContext
	reachedIDs map[ExecContext]bool
}

// reachedPage reports whether a send that failed with err reached the page,
// on positive evidence only: the connection wrote its message (written,
// set through webkit.WithWriteNotice), or the page answered with an
// error. A failure that the connection marks as not written
// (webkit.ErrNotSent) or as addressed to a replaced target or connection
// did not reach it, and a failure without a marker is no evidence either
// way.
func reachedPage(written *atomic.Bool, err error) bool {
	if errors.Is(err, webkit.ErrNotSent) || targetReplaced(err) {
		return false
	}
	var pe *webkit.ErrorData
	return written.Load() || errors.As(err, &pe)
}

// contextSource is a connection that keeps the execution contexts and the
// agent states of its targets: webkit.Session.
type contextSource interface {
	ActiveTarget() string
	ExecutionContexts(target string) []webkit.ExecutionContext
	AgentStates(target string) map[string]webkit.AgentState
}

// clientInventory is a connection with one target and its execution
// contexts but no agent states: webkit.Client, which core opens for a page
// given by an explicit WebSocket URL (ManagerOptions.OpenURL).
type clientInventory interface {
	ContextInventory() (target string, contexts []webkit.ExecutionContext)
}

// inventory is what the scheduler's connection reported about the
// execution contexts of its active target, read at one moment.
type inventory struct {
	conn     webkit.Caller
	target   string
	contexts []webkit.ExecutionContext
	// complete: Runtime is enabled on the target with a known outcome
	// (webkit.AgentEnabled), so WebKit reported every context before the
	// enable's reply and every later one since. Without that, a frame
	// missing from the inventory may still exist.
	complete bool
}

// currentConn returns the connection that the scheduler sends through now:
// the current connection of a pageConn, which reconnects replace, or the
// scheduler's own caller.
func (s *Scheduler) currentConn() webkit.Caller {
	if pc, ok := s.c.(interface{ currentConn() webkit.Caller }); ok {
		return pc.currentConn()
	}
	return s.c
}

// inventory reads the execution contexts of the active target of the
// scheduler's current connection. Completeness is read before the
// contexts: a report and an enable reply that land between the two reads
// are then seen with the report, never as a complete inventory without it.
func (s *Scheduler) inventory() inventory {
	conn := s.currentConn()
	switch src := conn.(type) {
	case contextSource:
		target := src.ActiveTarget()
		complete := src.AgentStates(target)["Runtime"] == webkit.AgentEnabled
		return inventory{conn: conn, target: target, contexts: src.ExecutionContexts(target), complete: complete}
	case clientInventory:
		target, list := src.ContextInventory()
		return inventory{conn: conn, target: target, contexts: list}
	}
	return inventory{conn: conn}
}

// activeTarget returns the target of conn that receives commands now.
func activeTarget(conn webkit.Caller) string {
	switch src := conn.(type) {
	case contextSource:
		return src.ActiveTarget()
	case clientInventory:
		target, _ := src.ContextInventory()
		return target
	}
	return ""
}

// current reports whether inv's connection and target still receive the
// scheduler's commands: no reconnect and no commit since inv was read.
func (s *Scheduler) current(inv inventory) bool {
	conn := s.currentConn()
	return conn == inv.conn && activeTarget(conn) == inv.target
}

// mainWorld reports whether c is a frame's page world (type normal), not
// an isolated world of an extension or of WebKit itself.
func mainWorld(c webkit.ExecutionContext) bool { return c.Type == "normal" && c.FrameID != "" }

// frame returns the newest main-world context of the frame frameID in
// inv, bound to inv's connection and target, or the zero ExecContext.
func (inv inventory) frame(frameID string) ExecContext {
	var id int64
	for _, c := range inv.contexts {
		if mainWorld(c) && c.FrameID == frameID {
			id = max(id, int64(c.ID))
		}
	}
	if id == 0 {
		return ExecContext{}
	}
	return ExecContext{ID: id, Frame: frameID, target: inv.target, conn: inv.conn}
}

// FrameContext returns the newest main-world execution context of the
// frame frameID that the page's active target reported, with that target
// and its connection, or the zero ExecContext.
func (s *Scheduler) FrameContext(frameID string) ExecContext {
	return s.inventory().frame(frameID)
}

func (s *Scheduler) installContext(epoch uint64, ctx ExecContext) {
	b := &s.ctxs
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.installed == nil {
		b.installed = map[uint64]*epochContexts{}
	}
	ec := b.installed[epoch]
	if ec == nil {
		ec = &epochContexts{ids: map[ExecContext]bool{}, stale: map[string]bool{}, reached: map[string]ExecContext{}, reachedIDs: map[ExecContext]bool{}}
		b.installed[epoch] = ec
	}
	ec.ids[ctx] = true
	delete(ec.reachedIDs, ctx) // newly installed state: no attempt has reached it yet
}

// contextsMoved records that the page's connection is replaced: a
// reconnect calls it when the new connection is open, before that
// connection replaces the scheduler's. Each recorded context becomes its
// frame id, whose context a later release finds on the connection it
// sends through; a recorded context of an unknown frame cannot be found
// on another connection and is dropped. The new connection's inventory
// needs no step here: it lists every context that its targets reported
// since it opened, also during a provisional target's setup. A context
// recorded after this step on the old connection keeps that connection,
// so its release is refused (errConnReplaced) and turns it into its frame.
func (s *Scheduler) contextsMoved() {
	b := &s.ctxs
	b.mu.Lock()
	defer b.mu.Unlock()
	for epoch, ec := range b.installed {
		for rec := range ec.ids {
			if rec.Frame != "" {
				ec.stale[rec.Frame] = true
			}
			delete(ec.ids, rec)
		}
		clear(ec.reachedIDs)
		if len(ec.stale) == 0 {
			delete(b.installed, epoch)
		}
	}
}

// ContextEpochsBefore returns the lease epochs below epoch that still have
// recorded execution contexts, in increasing order: their release did not
// run, or did not reach every context (a takeover, a failed release).
func (s *Scheduler) ContextEpochsBefore(epoch uint64) []uint64 {
	s.ctxs.mu.Lock()
	defer s.ctxs.mu.Unlock()
	var out []uint64
	for e := range s.ctxs.installed {
		if e < epoch {
			out = append(out, e)
		}
	}
	slices.Sort(out)
	return out
}

// errNoContext is WebKit's answer to a call in an execution context that no
// longer exists (PageRuntimeAgent::injectedScriptForEval).
const errNoContext = "Missing injected script for given executionContextId"

// ReleaseExecutionContexts runs release in every non-main execution context
// that operations of the lease epoch selected (SetExecutionContext), in
// increasing id order, through c with the context added to each
// Runtime.evaluate and every send addressed to the context's reporting
// target and connection (contextCaller), so a context id never reaches
// another target. A context that no longer exists counts as released.
// Released contexts are forgotten; a context whose release failed stays
// recorded, so a later release or owner change tries it again, and the
// errors are returned joined. Once ctx has ended, nothing more is sent and
// no record changes. A context whose reporting target or
// connection was replaced (webkit.ErrTargetSwapped, also errConnReplaced,
// or ErrTargetAbandoned) was not released: the error is returned and the
// context becomes its frame for a later release. A frame recorded so
// (contextsMoved, or such a replacement) is released in its newest
// main-world context on the current connection's active target, looked up
// now and never kept. A navigation of the frame since then leaves a
// context without the epoch's state, where the page-side release changes
// nothing (runtime.Release: the page releases only its owner's epoch and
// installs nothing). A frame without a context on the current connection
// cannot be reached: it stays recorded, with an error
// (errContextNotReported). A failed release whose last send reached the
// page (reachedPage) is marked for ForgetFailedExecutionContexts; a
// refused one clears the mark of its frame, and an ended ctx, a frame
// without a context and a send without evidence of its write leave the
// marks as they are.
func (s *Scheduler) ReleaseExecutionContexts(ctx context.Context, epoch uint64, c webkit.Caller, release func(context.Context, webkit.Caller) error) error {
	b := &s.ctxs
	b.mu.Lock()
	var recs []ExecContext
	var stale []string
	if ec := b.installed[epoch]; ec != nil {
		recs = slices.SortedFunc(maps.Keys(ec.ids), func(a, b ExecContext) int {
			return cmp.Or(cmp.Compare(a.ID, b.ID), strings.Compare(a.target, b.target))
		})
		stale = slices.Sorted(maps.Keys(ec.stale))
	}
	b.mu.Unlock()
	var errs []error
	// ended reports a ctx that has ended: nothing more is sent and no
	// record changes, because whether a send would reach its context is
	// not known any more.
	ended := func(kind logsafe.Label, id any) bool {
		if ctx.Err() == nil {
			return false
		}
		errs = append(errs, logsafe.Errorf("%s %v: %w: %w", kind, id, webkit.ErrNotSent, context.Cause(ctx)))
		return true
	}
	for _, frame := range stale {
		if ended("frame", logsafe.ID(frame)) {
			continue
		}
		fc := s.FrameContext(frame)
		if fc.ID == 0 {
			errs = append(errs, logsafe.Errorf("frame %s: %w", logsafe.ID(frame), errContextNotReported))
			continue
		}
		written := new(atomic.Bool)
		if err := release(ctx, contextCaller{Caller: c, ec: fc, written: written}); err != nil && !contextGone(err) {
			errs = append(errs, logsafe.Errorf("frame %s (execution context %d): %w", logsafe.ID(frame), fc.ID, err))
			b.forget(epoch, func(ec *epochContexts) {
				switch {
				case !ec.stale[frame]:
				case targetReplaced(err):
					delete(ec.reached, frame)
				case reachedPage(written, err):
					ec.reached[frame] = fc
				}
			})
			continue
		}
		b.forget(epoch, func(ec *epochContexts) { delete(ec.stale, frame); delete(ec.reached, frame) })
	}
	for _, rec := range recs {
		if ended("execution context", rec.ID) {
			continue
		}
		written := new(atomic.Bool)
		err := release(ctx, contextCaller{Caller: c, ec: rec, written: written})
		switch {
		case err == nil || contextGone(err):
			b.forget(epoch, func(ec *epochContexts) { delete(ec.ids, rec); delete(ec.reachedIDs, rec) })
		case targetReplaced(err):
			errs = append(errs, fmt.Errorf("execution context %d: %w", rec.ID, err))
			b.forget(epoch, func(ec *epochContexts) {
				delete(ec.ids, rec)
				delete(ec.reachedIDs, rec)
				if rec.Frame != "" {
					ec.stale[rec.Frame] = true
					delete(ec.reached, rec.Frame)
				}
			})
		default:
			errs = append(errs, fmt.Errorf("execution context %d: %w", rec.ID, err))
			if reachedPage(written, err) {
				b.forget(epoch, func(ec *epochContexts) {
					if ec.ids[rec] {
						ec.reachedIDs[rec] = true
					}
				})
			}
		}
	}
	return errors.Join(errs...)
}

// targetReplaced reports whether err says that the target a send was
// addressed to, or its connection, is gone or replaced.
func targetReplaced(err error) bool {
	return errors.Is(err, webkit.ErrTargetSwapped) || errors.Is(err, webkit.ErrTargetAbandoned)
}

// forget applies drop to the records of the lease epoch and drops the
// epoch when no record is left.
func (b *contextBook) forget(epoch uint64, drop func(*epochContexts)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ec := b.installed[epoch]; ec != nil {
		drop(ec)
		if len(ec.ids) == 0 && len(ec.stale) == 0 {
			delete(b.installed, epoch)
		}
	}
}

// errContextNotReported: a frame whose context an earlier connection
// recorded has no context in the current connection's inventory (it is
// gone, or Runtime is not enabled there), so its state cannot be reached.
var errContextNotReported = errors.New("the frame reported no execution context on the current connection")

// errFrameGone: a frame whose context an earlier connection recorded has
// no context in the current connection's inventory while that inventory
// is complete (Runtime enabled with a known outcome). WebKit reports every
// existing context when Runtime is enabled, so the frame no longer exists,
// and its page-side state with it.
var errFrameGone = errors.New("the frame reported no execution context while Runtime was enabled on the current connection; it is gone, so its state is counted as released")

// frameReleaseKind is the owner kind (SessionOwner) of the temporary
// Runtime ownership of a frame release (ReleaseFrameContexts).
const frameReleaseKind = "frame-release"

// Bounds of the wait for the frame reports of a frame release.
const (
	// frameReportWait is the longest wait. WebKit sends every context
	// report before the reply to Runtime.enable (PageRuntimeAgent::enable),
	// and the connection records them in its read loop, so the inventory
	// is complete when an enable returns. The wait covers an enable of
	// another owner whose reply is still on its way (the temporary owner
	// joined it without a send).
	frameReportWait = 250 * time.Millisecond
	// frameReportPoll is the interval between two looks at the inventory.
	frameReportPoll = 10 * time.Millisecond
)

// ReleaseFrameContexts runs ReleaseExecutionContexts of the lease epoch
// for session, the session that releases (the epoch's own at a lease
// release, the next owner's at an owner change). When a frame recorded on
// an earlier connection has no context in the current connection's
// inventory (no owner held Runtime at the reconnect, so nothing enabled
// it there), it first holds Runtime as the temporary owner
// frame-release:<session> in the desired state (design 2.4: only the
// first owner sends Runtime.enable, and Runtime.disable goes out only when
// the last owner leaves), waits for the frames' contexts (awaitReports),
// runs the release in each one found, and gives the ownership back on
// every path. A frame that has no context while the inventory is complete
// is gone: its record is dropped and it is named in the returned error
// with errFrameGone, for the log. When Runtime could not be enabled, or
// the wait ended without a complete inventory, the frames stay recorded
// (errContextNotReported). A Runtime.enable that reached the page and
// failed or got no answer is the frames' release attempt: they are marked
// for ForgetFailedExecutionContexts with the target that the inventory
// named before the enable. Old context ids are never called
// (ReleaseExecutionContexts), and c fences every send as before. The
// acquisition and the give-back are changes of the desired state
// (DesiredState.Change), as every owner's enable and disable: the context
// replay sends its Runtime.disable and Runtime.enable inside a change, and
// no owner's Runtime call may run between them. A Runtime cleanup that an
// earlier release left pending (Runtime unknown) is reset first, in the
// acquisition's change, while no owner holds Runtime (ResetPendingRuntime);
// when that reset fails, nothing is acquired and the frames stay recorded. The give-back keeps a failed
// cleanup pending (ReleaseOwnerChange): a refused Runtime.disable leaves
// Runtime unknown after the owner left. When the give-back's change cannot
// begin before ctx ends, the owner leaves the model without it, as a lease
// release removes owners without the change lock (forgetOwners): Runtime
// becomes unknown, and the next change that uses it resets it.
func (ps *PageSession) ReleaseFrameContexts(ctx context.Context, epoch uint64, session string, c webkit.Caller, release func(context.Context, webkit.Caller) error) error {
	s := ps.Sched
	// The marks are this release's own: one that sends nothing (an ended
	// ctx, an incomplete inventory) leaves none, whatever earlier releases
	// reached.
	s.ctxs.forget(epoch, func(ec *epochContexts) { clear(ec.reached); clear(ec.reachedIDs) })
	inv := s.inventory()
	frames := s.unreportedIn(epoch, inv)
	if len(frames) == 0 {
		return s.ReleaseExecutionContexts(ctx, epoch, c, release)
	}
	owner := SessionOwner(frameReleaseKind, session)
	var errs []error
	written := new(atomic.Bool)
	var aerr error // the acquisition's own failure, without the change's
	ran := false
	err := ps.State.Change(ctx, c, func(c webkit.Caller) error {
		if err := ResetPendingRuntime(ctx, ps, c); err != nil {
			return err // not the enable: it reached no frame
		}
		ran = true
		aerr = acquireFor(webkit.WithWriteNotice(ctx, written), ps.State, owner, c, "Runtime")
		return aerr
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("enabling Runtime for the frame contexts: %w", err))
	}
	if aerr != nil {
		if reachedPage(written, aerr) {
			s.ctxs.forget(epoch, func(ec *epochContexts) {
				for _, f := range frames {
					if ec.stale[f] {
						ec.reached[f] = ExecContext{Frame: f, target: inv.target, conn: inv.conn}
					}
				}
			})
		}
	} else if ran {
		for _, frame := range s.awaitReports(ctx, epoch) {
			errs = append(errs, logsafe.Errorf("frame %s: %w", logsafe.ID(frame), errFrameGone))
		}
	}
	errs = append(errs, s.ReleaseExecutionContexts(ctx, epoch, c, release))
	ran, err = ps.ReleaseOwnerChange(ctx, c, owner)
	if err != nil {
		errs = append(errs, fmt.Errorf("giving back the temporary Runtime ownership: %w", err))
	}
	if !ran && slices.Contains(ps.State.AllOwners(), owner) {
		ps.State.forgetOwners(func(o Owner) bool { return o == owner }, true, nil)
	}
	return errors.Join(errs...)
}

// unreportedIn returns the frames of the lease epoch, recorded on an
// earlier connection or target, that have no context in the connection's
// inventory inv, sorted.
func (s *Scheduler) unreportedIn(epoch uint64, inv inventory) []string {
	b := &s.ctxs
	b.mu.Lock()
	var stale []string
	if ec := b.installed[epoch]; ec != nil {
		stale = slices.Sorted(maps.Keys(ec.stale))
	}
	b.mu.Unlock()
	return slices.DeleteFunc(stale, func(f string) bool { return inv.frame(f).ID != 0 })
}

// dropFrames drops the frames of the lease epoch that an earlier
// connection recorded and returns those it had, sorted.
func (b *contextBook) dropFrames(epoch uint64, frames []string) []string {
	var gone []string
	b.forget(epoch, func(ec *epochContexts) {
		for _, f := range frames {
			if ec.stale[f] {
				delete(ec.stale, f)
				delete(ec.reached, f)
				gone = append(gone, f)
			}
		}
	})
	slices.Sort(gone)
	return gone
}

// awaitReports waits until every frame of the lease epoch that an earlier
// connection recorded has a context in the current connection's
// inventory, at most frameReportWait and half the time left before ctx's
// deadline. When the inventory is complete (inventory) and a frame still
// has no context, the frame is gone: its record is dropped and it is
// returned. When ctx has ended (canceled or past its deadline), at the
// start or during the wait, or the wait ends before the inventory is
// complete, nothing is dropped: whether the frame lives is unknown. An
// inventory whose target or connection was replaced while it was read
// (current) decides nothing either: the frame may live on the new one.
func (s *Scheduler) awaitReports(ctx context.Context, epoch uint64) []string {
	wait := frameReportWait
	if dl, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(dl)/2)
	}
	end := time.Now().Add(wait)
	for {
		if ctx.Err() != nil {
			return nil
		}
		// One inventory read gives completeness and contexts of the same
		// target (completeness first, see inventory).
		inv := s.inventory()
		missing := s.unreportedIn(epoch, inv)
		if len(missing) == 0 {
			return nil
		}
		// ctx is checked again after the reads: a cancel between the check
		// above and the reads must not let a complete inventory decide
		// that the frame is gone.
		if inv.complete && ctx.Err() == nil && s.current(inv) {
			return s.ctxs.dropFrames(epoch, missing)
		}
		left := time.Until(end)
		if left <= 0 {
			return nil
		}
		t := time.NewTimer(min(frameReportPoll, left))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// contextGone reports whether err is WebKit's answer for a context that no
// longer exists.
func contextGone(err error) bool {
	var pe *webkit.ErrorData
	return errors.As(err, &pe) && strings.Contains(pe.Message, errNoContext)
}

// ForgetFailedExecutionContexts drops, after a release of the lease epoch
// (ReleaseFrameContexts) that did not reach all of its records, the
// records whose attempt in that release reached the page's current target
// and failed or got no answer: a send of the release that the connection
// wrote, or a temporary Runtime.enable for a frame (epochContexts.reached,
// reachedPage). Such a record is not tried again,
// so a context that never answers does not cost every later command its
// budget. Every other record stays for the next release, which finds a
// frame on the current target: a send that core refused because its
// target or connection was replaced, a release whose budget ended before
// its send, a frame without a context in an incomplete inventory, a send
// that was not written, and an attempt that reached a target which is no
// longer the current one.
func (s *Scheduler) ForgetFailedExecutionContexts(epoch uint64) {
	conn := s.currentConn()
	target := activeTarget(conn)
	current := func(ec ExecContext) bool { return ec.conn == conn && ec.target == target }
	s.ctxs.forget(epoch, func(ec *epochContexts) {
		maps.DeleteFunc(ec.ids, func(rec ExecContext, _ bool) bool { return ec.reachedIDs[rec] && current(rec) })
		maps.DeleteFunc(ec.stale, func(f string, _ bool) bool {
			at, ok := ec.reached[f]
			return ok && current(at)
		})
		maps.DeleteFunc(ec.reachedIDs, func(rec ExecContext, _ bool) bool { return !ec.ids[rec] })
		maps.DeleteFunc(ec.reached, func(f string, _ ExecContext) bool { return !ec.stale[f] })
	})
}

// contextCaller sends Runtime.evaluate calls to one execution context, and
// every call to that context's reporting target and connection
// (ExecContext.pin). With written, it records whether the connection
// wrote the message of its latest call (webkit.WithWriteNotice), the call
// whose failure ends a release.
type contextCaller struct {
	webkit.Caller
	ec      ExecContext
	written *atomic.Bool
}

func (c contextCaller) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if p, ok := params.(map[string]any); ok && method == "Runtime.evaluate" {
		q := maps.Clone(p)
		q["contextId"] = c.ec.ID
		params = q
	}
	if c.written != nil {
		c.written.Store(false)
		ctx = webkit.WithWriteNotice(ctx, c.written)
	}
	return c.Caller.Send(c.ec.pin(ctx), method, params)
}
