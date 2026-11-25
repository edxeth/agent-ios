package devtools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/logsafe"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/wip"
)

// Route modes (design 4.2 "network route"): WIP request interception, or
// the fetch and XMLHttpRequest patch of the bootstrap (--mode js).
const (
	ModeWIP = "wip"
	ModeJS  = "js"
)

// Route is one "network route" rule.
type Route struct {
	ID          int               `json:"id"`
	Pattern     string            `json:"pattern"`
	Regex       string            `json:"regex"`
	Abort       bool              `json:"abort,omitempty"`
	Status      int               `json:"status,omitempty"`
	Body        *string           `json:"body,omitempty"`
	ContentType string            `json:"contentType,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Delay       time.Duration     `json:"delay,omitempty"`
	Mode        string            `json:"mode"`

	session string
	re      *regexp.Regexp
}

// fulfills reports whether the route answers with its own response.
func (r *Route) fulfills() bool { return r.Status != 0 || r.Body != nil }

// PatternRegex turns a route pattern into the regular expression that both
// WebKit (Network.addInterception with isRegex; WebKit searches the URL)
// and Go use. A pattern with "*" is a URL glob as in agent-browser and
// Playwright: "**" matches any characters, "*" any characters except "/",
// and the whole URL must match. A pattern without "*" matches when the URL
// contains it. The expression uses only syntax that JavaScript (WebKit's
// Yarr) and Go's RE2 read the same way.
func PatternRegex(pattern string) (string, error) {
	if pattern == "" {
		return "", fmt.Errorf("route pattern is empty")
	}
	if !strings.Contains(pattern, "*") {
		return regexp.QuoteMeta(pattern), nil
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			b.WriteString("[^/]*")
			i++
		default:
			j := i
			for j < len(pattern) && pattern[j] != '*' {
				j++
			}
			b.WriteString(regexp.QuoteMeta(pattern[i:j]))
			i = j
		}
	}
	b.WriteString("$")
	return b.String(), nil
}

// credential is an origin-scoped credential. The password never leaves
// this struct: it is not in errors, logs or status output.
type credential struct {
	origin string
	header string // "Basic base64(user:password)"
}

// heldReq is a request that a route's delay holds. target is the page
// target that paused it, which gets its answer.
type heldReq struct {
	session string
	target  string
	cancel  chan struct{}
}

// reqKey names a paused request: WebKit holds it in the backend of target
// (the event's source target, "" when the connection does not name
// targets), and request ids are unique per target only.
type reqKey struct{ target, id string }

// interceptor answers Network.requestIntercepted for the page's routes,
// offline mode and credentials. Completions go through the control lane
// (design 2.10), so they never wait behind an operation that the request
// itself holds up (an awaited fetch).
type interceptor struct {
	p *Page

	mu      sync.Mutex
	routes  []*Route
	nextID  int
	offline map[string]bool
	creds   map[string][]credential
	held    map[reqKey]*heldReq
	unsub   func()

	// tombs holds the rules that a removal took out of the desired state
	// while a page target may still hold them (tombstones), by the key of
	// their desired-state entry. nextTomb numbers the keys.
	tombs    map[string]core.InterceptRule
	nextTomb int
	// unclaimed rate-limits the log line of the safety net per rule.
	unclaimed map[logsafe.ID]*unclaimedLog
	// ruleIDs numbers the interception rules that the daemon log names
	// (ruleName), in the order the log first names them.
	ruleIDs map[core.InterceptRule]int

	// inbox holds the intercepted events that the read loop delivered and
	// the drain goroutine has not handled yet. It has no bound: every
	// entry is a request that WebKit holds until it gets an answer, so
	// dropping one would stall the page's request until the release. Its
	// size is bounded by the requests the page starts while the drain is
	// behind. inMu is never held while handling an event, so the read loop
	// never waits for mu.
	inMu     sync.Mutex
	inbox    []inboxItem
	draining bool
}

// inboxItem is one intercepted event, its source target and its stage.
type inboxItem struct {
	target string
	stage  string
	params json.RawMessage
}

// Interception stages (WIP Network.NetworkStage).
const (
	stageRequest  = "request"
	stageResponse = "response"
)

func (p *Page) interceptorLocked() *interceptor {
	if p.icpt == nil {
		ic := &interceptor{
			p: p, offline: map[string]bool{}, creds: map[string][]credential{}, held: map[reqKey]*heldReq{},
			tombs: map[string]core.InterceptRule{}, unclaimed: map[logsafe.ID]*unclaimedLog{}, ruleIDs: map[core.InterceptRule]int{},
		}
		ic.unsub = ic.subscribe(p.ps.Caller)
		p.icpt = ic
	}
	return p.icpt
}

// subscribe delivers Network.requestIntercepted and responseIntercepted
// to the interceptor without loss and after the scheduler has recorded the
// request. The scheduler records each intercepted request in a read-loop
// subscriber of those methods, and refuses a completion it has not
// recorded. A read-loop subscriber of webkit.AllEvents runs after every
// read-loop subscriber of the event's own method (webkit.Session
// dispatch), so the interceptor subscribes there and only appends the
// event to its inbox; a goroutine handles the inbox in order. A queued
// subscriber would keep the order but drops events when its bounded queue
// is full. A caller without read-loop subscribers (no daemon page
// connection) gets queued subscribers.
//
// Each event comes with its source target (webkit.TargetEventHandler):
// the target that holds the request, which gets the answer (W4 round 6).
// A provisional target pauses requests before it commits and keeps its id
// when it commits; WebKit refuses a completion that another target gets
// and leaves the request paused.
//
// The same subscriber sees Target.didCommitProvisionalTarget after core's
// own read-loop subscriber of that method made the committed target the
// desired state's active target (DesiredState.followCommits), and applies
// the tombstones to that target (applyTombstones).
func (ic *interceptor) subscribe(c webkit.Caller) func() {
	handle := func(target, method string, params json.RawMessage) {
		switch method {
		case "Network.requestIntercepted":
			ic.enqueue(target, stageRequest, params)
		case "Network.responseIntercepted":
			ic.enqueue(target, stageResponse, params)
		case "Target.didCommitProvisionalTarget":
			var v struct {
				NewTargetID string `json:"newTargetId"`
			}
			if json.Unmarshal(params, &v) == nil && v.NewTargetID != "" {
				go ic.applyTombstones(v.NewTargetID)
			}
		}
	}
	if sub := subscribeTargetInOrder(c); sub != nil {
		return sub(webkit.AllEvents, handle)
	}
	var unsub []func()
	for _, m := range []string{"Network.requestIntercepted", "Network.responseIntercepted", "Target.didCommitProvisionalTarget"} {
		unsub = append(unsub, webkit.SubscribeTarget(c, m, handle))
	}
	return func() {
		for _, u := range unsub {
			u()
		}
	}
}

// enqueue appends an intercepted event to the inbox and starts the drain
// goroutine when none runs. It never blocks on the interceptor's mu.
func (ic *interceptor) enqueue(target, stage string, params json.RawMessage) {
	ic.inMu.Lock()
	ic.inbox = append(ic.inbox, inboxItem{target, stage, params})
	start := !ic.draining
	ic.draining = true
	ic.inMu.Unlock()
	if start {
		go ic.drain()
	}
}

// drain handles the inbox in arrival order until it is empty.
func (ic *interceptor) drain() {
	for {
		ic.inMu.Lock()
		batch := ic.inbox
		ic.inbox = nil
		if len(batch) == 0 {
			ic.draining = false
			ic.inMu.Unlock()
			return
		}
		ic.inMu.Unlock()
		for _, it := range batch {
			ic.intercepted(it.target, it.stage, it.params)
		}
	}
}

type interceptedRequest struct {
	RequestID string `json:"requestId"`
	Request   struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers"`
	} `json:"request"`
	Response struct {
		URL string `json:"url"`
	} `json:"response"`
}

// intercepted runs in the drain goroutine after the scheduler recorded
// the request. It is the one place that decides whether a handler claims
// an intercepted request or the request is continued (W4 repair round 4,
// the safety net): every request that WebKit pauses gets exactly one
// answer, from its claimant or from here. A request is claimed by
//   - a handler of the lease holder: a WIP route, offline mode or an
//     origin's credentials (claim); or
//   - an owner outside internal/devtools whose desired-state rule matches
//     it (a legacy command, a CDP client), which answers it itself
//     (claimedElsewhere).
//
// Any other request is continued unchanged at its stage: a rule that no
// handler serves paused it, for example a rule that a provisional target
// kept after it refused the forwarded removal and then committed, also
// after the lease ended. Every completion runs in its own goroutine and
// goes to target, the event's source target, which holds the request.
func (ic *interceptor) intercepted(target, stage string, params json.RawMessage) {
	var ev interceptedRequest
	if json.Unmarshal(params, &ev) != nil || ev.RequestID == "" {
		return
	}
	u := ev.Request.URL
	if stage == stageResponse {
		u = ev.Response.URL
	}
	if (stage == stageRequest && ic.claim(target, ev)) || ic.claimedElsewhere(stage, u) {
		return
	}
	ic.noteUnclaimed(stage, u)
	go ic.continueUnclaimed(target, ev.RequestID, stage)
}

// claim starts the answer of the lease holder's handler that matches the
// request ev, which target holds, if any, and reports whether one did.
func (ic *interceptor) claim(target string, ev interceptedRequest) bool {
	session, _ := ic.p.ps.Sched.Lease()
	ic.mu.Lock()
	defer ic.mu.Unlock()
	for _, r := range ic.routes {
		if r.session == session && r.Mode == ModeWIP && r.re.MatchString(ev.Request.URL) {
			h := &heldReq{session: session, target: target, cancel: make(chan struct{})}
			ic.held[reqKey{target, ev.RequestID}] = h
			go ic.answerRoute(ev, r, h)
			return true
		}
	}
	if ic.offline[session] {
		go ic.complete(session, target, ev.RequestID, "Network.interceptRequestWithError", map[string]any{"errorType": "General"})
		return true
	}
	if o := originOf(ev.Request.URL); o != "" {
		for _, c := range ic.creds[session] {
			if c.origin == o {
				h := map[string]string{}
				for k, v := range ev.Request.Headers {
					if !strings.EqualFold(k, "Authorization") {
						h[k] = v
					}
				}
				h["Authorization"] = c.header
				go ic.complete(session, target, ev.RequestID, "Network.interceptWithRequest", map[string]any{"headers": h})
				return true
			}
		}
	}
	return false
}

// devtoolsRuleKinds are the owner kinds whose interception rules the
// interceptor's handlers serve (claim).
var devtoolsRuleKinds = map[string]bool{"route": true, "offline": true, "credentials": true}

// IsHandlerOwner reports whether the interception rules of o are served by
// the interceptor's handlers (a route, offline mode or credentials of a
// session), which claim the requests those rules pause.
func IsHandlerOwner(o core.Owner) bool { return isDevtoolsOwner(o) }

// RuleMatches reports whether WebKit's rule r pauses a request for rawURL
// at stage, and whether Go can decide it (see ruleMatches).
func RuleMatches(r core.InterceptRule, stage, rawURL string) (match, known bool) {
	return ruleMatches(r, stage, rawURL)
}

func isDevtoolsOwner(o core.Owner) bool {
	kind, _, _ := strings.Cut(string(o), ":")
	return devtoolsRuleKinds[kind]
}

// claimedElsewhere reports whether a desired-state rule of an owner outside
// internal/devtools may have paused the request for rawURL at stage: that
// owner answers it. A rule whose match cannot be decided here counts as a
// match, so the interceptor never answers another owner's request.
func (ic *interceptor) claimedElsewhere(stage, rawURL string) bool {
	for o, rules := range ic.p.ps.State.InterceptRules() {
		if isDevtoolsOwner(o) {
			continue
		}
		for _, r := range rules {
			if m, known := ruleMatches(r, stage, rawURL); m || !known {
				return true
			}
		}
	}
	return false
}

// ruleMatches reports whether WebKit's rule r pauses a request for rawURL
// at stage, as InspectorNetworkAgent::shouldIntercept decides it (WebKit
// 7615.3.12.11.2, also WebKitGTK 2.54 and main): the fragment is removed,
// an empty URL is never paused, a rule with an empty url matches every
// URL, and otherwise the rule is a regular expression search (isRegex) or
// the whole URL (ContentSearchUtilities ExactString: "^" + escaped + "$"),
// case-insensitive unless caseSensitive. known is false when Go cannot
// decide the match: a stage WebKit does not have, or an expression that
// Go's RE2 does not read (JavaScript syntax such as lookahead).
func ruleMatches(r core.InterceptRule, stage, rawURL string) (match, known bool) {
	if r.Stage != stageRequest && r.Stage != stageResponse {
		return false, false
	}
	if r.Stage != stage {
		return false, true
	}
	u, _, _ := strings.Cut(rawURL, "#")
	switch {
	case u == "":
		return false, true
	case r.URL == "":
		return true, true
	case !r.IsRegex && r.CaseSensitive:
		return u == r.URL, true
	case !r.IsRegex:
		return strings.EqualFold(u, r.URL), true
	}
	expr := r.URL
	if !r.CaseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return false, false
	}
	return re.MatchString(u), true
}

// continueUnclaimed continues an unclaimed request of target unchanged as
// the daemon's own control step: it needs no lease, so it answers a
// request paused after a release too. A refusal means that someone else
// (a barrier) completed the request first, or that target was destroyed
// with the request.
func (ic *interceptor) continueUnclaimed(target, requestID, stage string) {
	err := ic.p.ps.Sched.ContinueUnclaimed(context.Background(), target, requestID, stage)
	if err != nil && !errors.Is(err, core.ErrControlRefused) {
		ic.p.logf("continuing unclaimed intercepted request %s of page target %q (%s stage): %v", logsafe.ID(requestID), logsafe.ID(target), logsafe.ID(stage), logsafe.Error(err))
	}
}

// unclaimedLogEvery is the shortest time between two log lines of the
// safety net for one rule.
const unclaimedLogEvery = time.Minute

// unclaimedLog is the rate limit state of one rule's log line.
type unclaimedLog struct {
	last       time.Time
	suppressed int
}

// ruleName is the name of rule r in the daemon log and in errors that can
// reach it: a number that the interceptor gives each rule the first time
// it names it, and the stage. It never holds the rule's URL pattern: an
// exact rule can be a whole URL with a token or a password in it (W4 lib
// round-4 review, P1). The number is the same in every line about r.
func (ic *interceptor) ruleName(r core.InterceptRule) logsafe.ID {
	ic.mu.Lock()
	id, ok := ic.ruleIDs[r]
	if !ok {
		id = len(ic.ruleIDs) + 1
		ic.ruleIDs[r] = id
	}
	ic.mu.Unlock()
	return logsafe.ID(fmt.Sprintf("interception rule #%d (%s stage)", id, r.Stage))
}

// noteUnclaimed logs that a rule paused a request that no handler claims,
// at most one line per rule per unclaimedLogEvery, to help find the rule.
// The rule is a tombstone or a devtools rule of the desired state that
// matches (a handler that is gone), else one the daemon does not hold.
// The line names the rule by ruleName, never by its URL pattern, and never
// names the request's URL: both can carry secrets.
func (ic *interceptor) noteUnclaimed(stage, rawURL string) {
	var rule logsafe.ID = "a rule that the daemon does not hold"
	var candidates []core.InterceptRule
	ic.mu.Lock()
	for _, r := range ic.tombs {
		candidates = append(candidates, r)
	}
	ic.mu.Unlock()
	for o, rules := range ic.p.ps.State.InterceptRules() {
		if isDevtoolsOwner(o) {
			candidates = append(candidates, rules...)
		}
	}
	for _, r := range candidates {
		if m, _ := ruleMatches(r, stage, rawURL); m {
			rule = ic.ruleName(r)
			break
		}
	}
	now := time.Now()
	ic.mu.Lock()
	l := ic.unclaimed[rule]
	if l == nil {
		l = &unclaimedLog{}
		ic.unclaimed[rule] = l
	}
	if !l.last.IsZero() && now.Sub(l.last) < unclaimedLogEvery {
		l.suppressed++
		ic.mu.Unlock()
		return
	}
	n := l.suppressed
	l.last, l.suppressed = now, 0
	ic.mu.Unlock()
	ic.p.logf("continued an intercepted request unchanged: no handler claims it; it was paused by %s (%d more since the last line for this rule)", rule, n)
}

func (ic *interceptor) answerRoute(ev interceptedRequest, r *Route, h *heldReq) {
	continued := false
	if r.Delay > 0 {
		t := time.NewTimer(r.Delay)
		select {
		case <-t.C:
		case <-h.cancel:
			t.Stop()
			continued = true // unroute: continue unmodified
		}
	}
	ic.mu.Lock()
	if k := (reqKey{h.target, ev.RequestID}); ic.held[k] == h {
		delete(ic.held, k)
	}
	ic.mu.Unlock()
	switch {
	case continued || (!r.Abort && !r.fulfills()):
		ic.complete(h.session, h.target, ev.RequestID, "Network.interceptContinue", map[string]any{"stage": "request"})
	case r.Abort:
		ic.complete(h.session, h.target, ev.RequestID, "Network.interceptRequestWithError", map[string]any{"errorType": "General"})
	default:
		status := r.Status
		if status == 0 {
			status = http.StatusOK
		}
		body := ""
		if r.Body != nil {
			body = *r.Body
		}
		ct := r.ContentType
		if ct == "" {
			ct = "text/plain"
		}
		headers := map[string]string{"Content-Type": ct}
		for k, v := range r.Headers {
			headers[k] = v
		}
		ic.complete(h.session, h.target, ev.RequestID, "Network.interceptRequestWithResponse", map[string]any{
			"content": base64.StdEncoding.EncodeToString([]byte(body)), "base64Encoded": true,
			"mimeType": ct, "status": status, "statusText": http.StatusText(status), "headers": headers,
		})
	}
}

// complete sends a completion of the request requestID that target holds
// on the control lane under the current lease epoch of session, to target
// (ControlMsg.TargetID). A refusal (the lease ended, a release already
// continued the request, target was destroyed with it) needs no further
// action.
func (ic *interceptor) complete(session, target, requestID, method string, params map[string]any) {
	holder, epoch := ic.p.ps.Sched.Lease()
	if holder != session {
		return // the lease ended: its release continued the request
	}
	_, err := ic.p.ps.Sched.Control(context.Background(), core.ControlMsg{
		Kind:    core.ControlCompleteRequest,
		Session: session, Epoch: epoch, RequestID: requestID, TargetID: target, Method: method, Params: params,
	})
	if err != nil && !errors.Is(err, core.ErrControlRefused) {
		ic.p.logf("completing intercepted request %s of page target %q with %s: %v", logsafe.ID(requestID), logsafe.ID(target), logsafe.ID(method), logsafe.Error(err))
	}
}

// continueHeld continues the requests held by session's routes now (a
// delay in progress): "network unroute" and release (design 4.2).
func (ic *interceptor) continueHeld(session string) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	for k, h := range ic.held {
		if h.session == session {
			close(h.cancel)
			delete(ic.held, k)
		}
	}
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// interceptionUnsupported says why WIP interception cannot be used on the
// page's device, or "" when it can (unknown versions are not gated).
func (p *Page) interceptionUnsupported(methods ...string) string {
	for _, m := range append([]string{"Network.setInterceptionEnabled", "Network.addInterception"}, methods...) {
		if known, ok := wip.Supported(p.ps.Device.OSVersion, m); known && !ok {
			return fmt.Sprintf("%s is not in the iOS %s command list", m, p.ps.Device.OSVersion)
		}
	}
	return ""
}

// noEffect reports a failed call that provably changed nothing on the
// page: it was never written (webkit.ErrNotSent), or the page answered it
// with a protocol error. Any other failure (a lost reply, a cancellation
// after the write) leaves the outcome unknown. (core's noEffect, which is
// not exported.)
func noEffect(err error) bool {
	var pe *webkit.ErrorData
	return errors.Is(err, webkit.ErrNotSent) || errors.As(err, &pe)
}

// addRule makes o an interception owner with rule r: Network enabled,
// interception on, the rule added (design 2.4 desired state). It reports
// whether the page may hold the rule: true on success and when the
// outcome of Network.addInterception is unknown, which keeps the rule
// owned (the release removes it). The caller keeps the local handler of
// the rule while the page may hold it, so that every request the rule
// pauses gets an answer (W4 round 3, review finding intercept.go:652).
func (p *Page) addRule(ctx context.Context, c webkit.Caller, o core.Owner, r core.InterceptRule) (installed bool, err error) {
	if err := acquireDomain(ctx, p.ps, c, o, "Network"); err != nil {
		return false, err
	}
	if v := p.ps.State.SetInterception(o, true); v != nil {
		if _, err := c.Send(ctx, "Network.setInterceptionEnabled", map[string]bool{"enabled": *v}); err != nil {
			return false, fmt.Errorf("Network.setInterceptionEnabled: %w", err)
		}
	}
	p.ps.State.AddInterception(o, r)
	if _, err := c.Send(ctx, "Network.addInterception", r); err != nil {
		if !noEffect(err) {
			return true, fmt.Errorf("Network.addInterception (the page may hold the rule until the lease ends): %w", err)
		}
		p.ps.State.RemoveInterception(o, r)
		return false, fmt.Errorf("Network.addInterception: %w", err)
	}
	return true, nil
}

// removeRule removes r of o; with no rule of o left, o stops needing
// interception, and interception is turned off when nobody needs it. It
// first records a tombstone of r in ic (putTombstone) and returns its key:
// the caller drops the tombstone (dropTombstones) when its change
// succeeded, that is, when every page target took the removal.
func (p *Page) removeRule(ctx context.Context, c webkit.Caller, ic *interceptor, o core.Owner, r core.InterceptRule, ownerHasMore bool) (tomb string, err error) {
	tomb = ic.putTombstone(o, r)
	p.ps.State.RemoveInterception(o, r)
	if _, err := c.Send(ctx, "Network.removeInterception", r); err != nil {
		return tomb, fmt.Errorf("Network.removeInterception: %w", err)
	}
	return tomb, p.ruleRemoved(ctx, c, o, ownerHasMore)
}

// ruleRemoved ends o's interception when o has no rule left.
func (p *Page) ruleRemoved(ctx context.Context, c webkit.Caller, o core.Owner, ownerHasMore bool) error {
	if !ownerHasMore {
		if v := p.ps.State.SetInterception(o, false); v != nil {
			if _, err := c.Send(ctx, "Network.setInterceptionEnabled", map[string]bool{"enabled": *v}); err != nil {
				return fmt.Errorf("Network.setInterceptionEnabled: %w", err)
			}
		}
		return releaseDomain(ctx, p.ps, c, o, "Network")
	}
	return nil
}

func wipRule(regex string) core.InterceptRule {
	return core.InterceptRule{URL: regex, Stage: "request", CaseSensitive: true, IsRegex: true}
}

// AddRoute adds a route of session (design 4.2 "network route"). In WIP
// mode the rule is an owned interception rule (owner "route:<session>"),
// so a provisional target gets it before commit (2.7) and a release
// removes it; in JS mode the rules are the bootstrap part
// "routes:<session>" and are installed in the current document too.
func (p *Page) AddRoute(ctx context.Context, c webkit.Caller, session string, r Route) (Route, error) {
	re, err := PatternRegex(r.Pattern)
	if err != nil {
		return Route{}, err
	}
	r.Regex, r.re, r.session = re, regexp.MustCompile(re), session
	if r.Mode == "" {
		r.Mode = ModeWIP
	}
	if r.Abort && r.fulfills() {
		return Route{}, fmt.Errorf("network route: --abort cannot be combined with --body or --status")
	}
	if r.Mode == ModeWIP {
		if why := p.interceptionUnsupported("Network.interceptRequestWithResponse"); why != "" {
			return Route{}, fmt.Errorf("network route: %s; use --mode js", why)
		}
	}
	p.mu.Lock()
	ic := p.interceptorLocked()
	p.mu.Unlock()
	ic.mu.Lock()
	ic.nextID++
	r.ID = ic.nextID
	ic.routes = append(ic.routes, &r)
	ic.mu.Unlock()
	// installed: the page may hold the WIP rule (addRule) or run the JS
	// rules (syncJSRoutes). Then the route stays, also when the change
	// fails (a provisional target refused the forwarded rule, or a reply
	// was lost), so that the requests the rule pauses get their answer
	// and the release clears the JS rules of the current document;
	// "network unroute" or the release removes it.
	installed := false
	err = p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		p.ensureRouteEntry(session)
		switch r.Mode {
		case ModeWIP:
			var aerr error
			installed, aerr = p.addRule(ctx, c, owner("route", session), wipRule(re))
			return aerr
		case ModeJS:
			var serr error
			installed, serr = p.syncJSRoutes(ctx, c, session)
			return serr
		}
		return fmt.Errorf("network route: mode %q: want wip or js", r.Mode)
	})
	if err != nil && installed {
		return Route{}, fmt.Errorf("network route %s: %w; the route stays active until 'network unroute' or the end of the lease", r.Pattern, err)
	}
	if err != nil {
		ic.mu.Lock()
		ic.removeRouteLocked(r.ID)
		ic.mu.Unlock()
		return Route{}, err
	}
	return r, nil
}

func (ic *interceptor) removeRouteLocked(id int) {
	for i, x := range ic.routes {
		if x.ID == id {
			ic.routes = append(ic.routes[:i:i], ic.routes[i+1:]...)
			return
		}
	}
}

// Routes returns the routes of session.
func (p *Page) Routes(session string) []Route {
	p.mu.Lock()
	ic := p.icpt
	p.mu.Unlock()
	if ic == nil {
		return nil
	}
	ic.mu.Lock()
	defer ic.mu.Unlock()
	var out []Route
	for _, r := range ic.routes {
		if r.session == session {
			out = append(out, *r)
		}
	}
	return out
}

// RemoveRoutes removes the routes of session whose pattern is pattern (all
// when pattern is ""), continues the requests their delays hold, and
// returns how many it removed (design 4.2 "network unroute").
func (p *Page) RemoveRoutes(ctx context.Context, c webkit.Caller, session, pattern string) (int, error) {
	p.mu.Lock()
	ic := p.icpt
	p.mu.Unlock()
	if ic == nil {
		return 0, nil
	}
	ic.mu.Lock()
	var gone []*Route
	var keep []*Route
	for _, r := range ic.routes {
		if r.session == session && (pattern == "" || r.Pattern == pattern) {
			gone = append(gone, r)
		} else {
			keep = append(keep, r)
		}
	}
	ic.routes = keep
	wipLeft, jsChanged := 0, false
	for _, r := range keep {
		if r.session == session && r.Mode == ModeWIP {
			wipLeft++
		}
	}
	ic.mu.Unlock()
	ic.continueHeld(session)
	var tombs []string
	err := p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		var firstErr error
		o := owner("route", session)
		for i, r := range gone {
			if r.Mode == ModeJS {
				jsChanged = true
				continue
			}
			more := wipLeft > 0
			for _, later := range gone[i+1:] {
				more = more || later.Mode == ModeWIP
			}
			tomb, err := p.removeRule(ctx, c, ic, o, wipRule(r.Regex), more)
			tombs = append(tombs, tomb)
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if jsChanged {
			if _, err := p.syncJSRoutes(ctx, c, session); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	})
	if err == nil {
		ic.dropTombstones(tombs)
	}
	return len(gone), err
}

// ensureRouteEntry ties the routes of session to its lease: the release
// drops them and continues what their delays hold. The WIP rules and the
// JS part are released by the desired state itself.
func (p *Page) ensureRouteEntry(session string) {
	o := owner("route", session)
	p.ps.State.Put(core.Entry{
		Key: "devtools:route:" + session, Owner: o, Apply: noop,
		Release: func(ctx context.Context, s core.Sender) error {
			p.mu.Lock()
			ic := p.icpt
			p.mu.Unlock()
			if ic == nil {
				return nil
			}
			ic.mu.Lock()
			var keep []*Route
			js := false
			for _, r := range ic.routes {
				if r.session != session {
					keep = append(keep, r)
				} else if r.Mode == ModeJS {
					js = true
				}
			}
			ic.routes = keep
			ic.mu.Unlock()
			ic.continueHeld(session)
			if js {
				// The bootstrap part goes with the owner; the current
				// document's patch stops matching.
				_, err := s.Send(ctx, "Runtime.evaluate", map[string]any{"expression": jsRoutesClear, "returnByValue": true})
				return err
			}
			return nil
		},
	})
}

// SetOffline turns offline mode of session on or off: every request fails
// through interception (design 4.2 "set offline"; unverified on a device).
func (p *Page) SetOffline(ctx context.Context, c webkit.Caller, session string, on bool) error {
	if why := p.interceptionUnsupported("Network.interceptRequestWithError"); why != "" {
		return fmt.Errorf("set offline: unsupported: %s", why)
	}
	p.mu.Lock()
	ic := p.interceptorLocked()
	p.mu.Unlock()
	o := owner("offline", session)
	r := wipRule(".*")
	// The flag changes before the rule, so that no request that the new
	// rule holds finds the old flag (it would be left held). A failed rule
	// change restores the old flag, so that a retry changes the rule again
	// and success always means that the rule is in place; but a rule that
	// the page may hold keeps the flag on, so that the requests it pauses
	// get their answer (W4 round 3, review finding intercept.go:652).
	ic.mu.Lock()
	was := ic.offline[session]
	ic.offline[session] = on
	ic.mu.Unlock()
	if was == on {
		return nil
	}
	installed := false
	var tomb string
	// removeErr is the off path's own failure on the change's target; a
	// failure that only a provisional target reported leaves the active
	// target without the rule, and the tombstone removes it from that
	// target if it commits.
	var removeErr error
	err := p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		if !on {
			tomb, removeErr = p.removeRule(ctx, c, ic, o, r, false)
			return removeErr
		}
		// The entry stays after a failure: it ties what addRule acquired
		// (Network, interception) to the release of the session.
		p.ps.State.Put(core.Entry{
			Key: "devtools:offline:" + session, Owner: o, Apply: noop,
			Release: func(context.Context, core.Sender) error {
				ic.mu.Lock()
				delete(ic.offline, session)
				ic.mu.Unlock()
				return nil
			},
		})
		var aerr error
		installed, aerr = p.addRule(ctx, c, o, r)
		return aerr
	})
	if err != nil && installed {
		return fmt.Errorf("set offline: %w; offline mode stays on until 'set offline' turns it off or the lease ends", err)
	}
	if !on && err == nil {
		ic.dropTombstones([]string{tomb})
	}
	if err != nil && (on || removeErr != nil) {
		ic.mu.Lock()
		ic.offline[session] = was
		ic.mu.Unlock()
	}
	return err
}

// credentialsVerified lists the transports on which a test has shown that
// origin-scoped credentials never reach another origin: subresources of
// other origins and cross-origin redirects carry no Authorization header
// (design 4.2 "set credentials", 7.7 test 7). WebKitGTK is checked by
// e2e/webkitgtk TestCredentialsStayOnTheirOrigin; a device is added only
// after the runbook in docs/notes/w4.md passes on it.
var credentialsVerified = map[transport.Kind]bool{transport.KindWebKitGTK: true}

// ErrUnsupported marks a command that this page's transport cannot run.
var ErrUnsupported = errors.New("unsupported")

// SetCredentials adds HTTP Basic credentials for one origin of session:
// an interception rule for that origin adds the Authorization header to
// its requests only. The password is never logged or returned.
func (p *Page) SetCredentials(ctx context.Context, c webkit.Caller, session, origin, user, password string) (string, error) {
	o, err := normalizeOrigin(origin)
	if err != nil {
		return "", err
	}
	if !credentialsVerified[p.ps.Device.Transport] {
		return "", fmt.Errorf("set credentials: %w on transport %q: no test has shown yet that WebKit there keeps the Authorization header off other origins and cross-origin redirects (see the device runbook in docs/notes/w4.md)", ErrUnsupported, p.ps.Device.Transport)
	}
	if why := p.interceptionUnsupported("Network.interceptWithRequest"); why != "" {
		return "", fmt.Errorf("set credentials: %w: %s", ErrUnsupported, why)
	}
	if strings.Contains(user, ":") {
		return "", fmt.Errorf("set credentials: the user name must not contain ':' (HTTP Basic)")
	}
	cred := credential{origin: o, header: "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))}
	p.mu.Lock()
	ic := p.interceptorLocked()
	p.mu.Unlock()
	ic.mu.Lock()
	replaced := false
	for i, x := range ic.creds[session] {
		if x.origin == o {
			ic.creds[session][i], replaced = cred, true
		}
	}
	if !replaced {
		ic.creds[session] = append(ic.creds[session], cred)
	}
	ic.mu.Unlock()
	ow := owner("credentials", session)
	// installed: the page may hold the origin's rule (addRule; a replaced
	// credential's rule is in place already). Then the credential stays,
	// also when the change fails (a provisional target refused the
	// forwarded rule), so that the requests the rule pauses get their
	// answer (W4 round 3, review finding intercept.go:652).
	installed := replaced
	err = p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		p.ps.State.Put(core.Entry{
			Key: "devtools:credentials:" + session, Owner: ow, Apply: noop,
			Release: func(context.Context, core.Sender) error {
				ic.mu.Lock()
				delete(ic.creds, session)
				ic.mu.Unlock()
				return nil
			},
		})
		if replaced {
			return nil
		}
		var aerr error
		installed, aerr = p.addRule(ctx, c, ow, wipRule("^"+regexp.QuoteMeta(o)+"/"))
		return aerr
	})
	if err != nil && installed {
		return "", fmt.Errorf("set credentials for %s: %w; the credentials stay in place until they are cleared or the lease ends", o, err)
	}
	if err != nil {
		ic.mu.Lock()
		creds := ic.creds[session][:0]
		for _, x := range ic.creds[session] {
			if x.origin != o {
				creds = append(creds, x)
			}
		}
		ic.creds[session] = creds
		ic.mu.Unlock()
		return "", err
	}
	return o, nil
}

// ClearCredentials removes every credential of session.
func (p *Page) ClearCredentials(ctx context.Context, c webkit.Caller, session string) (int, error) {
	p.mu.Lock()
	ic := p.icpt
	p.mu.Unlock()
	if ic == nil {
		return 0, nil
	}
	ic.mu.Lock()
	creds := ic.creds[session]
	delete(ic.creds, session)
	ic.mu.Unlock()
	var tombs []string
	err := p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		var firstErr error
		for i, cr := range creds {
			tomb, err := p.removeRule(ctx, c, ic, owner("credentials", session), wipRule("^"+regexp.QuoteMeta(cr.origin)+"/"), i < len(creds)-1)
			tombs = append(tombs, tomb)
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	})
	if err == nil {
		ic.dropTombstones(tombs)
	}
	return len(creds), err
}

func normalizeOrigin(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("set credentials: --origin %q must be an origin such as https://example.com or http://127.0.0.1:8080", s)
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}
