package cdp

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"testing"
	"time"
)

// stallResponse requests path over a raw TCP connection, reads the
// response headers and then stops reading, like a client that hangs. The
// response must be larger than the socket buffers to block the server's
// write. It returns the open connection.
func stallResponse(t *testing.T, rawURL string) net.Conn {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	if tcp, ok := peer.(*net.TCPConn); ok {
		if err := tcp.SetReadBuffer(1024); err != nil {
			t.Fatal(err)
		}
	}
	// Listing a large page under -race and a loaded host takes seconds;
	// the read waits until the test's deadline (testCtx).
	readEnd, _ := testCtx(t).Deadline() // testCtx always has a deadline
	if err := peer.SetReadDeadline(readEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(peer, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", u.RequestURI(), u.Host); err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(peer)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the response headers: %v", err)
		}
		if line == "\r\n" {
			return peer
		}
	}
}

// bigTitle is a page title twice as large as the largest loopback send
// buffer of Linux autotuning (tcp_wmem max, 4 MiB by default), so that a
// response with it blocks on a client that does not read. Page metadata is
// remote-controlled, so a discovery response has no size bound. The pages
// are issued before it is listed: resolving a page reads the listing under
// a 5 s timeout, which a large listing can exceed under -race.
var bigTitle = strings.Repeat("x", 8<<20)

// Design 5.2 (a session release after revocation is final) with a client
// that stops reading a large /json/list response: RevokeSession closes the
// network connection of the request it waits for, so it returns at once
// and not when the client goes away. (Review reproduction
// TestReviewSlowReaderBlocksRevocation.)
func TestRevocationClosesStalledResponses(t *testing.T) {
	r := newRig(t)
	ep := r.issue(t, "s1", "One", ScopeDevice)
	r.devA.AddPage(bigTitle, "https://large.example/", r.fake["Two"].URL)
	stallResponse(t, ep.HTTP+"/json/list")
	// Precondition: the response write is stalled (Observe runs when the
	// request ends).
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(r.observedText(), "/json/list") {
		t.Fatal("the /json/list response did not stall; the test needs a larger response")
	}
	revoked := make(chan int, 1)
	go func() { revoked <- r.srv.RevokeSession("s1") }()
	select {
	case n := <-revoked:
		if n != 1 {
			t.Fatalf("RevokeSession revoked %d records, want 1", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RevokeSession waits for a response to a client that stopped reading")
	}
}

// Every HTTP response has a write deadline: a client that stops reading
// holds the request (and its record's drain) for at most the response
// write time, also without a revocation.
func TestStalledResponseWriteIsBounded(t *testing.T) {
	r := newRig(t)
	r.srv.mu.Lock()
	r.srv.respWrite = 300 * time.Millisecond
	r.srv.mu.Unlock()
	ep := r.issue(t, "s1", "One", ScopeDevice)
	r.devA.AddPage(bigTitle, "https://large.example/", r.fake["Two"].URL)
	stallResponse(t, ep.HTTP+"/json/list")
	eventually(t, 10*time.Second, "the stalled /json/list request ends", func() bool {
		return strings.Contains(r.observedText(), "/json/list 200")
	})
}

// The write deadline of a response ends with it (net/http clears it after
// the response): a WebSocket upgrade on the same keep-alive connection,
// after the deadline of an earlier response passed, still upgrades.
func TestUpgradeAfterAResponseOnTheSameConnection(t *testing.T) {
	r := newRig(t)
	r.srv.mu.Lock()
	r.srv.respWrite = 100 * time.Millisecond
	r.srv.mu.Unlock()
	ep := r.issue(t, "s1", "One", ScopePage)
	u, err := url.Parse(ep.WS)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	if err := peer.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(peer)
	base := strings.TrimSuffix(u.Path, "/devtools/browser/"+path.Base(u.Path))
	if _, err := fmt.Fprintf(peer, "GET %s/json/version HTTP/1.1\r\nHost: %s\r\n\r\n", base, u.Host); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(rd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil || resp.StatusCode != 200 || resp.Close {
		t.Fatalf("/json/version: %d, close %v, %v; want 200 on a kept connection", resp.StatusCode, resp.Close, err)
	}
	time.Sleep(300 * time.Millisecond) // the response's deadline passes
	if _, err := fmt.Fprintf(peer, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", u.Path, u.Host); err != nil {
		t.Fatal(err)
	}
	resp, err = http.ReadResponse(rd, nil)
	if err != nil {
		t.Fatalf("the upgrade on the kept connection got no response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade on the kept connection: %d, want 101", resp.StatusCode)
	}
}

// observedText is the requests that Options.Observe saw, one per line.
func (r *rig) observedText() string {
	r.obsMu.Lock()
	defer r.obsMu.Unlock()
	return strings.Join(*r.observed, "\n")
}

// Design 5.1 (the token is never logged): a value that a client sends
// is never logged raw, so a current or retired token or endpoint id in a
// header, the request method, a path segment outside the secret positions,
// or a CDP method name stays out of the log. The refusals are still
// logged. (Review reproduction TestReviewRetiredSecretInHeader.)
func TestClientValuesAreNeverLogged(t *testing.T) {
	r := newRig(t)
	ep := r.issue(t, "s1", "One", ScopePage)
	token, id := secretsOf(t, ep)
	r.srv.RevokeSession("s1")
	// A client of another record sends the retired endpoint id as a CDP
	// method, also with a method suffix.
	c := dial(t, r.issue(t, "s2", "Two", ScopePage).WS)
	for _, m := range []string{id, id + ".x", "Page." + id} {
		if _, err := c.Call(ctxFor(t, 5*time.Second), "", m, nil); err == nil {
			t.Fatalf("an unknown method was answered")
		}
	}
	c.Close()
	addr := r.srv.Status().Addr
	send := func(secret string) {
		for _, mod := range []func(*http.Request){
			func(q *http.Request) { q.Header.Set("Origin", secret) },
			func(q *http.Request) { q.Header.Set("Origin", "http://"+secret+".example") },
			func(q *http.Request) { q.Host = secret },
			func(q *http.Request) { q.Host = "127.0.0.1:1" + secret },
			func(q *http.Request) {
				q.Header.Set("Origin", "null")
				q.Header.Set("Referer", "http://a.test/"+secret)
			},
			func(q *http.Request) { q.Header.Set("Origin", "null"); q.Method = secret },
			func(q *http.Request) { q.Header.Set("Origin", "null"); q.URL.Path = "/json/close/" + secret },
			func(q *http.Request) { q.Host = "evil.example"; q.URL.Path = "/" + secret },
		} {
			if code, _ := httpDo(t, "GET", "http://"+addr+"/json/version", mod); code != http.StatusForbidden {
				t.Errorf("request with a secret in a client value: %d, want 403", code)
			}
		}
	}
	// The endpoint id is retired; the token is still current.
	send(id)
	send(token)
	r.srv.Stop()
	if _, err := r.srv.Start(0, nil); err != nil {
		t.Fatal(err)
	}
	addr = r.srv.Status().Addr
	// Both are retired now.
	send(id)
	send(token)
	log := r.log.text()
	if strings.Contains(log, token) || strings.Contains(log, id) {
		t.Fatalf("a client value with a token or endpoint id was logged raw:\n%s", log)
	}
	for _, want := range []string{"refused GET /json/version: Origin header ", "refused GET /json/version: Host header ", "refused <method ", "/json/close/<value ", "unknown method <method "} {
		if !strings.Contains(log, want) {
			t.Errorf("the log has no %q line:\n%s", want, log)
		}
	}
}
