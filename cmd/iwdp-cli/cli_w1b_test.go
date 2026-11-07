package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// privateDaemon returns the environment of CLI runs that share one daemon
// of their own, stopped at the end of the test.
func privateDaemon(t *testing.T) []string {
	t.Helper()
	dir, err := os.MkdirTemp(testRuntimeDir, "d")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopDaemonIn(filepath.Join(dir, "iwdp")) })
	return []string{"XDG_RUNTIME_DIR=" + dir}
}

func with(env []string, kv ...string) []string { return append(append([]string{}, env...), kv...) }

func wantExit(t *testing.T, res cliResult, code int, stderr string) {
	t.Helper()
	if res.code != code || !strings.Contains(res.stderr, stderr) {
		t.Fatalf("exit %d, stderr %q; want exit %d and %q", res.code, res.stderr, code, stderr)
	}
}

// physicalPage is one fake page of a physical device behind a fake iwdp
// listing, and a daemon that serves it as the default transport.
func physicalPage(t *testing.T, frontend bool, fo fakewebkit.Options) (env []string, f *fakewebkit.Server, dev *fakewebkit.ListedDevice, pg *fakewebkit.ListedPage) {
	t.Helper()
	f = fakewebkit.New(fo)
	t.Cleanup(f.Close)
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 2}})
	l := fakewebkit.NewListing(frontend)
	t.Cleanup(l.Close)
	dev = l.AddDevice("00008020-0000000000000001", "iPhone X", "16.7.10")
	pg = dev.AddPage("Fixture", "https://fixture.example/", f.URL)
	env = privateDaemon(t)
	if res := runCLI(t, "", env, "daemon", "start", "--transport", "iwdp", "--addr", l.Addr()); res.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", res.code, res.stderr)
	}
	return env, f, dev, pg
}

// 7.3 item 7 (b, c) at the CLI: pages of a physical device need a device
// reservation (D11) whether ownership is unknown (iwdp -F) or free; another
// owner's reservation refuses too. Exit 6, before any WebSocket dial.
func TestCLIReservationExitCodes(t *testing.T) {
	for _, frontend := range []bool{false, true} {
		env, f, _, _ := physicalPage(t, frontend, fakewebkit.Options{})
		wantExit(t, runCLI(t, "", env, "eval", "1+1"), 6, "device reserve --owner default")
		if res := runCLI(t, "", env, "device", "reserve", "--owner", "orchestrator"); res.code != 0 {
			t.Fatalf("device reserve: exit %d, stderr %q", res.code, res.stderr)
		}
		wantExit(t, runCLI(t, "", with(env, "IWDP_DEVICE_RESERVATION=other"), "eval", "1+1"), 6, "reserved by \"orchestrator\"")
		if n := f.ConnectionCount(); n != 0 {
			t.Fatalf("frontend %v: the refused attaches dialed the page %d times", frontend, n)
		}
		res := runCLI(t, "", with(env, "IWDP_DEVICE_RESERVATION=orchestrator"), "eval", "1+1")
		if res.code != 0 || strings.TrimSpace(res.stdout) != "2" {
			t.Fatalf("frontend %v: eval with the reservation: exit %d, stdout %q, stderr %q", frontend, res.code, res.stdout, res.stderr)
		}
	}
}

// 7.3 item 7 (a, d) at the CLI: the busy hint refuses the attach with exit
// 4 and no dial; a stolen connection makes the lease lost (exit 4,
// "ownership is unknown", no automatic reconnect); --takeover needs the
// reservation (exit 6) and with it reattaches.
func TestCLIBusyLostAndTakeoverExitCodes(t *testing.T) {
	env, f, dev, pg := physicalPage(t, true, fakewebkit.Options{Steal: true})
	if res := runCLI(t, "", env, "device", "reserve", "--owner", "orchestrator"); res.code != 0 {
		t.Fatalf("device reserve: exit %d, stderr %q", res.code, res.stderr)
	}
	owned := with(env, "IWDP_DEVICE_RESERVATION=orchestrator")
	// The hint needs iwdp's frontend: another page of the device lists its
	// devtoolsFrontendUrl, so an empty one means busy.
	dev.AddPage("Other", "https://other.example/", f.URL+"x")
	dev.SetBusy(pg, true)
	wantExit(t, runCLI(t, "", owned, "eval", "1+1"), 4, "held by another inspector client")
	if n := f.ConnectionCount(); n != 0 {
		t.Fatalf("the busy page was dialed %d times", n)
	}
	dev.SetBusy(pg, false)
	if res := runCLI(t, "", owned, "eval", "1+1"); res.code != 0 {
		t.Fatalf("eval: exit %d, stderr %q", res.code, res.stderr)
	}
	// Another inspector takes the page.
	thief, _, err := websocket.DefaultDialer.Dial(f.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = thief.Close() }()
	eventually(t, 5*time.Second, "the lease to become lost", func() bool {
		return strings.Contains(runCLI(t, "", env, "daemon", "status").stdout, "lease lost")
	})
	wantExit(t, runCLI(t, "", owned, "eval", "1+1"), 4, "ownership is unknown")
	if n := f.ConnectionCount(); n != 2 {
		t.Fatalf("connections %d, want 2 (the daemon's and the thief's): no automatic reconnect", n)
	}
	wantExit(t, runCLI(t, "", env, "--takeover", "eval", "1+1"), 6, "reserved by \"orchestrator\"")
	res := runCLI(t, "", owned, "--takeover", "eval", "1+1")
	if res.code != 0 || strings.TrimSpace(res.stdout) != "2" || f.ConnectionCount() != 3 {
		t.Fatalf("--takeover: exit %d, stdout %q, stderr %q, connections %d", res.code, res.stdout, res.stderr, f.ConnectionCount())
	}
}

// 7.3 item 8 at the CLI: the lease error (exit 4) comes at once;
// --wait-lease longer than the deadline exits 124 at the deadline (D9);
// --wait-lease succeeds after the holder's close; close --all exits 2
// before any connection; close in one session leaves the other alone.
func TestCLILeasesWaitLeaseAndClose(t *testing.T) {
	env := privateDaemon(t)
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 1}})
	e := with(env, "IWDP_WS_URL="+mock.URL)
	if res := runCLI(t, "", e, "--session", "s1", "eval", "1"); res.code != 0 {
		t.Fatalf("s1 eval: exit %d, stderr %q", res.code, res.stderr)
	}
	res := runCLI(t, "", e, "--session", "s2", "eval", "1")
	wantExit(t, res, 4, "leased by session \"s1\"")
	if res.elapsed > 3*time.Second {
		t.Fatalf("the lease error took %v", res.elapsed)
	}
	res = runCLI(t, "", e, "--session", "s2", "--wait-lease", "5s", "--timeout", "700ms", "eval", "1")
	wantExit(t, res, 124, "timed out")
	if res.elapsed > 3*time.Second {
		t.Fatalf("--wait-lease past the deadline ended after %v", res.elapsed)
	}
	wantExit(t, runCLI(t, "", e, "--session", "s1", "close", "--all"), 2, "unknown flag --all")
	if info := runCLI(t, "", e, "--session", "s1", "session", "info"); !strings.Contains(info.stdout, "1 leased page") {
		t.Fatalf("close --all changed s1: %q", info.stdout)
	}
	waiter := startCLI(t, e, "--session", "s2", "--wait-lease", "6s", "eval", "1")
	time.Sleep(500 * time.Millisecond)
	if res := runCLI(t, "", e, "--session", "s1", "close"); res.code != 0 || !strings.Contains(res.stdout, "released 1 page lease") {
		t.Fatalf("s1 close: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
	if code := waiter.wait(t, 10*time.Second); code != 0 {
		t.Fatalf("s2 --wait-lease after s1's close: exit %d, stderr %q", code, waiter.stderr.String())
	}
	if res := runCLI(t, "", e, "--session", "s1", "close"); res.code != 0 || !strings.Contains(res.stdout, "released 0 page lease") {
		t.Fatalf("second s1 close: exit %d, stdout %q", res.code, res.stdout)
	}
	if info := runCLI(t, "", e, "--session", "s2", "session", "info"); !strings.Contains(info.stdout, "1 leased page") {
		t.Fatalf("s1's close touched s2: %q", info.stdout)
	}
}

// close --all is refused by the parser: no daemon is started for it.
func TestCLICloseAllNeverConnects(t *testing.T) {
	dir, err := os.MkdirTemp(testRuntimeDir, "n")
	if err != nil {
		t.Fatal(err)
	}
	wantExit(t, runCLI(t, "", []string{"XDG_RUNTIME_DIR=" + dir}, "close", "--all"), 2, "unknown flag --all")
	if _, err := os.Lstat(filepath.Join(dir, "iwdp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("close --all created the daemon directory: %v", err)
	}
}

// 7.3 item 3 at the CLI (deadline exit 124 per D9): the fake stalls the
// call and the probe; eval --timeout 1s exits 124 within 1.2 s, the fake
// got exactly one evaluation (no retry), and the session reports the page
// stalled. Recovery is checked at core level
// (TestDeadProbeStallsThenRecoversOnce).
//
// The 1.2 s bound is measured from the CLI process's package
// initialization to its exit (runCLIFromInit). Design 2.6, "One absolute deadline per operation": the
// command deadline "is fixed when the request arrives" and covers
// everything from there; process creation and the Go runtime start come
// before the command exists, so they are not part of the bound.
func TestCLIEvalDeadlineWithDeadProbe(t *testing.T) {
	env := privateDaemon(t)
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Stall("Runtime.evaluate")
	e := with(env, "IWDP_WS_URL="+f.URL)
	if res := runCLI(t, "", e, "session"); res.code != 0 { // starts the daemon
		t.Fatalf("session: exit %d, stderr %q", res.code, res.stderr)
	}
	res, ran := runCLIFromInit(t, e, "eval", "--timeout", "1s", "6*7")
	wantExit(t, res, 124, "Runtime.evaluate")
	t.Logf("eval --timeout 1s: %v from package initialization to exit, %v including the process start", ran, res.elapsed)
	if ran > 1200*time.Millisecond {
		t.Fatalf("eval --timeout 1s exited %v after its package initialization, want at most 1.2 s", ran)
	}
	n := 0
	for _, r := range f.ReceivedMethod("Runtime.evaluate") {
		if strings.Contains(string(r.Params), "6*7") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the fake received %d evaluations of 6*7, want 1", n)
	}
	eventually(t, 3*time.Second, "session info to report the page stalled", func() bool {
		return strings.Contains(runCLI(t, "", e, "session", "info").stdout, "connection stalled")
	})
	// The page still does not answer: the one background reconnect fails
	// its probe, and the session stays stalled.
	time.Sleep(time.Second)
	if info := runCLI(t, "", e, "session", "info").stdout; !strings.Contains(info, "connection stalled") {
		t.Fatalf("session info after the recovery attempt: %q, want the page still stalled", info)
	}
}
