package webkit

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/gorilla/websocket"
)

// wss:// page URLs (--page accepts them: core isWSURL -> OpenURL ->
// webkit.NewClient) must carry commands: the single-frame writer writes
// above gorilla's TLS layer, never into the TCP connection under it.

// newTLSEchoIDServer starts a wss:// server that answers every message
// {"id":N,...} with {"id":N,"result":{}}, and returns its URL and a dialer
// that trusts its certificate.
func newTLSEchoIDServer(t *testing.T) (string, *websocket.Dialer) {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(data, &m)
			_ = c.WriteMessage(websocket.TextMessage, fmt.Appendf(nil, `{"id":%d,"result":{}}`, m.ID))
		}
	}))
	t.Cleanup(srv.Close)
	cfg := srv.Client().Transport.(*http.Transport).TLSClientConfig
	return "wss" + strings.TrimPrefix(srv.URL, "https"), &websocket.Dialer{TLSClientConfig: cfg, HandshakeTimeout: 5 * time.Second}
}

// testCtx is bounded by the test's own deadline.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx := context.Background()
	if d, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, d.Add(-5*time.Second))
		t.Cleanup(cancel)
	}
	return ctx
}

// checkWSSRoundTrip sends one command over a wss:// connection through
// webkit.Client and one through wsConn, and wants each reply.
func checkWSSRoundTrip(t *testing.T, wsURL string, d *websocket.Dialer) {
	t.Helper()
	ctx := testCtx(t)
	c, err := NewClientWithDialer(ctx, wsURL, d)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": strings.Repeat("x", 70_000)}); err != nil {
		t.Fatalf("Client.Send over wss: %v", err)
	}
	conn, err := dialWebSocket(ctx, d, wsURL, transport.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Write(ctx, []byte(`{"id":7,"method":"Runtime.evaluate"}`)); err != nil {
		t.Fatalf("Write over wss: %v", err)
	}
	got, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read over wss after a write: %v", err)
	}
	if string(got) != `{"id":7,"result":{}}` {
		t.Fatalf("reply %q", got)
	}
}

// F1: a wss:// page carries commands (Client.Send and wsConn.Write).
func TestWSSPageCarriesCommands(t *testing.T) {
	wsURL, d := newTLSEchoIDServer(t)
	checkWSSRoundTrip(t, wsURL, d)
}

// F1: a wss:// page behind an HTTP proxy (CONNECT) still works, as with
// gorilla's own proxy dial: the TLS handshake runs through the tunnel.
func TestWSSPageThroughHTTPProxy(t *testing.T) {
	wsURL, d := newTLSEchoIDServer(t)
	tunnels := make(chan string, 4)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		target, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		tunnels <- r.Host
		c, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = target.Close()
			return
		}
		_, _ = brw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
		_ = brw.Flush()
		pipe(c, brw.Reader, target)
	}))
	t.Cleanup(proxy.Close)
	proxyURL, _ := url.Parse(proxy.URL)
	d.Proxy = http.ProxyURL(proxyURL)
	checkWSSRoundTrip(t, wsURL, d)
	if len(tunnels) != 2 {
		t.Fatalf("%d CONNECT tunnels, want 2 (one per connection): the proxy was bypassed", len(tunnels))
	}
}

// F1: a wss:// page behind a SOCKS5 proxy still works.
func TestWSSPageThroughSOCKS5Proxy(t *testing.T) {
	wsURL, d := newTLSEchoIDServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	tunnels := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSOCKS5(c, tunnels)
		}
	}()
	d.Proxy = http.ProxyURL(&url.URL{Scheme: "socks5", Host: ln.Addr().String()})
	checkWSSRoundTrip(t, wsURL, d)
	if len(tunnels) != 2 {
		t.Fatalf("%d SOCKS5 tunnels, want 2 (one per connection): the proxy was bypassed", len(tunnels))
	}
}

// serveSOCKS5 serves one SOCKS5 CONNECT without authentication to an IPv4
// address (RFC 1928), then pipes.
func serveSOCKS5(c net.Conn, tunnels chan<- string) {
	br := bufio.NewReader(c)
	var hello [2]byte
	if _, err := io.ReadFull(br, hello[:]); err != nil || hello[0] != 5 {
		_ = c.Close()
		return
	}
	if _, err := io.ReadFull(br, make([]byte, hello[1])); err != nil {
		_ = c.Close()
		return
	}
	_, _ = c.Write([]byte{5, 0})
	var req [10]byte // VER CMD RSV ATYP=1 IPv4(4) PORT(2)
	if _, err := io.ReadFull(br, req[:]); err != nil || req[1] != 1 || req[3] != 1 {
		_ = c.Close()
		return
	}
	addr := net.JoinHostPort(net.IP(req[4:8]).String(), fmt.Sprint(binary.BigEndian.Uint16(req[8:])))
	target, err := net.Dial("tcp", addr)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		_ = c.Close()
		return
	}
	tunnels <- addr
	_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	pipe(c, br, target)
}

// pipe copies between the client c (read through r) and target until
// either side ends.
func pipe(c net.Conn, r io.Reader, target net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(target, r); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, target); done <- struct{}{} }()
	<-done
	_ = c.Close()
	_ = target.Close()
}
