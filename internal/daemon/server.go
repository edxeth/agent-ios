package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/cdp"
	"github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// DefaultCommandTimeout is the deadline of a request that names none
// (design 2.6, W0 default).
const DefaultCommandTimeout = 30 * time.Second

// eventQueue bounds the events buffered for one /v1/events stream; a slow
// client loses events (counted) instead of blocking the page connection.
const eventQueue = 4096

// Server serves the socket API of design 2.2 for one Manager.
type Server struct {
	m       *core.Manager
	version string
	started time.Time
	res     *core.Reservations
	logf    func(string, ...any)

	mu       sync.Mutex
	active   int // requests, operation holds and event streams in progress
	last     time.Time
	opCancel map[string]context.CancelFunc
	shutdown chan bool // receives force on POST /v1/shutdown

	stopping chan struct{} // closed by Stop when the daemon shuts down
	stopOnce sync.Once

	cdp *cdp.Server // the CDP endpoint (design 5.1); set by newCore
}

// NewServer returns the API server of m.
func NewServer(m *core.Manager, res *core.Reservations, version string, logf func(string, ...any)) *Server {
	return &Server{
		m: m, res: res, version: version, started: time.Now(), last: time.Now(), logf: logf,
		opCancel: map[string]context.CancelFunc{}, shutdown: make(chan bool, 1), stopping: make(chan struct{}),
	}
}

// Stop starts the shutdown of the API (design 2.6): every event stream
// sends a final "closed" item that names the shutdown and ends, so clients
// see the end of their page instead of a dropped connection. The CDP
// endpoint stops too: its connections release their state while the
// manager still runs, and its URLs are revoked.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopping)
		if s.cdp != nil {
			s.cdp.Stop()
		}
	})
}

// Shutdown is signalled by POST /v1/shutdown.
func (s *Server) Shutdown() <-chan bool { return s.shutdown }

// Idle reports how long the server has had nothing in progress, or 0
// while a request, hold or stream is open.
func (s *Server) Idle(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active > 0 {
		return 0
	}
	return now.Sub(s.last)
}

func (s *Server) enter() func() {
	s.mu.Lock()
	s.active++
	s.last = time.Now()
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.active--
		s.last = time.Now()
		s.mu.Unlock()
	}
}

// Handler returns the HTTP handler of the socket API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hello", s.hello)
	mux.HandleFunc("GET /v1/status", s.status)
	mux.HandleFunc("POST /v1/op", s.op)
	mux.HandleFunc("POST /v1/raw", s.raw)
	mux.HandleFunc("GET /v1/events", s.events)
	mux.HandleFunc("POST /v1/lease", s.lease)
	mux.HandleFunc("POST /v1/run", s.run)
	mux.HandleFunc("POST /v1/shutdown", s.shutdownReq)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer s.enter()()
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("bad request body: %w", err)
	}
	return nil
}

// stream writes NDJSON items with chunked encoding.
type stream struct {
	mu sync.Mutex
	w  http.ResponseWriter
	f  http.Flusher
}

func newStream(w http.ResponseWriter) *stream {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	return &stream{w: w, f: f}
}

func (st *stream) send(it Item) error {
	data, err := json.Marshal(it)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, err := st.w.Write(append(data, '\n')); err != nil {
		return err
	}
	if st.f != nil {
		st.f.Flush()
	}
	return nil
}

func (s *Server) hello(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.helloValue())
}

func (s *Server) helloValue() Hello {
	return Hello{
		ID: s.m.DaemonID(), Version: s.version, Protocol: Protocol, PID: os.Getpid(), StartedAt: s.started,
		Transports: []string{"iwdp", "direct", "webkitgtk"}, Leases: s.m.ActiveLeases(),
	}
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	out := Status{Daemon: s.helloValue(), Pages: s.m.Status()}
	if s.cdp != nil {
		out.CDP = s.cdp.Status()
	}
	if s.res != nil {
		if rs, err := s.res.List(); err == nil {
			out.Reservations = rs
		} else {
			out.ReservationsError = err.Error()
		}
	}
	writeJSON(w, out)
}

func pageRequest(p PageReq) core.PageRequest {
	return core.PageRequest{Transport: p.Transport, Addr: p.Addr, Page: p.Page}
}

func acquireOptions(o Owner) core.AcquireOptions {
	return core.AcquireOptions{Session: o.Session, Reservation: o.Reservation, WaitLease: durMs(o.WaitLeaseMs, 0), Takeover: o.Takeover}
}

func classOf(name string) (core.Class, error) {
	switch name {
	case "", "exclusive":
		return core.ClassExclusive, nil
	case "read":
		return core.ClassRead, nil
	case "mutate":
		return core.ClassMutate, nil
	}
	return 0, fmt.Errorf("unknown operation class %q", name)
}

func pageInfo(mo *core.ManagedOp) PageInfo {
	return PageInfo{Key: mo.Page.Key, Title: mo.Page.Title(), Routed: mo.Page.PS.Caller.IsTargetRouted(), Epoch: mo.Epoch}
}

// op serves POST /v1/op. begin answers with a stream that holds the
// operation open: its first item is the operation, and the operation ends
// when the client ends it or closes the stream (a killed client cancels
// its operation at once, design 2.6). A "closed" item reports that the page
// connection ended while the operation was open.
func (s *Server) op(w http.ResponseWriter, r *http.Request) {
	var req OpRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, RawReply{Error: errorf(2, "%v", err)})
		return
	}
	if req.Op == "end" {
		s.endOp(req.OpID)
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if req.Op != "begin" {
		writeJSON(w, RawReply{Error: errorf(2, "op %q: want begin or end", req.Op)})
		return
	}
	class, err := classOf(req.Class)
	if err != nil {
		writeJSON(w, RawReply{Error: errorf(2, "%v", err)})
		return
	}
	timeout := durMs(req.TimeoutMs, DefaultCommandTimeout)
	// The operation outlives this request's admission wait, so it gets
	// its own context with the command deadline.
	opCtx, cancel := context.WithTimeout(context.Background(), timeout)
	stop := context.AfterFunc(r.Context(), cancel)
	name := req.Name
	if name == "" {
		name = "legacy"
	}
	mo, err := s.m.Begin(opCtx, pageRequest(req.PageReq), acquireOptions(req.Owner), name, class)
	stop()
	if err != nil {
		cancel()
		writeJSON(w, RawReply{Error: toErrorReply(err)})
		return
	}
	s.mu.Lock()
	s.opCancel[mo.ID] = cancel
	s.mu.Unlock()
	defer s.endOp(mo.ID)
	dl, _ := opCtx.Deadline()
	st := newStream(w)
	info := pageInfo(mo)
	if err := st.send(Item{Type: "op", Op: &OpReply{OpID: mo.ID, Epoch: mo.Epoch, Deadline: dl, Page: info}}); err != nil {
		return
	}
	select {
	case <-r.Context().Done(): // the client ended or went away
		return
	case <-mo.Op.Context().Done(): // ended, canceled (release or a lost page) or expired
	case <-mo.Page.PS.Caller.Done():
	}
	select {
	case <-mo.Page.PS.Caller.Done():
		_ = st.send(Item{Type: "closed", Error: toErrorReply(&core.CodedError{Code: core.CodeBusy, Err: fmt.Errorf("page %q: %w", info.Title, core.ErrLost)})})
	default:
	}
}

func (s *Server) endOp(id string) {
	s.mu.Lock()
	cancel := s.opCancel[id]
	delete(s.opCancel, id)
	s.mu.Unlock()
	s.m.End(id)
	if cancel != nil {
		cancel()
	}
}

// raw serves POST /v1/raw: one protocol call inside an operation (design
// 2.10), or a one-call operation when no operation id is given.
func (s *Server) raw(w http.ResponseWriter, r *http.Request) {
	var req RawRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, RawReply{Error: errorf(2, "%v", err)})
		return
	}
	ctx := r.Context()
	if req.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, durMs(req.TimeoutMs, 0))
		defer cancel()
	}
	if kind, ok := core.ControlKindOf(req.Method); ok {
		res, err := s.control(ctx, req, kind)
		if err != nil {
			writeJSON(w, RawReply{Error: toErrorReply(err)})
			return
		}
		if res == nil {
			res = json.RawMessage("{}")
		}
		writeJSON(w, RawReply{Result: res})
		return
	}
	id := req.OpID
	if id == "" {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, DefaultCommandTimeout)
			defer cancel()
		}
		mo, err := s.m.Begin(ctx, pageRequest(req.PageReq), acquireOptions(req.Owner), req.Method, core.ClassExclusive)
		if err != nil {
			writeJSON(w, RawReply{Error: toErrorReply(err)})
			return
		}
		defer s.m.End(mo.ID)
		id = mo.ID
	} else if mo, err := s.m.Op(id); err == nil {
		// A call never outlives its operation's deadline.
		if dl, ok := mo.Op.Context().Deadline(); ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, dl)
			defer cancel()
		}
	}
	res, err := s.m.Raw(ctx, id, req.Method, req.Params)
	if err != nil {
		writeJSON(w, RawReply{Error: toErrorReply(err)})
		return
	}
	if res == nil {
		res = json.RawMessage("{}")
	}
	writeJSON(w, RawReply{Result: res})
}

// control sends a control-lane message (design 2.10): Debugger.resume, a
// completion of a held request or Target.resume. It never waits behind
// ordinary operations, and the scheduler validates it (current epoch, the
// lease holder's session, an open pending item, completed at most once).
// The sender's authority is the open operation it names (an ended one is
// refused), else the page key and the lease epoch that its caller acquired:
// a message never takes the epoch of a later lease, so a resume or a
// completion retained from an ended lease cannot act on the next one.
func (s *Server) control(ctx context.Context, req RawRequest, kind core.ControlKind) (json.RawMessage, error) {
	var pg *core.Page
	var session string
	var epoch uint64
	switch {
	case req.OpID != "":
		mo, err := s.m.Op(req.OpID)
		if err != nil {
			return nil, notSentError{err}
		}
		pg, session, epoch = mo.Page, mo.Session, mo.Epoch
	case req.Key != "" && req.Epoch != 0:
		if pg = s.m.LookupPage(req.Key); pg == nil {
			return nil, notSentError{&core.CodedError{Code: core.CodeNoPage, Err: fmt.Errorf("%s: %w: page %q is no longer attached", req.Method, core.ErrNoPage, req.Key)}}
		}
		session, epoch = req.Session, req.Epoch
		if session == "" {
			session = "default"
		}
	default:
		return nil, notSentError{&core.CodedError{Code: 2, Err: fmt.Errorf("%s: a control message needs its operation or the page key and lease epoch of its caller", req.Method)}}
	}
	msg := core.ControlMsg{Kind: kind, Session: session, Epoch: epoch, Method: req.Method}
	if len(req.Params) > 0 && string(req.Params) != "null" {
		if err := json.Unmarshal(req.Params, &msg.Params); err != nil {
			return nil, &core.CodedError{Code: 2, Err: fmt.Errorf("%s: bad params: %w", req.Method, err)}
		}
	}
	msg.RequestID, _ = msg.Params["requestId"].(string)
	msg.TargetID, _ = msg.Params["targetId"].(string)
	delete(msg.Params, "requestId")
	res, err := pg.PS.Sched.Control(ctx, msg)
	if errors.Is(err, core.ErrControlRefused) {
		err = notSentError{err}
	}
	return res, err
}

// notSentError marks a refused control message: it was never sent.
type notSentError struct{ error }

func (e notSentError) Unwrap() []error { return []error{e.error, webkit.ErrNotSent} }

// events serves GET /v1/events: the page's events for the session that
// holds its lease, until the client closes the stream, the page ends or the
// lease ends (release, break, expiry or takeover; design 2.5).
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pr := PageReq{Transport: q.Get("transport"), Addr: q.Get("addr"), Page: q.Get("page")}
	ow := Owner{Session: q.Get("session"), Reservation: q.Get("reservation")}
	methods := strings.Split(q.Get("methods"), ",")
	ctx, cancel := context.WithTimeout(r.Context(), DefaultCommandTimeout)
	pg, epoch, err := s.m.Touch(ctx, pageRequest(pr), acquireOptions(ow))
	cancel()
	var leaseEnded <-chan struct{}
	if err == nil {
		leaseEnded, err = s.m.LeaseEnded(pg, ow.Session, epoch)
	}
	if err != nil {
		writeJSON(w, Item{Type: "error", Error: toErrorReply(err)})
		return
	}
	// The open stream keeps the session's lease from the idle TTL (design
	// 2.6 heartbeat).
	defer s.m.HoldLease(pg, acquireOptions(ow))()
	type ev struct {
		method string
		params json.RawMessage
	}
	queue := make(chan ev, eventQueue)
	var dropped int64
	var dmu sync.Mutex
	var unsubs []func()
	var done <-chan struct{}
	for _, m := range methods {
		if m = strings.TrimSpace(m); m == "" {
			continue
		}
		unsub, d := s.m.Subscribe(pg, m, func(method string, params json.RawMessage) {
			select {
			case queue <- ev{method, params}:
			default:
				dmu.Lock()
				dropped++
				dmu.Unlock()
			}
		})
		unsubs = append(unsubs, unsub)
		done = d
	}
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()
	st := newStream(w)
	info := PageInfo{Key: pg.Key, Title: pg.Title(), Routed: pg.PS.Caller.IsTargetRouted()}
	if err := st.send(Item{Type: "subscribed", Page: &info}); err != nil {
		return
	}
	if done == nil {
		done = pg.PS.Caller.Done()
	}
	for {
		select {
		case e := <-queue:
			dmu.Lock()
			d := dropped
			dropped = 0
			dmu.Unlock()
			if err := st.send(Item{Type: "event", Method: e.method, Params: e.params, Dropped: d}); err != nil {
				return
			}
		case <-done:
			_ = st.send(Item{Type: "closed", Error: toErrorReply(&core.CodedError{Code: core.CodeBusy, Err: fmt.Errorf("page %q: %w", info.Title, core.ErrLost)})})
			return
		case <-leaseEnded:
			_ = st.send(Item{Type: "closed", Error: toErrorReply(&core.CodedError{Code: core.CodeBusy, Err: fmt.Errorf("page %q: the lease of session %q ended (release, lease break, idle expiry or takeover); the stream stops", info.Title, ow.Session)})})
			return
		case <-s.stopping:
			_ = st.send(Item{Type: "closed", Error: toErrorReply(&core.CodedError{Code: core.CodeBusy, Err: fmt.Errorf("page %q: the iwdp daemon is shutting down; select the page again", info.Title)})})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// lease serves POST /v1/lease (design 2.5).
func (s *Server) lease(w http.ResponseWriter, r *http.Request) {
	var req LeaseRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, RawReply{Error: errorf(2, "%v", err)})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), durMs(req.TimeoutMs, DefaultCommandTimeout))
	defer cancel()
	ao := acquireOptions(req.Owner)
	if ao.Session == "" {
		ao.Session = "default"
	}
	var err error
	switch req.Op {
	case "acquire", "renew", "takeover":
		ao.Takeover = ao.Takeover || req.Op == "takeover"
		var pg *core.Page
		var epoch uint64
		if pg, epoch, err = s.m.Touch(ctx, pageRequest(req.PageReq), ao); err == nil {
			writeJSON(w, map[string]any{"pages": s.m.Status(), "page": PageInfo{Key: pg.Key, Title: pg.Title(), Routed: pg.PS.Caller.IsTargetRouted(), Epoch: epoch}})
			return
		}
	case "release":
		if req.Page == "" {
			_, err = s.m.ReleaseSession(ctx, ao.Session)
		} else if pg := s.m.FindAttached(req.Page); pg != nil {
			err = s.m.Release(ctx, ao.Session, pg.Key)
		}
	case "break":
		pg := s.m.FindAttached(req.Page)
		if pg == nil {
			err = &core.CodedError{Code: core.CodeNoPage, Err: fmt.Errorf("%w: no attached page matches %q", core.ErrNoPage, req.Page)}
		} else {
			err = s.m.Break(ctx, pg.Key)
		}
	default:
		err = &core.CodedError{Code: 2, Err: fmt.Errorf("lease op %q: want acquire, release, break, renew or takeover", req.Op)}
	}
	if err != nil {
		writeJSON(w, map[string]any{"error": toErrorReply(err)})
		return
	}
	writeJSON(w, map[string]any{"pages": s.m.Status()})
}

func (s *Server) shutdownReq(w http.ResponseWriter, r *http.Request) {
	var req shutdownRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, shutdownReply{Error: errorf(2, "%v", err)})
		return
	}
	if n := s.m.ActiveLeases(); req.IfIdle && n > 0 {
		// Protocol retirement (design 2.2) applies only to an idle daemon,
		// judged now rather than from the client's earlier hello.
		writeJSON(w, shutdownReply{Error: errorf(core.CodeBusy, "the daemon has %d active lease(s); it stays", n)})
		return
	}
	writeJSON(w, shutdownReply{OK: true})
	select {
	case s.shutdown <- req.Force:
	default:
	}
}

// runEnv is the commands.Env of a registry command run by the daemon.
type runEnv struct {
	s   *Server
	req RunRequest
	st  *stream
	mo  *core.ManagedOp
}

func (e *runEnv) Page(context.Context) (*core.PageSession, error) {
	if e.mo == nil {
		return nil, fmt.Errorf("this command runs without a page")
	}
	return e.mo.Page.PS, nil
}

// Op is the operation the command runs in; commands use its Caller.
func (e *runEnv) Op() *core.Op {
	if e.mo == nil {
		return nil
	}
	return e.mo.Op
}

// Manager is the daemon's manager (design 2.11, Env.Manager).
func (e *runEnv) Manager() *core.Manager { return e.s.m }

// Reservations are the daemon's device reservations (design 2.5).
func (e *runEnv) Reservations() *core.Reservations { return e.s.res }

// CDP is the daemon's CDP endpoint (design 5.1; the cdp and get cdp-url
// commands).
func (e *runEnv) CDP() *cdp.Server { return e.s.cdp }

func (e *runEnv) Emit(line string) { _ = e.st.send(Item{Type: "line", Text: line}) }

func (e *runEnv) ResolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) || e.req.Cwd == "" {
		return p
	}
	return filepath.Join(e.req.Cwd, p)
}

// run serves POST /v1/run: one registry command. A command that needs a
// page runs inside one operation of its class; a retry-safe command is
// retried once after a reconnect (design 2.6).
func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, Item{Type: "error", Error: errorf(2, "%v", err)})
		return
	}
	inv, err := commands.Parse(req.Argv, func(string) string { return "" })
	if err != nil || inv.Cmd == nil {
		if err == nil {
			err = fmt.Errorf("no command")
		}
		writeJSON(w, Item{Type: "error", Error: toErrorReply(err)})
		return
	}
	if inv.Cmd.Local {
		writeJSON(w, Item{Type: "error", Error: errorf(2, "%s runs in the client, not in the daemon", inv.Cmd.Name)})
		return
	}
	g := &inv.Input.Global
	g.Session, g.JSON, g.Page, g.Transport = req.Session, req.JSON, req.Page, req.Transport
	g.Reservation, g.WaitLease, g.Takeover = req.Reservation, durMs(req.WaitLeaseMs, 0), req.Takeover
	if g.Session == "" {
		g.Session = "default"
	}
	g.Timeout = durMs(req.TimeoutMs, DefaultCommandTimeout)
	ctx, cancel := context.WithTimeout(r.Context(), durMs(req.TimeoutMs, DefaultCommandTimeout))
	defer cancel()
	// Registry commands start the lease hooks (the network capture of the
	// lease, design 4.2); legacy operations (/v1/op) do not.
	ctx = core.WithLeaseHooks(ctx)
	st := newStream(w)
	env := &runEnv{s: s, req: req, st: st}
	var res commands.Result
	runCmd := func(ctx context.Context) error {
		var err error
		res, err = inv.Cmd.Run(ctx, env, inv.Input)
		return err
	}
	if inv.Cmd.Page == commands.NoPage {
		err = runCmd(ctx)
	} else {
		class := core.ClassRead
		if inv.Cmd.Mutates {
			class = core.ClassMutate
		}
		err = s.m.Do(ctx, pageRequest(req.PageReq), acquireOptions(req.Owner), inv.Cmd.Name, class, inv.Cmd.RetrySafe,
			func(ctx context.Context, mo *core.ManagedOp) error {
				env.mo = mo
				return runCmd(ctx)
			})
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, core.ErrDeadline) {
			err = &core.CodedError{Code: core.CodeTimeout, Err: fmt.Errorf("%w: %w", err, context.DeadlineExceeded)}
		}
		_ = st.send(Item{Type: "error", Error: toErrorReply(err)})
		return
	}
	var data json.RawMessage
	if res.Data != nil {
		if data, err = json.Marshal(res.Data); err != nil {
			_ = st.send(Item{Type: "error", Error: toErrorReply(err)})
			return
		}
	}
	_ = st.send(Item{Type: "result", Text: res.Text, Data: data})
}
