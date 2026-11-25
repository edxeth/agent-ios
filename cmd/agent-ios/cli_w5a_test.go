package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// cdpPage is a fake page with the agent runtime of a page with one button
// "Save" (ref e1 in a snapshot) that also evaluates "1+1" for CDP clients.
type cdpPage struct {
	f     *fakewebkit.Server
	mu    sync.Mutex
	calls []string // runtime calls, in order
}

func newCDPPage(t *testing.T) *cdpPage {
	t.Helper()
	p := &cdpPage{f: fakewebkit.New(fakewebkit.Options{})}
	t.Cleanup(p.f.Close)
	p.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var params struct{ Expression string }
		_ = json.Unmarshal(c.Params, &params)
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(params.Expression); m != nil {
			return map[string]any{"result": map[string]any{"type": "object", "value": p.runtime(m[1])}}, nil
		}
		if params.Expression == "1+1" {
			return map[string]any{"result": map[string]any{"type": "number", "value": 2, "description": "2"}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 0, "description": "0"}}, nil
	})
	return p
}

func (p *cdpPage) runtime(fn string) any {
	p.mu.Lock()
	p.calls = append(p.calls, fn)
	p.mu.Unlock()
	ok := func(v any) any { return map[string]any{"ok": true, "value": v} }
	switch fn {
	case "snapshot":
		return ok(map[string]any{
			"tree":    []any{map[string]any{"role": "button", "name": "Save", "ref": "e1"}},
			"refs":    []any{map[string]any{"ref": "e1", "role": "button", "name": "Save", "unique": true}},
			"nextRef": 2, "doc": "d1", "url": "http://x.test/", "title": "X",
		})
	case "get":
		return ok("Save")
	case "ping":
		return ok(true)
	}
	return map[string]any{"ok": false, "error": map[string]string{"code": "unsupported", "message": "fake page has no " + fn}}
}

// cliFreePort returns a loopback port that was free a moment ago.
func cliFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// mustCLI runs agent-ios and fails the test unless it exits 0.
func mustCLI(t *testing.T, args ...string) string {
	t.Helper()
	res := runCLI(t, "", nil, args...)
	if res.code != 0 {
		t.Fatalf("agent-ios %s: exit %d, stdout %q, stderr %q", strings.Join(args, " "), res.code, res.stdout, res.stderr)
	}
	return res.stdout
}

// leaseEpoch is the lease epoch of session on its one page (session info).
func leaseEpoch(t *testing.T, session string) uint64 {
	t.Helper()
	var info struct {
		Pages []struct {
			Epoch uint64 `json:"epoch"`
			Lease string `json:"lease"`
		} `json:"pages"`
	}
	out := mustCLI(t, "--session", session, "session", "info", "--json")
	if err := json.Unmarshal([]byte(out), &info); err != nil || len(info.Pages) != 1 || info.Pages[0].Lease != "active" {
		t.Fatalf("session info of %s: %q (%v), want one active lease", session, out, err)
	}
	return info.Pages[0].Epoch
}

func cdpCall(t *testing.T, c *cdptest.Client, session, method string, params any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.Call(ctx, session, method, params)
	if err != nil {
		t.Fatalf("CDP %s: %v", method, err)
	}
	return res
}

func dialCDP(t *testing.T, url string) *cdptest.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := cdptest.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dialing the URL that get cdp-url printed: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// lastAttach returns the session id and target of the last
// Target.attachedToTarget event.
func lastAttach(t *testing.T, c *cdptest.Client) (string, string, string) {
	t.Helper()
	var last cdptest.Event
	for _, e := range c.Events() {
		if e.Method == "Target.attachedToTarget" {
			last = e
		}
	}
	var a struct {
		SessionID  string `json:"sessionId"`
		TargetInfo struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfo"`
	}
	if err := json.Unmarshal(last.Params, &a); err != nil {
		t.Fatalf("no attachedToTarget event: %v", err)
	}
	return a.SessionID, a.TargetInfo.Type, a.TargetInfo.TargetID
}

// Design 7.8 item 11 (a), (d) and (e) through the real CLI and daemon
// (each agent-ios run is its own process; the daemon is the test's own):
//
//	(a) "--session s1 snapshot -i", then "--session s1 get cdp-url"; a CDP
//	    client connects with the printed URL, attaches the tab and then its
//	    page as Puppeteer does, and evaluates in the page; s1's lease epoch
//	    is unchanged and the snapshot's ref still works for "get text @e1".
//	(d) "--session s2 get title" on the page exits 4 with the lease
//	    message; a client through the URL issued for s2 lists the page but
//	    Target.attachToTarget fails with the lease message; s1's epoch and
//	    its CDP connection are unchanged.
//	(e) after "--session s1 close", the s1 URLs get 404.
func TestCDPSessionBindingThroughTheCLI(t *testing.T) {
	p := newCDPPage(t)
	port := cliFreePort(t)
	if out := mustCLI(t, "cdp", "start", "--port", strconv.Itoa(port)); !strings.Contains(out, "127.0.0.1:"+strconv.Itoa(port)) {
		t.Fatalf("cdp start: %q", out)
	}
	// The CLI takes --page after the command words (registry commands).
	pg := []string{"--page", p.f.URL}
	s1 := func(args ...string) []string { return append(append([]string{"--session", "s1"}, args...), pg...) }
	if out := mustCLI(t, s1("snapshot", "-i")...); !strings.Contains(out, "[ref=e1]") {
		t.Fatalf("snapshot -i: %q, want the button with ref e1", out)
	}
	epoch := leaseEpoch(t, "s1")

	// (a)
	ws := strings.TrimSpace(mustCLI(t, s1("get", "cdp-url")...))
	var forms struct{ WS, HTTP string }
	if err := json.Unmarshal([]byte(mustCLI(t, s1("get", "cdp-url", "--json")...)), &forms); err != nil || forms.WS != ws {
		t.Fatalf("get cdp-url --json: %+v (%v), want ws %q", forms, err, ws)
	}
	c1 := dialCDP(t, ws)
	cdpCall(t, c1, "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}},
	})
	tab, kind, _ := lastAttach(t, c1)
	if kind != "tab" {
		t.Fatalf("browser auto-attach attached a %s, want the tab", kind)
	}
	cdpCall(t, c1, tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true})
	page, kind, pageID := lastAttach(t, c1)
	if kind != "page" || page == tab {
		t.Fatalf("tab auto-attach attached a %s (session %q), want the page in its own session", kind, page)
	}
	var ev struct {
		Result struct{ Value any } `json:"result"`
	}
	if err := json.Unmarshal(cdpCall(t, c1, page, "Runtime.evaluate", map[string]any{"expression": "1+1", "returnByValue": true}), &ev); err != nil || ev.Result.Value != float64(2) {
		t.Fatalf("Runtime.evaluate 1+1 through CDP = %+v (%v), want 2", ev, err)
	}
	if got := leaseEpoch(t, "s1"); got != epoch {
		t.Fatalf("s1's lease epoch went from %d to %d after the CDP attach", epoch, got)
	}
	if out := strings.TrimSpace(mustCLI(t, s1("get", "text", "@e1")...)); out != "Save" {
		t.Fatalf("get text @e1 after the CDP attach = %q, want Save", out)
	}

	// (d)
	res := runCLI(t, "", nil, "--session", "s2", "get", "title", "--page", p.f.URL)
	if res.code != 4 || !strings.Contains(res.stderr, `leased by session "s1"`) {
		t.Fatalf("s2 get title: exit %d, stderr %q; want exit 4 with the lease message", res.code, res.stderr)
	}
	ws2 := strings.TrimSpace(mustCLI(t, "--session", "s2", "get", "cdp-url", "--page", p.f.URL))
	if ws2 == ws {
		t.Fatal("s2 got s1's endpoint URL")
	}
	c2 := dialCDP(t, ws2)
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(cdpCall(t, c2, "", "Target.getTargets", nil), &targets); err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, ti := range targets.TargetInfos {
		listed = listed || (ti.Type == "page" && ti.TargetID == pageID)
	}
	if !listed {
		t.Fatalf("s2's getTargets = %+v, want the page %s listed", targets.TargetInfos, pageID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ce *cdptest.Error
	if _, err := c2.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": pageID, "flatten": true}); !errors.As(err, &ce) || !strings.Contains(ce.Message, `leased by session "s1"`) {
		t.Fatalf("s2 Target.attachToTarget: %v, want the lease message", err)
	}
	if got := leaseEpoch(t, "s1"); got != epoch {
		t.Fatalf("s1's lease epoch went from %d to %d after s2's attempts", epoch, got)
	}
	select {
	case <-c1.Done():
		t.Fatal("s1's CDP connection closed after s2's attempts")
	default:
	}
	if err := json.Unmarshal(cdpCall(t, c1, page, "Runtime.evaluate", map[string]any{"expression": "1+1", "returnByValue": true}), &ev); err != nil || ev.Result.Value != float64(2) {
		t.Fatalf("s1's CDP evaluate after s2's attempts = %+v (%v), want 2", ev, err)
	}

	// (e)
	if out := mustCLI(t, "--session", "s1", "close"); !strings.Contains(out, "released 1 page lease") {
		t.Fatalf("s1 close: %q", out)
	}
	for _, u := range []string{forms.HTTP + "/json/version", forms.HTTP + "/json/list"} {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET of an s1 URL after close: %d, want 404", resp.StatusCode)
		}
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if _, resp, err := cdptest.Dial(ctx2, ws, nil); err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("s1 WebSocket URL after close: %v, %v; want a refused upgrade with 404", err, resp)
	}
}

// Design 5.1 and 2.2: "daemon start --cdp-port" starts the CDP endpoint
// with the daemon, "daemon status" shows it, and "daemon stop" closes its
// port.
func TestDaemonCDPPortFlagThroughTheCLI(t *testing.T) {
	port := cliFreePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	mustCLI(t, "daemon", "start", "--cdp-port", strconv.Itoa(port))
	if out := mustCLI(t, "daemon", "status"); !strings.Contains(out, "CDP endpoint: listening on "+addr) {
		t.Fatalf("daemon status: %q, want the CDP endpoint on %s", out, addr)
	}
	if out := mustCLI(t, "cdp", "status"); !strings.Contains(out, "listening on "+addr) {
		t.Fatalf("cdp status: %q", out)
	}
	mustCLI(t, "daemon", "stop")
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatalf("the CDP port %s accepts connections after daemon stop", addr)
	}
}
