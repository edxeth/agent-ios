// Package cdp is the CDP endpoint of the daemon (design
// docs/design/agentic-parity.md, section 5): a listener on 127.0.0.1 that
// lets CDP clients (agent-browser --cdp, Puppeteer, Playwright,
// chrome-devtools-mcp) drive the pages of the daemon. It translates CDP to
// WebKit Inspector Protocol calls; a method that it does not translate is
// answered by the bridge and never forwarded (design 5.4).
//
// Every HTTP path and WebSocket upgrade needs the per-listener token as a
// path prefix (/t/<token>/), and every endpoint record (5.2) has its own
// opaque id below it (/t/<token>/e/<endpoint>/...). The token reaches
// clients only through the daemon's private socket API (get cdp-url) and
// never appears in logs or status output.
package cdp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
)

// DefaultPort is the default listener port (decision D3): 9221 to 9229
// belong to iwdp, and 9222 is often a local Chrome.
const DefaultPort = 9230

// Scopes of an endpoint record (design 5.2).
const (
	ScopePage   = "page"   // the selected page only (default)
	ScopeDevice = "device" // also the pages of the devices reserved by the record's reservation owner
)

// Timing defaults.
const (
	// DefaultPollEvery is how often the page list is read while a client
	// discovers or auto-attaches targets (design 5.3, Target row).
	DefaultPollEvery = 2 * time.Second
	// callTimeout bounds one CDP command, Later included.
	callTimeout = 60 * time.Second
	// cleanupTimeout bounds the release of an attachment's state.
	cleanupTimeout = 5 * time.Second
	// writeTimeout bounds one WebSocket write to a client, and the write
	// of one HTTP response from its first byte.
	writeTimeout = 10 * time.Second
	// readLimit is the largest client message (Playwright's injected
	// script is about 330 KB, proof P2).
	readLimit = 64 << 20
)

// Options configure a Server.
type Options struct {
	Manager      *core.Manager
	Reservations *core.Reservations // device scope; nil: no reserved devices
	Version      string             // iwdp-mcp version for /json/version
	Logf         func(format string, args ...any)
	// Enter marks a client connection as daemon activity (idle exit,
	// design 2.3) until the returned function runs. nil: none.
	Enter func() func()
	// Observe sees every HTTP request with the status it got (101 for an
	// upgrade). Tests use it; nil: none.
	Observe   func(r *http.Request, status int)
	PollEvery time.Duration
	// Port is the port of StartDefault (the daemon flag --cdp-port);
	// 0: DefaultPort.
	Port int
	// Dialogs is the dialog bridge of the logical sessions (design 3.7,
	// 5.2 "Dialogs"); the daemon sets internal/commands CDPDialogs. nil:
	// none (tests of other domains), so a dialog that an Input event
	// opens blocks the page until the command deadline.
	Dialogs Dialogs
}

// Server is the CDP endpoint of one daemon.
type Server struct {
	o Options

	mu      sync.Mutex
	l       net.Listener
	hs      *http.Server
	port    int
	token   string
	allow   map[string]bool
	records map[string]*record // by endpoint id
	conns   map[*conn]bool
	// respWrite bounds the write of one HTTP response (writeTimeout;
	// tests shorten it).
	respWrite time.Duration
	// unknown counts unknown methods by name (cdp log); seen keeps the
	// CDP sessions that logged a method already (design 5.4: once per
	// CDP session).
	unknown map[string]int
	seen    map[string]bool

	// claims holds the Fetch patterns of the CDP attachments of each page
	// (fetch_domain.go).
	claims fetchClaims

	// known replaces the current token and endpoint ids in log lines. It
	// is rebuilt under mu (knownChangedLocked) and read without it, so
	// that lines logged under mu are redacted too.
	known atomic.Pointer[strings.Replacer]
}

// New returns a stopped server. Every line it logs through Options.Logf is
// redacted first (redact).
func New(o Options) *Server {
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if o.PollEvery <= 0 {
		o.PollEvery = DefaultPollEvery
	}
	s := &Server{o: o, records: map[string]*record{}, conns: map[*conn]bool{}, unknown: map[string]int{}, seen: map[string]bool{}, respWrite: writeTimeout}
	s.o.Logf = func(format string, args ...any) { logf("%s", s.redact(fmt.Sprintf(format, args...))) }
	return s
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return hex.EncodeToString(b)
}

// tokenBytes is the token size: 256 bits (design 5.1).
const tokenBytes = 32

// Start listens on 127.0.0.1:port (0 picks a free port) with a new token
// and returns the address. allowOrigins lists the Origin header values
// that are accepted (empty by default: every request with an Origin header
// is refused). When the listener runs already, Start keeps it and fails
// only for another port or other allowed origins.
func (s *Server) Start(port int, allowOrigins []string) (string, error) {
	if port < 0 || port > 65535 {
		return "", fmt.Errorf("CDP port %d: want 0 to 65535", port)
	}
	return s.start(port, allowOrigins)
}

// StartDefault is Start on the configured port (Options.Port, else
// DefaultPort) when the listener does not run; a running listener is kept
// on its port (cdp start without --port, get cdp-url).
func (s *Server) StartDefault(allowOrigins []string) (string, error) {
	return s.start(-1, allowOrigins)
}

// start is Start; port -1 keeps a running listener on any port and
// otherwise starts on the configured port.
func (s *Server) start(port int, allowOrigins []string) (string, error) {
	allow := map[string]bool{}
	for _, o := range allowOrigins {
		if o = strings.TrimSpace(o); o != "" {
			allow[o] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.l != nil {
		if port > 0 && port != s.port {
			return "", fmt.Errorf("the CDP endpoint already listens on 127.0.0.1:%d; run 'iwdp-cli cdp stop' first to move it to port %d", s.port, port)
		}
		if len(allow) > 0 && !maps.Equal(allow, s.allow) {
			return "", fmt.Errorf("the CDP endpoint already runs on 127.0.0.1:%d with %d other allowed origin(s); run 'iwdp-cli cdp stop' first to change them", s.port, len(s.allow))
		}
		return s.addrLocked(), nil
	}
	if port < 0 {
		port = s.o.Port
		if port == 0 {
			port = DefaultPort
		}
	}
	// Loopback only (design 5.1): never a wildcard address.
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return "", fmt.Errorf("starting the CDP endpoint on 127.0.0.1:%d: %w; pick another port with --port", port, err)
	}
	s.l = l
	s.port = l.Addr().(*net.TCPAddr).Port
	s.token = randomHex(tokenBytes)
	s.knownChangedLocked()
	s.allow = allow
	s.hs = &http.Server{
		Handler: http.HandlerFunc(s.serveHTTP), ReadHeaderTimeout: 10 * time.Second,
		// Each request knows its network connection, so that a revocation
		// can close it (admit, drain).
		ConnContext: func(ctx context.Context, nc net.Conn) context.Context {
			return context.WithValue(ctx, netConnKey{}, nc)
		},
	}
	go func(hs *http.Server) { _ = hs.Serve(l) }(s.hs)
	s.o.Logf("cdp: listening on %s (allowed origins: %d)", s.addrLocked(), len(s.allow))
	return s.addrLocked(), nil
}

func (s *Server) addrLocked() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port)) }

// Running reports whether the listener runs.
func (s *Server) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.l != nil
}

// Stop closes the listener and every client connection (each runs its
// cleanup first) and revokes every endpoint record: their URLs get 404,
// and a later Start uses a new token. It returns after the requests
// admitted on the records ended.
func (s *Server) Stop() (records, conns int) {
	s.mu.Lock()
	hs := s.hs
	s.l, s.hs, s.token, s.port = nil, nil, "", 0
	gone := make([]*record, 0, len(s.records))
	for _, rec := range s.records {
		gone = append(gone, rec)
		s.retireLocked(rec)
	}
	s.knownChangedLocked()
	cs := s.connsLocked(nil)
	s.mu.Unlock()
	if hs != nil {
		_ = hs.Close()
		s.o.Logf("cdp: stopped (%d endpoint record(s) revoked, %d connection(s) closed)", len(gone), len(cs))
	}
	closeAll(cs)
	s.drain(gone)
	return len(gone), len(cs)
}

// retireLocked revokes rec: its id no longer admits requests or
// connections (admit, newConn), and the requests admitted on it are
// cancelled. The caller closes its connections and then drains it.
func (s *Server) retireLocked(rec *record) {
	delete(s.records, rec.id)
	rec.cancel()
}

// drain waits until the requests admitted on the revoked records ended
// (WebSocket upgrades included), so that nothing acts through them after
// the revocation returns (design 5.2: a session release after it is final).
// Their contexts are cancelled already (retireLocked), and their
// WebSocket connections closed after cleanup (closeAll). drain closes the
// network connection of each request whose response has started (for an
// upgrade, from its hijack until the end of the request: hijacked): such a
// request waits only for its client, which may have stopped reading. A
// request whose response has not started gets its answer (a cancelled
// action's error, 404 for an upgrade), and the write deadline bounds that
// write (statusRecorder). The page work of a request still ends before
// drain returns: closing its connection only ends its I/O.
func (s *Server) drain(recs []*record) {
	for _, rec := range recs {
		s.mu.Lock()
		var writing []net.Conn
		for q := range rec.inflight {
			if q.writing {
				writing = append(writing, q.nc)
			}
		}
		s.mu.Unlock()
		for _, nc := range writing {
			_ = nc.Close() // the write fails and the request ends; nothing to report
		}
		rec.work.Wait()
	}
}

// connsLocked returns the connections of rec (all when rec is nil).
func (s *Server) connsLocked(match func(*record) bool) []*conn {
	var out []*conn
	for c := range s.conns {
		if match == nil || match(c.rec) {
			out = append(out, c)
		}
	}
	return out
}

func closeAll(cs []*conn) {
	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.close()
		}()
	}
	wg.Wait()
}

// IssueRequest asks for the endpoint record of a logical session (design
// 5.2, get cdp-url).
type IssueRequest struct {
	Session     string           // logical session; "" = "default"
	Reservation string           // reservation owner; "" = the session
	Page        core.PageRequest // the session's page selection, resolved like any command's
	Scope       string           // ScopePage ("" too) or ScopeDevice
}

// Endpoint is an issued record's two URL forms (design 5.1). They carry
// the token: only the private socket API returns them.
type Endpoint struct {
	WS    string `json:"ws"`   // browser WebSocket URL: the form for every client
	HTTP  string `json:"http"` // discovery base, for clients that append /json/version
	Scope string `json:"scope"`
	Page  string `json:"page"` // title of the selected page
}

// Issue resolves the session's page and returns its endpoint record,
// creating it unless the same session, page and scope have one. Issuing
// takes no lease, so it also works for a page that another session
// holds; attaching then fails with the lease message (design 5.2).
func (s *Server) Issue(ctx context.Context, r IssueRequest) (Endpoint, error) {
	if r.Session == "" {
		r.Session = "default"
	}
	if r.Reservation == "" {
		r.Reservation = r.Session
	}
	switch r.Scope {
	case "":
		r.Scope = ScopePage
	case ScopePage, ScopeDevice:
	default:
		return Endpoint{}, fmt.Errorf("scope %q: want page or device", r.Scope)
	}
	if !s.Running() {
		return Endpoint{}, errors.New("the CDP endpoint is not running; run 'iwdp-cli cdp start'")
	}
	lp, err := s.o.Manager.ResolvePage(ctx, r.Page)
	if err != nil {
		return Endpoint{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.l == nil {
		return Endpoint{}, errors.New("the CDP endpoint stopped while the URL was issued; run 'iwdp-cli cdp start'")
	}
	var rec *record
	for _, x := range s.records {
		if x.session == r.Session && x.selected.Key == lp.Key && x.scope == r.Scope {
			rec = x
			break
		}
	}
	if rec == nil {
		rec = &record{
			s: s, id: randomHex(16), session: r.Session, scope: r.Scope,
			listing: core.PageRequest{Transport: r.Page.Transport, Addr: r.Page.Addr}, selected: lp,
		}
		rec.ctx, rec.cancel = context.WithCancel(context.Background())
		s.records[rec.id] = rec
		s.knownChangedLocked()
		s.o.Logf("cdp: issued an endpoint for session %q, page %q, scope %s", rec.session, lp.Title, rec.scope)
	}
	rec.setOwner(r.Reservation)
	return Endpoint{WS: s.wsBrowserLocked(rec), HTTP: s.httpBaseLocked(rec), Scope: rec.scope, Page: lp.Title}, nil
}

func (s *Server) baseLocked(rec *record) string { return "/t/" + s.token + "/e/" + rec.id }

func (s *Server) httpBaseLocked(rec *record) string {
	return "http://" + s.addrLocked() + s.baseLocked(rec)
}

func (s *Server) wsBrowserLocked(rec *record) string {
	return "ws://" + s.addrLocked() + s.baseLocked(rec) + "/devtools/browser/" + rec.id
}

func (s *Server) wsPageLocked(rec *record, targetID string) string {
	return "ws://" + s.addrLocked() + s.baseLocked(rec) + "/devtools/page/" + targetID
}

// RevokeSession ends the endpoint records of a logical session when the
// session ends (close, session release; design 5.2): their connections
// close after their cleanup, and their URLs get 404. It returns the number
// of records revoked, after the requests admitted on them ended.
func (s *Server) RevokeSession(session string) int {
	s.mu.Lock()
	var gone []*record
	for _, rec := range s.records {
		if rec.session == session {
			gone = append(gone, rec)
			s.retireLocked(rec)
		}
	}
	s.knownChangedLocked()
	cs := s.connsLocked(func(r *record) bool { return r.session == session })
	s.mu.Unlock()
	closeAll(cs)
	s.drain(gone)
	if len(gone) > 0 {
		s.o.Logf("cdp: revoked %d endpoint record(s) of session %q", len(gone), session)
	}
	return len(gone)
}

// netConnKey is the request context key of the request's network
// connection (http.Server.ConnContext).
type netConnKey struct{}

// request is an HTTP request admitted on a record.
type request struct {
	nc      net.Conn // the request's network connection
	writing bool     // the response has started (guarded by Server.mu)
}

// admit returns the live record of an endpoint id (under a valid token),
// counts one request on it in rec.work and keeps it in rec.inflight; the
// caller calls s.leave when the request ends. It returns nil for an
// unknown or revoked id. Admission and revocation share s.mu, so a
// revocation waits for every request admitted before it and can close its
// connection (drain).
func (s *Server) admit(id string, nc net.Conn) (*record, *request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.records[id]
	if rec == nil {
		return nil, nil
	}
	rec.work.Add(1)
	if rec.inflight == nil {
		rec.inflight = map[*request]bool{}
	}
	q := &request{nc: nc}
	rec.inflight[q] = true
	return rec, q
}

// writing marks the response of q as started.
func (s *Server) writing(q *request) {
	s.mu.Lock()
	q.writing = true
	s.mu.Unlock()
}

// hijacked marks the response of q as started when a WebSocket upgrade
// took over its connection nc (statusRecorder.Hijack). Until the
// connection loop owns the WebSocket (conn.run), only the HTTP 101
// handshake write uses nc, and a revocation does not see it as a
// connection of rec (closeAll); so drain closes nc like the connection of
// any started response. When rec was revoked already, drain may have read
// its requests before q started, so nc closes at once and the upgrade
// fails.
func (s *Server) hijacked(rec *record, q *request, nc net.Conn) {
	s.mu.Lock()
	q.nc, q.writing = nc, true
	revoked := s.records[rec.id] != rec
	s.mu.Unlock()
	if revoked {
		_ = nc.Close() // the handshake write fails; serveWS logs the failed upgrade
	}
}

// leave ends a request that admit counted.
func (s *Server) leave(rec *record, q *request) {
	s.mu.Lock()
	delete(rec.inflight, q)
	s.mu.Unlock()
	rec.work.Done()
}

// noteUnknown counts an unknown method and logs it once per CDP session
// (design 5.4).
func (s *Server) noteUnknown(sessionKey, method string) {
	method = cdpMethodName(method)
	s.mu.Lock()
	s.unknown[method]++
	first := !s.seen[sessionKey+" "+method]
	s.seen[sessionKey+" "+method] = true
	s.mu.Unlock()
	if first {
		s.o.Logf("cdp: unknown method %s on %s (answered \"wasn't found\", not forwarded)", method, sessionKey)
	}
}

// UnknownCount is the number of times a client sent an unknown method.
type UnknownCount struct {
	Method string `json:"method"`
	Count  int    `json:"count"`
}

// Unknown returns the unknown-method counts, most frequent first.
func (s *Server) Unknown() []UnknownCount {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]UnknownCount, 0, len(s.unknown))
	for m, n := range s.unknown {
		out = append(out, UnknownCount{m, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// Status describes the endpoint without the token or endpoint ids.
type Status struct {
	Listening    bool             `json:"listening"`
	Addr         string           `json:"addr,omitempty"`
	AllowOrigins []string         `json:"allowOrigins,omitempty"`
	Endpoints    []EndpointStatus `json:"endpoints"`
	Connections  int              `json:"connections"`
}

// EndpointStatus is one endpoint record in Status.
type EndpointStatus struct {
	Session     string `json:"session"`
	Scope       string `json:"scope"`
	Page        string `json:"page"`
	Connections int    `json:"connections"`
	Attachments int    `json:"attachments"`
}

// Status returns the listener state and the endpoint records.
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Listening: s.l != nil, Endpoints: []EndpointStatus{}, Connections: len(s.conns)}
	if s.l != nil {
		st.Addr = s.addrLocked()
	}
	for o := range s.allow {
		st.AllowOrigins = append(st.AllowOrigins, o)
	}
	sort.Strings(st.AllowOrigins)
	for _, rec := range s.records {
		es := EndpointStatus{Session: rec.session, Scope: rec.scope, Page: rec.selected.Title}
		for c := range s.conns {
			if c.rec == rec {
				es.Connections++
				es.Attachments += c.attachments()
			}
		}
		st.Endpoints = append(st.Endpoints, es)
	}
	sort.Slice(st.Endpoints, func(i, j int) bool {
		a, b := st.Endpoints[i], st.Endpoints[j]
		if a.Session != b.Session {
			return a.Session < b.Session
		}
		if a.Page != b.Page {
			return a.Page < b.Page
		}
		return a.Scope < b.Scope
	})
	return st
}

// secretSegment matches the URL path segments that carry a secret: the
// token after /t/, the endpoint id after /e/ and after /devtools/browser/.
var secretSegment = regexp.MustCompile(`(/t/|/e/|/devtools/browser/)[^/\s"'?#]+`)

// redact removes the token and endpoint ids from text (log lines). Every
// secret path segment is redacted whether or not its token or record is
// still known, so requests to revoked records or to a previous listener
// never log theirs; the current token and ids are also replaced wherever
// else they appear.
func (s *Server) redact(text string) string {
	if r := s.known.Load(); r != nil {
		text = r.Replace(text)
	}
	return secretSegment.ReplaceAllStringFunc(text, func(m string) string {
		switch {
		case strings.HasPrefix(m, "/t/"):
			return "/t/<token>"
		case strings.HasPrefix(m, "/e/"):
			return "/e/<endpoint>"
		}
		return "/devtools/browser/<endpoint>"
	})
}

// Values that a client chooses (header values, the request method, path
// segments, CDP method names) are never logged raw (design 5.1): a client
// can send a token or endpoint id in any of them, current or retired, and
// redact knows only the current ones and the secret path positions. A log
// line shows a short hash instead, which tells equal values apart.

// hashed is the log form of a value v that a client chose: kind and the
// first 8 hex digits of the SHA-256 of v.
func hashed(kind, v string) string {
	sum := sha256.Sum256([]byte(v))
	return "<" + kind + " " + hex.EncodeToString(sum[:4]) + ">"
}

// clientValue is the log form of a header value.
func clientValue(v string) string { return hashed("value", v) }

// logMethod is the log form of an HTTP request method: a standard method
// as is, any other hashed.
func logMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return m
	}
	return hashed("method", m)
}

// routeWords are the path segments of the HTTP routes (route) and of the
// secret prefix, logged as they are.
var routeWords = map[string]bool{
	"t": true, "e": true, "json": true, "version": true, "list": true, "protocol": true,
	"new": true, "close": true, "activate": true, "devtools": true, "browser": true, "page": true,
}

// logPath is the log form of a request path: the route words as they are,
// the segment after t, e and browser as the redact markers, and every
// other segment hashed.
func logPath(p string) string {
	segs := strings.Split(p, "/")
	prev := ""
	for i, seg := range segs {
		switch {
		case seg == "":
		case prev == "t":
			segs[i] = "<token>"
		case prev == "e" || prev == "browser":
			segs[i] = "<endpoint>"
		case !routeWords[seg]:
			segs[i] = hashed("value", seg)
		}
		prev = seg
	}
	return strings.Join(segs, "/")
}

// cdpMethodName is the log and count form of a CDP method that a client
// sent: a Domain.method name of letters as is, anything else hashed. A
// token or endpoint id is a run of at least 32 hex digits, so a kept name
// never contains one.
func cdpMethodName(m string) string {
	if cdpMethodShape.MatchString(m) && !hexRun.MatchString(m) {
		return m
	}
	return hashed("method", m)
}

var (
	cdpMethodShape = regexp.MustCompile(`^[A-Za-z]+\.[A-Za-z]+$`)
	hexRun         = regexp.MustCompile(`[0-9A-Fa-f]{32}`)
)

// knownChangedLocked rebuilds the replacer of the current token and
// endpoint ids after a change of either.
func (s *Server) knownChangedLocked() {
	var pairs []string
	if s.token != "" {
		pairs = append(pairs, s.token, "<token>")
	}
	for id := range s.records {
		pairs = append(pairs, id, "<endpoint>")
	}
	s.known.Store(strings.NewReplacer(pairs...))
}

// stableID is an opaque CDP target id derived from parts (design 5.2: the
// page targetId comes from the device id and the transport page id, so it
// is stable for the life of the tab and hides WebKit target swaps).
func stableID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return strings.ToUpper(hex.EncodeToString(sum[:16]))
}
