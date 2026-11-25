package fakewebkit

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/gorilla/websocket"
)

// WriteStall makes the client's write of one command stall until the
// write's deadline and then fail as a socket write past its deadline fails:
// a *net.OpError that wraps os.ErrDeadlineExceeded, returned at the
// deadline, before the caller's context has necessarily seen it end. This
// is the timing of decision D9's race: a command whose deadline passes
// while its message is written. Fail replaces the stall with an immediate
// wire failure.
type WriteStall struct {
	// Method is the command whose message stalls (the inner method of a
	// Target-routed message).
	Method string
	// Fail, when set, is returned at once instead of the stall.
	Fail    error
	stalled atomic.Int32
}

// Stalled returns how many writes of Method stalled or failed.
func (w *WriteStall) Stalled() int { return int(w.stalled.Load()) }

// matches reports whether msg is a message of w.Method, plain or wrapped
// in Target.sendMessageToTarget.
func (w *WriteStall) matches(msg []byte) bool {
	return bytes.Contains(msg, []byte(`"method":"`+w.Method+`"`)) ||
		bytes.Contains(msg, []byte(`\"method\":\"`+w.Method+`\"`))
}

// fail stalls until deadline (zero: no deadline, a test error) and returns
// the socket's timeout, or returns w.Fail at once.
func (w *WriteStall) fail(deadline time.Time) error {
	w.stalled.Add(1)
	if w.Fail != nil {
		return w.Fail
	}
	if deadline.IsZero() {
		return fmt.Errorf("fakewebkit: write of %s has no deadline to stall until", w.Method)
	}
	time.Sleep(time.Until(deadline))
	return &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}
}

// Conn wraps a client MessageConn (webkit.Session's transport): a Write of
// Method stalls until the Write context's deadline.
func (w *WriteStall) Conn(c transport.MessageConn) transport.MessageConn {
	return stallMessageConn{MessageConn: c, w: w}
}

type stallMessageConn struct {
	transport.MessageConn
	w *WriteStall
}

func (c stallMessageConn) Write(ctx context.Context, msg []byte) error {
	if c.w.matches(msg) {
		dl, _ := ctx.Deadline()
		return c.w.fail(dl)
	}
	return c.MessageConn.Write(ctx, msg)
}

// Dialer returns a WebSocket dialer (for webkit.NewClientWithDialer)
// whose socket stalls the frame of a Method message until the socket's
// write deadline.
func (w *WriteStall) Dialer() *websocket.Dialer {
	return &websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &stallNetConn{Conn: c, w: w}, nil
	}}
}

type stallNetConn struct {
	net.Conn
	w        *WriteStall
	mu       sync.Mutex
	deadline time.Time // the write deadline
}

func (c *stallNetConn) SetDeadline(t time.Time) error {
	c.setWrite(t)
	return c.Conn.SetDeadline(t)
}

func (c *stallNetConn) SetWriteDeadline(t time.Time) error {
	c.setWrite(t)
	return c.Conn.SetWriteDeadline(t)
}

func (c *stallNetConn) setWrite(t time.Time) {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
}

func (c *stallNetConn) Write(b []byte) (int, error) {
	if payload, err := clientFramePayload(b); err == nil && c.w.matches(payload) {
		c.mu.Lock()
		dl := c.deadline
		c.mu.Unlock()
		return 0, c.w.fail(dl)
	}
	return c.Conn.Write(b)
}

// clientFramePayload unmasks the payload of the masked WebSocket client
// frame at the start of b (RFC 6455 section 5.2); a payload longer than b
// is cut at the end of b.
func clientFramePayload(b []byte) ([]byte, error) {
	if len(b) < 2 || b[1]&0x80 == 0 {
		return nil, errors.New("not a masked frame")
	}
	n, pos := uint64(b[1]&0x7f), 2
	switch n {
	case 126:
		if len(b) < 4 {
			return nil, errors.New("short frame header")
		}
		n, pos = uint64(binary.BigEndian.Uint16(b[2:4])), 4
	case 127:
		if len(b) < 10 {
			return nil, errors.New("short frame header")
		}
		n, pos = binary.BigEndian.Uint64(b[2:10]), 10
	}
	if len(b) < pos+4 {
		return nil, errors.New("short frame header")
	}
	key := b[pos : pos+4]
	data := b[pos+4:]
	if uint64(len(data)) > n {
		data = data[:n]
	}
	out := make([]byte, len(data))
	for i := range data {
		out[i] = data[i] ^ key[i%4]
	}
	return out, nil
}
