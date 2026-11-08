//go:build webkitgtk

package webkitgtk

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/webkitgtk"
)

// W4 DevTools suite (design docs/design/agentic-parity.md, section 7.7,
// tests 1, 3, 4, 6, 7 and 8). Every test drives its own MiniBrowser
// through the daemon with the iwdp-cli binary, so the DevTools state lives
// where it lives in production, and checks the final state on the wire:
// the local HTTP servers record every request they get.
//
// Two origins: site A is http://127.0.0.1:<port> and site B is
// http://localhost:<port>. They are different sites, so a navigation from
// A to B swaps the web process (proof P1, docs/notes/w1a.md; two ports of
// one host are the same site and do not swap).

// seen is one request that a site received.
type seen struct {
	path   string // path and query
	header http.Header
}

// site is a local HTTP server that records every request. Handlers are
// keyed by path; an unknown path answers "server:<path>".
type site struct {
	url string // origin, for example http://localhost:40123

	mu       sync.Mutex
	reqs     []seen
	handlers map[string]http.HandlerFunc
}

func newSite(t *testing.T, host string) *site {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	s := &site{url: "http://" + host + ":" + port, handlers: map[string]http.HandlerFunc{}}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, seen{path: r.URL.RequestURI(), header: r.Header.Clone()})
		h := s.handlers[r.URL.Path]
		s.mu.Unlock()
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-store")
		if h != nil {
			h(w, r)
			return
		}
		_, _ = io.WriteString(w, "server:"+r.URL.Path)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return s
}

// html serves a page at path.
func (s *site) html(path, body string) {
	s.handle(path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body)
	})
}

func (s *site) handle(path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[path] = h
}

// got returns the requests whose path and query start with prefix.
func (s *site) got(prefix string) []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []seen
	for _, r := range s.reqs {
		if strings.HasPrefix(r.path, prefix) {
			out = append(out, r)
		}
	}
	return out
}

// waitGot waits until the site got a request whose path starts with prefix.
func (s *site) waitGot(t *testing.T, prefix string) seen {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if g := s.got(prefix); len(g) > 0 {
			return g[len(g)-1]
		}
	}
	t.Fatalf("%s got no request for %s within 10 s", s.url, prefix)
	return seen{}
}

// cutProxy forwards TCP connections to the inspector of MiniBrowser. cut
// closes every open connection, so the daemon loses its page connection
// as on a dropped link, while the browser and its page stay.
type cutProxy struct {
	l      net.Listener
	target string

	mu    sync.Mutex
	conns []net.Conn
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{l: l, target: target}
	// The copy goroutines and cut close the same connections, and cut
	// closes them on purpose under live copies, so the errors of these
	// closes (already closed, broken pipe) carry no test signal.
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, u)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(u, c); _ = u.Close() }()
			go func() { _, _ = io.Copy(c, u); _ = c.Close() }()
		}
	}()
	t.Cleanup(func() { _ = l.Close(); p.cut() })
	return p
}

func (p *cutProxy) addr() string { return p.l.Addr().String() }

func (p *cutProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// devtoolsBrowser starts broadwayd and MiniBrowser on url, waits until
// the inspector lists the page with a title, and starts the suite's daemon
// on the WebKitGTK transport through a cutProxy. The browser and the
// daemon end at test cleanup.
func devtoolsBrowser(t *testing.T, url string) *cutProxy {
	t.Helper()
	if _, err := os.Stat(miniBrowser); err != nil {
		t.Fatalf("WebKitGTK MiniBrowser not found at %s: %v", miniBrowser, err)
	}
	display := fmt.Sprintf(":%d", 200+time.Now().Nanosecond()%50)
	// broadwayd's HTTP port defaults to 8080 plus the display number, which
	// another program can hold (8317, display 237, on the W4 test host);
	// broadwayd then exits and MiniBrowser never lists the page. A free
	// port avoids that.
	_, port, err := net.SplitHostPort(freeAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	startProcess(t, nil, "broadwayd", "--port", port, display)
	time.Sleep(500 * time.Millisecond)
	addr := freeAddr(t)
	// A private XDG_CONFIG_HOME: the user's GTK settings (a dark theme
	// preference) would change prefers-color-scheme.
	startProcess(t, []string{"GDK_BACKEND=broadway", "BROADWAY_DISPLAY=" + display, "WEBKIT_INSPECTOR_HTTP_SERVER=" + addr, "XDG_CONFIG_HOME=" + t.TempDir()}, miniBrowser, url)
	tr := webkitgtk.New(addr)
	listed := false
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end) && !listed; time.Sleep(200 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		pages, err := tr.Pages(ctx, "webkitgtk-"+addr)
		cancel()
		listed = err == nil && len(pages) > 0 && pages[0].Title != ""
	}
	if !listed {
		t.Fatalf("MiniBrowser did not list the page %s at %s within 15 s", url, addr)
	}
	p := newCutProxy(t, addr)
	if r := cli(t, "daemon", "start", "--transport", "webkitgtk", "--addr", p.addr()); r.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", r.code, r.stderr)
	}
	t.Cleanup(func() {
		if r := cli(t, "daemon", "stop"); r.code != 0 {
			t.Errorf("daemon stop: exit %d, stderr %q", r.code, r.stderr)
		}
	})
	return p
}

// mustCLI runs iwdp-cli and fails the test unless it exits 0.
func mustCLI(t *testing.T, args ...string) string {
	t.Helper()
	r := cli(t, args...)
	if r.code != 0 {
		t.Fatalf("iwdp-cli %s: exit %d, stdout %q, stderr %q", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

// cliStdin runs iwdp-cli with stdin.
func cliStdin(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(cliBin, args...)
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &out, &errb
	err := cmd.Run()
	r := result{stdout: out.String(), stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		r.code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running iwdp-cli %v: %v", args, err)
	}
	return r
}

// js evaluates expr in the page (awaiting a promise) and returns the
// printed value: a string as is.
func js(t *testing.T, expr string) string {
	t.Helper()
	return strings.TrimSpace(mustCLI(t, "eval", "--await", expr))
}

// goTo navigates the page to url and waits until the new document is
// complete and ready (JavaScript) is true. Evaluations fail while the
// process swaps; the loop ignores them.
func goTo(t *testing.T, url, ready string) {
	t.Helper()
	_ = cli(t, "eval", fmt.Sprintf("location.href = %q", url))
	cond := fmt.Sprintf("String(location.href === %q && document.readyState === 'complete' && !!(%s))", url, ready)
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if r := cli(t, "eval", cond); r.code == 0 && strings.TrimSpace(r.stdout) == "true" {
			return
		}
	}
	t.Fatalf("the page did not load %s (ready: %s) within 15 s", url, ready)
}

// requestID returns the id of the last line of "network requests" output
// whose URL is url.
func requestID(t *testing.T, out, url string) string {
	t.Helper()
	id := ""
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[len(f)-1] == url {
			id = f[0]
		}
	}
	if id == "" {
		t.Fatalf("network requests lists no request for %s:\n%s", url, out)
	}
	return id
}

// 7.7 test 1: the netlog keeps the requests of the old process across a
// cross-process swap, captures the new document and its fetch (Network
// is enabled on the provisional target), and "network request <id>
// --body" returns the body of a request seen while the capture ran.
func TestNetlogAcrossACrossProcessSwap(t *testing.T) {
	a, b := newSite(t, "127.0.0.1"), newSite(t, "localhost")
	a.html("/", "<!doctype html><title>A</title><p>site A")
	b.html("/b.html", `<!doctype html><title>B</title><script>fetch("/data.json").then(r => r.text()).then(t => { window.__data = t })</script>`)
	b.handle("/data.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"site":"B","n":42}`)
	})
	devtoolsBrowser(t, a.url+"/")
	mustCLI(t, "network", "requests") // the first registry command starts the lease and its capture
	if got := js(t, `fetch("/a.txt").then(r => r.text())`); got != "server:/a.txt" {
		t.Fatalf("fetch /a.txt on A: %q", got)
	}
	goTo(t, b.url+"/b.html", "window.__data")

	out := mustCLI(t, "network", "requests")
	requestID(t, out, a.url+"/a.txt")
	requestID(t, out, b.url+"/b.html")
	id := requestID(t, out, b.url+"/data.json")
	body := mustCLI(t, "network", "request", id, "--body")
	// The body is printed through the secret mask, which re-encodes JSON.
	if !strings.Contains(body, `"site":"B"`) || !strings.Contains(body, `"n":42`) {
		t.Fatalf("network request %s --body after the swap:\n%s\nwant the body served by B", id, body)
	}
}

// 7.7 test 3, both modes: abort, fulfill with body and status, delay, and
// unroute continues a held request; the server never sees an aborted or
// fulfilled request, and sees every request after "network unroute".
func TestRoutesOnTheWire(t *testing.T) {
	a := newSite(t, "127.0.0.1")
	a.html("/", "<!doctype html><title>A</title><p>site A")
	devtoolsBrowser(t, a.url+"/")
	for _, mode := range []string{"wip", "js"} {
		t.Run(mode, func(t *testing.T) {
			p := "/" + mode + "/"
			mustCLI(t, "network", "route", "**"+p+"abort*", "--abort", "--mode", mode)
			mustCLI(t, "network", "route", "**"+p+"fulfill*", "--body", "fulfilled", "--status", "201", "--mode", mode)
			mustCLI(t, "network", "route", "**"+p+"delay*", "--delay", "800ms", "--mode", mode)

			if got := js(t, fmt.Sprintf(`fetch(%q).then(() => "resolved", () => "rejected")`, p+"abort")); got != "rejected" {
				t.Fatalf("fetch of an aborted route: %q, want rejected", got)
			}
			if got := js(t, fmt.Sprintf(`fetch(%q).then(async r => r.status + " " + await r.text())`, p+"fulfill")); got != "201 fulfilled" {
				t.Fatalf("fetch of a fulfilled route: %q, want 201 fulfilled", got)
			}
			got := js(t, fmt.Sprintf(`(async () => { const t0 = performance.now(); const r = await fetch(%q); return (performance.now() - t0 >= 750) + " " + await r.text() })()`, p+"delay"))
			if got != "true server:"+p+"delay" {
				t.Fatalf("fetch of a delayed route: %q, want at least 750 ms and the server's answer", got)
			}
			if n := len(a.got(p + "abort")); n != 0 {
				t.Fatalf("the server got %d aborted request(s)", n)
			}
			if n := len(a.got(p + "fulfill")); n != 0 {
				t.Fatalf("the server got %d fulfilled request(s)", n)
			}

			// A request held by a long delay continues when its route goes.
			mustCLI(t, "network", "route", "**"+p+"hold*", "--delay", "60s", "--mode", mode)
			js(t, fmt.Sprintf(`window.__held = fetch(%q).then(r => r.text()); "started"`, p+"hold"))
			time.Sleep(500 * time.Millisecond)
			if n := len(a.got(p + "hold")); n != 0 {
				t.Fatalf("the held request reached the server before unroute")
			}
			mustCLI(t, "network", "unroute", "**"+p+"hold*")
			if got := js(t, `window.__held`); got != "server:"+p+"hold" {
				t.Fatalf("the held request after unroute: %q", got)
			}
			mustCLI(t, "network", "unroute")
			if got := js(t, fmt.Sprintf(`fetch(%q).then(r => r.text())`, p+"fulfill")); got != "server:"+p+"fulfill" {
				t.Fatalf("fetch after unroute: %q, want the server's answer", got)
			}
		})
	}
}

// 7.7 test 3, early interception: a route set before a cross-process
// navigation answers a fetch made by the first inline script of the new
// document (the rules reach the provisional target before it commits).
func TestRouteInterceptsTheFirstScriptAfterACrossProcessSwap(t *testing.T) {
	a, b := newSite(t, "127.0.0.1"), newSite(t, "localhost")
	a.html("/", "<!doctype html><title>A</title><p>site A")
	b.html("/early.html", `<!doctype html><title>B</title><script>fetch("/early").then(r => r.text()).then(t => { window.__early = t }, e => { window.__early = "error: " + e })</script>`)
	devtoolsBrowser(t, a.url+"/")
	mustCLI(t, "network", "route", "**/early", "--body", "intercepted")
	goTo(t, b.url+"/early.html", "window.__early")
	if got := js(t, "window.__early"); got != "intercepted" {
		t.Fatalf("the first inline script's fetch got %q, want the route's body", got)
	}
	if n := len(b.got("/early")); n != 1 { // only /early.html
		t.Fatalf("site B got %d requests for /early*, want only the document: %v", n, b.got("/early"))
	}
}

// 7.7 test 4: a message logged before the capture comes from the replay
// of Console.enable; after a reconnect WebKit replays it again, and the
// buffer still holds it once.
func TestConsoleReplayIsStoredOnce(t *testing.T) {
	a := newSite(t, "127.0.0.1")
	a.html("/", `<!doctype html><title>A</title><script>console.log("replay-marker-4")</script>`)
	p := devtoolsBrowser(t, a.url+"/")
	count := func(out string) int { return strings.Count(out, "replay-marker-4") }
	if out := mustCLI(t, "console"); count(out) != 1 {
		t.Fatalf("console before the reconnect:\n%s\nwant the replayed message once", out)
	}
	reconnect(t, p)
	if out := mustCLI(t, "console"); count(out) != 1 {
		t.Fatalf("console after the reconnect:\n%s\nwant the replayed message once", out)
	}
}

// reconnect cuts the daemon's page connection and takes the page over
// again: the daemon dials a new connection and re-applies the desired
// state (design 2.4, 2.6).
func reconnect(t *testing.T, p *cutProxy) {
	t.Helper()
	p.cut()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if r := cli(t, "get", "title"); r.code == 4 {
			break // the daemon saw the loss
		}
	}
	if r := cli(t, "--takeover", "get", "title"); r.code != 0 {
		t.Fatalf("takeover after the cut: exit %d, stderr %q", r.code, r.stderr)
	}
}

// 7.7 test 6: "set media dark" changes matchMedia on WebKitGTK, and the
// override is applied again on the new connection after a reconnect.
func TestSetMediaDarkSurvivesAReconnect(t *testing.T) {
	a := newSite(t, "127.0.0.1")
	a.html("/", "<!doctype html><title>A</title><p>site A")
	p := devtoolsBrowser(t, a.url+"/")
	const dark = `String(matchMedia("(prefers-color-scheme: dark)").matches)`
	if got := js(t, dark); got != "false" {
		t.Fatalf("prefers-color-scheme: dark before set media: %s", got)
	}
	mustCLI(t, "set", "media", "dark")
	if got := js(t, dark); got != "true" {
		t.Fatalf("prefers-color-scheme: dark after set media dark: %s", got)
	}
	reconnect(t, p)
	if got := js(t, dark); got != "true" {
		t.Fatalf("prefers-color-scheme: dark after a reconnect: %s, want the override applied again", got)
	}
}

// 7.7 test 7: credentials for origin A reach requests to A only. A
// subresource on B and a redirect from A to B carry no Authorization, and
// the password appears in no output, error or daemon file.
func TestCredentialsStayOnTheirOrigin(t *testing.T) {
	a, b := newSite(t, "127.0.0.1"), newSite(t, "localhost")
	a.html("/", "<!doctype html><title>A</title><p>site A")
	a.handle("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.url+"/target", http.StatusFound)
	})
	devtoolsBrowser(t, a.url+"/")
	const user, password = "agent", "pw-7f3c-Secret"
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
	r := cliStdin(t, password+"\n", "set", "credentials", "--origin", a.url, user)
	if r.code != 0 {
		t.Fatalf("set credentials: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	outputs := []string{r.stdout, r.stderr}

	js(t, `fetch("/own").then(r => r.text())`)
	if got := a.waitGot(t, "/own").header.Get("Authorization"); got != want {
		t.Fatalf("request to origin A: Authorization %q, want the credentials", got)
	}
	// A subresource of the page on origin B.
	js(t, fmt.Sprintf(`new Promise(ok => { const i = new Image(); i.onload = i.onerror = () => ok("done"); i.src = %q })`, b.url+"/img.png"))
	b.waitGot(t, "/img.png")
	// A request to A that redirects to B.
	js(t, `fetch("/redirect", {mode: "no-cors"}).then(() => "done", () => "done")`)
	if got := a.waitGot(t, "/redirect").header.Get("Authorization"); got != want {
		t.Fatalf("redirecting request to origin A: Authorization %q, want the credentials", got)
	}
	b.waitGot(t, "/target")
	for _, s := range b.got("/") {
		if v, ok := s.header["Authorization"]; ok {
			t.Fatalf("origin B got Authorization %q on %s", v, s.path)
		}
	}

	// The password is in no output, error text or daemon file.
	bad := cliStdin(t, password+"\n", "set", "credentials", "--origin", "not an origin", user)
	if bad.code == 0 {
		t.Fatalf("set credentials with a bad origin: exit 0")
	}
	outputs = append(outputs, bad.stdout, bad.stderr)
	for _, args := range [][]string{{"daemon", "status"}, {"session", "info"}, {"daemon", "logs"}, {"network", "requests"}} {
		r := cli(t, args...)
		outputs = append(outputs, r.stdout, r.stderr)
	}
	id := requestID(t, mustCLI(t, "network", "requests"), a.url+"/own")
	r = cli(t, "network", "request", id)
	outputs = append(outputs, r.stdout, r.stderr)
	for _, o := range outputs {
		if strings.Contains(o, password) || strings.Contains(o, want) {
			t.Fatalf("the password appears in command output: %q", o)
		}
	}
	err := filepath.WalkDir(runtimeDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() || path == cliBin {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(password)) || bytes.Contains(data, []byte(want)) {
			t.Errorf("the password appears in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// 7.7 test 8: with a netlog capture and a route active, extra headers set
// and then removed ('{}') are absent from the next request on the wire;
// the route still applies; the netlog shows the gap and closes the request
// in flight as capture_reset; an earlier body is reported as gone. The
// header owner is the registry command or the legacy set_extra_headers
// tool (through the daemon's legacy bridge).
func TestHeaderRemovalOnTheWire(t *testing.T) {
	for _, owner := range []string{"set headers", "legacy set_extra_headers"} {
		t.Run(strings.ReplaceAll(owner, " ", "_"), func(t *testing.T) {
			a := newSite(t, "127.0.0.1")
			a.html("/", "<!doctype html><title>A</title><p>site A")
			release := make(chan struct{})
			a.handle("/slow", func(w http.ResponseWriter, _ *http.Request) {
				select {
				case <-release:
				case <-time.After(20 * time.Second):
				}
				_, _ = io.WriteString(w, "slow")
			})
			devtoolsBrowser(t, a.url+"/")
			setHeaders := func(h map[string]string, cmdJSON string) {
				t.Helper()
				if owner == "set headers" {
					mustCLI(t, "set", "headers", cmdJSON)
					return
				}
				legacySetHeaders(t, h)
			}
			mustCLI(t, "network", "requests")
			mustCLI(t, "network", "route", "**/routed*", "--body", "routed")
			setHeaders(map[string]string{"X-A": "1"}, `{"X-A":"1"}`)
			js(t, `fetch("/echo?1").then(r => r.text())`)
			if got := a.waitGot(t, "/echo?1").header.Get("X-A"); got != "1" {
				t.Fatalf("X-A on the wire after set: %q, want 1", got)
			}
			js(t, `window.__slow = fetch("/slow").then(r => r.text(), e => "error: " + e); "started"`)
			a.waitGot(t, "/slow")

			setHeaders(map[string]string{}, `{}`)
			close(release)
			js(t, `fetch("/echo?2").then(r => r.text())`)
			if v, ok := a.waitGot(t, "/echo?2").header["X-A"]; ok {
				t.Fatalf("X-A on the wire after removal: %q, want no X-A field", v)
			}
			if got := js(t, `fetch("/routed").then(r => r.text())`); got != "routed" {
				t.Fatalf("the route after the reset: %q, want routed", got)
			}
			if n := len(a.got("/routed")); n != 0 {
				t.Fatalf("the server got the routed request")
			}
			out := mustCLI(t, "network", "requests")
			if !strings.Contains(out, "-- network reset") {
				t.Fatalf("network requests shows no gap marker:\n%s", out)
			}
			slowID := requestID(t, out, a.url+"/slow")
			if !strings.Contains(out, slowID+" GET 200 capture_reset") {
				t.Fatalf("the request in flight across the reset (%s) is not closed as capture_reset:\n%s", slowID, out)
			}
			earlier := requestID(t, out, a.url+"/echo?1")
			r := cli(t, "network", "request", earlier, "--body")
			if r.code == 0 || !strings.Contains(r.stderr, "no longer available") {
				t.Fatalf("network request %s --body after the reset: exit %d, stdout %q, stderr %q; want the body reported as gone", earlier, r.code, r.stdout, r.stderr)
			}
		})
	}
}

// legacySetHeaders runs the legacy set_extra_headers tool through the
// daemon's legacy bridge for the default session (design 2.10).
func legacySetHeaders(t *testing.T, h map[string]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := daemon.Connect(ctx, daemon.Options{NoStart: true})
	if err != nil {
		t.Fatalf("connecting to the daemon: %v", err)
	}
	pc, err := c.OpenPage(ctx, daemon.PageOptions{Owner: daemon.Owner{Session: "default"}, Name: "set_extra_headers"})
	if err != nil {
		t.Fatalf("opening the page through the legacy bridge: %v", err)
	}
	defer func() {
		if err := pc.Close(); err != nil {
			t.Errorf("closing the legacy bridge page: %v", err)
		}
	}()
	if err := tools.SetExtraHeaders(ctx, pc, h); err != nil {
		t.Fatalf("legacy set_extra_headers %v: %v", h, err)
	}
}

// W4 review finding (overrides.go geolocation): the release of the session
// gives the current document its own navigator.geolocation methods back,
// so the next lease does not inherit a fake position.
func TestGeolocationIsRestoredOnRelease(t *testing.T) {
	a := newSite(t, "127.0.0.1")
	a.html("/", "<!doctype html><title>A</title>")
	devtoolsBrowser(t, a.url+"/")
	const same = `String(["getCurrentPosition", "watchPosition", "clearWatch"].every(n => navigator.geolocation[n] === window.__origGeo[n]))`
	js(t, `window.__origGeo = {getCurrentPosition: navigator.geolocation.getCurrentPosition, watchPosition: navigator.geolocation.watchPosition, clearWatch: navigator.geolocation.clearWatch}; "saved"`)
	mustCLI(t, "set", "geo", "12", "34")
	mustCLI(t, "set", "geo", "56", "78") // a second shim keeps the originals of the first
	if got := js(t, `new Promise(ok => navigator.geolocation.getCurrentPosition(p => ok(p.coords.latitude + "," + p.coords.longitude)))`); got != "56,78" {
		t.Fatalf("position with the shim: %q, want 56,78", got)
	}
	mustCLI(t, "session", "release")
	if got := js(t, same); got != "true" {
		t.Fatalf("navigator.geolocation methods after the release are the page's own: %s, want true", got)
	}
}

// W4 review finding (jsroutes.go): a reused XMLHttpRequest shows the
// server's answer for its next, unrouted request, not the earlier route
// answer.
func TestJSRouteXHRReuse(t *testing.T) {
	a := newSite(t, "127.0.0.1")
	a.html("/", "<!doctype html><title>A</title>")
	devtoolsBrowser(t, a.url+"/")
	mustCLI(t, "network", "route", "**/fulfilled", "--body", "route-body", "--status", "201", "--mode", "js")
	got := js(t, `new Promise(resolve => {
  const x = new XMLHttpRequest();
  x.open("GET", "/fulfilled");
  x.onload = () => {
    const first = x.status + " " + x.responseText;
    x.open("GET", "/plain");
    x.onload = () => resolve(first + "|" + x.status + " " + x.responseText + " " + x.getResponseHeader("Content-Type"));
    x.send();
  };
  x.send();
})`)
	if got != "201 route-body|200 server:/plain text/plain; charset=utf-8" {
		t.Fatalf("reused XHR after a JS route: %q, want the route answer, then the server's", got)
	}
}
