package cdp

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/logsafe"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// Event order, provisional targets and bounded staging (design 2.7, 5.2;
// W5b-1 review rounds 1 and 2, findings F1, F2 and F5).
//
// The attachment subscribes in the read loop of each connection of its
// page (core.Manager.SubscribeOrdered). There each event comes with its
// position: the delivering connection and that connection's active target
// right after it read the event. An event of a target that is not the
// active target at its position belongs to a provisional target (WebKit
// sends no events of a replaced target before its destruction), unless a
// Target.targetCreated announced that target as no provisional one (a
// worker). The commit (Target.didCommitProvisionalTarget) is the position
// where the target becomes active. So the classification never runs ahead
// of or behind the event, also for a provisional target that existed
// before the attachment and for the targets of a new connection. The read
// loop only queues the classified event (eventQueue); the attachment's
// goroutine (pageState.run) translates the queue in order under ps.mu.
//
// A provisional target's events are staged and translated at its commit
// in their order, so its document is published once, at the commit; a
// target destroyed without a commit takes its staged events with it.
//
// Connections: the attachment knows the connection that delivers its
// events (ps.conn, first the one installed when it subscribed). The first
// event of another connection (a reconnect) resets the document (see
// below): the page may have navigated while no connection delivered its
// events. Context-specific Runtime calls use that connection only
// (runtime_domain.go, wipContext).
//
// Bounds (F5): the queue holds at most maxQueuedEvents events and
// maxQueuedBytes of payload; the staged events of all provisional targets
// together at most maxStagedEvents events and maxStagedBytes, of at most
// maxStagedTargets targets. All loss bookkeeping is bounded too: the
// overflow marks belong to staged targets, the lost targets are a ring of
// lostMemory entries, and at most maxLiveTargets targets are remembered as
// no provisional ones. Nothing is published incomplete:
//   - A provisional target that would exceed the staging bounds loses its
//     staged events, and its later ones are not staged (overflow; the
//     target keeps its staging slot, marked). When maxStagedTargets targets
//     are staged, a new one pushes out the oldest, which is remembered as
//     lost. At the commit of such a target the attachment publishes a
//     reset instead of a partial document.
//   - When the queue is full, the events after it are dropped until the
//     goroutine has caught up; it then publishes a reset, forgets the
//     staged targets as lost, and resets again at the next commit (one may
//     have been dropped).
//   - When the loss record is no longer exact (the lost ring overwrote a
//     target whose commit could still come), the next commit resets too
//     (resetOnCommit).
//   - A commit whose staged events do not give the committed target's
//     document (none were seen, for example because the target reported
//     its document before the attachment existed) resets as well (F1): the
//     attachment never keeps the replaced target's document.
//   - A reset is Runtime.executionContextsCleared at once, then a fresh
//     frame tree read from the page (refreshAfterReset): frameNavigated of
//     the main frame with a new loader id as a new document, the init
//     lifecycle event, frameAttached and frameNavigated of each child
//     frame, and, with Runtime enabled, the new default contexts. So the
//     client reloads its state.

// Tree reads (W5b-1 review r3, F9; review r4, F10): a Page.getResourceTree
// read and the events run on the same connection, but the attachment's
// goroutine may lag behind the read loop. WebKit sends a target's events
// and the replies to its commands in one ordered stream (through iwdp in
// Target.dispatchMessageFromTarget, on the direct transport in the
// device's message order), and the session's read loop handles both in
// that order. So every tree read (readTree, the only path that seeds a
// document: Page.enable, Runtime.enable, the Page handlers and the read
// after a reset, a commit without a document or a new connection) is
// applied at the exact place of its reply in the event stream:
//   - The read loop queues the reply as one more item of the stream
//     (core.WithReplyPosition, treeRead.replied), after the events read
//     before it and before the events read after it. The goroutine applies
//     the tree when it gets to that item (treeLocked), and readTree
//     returns after that. So every event that the page sent before the
//     reply, which the tree reflects, is applied before it, and every
//     later event after it, also when the page returned to a document it
//     showed before (a back/forward restore repeats the target and loader
//     of that document; the place of the reply, not the document's
//     identity, orders the tree).
//   - Core keeps that order through a reconnect's hand-over too: a reply
//     that a new connection reads while its earlier events are still held
//     for the swap is held with them, in read order (core
//     pageConn.replyRead). So no event is ever discarded for coming
//     before a tree.
//   - At its place, the tree is the truth for the frames of its document
//     (seedLocked): frames that it lacks are removed, and the frames it
//     has take its data. It carries no contexts and no console records, so
//     it leaves them as they are.
//   - A reply of a target that was not the active one at its position (a
//     commit replaced it) is not applied, nor a read that started before a
//     reset (connSeq): the reset reads again.

// Bounds of the event queue and of the staged events (F5).
const (
	maxQueuedEvents  = 4096
	maxQueuedBytes   = 8 << 20
	maxStagedTargets = 4
	maxStagedEvents  = 1024
	maxStagedBytes   = 1 << 20
	lostMemory       = 8
	maxLiveTargets   = 64
	// resetReadBudget bounds the read of the frame tree after a reset.
	resetReadBudget = 10 * time.Second
)

// WIP Target events that move the targets.
const (
	evTargetCreated   = "Target.targetCreated"
	evTargetCommitted = "Target.didCommitProvisionalTarget"
	evTargetDestroyed = "Target.targetDestroyed"
	// evTreeReply is the method of a tree read's reply in the queue
	// (treeRead.replied); no WIP event has it.
	evTreeReply = "Page.getResourceTree"
)

// wipEvent is one WIP event at its position in the connection's stream.
type wipEvent struct {
	target, method string
	params         json.RawMessage
	provisional    bool           // target was not the active target at the event's position
	conn           webkit.Caller  // the delivering connection; nil in a model
	seq            uint64         // its position in conn's stream (core.EventPosition.Seq)
	tree           *treeRead      // the read whose reply this is (method evTreeReply); nil for an event
	at             float64        // arrival time (monotonic), the base of the Network clock offsets
	claim          *claimDecision // an interception event's arbitration (fetchClaims); nil in a model
	hold           uint64         // the generation of core's hold that an interception event announced (eventHold); 0 in a model
}

// size is what ev counts against maxQueuedBytes: its payload, or nothing
// for a tree read's reply, whose bytes the read holds anyway (readTree
// decodes the same reply).
func (ev wipEvent) size() int {
	if ev.tree != nil {
		return 0
	}
	return len(ev.params)
}

// eventAt is an event or a reply of target read at pos: its target is
// provisional when it is not the active target at that position.
func eventAt(pos core.EventPosition, target, method string, params json.RawMessage) wipEvent {
	return wipEvent{target: target, method: method, params: params, provisional: target != "" && target != pos.ActiveTarget, conn: pos.Conn, seq: pos.Seq, at: monotonic()}
}

// eventQueue passes the classified events from the read loop to the
// attachment's goroutine.
type eventQueue struct {
	mu      sync.Mutex
	items   []wipEvent
	bytes   int
	dropped uint64        // events dropped since the goroutine last took the queue
	wire    uint64        // every event pushed, queued or dropped (pageState.caughtUp)
	wake    chan struct{} // capacity 1
	closed  bool          // the attachment closed: nothing is queued any more
}

// staging is the event part of pageState; ps.mu guards it (the queue has
// its own lock).
type staging struct {
	q             eventQueue
	conn          webkit.Caller         // the connection whose events the attachment translates
	connSeq       uint64                // grows at each reset, so at each change of conn too (readTree)
	live          map[string]bool       // targets announced as no provisional ones
	liveOrder     []string              // the targets of live, oldest first
	staged        map[string][]wipEvent // events of provisional targets, until their commit
	stagedOrder   []string              // staged targets, oldest first
	stagedEvents  int                   // all staged events
	stagedBytes   int                   // their payload
	overflowed    map[string]bool       // staged targets that exceeded the bounds; their slot stays, empty
	lost          [lostMemory]string    // targets whose staged events are gone
	lostNext      int                   // next slot of lost
	resetOnCommit bool                  // the loss record is not exact: the next commit publishes a reset
	reseed        bool                  // the next seed announces a new document (resetLocked)
	applied       uint64                // pushed events that the goroutine handled
	appliedSig    chan struct{}         // closed and replaced when applied grows
	modelProv     map[string]bool       // provisional targets of a model (dispatch, in support_test.go)
	testApply     func(wipEvent)        // test seam: runs before each queued event is applied, without ps.mu; nil in production
}

func (st *staging) initStaging() {
	st.live = map[string]bool{}
	st.staged = map[string][]wipEvent{}
	st.overflowed = map[string]bool{}
	st.modelProv = map[string]bool{}
	st.appliedSig = make(chan struct{})
	st.q.wake = make(chan struct{}, 1)
}

// onWire runs in the connection's read loop for every event: it
// classifies the event at its position and queues it. It does not block.
func (ps *pageState) onWire(pos core.EventPosition, target, method string, params json.RawMessage) {
	if len(pageEventFns[method]) == 0 && method != evTargetCreated && method != evTargetCommitted && method != evTargetDestroyed {
		return
	}
	ev := eventAt(pos, target, method, params)
	// The page's Fetch arbitration (fetch_domain.go fetchClaims) learns a
	// request's type here, so that every attachment knows it, and decides
	// an interception event once for every attachment.
	switch {
	case method == "Network.requestWillBeSent":
		ps.s.c.s.claims.noteType(ps.s.pg, target, params)
	case interceptionEvent(method):
		ev.claim = ps.s.c.s.claims.decide(ps.s.pg, pos, target, method, params)
		stage := stageRequest
		if method == evResponseIntercepted {
			stage = stageResponse
		}
		ev.hold = ps.eventHold(netKey{target, requestIDOf(params)}, stage)
	}
	if _, queued := ps.q.push(ev); !queued && interceptionEvent(method) {
		// A request paused for nobody that will translate it (the queue
		// is full, or the attachment closed): fetch_domain.go.
		go ps.continueUnreported(ev)
	}
}

// push queues ev, or drops it when the queue is full or closed, and
// returns its place in the stream: the number of events pushed up to it
// (caughtUpTo), and whether it was queued.
func (q *eventQueue) push(ev wipEvent) (uint64, bool) {
	q.mu.Lock()
	q.wire++
	n := q.wire
	queued := false
	switch {
	case q.closed:
	case q.dropped > 0 || len(q.items) >= maxQueuedEvents || q.bytes+ev.size() > maxQueuedBytes:
		q.dropped++
	default:
		q.items = append(q.items, ev)
		q.bytes += ev.size()
		queued = true
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return n, queued
}

// close stops queueing and returns the events that were queued and not
// taken.
func (q *eventQueue) close() []wipEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	items := q.items
	q.items, q.bytes = nil, 0
	return items
}

// take returns the queued events and the number of events dropped after
// them, and empties the queue.
func (q *eventQueue) take() ([]wipEvent, uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	items, dropped := q.items, q.dropped
	q.items, q.bytes, q.dropped = nil, 0, 0
	return items, dropped
}

// pushed returns the number of events pushed so far.
func (q *eventQueue) pushed() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.wire
}

// run translates the queued events in their order until the attachment
// closes.
func (ps *pageState) run() {
	for {
		select {
		case <-ps.q.wake:
		case <-ps.life.Done():
			return
		}
		items, dropped := ps.q.take()
		for _, ev := range items {
			ps.mu.Lock()
			if h := ps.testApply; h != nil {
				ps.mu.Unlock()
				h(ev)
				ps.mu.Lock()
			}
			ps.applyLocked(ev)
			ps.appliedLocked(1)
			ps.mu.Unlock()
		}
		if dropped > 0 {
			ps.mu.Lock()
			ps.eventsLostLocked()
			ps.appliedLocked(dropped)
			ps.mu.Unlock()
		}
	}
}

func (ps *pageState) appliedLocked(n uint64) {
	ps.applied += n
	if at := ps.net.gapAt; at != 0 && ps.applied >= at {
		ps.net.gapAt = 0
		ps.netGapLocked() // a Network reset reported before the next events (netGap)
	}
	close(ps.appliedSig)
	ps.appliedSig = make(chan struct{})
}

// caughtUp waits until the attachment has handled every event that its
// connections delivered before the call: its document, frames and
// contexts are then at least as new as anything a connection reported
// before (wipContext).
func (ps *pageState) caughtUp(ctx context.Context) error {
	return ps.caughtUpTo(ctx, ps.q.pushed())
}

// caughtUpTo waits until the attachment has handled the first want
// events that its connections delivered (eventQueue.push).
func (ps *pageState) caughtUpTo(ctx context.Context, want uint64) error {
	for {
		ps.mu.Lock()
		done, sig := ps.applied >= want || ps.closed, ps.appliedSig
		ps.mu.Unlock()
		if done {
			return nil
		}
		select {
		case <-sig:
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ps.life.Done():
			return nil // closed: the caller finds no context
		}
	}
}

// targetParams are the params of the WIP Target events.
type targetParams struct {
	TargetInfo struct {
		TargetID      string `json:"targetId"`
		IsProvisional bool   `json:"isProvisional"`
	} `json:"targetInfo"`
	OldTargetID string `json:"oldTargetId"`
	NewTargetID string `json:"newTargetId"`
	TargetID    string `json:"targetId"`
}

// applyLocked takes one classified event: the first event of another
// connection resets the document; a commit publishes the target's staged
// events, a destruction drops them, a provisional target's event is
// staged, and any other event goes to its translators.
func (ps *pageState) applyLocked(ev wipEvent) {
	if ps.closed {
		if ps.fetch.used && interceptionEvent(ev.method) && ps.s != nil {
			go ps.continueUnreported(ev)
		}
		return
	}
	if ev.conn != nil && ev.conn != ps.conn {
		ps.conn = ev.conn
		ps.forgetStagedLocked()
		ps.logf("cdp: page %s: events of a new connection; the document is reset", logsafe.ID(ps.target))
		ps.netResetLocked(true)
		ps.resetLocked()
	}
	if ev.tree != nil {
		ps.treeLocked(ev)
		return
	}
	switch ev.method {
	case evTargetCreated, evTargetCommitted, evTargetDestroyed:
		var p targetParams
		if json.Unmarshal(ev.params, &p) != nil {
			return
		}
		switch ev.method {
		case evTargetCreated:
			if id := p.TargetInfo.TargetID; id != "" && !p.TargetInfo.IsProvisional {
				ps.addLiveLocked(id)
			}
		case evTargetCommitted:
			ps.commitLocked(p.OldTargetID, p.NewTargetID)
		case evTargetDestroyed:
			ps.unstageLocked(p.TargetID)
			ps.dropLiveLocked(p.TargetID)
			ps.netTargetGoneLocked(p.TargetID)
		}
		return
	}
	if ev.provisional && !ps.live[ev.target] && !ps.publishEarlyLocked(ev) {
		ps.stageLocked(ev)
		return
	}
	ps.translateLocked(ev)
}

// publishEarlyLocked reports whether an event of a provisional target is
// translated at once instead of at the commit (the staging rule of the
// document request, fetch_domain.go): an interception event (WebKit
// commits only after the paused request is answered), and the
// requestWillBeSent of a request that interception paused before it
// (a client handles requestPaused only after it). The request's other
// events stay staged and are published at the commit, or the request
// ends as aborted with an abandoned target (netTargetGoneLocked).
func (ps *pageState) publishEarlyLocked(ev wipEvent) bool {
	if interceptionEvent(ev.method) {
		return true
	}
	if ev.method != "Network.requestWillBeSent" {
		return false
	}
	r := ps.net.live[netKey{ev.target, requestIDOf(ev.params)}]
	return r != nil && !r.started
}

// publishStagedRequestLocked translates the staged requestWillBeSent of
// the request k of a provisional target now, and removes it from the
// staged events.
func (ps *pageState) publishStagedRequestLocked(k netKey) {
	evs := ps.staged[k.target]
	for i, ev := range evs {
		if ev.method == "Network.requestWillBeSent" && requestIDOf(ev.params) == k.wip {
			ps.staged[k.target] = append(evs[:i:i], evs[i+1:]...)
			ps.stagedEvents--
			ps.stagedBytes -= len(ev.params)
			ps.translateLocked(ev)
			return
		}
	}
}

// requestIDOf is the requestId of a WIP Network event, "" without one.
func requestIDOf(params json.RawMessage) string {
	var v struct {
		RequestID string `json:"requestId"`
	}
	_ = json.Unmarshal(params, &v)
	return v.RequestID
}

// treeLocked applies the reply of a tree read at its place in the stream
// ("Tree reads"): the frame tree of the reply's target, unless that target
// was replaced at the reply's position or a reset came after the read
// started.
func (ps *pageState) treeLocked(ev wipEvent) {
	var r struct {
		FrameTree wipTree `json:"frameTree"`
	}
	if ev.provisional || ev.tree.seq != ps.connSeq || json.Unmarshal(ev.params, &r) != nil {
		return
	}
	ps.seedLocked(ev.target, r.FrameTree)
}

func (ps *pageState) translateLocked(ev wipEvent) {
	ps.net.at, ps.fetch.claim, ps.fetch.hold = ev.at, ev.claim, ev.hold
	for _, fn := range pageEventFns[ev.method] {
		fn(ps, ev.target, ev.params)
	}
	ps.fetch.claim, ps.fetch.hold = nil, 0
}

// commitLocked publishes the staged events of the committed target in
// their order, or a reset when they are incomplete or do not give the
// target's document (F1).
func (ps *pageState) commitLocked(old, target string) {
	if target == "" {
		return
	}
	evs := ps.staged[target]
	lost := ps.resetOnCommit || ps.overflowed[target] || ps.wasLostLocked(target)
	ps.unstageLocked(target)
	ps.unstageLocked(old)
	ps.dropLiveLocked(old)
	ps.netTargetGoneLocked(old)
	ps.resetOnCommit = false
	if lost {
		ps.logf("cdp: page %s: the events of the committed target %s are incomplete; the document is reset", logsafe.ID(ps.target), logsafe.ID(target))
		ps.resetLocked()
		return
	}
	for _, ev := range evs {
		ev.target = target
		ps.translateLocked(ev)
	}
	if ps.main == nil || ps.main.target != target {
		// The loader ids stay: the committed target's documents have
		// their own (target, loader) keys (2.7), and its staged events
		// may have given this document its id already (the document
		// request, events.go).
		ps.logf("cdp: page %s: the committed target %s reported no document to this attachment; the document is reset", logsafe.ID(ps.target), logsafe.ID(target))
		ps.clearDocumentLocked()
	}
}

// addLiveLocked remembers target as no provisional one; beyond
// maxLiveTargets the oldest is forgotten (its destruction was lost, or
// it is a long-lived worker whose later events are then staged).
func (ps *pageState) addLiveLocked(target string) {
	if ps.live[target] {
		return
	}
	if len(ps.liveOrder) >= maxLiveTargets {
		ps.dropLiveLocked(ps.liveOrder[0])
	}
	ps.live[target] = true
	ps.liveOrder = append(ps.liveOrder, target)
}

func (ps *pageState) dropLiveLocked(target string) {
	if !ps.live[target] {
		return
	}
	delete(ps.live, target)
	for i, t := range ps.liveOrder {
		if t == target {
			ps.liveOrder = append(ps.liveOrder[:i], ps.liveOrder[i+1:]...)
			break
		}
	}
}

// stageLocked keeps a provisional target's event within the bounds.
func (ps *pageState) stageLocked(ev wipEvent) {
	t := ev.target
	if ps.overflowed[t] {
		return
	}
	if _, ok := ps.staged[t]; !ok {
		if len(ps.stagedOrder) >= maxStagedTargets {
			oldest := ps.stagedOrder[0]
			ps.unstageLocked(oldest)
			ps.rememberLostLocked(oldest)
		}
		ps.staged[t] = nil
		ps.stagedOrder = append(ps.stagedOrder, t)
	}
	n := len(ev.params)
	if ps.stagedEvents+1 > maxStagedEvents || ps.stagedBytes+n > maxStagedBytes {
		ps.dropStagedEventsLocked(t)
		ps.overflowed[t] = true
		ps.logf("cdp: page %s: provisional target %s exceeded the staging bounds (%d events, %d bytes); its commit resets the document", logsafe.ID(ps.target), logsafe.ID(t), maxStagedEvents, maxStagedBytes)
		return
	}
	ps.staged[t] = append(ps.staged[t], ev)
	ps.stagedEvents++
	ps.stagedBytes += n
}

// dropStagedEventsLocked forgets the staged events of target; the target
// keeps its staging slot.
func (ps *pageState) dropStagedEventsLocked(target string) {
	evs := ps.staged[target]
	ps.stagedEvents -= len(evs)
	for _, ev := range evs {
		ps.stagedBytes -= len(ev.params)
	}
	ps.staged[target] = nil
}

// unstageLocked forgets target's staged events, its slot and its overflow
// mark.
func (ps *pageState) unstageLocked(target string) {
	if _, ok := ps.staged[target]; !ok {
		return
	}
	ps.dropStagedEventsLocked(target)
	delete(ps.staged, target)
	delete(ps.overflowed, target)
	for i, t := range ps.stagedOrder {
		if t == target {
			ps.stagedOrder = append(ps.stagedOrder[:i], ps.stagedOrder[i+1:]...)
			break
		}
	}
}

// rememberLostLocked records target as lost. When that overwrites an
// older lost target, whose commit could still come, the next commit
// resets.
func (ps *pageState) rememberLostLocked(target string) {
	if ps.lost[ps.lostNext] != "" {
		ps.resetOnCommit = true
	}
	ps.lost[ps.lostNext] = target
	ps.lostNext = (ps.lostNext + 1) % lostMemory
}

func (ps *pageState) wasLostLocked(target string) bool {
	for _, t := range ps.lost {
		if t != "" && t == target {
			return true
		}
	}
	return false
}

// forgetStagedLocked drops every staged target as lost: its commit resets.
func (ps *pageState) forgetStagedLocked() {
	for len(ps.stagedOrder) > 0 {
		t := ps.stagedOrder[0]
		ps.unstageLocked(t)
		ps.rememberLostLocked(t)
	}
}

// eventsLostLocked follows a queue overflow: the staged targets are lost,
// the next commit resets again, and the document is reset now.
func (ps *pageState) eventsLostLocked() {
	ps.forgetStagedLocked()
	ps.resetOnCommit = true
	ps.logf("cdp: page %s: events were dropped (more than %d events or %d bytes queued); the document is reset", logsafe.ID(ps.target), maxQueuedEvents, maxQueuedBytes)
	ps.resetLocked()
}

// resetLocked publishes a reset instead of an incomplete document:
// Runtime.executionContextsCleared now, and a fresh frame tree from the
// page (refreshAfterReset), announced as a new document (seedLocked).
// The remembered loader ids are dropped, so the announced document and
// its frames get new bridge loader ids even when the page still shows the
// same WIP document (F7). Only the loader id of a main-frame document
// request that stays in flight across the reset (held paused,
// netResetLocked) is kept, unless its document was announced already:
// the document announced for that request has its loader id, so
// requestId == loaderId holds (5.2; W5c-1a repair round 1, F3).
func (ps *pageState) resetLocked() {
	ps.netResetLocked(false)
	ps.loaders = ps.pausedDocLoadersLocked()
	clear(ps.lastConsole) // events may be missing since: a repeat would name another message
	ps.clearDocumentLocked()
}

// pausedDocLoadersLocked returns the remembered loader ids that a reset
// keeps (resetLocked): those of the main-frame document requests that the
// attachment holds paused, except the announced document's.
func (ps *pageState) pausedDocLoadersLocked() []loaderEntry {
	var keep []loaderEntry
	for k := range ps.fetchHeldKeysLocked() {
		r := ps.net.live[k]
		if r == nil || !r.doc || (ps.main != nil && ps.main.loader == r.loader) {
			continue
		}
		for _, e := range ps.loaders {
			if e.id == r.loader {
				keep = append(keep, e)
			}
		}
	}
	return keep
}

// clearDocumentLocked is resetLocked without forgetting the loader ids. A
// tree read that started before it is not seeded (connSeq, readTree).
func (ps *pageState) clearDocumentLocked() {
	ps.gen++
	ps.connSeq++
	ps.main = nil
	ps.frames = map[string]*frameInfo{}
	ps.clearContextsLocked(true)
	ps.reseed = true
	if ps.s != nil {
		go ps.refreshAfterReset()
	}
}

// refreshAfterReset reads the page's frame tree after a reset; its seed
// announces the document (seedLocked).
func (ps *pageState) refreshAfterReset() {
	ctx, cancel := context.WithTimeout(ps.life, resetReadBudget)
	defer cancel()
	if _, err := ps.refresh(ctx); err != nil && ps.life.Err() == nil {
		ps.logf("cdp: page %s: reading the frame tree after a reset: %v", logsafe.ID(ps.target), logsafe.Error(err))
	}
}

// logf logs through the daemon; a model does not log.
func (ps *pageState) logf(format string, args ...any) {
	if ps.s != nil {
		ps.s.c.s.o.Logf(format, args...)
	}
}
