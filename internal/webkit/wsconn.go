package webkit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
)

// wsConn is a transport.MessageConn over one WebSocket connection.
type wsConn struct {
	conn   *websocket.Conn
	wire   *cutConn // the network connection under conn (interruptWrites)
	limits transport.Limits
	// writeSem holds one token while a goroutine writes (gorilla/websocket is
	// not concurrent-write-safe). A channel, so Write can stop waiting for it
	// when its context ends.
	writeSem  chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// DialWebSocket opens a WebSocket MessageConn to wsURL. The handshake is
// bounded by DialTimeout and by ctx; a handshake that ctx's deadline ends
// fails as that deadline (deadlineCaused). A zero limits.MaxIncoming means
// transport.DefaultMaxIncoming.
func DialWebSocket(ctx context.Context, wsURL string, limits transport.Limits) (transport.MessageConn, error) {
	return dialWebSocket(ctx, &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: DialTimeout,
	}, wsURL, limits)
}

// dialWebSocket is DialWebSocket with the given dialer (tests set the
// socket buffers through it).
func dialWebSocket(ctx context.Context, dialer *websocket.Dialer, wsURL string, limits transport.Limits) (transport.MessageConn, error) {
	conn, wire, resp, err := dialCuttable(ctx, dialer, wsURL)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", wsURL, deadlineCaused(ctx, err))
	}
	if limits.MaxIncoming == 0 {
		limits.MaxIncoming = transport.DefaultMaxIncoming
	}
	conn.SetReadLimit(int64(limits.MaxIncoming))
	return &wsConn{conn: conn, wire: wire, limits: limits, writeSem: make(chan struct{}, 1)}, nil
}

func (w *wsConn) Limits() transport.Limits { return w.limits }

// Write sends one text message. Every write has a deadline: the ctx
// deadline, or DefaultSendTimeout when ctx has none.
func (w *wsConn) Write(ctx context.Context, msg []byte) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(DefaultSendTimeout)
	}
	select {
	case w.writeSem <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("%w, another write was still in progress: %w", transport.ErrNotWritten, ctxErr(ctx))
	}
	defer func() { <-w.writeSem }()
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", transport.ErrNotWritten, ctxErr(ctx))
	}
	if w.wire.cut.Load() {
		return fmt.Errorf("%w: %w", transport.ErrNotWritten, errWritesCut)
	}
	if err := w.conn.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("%w: setting the write deadline: %w", transport.ErrNotWritten, err)
	}
	return w.conn.WriteMessage(websocket.TextMessage, msg)
}

// Read returns the next message. Ending ctx ends the connection, because a
// gorilla connection cannot continue after an interrupted read.
func (w *wsConn) Read(ctx context.Context) ([]byte, error) {
	stop := context.AfterFunc(ctx, func() { _ = w.conn.SetReadDeadline(time.Now()) })
	defer stop()
	_, data, err := w.conn.ReadMessage()
	return data, err
}

// interruptWrites ends the write in progress, if any, and then fails every
// later write (Session.InterruptWrites).
func (w *wsConn) interruptWrites() { interruptInProgress(w.writeSem, w.wire) }

// interruptInProgress interrupts wire's writes when a write holds sem.
// Without a write in progress the connection stays writable, so the
// Target.resume calls that Session.Close sends to targets paused on start
// (design 2.7) still go out on a socket that takes data. An interrupted
// gorilla connection cannot be written again anyway: the failed frame
// breaks the stream.
func interruptInProgress(sem chan struct{}, wire *cutConn) {
	select {
	case sem <- struct{}{}:
		<-sem // nothing is being written
		return
	default:
	}
	wire.interrupt()
}

func (w *wsConn) Close() error {
	w.closeOnce.Do(func() { w.closeErr = w.conn.Close() })
	return w.closeErr
}

// errWritesCut fails the writes of a connection after InterruptWrites.
var errWritesCut = errors.New("the connection's writes were interrupted because it stopped answering; it is being replaced")

// cutConn is the network connection under a WebSocket connection, with a
// way to interrupt its writes: once interrupt ran, the write in progress
// ends at once and every later write fails before writing anything
// (interruptInProgress calls it only while a write is in progress). A
// write deadline set from outside alone would not do: gorilla/websocket
// sets the deadline it was given again before each frame, so the next frame
// of a large message would wait again. A gorilla connection whose write
// failed fails every later write too, so the connection cannot be written
// again either way; the daemon replaces it.
type cutConn struct {
	net.Conn
	cut atomic.Bool
}

// pastDeadline is a write deadline that has already passed.
var pastDeadline = time.Unix(1, 0)

func (c *cutConn) Write(b []byte) (int, error) {
	if c.cut.Load() {
		return 0, errWritesCut
	}
	return c.Conn.Write(b)
}

// SetWriteDeadline keeps the passed deadline once the writes were
// interrupted. cut is checked before the deadline is set, and interrupt
// sets cut before its own deadline, so a deadline that a frame sets after
// interrupt is followed by a Write that sees cut.
func (c *cutConn) SetWriteDeadline(t time.Time) error {
	if c.cut.Load() {
		t = pastDeadline
	}
	return c.Conn.SetWriteDeadline(t)
}

func (c *cutConn) interrupt() {
	c.cut.Store(true)
	// A write blocked in the kernel ends at a passed deadline. The error
	// only says that the connection is closed already, so no write runs.
	_ = c.Conn.SetWriteDeadline(pastDeadline)
}

// dialCuttable dials wsURL with a copy of d whose network connection is a
// cutConn, and returns both. d's NetDialContext and NetDialTLSContext are
// wrapped; the deprecated NetDial is not used in this repository and is
// replaced by a net.Dialer when NetDialContext is unset.
func dialCuttable(ctx context.Context, d *websocket.Dialer, wsURL string) (*websocket.Conn, *cutConn, *http.Response, error) {
	var wire *cutConn
	wrap := func(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			wire = &cutConn{Conn: c}
			return wire, nil
		}
	}
	dc := *d
	if dc.NetDialContext == nil {
		dc.NetDialContext = (&net.Dialer{}).DialContext
	}
	dc.NetDialContext = wrap(dc.NetDialContext)
	if dc.NetDialTLSContext != nil {
		dc.NetDialTLSContext = wrap(dc.NetDialTLSContext)
	}
	conn, resp, err := dc.DialContext(ctx, wsURL, nil)
	return conn, wire, resp, err
}
