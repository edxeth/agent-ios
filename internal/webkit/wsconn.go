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

	"github.com/edxeth/agent-ios/internal/logcause"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/gorilla/websocket"
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
	if err := w.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		return w.wire.failed(err)
	}
	return nil
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

// errWritesCut fails the writes of a connection after InterruptWrites or
// after a failed write (cutConn.failed).
var errWritesCut = errors.New("the connection takes no more writes: an earlier write was interrupted because the page stopped answering, or it failed")

// ErrWriteFailed is in the chain of Session.Err and Client.Err when the
// connection ended because a socket write failed. gorilla/websocket keeps
// the first write error of a connection and returns it for every later
// write, so such a connection is closed. The chain also holds the
// category of the failure: ErrWriteTimeout or ErrWriteBroken.
var ErrWriteFailed = errors.New("a write to the page failed, and a WebSocket connection takes no write after a failed one")

// ErrWriteTimeout is the category of a failed write that its deadline or
// call budget ended (a net.Error with Timeout): the far side was still
// connected but did not take the data in time. It is the only failed write
// after which the daemon reconnects by itself (core pageConn.writeFailed),
// as after a failed liveness probe.
var ErrWriteTimeout = errors.New("the write timed out: the page did not take the data before the write's deadline")

// ErrWriteBroken is the category of every other failed write: the peer or
// the network ended the connection (EPIPE, ECONNRESET, a closed connection)
// or the failure is not known to be local. The daemon treats it as an
// unexpected disconnect: the page is lost (design 2.5 rule 3).
var ErrWriteBroken = errors.New("the write failed because the peer or the network ended the connection")

// retiringError is a write error after which the connection must end
// (cutConn.failed); it keeps the text and chain of err. local is the
// category: true for a timeout (localWriteFailure).
type retiringError struct {
	err   error
	local bool
}

func (e retiringError) Error() string { return e.err.Error() }
func (e retiringError) Unwrap() error { return e.err }

// localWriteFailure reports whether err, a failed socket write, was ended
// by its own deadline or call budget: a net.Error with Timeout (the socket's
// os.ErrDeadlineExceeded, or context.DeadlineExceeded). Anything else is a
// peer or network failure; when in doubt the page is lost.
func localWriteFailure(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// retiredBy returns the cause with which the owner of a connection ends it
// after err, or nil when err does not retire the connection (retiringError).
// The cause holds ErrSessionClosed, ErrWriteFailed and the category
// (ErrWriteTimeout or ErrWriteBroken), and only the text of err: its
// deadline or socket timeout belongs to the call whose write failed, not to
// the calls that the end of the connection fails.
func retiredBy(err error) error {
	var re retiringError
	if !errors.As(err, &re) {
		return nil
	}
	category := ErrWriteBroken
	if re.local {
		category = ErrWriteTimeout
	}
	return logcause.Errorf("%w: %w: %w (%v)", ErrSessionClosed, ErrWriteFailed, category, err)
}

// cutConn is the network connection under a WebSocket connection, with a
// way to interrupt its writes: once interrupt ran, the write in progress
// ends at once and every later write fails before writing anything
// (interruptInProgress calls it only while a write is in progress). A
// write deadline set from outside alone would not do: gorilla/websocket
// sets the deadline it was given again before each frame, so the next frame
// of a large message would wait again. A gorilla connection whose write
// failed fails every later write too, so a failed write sets cut as well
// (failed), and its owner ends the connection; the daemon replaces it.
//
// Reads acknowledge at once (quickAcker): ios_webkit_debug_proxy answers
// each Target.sendMessageToTarget with two frames, its own empty result
// and then the page's reply in Target.dispatchMessageFromTarget, on a
// socket with Nagle's algorithm on, so the reply waited until the daemon
// acknowledged the first frame, 40 ms later on Linux (FIX-R).
type cutConn struct {
	net.Conn
	cut    atomic.Bool
	ackNow func() // nil: not a TCP connection, or not Linux
}

func (c *cutConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.ackNow != nil {
		c.ackNow()
	}
	return n, err
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

// failed handles err, the error of a WebSocket write on c. gorilla/websocket
// fails every later write of the connection with the first write error, so
// failed sets cut: every later write is refused before it writes anything.
// When the cut was not set before, this failure retires the connection, and
// err is returned as a retiringError with its category: its owner ends the
// connection (Session.retireOn, Client.retireOn). A write that interrupt ended is
// returned unchanged: the liveness probe declared the stall, the daemon
// replaces the connection, and the connection still reads (core-events
// r6).
func (c *cutConn) failed(err error) error {
	if c.cut.CompareAndSwap(false, true) {
		return retiringError{err: err, local: localWriteFailure(err)}
	}
	return err
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
			wire = &cutConn{Conn: c, ackNow: quickAcker(c)}
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
