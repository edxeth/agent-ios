package cdp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/edxeth/agent-ios/internal/devtools"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// Page events per CDP attachment (design docs/design/agentic-parity.md,
// sections 2.7, 5.2 and 5.3; proof P2 items 1 and 8).
//
// Each page attachment has a pageState for its whole life. It subscribes
// to every event of its page in the read loop of the page's connections
// (core Manager.SubscribeOrdered with webkit.AllEvents), so it sees the
// WIP events of all targets of the page's connection in arrival order,
// also those of a provisional target and across reconnects, each with its
// position (staging.go). One goroutine of the attachment takes them in
// that order; each event goes to the translators that the domain files
// registered for its method (onPageEvent), under ps.mu. Command handlers
// that emit events before their reply (Runtime.enable,
// Page.createIsolatedWorld) take the same lock, so all CDP events of an
// attachment keep one order.
//
// Provisional targets (2.7): WebKit loads a cross-process navigation in a
// provisional target, which reports its document and contexts before it
// commits (Target.didCommitProvisionalTarget) or is abandoned
// (Target.targetDestroyed without a commit). The events of a provisional
// target are staged in arrival order, within bounds, and translated only
// at its commit, so the new document generation is published once, at
// the commit; an abandoned target's staged events are dropped. A target
// is provisional at an event when it is not the delivering connection's
// active target at that event's position (staging.go); the commit signal
// is the outer Target event of the same stream, the one that core uses
// (DesiredState.followCommits, the scheduler's holds). The translators
// also check that a frame, context or lifecycle event comes from the
// target that reported the frame or document it names (reportedBy),
// because WIP frame ids are numbered per process.
//
// The pageState also keeps the frame and loader data of the current
// document generation (the bridge owns loader ids, 5.2) and the execution
// contexts and world aliases of worlds.go. A document is identified by its
// source target and its WIP main-frame loader id (2.7): a main-frame
// Page.frameNavigated with another (target, loader) pair starts a new
// generation; same-document navigations (the runtime hook of
// 66-cdp-page.js) do not.
//
// API for the domain files (W5b, W5c):
//   - onPageEvent(method, fn) in init(): fn translates a WIP event; it runs
//     with ps.mu held, in the attachment's event order.
//   - ps.emitLocked(domain, method, params): sends a CDP event on the
//     attachment's session when domain is enabled ("" = always).
//   - ps.loaderFor(target, wipLoader): the bridge loader id of a WIP
//     loader. W5c's Network translator must use it as the requestId of the
//     main-frame document request (Network.requestWillBeSent with type
//     Document and the main frame's WIP frame id, ps.main.wip.ID), so that
//     requestId == loaderId (5.2; Playwright frames.ts lines 719-745);
//     frameNavigated then carries the same id. To let Page.navigate return
//     at the document request rather than at the commit, that translator
//     calls ps.noteNavigationLocked(loader).
//   - ps.close() at detach: no event of the attachment is sent after it.
//   - Dialog warnings (5.2 "Dialogs"): every dialog that the session's
//     dialog override answers, whatever opened it (a page script, W3b's
//     actions, W5b-2's Input through the W3b runtime path), writes the
//     marked console.debug message of 80-dialogs.js. consoleMessage
//     (runtime_domain.go) turns it into a Log.entryAdded warning
//     (dialogEntryLocked, log_domain.go) on each attachment with Log
//     enabled, so no domain calls anything for it. Page.enable installs
//     the session's policy when the session has none
//     (session.ensureDialogPolicy); the override's Console ownership keeps
//     the messages flowing.

// Domains whose CDP events an attachment can enable. lifecycle is
// Page.setLifecycleEventsEnabled.
const (
	domPage      = "Page"
	domRuntime   = "Runtime"
	domLog       = "Log"
	domLifecycle = "lifecycle"
)

// pageEventFn translates one WIP event of a page attachment.
type pageEventFn func(ps *pageState, target string, params json.RawMessage)

var pageEventFns = map[string][]pageEventFn{}

// onPageEvent registers fn for the WIP event method. Domain files call it
// from init().
func onPageEvent(method string, fn pageEventFn) {
	pageEventFns[method] = append(pageEventFns[method], fn)
}

// wipFrame is a WIP Page.Frame.
type wipFrame struct {
	ID             string `json:"id"`
	ParentID       string `json:"parentId"`
	LoaderID       string `json:"loaderId"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	SecurityOrigin string `json:"securityOrigin"`
	MimeType       string `json:"mimeType"`
}

// frameInfo is a frame of the current document generation.
type frameInfo struct {
	wip    wipFrame
	target string // the target that reported it; "" when unknown
	loader string // bridge loader id
}

// loaderKey is a document: a WIP loader id of one target (2.7).
type loaderKey struct{ target, wip string }

// loaderMemory bounds the remembered loader ids: the current document,
// the document a navigation is loading, and back/forward cache entries
// that come back with their own loader id.
const loaderMemory = 16

// pageState is the page side of one page attachment.
type pageState struct {
	s      *session                        // nil in model tests
	target string                          // the CDP page target id
	send   func(method string, params any) // writes a CDP event on the attachment's session

	mu        sync.Mutex
	closed    bool
	unsub     func()
	unsubOnce sync.Once
	staging   // event order, provisional targets and their staged events (staging.go)
	enabled   map[string]bool
	life      context.Context // ends at close; bounds the attachment's goroutines
	endLife   context.CancelFunc

	// Document generation (2.7) and frames.
	gen     uint64 // 0 until the first document is known
	main    *frameInfo
	frames  map[string]*frameInfo // child frames by WIP frame id
	loaders []loaderEntry         // recent, oldest first

	// Navigations: navSeq grows with each new document and each
	// same-document navigation; navChanged is closed and replaced then.
	navSeq     uint64
	navLoader  string // loader of the last navigation; "" for a same-document one
	navChanged chan struct{}

	// Network idleness (page_domain.go, networkIdleLocked): the session's
	// network log once lifecycle events were turned on, and the document
	// generations whose idle wait started (idleWait) and ended
	// (idleReached).
	netlog      *devtools.Netlog
	idleWait    uint64
	idleReached uint64

	worldState          // execution contexts and aliases (worlds.go)
	css        cssState // CSS opt-in: own node ids, announced style sheets (css_domain.go)

	net   netState   // requests and their CDP ids (network_domain.go)
	dbg   dbgState   // parsed scripts and Debugger events (debugger_domain.go)
	fetch fetchState // interception (fetch_domain.go)

	// The attachment's bootstrap part (page_domain.go, runtime_domain.go).
	scripts    []initScript
	nextScript int
	bindings   map[string]binding // by binding name
	frameBound map[frameBind]bool // bindings installed in child frame documents (runtime_domain.go)
	exceptions int                // exceptionThrown ids
	jsHeap     float64            // last Memory tracking javascript size (performance_domain.go)

	// lastConsole is the last WIP Console.messageAdded params of each
	// target (runtime_domain.go consoleRepeated); a reset forgets them.
	lastConsole map[string]json.RawMessage
}

type loaderEntry struct {
	key loaderKey
	id  string
}

// newPageState starts the event subscription of the page attachment s and
// the goroutine that translates its events (staging.go). The connection
// installed at the subscription is the attachment's connection.
func newPageState(s *session) *pageState {
	ps := newPageModel(s.targetID(), func(method string, params any) { s.c.event(s.id, method, params) })
	ps.s = s
	s.c.s.claims.join(s.pg) // before the first event (onWire)
	ps.conn, ps.unsub = s.c.s.o.Manager.SubscribeOrdered(s.pg, webkit.AllEvents, ps.onWire)
	go ps.run()
	return ps
}

// newPageModel is a pageState without a subscription: the document,
// frame and context model that writes its events to send.
func newPageModel(target string, send func(method string, params any)) *pageState {
	life, endLife := context.WithCancel(context.Background())
	ps := &pageState{
		target: target, send: send, enabled: map[string]bool{}, frames: map[string]*frameInfo{},
		navChanged: make(chan struct{}), bindings: map[string]binding{}, frameBound: map[frameBind]bool{}, lastConsole: map[string]json.RawMessage{}, unsub: func() {},
		life: life, endLife: endLife,
	}
	ps.init()
	ps.initStaging()
	ps.net.initNet()
	ps.fetch.paused = map[string]pausedReq{}
	return ps
}

// reportedBy reports whether an event of target may describe the frame f:
// f was reported by target, or one of them is unknown ("").
func reportedBy(f *frameInfo, target string) bool {
	return target == "" || f.target == "" || f.target == target
}

// close ends the attachment's events; no event of the attachment is sent
// after it returns. The requests that it reported paused (Fetch) are
// continued, and so are the interception events that it queued but will
// not translate. When its Fetch rules may still pause requests, the
// subscription stays until unsubscribe (after the detach released the
// rules), and every interception event until then is continued unless
// another owner's rule paused it (fetch_domain.go).
func (ps *pageState) close() {
	ps.mu.Lock()
	ps.closed = true
	ps.publishClaimLocked()
	paused := ps.takePausedLocked()
	used := ps.fetch.used
	ps.mu.Unlock()
	// An offline rule that may be installed keeps the subscription until
	// the release too: its requests are decided in the read loop
	// (network_conditions.go offlinePhase).
	offline := ps.s != nil && ps.s.c.s.claims.closeOffline(ps.s)
	ps.endLife()
	ps.unwatchNetworkReset()
	queued := ps.q.close()
	if !used && !offline {
		ps.unsubscribe()
	}
	var wg sync.WaitGroup
	for _, ev := range queued {
		if used && interceptionEvent(ev.method) {
			wg.Go(func() { ps.continueUnreported(ev) })
		}
	}
	ps.continueAll(paused)
	wg.Wait()
}

// unsubscribe ends the attachment's subscription (once), and with it its
// place in the page's Fetch registry (fetchClaims). The claim goes first:
// a claimant of an event gets that event (fetchClaims.decide).
func (ps *pageState) unsubscribe() {
	ps.unsubOnce.Do(func() {
		ps.dropClaim()
		if ps.s != nil {
			ps.s.c.s.claims.setOffline(ps.s, offlineOff, nil) // the release removed its rule
		}
		ps.unsub()
		if ps.s != nil {
			ps.s.c.s.claims.leave(ps.s.pg)
		}
	})
}

// setEnabled turns the CDP events of domain on or off.
func (ps *pageState) setEnabled(domain string, on bool) {
	ps.mu.Lock()
	ps.enabled[domain] = on
	ps.mu.Unlock()
}

// emitLocked sends a CDP event on the attachment's session when domain is
// enabled ("" = always). The caller holds ps.mu.
func (ps *pageState) emitLocked(domain, method string, params any) {
	if ps.closed || (domain != "" && !ps.enabled[domain]) {
		return
	}
	ps.send(method, params)
}

// loaderFor returns the bridge loader id of the document (target,
// wipLoader), allocating one for a document not seen recently.
func (ps *pageState) loaderFor(target, wipLoader string) string {
	k := loaderKey{target, wipLoader}
	for _, e := range ps.loaders {
		if e.key == k {
			return e.id
		}
	}
	id := strings.ToUpper(randomHex(16))
	ps.loaders = append(ps.loaders, loaderEntry{k, id})
	if len(ps.loaders) > loaderMemory {
		ps.loaders = ps.loaders[1:]
	}
	return id
}

// frameID is the CDP id of the WIP frame wipID: the page's target id for
// the main frame (P2 item 1: Playwright finds a frame's session by it),
// the WIP id for a child frame.
func (ps *pageState) frameID(wipID string) string {
	if ps.main != nil && wipID == ps.main.wip.ID {
		return ps.target
	}
	return wipID
}

// cdpFrame is the CDP Page.Frame of f.
func (ps *pageState) cdpFrame(f *frameInfo) map[string]any {
	secure := "InsecureScheme"
	if strings.HasPrefix(f.wip.SecurityOrigin, "https://") {
		secure = "Secure"
	}
	out := map[string]any{
		"id": ps.frameID(f.wip.ID), "loaderId": f.loader, "url": f.wip.URL, "domainAndRegistry": "",
		"securityOrigin": f.wip.SecurityOrigin, "mimeType": f.wip.MimeType, "secureContextType": secure,
		"crossOriginIsolatedContextType": "NotIsolated", "gatedAPIFeatures": []string{},
	}
	if f.wip.ParentID != "" {
		out["parentId"] = ps.frameID(f.wip.ParentID)
	}
	if f.wip.Name != "" {
		out["name"] = f.wip.Name
	}
	return out
}

// wipTree is a WIP Page.FrameResourceTree.
type wipTree struct {
	Frame       wipFrame          `json:"frame"`
	ChildFrames []wipTree         `json:"childFrames"`
	Resources   []json.RawMessage `json:"resources"`
}

// seedLocked takes the current document from a Page.getResourceTree of
// target when no event has given one yet, and otherwise refreshes the
// frames of the same document. The tree is applied at its place in the
// event stream (staging.go, "Tree reads"), so for its document it is the
// truth about the frames there: a child frame that it lacks is removed
// (Page.frameDetached, as for the event), and a child frame that it has
// takes its data (loader, URL). It carries no contexts and no console
// records, so it leaves the other frames' contexts as they are. A tree of
// another document is older or newer than the event stream, which brings
// or brought its own frameNavigated, so it is not applied. After a reset
// (resetLocked) the seeded document is announced as a new one, with its
// child frames and, while Runtime is enabled, its default contexts, so the
// client reloads its state.
//
// A child frame that a tree adds to the current document is announced in
// the same way (frameAttached, frameNavigated and, while Runtime is
// enabled, its default context when the page has one): in Chrome's model
// a client learns every frame from these events, and a CDP client reads
// the frame tree only when it attaches (Puppeteer's and Playwright's frame
// managers), so a frame added silently would stay unknown to it. WebKit
// sends no frameAttached for the child frames of a document restored from
// the back/forward cache; a tree read is how the attachment learns them.
// Only the first seed of an attachment (no reset before it) adds its
// frames silently: the client reads them with Page.getFrameTree.
func (ps *pageState) seedLocked(target string, tree wipTree) {
	announce := false // the document is new to the client (a reset)
	current := false  // the tree refreshes the current document
	switch {
	case ps.main == nil:
		announce = ps.reseed
		ps.newDocumentLocked(target, tree.Frame, announce)
	case !ps.sameDocument(target, tree.Frame):
		return
	default:
		ps.main.wip = tree.Frame
		current = true
	}
	inTree := map[string]bool{}
	var added []string // child frames that the tree adds to the current document
	var walk func(ts []wipTree)
	walk = func(ts []wipTree) {
		for _, c := range ts {
			inTree[c.Frame.ID] = true
			if fi := ps.frames[c.Frame.ID]; fi != nil {
				if fi.wip.LoaderID != c.Frame.LoaderID {
					fi.loader = ps.loaderFor(fi.target, c.Frame.LoaderID)
				}
				fi.wip = c.Frame
			} else {
				fi := &frameInfo{wip: c.Frame, target: target, loader: ps.loaderFor(target, c.Frame.LoaderID)}
				ps.frames[c.Frame.ID] = fi
				if announce || current {
					ps.emitLocked(domPage, "Page.frameAttached", map[string]any{"frameId": c.Frame.ID, "parentFrameId": ps.frameID(c.Frame.ParentID)})
					ps.emitLocked(domPage, "Page.frameNavigated", map[string]any{"frame": ps.cdpFrame(fi), "type": "Navigation"})
				}
				if current {
					added = append(added, c.Frame.ID)
				}
			}
			walk(c.ChildFrames)
		}
	}
	walk(tree.ChildFrames)
	for id := range ps.frames {
		if !inTree[id] {
			ps.removeFrameLocked(id)
		}
	}
	if !ps.enabled[domRuntime] {
		return
	}
	switch {
	case announce:
		for _, f := range ps.framesLocked() {
			if f == ps.main.wip.ID || ps.pageHasContext(f) {
				ps.frameContextsLocked(f)
			}
		}
	case current:
		for _, f := range added {
			if ps.pageHasContext(f) {
				ps.frameContextsLocked(f)
			}
		}
	}
}

// pageHasContext reports whether the page's connection knows a default
// context of the WIP frame f (the scheduler's inventory).
func (ps *pageState) pageHasContext(f string) bool {
	return ps.s != nil && ps.s.pg.PS.Sched.FrameContext(f).ID != 0
}

// sameDocument reports whether the main frame f of target is the current
// document. A document seeded without its target adopts the first target
// that reports it.
func (ps *pageState) sameDocument(target string, f wipFrame) bool {
	m := ps.main
	if m == nil || m.wip.ID != f.ID || m.wip.LoaderID != f.LoaderID || (m.target != "" && m.target != target) {
		return false
	}
	if m.target == "" {
		m.target = target
	}
	return true
}

// newDocumentLocked starts a document generation with main frame f of
// target: the contexts and aliases of the old document are cleared, and
// with announce the init lifecycle event and frameNavigated are sent
// (5.2: "the bridge emits Runtime.executionContextsCleared,
// Page.frameNavigated, Runtime.executionContextCreated and lifecycle
// events instead" of a WebKit swap).
func (ps *pageState) newDocumentLocked(target string, f wipFrame, announce bool) {
	// After a reset (staging.go) its executionContextsCleared went out,
	// and no context was announced without a document since.
	cleared := ps.main == nil && ps.reseed
	ps.gen++
	ps.reseed = false
	if ps.main != nil && target != "" && ps.main.target == "" {
		ps.main.target = target
	}
	ps.main = &frameInfo{wip: f, target: target, loader: ps.loaderFor(target, f.LoaderID)}
	ps.frames = map[string]*frameInfo{}
	ps.clearContextsLocked(announce && !cleared)
	if announce {
		ps.lifecycleLocked("init")
		ps.emitLocked(domPage, "Page.frameNavigated", map[string]any{"frame": ps.cdpFrame(ps.main), "type": "Navigation"})
	}
	ps.cssNewDocumentLocked()
	ps.noteNavigationLocked(ps.main.loader)
}

// noteNavigationLocked wakes the waiters of Page.navigate: loader is the
// new document's loader id, "" for a same-document navigation.
func (ps *pageState) noteNavigationLocked(loader string) {
	ps.navSeq++
	ps.navLoader = loader
	close(ps.navChanged)
	ps.navChanged = make(chan struct{})
}

// navSignal is closed at the next change of the navigation sequence.
// (A named result type also keeps the quality gate's function parser,
// which reads "struct{}" in a result list as the body, measuring
// navState.)
type navSignal = <-chan struct{}

// navState returns the navigation sequence and a channel closed at its
// next change.
func (ps *pageState) navState() (uint64, navSignal) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.navSeq, ps.navChanged
}

// lifecycleLocked sends a Page.lifecycleEvent of the main frame's current
// document on the attachment's session (5.2: lifecycle events carry the
// page session id).
func (ps *pageState) lifecycleLocked(name string) {
	if ps.main == nil {
		return
	}
	ps.emitLocked(domLifecycle, "Page.lifecycleEvent", map[string]any{
		"frameId": ps.target, "loaderId": ps.main.loader, "name": name, "timestamp": monotonic(),
	})
}

var started = time.Now()

// monotonic is a CDP MonotonicTime: seconds since an arbitrary start.
func monotonic() float64 { return time.Since(started).Seconds() }
