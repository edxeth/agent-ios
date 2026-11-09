package cdp

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
	"github.com/nnemirovsky/iwdp-mcp/internal/logsafe"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// The Fetch domain (tier 2; design docs/design/agentic-parity.md, sections
// 2.10 and 5.3; acceptance 7.8 item 12), on WIP request interception.
//
// Fetch.enable installs one WIP rule (Network.addInterception, a regular
// expression) per pattern, owned by the attachment's Fetch owner
// (session.fetchOwner, a desired-state owner of its own, so that
// Network.disable of the attachment leaves the rules alone and the detach
// releases both). WebKit pauses a matching request and sends
// Network.requestIntercepted (responseIntercepted at the response stage);
// the attachment reports it as Fetch.requestPaused with networkId = the
// request's CDP Network id. The answers (continueRequest, fulfillRequest,
// failRequest) go through the scheduler's control lane (core
// Scheduler.Control, design 2.10): they never wait behind an ordinary
// operation, such as a Runtime.evaluate whose promise waits for the
// paused request.
//
// Ownership of a paused request (decisions):
//   - CLI interception first. A request that a rule of the session's
//     interception handlers matches (a W4 "network route" in WIP mode,
//     "set offline", "set credentials"; devtools.IsHandlerOwner) is
//     answered by that handler (internal/devtools intercept.go claim) and
//     never reported to the CDP client, which sees only its Network
//     events. Every other request that a Fetch rule of the attachment
//     matches is the attachment's: the W4 safety net leaves it alone,
//     because an owner outside internal/devtools holds a matching rule
//     (claimedElsewhere). So no request gets two answers from the two
//     paths, and neither path waits for the other.
//   - Between CDP attachments of one page (design 5.2), the attachment
//     whose pattern matches the request's URL, stage and resource type
//     reports it. Every attachment whose rules may pause requests sees
//     every interception event of the page. The page takes one decision
//     per event for all of them (fetchClaims, W5c-1a review F10), in the
//     connection's read loop when the first attachment gets the event:
//     the request's type, from its latest requestWillBeSent, which the
//     read loop records for the page, so also for an attachment that
//     attached after it; and the attachments that claim the request, from
//     one registry of their patterns, not from their WIP rules, which
//     have no resource type. A claimant reports the request; a claimant
//     that no longer reports it when it handles the event (Fetch
//     disabled, attachment closed, event dropped) withdraws its claim. A
//     request that no attachment claims (any more) and no other owner's
//     rule matches is continued by each attachment that sees that; the
//     scheduler sends the first continue only. Limits: a request whose
//     requestWillBeSent no attachment got, or one of the oldest beyond
//     maxRequestTypes, has the type Other; an event of a connection that
//     does not count its messages (core.EventPosition.Seq 0; the
//     transports' webkit.Session counts them) gets a decision per
//     attachment, from the same type.
//   - No request stays paused. Requests that the attachment reported and
//     the client did not answer are continued unchanged when Fetch is
//     disabled, when the attachment detaches (also when its connection is
//     lost, conn.detachAll) and when the lease ends (core's release
//     barrier continues every hold first; the detach that follows finds
//     them answered). While the attachment's rules may still pause
//     requests that it will not report (after Fetch.disable, after the
//     detach until its owner's rules are gone, or when its event queue is
//     full), it continues each request that no other owner's rule
//     matches (Scheduler.ContinueUnclaimed). A completion of a request
//     that is no longer held is refused by the scheduler without a send,
//     so a request is answered at most once.
//   - A provisional target's paused request is reported at once, ahead of
//     the target's commit, with its requestWillBeSent (staging.go
//     publishEarlyLocked): WebKit commits a cross-process navigation only
//     after its document request is answered, and Puppeteer and
//     Playwright handle requestPaused only after the request's
//     requestWillBeSent.
//
// Limits: WIP has no resource-type filter (a pattern's resourceType is
// checked here, see the arbitration between attachments above), no
// response-stage failure, no request-to-response interception
// (continueRequest{interceptResponse}) and no authentication challenges
// (handleAuthRequests has no effect; continueWithAuth fails with Chrome's
// "authChallengeResponse not expected.").

const domFetch = "Fetch"

var (
	errInterception = &Error{Code: codeInvalidParams, Message: "Invalid InterceptionId."}
	// The bridge never pauses a request for an authentication challenge
	// (Fetch.authRequired is never sent), so every continueWithAuth is the
	// case that Chrome answers with this error.
	errAuth = &Error{Code: codeInvalidParams, Message: "authChallengeResponse not expected."}
)

func init() {
	register(
		Method{Name: "Fetch.enable", Levels: OnPage, Handle: fetchEnable},
		Method{Name: "Fetch.disable", Levels: OnPage, Handle: fetchDisable},
		Method{Name: "Fetch.continueRequest", Levels: OnPage, Handle: fetchAnswer(continueRequest)},
		Method{Name: "Fetch.fulfillRequest", Levels: OnPage, Handle: fetchAnswer(fulfillRequest)},
		Method{Name: "Fetch.failRequest", Levels: OnPage, Handle: fetchAnswer(failRequest)},
		Method{Name: "Fetch.continueWithAuth", Levels: OnPage, Handle: func(context.Context, *Call) (any, Later, error) {
			return nil, nil, errAuth
		}},
	)
	registerEvents(domFetch, "requestPaused")
	onPageEvent(evRequestIntercepted, fetchIntercepted(stageRequest))
	onPageEvent(evResponseIntercepted, fetchIntercepted(stageResponse))
}

// WIP interception events and stages.
const (
	evRequestIntercepted  = "Network.requestIntercepted"
	evResponseIntercepted = "Network.responseIntercepted"
	stageRequest          = "request"
	stageResponse         = "response"
)

// interceptionEvent reports whether method is a WIP interception event.
func interceptionEvent(method string) bool {
	return method == evRequestIntercepted || method == evResponseIntercepted
}

// fetchPattern is one Fetch.enable pattern and its WIP rule.
type fetchPattern struct {
	rule core.InterceptRule
	typ  string // CDP resource type; "" for any
}

// pausedReq is a request that the attachment reported paused, with the
// generation of core's hold that its event announced (holdOpen).
type pausedReq struct {
	key   netKey
	stage string
	gen   uint64
}

// fetchState is the Fetch part of pageState; ps.mu guards it.
type fetchState struct {
	on       bool // requests that its patterns match are reported
	used     bool // Fetch was enabled once: its rules may pause requests
	patterns []fetchPattern
	// rules and intercepting are what WebKit confirmed for the Fetch
	// owner: the rules it installed and whether it enabled interception.
	// A failed call leaves them as they were, so that a retry sends it
	// again (W5c-1a review F9); what the owner may hold besides is core's
	// record, which the release cleans up (fetchDisable).
	rules        []core.InterceptRule
	intercepting bool
	paused       map[string]pausedReq // by Fetch requestId
	next         uint64
	// claim is the decision of the interception event being translated
	// (translateLocked); nil in a model, which has no other attachment.
	claim *claimDecision
	// hold is the generation of core's hold that the interception event
	// being translated announced (wipEvent.hold).
	hold uint64
}

// fetchOwner is the desired-state owner of the attachment's Fetch rules.
func (s *session) fetchOwner() core.Owner {
	return core.SessionOwner("cdp-"+s.id+"-fetch", s.c.rec.session)
}

// globRegex turns a Fetch urlPattern ('*' any characters, '?' one,
// backslash escapes) into a regular expression that WebKit (Yarr, as a
// JavaScript RegExp) and Go read alike, anchored at both ends. tag makes
// the rule text unique per attachment, as an optional suffix that no URL
// has (a NUL character): WebKit refuses a second rule with the same text,
// and core does not count rule texts per owner, so two attachments with
// the same pattern must not share one rule.
func globRegex(glob, tag string) string {
	var b strings.Builder
	b.WriteString("^")
	esc := false
	for _, c := range glob {
		switch {
		case esc:
			b.WriteString(regexp.QuoteMeta(string(c)))
			esc = false
		case c == '\\':
			esc = true
		case c == '*':
			b.WriteString(".*")
		case c == '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`(?:\x00` + tag + `)?$`)
	return b.String()
}

// patternsMatch reports whether one of pats pauses a request of the CDP
// resource type typ for rawURL at stage.
func patternsMatch(pats []fetchPattern, stage, rawURL, typ string) bool {
	for _, p := range pats {
		if m, _ := devtools.RuleMatches(p.rule, stage, rawURL); m && (p.typ == "" || p.typ == typ) {
			return true
		}
	}
	return false
}

// requestTypeLocked is the CDP resource type of the request k in a model
// (an attachment takes it from the page's decision, fetchClaims): from
// its record, else from its requestWillBeSent staged for a provisional
// target (published only when the attachment reports the request), else
// Other.
func (ps *pageState) requestTypeLocked(k netKey) string {
	if r := ps.net.live[k]; r != nil && r.started {
		return r.typ
	}
	for _, ev := range ps.staged[k.target] {
		if ev.method == "Network.requestWillBeSent" && requestIDOf(ev.params) == k.wip {
			var e struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(ev.params, &e) // an event without a type is Other
			return devtools.CDPResourceType(e.Type)
		}
	}
	return "Other"
}

// fetchClaims is the Fetch arbitration of each page between its CDP
// attachments (W5c-1a review F10): one decision per interception event,
// which every attachment applies, also one that attached after the
// request started. Its mu is taken under a pageState's mu, never the
// other way round, and in the connection's read loop (onWire), where it
// guards only memory.
type fetchClaims struct {
	mu sync.Mutex
	m  map[*core.Page]*pageClaims
}

// Bounds of a page's arbitration memory.
const (
	// maxRequestTypes is the number of request types remembered, the
	// newest; an older request is taken as Other.
	maxRequestTypes = 4096
	// maxClaimDecisions is the number of interception events whose
	// decision is kept for the attachments that have not taken it from
	// the read loop yet (the read loop gives an event to every
	// subscription before it reads the next one).
	maxClaimDecisions = 64
)

// pageClaims is the arbitration state of one page; it lives while an
// attachment of the page is subscribed (join, leave).
type pageClaims struct {
	members int
	// patterns are the Fetch patterns of every attachment whose rules may
	// pause requests on the page (from its first Fetch.enable to the end
	// of its subscription), by its Fetch owner: nil while it reports
	// nothing (Fetch disabled, or the attachment closed).
	patterns map[core.Owner][]fetchPattern
	// types is the CDP resource type of each request by its latest
	// requestWillBeSent, which the read loop records for every
	// attachment before any of them handles a later event.
	types     map[netKey]string
	typeOrder []netKey
	decisions map[eventKey]*claimDecision
	decOrder  []eventKey
}

// eventKey names one event of a page: its connection and its place in
// that connection's stream (core.EventPosition).
type eventKey struct {
	conn webkit.Caller
	seq  uint64
}

// claimDecision is the arbitration of one interception event: the
// request's type and the attachments that claim it (their patterns match
// it), shared by every attachment that gets the event. A claimant that
// will not report it (Fetch was disabled since, or the attachment closed
// or dropped the event) withdraws (take), so the last one continues it
// unless another owner may answer it.
type claimDecision struct {
	typ       string
	claimants map[core.Owner]bool // guarded by fetchClaims.mu
}

// pageLocked returns pg's state, created on first use; c.mu is held.
func (c *fetchClaims) pageLocked(pg *core.Page) *pageClaims {
	if c.m == nil {
		c.m = map[*core.Page]*pageClaims{}
	}
	p := c.m[pg]
	if p == nil {
		p = &pageClaims{patterns: map[core.Owner][]fetchPattern{}, types: map[netKey]string{}, decisions: map[eventKey]*claimDecision{}}
		c.m[pg] = p
	}
	return p
}

// join counts an attachment subscribed to pg's events.
func (c *fetchClaims) join(pg *core.Page) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pageLocked(pg).members++
}

// leave ends the count of join, and forgets pg after its last attachment.
func (c *fetchClaims) leave(pg *core.Page) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.m[pg]; p != nil {
		if p.members--; p.members <= 0 {
			delete(c.m, pg)
		}
	}
}

// set records the patterns pats of owner o on pg.
func (c *fetchClaims) set(pg *core.Page, o core.Owner, pats []fetchPattern) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pageLocked(pg).patterns[o] = pats
}

// drop forgets owner o on pg.
func (c *fetchClaims) drop(pg *core.Page, o core.Owner) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.m[pg]; p != nil {
		delete(p.patterns, o)
	}
}

// owners returns the CDP Fetch owners on pg.
func (c *fetchClaims) owners(pg *core.Page) map[core.Owner]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[core.Owner]bool{}
	if p := c.m[pg]; p != nil {
		for o := range p.patterns {
			out[o] = true
		}
	}
	return out
}

// noteType records the type of the request that the requestWillBeSent
// params announce on target.
func (c *fetchClaims) noteType(pg *core.Page, target string, params json.RawMessage) {
	var e struct {
		RequestID string `json:"requestId"`
		Type      string `json:"type"`
	}
	if json.Unmarshal(params, &e) != nil || e.RequestID == "" {
		return
	}
	k := netKey{target, e.RequestID}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pageLocked(pg)
	if _, ok := p.types[k]; !ok {
		if len(p.typeOrder) == maxRequestTypes {
			delete(p.types, p.typeOrder[0])
			p.typeOrder = p.typeOrder[1:]
		}
		p.typeOrder = append(p.typeOrder, k)
	}
	p.types[k] = devtools.CDPResourceType(e.Type)
}

// decide returns the decision for the interception event (params, of
// method, from target) at pos: the one taken for it already, or a new one
// from the request's type and the patterns now. An event without a place
// in its stream (pos.Seq 0: a connection that does not count its
// messages) gets a decision of its own.
func (c *fetchClaims) decide(pg *core.Page, pos core.EventPosition, target, method string, params json.RawMessage) *claimDecision {
	var e interceptedReq
	_ = json.Unmarshal(params, &e) // an event without a request id claims nothing; its handlers skip it
	stage := stageRequest
	if method == evResponseIntercepted {
		stage = stageResponse
	}
	key := eventKey{pos.Conn, pos.Seq}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pageLocked(pg)
	if d := p.decisions[key]; d != nil && pos.Seq != 0 {
		return d
	}
	d := &claimDecision{typ: cmp.Or(p.types[netKey{target, e.RequestID}], "Other"), claimants: map[core.Owner]bool{}}
	for o, pats := range p.patterns {
		if patternsMatch(pats, stage, e.url(), d.typ) {
			d.claimants[o] = true
		}
	}
	if pos.Seq != 0 {
		if len(p.decOrder) == maxClaimDecisions {
			delete(p.decisions, p.decOrder[0])
			p.decOrder = p.decOrder[1:]
		}
		p.decOrder = append(p.decOrder, key)
		p.decisions[key] = d
	}
	return d
}

// take applies the decision d for owner o, which reports the request when
// reports is true: mine is whether o reports it, claimed whether another
// attachment still claims it. A claimant that does not report withdraws.
func (c *fetchClaims) take(d *claimDecision, o core.Owner, reports bool) (mine, claimed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d.claimants[o] {
		if reports {
			return true, true
		}
		delete(d.claimants, o)
	}
	return false, len(d.claimants) > 0
}

// publishClaimLocked records the attachment's patterns in the page's
// registry (fetchClaims) once Fetch was enabled: they claim requests while
// Fetch is on and the attachment is open.
func (ps *pageState) publishClaimLocked() {
	if ps.s == nil || !ps.fetch.used {
		return
	}
	var pats []fetchPattern
	if ps.fetch.on && !ps.closed {
		pats = ps.fetch.patterns
	}
	ps.s.c.s.claims.set(ps.s.pg, ps.s.fetchOwner(), pats)
}

// dropClaim removes the attachment from the page's registry when its
// subscription ends (its rules are released).
func (ps *pageState) dropClaim() {
	if ps.s != nil {
		ps.s.c.s.claims.drop(ps.s.pg, ps.s.fetchOwner())
	}
}

// otherRules reports whether an owner outside the CDP endpoint has a rule
// that may pause the request for rawURL at stage, and whether one of them
// is an interception handler of internal/devtools (a CLI route, offline
// mode, credentials). The CDP attachments are arbitrated by the event's
// decision (fetchClaims) instead.
func (ps *pageState) otherRules(stage, rawURL string) (handler, others bool) {
	if ps.s == nil {
		return false, false
	}
	mine := ps.s.fetchOwner()
	cdpOwners := ps.s.c.s.claims.owners(ps.s.pg)
	for o, rules := range ps.s.pg.PS.State.InterceptRules() {
		if o == mine || cdpOwners[o] {
			continue
		}
		for _, r := range rules {
			if m, known := devtools.RuleMatches(r, stage, rawURL); m || !known {
				others = true
				handler = handler || devtools.IsHandlerOwner(o)
			}
		}
	}
	return handler, others
}

// interceptedReq is a WIP interception event.
type interceptedReq struct {
	RequestID string `json:"requestId"`
	Request   *struct {
		URL      string            `json:"url"`
		Method   string            `json:"method"`
		Headers  map[string]string `json:"headers"`
		PostData *string           `json:"postData"`
	} `json:"request"`
	Response *struct {
		URL        string            `json:"url"`
		Status     int               `json:"status"`
		StatusText string            `json:"statusText"`
		Headers    map[string]string `json:"headers"`
	} `json:"response"`
}

func (e *interceptedReq) url() string {
	switch {
	case e.Response != nil:
		return e.Response.URL
	case e.Request != nil:
		return e.Request.URL
	}
	return ""
}

// fetchIntercepted reports a request that a pattern of the attachment
// paused as Fetch.requestPaused; a request that only its rules paused
// while it does not report them is continued (see the domain comment).
func fetchIntercepted(stage string) pageEventFn {
	return func(ps *pageState, target string, params json.RawMessage) {
		var e interceptedReq
		if !ps.fetch.used || json.Unmarshal(params, &e) != nil || e.RequestID == "" {
			return
		}
		k := netKey{target, e.RequestID}
		if ps.net.gapAt != 0 && !ps.holdOpen(k, stage, ps.fetch.hold) {
			return // a reset that comes after this event continued its hold (netGap)
		}
		u := e.url()
		var typ string
		var mine, claimed bool
		if ps.s == nil { // a model: no other attachment
			typ = ps.requestTypeLocked(k)
			mine = ps.fetch.on && patternsMatch(ps.fetch.patterns, stage, u, typ)
		} else { // the read loop decided the event (onWire)
			d := ps.fetch.claim
			typ = d.typ
			mine, claimed = ps.s.c.s.claims.take(d, ps.s.fetchOwner(), ps.fetch.on)
		}
		handler, others := ps.otherRules(stage, u)
		switch {
		case handler: // a CLI handler answers it
			return
		case mine:
		case !claimed && !others:
			ps.continueSoon(target, e.RequestID, stage)
			return
		default: // another attachment reports it, or another owner answers it
			return
		}
		r := ps.net.live[k]
		if r == nil || !r.started {
			ps.publishStagedRequestLocked(k)
			r = ps.net.live[k]
		}
		if r == nil {
			r = ps.addRequestLocked(k, ps.net.newID())
		}
		ps.fetch.next++
		id := fmt.Sprintf("interception-job-%d.0", ps.fetch.next)
		ps.fetch.paused[id] = pausedReq{key: k, stage: stage, gen: ps.fetch.hold}
		frame := r.frame
		if frame == "" {
			frame = ps.target
		}
		ev := map[string]any{"requestId": id, "frameId": frame, "resourceType": typ, "networkId": r.id}
		req := map[string]any{"url": r.url, "method": "GET", "headers": map[string]string{}, "initialPriority": "Medium", "referrerPolicy": "strict-origin-when-cross-origin"}
		if e.Request != nil {
			req["url"], req["method"] = e.Request.URL, e.Request.Method
			if e.Request.Headers != nil {
				req["headers"] = e.Request.Headers
			}
			if e.Request.PostData != nil && *e.Request.PostData != "" {
				req["postData"], req["hasPostData"] = *e.Request.PostData, true
			}
		}
		if e.Response != nil {
			ev["responseStatusCode"] = e.Response.Status
			ev["responseStatusText"] = e.Response.StatusText
			ev["responseHeaders"] = headerList(e.Response.Headers)
		}
		ev["request"] = req
		ps.emitLocked("", "Fetch.requestPaused", ev)
	}
}

// eventHold returns the generation of core's hold that the interception
// event of request k at stage announced, 0 when core has none open at that
// stage. It runs in the read loop (onWire), where core already recorded
// the hold (core Scheduler.OpenHold).
func (ps *pageState) eventHold(k netKey, stage string) uint64 {
	if st, gen, ok := ps.s.pg.PS.Sched.OpenHold(k.target, k.wip); ok && st == stage {
		return gen
	}
	return 0
}

// holdOpen reports whether core's hold of request k at stage with the
// generation gen is still open (W5c-1a review F14): a later hold of the
// same request, also at another stage, is another hold.
func (ps *pageState) holdOpen(k netKey, stage string, gen uint64) bool {
	st, g, ok := ps.s.pg.PS.Sched.OpenHold(k.target, k.wip)
	return ok && gen != 0 && st == stage && g == gen
}

// headerList is a CDP HeaderEntry list, sorted by name.
func headerList(h map[string]string) []map[string]string {
	out := []map[string]string{}
	for name, v := range h {
		out = append(out, map[string]string{"name": name, "value": v})
	}
	slices.SortFunc(out, func(a, b map[string]string) int { return strings.Compare(a["name"], b["name"]) })
	return out
}

// continueSoon continues the request id that target holds at stage,
// unchanged, as the daemon's own control step (no lease needed). A
// refusal means that it was answered already or that its target is gone.
func (ps *pageState) continueSoon(target, id, stage string) {
	if ps.s == nil {
		return
	}
	go ps.continueNow(target, id, stage)
}

func (ps *pageState) continueNow(target, id, stage string) {
	ctx, cancel := context.WithTimeout(context.Background(), core.DefaultControlBudget)
	defer cancel()
	err := ps.s.pg.PS.Sched.ContinueUnclaimed(ctx, target, id, stage)
	if err != nil && !errors.Is(err, core.ErrControlRefused) {
		ps.logf("cdp: page %s: continuing intercepted request %s of target %q: %v", logsafe.ID(ps.target), logsafe.ID(id), logsafe.ID(target), logsafe.Error(err))
	}
}

// continueAll continues the paused requests ps, each in parallel, and
// waits for them.
func (ps *pageState) continueAll(paused []pausedReq) {
	if ps.s == nil {
		return
	}
	var wg sync.WaitGroup
	for _, p := range paused {
		wg.Go(func() { ps.continueNow(p.key.target, p.key.wip, p.stage) })
	}
	wg.Wait()
}

// takePausedLocked empties the attachment's paused requests and returns
// them.
func (ps *pageState) takePausedLocked() []pausedReq {
	var out []pausedReq
	for _, p := range ps.fetch.paused {
		out = append(out, p)
	}
	clear(ps.fetch.paused)
	return out
}

// fetchHeldKeysLocked returns the requests the attachment holds paused.
func (ps *pageState) fetchHeldKeysLocked() map[netKey]bool {
	out := map[netKey]bool{}
	for _, p := range ps.fetch.paused {
		out[p.key] = true
	}
	return out
}

// fetchDropAllLocked forgets the paused requests: WebKit no longer holds
// them (a new connection).
func (ps *pageState) fetchDropAllLocked() { clear(ps.fetch.paused) }

// fetchTargetGoneLocked forgets the paused requests of a target that
// WebKit destroyed (core drops their holds too).
func (ps *pageState) fetchTargetGoneLocked(target string) {
	for id, p := range ps.fetch.paused {
		if p.key.target == target {
			delete(ps.fetch.paused, id)
		}
	}
}

// continueUnreported handles an interception event that the attachment
// will not translate (it closed, or its queue was full): the attachment
// withdraws its claim (fetchClaims.take), and the request is continued
// unless another attachment claims it or another owner may answer it
// (otherRules).
func (ps *pageState) continueUnreported(ev wipEvent) {
	var e interceptedReq
	if json.Unmarshal(ev.params, &e) != nil || e.RequestID == "" {
		return
	}
	stage := stageRequest
	if ev.method == evResponseIntercepted {
		stage = stageResponse
	}
	_, claimed := ps.s.c.s.claims.take(ev.claim, ps.s.fetchOwner(), false) // decided in the read loop (onWire)
	if _, others := ps.otherRules(stage, e.url()); !claimed && !others {
		ps.continueNow(ev.target, e.RequestID, stage)
	}
}

// ---- methods ----

// ownedBy sends method with owner as its desired-state owner inside mo.
func (s *session) ownedBy(ctx context.Context, mo *core.ManagedOp, owner core.Owner, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("%s: encoding params: %w", method, err)
	}
	_, err = s.c.s.o.Manager.RawAs(ctx, mo, owner, method, raw)
	return err
}

// fetchEnable sets the attachment's patterns (replacing earlier ones) and
// installs their WIP rules. The patterns count before the rules are
// installed, so no request that a new rule pauses is taken for an
// unreported one. It sends only what WebKit has not confirmed yet
// (fetchState.rules and intercepting), so a retry after a failed call
// sends that call again.
func fetchEnable(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Patterns *[]struct {
			URLPattern   *string `json:"urlPattern"`
			ResourceType string  `json:"resourceType"`
			RequestStage string  `json:"requestStage"`
		} `json:"patterns"`
		HandleAuthRequests bool `json:"handleAuthRequests"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	s := call.session
	ps := s.page
	all := "*"
	pats := []fetchPattern{}
	if p.Patterns == nil {
		pats = append(pats, fetchPattern{rule: core.InterceptRule{URL: globRegex(all, s.id[:8]), Stage: stageRequest, CaseSensitive: true, IsRegex: true}})
	} else {
		for _, q := range *p.Patterns {
			glob := all
			if q.URLPattern != nil {
				glob = *q.URLPattern
			}
			stage := stageRequest
			switch q.RequestStage {
			case "", "Request":
			case "Response":
				stage = stageResponse
			default:
				return nil, nil, &Error{Code: codeInvalidParams, Message: fmt.Sprintf("Invalid parameters: unknown requestStage %q", q.RequestStage)}
			}
			pats = append(pats, fetchPattern{rule: core.InterceptRule{URL: globRegex(glob, s.id[:8]), Stage: stage, CaseSensitive: true, IsRegex: true}, typ: q.ResourceType})
		}
	}
	var rules []core.InterceptRule
	for _, q := range pats {
		if !slices.Contains(rules, q.rule) {
			rules = append(rules, q.rule)
		}
	}
	ps.mu.Lock()
	ps.fetch.on, ps.fetch.used, ps.fetch.patterns = true, true, pats
	ps.publishClaimLocked()
	ps.mu.Unlock()
	owner := s.fetchOwner()
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		ps.mu.Lock()
		installed, intercepting := slices.Clone(ps.fetch.rules), ps.fetch.intercepting
		ps.mu.Unlock()
		defer func() { // what WebKit confirmed, also when a call failed part-way
			ps.mu.Lock()
			ps.fetch.rules, ps.fetch.intercepting = installed, intercepting
			ps.mu.Unlock()
		}()
		if !intercepting && len(rules) > 0 {
			if err := s.ownedBy(ctx, mo, owner, "Network.setInterceptionEnabled", map[string]bool{"enabled": true}); err != nil {
				return err
			}
			intercepting = true
		}
		for _, r := range rules {
			if !slices.Contains(installed, r) {
				if err := s.ownedBy(ctx, mo, owner, "Network.addInterception", r); err != nil {
					return err
				}
				installed = append(installed, r)
			}
		}
		for _, r := range slices.Clone(installed) {
			if !slices.Contains(rules, r) {
				// A target that does not hold the rule refuses its removal;
				// the rule is gone, as wanted.
				if err := s.ownedBy(ctx, mo, owner, "Network.removeInterception", r); err != nil && !core.MissingInterceptRule(err) {
					return err
				}
				installed = slices.DeleteFunc(installed, func(x core.InterceptRule) bool { return x == r })
			}
		}
		if intercepting && len(installed) == 0 {
			if err := s.ownedBy(ctx, mo, owner, "Network.setInterceptionEnabled", map[string]bool{"enabled": false}); err != nil {
				return err
			}
			intercepting = false
		}
		return nil
	})
	if err != nil {
		ps.mu.Lock()
		ps.fetch.on, ps.fetch.patterns = false, nil
		ps.publishClaimLocked()
		paused := ps.takePausedLocked()
		ps.mu.Unlock()
		ps.continueAll(paused)
		return nil, nil, err
	}
	return nil, nil, nil
}

// fetchDisable stops the reports, continues the paused requests on the
// control lane first (an operation that waits for one of them would hold
// the disable's own operation), then releases what core records for the
// Fetch owner, which includes rules whose installation or removal failed
// with an unknown outcome.
func fetchDisable(ctx context.Context, call *Call) (any, Later, error) {
	s := call.session
	ps := s.page
	owner := s.fetchOwner()
	ps.mu.Lock()
	ps.fetch.on, ps.fetch.patterns = false, nil
	ps.publishClaimLocked()
	paused := ps.takePausedLocked()
	ps.mu.Unlock()
	ps.continueAll(paused)
	if !slices.Contains(s.pg.PS.State.AllOwners(), owner) {
		return nil, nil, nil
	}
	return nil, nil, s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		err := s.c.s.o.Manager.ReleaseOwner(ctx, mo, owner)
		// Core forgets the owner also when a cleanup call failed (its
		// domain is then reset), so nothing stays confirmed.
		ps.mu.Lock()
		ps.fetch.rules, ps.fetch.intercepting = nil, false
		ps.mu.Unlock()
		return err
	})
}

// answer is a Fetch answer built for a request paused at stage: a WIP
// completion method and its params besides requestId.
type answer func(params json.RawMessage, stage string) (method string, wip map[string]any, err error)

// fetchAnswer completes a paused request through the control lane
// (Scheduler.Control under the attachment's lease and epoch), so the
// answer never waits behind an ordinary operation. An unknown id, or a
// request that is no longer held, gets Chrome's "Invalid InterceptionId.".
func fetchAnswer(build answer) Handler {
	return func(ctx context.Context, call *Call) (any, Later, error) {
		var p struct {
			RequestID string `json:"requestId"`
		}
		if err := call.decode(&p); err != nil {
			return nil, nil, err
		}
		s := call.session
		ps := s.page
		ps.mu.Lock()
		pr, ok := ps.fetch.paused[p.RequestID]
		ps.mu.Unlock()
		if !ok {
			return nil, nil, errInterception
		}
		method, params, err := build(call.Params, pr.stage)
		if err != nil {
			return nil, nil, err
		}
		err = s.control(func() error {
			_, err := s.pg.PS.Sched.Control(ctx, core.ControlMsg{
				Kind: core.ControlCompleteRequest, Session: s.c.rec.session, Epoch: s.epoch,
				RequestID: pr.key.wip, TargetID: pr.key.target, Method: method, Params: params,
			})
			return err
		})
		if err == nil || errors.Is(err, core.ErrControlRefused) {
			ps.mu.Lock()
			if ps.fetch.paused[p.RequestID] == pr {
				delete(ps.fetch.paused, p.RequestID)
			}
			ps.mu.Unlock()
		}
		if errors.Is(err, core.ErrControlRefused) {
			return nil, nil, errInterception
		}
		return nil, nil, err
	}
}

// control runs fn, a control-lane step of the attachment: refused after
// the detach, and waited for by it, but never queued behind an operation.
func (s *session) control(fn func() error) error {
	s.fmu.Lock()
	if s.fenced {
		s.fmu.Unlock()
		return errDetached
	}
	s.work.Add(1)
	s.fmu.Unlock()
	defer s.work.Done()
	return fn()
}

// cdpHeaders is a CDP HeaderEntry list as a WIP header object; repeated
// names are joined with ", ".
type cdpHeaders []struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (h cdpHeaders) object() map[string]string {
	out := map[string]string{}
	for _, e := range h {
		if v, ok := out[e.Name]; ok {
			out[e.Name] = v + ", " + e.Value
		} else {
			out[e.Name] = e.Value
		}
	}
	return out
}

func invalidParams(format string, a ...any) *Error {
	return &Error{Code: codeInvalidParams, Message: "Invalid parameters: " + fmt.Sprintf(format, a...)}
}

func continueRequest(raw json.RawMessage, stage string) (string, map[string]any, error) {
	var p struct {
		URL               *string     `json:"url"`
		Method            *string     `json:"method"`
		PostData          *string     `json:"postData"`
		Headers           *cdpHeaders `json:"headers"`
		InterceptResponse bool        `json:"interceptResponse"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", nil, invalidParams("%v", err)
	}
	if p.InterceptResponse {
		return "", nil, serverError("Fetch.continueRequest with interceptResponse is not supported on iOS WebKit; enable Fetch with a pattern whose requestStage is Response")
	}
	changed := p.URL != nil || p.Method != nil || p.PostData != nil || p.Headers != nil
	if !changed {
		return "Network.interceptContinue", map[string]any{"stage": stage}, nil
	}
	if stage != stageRequest {
		return "", nil, serverError("Fetch.continueRequest can change a request only at the request stage")
	}
	params := map[string]any{}
	if p.URL != nil {
		params["url"] = *p.URL
	}
	if p.Method != nil {
		params["method"] = *p.Method
	}
	if p.PostData != nil {
		params["postData"] = *p.PostData // base64 in CDP and in WIP
	}
	if p.Headers != nil {
		params["headers"] = p.Headers.object()
	}
	return "Network.interceptWithRequest", params, nil
}

func fulfillRequest(raw json.RawMessage, stage string) (string, map[string]any, error) {
	var p struct {
		ResponseCode          int         `json:"responseCode"`
		ResponseHeaders       *cdpHeaders `json:"responseHeaders"`
		BinaryResponseHeaders string      `json:"binaryResponseHeaders"`
		Body                  *string     `json:"body"`
		ResponsePhrase        string      `json:"responsePhrase"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", nil, invalidParams("%v", err)
	}
	if p.ResponseCode < 100 || p.ResponseCode > 999 {
		return "", nil, invalidParams("responseCode %d is not an HTTP status code", p.ResponseCode)
	}
	if stage == stageResponse && p.Body == nil {
		// CDP keeps the original body when body is absent at the response
		// stage; WIP's interceptWithResponse always replaces it, so that
		// form is refused rather than erasing the body. body "" is the
		// empty-body operation.
		return "", nil, serverError("Fetch.fulfillRequest without body at the response stage (keep the original body) is not supported on iOS WebKit; send the body, or continueRequest to keep the response")
	}
	headers := map[string]string{}
	switch {
	case p.ResponseHeaders != nil:
		headers = p.ResponseHeaders.object()
	case p.BinaryResponseHeaders != "":
		b, err := base64.StdEncoding.DecodeString(p.BinaryResponseHeaders)
		if err != nil {
			return "", nil, invalidParams("binaryResponseHeaders: %v", err)
		}
		for _, line := range strings.Split(string(b), "\x00") {
			if name, v, ok := strings.Cut(line, ":"); ok {
				headers[strings.TrimSpace(name)] = strings.TrimSpace(v)
			}
		}
	}
	mimeType := "text/plain"
	for name, v := range headers {
		if strings.EqualFold(name, "Content-Type") {
			if mt, _, err := mime.ParseMediaType(v); err == nil {
				mimeType = mt
			}
		}
	}
	text := p.ResponsePhrase
	if text == "" {
		text = http.StatusText(p.ResponseCode)
	}
	params := map[string]any{
		"content": "", "base64Encoded": false, "mimeType": mimeType, "status": p.ResponseCode, "statusText": text, "headers": headers,
	}
	if p.Body != nil && *p.Body != "" {
		if _, err := base64.StdEncoding.DecodeString(*p.Body); err != nil {
			return "", nil, invalidParams("body is not base64: %v", err)
		}
		params["content"], params["base64Encoded"] = *p.Body, true
	}
	if stage == stageResponse {
		return "Network.interceptWithResponse", params, nil
	}
	return "Network.interceptRequestWithResponse", params, nil
}

// wipErrorTypes maps CDP Network.ErrorReason to WIP ResourceErrorType.
var wipErrorTypes = map[string]string{
	"Failed": "General", "Aborted": "Cancellation", "TimedOut": "Timeout", "AccessDenied": "AccessControl",
	"ConnectionClosed": "General", "ConnectionReset": "General", "ConnectionRefused": "General",
	"ConnectionAborted": "General", "ConnectionFailed": "General", "NameNotResolved": "General",
	"InternetDisconnected": "General", "AddressUnreachable": "General", "BlockedByClient": "AccessControl",
	"BlockedByResponse": "AccessControl",
}

func failRequest(raw json.RawMessage, stage string) (string, map[string]any, error) {
	var p struct {
		ErrorReason string `json:"errorReason"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", nil, invalidParams("%v", err)
	}
	t, ok := wipErrorTypes[p.ErrorReason]
	if !ok {
		return "", nil, invalidParams("unknown errorReason %q", p.ErrorReason)
	}
	if stage != stageRequest {
		return "", nil, serverError("Fetch.failRequest at the response stage is not supported on iOS WebKit; continue or fulfill the response")
	}
	return "Network.interceptRequestWithError", map[string]any{"errorType": t}, nil
}
