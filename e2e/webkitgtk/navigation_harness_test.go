//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/daemon"
	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/transport/webkitgtk"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// W3b-2a suite: navigation, waits across navigations, dialogs, early
// install, annotated screenshots, tabs, frames and find actions on
// WebKitGTK (design docs/design/agentic-parity.md, 7.6 items 6 to 11).
// Every test runs its own MiniBrowser and its own daemon core in this
// process: a core.Manager and daemon.Server on a private socket, whose
// transport wraps each page connection in a protocolTap. The tap logs
// every message and is the daemon test hook of 7.6 item 7: it drops a
// chosen reply and runs a probe between the two phases of reload. It can
// also drop chosen events and close the page connections without a request
// from the daemon.

// navSite is one of the two sites of the fixture server.
type navSite struct{ a, b string } // http://127.0.0.1:N, http://localhost:M

// recorded is one dialog result that a fixture document reported.
type recorded struct {
	Doc, Type, Result, Origin string
}

type navEnv struct {
	t     *testing.T
	sites navSite
	tap   *tapHooks
	c     *daemon.Client
	m     *core.Manager

	recMu    sync.Mutex
	records  []recorded
	slowDone time.Time // when /slow last answered
}

// navFixtures serves e2e/fixtures/navigation on 127.0.0.1 and localhost
// (two sites, so a navigation between them swaps the process, proof P1).
func navFixtures(t *testing.T, e *navEnv) navSite {
	t.Helper()
	dir := filepath.Join(fixtureDir(), "..", "navigation")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(dir)))
	// /slow?ms=N answers after N milliseconds (wait --load networkidle).
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		e.recMu.Lock()
		e.slowDone = time.Now()
		e.recMu.Unlock()
		_, _ = w.Write([]byte("slow"))
	})
	// /nocontent answers 204 No Content: a navigation to it leaves the
	// current document in place (TestAbandonedProvisionalTarget).
	mux.HandleFunc("/nocontent", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/record", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		e.recMu.Lock()
		e.records = append(e.records, recorded{q.Get("doc"), q.Get("type"), q.Get("result"), q.Get("origin")})
		e.recMu.Unlock()
	})
	serve := func(host string) string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: mux}
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(func() { _ = srv.Close() })
		return "http://" + strings.Replace(l.Addr().String(), "127.0.0.1", host, 1)
	}
	return navSite{a: serve("127.0.0.1"), b: serve("localhost")}
}

func (e *navEnv) recordsOf(typ string) []recorded {
	e.recMu.Lock()
	defer e.recMu.Unlock()
	var out []recorded
	for _, r := range e.records {
		if r.Type == typ {
			out = append(out, r)
		}
	}
	return out
}

// newNavEnv starts MiniBrowser on page (a path on site a) and a daemon core
// for it; pause turns on pause-on-start for the page sessions (2.7).
func newNavEnv(t *testing.T, page string, pause bool) *navEnv {
	t.Helper()
	e := &navEnv{t: t, tap: &tapHooks{}}
	e.sites = navFixtures(t, e)
	shim, err := buildDPIShim(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	display := broadway(t)
	addr := freeAddr(t)
	startProcess(t, []string{"GDK_BACKEND=broadway", "BROADWAY_DISPLAY=" + display, "WEBKIT_INSPECTOR_HTTP_SERVER=" + addr, "LD_PRELOAD=" + shim},
		miniBrowser, e.sites.a+"/"+page)
	tr := webkitgtk.New(addr)
	for end := time.Now().Add(browserStart); ; time.Sleep(200 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		pages, err := tr.Pages(ctx, "webkitgtk-"+addr)
		cancel()
		if err == nil && len(pages) > 0 && pages[0].Title != "" {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("MiniBrowser did not list %s at %s within %v", page, addr, browserStart)
		}
	}
	dir, err := os.MkdirTemp("", "w3bnav")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := daemon.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	res := &core.Reservations{Dir: daemon.ReservationsDir(dir)}
	e.m = core.NewManager(core.ManagerOptions{
		Transport: func(context.Context, string, string) (transport.Transport, error) {
			return &tapTransport{Transport: tr, hooks: e.tap}, nil
		},
		Reservations: res,
		Session:      webkit.SessionOptions{TargetWait: 12 * time.Second, PauseOnStart: pause},
		DaemonID:     "w3b-nav-test",
		Logf:         t.Logf,
	})
	srv := daemon.NewServer(e.m, res, "test", t.Logf)
	l, err := net.Listen("unix", daemon.SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go func() { _ = hs.Serve(l) }()
	t.Cleanup(func() {
		srv.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.m.Close(ctx)
		_ = hs.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e.c, err = daemon.Connect(ctx, daemon.Options{Dir: dir, NoStart: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.c.Close)
	return e
}

// run runs a registry command line in the daemon for session; code is
// the exit code (0 on success).
func (e *navEnv) run(session string, argv ...string) (string, int, string) {
	e.t.Helper()
	out, code, msg, err := e.runAs(daemon.Owner{Session: session}, argv...)
	if err != nil {
		e.t.Fatalf("%v: %v", argv, err)
	}
	return out, code, msg
}

// runAs is run with a full owner (for example with Takeover). It does not
// stop the test, so other goroutines may call it; err is a failure to talk
// to the daemon, not a command error.
func (e *navEnv) runAs(owner daemon.Owner, argv ...string) (string, int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	timeout := 30 * time.Second
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--timeout" {
			d, err := time.ParseDuration(argv[i+1])
			if err != nil {
				return "", 0, "", err
			}
			timeout = d
			argv = append(append([]string{}, argv[:i]...), argv[i+2:]...)
			break
		}
	}
	r, err := e.c.Run(ctx, daemon.RunRequest{Argv: argv, Deadline: time.Now().Add(timeout), Owner: owner}, nil)
	if err != nil {
		var re *daemon.RemoteError
		if errors.As(err, &re) {
			return "", re.E.Code, re.E.Message, nil
		}
		return "", 0, "", err
	}
	return r.Text, 0, "", nil
}

func (e *navEnv) must(session string, argv ...string) string {
	e.t.Helper()
	out, code, msg := e.run(session, argv...)
	if code != 0 {
		e.t.Fatalf("%s: exit %d: %s", strings.Join(argv, " "), code, msg)
	}
	return out
}

// js evaluates expr in session's current page, in an operation of the
// daemon core, and returns its value as JSON.
func (e *navEnv) js(session, expr string) string {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out string
	err := e.m.Do(ctx, core.PageRequest{}, core.AcquireOptions{Session: session}, "test eval", core.ClassRead, false,
		func(ctx context.Context, mo *core.ManagedOp) error {
			raw, err := mo.Op.Caller().Send(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
			if err != nil {
				return err
			}
			var r struct {
				Result struct {
					Value       json.RawMessage `json:"value"`
					Description string          `json:"description"`
				} `json:"result"`
				WasThrown bool `json:"wasThrown"`
			}
			if err := json.Unmarshal(raw, &r); err != nil || r.WasThrown {
				return fmt.Errorf("%s: %s", expr, raw)
			}
			out = string(r.Result.Value)
			return nil
		})
	if err != nil {
		e.t.Fatalf("evaluate %q: %v", expr, err)
	}
	return out
}

// ---- protocol tap (the daemon test hook) ----

type tapHooks struct {
	mu   sync.Mutex
	log  []tapRecord
	seq  int
	drop func(method string, params json.RawMessage) bool // drop the reply of the next matching call
	// dropReply drops every reply whose result it matches; dropEvent drops
	// every event it matches. Both stay set until cleared.
	dropReply func(result json.RawMessage) bool
	dropEvent func(method string, params json.RawMessage) bool
	conns     []*tapConn // every page connection opened, for closeConns
	// beforeReload runs before Page.reload is written, on the writer's
	// goroutine, with a probe that evaluates in the page.
	beforeReload func(probe func(expr string) (string, error))
	dropIDs      map[int64]bool
}

type tapRecord struct {
	Seq    int
	Out    bool
	ID     int64
	Method string
	Params string
	Reply  bool
	Conn   int    // the page connection of a sent command (tapConn.n)
	Target string // the target of a sent command
}

// withoutAgentResets drops the agent resets from rs: before the first
// <Domain>.enable that a webkit.Session sends to a target whose agent
// state it does not know, it sends the agent's reset, <Domain>.disable
// (core resetCalls). Each sent <Domain>.disable before the domain's first
// sent enable on its connection and target is such a reset; the other
// records are the domain owners' calls.
func withoutAgentResets(rs []tapRecord) []tapRecord {
	type place struct {
		conn           int
		target, domain string
	}
	enabled := map[place]bool{}
	var out []tapRecord
	for _, r := range rs {
		dom, verb, _ := strings.Cut(r.Method, ".")
		p := place{r.Conn, r.Target, dom}
		switch {
		case r.Out && verb == "enable":
			enabled[p] = true
		case r.Out && verb == "disable" && !enabled[p]:
			continue
		}
		out = append(out, r)
	}
	return out
}

func (h *tapHooks) add(r tapRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	r.Seq = h.seq
	h.log = append(h.log, r)
}

func (h *tapHooks) records() []tapRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]tapRecord(nil), h.log...)
}

// closeConns closes every page connection without a request from the
// daemon, as another inspector or the device would (design 2.5 rule 3).
func (h *tapHooks) closeConns() {
	h.mu.Lock()
	conns := h.conns
	h.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (h *tapHooks) set(f func(*tapHooks)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

type tapTransport struct {
	transport.Transport
	hooks *tapHooks
}

func (t *tapTransport) Open(ctx context.Context, p transport.Page) (transport.MessageConn, error) {
	c, err := t.Transport.Open(ctx, p)
	if err != nil {
		return nil, err
	}
	tc := &tapConn{MessageConn: c, h: t.hooks, probes: map[int64]chan string{}}
	t.hooks.mu.Lock()
	tc.n = len(t.hooks.conns) + 1
	t.hooks.conns = append(t.hooks.conns, tc)
	t.hooks.mu.Unlock()
	return tc, nil
}

type tapConn struct {
	transport.MessageConn
	h      *tapHooks
	n      int // 1 for the first page connection, and so on
	mu     sync.Mutex
	target string
	probes map[int64]chan string // injected calls by inner id
	next   int64
}

type outer struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type innerMsg struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

const probeBase = 900000 // inner ids of injected probes

func (c *tapConn) Write(ctx context.Context, msg []byte) error {
	var o outer
	_ = json.Unmarshal(msg, &o)
	if o.Method == "Target.sendMessageToTarget" {
		var p struct {
			TargetID string `json:"targetId"`
			Message  string `json:"message"`
		}
		_ = json.Unmarshal(o.Params, &p)
		var in innerMsg
		_ = json.Unmarshal([]byte(p.Message), &in)
		c.mu.Lock()
		c.target = p.TargetID
		c.mu.Unlock()
		c.h.mu.Lock()
		drop := c.h.drop != nil && c.h.drop(in.Method, in.Params)
		if drop {
			c.h.drop = nil
			if c.h.dropIDs == nil {
				c.h.dropIDs = map[int64]bool{}
			}
			c.h.dropIDs[in.ID] = true
		}
		before := c.h.beforeReload
		if in.Method == "Page.reload" {
			c.h.beforeReload = nil
		}
		c.h.mu.Unlock()
		if in.Method == "Page.reload" && before != nil {
			before(func(expr string) (string, error) { return c.probe(ctx, p.TargetID, expr) })
		}
		c.h.add(tapRecord{Out: true, ID: in.ID, Method: in.Method, Params: string(in.Params), Conn: c.n, Target: p.TargetID})
	}
	return c.MessageConn.Write(ctx, msg)
}

// probe evaluates expr in the page on this connection, out of band.
func (c *tapConn) probe(ctx context.Context, target, expr string) (string, error) {
	c.mu.Lock()
	c.next++
	id := probeBase + c.next
	ch := make(chan string, 1)
	c.probes[id] = ch
	c.mu.Unlock()
	inner, _ := json.Marshal(map[string]any{"id": id, "method": "Runtime.evaluate", "params": map[string]any{"expression": expr, "returnByValue": true}})
	msg, _ := json.Marshal(map[string]any{"id": id, "method": "Target.sendMessageToTarget", "params": map[string]string{"targetId": target, "message": string(inner)}})
	if err := c.MessageConn.Write(ctx, msg); err != nil {
		return "", err
	}
	select {
	case v := <-ch:
		return v, nil
	case <-time.After(5 * time.Second):
		return "", errors.New("probe: no reply in 5 s")
	}
}

func (c *tapConn) Read(ctx context.Context) ([]byte, error) {
	for {
		msg, err := c.MessageConn.Read(ctx)
		if err != nil {
			return msg, err
		}
		var o outer
		_ = json.Unmarshal(msg, &o)
		if o.ID >= probeBase {
			continue // the outer ack of a probe
		}
		if o.Method != "Target.dispatchMessageFromTarget" {
			if o.Method != "" {
				c.h.add(tapRecord{Method: o.Method, Params: string(o.Params)})
			}
			return msg, nil
		}
		var p struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(o.Params, &p)
		var in innerMsg
		_ = json.Unmarshal([]byte(p.Message), &in)
		if in.ID >= probeBase {
			c.mu.Lock()
			ch := c.probes[in.ID]
			c.mu.Unlock()
			if ch != nil {
				var r struct {
					Result struct {
						Value json.RawMessage `json:"value"`
					} `json:"result"`
				}
				_ = json.Unmarshal(in.Result, &r)
				ch <- string(r.Result.Value)
			}
			continue
		}
		c.h.mu.Lock()
		dropped := in.ID != 0 && c.h.dropIDs[in.ID]
		if in.ID != 0 && c.h.dropReply != nil && c.h.dropReply(in.Result) {
			dropped = true
		}
		if in.ID == 0 && c.h.dropEvent != nil && c.h.dropEvent(in.Method, in.Params) {
			dropped = true
		}
		c.h.mu.Unlock()
		c.h.add(tapRecord{ID: in.ID, Method: in.Method, Reply: in.ID != 0, Params: map[bool]string{true: "dropped"}[dropped]})
		if dropped {
			continue
		}
		return msg, nil
	}
}

// evalWith reports whether method is Runtime.evaluate of an expression
// that contains every part.
func evalWith(parts ...string) func(string, json.RawMessage) bool {
	return func(method string, params json.RawMessage) bool {
		if method != "Runtime.evaluate" {
			return false
		}
		var p struct{ Expression string }
		_ = json.Unmarshal(params, &p)
		for _, s := range parts {
			if !strings.Contains(p.Expression, s) {
				return false
			}
		}
		return true
	}
}
