package daemon

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/logsafe"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
)

// noDaemonEnv selects the in-process core (design 2.1).
const noDaemonEnv = "IWDP_NO_DAEMON"

// NoDaemon reports whether IWDP_NO_DAEMON=1 asks for the in-process core:
// the same core and socket API without a daemon process, a socket or a
// socket directory, for debugging and test isolation. It gives no leases
// across processes.
func NoDaemon() bool { return os.Getenv(noDaemonEnv) == "1" }

// inproc is the process's in-process core, started on first use; every
// client of the process shares it, so one page has one connection.
// CloseInProcess ends it.
var inproc struct {
	mu   sync.Mutex
	l    *memListener
	m    *core.Manager
	srv  *Server
	hs   *http.Server
	tr   *transports
	logf func(string, ...any)
}

// connectInProcess returns a client of the in-process core. Its API
// requests run over in-memory connections to the same Server that a daemon
// runs.
func connectInProcess(ctx context.Context, o Options) (*Client, error) {
	inproc.mu.Lock()
	if inproc.l == nil {
		ro := RunOptions{Dir: o.Dir, Version: o.Version, Transport: string(transport.KindIWDP), StartIWDP: o.StartIWDP, Log: os.Stderr}
		logger := log.New(ro.Log, "iwdp (no daemon): ", log.LstdFlags|log.Lmicroseconds)
		m, srv, tr := newCore(ro, logger.Printf)
		l := &memListener{conns: make(chan net.Conn), done: make(chan struct{})}
		hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = hs.Serve(l) }()
		inproc.l, inproc.m, inproc.srv, inproc.hs, inproc.tr, inproc.logf = l, m, srv, hs, tr, logger.Printf
	}
	l := inproc.l
	inproc.mu.Unlock()
	c := newClientDial(o.Dir, l.dial)
	h, err := c.hello(ctx)
	if err != nil {
		return nil, fmt.Errorf("starting the in-process core (%s=1): %w", noDaemonEnv, err)
	}
	c.Hello = h
	return c, nil
}

// CloseInProcess ends the in-process core of IWDP_NO_DAEMON=1, if one
// runs, as a daemon ends at shutdown (design 2.3, 2.4): event streams get
// their final item, every lease is released with the cleanup of its owned
// page state within ctx, and the page connections close. A CLI process
// calls it before it exits; without an in-process core it does
// nothing. A later Connect starts a new core.
func CloseInProcess(ctx context.Context) error {
	inproc.mu.Lock()
	l, m, srv, hs, tr, logf := inproc.l, inproc.m, inproc.srv, inproc.hs, inproc.tr, inproc.logf
	inproc.l, inproc.m, inproc.srv, inproc.hs, inproc.tr = nil, nil, nil, nil, nil
	inproc.mu.Unlock()
	if l == nil {
		return nil
	}
	srv.Stop()
	err := m.Close(ctx)
	if err != nil {
		logf("cleanup: %v", logsafe.Error(err))
	}
	sctx, cancel := context.WithTimeout(ctx, streamDrain)
	_ = hs.Shutdown(sctx)
	cancel()
	_ = hs.Close()
	_ = l.Close()
	tr.close()
	return err
}

// memListener is a net.Listener of in-memory connections (net.Pipe).
type memListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *memListener) Addr() net.Addr { return memAddr{} }

func (l *memListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type memAddr struct{}

func (memAddr) Network() string { return "memory" }
func (memAddr) String() string  { return "iwdp-in-process" }
