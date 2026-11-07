package webkit

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
)

// wsConn is a transport.MessageConn over one WebSocket connection.
type wsConn struct {
	conn   *websocket.Conn
	limits transport.Limits
	// writeSem holds one token while a goroutine writes (gorilla/websocket is
	// not concurrent-write-safe). A channel, so Write can stop waiting for it
	// when its context ends.
	writeSem  chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// DialWebSocket opens a WebSocket MessageConn to wsURL. The handshake is
// bounded by DialTimeout and by ctx. A zero limits.MaxIncoming means
// transport.DefaultMaxIncoming.
func DialWebSocket(ctx context.Context, wsURL string, limits transport.Limits) (transport.MessageConn, error) {
	dialer := &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: DialTimeout,
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", wsURL, err)
	}
	if limits.MaxIncoming == 0 {
		limits.MaxIncoming = transport.DefaultMaxIncoming
	}
	conn.SetReadLimit(int64(limits.MaxIncoming))
	return &wsConn{conn: conn, limits: limits, writeSem: make(chan struct{}, 1)}, nil
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

func (w *wsConn) Close() error {
	w.closeOnce.Do(func() { w.closeErr = w.conn.Close() })
	return w.closeErr
}
