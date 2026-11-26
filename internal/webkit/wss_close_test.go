package webkit

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/gorilla/websocket"
)

// F4 (closure review of f9e00659): on a wss:// page that stopped reading,
// closing a cut connection must not wait for a TLS close_notify.
// tls.Conn.Close sets its own write deadline of 5 s for that alert, which
// replaces the passed deadline of the cut, so a cut connection closes the
// TCP connection under TLS instead (cutConn.Close).

// stalledWSSServer starts a wss:// server that upgrades and then reads
// nothing, and returns its URL and a dialer that trusts its certificate.
// Both socket buffers are small (Linux autotunes a receive buffer up to
// tcp_rmem's maximum, 32 MiB here), so a write of a few MiB blocks in the
// kernel. The dialer counts the bytes written to the TCP connection.
func stalledWSSServer(t *testing.T) (string, *websocket.Dialer, *atomic.Int64) {
	t.Helper()
	release := make(chan struct{})
	up := websocket.Upgrader{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		<-release // reads nothing
	}))
	srv.Listener = smallReadBufferListener{srv.Listener}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	cfg := srv.Client().Transport.(*http.Transport).TLSClientConfig
	var written atomic.Int64
	d := &websocket.Dialer{
		TLSClientConfig:  cfg,
		HandshakeTimeout: 5 * time.Second,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if tcp, ok := c.(*net.TCPConn); ok {
				_ = tcp.SetWriteBuffer(4096)
			}
			return countingConn{Conn: c, n: &written}, nil
		},
	}
	return "wss" + strings.TrimPrefix(srv.URL, "https"), d, &written
}

// smallReadBufferListener gives each accepted connection a small receive
// buffer.
type smallReadBufferListener struct{ net.Listener }

func (l smallReadBufferListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(4096)
	}
	return c, err
}

// countingConn adds the bytes written to n.
type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.n.Add(int64(n))
	return n, err
}

// stalledWSSClient connects a Client to a stalledWSSServer.
func stalledWSSClient(t *testing.T) (*Client, *atomic.Int64) {
	t.Helper()
	wsURL, d, written := stalledWSSServer(t)
	c, err := NewClientWithDialer(testCtx(t), wsURL, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, written
}

// waitWriteBlocked waits until a write holds sem and no byte has reached
// the TCP connection for 300 ms: the write is blocked in the kernel.
func waitWriteBlocked(t *testing.T, sem chan struct{}, written *atomic.Int64) {
	t.Helper()
	ctx := testCtx(t)
	last, since := written.Load(), time.Now()
	for len(sem) == 0 || time.Since(since) < 300*time.Millisecond {
		select {
		case <-ctx.Done():
			t.Fatalf("the write did not block (held: %v, %d bytes written)", len(sem) > 0, written.Load())
		case <-time.After(10 * time.Millisecond):
		}
		if n := written.Load(); n != last || len(sem) == 0 {
			last, since = n, time.Now()
		}
	}
}

// bigExpression is far more than the socket buffers of a page that reads
// nothing take.
var bigExpression = strings.Repeat("x", 4<<20)

// F4: a Send whose write ends at its deadline retires the connection
// (retireOn -> end -> Close) in the caller, so it returns soon after its
// deadline, not 5 s later.
func TestWSSStalledSendReturnsAtItsDeadline(t *testing.T) {
	c, _ := stalledWSSClient(t)
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := c.Send(sctx, "Runtime.evaluate", map[string]any{"expression": bigExpression})
	el := time.Since(start)
	t.Logf("Send over a stalled wss page: %v after %v", err, el.Round(10*time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "writing message") {
		t.Fatalf("Send = %v, want a failed write (the test needs the write to start)", err)
	}
	if el > 4*time.Second {
		t.Fatalf("Send with a 2 s deadline returned after %v: Close waited for the TLS close_notify", el.Round(10*time.Millisecond))
	}
}

// F4: after the liveness probe's InterruptWrites, Close (pageConn.retire,
// before the recovery dials a new connection) returns at once.
func TestWSSClientCloseAfterInterruptIsPrompt(t *testing.T) {
	c, written := stalledWSSClient(t)
	sent := make(chan error, 1)
	go func() {
		_, err := c.Send(context.Background(), "Runtime.evaluate", map[string]any{"expression": bigExpression})
		sent <- err
	}()
	waitWriteBlocked(t, c.writeSem, written)
	c.InterruptWrites()
	select {
	case err := <-sent:
		t.Logf("Send after InterruptWrites: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the write did not end after InterruptWrites")
	}
	start := time.Now()
	_ = c.Close()
	el := time.Since(start)
	t.Logf("Close after InterruptWrites took %v", el.Round(10*time.Millisecond))
	if el > 2*time.Second {
		t.Fatalf("Close took %v: it waited for the TLS close_notify on a page that reads nothing", el.Round(10*time.Millisecond))
	}
}

// F4: the same for a Session's connection (wsConn.Close after
// interruptWrites).
func TestWSSConnCloseAfterInterruptIsPrompt(t *testing.T) {
	wsURL, d, n := stalledWSSServer(t)
	conn, err := dialWebSocket(testCtx(t), d, wsURL, transport.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	w := conn.(*wsConn)
	t.Cleanup(func() { _ = w.Close() })
	written := make(chan error, 1)
	go func() { written <- w.Write(context.Background(), []byte(bigExpression)) }()
	waitWriteBlocked(t, w.writeSem, n)
	w.interruptWrites()
	select {
	case err := <-written:
		t.Logf("Write after interruptWrites: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the write did not end after interruptWrites")
	}
	start := time.Now()
	_ = w.Close()
	el := time.Since(start)
	t.Logf("Close after interruptWrites took %v", el.Round(10*time.Millisecond))
	if el > 2*time.Second {
		t.Fatalf("Close took %v: it waited for the TLS close_notify on a page that reads nothing", el.Round(10*time.Millisecond))
	}
}
