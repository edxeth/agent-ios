package e2e_test

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The teardown of scripts/sim-setup.sh stops the iwdp that setup started
// only through 'scripts/iwdp-owner-check.sh --terminate', which checks the
// process from one read of the record and sends SIGTERM only to the
// process that it verified: it has the recorded start time, carries the
// recorded start token in its environment (read with exact boundaries:
// Linux /proc/<pid>/environ, macOS KERN_PROCARGS2) and runs
// ios_webkit_debug_proxy. These tests run on any host with fakes: a fake
// uname chooses the system, a fake ps answers the start time for one pid,
// and the recorded "iwdp" is a private Python probe of the test, never a
// real iwdp. With a fake "Darwin" on a Linux kernel the helper builds the
// KERN_PROCARGS2 buffer from the probe's real /proc files.

// ownerProbe is a process that stands in for a recorded iwdp. On SIGTERM
// it creates the file sys.argv[1] and exits when sys.argv[2] is "exit";
// it keeps running when it is "ignore". Its later arguments are markers
// that no output may show.
const ownerProbe = `import signal, sys, time
marker, mode = sys.argv[1], sys.argv[2]
def on_term(signum, frame):
    open(marker, "w").close()
    if mode == "exit":
        sys.exit(0)
signal.signal(signal.SIGTERM, on_term)
print("ready", flush=True)
while True:
    time.sleep(60)
`

// swapRecordOnce, the body of a shell function, replaces the record
// $FAKE_SWAP_RECORD with the file $FAKE_SWAP_TO once (when both are set):
// another program of this user rewrites the record while a check runs.
// The fake ps runs it at its first call; the fake uname runs it first when
// FAKE_SWAP_AT_UNAME=1, which is right after the helper read the record
// and before it checks that the process exists.
const swapRecordOnce = `if [ -n "${FAKE_SWAP_TO:-}" ] && [ ! -e "$FAKE_SWAP_TO.done" ]; then
  : > "$FAKE_SWAP_TO.done"
  cat "$FAKE_SWAP_TO" > "$FAKE_SWAP_RECORD"
fi`

// fakePS answers "ps -p PID -o lstart=" with $FAKE_PS_LSTART and
// "ps [-E] -ww -o command= -p PID" with $FAKE_PS_COMMAND, followed with
// -E by $FAKE_PS_ENV (what macOS ps -E prints), for the one pid
// $FAKE_PS_PID; the helper reads only the start time from ps, the earlier
// helpers also the rest. For another pid, another field or with
// FAKE_PS_FAIL it prints nothing and fails, as ps does for a process it
// cannot read. It logs its arguments to $FAKE_XCRUN_LOG, and its first
// call runs swapRecordOnce.
const fakePS = `#!/bin/sh
printf 'ps %s\n' "$*" >> "$FAKE_XCRUN_LOG"
` + swapRecordOnce + `
if [ -n "${FAKE_PS_FAIL:-}" ]; then exit 1; fi
pid="" field="" env=""
while [ $# -gt 0 ]; do
  case "$1" in
    -p) pid=$2; shift ;;
    -o) field=$2; shift ;;
    -E) env=1 ;;
  esac
  shift
done
[ "$pid" = "$FAKE_PS_PID" ] || exit 1
case "$field" in
  lstart=) printf '%s\n' "$FAKE_PS_LSTART" ;;
  command=)
    if [ -z "$env" ]; then printf '%s\n' "$FAKE_PS_COMMAND"
    else printf '%s %s\n' "$FAKE_PS_COMMAND" "$FAKE_PS_ENV"; fi ;;
  *) exit 1 ;;
esac
`

// bashEnvSwap is a BASH_ENV file: the earlier helper ran "kill -0"
// between its reads of the record, and there it runs swapRecordOnce.
const bashEnvSwap = `kill() {
  if [ "${1:-}" = -0 ]; then
` + swapRecordOnce + `
  fi
  builtin kill "$@"
}
`

// forgetRaceSite is a sitecustomize.py for the helper's Python process:
// when --forget moves the record aside (an os.rename to a ".forget-"
// name), another program of this user first rewrites the record in place
// with forgetRaceChanged, so it no longer matches the read, and right
// after the move writes a new record forgetRaceNew at the path. --forget
// must then keep the changed record under its private name.
const forgetRaceSite = `import os
_rename = os.rename
def rename(src, dst, *a, **kw):
    if ".forget-" in str(dst):
        with open(src, "r+b") as f:
            f.truncate(0)
            f.write(b"4242 changed-token Mon Nov 10 10:00:00 2026\n")
        _rename(src, dst, *a, **kw)
        with open(src, "xb") as f:
            f.write(b"4343 new-token Mon Nov 10 10:00:00 2026\n")
        return
    _rename(src, dst, *a, **kw)
os.rename = rename
`

// The contents that forgetRaceSite writes.
const (
	forgetRaceChanged = "4242 changed-token Mon Nov 10 10:00:00 2026\n"
	forgetRaceNew     = "4343 new-token Mon Nov 10 10:00:00 2026\n"
)

// forgetRaceEnv writes forgetRaceSite and returns the PYTHONPATH entry
// that loads it into the helper's Python process.
func forgetRaceEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sitecustomize.py"), []byte(forgetRaceSite), 0o600); err != nil {
		t.Fatal(err)
	}
	return "PYTHONPATH=" + dir
}

// What setup records for its iwdp and what ps shows for the probe.
const (
	recordedIWDPStart = "Mon Nov 10 10:00:00 2026"
	otherIWDPStart    = "Mon Nov 10 11:30:00 2026"
	iwdpProgram       = "ios_webkit_debug_proxy"
	simTokenEnv       = "AGENT_IOS_SIM_START_TOKEN"
)

// randomHex returns n random bytes in hex: a start token, or a marker
// that no output may show.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// probeSpec is the program (first argument), the extra arguments, the
// environment (besides PATH) and the SIGTERM behavior of a probe.
type probeSpec struct {
	argv0      string
	args, env  []string
	exitOnTerm bool
}

// probeProcess is one running ownerProbe.
type probeProcess struct {
	pid    int
	marker string        // the argument that marks its command line
	argv   []string      // its arguments, from argv0 on
	env    []string      // its environment
	signal string        // the file that its SIGTERM handler creates
	exited chan struct{} // closed when it has exited and was reaped
}

// signaled reports whether the probe got SIGTERM.
func (p *probeProcess) signaled() bool {
	_, err := os.Stat(p.signal)
	return err == nil
}

// awaitSignal reports whether the probe got SIGTERM, for a caller that
// expects it: the probe's handler runs after the signal arrives, so its
// file can appear after the sender has returned. It waits until the file
// exists, the probe exits, one of the probes others got SIGTERM instead,
// or the test's deadline passes.
func (p *probeProcess) awaitSignal(t *testing.T, others ...*probeProcess) bool {
	t.Helper()
	deadline, hasDeadline := t.Deadline()
	for !p.signaled() {
		if hasDeadline && time.Now().After(deadline) {
			return false
		}
		for _, o := range others {
			if o != nil && o.signaled() {
				return p.signaled()
			}
		}
		select {
		case <-p.exited:
			return p.signaled()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return true
}

// awaitExit reports whether the probe has exited, for a caller that sent
// it SIGTERM and expects it to exit: it waits until it has exited and was
// reaped, or the test's deadline passes.
func (p *probeProcess) awaitExit(t *testing.T) bool {
	t.Helper()
	var timeout <-chan time.Time
	if deadline, ok := t.Deadline(); ok {
		timeout = time.After(time.Until(deadline))
	}
	select {
	case <-p.exited:
		return true
	case <-timeout:
		return false
	}
}

// kill ends the probe with SIGKILL and waits until it is reaped: its pid
// is then gone.
func (p *probeProcess) kill(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-p.exited
}

// startOwnerProbe starts ownerProbe as spec says, with the environment
// PATH and spec.env. The process is reaped as soon as it exits, so an exit
// is visible as a missing pid.
func startOwnerProbe(t *testing.T, spec probeSpec) *probeProcess {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("the teardown tests need python3: %v", err)
	}
	mode := "ignore"
	if spec.exitOnTerm {
		mode = "exit"
	}
	argv0 := spec.argv0
	if argv0 == "" {
		argv0 = "python3"
	}
	p := &probeProcess{marker: "FOREIGN_CMDLINE_" + randomHex(t, 8), signal: filepath.Join(t.TempDir(), "got-term"), exited: make(chan struct{})}
	p.argv = append([]string{argv0, "-c", ownerProbe, p.signal, mode, p.marker}, spec.args...)
	// A small environment of its own: a failing check may print it.
	p.env = append([]string{"PATH=" + os.Getenv("PATH")}, spec.env...)
	cmd := &exec.Cmd{Path: python, Args: p.argv, Env: p.env, SysProcAttr: probeProcAttr()}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // by its pid only; an error means it has exited
		<-p.exited
	})
	if err != nil || line != "ready\n" {
		t.Fatalf("the probe did not start: %q, %v", line, err)
	}
	p.pid = cmd.Process.Pid
	return p
}

// ownerWorld is what the fakes show for the probe: the system that the
// fake uname reports, the start time that ps shows (psFail: ps fails),
// and whether the user may signal it (notPermitted: the helper's test
// seam, which can only keep a signal from being sent).
type ownerWorld struct {
	system       string // Linux or Darwin
	psStart      string
	psFail       bool
	notPermitted bool
}

// ownerRun is one prepared teardown: the script's environment, its fake
// Simulator and the path of the iwdp record.
type ownerRun struct {
	env    []string
	sim    *fakeSim
	record string
}

// prepareOwnerRun writes the fakes of w for the probe p and the record
// text, and returns the run. extra are more environment entries.
func prepareOwnerRun(t *testing.T, w ownerWorld, p *probeProcess, record string, extra ...string) ownerRun {
	t.Helper()
	env := []string{
		"FAKE_PS_PID=" + strconv.Itoa(p.pid), "FAKE_PS_LSTART=" + w.psStart,
		"FAKE_PS_COMMAND=" + strings.Join(p.argv, " "), "FAKE_PS_ENV=" + strings.Join(p.env, " "),
		"AGENT_IOS_SIM_STOP_WAIT_SECONDS=1",
	}
	if w.psFail {
		env = append(env, "FAKE_PS_FAIL=1")
	}
	if w.notPermitted {
		env = append(env, "AGENT_IOS_OWNER_CHECK_TEST_NOT_PERMITTED=1")
	}
	full, sim := simScriptEnv(t, "Shutdown", append(env, extra...)...)
	dir := filepath.Dir(sim.stateDir)
	bashEnv := filepath.Join(dir, "bash-env.sh")
	for name, content := range map[string]string{
		filepath.Join(dir, "bin", "ps"):    fakePS,
		filepath.Join(dir, "bin", "uname"): "#!/bin/sh\nif [ \"${FAKE_SWAP_AT_UNAME:-}\" = 1 ]; then\n" + swapRecordOnce + "\nfi\necho " + w.system + "\n",
		bashEnv:                            bashEnvSwap,
	} {
		if err := os.WriteFile(name, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeSimState(t, sim, "iwdp.pid", record)
	r := ownerRun{record: filepath.Join(sim.stateDir, "iwdp-sim-"+strconv.Itoa(os.Getuid()), "iwdp.pid"), sim: sim}
	r.env = append(full, "BASH_ENV="+bashEnv, "FAKE_SWAP_RECORD="+r.record)
	return r
}

// recordKept reports whether the iwdp record still exists.
func (r ownerRun) recordKept(t *testing.T) bool {
	t.Helper()
	_, err := os.Lstat(r.record)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// simRecord is the record text: setup's first line "<pid> <token>
// <start>"; command, when not empty, is the second line, the command line
// that the version before the identity check recorded (iwdp-owner-check.sh
// ignores it; that version compared it on macOS).
func simRecord(pid int, token, start, command string) string {
	s := fmt.Sprintf("%d %s %s", pid, token, start)
	if command != "" {
		s += "\n" + command
	}
	return s
}

// wrapNestedBash puts a "bash" first on the run's PATH that runs the real
// bash and, after the first run that exits 0 (the helper's "verified"),
// replaces the record (its last argument) with the file $SYNTH_SWAP_TO:
// another program of this user rewrites the record right after the check
// (record-swap-race.sh, review WS3 round 4).
func (r *ownerRun) wrapNestedBash(t *testing.T, text string) {
	t.Helper()
	realBash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "swapped-record")
	if err := os.WriteFile(path, []byte(text+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!" + realBash + `
out=$("` + realBash + `" "$@")
rc=$?
if [ "$rc" -eq 0 ] && [ ! -e "$SYNTH_SWAP_TO.done" ]; then
  : > "$SYNTH_SWAP_TO.done"
  for last; do :; done
  cat "$SYNTH_SWAP_TO" > "$last"
fi
printf '%s\n' "$out"
exit "$rc"
`
	bin := filepath.Join(filepath.Dir(r.sim.stateDir), "bin", "bash")
	if err := os.WriteFile(bin, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	r.env = append(r.env, "SYNTH_SWAP_TO="+path)
}

// TestSimTeardownStopsOnlyAVerifiedIWDP: teardown sends SIGTERM only to a
// process that iwdp-owner-check.sh verifies, removes the record only when
// that process is gone, keeps it (with a warning) in every other case,
// and never prints another process's command line or a token. The cases
// named after a file are the reviewers' repros of WS3 rounds 3 and 4
// (/home/devkit/.pi/artifacts/reviewer/ws3-r1-r3, ws3-r3-repro, ws3-r1-r4
// and ws3-r4-repro).
func TestSimTeardownStopsOnlyAVerifiedIWDP(t *testing.T) {
	type teardownCase struct {
		name         string
		probe        probeSpec // {token} in env and args is the recorded token
		world        ownerWorld
		gone         bool // the probe has exited before the teardown
		oldCommand   bool // the record has the probe's command line as line 2
		swapAfter    bool // the record names a decoy right after the check
		swapAtRead   bool // the record names a live decoy right after the helper read it
		keptAside    bool // while --forget compares the record, it changes and a new record takes its path (forgetRaceSite)
		wantSignal   bool
		wantKept     bool
		wantStderr   string
		onLinuxProcs bool // reads /proc of the probe as Linux: Linux hosts only
	}
	darwin := ownerWorld{system: "Darwin", psStart: recordedIWDPStart}
	linux := ownerWorld{system: "Linux", psStart: recordedIWDPStart}
	token := []string{simTokenEnv + "={token}"}
	cases := []teardownCase{
		{
			name: "macOS: verified iwdp that exits on SIGTERM", world: darwin,
			probe:      probeSpec{argv0: iwdpProgram, env: token, exitOnTerm: true},
			wantSignal: true, wantStderr: "Stopped the ios_webkit_debug_proxy",
		},
		{
			// darwin-forged-record-repro.sh: a record, which another
			// program of this user can write, names a live unrelated
			// process with its real start time and command line.
			name: "macOS: forged record of an unrelated process (darwin-forged-record-repro.sh)", world: darwin, oldCommand: true,
			probe:    probeSpec{argv0: "sleep", exitOnTerm: true},
			wantKept: true, wantStderr: "does not carry the recorded start token",
		},
		{
			// sim-teardown-identity.py: a pid reused in the same second by
			// another iwdp with the same command line, without the token.
			name: "macOS: same-second reuse by the same command without the token (sim-teardown-identity.py)", world: darwin, oldCommand: true,
			probe:    probeSpec{argv0: iwdpProgram, exitOnTerm: true},
			wantKept: true, wantStderr: "does not carry the recorded start token",
		},
		{
			// darwin-argv-spoof.sh and darwin-argument-token.py: the token
			// text is only an argument; ps -E printed it like an
			// environment entry.
			name: "macOS: the token only in an argument (darwin-argv-spoof.sh, darwin-argument-token.py)", world: darwin,
			probe:    probeSpec{argv0: iwdpProgram, args: token, exitOnTerm: true},
			wantKept: true, wantStderr: "does not carry the recorded start token",
		},
		{
			// teardown-command-disclosure.sh: the pid now runs another
			// command whose arguments hold a private value.
			name: "macOS: another command line is never printed (teardown-command-disclosure.sh)", world: darwin,
			probe:    probeSpec{args: []string{"SYNTHETIC_PRIVATE_MARKER"}, exitOnTerm: true},
			wantKept: true, wantStderr: "left running",
		},
		{
			// teardown-live-after-signal.sh: a verified iwdp that ignores
			// SIGTERM keeps its record.
			name: "macOS: verified iwdp that ignores SIGTERM (teardown-live-after-signal.sh)", world: darwin,
			probe:      probeSpec{argv0: iwdpProgram, env: token},
			wantSignal: true, wantKept: true, wantStderr: "did not exit within 1 s",
		},
		{
			// record-swap-race.sh: the record names an unrelated process
			// right after the check; only the verified process gets
			// SIGTERM (the decoy's signal is checked below).
			name: "macOS: record replaced right after the check (record-swap-race.sh)", world: darwin, swapAfter: true,
			probe:      probeSpec{argv0: iwdpProgram, env: token, exitOnTerm: true},
			wantSignal: true, wantKept: true, wantStderr: "Its record stays",
		},
		{
			// teardown-inaccessible-repro.sh: a live process that this user
			// may not signal (a pid that another user's process reuses).
			name:     "a live process this user may not signal keeps its record (teardown-inaccessible-repro.sh)",
			world:    ownerWorld{system: "Darwin", psStart: recordedIWDPStart, notPermitted: true},
			probe:    probeSpec{argv0: iwdpProgram, env: token, exitOnTerm: true},
			wantKept: true, wantStderr: "may not signal it",
		},
		{
			name: "macOS: reused pid with another start time", world: ownerWorld{system: "Darwin", psStart: otherIWDPStart},
			probe:    probeSpec{argv0: iwdpProgram, env: token, exitOnTerm: true},
			wantKept: true, wantStderr: "another time than the recorded one",
		},
		{
			name: "macOS: token holder that is not iwdp", world: darwin,
			probe:    probeSpec{env: token, exitOnTerm: true},
			wantKept: true, wantStderr: "is not ios_webkit_debug_proxy",
		},
		{
			name: "macOS: unreadable process", world: ownerWorld{system: "Darwin", psFail: true},
			probe:    probeSpec{argv0: iwdpProgram, env: token, exitOnTerm: true},
			wantKept: true, wantStderr: "cannot be read",
		},
		{
			name: "the recorded process is gone", world: darwin, gone: true,
			probe: probeSpec{argv0: iwdpProgram, env: token, exitOnTerm: true},
		},
		{
			// Review WS3 round 5, r2: teardown removed the record after
			// "gone" without checking that the path still named the record
			// that the check had read.
			name: "a gone iwdp's record replaced after the read by a live decoy's record", world: darwin, gone: true, swapAtRead: true,
			probe:    probeSpec{argv0: iwdpProgram, env: token, exitOnTerm: true},
			wantKept: true, wantStderr: "was not removed",
		},
		{
			// Review WS3 round 6 (rename reviewer): the changed record that
			// --forget keeps under a private name is not checked by the
			// next teardown; the warning names it and says what to do.
			name: "a gone iwdp's record kept under a private name", world: darwin, gone: true, keptAside: true,
			probe:    probeSpec{argv0: iwdpProgram, env: token, exitOnTerm: true},
			wantKept: true, wantStderr: "The next teardown does not check that file, and the next setup does not refuse because of it: check it by hand as docs/device-runbook.md (Stopping an iwdp by hand) describes",
		},
		{
			name: "Linux: verified iwdp that exits on SIGTERM", world: linux, onLinuxProcs: true,
			probe:      probeSpec{argv0: iwdpProgram, env: token, exitOnTerm: true},
			wantSignal: true, wantStderr: "Stopped the ios_webkit_debug_proxy",
		},
		{
			name: "Linux: verified iwdp that ignores SIGTERM (teardown-live-after-signal.sh)", world: linux, onLinuxProcs: true,
			probe:      probeSpec{argv0: iwdpProgram, env: token},
			wantSignal: true, wantKept: true, wantStderr: "did not exit within 1 s",
		},
		{
			name: "Linux: record replaced right after the check (record-swap-race.sh)", world: linux, onLinuxProcs: true, swapAfter: true,
			probe:      probeSpec{argv0: iwdpProgram, env: token, exitOnTerm: true},
			wantSignal: true, wantKept: true, wantStderr: "Its record stays",
		},
		{
			name: "Linux: no start token", world: linux, onLinuxProcs: true,
			probe:    probeSpec{argv0: iwdpProgram, exitOnTerm: true},
			wantKept: true, wantStderr: "does not carry the recorded start token",
		},
		{
			name: "Linux: token holder that is not iwdp", world: linux, onLinuxProcs: true,
			probe:    probeSpec{env: token, exitOnTerm: true},
			wantKept: true, wantStderr: "is not ios_webkit_debug_proxy",
		},
	}
	for _, tc := range cases {
		// The helper reads the probe's /proc files for Linux and, under a
		// fake "Darwin" on a Linux kernel, to build KERN_PROCARGS2; on a
		// macOS host only the macOS cases run, through sysctl.
		if tc.onLinuxProcs && runtime.GOOS != "linux" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			tok := randomHex(t, 16)
			fill := func(items []string) []string {
				out := make([]string, len(items))
				for i, s := range items {
					out[i] = strings.ReplaceAll(s, "{token}", tok)
				}
				return out
			}
			spec := tc.probe
			spec.env, spec.args = fill(spec.env), fill(spec.args)
			probe := startOwnerProbe(t, spec)
			command := ""
			if tc.oldCommand {
				command = strings.Join(probe.argv, " ")
			}
			run := prepareOwnerRun(t, tc.world, probe, simRecord(probe.pid, tok, recordedIWDPStart, command))
			var decoy *probeProcess
			if tc.swapAfter {
				decoy = startOwnerProbe(t, probeSpec{exitOnTerm: true})
				run.wrapNestedBash(t, simRecord(decoy.pid, tok, recordedIWDPStart, ""))
			}
			var decoyRecord string
			if tc.swapAtRead {
				decoy = startOwnerProbe(t, probeSpec{argv0: iwdpProgram, env: []string{simTokenEnv + "=" + tok}, exitOnTerm: true})
				decoyRecord = simRecord(decoy.pid, tok, recordedIWDPStart, "")
				swapTo := filepath.Join(t.TempDir(), "decoy-record")
				if err := os.WriteFile(swapTo, []byte(decoyRecord+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				run.env = append(run.env, "FAKE_SWAP_TO="+swapTo, "FAKE_SWAP_AT_UNAME=1")
			}
			if tc.keptAside {
				run.env = append(run.env, forgetRaceEnv(t))
			}
			if tc.gone {
				probe.kill(t)
			}

			_, stderr, code := runSimScript(t, run.env, "--teardown")
			if code != 0 {
				t.Fatalf("--teardown exited %d: %s", code, stderr)
			}
			if decoy != nil && decoy.signaled() {
				t.Fatalf("teardown signaled the decoy that the record named after the check; stderr: %s", stderr)
			}
			if got := probe.signaled(); got != tc.wantSignal {
				t.Fatalf("teardown signaled the probe: %v; want %v; stderr: %s", got, tc.wantSignal, stderr)
			}
			if got := run.recordKept(t); got != tc.wantKept {
				t.Fatalf("record kept: %v; want %v; stderr: %s", got, tc.wantKept, stderr)
			}
			if got, err := os.ReadFile(run.record); tc.swapAtRead && (err != nil || strings.TrimSpace(string(got)) != decoyRecord) {
				t.Fatalf("the record holds %q (%v); want the decoy's record; stderr: %s", got, err, stderr)
			}
			if !strings.Contains(stderr, tc.wantStderr) {
				t.Fatalf("stderr %q does not say %q", stderr, tc.wantStderr)
			}
			if stays := "stays in " + run.record + "; the next teardown checks it again"; tc.wantKept && !strings.Contains(stderr, stays) {
				t.Fatalf("stderr %q does not say %q", stderr, stays)
			}
			if tc.keptAside {
				aside, err := filepath.Glob(filepath.Join(filepath.Dir(run.record), ".iwdp.pid.forget-*"))
				if err != nil || len(aside) != 1 {
					t.Fatalf("private files %v (%v); want one", aside, err)
				}
				if got, err := os.ReadFile(aside[0]); err != nil || string(got) != forgetRaceChanged || !strings.Contains(stderr, "it was kept as "+aside[0]+")") {
					t.Fatalf("%s holds %q (%v); want the changed record, named in stderr %q", aside[0], got, err, stderr)
				}
				if got, err := os.ReadFile(run.record); err != nil || string(got) != forgetRaceNew {
					t.Fatalf("the record holds %q (%v); want the new record", got, err)
				}
			}
			for _, secret := range []string{tok, probe.marker, "SYNTHETIC_PRIVATE_MARKER"} {
				if strings.Contains(stderr, secret) {
					t.Fatalf("teardown printed a token or another process's command line (%q): %s", secret, stderr)
				}
			}
			calls := run.sim.calls(t)
			if strings.Contains(calls, tok) {
				t.Fatalf("a command got the start token in its arguments:\n%s", calls)
			}
			if strings.Contains(calls, "UNEXPECTED") {
				t.Fatalf("the script reached a guarded command:\n%s", calls)
			}
		})
	}
}

// TestSimSetupReplacesStateAtomically: a state write that fails (here
// with a file-size limit of 0) keeps the old record. The old write_state
// removed the record before it wrote the new one
// (record-rewrite-failure.sh, review WS3 round 3 F6).
func TestSimSetupReplacesStateAtomically(t *testing.T) {
	script, err := os.ReadFile(simSetupScript)
	if err != nil {
		t.Fatal(err)
	}
	fn := ""
	if _, after, ok := strings.Cut(string(script), "\nwrite_state() {\n"); ok {
		if body, _, ok := strings.Cut(after, "\n}\n"); ok {
			fn = "write_state() {\n" + body + "\n}\n"
		}
	}
	if fn == "" {
		t.Fatalf("%s has no function write_state", simSetupScript)
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "write_state.sh")
	if err := os.WriteFile(lib, []byte(fn), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "iwdp.pid")
	const old = "4242 0123456789abcdef0123456789abcdef Mon Nov 10 10:00:00 2026\n"
	if err := os.WriteFile(record, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	write := func(limit string) error {
		cmd := exec.CommandContext(t.Context(), "bash", "-c", `trap 'exit 1' XFSZ; ulimit -f "$3"; source "$1"; write_state "$2" "4243 ffffffffffffffffffffffffffffffff Tue Nov 11 10:00:00 2026"`, "_", lib, record, limit)
		return cmd.Run()
	}
	if err := write("0"); err == nil {
		t.Fatal("write_state with a file-size limit of 0 succeeded; want a failure")
	}
	if got, err := os.ReadFile(record); err != nil || string(got) != old {
		t.Fatalf("after a failed write the record holds %q (%v); want the old record %q", got, err, old)
	}

	if err := write("unlimited"); err != nil {
		t.Fatalf("write_state: %v", err)
	}
	got, err := os.ReadFile(record)
	if err != nil || string(got) != "4243 ffffffffffffffffffffffffffffffff Tue Nov 11 10:00:00 2026\n" {
		t.Fatalf("after a write the record holds %q (%v)", got, err)
	}
	if fi, err := os.Stat(record); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the record's mode: %v (%v); want 0600", fi.Mode().Perm(), err)
	}
}
