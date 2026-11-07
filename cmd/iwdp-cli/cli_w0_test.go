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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
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

func TestEveryCommandHasHelp(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range helpSections {
		for _, c := range s.commands {
			if seen[c.name] {
				t.Errorf("duplicate help entry %q", c.name)
			}
			seen[c.name] = true
			if _, ok := commands[c.name]; !ok {
				t.Errorf("help entry %q has no command", c.name)
			}
			if !strings.HasPrefix(c.synopsis, c.name) {
				t.Errorf("synopsis %q does not start with %q", c.synopsis, c.name)
			}
		}
	}
	for name := range commands {
		if !seen[name] {
			t.Errorf("command %q has no help entry", name)
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

	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, flag := range []string{"--help", "-h"} {
			// Put the flag after a plausible argument, as agents do.
			res := runCLI(t, cwd, env, name, "x", flag)
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

// --- Deadlines (criterion 1) ---

func TestParseGlobalOptions(t *testing.T) {
	args, opts, err := parseGlobalOptions([]string{"eval", "1"}, "")
	if err != nil || opts.timeout != 30*time.Second || opts.timeoutSet || len(args) != 2 {
		t.Errorf("default: args=%v opts=%+v err=%v; want 30s default", args, opts, err)
	}
	_, opts, err = parseGlobalOptions([]string{"eval", "1"}, "7s")
	if err != nil || opts.timeout != 7*time.Second || !opts.timeoutSet {
		t.Errorf("env: opts=%+v err=%v", opts, err)
	}
	args, opts, err = parseGlobalOptions([]string{"--timeout", "2m", "eval", "1", "--timeout=45"}, "7s")
	if err != nil || opts.timeout != 45*time.Second || strings.Join(args, " ") != "eval 1" {
		t.Errorf("flags: args=%v opts=%+v err=%v; last flag wins over env", args, opts, err)
	}
	for _, bad := range [][]string{{"--timeout"}, {"--timeout", "soon"}, {"--timeout=0"}, {"--timeout", "-5s"}} {
		if _, _, err := parseGlobalOptions(bad, ""); err == nil {
			t.Errorf("parseGlobalOptions(%v): expected an error", bad)
		}
	}
	if _, _, err := parseGlobalOptions(nil, "never"); err == nil || !strings.Contains(err.Error(), "IWDP_TIMEOUT") {
		t.Errorf("bad IWDP_TIMEOUT: err = %v", err)
	}
}

func TestCommandBudget(t *testing.T) {
	def := globalOptions{timeout: 30 * time.Second}
	explicit := globalOptions{timeout: 10 * time.Second, timeoutSet: true}
	tests := []struct {
		name      string
		command   string
		args      []string
		opts      globalOptions
		want      time.Duration
		unlimited bool
	}{
		{"plain command", "eval", []string{"1"}, def, 30 * time.Second, false},
		{"console default capture", "console", nil, def, 40 * time.Second, false},
		{"network custom capture", "network", []string{"-d", "5s"}, def, 35 * time.Second, false},
		{"console follow", "console", []string{"--follow"}, def, 0, true},
		{"recording seconds", "timeline-record", []string{"-d", "7"}, def, 37 * time.Second, false},
		{"heap snapshot default", "heap-snapshot", nil, def, tools.HeapSnapshotTimeout, false},
		{"heap snapshot explicit", "heap-snapshot", nil, explicit, 10 * time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, unlimited, err := commandBudget(tt.command, tt.args, tt.opts)
			if err != nil || got != tt.want || unlimited != tt.unlimited {
				t.Errorf("commandBudget = %v, %v, %v; want %v, %v", got, unlimited, err, tt.want, tt.unlimited)
			}
		})
	}
	if _, _, err := commandBudget("console", []string{"-d", "x"}, def); err == nil {
		t.Error("expected an error for an invalid duration")
	}
}

func TestEvalTimeoutNamesCommandPageAndCauses(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.NeverRespond("Runtime.evaluate")

	for name, env := range map[string][]string{
		"flag": {"IWDP_WS_URL=" + mock.URL},
		"env":  {"IWDP_WS_URL=" + mock.URL, "IWDP_TIMEOUT=1s"},
	} {
		t.Run(name, func(t *testing.T) {
			args := []string{"eval", "document.title"}
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

// --- Bounded streaming (criterion 2) ---

func TestConsoleStopsAfterDurationWithSummary(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.Handle("Console.enable", func(string, json.RawMessage) (interface{}, error) {
		go func() {
			time.Sleep(100 * time.Millisecond)
			for _, text := range []string{"first", "second"} {
				_ = mock.SendEvent("Console.messageAdded", map[string]interface{}{
					"message": map[string]interface{}{"level": "log", "text": text},
				})
			}
		}()
		return map[string]interface{}{}, nil
	})

	res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, "console", "-d", "1s")
	if res.code != 0 {
		t.Fatalf("exit %d; stderr %q", res.code, res.stderr)
	}
	if !strings.Contains(res.stdout, "[log] first") || !strings.Contains(res.stdout, "[log] second") {
		t.Errorf("stdout lacks the messages: %q", res.stdout)
	}
	if !strings.Contains(res.stderr, "Captured 2 console messages") {
		t.Errorf("stderr lacks the summary: %q", res.stderr)
	}
	if res.elapsed > 8*time.Second {
		t.Errorf("console -d 1s ran %v", res.elapsed)
	}
	if !contains(mock.ReceivedMethods(), "Console.disable") {
		t.Error("console did not disable the Console domain at the end")
	}
}

func TestNetworkStopsAfterDurationWithSummary(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.Handle("Network.enable", func(string, json.RawMessage) (interface{}, error) {
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = mock.SendEvent("Network.requestWillBeSent", map[string]interface{}{
				"requestId": "1", "request": map[string]string{"method": "GET", "url": "https://example.com/a"},
			})
			_ = mock.SendEvent("Network.responseReceived", map[string]interface{}{
				"requestId": "1", "response": map[string]interface{}{"url": "https://example.com/a", "status": 200, "statusText": "OK", "mimeType": "text/html"},
			})
		}()
		return map[string]interface{}{}, nil
	})

	res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, "network", "--duration=1s")
	if res.code != 0 {
		t.Fatalf("exit %d; stderr %q", res.code, res.stderr)
	}
	if !strings.Contains(res.stdout, "GET https://example.com/a") {
		t.Errorf("stdout lacks the request: %q", res.stdout)
	}
	if !strings.Contains(res.stderr, "Captured 1 requests, 1 responses, 0 failures") {
		t.Errorf("stderr lacks the summary: %q", res.stderr)
	}
	if res.elapsed > 8*time.Second {
		t.Errorf("network --duration=1s ran %v", res.elapsed)
	}
}

// streamCases are the two streaming commands with their domain and one event
// that each prints on stdout.
var streamCases = []struct {
	command, domain, event string
	params                 map[string]interface{}
	printed                string
}{
	{"console", "Console", "Console.messageAdded", map[string]interface{}{"message": map[string]interface{}{"level": "log", "text": "late"}}, "[log] late"},
	{"network", "Network", "Network.requestWillBeSent", map[string]interface{}{"requestId": "1", "request": map[string]string{"method": "GET", "url": "https://example.com/late"}}, "GET https://example.com/late"},
}

// TestStreamDeadlineExitsWithTimeout pins that a bounded stream cut short by
// its command deadline is a deadline error (exit 124, CLAUDE.md "Deadlines"),
// not a success. Slow setup uses up part of the --timeout allowance, so the
// deadline ends the -d window early.
func TestStreamDeadlineExitsWithTimeout(t *testing.T) {
	for _, tc := range streamCases {
		t.Run(tc.command, func(t *testing.T) {
			mock := testutil.NewMockServer()
			defer mock.Close()
			mock.Handle(tc.domain+".enable", func(string, json.RawMessage) (interface{}, error) {
				time.Sleep(500 * time.Millisecond)
				return map[string]interface{}{}, nil
			})
			startTestDaemon(t) // the 200ms allowance is for the page, not the daemon's start
			res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, tc.command, "--timeout", "200ms", "-d", "1s")
			if res.code != exitTimeout {
				t.Fatalf("exit %d, want %d; stderr %q", res.code, exitTimeout, res.stderr)
			}
			for _, want := range []string{"iwdp-cli " + tc.command + " timed out after", "Captured"} {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("stderr lacks %q: %s", want, res.stderr)
				}
			}
			if !contains(mock.ReceivedMethods(), tc.domain+".disable") {
				t.Errorf("%s did not disable the %s domain after the deadline", tc.command, tc.domain)
			}
			if res.elapsed > 8*time.Second {
				t.Errorf("%s ran %v with a 1.2s deadline", tc.command, res.elapsed)
			}
		})
	}
}

// TestStreamTeardownDeadlineExitsWithTimeout pins that a disable call that
// stalls at the end of a bounded stream ends at the command deadline with the
// deadline error naming that call, instead of running on until the watchdog.
func TestStreamTeardownDeadlineExitsWithTimeout(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.NeverRespond("Console.disable")
	res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, "console", "--timeout", "1s", "-d", "500ms")
	if res.code != exitTimeout {
		t.Fatalf("exit %d, want %d; stderr %q", res.code, exitTimeout, res.stderr)
	}
	if !strings.Contains(res.stderr, "Console.disable") || strings.Contains(res.stderr, "watchdog") {
		t.Errorf("stderr = %q, want the deadline error naming Console.disable, not the watchdog", res.stderr)
	}
}

// TestFollowOutlivesSetupTimeoutUntilSignal pins the --follow lifecycle
// (CLAUDE.md "Deadlines", usage of console and network): --timeout bounds only
// connect and enable, the stream keeps printing events after it, and an
// interrupt disables the domain and exits 0 with the summary.
func TestFollowOutlivesSetupTimeoutUntilSignal(t *testing.T) {
	for _, tc := range streamCases {
		t.Run(tc.command, func(t *testing.T) {
			mock := testutil.NewMockServer()
			defer mock.Close()
			const setupTimeout = 300 * time.Millisecond
			// The setup timeout bounds connect and enable; a cold daemon
			// start under -race on a loaded host took most of it (R6-5).
			startTestDaemon(t)
			p := startCLI(t, []string{"IWDP_WS_URL=" + mock.URL}, tc.command, "--follow", "--timeout", setupTimeout.String())
			eventually(t, 5*time.Second, tc.domain+".enable", func() bool {
				select {
				case <-p.done:
					t.Fatalf("iwdp-cli exited %d before %s.enable; stderr %q", p.code, tc.domain, p.stderr.String())
				default:
				}
				return contains(mock.ReceivedMethods(), tc.domain+".enable")
			})

			time.Sleep(2 * setupTimeout) // well past the setup timeout
			if err := mock.SendEvent(tc.event, tc.params); err != nil {
				t.Fatal(err)
			}
			eventually(t, 5*time.Second, "the event on stdout", func() bool { return strings.Contains(p.stdout.String(), tc.printed) })

			if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			if code := p.wait(t, 8*time.Second); code != 0 {
				t.Fatalf("exit %d after interrupt, want 0; stderr %q", code, p.stderr.String())
			}
			if !strings.Contains(p.stderr.String(), "(interrupted)") {
				t.Errorf("stderr lacks the interrupted summary: %q", p.stderr.String())
			}
			if !contains(mock.ReceivedMethods(), tc.domain+".disable") {
				t.Errorf("%s --follow did not disable the %s domain on interrupt", tc.command, tc.domain)
			}
		})
	}
}

// TestFollowSetupStillTimesOut pins that --follow keeps a deadline on its
// setup: a stalled enable ends with exit 124 at the --timeout value.
func TestFollowSetupStillTimesOut(t *testing.T) {
	for _, tc := range streamCases {
		t.Run(tc.command, func(t *testing.T) {
			mock := testutil.NewMockServer()
			defer mock.Close()
			mock.NeverRespond(tc.domain + ".enable")
			res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, tc.command, "--follow", "--timeout", "500ms")
			if res.code != exitTimeout || !strings.Contains(res.stderr, tc.domain+".enable") {
				t.Errorf("exit %d stderr %q; want %d and an error naming %s.enable", res.code, res.stderr, exitTimeout, tc.domain)
			}
			if res.elapsed > 8*time.Second {
				t.Errorf("%s --follow ran %v with a 500ms setup timeout", tc.command, res.elapsed)
			}
		})
	}
}

func TestConsoleMessagesDurationStillWorks(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, "console-messages", "-d", "1")
	if res.code != 0 || strings.TrimSpace(res.stdout) != "[]" {
		t.Errorf("console-messages -d 1: exit %d stdout %q stderr %q", res.code, res.stdout, res.stderr)
	}
}

func TestStreamRejectsUnknownOptionBeforeConnecting(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, "console", "--forever")
	if res.code != 1 || mock.ConnectionCount() != 0 {
		t.Errorf("exit %d, connections %d; want 1 and 0", res.code, mock.ConnectionCount())
	}
}

// --- Cookies (criterion 4) ---

const secretValue = "s3cr3t-session-token-1234567890"

func TestCookiesRedactValuesByDefault(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Page.getCookies", map[string]interface{}{"cookies": []webkit.Cookie{
		{Name: "sid", Value: secretValue, Domain: ".example.com", Path: "/", HTTPOnly: true, Secure: true, Session: true, SameSite: "Lax"},
	}})
	env := []string{"IWDP_WS_URL=" + mock.URL}

	res := runCLI(t, "", env, "cookies")
	if res.code != 0 {
		t.Fatalf("exit %d; stderr %q", res.code, res.stderr)
	}
	if strings.Contains(res.stdout+res.stderr, secretValue) {
		t.Errorf("cookies printed the value: %q", res.stdout)
	}
	if !strings.Contains(res.stdout, "sid=s3cr...(redacted, 31 chars)") || !strings.Contains(res.stdout, "sameSite=Lax") {
		t.Errorf("stdout = %q, want the redacted value with length and prefix", res.stdout)
	}

	res = runCLI(t, "", env, "cookies", "--show-values")
	if res.code != 0 || !strings.Contains(res.stdout, "sid="+secretValue) {
		t.Errorf("--show-values: exit %d stdout %q", res.code, res.stdout)
	}
}

func TestSetCookieSendsAttributesAndVerifies(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	var mu sync.Mutex
	var sent []webkit.Cookie
	mock.Handle("Page.setCookie", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			Cookie webkit.Cookie `json:"cookie"`
		}
		_ = json.Unmarshal(params, &p)
		mu.Lock()
		sent = append(sent, p.Cookie)
		mu.Unlock()
		return map[string]interface{}{}, nil
	})
	mock.Handle("Page.getCookies", func(string, json.RawMessage) (interface{}, error) {
		mu.Lock()
		defer mu.Unlock()
		return map[string]interface{}{"cookies": sent[len(sent)-1:]}, nil
	})
	env := []string{"IWDP_WS_URL=" + mock.URL}

	res := runCLI(t, "", env, "set-cookie", "sid", secretValue, "example.com",
		"--same-site", "strict", "--secure", "--http-only", "--path", "/app", "--expires", "2030-01-01T00:00:00Z")
	if res.code != 0 {
		t.Fatalf("exit %d; stderr %q", res.code, res.stderr)
	}
	mu.Lock()
	got := sent[0]
	mu.Unlock()
	wantExpires := float64(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	if got.SameSite != "Strict" || !got.Secure || !got.HTTPOnly || got.Path != "/app" || got.Expires != wantExpires || got.Session {
		t.Errorf("Page.setCookie cookie = %+v", got)
	}
	if !strings.Contains(res.stdout, "verified") || strings.Contains(res.stdout+res.stderr, secretValue) {
		t.Errorf("stdout = %q, want a verified message without the value", res.stdout)
	}

	res = runCLI(t, "", env, "set-cookie", "plain", "v", "example.com")
	mu.Lock()
	got = sent[len(sent)-1]
	mu.Unlock()
	if res.code != 0 || got.SameSite != "Lax" || !got.Session {
		t.Errorf("default set-cookie: exit %d, cookie %+v; want sameSite Lax session cookie", res.code, got)
	}
}

func TestSetCookieFailsWhenNotReadBack(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Page.getCookies", map[string]interface{}{"cookies": []webkit.Cookie{}})
	res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, "set-cookie", "sid", "v", "example.com")
	if res.code != 1 || !strings.Contains(res.stderr, "not found") {
		t.Errorf("exit %d stderr %q; want 1 and a not-found error", res.code, res.stderr)
	}
}

func TestSetCookieRejectsBadSameSiteBeforeConnecting(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, "set-cookie", "sid", "v", "example.com", "--same-site", "sometimes")
	if res.code != 1 || mock.ConnectionCount() != 0 || !strings.Contains(res.stderr, "sometimes") {
		t.Errorf("exit %d, connections %d, stderr %q", res.code, mock.ConnectionCount(), res.stderr)
	}
}

func TestParseCookieExpiry(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"1893456000":           time.Unix(1893456000, 0),
		"2030-01-01T00:00:00Z": time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		"24h":                  now.Add(24 * time.Hour),
	} {
		got, err := parseCookieExpiry(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseCookieExpiry(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseCookieExpiry("tomorrow", now); err == nil {
		t.Error("expected an error for an unparseable expiry")
	}
}

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
	res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, "eval", "nope")
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
	res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, "eval", "--await", "Promise.resolve({ok: true, n: 2})")
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

	res := runCLI(t, cwd, []string{"IWDP_WS_URL=" + mock.URL, "TMPDIR=" + tmp}, "screenshot")
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

	res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, "screenshot", "--selector", "#card", "-o", out)
	if res.code != 0 || !strings.Contains(res.stdout, out) || !strings.Contains(res.stdout, "4x4 px") {
		t.Fatalf("exit %d stdout %q stderr %q", res.code, res.stdout, res.stderr)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("output file: %v", err)
	}
}

// TestSnapshotNodeDefaultsToTMPDIR pins the snapshot-node default output
// (usage of snapshot-node): without -o it writes only a new
// iwdp-snapshot-<timestamp>.png under TMPDIR and prints its path and size.
func TestSnapshotNodeDefaultsToTMPDIR(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	var mu sync.Mutex
	var nodeID float64
	mock.Handle("Page.snapshotNode", func(_ string, params json.RawMessage) (interface{}, error) {
		var p struct {
			NodeID float64 `json:"nodeId"`
		}
		_ = json.Unmarshal(params, &p)
		mu.Lock()
		nodeID = p.NodeID
		mu.Unlock()
		return map[string]string{"dataURL": pngDataURL(t, 5, 3)}, nil
	})
	cwd, tmp := t.TempDir(), t.TempDir()

	res := runCLI(t, cwd, []string{"IWDP_WS_URL=" + mock.URL, "TMPDIR=" + tmp}, "snapshot-node", "7")
	if res.code != 0 {
		t.Fatalf("exit %d; stderr %q", res.code, res.stderr)
	}
	if files := dirEntries(t, cwd); len(files) != 0 {
		t.Errorf("snapshot-node wrote into the current directory: %v", files)
	}
	files := dirEntries(t, tmp)
	if len(files) != 1 || !strings.HasPrefix(files[0], "iwdp-snapshot-") || !strings.HasSuffix(files[0], ".png") {
		t.Fatalf("TMPDIR holds %v, want one iwdp-snapshot-<timestamp>.png", files)
	}
	path := filepath.Join(tmp, files[0])
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Snapshot saved to " + path + " (5x3 px, " + itoa(info.Size()) + " bytes)"; !strings.Contains(res.stdout, want) {
		t.Errorf("stdout %q lacks %q", res.stdout, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if nodeID != 7 {
		t.Errorf("Page.snapshotNode nodeId = %v, want 7", nodeID)
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
		res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, args...)
		if res.code != 1 {
			t.Errorf("%v: exit %d, want 1", args, res.code)
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
