package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// lockedBuffer is a daemon log that the test reads while the daemon writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// freePort returns a loopback port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// runOn runs a registry command for session with the page selection page.
func runOn(t *testing.T, c *Client, session, page string, argv ...string) (RunResult, error) {
	t.Helper()
	return c.Run(ctxFor(t, 15*time.Second), RunRequest{Argv: argv, Owner: Owner{Session: session}, PageReq: PageReq{Page: page}}, nil)
}

func mustRunOn(t *testing.T, c *Client, session, page string, argv ...string) RunResult {
	t.Helper()
	res, err := runOn(t, c, session, page, argv...)
	if err != nil {
		t.Fatalf("%s: %v", strings.Join(argv, " "), err)
	}
	return res
}

// cdpURLs are the two forms of "get cdp-url --json" (design 5.1).
type cdpURLs struct {
	WS   string `json:"ws"`
	HTTP string `json:"http"`
}

// issueURLs runs "get cdp-url --json" for session on page.
func issueURLs(t *testing.T, c *Client, session, page string) cdpURLs {
	t.Helper()
	res, err := c.Run(ctxFor(t, 10*time.Second), RunRequest{Argv: []string{"get", "cdp-url"}, JSON: true, Owner: Owner{Session: session}, PageReq: PageReq{Page: page}}, nil)
	if err != nil {
		t.Fatalf("get cdp-url --json for %s: %v", session, err)
	}
	var u cdpURLs
	if err := json.Unmarshal(res.Data, &u); err != nil {
		t.Fatalf("get cdp-url --json data %s: %v", res.Data, err)
	}
	return u
}

// secretsOf splits an http form into its token and endpoint id.
func secretsOf(t *testing.T, httpForm string) (token, endpoint string) {
	t.Helper()
	_, rest, ok := strings.Cut(httpForm, "/t/")
	if !ok {
		t.Fatalf("http form %q has no /t/<token>/ prefix", httpForm)
	}
	token, endpoint, ok = strings.Cut(rest, "/e/")
	if !ok || len(token) != 64 || endpoint == "" || strings.Contains(endpoint, "/") {
		t.Fatalf("http form %q: want /t/<64 hex>/e/<endpoint>", httpForm)
	}
	return token, endpoint
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctxFor(t, 5*time.Second), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// Design 4.2 (cdp and get cdp-url rows), 5.1 and 5.5 through the daemon:
// the daemon flag --cdp-port starts the listener; cdp stop ends it and
// revokes the URLs; get cdp-url starts it again on that port, prints the
// ws form (one line), --form http prints the discovery base and --json
// both; cdp start on another port while it runs fails and names cdp stop;
// cdp log counts the unknown methods a client sent; cdp status describes
// the listener and the endpoint records.
func TestCDPCommandsThroughTheDaemon(t *testing.T) {
	port := freePort(t)
	o := startInProcess(t, RunOptions{CDPPort: port})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	addr := "127.0.0.1:" + strconv.Itoa(port)

	st := mustRunOn(t, c, "s1", "", "cdp", "status")
	if !strings.Contains(st.Text, "listening on "+addr) {
		t.Fatalf("cdp status after the daemon flag --cdp-port %d: %q, want listening on %s", port, st.Text, addr)
	}
	stop := mustRunOn(t, c, "s1", "", "cdp", "stop")
	if !strings.Contains(stop.Text, "Stopped the CDP endpoint") {
		t.Fatalf("cdp stop: %q", stop.Text)
	}
	if st := mustRunOn(t, c, "s1", "", "cdp", "status"); !strings.Contains(st.Text, "not running") {
		t.Fatalf("cdp status after cdp stop: %q, want not running", st.Text)
	}
	if stop := mustRunOn(t, c, "s1", "", "cdp", "stop"); !strings.Contains(stop.Text, "not running") {
		t.Fatalf("second cdp stop: %q, want not running", stop.Text)
	}

	ws := mustRunOn(t, c, "s1", f.URL, "get", "cdp-url").Text
	if !strings.HasPrefix(ws, "ws://"+addr+"/t/") || strings.Contains(ws, "\n") || !strings.Contains(ws, "/devtools/browser/") {
		t.Fatalf("get cdp-url = %q, want one ws://%s/t/<token>/e/<endpoint>/devtools/browser/<endpoint> line", ws, addr)
	}
	both := issueURLs(t, c, "s1", f.URL)
	token, endpoint := secretsOf(t, both.HTTP)
	if both.WS != ws || both.HTTP != "http://"+addr+"/t/"+token+"/e/"+endpoint || ws != "ws://"+addr+"/t/"+token+"/e/"+endpoint+"/devtools/browser/"+endpoint {
		t.Fatalf("--json = %+v, plain = %q; want the same record in both forms", both, ws)
	}
	if h := mustRunOn(t, c, "s1", f.URL, "get", "cdp-url", "--form", "http").Text; h != both.HTTP {
		t.Fatalf("get cdp-url --form http = %q, want %q", h, both.HTTP)
	}
	if code, body := httpGet(t, both.HTTP+"/json/version"); code != 200 || !strings.Contains(body, ws) {
		t.Fatalf("GET <http form>/json/version: %d %q, want the ws form as webSocketDebuggerUrl", code, body)
	}
	if _, err := runOn(t, c, "s1", f.URL, "get", "cdp-url", "--scope", "tab"); commands.ExitCodeOf(err) != commands.ExitUsage {
		t.Fatalf("get cdp-url --scope tab: %v (exit %d), want a usage error", err, commands.ExitCodeOf(err))
	}
	if _, err := runOn(t, c, "s1", "", "cdp", "start", "--port", strconv.Itoa(freePort(t))); err == nil || !strings.Contains(err.Error(), "cdp stop") {
		t.Fatalf("cdp start on another port while running: %v, want an error that names cdp stop", err)
	}
	if res := mustRunOn(t, c, "s1", "", "cdp", "start"); !strings.Contains(res.Text, addr) {
		t.Fatalf("cdp start while running: %q, want the running address %s", res.Text, addr)
	}

	cl, _, err := cdptest.Dial(ctxFor(t, 5*time.Second), ws, nil)
	if err != nil {
		t.Fatalf("dialing the printed URL: %v", err)
	}
	defer cl.Close()
	var ce *cdptest.Error
	if _, err := cl.Call(ctxFor(t, 5*time.Second), "", "Foo.notAMethod", nil); !errors.As(err, &ce) || ce.Code != -32601 {
		t.Fatalf("unknown method: %v, want error -32601", err)
	}
	if lg := mustRunOn(t, c, "s1", "", "cdp", "log"); !strings.Contains(lg.Text, "Foo.notAMethod") || !strings.Contains(lg.Text, "1") {
		t.Fatalf("cdp log = %q, want Foo.notAMethod counted once", lg.Text)
	}
	st = mustRunOn(t, c, "s1", "", "cdp", "status")
	if !strings.Contains(st.Text, `session "s1"`) || !strings.Contains(st.Text, "1 connection") {
		t.Fatalf("cdp status with a client: %q, want the s1 record with 1 connection", st.Text)
	}
}

// Design 7.8 item 2 at the daemon level (5.1): the token and the endpoint
// ids never appear in the daemon log, in cdp status (text and JSON), in
// cdp log or in the daemon's /v1/status, also after refused requests, an
// unknown method, a close that revokes the record and cdp stop.
func TestCDPTokenNeverInTheDaemonLogOrStatus(t *testing.T) {
	logBuf := &lockedBuffer{}
	o := startInProcess(t, RunOptions{Log: logBuf})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	if res := mustRunOn(t, c, "s1", "", "cdp", "start", "--port", "0"); !strings.Contains(res.Text, "127.0.0.1:") {
		t.Fatalf("cdp start --port 0: %q", res.Text)
	}
	u := issueURLs(t, c, "s1", f.URL)
	token, endpoint := secretsOf(t, u.HTTP)
	cl, _, err := cdptest.Dial(ctxFor(t, 5*time.Second), u.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	_, _ = cl.Call(ctxFor(t, 5*time.Second), "", "Foo.notAMethod", nil)
	if _, err := cl.Call(ctxFor(t, 5*time.Second), "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true}); err != nil {
		t.Fatal(err)
	}
	// A refused request (wrong Host) is logged with its path.
	req, _ := http.NewRequestWithContext(ctxFor(t, 5*time.Second), http.MethodGet, u.HTTP+"/json/version", nil)
	req.Host = "evil.example"
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
	var outputs []string
	for _, argv := range [][]string{{"cdp", "status"}, {"cdp", "log"}} {
		outputs = append(outputs, mustRunOn(t, c, "s1", "", argv...).Text)
		res, err := c.Run(ctxFor(t, 10*time.Second), RunRequest{Argv: argv, JSON: true, Owner: Owner{Session: "s1"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, string(res.Data))
	}
	if !strings.Contains(outputs[1], `"listening":true`) {
		t.Fatalf("cdp status --json = %s, want listening", outputs[1])
	}
	st, err := c.Status(ctxFor(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !st.CDP.Listening || len(st.CDP.Endpoints) != 1 {
		t.Fatalf("/v1/status cdp = %+v, want the listener with one endpoint record", st.CDP)
	}
	data, _ := json.Marshal(st)
	outputs = append(outputs, string(data))
	mustRunOn(t, c, "s1", "", "close")
	mustRunOn(t, c, "s1", "", "cdp", "stop")
	outputs = append(outputs, logBuf.String())
	if !strings.Contains(logBuf.String(), "cdp: refused GET") || !strings.Contains(logBuf.String(), "unknown method Foo.notAMethod") {
		t.Fatalf("the daemon log lacks the refused request or the unknown method:\n%s", logBuf.String())
	}
	for i, out := range outputs {
		if strings.Contains(out, token) || strings.Contains(out, endpoint) {
			t.Errorf("output %d contains the token or the endpoint id:\n%s", i, strings.ReplaceAll(strings.ReplaceAll(out, token, "<TOKEN>"), endpoint, "<ENDPOINT>"))
		}
	}
}

// Design 5.2 and 4.2 row "close": close revokes the session's endpoint
// records (their URLs get 404 and their connections close) and releases
// its leases; another session's record stays.
func TestCloseRevokesTheSessionsEndpoints(t *testing.T) {
	c := connect(t, startInProcess(t, RunOptions{}))
	f1 := newFake(t, fakewebkit.Options{})
	f2 := newFake(t, fakewebkit.Options{})
	mustRunOn(t, c, "s1", "", "cdp", "start", "--port", "0")
	u1, u2 := issueURLs(t, c, "s1", f1.URL), issueURLs(t, c, "s2", f2.URL)
	cl, _, err := cdptest.Dial(ctxFor(t, 5*time.Second), u1.WS, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if _, err := cl.Call(ctxFor(t, 5*time.Second), "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.WaitEvent(ctxFor(t, 5*time.Second), 0, "Target.attachedToTarget", nil); err != nil {
		t.Fatalf("the s1 client attached nothing: %v", err)
	}
	if res := mustRunOn(t, c, "s1", "", "close"); !strings.Contains(res.Text, "released 1 page lease") {
		t.Fatalf("close: %q, want the page lease of the CDP attachment released", res.Text)
	}
	if code, _ := httpGet(t, u1.HTTP+"/json/version"); code != 404 {
		t.Fatalf("s1 URL after close: %d, want 404", code)
	}
	if _, resp, err := cdptest.Dial(ctxFor(t, 5*time.Second), u1.WS, nil); err == nil || resp == nil || resp.StatusCode != 404 {
		t.Fatalf("s1 WebSocket after close: %v, %v; want a refused upgrade with 404", err, resp)
	}
	select {
	case <-cl.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the s1 CDP connection is still open after close")
	}
	if code, _ := httpGet(t, u2.HTTP+"/json/version"); code != 200 {
		t.Fatalf("s2 URL after s1's close: %d, want 200", code)
	}
	st, err := c.Status(ctxFor(t, 2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range st.Pages {
		if p.Session == "s1" {
			t.Fatalf("s1 still leases %s after close", p.Key)
		}
	}
}

// Design 2.3 and 5.2: daemon shutdown stops the CDP listener (its port
// refuses connections) and closes the CDP connections.
func TestDaemonShutdownStopsTheCDPListener(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	mustRunOn(t, c, "s1", "", "cdp", "start", "--port", "0")
	ws := mustRunOn(t, c, "s1", f.URL, "get", "cdp-url").Text
	cl, _, err := cdptest.Dial(ctxFor(t, 5*time.Second), ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	addr := strings.SplitN(strings.TrimPrefix(ws, "ws://"), "/", 2)[0]
	if err := c.Shutdown(ctxFor(t, 5*time.Second), false); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitGone(ctxFor(t, 8*time.Second), 7*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cl.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the CDP connection is still open after the daemon stopped")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = conn.Close()
		t.Fatalf("the CDP port %s still accepts connections after the daemon stopped", addr)
	}
}

// The daemon flag --cdp-port on a busy port ends "daemon run" with an
// error that names the flag, and leaves no socket behind (design 5.1).
func TestDaemonCDPPortBusyFailsTheStart(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	busy := l.Addr().(*net.TCPAddr).Port
	base := shortDir(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	dir := filepath.Join(base, "iwdp")
	err = Run(ctxFor(t, 10*time.Second), RunOptions{Dir: dir, Version: "test", CDPPort: busy, Log: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "--cdp-port "+strconv.Itoa(busy)) {
		t.Fatalf("daemon run --cdp-port %d (busy): %v, want an error naming the flag", busy, err)
	}
	if _, err := os.Lstat(SocketPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket is left after the failed start: %v", err)
	}
}

// Design 5.2: get cdp-url resolves the session's page like any command
// and exits 5 when there is none; it issues no record then.
func TestGetCDPURLWithoutAPageExits5(t *testing.T) {
	c := connect(t, startInProcess(t, RunOptions{}))
	mustRunOn(t, c, "s1", "", "cdp", "start", "--port", "0")
	res, err := runOn(t, c, "s1", "no-such-page", "get", "cdp-url")
	if commands.ExitCodeOf(err) != 5 || res.Text != "" {
		t.Fatalf("get cdp-url without a page: %q, %v (exit %d); want exit 5", res.Text, err, commands.ExitCodeOf(err))
	}
	if st := mustRunOn(t, c, "s1", "", "cdp", "status"); !strings.Contains(st.Text, "Endpoint records: 0") {
		t.Fatalf("cdp status after the failed issue: %q, want no record", st.Text)
	}
}

// "daemon status" shows the CDP listener (design 2.2: the status lists the
// CDP listener), and the daemon flag --cdp-port reaches RunOptions.
func TestDaemonStatusShowsTheCDPListener(t *testing.T) {
	c := connect(t, startInProcess(t, RunOptions{}))
	st, err := c.Status(ctxFor(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if text := statusText(c, st); !strings.Contains(text, "CDP endpoint: not running") {
		t.Fatalf("daemon status without the endpoint: %q", text)
	}
	mustRunOn(t, c, "s1", "", "cdp", "start", "--port", "0")
	if st, err = c.Status(ctxFor(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if text := statusText(c, st); !strings.Contains(text, "CDP endpoint: listening on "+st.CDP.Addr+", 0 endpoint record(s), 0 connection(s)") || st.CDP.Addr == "" {
		t.Fatalf("daemon status with the endpoint: %q", text)
	}
	if ro := runOptions(RunOptions{}, commands.Input{Flags: map[string]any{"cdp-port": 9333}}); ro.CDPPort != 9333 {
		t.Fatalf("runOptions with --cdp-port 9333: CDPPort %d", ro.CDPPort)
	}
}
