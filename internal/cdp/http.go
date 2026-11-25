package cdp

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/edxeth/agent-ios/internal/logsafe"
)

// statusRecorder keeps the status of a response for Options.Observe, and
// gives the response a write deadline when it starts: a client that stops
// reading a response holds its request for at most writeFor.
type statusRecorder struct {
	http.ResponseWriter
	status   int
	writeFor time.Duration
	logf     func(format string, args ...any)
	started  func()         // called when the response starts; nil: nothing
	hijacked func(net.Conn) // called with the connection that Hijack hands over; nil: nothing
}

// begin records the status of the first write and sets the write
// deadline of the response (net/http clears it when the response ends).
func (w *statusRecorder) begin(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	if w.started != nil {
		w.started()
	}
	if err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.writeFor)); err != nil {
		w.logf("cdp: setting the write deadline of a response: %v", logsafe.Error(err))
	}
}

func (w *statusRecorder) WriteHeader(code int) {
	w.begin(code)
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	w.begin(http.StatusOK)
	return w.ResponseWriter.Write(b)
}

// Hijack lets the WebSocket upgrade take the connection; the upgrade
// counts as status 101. begin does not run for it: Gorilla clears the
// deadlines of the connection and writes the HTTP 101 handshake itself,
// under the Upgrader's HandshakeTimeout (serveWS). The hijacked callback
// lets a revocation close the connection while that write runs (drain).
// After the upgrade, each WebSocket write sets its own deadline
// (conn.write).
func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("the response cannot be hijacked")
	}
	c, rw, err := h.Hijack()
	if err == nil {
		if w.status == 0 {
			w.status = http.StatusSwitchingProtocols
		}
		if w.hijacked != nil {
			w.hijacked(c)
		}
	}
	return c, rw, err
}

// serveHTTP serves the HTTP surface of design 5.1. The checks run in this
// order before anything else: Host (DNS rebinding guard), Origin (web
// pages), token, endpoint record. A request that fails one gets no
// discovery data.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	port, token, writeFor := s.port, s.token, s.respWrite
	allowed := s.allow[r.Header.Get("Origin")]
	s.mu.Unlock()
	rec := &statusRecorder{ResponseWriter: w, writeFor: writeFor, logf: s.o.Logf}
	defer func() {
		if s.o.Observe != nil {
			s.o.Observe(r, rec.status)
		}
	}()
	if token == "" {
		http.NotFound(rec, r) // stopped
		return
	}
	p := strconv.Itoa(port)
	if r.Host != "127.0.0.1:"+p && r.Host != "localhost:"+p {
		s.o.Logf("cdp: refused %s %s: Host header %s is not 127.0.0.1:%s or localhost:%s", logsafe.ID(logMethod(r.Method)), logsafe.ID(logPath(r.URL.Path)), logsafe.ID(clientValue(r.Host)), logsafe.ID(p), logsafe.ID(p))
		http.Error(rec, "Host header is not 127.0.0.1 or localhost with the endpoint's port", http.StatusForbidden)
		return
	}
	if _, has := r.Header["Origin"]; has && !allowed {
		s.o.Logf("cdp: refused %s %s: Origin header %s is not allowed (cdp start --allow-origin)", logsafe.ID(logMethod(r.Method)), logsafe.ID(logPath(r.URL.Path)), logsafe.ID(clientValue(r.Header.Get("Origin"))))
		http.Error(rec, "Rejected an incoming request with an Origin header; allow the origin with 'agent-ios cdp start --allow-origin'", http.StatusForbidden)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/t/")
	if !ok {
		http.NotFound(rec, r)
		return
	}
	got, rest, _ := strings.Cut(rest, "/")
	if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		http.NotFound(rec, r)
		return
	}
	rest, ok = strings.CutPrefix(rest, "e/")
	if !ok {
		http.NotFound(rec, r)
		return
	}
	id, rest, _ := strings.Cut(rest, "/")
	nc := r.Context().Value(netConnKey{}).(net.Conn) // set by ConnContext (start)
	rd, q := s.admit(id, nc)
	if rd == nil {
		http.NotFound(rec, r)
		return
	}
	defer s.leave(rd, q)
	rec.started = func() { s.writing(q) }
	rec.hijacked = func(c net.Conn) { s.hijacked(rd, q, c) }
	// The request ends with its record: a revocation cancels it, then
	// waits for it (drain).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(rd.ctx, cancel)()
	s.route(rec, r.WithContext(ctx), rd, "/"+rest)
}

// unsafeVerb is Chrome's answer to GET on an endpoint with side effects.
func unsafeVerb(w http.ResponseWriter, path string) {
	http.Error(w, fmt.Sprintf("Using unsafe HTTP verb GET to invoke %s. This action supports only PUT verb.", path), http.StatusMethodNotAllowed)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request, rec *record, path string) {
	get := r.Method == http.MethodGet
	switch {
	case path == "/json/version" || path == "/json/version/":
		if !get {
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		s.version(w, r, rec)
	case path == "/json" || path == "/json/" || path == "/json/list" || path == "/json/list/":
		if !get {
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		s.list(w, r, rec)
	case path == "/json/protocol" || path == "/json/protocol/":
		if !get {
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, protocol())
	case path == "/json/new" || strings.HasPrefix(path, "/json/close/") || strings.HasPrefix(path, "/json/activate/"):
		// No side effects on GET (design 5.1): a web page can send GETs.
		if r.Method != http.MethodPut {
			unsafeVerb(w, path)
			return
		}
		s.putAction(w, r, rec, path)
	case path == "/devtools/browser/"+rec.id:
		s.serveWS(w, r, rec, "")
	case strings.HasPrefix(path, "/devtools/page/"):
		s.serveWS(w, r, rec, strings.TrimPrefix(path, "/devtools/page/"))
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(v)
}

// version serves /json/version (design 5.1).
func (s *Server) version(w http.ResponseWriter, r *http.Request, rec *record) {
	s.mu.Lock()
	ws := s.wsBrowserLocked(rec)
	s.mu.Unlock()
	writeJSON(w, map[string]any{
		"Browser":              rec.product() + " (agent-ios " + s.o.Version + ")",
		"Protocol-Version":     "1.3",
		"User-Agent":           "",
		"V8-Version":           "",
		"WebKit-Version":       "",
		"webSocketDebuggerUrl": ws,
	})
}

// list serves /json and /json/list: one entry per page in the record's
// scope.
func (s *Server) list(w http.ResponseWriter, r *http.Request, rec *record) {
	ts, err := rec.targets(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("listing the pages: %v", err), http.StatusServiceUnavailable)
		return
	}
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, s.listEntry(rec, t))
	}
	writeJSON(w, out)
}

func (s *Server) listEntry(rec *record, t target) map[string]any {
	s.mu.Lock()
	ws := s.wsPageLocked(rec, t.pageID)
	s.mu.Unlock()
	return map[string]any{
		"description": "", "id": t.pageID, "title": t.title, "type": "page", "url": t.url,
		"webSocketDebuggerUrl": ws,
	}
}

// putAction serves PUT /json/new, /json/close/<id> and /json/activate/<id>.
func (s *Server) putAction(w http.ResponseWriter, r *http.Request, rec *record, path string) {
	ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
	defer cancel()
	switch {
	case path == "/json/new":
		// Chrome's form: /json/new?<url>, the URL without a parameter name.
		u, err := url.QueryUnescape(r.URL.RawQuery)
		if err != nil {
			http.Error(w, fmt.Sprintf("bad URL %q: %v", r.URL.RawQuery, err), http.StatusBadRequest)
			return
		}
		t, err := rec.createTarget(ctx, u)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s.listEntry(rec, t))
	case strings.HasPrefix(path, "/json/close/"):
		if err := rec.closeTarget(ctx, strings.TrimPrefix(path, "/json/close/")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("Target is closing"))
	default:
		if err := rec.activateTarget(ctx, strings.TrimPrefix(path, "/json/activate/")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("Target activated"))
	}
}

// serveWS upgrades a browser connection (pageID "") or a page connection.
func (s *Server) serveWS(w http.ResponseWriter, r *http.Request, rec *record, pageID string) {
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "WebSocket upgrade expected", http.StatusBadRequest)
		return
	}
	c := s.newConn(rec)
	if c == nil { // revoked after admission
		http.NotFound(w, r)
		return
	}
	if pageID != "" {
		// A page connection acts on one page from its first message, so it
		// attaches before the upgrade; a refused attach (another session's
		// lease, a missing reservation) is an HTTP error with the reason.
		ctx, cancel := context.WithTimeout(r.Context(), callTimeout)
		err := c.attachRoot(ctx, pageID)
		cancel()
		if err != nil {
			s.dropConn(c)
			http.Error(w, toError(err).Message, http.StatusConflict)
			return
		}
	}
	s.mu.Lock()
	writeFor := s.respWrite
	s.mu.Unlock()
	up := websocket.Upgrader{
		// The Origin check of serveHTTP ran already.
		CheckOrigin: func(*http.Request) bool { return true },
		// Bounds the write of the HTTP 101 handshake to a client that does
		// not read it, like the write of any HTTP response.
		HandshakeTimeout: writeFor,
	}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		c.close()
		s.o.Logf("cdp: WebSocket upgrade of %s failed: %v", logsafe.ID(logPath(r.URL.Path)), logsafe.Error(err))
		return
	}
	c.run(ws)
}
