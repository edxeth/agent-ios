package cdp

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"testing"
	"testing/quick"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
)

// worldStep is one step of a random page history for the context-id
// property. Op selects the step; A and B pick a frame, a world name or an
// older document.
type worldStep struct {
	Op, A, B uint8
}

// Steps of the context-id property.
const (
	wsNavigate      = iota // the main frame loads a new document (new WIP loader)
	wsSwap                 // a provisional target reports a document with the same WIP loader string and commits (2.7)
	wsRestore              // the main frame returns to a remembered document (back/forward cache)
	wsIsolatedWorld        // Page.createIsolatedWorld{frameId, worldName}
	wsNewDocWorld          // Page.addScriptToEvaluateOnNewDocument{worldName}
	wsChildAttach          // a child frame attaches and loads
	wsChildNavigate        // a child frame loads a new document
	wsChildDetach          // a child frame is removed
	wsRuntimeEnable        // Runtime.enable announces the current contexts again
	wsWIPReplay            // WebKit reports a main world again (its own Runtime.enable replay)
	wsAbort                // a provisional target reports a document and is destroyed without a commit
	wsFlush                // ordered runs: the lagging attachment handles the events delivered so far
	wsLateSwap             // a provisional target whose document the attachment never saw commits: a reset (F1)
	wsEventsLost           // events were dropped (the queue overflowed), among them the child frames' removal: a reset (F5)
	wsStall                // a provisional target reports a document and neither commits nor ends: it stays staged until pushed out (F5)
	wsRead                 // a Page handler reads the frame tree (readTree); in ordered runs its reply comes some steps later
	wsHandover             // ordered runs: a reconnect installs a new connection (core pageConn.swap), during the tree reads in flight
	worldSteps
)

const maxWorldSteps = 40

var worldNames = []string{"util", "__puppeteer_utility_world__", "__playwright_utility_world__"}

// worldRun drives a pageState model (no page, no subscription) through
// real WIP event translators (pageState.dispatch) and the world calls of
// the Page handlers, and records the CDP events it sends.
type worldRun struct {
	ps      *pageState
	events  []sentEvent
	target  string   // the WIP target that reports the main frame
	targets int      // targets created so far
	loader  int      // last WIP loader number
	loaders []string // WIP main-frame loaders seen, oldest first
	wipCtx  int      // last WIP context id
	kids    []string // child frames of the current document
	main    string   // WIP loader of the current main-frame document
	err     error    // a violation seen during a step

	// Ordered runs (TestContextIDPropertyOrdered) take the read-loop path
	// of staging.go: each event is classified at its position (the
	// connection's active target then) and queued; the attachment
	// handles the queue only at wsFlush and before a step that reads its
	// state, so it lags across commits. A provisional target may have
	// been created before the attachment, so that no targetCreated
	// announces it. The tree read of a reset or of wsRead is answered
	// after 0 to 3 more steps: its reply is queued at that place with the
	// page's frame tree there (staging.go, "Tree reads"), also after
	// navigations and back/forward restores that repeat a document's
	// target and loader.
	ordered bool
	conn    *otherConn // the connection
	seq     uint64     // messages the connection read
	active  string     // the connection's active target
	pending []wipEvent // delivered, not yet handled
	reads   []runRead  // tree reads whose reply has not come yet
	resets  uint64     // ps.connSeq when the run last started a reset's read
	delay   int        // steps until the reply of a read started in this step
	// kidsOf holds the child frames of each document that the main frame
	// left: a back/forward restore brings them back without any child
	// frame event (WebKitGTK, review r4 bfcache-wire.log), so only a tree
	// read reports them.
	kidsOf map[runDoc][]string

	// The world names each frame of the current document must have, by
	// the rules of worlds.go: createIsolatedWorld names stay with their
	// frame (the main frame across documents, a child frame until the
	// main frame's next document, also one that a reset announces),
	// addScriptToEvaluateOnNewDocument names reach every frame from its
	// next document on.
	expected  map[string]map[string]bool // by CDP frame id
	sticky    map[string][]string        // createIsolatedWorld names by CDP frame id
	docWorlds []string
}

type sentEvent struct {
	method string
	params json.RawMessage
}

// runDoc is the page's main-frame document: its target and WIP loader.
type runDoc struct{ target, loader string }

// runRead is a tree read of an ordered run whose reply has not come yet.
type runRead struct {
	rd   *treeRead
	left int // steps until its reply
}

func newWorldRun(t *testing.T) *worldRun {
	w := &worldRun{target: "wk-0", expected: map[string]map[string]bool{}, sticky: map[string][]string{}, conn: &otherConn{n: 1}, kidsOf: map[runDoc][]string{}}
	w.ps = newPageModel("PAGE", func(method string, params any) {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("encoding %s: %v", method, err)
		}
		w.events = append(w.events, sentEvent{method, raw})
	})
	w.ps.setEnabled(domPage, true)
	w.ps.setEnabled(domRuntime, true)
	w.ps.conn, w.active = w.conn, w.target
	w.mainDocument(w.nextLoader())
	return w
}

func (w *worldRun) emit(method string, params any) {
	raw, _ := json.Marshal(params) // test maps
	w.deliver(w.target, method, raw)
}

// emitOuter dispatches a Target event of the connection.
func (w *worldRun) emitOuter(method string, params any) {
	raw, _ := json.Marshal(params) // test maps
	w.deliver("", method, raw)
}

// deliver hands one event of target to the attachment: at once, or in an
// ordered run classified at its position and queued.
func (w *worldRun) deliver(target, method string, raw json.RawMessage) {
	if method == evTargetCommitted {
		var p targetParams
		_ = json.Unmarshal(raw, &p) // test maps
		w.active = p.NewTargetID    // the connection applies the commit before its subscribers run
	}
	if !w.ordered {
		w.ps.dispatch(target, method, raw)
		w.ps.mu.Lock()
		w.rereadLocked()
		w.ps.mu.Unlock()
		return
	}
	w.pending = append(w.pending, eventAt(w.position(), target, method, raw))
}

// position is the place of the next message that the connection reads.
func (w *worldRun) position() core.EventPosition {
	w.seq++
	return core.EventPosition{Conn: w.conn, ActiveTarget: w.active, Seq: w.seq}
}

// flush lets an ordered run's attachment handle the queued events, in
// their order. An event of a target that was provisional at its position
// sends nothing when it is handled (the first item of a new connection
// resets the document first, which clears the contexts). Each reset
// starts its tree read (refreshAfterReset), whose reply comes some steps
// later (tick).
func (w *worldRun) flush() {
	w.ps.mu.Lock()
	defer w.ps.mu.Unlock()
	for _, ev := range w.pending {
		before := len(w.events)
		if ev.conn != w.ps.conn {
			before++ // Runtime.executionContextsCleared of the reset (staging.go applyLocked)
		}
		w.applyLocked(ev)
		if ev.provisional && len(w.events) != before && w.err == nil {
			w.err = fmt.Errorf("event %s of %s, provisional at its position, sent %v", ev.method, ev.target, w.events[min(before, len(w.events)):])
		}
		w.resetReadLocked()
	}
	w.pending = nil
}

// resetReadLocked starts the tree read of an ordered run's reset since
// the last call (connSeq grows at each reset). A reset announces the
// page's document as a new main-frame document (staging.go resetLocked),
// so the createIsolatedWorld names of the child frames end with the old
// one, as at a navigation; the main frame keeps its own.
func (w *worldRun) resetReadLocked() {
	if w.ps.connSeq != w.resets {
		w.resets = w.ps.connSeq
		maps.DeleteFunc(w.sticky, func(id string, _ []string) bool { return id != "PAGE" })
		w.reads = append(w.reads, runRead{&treeRead{ps: w.ps, seq: w.ps.connSeq}, w.delay})
	}
}

// applyLocked hands ev to the attachment. A model has no context
// inventory (core Scheduler.FrameContext) that would give the child frames
// that a tree adds their contexts, so the run expects none for them.
func (w *worldRun) applyLocked(ev wipEvent) {
	known := w.ps.frames
	w.ps.applyLocked(ev)
	if ev.tree == nil {
		return
	}
	for id := range w.ps.frames {
		if known[id] == nil {
			delete(w.expected, id)
		}
	}
}

// tick ends a step of an ordered run: the reads whose time has come get
// their reply, after the events of the step.
func (w *worldRun) tick() {
	var left []runRead
	for _, r := range w.reads {
		if r.left--; r.left >= 0 {
			left = append(left, r)
			continue
		}
		w.reply(r.rd)
	}
	w.reads = left
}

// reply gives rd its reply at the connection's next position: the page's
// frame tree now, from its active target. An unordered run's attachment
// applies it at once.
func (w *worldRun) reply(rd *treeRead) {
	tree := wipTree{Frame: wipFrame{ID: "main", LoaderID: w.main, URL: "https://w.example/" + w.main, SecurityOrigin: "https://w.example"}}
	for _, k := range w.kids {
		tree.ChildFrames = append(tree.ChildFrames, wipTree{Frame: wipFrame{ID: k, ParentID: "main", LoaderID: "kid." + k, URL: "https://c.example/" + k, SecurityOrigin: "https://c.example"}})
	}
	raw, _ := json.Marshal(map[string]any{"frameTree": tree}) // test structs
	ev := eventAt(w.position(), w.target, evTreeReply, raw)
	ev.tree = rd
	if !w.ordered {
		w.ps.mu.Lock()
		w.applyLocked(ev)
		w.ps.mu.Unlock()
		return
	}
	w.pending = append(w.pending, ev)
}

// read starts a tree read of a Page handler: an ordered run answers it
// some steps later, an unordered one at once.
func (w *worldRun) read() {
	w.ps.mu.Lock()
	rd := &treeRead{ps: w.ps, seq: w.ps.connSeq}
	w.ps.mu.Unlock()
	if !w.ordered {
		w.reply(rd)
		return
	}
	w.reads = append(w.reads, runRead{rd, w.delay})
}

// finish ends a run: a last tree read reports the child frames that
// back/forward restores brought back without events, the reads still
// running get their reply, and the attachment handles everything, also
// the read of a reset that the last items start (a new connection's
// first item).
func (w *worldRun) finish() {
	w.delay = 0
	w.read()
	for w.ordered {
		w.flush()
		if len(w.reads) == 0 {
			break
		}
		for _, r := range w.reads {
			w.reply(r.rd)
		}
		w.reads = nil
	}
}

// handover installs a new connection in an ordered run, as a reconnect
// does (core pageConn.prepare and swap): the reads in flight on the old
// connection fail with it and get no reply, and the new connection reads
// from position 1. Core delivers the new connection's events and the
// replies that it reads in one order, also those it held for the swap
// (pageConn.replyRead), so the run queues them in read order. Its first
// item resets the attachment's document (staging.go), whose read then
// reports the page's frames again, while the reads started after the
// swap are answered later, also before the attachment reached that
// first item.
func (w *worldRun) handover() {
	if !w.ordered {
		return
	}
	w.reads = nil
	w.conn = &otherConn{n: w.conn.n + 1}
	w.seq = 0
}

// rereadLocked stands for the frame tree read after a reset in an
// unordered run (refreshAfterReset), which a model does not run: when the
// attachment has no document, it seeds the page's document, without child
// frames: a reset follows a commit of a new document (none attached yet)
// or dropped events (wsEventsLost, which removed them).
func (w *worldRun) rereadLocked() {
	if w.ps.main != nil {
		return
	}
	w.ps.seedLocked(w.target, wipTree{Frame: wipFrame{ID: "main", LoaderID: w.main, URL: "https://w.example/" + w.main, SecurityOrigin: "https://w.example"}})
}

// forgetDocument sets the run's expectation to the main-frame document l
// without child frames.
func (w *worldRun) forgetDocument(l string) {
	for _, k := range w.kids {
		delete(w.sticky, k)
	}
	w.kids, w.main = nil, l
	w.expected = map[string]map[string]bool{"PAGE": w.namesFor("PAGE")}
}

// provisional announces the provisional target id; in an ordered run with
// missed, the target was created before the attachment and is not
// announced.
func (w *worldRun) provisional(id string, missed bool) {
	if w.ordered && missed {
		return
	}
	w.emitOuter("Target.targetCreated", map[string]any{"targetInfo": map[string]any{"targetId": id, "type": "page", "isProvisional": true}})
}

func (w *worldRun) nextLoader() string {
	w.loader++
	l := fmt.Sprintf("0.%d", w.loader)
	w.loaders = append(w.loaders, l)
	return l
}

// mainDocument reports a main-frame document with WIP loader l and its
// main world, as WebKit does on a navigation. The document left keeps its
// child frames for a back/forward restore (kidsOf).
func (w *worldRun) mainDocument(l string) {
	w.kidsOf[runDoc{w.target, w.main}] = slices.Clone(w.kids)
	w.forgetDocument(l)
	w.emit("Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main", "loaderId": l, "url": "https://w.example/" + l, "securityOrigin": "https://w.example"}})
	w.mainWorld("main")
}

func (w *worldRun) mainWorld(frame string) {
	w.wipCtx++
	w.emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": w.wipCtx, "type": "normal", "frameId": frame}})
}

// namesFor lists the world names of a new document of the frame id.
func (w *worldRun) namesFor(id string) map[string]bool {
	m := map[string]bool{}
	for _, n := range append(append([]string(nil), w.docWorlds...), w.sticky[id]...) {
		m[n] = true
	}
	return m
}

func (w *worldRun) childDocument(id string) {
	w.expected[id] = w.namesFor(id)
	w.emit("Page.frameNavigated", map[string]any{"frame": map[string]any{"id": id, "parentId": "main", "loaderId": w.nextLoader(), "url": "https://c.example/" + id, "securityOrigin": "https://c.example"}})
	w.mainWorld(id)
}

func (w *worldRun) step(s worldStep) {
	name := worldNames[int(s.B)%len(worldNames)]
	op := s.Op % worldSteps
	w.delay = int(s.A % 4)
	if w.ordered {
		defer w.tick()
	}
	switch op {
	case wsIsolatedWorld, wsNewDocWorld, wsRuntimeEnable, wsFlush:
		w.flush() // these read the attachment's state, as its command handlers do
	}
	switch op {
	case wsNavigate:
		w.mainDocument(w.nextLoader())
	case wsSwap:
		w.targets++
		old := w.target
		w.target = fmt.Sprintf("wk-%d", w.targets)
		w.provisional(w.target, s.A%2 == 1)
		before := len(w.events)
		w.mainDocument(w.loaders[len(w.loaders)-1])
		if len(w.events) != before && w.err == nil {
			w.err = fmt.Errorf("provisional target %s sent %d events before its commit", w.target, len(w.events)-before)
		}
		w.emitOuter("Target.didCommitProvisionalTarget", map[string]any{"oldTargetId": old, "newTargetId": w.target})
		w.emitOuter("Target.targetDestroyed", map[string]any{"targetId": old})
	case wsAbort:
		w.targets++
		id := fmt.Sprintf("wk-%d", w.targets)
		w.provisional(id, s.A%2 == 1)
		before := len(w.events)
		for _, e := range []struct {
			method string
			params map[string]any
		}{
			{"Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main", "loaderId": "abandoned", "url": "https://gone.example/", "securityOrigin": "https://gone.example"}}},
			{"Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 1, "type": "normal", "frameId": "main"}}},
			{"Page.frameAttached", map[string]any{"frameId": "c0", "parentFrameId": "main"}},
		} {
			raw, _ := json.Marshal(e.params) // test maps
			w.deliver(id, e.method, raw)
		}
		w.emitOuter("Target.targetDestroyed", map[string]any{"targetId": id})
		if len(w.events) != before && w.err == nil {
			w.err = fmt.Errorf("abandoned provisional target %s sent %d events", id, len(w.events)-before)
		}
	case wsLateSwap:
		w.targets++
		old := w.target
		w.target = fmt.Sprintf("wk-%d", w.targets)
		w.provisional(w.target, s.A%2 == 1)
		w.forgetDocument(w.nextLoader()) // reported before the attachment existed
		w.emitOuter("Target.didCommitProvisionalTarget", map[string]any{"oldTargetId": old, "newTargetId": w.target})
		w.emitOuter("Target.targetDestroyed", map[string]any{"targetId": old})
	case wsEventsLost:
		w.flush()
		w.forgetDocument(w.main)
		w.ps.mu.Lock()
		w.ps.eventsLostLocked()
		if w.ordered {
			w.resetReadLocked()
		} else {
			w.rereadLocked()
		}
		w.ps.mu.Unlock()
	case wsStall:
		w.targets++
		id := fmt.Sprintf("wk-%d", w.targets)
		w.provisional(id, s.A%2 == 1)
		before := len(w.events)
		raw, _ := json.Marshal(map[string]any{"frame": map[string]any{"id": "main", "loaderId": "stalled", "url": "https://stalled.example/", "securityOrigin": "https://stalled.example"}}) // test maps
		w.deliver(id, "Page.frameNavigated", raw)
		if len(w.events) != before && w.err == nil {
			w.err = fmt.Errorf("stalled provisional target %s sent %d events", id, len(w.events)-before)
		}
	case wsRestore:
		if l := w.loaders[int(s.A)%len(w.loaders)]; l != w.main {
			w.mainDocument(l)
			// The restored document's child frames come back without
			// events, so no contexts are expected for them.
			w.kids = slices.Clone(w.kidsOf[runDoc{w.target, l}])
		}
	case wsRead:
		w.read()
	case wsHandover:
		w.handover()
	case wsIsolatedWorld:
		w.ps.mu.Lock()
		frames := w.ps.framesLocked()
		if len(frames) == 0 {
			w.ps.mu.Unlock()
			return // no document until a reset's read: the handler fails with "retry the command"
		}
		f := frames[int(s.A)%len(frames)]
		w.ps.registerWorldLocked(f, name)
		id := w.ps.frameID(f)
		w.ps.mu.Unlock()
		w.sticky[id] = append(w.sticky[id], name)
		if w.expected[id] != nil { // a frame that a tree added has no expected contexts (applyLocked)
			w.expected[id][name] = true
		}
	case wsNewDocWorld:
		w.ps.mu.Lock()
		w.ps.addWorldLocked(name)
		w.ps.mu.Unlock()
		w.docWorlds = append(w.docWorlds, name)
	case wsChildAttach:
		id := fmt.Sprintf("c%d", s.A%3)
		if slices.Contains(w.kids, id) {
			return
		}
		w.emit("Page.frameAttached", map[string]any{"frameId": id, "parentFrameId": "main"})
		w.childDocument(id)
		w.kids = append(w.kids, id)
	case wsChildNavigate:
		if len(w.kids) > 0 {
			w.childDocument(w.kids[int(s.A)%len(w.kids)])
		}
	case wsChildDetach:
		if len(w.kids) > 0 {
			i := int(s.A) % len(w.kids)
			id := w.kids[i]
			w.emit("Page.frameDetached", map[string]any{"frameId": id})
			w.kids = slices.Delete(w.kids, i, i+1)
			delete(w.expected, id)
			delete(w.sticky, id)
		}
	case wsRuntimeEnable:
		w.ps.mu.Lock()
		w.ps.announceAllLocked()
		w.ps.mu.Unlock()
	case wsWIPReplay:
		w.emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": w.wipCtx, "type": "normal", "frameId": "main"}})
	}
}

// ctxKey is what a context id names: a world of a frame.
type ctxKey struct{ frame, name string }

// checkContexts replays the CDP events of a run and reports the first
// violation of the context-id rule (design 5.2, acceptance 7.8 item 4):
//   - a context id is never emitted under two names (or two frames);
//   - an id that executionContextsCleared or executionContextDestroyed
//     ended is never emitted again, and a frame's contexts end before
//     the frameNavigated of its next document, so each document of a
//     frame gets new ids;
//   - among the live contexts, each world of a frame has one id, and
//     isDefault and type match the name ("" is the default world);
//   - finally, every frame of the current document has a live default
//     context and an alias for each world name it must have
//     (worldRun.expected), and the attachment describes the page's
//     current main-frame document, of its current target (F1: a commit
//     never leaves the replaced target's document).
func checkContexts(w *worldRun) error {
	if w.err != nil {
		return w.err
	}
	w.ps.mu.Lock()
	m := w.ps.main
	w.ps.mu.Unlock()
	if m == nil || m.target != w.target || m.wip.LoaderID != w.main {
		return fmt.Errorf("the attachment describes document %+v, want loader %s of target %s", m, w.main, w.target)
	}
	w.ps.mu.Lock()
	kids := w.ps.framesLocked()[1:]
	w.ps.mu.Unlock()
	if !slices.Equal(slices.Sorted(slices.Values(kids)), slices.Sorted(slices.Values(w.kids))) {
		return fmt.Errorf("the attachment has the child frames %v, want %v (F9: no frame of a replaced document)", kids, w.kids)
	}
	ever := map[int]ctxKey{}
	live := map[int]ctxKey{}
	dead := map[int]bool{}
	for i, e := range w.events {
		switch e.method {
		case "Runtime.executionContextCreated":
			var p struct{ Context cdpContext }
			if err := json.Unmarshal(e.params, &p); err != nil {
				return fmt.Errorf("event %d: %v", i, err)
			}
			x := p.Context
			k := ctxKey{x.AuxData.FrameID, x.Name}
			if (x.Name == "") != x.AuxData.IsDefault || (x.Name == "") != (x.AuxData.Type == "default") {
				return fmt.Errorf("event %d: context %d %q has isDefault %v and type %q", i, x.ID, x.Name, x.AuxData.IsDefault, x.AuxData.Type)
			}
			if dead[x.ID] {
				return fmt.Errorf("event %d: context id %d emitted again after it ended (%v)", i, x.ID, k)
			}
			if prev, ok := ever[x.ID]; ok && prev != k {
				return fmt.Errorf("event %d: context id %d emitted as %v and as %v", i, x.ID, prev, k)
			}
			for id, lk := range live {
				if lk == k && id != x.ID {
					return fmt.Errorf("event %d: %v has the ids %d and %d in one document", i, k, id, x.ID)
				}
			}
			ever[x.ID], live[x.ID] = k, k
		case "Page.frameNavigated":
			// A new document of a frame starts without contexts: the old
			// document's ended before (cleared for the main frame,
			// destroyed for a child frame).
			var p navFrame
			if err := json.Unmarshal(e.params, &p); err != nil {
				return fmt.Errorf("event %d: %v", i, err)
			}
			for id, k := range live {
				if p.Frame.ParentID == "" || k.frame == p.Frame.ID {
					return fmt.Errorf("event %d: context %d (%v) of the old document is live at frameNavigated of %s", i, id, k, p.Frame.ID)
				}
			}
		case "Runtime.executionContextsCleared":
			for id := range live {
				dead[id] = true
			}
			live = map[int]ctxKey{}
		case "Runtime.executionContextDestroyed":
			var p struct{ ExecutionContextID int }
			if err := json.Unmarshal(e.params, &p); err != nil {
				return fmt.Errorf("event %d: %v", i, err)
			}
			if _, ok := live[p.ExecutionContextID]; !ok {
				return fmt.Errorf("event %d: destroyed context %d is not live", i, p.ExecutionContextID)
			}
			dead[p.ExecutionContextID] = true
			delete(live, p.ExecutionContextID)
		}
	}
	has := map[ctxKey]bool{}
	for _, k := range live {
		has[k] = true
	}
	for id, names := range w.expected {
		if !has[ctxKey{id, ""}] {
			return fmt.Errorf("frame %s has no live default context", id)
		}
		for n := range names {
			if !has[ctxKey{id, n}] {
				return fmt.Errorf("frame %s has no alias %q", id, n)
			}
		}
	}
	if len(live) != len(has) {
		return fmt.Errorf("live contexts %v repeat a world of a frame", live)
	}
	return nil
}

// checkClientFrames replays the Page frame events of a run as a CDP
// client's frame manager does (Puppeteer's and Playwright's: they read the
// frame tree only when they attach, and follow these events afterwards),
// and reports the first event that such a client cannot apply, or a final
// frame tree that differs from the page's (W5b-1 r6: every child frame
// that the attachment learns, also from a tree read, is announced; every
// one it removes is detached):
//   - a main-frame frameNavigated starts a document without child frames;
//   - frameAttached adds a frame that the client does not have, under a
//     parent that it has;
//   - a child frame's frameNavigated is of a frame that the client has;
//   - frameDetached removes a frame that the client has;
//   - at the end, the client has exactly the page's child frames.
func checkClientFrames(w *worldRun) error {
	kids := map[string]bool{}
	for i, e := range w.events {
		var p struct {
			FrameID       string `json:"frameId"`
			ParentFrameID string `json:"parentFrameId"`
			Frame         struct {
				ID       string `json:"id"`
				ParentID string `json:"parentId"`
			} `json:"frame"`
		}
		switch e.method {
		case "Page.frameNavigated", "Page.frameAttached", "Page.frameDetached":
			if err := json.Unmarshal(e.params, &p); err != nil {
				return fmt.Errorf("event %d: %v", i, err)
			}
		default:
			continue
		}
		switch {
		case e.method == "Page.frameNavigated" && p.Frame.ParentID == "":
			kids = map[string]bool{}
		case e.method == "Page.frameNavigated" && !kids[p.Frame.ID]:
			return fmt.Errorf("event %d: frameNavigated of child frame %s, which was never attached", i, p.Frame.ID)
		case e.method == "Page.frameAttached" && (kids[p.FrameID] || p.ParentFrameID != "PAGE" && !kids[p.ParentFrameID]):
			return fmt.Errorf("event %d: frameAttached of %s under %s, a frame the client has or a parent it has not", i, p.FrameID, p.ParentFrameID)
		case e.method == "Page.frameAttached":
			kids[p.FrameID] = true
		case e.method == "Page.frameDetached" && !kids[p.FrameID]:
			return fmt.Errorf("event %d: frameDetached of %s, which the client has not", i, p.FrameID)
		case e.method == "Page.frameDetached":
			delete(kids, p.FrameID)
		}
	}
	if got := slices.Sorted(maps.Keys(kids)); !slices.Equal(got, slices.Sorted(slices.Values(w.kids))) {
		return fmt.Errorf("the client has the child frames %v, want the page's %v", got, w.kids)
	}
	return nil
}

// Design 5.2 and acceptance 7.8 item 4, over random sequences of
// navigations, provisional swaps (with a repeated WIP loader string;
// nothing of the provisional target is sent before its commit), aborted
// and stalled provisional targets (nothing of them is ever sent), resets
// (a commit of a target whose document the attachment missed, dropped
// events, a commit after the staged events were pushed out),
// back/forward restores, child frames, createIsolatedWorld,
// addScriptToEvaluateOnNewDocument{worldName} and context replays: no
// context id is ever emitted under two names, and each alias has its own
// id per frame and document (checkContexts).
func TestContextIDProperty(t *testing.T) {
	prop := func(steps []worldStep) bool {
		if err := runWorldSteps(t, false, steps); err != nil {
			t.Logf("steps %v: %v", steps, err)
			return false
		}
		return true
	}
	if err := quick.Check(prop, worldsQuickConfig(t)); err != nil {
		t.Fatal(err)
	}
}

// TestContextIDProperty over the read-loop path (staging.go; W5b-1 review
// F1): events classified at their position, a provisional target that the
// attachment may not have seen announced (created before it attached),
// an attachment that lags behind the connection across commits, and
// reconnects during tree reads (wsHandover; review r5, F11).
func TestContextIDPropertyOrdered(t *testing.T) {
	prop := func(steps []worldStep) bool {
		if err := runWorldSteps(t, true, steps); err != nil {
			t.Logf("steps %v: %v", steps, err)
			return false
		}
		return true
	}
	if err := quick.Check(prop, worldsQuickConfig(t)); err != nil {
		t.Fatal(err)
	}
}

// worldsQuickConfig configures the context-id properties: 400 step lists
// generated from the seed AGENT_IOS_WORLDS_SEED, or from the time. The seed is
// logged, so a failing run can be repeated.
func worldsQuickConfig(t *testing.T) *quick.Config {
	seed := time.Now().UnixNano()
	if s := os.Getenv("AGENT_IOS_WORLDS_SEED"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("AGENT_IOS_WORLDS_SEED=%q: want an integer", s)
		}
		seed = n
	}
	t.Logf("generated step lists: AGENT_IOS_WORLDS_SEED=%d", seed)
	return &quick.Config{MaxCount: 400, Rand: rand.New(rand.NewSource(seed))}
}

// runWorldSteps runs the context-id property on steps (at most
// maxWorldSteps of them), ordered or not, and returns its first violation.
func runWorldSteps(t *testing.T, ordered bool, steps []worldStep) error {
	if len(steps) > maxWorldSteps {
		steps = steps[:maxWorldSteps]
	}
	w := newWorldRun(t)
	w.ordered = ordered
	for _, s := range steps {
		w.step(s)
	}
	w.finish()
	if err := checkContexts(w); err != nil {
		return err
	}
	return checkClientFrames(w)
}

// TestContextIDPropertyOrderedResetEndsChildWorlds replays counterexamples
// of TestContextIDPropertyOrdered (WS2a coverage gate, input #119, and its
// minimized form): a child frame gets a createIsolatedWorld name, a
// reconnect (wsHandover) resets the attachment, whose tree read announces
// the page's document as a new main-frame document with the child frame
// attached again, and the child frame then loads a new document. The reset
// ends the child frame's createIsolatedWorld names with the old document,
// as a main-frame navigation does (worlds.go clearContextsLocked; staging.go
// resetLocked), so its new document has no alias for that name.
func TestContextIDPropertyOrderedResetEndsChildWorlds(t *testing.T) {
	for name, steps := range map[string][]worldStep{
		"logged": {{225, 207, 146}, {17, 126, 131}, {246, 166, 176}, {177, 215, 39}, {243, 70, 75}, {23, 87, 119}, {134, 3, 117}, {15, 95, 4}, {67, 41, 41}, {224, 149, 126}, {180, 171, 157}, {95, 204, 243}, {62, 40, 82}, {144, 137, 169}, {108, 64, 210}},
		// child frame c1 attaches; reconnect; createIsolatedWorld{c1, util};
		// an aborted provisional target; flush (the reset); Runtime.enable
		// (its tree read's reply); c1 loads a new document
		"minimized": {{243, 70, 75}, {67, 41, 41}, {224, 149, 126}, {95, 204, 243}, {62, 40, 82}, {144, 137, 169}, {108, 64, 210}},
	} {
		if err := runWorldSteps(t, true, steps); err != nil {
			t.Errorf("%s steps %v: %v", name, steps, err)
		}
	}
}
