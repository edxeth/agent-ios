package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
)

// Kinds of attached targets.
const (
	kindTab  = "tab"
	kindPage = "page"
)

// sessionQueue bounds the messages of one CDP session that wait for their
// turn; a client that sends more is too far ahead and is disconnected.
const sessionQueue = 1024

var errConnClosed = errors.New("the CDP connection closed")

// conn is one CDP client connection (a browser or a page WebSocket).
// Every message is handed to its CDP session's queue at once, so one
// session's wait never holds another's messages (design 2.10), and each
// session's messages are handled in arrival order (proof P2 item 6).
type conn struct {
	s     *Server
	rec   *record
	name  string
	ctx   context.Context
	stop  context.CancelFunc
	leave func()
	done  chan struct{}
	final sync.Once // finalize runs once, whether close or run gets there first
	wmu   sync.Mutex

	attachMu sync.Mutex // serializes attach decisions (auto-attach and explicit)

	mu       sync.Mutex
	ws       *websocket.Conn
	closing  bool
	sessions map[string]*session
	queues   map[string]chan *Call
	root     *session       // the page of a /devtools/page/<id> connection
	isPage   bool           // a page connection: messages without a sessionId go to root
	wg       sync.WaitGroup // session workers, Later replies and the poller
	watchers sync.WaitGroup // lease watchers of the attachments

	discMu         sync.Mutex // serializes the announcements of discovery (setDiscoverTargets, refresh)
	discover       bool
	discoverFilter filter
	discovered     map[string]target // by page target id
	reported       map[string]bool   // target ids announced with targetCreated
	autoAttach     bool
	autoFilter     filter
	tried          map[string]bool // target keys that browser auto-attach handled since its last setAutoAttach
	polling        bool
}

var connSeq struct {
	sync.Mutex
	n int
}

// newConn registers a connection of rec, or returns nil when rec was
// revoked.
func (s *Server) newConn(rec *record) *conn {
	connSeq.Lock()
	connSeq.n++
	n := connSeq.n
	connSeq.Unlock()
	c := &conn{
		s: s, rec: rec, name: fmt.Sprintf("cdp%d", n), done: make(chan struct{}),
		sessions: map[string]*session{}, queues: map[string]chan *Call{}, tried: map[string]bool{},
	}
	c.ctx, c.stop = context.WithCancel(context.Background())
	c.leave = func() {}
	if s.o.Enter != nil {
		c.leave = s.o.Enter()
	}
	s.mu.Lock()
	// The record check and the registration share s.mu with RevokeSession
	// and Stop: a connection registers while its record is live, and the
	// revocation then closes it, or it finds the record revoked and never
	// serves (design 5.2).
	if s.records[rec.id] != rec {
		s.mu.Unlock()
		c.stop()
		c.leave()
		return nil
	}
	s.conns[c] = true
	s.mu.Unlock()
	return c
}

func (s *Server) dropConn(c *conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	c.stop()
	c.leave()
}

func (c *conn) attachments() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sessions)
}

// run serves the upgraded connection until the client or the server
// closes it, then detaches every session with its cleanup.
func (c *conn) run(ws *websocket.Conn) {
	ws.SetReadLimit(readLimit)
	c.mu.Lock()
	c.ws = ws
	closing := c.closing
	c.mu.Unlock()
	c.s.o.Logf("cdp: %s connected (session %q)", c.name, c.rec.session)
	for !closing {
		_, data, err := ws.ReadMessage()
		if err != nil {
			break
		}
		var m struct {
			ID        int64           `json:"id"`
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
			SessionID string          `json:"sessionId"`
		}
		if err := json.Unmarshal(data, &m); err != nil || m.Method == "" {
			c.write(map[string]any{"id": m.ID, "error": &Error{Code: -32700, Message: "Message must be a JSON object with an id and a method"}})
			continue
		}
		if !c.enqueue(&Call{ID: m.ID, Method: m.Method, Params: m.Params, SessionID: m.SessionID, conn: c}) {
			break
		}
	}
	c.finish(ws)
}

func (c *conn) finish(ws *websocket.Conn) {
	c.stop()
	c.mu.Lock()
	c.closing = true
	for _, q := range c.queues {
		close(q)
	}
	c.queues = map[string]chan *Call{}
	c.mu.Unlock()
	_ = ws.Close()
	c.wg.Wait()
	c.finalize()
}

// finalize detaches every attachment and gives the connection back to
// the server exactly once (dropConn runs Enter's leave, which the daemon
// counts its activity with). A server-side close before the upgrade and
// the run loop that starts later both end here.
func (c *conn) finalize() {
	c.final.Do(func() {
		c.detachAll()
		c.watchers.Wait()
		c.s.dropConn(c)
		c.s.o.Logf("cdp: %s closed", c.name)
		close(c.done)
	})
}

// close ends the connection from the server side and waits for its
// cleanup.
func (c *conn) close() {
	c.mu.Lock()
	ws := c.ws
	c.closing = true
	c.mu.Unlock()
	if ws == nil { // never upgraded; a later run sees closing and ends at once
		c.finalize()
		return
	}
	_ = ws.Close()
	<-c.done
}

func (c *conn) enqueue(call *Call) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return false
	}
	q, ok := c.queues[call.SessionID]
	if !ok {
		q = make(chan *Call, sessionQueue)
		c.queues[call.SessionID] = q
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			for call := range q {
				c.dispatch(call)
			}
		}()
	}
	select {
	case q <- call:
		return true
	default:
		c.s.o.Logf("cdp: %s: more than %d messages wait on one CDP session; disconnecting the client", c.name, sessionQueue)
		return false
	}
}

func (c *conn) session(id string) *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == "" {
		return c.root
	}
	return c.sessions[id]
}

func (c *conn) dispatch(call *Call) {
	level := OnBrowser
	where := c.name + " browser session"
	base := c.ctx
	c.mu.Lock()
	isPage := c.isPage
	c.mu.Unlock()
	if call.SessionID != "" || isPage {
		s := c.session(call.SessionID)
		if s == nil {
			c.reply(call, nil, errNoSession)
			return
		}
		call.session = s
		level = s.level()
		where = c.name + " " + s.kind + " session " + shortID(s.id)
		base = s.ctx // the attachment's detach cancels its commands
	}
	m, ok := lookup(call.Method, level)
	if !ok {
		c.s.noteUnknown(where, call.Method)
		c.reply(call, nil, notFound(call.Method))
		return
	}
	if m.Handle == nil {
		c.reply(call, nil, nil)
		return
	}
	ctx, cancel := context.WithTimeout(base, callTimeout)
	res, later, err := c.handle(call, func() (any, Later, error) { return m.Handle(ctx, call) })
	if later != nil && err == nil {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer cancel()
			r, _, err := c.handle(call, func() (any, Later, error) {
				r, err := later(ctx)
				return r, nil, err
			})
			c.reply(call, r, err)
		}()
		return
	}
	cancel()
	c.reply(call, res, err)
}

// handle runs a command's handler or its Later. A panic there would end
// the daemon (it runs in a session worker), so it becomes the command's
// error, and the log gets the stack. State that the handler held may
// stay locked; the panic is a bug to fix.
func (c *conn) handle(call *Call, run func() (any, Later, error)) (res any, later Later, err error) {
	defer func() {
		if p := recover(); p != nil {
			c.s.o.Logf("cdp: %s: %s panicked: %v\n%s", c.name, call.Method, p, debug.Stack())
			res, later, err = nil, nil, serverError("%s failed with an internal error in the bridge (%v); the daemon log has the details", call.Method, p)
		}
	}()
	return run()
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (c *conn) reply(call *Call, result any, err error) {
	if call.after != nil {
		defer call.after()
	}
	out := map[string]any{"id": call.ID}
	if call.SessionID != "" {
		out["sessionId"] = call.SessionID
	}
	if err != nil {
		out["error"] = toError(err)
	} else {
		if result == nil {
			result = map[string]any{}
		}
		out["result"] = result
	}
	c.write(out)
}

// event sends an event on a CDP session ("" = the browser session or the
// root of a page connection).
func (c *conn) event(sessionID, method string, params any) {
	m := map[string]any{"method": method, "params": params}
	if sessionID != "" {
		m["sessionId"] = sessionID
	}
	c.write(m)
}

func (c *conn) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		c.s.o.Logf("cdp: %s: encoding a message: %v", c.name, err)
		return
	}
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws == nil {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
		_ = ws.Close() // the read loop ends and cleans up
	}
}

// ---- attachments ----

// session is one CDP attachment (a tab or page target session). It acts
// for the record's logical session under that session's lease (design
// 5.2), and it has its own desired-state owner, so that detaching releases
// only the state it set.
type session struct {
	id     string
	kind   string
	parent string // parent CDP session id; "" = the browser session
	auto   bool   // attached by Target.setAutoAttach (browser or tab level)
	t      target
	c      *conn

	pg     *core.Page
	epoch  uint64
	owner  core.Owner
	unhold func()
	gone   chan struct{} // closed at detach
	page   *pageState    // events, frames and contexts of a page attachment (events.go); nil for a tab

	// ctx ends when the attachment detaches: its queued and running
	// commands and the child attaches it started are canceled.
	ctx    context.Context
	cancel context.CancelFunc

	// The fence of the attachment's page work (review finding 1): begin
	// admits an operation only while the attachment lives, and detach
	// waits for the admitted ones before it releases the attachment's
	// state, so no command adds state after that release.
	fmu    sync.Mutex
	fenced bool
	work   sync.WaitGroup
	// announced: attachedToTarget was sent (guarded by fmu). An attachment
	// that detaches before its announcement is never announced, and its
	// detach sends no detachedFromTarget, so a client never sees an
	// attachment after its detach.
	announced bool

	children []string // guarded by c.mu

	input inputState // the mouse buttons it holds (input_domain.go)
	emu   emuState   // the overrides it set (emulation_domain.go)
}

var (
	errNoSession = serverError("Session with given id not found.")
	errDetached  = serverError("the CDP session detached, so the command was not run")
)

func (s *session) level() Level {
	if s.kind == kindTab {
		return OnTab
	}
	return OnPage
}

func (s *session) targetID() string { return s.t.id(s.kind) }

// begin opens an operation of the page under the attachment's lease for
// the attachment's own work. It is refused once the attachment detaches;
// the operation is canceled when the attachment detaches; and end must be
// called when the work is done (detach waits for it).
func (s *session) begin(ctx context.Context, name string, class core.Class) (mo *core.ManagedOp, end func(), err error) {
	s.fmu.Lock()
	if s.fenced {
		s.fmu.Unlock()
		return nil, nil, errDetached
	}
	s.work.Add(1)
	s.fmu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	unlink := context.AfterFunc(s.ctx, cancel)
	m := s.c.s.o.Manager
	mo, err = m.BeginLeased(ctx, s.pg, s.c.rec.session, s.epoch, "cdp "+name, class)
	if err != nil {
		unlink()
		cancel()
		s.work.Done()
		if s.ctx.Err() != nil {
			return nil, nil, errDetached
		}
		return nil, nil, err
	}
	return mo, func() {
		m.End(mo.ID)
		unlink()
		cancel()
		s.work.Done()
	}, nil
}

// do runs fn in one operation of the page under the attachment's lease
// (see begin).
func (s *session) do(ctx context.Context, name string, class core.Class, fn func(ctx context.Context, mo *core.ManagedOp) error) error {
	mo, end, err := s.begin(ctx, name, class)
	if err != nil {
		return err
	}
	defer end()
	return fn(mo.Op.Context(), mo)
}

// closePage ends the page events of a page attachment.
func (s *session) closePage() {
	if s.page != nil {
		s.page.close()
	}
}

// fence refuses new work of the attachment and cancels the running work.
func (s *session) fence() {
	s.fmu.Lock()
	s.fenced = true
	s.fmu.Unlock()
	s.cancel()
}

// attach creates an attachment of kind on t below the CDP session parent
// (nil: the browser session); auto marks an auto-attached one. It takes
// the logical session's lease on the page, or reuses it with its epoch
// when the session holds it already (design 5.2); a page that another
// session leases is refused with the lease message. The attachment is
// published only while its parent is attached, in the same critical
// section in which detach removes the parent with its children, so no
// child outlives its parent (review finding 2). The caller announces it
// (Target.attachedToTarget).
func (c *conn) attach(ctx context.Context, parent *session, kind string, t target, auto bool) (*session, error) {
	parentID := ""
	if parent != nil {
		parentID = parent.id
		if parent.ctx.Err() != nil {
			return nil, errDetached
		}
	}
	m := c.s.o.Manager
	ao := c.rec.ao()
	pg, epoch, err := m.Touch(ctx, t.req, ao)
	if err != nil {
		return nil, err
	}
	ended, err := m.LeaseEnded(pg, c.rec.session, epoch)
	if err != nil {
		return nil, err
	}
	id := strings.ToUpper(randomHex(16))
	s := &session{
		id: id, kind: kind, parent: parentID, auto: auto, t: t, c: c, pg: pg, epoch: epoch,
		owner: core.SessionOwner("cdp-"+id, c.rec.session), unhold: m.HoldLease(pg, ao), gone: make(chan struct{}),
	}
	s.ctx, s.cancel = context.WithCancel(c.ctx)
	if kind == kindPage {
		s.page = newPageState(s)
	}
	c.mu.Lock()
	var refused error
	switch {
	case c.closing:
		refused = errConnClosed
	case parent != nil && c.sessions[parentID] != parent:
		refused = errDetached
	}
	if refused != nil {
		c.mu.Unlock()
		s.cancel()
		s.closePage()
		s.unhold()
		return nil, refused
	}
	c.sessions[id] = s
	if parent != nil {
		parent.children = append(parent.children, id)
	}
	c.watchers.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.watchers.Done()
		select {
		case <-ended:
			c.s.o.Logf("cdp: %s: the lease of session %q on %q ended; detaching %s session %s", c.name, c.rec.session, t.title, kind, shortID(id))
			c.detach(id, true)
		case <-s.gone:
		}
	}()
	return s, nil
}

// attachRoot attaches the page of a page connection.
func (c *conn) attachRoot(ctx context.Context, pageID string) error {
	t, kind, err := c.rec.find(ctx, pageID)
	if err != nil {
		return err
	}
	if kind != kindPage {
		return serverError("No page target with given id found")
	}
	s, err := c.attach(ctx, nil, kindPage, t, false)
	if err != nil {
		return err
	}
	s.fmu.Lock()
	s.announced = true // the connection itself is the page's announcement
	s.fmu.Unlock()
	c.mu.Lock()
	c.root, c.isPage = s, true
	c.mu.Unlock()
	return nil
}

// announceAttached sends Target.attachedToTarget for s to its parent,
// unless s detached meanwhile.
func (c *conn) announceAttached(s *session) {
	info := c.targetInfo(s.t, s.kind)
	s.fmu.Lock()
	defer s.fmu.Unlock()
	if s.fenced {
		return
	}
	s.announced = true
	c.event(s.parent, "Target.attachedToTarget", map[string]any{
		"sessionId": s.id, "targetInfo": info, "waitingForDebugger": false,
	})
}

// detach ends an attachment and its children: it releases the state each
// attachment set (only its own owner) and its lease hold; the lease stays
// with the logical session (design 5.2). The whole subtree leaves the
// connection in one critical section, and each attachment is fenced:
// its work that was admitted before is canceled and waited for before its
// state is released, so nothing of it outlives the detach (review
// findings 1 and 2). notify sends Target.detachedFromTarget to each
// parent session, children first.
func (c *conn) detach(id string, notify bool) bool {
	c.mu.Lock()
	s := c.sessions[id]
	if s == nil {
		c.mu.Unlock()
		return false
	}
	var subtree []*session // children before their parents
	var remove func(s *session)
	remove = func(s *session) {
		delete(c.sessions, s.id)
		if c.root == s {
			c.root = nil
		}
		for _, ch := range s.children {
			if cs := c.sessions[ch]; cs != nil {
				remove(cs)
			}
		}
		subtree = append(subtree, s)
	}
	remove(s)
	if p := c.sessions[s.parent]; p != nil {
		p.children = slices.DeleteFunc(p.children, func(ch string) bool { return ch == id })
	}
	c.mu.Unlock()
	for _, d := range subtree {
		d.fence()
		d.closePage() // no event of d after its detachedFromTarget
	}
	for _, d := range subtree {
		d.work.Wait()
		close(d.gone)
		c.cleanup(d)
		d.fmu.Lock()
		if notify && d.announced {
			c.event(d.parent, "Target.detachedFromTarget", map[string]any{"sessionId": d.id, "targetId": d.targetID()})
		}
		d.fmu.Unlock()
	}
	return true
}

// cleanup releases the desired state that s set, if any, in one exclusive
// operation under its lease. s is fenced and its work has ended, so no
// entry of its owner appears after this check. When the lease ended, the
// release of the lease already removed every owner of the session, this
// one included. Otherwise the owner leaves the desired state also when
// its cleanup fails or no operation opens before cleanupTimeout; the
// page-side cleanup then stays pending in core (core.ReleaseOwner,
// core.ForgetOwner).
func (c *conn) cleanup(s *session) {
	defer s.unhold()
	owned := false
	for _, o := range s.pg.PS.State.AllOwners() {
		owned = owned || o == s.owner
	}
	if !owned {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	m := c.s.o.Manager
	mo, err := m.BeginLeased(ctx, s.pg, c.rec.session, s.epoch, "cdp detach", core.ClassExclusive)
	switch {
	case err == nil:
		err = m.ReleaseOwner(mo.Op.Context(), mo, s.owner)
		m.End(mo.ID)
	case !errors.Is(err, core.ErrLeased):
		m.ForgetOwner(s.pg, s.owner)
	}
	if err != nil && !errors.Is(err, core.ErrLeased) {
		c.s.o.Logf("cdp: %s: releasing the state of %s session %s on %q: %v", c.name, s.kind, shortID(s.id), s.t.title, err)
	}
}

// detachAll detaches every attachment without events (the connection is
// gone).
func (c *conn) detachAll() {
	c.mu.Lock()
	var top []string
	for id, s := range c.sessions {
		if c.sessions[s.parent] == nil {
			top = append(top, id)
		}
	}
	c.mu.Unlock()
	for _, id := range top {
		c.detach(id, false)
	}
}

// id is the CDP targetId of t as a tab or page target.
func (t target) id(kind string) string {
	if kind == kindTab {
		return t.tabID
	}
	return t.pageID
}

// targetInfo is a CDP TargetInfo of t as a tab or page target, with
// attached set when this connection has a session on it.
func (c *conn) targetInfo(t target, kind string) map[string]any {
	id := t.id(kind)
	attached := false
	c.mu.Lock()
	for _, s := range c.sessions {
		attached = attached || (s.t.key == t.key && s.kind == kind)
	}
	c.mu.Unlock()
	return map[string]any{
		"targetId": id, "type": kind, "title": t.title, "url": t.url, "attached": attached,
		"canAccessOpener": false, "browserContextId": defaultContext,
	}
}

// defaultContext is the only browser context (design 4.3: no browser
// contexts on iOS).
const defaultContext = "default"
