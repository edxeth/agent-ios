//go:build cdpcompat

package cdpcompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/cdp/cdptest"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// fakePage is a fake WebKit page (no browser) for endpoint resolution.
func fakePage(t *testing.T) *fakewebkit.Server {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 2}})
	return f
}

// Design 7.8 test 10, endpoint resolution (no browser; the in-process
// daemon records every request through RunOptions.CDPObserve):
//   - each pinned client receives the exact output of "get cdp-url";
//   - agent-browser 0.41.2 with the ws form opens exactly that WebSocket
//     URL, token and endpoint path included, and makes no discovery
//     request; with the http form it requests the unprefixed /json/version
//     and gets 404 (then the unprefixed /json/list and /devtools/browser,
//     each 404);
//   - playwright-core 1.65.0 and puppeteer-core 25.16.0 with the http form
//     request /t/<token>/e/<endpoint>/json/version (Playwright with a
//     trailing slash) and then open the advertised WebSocket URL; with the
//     ws form they open exactly that URL and make no discovery request;
//   - no request without the token gets discovery data.
//
// agent-browser runs twice when PATH names another build (the user's
// decision): the pinned release binary, and the build that PATH's
// agent-browser runs (agent-browser-path-*), with no checksum pin. Both
// must pass; agent-browser-builds.json records each build.
func TestEndpointResolution(t *testing.T) {
	requireNode(t)
	pinned := pinnedAgentBrowser(t)
	builds := []agentBrowserBuild{pinned}
	fromPath, runPath := pathAgentBrowser(t, pinned)
	builds = append(builds, fromPath)
	writeBuilds(t, builds)
	e := startDaemon(t)
	f := fakePage(t)
	u := e.getCDPURL(t, "cdpcompat", f.URL)
	wsPath := strings.TrimPrefix(u.WS, "ws://"+u.Host)
	advertised := e.advertisedURL(t, u)
	if advertised != u.WS {
		t.Errorf("/json/version advertises %q, want the ws form %q", e.redact(advertised), e.redact(u.WS))
	}
	upgrade := request{Method: http.MethodGet, URI: wsPath, Host: u.Host, Upgrade: true, Status: http.StatusSwitchingProtocols}
	discovery := func(path string, status int) request {
		return request{Method: http.MethodGet, URI: path, Host: u.Host, Status: status}
	}

	type tcase struct {
		name, client, url string
		bin               string    // agent-browser binary
		want              []request // the requests of the run, in order
		// abOutcome checks agent-browser's exit code and output.
		abOutcome func(code int, out string) bool
		abWant    string // abOutcome in words
	}
	// With the ws form agent-browser attaches; on the fake page a later
	// command may fail, but only with a CDP error after the connection,
	// never in discovery or the WebSocket connect.
	attached := func(code int, out string) bool {
		return !strings.Contains(out, "discovery") && !strings.Contains(out, "WebSocket connect failed") &&
			(code == 0 || strings.Contains(out, "CDP error"))
	}
	discoveryFailed := func(code int, out string) bool {
		return code != 0 && strings.Contains(out, "All CDP discovery methods failed")
	}
	var cases []tcase
	abCases := func(prefix, bin string) {
		cases = append(cases,
			tcase{
				name: prefix + "-ws", client: "agent-browser", url: u.WS, bin: bin, want: []request{upgrade},
				abOutcome: attached, abWant: "exit 0 or a CDP error after the connection",
			},
			// agent-browser keeps only host and port of an http URL and
			// tries its discovery methods in turn; each misses the token
			// (design 5.1), so the command fails.
			tcase{name: prefix + "-http", client: "agent-browser", url: u.HTTP, bin: bin, want: []request{
				discovery("/json/version", http.StatusNotFound), discovery("/json/list", http.StatusNotFound),
				{Method: http.MethodGet, URI: "/devtools/browser", Host: u.Host, Upgrade: true, Status: http.StatusNotFound},
			}, abOutcome: discoveryFailed, abWant: `a failed exit with "All CDP discovery methods failed"`})
	}
	abCases("agent-browser", pinned.Binary)
	if runPath {
		abCases("agent-browser-path", fromPath.Binary)
	}
	cases = append(cases, []tcase{
		{name: "playwright-ws", client: "playwright-core", url: u.WS, want: []request{upgrade}},
		{name: "playwright-http", client: "playwright-core", url: u.HTTP, want: []request{discovery(u.Prefix+"/json/version/", http.StatusOK), upgrade}},
		{name: "puppeteer-ws", client: "puppeteer-core", url: u.WS, want: []request{upgrade}},
		{name: "puppeteer-http", client: "puppeteer-core", url: u.HTTP, want: []request{discovery(u.Prefix+"/json/version", http.StatusOK), upgrade}},
	}...)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mark := e.rec.mark()
			var unknown map[string]int
			if c.client == "agent-browser" {
				var code int
				var out string
				code, out, unknown = e.runAgentBrowser(t, c.bin, c.name, c.url, "get", "url")
				t.Logf("agent-browser exit %d: %s", code, strings.TrimSpace(out))
				if !c.abOutcome(code, out) {
					t.Errorf("agent-browser exit %d, output %q; want %s", code, strings.TrimSpace(out), c.abWant)
				}
			} else {
				run := e.runNode(t, c.name, c.client, "resolve", c.url)
				unknown = run.unknown
				ok, errText, _ := run.result.step("connect")
				t.Logf("%s connect ok=%v %s; sent %d commands: %v", c.client, ok, errText, len(run.result.Sent), run.result.methods())
				// The protocol hook saw every command: each method that the
				// endpoint counted as unknown is in the script's record.
				for m := range unknown {
					if !slices.Contains(run.result.methods(), m) {
						t.Errorf("the endpoint counted the unknown method %s, which %s's protocol hook did not record", m, c.client)
					}
				}
				// A client that opened the WebSocket sent commands, and its
				// hook recorded them.
				if len(run.result.Sent) == 0 {
					t.Errorf("%s opened the WebSocket, but its protocol hook recorded no command", c.client)
				}
			}
			got := e.rec.since(mark)
			t.Logf("requests: %s", e.redact(fmt.Sprint(got)))
			t.Logf("unknown methods (cdp log): %v", unknown)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("%s made the requests\n%s\nwant\n%s", c.name, e.redact(lines(got)), e.redact(lines(c.want)))
			}
		})
	}

	t.Run("no discovery without the token", func(t *testing.T) {
		e.checkTokenless(t, u)
		for _, r := range e.rec.since(0) {
			if !strings.HasPrefix(r.URI, u.Prefix+"/") && r.Status != http.StatusNotFound {
				t.Errorf("a request without the record's token and endpoint path got %d, want 404: %s", r.Status, e.redact(r.String()))
			}
		}
	})
}

func lines(rs []request) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString("  " + r.String() + "\n")
	}
	return b.String()
}

// advertisedURL reads webSocketDebuggerUrl from <http form>/json/version.
func (e *endpoint) advertisedURL(t *testing.T, u cdpURLs) string {
	t.Helper()
	code, body := httpGet(t, u.HTTP+"/json/version")
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal([]byte(body), &v); code != http.StatusOK || err != nil {
		t.Fatalf("GET <http form>/json/version: %d %q (%v)", code, e.redact(body), err)
	}
	return v.WebSocketDebuggerURL
}

// checkTokenless sends discovery requests and a WebSocket upgrade that
// lack the token, the endpoint id or both, or carry wrong ones, and checks
// that each gets 404 without discovery data.
func (e *endpoint) checkTokenless(t *testing.T, u cdpURLs) {
	t.Helper()
	token, id, _ := strings.Cut(strings.TrimPrefix(u.Prefix, "/t/"), "/e/")
	wrongToken := strings.Repeat("0", len(token))
	base := "http://" + u.Host
	var paths []string
	for _, p := range []string{"/json/version", "/json/version/", "/json", "/json/list", "/json/protocol"} {
		paths = append(paths, p, "/t/"+token+p, "/e/"+id+p, "/t/"+wrongToken+"/e/"+id+p, "/t/"+token+"/e/wrong"+p)
	}
	for _, p := range paths {
		code, body := httpGet(t, base+p)
		if code != http.StatusNotFound || strings.Contains(body, "webSocketDebuggerUrl") || strings.Contains(body, id) {
			t.Errorf("GET %s: %d %q, want 404 without discovery data", e.redact(p), code, e.redact(body))
		}
	}
	for _, p := range []string{"/devtools/browser/" + id, "/t/" + token + "/devtools/browser/" + id, "/t/" + wrongToken + "/e/" + id + "/devtools/browser/" + id} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, resp, err := cdptest.Dial(ctx, "ws://"+u.Host+p, nil)
		cancel()
		if err == nil {
			c.Close()
		}
		if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
			t.Errorf("WebSocket upgrade of %s: %v, %v; want a refused upgrade with 404", e.redact(p), err, resp)
		}
	}
}

// httpGet returns the status and body of a GET of rawURL.
func httpGet(t *testing.T, rawURL string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if ue := (*url.Error)(nil); errors.As(err, &ue) {
		t.Fatalf("GET request: %v", ue.Err) // without the URL, which holds the token
	} else if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, readAll(resp.Body)
}
