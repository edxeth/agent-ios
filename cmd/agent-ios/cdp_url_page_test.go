package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/cdp/cdptest"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Design docs/design/agentic-parity.md 5.2 through the CLI: get cdp-url
// selects the session's current page "resolved like the page of any
// command (exit 5 when there is none)": the tab chosen with "tab <id>",
// else the session's default page, else the listing's first page; --page
// takes a tab id as on any command.

// cliTwoPages is a daemon on a fake iwdp listing of one simulator with the
// pages "Fixture" (listed first) and "Other", with the CDP endpoint
// running.
type cliTwoPages struct {
	env          []string
	dev          *fakewebkit.ListedDevice
	other        *fakewebkit.Server
	otherListing *fakewebkit.ListedPage
}

func newCLITwoPages(t *testing.T) *cliTwoPages {
	t.Helper()
	page := func() *fakewebkit.Server {
		f := fakewebkit.New(fakewebkit.Options{})
		t.Cleanup(f.Close)
		f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 2}})
		return f
	}
	fixture, other := page(), page()
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("SIMULATOR", "Simulator", "16.7")
	dev.AddPage("Fixture", "https://fixture.example/", fixture.URL)
	p := &cliTwoPages{env: privateDaemon(t), dev: dev, other: other, otherListing: dev.AddPage("Other", "https://other.example/", other.URL)}
	if res := runCLI(t, "", p.env, "daemon", "start", "--transport", "iwdp", "--addr", l.Addr()); res.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", res.code, res.stderr)
	}
	// Port 0: the endpoint binds a free port itself, so no other test can
	// take a port picked in advance (get cdp-url keeps a running endpoint).
	mustRun(t, p.env, "cdp", "start", "--port", "0")
	return p
}

// cliEndpoint is "get cdp-url --json".
type cliEndpoint struct {
	WS, HTTP, Page string
}

// cliIssue runs "get cdp-url --json" with args and decodes its endpoint.
func cliIssue(t *testing.T, env []string, args ...string) cliEndpoint {
	t.Helper()
	res := runCLI(t, "", env, append(append([]string{}, args...), "get", "cdp-url", "--json")...)
	var e cliEndpoint
	if res.code != 0 || json.Unmarshal([]byte(res.stdout), &e) != nil {
		t.Fatalf("get cdp-url %v: exit %d, stdout %q, stderr %q", args, res.code, res.stdout, res.stderr)
	}
	return e
}

func mustRun(t *testing.T, env []string, args ...string) {
	t.Helper()
	if res := runCLI(t, "", env, args...); res.code != 0 {
		t.Fatalf("agent-ios %s: exit %d, stdout %q, stderr %q", strings.Join(args, " "), res.code, res.stdout, res.stderr)
	}
}

// After "tab t2" the URL is for page 2 (the endpoint's page and the
// record's /json/list); "--page t2" in another session names the same tab.
// That session leases nothing by issuing, and attaching through its URL
// fails with the lease message of s1, which holds the tab.
func TestCLIGetCDPURLFollowsTheSessionsTab(t *testing.T) {
	p := newCLITwoPages(t)
	mustRun(t, p.env, "--session", "s1", "tab", "list")
	mustRun(t, p.env, "--session", "s1", "tab", "t2")

	e := cliIssue(t, p.env, "--session", "s1")
	if e.Page != "Other" {
		t.Fatalf("get cdp-url after 'tab t2' selected %q, want the tab's page Other", e.Page)
	}
	resp, err := http.Get(e.HTTP + "/json/list")
	if err != nil {
		t.Fatal(err)
	}
	var list []struct{ Title, ID string }
	err = json.NewDecoder(resp.Body).Decode(&list)
	_ = resp.Body.Close()
	if err != nil || len(list) != 1 || list[0].Title != "Other" {
		t.Fatalf("the record's /json/list = %+v (%v), want only Other", list, err)
	}

	e2 := cliIssue(t, p.env, "--session", "s2", "--page", "t2")
	if e2.Page != "Other" || e2.WS == e.WS {
		t.Fatalf("get cdp-url --page t2 for s2 = %+v; want Other in a record of s2", e2)
	}
	res := runCLI(t, "", p.env, "--session", "s2", "session", "info", "--json")
	if res.code != 0 || !strings.Contains(res.stdout, `"pages":null`) {
		t.Fatalf("session info of s2 after issuing: exit %d, %q; want no page leased", res.code, res.stdout)
	}
	c := dialCDP(t, e2.WS)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ce *cdptest.Error
	if _, err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": list[0].ID, "flatten": true}); !errors.As(err, &ce) || !strings.Contains(ce.Message, `leased by session "s1"`) {
		t.Fatalf("s2 Target.attachToTarget on s1's tab: %v, want the lease message", err)
	}
}

// When the session's tab is gone (left the listing, its connection ended),
// get cdp-url exits 5 like the session's other commands, instead of
// issuing a URL for another page.
func TestCLIGetCDPURLWithoutTheSessionsPageExits5(t *testing.T) {
	p := newCLITwoPages(t)
	mustRun(t, p.env, "--session", "s1", "tab", "list")
	mustRun(t, p.env, "--session", "s1", "tab", "t2")
	p.dev.RemovePage(p.otherListing)
	p.other.CloseConnections()
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		res := runCLI(t, "", p.env, "--session", "s1", "session", "info", "--json")
		if strings.Contains(res.stdout, `"lease":"lost"`) {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("session info of s1: %q; want its page lost", res.stdout)
		}
	}
	if res := runCLI(t, "", p.env, "--session", "s1", "get", "title"); res.code != 5 {
		t.Fatalf("get title of s1 after its tab left: exit %d, stderr %q; want exit 5", res.code, res.stderr)
	}
	if res := runCLI(t, "", p.env, "--session", "s1", "get", "cdp-url"); res.code != 5 || res.stdout != "" {
		t.Fatalf("get cdp-url of s1 after its tab left: exit %d, stdout %q, stderr %q; want exit 5 and no URL", res.code, res.stdout, res.stderr)
	}
}

// Listing independence (docs/notes/w1c.md): the session's default page,
// attached by its last command, is selected without a listing read, so
// get cdp-url works on the iwdp that does not list pages while a page is
// connected.
func TestCLIGetCDPURLNeedsNoListingForTheAttachedPage(t *testing.T) {
	env, l, _, _ := silentListingPages(t)
	wantEval2(t, runCLI(t, "", env, "eval", "1+1"), "the first eval (cold attach)")
	mustRun(t, env, "cdp", "start", "--port", "0")
	if listingAnswers(t, l) {
		t.Fatal("the fake listing answers while a page is connected; the model is off")
	}
	if e := cliIssue(t, env); e.Page != "Fixture" {
		t.Fatalf("get cdp-url selected %q, want the session's attached page Fixture", e.Page)
	}
}
