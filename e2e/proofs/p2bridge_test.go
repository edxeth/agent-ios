//go:build webkitgtk

package proofs

// The minimal CDP bridge of proof P2 (design docs/design/agentic-parity.md,
// sections 5.1, 5.2 and 7.1). It serves one WebKitGTK page through one
// webkit.Session: a tab target with one page target, filtered auto-attach,
// world-name aliases of the main world, Runtime.evaluate and callFunctionOn,
// DOM.describeNode and DOM.resolveNode through page-side handle numbers, and
// Performance.enable. Every CDP method it receives is logged with its outcome.
// It is proof code: W5 replaces it with internal/cdp and deletes this file.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// wipChunkRunes bounds one chunk of an expression that is too large for one
// WebKitGTK message. The chunk is escaped twice on the wire (JSON string
// literal inside the inner message, inner message inside the outer one), so
// 8000 runes stay below the 127 KiB limit even when every rune is escaped.
const wipChunkRunes = 8000

// mainContextID is the CDP id of the main world of the current document. Alias
// ids come from a counter that starts above it and never repeats.
const mainContextID = 1

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return fmt.Sprintf("%d %s", e.Code, e.Message) }

func notFound(method string) *cdpError {
	return &cdpError{Code: -32601, Message: fmt.Sprintf("'%s' wasn't found", method)}
}

func serverError(format string, args ...any) *cdpError {
	return &cdpError{Code: -32000, Message: fmt.Sprintf(format, args...)}
}

// p2Bridge serves one page.
type p2Bridge struct {
	wip      *webkit.Session
	token    string
	endpoint string
	addr     string
	srv      *http.Server
	logf     func(format string, args ...any)

	tabID, pageID string

	mu         sync.Mutex
	frameID    string // CDP main frame id: the page targetId (see newP2Bridge)
	loaderID   string
	url, title string
	origin     string
	worlds     map[string]int // world name -> alias context id, current document
	nextCtx    int
	globalObj  string // WIP objectId of globalThis, cached
	nextScript int
	nextConn   int
	chunkSeq   int
	stats      map[string]int // "<conn> <session kind> <method> <outcome>" -> count
	upgrades   []string       // redacted WebSocket paths and their Origin header
	requests   []string       // redacted HTTP requests
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on Linux
	}
	return hex.EncodeToString(b)
}

func stableID(prefix, s string) string {
	sum := sha256.Sum256([]byte(prefix + s))
	return strings.ToUpper(hex.EncodeToString(sum[:16]))
}

// newP2Bridge reads the page's frame and starts the listener on 127.0.0.1.
func newP2Bridge(ctx context.Context, wip *webkit.Session, deviceID, pageID string, logf func(string, ...any)) (*p2Bridge, error) {
	b := &p2Bridge{
		wip: wip, token: randomHex(32), endpoint: randomHex(16), logf: logf,
		tabID: stableID("tab:", deviceID+pageID), pageID: stableID("page:", deviceID+pageID),
		worlds: map[string]int{}, nextCtx: mainContextID + 1, stats: map[string]int{},
	}
	if _, err := wip.Send(ctx, "Page.enable", nil); err != nil {
		return nil, fmt.Errorf("WIP Page.enable: %w", err)
	}
	raw, err := wip.Send(ctx, "Page.getResourceTree", nil)
	if err != nil {
		return nil, fmt.Errorf("WIP Page.getResourceTree: %w", err)
	}
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID             string `json:"id"`
				LoaderID       string `json:"loaderId"`
				URL            string `json:"url"`
				SecurityOrigin string `json:"securityOrigin"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("decoding WIP resource tree %s: %w", raw, err)
	}
	f := tree.FrameTree.Frame
	logf("WIP Page.getResourceTree frame: %s", truncate(string(raw), 400))
	// Playwright requires the main frame id to equal the page targetId, as in
	// Chrome (crPage.ts _sessionForFrame throws "Frame has been detached"
	// otherwise), so the WIP frame id is not exposed.
	b.frameID = b.pageID
	b.loaderID, b.url, b.origin = f.LoaderID, f.URL, f.SecurityOrigin
	title, err := b.wipValue(ctx, "document.title")
	if err != nil {
		return nil, err
	}
	b.title = fmt.Sprint(title)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b.addr = l.Addr().String()
	b.srv = &http.Server{Handler: http.HandlerFunc(b.serveHTTP), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = b.srv.Serve(l) }()
	return b, nil
}

func (b *p2Bridge) Close() { _ = b.srv.Close() }

func (b *p2Bridge) base() string { return "/t/" + b.token + "/e/" + b.endpoint }

// WSURL is the tokenized browser endpoint (the "ws" form of 5.1).
func (b *p2Bridge) WSURL() string {
	return "ws://" + b.addr + b.base() + "/devtools/browser/" + b.endpoint
}

// HTTPURL is the discovery base (the "http" form of 5.1).
func (b *p2Bridge) HTTPURL() string { return "http://" + b.addr + b.base() }

func (b *p2Bridge) redact(s string) string { return strings.ReplaceAll(s, b.token, "<token>") }

func (b *p2Bridge) count(conn, kind, method, outcome string) {
	b.mu.Lock()
	b.stats[conn+" "+kind+" "+method+" "+outcome]++
	b.mu.Unlock()
}

// worldOf names the world a Runtime or DOM call runs in, for the proof's
// evidence that clients use the utility-world alias.
func (b *p2Bridge) worldOf(params json.RawMessage) string {
	var p struct {
		ContextID          int    `json:"contextId"`
		ExecutionContextID int    `json:"executionContextId"`
		ObjectID           string `json:"objectId"`
	}
	_ = json.Unmarshal(params, &p)
	id := max(p.ContextID, p.ExecutionContextID)
	switch {
	case p.ObjectID != "":
		return "on-object"
	case id == 0, id == mainContextID:
		return "main-world"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, v := range b.worlds {
		if v == id {
			return "alias:" + name
		}
	}
	return fmt.Sprintf("unknown-context-%d", id)
}

// Stats returns the method counts, sorted.
func (b *p2Bridge) Stats() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for k, v := range b.stats {
		out = append(out, fmt.Sprintf("%s x%d", k, v))
	}
	sort.Strings(out)
	return out
}

func (b *p2Bridge) Upgrades() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.upgrades...)
}

func (b *p2Bridge) Requests() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.requests...)
}

func (b *p2Bridge) tabInfo(attached bool) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{"targetId": b.tabID, "type": "tab", "title": b.title, "url": b.url, "attached": attached, "canAccessOpener": false, "browserContextId": "default"}
}

func (b *p2Bridge) pageInfo(attached bool) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{"targetId": b.pageID, "type": "page", "title": b.title, "url": b.url, "attached": attached, "canAccessOpener": false, "browserContextId": "default"}
}

func browserInfo() map[string]any {
	return map[string]any{"targetId": "browser", "type": "browser", "title": "", "url": "", "attached": true, "canAccessOpener": false}
}

// ---- HTTP surface (5.1): everything below /t/<token>/e/<endpoint>.

var upgrader = websocket.Upgrader{
	// The bridge logs the Origin header instead of enforcing 5.1, so the
	// proof shows what each client sends.
	CheckOrigin: func(*http.Request) bool { return true },
}

func (b *p2Bridge) serveHTTP(w http.ResponseWriter, r *http.Request) {
	line := fmt.Sprintf("%s %s Host=%q Origin=%q", r.Method, b.redact(r.URL.RequestURI()), r.Host, r.Header.Get("Origin"))
	b.mu.Lock()
	b.requests = append(b.requests, line)
	b.mu.Unlock()
	b.logf("http: %s", line)
	rest, ok := strings.CutPrefix(r.URL.Path, b.base())
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case rest == "/json/version" || rest == "/json/version/":
		writeJSON(w, map[string]any{
			"Browser": "Safari/WebKitGTK (iwdp-mcp P2 proof)", "Protocol-Version": "1.3",
			"User-Agent": "", "webSocketDebuggerUrl": b.WSURL(),
		})
	case rest == "/json" || rest == "/json/" || rest == "/json/list" || rest == "/json/list/":
		info := b.pageInfo(false)
		writeJSON(w, []map[string]any{{
			"id": info["targetId"], "type": "page", "title": info["title"], "url": info["url"],
			"webSocketDebuggerUrl": "ws://" + b.addr + b.base() + "/devtools/page/" + b.pageID,
		}})
	case rest == "/devtools/browser/"+b.endpoint:
		b.mu.Lock()
		b.nextConn++
		n := b.nextConn
		b.upgrades = append(b.upgrades, fmt.Sprintf("conn%d %s Origin=%q", n, b.redact(r.URL.RequestURI()), r.Header.Get("Origin")))
		b.mu.Unlock()
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			b.logf("ws upgrade: %v", err)
			return
		}
		c := &cdpConn{b: b, ws: ws, name: fmt.Sprintf("conn%d", n), sessions: map[string]*cdpSession{}, queues: map[string]chan cdpMsg{}}
		c.run()
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- CDP connection and sessions.

type cdpMsg struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpSession struct {
	id       string
	kind     string // "tab" or "page"
	targetID string
	// guarded by cdpConn.mu
	runtimeEnabled bool
	attachedPage   bool // tab session: its page is attached
}

type cdpConn struct {
	b    *p2Bridge
	ws   *websocket.Conn
	name string
	wmu  sync.Mutex

	mu            sync.Mutex
	sessions      map[string]*cdpSession
	queues        map[string]chan cdpMsg
	discover      bool
	browserAttach bool
	closed        bool
	wg            sync.WaitGroup
}

func (c *cdpConn) run() {
	c.b.logf("%s: connected", c.name)
	defer func() {
		c.mu.Lock()
		c.closed = true
		for _, q := range c.queues {
			close(q)
		}
		c.mu.Unlock()
		c.wg.Wait()
		_ = c.ws.Close()
		c.b.logf("%s: closed", c.name)
	}()
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var m cdpMsg
		if err := json.Unmarshal(data, &m); err != nil {
			c.b.logf("%s: malformed message %q", c.name, truncate(string(data), 200))
			continue
		}
		c.enqueue(m)
	}
}

// enqueue keeps the order of one session's messages, as Chrome does: for
// example a client's Page.getFrameTree reply comes before the context events
// of its later Runtime.enable.
func (c *cdpConn) enqueue(m cdpMsg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	q, ok := c.queues[m.SessionID]
	if !ok {
		q = make(chan cdpMsg, 256)
		c.queues[m.SessionID] = q
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			for m := range q {
				c.dispatch(m)
			}
		}()
	}
	q <- m
}

func (c *cdpConn) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		c.b.logf("%s: encoding %v: %v", c.name, v, err)
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.WriteMessage(websocket.TextMessage, data)
}

func (c *cdpConn) event(sessionID, method string, params any) {
	m := map[string]any{"method": method, "params": params}
	if sessionID != "" {
		m["sessionId"] = sessionID
	}
	c.b.logf("%s: event %s session=%s", c.name, method, short(sessionID))
	c.write(m)
}

func (c *cdpConn) reply(m cdpMsg, kind string, result any, err error) {
	out := map[string]any{"id": m.ID}
	if m.SessionID != "" {
		out["sessionId"] = m.SessionID
	}
	outcome := "ok"
	if err != nil {
		var ce *cdpError
		if !errors.As(err, &ce) {
			ce = serverError("%v", err)
		}
		out["error"] = ce
		outcome = "error"
		if ce.Code == -32601 {
			outcome = "unknown"
		}
		c.b.logf("%s: %s %s session=%s -> %s", c.name, kind, m.Method, short(m.SessionID), ce.Message)
	} else {
		if result == nil {
			result = map[string]any{}
		}
		out["result"] = result
	}
	c.b.count(c.name, kind, m.Method, outcome)
	c.write(out)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// stubbed methods answer success with no effect. Each one is listed in the
// proof notes as a method the minimal shape did not plan.
var stubbed = map[string]bool{
	"Page.enable": true, "Page.setLifecycleEventsEnabled": true, "Log.enable": true, "Network.enable": true,
	"Runtime.runIfWaitingForDebugger": true, "Performance.enable": true, "Runtime.addBinding": true,
	"Emulation.setFocusEmulationEnabled": true, "Page.setInterceptFileChooserDialog": true,
	"Browser.setDownloadBehavior": true, "Target.activateTarget": true, "Page.bringToFront": true,
	"Emulation.setEmulatedMedia": true, "Audits.enable": true,
}

func (c *cdpConn) dispatch(m cdpMsg) {
	kind := "browser"
	var s *cdpSession
	if m.SessionID != "" {
		c.mu.Lock()
		s = c.sessions[m.SessionID]
		c.mu.Unlock()
		if s == nil {
			c.reply(m, "unknown-session", nil, serverError("Session with given id not found."))
			return
		}
		kind = s.kind
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	result, async, err := c.handle(ctx, m, s)
	if async != nil {
		go func() {
			defer cancel()
			r, err := async(ctx)
			c.reply(m, kind, r, err)
		}()
		return
	}
	cancel()
	c.reply(m, kind, result, err)
}

type asyncStep func(ctx context.Context) (any, error)

func (c *cdpConn) handle(ctx context.Context, m cdpMsg, s *cdpSession) (any, asyncStep, error) {
	if stubbed[m.Method] {
		if m.Method == "Page.setLifecycleEventsEnabled" && s != nil && s.kind == "page" {
			c.replayLifecycle(s)
		}
		return nil, nil, nil
	}
	switch m.Method {
	case "Browser.getVersion":
		ua, _ := c.b.wipValue(ctx, "navigator.userAgent")
		return map[string]any{"protocolVersion": "1.3", "product": "Safari/WebKitGTK (iwdp-mcp P2 proof)", "revision": "", "userAgent": fmt.Sprint(ua), "jsVersion": ""}, nil, nil
	case "Browser.close":
		// Detach only (5.3): the browser is not closed.
		return nil, nil, nil
	case "Target.getBrowserContexts":
		return map[string]any{"browserContextIds": []string{}}, nil, nil
	case "Target.setDiscoverTargets":
		return nil, nil, c.setDiscoverTargets(m.Params)
	case "Target.getTargets":
		var p struct {
			Filter *[]targetFilter `json:"filter"`
		}
		_ = json.Unmarshal(m.Params, &p)
		var infos []map[string]any
		for _, t := range []string{"tab", "page"} {
			if filterAllows(p.Filter, t) {
				infos = append(infos, c.targetInfo(t))
			}
		}
		return map[string]any{"targetInfos": infos}, nil, nil
	case "Target.getTargetInfo":
		var p struct {
			TargetID string `json:"targetId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		switch {
		case p.TargetID == "" && s == nil, p.TargetID == "browser":
			return map[string]any{"targetInfo": browserInfo()}, nil, nil
		case p.TargetID == "":
			return map[string]any{"targetInfo": c.targetInfo(s.kind)}, nil, nil
		case p.TargetID == c.b.tabID:
			return map[string]any{"targetInfo": c.targetInfo("tab")}, nil, nil
		case p.TargetID == c.b.pageID:
			return map[string]any{"targetInfo": c.targetInfo("page")}, nil, nil
		}
		return nil, nil, serverError("No target with given id found")
	case "Target.setAutoAttach":
		return nil, nil, c.setAutoAttach(m, s)
	case "Target.attachToTarget":
		var p struct {
			TargetID string `json:"targetId"`
			Flatten  bool   `json:"flatten"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if !p.Flatten {
			return nil, nil, serverError("only flatten:true sessions are supported")
		}
		var t string
		switch p.TargetID {
		case c.b.tabID:
			t = "tab"
		case c.b.pageID:
			t = "page"
		default:
			return nil, nil, serverError("No target with given id found")
		}
		ns := c.attach(m.SessionID, t)
		return map[string]any{"sessionId": ns.id}, nil, nil
	case "Target.detachFromTarget":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		ds := c.sessions[p.SessionID]
		delete(c.sessions, p.SessionID)
		c.mu.Unlock()
		if ds == nil {
			return nil, nil, serverError("No session with given id")
		}
		c.event(m.SessionID, "Target.detachedFromTarget", map[string]any{"sessionId": ds.id, "targetId": ds.targetID})
		return nil, nil, nil
	}
	if s == nil || s.kind != "page" {
		return nil, nil, notFound(m.Method)
	}
	switch m.Method {
	case "Page.getFrameTree":
		return map[string]any{"frameTree": map[string]any{"frame": c.b.frame()}}, nil, nil
	case "Page.createIsolatedWorld":
		var p struct {
			FrameID   string `json:"frameId"`
			WorldName string `json:"worldName"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.FrameID != c.b.frameID {
			return nil, nil, serverError("No frame for given id found")
		}
		id, created := c.b.world(p.WorldName)
		c.mu.Lock()
		enabled := s.runtimeEnabled
		c.mu.Unlock()
		if enabled && created {
			c.contextCreated(s, id, p.WorldName)
		}
		return map[string]any{"executionContextId": id}, nil, nil
	case "Page.addScriptToEvaluateOnNewDocument":
		var p struct {
			Source    string `json:"source"`
			WorldName string `json:"worldName"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.b.mu.Lock()
		c.b.nextScript++
		n := c.b.nextScript
		c.b.mu.Unlock()
		c.b.logf("%s: addScriptToEvaluateOnNewDocument world=%q source=%q (registered, not run: the proof does not navigate)", c.name, p.WorldName, truncate(p.Source, 80))
		return map[string]any{"identifier": fmt.Sprint(n)}, nil, nil
	case "Runtime.enable":
		c.mu.Lock()
		s.runtimeEnabled = true
		c.mu.Unlock()
		c.contextCreated(s, mainContextID, "")
		c.b.mu.Lock()
		type w struct {
			name string
			id   int
		}
		var ws []w
		for name, id := range c.b.worlds {
			ws = append(ws, w{name, id})
		}
		c.b.mu.Unlock()
		sort.Slice(ws, func(i, j int) bool { return ws[i].id < ws[j].id })
		for _, x := range ws {
			c.contextCreated(s, x.id, x.name)
		}
		return nil, nil, nil
	case "Runtime.evaluate":
		c.b.count(c.name, "world", m.Method, c.b.worldOf(m.Params))
		return c.b.evaluate(ctx, m.Params)
	case "Runtime.callFunctionOn":
		c.b.count(c.name, "world", m.Method, c.b.worldOf(m.Params))
		return c.b.callFunctionOn(ctx, m.Params)
	case "Runtime.releaseObject", "Runtime.releaseObjectGroup":
		raw, err := c.b.wip.Send(ctx, m.Method, m.Params)
		return raw, nil, err
	case "DOM.describeNode":
		return c.b.describeNode(ctx, m.Params)
	case "DOM.resolveNode":
		c.b.count(c.name, "world", m.Method, c.b.worldOf(m.Params))
		return c.b.resolveNode(ctx, m.Params)
	}
	return nil, nil, notFound(m.Method)
}

func (c *cdpConn) targetInfo(t string) map[string]any {
	attached := false
	c.mu.Lock()
	for _, s := range c.sessions {
		if s.kind == t {
			attached = true
		}
	}
	c.mu.Unlock()
	if t == "tab" {
		return c.b.tabInfo(attached)
	}
	return c.b.pageInfo(attached)
}

type targetFilter struct {
	Type    string `json:"type"`
	Exclude bool   `json:"exclude"`
}

// chromeDefaultFilter applies when a client gives no filter (5.2).
var chromeDefaultFilter = []targetFilter{{Type: "browser", Exclude: true}, {Type: "tab", Exclude: true}, {}}

func filterAllows(f *[]targetFilter, typ string) bool {
	entries := chromeDefaultFilter
	if f != nil {
		entries = *f
	}
	for _, e := range entries {
		if e.Type == "" || e.Type == typ {
			return !e.Exclude
		}
	}
	return false
}

func (c *cdpConn) setDiscoverTargets(params json.RawMessage) error {
	var p struct {
		Discover bool            `json:"discover"`
		Filter   *[]targetFilter `json:"filter"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return serverError("Invalid parameters: %v", err)
	}
	c.mu.Lock()
	was := c.discover
	c.discover = p.Discover
	c.mu.Unlock()
	if p.Discover && !was {
		for _, t := range []string{"tab", "page"} {
			if filterAllows(p.Filter, t) {
				c.event("", "Target.targetCreated", map[string]any{"targetInfo": c.targetInfo(t)})
			}
		}
	}
	return nil
}

// setAutoAttach announces the existing targets as attached before it
// replies, as Chrome does; Puppeteer counts a tab for its initial set only
// when the event arrives before the reply.
func (c *cdpConn) setAutoAttach(m cdpMsg, s *cdpSession) error {
	var p struct {
		AutoAttach bool            `json:"autoAttach"`
		Flatten    bool            `json:"flatten"`
		Filter     *[]targetFilter `json:"filter"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return serverError("Invalid parameters: %v", err)
	}
	if !p.AutoAttach {
		return nil
	}
	if !p.Flatten {
		return serverError("only flatten:true auto-attach is supported")
	}
	switch {
	case s == nil:
		c.mu.Lock()
		done := c.browserAttach
		c.browserAttach = true
		c.mu.Unlock()
		if done {
			return nil
		}
		if filterAllows(p.Filter, "tab") {
			c.attach("", "tab")
		} else if filterAllows(p.Filter, "page") {
			c.attach("", "page")
		}
	case s.kind == "tab":
		c.mu.Lock()
		done := s.attachedPage
		s.attachedPage = true
		c.mu.Unlock()
		if !done && filterAllows(p.Filter, "page") {
			c.attach(s.id, "page")
		}
	}
	// A page session has no child targets in the proof (no workers, no
	// cross-origin frames).
	return nil
}

func (c *cdpConn) attach(parentSession, kind string) *cdpSession {
	ns := &cdpSession{id: strings.ToUpper(randomHex(16)), kind: kind, targetID: c.b.tabID}
	if kind == "page" {
		ns.targetID = c.b.pageID
	}
	c.mu.Lock()
	c.sessions[ns.id] = ns
	c.mu.Unlock()
	c.event(parentSession, "Target.attachedToTarget", map[string]any{
		"sessionId": ns.id, "targetInfo": c.targetInfo(kind), "waitingForDebugger": false,
	})
	return ns
}

func (c *cdpConn) contextCreated(s *cdpSession, id int, worldName string) {
	c.b.mu.Lock()
	frameID, origin := c.b.frameID, c.b.origin
	c.b.mu.Unlock()
	aux := map[string]any{"frameId": frameID, "isDefault": true, "type": "default"}
	if worldName != "" {
		aux = map[string]any{"frameId": frameID, "isDefault": false, "type": "isolated"}
	}
	c.event(s.id, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{
		"id": id, "origin": origin, "name": worldName, "uniqueId": fmt.Sprintf("p2-%d", id), "auxData": aux,
	}})
}

// replayLifecycle reports the lifecycle of the already loaded document, as
// Chrome does when lifecycle events are enabled.
func (c *cdpConn) replayLifecycle(s *cdpSession) {
	c.b.mu.Lock()
	frameID, loaderID := c.b.frameID, c.b.loaderID
	c.b.mu.Unlock()
	for _, name := range []string{"init", "DOMContentLoaded", "load", "networkAlmostIdle", "networkIdle"} {
		c.event(s.id, "Page.lifecycleEvent", map[string]any{"frameId": frameID, "loaderId": loaderID, "name": name, "timestamp": float64(time.Now().UnixNano()) / 1e9})
	}
}

// ---- Page model.

func (b *p2Bridge) frame() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{
		"id": b.frameID, "loaderId": b.loaderID, "url": b.url, "domainAndRegistry": "", "securityOrigin": b.origin,
		"mimeType": "text/html", "secureContextType": "InsecureScheme", "crossOriginIsolatedContextType": "NotIsolated", "gatedAPIFeatures": []string{},
	}
}

// world returns the alias context id of a world name for the current
// document, and whether it was created by this call.
func (b *p2Bridge) world(name string) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id, ok := b.worlds[name]; ok {
		return id, false
	}
	id := b.nextCtx
	b.nextCtx++
	b.worlds[name] = id
	return id, true
}

func (b *p2Bridge) knownContext(id int) bool {
	if id == 0 || id == mainContextID {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, v := range b.worlds {
		if v == id {
			return true
		}
	}
	return false
}

// ---- Runtime and DOM translation. Every alias runs in the WIP main world.

type remoteObject map[string]any

type wipResult struct {
	Result    remoteObject `json:"result"`
	WasThrown bool         `json:"wasThrown"`
}

// toCDP converts a WIP RemoteObject: WIP has no unserializableValue, and
// its preview has another shape, so it is dropped.
func toCDP(o remoteObject) remoteObject {
	out := remoteObject{}
	for _, k := range []string{"type", "subtype", "className", "value", "description", "objectId"} {
		if v, ok := o[k]; ok {
			out[k] = v
		}
	}
	if out["subtype"] == "class" {
		delete(out, "subtype")
	}
	_, hasValue := o["value"]
	if t := o["type"]; (t == "number" || t == "bigint") && !hasValue {
		out["unserializableValue"] = o["description"]
	}
	return out
}

func isPromise(o remoteObject) bool {
	return o["type"] == "object" && o["className"] == "Promise" && o["objectId"] != nil
}

func cdpResult(r wipResult) map[string]any {
	res := toCDP(r.Result)
	out := map[string]any{"result": res}
	if r.WasThrown {
		out["exceptionDetails"] = map[string]any{
			"exceptionId": 1, "text": "Uncaught", "lineNumber": 0, "columnNumber": 0, "exception": res,
		}
	}
	return out
}

func (b *p2Bridge) wipCall(ctx context.Context, method string, params any) (wipResult, error) {
	raw, err := b.wip.Send(ctx, method, params)
	if err != nil {
		return wipResult{}, err
	}
	var r wipResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return wipResult{}, fmt.Errorf("decoding WIP %s reply %s: %w", method, truncate(string(raw), 200), err)
	}
	return r, nil
}

// wipValue evaluates expr in the main world and returns its value.
func (b *p2Bridge) wipValue(ctx context.Context, expr string) (any, error) {
	r, err := b.wipCall(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
	if err != nil {
		return nil, fmt.Errorf("WIP evaluate %q: %w", expr, err)
	}
	if r.WasThrown {
		return nil, fmt.Errorf("WIP evaluate %q threw %v", expr, r.Result["description"])
	}
	return r.Result["value"], nil
}

// stageSource stores a source that is too large for one WebKitGTK message
// in a page global, in chunks, and returns the global's name.
func (b *p2Bridge) stageSource(ctx context.Context, src string) (string, error) {
	b.mu.Lock()
	b.chunkSeq++
	key := fmt.Sprintf("__p2chunks%d", b.chunkSeq)
	b.mu.Unlock()
	runes := []rune(src)
	for i := 0; i < len(runes); i += wipChunkRunes {
		end := min(i+wipChunkRunes, len(runes))
		lit, err := json.Marshal(string(runes[i:end]))
		if err != nil {
			return "", err
		}
		expr := fmt.Sprintf("(globalThis.%s = globalThis.%s || []).push(%s), 0", key, key, lit)
		if _, err := b.wip.Send(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true}); err != nil {
			return "", fmt.Errorf("staging chunk %d of %d runes: %w", i/wipChunkRunes, len(runes), err)
		}
	}
	b.logf("staged %d runes in %d chunks as %s (WebKitGTK message limit)", len(runes), (len(runes)+wipChunkRunes-1)/wipChunkRunes, key)
	return key, nil
}

func takeStaged(key string) string {
	return fmt.Sprintf("(() => { const s = globalThis.%s.join(''); delete globalThis.%s; return s; })()", key, key)
}

func isWireLimit(err error) bool {
	var wl *webkit.WireLimitError
	return errors.As(err, &wl)
}

type evalParams struct {
	Expression          string         `json:"expression"`
	FunctionDeclaration string         `json:"functionDeclaration"`
	ObjectID            string         `json:"objectId"`
	ContextID           int            `json:"contextId"`
	ExecutionContextID  int            `json:"executionContextId"`
	Arguments           []callArgument `json:"arguments"`
	ReturnByValue       bool           `json:"returnByValue"`
	AwaitPromise        bool           `json:"awaitPromise"`
	UserGesture         bool           `json:"userGesture"`
	Silent              bool           `json:"silent"`
	ObjectGroup         string         `json:"objectGroup"`
}

type callArgument struct {
	Value               json.RawMessage `json:"value,omitempty"`
	UnserializableValue string          `json:"unserializableValue,omitempty"`
	ObjectID            string          `json:"objectId,omitempty"`
}

func (b *p2Bridge) evaluate(ctx context.Context, params json.RawMessage) (any, asyncStep, error) {
	var p evalParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, nil, serverError("Invalid parameters: %v", err)
	}
	if !b.knownContext(p.ContextID) {
		return nil, nil, serverError("Cannot find context with specified id")
	}
	wip := map[string]any{
		"expression": p.Expression, "returnByValue": p.ReturnByValue && !p.AwaitPromise,
		"emulateUserGesture": p.UserGesture, "doNotPauseOnExceptionsAndMuteConsole": p.Silent,
	}
	if p.ObjectGroup != "" {
		wip["objectGroup"] = p.ObjectGroup
	}
	r, err := b.wipCall(ctx, "Runtime.evaluate", wip)
	if isWireLimit(err) {
		key, serr := b.stageSource(ctx, p.Expression)
		if serr != nil {
			return nil, nil, serr
		}
		wip["expression"] = "(0, eval)(" + takeStaged(key) + ")"
		r, err = b.wipCall(ctx, "Runtime.evaluate", wip)
	}
	if err != nil {
		return nil, nil, err
	}
	return b.finish(r, p)
}

// finish applies awaitPromise and returnByValue, which WIP evaluate does
// not support (spike A), through Runtime.awaitPromise.
func (b *p2Bridge) finish(r wipResult, p evalParams) (any, asyncStep, error) {
	if r.WasThrown || !p.AwaitPromise {
		return cdpResult(r), nil, nil
	}
	if isPromise(r.Result) {
		return nil, func(ctx context.Context) (any, error) {
			ar, err := b.wipCall(ctx, "Runtime.awaitPromise", map[string]any{"promiseObjectId": r.Result["objectId"], "returnByValue": p.ReturnByValue})
			if err != nil {
				return nil, err
			}
			return cdpResult(ar), nil
		}, nil
	}
	if p.ReturnByValue && r.Result["objectId"] != nil {
		return nil, func(ctx context.Context) (any, error) {
			vr, err := b.wipCall(ctx, "Runtime.callFunctionOn", map[string]any{"objectId": r.Result["objectId"], "functionDeclaration": "function() { return this; }", "returnByValue": true})
			if err != nil {
				return nil, err
			}
			return cdpResult(vr), nil
		}, nil
	}
	return cdpResult(r), nil, nil
}

// unserializable lists the CDP unserializable values the bridge accepts as
// arguments; WIP CallArgument has only value and objectId.
var unserializable = regexp.MustCompile(`^(undefined|NaN|Infinity|-Infinity|-0|-?[0-9]+n)$`)

func (b *p2Bridge) globalObject(ctx context.Context) (string, error) {
	b.mu.Lock()
	id := b.globalObj
	b.mu.Unlock()
	if id != "" {
		return id, nil
	}
	r, err := b.wipCall(ctx, "Runtime.evaluate", map[string]any{"expression": "globalThis", "objectGroup": "p2-bridge"})
	if err != nil {
		return "", err
	}
	id, _ = r.Result["objectId"].(string)
	if id == "" {
		return "", fmt.Errorf("WIP evaluate globalThis returned no objectId: %v", r.Result)
	}
	b.mu.Lock()
	b.globalObj = id
	b.mu.Unlock()
	return id, nil
}

func (b *p2Bridge) callFunctionOn(ctx context.Context, params json.RawMessage) (any, asyncStep, error) {
	var p evalParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, nil, serverError("Invalid parameters: %v", err)
	}
	this := p.ObjectID
	if this == "" {
		if !b.knownContext(p.ExecutionContextID) {
			return nil, nil, serverError("Cannot find context with specified id")
		}
		g, err := b.globalObject(ctx)
		if err != nil {
			return nil, nil, err
		}
		this = g
	}
	decl := p.FunctionDeclaration
	args := make([]map[string]any, len(p.Arguments))
	var fixes []string
	for i, a := range p.Arguments {
		switch {
		case a.ObjectID != "":
			args[i] = map[string]any{"objectId": a.ObjectID}
		case a.UnserializableValue != "":
			if !unserializable.MatchString(a.UnserializableValue) {
				return nil, nil, serverError("unsupported unserializableValue %q", a.UnserializableValue)
			}
			args[i] = map[string]any{}
			fixes = append(fixes, fmt.Sprintf("a[%d] = %s;", i, a.UnserializableValue))
		case a.Value != nil:
			args[i] = map[string]any{"value": a.Value}
		default:
			args[i] = map[string]any{}
		}
	}
	if len(fixes) > 0 {
		decl = fmt.Sprintf("(function() { const f = (%s); return function(...a) { %s return f.apply(this, a); }; })()", decl, strings.Join(fixes, " "))
	}
	wip := map[string]any{
		"objectId": this, "functionDeclaration": decl, "arguments": args,
		"returnByValue": p.ReturnByValue && !p.AwaitPromise, "emulateUserGesture": p.UserGesture,
		"doNotPauseOnExceptionsAndMuteConsole": p.Silent,
	}
	r, err := b.wipCall(ctx, "Runtime.callFunctionOn", wip)
	if isWireLimit(err) {
		// Turn the declaration into a function object first, then call it
		// through a small trampoline with the original this and arguments.
		key, serr := b.stageSource(ctx, decl)
		if serr != nil {
			return nil, nil, serr
		}
		fr, ferr := b.wipCall(ctx, "Runtime.evaluate", map[string]any{"expression": "(0, eval)('(' + " + takeStaged(key) + " + ')')"})
		if ferr != nil {
			return nil, nil, ferr
		}
		if fr.WasThrown {
			return cdpResult(fr), nil, nil
		}
		wip["functionDeclaration"] = "function(f, ...a) { return f.apply(this, a); }"
		wip["arguments"] = append([]map[string]any{{"objectId": fr.Result["objectId"]}}, args...)
		r, err = b.wipCall(ctx, "Runtime.callFunctionOn", wip)
	}
	if err != nil {
		return nil, nil, err
	}
	return b.finish(r, p)
}

// handleRegistry is the page side of backendNodeId: a number per node,
// stable for the document (5.2, "Node ids").
const handleRegistry = `(globalThis.__p2handles || (globalThis.__p2handles = {list: [null], map: new WeakMap()}))`

func (b *p2Bridge) describeNode(ctx context.Context, params json.RawMessage) (any, asyncStep, error) {
	var p struct {
		ObjectID string `json:"objectId"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.ObjectID == "" {
		return nil, nil, serverError("DOM.describeNode in the proof bridge needs objectId")
	}
	fn := "function() { const h = " + handleRegistry + "; let n = h.map.get(this); if (!n) { n = h.list.length; h.list.push(this); h.map.set(this, n); } " +
		"return {backendNodeId: n, nodeType: this.nodeType, nodeName: this.nodeName, localName: this.localName || '', nodeValue: this.nodeValue || '', childNodeCount: this.childNodes ? this.childNodes.length : 0}; }"
	r, err := b.wipCall(ctx, "Runtime.callFunctionOn", map[string]any{"objectId": p.ObjectID, "functionDeclaration": fn, "returnByValue": true})
	if err != nil {
		return nil, nil, err
	}
	if r.WasThrown {
		return nil, nil, serverError("describeNode: %v", r.Result["description"])
	}
	node, _ := r.Result["value"].(map[string]any)
	if node == nil {
		return nil, nil, serverError("Object id doesn't reference a Node")
	}
	node["nodeId"] = 0
	return map[string]any{"node": node}, nil, nil
}

func (b *p2Bridge) resolveNode(ctx context.Context, params json.RawMessage) (any, asyncStep, error) {
	var p struct {
		NodeID             int    `json:"nodeId"`
		BackendNodeID      int    `json:"backendNodeId"`
		ExecutionContextID int    `json:"executionContextId"`
		ObjectGroup        string `json:"objectGroup"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, nil, serverError("Invalid parameters: %v", err)
	}
	if !b.knownContext(p.ExecutionContextID) {
		return nil, nil, serverError("Cannot find context with specified id")
	}
	if p.NodeID != 0 {
		raw, err := b.wip.Send(ctx, "DOM.resolveNode", map[string]any{"nodeId": p.NodeID, "objectGroup": p.ObjectGroup})
		if err != nil {
			return nil, nil, err
		}
		var r struct {
			Object remoteObject `json:"object"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, nil, err
		}
		return map[string]any{"object": toCDP(r.Object)}, nil, nil
	}
	expr := fmt.Sprintf("%s.list[%d] || null", handleRegistry, p.BackendNodeID)
	r, err := b.wipCall(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "objectGroup": p.ObjectGroup})
	if err != nil {
		return nil, nil, err
	}
	if r.WasThrown || r.Result["objectId"] == nil {
		return nil, nil, serverError("No node with given id found")
	}
	return map[string]any{"object": toCDP(r.Result)}, nil, nil
}
