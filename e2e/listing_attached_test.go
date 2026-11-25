package e2e_test

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// exitNoPage is the exit code of a listing failure (core.CodeNoPage).
const exitNoPage = 5

// runBin runs bin with args and returns its combined output and exit code.
func runBin(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	}
	t.Fatalf("running %s %v: %v", bin, args, err)
	return "", 0
}

// docs/known-limits.md, "iwdp does not list pages while a page is
// connected": the legacy listing commands never call an iwdp that owns its
// port and closes the listing request without a reply "not running"; they
// name the empty reply and the advice. devices exits 5, the code
// of a listing failure; status has no exit-code contract. The binary reads
// the private listing of these tests (deadCLIListing), which answers that
// way.
func TestCLI_LegacyListingCommandsOnAnEmptyReply(t *testing.T) {
	bin := buildCLI(t)
	for _, cmd := range []string{"devices", "status"} {
		output, code := runBin(t, bin, cmd)
		if code == 0 {
			t.Errorf("%s succeeded on an empty reply: %s", cmd, output)
			continue
		}
		if cmd != "status" && code != exitNoPage {
			t.Errorf("%s: exit %d, want %d (a listing failure)", cmd, code, exitNoPage)
		}
		for _, want := range []string{deadCLIListing().Addr(), "closed the connection without an answer", "agent-ios daemon stop"} {
			if !strings.Contains(output, want) {
				t.Errorf("%s: output %q does not contain %q", cmd, output, want)
			}
		}
		if strings.Contains(strings.ToLower(output), "not running") {
			t.Errorf("%s: output %q calls the iwdp not running", cmd, output)
		}
	}
}

// Review F1 (review brief, Judge 2): the legacy listing commands exit 5
// with the advice on an empty listing reply.
func TestReviewLegacyListingExitCode(t *testing.T) {
	bin := buildCLI(t)
	for _, args := range [][]string{{"devices"}} {
		output, code := runBin(t, bin, args...)
		if code != exitNoPage {
			t.Errorf("%v: got exit %d; want exit %d; output: %s", args, code, exitNoPage, output)
		}
	}
}

// On a healthy listing the legacy listing commands answer from it: the
// device list and status.
func TestCLI_LegacyListingCommandsOnAHealthyListing(t *testing.T) {
	listing, device := fakeDeviceListing()
	bin := buildCLIFor(t, listing.Addr())
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"devices"}, []string{e2eDeviceName, e2eDeviceID, "(pages on " + device.Addr() + ")"}},
		{[]string{"status"}, []string{"ios_webkit_debug_proxy is running", "iwdp log of an iwdp that agent-ios starts: "}},
	} {
		before := listing.hits.Load()
		output, code := runBin(t, bin, c.args...)
		if code != 0 {
			t.Errorf("%v on a healthy listing: exit %d, output %q", c.args, code, output)
			continue
		}
		for _, want := range c.want {
			if !strings.Contains(output, want) {
				t.Errorf("%v: output %q does not contain %q", c.args, output, want)
			}
		}
		if listing.hits.Load() == before {
			t.Errorf("%v did not check the private listing %s", c.args, listing.Addr())
		}
	}
}

// When no process owns the listing port, iwdp is not running: status says
// so with the start advice and exits 1. devices starts iwdp as page
// commands do (decision D10, device check of 2026-11-12); the fake iwdp of
// these tests exits at once, so devices exits 1 with the failed start and
// the manual command of this system.
func TestCLI_LegacyListingCommandsOnAnUnownedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	bin := buildCLIFor(t, addr)
	// Review round 2, F5: the start advice is the command line of this
	// system; on Linux it has -s 127.0.0.1:27753 (docs/known-limits.md).
	manual := "ios_webkit_debug_proxy --no-frontend"
	if runtime.GOOS == "linux" {
		manual += " -s 127.0.0.1:27753"
	}
	for _, c := range []struct {
		args       []string
		want, hint string
	}{
		{[]string{"devices"}, "ios_webkit_debug_proxy is not running and starting it failed: ios_webkit_debug_proxy exited during its start", "run '" + manual + "' to see why"},
		{[]string{"status"}, "ios_webkit_debug_proxy is NOT running", "Start it with: " + manual + "\n"},
	} {
		output, code := runBin(t, bin, c.args...)
		if code != 1 {
			t.Errorf("%v on an unowned port: exit %d, want 1; output %q", c.args, code, output)
		}
		for _, want := range []string{c.want, c.hint} {
			if !strings.Contains(output, want) {
				t.Errorf("%v: output %q does not contain %q", c.args, output, want)
			}
		}
	}
}

// runBinStreams runs bin with args and returns its stdout, its stderr and
// its exit code.
func runBinStreams(t *testing.T, bin string, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), stderr.String(), 0
	case errors.As(err, &ee):
		return stdout.String(), stderr.String(), ee.ExitCode()
	}
	t.Fatalf("running %s %v: %v", bin, args, err)
	return "", "", 0
}

// Review round 2, F5: when no iwdp runs, the legacy commands tell how to
// start one with the command line of this system: status as its start
// advice, devices in the error of its failed start (it starts iwdp since
// 2026-11-12; the fake iwdp of these tests exits at once). On Linux it has
// -s 127.0.0.1:27753, as docs/known-limits.md tells a user who starts iwdp
// by hand; macOS keeps iwdp's default. The binary reads a private listing
// address that nothing owns (review round 3, F7: never 127.0.0.1:9221).
func TestStartHintNamesTheManualCommandOfThisSystem(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	bin := buildCLIFor(t, addr)
	manual := "ios_webkit_debug_proxy --no-frontend"
	if runtime.GOOS == "linux" {
		manual += " -s 127.0.0.1:27753"
	}
	for args, want := range map[string]string{"status": "Start it with: " + manual + "\n", "devices": "run '" + manual + "' to see why"} {
		_, stderr, code := runBinStreams(t, bin, args)
		if code != 1 || !strings.Contains(stderr, want) {
			t.Errorf("agent-ios %s without iwdp: exit %d, stderr %q; want exit 1 and %q", args, code, stderr, want)
		}
	}
}

// Review devfix-R1 F3 (device check 3, P3-D): status names the iwdp log
// when an owned listing is broken, too. The broken listing is a private
// loopback server (review round 3, F7: never 127.0.0.1:9221); the error
// names its address, which shows that the binary read it.
func TestReviewBrokenListingStatusLogPath(t *testing.T) {
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fixture broken", http.StatusServiceUnavailable)
	}))
	defer listing.Close()
	addr := listing.Listener.Addr().String()
	stdout, stderr, code := runBinStreams(t, buildCLIFor(t, addr), "status")
	if code == 0 {
		t.Fatal("broken listing unexpectedly healthy")
	}
	if !strings.Contains(stdout+stderr, "iwdp log of an iwdp that agent-ios starts:") {
		t.Fatalf("status omits log path on the listing failure: stdout %q, stderr %q", stdout, stderr)
	}
	if !strings.Contains(stderr, addr) {
		t.Fatalf("status error %q does not name the private listing %s", stderr, addr)
	}
}

// Review round 3, F7: the two tests above pass while another process owns
// iwdp's default listing address 127.0.0.1:9221, as on a host where the
// user's iwdp runs. This test owns that address itself when it is free;
// when it is not, another process owns it already. Nothing here sends a
// request there: a request to this listener, which never answers, would
// hang until the command's deadline and fail the tests.
func TestCLIHintTestsWithAnOccupiedDefaultListing(t *testing.T) {
	const defaultListing = "127.0.0.1:9221"
	if ln, err := net.Listen("tcp", defaultListing); err == nil {
		defer func() { _ = ln.Close() }()
	} else {
		t.Logf("%s is owned already: %v", defaultListing, err)
	}
	t.Run("start-hint", TestStartHintNamesTheManualCommandOfThisSystem)
	t.Run("broken-listing", TestReviewBrokenListingStatusLogPath)
}

// closeWithoutReply closes the connection of the request without a reply,
// as the iwdp of the second device check did while a page WebSocket was
// open.
func closeWithoutReply(t *testing.T, w http.ResponseWriter) {
	c, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("hijacking the request: %v", err)
		return
	}
	_ = c.Close()
}

// wantListingAdvice checks a legacy listing command whose read of the
// listing failed after its preliminary check: exit 5, the empty reply and
// the advice, never "not running".
func wantListingAdvice(t *testing.T, what, output string, code int) {
	t.Helper()
	if code != exitNoPage {
		t.Errorf("%s: exit %d, want %d (a listing failure); output %q", what, code, exitNoPage, output)
	}
	for _, want := range []string{"closed the connection without an answer", "agent-ios daemon stop"} {
		if !strings.Contains(output, want) {
			t.Errorf("%s: output %q does not contain %q", what, output, want)
		}
	}
	if strings.Contains(strings.ToLower(output), "not running") {
		t.Errorf("%s: output %q calls the iwdp not running", what, output)
	}
}

// Review F5: the listing answers the preliminary check and then closes
// each request without a reply. The legacy devices command reports the
// failed read as a listing failure with the advice: the check is no
// evidence that the read succeeds.
func TestCLI_LegacyDevicesWhenTheListingFailsAfterItsCheck(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			_, _ = w.Write([]byte("[]"))
			return
		}
		closeWithoutReply(t, w)
	}))
	t.Cleanup(srv.Close)
	bin := buildCLIFor(t, strings.TrimPrefix(srv.URL, "http://"))
	output, code := runBin(t, bin, "devices")
	if n := hits.Load(); n != 2 {
		t.Fatalf("the listing got %d requests, want 2 (the check, then the read)", n)
	}
	wantListingAdvice(t, "devices after a passed check", output, code)
}

// A failed iwdp start backs off the next devices command of another
// process: devices runs in the daemon, whose start back-off covers every
// automatic start (review of 2026-11-12; with devices in the client
// process, every devices command started iwdp again at once). The fake
// ios_webkit_debug_proxy of these tests exits at once and logs its calls.
func TestDevicesProcessesShareTheStartBackoff(t *testing.T) {
	// The shared daemon is the case under test: with AGENT_IOS_NO_DAEMON=1
	// from the runner, each command would have its own back-off (an
	// accepted limit, docs/known-limits.md).
	t.Setenv("AGENT_IOS_NO_DAEMON", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	// The fake's calls of every test of this package are in one log; the
	// tests run one at a time, so this test's starts are the new lines.
	count := func() int {
		data, _ := os.ReadFile(filepath.Join(e2eFakeBin, "calls.log"))
		n := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "ios_webkit_debug_proxy ") {
				n++
			}
		}
		return n
	}
	bin := buildCLIFor(t, addr)
	before := count()
	starts := func() int { return count() - before }
	first, code := runBin(t, bin, "devices")
	if code != 1 || !strings.Contains(first, "starting it failed") || starts() != 1 {
		t.Fatalf("first devices: exit %d, output %q, %d starts; want the failed start", code, first, starts())
	}
	second, code := runBin(t, bin, "devices")
	if code != 1 || !strings.Contains(second, "not trying again") || starts() != 1 {
		t.Fatalf("second devices (another process): exit %d, output %q, %d starts; want the back-off and no second start", code, second, starts())
	}
}
