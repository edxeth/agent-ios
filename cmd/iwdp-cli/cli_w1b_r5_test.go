package main

import (
	"context"
	"net"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// cookieMock is a page that answers Page.getCookies with no cookies.
func cookieMock() *testutil.MockServer {
	m := testutil.NewMockServer()
	m.HandleFunc("Page.getCookies", map[string]any{"cookies": []any{}})
	return m
}

// cookieMockAt is cookieMock listening on wsURL, the URL of an earlier mock
// that was closed: the case of an ephemeral port that the system gives
// again.
func cookieMockAt(t *testing.T, wsURL string) *testutil.MockServer {
	t.Helper()
	m := cookieMock()
	handler := m.Server.Config.Handler
	m.Server.Close()
	u, err := url.Parse(wsURL)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(handler)
	s.Listener = l
	s.Start()
	m.Server, m.URL = s, wsURL
	t.Cleanup(m.Close)
	return m
}

// waitLost waits until the daemon that CLI runs with env reach reports the
// page at wsURL lost.
func waitLost(t *testing.T, env []string, wsURL string) {
	t.Helper()
	var dir string // the last value is the one the CLI process gets
	for _, kv := range cliCommand(t, "", env, "cookies").Env {
		if v, ok := strings.CutPrefix(kv, "XDG_RUNTIME_DIR="); ok {
			dir = v
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := daemon.Connect(ctx, daemon.Options{Dir: filepath.Join(dir, "iwdp"), NoStart: true, Version: daemon.BuildVersion()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	eventually(t, 5*time.Second, wsURL+" to be lost", func() bool {
		s, err := c.Status(ctx)
		if err != nil {
			return false
		}
		for _, p := range s.Pages {
			if strings.HasSuffix(p.Key, wsURL) && p.State == "lost" {
				return true
			}
		}
		return false
	})
}

// R5-4 (review w1b-repair-round5, after the reviewer's URL-reuse
// reproduction): a test whose mock gets the loopback URL of a mock that an
// earlier test closed reaches the new mock. Before, both tests shared the
// suite daemon, which keeps the closed page as lost and refuses the URL
// with exit 4 before any connection. The lost-lease guard itself is
// unchanged: inside one daemon the reused URL is still refused until
// --takeover.
func TestReusedMockURLIsAFreshPageForTheNextTest(t *testing.T) {
	var oldURL string
	t.Run("first", func(t *testing.T) {
		old := cookieMock()
		oldURL = old.URL
		env := []string{"IWDP_WS_URL=" + oldURL}
		if r := runCLI(t, "", env, "cookies", "--page", oldURL); r.code != 0 {
			old.Close()
			t.Fatalf("cookies: exit %d, stderr %q", r.code, r.stderr)
		}
		old.Close()
		waitLost(t, env, oldURL)
	})
	t.Run("second", func(t *testing.T) {
		fresh := cookieMockAt(t, oldURL)
		r := runCLI(t, "", []string{"IWDP_WS_URL=" + oldURL}, "cookies", "--page", oldURL)
		if r.code != 0 || fresh.ConnectionCount() != 1 {
			t.Fatalf("cookies on the reused URL: exit %d, connections %d, stderr %q; want exit 0 and one connection", r.code, fresh.ConnectionCount(), r.stderr)
		}
	})
	t.Run("guard", func(t *testing.T) {
		old := cookieMock()
		env := []string{"IWDP_WS_URL=" + old.URL}
		if r := runCLI(t, "", env, "cookies", "--page", old.URL); r.code != 0 {
			old.Close()
			t.Fatalf("cookies: exit %d, stderr %q", r.code, r.stderr)
		}
		old.Close()
		waitLost(t, env, old.URL)
		fresh := cookieMockAt(t, old.URL)
		r := runCLI(t, "", env, "cookies", "--page", old.URL)
		if r.code != 4 || !strings.Contains(r.stderr, "connection to page was closed by someone else") || fresh.ConnectionCount() != 0 {
			t.Fatalf("reused URL in the same daemon: exit %d, connections %d, stderr %q; want the lost-lease refusal (exit 4, no connection)", r.code, fresh.ConnectionCount(), r.stderr)
		}
		if r := runCLI(t, "", env, "cookies", "--page", old.URL, "--takeover"); r.code != 0 || fresh.ConnectionCount() != 1 {
			t.Fatalf("--takeover: exit %d, connections %d, stderr %q", r.code, fresh.ConnectionCount(), r.stderr)
		}
	})
}
