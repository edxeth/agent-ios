package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	reg "github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// runMainEnv makes the test binary run main() instead of the tests, so CLI
// tests can check exit codes, output and files of a real process.
const runMainEnv = "IWDP_CLI_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
		os.Exit(0)
	}
	code := m.Run()
	stopTestDaemons() // daemon_harness_test.go
	os.Exit(code)
}

type cliResult struct {
	stdout, stderr string
	code           int
	elapsed        time.Duration
}

// runCLI runs iwdp-cli with args in dir (a fresh temp dir when empty). env
// adds variables; IWDP_* variables of the test environment are dropped.
func runCLI(t *testing.T, dir string, env []string, args ...string) cliResult {
	t.Helper()
	cmd := cliCommand(t, dir, env, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	res := cliResult{stdout: stdout.String(), stderr: stderr.String(), elapsed: time.Since(start)}
	if exitErr, ok := err.(*exec.ExitError); ok {
		res.code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running iwdp-cli %v: %v", args, err)
	}
	return res
}

// cliCommand builds the iwdp-cli process that runCLI and startCLI run.
func cliCommand(t *testing.T, dir string, env []string, args ...string) *exec.Cmd {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "IWDP_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, runMainEnv+"=1")
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") }) {
		// The test's own daemon, never the suite's (testRuntime).
		cmd.Env = append(cmd.Env, "XDG_RUNTIME_DIR="+testRuntime(t))
	}
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

// syncBuffer is a bytes.Buffer that a running process writes while the test
// reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runningCLI is an iwdp-cli process started by startCLI.
type runningCLI struct {
	cmd            *exec.Cmd
	stdout, stderr *syncBuffer
	done           chan struct{}
	code           int
}

// startCLI starts iwdp-cli in the background; wait collects its exit code.
func startCLI(t *testing.T, env []string, args ...string) *runningCLI {
	t.Helper()
	r := &runningCLI{cmd: cliCommand(t, "", env, args...), stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan struct{})}
	r.cmd.Stdout, r.cmd.Stderr = r.stdout, r.stderr
	if err := r.cmd.Start(); err != nil {
		t.Fatalf("starting iwdp-cli %v: %v", args, err)
	}
	go func() {
		err := r.cmd.Wait()
		if exitErr, ok := err.(*exec.ExitError); ok {
			r.code = exitErr.ExitCode()
		} else if err != nil {
			r.code = -1
		}
		close(r.done)
	}()
	t.Cleanup(func() {
		select {
		case <-r.done:
		default:
			_ = r.cmd.Process.Kill()
			<-r.done
		}
	})
	return r
}

// wait returns the exit code, or fails the test when the process is still
// running after limit.
func (r *runningCLI) wait(t *testing.T, limit time.Duration) int {
	t.Helper()
	select {
	case <-r.done:
		return r.code
	case <-time.After(limit):
		t.Fatalf("iwdp-cli still running after %v; stderr %q", limit, r.stderr.String())
		return 0
	}
}

// eventually fails the test when cond is still false after limit.
func eventually(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(limit); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out after %v waiting for %s", limit, what)
		}
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// --- Help (criterion 3) ---

// TestEveryCommandHasHelp pins that the overview lists every registry
// command with its summary and that "help <command>" prints the command's
// own usage: iwdp-cli runs only registry commands, so the registry is the
// only source of commands and help.
func TestEveryCommandHasHelp(t *testing.T) {
	var overview bytes.Buffer
	printUsage(&overview)
	all := reg.All()
	if len(all) == 0 {
		t.Fatal("the registry has no commands")
	}
	for _, c := range all {
		if !strings.Contains(overview.String(), "  "+c.Name+" ") || !strings.Contains(overview.String(), c.Summary) {
			t.Errorf("the overview does not list %q with its summary", c.Name)
		}
		var help bytes.Buffer
		if !printCommandHelp(&help, c.Name) || !strings.HasPrefix(help.String(), "Usage: iwdp-cli "+c.Name) {
			t.Errorf("help %s = %q, want its usage", c.Name, help.String())
		}
	}
}

// TestHelpNeverConnectsOrWrites runs "<command> --help" and "-h" for every
// command with a reachable page configured: each must print its usage, exit
// 0, open no connection and write no file.
func TestHelpNeverConnectsOrWrites(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	tmp := t.TempDir()
	cwd := t.TempDir()
	env := []string{"IWDP_WS_URL=" + mock.URL, "TMPDIR=" + tmp}

	for _, c := range reg.All() {
		name := c.Name
		for _, flag := range []string{"--help", "-h"} {
			// Put the flag after a plausible argument, as agents do.
			res := runCLI(t, cwd, env, append(strings.Fields(name), "x", flag, "--page", mock.URL)...)
			if res.code != 0 {
				t.Errorf("%s %s: exit %d, stderr %q", name, flag, res.code, res.stderr)
			}
			if !strings.Contains(res.stdout, "Usage: iwdp-cli "+name) {
				t.Errorf("%s %s: stdout lacks its usage: %q", name, flag, res.stdout)
			}
		}
	}
	if n := mock.ConnectionCount(); n != 0 {
		t.Errorf("help opened %d page connections, want 0", n)
	}
	if files := append(dirEntries(t, cwd), dirEntries(t, tmp)...); len(files) != 0 {
		t.Errorf("help wrote files: %v", files)
	}
}

func TestHelpCommandForms(t *testing.T) {
	res := runCLI(t, "", nil, "help", "screenshot")
	if res.code != 0 || !strings.Contains(res.stdout, "--full-page") {
		t.Errorf("help screenshot: exit %d, stdout %q", res.code, res.stdout)
	}
	res = runCLI(t, "", nil, "--help")
	if res.code != 0 || !strings.Contains(res.stdout, "Usage:") {
		t.Errorf("--help: exit %d, stdout %q", res.code, res.stdout)
	}
	res = runCLI(t, "", nil, "help", "no-such-command")
	if res.code == 0 {
		t.Error("help for an unknown command must fail")
	}
}

// The help of restart-iwdp states which iwdp it may stop: only one that
// iwdp-cli started and recorded (internal/proxy/restart.go; review WS1 S2
// F1 and F2).
func TestRestartIWDPHelpStatesTheOwnershipRule(t *testing.T) {
	res := runCLI(t, "", nil, "restart-iwdp", "--help")
	if res.code != 0 {
		t.Fatalf("restart-iwdp --help: exit %d, stderr %q", res.code, res.stderr)
	}
	help := strings.Join(strings.Fields(res.stdout), " ")
	for _, want := range []string{
		"Restart the ios_webkit_debug_proxy that iwdp-cli started.",
		"Stops only an iwdp that iwdp-cli started and recorded,",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("restart-iwdp --help does not state %q:\n%s", want, res.stdout)
		}
	}
}

// --- Deadlines (criterion 1) ---

func TestEvalTimeoutNamesCommandPageAndCauses(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.NeverRespond("Runtime.evaluate")

	for name, env := range map[string][]string{
		"flag": nil,
		"env":  {"IWDP_TIMEOUT=1s"},
	} {
		t.Run(name, func(t *testing.T) {
			args := []string{"eval", "document.title", "--page", mock.URL}
			if name == "flag" {
				args = append(args, "--timeout", "1s")
			}
			res := runCLI(t, "", env, args...)
			if res.code != exitTimeout {
				t.Fatalf("exit %d, want %d; stderr %q", res.code, exitTimeout, res.stderr)
			}
			for _, want := range []string{"iwdp-cli eval timed out after", mock.URL, "Runtime.evaluate", "another inspector session", "device is locked", "--timeout"} {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("stderr lacks %q: %s", want, res.stderr)
				}
			}
			if res.elapsed > 8*time.Second {
				t.Errorf("command took %v with a 1s deadline", res.elapsed)
			}
		})
	}
}

func TestWatchdogEndsCommand(t *testing.T) {
	saved := current
	current = &commandRun{name: "eval", page: "ws://page", start: time.Now(), budget: time.Second}
	defer func() { current = saved }()

	var out bytes.Buffer
	var mu sync.Mutex
	codes := make(chan int, 1)
	armWatchdog(10*time.Millisecond, &processExit{
		cleanup: func(context.Context) error { return nil }, budget: time.Second, grace: time.Second,
		stderr: writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return out.Write(p)
		}),
		exit: func(code int) { codes <- code },
	})
	select {
	case code := <-codes:
		if code != exitTimeout {
			t.Errorf("exit code %d, want %d", code, exitTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not fire")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(out.String(), "iwdp-cli eval timed out") || !strings.Contains(out.String(), "watchdog") {
		t.Errorf("watchdog message = %q", out.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// --- Cookies (criterion 4) ---

const secretValue = "s3cr3t-session-token-1234567890"

// --- eval (criterion 5) ---

func TestEvalExceptionPrintsMessageAndStack(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Runtime.evaluate", map[string]interface{}{
		"result":    map[string]interface{}{"type": "object", "subtype": "error", "description": "ReferenceError: Can't find variable: nope", "objectId": "e1"},
		"wasThrown": true,
	})
	mock.HandleFunc("Runtime.callFunctionOn", map[string]interface{}{
		"result": map[string]interface{}{"type": "string", "value": "global code@\nevaluateWithScopeExtension@[native code]"},
	})
	res := runCLI(t, "", nil, "eval", "nope", "--page", mock.URL)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1; stdout %q", res.code, res.stdout)
	}
	if !strings.Contains(res.stderr, "ReferenceError: Can't find variable: nope") || !strings.Contains(res.stderr, "global code@") {
		t.Errorf("stderr = %q, want message and stack", res.stderr)
	}
	if strings.Contains(res.stdout, "undefined") {
		t.Errorf("stdout = %q", res.stdout)
	}
}

func TestEvalAwaitPrintsSettledValueAsJSON(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Runtime.evaluate", map[string]interface{}{
		"result": map[string]interface{}{"type": "object", "className": "Promise", "objectId": "p1"},
	})
	mock.HandleFunc("Runtime.awaitPromise", map[string]interface{}{
		"result": map[string]interface{}{"type": "object", "value": map[string]interface{}{"ok": true, "n": 2}},
	})
	res := runCLI(t, "", nil, "eval", "--await", "--page", mock.URL, "Promise.resolve({ok: true, n: 2})")
	if res.code != 0 {
		t.Fatalf("exit %d; stderr %q", res.code, res.stderr)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(res.stdout), &got); err != nil || got["ok"] != true || got["n"] != float64(2) {
		t.Errorf("stdout = %q, want the settled object as JSON", res.stdout)
	}
	if !contains(mock.ReceivedMethods(), "Runtime.awaitPromise") {
		t.Error("--await did not call Runtime.awaitPromise")
	}
}

// --- Screenshots (criterion 6) ---

func pngDataURL(t *testing.T, w, h int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestScreenshotDefaultsToViewportInTMPDIR(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Runtime.evaluate", map[string]interface{}{
		"result": map[string]interface{}{"type": "string", "value": `{"width":3,"height":2}`},
	})
	var mu sync.Mutex
	var coordinateSystem string
	dataURL := pngDataURL(t, 3, 2)
	mock.Handle("Page.snapshotRect", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			CoordinateSystem string `json:"coordinateSystem"`
		}
		_ = json.Unmarshal(params, &p)
		mu.Lock()
		coordinateSystem = p.CoordinateSystem
		mu.Unlock()
		return map[string]string{"dataURL": dataURL}, nil
	})
	cwd, tmp := t.TempDir(), t.TempDir()

	res := runCLI(t, cwd, []string{"TMPDIR=" + tmp}, "screenshot", "--page", mock.URL)
	if res.code != 0 {
		t.Fatalf("exit %d; stderr %q", res.code, res.stderr)
	}
	if files := dirEntries(t, cwd); len(files) != 0 {
		t.Errorf("screenshot wrote into the current directory: %v", files)
	}
	files := dirEntries(t, tmp)
	if len(files) != 1 || !strings.HasPrefix(files[0], "iwdp-screenshot-") || !strings.HasSuffix(files[0], ".png") {
		t.Fatalf("TMPDIR holds %v, want one iwdp-screenshot-<timestamp>.png", files)
	}
	path := filepath.Join(tmp, files[0])
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{path, "3x2 px", itoa(info.Size()) + " bytes"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout %q lacks %q", res.stdout, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if coordinateSystem != "Viewport" {
		t.Errorf("coordinateSystem = %q, want Viewport", coordinateSystem)
	}
}

func TestScreenshotSelectorAndOutput(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("DOM.getDocument", map[string]interface{}{"root": map[string]interface{}{"nodeId": 1}})
	mock.HandleFunc("DOM.querySelector", map[string]int{"nodeId": 5})
	mock.HandleFunc("Page.snapshotNode", map[string]string{"dataURL": pngDataURL(t, 4, 4)})
	out := filepath.Join(t.TempDir(), "card.png")

	res := runCLI(t, "", nil, "screenshot", "--selector", "#card", "-o", out, "--page", mock.URL)
	if res.code != 0 || !strings.Contains(res.stdout, out) || !strings.Contains(res.stdout, "4x4 px") {
		t.Fatalf("exit %d stdout %q stderr %q", res.code, res.stdout, res.stderr)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("output file: %v", err)
	}
}

func TestScreenshotRejectsConflictingOptionsBeforeConnecting(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	for _, args := range [][]string{
		{"screenshot", "--full-page", "--selector", "#a"},
		{"screenshot", "--fullpage"},
		{"screenshot", "-o"},
	} {
		res := runCLI(t, "", nil, append(args, "--page", mock.URL)...)
		if res.code != 2 {
			t.Errorf("%v: exit %d, want 2 (usage); stderr %q", args, res.code, res.stderr)
		}
	}
	if n := mock.ConnectionCount(); n != 0 {
		t.Errorf("rejected screenshots opened %d connections", n)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
