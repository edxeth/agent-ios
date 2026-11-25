package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// The iwdp of the second device check (docs/known-limits.md, "iwdp does
// not list pages while a page is connected"): while any WebSocket to one of
// its pages is open, it closes every listing request without a reply.
// Commands on the attached page must not need the listing; commands that
// need it fail with advice and never take iwdp for dead.

// silentListingPages is a daemon on a fake iwdp with two pages of one
// reserved physical device ("Fixture" first, then "Other"); the listing
// closes its requests without a reply while either page has a connection.
func silentListingPages(t *testing.T) (env []string, l *fakewebkit.Listing, a, b *fakewebkit.Server) {
	t.Helper()
	page := func() *fakewebkit.Server {
		f := fakewebkit.New(fakewebkit.Options{})
		t.Cleanup(f.Close)
		f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 2}})
		return f
	}
	a, b = page(), page()
	l = fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("00008020-0000000000000001", "iPhone X", "16.7.10")
	dev.AddPage("Fixture", "https://fixture.example/", a.URL)
	dev.AddPage("Other", "https://other.example/", b.URL)
	l.EmptyReplyWhileConnected(a, b)
	env = privateDaemon(t)
	if res := runCLI(t, "", env, "daemon", "start", "--transport", "iwdp", "--addr", l.Addr()); res.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", res.code, res.stderr)
	}
	if res := runCLI(t, "", env, "device", "reserve", "--owner", "default"); res.code != 0 {
		t.Fatalf("device reserve: exit %d, stderr %q", res.code, res.stderr)
	}
	return env, l, a, b
}

// listingAnswers reports whether the fake listing answers GET /json now.
func listingAnswers(t *testing.T, l *fakewebkit.Listing) bool {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + l.Addr() + "/json")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func wantEval2(t *testing.T, res cliResult, what string) {
	t.Helper()
	if res.code != 0 || strings.TrimSpace(res.stdout) != "2" {
		t.Fatalf("%s: exit %d, stdout %q, stderr %q; want 2", what, res.code, res.stdout, res.stderr)
	}
}

// The device's failure (F1): after the first attach, plain "eval 1+1"
// failed with exit 5 every time, because each command read the listing.
// The session's page is attached, so later commands run without the
// listing.
func TestCLIAttachedPageCommandsNeedNoListing(t *testing.T) {
	env, l, a, b := silentListingPages(t)
	wantEval2(t, runCLI(t, "", env, "eval", "1+1"), "the first eval (cold attach)")
	if listingAnswers(t, l) {
		t.Fatal("the fake listing answers while a page is connected; the model is off")
	}
	for range 3 {
		wantEval2(t, runCLI(t, "", env, "eval", "1+1"), "eval on the attached page")
	}
	if res := runCLI(t, "", env, strings.Fields(heldRegistryCommand)...); res.code != 0 {
		t.Fatalf("registry command %q on the attached page: exit %d, stdout %q, stderr %q", heldRegistryCommand, res.code, res.stdout, res.stderr)
	}
	if n := a.ConnectionCount(); n != 1 {
		t.Fatalf("page Fixture got %d connections, want 1 (the attach is reused)", n)
	}
	if n := b.ConnectionCount(); n != 0 {
		t.Fatalf("page Other got %d connections, want 0", n)
	}
}

// The session's current page chosen with "tab <id>" is used without the
// listing while it is attached.
func TestCLISelectedTabNeedsNoListing(t *testing.T) {
	env, l, a, b := silentListingPages(t)
	if res := runCLI(t, "", env, "tab", "list"); res.code != 0 || !strings.Contains(res.stdout, "t2") {
		t.Fatalf("tab list before any attach: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	if res := runCLI(t, "", env, "tab", "t2"); res.code != 0 {
		t.Fatalf("tab t2: exit %d, stderr %q", res.code, res.stderr)
	}
	if listingAnswers(t, l) {
		t.Fatal("the fake listing answers while a page is connected; the model is off")
	}
	wantEval2(t, runCLI(t, "", env, "eval", "1+1"), "eval on the selected tab")
	if na, nb := a.ConnectionCount(), b.ConnectionCount(); na != 0 || nb != 1 {
		t.Fatalf("connections: Fixture %d, Other %d; want 0 and 1 (the selected tab)", na, nb)
	}
}

// Commands that need the listing fail with advice while a page connection
// is open: what happened, what still works, and what to do. None of them
// takes iwdp for dead.
func TestCLIListingCommandsWhileAPageIsConnected(t *testing.T) {
	env, _, _, _ := silentListingPages(t)
	wantEval2(t, runCLI(t, "", env, "eval", "1+1"), "the first eval (cold attach)")
	for _, args := range [][]string{
		{"tab", "list"},
		{"tab", "new", "https://new.example/"},
		{"tab", "close", "t2"},
		{"tab", "t2"},
		{"get", "url", "--page", "Other"},
		{"eval", "1+1", "--session", "other"},
	} {
		res := runCLI(t, "", env, args...)
		if res.code != 5 {
			t.Errorf("%v: exit %d, stderr %q; want exit 5", args, res.code, res.stderr)
			continue
		}
		for _, want := range []string{
			"did not answer the page list while the daemon has 1 page connection open",
			"commands on an attached page still work",
			"agent-ios daemon stop",
			"closed the connection without an answer",
		} {
			if !strings.Contains(res.stderr, want) {
				t.Errorf("%v: stderr %q does not contain %q", args, res.stderr, want)
			}
		}
		for _, bad := range []string{"not running", "busy or stuck", "within"} {
			if strings.Contains(res.stderr, bad) {
				t.Errorf("%v: stderr %q contains %q", args, res.stderr, bad)
			}
		}
	}
	wantEval2(t, runCLI(t, "", env, "eval", "1+1"), "eval on the attached page after the refused listings")
}
