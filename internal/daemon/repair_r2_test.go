package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Regression tests for the second W1b review (docs/notes/w1b.md, "Review
// repairs, round 2").

// deadListing is a loopback address where nothing listens.
func deadListing(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// R2-2, D10: classifying an explicit page URL resolves the daemon's
// default transport (core.WithoutTransportStart) without the iwdp check or
// start; a request without the mark checks the listing as before.
func TestTransportForURLClassificationNeverChecksIWDP(t *testing.T) {
	starts := 0
	addr := deadListing(t)
	ts := &transports{o: RunOptions{Transport: "iwdp", Addr: addr, StartIWDP: func(context.Context) error { starts++; return nil }}, logf: t.Logf}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := ts.get(core.WithoutTransportStart(ctx), "auto", "")
	if err != nil {
		t.Fatalf("transport for URL classification: %v", err)
	}
	if it, ok := tr.(*iwdp.Transport); !ok || it.ListingAddr() != addr {
		t.Fatalf("transport %T does not read the custom listing %s", tr, addr)
	}
	if _, err := ts.get(ctx, "auto", ""); err == nil {
		t.Fatal("a dead custom listing passed the iwdp check")
	}
	if starts != 0 {
		t.Fatalf("iwdp started %d times for a custom listing address, want 0", starts)
	}
}

// pauseOn makes the fake's next evaluation pause at a breakpoint until a
// Debugger.resume arrives.
func pauseOn(f *fakewebkit.Server) {
	f.HoldUntil("Runtime.evaluate", "Debugger.resume", "Debugger.paused", map[string]any{"callFrames": []any{}, "reason": "other"})
}

// evalAsync runs an evaluation of "debugger" in ctx and reports its end.
func evalAsync(p *PageCaller, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := p.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "debugger"})
		done <- err
	}()
	return done
}

func wantResumed(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the paused evaluation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the paused evaluation did not finish after its own resume")
	}
}

// R2-5: an attach-only control caller (the CLI resume command) carries
// the epoch of the lease it attached under. After a lease break and a new
// lease of the same session, its resume is refused unsent and the new lease's pause stays for the new holder.
func TestControlFromAnOldLeaseIsRefused(t *testing.T) {
	c := connect(t, startInProcess(t, RunOptions{}))
	f := newFake(t, fakewebkit.Options{})
	b, err := c.AttachPage(ctxFor(t, 5*time.Second), PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: "s1"}, Name: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if _, err := run(t, c, "s2", "lease", "break", f.URL); err != nil {
		t.Fatalf("lease break: %v", err)
	}
	pauseOn(f)
	a, err := openPage(t, c, f, "s1", 10*time.Second)
	if err != nil {
		t.Fatalf("s1 after the break: %v", err)
	}
	defer func() { _ = a.Close() }()
	done := evalAsync(a, ctxFor(t, 10*time.Second))
	waitSent(t, f, "Debugger.paused")
	if _, err := b.Send(ctxFor(t, 3*time.Second), "Debugger.resume", nil); err == nil || !errors.Is(err, webkit.ErrNotSent) {
		t.Fatalf("resume under the broken lease: %v, want a refusal that was not sent", err)
	}
	if n := len(f.ReceivedMethod("Debugger.resume")); n != 0 {
		t.Fatalf("the page received %d Debugger.resume under the broken lease", n)
	}
	if _, err := a.Send(ctxFor(t, 3*time.Second), "Debugger.resume", nil); err != nil {
		t.Fatalf("resume under the new lease: %v", err)
	}
	wantResumed(t, done)
}

// R2-7, design 2.6: a daemon that shuts down ends every event stream with a
// final "closed" item that says so, before it closes the connections.
func TestShutdownSendsAFinalClosedEvent(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	if _, err := c.AttachPage(ctxFor(t, 5*time.Second), PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: "mcp"}}); err != nil {
		t.Fatal(err)
	}
	resp, err := c.request(ctxFor(t, 10*time.Second), http.MethodGet, "/v1/events?methods=&session=mcp&page="+url.QueryEscape(f.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	var items []Item
	if !sc.Scan() {
		t.Fatalf("no first item: %v", sc.Err())
	}
	if err := c.Shutdown(ctxFor(t, 5*time.Second), true); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	for sc.Scan() {
		var it Item
		if err := json.Unmarshal(sc.Bytes(), &it); err != nil {
			t.Fatalf("item %q: %v", sc.Bytes(), err)
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		t.Fatal("the stream ended without a final item")
	}
	last := items[len(items)-1]
	if last.Type != "closed" || last.Error == nil || !strings.Contains(last.Error.Message, "shutting down") {
		t.Fatalf("last item %+v, want closed with the shutdown reason", last)
	}
}

// fakeStreamDaemon serves a daemon API on dir whose event streams end
// abruptly after "subscribed" (a killed daemon: no final event).
func fakeStreamDaemon(t *testing.T, dir string) {
	t.Helper()
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hello", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Hello{ID: "fake", Version: "test", Protocol: Protocol, PID: 1})
	})
	mux.HandleFunc("POST /v1/lease", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"page": PageInfo{Key: "k", Title: "t", Epoch: 1}})
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Item{Type: "subscribed", Page: &PageInfo{Key: "k", Title: "t"}})
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		panic(http.ErrAbortHandler) // drop the connection without a final item
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// R2-7: the client treats an event stream that ends without a final item
// (EOF or a read error: the daemon died) as the end of the page: the
// heartbeat's Done closes, and Err says why.
func TestHeartbeatEOFEndsThePage(t *testing.T) {
	dir := shortDir(t)
	fakeStreamDaemon(t, dir)
	c := connect(t, Options{Dir: dir, NoStart: true, Version: "test"})
	pc, err := c.AttachPage(ctxFor(t, 5*time.Second), PageOptions{Page: PageReq{Page: "ws://127.0.0.1:1/p"}, Owner: Owner{Session: "mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	pc.Subscribe("", func(string, json.RawMessage) {})
	select {
	case <-pc.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done stayed open after the heartbeat stream ended without a final event")
	}
	if err := pc.Err(); err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("Err() = %v, want the end of the daemon connection", err)
	}
}
