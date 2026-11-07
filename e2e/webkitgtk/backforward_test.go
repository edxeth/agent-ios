//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/webkitgtk"
)

// bfcacheUA is the user-agent override of TestBackAndForwardAcrossSites.
const bfcacheUA = "iwdp-bfcache-ua"

// twoSites serves pages on 127.0.0.1 and on localhost, two sites, so that
// WebKitGTK swaps processes between them. It records the User-Agent and
// X-Iwdp headers of every request by its request URI.
type twoSites struct {
	a, b string // site origins
	mu   sync.Mutex
	reqs map[string][2]string // request URI -> {User-Agent, X-Iwdp}
}

func (s *twoSites) serve(t *testing.T, host string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs[r.Host+r.URL.RequestURI()] = [2]string{r.Header.Get("User-Agent"), r.Header.Get("X-Iwdp")}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, "<!doctype html><title>%s</title><p>%s</p>", r.URL.Path, r.URL.Path)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return "http://" + host + ":" + port
}

// request returns the headers that url's request carried.
func (s *twoSites) request(url string) ([2]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.reqs[url[len("http://"):]]
	return h, ok
}

// W3b finding (finding-back-across-sites.txt), reproduced at the W1b level:
// "back" across two sites brings the earlier page target back from WebKit's
// back/forward cache with the same target id and the backend it had, whose
// Page and Debugger agents are still enabled. The provisional setup sent
// Page.enable to it, WebKit answered "Page domain already enabled", and
// every later call of the page was refused with the setup failure until
// the deadline. Forward does the same with the other target. Now every
// call succeeds after back and forward, the page is the cached document
// (its marker survives), and the desired state holds on each restored
// backend: its requests carry the user-agent override and the extra
// header, and a new document of the restored A target runs the bootstrap
// script. WebKit does not cache a page that was loaded by a reload, so
// the test checks new documents with same-site navigations instead.
func TestBackAndForwardAcrossSites(t *testing.T) {
	backAndForward(t, fmt.Sprintf(":%d", 200+time.Now().Nanosecond()%50), false)
}

// W1b closure review (tree ef4d5221): a page cached while the connection
// that enabled its agents closes comes back with those agents under the
// next connection. The new connection's agent record has no entry for the
// restored target, and its setup sent Page.enable as to a fresh backend:
// "Page.enable: Page domain already enabled", and every later call of the
// page was refused. A connection cannot tell a restored target from a
// fresh one (both arrive as a provisional Target.targetCreated with an id
// it never saw), so the session resets an agent whose state it does not
// know before it enables it. Now the restored A target gets the desired
// state of the second connection, as in TestBackAndForwardAcrossSites.
func TestBackAcrossAConnectionChange(t *testing.T) {
	backAndForward(t, fmt.Sprintf(":%d", 20000+os.Getpid()%10000), true)
}

// backAndForward runs the back and forward checks on its own broadway
// display. With reconnect, the page caller, its daemon connection and the
// daemon end while A is in the back/forward cache and B is active; a new
// daemon attaches B and the same desired state is set again before back.
func backAndForward(t *testing.T, display string, reconnect bool) {
	sites := &twoSites{reqs: map[string][2]string{}}
	sites.a, sites.b = sites.serve(t, "127.0.0.1"), sites.serve(t, "localhost")
	startProcess(t, nil, "broadwayd", display)
	time.Sleep(500 * time.Millisecond)
	addr := freeAddr(t)
	startProcess(t, []string{"GDK_BACKEND=broadway", "BROADWAY_DISPLAY=" + display, "WEBKIT_INSPECTOR_HTTP_SERVER=" + addr}, miniBrowser, sites.a+"/a.html")
	tr := webkitgtk.New(addr)
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		pages, err := tr.Pages(ctx, "webkitgtk-"+addr)
		cancel()
		if err == nil && len(pages) > 0 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("MiniBrowser did not list a page at %s within 15 s", addr)
		}
	}
	if r := cli(t, "daemon", "start", "--transport", "webkitgtk", "--addr", addr); r.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", r.code, r.stderr)
	}
	t.Cleanup(func() {
		if r := cli(t, "daemon", "stop"); r.code != 0 {
			t.Errorf("daemon stop: exit %d, stderr %q", r.code, r.stderr)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dc, err := daemon.Connect(ctx, daemon.Options{NoStart: true})
	if err != nil {
		t.Fatalf("connecting to the daemon: %v", err)
	}
	defer func() { dc.Close() }()
	pc, err := dc.AttachPage(ctx, daemon.PageOptions{Page: daemon.PageReq{Transport: "webkitgtk", Addr: addr}, Owner: daemon.Owner{Session: "bfcache"}})
	if err != nil {
		t.Fatalf("attaching the page: %v", err)
	}
	defer func() { _ = pc.Close() }()
	call := func(method string, params any) {
		t.Helper()
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err := pc.Send(c, method, params); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	evalOnce := func(expr string) (string, error) {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		raw, err := pc.Send(c, "Runtime.evaluate", map[string]any{"expression": "String(" + expr + ")", "returnByValue": true})
		if err != nil {
			return "", err
		}
		var r struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", fmt.Errorf("Runtime.evaluate reply %s: %w", raw, err)
		}
		return r.Result.Value, nil
	}
	eval := func(expr string) string {
		t.Helper()
		v, err := evalOnce(expr)
		if err != nil {
			t.Fatalf("evaluate %s: %v", expr, err)
		}
		return v
	}
	// navigate runs js and waits until the page shows url, loaded. Calls
	// fail while a navigation swaps targets; a call refused for good fails
	// the test at the deadline with its error.
	navigate := func(js, url, what string) {
		t.Helper()
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, _ = pc.Send(c, "Runtime.evaluate", map[string]any{"expression": js})
		cancel()
		var last string
		for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			v, err := evalOnce("location.href + ' ' + document.readyState")
			if err == nil && v == url+" complete" {
				return
			}
			last = fmt.Sprintf("%q, %v", v, err)
		}
		t.Fatalf("%s: the page did not show %s within 20 s; last check: %s", what, url, last)
	}
	// wantHeaders checks that the request of url carried the overrides.
	wantHeaders := func(url, where string) {
		t.Helper()
		h, ok := sites.request(url)
		if !ok || h != [2]string{bfcacheUA, "1"} {
			t.Fatalf("%s: request %s carried User-Agent and X-Iwdp %q (seen %v), want %q and \"1\"", where, url, h, ok, bfcacheUA)
		}
	}
	// requestFromPage sends a synchronous request from the current
	// document and checks its headers.
	n := 0
	requestFromPage := func(origin, where string) {
		t.Helper()
		n++
		url := fmt.Sprintf("%s/probe?%d", origin, n)
		js := fmt.Sprintf("(function(){ var x = new XMLHttpRequest(); x.open('GET', %q, false); x.send(); return x.status; })()", url)
		if got := eval(js); got != "200" {
			t.Fatalf("%s: request %s from the page: status %s", where, url, got)
		}
		wantHeaders(url, where)
	}
	// newDocument navigates within the site and checks the new document.
	newDocument := func(url, where string) {
		t.Helper()
		navigate(fmt.Sprintf("location.href = %q", url), url, where)
		if got := eval("window.__iwdpBoot"); got != "on" {
			t.Fatalf("%s: window.__iwdpBoot = %q, want the bootstrap script's \"on\"", where, got)
		}
		if got := eval("navigator.userAgent"); got != bfcacheUA {
			t.Fatalf("%s: navigator.userAgent = %q, want the override %q", where, got, bfcacheUA)
		}
		wantHeaders(url, where)
	}
	marker := func(want, where string) {
		t.Helper()
		if got := eval("window.__marker"); got != want {
			t.Fatalf("%s: window.__marker = %q, want %q (the document from the back/forward cache)", where, got, want)
		}
	}

	setState := func() {
		t.Helper()
		call("Page.setBootstrapScript", map[string]string{"source": "window.__iwdpBoot = 'on';"})
		call("Page.overrideUserAgent", map[string]string{"value": bfcacheUA})
		call("Debugger.enable", nil)
		call("Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"X-Iwdp": "1"}})
	}

	a1, b1 := sites.a+"/a.html?1", sites.b+"/b.html?1"
	navigate("1", sites.a+"/a.html", "first load")
	setState()
	newDocument(a1, "site A")
	eval("window.__marker = 'A'")

	navigate(fmt.Sprintf("location.href = %q", b1), b1, "to site B")
	requestFromPage(sites.b, "site B")
	eval("window.__marker = 'B'")

	if reconnect {
		// A is cached; the connection that enabled its agents ends.
		if err := pc.Close(); err != nil {
			t.Fatalf("closing the first page caller: %v", err)
		}
		dc.Close()
		if r := cli(t, "daemon", "stop"); r.code != 0 {
			t.Fatalf("stopping the first daemon: exit %d, stderr %q", r.code, r.stderr)
		}
		if r := cli(t, "daemon", "start", "--transport", "webkitgtk", "--addr", addr); r.code != 0 {
			t.Fatalf("starting the second daemon: exit %d, stderr %q", r.code, r.stderr)
		}
		if dc, err = daemon.Connect(ctx, daemon.Options{NoStart: true}); err != nil {
			t.Fatalf("connecting to the second daemon: %v", err)
		}
		if pc, err = dc.AttachPage(ctx, daemon.PageOptions{Page: daemon.PageReq{Transport: "webkitgtk", Addr: addr}, Owner: daemon.Owner{Session: "bfcache"}}); err != nil {
			t.Fatalf("attaching site B through the second daemon: %v", err)
		}
		marker("B", "site B under the second daemon")
		setState()
	}

	navigate("history.back()", a1, "back to site A")
	marker("A", "after back")
	requestFromPage(sites.a, "site A after back")

	navigate("history.forward()", b1, "forward to site B")
	marker("B", "after forward")
	requestFromPage(sites.b, "site B after forward")

	navigate("history.back()", a1, "back to site A again")
	marker("A", "after the second back")
	requestFromPage(sites.a, "site A after the second back")
	newDocument(sites.a+"/a.html?2", "a new document of the restored site A target")
}
