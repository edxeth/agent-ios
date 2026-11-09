package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/wip"
)

// The Network domain (design docs/design/agentic-parity.md, sections 5.2
// "Navigation identity" and 5.3; acceptance 7.8 items 1 and 5).
//
// WIP Network events reach the attachment's ordered event stream
// (events.go, staging.go) and are translated here, under ps.mu, in their
// order:
//   - Request ids. WIP numbers requests per process, so two targets of one
//     page (a provisional swap) can report the same WIP id. The bridge
//     gives each request its own CDP id, which never repeats within the
//     attachment: the main-frame document request gets its document's
//     loader id (requestId == loaderId, 5.2; Playwright frames.ts lines
//     719-745), every other request the next number of a counter. A loader
//     id that already named a request is not given again (a counter id
//     is used instead), so the rule "unique" wins over "equal to the
//     loader" (TestNetworkIDProperty).
//   - The main-frame document request is a WIP request of type Document
//     in the main frame of its target: the attachment's main frame when
//     the target reported the current document, else the frame of the
//     target's first document request (a provisional target, which loads
//     only its main document before its commit), else any frame that is
//     not a known child frame of that target. Its translation calls
//     ps.noteNavigationLocked(loader), so Page.navigate returns at the
//     request (events.go).
//   - Every event carries the request's CDP frameId (the main frame is the
//     page target id, child frames keep their WIP ids, as in the Page
//     domain) and loaderId. WIP timestamps run on each target's inspector
//     stopwatch; they become the bridge's MonotonicTime (the base of the
//     Page lifecycle events) with an offset per target fixed by the
//     target's first event (its arrival time). WIP's ResourceTiming
//     (fetchStart in seconds, the rest in milliseconds after it) becomes
//     CDP's (requestTime in seconds, the rest in milliseconds after it).
//   - Requests in flight that can no longer end (their target went away,
//     or a reset, staging.go) end for the client with Network.loadingFailed
//     {canceled: true, errorText "net::ERR_ABORTED"}, so no client waits
//     on a request half reported. Their later WIP events are dropped.
//
// Network.enable makes the attachment's owner a Network owner (design
// 2.4); Network.disable gives that up, and with it the owner's extra
// headers, cache setting and network conditions, as Chrome's
// Network.disable resets them. While it is a Network owner, the
// attachment follows core's Network resets (WatchNetworkReset, design
// 2.4: the barrier that removes an extra header, a new page connection, a
// setup that disables Network): at the reset's place in its event stream
// (netGap), the requests in flight end with loadingFailed unless they are
// still held paused, the requests that the barrier continued are no
// longer paused for the client, the bodies of earlier requests are gone
// (getResponseBody gets the eviction text), and the client gets a
// Log.entryAdded warning. The page's document is not reset: WebKit's
// Network agent restarted, not the page.

const domNetwork = "Network"

// Bounds of the request records.
const (
	// maxNetRecords bounds the requests the attachment remembers (in
	// flight and ended, for getResponseBody and getRequestPostData).
	maxNetRecords = 2000
	// maxPostData bounds the post data kept per request; maxPostTotal all
	// of it together.
	maxPostData  = 1 << 20
	maxPostTotal = 16 << 20
	// postDataInEvent is the largest post data sent in
	// requestWillBeSent; larger post data is read with getRequestPostData
	// (Chrome's maxPostDataSize default).
	postDataInEvent = 64 << 10
	// evictedMemory bounds the ids remembered as evicted (their
	// getResponseBody gets Chrome's eviction error).
	evictedMemory = 256
	// docIDMemory bounds the loader ids remembered as given to a
	// document request.
	docIDMemory = 64
	// maxNetTargets bounds the per-target memos (main frame, clock offset).
	maxNetTargets = 16
)

// Chrome's error texts (inspector_network_agent.cc).
var (
	errNoResource  = serverError("No resource with given identifier found")
	errNoData      = serverError("No data found for resource with given identifier")
	errEvicted     = serverError("Request content was evicted from inspector cache")
	errNoPostRes   = serverError("No resource with given id was found")
	errNoPostData  = serverError("No post data available for the request")
	abortedRequest = "net::ERR_ABORTED"
)

func init() {
	register(
		Method{Name: "Network.enable", Levels: OnPage, Handle: networkEnable},
		Method{Name: "Network.disable", Levels: OnPage, Handle: networkDisable},
		Method{Name: "Network.getResponseBody", Levels: OnPage, Handle: getResponseBody},
		Method{Name: "Network.getRequestPostData", Levels: OnPage, Handle: getRequestPostData},
		Method{Name: "Network.setCacheDisabled", Levels: OnPage, Handle: setCacheDisabled},
		// The same override as Emulation.setUserAgentOverride (W5b-2): one
		// value per attachment, Page.overrideUserAgent.
		Method{Name: "Network.setUserAgentOverride", Levels: OnPage, Handle: emuUserAgent},
		Method{Name: "Network.emulateNetworkConditions", Levels: OnPage, Handle: emulateNetworkConditions},
	)
	registerEvents(domNetwork, "requestWillBeSent", "responseReceived", "dataReceived", "loadingFinished",
		"loadingFailed", "requestServedFromCache")
	onPageEvent("Network.requestWillBeSent", func(ps *pageState, target string, params json.RawMessage) {
		ps.requestWillBeSentLocked(target, params, false)
	})
	onPageEvent("Network.responseReceived", netResponseReceived)
	onPageEvent("Network.dataReceived", netRequestEvent("Network.dataReceived", false))
	onPageEvent("Network.loadingFinished", netRequestEvent("Network.loadingFinished", true))
	onPageEvent("Network.loadingFailed", netRequestEvent("Network.loadingFailed", true))
	onPageEvent("Network.requestServedFromMemoryCache", netMemoryCache)
}

// ---- request records ----

// netKey is a WIP request: WIP request ids are numbered per target.
type netKey struct{ target, wip string }

// netReq is one request of the attachment.
type netReq struct {
	key      netKey
	id       string // CDP requestId
	loader   string // CDP loaderId
	frame    string // CDP frameId
	typ      string // CDP resource type
	wipFrame string
	url      string
	doc      bool // the main-frame document request
	started  bool // its requestWillBeSent was translated
	shown    bool // the client got its requestWillBeSent (Network was enabled)
	ended    bool
	post     string
	hasPost  bool
	postGone bool // its post data was larger than maxPostData or dropped by maxPostTotal
	bodyGone bool // a Network reset cleared WebKit's copy of its response body (netGapLocked)
}

// netState is the Network part of pageState; ps.mu guards it.
type netState struct {
	live      map[netKey]*netReq // requests in flight
	byID      map[string]*netReq // every remembered request
	order     []string           // the ids of byID, oldest first
	next      uint64             // the counter of request ids
	evicted   [evictedMemory]string
	evNext    int
	docIDs    [docIDMemory]string
	docNext   int
	mainOf    map[string]string  // target -> WIP main frame id of its document request
	offsets   map[string]float64 // target -> offset of its stopwatch from the bridge's clock
	postBytes int
	at        float64 // arrival time of the event being translated (translateLocked)
	// gapAt is the place in the event stream (eventQueue.push) of a
	// Network reset that the attachment has not applied yet (netGap); 0
	// for none.
	gapAt uint64
	// unwatch ends the attachment's watch of core's Network resets; nil
	// while it does not watch (networkEnable).
	unwatch func()
}

func (n *netState) initNet() {
	n.live = map[netKey]*netReq{}
	n.byID = map[string]*netReq{}
	n.mainOf = map[string]string{}
	n.offsets = map[string]float64{}
}

// usedDocID reports whether id named a document request already.
func (n *netState) usedDocID(id string) bool {
	for _, d := range n.docIDs {
		if d == id {
			return true
		}
	}
	return n.byID[id] != nil
}

// newID is the next counter id; it never equals a loader id (32 hex
// digits) or an earlier counter id.
func (n *netState) newID() string {
	n.next++
	return strconv.FormatUint(n.next, 10)
}

// wasEvicted reports whether id named a request that the bound dropped.
func (n *netState) wasEvicted(id string) bool {
	for _, e := range n.evicted {
		if e != "" && e == id {
			return true
		}
	}
	return false
}

// memo bounds a per-target memo before a new entry: beyond maxNetTargets
// it starts again (the entries are hints that a target's next events set
// again).
func memo[V any](m map[string]V, target string) {
	if _, ok := m[target]; !ok && len(m) >= maxNetTargets {
		clear(m)
	}
}

// addRequestLocked records a new request of key with the CDP id id. The
// oldest ended request makes room; with maxNetRecords requests in flight
// the oldest of them ends as aborted.
func (ps *pageState) addRequestLocked(k netKey, id string) *netReq {
	n := &ps.net
	for len(n.order) >= maxNetRecords {
		victim := 0
		for i, old := range n.order {
			if n.byID[old].ended {
				victim = i
				break
			}
		}
		r := n.byID[n.order[victim]]
		ps.failRequestLocked(r)
		ps.forgetRequestLocked(r)
		n.order = append(n.order[:victim], n.order[victim+1:]...)
		n.evicted[n.evNext] = r.id
		n.evNext = (n.evNext + 1) % evictedMemory
	}
	r := &netReq{key: k, id: id}
	if old := n.live[k]; old != nil {
		ps.endRequestLocked(old)
	}
	n.live[k] = r
	n.byID[id] = r
	n.order = append(n.order, id)
	return r
}

// forgetRequestLocked drops r from byID and frees its post data.
func (ps *pageState) forgetRequestLocked(r *netReq) {
	n := &ps.net
	ps.endRequestLocked(r)
	n.postBytes -= len(r.post)
	r.post = ""
	delete(n.byID, r.id)
}

// endRequestLocked marks r ended; its later WIP events are dropped.
func (ps *pageState) endRequestLocked(r *netReq) {
	r.ended = true
	if ps.net.live[r.key] == r {
		delete(ps.net.live, r.key)
	}
}

// failRequestLocked ends r for the client: a request whose
// requestWillBeSent it got and whose end it did not get gets
// loadingFailed (canceled), so it does not wait for it.
func (ps *pageState) failRequestLocked(r *netReq) {
	if r.shown && !r.ended {
		ps.emitLocked(domNetwork, "Network.loadingFailed", map[string]any{
			"requestId": r.id, "frameId": r.frame, "loaderId": r.loader, "timestamp": monotonic(), "type": r.typ, "errorText": abortedRequest, "canceled": true,
		})
	}
	ps.endRequestLocked(r)
}

// storePostLocked keeps r's post data within the bounds.
func (ps *pageState) storePostLocked(r *netReq, post *string) {
	n := &ps.net
	n.postBytes -= len(r.post)
	r.post, r.hasPost, r.postGone = "", post != nil && *post != "", false
	if !r.hasPost {
		return
	}
	if len(*post) > maxPostData {
		r.postGone = true
		return
	}
	for _, id := range n.order {
		if n.postBytes+len(*post) <= maxPostTotal {
			break
		}
		if o := n.byID[id]; o != nil && o.post != "" {
			n.postBytes -= len(o.post)
			o.post, o.postGone = "", true
		}
	}
	r.post = *post
	n.postBytes += len(r.post)
}

// isMainFrameLocked reports whether the WIP frame wipFrame of target is
// the main frame (see the domain comment).
func (ps *pageState) isMainFrameLocked(target, wipFrame string) bool {
	if ps.main != nil && (ps.main.target == target || ps.main.target == "" || target == "") {
		return wipFrame == ps.main.wip.ID
	}
	if m, ok := ps.net.mainOf[target]; ok {
		return wipFrame == m
	}
	f := ps.frames[wipFrame]
	return f == nil || f.target != target
}

// netFrameLocked is the CDP frame id of the WIP frame wipFrame of target.
func (ps *pageState) netFrameLocked(target, wipFrame string) string {
	if m, ok := ps.net.mainOf[target]; ok && m == wipFrame {
		return ps.target
	}
	return ps.frameID(wipFrame)
}

// wipTime converts a WIP timestamp of target to the bridge's
// MonotonicTime (see the domain comment). A missing timestamp (0) is the
// arrival time.
func (ps *pageState) wipTime(target string, ts float64) float64 {
	n := &ps.net
	if ts <= 0 {
		return n.at
	}
	off, ok := n.offsets[target]
	if !ok {
		memo(n.offsets, target)
		off = n.at - ts
		n.offsets[target] = off
	}
	return ts + off
}

// netTargetGoneLocked ends the requests of a target that WebKit
// destroyed or that a commit replaced: they never finish.
func (ps *pageState) netTargetGoneLocked(target string) {
	for k, r := range ps.net.live {
		if k.target == target {
			ps.failRequestLocked(r)
		}
	}
	delete(ps.net.mainOf, target)
	delete(ps.net.offsets, target)
	ps.fetchTargetGoneLocked(target)
}

// netResetLocked ends every request in flight at a reset (staging.go):
// events may be missing since, so the attachment cannot finish them.
// Requests that the attachment holds paused (Fetch.requestPaused without
// an answer) stay when the connection stays: WebKit still holds them and
// the client can answer. On a new connection they go too: WebKit
// continued them when the old connection's frontend went away.
func (ps *pageState) netResetLocked(newConn bool) {
	held := ps.fetchHeldKeysLocked()
	if newConn {
		held = nil
		ps.fetchDropAllLocked()
		clear(ps.net.offsets)
	}
	for k, r := range ps.net.live {
		if !held[k] {
			ps.failRequestLocked(r)
		}
	}
}

// watchNetworkReset makes the attachment follow core's Network resets
// (netGap), once.
func (ps *pageState) watchNetworkReset() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.net.unwatch == nil && !ps.closed {
		ps.net.unwatch = ps.s.pg.PS.State.WatchNetworkReset(ps.s.owner, ps.netGap)
	}
}

// unwatchNetworkReset ends the watch of watchNetworkReset.
func (ps *pageState) unwatchNetworkReset() {
	ps.mu.Lock()
	stop := ps.net.unwatch
	ps.net.unwatch = nil
	ps.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// netGap is core's report of a Network reset (core.NetworkGap). The
// requests that the barrier continued are no longer paused for the client
// at once. The rest applies at the reset's place in the event stream: the
// events that the connections delivered before the report describe
// requests from before the reset, so netGapLocked runs when the
// attachment has handled them (appliedLocked). Until then an interception
// event that a connection delivered before the report is a pause for the
// client only when core still holds the very hold that the event
// announced, at its stage and generation (fetchIntercepted): the reset
// continued the others, by its barrier or by its Network.disable,
// also those whose events the attachment had not handled yet (W5c-1a
// review F11).
func (ps *pageState) netGap(g core.NetworkGap) {
	at := ps.q.pushed()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for id, p := range ps.fetch.paused {
		if slices.Contains(g.Requests, p.key.wip) {
			delete(ps.fetch.paused, id)
		}
	}
	if ps.applied >= at {
		ps.netGapLocked()
		return
	}
	ps.net.gapAt = max(ps.net.gapAt, at)
}

// netGapLocked applies a Network reset (netGap): the requests in flight
// that the attachment does not hold paused end, the response bodies of
// the others are gone, and the client is warned (design 2.4). Resets that
// were pending together get one warning. A paused request whose hold
// core no longer has open (the same stage and generation, holdOpen) is no
// longer held for the client either: the reset continued it, also when
// its pause was reported after the barrier's snapshot of the holds.
func (ps *pageState) netGapLocked() {
	if ps.s != nil {
		for id, p := range ps.fetch.paused {
			if !ps.holdOpen(p.key, p.stage, p.gen) {
				delete(ps.fetch.paused, id)
			}
		}
	}
	held := ps.fetchHeldKeysLocked()
	ended := 0
	for k, r := range ps.net.live {
		if !held[k] {
			if r.shown {
				ended++
			}
			ps.failRequestLocked(r)
		}
	}
	for _, r := range ps.net.byID {
		if r.ended || !held[r.key] {
			r.bodyGone = true
		}
	}
	text := fmt.Sprintf("Network was reset on the page (WebKit's Network agent was reinitialized: an extra HTTP header was removed, the page connection was renewed, or a setup disabled Network): %d requests in flight ended as failed, and the response bodies of earlier requests can no longer be read", ended)
	ps.emitLocked(domLog, "Log.entryAdded", map[string]any{"entry": map[string]any{
		"source": "network", "level": "warning", "text": text, "timestamp": nowMillis(),
	}})
}

// ---- event translators ----

// wipNetEvent is the part of a WIP Network event the translators read.
type wipNetEvent struct {
	RequestID        string          `json:"requestId"`
	FrameID          string          `json:"frameId"`
	LoaderID         string          `json:"loaderId"`
	Type             string          `json:"type"`
	Timestamp        float64         `json:"timestamp"`
	RedirectResponse json.RawMessage `json:"redirectResponse"`
	Request          *struct {
		URL      string  `json:"url"`
		PostData *string `json:"postData"`
	} `json:"request"`
	Response *struct {
		Source string `json:"source"`
	} `json:"response"`
}

// wipNetTiming is WIP's Network.ResourceTiming.
type wipNetTiming struct {
	FetchStart            float64 `json:"fetchStart"`
	DomainLookupStart     float64 `json:"domainLookupStart"`
	DomainLookupEnd       float64 `json:"domainLookupEnd"`
	ConnectStart          float64 `json:"connectStart"`
	ConnectEnd            float64 `json:"connectEnd"`
	SecureConnectionStart float64 `json:"secureConnectionStart"`
	RequestStart          float64 `json:"requestStart"`
	ResponseStart         float64 `json:"responseStart"`
}

// cdpTiming converts WIP's ResourceTiming of target to CDP's: requestTime
// is fetchStart on the bridge's clock; the phases stay milliseconds after
// it. WIP reports a phase that did not happen as 0, CDP as -1 (DNS,
// connect, TLS); sending and receiving the headers always happen.
func (ps *pageState) cdpTiming(target string, t *wipNetTiming) map[string]any {
	opt := func(v float64) float64 {
		if v <= 0 {
			return -1
		}
		return v
	}
	ssl, sslEnd := opt(t.SecureConnectionStart), -1.0
	if ssl >= 0 {
		sslEnd = opt(t.ConnectEnd)
	}
	return map[string]any{
		"requestTime": ps.wipTime(target, t.FetchStart), "proxyStart": -1, "proxyEnd": -1,
		"dnsStart": opt(t.DomainLookupStart), "dnsEnd": opt(t.DomainLookupEnd),
		"connectStart": opt(t.ConnectStart), "connectEnd": opt(t.ConnectEnd), "sslStart": ssl, "sslEnd": sslEnd,
		"workerStart": -1, "workerReady": -1, "workerFetchStart": -1, "workerRespondWithSettled": -1,
		"sendStart": t.RequestStart, "sendEnd": t.RequestStart, "pushStart": 0, "pushEnd": 0,
		"receiveHeadersStart": t.ResponseStart, "receiveHeadersEnd": t.ResponseStart,
	}
}

// cdpEvent converts a WIP Network event with W4's converter
// (devtools.ToCDP) and gives it the request's CDP ids and frame and the
// bridge's clock.
func (ps *pageState) cdpEvent(target, method string, params json.RawMessage, r *netReq, e *wipNetEvent) map[string]any {
	_, raw, ok := devtools.ToCDP(method, params)
	var out map[string]any
	if !ok || json.Unmarshal(raw, &out) != nil {
		return nil
	}
	out["requestId"] = r.id
	out["timestamp"] = ps.wipTime(target, e.Timestamp)
	// Every Network event of the bridge names the request's frame and
	// loader (docs/notes/w5.md W5c-1a), also those whose CDP form has no
	// such field.
	out["loaderId"], out["frameId"] = r.loader, r.frame
	if _, has := out["type"]; has {
		out["type"] = r.typ
	}
	return out
}

// requestWillBeSentLocked translates a WIP requestWillBeSent (also one
// built for a memory-cache hit, cached). A request id that its target
// reports again without a redirect is a new request (WebKit never does
// it, but the id map must not merge two requests).
func (ps *pageState) requestWillBeSentLocked(target string, params json.RawMessage, cached bool) *netReq {
	var e wipNetEvent
	if json.Unmarshal(params, &e) != nil || e.RequestID == "" || e.Request == nil {
		return nil
	}
	n := &ps.net
	k := netKey{target, e.RequestID}
	r := n.live[k]
	redirect := r != nil && r.started && len(e.RedirectResponse) > 0 && string(e.RedirectResponse) != "null"
	if r != nil && r.started && !redirect {
		ps.endRequestLocked(r)
		r = nil
	}
	if !redirect {
		doc := !cached && e.Type == "Document" && ps.isMainFrameLocked(target, e.FrameID)
		loader := ps.loaderFor(target, e.LoaderID)
		if r == nil {
			id := n.newID()
			if doc && !n.usedDocID(loader) {
				id = loader
			}
			r = ps.addRequestLocked(k, id)
		}
		if doc {
			memo(n.mainOf, target)
			n.mainOf[target] = e.FrameID
			n.docIDs[n.docNext] = loader
			n.docNext = (n.docNext + 1) % docIDMemory
		}
		r.doc, r.loader, r.wipFrame = doc, loader, e.FrameID
		r.frame = ps.netFrameLocked(target, e.FrameID)
		r.typ = devtools.CDPResourceType(e.Type)
		r.shown = ps.enabled[domNetwork]
		r.started = true
	}
	r.url = e.Request.URL
	ps.storePostLocked(r, e.Request.PostData)
	if out := ps.cdpEvent(target, "Network.requestWillBeSent", params, r, &e); out != nil && r.shown {
		if req, ok := out["request"].(map[string]any); ok && (r.postGone || len(r.post) > postDataInEvent) {
			delete(req, "postData")
		}
		if redirect {
			if rr, ok := out["redirectResponse"].(map[string]any); ok {
				ps.addTimingLocked(target, rr, e.RedirectResponse)
			}
		}
		ps.emitLocked(domNetwork, "Network.requestWillBeSent", out)
	}
	if r.doc && !redirect {
		ps.noteNavigationLocked(r.loader)
	}
	return r
}

// addTimingLocked adds the converted timing of the WIP response raw to
// the CDP response out.
func (ps *pageState) addTimingLocked(target string, out map[string]any, raw json.RawMessage) {
	var w struct {
		Timing *wipNetTiming `json:"timing"`
	}
	if json.Unmarshal(raw, &w) == nil && w.Timing != nil && w.Timing.FetchStart > 0 {
		out["timing"] = ps.cdpTiming(target, w.Timing)
	}
}

// liveRequestLocked returns the request of a WIP event of target that the
// attachment follows, or nil.
func (ps *pageState) liveRequestLocked(target string, params json.RawMessage) (*netReq, *wipNetEvent) {
	var e wipNetEvent
	if json.Unmarshal(params, &e) != nil || e.RequestID == "" {
		return nil, nil
	}
	r := ps.net.live[netKey{target, e.RequestID}]
	if r == nil || !r.started {
		return nil, nil
	}
	return r, &e
}

func netResponseReceived(ps *pageState, target string, params json.RawMessage) {
	r, e := ps.liveRequestLocked(target, params)
	if r == nil {
		return
	}
	out := ps.cdpEvent(target, "Network.responseReceived", params, r, e)
	if out == nil || !r.shown {
		return
	}
	var w struct {
		Response json.RawMessage `json:"response"`
	}
	_ = json.Unmarshal(params, &w)
	if resp, ok := out["response"].(map[string]any); ok {
		ps.addTimingLocked(target, resp, w.Response)
	}
	if e.Response != nil && (e.Response.Source == "disk-cache" || e.Response.Source == "memory-cache") {
		ps.emitLocked(domNetwork, "Network.requestServedFromCache", map[string]any{"requestId": r.id, "frameId": r.frame, "loaderId": r.loader})
	}
	ps.emitLocked(domNetwork, "Network.responseReceived", out)
}

// netRequestEvent translates dataReceived, loadingFinished and
// loadingFailed; end marks the request's last event.
func netRequestEvent(method string, end bool) pageEventFn {
	return func(ps *pageState, target string, params json.RawMessage) {
		r, e := ps.liveRequestLocked(target, params)
		if r == nil {
			return
		}
		if out := ps.cdpEvent(target, method, params, r, e); out != nil && r.shown {
			if method == "Network.loadingFailed" {
				out["type"] = r.typ
			}
			ps.emitLocked(domNetwork, method, out)
		}
		if end {
			ps.endRequestLocked(r)
		}
	}
}

// netMemoryCache translates WIP's requestServedFromMemoryCache into
// Chrome's sequence for a memory-cache hit: requestWillBeSent,
// requestServedFromCache, responseReceived, loadingFinished.
func netMemoryCache(ps *pageState, target string, params json.RawMessage) {
	var e struct {
		RequestID   string          `json:"requestId"`
		FrameID     string          `json:"frameId"`
		LoaderID    string          `json:"loaderId"`
		DocumentURL string          `json:"documentURL"`
		Timestamp   float64         `json:"timestamp"`
		Initiator   json.RawMessage `json:"initiator"`
		Resource    struct {
			URL      string          `json:"url"`
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			BodySize int64           `json:"bodySize"`
		} `json:"resource"`
	}
	if json.Unmarshal(params, &e) != nil || e.RequestID == "" {
		return
	}
	base := map[string]any{"requestId": e.RequestID, "frameId": e.FrameID, "loaderId": e.LoaderID, "timestamp": e.Timestamp, "type": e.Resource.Type}
	with := func(kv ...any) json.RawMessage {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		raw, _ := json.Marshal(m) // plain maps of decoded values always encode
		return raw
	}
	req := map[string]any{"url": e.Resource.URL, "method": "GET", "headers": map[string]string{}}
	r := ps.requestWillBeSentLocked(target, with("documentURL", e.DocumentURL, "request", req,
		"walltime", float64(time.Now().UnixMilli())/1000, "initiator", e.Initiator), true)
	if r == nil {
		return
	}
	if r.shown {
		ps.emitLocked(domNetwork, "Network.requestServedFromCache", map[string]any{"requestId": r.id, "frameId": r.frame, "loaderId": r.loader})
	}
	if len(e.Resource.Response) > 0 && string(e.Resource.Response) != "null" {
		netResponseReceivedNoCacheEvent(ps, target, with("response", e.Resource.Response))
	}
	netRequestEvent("Network.loadingFinished", true)(ps, target, with("metrics", map[string]any{"responseBodyBytesReceived": e.Resource.BodySize}))
}

// netResponseReceivedNoCacheEvent is netResponseReceived for a response
// whose requestServedFromCache was sent already.
func netResponseReceivedNoCacheEvent(ps *pageState, target string, params json.RawMessage) {
	r, e := ps.liveRequestLocked(target, params)
	if r == nil || !r.shown {
		return
	}
	if out := ps.cdpEvent(target, "Network.responseReceived", params, r, e); out != nil {
		ps.emitLocked(domNetwork, "Network.responseReceived", out)
	}
}

// ---- methods ----

// networkEnable makes the attachment a Network owner and starts its
// Network events.
func networkEnable(ctx context.Context, call *Call) (any, Later, error) {
	s := call.session
	s.page.setEnabled(domNetwork, true)
	s.page.watchNetworkReset()
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		return s.owned(ctx, mo, "Network.enable", nil)
	})
	if err != nil {
		s.page.setEnabled(domNetwork, false)
		s.page.unwatchNetworkReset()
		return nil, nil, err
	}
	return nil, nil, nil
}

// networkDisable stops the attachment's Network events and gives up its
// Network ownership with the owner's headers, cache and conditions
// settings (core: DesiredState.dropOwner); a Fetch of the attachment
// keeps its own owner (fetch_domain.go).
func networkDisable(ctx context.Context, call *Call) (any, Later, error) {
	s := call.session
	s.page.setEnabled(domNetwork, false)
	s.page.unwatchNetworkReset()
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		return s.owned(ctx, mo, "Network.disable", nil)
	})
	return nil, nil, err
}

// requestOf returns the request of the CDP id id, or Chrome's error for
// an unknown (noRes) or evicted one.
func (ps *pageState) requestOf(id string, noRes *Error) (netReq, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r := ps.net.byID[id]
	switch {
	case r != nil:
		return *r, nil
	case ps.net.wasEvicted(id):
		return netReq{}, errEvicted
	}
	return netReq{}, noRes
}

// WebKit's getResponseBody errors (InspectorNetworkAgent::getResponseBody)
// and Chrome's texts for them.
var wipBodyErrors = map[string]*Error{
	"Missing resource for given requestId":              errNoResource,
	"Resource content was evicted from inspector cache": errEvicted,
	"Missing content of resource for given requestId":   errNoData,
}

// getResponseBody reads the body from the request's own target with
// WebKit's request id (WIP Network.getResponseBody, as W4's
// Netlog.Body). A target that is gone has no body any more.
func getResponseBody(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		RequestID string `json:"requestId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	s := call.session
	r, err := s.page.requestOf(p.RequestID, errNoResource)
	if err != nil {
		return nil, nil, err
	}
	if r.bodyGone {
		return nil, nil, errEvicted // a Network reset cleared it (netGapLocked)
	}
	var res json.RawMessage
	err = s.do(ctx, call.Method, core.ClassRead, func(ctx context.Context, mo *core.ManagedOp) error {
		var err error
		res, err = mo.Op.Caller().Send(webkit.WithTarget(ctx, r.key.target), "Network.getResponseBody", map[string]string{"requestId": r.key.wip})
		return err
	})
	var pe *webkit.ErrorData
	switch {
	case err == nil:
		var b struct {
			Body          string `json:"body"`
			Base64Encoded bool   `json:"base64Encoded"`
		}
		if err := json.Unmarshal(res, &b); err != nil {
			return nil, nil, serverError("Network.getResponseBody: WebKit returned %s: %v", res, err)
		}
		return map[string]any{"body": b.Body, "base64Encoded": b.Base64Encoded}, nil, nil
	case errors.As(err, &pe) && wipBodyErrors[pe.Message] != nil:
		return nil, nil, wipBodyErrors[pe.Message]
	case errors.Is(err, webkit.ErrTargetSwapped) || errors.Is(err, webkit.ErrTargetAbandoned):
		return nil, nil, errEvicted // the request's target is gone (devtools targetGone)
	}
	return nil, nil, err
}

// getRequestPostData returns the post data stored from the request's
// requestWillBeSent.
func getRequestPostData(_ context.Context, call *Call) (any, Later, error) {
	var p struct {
		RequestID string `json:"requestId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	r, err := call.session.page.requestOf(p.RequestID, errNoPostRes)
	switch {
	case err != nil:
		return nil, nil, err
	case !r.hasPost:
		return nil, nil, errNoPostData
	case r.postGone:
		return nil, nil, errEvicted
	}
	return map[string]any{"postData": r.post}, nil, nil
}

// setCacheDisabled is WIP Network.setResourceCachingDisabled, owned by
// the attachment.
func setCacheDisabled(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		CacheDisabled bool `json:"cacheDisabled"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	s := call.session
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		return s.owned(ctx, mo, "Network.setResourceCachingDisabled", map[string]bool{"disabled": p.CacheDisabled})
	})
	return nil, nil, err
}

// emulateNetworkConditions limits the download rate with WIP
// Network.setEmulatedConditions, where the device's iOS command list has
// it. WIP has no latency, upload or offline emulation: a positive
// latency or upload limit and offline are refused (iwdp-cli set offline
// emulates offline by interception).
func emulateNetworkConditions(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Offline            bool    `json:"offline"`
		Latency            float64 `json:"latency"`
		DownloadThroughput float64 `json:"downloadThroughput"`
		UploadThroughput   float64 `json:"uploadThroughput"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	s := call.session
	if p.Offline {
		return nil, nil, serverError("offline emulation through Network.emulateNetworkConditions is not supported on iOS WebKit; use iwdp-cli set offline on")
	}
	// A positive latency or upload limit turns throttling on in Chrome;
	// WIP cannot emulate either, so it is refused before any change. 0 and
	// -1 (no limit, what clients send to clear throttling) are accepted.
	if p.Latency > 0 || p.UploadThroughput > 0 {
		return nil, nil, serverError("latency and upload throttling cannot be emulated on iOS WebKit (latency %v ms, uploadThroughput %v); only downloadThroughput is supported", p.Latency, p.UploadThroughput)
	}
	if v := s.t.osVersion; v != "" {
		if known, ok := wip.Supported(v, "Network.setEmulatedConditions"); known && !ok {
			return nil, nil, serverError("network conditions cannot be emulated on iOS %s: Network.setEmulatedConditions is not in its command list", v)
		}
	}
	params := map[string]any{}
	if p.DownloadThroughput > 0 {
		params["bytesPerSecondLimit"] = int64(p.DownloadThroughput)
	}
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		return s.owned(ctx, mo, "Network.setEmulatedConditions", params)
	})
	return nil, nil, err
}
