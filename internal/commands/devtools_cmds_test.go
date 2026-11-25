package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/transport/iwdp"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// dtHarness runs registry command lines against a fake iPhone page through
// a core.Manager, as the daemon's /v1/run does: each page command is one
// operation of its class, under core.WithLeaseHooks.
type dtHarness struct {
	f   *fakewebkit.Server
	m   *core.Manager
	req core.PageRequest
	dir string

	forwarded []Input // inputs that RunRemote received
}

func newDTHarness(t *testing.T) *dtHarness {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("00008020-0000000000000001", "iPhone X", "16.7.10")
	dev.AddPage("Fixture", "https://fixture.example/", f.URL)
	res := &core.Reservations{Dir: t.TempDir()}
	if _, err := res.Reserve(dev.ID, "default", time.Hour); err != nil {
		t.Fatal(err)
	}
	m := core.NewManager(core.ManagerOptions{
		Reservations: res, Session: webkit.SessionOptions{TargetWait: time.Second},
		Transport: func(_ context.Context, _, addr string) (transport.Transport, error) {
			return iwdp.New(iwdp.Options{ListingAddr: addr}), nil
		},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return &dtHarness{f: f, m: m, req: core.PageRequest{Transport: "iwdp", Addr: l.Addr()}, dir: t.TempDir()}
}

type dtEnv struct {
	h  *dtHarness
	mo *core.ManagedOp
}

func (e dtEnv) Page(context.Context) (*core.PageSession, error) { return e.mo.Page.PS, nil }
func (e dtEnv) Op() *core.Op                                    { return e.mo.Op }
func (dtEnv) Emit(string)                                       {}
func (e dtEnv) ResolvePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(e.h.dir, p)
}

func (e dtEnv) RunRemote(ctx context.Context, c *Command, in Input) (Result, error) {
	e.h.forwarded = append(e.h.forwarded, in)
	class := core.ClassRead
	if c.Mutates {
		class = core.ClassMutate
	}
	var res Result
	err := e.h.m.Do(core.WithLeaseHooks(ctx), e.h.req, core.AcquireOptions{Session: in.Global.Session}, c.Name, class, false,
		func(ctx context.Context, mo *core.ManagedOp) error {
			var err error
			res, err = c.Run(ctx, dtEnv{h: e.h, mo: mo}, in)
			return err
		})
	return res, err
}

func (h *dtHarness) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := RunCLIStarted(context.Background(), append([]string{"--timeout", "5s"}, args...), &out, &errb, nil,
		func(context.Context, *Invocation) (Env, error) { return dtEnv{h: h}, nil })

	return code, strings.TrimRight(out.String(), "\n"), errb.String()
}

func (h *dtHarness) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := h.run(args...)
	if code != 0 {
		t.Fatalf("%s: exit %d: %s", strings.Join(args, " "), code, errOut)
	}
	return out
}

func (h *dtHarness) params(method string) []map[string]any {
	var out []map[string]any
	for _, r := range h.f.ReceivedMethod(method) {
		var p map[string]any
		_ = json.Unmarshal(r.Params, &p)
		out = append(out, p)
	}
	return out
}

func lastBootstrap(h *dtHarness) string {
	boot := h.params("Page.setBootstrapScript")
	if len(boot) == 0 {
		return ""
	}
	s, _ := boot[len(boot)-1]["source"].(string)
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("%s: not within 3 s", what)
		}
	}
}

// Design 4.2 "Console and errors", 7.7 test 4: messages that arrive after
// the lease's first command and before "console" are printed; --level and
// --since filter; --clear empties the buffer; "errors" installs the hooks
// and prints a hook report with its stack.
func TestConsoleAndErrorsCommands(t *testing.T) {
	h := newDTHarness(t)
	h.mustRun(t, "set", "media", "light") // an unrelated first command starts the lease
	for i, m := range []struct{ level, text string }{{"log", "hello"}, {"error", "bad thing"}} {
		h.f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{"source": "console-api", "level": m.level, "text": m.text, "timestamp": float64(i)}})
	}
	var out string
	waitFor(t, "two messages", func() bool { out = h.mustRun(t, "console"); return strings.Contains(out, "bad thing") })
	if !strings.Contains(out, "#1 [log] hello") || !strings.Contains(out, "#2 [error] bad thing") {
		t.Fatalf("console = %q, want both messages with ids and levels", out)
	}
	if out := h.mustRun(t, "console", "--level", "error"); strings.Contains(out, "hello") || !strings.Contains(out, "bad thing") {
		t.Fatalf("console --level error = %q", out)
	}
	if out := h.mustRun(t, "console", "--since", "1"); strings.Contains(out, "hello") || !strings.Contains(out, "bad thing") {
		t.Fatalf("console --since 1 = %q", out)
	}
	h.mustRun(t, "console", "--clear")
	if out := h.mustRun(t, "console"); out != "No console messages." || len(h.f.ReceivedMethod("Console.clearMessages")) != 1 {
		t.Fatalf("console after --clear = %q, clearMessages %d", out, len(h.f.ReceivedMethod("Console.clearMessages")))
	}
	if code, _, errOut := h.run("console", "--level", "loud"); code != ExitUsage || !strings.Contains(errOut, "loud") {
		t.Fatalf("console --level loud: exit %d %q, want a usage error", code, errOut)
	}

	if out := h.mustRun(t, "errors"); out != "No errors." {
		t.Fatalf("errors = %q", out)
	}
	if !strings.Contains(lastBootstrap(h), "unhandledrejection") {
		t.Fatal("errors did not install the error hooks as a bootstrap part")
	}
	report, _ := json.Marshal(map[string]any{"kind": "unhandledrejection", "message": "boom", "stack": "f@https://fixture.example/a.js:1:2"})
	h.f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{"source": "console-api", "level": "debug", "text": "[agent-ios:error] " + string(report), "timestamp": 5.0}})
	waitFor(t, "the rejection", func() bool { out = h.mustRun(t, "errors"); return strings.Contains(out, "boom") })
	if !strings.Contains(out, "unhandledrejection: boom") || !strings.Contains(out, "    f@https://fixture.example/a.js:1:2") {
		t.Fatalf("errors = %q, want the rejection and its stack", out)
	}
}

// Device check of 2026-11-12: iOS 16.7 WebKit gives an error of code
// without a URL (here thrown in a timer that eval set) the url
// "undefined", and errors printed "(undefined:1:86)". Such a location
// prints as <anonymous>, in the error line, its stack and the console.
func TestErrorsNameAScriptWithoutURLAnonymous(t *testing.T) {
	h := newDTHarness(t)
	if out := h.mustRun(t, "errors"); out != "No errors." {
		t.Fatalf("errors = %q", out)
	}
	h.f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{
		"source": "javascript", "level": "error", "text": "Error: smoke-uncaught", "url": "undefined", "line": 1, "column": 86, "timestamp": 1.0,
		"stackTrace": map[string]any{"callFrames": []any{map[string]any{"functionName": "", "url": "undefined", "lineNumber": 1, "columnNumber": 86}}},
	}})
	var out string
	waitFor(t, "the error", func() bool { out = h.mustRun(t, "errors"); return strings.Contains(out, "smoke-uncaught") })
	if strings.Contains(out, "undefined:") || !strings.Contains(out, "console: Error: smoke-uncaught (<anonymous>:1:86)") || !strings.Contains(out, "    at (anonymous) (<anonymous>:1:86)") {
		t.Fatalf("errors = %q, want the location and the frame as <anonymous>:1:86", out)
	}
	if out := h.mustRun(t, "console"); strings.Contains(out, "undefined:") || !strings.Contains(out, "[error] Error: smoke-uncaught (<anonymous>:1)") {
		t.Fatalf("console = %q, want the location as <anonymous>:1", out)
	}
}

// Design 4.2 "network requests" and "network request", 7.7 test 1: the
// log lists requests seen since the lease with filters; one request shows
// its headers with secrets masked, --show-secrets keeps them, and --body
// returns the body or WebKit's error text.
func TestNetworkRequestCommands(t *testing.T) {
	h := newDTHarness(t)
	h.f.Handle("Network.getResponseBody", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if strings.Contains(string(c.Params), "\"r1\"") {
			return map[string]any{"body": "{\"name\":\"a\",\"token\":\"t0k\"}", "base64Encoded": false}, nil
		}
		return nil, &fakewebkit.Error{Code: -32000, Message: "Missing resource for given requestId"}
	})
	h.f.Handle("Page.getResourceContent", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "No resource with given URL found"}
	})
	h.mustRun(t, "set", "media", "light")
	for _, r := range []struct{ id, url, typ string }{{"r1", "https://fixture.example/api?token=s3cret", "Fetch"}, {"r2", "https://fixture.example/app.js", "Script"}} {
		h.f.Emit("Network.requestWillBeSent", map[string]any{
			"requestId": r.id, "frameId": "f1", "loaderId": "l1", "timestamp": 1.0, "walltime": 1.7e9, "type": r.typ,
			"request": map[string]any{"url": r.url, "method": "GET", "headers": map[string]string{"Authorization": "Bearer s3cret", "Accept": "*/*"}},
		})
		h.f.Emit("Network.responseReceived", map[string]any{
			"requestId": r.id, "frameId": "f1", "loaderId": "l1", "timestamp": 1.1, "type": r.typ,
			"response": map[string]any{"url": r.url, "status": 200, "statusText": "OK", "headers": map[string]string{"Content-Type": "application/json"}, "mimeType": "application/json", "source": "network"},
		})
		h.f.Emit("Network.loadingFinished", map[string]any{"requestId": r.id, "timestamp": 1.2})
	}
	var out string
	// Request ids name the page target (devtools.Request.ID).
	waitFor(t, "two requests", func() bool { out = h.mustRun(t, "network", "requests"); return strings.Contains(out, "page-1:r2 ") })
	if !strings.Contains(out, "page-1:r1 GET 200 Fetch https://fixture.example/api?token=") || strings.Contains(out, "s3cret") {
		t.Fatalf("network requests = %q, want page-1:r1 with its token masked", out)
	}
	if out := h.mustRun(t, "network", "requests", "--type", "script"); strings.Contains(out, "page-1:r1 ") || !strings.Contains(out, "page-1:r2 ") {
		t.Fatalf("--type script = %q", out)
	}
	out = h.mustRun(t, "network", "request", "page-1:r1", "--body")
	if strings.Contains(out, "s3cret") || strings.Contains(out, "t0k") || !strings.Contains(out, "Authorization: [masked]") || !strings.Contains(out, "\"name\":\"a\"") {
		t.Fatalf("network request page-1:r1 --body = %q, want masked secrets and the body", out)
	}
	if out := h.mustRun(t, "network", "request", "page-1:r1", "--body", "--show-secrets"); !strings.Contains(out, "Bearer s3cret") || !strings.Contains(out, "t0k") {
		t.Fatalf("--show-secrets = %q", out)
	}
	if code, _, errOut := h.run("network", "request", "page-1:r2", "--body"); code != ExitError || !strings.Contains(errOut, "Missing resource for given requestId") {
		t.Fatalf("network request page-1:r2 --body: exit %d %q, want WebKit's error text", code, errOut)
	}
	// WebKit's id alone does not name a request: two targets can use it.
	for _, id := range []string{"nope", "r1"} {
		if code, _, errOut := h.run("network", "request", id); code != ExitError || !strings.Contains(errOut, "not in the log") {
			t.Fatalf("unknown id %s: exit %d %q", id, code, errOut)
		}
	}
}

// Design 4.2 "network route" and "network unroute", 7.7 test 3 (fake
// part): the flags become the WIP completion; unroute continues a held
// request; the JS mode installs a bootstrap part.
// A request that WebKit reported only by its response (the document of a
// cross-process navigation on WebKitGTK 2.54, devtools.Netlog) is listed
// with "-" for its unknown method.
func TestNetworkRequestsListsAResponseOnlyRequest(t *testing.T) {
	h := newDTHarness(t)
	h.mustRun(t, "network", "requests")
	h.f.Emit("Network.responseReceived", map[string]any{
		"requestId": "0.9", "frameId": "f1", "loaderId": "l2", "timestamp": 2.0, "type": "Document",
		"response": map[string]any{"url": "https://other.example/doc.html", "status": 200, "statusText": "OK", "headers": map[string]string{}, "mimeType": "text/html"},
	})
	h.f.Emit("Network.loadingFinished", map[string]any{"requestId": "0.9", "timestamp": 2.1})
	waitFor(t, "the document", func() bool {
		return strings.Contains(h.mustRun(t, "network", "requests"), "0.9 - 200 Document https://other.example/doc.html")
	})
}

func TestNetworkRouteCommands(t *testing.T) {
	h := newDTHarness(t)
	if out := h.mustRun(t, "network", "route", "**/api/*", "--body", "hi", "--status", "201"); !strings.Contains(out, "fulfill 201 (2 bytes)") {
		t.Fatalf("route = %q", out)
	}
	h.mustRun(t, "network", "route", "/blocked", "--abort")
	h.mustRun(t, "network", "route", "/held", "--delay", "1h")
	if got := h.params("Network.addInterception"); len(got) != 3 || got[0]["url"] != "^.*/api/[^/]*$" || got[1]["url"] != "/blocked" {
		t.Fatalf("addInterception = %v", got)
	}
	intercept := func(id, url string) {
		h.f.Emit("Network.requestIntercepted", map[string]any{"requestId": id, "request": map[string]any{"url": url, "method": "GET", "headers": map[string]string{}}})
	}
	intercept("a", "https://x.example/api/items")
	intercept("b", "https://x.example/blocked/1")
	intercept("c", "https://x.example/held")
	waitFor(t, "fulfill and abort", func() bool {
		return len(h.f.ReceivedMethod("Network.interceptRequestWithResponse")) == 1 && len(h.f.ReceivedMethod("Network.interceptRequestWithError")) == 1
	})
	f := h.params("Network.interceptRequestWithResponse")[0]
	body, _ := base64.StdEncoding.DecodeString(f["content"].(string))
	if f["requestId"] != "a" || f["status"] != float64(201) || string(body) != "hi" {
		t.Fatalf("fulfill = %v", f)
	}
	if e := h.params("Network.interceptRequestWithError")[0]; e["requestId"] != "b" {
		t.Fatalf("abort = %v", e)
	}
	if out := h.mustRun(t, "network", "unroute", "/held"); out != "Removed 1 route(s)." {
		t.Fatalf("unroute = %q", out)
	}
	waitFor(t, "the held request continues", func() bool { return len(h.f.ReceivedMethod("Network.interceptContinue")) == 1 })
	h.mustRun(t, "network", "route", "/js", "--mode", "js", "--body", "j")
	if !strings.Contains(lastBootstrap(h), "__iwdpRoute") {
		t.Fatal("--mode js did not add the route patch to the bootstrap script")
	}
	for _, args := range [][]string{{"network", "route", "/x", "--abort", "--body", "y"}, {"network", "route", "/x", "--status", "42"}, {"network", "route", "/x", "--mode", "proxy"}} {
		if code, _, _ := h.run(args...); code != ExitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, ExitUsage)
		}
	}
}

// Design 4.2 "network har", 7.7 test 2 and 7.9: the HAR file has mode
// 0600, holds the requests after start and masks secrets by default.
func TestHARCommands(t *testing.T) {
	h := newDTHarness(t)
	h.mustRun(t, "network", "har", "start", "--content", "none")
	h.f.Emit("Network.requestWillBeSent", map[string]any{
		"requestId": "h1", "frameId": "f1", "loaderId": "l1", "timestamp": 1.0, "walltime": 1.7e9, "type": "Fetch",
		"request": map[string]any{"url": "https://fixture.example/h?api_key=k3y", "method": "GET", "headers": map[string]string{"Cookie": "sid=c00kie"}},
	})
	h.f.Emit("Network.loadingFailed", map[string]any{"requestId": "h1", "timestamp": 1.5, "errorText": "cancelled"})
	waitFor(t, "the request", func() bool { return strings.Contains(h.mustRun(t, "network", "requests"), "h1 ") })
	out := h.mustRun(t, "network", "har", "stop", "out.har")
	path := filepath.Join(h.dir, "out.har")
	if !strings.Contains(out, "Wrote 1 entries to "+path) {
		t.Fatalf("har stop = %q", out)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("HAR file: %v, %v; want mode 0600", err, st)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("k3y")) || bytes.Contains(data, []byte("c00kie")) || !bytes.Contains(data, []byte("\"version\": \"1.2\"")) {
		t.Fatalf("HAR = %s, want version 1.2 with secrets masked", data)
	}
	if code, _, errOut := h.run("network", "har", "stop"); code != ExitError || !strings.Contains(errOut, "network har start") {
		t.Fatalf("second stop: exit %d %q", code, errOut)
	}
}

// Design 4.2 "Emulation and settings" and 4.3: each set command sends its
// WIP mechanism; bad values are usage errors; viewport and device refuse
// with the reason.
func TestSetCommands(t *testing.T) {
	h := newDTHarness(t)
	h.mustRun(t, "set", "media", "dark", "--reduced-motion")
	prefs := map[string]string{}
	for _, p := range h.params("Page.overrideUserPreference") {
		prefs[p["name"].(string)], _ = p["value"].(string)
	}
	if prefs["PrefersColorScheme"] != "Dark" || prefs["PrefersReducedMotion"] != "Reduce" {
		t.Fatalf("overrideUserPreference = %v", prefs)
	}
	h.mustRun(t, "set", "useragent", "Agent/1")
	if p := h.params("Page.overrideUserAgent"); len(p) != 1 || p[0]["value"] != "Agent/1" {
		t.Fatalf("overrideUserAgent = %v", p)
	}
	h.mustRun(t, "set", "headers", "{\"X-A\":\"1\"}")
	if p := h.params("Network.setExtraHTTPHeaders"); len(p) != 1 || p[0]["headers"].(map[string]any)["X-A"] != "1" {
		t.Fatalf("setExtraHTTPHeaders = %v", p)
	}
	h.mustRun(t, "set", "geo", "52.5", "13.4")
	if !strings.Contains(lastBootstrap(h), "latitude: 52.5") {
		t.Fatal("set geo did not add the geolocation shim")
	}
	h.mustRun(t, "set", "offline", "on")
	if p := h.params("Network.addInterception"); len(p) != 1 || p[0]["url"] != ".*" {
		t.Fatalf("set offline on: addInterception = %v", p)
	}
	id := h.mustRun(t, "addinitscript", "window.__x = 1")
	if id != "init1" || !strings.Contains(lastBootstrap(h), "window.__x = 1") {
		t.Fatalf("addinitscript printed %q", id)
	}
	h.mustRun(t, "removeinitscript", "init1")
	if strings.Contains(lastBootstrap(h), "window.__x = 1") {
		t.Fatal("removeinitscript left the script in the bootstrap")
	}
	for _, args := range [][]string{{"set", "media", "blue"}, {"set", "headers", "[1]"}, {"set", "geo", "x", "1"}, {"set", "offline", "maybe"}, {"set", "useragent"}, {"addinitscript"}} {
		if code, _, _ := h.run(args...); code != ExitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, ExitUsage)
		}
	}
	for _, name := range []string{"viewport", "device"} {
		if code, _, errOut := h.run("set", name, "390", "844"); code != ExitError || !strings.Contains(errOut, "setScreenSizeOverride") {
			t.Errorf("set %s: exit %d %q, want the refusal with its reason", name, code, errOut)
		}
	}
}

// Design 4.2 "set credentials", 7.7 test 7 and W4-2a decision 4: on a
// command line the password comes from stdin, read by the client step in
// the agent-ios process; the input that goes to the daemon carries it (in
// the /v1/run body, never in argv). A transport without a verified origin
// scope refuses; the password is in no output and no error. A --password
// on a command line (where other local users can read argv) is refused
// before any connection, without echoing it.
func TestSetCredentialsReadsStdinAndNeverPrintsThePassword(t *testing.T) {
	const password = "pa55-w0rd-xyz"
	h := newDTHarness(t)
	stdinPipe(t, password+"\n", true)
	code, out, errOut := h.run("set", "credentials", "--origin", "https://fixture.example", "alice")
	if code != ExitError || !strings.Contains(errOut, "unsupported") || strings.Contains(out+errOut, password) {
		t.Fatalf("set credentials on a device: exit %d, stdout %q, stderr %q; want unsupported without the password", code, out, errOut)
	}
	if n := len(h.forwarded); n != 1 || h.forwarded[0].Flags["password"] != password || h.forwarded[0].Args[0] != "alice" {
		t.Fatalf("forwarded %d inputs, want one with the stdin password", n)
	}
	stdinPipe(t, "", true)
	if code, _, errOut := h.run("set", "credentials", "--origin", "https://fixture.example", "alice"); code != ExitUsage || !strings.Contains(errOut, "no password on stdin") {
		t.Fatalf("empty stdin: exit %d %q", code, errOut)
	}
	if code, _, _ := h.run("set", "credentials", "alice"); code != ExitUsage {
		t.Fatalf("no --origin: exit %d, want %d", code, ExitUsage)
	}
	if n := len(h.forwarded); n != 1 {
		t.Fatalf("usage errors reached the daemon (%d forwards)", n)
	}

	// stdin holds a password too, so only the refusal stops this line.
	stdinPipe(t, "from-stdin\n", true)
	envBuilt := false
	var out2, errb bytes.Buffer
	code = RunCLIStarted(context.Background(), []string{"set", "credentials", "--origin", "https://fixture.example", "--password", password, "alice"}, &out2, &errb, nil,
		func(context.Context, *Invocation) (Env, error) { envBuilt = true; return dtEnv{h: h}, nil })

	if code != ExitUsage || envBuilt || !strings.Contains(errb.String(), "stdin") || strings.Contains(out2.String()+errb.String(), password) {
		t.Fatalf("set credentials --password on a command line: exit %d, env built %v, stdout %q, stderr %q; want a usage error that points to stdin, without the password", code, envBuilt, out2.String(), errb.String())
	}

	// --clear reads no stdin (an empty, still open pipe would block) and
	// takes no user or --origin.
	stdinPipe(t, "", false)
	if code, _, errOut := h.run("set", "credentials", "--clear", "alice"); code != ExitUsage || !strings.Contains(errOut, "--clear takes no user") {
		t.Fatalf("set credentials --clear alice: exit %d %q; want a usage error", code, errOut)
	}
	h.run("set", "credentials", "--clear")
	if n := len(h.forwarded); n != 2 || h.forwarded[1].Flags["clear"] != true || h.forwarded[1].Flags["password"] != nil {
		t.Fatalf("set credentials --clear: %d forwards (%+v); want a second one with clear and no password", n, h.forwarded)
	}
}

// Design 4.2 "emulate network <preset>": a preset throttles with
// Network.setEmulatedConditions (slow-3g is Chrome DevTools' Slow 3G,
// 400 kbit/s = 50000 bytes/s); --bytes_per_second_limit sets a rate;
// "none" removes the throttle (WebKit: no bytesPerSecondLimit removes the
// limit); release removes it too.
func TestEmulateNetwork(t *testing.T) {
	h := newDTHarness(t)
	h.mustRun(t, "emulate", "network", "slow-3g")
	h.mustRun(t, "emulate", "network", "--bytes_per_second_limit", "1000")
	h.mustRun(t, "emulate", "network", "none")
	got := h.params("Network.setEmulatedConditions")
	if len(got) != 3 || got[0]["bytesPerSecondLimit"] != float64(50000) || got[1]["bytesPerSecondLimit"] != float64(1000) || len(got[2]) != 0 {
		t.Fatalf("setEmulatedConditions = %v; want 50000, 1000, then no limit", got)
	}
	for _, args := range [][]string{{"emulate", "network"}, {"emulate", "network", "4g", "--bytes_per_second_limit", "5"}, {"emulate", "network", "--bytes_per_second_limit", "-1"}, {"emulate", "network", "2g"}} {
		if code, _, _ := h.run(args...); code != ExitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, ExitUsage)
		}
	}
	h.mustRun(t, "emulate", "network", "4g")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.m.ReleaseSession(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	got = h.params("Network.setEmulatedConditions")
	if last := got[len(got)-1]; len(got) != 5 || len(last) != 0 {
		t.Fatalf("setEmulatedConditions after release = %v; want the limit removed", got)
	}
}

// Device check 3, P3-H: the runtime's own console.debug messages (the
// dialog override's "__iwdp_dialog__ {...}" records, 80-dialogs.js, and the
// "__iwdp_nav__ " and "__iwdp_bind__ " hooks of the CDP page script) are
// internal; "console" and "console --json" (the Data) leave
// them out, as the CDP endpoint does (internal/cdp consoleMessage). A user
// message that only contains the marker text stays.
func TestConsoleLeavesOutTheRuntimesMarkers(t *testing.T) {
	h := newDTHarness(t)
	h.mustRun(t, "set", "media", "light")
	for i, text := range []string{
		"before",
		`__iwdp_dialog__ {"id":"d1","type":"confirm","message":"Sure?","result":true}`,
		`__iwdp_nav__ {"u":"https://fixture.example/#a","t":"fragment"}`,
		`__iwdp_bind__ {"n":"f","k":"k1","p":"x"}`,
		"user text with __iwdp_dialog__ inside",
		"after",
	} {
		h.f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{"source": "console-api", "level": "debug", "text": text, "timestamp": float64(i)}})
	}
	var out string
	waitFor(t, "the last message", func() bool { out = h.mustRun(t, "console"); return strings.Contains(out, "after") })
	for _, want := range []string{"before", "user text with __iwdp_dialog__ inside", "after"} {
		if !strings.Contains(out, want) {
			t.Errorf("console = %q, want the page's message %q", out, want)
		}
	}
	for _, marker := range []string{"[debug] __iwdp_dialog__", "__iwdp_nav__", "__iwdp_bind__"} {
		if strings.Contains(out, marker) {
			t.Errorf("console = %q, want no runtime marker message %q", out, marker)
		}
	}
	_, js, _ := h.run("console", "--json")
	if strings.Contains(js, `"text":"__iwdp_`) || !strings.Contains(js, "user text with __iwdp_dialog__ inside") {
		t.Errorf("console --json = %s, want the page's messages without the runtime's markers", js)
	}
}

// The error hooks' report of an error without a URL (WebKit's "undefined")
// prints as <anonymous> too (review of 2026-11-12).
func TestHookErrorWithoutURLIsAnonymous(t *testing.T) {
	h := newDTHarness(t)
	h.mustRun(t, "errors")
	h.f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{
		"source": "console-api", "level": "debug", "text": `[agent-ios:error] {"kind":"error","message":"hook failure","url":"undefined","line":1,"column":9}`, "timestamp": 1.0,
	}})
	var out string
	waitFor(t, "the hook's error", func() bool { out = h.mustRun(t, "errors"); return strings.Contains(out, "hook failure") })
	if strings.Contains(out, "undefined:") || !strings.Contains(out, "error: hook failure (<anonymous>:1:9)") {
		t.Fatalf("errors = %q, want the location as <anonymous>:1:9", out)
	}
}
