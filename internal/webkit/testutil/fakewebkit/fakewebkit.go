// Package fakewebkit is a scriptable fake WebKit inspector endpoint for unit
// tests (design docs/design/agentic-parity.md, section 8.1). It speaks WIP
// with Target routing like iwdp, WebKitGTK and webinspectord: it announces a
// page target, expects every page command wrapped in
// Target.sendMessageToTarget and answers through
// Target.dispatchMessageFromTarget. Tests script stalls, withheld and held
// replies, provisional swaps, connection failures and page runtime calls,
// and read back every message in order. Like WebKit's backend dispatcher,
// it refuses a Network interception command whose parameters are missing or
// of the wrong type, and it runs each target's ScriptProfiler as WebKit does
// (scriptprofiler.go).
package fakewebkit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Options configure a Server. Zero values give a well-behaved page target.
type Options struct {
	TargetID    string        // first page target; default "page-1"
	TargetDelay time.Duration // delay before Target.targetCreated
	NoTarget    bool          // never announce a target
	// CloseBeforeTarget closes alternate connections (the 1st, 3rd, ...)
	// before they announce a target (spike A step B).
	CloseBeforeTarget bool
	// MaxIncoming closes the connection on a client message larger than
	// this many bytes (WebKitGTK's 128 KiB limit, spike A step D). 0: none.
	MaxIncoming int
	// Steal closes the first connection when a second one opens (spike A step C).
	Steal bool
	// ReconnectGap: after a connection closes, new connections get no
	// Target.targetCreated for this long (MAP fact 2).
	ReconnectGap time.Duration
	// SuspendOnCommit selects the commit model of iOS 16 and WebKitGTK
	// 2.54: the target that a commit replaced goes into the back/forward
	// cache with its backend still connected, so its held requests stay
	// pending there; they are back when the same target id is announced
	// again (a restore, Provisional), and its Network.disable continues
	// them. false: WebKit main's model, in which the commit disconnects
	// the replaced target's backend (as closing a page that is not cached
	// does), so its holds end with the commit.
	SuspendOnCommit bool
}

// Error is a protocol error reply. Data lists the nested errors, as
// WebKit's BackendDispatcher sends them.
type Error struct {
	Code    int     `json:"code"`
	Message string  `json:"message"`
	Data    []Error `json:"data,omitempty"`
}

// Call is one page command received by the fake.
type Call struct {
	ID     int64
	Method string
	Params json.RawMessage
	Target string
	Conn   int
}

// Handler answers a page command with a result (marshaled to JSON; nil
// becomes {}) or a protocol error.
type Handler func(c Call) (any, *Error)

// RuntimeHandler answers a runtime call (window.__iwdp.call(fn, args)) with
// the value of the call expression. It runs on the connection's goroutine.
type RuntimeHandler func(c Call, fn string, args json.RawMessage) any

// Record is one message in the fake's log, in the order the fake saw or
// sent it.
type Record struct {
	Seq    int
	In     bool // true: received from the client; false: sent by the fake (events only)
	Method string
	Params json.RawMessage
	Target string // "" for an unwrapped (outer) message
	Conn   int
	At     time.Time
}

type hold struct {
	until string
	emit  []event
}

type event struct {
	method string
	params any
}

type held struct {
	conn   *conn
	target string
	data   []byte
}

// Server is a fake page endpoint. Create it with New; Close it when done.
type Server struct {
	URL  string // ws:// URL of the page socket
	opts Options
	srv  *httptest.Server

	mu           sync.Mutex
	conns        []*conn
	connCount    int
	lastClose    time.Time
	active       string
	targets      map[string]bool // live targets (active and provisional)
	paused       map[string]chan struct{}
	pauseOnStart bool
	log          []Record
	seq          int
	handlers     map[string]Handler
	stalled      map[string]bool
	stallAll     bool
	stalledConns map[int]bool // StallConnection
	withheld     map[string]bool
	waiting      []held // withheld replies, sent on the next client message
	holds        map[string]hold
	holding      map[string][]held // release method -> replies
	malformed    map[string]string
	dropReply    map[string]int // method -> number of replies to drop
	delays       map[string]time.Duration
	runtime      RuntimeHandler
	intercepts   *intercepts             // nil until the first Intercept (intercept.go)
	profiling    map[string]*profilerRun // tracking runs by target (scriptprofiler.go)
	profClock    float64                 // the profiler's timestamps, in seconds
}

type conn struct {
	id   int
	ws   *websocket.Conn
	wmu  sync.Mutex
	done chan struct{}
	once sync.Once
}

func (c *conn) write(data []byte) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.WriteMessage(websocket.TextMessage, data)
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.ws.Close()
	})
}

// New starts a fake page endpoint.
func New(o Options) *Server {
	if o.TargetID == "" {
		o.TargetID = "page-1"
	}
	s := &Server{
		opts:      o,
		active:    o.TargetID,
		targets:   map[string]bool{o.TargetID: true},
		paused:    make(map[string]chan struct{}),
		handlers:  make(map[string]Handler),
		stalled:   make(map[string]bool),
		withheld:  make(map[string]bool),
		holds:     make(map[string]hold),
		holding:   make(map[string][]held),
		malformed: make(map[string]string),
		dropReply: make(map[string]int),
		delays:    make(map[string]time.Duration),
	}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if o.MaxIncoming > 0 {
			ws.SetReadLimit(int64(o.MaxIncoming))
		}
		s.serve(ws)
	}))
	s.URL = "ws" + strings.TrimPrefix(s.srv.URL, "http") + "/devtools/page/1"
	return s
}

// Close stops the server and closes every connection.
func (s *Server) Close() {
	s.mu.Lock()
	conns := append([]*conn(nil), s.conns...)
	s.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
	s.srv.Close()
}

// Handle sets the handler of a page method. The Network interception
// commands reach their handler only with valid parameters (see checkParams);
// otherwise they get WebKit's parameter error.
func (s *Server) Handle(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// Result answers method with a fixed result.
func (s *Server) Result(method string, result any) {
	s.Handle(method, func(Call) (any, *Error) { return result, nil })
}

// Unsupported answers method with WebKit's "was not found" error.
func (s *Server) Unsupported(method string) {
	s.Handle(method, func(Call) (any, *Error) {
		return nil, &Error{Code: -32601, Message: fmt.Sprintf("'%s' was not found", method)}
	})
}

// Stall accepts method and never answers it.
func (s *Server) Stall(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalled[method] = true
}

// StallConnection makes connection id (1 for the first) accept every page
// command and answer none; other connections are unaffected.
func (s *Server) StallConnection(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stalledConns == nil {
		s.stalledConns = map[int]bool{}
	}
	s.stalledConns[id] = true
}

// StallAll accepts every page command and answers none (a hidden tab, MAP fact 12).
func (s *Server) StallAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stallAll = true
}

// Withhold holds the replies to method until the next client message
// arrives (iOS 26 behavior, MAP fact 7).
func (s *Server) Withhold(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.withheld[method] = true
}

// HoldUntil holds the reply to the next call of method until a page message
// named until arrives; before holding, it emits the given events (method,
// params pairs), for example Network.requestIntercepted or Debugger.paused.
// Later calls of method are answered normally.
func (s *Server) HoldUntil(method, until string, emit ...any) {
	h := hold{until: until}
	for i := 0; i+1 < len(emit); i += 2 {
		h.emit = append(h.emit, event{method: emit[i].(string), params: emit[i+1]})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holds[method] = h
}

// Malformed answers method with an inner reply message built from format,
// in which %d stands for the id of the call, for example {"id":%d,"result":7}.
func (s *Server) Malformed(method, format string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.malformed[method] = format
}

// DropReplies runs the next n calls of method and drops their replies.
func (s *Server) DropReplies(method string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropReply[method] = n
}

// DelayReplies sends each reply to method d after the call arrived; the call
// itself runs on arrival. A method of the form "runtime:<fn>" delays only
// the runtime calls of fn.
func (s *Server) DelayReplies(method string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays[method] = d
}

// SetRuntime installs the handler of runtime calls (see RuntimeCallPattern).
func (s *Server) SetRuntime(h RuntimeHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtime = h
}

// ActiveTarget returns the target that receives page commands.
func (s *Server) ActiveTarget() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// ConnectionCount returns the number of WebSocket connections accepted.
func (s *Server) ConnectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connCount
}

// Log returns every record so far.
func (s *Server) Log() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.log...)
}

// Received returns the messages received from clients, in order.
func (s *Server) Received() []Record {
	var out []Record
	for _, r := range s.Log() {
		if r.In {
			out = append(out, r)
		}
	}
	return out
}

// ReceivedMethod returns the received page commands named method.
func (s *Server) ReceivedMethod(method string) []Record {
	var out []Record
	for _, r := range s.Received() {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

// WithoutAgentResets returns recs without the agent resets of a
// webkit.Session: before the first <Domain>.enable that a session sends to
// a target whose agent state it does not know, it sends the agent's reset,
// by default <Domain>.disable (webkit.SessionOptions.AgentReset). Each
// <Domain>.disable received on a connection and target before that
// domain's first <Domain>.enable there is such a reset; the other records
// are the calls of the domain's owners.
func WithoutAgentResets(recs []Record) []Record {
	type place struct {
		conn           int
		target, domain string
	}
	enabled := map[place]bool{}
	var out []Record
	for _, r := range recs {
		dom, verb, _ := strings.Cut(r.Method, ".")
		p := place{r.Conn, r.Target, dom}
		switch {
		case verb == "enable":
			enabled[p] = true
		case verb == "disable" && !enabled[p]:
			continue
		}
		out = append(out, r)
	}
	return out
}

// Emit sends a page event from the active target to every connection.
func (s *Server) Emit(method string, params any) {
	s.mu.Lock()
	target := s.active
	s.mu.Unlock()
	s.EmitFrom(target, method, params)
}

// EmitFrom sends a page event from target to every connection.
func (s *Server) EmitFrom(target, method string, params any) {
	inner := mustJSON(map[string]any{"method": method, "params": params})
	data := mustJSON(map[string]any{
		"method": "Target.dispatchMessageFromTarget",
		"params": map[string]string{"targetId": target, "message": string(inner)},
	})
	s.record(false, method, mustJSON(params), target, 0)
	s.broadcast(data)
}

// EmitOuter sends an unwrapped event (Target domain) to every connection.
func (s *Server) EmitOuter(method string, params any) {
	s.record(false, method, mustJSON(params), "", 0)
	s.broadcast(mustJSON(map[string]any{"method": method, "params": params}))
}

// Swap runs a provisional-target swap to newID: targetCreated with
// isProvisional (and isPaused when the client enabled pause on start), then,
// commitAfter later (counted from Target.resume for a paused target),
// didCommitProvisionalTarget and targetDestroyed of the old target. It
// returns after the swap.
func (s *Server) Swap(newID string, commitAfter time.Duration) {
	resumed := s.createProvisional(newID)
	<-resumed
	time.Sleep(commitAfter)
	s.Commit(newID)
}

// AbortProvisional creates a provisional target and destroys it without
// commit after destroyAfter (counted from Target.resume for a paused target).
func (s *Server) AbortProvisional(newID string, destroyAfter time.Duration) {
	resumed := s.createProvisional(newID)
	<-resumed
	time.Sleep(destroyAfter)
	s.DestroyTarget(newID)
}

// DestroyActive destroys the active target without a swap (the tab closed).
func (s *Server) DestroyActive() {
	s.DestroyTarget(s.ActiveTarget())
}

// Provisional announces the provisional target id (Target.targetCreated
// with isProvisional, and isPaused when the client enabled pause on start)
// and returns a channel that is closed when the target is resumed, at once
// when it is not paused. The target answers page commands until Commit or
// DestroyTarget.
func (s *Server) Provisional(id string) <-chan struct{} { return s.createProvisional(id) }

// Commit commits the provisional target newID as WebKit does:
// Target.didCommitProvisionalTarget, then Target.targetDestroyed of the old
// active target. newID keeps its id and its held requests; the old target's
// held requests are gone with it, or stay pending in its suspended backend
// with Options.SuspendOnCommit.
func (s *Server) Commit(newID string) {
	s.mu.Lock()
	old := s.active
	s.active = newID
	if s.opts.SuspendOnCommit {
		delete(s.targets, old)
	} else {
		s.dropTargetLocked(old)
	}
	s.mu.Unlock()
	s.EmitOuter("Target.didCommitProvisionalTarget", map[string]string{"oldTargetId": old, "newTargetId": newID})
	s.EmitOuter("Target.targetDestroyed", map[string]string{"targetId": old})
}

// DestroyTarget destroys target id without a commit (an abandoned
// provisional target, or the active target when the tab closed). Its held
// requests are gone with it.
func (s *Server) DestroyTarget(id string) {
	s.mu.Lock()
	s.dropTargetLocked(id)
	s.mu.Unlock()
	s.EmitOuter("Target.targetDestroyed", map[string]string{"targetId": id})
}

// dropTargetLocked forgets target id, the requests it held and its
// ScriptProfiler run.
func (s *Server) dropTargetLocked(id string) {
	delete(s.targets, id)
	delete(s.profiling, id)
	if s.intercepts != nil {
		delete(s.intercepts.held, id)
	}
}

// OpenConnections is the number of connections that are open now.
func (s *Server) OpenConnections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// CloseConnections closes every open connection from the server side.
func (s *Server) CloseConnections() {
	s.mu.Lock()
	conns := append([]*conn(nil), s.conns...)
	s.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

func (s *Server) createProvisional(id string) <-chan struct{} {
	s.mu.Lock()
	s.targets[id] = true
	paused := s.pauseOnStart
	resumed := make(chan struct{})
	if paused {
		s.paused[id] = resumed
	} else {
		close(resumed)
	}
	s.mu.Unlock()
	s.EmitOuter("Target.targetCreated", map[string]any{"targetInfo": map[string]any{
		"targetId": id, "type": "page", "isProvisional": true, "isPaused": paused,
	}})
	return resumed
}

func (s *Server) broadcast(data []byte) {
	s.mu.Lock()
	conns := append([]*conn(nil), s.conns...)
	s.mu.Unlock()
	for _, c := range conns {
		c.write(data)
	}
}

func (s *Server) record(in bool, method string, params json.RawMessage, target string, connID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	s.log = append(s.log, Record{Seq: s.seq, In: in, Method: method, Params: params, Target: target, Conn: connID, At: time.Now()})
}

func (s *Server) serve(ws *websocket.Conn) {
	s.mu.Lock()
	s.connCount++
	c := &conn{id: s.connCount, ws: ws, done: make(chan struct{})}
	var victims []*conn
	if s.opts.Steal {
		victims = append(victims, s.conns...)
	}
	s.conns = append(s.conns, c)
	announce := !s.opts.NoTarget
	closeEarly := s.opts.CloseBeforeTarget && c.id%2 == 1
	gap := time.Duration(0)
	if s.opts.ReconnectGap > 0 && !s.lastClose.IsZero() {
		gap = s.opts.ReconnectGap - time.Since(s.lastClose)
	}
	target := s.active
	s.mu.Unlock()
	for _, v := range victims {
		v.close()
	}
	defer func() {
		c.close()
		s.mu.Lock()
		s.lastClose = time.Now()
		for i, x := range s.conns {
			if x == c {
				s.conns = append(s.conns[:i], s.conns[i+1:]...)
				break
			}
		}
		if len(s.conns) == 0 {
			s.profiling = nil // the last frontend went: WebKit ends every run
		}
		s.mu.Unlock()
	}()

	go func() {
		if closeEarly {
			time.Sleep(20 * time.Millisecond)
			c.close()
			return
		}
		if !announce || gap > 0 {
			return
		}
		select {
		case <-time.After(s.opts.TargetDelay):
		case <-c.done:
			return
		}
		c.write(mustJSON(map[string]any{"method": "Target.targetCreated", "params": map[string]any{
			"targetInfo": map[string]any{"targetId": target, "type": "page"},
		}}))
	}()

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		s.handleMessage(c, data)
	}
}

type wire struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (s *Server) handleMessage(c *conn, data []byte) {
	// A message from the client releases withheld replies (MAP fact 7).
	s.mu.Lock()
	release := s.waiting
	s.waiting = nil
	s.mu.Unlock()

	var m wire
	if err := json.Unmarshal(data, &m); err != nil {
		s.record(true, "<malformed>", nil, "", c.id)
		return
	}
	switch m.Method {
	case "Target.sendMessageToTarget":
		var p struct {
			TargetID string `json:"targetId"`
			Message  string `json:"message"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.write(mustJSON(map[string]any{"id": m.ID, "result": map[string]any{}}))
		var inner wire
		if err := json.Unmarshal([]byte(p.Message), &inner); err != nil {
			s.record(true, "<malformed inner>", nil, p.TargetID, c.id)
		} else {
			s.record(true, inner.Method, inner.Params, p.TargetID, c.id)
			s.handlePage(c, Call{ID: inner.ID, Method: inner.Method, Params: inner.Params, Target: p.TargetID, Conn: c.id})
		}
	case "Target.setPauseOnStart":
		s.record(true, m.Method, m.Params, "", c.id)
		var p struct {
			PauseOnStart bool `json:"pauseOnStart"`
		}
		_ = json.Unmarshal(m.Params, &p)
		s.mu.Lock()
		s.pauseOnStart = p.PauseOnStart
		s.mu.Unlock()
		c.write(mustJSON(map[string]any{"id": m.ID, "result": map[string]any{}}))
	case "Target.resume":
		s.record(true, m.Method, m.Params, "", c.id)
		var p struct {
			TargetID string `json:"targetId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		s.mu.Lock()
		if ch, ok := s.paused[p.TargetID]; ok {
			close(ch)
			delete(s.paused, p.TargetID)
		}
		s.mu.Unlock()
		c.write(mustJSON(map[string]any{"id": m.ID, "result": map[string]any{}}))
	default:
		// Unwrapped page commands get no answer on a routed endpoint.
		s.record(true, m.Method, m.Params, "", c.id)
	}
	s.deliver(release)
	s.releaseHeld(m, data)
}

// releaseHeld sends replies held by HoldUntil when their release message arrives.
func (s *Server) releaseHeld(m wire, data []byte) {
	method := m.Method
	if method == "Target.sendMessageToTarget" {
		var p struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(m.Params, &p)
		var inner wire
		_ = json.Unmarshal([]byte(p.Message), &inner)
		method = inner.Method
	}
	s.mu.Lock()
	rel := s.holding[method]
	delete(s.holding, method)
	s.mu.Unlock()
	s.deliver(rel)
}

func (s *Server) deliver(hs []held) {
	for _, h := range hs {
		h.conn.write(h.data)
	}
}

// RuntimeCallPattern matches the runtime call expression of design 3.1:
// (window.__iwdp && window.__iwdp.v === "<ver>") ? window.__iwdp.call("<fn>", <json args>) : "__IWDP_MISSING__"
var RuntimeCallPattern = regexp.MustCompile(`(?s)window\.__iwdp\.call\("([A-Za-z0-9_.]+)", (.*)\) : "__IWDP_MISSING__"$`)

func (s *Server) handlePage(c *conn, call Call) {
	s.mu.Lock()
	if s.stallAll || s.stalled[call.Method] || s.stalledConns[c.id] {
		s.mu.Unlock()
		return
	}
	h := s.handlers[call.Method]
	raw, isMalformed := s.malformed[call.Method]
	withhold := s.withheld[call.Method]
	hd, isHeld := s.holds[call.Method]
	delete(s.holds, call.Method)
	rt := s.runtime
	drop := s.dropReply[call.Method] > 0
	if drop {
		s.dropReply[call.Method]--
	}
	live := s.targets[call.Target]
	delays := s.delays
	tracked := s.intercepts != nil
	s.mu.Unlock()
	delay := func(fn string) time.Duration {
		s.mu.Lock()
		defer s.mu.Unlock()
		if d, ok := delays["runtime:"+fn]; ok && fn != "" {
			return d
		}
		return delays[call.Method]
	}
	if !live {
		return // a destroyed or unknown target never answers
	}

	var inner []byte
	var fn string
	switch {
	case isMalformed:
		inner = []byte(strings.ReplaceAll(raw, "%d", fmt.Sprint(call.ID)))
	default:
		var result any
		var perr *Error
		switch invalid := checkParams(call.Method, call.Params); {
		case invalid != nil:
			perr = invalid
		case h != nil:
			result, perr = h(call)
		case call.Method == "ScriptProfiler.startTracking" || call.Method == "ScriptProfiler.stopTracking":
			s.scriptProfiler(call)
		case tracked && interceptStage(call) != "":
			perr = s.takeHold(call)
		case tracked && call.Method == "Network.disable":
			s.continueHolds(call.Target)
		case call.Method == "Runtime.evaluate":
			result, fn = s.evaluate(call, rt)
			if result == nil {
				return // the runtime handler dropped the reply
			}
		}
		if perr != nil {
			inner = mustJSON(map[string]any{"id": call.ID, "error": perr})
		} else {
			if result == nil {
				result = map[string]any{}
			}
			inner = mustJSON(map[string]any{"id": call.ID, "result": result})
		}
	}
	if drop {
		return
	}
	out := held{conn: c, target: call.Target, data: mustJSON(map[string]any{
		"method": "Target.dispatchMessageFromTarget",
		"params": map[string]string{"targetId": call.Target, "message": string(inner)},
	})}
	switch {
	case isHeld:
		for _, e := range hd.emit {
			s.EmitFrom(call.Target, e.method, e.params)
		}
		s.mu.Lock()
		s.holding[hd.until] = append(s.holding[hd.until], out)
		s.mu.Unlock()
	case withhold:
		s.mu.Lock()
		s.waiting = append(s.waiting, out)
		s.mu.Unlock()
	case delay(fn) > 0:
		go func(d time.Duration) {
			time.Sleep(d)
			c.write(out.data)
		}(delay(fn))
	default:
		c.write(out.data)
	}
}

// evaluate is the default Runtime.evaluate: runtime calls go to the runtime
// handler; any other expression evaluates to the number 0. It returns nil
// when the runtime handler dropped the reply.
func (s *Server) evaluate(call Call, rt RuntimeHandler) (any, string) {
	var p struct {
		Expression string `json:"expression"`
	}
	_ = json.Unmarshal(call.Params, &p)
	if m := RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil && rt != nil {
		v := rt(call, m[1], json.RawMessage(m[2]))
		if v == NoReply {
			return nil, m[1]
		}
		return map[string]any{"result": map[string]any{"type": "object", "value": v}}, m[1]
	}
	return map[string]any{"result": map[string]any{"type": "number", "value": 0, "description": "0"}}, ""
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("fakewebkit: marshal %T: %v", v, err))
	}
	return data
}
