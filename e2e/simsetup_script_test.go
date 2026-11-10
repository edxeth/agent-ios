package e2e_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests run the page selection and the Simulator ownership code of
// scripts/sim-setup.sh on any host. They use fakes only: loopback HTTP
// servers for the iwdp listing and a fake xcrun first on PATH. They never
// contact a device, a real iwdp or a Simulator.

const simSetupScript = "../scripts/sim-setup.sh"

// The one Simulator of the fake simctl.
const fakeSimUDID = "BBBBBBBB-1111-2222-3333-444444444444"

// fakeXcrun is a stateful fake of the simctl commands that the script
// runs, for the one Simulator fakeSimUDID; $FAKE_SIM_STATE holds its
// state. It appends each argument list to $FAKE_XCRUN_LOG. "simctl boot"
// fails, as simctl does, unless the Simulator is Shutdown. Options:
// FAKE_OTHER_BOOTS_AFTER_LIST=n (another task boots it right after the
// n-th "simctl list"), FAKE_LIST_SLEEP (seconds a list takes), FAKE_BOOT_FAIL (boot
// fails, state unchanged) and
// FAKE_BOOTSTATUS_FAIL (the boot wait fails). "bootstatus -b" boots a
// Shutdown Simulator, as simctl does.
const fakeXcrun = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_XCRUN_LOG"
state=$(cat "$FAKE_SIM_STATE")
case "$1 $2" in
  'simctl list')
    if [ -n "${FAKE_LIST_SLEEP:-}" ]; then sleep "$FAKE_LIST_SLEEP"; fi
    printf '{"devices":{"com.apple.CoreSimulator.SimRuntime.iOS-18-0":[{"udid":"%s","name":"iPhone 16","state":"%s","isAvailable":true}]}}\n' "$FAKE_SIM_UDID" "$state"
    echo x >> "$FAKE_SIM_STATE.lists"
    if [ "$(wc -l < "$FAKE_SIM_STATE.lists")" -eq "${FAKE_OTHER_BOOTS_AFTER_LIST:-0}" ]; then echo Booted > "$FAKE_SIM_STATE"; fi ;;
  'simctl boot')
    if [ "$state" != Shutdown ]; then echo "Unable to boot device in current state: $state" >&2; exit 149; fi
    if [ -n "${FAKE_BOOT_FAIL:-}" ]; then echo "boot failed" >&2; exit 1; fi
    echo Booted > "$FAKE_SIM_STATE" ;;
  'simctl bootstatus')
    if [ -n "${FAKE_BOOTSTATUS_FAIL:-}" ]; then echo "bootstatus failed" >&2; exit 1; fi
    case " $* " in *' -b '*) if [ "$state" = Shutdown ]; then echo Booted > "$FAKE_SIM_STATE"; fi ;; esac ;;
  'simctl shutdown')
    if [ -n "${FAKE_BLOCK_SHUTDOWN:-}" ]; then
      touch "$FAKE_SIM_STATE.paused"
      while [ ! -e "$FAKE_SIM_STATE.resume" ]; do sleep 0.05; done
    fi
    echo Shutdown > "$FAKE_SIM_STATE" ;;
esac
`

// simScriptEnv returns the environment of one script run and its fake
// Simulator: a private state directory, the stateful fake xcrun
// (fakeXcrun) with the Simulator in state, and, first on PATH, guards
// that fail and log any call of ios_webkit_debug_proxy, lsof or curl, so
// that a script run never reaches a real iwdp. extra are more
// environment entries, such as the fake's options.
func simScriptEnv(t *testing.T, state string, extra ...string) (env []string, sim *fakeSim) {
	t.Helper()
	for _, tool := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("scripts/sim-setup.sh needs %s on PATH: %v", tool, err)
		}
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	stateDir := filepath.Join(dir, "state")
	for _, d := range []string{bin, stateDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sim = &fakeSim{log: filepath.Join(dir, "xcrun.log"), state: filepath.Join(dir, "sim-state"), stateDir: stateDir}
	if err := os.WriteFile(sim.state, []byte(state+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"xcrun": fakeXcrun}
	for _, guard := range []string{"ios_webkit_debug_proxy", "lsof", "curl"} {
		files[guard] = "#!/bin/sh\necho \"UNEXPECTED " + guard + " $*\" >> \"$FAKE_XCRUN_LOG\"\nexit 97\n"
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_RUNTIME_DIR="+stateDir, "TMPDIR="+stateDir,
		"FAKE_XCRUN_LOG="+sim.log, "FAKE_SIM_STATE="+sim.state, "FAKE_SIM_UDID="+fakeSimUDID,
	)
	return append(env, extra...), sim
}

// fakeSim is the fake Simulator of one test: its xcrun call log, its
// state file and the script's state directory.
type fakeSim struct{ log, state, stateDir string }

func (f *fakeSim) calls(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func (f *fakeSim) simState(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.state)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// runSimScript runs scripts/sim-setup.sh with args and env and returns
// its stdout, its stderr and its exit code.
func runSimScript(t *testing.T, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{simSetupScript}, args...)...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("running %s %v: %v", simSetupScript, args, err)
		}
	}
	return out.String(), errb.String(), cmd.ProcessState.ExitCode()
}

// fakeIWDPDevice is one device of a fake iwdp listing with its pages.
type fakeIWDPDevice struct {
	id    string
	pages []map[string]string
}

// fakeIWDPListing serves an iwdp listing, as iwdp 1.9.2 writes it
// (ios_webkit_debug_proxy.c: deviceId, deviceName, deviceOSVersion and
// url "host:port" per device; title, url, webSocketDebuggerUrl and appId
// per page), with one loopback server per device. It returns the
// listing's base URL.
func fakeIWDPListing(t *testing.T, devices ...fakeIWDPDevice) string {
	t.Helper()
	var entries []map[string]string
	for _, d := range devices {
		pages := d.pages
		if pages == nil {
			pages = []map[string]string{}
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/json" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(pages)
		}))
		t.Cleanup(srv.Close)
		entries = append(entries, map[string]string{
			"deviceId": d.id, "deviceName": d.id, "deviceOSVersion": "0.0.0",
			"url": strings.TrimPrefix(srv.URL, "http://"),
		})
	}
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(entries)
	}))
	t.Cleanup(listing.Close)
	return listing.URL
}

func fakePage(url, ws string) map[string]string {
	return map[string]string{"title": "Example Domain", "url": url, "webSocketDebuggerUrl": ws, "appId": "PID:1"}
}

// The page URL that a setup run opens: unique by its random query.
const simTestPageURL = "https://example.com/?iwdp-sim=0123456789abcdef0123456789abcdef"

// TestSimSetupSelectsOnlyTheSimulatorsTestPage: the setup selects only
// the page with the URL it opened on the SIMULATOR entry, never a page of
// another device or another Simulator tab (review F5).
func TestSimSetupSelectsOnlyTheSimulatorsTestPage(t *testing.T) {
	env, _ := simScriptEnv(t, "Shutdown")
	phone := fakeIWDPDevice{id: "00008030-PHONE", pages: []map[string]string{
		fakePage(simTestPageURL, "ws://localhost:9222/devtools/page/1"),
		fakePage("https://bank.example/", "ws://localhost:9222/devtools/page/2"),
	}}
	sim := fakeIWDPDevice{id: "SIMULATOR", pages: []map[string]string{
		fakePage("https://mail.example/", "ws://localhost:9223/devtools/page/1"),
		fakePage(simTestPageURL, "ws://localhost:9223/devtools/page/2"),
	}}
	// Both listing orders: the device, not its position, decides.
	for _, order := range [][]fakeIWDPDevice{{phone, sim}, {sim, phone}} {
		listing := fakeIWDPListing(t, order...)
		stdout, stderr, code := runSimScript(t, env, "--select-page", listing, simTestPageURL)
		if code != 0 || strings.TrimSpace(stdout) != "ws://localhost:9223/devtools/page/2" {
			t.Fatalf("listing %s then %s: selected %q (exit %d, stderr %q); want the Simulator's test page ws://localhost:9223/devtools/page/2", order[0].id, order[1].id, stdout, code, stderr)
		}
	}
}

// TestSimSetupRefusesAPhonePageWhileTheSimulatorListsNone: while the
// Simulator lists no page yet (Safari starts), the selection fails and
// prints no WebSocket URL, even when a phone lists pages (review F5).
func TestSimSetupRefusesAPhonePageWhileTheSimulatorListsNone(t *testing.T) {
	env, _ := simScriptEnv(t, "Shutdown")
	listing := fakeIWDPListing(t,
		fakeIWDPDevice{id: "SIMULATOR"},
		fakeIWDPDevice{id: "00008030-PHONE", pages: []map[string]string{fakePage("https://bank.example/", "ws://localhost:9223/devtools/page/1")}},
	)
	stdout, stderr, code := runSimScript(t, env, "--select-page", listing, simTestPageURL)
	if code == 0 || strings.Contains(stdout, "ws://") {
		t.Fatalf("selected %q with exit %d; want a failure without a WebSocket URL", stdout, code)
	}
	if !strings.Contains(stderr, simTestPageURL) {
		t.Errorf("stderr %q does not name the missing test page %s", stderr, simTestPageURL)
	}
}

// TestSimSetupRefusesAnAmbiguousTestPage: two Simulator pages with the
// test page's URL leave the target unknown, so the selection fails.
func TestSimSetupRefusesAnAmbiguousTestPage(t *testing.T) {
	env, _ := simScriptEnv(t, "Shutdown")
	listing := fakeIWDPListing(t, fakeIWDPDevice{id: "SIMULATOR", pages: []map[string]string{
		fakePage(simTestPageURL, "ws://localhost:9222/devtools/page/1"),
		fakePage(simTestPageURL, "ws://localhost:9222/devtools/page/2"),
	}})
	stdout, _, code := runSimScript(t, env, "--select-page", listing, simTestPageURL)
	if code == 0 || strings.Contains(stdout, "ws://") {
		t.Fatalf("selected %q with exit %d; want a failure for two matching pages", stdout, code)
	}
}

// TestSimTeardownShutsDownOnlyASimulatorThatSetupBooted: ownership comes
// only from setup's own successful "simctl boot". Teardown, also the one
// on a failed setup, shuts down that Simulator by its UDID and leaves
// every other one running: one that was booted before, one that another
// task booted between setup's list and its boot, and one whose boot
// failed (review F7 and round 3).
func TestSimTeardownShutsDownOnlyASimulatorThatSetupBooted(t *testing.T) {
	for _, tc := range []struct {
		name, state  string
		options      []string
		wantFail     bool // --boot-simulator exits non-zero
		wantShutdown bool
	}{
		{"already booted", "Booted", nil, false, false},
		{"booted by setup", "Shutdown", nil, false, true},
		{"booted by another task after the choice", "Shutdown", []string{"FAKE_OTHER_BOOTS_AFTER_LIST=1"}, false, false},
		{"booted by another task right before setup's boot", "Shutdown", []string{"FAKE_OTHER_BOOTS_AFTER_LIST=2"}, false, false},
		{"booted by setup, boot wait fails", "Shutdown", []string{"FAKE_BOOTSTATUS_FAIL=1"}, true, true},
		{"already booted, boot wait fails", "Booted", []string{"FAKE_BOOTSTATUS_FAIL=1"}, true, false},
		{"boot fails", "Shutdown", []string{"FAKE_BOOT_FAIL=1"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, sim := simScriptEnv(t, tc.state, tc.options...)
			_, stderr, code := runSimScript(t, env, "--boot-simulator")
			if (code != 0) != tc.wantFail {
				t.Fatalf("--boot-simulator exited %d; want failure %v; stderr: %s", code, tc.wantFail, stderr)
			}
			if code == 0 {
				if _, stderr, code := runSimScript(t, env, "--teardown"); code != 0 {
					t.Fatalf("--teardown exited %d: %s", code, stderr)
				}
			}
			calls := sim.calls(t)
			want := "simctl shutdown " + fakeSimUDID + "\n"
			shutdowns := strings.Count(calls, "simctl shutdown")
			switch {
			case tc.wantShutdown && (shutdowns != 1 || !strings.Contains(calls, want)):
				t.Fatalf("xcrun calls:\n%s\nwant exactly one %q", calls, want)
			case !tc.wantShutdown && shutdowns != 0:
				t.Fatalf("xcrun calls:\n%s\nwant no shutdown of a Simulator that setup did not boot", calls)
			}
			if strings.Contains(calls, "UNEXPECTED") {
				t.Fatalf("the script reached a guarded command:\n%s", calls)
			}
		})
	}
}

// TestSimSetupRefusesWhileAnEarlierSetupIsRecorded: a setup never tears
// down an earlier setup that is still recorded, since that run may still
// use its Simulator and iwdp. It exits non-zero, names make sim-teardown,
// and changes nothing; the earlier run's teardown still works (review
// round 3).
func TestSimSetupRefusesWhileAnEarlierSetupIsRecorded(t *testing.T) {
	env, sim := simScriptEnv(t, "Shutdown")
	if _, stderr, code := runSimScript(t, env, "--boot-simulator"); code != 0 {
		t.Fatalf("first setup exited %d: %s", code, stderr)
	}
	before := sim.calls(t)
	for _, args := range [][]string{{"--boot-simulator"}, {}} {
		stdout, stderr, code := runSimScript(t, env, args...)
		if code == 0 || !strings.Contains(stderr, "make sim-teardown") {
			t.Fatalf("setup %v with an earlier setup recorded: exit %d, stderr %q; want a failure that names make sim-teardown", args, code, stderr)
		}
		if strings.Contains(stdout, "IWDP_SIM_") {
			t.Fatalf("setup %v printed %q; want no variables", args, stdout)
		}
		if calls := sim.calls(t); calls != before {
			t.Fatalf("setup %v ran more commands after the refusal:\n%s", args, strings.TrimPrefix(calls, before))
		}
	}
	if got := sim.simState(t); got != "Booted" {
		t.Fatalf("the earlier setup's Simulator is %s; want it still Booted", got)
	}
	if _, stderr, code := runSimScript(t, env, "--teardown"); code != 0 {
		t.Fatalf("--teardown exited %d: %s", code, stderr)
	}
	if got := sim.simState(t); got != "Shutdown" {
		t.Fatalf("after the earlier setup's teardown the Simulator is %s; want Shutdown", got)
	}
}

// TestSimSetupTakesALeaseThatOneRunHolds: the setup state is a lease of
// one run. Of two setups that start together only one gets it; a lease
// without a device record still makes setup refuse; and teardown shuts
// down a recorded Simulator only when the lease holder's run wrote that
// record (review round 3).
func TestSimSetupTakesALeaseThatOneRunHolds(t *testing.T) {
	t.Run("two setups at once", func(t *testing.T) {
		env, sim := simScriptEnv(t, "Shutdown", "FAKE_LIST_SLEEP=1")
		type result struct {
			stderr string
			code   int
		}
		results := make(chan result, 2)
		for range 2 {
			go func() {
				cmd := exec.CommandContext(t.Context(), "bash", simSetupScript, "--boot-simulator")
				cmd.Env = env
				var errb bytes.Buffer
				cmd.Stderr = &errb
				_ = cmd.Run()
				results <- result{errb.String(), cmd.ProcessState.ExitCode()}
			}()
		}
		var ok, refused int
		for range 2 {
			r := <-results
			switch {
			case r.code == 0:
				ok++
			case strings.Contains(r.stderr, "make sim-teardown"):
				refused++
			default:
				t.Errorf("a setup exited %d: %s", r.code, r.stderr)
			}
		}
		if ok != 1 || refused != 1 {
			t.Fatalf("%d setups succeeded and %d refused; want one of each\nxcrun calls:\n%s", ok, refused, sim.calls(t))
		}
		if n := strings.Count(sim.calls(t), "simctl boot "); n != 1 {
			t.Fatalf("simctl boot ran %d times; want once (only the lease holder boots)", n)
		}
	})

	// Any recorded state, also without a lease (for example from an older
	// version of the script), belongs to a run that may still be going.
	for _, rec := range []struct{ file, text string }{
		{"lease", "0123456789abcdef0123456789abcdef"},
		{"device", fakeSimUDID + " booted"},
		{"iwdp.pid", "4242 0123456789abcdef0123456789abcdef Mon Nov 10 10:00:00 2026"},
	} {
		t.Run("a recorded "+rec.file+" alone", func(t *testing.T) {
			env, sim := simScriptEnv(t, "Shutdown")
			writeSimState(t, sim, rec.file, rec.text)
			if _, stderr, code := runSimScript(t, env, "--boot-simulator"); code == 0 || !strings.Contains(stderr, "make sim-teardown") {
				t.Fatalf("setup with a recorded %s: exit %d, stderr %q; want a refusal that names make sim-teardown", rec.file, code, stderr)
			}
			if calls := sim.calls(t); calls != "" {
				t.Fatalf("setup ran commands while an earlier run was recorded:\n%s", calls)
			}
		})
	}

	t.Run("a device record of another run", func(t *testing.T) {
		env, sim := simScriptEnv(t, "Shutdown")
		if _, stderr, code := runSimScript(t, env, "--boot-simulator"); code != 0 {
			t.Fatalf("setup exited %d: %s", code, stderr)
		}
		writeSimState(t, sim, "device", "ffffffffffffffffffffffffffffffff "+fakeSimUDID+" booted")
		if _, stderr, code := runSimScript(t, env, "--teardown"); code != 0 {
			t.Fatalf("--teardown exited %d: %s", code, stderr)
		}
		if calls := sim.calls(t); strings.Contains(calls, "simctl shutdown") {
			t.Fatalf("teardown shut down a Simulator that another run recorded:\n%s", calls)
		}
		if _, stderr, code := runSimScript(t, env, "--boot-simulator"); code != 0 {
			t.Fatalf("setup after the teardown released the lease exited %d: %s", code, stderr)
		}
	})
}

// writeSimState replaces the file name of the script's private state
// directory with text, mode 0600, as the script writes its state.
func writeSimState(t *testing.T, sim *fakeSim, name, text string) {
	t.Helper()
	dir := filepath.Join(sim.stateDir, "iwdp-sim-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSimTeardownAndSetupAreSerialized: a teardown holds the lock through
// its whole shutdown. A second teardown and a new setup that start while
// it shuts down wait for it, so the old teardown can never shut down or
// release the new run (review round 4, part-3 F7: lease-teardown-race.py).
func TestSimTeardownAndSetupAreSerialized(t *testing.T) {
	env, sim := simScriptEnv(t, "Shutdown")
	if _, stderr, code := runSimScript(t, env, "--boot-simulator"); code != 0 {
		t.Fatalf("first setup exited %d: %s", code, stderr)
	}
	resume := func() { _ = os.WriteFile(sim.state+".resume", nil, 0o600) }
	t.Cleanup(resume)
	start := func(extraEnv []string, args ...string) (*exec.Cmd, *bytes.Buffer) {
		cmd := exec.CommandContext(t.Context(), "bash", append([]string{simSetupScript}, args...)...)
		cmd.Env = append(append([]string{}, env...), extraEnv...)
		var errb bytes.Buffer
		cmd.Stderr = &errb
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd, &errb
	}
	a, aErr := start([]string{"FAKE_BLOCK_SHUTDOWN=1"}, "--teardown")
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for !cond() {
			if t.Context().Err() != nil {
				t.Fatalf("waiting for %s: %v", what, t.Context().Err())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("teardown A in its shutdown", func() bool { _, err := os.Stat(sim.state + ".paused"); return err == nil })

	// Teardown B and a new setup C start while A shuts down.
	b, bErr := start(nil, "--teardown")
	bDone := make(chan error, 1)
	go func() { bDone <- b.Wait() }()
	select {
	case err := <-bDone:
		resume()
		_ = a.Wait()
		t.Fatalf("teardown B ended (%v) while teardown A was still shutting down; want it to wait for the lock\n%s", err, bErr)
	case <-time.After(500 * time.Millisecond):
	}
	resume()
	if err := a.Wait(); err != nil {
		t.Fatalf("teardown A: %v\n%s", err, aErr)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("teardown B: %v\n%s", err, bErr)
	}
	if _, stderr, code := runSimScript(t, env, "--boot-simulator"); code != 0 {
		t.Fatalf("new setup C after both teardowns exited %d: %s", code, stderr)
	}
	if got := sim.simState(t); got != "Booted" {
		t.Fatalf("the new run's Simulator is %s; want Booted\n%s", got, sim.calls(t))
	}
	if n := strings.Count(sim.calls(t), "simctl shutdown"); n != 1 {
		t.Fatalf("simctl shutdown ran %d times; want once (teardown A only)\n%s", n, sim.calls(t))
	}
}

// TestSimSetupFailsClearlyOnAStaleLock: a lock that stays taken (a crashed
// setup or teardown) makes setup fail with advice after its wait, without
// any simctl call.
func TestSimSetupFailsClearlyOnAStaleLock(t *testing.T) {
	env, sim := simScriptEnv(t, "Shutdown", "IWDP_SIM_LOCK_WAIT_SECONDS=1")
	dir := filepath.Join(sim.stateDir, "iwdp-sim-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(filepath.Join(dir, "lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runSimScript(t, env, "--boot-simulator")
	if code == 0 || !strings.Contains(stderr, "stayed taken") || !strings.Contains(stderr, "rmdir") {
		t.Fatalf("setup with a stale lock: exit %d, stderr %q; want a failure that names the lock and rmdir", code, stderr)
	}
	if calls := sim.calls(t); calls != "" {
		t.Fatalf("setup ran commands without the lock:\n%s", calls)
	}
}
