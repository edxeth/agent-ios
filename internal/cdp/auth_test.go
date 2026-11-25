package cdp

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
)

func httpDo(t *testing.T, method, url string, mod func(*http.Request)) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(testCtx(t), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mod != nil {
		mod(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// Design 7.8 item 2 (5.1): token, Origin, Host, PUT-only side effects,
// loopback binding, and no token in the log or status.
func TestAuthorization(t *testing.T) {
	r := newRig(t)
	ep := r.issue(t, "s1", "One", ScopePage)
	base := ep.HTTP // http://127.0.0.1:<port>/t/<token>/e/<endpoint>
	host := strings.TrimPrefix(base, "http://")
	host, _, _ = strings.Cut(host, "/")
	token := strings.Split(strings.TrimPrefix(base, "http://"+host+"/t/"), "/")[0]
	endpoint := base[strings.LastIndex(base, "/")+1:]
	if len(token) != 64 {
		t.Fatalf("token %d hex chars, want 64 (256 bits)", len(token))
	}
	wrongToken := strings.Repeat("0", 64)
	paths := []string{"/json/version", "/json/version/", "/json", "/json/list", "/json/protocol"}
	puts := []string{"/json/new?about:blank", "/json/close/X", "/json/activate/X"}

	// The right token and endpoint give discovery data; Browser is
	// "Safari/<iOS version> (agent-ios <version>)" (design 5.1).
	if code, body := httpDo(t, "GET", base+"/json/version", nil); code != 200 || !strings.Contains(body, ep.WS) || !strings.Contains(body, `"Browser":"Safari/16.7.10 (agent-ios test)"`) {
		t.Fatalf("GET /json/version with the token: %d %q", code, body)
	}
	// Missing token, wrong token, wrong endpoint: 404 with no discovery data.
	for _, prefix := range []string{"", "/t/" + wrongToken + "/e/" + endpoint, "/t/" + token + "/e/" + strings.Repeat("0", 32), "/t/" + token, "/e/" + endpoint} {
		for _, p := range paths {
			code, body := httpDo(t, "GET", "http://"+host+prefix+p, nil)
			if code != 404 || strings.Contains(body, "webSocketDebuggerUrl") || strings.Contains(body, "domains") {
				t.Errorf("GET %s%s: %d %q, want 404 without data", redactFor(prefix, token), p, code, body)
			}
		}
		for _, p := range puts {
			if code, _ := httpDo(t, "PUT", "http://"+host+prefix+p, nil); code != 404 {
				t.Errorf("PUT %s%s: %d, want 404", redactFor(prefix, token), p, code)
			}
		}
		for _, p := range []string{"/devtools/browser/" + endpoint, "/devtools/page/X"} {
			_, resp, err := cdptest.Dial(ctxFor(t, 5*time.Second), "ws://"+host+prefix+p, nil)
			if err == nil || resp == nil || resp.StatusCode != 404 {
				t.Errorf("WebSocket %s%s: err %v, response %v; want a refused upgrade with 404", redactFor(prefix, token), p, err, resp)
			}
		}
	}
	// Origin: any Origin header, null included, is refused.
	for _, origin := range []string{"http://evil.example", "null", ""} {
		withOrigin := func(req *http.Request) { req.Header["Origin"] = []string{origin} }
		if code, body := httpDo(t, "GET", base+"/json/version", withOrigin); code != 403 || strings.Contains(body, "webSocketDebuggerUrl") {
			t.Errorf("Origin %q: %d %q, want 403", origin, code, body)
		}
		_, resp, err := cdptest.Dial(ctxFor(t, 5*time.Second), ep.WS, http.Header{"Origin": {origin}})
		if err == nil || resp == nil || resp.StatusCode != 403 {
			t.Errorf("WebSocket with Origin %q: err %v; want 403", origin, err)
		}
	}
	// Host: only 127.0.0.1:<port> and localhost:<port> (DNS rebinding guard).
	_, port, _ := net.SplitHostPort(host)
	for _, h := range []string{"evil.example:" + port, "127.0.0.1:1", "localhost", "[::1]:" + port} {
		if code, body := httpDo(t, "GET", base+"/json/version", func(req *http.Request) { req.Host = h }); code != 403 || strings.Contains(body, "webSocketDebuggerUrl") {
			t.Errorf("Host %q: %d %q, want 403", h, code, body)
		}
	}
	if code, _ := httpDo(t, "GET", base+"/json/version", func(req *http.Request) { req.Host = "localhost:" + port }); code != 200 {
		t.Errorf("Host localhost:%s: %d, want 200", port, code)
	}
	// No side effects on GET.
	for _, p := range puts {
		if code, _ := httpDo(t, "GET", base+p, nil); code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d, want 405", p, code)
		}
	}
	for title, f := range r.fake {
		if n := len(f.Received()); n != 0 {
			t.Errorf("page %s received %d messages from refused requests: %v", title, n, received(f))
		}
	}
	// Loopback only.
	if !strings.HasPrefix(host, "127.0.0.1:") {
		t.Errorf("listener address %s, want 127.0.0.1", host)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
				continue
			}
			if c, err := net.DialTimeout("tcp", net.JoinHostPort(ipn.IP.String(), port), time.Second); err == nil {
				_ = c.Close()
				t.Errorf("the endpoint answers on %s; it must bind 127.0.0.1 only", ipn.IP)
			}
		}
	}
	// A client through the URL attaches, so the log has connection lines.
	c := dial(t, ep.WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	if _, err := c.Call(ctxFor(t, 5*time.Second), "", "Foo.bar", nil); err == nil {
		t.Fatal("Foo.bar succeeded")
	}
	st := r.srv.Status()
	for _, s := range []string{r.log.text(), statusText(t, st)} {
		if strings.Contains(s, token) || strings.Contains(s, endpoint) {
			t.Fatalf("the token or endpoint id appears in the log or status:\n%s", s)
		}
	}
	if !st.Listening || len(st.Endpoints) != 1 || st.Endpoints[0].Session != "s1" || st.Endpoints[0].Connections != 1 {
		t.Fatalf("status = %+v", st)
	}
}

func redactFor(s, token string) string { return strings.ReplaceAll(s, token, "<token>") }

func statusText(t *testing.T, st Status) string {
	t.Helper()
	return strings.Join([]string{st.Addr, strings.Join(st.AllowOrigins, ","), func() string {
		var b strings.Builder
		for _, e := range st.Endpoints {
			b.WriteString(e.Session + e.Scope + e.Page)
		}
		return b.String()
	}()}, " ")
}

// --cdp-allow-origin: a listed origin passes, others stay refused.
func TestAllowedOrigin(t *testing.T) {
	r := newRig(t)
	r.srv.Stop()
	if _, err := r.srv.Start(0, []string{"http://tool.local"}); err != nil {
		t.Fatal(err)
	}
	ep := r.issue(t, "s1", "One", ScopePage)
	if code, _ := httpDo(t, "GET", ep.HTTP+"/json/version", func(req *http.Request) { req.Header.Set("Origin", "http://tool.local") }); code != 200 {
		t.Fatalf("allowed origin: %d, want 200", code)
	}
	if code, _ := httpDo(t, "GET", ep.HTTP+"/json/version", func(req *http.Request) { req.Header.Set("Origin", "http://other.local") }); code != 403 {
		t.Fatalf("other origin: %d, want 403", code)
	}
}

// Stop and RevokeSession end the records: their URLs get 404, and their
// connections close.
func TestRevokeAndStop(t *testing.T) {
	r := newRig(t)
	ep := r.issue(t, "s1", "One", ScopePage)
	other := r.issue(t, "s2", "One", ScopePage)
	c := dial(t, ep.WS)
	if n := r.srv.RevokeSession("s1"); n != 1 {
		t.Fatalf("RevokeSession = %d, want 1", n)
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the s1 connection is still open after the revocation")
	}
	if code, _ := httpDo(t, "GET", ep.HTTP+"/json/version", nil); code != 404 {
		t.Fatalf("revoked record: %d, want 404", code)
	}
	if code, _ := httpDo(t, "GET", other.HTTP+"/json/version", nil); code != 200 {
		t.Fatalf("s2 record after s1's revocation: %d, want 200", code)
	}
	r.srv.Stop()
	if r.srv.Running() {
		t.Fatal("Running after Stop")
	}
	if _, err := r.srv.Issue(ctxFor(t, time.Second), IssueRequest{Session: "s2", Page: r.req}); err == nil {
		t.Fatal("Issue after Stop succeeded")
	}
}
