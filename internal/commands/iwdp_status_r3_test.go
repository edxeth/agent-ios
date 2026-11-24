package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// emitEnv is the environment of runStatusCLI: like the CLI's
// (cmd/iwdp-cli cliHost), Emit writes a line to the command's stdout.
type emitEnv struct {
	fakeEnv
	w io.Writer
}

func (e emitEnv) Emit(line string) { _, _ = fmt.Fprintln(e.w, line) }

// runStatusCLI runs the command line args as the CLI does, with stdout
// shared by the result writer and Emit; it returns the exit code, stdout
// and stderr.
func runStatusCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := RunCLIStarted(context.Background(), args, &out, &errb, nil, func(context.Context, *Invocation) (Env, error) {
		return emitEnv{w: &out}, nil
	})
	return code, out.String(), errb.String()
}

// useStatusListing points status at the private listing addr for t.
func useStatusListing(t *testing.T, addr string) {
	t.Helper()
	saved := statusListing
	statusListing = func() string { return addr }
	t.Cleanup(func() { statusListing = saved })
}

// boundedText matches stdout marked with content boundaries (output.go:
// agent-browser's markers) and captures the nonce and the content.
var boundedText = regexp.MustCompile(`(?s)^--- AGENT_BROWSER_PAGE_CONTENT nonce=([0-9a-f]{32}) origin=unknown ---\n(.*)\n--- END_AGENT_BROWSER_PAGE_CONTENT nonce=([0-9a-f]{32}) ---\n$`)

// WS2 round-2 review part 4 F1: a failing status printed what it found on
// stdout before the result writer, so --content-boundaries and
// --max-output did not apply (repro status.out: no boundary with
// --json --content-boundaries). The output flags apply to the result of a
// failing status as to any other (docs/migration.md, the status row); the
// exit status stays 1 with the start advice on stderr.
func TestFailingStatusOutputTakesTheOutputFlags(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	logPath := filepath.Join(state, "iwdp-cli", "iwdp.log")
	logLine := "iwdp log of an iwdp that iwdp-cli starts: " + logPath
	useStatusListing(t, freeLoopbackAddr(t)) // nothing listens: not running
	text := "ios_webkit_debug_proxy is NOT running\n" + logLine

	code, out, stderr := runStatusCLI("status")
	if code != ExitError || out != text+"\n" || !strings.Contains(stderr, "ios_webkit_debug_proxy is not running.\nStart it with: ") {
		t.Errorf("status: exit %d, stdout %q, stderr %q; want exit 1, %q and the start advice", code, out, stderr, text)
	}
	code, out, _ = runStatusCLI("status", "--json")
	var st map[string]any
	if code != ExitError || json.Unmarshal([]byte(out), &st) != nil || st["running"] != false || st["log"] != logPath || len(st) != 2 {
		t.Errorf("status --json: exit %d, stdout %q; want exit 1 and {running: false, log: %q}", code, out, logPath)
	}
	code, out, _ = runStatusCLI("status", "--content-boundaries")
	m := boundedText.FindStringSubmatch(out)
	if code != ExitError || m == nil || m[1] != m[3] || m[2] != text {
		t.Errorf("status --content-boundaries: exit %d, stdout %q; want exit 1 and the text inside the boundary markers", code, out)
	}
	code, out, _ = runStatusCLI("status", "--json", "--content-boundaries")
	st = nil
	if code != ExitError || json.Unmarshal([]byte(out), &st) != nil || st["running"] != false {
		t.Errorf("status --json --content-boundaries: exit %d, stdout %q; want exit 1 and the status object", code, out)
	} else if b, ok := st["_boundary"].(map[string]any); !ok || b["origin"] != "unknown" || len(b["nonce"].(string)) != 32 {
		t.Errorf("status --json --content-boundaries: stdout %q; want the _boundary member", out)
	}
	code, out, _ = runStatusCLI("status", "--max-output", "10")
	want := text[:10] + "\n[output truncated: 10 of " + strconv.Itoa(len(text)) + " bytes shown]\n"
	if code != ExitError || out != want {
		t.Errorf("status --max-output 10: exit %d, stdout %q; want %q", code, out, want)
	}
}

// A process that owns the listing address but does not answer as iwdp:
// status fails with how it failed (exit 1), and its stdout, the log line,
// takes the output flags too.
func TestStatusOfABrokenListingTakesTheOutputFlags(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "broken", http.StatusInternalServerError)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	useStatusListing(t, l.Addr().String())
	code, out, stderr := runStatusCLI("status", "--content-boundaries")
	m := boundedText.FindStringSubmatch(out)
	if code != ExitError || m == nil || !strings.HasPrefix(m[2], "iwdp log of an iwdp that iwdp-cli starts: ") || !strings.Contains(stderr, "HTTP 500") {
		t.Errorf("status --content-boundaries on a broken listing: exit %d, stdout %q, stderr %q; want exit 1, the marked log line and the HTTP status", code, out, stderr)
	}
}

// status counts the devices of the listing without iwdp's SIMULATOR
// entry, with one more read of the listing that starts nothing. Private
// mode prints the count and no identifier (review of 2026-11-13, F14 and
// F17: a caller that may not restart iwdp learns whether a device is
// there before any command that can recover one).
func TestStatusCountsTheListedDevices(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `[{"deviceId":"SIMULATOR","deviceName":"SIMULATOR","url":"127.0.0.1:9225"},{"deviceId":"%s-udid","deviceName":"Phone of %s","url":"127.0.0.1:9224"}]`, privateMarker, privateMarker)
	}))
	t.Cleanup(srv.Close)
	useStatusListing(t, strings.TrimPrefix(srv.URL, "http://"))
	code, out, errOut := runStatusCLI("--private", "status")
	if code != ExitOK || out != "iwdp answers: true\ndevices 1\n" || errOut != "status exit 0\n" {
		t.Fatalf("--private status: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, out, _ = runStatusCLI("status")
	if code != ExitOK || !strings.Contains(out, "ios_webkit_debug_proxy is running\ndevices: 1\n") || strings.Contains(out, privateMarker) {
		t.Fatalf("status: exit %d, stdout %q; want the count and no identifier", code, out)
	}
}

// A status without a count is no success: iwdp answered the check and then
// not the read of its devices. A caller that stops on "devices 0" would go
// on to a command that can restart iwdp (review of 2026-11-13, F19).
func TestStatusFailsWhenTheDeviceListCannotBeRead(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if gets.Add(1)%2 == 1 { // the check of the listing
			_, _ = fmt.Fprint(w, "[]")
			return
		}
		http.Error(w, "broken "+privateMarker, http.StatusInternalServerError) // the read of the devices
	}))
	t.Cleanup(srv.Close)
	useStatusListing(t, strings.TrimPrefix(srv.URL, "http://"))
	code, out, errOut := runStatusCLI("--private", "status")
	if want := "status exit 1: the command failed (private mode hides the message)\n"; code != ExitError || out != "iwdp answers: true\n" || errOut != want {
		t.Fatalf("--private status: exit %d, stdout %q, stderr %q; want exit 1, no count and %q", code, out, errOut, want)
	}
	code, out, errOut = runStatusCLI("status")
	if code != ExitError || !strings.Contains(out, "its device list could not be read") || strings.Contains(out, "devices:") || !strings.Contains(errOut, "reading the device list") {
		t.Fatalf("status: exit %d, stdout %q, stderr %q; want exit 1, no count and the error of the read", code, out, errOut)
	}
	if got := gets.Load(); got != 4 {
		t.Fatalf("the listing got %d requests; want a check and a read for each of the 2 commands", got)
	}
}

// A command that fails with a result (status) and cannot write that result
// still prints exactly one status line in private mode, that of its own
// error (review of 2026-11-13, F20). Without the mode both errors print,
// as before.
func TestAFailedCommandThatCannotWriteItsResultPrintsOnePrivateLine(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	useStatusListing(t, freeLoopbackAddr(t)) // nothing listens: status fails with a result
	env := func(context.Context, *Invocation) (Env, error) { return emitEnv{w: io.Discard}, nil }
	var errOut bytes.Buffer
	code := RunCLIStarted(context.Background(), []string{"--private", "status"}, failingWriter{}, &errOut, nil, env)
	if want := "status exit 1: the command failed (private mode hides the message)\n"; code != ExitError || errOut.String() != want {
		t.Fatalf("--private status: exit %d, stderr %q; want exit 1 and only %q", code, errOut.String(), want)
	}
	errOut.Reset()
	code = RunCLIStarted(context.Background(), []string{"status"}, failingWriter{}, &errOut, nil, env)
	if code != ExitError || !strings.Contains(errOut.String(), "Error: writing the result: write refused") || !strings.Contains(errOut.String(), "ios_webkit_debug_proxy is not running.") {
		t.Fatalf("status: exit %d, stderr %q; want exit 1, the error of the write and the error of the command", code, errOut.String())
	}
}

// With no device listed and a simulator configured, status says how to
// use the simulator, in private mode too: "devices 0" alone read as "no
// iOS target", and an agent stopped there (live run, 2026-11-24). Without
// a configuration, or with a device, it prints the count alone.
func TestStatusPointsAtAConfiguredSimulator(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `[{"deviceId":"SIMULATOR","deviceName":"SIMULATOR","url":"127.0.0.1:9225"}]`)
	}))
	t.Cleanup(srv.Close)
	useStatusListing(t, strings.TrimPrefix(srv.URL, "http://"))
	saved := simConfigured
	t.Cleanup(func() { simConfigured = saved })
	simConfigured = func() bool { return true }
	code, out, _ := runStatusCLI("--private", "status")
	if code != ExitOK || out != "iwdp answers: true\ndevices 0\n"+simulatorHint+"\n" {
		t.Fatalf("--private status with a simulator: exit %d, stdout %q", code, out)
	}
	if _, out, _ := runStatusCLI("status"); !strings.Contains(out, "devices: 0\n"+simulatorHint+"\n") {
		t.Fatalf("status with a simulator: %q", out)
	}
	simConfigured = func() bool { return false }
	if _, out, _ := runStatusCLI("--private", "status"); out != "iwdp answers: true\ndevices 0\n" {
		t.Fatalf("--private status without a simulator: %q", out)
	}
}
