package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp"
	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
)

// DefaultCommandTimeout is the deadline of a request that names none
// (design 2.6, W0 default).
const DefaultCommandTimeout = 30 * time.Second

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
	exe Executable  // the executable the daemon runs (Hello); set by Run
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
		ID: s.m.DaemonID(), Version: s.version, Protocol: Protocol, Executable: s.exe, PID: os.Getpid(), StartedAt: s.started,
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
	if err == nil && inv.Help {
		// A help request runs nothing; the client prints the help: of a
		// command, of a group (no Cmd) or the overview (no Cmd). Parse has
		// checked the option values of a command (Command.Check).
		name := "iwdp-cli"
		if inv.Cmd != nil {
			name = inv.Cmd.Name
		}
		writeJSON(w, Item{Type: "error", Error: errorf(2, "%s --help: the client prints the help; the daemon runs no help request", name)})
		return
	}
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
	g.OnlyTab = req.OnlyTab
	if err := commands.CheckPageGlobals(*g); err != nil {
		writeJSON(w, Item{Type: "error", Error: toErrorReply(err)})
		return
	}
	if err := commands.CheckPageArgs(inv.Cmd, inv.Input); err != nil {
		writeJSON(w, Item{Type: "error", Error: toErrorReply(err)})
		return
	}
	if g.Session == "" {
		g.Session = "default"
	}
	deadline := req.Deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(DefaultCommandTimeout)
	}
	g.Timeout = time.Until(deadline)
	if g.Timeout <= 0 {
		// The request reached the daemon after the caller's deadline:
		// no attach, lease or queue admission happens for it.
		writeJSON(w, Item{Type: "error", Error: toErrorReply(&core.CodedError{Code: core.CodeTimeout, Err: fmt.Errorf("%s: the request reached the daemon %s after the command's deadline: %w", inv.Cmd.Name, (-g.Timeout).Round(time.Millisecond), context.DeadlineExceeded)})})
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	// Registry commands start the lease hooks (the network capture of the
	// lease, design 4.2).
	ctx = core.WithLeaseHooks(ctx)
	st := newStream(w)
	// The client names the page in a deadline error: it is sent as soon as
	// the command's operation resolved it, before the attach, the lease
	// and the queue; commands without a page that open operations of
	// their own (tab, tab new, tab close) send the page of each.
	ctx = core.WithPageObserver(ctx, func(key, title string) {
		_ = st.send(Item{Type: "page", Page: &PageInfo{Key: key, Title: title}})
	})
	env := &runEnv{s: s, req: req, st: st}
	var res commands.Result
	runCmd := func(ctx context.Context) error {
		var err error
		res, err = inv.Cmd.Run(ctx, env, inv.Input)
		return err
	}
	if req.OnlyTab && (inv.Cmd.Page != commands.NoPage || inv.Cmd.ReadsPage) {
		// --only-tab: the one tab of the listing, or no page at all, for
		// every command that takes --page (also one that resolves its
		// page itself, as get cdp-url does). A command that ignores
		// --page ignores --only-tab.
		var key string
		if key, err = s.m.OnlyTab(ctx, pageRequest(req.PageReq), acquireOptions(req.Owner)); err == nil {
			req.Page, g.Page = key, key
		}
	}
	switch {
	case err != nil:
	case inv.Cmd.Page == commands.NoPage:
		err = runCmd(ctx)
	default:
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
