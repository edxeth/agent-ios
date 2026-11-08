package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/cdp"
	"github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/proxy"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/direct"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/webkitgtk"
)

// Lifecycle defaults (design 2.3).
const (
	DefaultIdle    = 15 * time.Minute
	ShutdownBudget = 5 * time.Second // cleanup of every page on shutdown
	idleEnv        = "IWDP_DAEMON_IDLE"
	// dirEnv carries the socket directory from the client that starts a
	// daemon to the daemon, so both use the same one.
	dirEnv = "IWDP_DAEMON_DIR"
	// streamDrain bounds the wait for open streams at shutdown.
	streamDrain = time.Second
	// PauseOnStart is the daemon's pause-on-start setting for every page
	// session (webkit.SessionOptions.PauseOnStart, design 2.7 item 3). The
	// design turns it on while a session has early-install parts; the
	// agent runtime is a bootstrap part of every daemon page before its
	// first connection opens, so it is always on (decision C01). Each new
	// provisional target then waits paused until its desired-state setup
	// ended, at most webkit.DefaultResumeAfter, so that the dialog override,
	// init scripts and interception exist before its first script runs.
	// WebKit drops the setting when a frontend disconnects; the session
	// sends it again on every connection, reconnects included. It is best
	// effort: an endpoint that refuses it or does not answer within a
	// second keeps the connection without it, and the daemon log says so
	// (webkit.SessionOptions.PauseOnStart). Tests that
	// build their own core use this value to run with the daemon's setting.
	PauseOnStart = true
)

// RunOptions configure Run.
type RunOptions struct {
	Dir     string        // socket directory; "" = IWDP_DAEMON_DIR, else SocketDir()
	Version string        // reported by /v1/hello
	Idle    time.Duration // idle exit; 0 = IWDP_DAEMON_IDLE, else DefaultIdle
	// Transport answers requests for "auto": iwdp (the default), direct or
	// webkitgtk. Addr is that transport's listing or inspector address
	// ("" = its default; webkitgtk needs one).
	Transport string
	Addr      string
	// StartIWDP starts ios_webkit_debug_proxy when iwdp is selected and its
	// device list does not answer (decision D10: like the legacy
	// auto-start; a running iwdp is never stopped or restarted). nil: the
	// daemon reports how to start it.
	StartIWDP func(context.Context) error
	// Manager carries timing options (tests); Run sets Transport,
	// Reservations, DaemonID, Logf, Session.PauseOnStart and Session.Logf.
	Manager core.ManagerOptions
	Log     io.Writer // daemon log; nil = stderr
	// CDPPort starts the CDP endpoint on 127.0.0.1:CDPPort at daemon start
	// (daemon flag --cdp-port, design 5.1) and is the port of a later start
	// that names none. 0: the endpoint starts only on request, on
	// cdp.DefaultPort (decision D3).
	CDPPort int
}

// Run is "daemon run": it checks the socket directory (design 2.2), serves
// the socket API until ctx ends (SIGTERM), POST /v1/shutdown, the idle
// timeout or the removal of its socket, then runs the cleanup of every
// page within ShutdownBudget and removes the socket.
func Run(ctx context.Context, o RunOptions) error {
	if o.Dir == "" {
		o.Dir = os.Getenv(dirEnv)
	}
	if o.Dir == "" {
		o.Dir = SocketDir()
	}
	if o.Idle == 0 {
		o.Idle = DefaultIdle
		if v := os.Getenv(idleEnv); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return fmt.Errorf("%s %q: want a positive duration such as 15m", idleEnv, v)
			}
			o.Idle = d
		}
	}
	switch o.Transport {
	case "":
		o.Transport = string(transport.KindIWDP)
	case string(transport.KindIWDP), string(transport.KindDirect):
	case string(transport.KindWebKitGTK):
		if o.Addr == "" {
			return fmt.Errorf("--transport webkitgtk needs --addr host:port (the WEBKIT_INSPECTOR_HTTP_SERVER address)")
		}
	default:
		return fmt.Errorf("--transport %q: want iwdp, direct or webkitgtk", o.Transport)
	}
	if o.Log == nil {
		o.Log = os.Stderr
	}
	logger := log.New(o.Log, "", log.LstdFlags|log.Lmicroseconds)
	logf := logger.Printf
	if err := EnsureDir(o.Dir); err != nil {
		return err
	}
	unlock, err := runLock(filepath.Join(o.Dir, "run.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	sock := SocketPath(o.Dir)
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the stale socket %s: %w", sock, err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", sock, err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false) // removed below, only while it is still ours
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = l.Close()
		return fmt.Errorf("socket mode: %w", err)
	}
	sockInfo, err := os.Lstat(sock)
	if err != nil {
		_ = l.Close()
		return fmt.Errorf("socket %s: %w", sock, err)
	}
	m, srv, tr := newCore(o, logf)
	defer tr.close()
	if o.CDPPort != 0 {
		if _, err := srv.cdp.Start(o.CDPPort, nil); err != nil {
			_ = l.Close()
			_ = os.Remove(sock)
			cctx, cancel := context.WithTimeout(context.Background(), ShutdownBudget)
			_ = m.Close(cctx)
			cancel()
			return fmt.Errorf("daemon --cdp-port %d: %w", o.CDPPort, err)
		}
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(l) }()
	logf("iwdp daemon %s (protocol %d) pid %d listening on %s (transport %s, idle exit %s)", o.Version, Protocol, os.Getpid(), sock, o.Transport, o.Idle)

	tick := min(o.Idle/4, time.Second)
	t := time.NewTicker(tick)
	defer t.Stop()
	var reason string
loop:
	for {
		select {
		case <-ctx.Done():
			reason = "signal"
			break loop
		case <-srv.Shutdown():
			reason = "shutdown request"
			break loop
		case err := <-served:
			reason = fmt.Sprintf("the listener failed: %v", err)
			break loop
		case now := <-t.C:
			if srv.Idle(now) >= o.Idle && m.ActiveLeases() == 0 {
				reason = fmt.Sprintf("idle for %s", o.Idle)
				break loop
			}
			if fi, err := os.Lstat(sock); err != nil || !os.SameFile(fi, sockInfo) {
				reason = "the socket was removed or replaced; no client can reach this daemon"
				break loop
			}
		}
	}
	logf("shutting down: %s", reason)
	srv.Stop() // event streams send their final item first
	cctx, cancel := context.WithTimeout(context.Background(), ShutdownBudget)
	defer cancel()
	if err := m.Close(cctx); err != nil {
		logf("cleanup: %v", err)
	}
	// Let the handlers write their last items (streams end on Stop, holds
	// with their leases) before the connections close.
	sctx, scancel := context.WithTimeout(context.Background(), streamDrain)
	_ = hs.Shutdown(sctx)
	scancel()
	_ = hs.Close()
	if fi, err := os.Lstat(sock); err == nil && os.SameFile(fi, sockInfo) {
		_ = os.Remove(sock)
	}
	logf("stopped")
	return nil
}

// newCore builds the manager and the API server of o: the daemon's, or the
// in-process core of IWDP_NO_DAEMON (design 2.1).
func newCore(o RunOptions, logf func(string, ...any)) (*core.Manager, *Server, *transports) {
	direct.SetLogger(slog.New(slog.NewTextHandler(o.Log, nil)))
	tr := &transports{o: o, logf: logf}
	res := &core.Reservations{Dir: ReservationsDir(o.Dir)}
	mo := o.Manager
	mo.Transport, mo.Reservations, mo.Logf, mo.DaemonID = tr.get, res, logf, newDaemonID()
	mo.Session.PauseOnStart, mo.Session.Logf = PauseOnStart, logf
	m := core.NewManager(mo)
	srv := NewServer(m, res, o.Version, logf)
	srv.cdp = cdp.New(cdp.Options{Manager: m, Reservations: res, Version: o.Version, Logf: logf, Enter: srv.enter, Port: o.CDPPort, Dialogs: commands.CDPDialogs})
	return m, srv, tr
}

// runLock holds an exclusive flock on path for the daemon's life, so only
// one daemon serves a directory.
func runLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon run lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("an iwdp daemon is already running for %s; use 'iwdp-cli daemon status' or 'iwdp-cli daemon stop'", filepath.Dir(path))
		}
		return nil, fmt.Errorf("daemon run lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

func newDaemonID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return hex.EncodeToString(b)
}

// transports resolves the transport of a request (core.TransportFunc).
// Transports are kept per kind and address, so iwdp's device ports and the
// direct transport's device connections are reused.
type transports struct {
	o    RunOptions
	logf func(string, ...any)

	mu     sync.Mutex
	byKey  map[string]transport.Transport
	starts sync.Mutex // one iwdp start at a time
}

func (ts *transports) get(ctx context.Context, kind, addr string) (transport.Transport, error) {
	if kind == "" || kind == "auto" {
		kind = ts.o.Transport
		if addr == "" {
			addr = ts.o.Addr
		}
	}
	key := kind + "|" + addr
	ts.mu.Lock()
	t := ts.byKey[key]
	ts.mu.Unlock()
	if t == nil {
		switch transport.Kind(kind) {
		case transport.KindIWDP:
			t = iwdp.New(iwdp.Options{ListingAddr: addr})
		case transport.KindDirect:
			d, err := direct.New(direct.Options{SocketAddress: addr})
			if err != nil {
				return nil, err
			}
			t = d
		case transport.KindWebKitGTK:
			if addr == "" {
				return nil, fmt.Errorf("the webkitgtk transport needs an inspector address; start the daemon with 'daemon run --transport webkitgtk --addr host:port'")
			}
			t = webkitgtk.New(addr)
		default:
			return nil, fmt.Errorf("unknown transport %q: want auto, iwdp, direct or webkitgtk", kind)
		}
		ts.mu.Lock()
		if ts.byKey == nil {
			ts.byKey = map[string]transport.Transport{}
		}
		if old := ts.byKey[key]; old != nil {
			t = old
		} else {
			ts.byKey[key] = t
		}
		ts.mu.Unlock()
	}
	// Explicit-URL classification (core.WithoutTransportStart) neither
	// checks nor starts iwdp: its listing read reports a dead iwdp itself.
	if it, ok := t.(*iwdp.Transport); ok && core.TransportStartAllowed(ctx) {
		if err := ts.ensureIWDP(ctx, it.ListingAddr(), addr == ""); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// ensureIWDP checks that iwdp's device list answers and, for the default
// address, starts iwdp when nothing listens there (D10). While a process
// owns the listing port, slow or silent, it never starts a second iwdp.
func (ts *transports) ensureIWDP(ctx context.Context, listing string, isDefault bool) error {
	err := proxy.CheckListing(ctx, listing)
	if !errors.Is(err, proxy.ErrNotListening) {
		return err // nil (answers) or a process owns the port
	}
	if !isDefault || ts.o.StartIWDP == nil {
		return fmt.Errorf("ios_webkit_debug_proxy is not running on %s; start it with: ios_webkit_debug_proxy --no-frontend (install: brew install ios-webkit-debug-proxy)", listing)
	}
	ts.starts.Lock()
	defer ts.starts.Unlock()
	err = proxy.CheckListing(ctx, listing)
	if !errors.Is(err, proxy.ErrNotListening) {
		return err
	}
	ts.logf("ios_webkit_debug_proxy is not running; starting it")
	if err := ts.o.StartIWDP(ctx); err != nil {
		return fmt.Errorf("ios_webkit_debug_proxy is not running and starting it failed: %w", err)
	}
	return nil
}

func (ts *transports) close() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, t := range ts.byKey {
		if c, ok := t.(io.Closer); ok {
			_ = c.Close()
		}
	}
}
