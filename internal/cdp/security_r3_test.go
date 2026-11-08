package cdp

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// handshakeConn is the server end of a net.Pipe: a real blocking
// net.Conn, whose writes wait until the peer reads. It records the write
// deadline that its first write (the HTTP 101 handshake of the WebSocket
// upgrade) runs under, and reports that write.
type handshakeConn struct {
	net.Conn
	written chan struct{}
	once    sync.Once
	mu      sync.Mutex
	set     time.Time // the current write deadline
	atWrite time.Time // the write deadline of the first write
}

func (c *handshakeConn) Write(p []byte) (int, error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.atWrite = c.set
		c.mu.Unlock()
		close(c.written)
	})
	return c.Conn.Write(p)
}

func (c *handshakeConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.set = t
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *handshakeConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.set = t
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

func (c *handshakeConn) writeDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.atWrite
}

// hijackWriter stands in for the http.ResponseWriter of net/http: Hijack
// hands over the connection after hold returns.
type hijackWriter struct {
	nc   net.Conn
	h    http.Header
	hold func()
}

func (w *hijackWriter) Header() http.Header         { return w.h }
func (w *hijackWriter) WriteHeader(int)             {}
func (w *hijackWriter) Write(p []byte) (int, error) { return w.nc.Write(p) }

func (w *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hold()
	return w.nc, bufio.NewReadWriter(bufio.NewReader(w.nc), bufio.NewWriter(w.nc)), nil
}

// stalledUpgrade serves a WebSocket upgrade of ep's browser URL over a
// net.Pipe whose peer never reads, so the handshake write blocks. The
// real Gorilla upgrade runs; hold runs in Hijack before the connection is
// handed over. It returns the server end and a channel that closes when
// the request ended.
func stalledUpgrade(t *testing.T, r *rig, ep Endpoint, hold func()) (*handshakeConn, <-chan struct{}) {
	t.Helper()
	nc, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	server := &handshakeConn{Conn: nc, written: make(chan struct{})}
	t.Cleanup(func() { _ = server.Close() })
	// ConnContext gives every request its network connection (start).
	ctx := context.WithValue(context.Background(), netConnKey{}, net.Conn(server))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.Replace(ep.WS, "ws://", "http://", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		r.srv.serveHTTP(&hijackWriter{nc: server, h: http.Header{}, hold: hold}, req)
	}()
	return server, ended
}

// Design 5.2 (a session release after revocation is final) for an upgrade
// whose HTTP 101 handshake write stalls on a client that does not read:
// RevokeSession and Stop close the hijacked connection of the upgrade, so
// they return at once and not when the client goes away. The handshake
// write also runs under a write deadline. (Review reproduction
// TestReviewHijackedHandshakeBlocksRevocation.)
func TestRevocationClosesStalledHandshake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(*Server) int
	}{
		{"RevokeSession", func(s *Server) int { return s.RevokeSession("s1") }},
		{"Stop", func(s *Server) int { n, _ := s.Stop(); return n }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			ep := r.issue(t, "s1", "One", ScopePage)
			start := time.Now()
			server, ended := stalledUpgrade(t, r, ep, func() {})
			select {
			case <-server.written:
			case <-time.After(5 * time.Second):
				t.Fatal("the upgrade did not write its handshake")
			}
			revoked := make(chan int, 1)
			go func() { revoked <- tc.revoke(r.srv) }()
			select {
			case n := <-revoked:
				if n != 1 {
					t.Fatalf("%s revoked %d records, want 1", tc.name, n)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("%s waits for an upgrade whose handshake write stalled", tc.name)
			}
			select {
			case <-ended:
			case <-time.After(3 * time.Second):
				t.Fatal("the stalled upgrade request did not end after the revocation")
			}
			if dl := server.writeDeadline(); dl.IsZero() || dl.Before(start) || dl.After(time.Now().Add(writeTimeout)) {
				t.Errorf("the handshake was written under write deadline %v, want one within writeTimeout (%v) of the upgrade at %v", dl, writeTimeout, start)
			}
			if c := r.srv.Status().Connections; c != 0 {
				t.Errorf("%d connection(s) after the revocation, want 0", c)
			}
		})
	}
}

// The handshake write of an upgrade is bounded also without a revocation:
// a client that never reads it holds the request for at most the
// response write time.
func TestStalledHandshakeIsBounded(t *testing.T) {
	r := newRig(t)
	r.srv.mu.Lock()
	r.srv.respWrite = 300 * time.Millisecond
	r.srv.mu.Unlock()
	ep := r.issue(t, "s1", "One", ScopePage)
	_, ended := stalledUpgrade(t, r, ep, func() {})
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("an upgrade whose handshake write stalled did not end on its write deadline")
	}
	if c := r.srv.Status().Connections; c != 0 {
		t.Errorf("%d connection(s) after the failed upgrade, want 0", c)
	}
}

// An upgrade that hijacks its connection after the revocation of its
// record began, so possibly after drain looked at the record's requests,
// closes that connection itself: RevokeSession does not wait for the
// handshake write deadline either.
func TestHijackAfterRevocationCloses(t *testing.T) {
	r := newRig(t)
	ep := r.issue(t, "s1", "One", ScopePage)
	inHijack, resume := make(chan struct{}), make(chan struct{})
	_, ended := stalledUpgrade(t, r, ep, func() {
		close(inHijack)
		<-resume
	})
	select {
	case <-inHijack:
	case <-time.After(5 * time.Second):
		t.Fatal("the upgrade did not reach Hijack")
	}
	revoked := make(chan int, 1)
	go func() { revoked <- r.srv.RevokeSession("s1") }()
	// The revocation closes the connection that has not upgraded yet
	// (closeAll); its drain then waits for the request.
	eventually(t, 5*time.Second, "the revocation closes the connection", func() bool {
		st := r.srv.Status()
		return len(st.Endpoints) == 0 && st.Connections == 0
	})
	time.Sleep(100 * time.Millisecond) // drain reads the admitted requests
	close(resume)
	select {
	case n := <-revoked:
		if n != 1 {
			t.Fatalf("RevokeSession revoked %d records, want 1", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RevokeSession waits for an upgrade that hijacked its connection after the revocation")
	}
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the upgrade request did not end")
	}
}
