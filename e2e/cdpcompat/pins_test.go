//go:build cdpcompat

package cdpcompat

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Pins (design 5.2 "Pins" and 8.4). package.json and package-lock.json pin
// the clients; TestNodeProjectPins checks that they agree with these.
const (
	playwrightPin   = "1.65.0"
	puppeteerPin    = "25.16.0"
	devtoolsMCPPin  = "1.12.0"
	agentBrowserPin = "0.41.2"
	// npmRegistry is where every locked package comes from.
	npmRegistry = "https://registry.npmjs.org/"
)

// agentBrowserRelease maps a platform to the file name of the agent-browser
// release binary in the npm package (node_modules/agent-browser/bin) and
// its SHA-256. The lockfile pins the package by its integrity hash; the
// checksum also pins the binary that runs. A build of another commit,
// also one that reports 0.41.2, does not match.
var agentBrowserRelease = map[string]struct{ file, sha256 string }{
	"linux/amd64": {"agent-browser-linux-x64", "c4351f3802ffa5254d8c69f20a52e2fcf56925c6016fcf4203db25a07d811379"},
}

// pinnedAgentBrowser returns the pinned agent-browser binary: the release
// binary of the agent-browser@0.41.2 package that npm ci installed, with
// the recorded SHA-256, executable, and reporting version 0.41.2.
func pinnedAgentBrowser(t *testing.T) agentBrowserBuild {
	t.Helper()
	rel, ok := agentBrowserRelease[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		t.Fatalf("no agent-browser %s binary is recorded for %s/%s; add its file name and SHA-256 to agentBrowserRelease (e2e/cdpcompat/pins_test.go)", agentBrowserPin, runtime.GOOS, runtime.GOARCH)
	}
	bin := filepath.Join(nodeDir, "node_modules", "agent-browser", "bin", rel.file)
	b, err := describeBuild("pinned", bin, bin)
	if err != nil {
		t.Fatalf("agent-browser %s: %v; run 'make test-cdpcompat', which runs npm ci --ignore-scripts and makes the binaries executable", agentBrowserPin, err)
	}
	if b.SHA256 != rel.sha256 {
		t.Fatalf("%s has SHA-256 %s, want %s (the agent-browser %s release binary); run 'npm ci --ignore-scripts' in %s", bin, b.SHA256, rel.sha256, agentBrowserPin, nodeDir)
	}
	if b.Version != "agent-browser "+agentBrowserPin {
		t.Fatalf("%s --version printed %q, want agent-browser %s", bin, b.Version, agentBrowserPin)
	}
	return b
}

// requireNode checks node and the installed pinned packages, and that
// chrome-devtools-mcp bundles the pinned puppeteer-core.
func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is not on PATH (%v); install Node.js 24 or later, then run 'make test-cdpcompat'", err)
	}
	for pkg, want := range nodePins {
		var meta struct{ Version string }
		data, err := os.ReadFile(filepath.Join(nodeDir, "node_modules", pkg, "package.json"))
		if err == nil {
			err = json.Unmarshal(data, &meta)
		}
		if err != nil || meta.Version != want {
			t.Fatalf("%s %s is not installed in %s (found %q, %v); run 'make test-cdpcompat', which runs npm ci --ignore-scripts there", pkg, want, nodeDir, meta.Version, err)
		}
	}
	bundled := map[string]string{}
	data, err := os.ReadFile(filepath.Join(nodeDir, "node_modules", "chrome-devtools-mcp", "build", "src", "third_party", "bundled-packages.json"))
	if err == nil {
		err = json.Unmarshal(data, &bundled)
	}
	if err != nil || bundled["puppeteer-core"] != puppeteerPin {
		t.Fatalf("chrome-devtools-mcp %s bundles puppeteer-core %q (%v), want %s (design 5.2)", devtoolsMCPPin, bundled["puppeteer-core"], err, puppeteerPin)
	}
}

// nodePins are the exact dependencies of package.json.
var nodePins = map[string]string{"agent-browser": agentBrowserPin, "chrome-devtools-mcp": devtoolsMCPPin, "playwright-core": playwrightPin, "puppeteer-core": puppeteerPin}

// package.json pins exactly the four clients, and the lockfile resolves
// them, and every other package, from registry.npmjs.org.
func TestNodeProjectPins(t *testing.T) {
	var pkg struct{ Dependencies map[string]string }
	data, err := os.ReadFile("package.json")
	if err == nil {
		err = json.Unmarshal(data, &pkg)
	}
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(pkg.Dependencies) != fmt.Sprint(nodePins) {
		t.Fatalf("package.json dependencies %v, want exactly %v", pkg.Dependencies, nodePins)
	}
	var lock struct {
		Packages map[string]struct {
			Version   string
			Resolved  string
			Integrity string
		}
	}
	data, err = os.ReadFile("package-lock.json")
	if err == nil {
		err = json.Unmarshal(data, &lock)
	}
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range nodePins {
		if got := lock.Packages["node_modules/"+name].Version; got != v {
			t.Errorf("package-lock.json locks %s %q, want %s", name, got, v)
		}
	}
	for path, p := range lock.Packages {
		if path == "" {
			continue // the project itself
		}
		if !strings.HasPrefix(p.Resolved, npmRegistry) || p.Integrity == "" {
			t.Errorf("package-lock.json: %s resolves to %q (integrity %q), want %s with an integrity hash", path, p.Resolved, p.Integrity, npmRegistry)
		}
	}
}

// Node client scripts (clients/*.mjs) by client name.
var nodeScripts = map[string]string{
	"chrome-devtools-mcp": "clients/chrome-devtools-mcp.mjs",
	"playwright-core":     "clients/playwright.mjs",
	"puppeteer-core":      "clients/puppeteer.mjs",
}

// nodeEnv is the extra environment of a client script. Playwright's method
// record reads its pw:protocol debug lines from stderr, so the driver sets
// the debug selection itself (clientEnv removed every inherited DEBUG and
// DEBUG_* variable, DEBUG_FILE included).
var nodeEnv = map[string][]string{
	"playwright-core": {"DEBUG=pw:protocol", "DEBUG_COLORS=no"},
	// chrome-devtools-mcp sends no usage statistics, makes no update check
	// and reads no configuration file (PLUGIN_DATA, ~/.config/cd4a).
	"chrome-devtools-mcp": {"CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1", "CHROME_DEVTOOLS_MCP_NO_UPDATE_CHECKS=1", "CHROME_DEVTOOLS_MCP_NO_CONFIG_DISCOVERY=1"},
}

// clientRun is one client run: its result file (Node clients) and the
// unknown methods that the endpoint counted during it (cdp log).
type clientRun struct {
	result  clientResult
	unknown map[string]int
}

// runNode runs the script of client for scenario with url and the
// scenario arguments args, and fails the test unless the run succeeded
// (judgeNodeRun).
func (e *endpoint) runNode(t *testing.T, logName, client, scenario, url string, args ...string) clientRun {
	t.Helper()
	return e.runNodeWith(t, logName, client, scenario, url, nodeOptions{args: args})
}

// nodeOptions are the optional parts of a script run.
type nodeOptions struct {
	// preload, when not empty, is a module that node loads with --import
	// before the script: the driver's only way to inject a fault, because
	// clientEnv drops NODE_OPTIONS.
	preload string
	// args are the scenario arguments after the result file.
	args []string
	// checkpoint answers the script's checkpoints (clients/result.mjs
	// checkpoint) in the test goroutine; the script continues when it
	// returns nil and fails at that checkpoint otherwise. nil: the script
	// gets no stdin.
	checkpoint func(name string) error
}

// runNodeWith is runNode with the options o.
func (e *endpoint) runNodeWith(t *testing.T, logName, client, scenario, url string, o nodeOptions) clientRun {
	t.Helper()
	before := unknownCounts(t)
	code, data, err := e.execNode(t, logName, client, scenario, url, o)
	if err == nil {
		var r clientResult
		if r, err = judgeNodeRun(code, data, client, scenario, url); err == nil {
			return clientRun{result: r, unknown: newUnknown(before, unknownCounts(t))}
		}
	}
	t.Fatalf("%s: %s; log %s", logName, e.redact(err.Error()), filepath.Join(logDir, logName+".log"))
	return clientRun{}
}

// execNode runs the script with the options o and returns its exit code
// and result file.
func (e *endpoint) execNode(t *testing.T, logName, client, scenario, url string, o nodeOptions) (int, []byte, error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "result.json")
	var args []string
	if o.preload != "" {
		args = append(args, "--import="+o.preload)
	}
	args = append(append(args, nodeScripts[client], scenario, url, file), o.args...)
	var code int
	if o.checkpoint == nil {
		code, _ = e.runLogged(t, logName+".log", nodeDir, clientTimeout, nodeEnv[client], "node", args...)
	} else {
		code = e.runInteractive(t, logName+".log", client, o.checkpoint, args)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return code, nil, fmt.Errorf("%s exited %d and wrote no result file: %w", client, code, err)
	}
	if err := os.WriteFile(filepath.Join(logDir, logName+".json"), []byte(e.redact(string(data))), 0o644); err != nil {
		t.Errorf("keeping the result: %v", err)
	}
	return code, data, nil
}

// checkpointMark starts a checkpoint line of a script's stdout.
const checkpointMark = "@checkpoint "

// runInteractive runs node with args like runLogged and answers each
// checkpoint line of its stdout with checkpoint's verdict on its stdin
// ("ok", or "fail" after the test recorded the error). The checkpoints run
// in the test goroutine. When the test ends early (a fatal error in
// checkpoint), the cleanup closes the script's stdin, so its checkpoint
// fails and the script ends, and waits for the run.
func (e *endpoint) runInteractive(t *testing.T, logName, client string, checkpoint func(string) error, args []string) int {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	names := make(chan string)
	stop, done := make(chan struct{}), make(chan struct{})
	var code int
	go func() {
		defer close(done)
		defer func() { _ = r.Close() }()
		code, _ = e.runLoggedIO(t, logName, nodeDir, clientTimeout, nodeEnv[client], clientIO{stdin: r, line: func(l string) {
			if name, ok := strings.CutPrefix(l, checkpointMark); ok {
				select {
				case names <- name:
				case <-stop:
				}
			}
		}}, "node", args...)
	}()
	ended := false
	t.Cleanup(func() {
		if !ended {
			close(stop)
			_ = w.Close()
			<-done
		}
	})
	for {
		select {
		case name := <-names:
			answer := "ok"
			if err := checkpoint(name); err != nil {
				t.Errorf("%s checkpoint %s: %v", client, name, err)
				answer = "fail"
			}
			if _, err := fmt.Fprintln(w, answer); err != nil {
				t.Errorf("answering %s checkpoint %s: %v", client, name, err)
			}
		case <-done:
			ended = true
			_ = w.Close()
			return code
		}
	}
}

// judgeNodeRun checks one script run against the result format (README.md,
// "Result format"): the script exited 0, the result parses, names the
// pinned client and version, the scenario and the URL exactly as handed
// over, and reports ok:true without a script-level error. A failed step
// with ok:true is a scenario's expected or tolerated outcome (the resolve
// scenario records a connect that fails after the requests the driver
// judges), not a script failure.
func judgeNodeRun(code int, data []byte, client, scenario, url string) (clientResult, error) {
	r, err := parseResult(data)
	if err != nil {
		return r, fmt.Errorf("%s (exit %d): %w", client, code, err)
	}
	pin := nodePins[client]
	switch {
	case r.Client != client || r.Version != pin || r.Scenario != scenario:
		return r, fmt.Errorf("result names client %s %s, scenario %s; want %s %s, %s", r.Client, r.Version, r.Scenario, client, pin, scenario)
	case r.URL != url:
		return r, fmt.Errorf("%s received the URL %q, want the printed URL %q unchanged", client, r.URL, url)
	case code != 0 || !r.OK || r.Error != "":
		return r, fmt.Errorf("%s %s failed: exit %d, ok %v, error %q", client, scenario, code, r.OK, r.Error)
	}
	return r, nil
}

// agentBrowserRuns numbers the agent-browser sessions of this process.
var agentBrowserRuns atomic.Int32

// agentBrowserClose bounds "agent-browser close".
const agentBrowserClose = 30 * time.Second

// runAgentBrowser runs "agent-browser --config <driver config> --session
// <s> --cdp <url> <args>" with the driver's home (clientEnv) as working
// directory, a session name unique to this run, and closes the session at
// cleanup. The explicit --config replaces agent-browser's config discovery
// (~/.agent-browser/config.json, ./agent-browser.json). It returns the exit
// code, the output and the unknown methods.
func (e *endpoint) runAgentBrowser(t *testing.T, bin, logName, url string, args ...string) (int, string, map[string]int) {
	t.Helper()
	ab := e.agentBrowserSession(t, bin, logName, url)
	before := unknownCounts(t)
	code, out := ab.run(t, logName, args...)
	return code, out, newUnknown(before, unknownCounts(t))
}

// agentBrowser is one agent-browser session of a run: the commands of a
// scenario share it, so the refs of its snapshot stay valid.
type agentBrowser struct {
	e    *endpoint
	bin  string
	url  string
	base []string // --config and --session
}

// agentBrowserSession starts an agent-browser session with a name unique
// to this run, and closes it at cleanup (log logName-close.log).
func (e *endpoint) agentBrowserSession(t *testing.T, bin, logName, url string) *agentBrowser {
	t.Helper()
	// agent-browser refuses a session whose socket path is longer than
	// 103 bytes, so the name is short and the sockets live in
	// agentBrowserSockets.
	session := "cdpc-" + strconv.Itoa(os.Getpid()) + "-" + strconv.Itoa(int(agentBrowserRuns.Add(1)))
	ab := &agentBrowser{e: e, bin: bin, url: url, base: []string{"--config", agentBrowserConfig, "--session", session}}
	t.Cleanup(func() {
		if code, out := e.runLogged(t, logName+"-close.log", clientHome, agentBrowserClose, nil, bin, append(ab.base, "close")...); code != 0 {
			t.Errorf("agent-browser --session %s close: exit %d: %s", session, code, out)
		}
	})
	return ab
}

// run runs "agent-browser --config <driver config> --session <s> --cdp
// <url> <args>" (log logName.log) and returns its exit code and output.
func (ab *agentBrowser) run(t *testing.T, logName string, args ...string) (int, string) {
	t.Helper()
	return ab.e.runLogged(t, logName+".log", clientHome, clientTimeout, nil, ab.bin, append(append(append([]string(nil), ab.base...), "--cdp", ab.url), args...)...)
}

// agentBrowserBuild records an agent-browser build that a run used.
type agentBrowserBuild struct {
	Source string `json:"source"`          // pinned or PATH
	Entry  string `json:"entry,omitempty"` // what PATH named (PATH runs)
	// Script is the script that PATH named, and Mapping the value of
	// pathMappingVar that names the binary it runs (PATH runs of a script).
	Script  string `json:"script,omitempty"`
	Mapping string `json:"mapping,omitempty"`
	// Binary is the native file that ran, with symlinks resolved.
	Binary  string `json:"binary,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	ModTime string `json:"mtime,omitempty"`
	Version string `json:"version,omitempty"`
	// Resolution tells how Binary was chosen.
	Resolution string `json:"resolution,omitempty"`
	// Checkout is the git checkout that holds the binary, when there is one:
	// its HEAD and git describe. A build can be older than its checkout's
	// HEAD; ModTime and SHA256 identify the build itself.
	CheckoutDir      string `json:"checkoutDir,omitempty"`
	CheckoutHead     string `json:"checkoutHead,omitempty"`
	CheckoutDescribe string `json:"checkoutDescribe,omitempty"`
	// Note says why a PATH build was not run (missing, identical).
	Note string `json:"note,omitempty"`
}

// describeBuild records the binary bin (found through entry): it must be
// an executable ELF file. Its version comes from "--config <driver
// config> --version" in the client environment. Every command runs under
// metadataTimeout (runBounded).
func describeBuild(source, entry, bin string) (agentBrowserBuild, error) {
	b := agentBrowserBuild{Source: source, Entry: entry, Binary: bin}
	fi, err := os.Stat(bin)
	if err != nil {
		return b, err
	}
	if fi.Mode()&0o111 == 0 {
		return b, fmt.Errorf("%s is not executable", bin)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		return b, err
	}
	if !bytes.HasPrefix(data, []byte("\x7fELF")) {
		return b, fmt.Errorf("%s is not an ELF binary", bin)
	}
	sum := sha256.Sum256(data)
	b.SHA256 = hex.EncodeToString(sum[:])
	b.ModTime = fi.ModTime().UTC().Format(time.RFC3339)
	out, err := runBounded(metadataTimeout, clientHome, clientEnv(nil), bin, "--config", agentBrowserConfig, "--version")
	if err != nil {
		return b, err
	}
	b.Version = strings.TrimSpace(string(out))
	// The source checkout of a PATH build (the pinned binary comes from
	// npm). GIT_OPTIONAL_LOCKS=0 keeps git describe --dirty from
	// refreshing the checkout's index: the description only reads.
	gitEnv := append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	for dir := filepath.Dir(bin); source == "PATH" && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
			continue
		}
		b.CheckoutDir = dir
		head, err := runBounded(metadataTimeout, dir, gitEnv, "git", "rev-parse", "HEAD")
		if err != nil {
			return b, fmt.Errorf("the source checkout of %s: %w", bin, err)
		}
		b.CheckoutHead = strings.TrimSpace(string(head))
		d, err := runBounded(metadataTimeout, dir, gitEnv, "git", "describe", "--tags", "--always", "--dirty")
		if err != nil {
			return b, fmt.Errorf("the source checkout of %s: %w", bin, err)
		}
		b.CheckoutDescribe = strings.TrimSpace(string(d))
		break
	}
	return b, nil
}

// pathAgentBrowser is resolvePathBuild; an error fails the test.
func pathAgentBrowser(t *testing.T, pinned agentBrowserBuild) (agentBrowserBuild, bool) {
	t.Helper()
	b, run, err := resolvePathBuild(pinned)
	if err != nil {
		t.Fatal(err)
	}
	return b, run
}

func isELF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	magic := make([]byte, 4)
	_, err = io.ReadFull(f, magic)
	return err == nil && string(magic) == "\x7fELF"
}

// writeBuilds keeps the build records of a run in agent-browser-builds.json.
func writeBuilds(t *testing.T, builds []agentBrowserBuild) {
	t.Helper()
	data, err := json.MarshalIndent(builds, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "agent-browser-builds.json"), append(data, '\n'), 0o644); err != nil {
		t.Errorf("keeping the build records: %v", err)
	}
	t.Logf("agent-browser builds: %s", data)
}

// readAll returns the body of r, or the read error as text.
func readAll(r io.Reader) string {
	b, err := io.ReadAll(r)
	if err != nil {
		return fmt.Sprintf("<read error: %v>", err)
	}
	return string(b)
}
