//go:build cdpcompat

// Package cdpcompat is the CDP compatibility suite (design
// docs/design/agentic-parity.md, sections 7.8 and 8.4): a Go driver that
// runs the pinned CDP clients (playwright-core, puppeteer-core,
// chrome-devtools-mcp from the Node project in this directory, and
// agent-browser from PATH) against the daemon's CDP endpoint. Run it with
// "make test-cdpcompat"; README.md describes the pins, the result format
// and the expected-missing lists.
//
// The driver runs the daemon in this test process (daemon.Run with
// RunOptions.CDPObserve), so it records every HTTP request and WebSocket
// upgrade that the endpoint sees. The real iwdp-cli binary, built from
// this checkout, talks to that daemon through the shared socket directory
// and prints the URLs; each client gets the printed URL unchanged.
package cdpcompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/iwdp"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// gateEnv is the design's gate of the suite (8.4). The build tag cdpcompat
// keeps the suite out of "go test ./..."; the gate makes a run with the
// tag an explicit choice ("make test-cdpcompat" sets it).
const gateEnv = "IWDP_CDP_COMPAT"

// proxyListingVar is the link-time seam of internal/proxy (W1c T01): the
// iwdp listing address of a binary.
const proxyListingVar = "github.com/nnemirovsky/iwdp-mcp/internal/proxy.listingAddr"

// Time limits of the driver.
const (
	// cliTimeout bounds one iwdp-cli run.
	cliTimeout = 30 * time.Second
	// buildTimeout bounds the build of iwdp-cli in TestMain.
	buildTimeout = 5 * time.Minute
	// daemonStart bounds the start of the in-process daemon.
	daemonStart = 10 * time.Second
	// daemonStop bounds its shutdown (daemon.ShutdownBudget plus margin).
	daemonStop = daemon.ShutdownBudget + 3*time.Second
)

// clientTimeout bounds one client run; each Node script stops itself after
// 60 s (clients/result.mjs scriptLimit). A variable: a test shortens it.
var clientTimeout = 90 * time.Second

// clientOutputLimit bounds the output of one client or iwdp-cli run that
// the driver keeps; the log says how much was dropped.
const clientOutputLimit = 8 << 20

// Paths set up by TestMain.
var (
	// nodeDir is this directory: the Node project of the client scripts.
	nodeDir string
	// cliBin is the iwdp-cli binary of this checkout, linked to a dead
	// iwdp listing.
	cliBin string
	// logDir keeps the logs of a run: CDPCOMPAT_LOG_DIR, else a temp dir.
	logDir string
	// fakeBin holds the fake iwdp executables, first on PATH.
	fakeBin string
	// clientHome is the HOME, working directory and XDG base of every
	// client process: the driver owns it, so no client reads or changes
	// the user's configuration, sessions or state.
	clientHome string
	// agentBrowserConfig is the driver's agent-browser configuration
	// ("{}"), passed with --config, which replaces agent-browser's config
	// discovery.
	agentBrowserConfig string
	// agentBrowserSockets is agent-browser's socket directory
	// (AGENT_BROWSER_SOCKET_DIR): a short path below /tmp, because a
	// socket path may have at most 103 bytes.
	agentBrowserSockets string
)

// TestMain checks the gate and isolates every process of the suite from
// the user's daemon, iwdp and devices (the e2e harness guard of W1c): a
// private XDG_RUNTIME_DIR and XDG_STATE_HOME, fake ios_webkit_debug_proxy
// and pkill first on PATH, a dead iwdp listing for the in-process daemon
// (iwdp.DefaultListingAddr) and for the iwdp-cli binary (link-time seam).
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if mode, ok := os.LookupEnv(fakeAgentBrowserVar); ok {
		return fakeAgentBrowser(mode) // this binary runs as a test's fake agent-browser
	}
	if os.Getenv(gateEnv) != "1" {
		fmt.Fprintf(os.Stderr, "the CDP compatibility suite runs only with %s=1; run it with 'make test-cdpcompat' (docs: e2e/cdpcompat/README.md)\n", gateEnv)
		return 2
	}
	var err error
	if nodeDir, err = os.Getwd(); err != nil {
		fmt.Fprintf(os.Stderr, "cdpcompat: working directory: %v\n", err)
		return 2
	}
	tmp, err := os.MkdirTemp("", "cdpc-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cdpcompat: temp dir: %v\n", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if agentBrowserSockets, err = os.MkdirTemp("/tmp", "cdpab-"); err != nil {
		fmt.Fprintf(os.Stderr, "cdpcompat: agent-browser socket dir: %v\n", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(agentBrowserSockets) }()
	if err := isolate(tmp); err != nil {
		fmt.Fprintf(os.Stderr, "cdpcompat: %v\n", err)
		return 2
	}
	return m.Run()
}

// isolate sets up the private environment below tmp and builds iwdp-cli.
func isolate(tmp string) error {
	run := filepath.Join(tmp, "run")
	if err := os.Mkdir(run, 0o700); err != nil {
		return err
	}
	for k, v := range map[string]string{
		"XDG_RUNTIME_DIR": run,
		"XDG_STATE_HOME":  filepath.Join(tmp, "state"),
	} {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	for _, k := range []string{"IWDP_SESSION", "IWDP_DEVICE_RESERVATION", "IWDP_TRANSPORT", "IWDP_DAEMON_DIR", "IWDP_NO_DAEMON"} {
		if err := os.Unsetenv(k); err != nil {
			return err
		}
	}
	clientHome = filepath.Join(tmp, "home")
	agentBrowserConfig = filepath.Join(tmp, "agent-browser-config.json")
	for _, d := range []string{".agent-browser", ".config", ".cache", ".local/share", ".local/state"} {
		if err := os.MkdirAll(filepath.Join(clientHome, d), 0o700); err != nil {
			return err
		}
	}
	for _, f := range []string{agentBrowserConfig, filepath.Join(clientHome, ".agent-browser", "config.json")} {
		if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
			return err
		}
	}
	fakeBin = filepath.Join(tmp, "bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		return err
	}
	if err := testutil.InstallFakeIWDP(fakeBin); err != nil {
		return err
	}
	dead := testutil.DeadIWDPListing()
	iwdp.DefaultListingAddr = dead
	logDir = os.Getenv("CDPCOMPAT_LOG_DIR")
	if logDir == "" {
		logDir = filepath.Join(tmp, "logs")
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	cliBin = filepath.Join(tmp, "iwdp-cli")
	if _, err := runBounded(buildTimeout, "", nil, "go", "build", "-ldflags", "-X "+proxyListingVar+"="+dead, "-o", cliBin, "../../cmd/iwdp-cli"); err != nil {
		return fmt.Errorf("building iwdp-cli: %w", err)
	}
	return nil
}

// request is one HTTP request that the CDP endpoint saw
// (RunOptions.CDPObserve): its raw request target, exactly as the client
// sent it, its Host header and the status it got (101 for an upgrade).
type request struct {
	Method  string `json:"method"`
	URI     string `json:"uri"`
	Host    string `json:"host"`
	Upgrade bool   `json:"upgrade"`
	Status  int    `json:"status"`
}

func (r request) String() string {
	return fmt.Sprintf("%s %s (Host %s, upgrade %v) -> %d", r.Method, r.URI, r.Host, r.Upgrade, r.Status)
}

// recorder keeps the requests in the order they ended.
type recorder struct {
	mu   sync.Mutex
	reqs []request
}

func (rc *recorder) observe(r *http.Request, status int) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.reqs = append(rc.reqs, request{
		Method: r.Method, URI: r.RequestURI, Host: r.Host,
		Upgrade: strings.EqualFold(r.Header.Get("Upgrade"), "websocket"), Status: status,
	})
}

// mark returns the number of requests so far; since(mark) returns the
// later ones.
func (rc *recorder) mark() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.reqs)
}

func (rc *recorder) since(n int) []request {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]request(nil), rc.reqs[n:]...)
}

// endpoint is the in-process daemon of one test with its CDP endpoint.
type endpoint struct {
	rec  *recorder
	port int

	mu      sync.Mutex
	secrets []string // tokens and endpoint ids, replaced in logs (redact)
}

// startDaemon runs the daemon in this process with the CDP endpoint on a
// free port and request recording, and stops it at cleanup. The iwdp-cli
// runs of the test reach it through the shared socket directory.
func startDaemon(t *testing.T) *endpoint {
	t.Helper()
	e := &endpoint{rec: &recorder{}, port: freePort(t)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	logf, err := os.Create(filepath.Join(logDir, t.Name()+"-daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	log := &redactingWriter{w: logf, e: e}
	go func() {
		done <- daemon.Run(ctx, daemon.RunOptions{Version: "cdpcompat", CDPPort: e.port, CDPObserve: e.rec.observe, Log: log})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("in-process daemon ended with an error: %v", err)
			}
		case <-time.After(daemonStop):
			t.Errorf("in-process daemon did not stop within %v", daemonStop)
		}
		_ = logf.Close()
	})
	for deadline := time.Now().Add(daemonStart); ; time.Sleep(20 * time.Millisecond) {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Second)
		c, err := daemon.Connect(cctx, daemon.Options{NoStart: true, Version: "cdpcompat"})
		ccancel()
		if err == nil {
			c.Close()
			return e
		}
		select {
		case err := <-done:
			t.Fatalf("in-process daemon ended at start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("in-process daemon did not answer within %v: %v", daemonStart, err)
		}
	}
}

// addSecrets registers the token and endpoint id of an issued URL for
// redaction.
func (e *endpoint) addSecrets(s ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.secrets = append(e.secrets, s...)
}

// redact replaces the registered secrets in s with <secretN>.
func (e *endpoint) redact(s string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, sec := range e.secrets {
		s = strings.ReplaceAll(s, sec, "<secret"+strconv.Itoa(i)+">")
	}
	return s
}

// redactingWriter writes daemon log lines with the secrets redacted. The
// daemon redacts its own lines; this is a second guard for the files that
// a run keeps.
type redactingWriter struct {
	w io.Writer
	e *endpoint
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(w.w, w.e.redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// cli runs the real iwdp-cli (runOwned, within cliTimeout) and fails the
// test unless it exits 0 with all of its output kept. It returns stdout.
func cli(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(cliBin, args...)
	out, errb := &cappedBuffer{limit: clientOutputLimit}, &cappedBuffer{limit: clientOutputLimit}
	cmd.Stdout, cmd.Stderr = out, errb
	timedOut, err := runOwned(cmd, cliTimeout, false)
	if timedOut || err != nil || out.dropped > 0 {
		t.Fatalf("iwdp-cli %s: %v (timed out after %v: %v; stdout bytes dropped: %d); stdout %q, stderr %q", strings.Join(args, " "), err, cliTimeout, timedOut, out.dropped, out.String(), errb.String())
	}
	return out.String()
}

// cdpURLs are the outputs of "get cdp-url": WS is the printed line without
// its newline, HTTP the http member of "get cdp-url --json" (design 5.1).
type cdpURLs struct {
	WS   string
	HTTP string
	// Prefix is the path prefix of the record, /t/<token>/e/<endpoint>.
	Prefix string
	// Host is host:port of both forms.
	Host string
}

// getCDPURL runs "iwdp-cli --session <session> get cdp-url" and the same
// with --json for page, and checks that both name the same record.
func (e *endpoint) getCDPURL(t *testing.T, session, page string) cdpURLs {
	t.Helper()
	printed := cli(t, "--session", session, "get", "cdp-url", "--page", page)
	ws, ok := strings.CutSuffix(printed, "\n")
	if !ok || strings.Contains(ws, "\n") {
		t.Fatalf("get cdp-url printed %q, want one line", printed)
	}
	var j struct{ WS, HTTP string }
	if err := json.Unmarshal([]byte(cli(t, "--session", session, "get", "cdp-url", "--page", page, "--json")), &j); err != nil {
		t.Fatalf("get cdp-url --json: %v", err)
	}
	host := "127.0.0.1:" + strconv.Itoa(e.port)
	rest, ok := strings.CutPrefix(j.HTTP, "http://"+host+"/t/")
	token, id, _ := strings.Cut(rest, "/e/")
	if !ok || len(token) != 64 || id == "" || strings.Contains(id, "/") {
		t.Fatalf("get cdp-url --json http form %q: want http://%s/t/<64 hex>/e/<endpoint>", j.HTTP, host)
	}
	e.addSecrets(token, id)
	u := cdpURLs{WS: ws, HTTP: j.HTTP, Prefix: "/t/" + token + "/e/" + id, Host: host}
	if j.WS != ws || ws != "ws://"+host+u.Prefix+"/devtools/browser/"+id {
		t.Fatalf("get cdp-url printed %q and --json ws %q; want the same ws form of the record %s", e.redact(ws), e.redact(j.WS), e.redact(j.HTTP))
	}
	return u
}

// unknownCounts runs "iwdp-cli cdp log --json": the endpoint's counts of
// unknown methods (design 5.4).
func unknownCounts(t *testing.T) map[string]int {
	t.Helper()
	var counts []struct {
		Method string `json:"method"`
		Count  int    `json:"count"`
	}
	if err := json.Unmarshal([]byte(cli(t, "cdp", "log", "--json")), &counts); err != nil {
		t.Fatalf("cdp log --json: %v", err)
	}
	out := map[string]int{}
	for _, c := range counts {
		out[c.Method] = c.Count
	}
	return out
}

// newUnknown returns the counts in after that grew since before.
func newUnknown(before, after map[string]int) map[string]int {
	out := map[string]int{}
	for m, n := range after {
		if d := n - before[m]; d > 0 {
			out[m] = d
		}
	}
	return out
}

// userSetting reports whether the environment variable k is a user
// setting that a client must not inherit:
//   - proxies (nothing may sit between a client and the endpoint);
//   - agent-browser settings;
//   - the debug selection that Playwright's method record depends on (the
//     debug module reads DEBUG and every DEBUG_* variable, DEBUG_FILE
//     redirects Playwright's log away from stderr);
//   - what changes how Node or npm load code: every NODE_* variable (Node
//     reads NODE_OPTIONS, whose --import and --require load modules first,
//     NODE_PATH, NODE_PRESERVE_SYMLINKS, NODE_COMPILE_CACHE, NODE_ICU_DATA
//     and more; checked in the Node 26.12.1 binary), the OpenSSL
//     configuration and module paths from which Node's crypto loads
//     providers and engines, and npm's npm_config_* settings (for example
//     node-options), in case a client starts npm or npx. A test injects a
//     fault only through execNode's explicit preload.
//
// clientEnv also replaces the home and XDG directories.
func userSetting(k string) bool {
	u := strings.ToUpper(k)
	switch {
	case u == "HTTP_PROXY", u == "HTTPS_PROXY", u == "ALL_PROXY", u == "NO_PROXY",
		strings.HasPrefix(u, "AGENT_BROWSER_"), u == "DEBUG", strings.HasPrefix(u, "DEBUG_"),
		strings.HasPrefix(u, "NODE_"), strings.HasPrefix(u, "NPM_CONFIG_"),
		u == "OPENSSL_CONF", u == "OPENSSL_CONF_INCLUDE", u == "OPENSSL_MODULES", u == "OPENSSL_ENGINES":
		return true
	}
	return false
}

// clientEnv is the environment of a client process: the test's own (the
// private runtime directory and the guard on PATH) without user settings
// (userSetting), with the driver's home (clientHome) as HOME and XDG base,
// agent-browser's sockets in agentBrowserSockets, and extra appended.
func clientEnv(extra []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME":
			continue
		}
		if !userSetting(k) {
			env = append(env, kv)
		}
	}
	env = append(env, "HOME="+clientHome,
		"XDG_CONFIG_HOME="+filepath.Join(clientHome, ".config"), "XDG_CACHE_HOME="+filepath.Join(clientHome, ".cache"),
		"XDG_DATA_HOME="+filepath.Join(clientHome, ".local", "share"), "XDG_STATE_HOME="+filepath.Join(clientHome, ".local", "state"),
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost", "AGENT_BROWSER_SOCKET_DIR="+agentBrowserSockets)
	return append(env, extra...)
}

// runLogged is runClient; a run that outlives timeout, a start or wait
// failure (for example a process that kept the output open after the
// client exited) and a failed log write fail the test. Any of them leaves
// the caller's cleanups, agent-browser's session close among them, to run.
func (e *endpoint) runLogged(t *testing.T, logName, dir string, timeout time.Duration, extraEnv []string, name string, args ...string) (int, string) {
	t.Helper()
	code, out, timedOut, err := e.runClient(logName, dir, timeout, extraEnv, name, args...)
	if timedOut {
		t.Errorf("%s did not end within %v; its process group was killed; log %s", name, timeout, filepath.Join(logDir, logName))
	}
	if err != nil {
		t.Errorf("%s: %s; log %s", name, e.redact(err.Error()), filepath.Join(logDir, logName))
	}
	return code, out
}

// runClient runs a client command in dir within timeout through runOwned
// (without endGroup: agent-browser's session outlives the command) and
// writes its redacted command line, exit status and combined output to
// logName in logDir. It returns the exit code (-1 when a signal ended it),
// the redacted output (at most clientOutputLimit bytes, then a note of
// what was dropped), whether timeout ended it, and an error for anything
// but an exit status: a start or wait failure, or a failed log write.
// extraEnv is added to the client environment.
func (e *endpoint) runClient(logName, dir string, timeout time.Duration, extraEnv []string, name string, args ...string) (int, string, bool, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = clientEnv(extraEnv)
	buf := &cappedBuffer{limit: clientOutputLimit}
	cmd.Stdout, cmd.Stderr = buf, buf
	timedOut, werr := runOwned(cmd, timeout, false)
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	out := e.redact(buf.String())
	if buf.dropped > 0 {
		out += fmt.Sprintf("\n[the driver dropped %d more bytes of output]\n", buf.dropped)
	}
	text := e.redact(fmt.Sprintf("$ %s %s\nexit: %d (%v, timed out after %v: %v)\n", name, strings.Join(args, " "), code, werr, timeout, timedOut)) + out
	var err error
	if exit := (*exec.ExitError)(nil); werr != nil && !errors.As(werr, &exit) {
		err = werr
	}
	if lerr := os.WriteFile(filepath.Join(logDir, logName), []byte(text), 0o644); lerr != nil {
		err = errors.Join(err, fmt.Errorf("writing the log %s: %w", logName, lerr))
	}
	return code, out, timedOut, err
}
