package devtools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// Bounds of a network log (design 7.7 test 1).
const (
	DefaultMaxEntries   = 2000     // requests kept; the oldest are dropped first
	DefaultMaxBodyBytes = 64 << 20 // bytes of response bodies kept in memory
	// DefaultIdleQuiet is the quiet time of "wait --load networkidle"
	// (design 3.6): no request in flight for 500 ms.
	DefaultIdleQuiet = 500 * time.Millisecond
)

// Request states. capture_reset closes a request that was in flight across
// a Network reinitialization barrier (design 2.4).
const (
	StatePending      = "pending"
	StateFinished     = "finished"
	StateFailed       = "failed"
	StateRedirected   = "redirected"
	StateCaptureReset = "capture_reset"
)

// Request is one network request of the log. Target is the page target
// that sent the request's events: a provisional target's own id, before
// and after its commit ("" when the connection does not name targets).
// RequestID is WebKit's request id, which is unique per target only. ID is
// the request's id in the log, unique among the log's requests:
// "<target>:<requestId>" ("<requestId>" without a target), with '%', ':'
// and '#' in either part escaped as %25, %3A and %23. A redirect hop
// gets the id of its chain with a ":redirect<n>" suffix, and the last hop
// keeps the chain's id. When a target reuses a WebKit id for a new
// request, and when an id is taken, the id gets a "#<seq>" suffix. A
// chain keeps its id from its first event to its end, also when the
// history drops its record. "network requests",
// "network request <id>", HAR entries (_requestId) and the CDP events
// (SubscribeCDP) use ID.
type Request struct {
	ID              string            `json:"id"`
	RequestID       string            `json:"requestId"`
	Target          string            `json:"target,omitempty"`
	Seq             int               `json:"seq"`
	URL             string            `json:"url"`
	Method          string            `json:"method"`
	Type            string            `json:"type,omitempty"`
	FrameID         string            `json:"frameId,omitempty"`
	LoaderID        string            `json:"loaderId,omitempty"`
	RequestHeaders  map[string]string `json:"requestHeaders,omitempty"`
	PostData        string            `json:"postData,omitempty"`
	Status          int               `json:"status,omitempty"`
	StatusText      string            `json:"statusText,omitempty"`
	ResponseHeaders map[string]string `json:"responseHeaders,omitempty"`
	MimeType        string            `json:"mimeType,omitempty"`
	FromCache       bool              `json:"fromCache,omitempty"`
	State           string            `json:"state"`
	Error           string            `json:"error,omitempty"`
	// WallTime is when the request started (from WebKit's walltime).
	WallTime time.Time `json:"wallTime"`
	// StartTS, ResponseTS and EndTS are WebKit's monotonic timestamps in
	// seconds; 0 when the event was not seen.
	StartTS     float64          `json:"startTs"`
	ResponseTS  float64          `json:"responseTs,omitempty"`
	EndTS       float64          `json:"endTs,omitempty"`
	DataLength  int64            `json:"dataLength,omitempty"`
	EncodedSize int64            `json:"encodedSize,omitempty"`
	Timing      *json.RawMessage `json:"timing,omitempty"`
	// BodyGone says why the response body can no longer be read.
	BodyGone string `json:"bodyGone,omitempty"`

	chain int    // the request chain (rawSlot.chain) the request belongs to
	lost  string // why WebKit no longer has the body (lostLocked)
}

// Gap is a discontinuity of the log: a Network reinitialization barrier
// (design 2.4), or the Network reset of a new page connection (reattach,
// takeover or recovery; core reports both through WatchNetworkReset).
// Requests in flight across it are closed as capture_reset.
type Gap struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	AfterSeq int       `json:"afterSeq"` // the last request seen before the gap
	Closed   []string  `json:"closed,omitempty"`
}

type body struct {
	data   []byte
	base64 bool
}

// Netlog is the network capture of one session on one page (design 4.2
// "network requests": daemon netlog from Network.* events; there is no
// replay, so it starts when the capture starts).
type Netlog struct {
	p       *Page
	session string
	owner   core.Owner
	started time.Time

	mu         sync.Mutex
	maxEntries int
	maxBody    int64
	reqs       []*Request
	ids        map[string]*Request // every request of reqs by its ID
	slots      map[reqKey]*rawSlot // per target and WebKit request id
	chainIDs   map[string]*rawSlot // the slots by their chain's log id
	chains     int                 // request chains started; numbers them
	seq        int
	dropped    int
	bodies     map[string]body // by Request.ID, which names the target
	bodyOrder  []string
	bodyBytes  int64
	gaps       []Gap
	resets     int // network resets seen (onGap); Clear keeps the count
	// inflight holds the requests (target and WebKit request id) that
	// started and have not finished or failed. It is the network-idle
	// tracker and is kept apart from reqs: trimming the bounded history or
	// Clear does not finish a request. Only a network reset (onGap) closes
	// them all.
	inflight   map[reqKey]bool
	activity   time.Time
	changed    chan struct{}
	unsub      []func()
	cdpSubs    map[int]webkit.TargetEventHandler
	cdpNext    int
	harStart   int // Seq after which the running HAR recording starts; -1: none
	harContent string
}

// StartNetlog returns the network log of session, starting it when it does
// not run: the log subscribes to the Network events, then enables Network
// as owner "netlog:<session>" (design 2.4). started reports a new capture,
// which has not seen the requests that began before it. A start is one
// desired-state change, so provisional targets get Network.enable too.
func (p *Page) StartNetlog(ctx context.Context, c webkit.Caller, session string) (n *Netlog, started bool, err error) {
	if cur := p.Netlog(session); cur != nil {
		return cur, false, nil
	}
	err = p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		var cerr error
		n, started, cerr = p.startNetlog(ctx, c, session)
		return cerr
	})
	if err != nil {
		return nil, false, err
	}
	return n, started, nil
}

// startNetlog is StartNetlog inside its change. The change comes before
// p.mu, as in a lease release, whose change runs the capture's Release.
func (p *Page) startNetlog(ctx context.Context, c webkit.Caller, session string) (n *Netlog, started bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := p.nets[session]; n != nil {
		return n, false, nil
	}
	n = newNetlog(p, session)
	sub := subscribeTargetInOrder(p.ps.Caller)
	if sub == nil {
		sub = func(m string, h webkit.TargetEventHandler) func() { return webkit.SubscribeTarget(p.ps.Caller, m, h) }
	}
	for _, m := range netEvents {
		n.unsub = append(n.unsub, sub(m, n.event))
	}
	if err := acquireDomain(ctx, p.ps, c, n.owner, "Network"); err != nil {
		n.stop()
		return nil, false, err
	}
	n.unsub = append(n.unsub, p.ps.State.WatchNetworkReset(n.owner, n.onGap))
	p.ps.State.Put(core.Entry{
		Key: "devtools:netlog:" + session, Owner: n.owner, Apply: noop,
		Release: func(context.Context, core.Sender) error { p.dropNetlog(session, n); return nil },
	})
	if !p.ps.State.HasEntry("devtools:netlog:" + session) {
		// The operation ended, so the change recorded nothing (core-events
		// r3): no release would stop this capture.
		n.stop()
		return nil, false, fmt.Errorf("network log of session %q: the operation ended before the capture was recorded", session)
	}
	p.nets[session] = n
	return n, true, nil
}

// newNetlog returns an empty log of session on p that is not subscribed.
func newNetlog(p *Page, session string) *Netlog {
	return &Netlog{
		p: p, session: session, owner: owner("netlog", session), started: time.Now(),
		maxEntries: DefaultMaxEntries, maxBody: DefaultMaxBodyBytes,
		ids: map[string]*Request{}, slots: map[reqKey]*rawSlot{}, chainIDs: map[string]*rawSlot{}, bodies: map[string]body{}, inflight: map[reqKey]bool{},
		changed: make(chan struct{}), activity: time.Now(), cdpSubs: map[int]webkit.TargetEventHandler{}, harStart: -1,
	}
}

// Netlog returns the running network log of session, or nil.
func (p *Page) Netlog(session string) *Netlog {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nets[session]
}

func (p *Page) dropNetlog(session string, n *Netlog) {
	p.mu.Lock()
	if p.nets[session] == n {
		delete(p.nets, session)
	}
	p.mu.Unlock()
	n.stop()
}

func (n *Netlog) stop() {
	n.mu.Lock()
	unsub := n.unsub
	n.unsub = nil
	n.mu.Unlock()
	for _, u := range unsub {
		u()
	}
}

// SetLimits changes the bounds (tests and memory-constrained callers).
func (n *Netlog) SetLimits(maxEntries int, maxBodyBytes int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.maxEntries, n.maxBody = maxEntries, maxBodyBytes
	n.trimLocked()
}

var netEvents = []string{
	"Network.requestWillBeSent", "Network.responseReceived", "Network.dataReceived",
	"Network.loadingFinished", "Network.loadingFailed", "Network.requestServedFromMemoryCache",
}

// subscribeInOrder subscribes in the connection's read loop when it can, so
// that the events of one request are seen in the order WebKit sent them.
// Handlers must not block.
func subscribeInOrder(c webkit.Caller) func(string, webkit.EventHandler) func() {
	if ss, ok := c.(interface {
		SubscribeSync(string, webkit.EventHandler) func()
	}); ok {
		return ss.SubscribeSync
	}
	return c.Subscribe
}

// subscribeTargetInOrder returns the read-loop subscription of c with each
// event's source target (webkit.TargetEventHandler), or nil when c has
// none. webkit.Session and the daemon's page connection have it, and they
// are the only callers with read-loop subscribers.
func subscribeTargetInOrder(c webkit.Caller) func(string, webkit.TargetEventHandler) func() {
	if ss, ok := c.(interface {
		SubscribeTargetSync(string, webkit.TargetEventHandler) func()
	}); ok {
		return ss.SubscribeTargetSync
	}
	return nil
}

type wipRequest struct {
	URL      string            `json:"url"`
	Method   string            `json:"method"`
	Headers  map[string]string `json:"headers"`
	PostData string            `json:"postData"`
}

type wipResponse struct {
	URL        string            `json:"url"`
	Status     int               `json:"status"`
	StatusText string            `json:"statusText"`
	Headers    map[string]string `json:"headers"`
	MimeType   string            `json:"mimeType"`
	Source     string            `json:"source"`
	Timing     *json.RawMessage  `json:"timing"`
}

type wipEvent struct {
	RequestID        string       `json:"requestId"`
	FrameID          string       `json:"frameId"`
	LoaderID         string       `json:"loaderId"`
	Request          *wipRequest  `json:"request"`
	Response         *wipResponse `json:"response"`
	RedirectResponse *wipResponse `json:"redirectResponse"`
	Type             string       `json:"type"`
	Timestamp        float64      `json:"timestamp"`
	Walltime         float64      `json:"walltime"`
	DataLength       int64        `json:"dataLength"`
	EncodedLength    int64        `json:"encodedDataLength"`
	ErrorText        string       `json:"errorText"`
	Canceled         bool         `json:"canceled"`
	Metrics          *struct {
		ResponseBodyBytesReceived int64 `json:"responseBodyBytesReceived"`
	} `json:"metrics"`
	Resource *struct {
		URL      string       `json:"url"`
		Type     string       `json:"type"`
		Response *wipResponse `json:"response"`
		BodySize int64        `json:"bodySize"`
	} `json:"resource"`
}

func (n *Netlog) event(target, method string, params json.RawMessage) {
	var e wipEvent
	if json.Unmarshal(params, &e) != nil || e.RequestID == "" {
		return
	}
	n.mu.Lock()
	id := n.applyLocked(target, method, e)
	n.activity = time.Now()
	close(n.changed)
	n.changed = make(chan struct{})
	subs := make([]webkit.TargetEventHandler, 0, len(n.cdpSubs))
	for _, f := range n.cdpSubs {
		subs = append(subs, f)
	}
	n.mu.Unlock()
	if len(subs) > 0 {
		if m, p, ok := toCDP(method, params, id); ok {
			for _, f := range subs {
				f(target, m, p)
			}
		}
	}
}

// idEscaper escapes the separators of log ids in a target or WebKit id
// (see Request), so that no id part reads as a separator.
var idEscaper = strings.NewReplacer("%", "%25", ":", "%3A", "#", "%23")

// logID is the log id of WebKit's request id of target, before the
// uniqueness suffix (see Request).
func logID(target, requestID string) string {
	if target == "" {
		return idEscaper.Replace(requestID)
	}
	return idEscaper.Replace(target) + ":" + idEscaper.Replace(requestID)
}

// rawSlot is the log's state of one WebKit request id of a target. WebKit
// keeps the events and the response body of the latest request with an id
// only; a later request with the id starts a new chain in the slot. The
// slot outlives the history entry of a running chain, so the chain keeps
// its id until it ends, and it lives while the history has a request
// with the id.
type rawSlot struct {
	chain int      // the number of the latest chain with the id (Request.chain)
	id    string   // that chain's log id, from its first event to its end
	hops  int      // its redirects so far
	cur   *Request // its current hop; nil when the history dropped it
	live  bool     // the chain has not finished, failed or been reset
	refs  int      // requests of the history with the id
}

// allocLocked returns id, or id with a "#<seq>" suffix when a request of
// the history or a chain of a slot has id.
func (n *Netlog) allocLocked(id string) string {
	base := id
	for i := n.seq; n.ids[id] != nil || n.chainIDs[id] != nil; i++ {
		id = fmt.Sprintf("%s#%d", base, i)
	}
	return id
}

// newChainLocked starts a new request chain in the slot of k and gives it
// a log id. Earlier requests with the WebKit id lose their WebKit body.
func (n *Netlog) newChainLocked(k reqKey, live bool) *rawSlot {
	s := n.slots[k]
	if s == nil {
		s = &rawSlot{}
		n.slots[k] = s
	} else {
		if n.chainIDs[s.id] == s {
			delete(n.chainIDs, s.id)
		}
		if s.refs > 0 {
			n.reusedLocked(k)
		}
	}
	n.chains++
	s.chain, s.hops, s.cur, s.live = n.chains, 0, nil, live
	s.id = n.allocLocked(logID(k.target, k.id))
	n.chainIDs[s.id] = s
	return s
}

// reusedLocked marks the WebKit bodies of the history's requests with the
// WebKit id of k as lost: a later request reused the id, and WebKit would
// answer with that request's body.
func (n *Netlog) reusedLocked(k reqKey) {
	why := fmt.Sprintf("the response body is unavailable: a later request of the page target reused WebKit's request id %s, and WebKit keeps the body of the latest request with an id only", k.id)
	for _, r := range n.reqs {
		if r.Target == k.target && r.RequestID == k.id && r.lost == "" {
			n.lostLocked(r, why)
		}
	}
}

// lostLocked records that WebKit no longer has the body of r, for why. A
// body read before stays readable from the cache; BodyGone says why once
// the cache does not hold it (dropBodyLocked).
func (n *Netlog) lostLocked(r *Request, why string) {
	r.lost = why
	if _, cached := n.bodies[r.ID]; !cached {
		r.BodyGone = why
	}
}

// addLocked appends r, the new current hop of the chain of s, to the log.
func (n *Netlog) addLocked(s *rawSlot, r *Request) {
	r.chain = s.chain
	s.cur = r
	s.refs++
	n.ids[r.ID] = r
	n.reqs = append(n.reqs, r)
	n.trimLocked()
}

// pruneLocked forgets the slot s of k when its chain ended and the history
// has no request with its WebKit id.
func (n *Netlog) pruneLocked(k reqKey, s *rawSlot) {
	if s.refs > 0 || s.live || n.slots[k] != s {
		return
	}
	delete(n.slots, k)
	if n.chainIDs[s.id] == s {
		delete(n.chainIDs, s.id)
	}
}

// applyLocked applies the event method of target to the log and returns
// the log id of the event's request chain (logID when the log has no
// slot for the request).
func (n *Netlog) applyLocked(target, method string, e wipEvent) string {
	k := reqKey{target, e.RequestID}
	s := n.slots[k]
	switch method {
	case "Network.requestWillBeSent":
		n.seq++
		if s != nil && s.live && e.RedirectResponse != nil {
			// The next hop keeps the chain's id; the current one gets a
			// hop id (addLocked indexes the chain's id to the next hop).
			s.hops++
			if r := s.cur; r != nil {
				r.ID = n.allocLocked(fmt.Sprintf("%s:redirect%d", s.id, s.hops))
				n.ids[r.ID] = r
				r.State, r.EndTS = StateRedirected, e.Timestamp
				setResponse(r, e.RedirectResponse)
			}
		} else {
			s = n.newChainLocked(k, true)
		}
		r := &Request{
			ID: s.id, RequestID: e.RequestID, Target: target, Seq: n.seq, Type: e.Type, FrameID: e.FrameID, LoaderID: e.LoaderID,
			State: StatePending, StartTS: e.Timestamp,
		}
		if e.Walltime > 0 {
			r.WallTime = time.Unix(0, int64(e.Walltime*1e9))
		} else {
			r.WallTime = time.Now()
		}
		if e.Request != nil {
			r.URL, r.Method, r.RequestHeaders, r.PostData = e.Request.URL, e.Request.Method, e.Request.Headers, e.Request.PostData
		}
		n.inflight[k] = true
		n.addLocked(s, r)
	case "Network.requestServedFromMemoryCache":
		n.seq++
		s = n.newChainLocked(k, false)
		r := &Request{
			ID: s.id, RequestID: e.RequestID, Target: target, Seq: n.seq,
			FrameID: e.FrameID, LoaderID: e.LoaderID, Method: "GET",
			State: StateFinished, FromCache: true, StartTS: e.Timestamp, EndTS: e.Timestamp, WallTime: time.Now(),
		}
		if e.Resource != nil {
			r.URL, r.Type, r.DataLength = e.Resource.URL, e.Resource.Type, e.Resource.BodySize
			if e.Resource.Response != nil {
				setResponse(r, e.Resource.Response)
			}
		}
		n.addLocked(s, r)
	case "Network.responseReceived":
		if e.Response == nil {
			break
		}
		if s == nil || (!s.live && s.cur == nil) {
			// WebKitGTK 2.54 starts the document load of a cross-process
			// navigation before the provisional target has Network
			// enabled, so the new target reports the document's response
			// without its requestWillBeSent. Log it from the response; the
			// method and request headers stay unknown (empty). A running
			// chain whose record the history dropped gets no new record.
			n.seq++
			s = n.newChainLocked(k, true)
			n.inflight[k] = true
			n.addLocked(s, &Request{
				ID: s.id, RequestID: e.RequestID, Target: target, Seq: n.seq,
				URL: e.Response.URL, FrameID: e.FrameID, LoaderID: e.LoaderID, State: StatePending, StartTS: e.Timestamp, WallTime: time.Now(),
			})
		}
		if r := s.cur; r != nil {
			setResponse(r, e.Response)
			r.ResponseTS = e.Timestamp
			if e.Type != "" {
				r.Type = e.Type
			}
		}
	case "Network.dataReceived":
		if s != nil && s.cur != nil {
			s.cur.DataLength += e.DataLength
			s.cur.EncodedSize += e.EncodedLength
		}
	case "Network.loadingFinished":
		delete(n.inflight, k)
		if s == nil {
			break
		}
		s.live = false
		if r := s.cur; r != nil && r.State == StatePending {
			r.State, r.EndTS = StateFinished, e.Timestamp
			if e.Metrics != nil && e.Metrics.ResponseBodyBytesReceived > 0 {
				r.EncodedSize = e.Metrics.ResponseBodyBytesReceived
			}
		}
	case "Network.loadingFailed":
		delete(n.inflight, k)
		if s == nil {
			break
		}
		s.live = false
		if r := s.cur; r != nil && r.State == StatePending {
			r.State, r.EndTS, r.Error = StateFailed, e.Timestamp, e.ErrorText
			if e.Canceled && r.Error == "" {
				r.Error = "canceled"
			}
		}
	}
	if s == nil {
		return logID(target, e.RequestID)
	}
	n.pruneLocked(k, s)
	return s.id
}

func setResponse(r *Request, resp *wipResponse) {
	r.Status, r.StatusText, r.ResponseHeaders, r.MimeType = resp.Status, resp.StatusText, resp.Headers, resp.MimeType
	r.FromCache = r.FromCache || resp.Source == "memory-cache" || resp.Source == "disk-cache"
	if resp.Timing != nil {
		r.Timing = resp.Timing
	}
}

// trimLocked drops the oldest requests over the entry bound and the oldest
// bodies over the byte bound.
func (n *Netlog) trimLocked() {
	for len(n.reqs) > n.maxEntries && len(n.reqs) > 0 {
		old := n.reqs[0]
		n.reqs = n.reqs[1:]
		if k := (reqKey{old.Target, old.RequestID}); n.slots[k] != nil {
			s := n.slots[k]
			s.refs--
			if s.cur == old {
				s.cur = nil
			}
			n.pruneLocked(k, s)
		}
		if n.ids[old.ID] == old {
			delete(n.ids, old.ID)
		}
		n.dropBodyLocked(old.ID)
		n.dropped++
	}
	for n.bodyBytes > n.maxBody && len(n.bodyOrder) > 0 {
		n.dropBodyLocked(n.bodyOrder[0])
	}
}

func (n *Netlog) dropBodyLocked(id string) {
	if b, ok := n.bodies[id]; ok {
		n.bodyBytes -= int64(len(b.data))
		delete(n.bodies, id)
		if r := n.ids[id]; r != nil && r.lost != "" {
			r.BodyGone = r.lost
		}
		for i, x := range n.bodyOrder {
			if x == id {
				n.bodyOrder = append(n.bodyOrder[:i:i], n.bodyOrder[i+1:]...)
				break
			}
		}
	}
}

// onGap records a Network reinitialization barrier (design 2.4) or the
// Network reset of a new page connection, whose close and setup disable
// the backend's Network agent (core reports it also when a later setup
// step fails): requests in flight are closed as capture_reset, and WebKit
// no longer has the bodies of earlier requests.
func (n *Netlog) onGap(g core.NetworkGap) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.resets++
	gap := Gap{Start: g.Start, End: g.End, AfterSeq: n.seq}
	why := fmt.Sprintf("the response body is no longer available: a network reset at %s cleared WebKit's stored bodies", g.Start.Format(time.TimeOnly))
	for _, r := range n.reqs {
		if r.State == StatePending {
			r.State, r.Error = StateCaptureReset, "closed by a network reset (capture_reset)"
			gap.Closed = append(gap.Closed, r.ID)
		}
		n.lostLocked(r, why)
	}
	// Requests in flight that the history no longer holds are closed too.
	clear(n.inflight)
	for k, s := range n.slots {
		s.live = false
		n.pruneLocked(k, s)
	}
	n.gaps = append(n.gaps, gap)
	n.activity = time.Now()
	close(n.changed)
	n.changed = make(chan struct{})
}

// Snapshot is a copy of the log.
type Snapshot struct {
	Requests []Request `json:"requests"`
	Gaps     []Gap     `json:"gaps,omitempty"`
	Dropped  int       `json:"dropped,omitempty"`
	Started  time.Time `json:"started"`
}

// Filter selects requests of a snapshot. Zero fields match everything.
type Filter struct {
	URL    string // substring of the URL
	Type   string // WebKit resource type, case-insensitive
	Status string // exact code, "4xx" class, or "failed"
}

func (f Filter) match(r Request) bool {
	if f.URL != "" && !strings.Contains(r.URL, f.URL) {
		return false
	}
	if f.Type != "" && !strings.EqualFold(f.Type, r.Type) {
		return false
	}
	switch s := strings.ToLower(f.Status); {
	case s == "":
	case s == "failed":
		return r.State == StateFailed || r.State == StateCaptureReset
	case len(s) == 3 && strings.HasSuffix(s, "xx"):
		return r.Status/100 == int(s[0]-'0')
	default:
		return fmt.Sprint(r.Status) == s
	}
	return true
}

// Snapshot returns the requests that match f, oldest first, and the gaps.
func (n *Netlog) Snapshot(f Filter) Snapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := Snapshot{Dropped: n.dropped, Started: n.started, Gaps: append([]Gap(nil), n.gaps...)}
	for _, r := range n.reqs {
		if f.match(*r) {
			s.Requests = append(s.Requests, *r)
		}
	}
	return s
}

// Clear drops every request, gap and body of the log; the capture runs on.
// Requests in flight stay in flight for WaitIdle.
func (n *Netlog) Clear() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reqs, n.gaps, n.dropped = nil, nil, 0
	n.ids, n.bodies, n.bodyOrder, n.bodyBytes = map[string]*Request{}, map[string]body{}, nil, 0
	for k, s := range n.slots { // running chains keep their ids
		s.refs, s.cur = 0, nil
		n.pruneLocked(k, s)
	}
	if n.harStart >= 0 {
		n.harStart = n.seq
	}
}

// Get returns the request with log id id (Request.ID).
func (n *Netlog) Get(id string) (Request, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if r := n.ids[id]; r != nil {
		return *r, true
	}
	return Request{}, false
}

// Body returns the response body of the request with log id id: from
// memory when it was read before, else Network.getResponseBody, else
// Page.getResourceContent, both sent to the request's own target with
// WebKit's request id. It returns WebKit's error text when neither has
// it (a getResourceContent reply without content is no body), says so
// when a network reset cleared WebKit's bodies (design 2.4), and reports
// the body as unavailable when the request's target is gone, or when a
// later request of the target reused the WebKit id and the body was not
// read before (WebKit would answer with the later request's body). A reply
// that arrives after the log dropped the request, after a later request
// reused the WebKit id or after a network reset is discarded (bodyGen):
// Body then returns the body cached meanwhile, or "unavailable".
func (n *Netlog) Body(ctx context.Context, c webkit.Caller, id string) (data []byte, base64Encoded bool, err error) {
	n.mu.Lock()
	r := n.ids[id]
	if r == nil {
		n.mu.Unlock()
		return nil, false, fmt.Errorf("network request %q is not in the log (see 'network requests')", id)
	}
	if b, ok := n.bodies[id]; ok {
		n.mu.Unlock()
		return b.data, b.base64, nil
	}
	req, gen := *r, n.bodyGenLocked(r)
	n.mu.Unlock()
	switch {
	case req.BodyGone != "":
		return nil, false, fmt.Errorf("network request %s: %s", id, req.BodyGone)
	case req.State == StatePending:
		return nil, false, fmt.Errorf("network request %s is still in flight; its body is not complete", id)
	case req.State == StateRedirected:
		return nil, false, fmt.Errorf("network request %s is a redirect; it has no body", id)
	}
	tctx := webkit.WithTarget(ctx, req.Target)
	var v struct {
		Body          string `json:"body"`
		Base64Encoded bool   `json:"base64Encoded"`
		Content       string `json:"content"`
	}
	err = sendJSON(tctx, c, "Network.getResponseBody", map[string]string{"requestId": req.RequestID}, &v)
	if targetGone(err) {
		return nil, false, fmt.Errorf("network request %s: the response body is unavailable: page target %s, which loaded it, is gone: %w", id, req.Target, err)
	}
	if err != nil && req.FrameID != "" && req.URL != "" {
		ferr := sendJSON(tctx, c, "Page.getResourceContent", map[string]string{"frameId": req.FrameID, "url": req.URL}, &v)
		switch {
		case ferr == nil && v.Content != "":
			v.Body, err = v.Content, nil
		case ferr == nil:
			err = fmt.Errorf("%w; Page.getResourceContent returned no content", err)
		default:
			err = fmt.Errorf("%w; %w", err, ferr)
		}
	}
	if err != nil {
		return nil, false, fmt.Errorf("network request %s: WebKit returned no body: %w", id, err)
	}
	data = []byte(v.Body)
	n.mu.Lock()
	defer n.mu.Unlock()
	held := n.ids[id] == r
	if !held || n.bodyGenLocked(r) != gen {
		// The reply can be another request's body: keep a body that
		// another read cached before the change, else report the loss.
		if b, ok := n.bodies[id]; ok && held {
			return b.data, b.base64, nil
		}
		why := r.lost
		if why == "" {
			why = "the response body is unavailable: the log dropped the request (Clear or the history bound) while its body was read"
		}
		return nil, false, fmt.Errorf("network request %s: %s", id, why)
	}
	if _, cached := n.bodies[id]; !cached { // a concurrent read may have cached it
		n.bodies[id] = body{data: data, base64: v.Base64Encoded}
		n.bodyOrder = append(n.bodyOrder, id)
		n.bodyBytes += int64(len(data))
		n.trimLocked()
	}
	return data, v.Base64Encoded, nil
}

// bodyGen is the generation of WebKit's body of a request: the chain that
// WebKit's latest request with the request's WebKit id belongs to, and
// the count of network resets. While the log holds a request, its slot
// exists, and each later request with the WebKit id starts a chain with a
// new number (n.chains only grows), so an unchanged generation means
// that WebKit still holds the request's own body.
type bodyGen struct{ chain, resets int }

// bodyGenLocked returns the generation of WebKit's body of r, a request
// that the log holds.
func (n *Netlog) bodyGenLocked(r *Request) bodyGen {
	g := bodyGen{chain: -1, resets: n.resets}
	if s := n.slots[reqKey{r.Target, r.RequestID}]; s != nil {
		g.chain = s.chain
	}
	return g
}

// targetGone reports whether err says that the call's target no longer
// exists (a commit replaced it, or it was destroyed).
func targetGone(err error) bool {
	return errors.Is(err, webkit.ErrTargetSwapped) || errors.Is(err, webkit.ErrTargetAbandoned)
}

// DecodedBody returns the body bytes, decoding base64.
func DecodedBody(data []byte, base64Encoded bool) ([]byte, error) {
	if !base64Encoded {
		return data, nil
	}
	return base64.StdEncoding.DecodeString(string(data))
}

// InFlight returns the number of requests in flight, also those that the
// bounded history no longer holds.
func (n *Netlog) InFlight() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.inFlightLocked()
}

func (n *Netlog) inFlightLocked() int { return len(n.inflight) }

// IdleResult reports a network-idle wait.
type IdleResult struct {
	// StartedCapture: the wait started the capture, so requests that began
	// before it are not seen (design 3.6).
	StartedCapture bool          `json:"startedCapture"`
	Waited         time.Duration `json:"waited"`
	InFlight       int           `json:"inFlight"`
}

// WaitIdle waits until no request has been in flight for quiet, at most
// until ctx ends. On expiry it returns ctx's error with the number of
// requests still in flight.
func (n *Netlog) WaitIdle(ctx context.Context, quiet time.Duration) (IdleResult, error) {
	start := time.Now()
	for {
		n.mu.Lock()
		inflight, since, changed := n.inFlightLocked(), time.Since(n.activity), n.changed
		n.mu.Unlock()
		var wait <-chan time.Time
		if inflight == 0 {
			if since >= quiet {
				return IdleResult{Waited: time.Since(start)}, nil
			}
			t := time.NewTimer(quiet - since)
			wait = t.C
			defer t.Stop() // at most one timer per change of the log
		}
		select {
		case <-ctx.Done():
			return IdleResult{Waited: time.Since(start), InFlight: inflight}, fmt.Errorf("network did not become idle: %d request(s) still in flight: %w", inflight, ctx.Err())
		case <-changed:
		case <-wait:
		}
	}
}

// WaitNetworkIdle is the network tracker of "wait --load networkidle"
// (design 3.6, provided to W3b): it waits until the page has had no request
// in flight for quiet (DefaultIdleQuiet when 0). When the session's network
// capture does not run, it starts it (owner "netlog:<session>"), so the
// first wait can miss requests that started earlier; the result says so.
// c is the operation's caller for the Network.enable.
func WaitNetworkIdle(ctx context.Context, ps *core.PageSession, c webkit.Caller, quiet time.Duration) (IdleResult, error) {
	if quiet <= 0 {
		quiet = DefaultIdleQuiet
	}
	p, err := For(ps)
	if err != nil {
		return IdleResult{}, err
	}
	session, err := p.Session()
	if err != nil {
		return IdleResult{}, err
	}
	n, started, err := p.StartNetlog(ctx, c, session)
	if err != nil {
		return IdleResult{}, err
	}
	res, err := n.WaitIdle(ctx, quiet)
	res.StartedCapture = started
	return res, err
}

// SubscribeCDP delivers every later Network event of the log converted to
// CDP shape (ToCDP), for the CDP endpoint (W5), with its source target
// ("" when the connection does not name targets). The event's requestId
// is the log id of its request chain (Request.ID), so it is unique across
// targets and the same for every hop of a redirect chain, as CDP's is.
// fn runs in the page connection's read loop and must not block. It
// returns the unsubscribe.
func (n *Netlog) SubscribeCDP(fn webkit.TargetEventHandler) func() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cdpNext++
	id := n.cdpNext
	n.cdpSubs[id] = fn
	return func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.cdpSubs, id)
	}
}

// sortedHeaderNames returns the names of h in a stable order.
func sortedHeaderNames(h map[string]string) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
