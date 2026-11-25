package e2e_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/quick"
)

const ownerCheckScript = "../scripts/iwdp-owner-check.sh"

// runOwnerCheck runs scripts/iwdp-owner-check.sh with args and env and
// returns its stdout, its stderr and its exit code.
func runOwnerCheck(t *testing.T, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{ownerCheckScript}, args...)...)
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("running %s: %v", ownerCheckScript, err)
		}
	}
	return out.String(), errb.String(), cmd.ProcessState.ExitCode()
}

// noPidfdSite is a sitecustomize.py for the helper's Python process. It
// removes the functions that $OWNER_CHECK_REMOVE names (a comma-separated
// subset of os.pidfd_open and signal.pidfd_send_signal; the standalone
// Python builds that uv and mise install lack both on Linux, review WS3
// round 8), and logs to $OWNER_CHECK_CALL_LOG, one JSON array per
// line, which of the two functions the process has after the removal
// (["has", os.pidfd_open, signal.pidfd_send_signal], once per process:
// such a Python may lack a function that the test did not remove), every
// os.kill (its audit event: ["kill", pid, signal]) and every
// call of a libc function through ctypes (["ctypes", name, arguments...],
// an argument that is not an integer as null). Three seams make the
// fallback fail or choose its path: with $OWNER_CHECK_PIDFD_OPEN_ERRNO the
// pidfd_open system call (434) is not made and fails with that errno, with
// $OWNER_CHECK_MACHINE os.uname reports that machine, and with
// $OWNER_CHECK_POINTER_SIZE ctypes reports that size for a pointer
// (c_void_p).
const noPidfdSite = `import ctypes, json, os, signal, sys
_remove = os.environ["OWNER_CHECK_REMOVE"].split(",")
for module, name in ((os, "pidfd_open"), (signal, "pidfd_send_signal")):
    if name in _remove and hasattr(module, name):
        delattr(module, name)
_log_path = os.environ["OWNER_CHECK_CALL_LOG"]
_open_errno = int(os.environ.get("OWNER_CHECK_PIDFD_OPEN_ERRNO") or 0)
_machine = os.environ.get("OWNER_CHECK_MACHINE")
if _machine:
    _uname = os.uname
    def _fake_uname():
        u = _uname()
        return type(u)((u.sysname, u.nodename, u.release, u.version, _machine))
    os.uname = _fake_uname
_pointer_size = int(os.environ.get("OWNER_CHECK_POINTER_SIZE") or 0)
if _pointer_size:
    _sizeof = ctypes.sizeof
    ctypes.sizeof = lambda t: _pointer_size if t is ctypes.c_void_p else _sizeof(t)
def _log(entry):
    with open(_log_path, "a") as f:
        f.write(json.dumps(entry) + "\n")
_log(["has", hasattr(os, "pidfd_open"), hasattr(signal, "pidfd_send_signal")])
def _audit(event, args):
    if event == "os.kill":
        _log(["kill"] + [int(a) for a in args])
sys.addaudithook(_audit)
class _Logged:
    def __init__(self, name, func):
        object.__setattr__(self, "_name", name)
        object.__setattr__(self, "_func", func)
    def __setattr__(self, key, value):
        setattr(self._func, key, value)
    def __getattr__(self, key):
        return getattr(self._func, key)
    def __call__(self, *args):
        _log(["ctypes", self._name] + [a if isinstance(a, int) else None for a in args])
        if _open_errno and self._name == "syscall" and args[:1] == (434,):
            ctypes.set_errno(_open_errno)
            return -1
        return self._func(*args)
class _CDLL(ctypes.CDLL):
    def __getitem__(self, name):
        return _Logged(name, super().__getitem__(name))
ctypes.CDLL = _CDLL
`

// pidfdCalls is what the helper did under noPidfdSite: the signals that
// it sent by pid with os.kill, the pids of its pidfd_open system calls
// (434) and the signals of its pidfd_send_signal system calls (424). The
// Python processes logged their functions procs times; hasOpen and
// hasSend say whether they had os.pidfd_open and
// signal.pidfd_send_signal after the removal.
type pidfdCalls struct {
	kills, opens, sends []int
	procs               int
	hasOpen, hasSend    bool
}

// pidfdRemoved names the pidfd functions that noPidfdSite removes. The
// helper chooses per function: each missing one (removed, or missing in
// this Python, as in the uv and mise builds) takes its system call
// fallback, each present one is used as it is.
type pidfdRemoved struct{ open, send bool }

// The Pythons of the tests: without both functions (the uv and mise
// builds) and each mixed case.
var (
	removeBoth     = pidfdRemoved{open: true, send: true}
	removeOpenOnly = pidfdRemoved{open: true}
	removeSendOnly = pidfdRemoved{send: true}
)

func (r pidfdRemoved) String() string {
	var names []string
	if r.open {
		names = append(names, "pidfd_open")
	}
	if r.send {
		names = append(names, "pidfd_send_signal")
	}
	return strings.Join(names, ",")
}

// withoutPidfdFunctions writes noPidfdSite and returns the environment
// entries that load it into the helper's Python process with the
// functions of removed taken away, and a function that reads the calls
// that it logged.
func withoutPidfdFunctions(t *testing.T, removed pidfdRemoved) (env []string, calls func() pidfdCalls) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sitecustomize.py"), []byte(noPidfdSite), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls.log")
	return []string{"PYTHONPATH=" + dir, "OWNER_CHECK_CALL_LOG=" + log, "OWNER_CHECK_REMOVE=" + removed.String()}, func() pidfdCalls {
		t.Helper()
		data, err := os.ReadFile(log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		const sysPidfdOpen, sysPidfdSendSignal = 434, 424 // the Linux system call numbers
		var c pidfdCalls
		for line := range strings.Lines(string(data)) {
			var entry []any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("the call log line %q: %v", line, err)
			}
			n := func(i int) int {
				if i >= len(entry) {
					t.Fatalf("the call log line %q has no argument %d", line, i)
				}
				f, ok := entry[i].(float64)
				if !ok {
					t.Fatalf("the call log line %q: argument %d is not an integer", line, i)
				}
				return int(f)
			}
			switch {
			case entry[0] == "has":
				var hasOpen, hasSend, ok1, ok2 bool
				if len(entry) == 3 {
					hasOpen, ok1 = entry[1].(bool)
					hasSend, ok2 = entry[2].(bool)
				}
				if !ok1 || !ok2 {
					t.Fatalf("the call log line %q: want [\"has\", <bool>, <bool>]", line)
				}
				if c.procs > 0 && (hasOpen != c.hasOpen || hasSend != c.hasSend) {
					t.Fatalf("the call log line %q: the helper's Python processes disagree on their pidfd functions (before: os.pidfd_open %v, signal.pidfd_send_signal %v)", line, c.hasOpen, c.hasSend)
				}
				c.procs++
				c.hasOpen, c.hasSend = hasOpen, hasSend
			case entry[0] == "kill":
				c.kills = append(c.kills, n(2))
			case entry[0] == "ctypes" && entry[1] == "syscall" && n(2) == sysPidfdOpen:
				c.opens = append(c.opens, n(3))
			case entry[0] == "ctypes" && entry[1] == "syscall" && n(2) == sysPidfdSendSignal:
				c.sends = append(c.sends, n(4))
			}
		}
		return c
	}
}

// checkPidfdCalls checks the calls c of the helper under noPidfdSite with
// the functions of removed taken away, on the record of process pid, after
// the result word. The helper's Python logged its functions, and each
// function of removed is missing there. The expected path of each function
// comes from that log, not from removed: a Python may lack a function by
// itself (the uv and mise builds lack both). On a Linux kernel (the
// kernel decides, as the helper's header documents) it never signals by
// pid. Without os.pidfd_open, when it reached the process (verified,
// mismatch or gone), it opened one pidfd of pid with the system call;
// with it, it made no pidfd_open system call. Without
// signal.pidfd_send_signal, for a live process, it sent signal 0 through
// the system call, and besides signal 0 exactly the signals term (SIGTERM
// with --terminate); with it, it made no pidfd_send_signal system call.
// On another kernel it makes no pidfd system call.
func checkPidfdCalls(t *testing.T, c pidfdCalls, removed pidfdRemoved, pid int, word string, term ...int) {
	t.Helper()
	if c.procs == 0 || removed.open && c.hasOpen || removed.send && c.hasSend {
		t.Fatalf("noPidfdSite without %s: %d Python processes logged their functions, os.pidfd_open %v, signal.pidfd_send_signal %v; want at least one, and each removed function missing", removed, c.procs, c.hasOpen, c.hasSend)
	}
	if runtime.GOOS != "linux" {
		if len(c.opens)+len(c.sends) != 0 {
			t.Errorf("on %s: pidfd system calls (opened %v, signals %v); want none", runtime.GOOS, c.opens, c.sends)
		}
		return
	}
	reached := word == "verified" || word == "mismatch" || word == "gone"
	live := word == "verified" || word == "mismatch"
	var others []int
	for _, s := range c.sends {
		if s != 0 {
			others = append(others, s)
		}
	}
	// A cannot-check may come before or after the process was held.
	opensOK := len(c.opens) == 0
	switch {
	case !c.hasOpen && reached:
		opensOK = slices.Equal(c.opens, []int{pid})
	case !c.hasOpen:
		opensOK = len(c.opens) == 0 || slices.Equal(c.opens, []int{pid})
	}
	sendsOK := len(c.sends) == 0
	if !c.hasSend {
		sendsOK = (!live || slices.Contains(c.sends, 0)) && slices.Equal(others, term)
	}
	if len(c.kills) != 0 || !opensOK || !sendsOK {
		t.Errorf("a python3 without %s (left: os.pidfd_open %v, signal.pidfd_send_signal %v), result %s: signals by pid %v, pidfd_open system calls for %v, pidfd_send_signal system calls with %v; want no signal by pid, pidfd_open system calls only without os.pidfd_open (one for %d when the check reached the process), and pidfd_send_signal system calls only without signal.pidfd_send_signal (signal 0 for a live process, and besides only %v)", removed, c.hasOpen, c.hasSend, word, c.kills, c.opens, c.sends, pid, term)
	}
}

// linuxStartTicks returns the start time of process pid as the agent-ios
// record holds it on Linux: field 22 of /proc/<pid>/stat.
func linuxStartTicks(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	return f[19]
}

// TestOwnerCheckContract: iwdp-owner-check.sh prints one line
// "<result>: <reason>" and exits 0 (verified), 1 (mismatch), 2
// (cannot-check) or 3 (gone), as its header documents, for the records
// of agent-ios, of the removed iwdp-mcp and of scripts/sim-setup.sh; the
// manual stop of docs/device-runbook.md runs it on the first two.
func TestOwnerCheckContract(t *testing.T) {
	type checkCase struct {
		name     string
		system   string
		tokenVar string // the variable that carries the token in the probe
		argv0    string
		inArg    bool // the probe has <tokenVar>=<token> as an argument, not in its environment
		gone     bool
		name2    string // the record's file name, else by tokenVar's kind
		record   func(pid int, token, ticks string) string
		mode     os.FileMode
		symlink  bool
		noPidfd  bool // run the helper under noPidfdSite and check its calls
		wantCode int
		wantWord string
		wantWhy  string
	}
	cliRecord := func(pid int, token, ticks string) string {
		return fmt.Sprintf(`{"pid":%d,"pgid":%d,"addr":"127.0.0.1:9221","start":%q,"token":%q,"writer":"agent-ios"}`, pid, pid, ticks, token)
	}
	darwin := func(pid int, token, _ string) string {
		return fmt.Sprintf(`{"pid":%d,"start":%q,"token":%q}`, pid, recordedIWDPStart, token)
	}
	cases := []checkCase{
		{name: "macOS: agent-ios record", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN", record: darwin, wantCode: 0, wantWord: "verified"},
		{name: "macOS: iwdp-mcp record", system: "Darwin", tokenVar: "IWDP_MCP_START_TOKEN", record: darwin, wantCode: 0, wantWord: "verified"},
		// darwin-argv-spoof.sh and darwin-argument-token.py: the token text
		// only in an argument (the earlier helper read macOS ps -E, which
		// prints the arguments and the environment as one list).
		{name: "macOS: the token only in an argument (darwin-argv-spoof.sh)", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN", inArg: true, record: darwin, wantCode: 1, wantWord: "mismatch", wantWhy: "start token"},
		{name: "macOS: a JSON record does not take the Simulator's variable", system: "Darwin", tokenVar: simTokenEnv, record: darwin, wantCode: 1, wantWord: "mismatch", wantWhy: "start token"},
		{
			name: "macOS: a Linux start time cannot be checked", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN",
			record: func(pid int, token, _ string) string {
				return fmt.Sprintf(`{"pid":%d,"start":"12345","token":%q}`, pid, token)
			},
			wantCode: 2, wantWord: "cannot-check", wantWhy: "start time",
		},
		{name: "another system", system: "FreeBSD", tokenVar: "AGENT_IOS_START_TOKEN", record: darwin, wantCode: 2, wantWord: "cannot-check", wantWhy: "this system"},
		{name: "a record that group members can write", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN", record: darwin, mode: 0o620, wantCode: 2, wantWord: "cannot-check", wantWhy: "not a private file"},
		// The record is checked before the process (review WS3 round 6,
		// stop-safety reviewer): an invalid record is never "gone".
		{name: "a record that others can write, of a gone process", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN", record: darwin, mode: 0o602, gone: true, wantCode: 2, wantWord: "cannot-check", wantWhy: "not a private file"},
		{name: "a symlink record", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN", record: darwin, symlink: true, wantCode: 2, wantWord: "cannot-check", wantWhy: "cannot be opened"},
		{
			name: "a record without a token", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN",
			record:   func(pid int, _, _ string) string { return fmt.Sprintf(`{"pid":%d,"start":%q}`, pid, recordedIWDPStart) },
			wantCode: 2, wantWord: "cannot-check", wantWhy: "no start token",
		},
		{
			name: "a record without a pid", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN",
			record: func(_ int, token, _ string) string {
				return fmt.Sprintf(`{"pid":"1","start":%q,"token":%q}`, recordedIWDPStart, token)
			},
			wantCode: 2, wantWord: "cannot-check", wantWhy: "names no pid",
		},
		{
			name: "a record of pid 1", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN", name2: "iwdp.pid",
			record:   func(_ int, token, _ string) string { return simRecord(1, token, recordedIWDPStart, "") },
			wantCode: 2, wantWord: "cannot-check", wantWhy: "names no pid",
		},
		// cross-token-namespace.sh: the variable belongs to the record kind,
		// which the file name gives.
		{name: "an agent-ios record and only the old variable (cross-token-namespace.sh)", system: "Darwin", tokenVar: "IWDP_MCP_START_TOKEN", name2: "agent-ios-started-0-127.0.0.1:9221.json", record: darwin, wantCode: 1, wantWord: "mismatch", wantWhy: "start token"},
		{name: "an iwdp-mcp record and only the new variable", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN", name2: "iwdp-mcp-started-0-127.0.0.1:9221.json", record: darwin, wantCode: 1, wantWord: "mismatch", wantWhy: "start token"},
		{name: "a JSON record of an unclear kind", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN", name2: "record.json", record: darwin, wantCode: 2, wantWord: "cannot-check", wantWhy: "kind"},
		{name: "a pid of 5000 digits (long-pid.record)", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN", name2: "iwdp.pid", record: func(_ int, token, _ string) string {
			return strings.Repeat("9", 5000) + " " + token + " " + recordedIWDPStart
		}, wantCode: 2, wantWord: "cannot-check", wantWhy: "names no pid"},
		{name: "a gone process", system: "Darwin", tokenVar: "AGENT_IOS_START_TOKEN", record: darwin, gone: true, wantCode: 3, wantWord: "gone"},
	}
	// Linux reads the probe's real /proc files: the start time in clock
	// ticks (agent-ios's Linux record), the environment and the arguments.
	if runtime.GOOS == "linux" {
		cases = append(cases,
			checkCase{name: "Linux: agent-ios record", system: "Linux", tokenVar: "AGENT_IOS_START_TOKEN", argv0: "/usr/local/bin/" + iwdpProgram, record: cliRecord, wantCode: 0, wantWord: "verified"},
			checkCase{name: "Linux: iwdp-mcp record", system: "Linux", tokenVar: "IWDP_MCP_START_TOKEN", argv0: iwdpProgram, record: cliRecord, wantCode: 0, wantWord: "verified"},
			checkCase{
				name: "Linux: another start time", system: "Linux", tokenVar: "AGENT_IOS_START_TOKEN", argv0: iwdpProgram,
				record:   func(pid int, token, ticks string) string { return cliRecord(pid, token, ticks+"1") },
				wantCode: 1, wantWord: "mismatch", wantWhy: "another time",
			},
			checkCase{name: "Linux: a token holder that is not iwdp", system: "Linux", tokenVar: "AGENT_IOS_START_TOKEN", argv0: "python3", record: cliRecord, wantCode: 1, wantWord: "mismatch", wantWhy: "not ios_webkit_debug_proxy"},
			checkCase{
				name: "Linux: the token inside another variable", system: "Linux", tokenVar: "OTHER_AGENT_IOS_START_TOKEN", argv0: iwdpProgram, record: cliRecord,
				wantCode: 1, wantWord: "mismatch", wantWhy: "start token",
			},
		)
	}
	// Each case runs under this python3 and under a python3 without
	// os.pidfd_open and signal.pidfd_send_signal (noPidfdSite, the
	// standalone builds of uv and mise; review WS3 round 8): the results
	// are the same, and on a Linux kernel the second holds the process
	// with the pidfd system calls.
	for _, tc := range slices.Clone(cases) {
		tc.name, tc.noPidfd = "without the pidfd functions: "+tc.name, true
		cases = append(cases, tc)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := randomHex(t, 16)
			argv0 := tc.argv0
			if argv0 == "" {
				argv0 = iwdpProgram
			}
			spec := probeSpec{argv0: argv0, env: []string{tc.tokenVar + "=" + token}, exitOnTerm: true}
			if tc.inArg {
				spec.args, spec.env = spec.env, nil
			}
			probe := startOwnerProbe(t, spec)
			ticks := ""
			if tc.system == "Linux" {
				ticks = linuxStartTicks(t, probe.pid)
			}
			if tc.gone {
				probe.kill(t)
			}
			run := prepareOwnerRun(t, ownerWorld{system: tc.system, psStart: recordedIWDPStart}, probe, "")
			name := tc.name2
			if name == "" {
				name = "agent-ios-started-0-127.0.0.1:9221.json"
				if tc.tokenVar == "IWDP_MCP_START_TOKEN" {
					name = "iwdp-mcp-started-0-127.0.0.1:9221.json"
				}
			}
			record := filepath.Join(t.TempDir(), name)
			mode := tc.mode
			if mode == 0 {
				mode = 0o600
			}
			if err := os.WriteFile(record, []byte(tc.record(probe.pid, token, ticks)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(record, mode); err != nil {
				t.Fatal(err)
			}
			if tc.symlink {
				link := record + ".link"
				if err := os.Symlink(record, link); err != nil {
					t.Fatal(err)
				}
				record = link
			}
			env := run.env
			var calls func() pidfdCalls
			if tc.noPidfd {
				var extra []string
				extra, calls = withoutPidfdFunctions(t, removeBoth)
				env = append(slices.Clone(run.env), extra...)
			}
			stdout, stderr, code := runOwnerCheck(t, env, record)
			word, why, _ := strings.Cut(strings.TrimSuffix(stdout, "\n"), ": ")
			if code != tc.wantCode || word != tc.wantWord || !strings.Contains(why, tc.wantWhy) || strings.Count(stdout, "\n") != 1 || stderr != "" {
				t.Fatalf("iwdp-owner-check.sh: exit %d, stdout %q, stderr %q; want exit %d and one line %q with a reason that says %q", code, stdout, stderr, tc.wantCode, tc.wantWord, tc.wantWhy)
			}
			if tc.noPidfd {
				checkPidfdCalls(t, calls(), removeBoth, probe.pid, word)
			}
			for _, secret := range []string{token, probe.marker, ticks} {
				if secret != "" && strings.Contains(stdout, secret) {
					t.Fatalf("the output holds a value (%q): %s", secret, stdout)
				}
			}
			if probe.signaled() {
				t.Fatal("the check signaled the process")
			}
			if calls := run.sim.calls(t); strings.Contains(calls, token) {
				t.Fatalf("a command got the start token as an argument:\n%s", calls)
			}
		})
	}

	t.Run("usage", func(t *testing.T) {
		for _, args := range [][]string{{}, {"a", "b"}} {
			stdout, stderr, code := runOwnerCheck(t, os.Environ(), args...)
			if code != 64 || stdout != "" || !strings.Contains(stderr, "usage:") {
				t.Fatalf("args %q: exit %d, stdout %q, stderr %q; want exit 64 and the usage on stderr", args, code, stdout, stderr)
			}
		}
	})
}

// TestOwnerCheckTerminateWithoutPythonPidfdFunctions: under a python3
// without os.pidfd_open and signal.pidfd_send_signal (noPidfdSite: the
// standalone builds that uv and mise install, review WS3 round 8), and
// under one without only one of them, 'iwdp-owner-check.sh --terminate'
// and the Simulator teardown still verify the recorded iwdp and stop it,
// and report a gone one as gone. On a Linux kernel each missing function
// becomes its system call (pidfd_open, pidfd_send_signal with SIGTERM),
// each present one is used, and no signal goes by pid, also when uname
// says Darwin (the test seam that selects only the readers of the process
// data).
func TestOwnerCheckTerminateWithoutPythonPidfdFunctions(t *testing.T) {
	systems := []string{"Darwin"}
	if runtime.GOOS == "linux" {
		systems = append(systems, "Linux")
	}
	sigterm := int(syscall.SIGTERM)
	for _, removed := range []pidfdRemoved{removeBoth, removeOpenOnly, removeSendOnly} {
		for _, system := range systems {
			for _, via := range []string{"--terminate", "teardown"} {
				for _, gone := range []bool{false, true} {
					name := "without " + removed.String() + ", " + system + ", " + via + ", a live iwdp"
					if gone {
						name = "without " + removed.String() + ", " + system + ", " + via + ", a gone iwdp"
					}
					t.Run(name, func(t *testing.T) {
						tok := randomHex(t, 16)
						probe := startOwnerProbe(t, probeSpec{argv0: iwdpProgram, env: []string{simTokenEnv + "=" + tok}, exitOnTerm: true})
						if gone {
							probe.kill(t)
						}
						run := prepareOwnerRun(t, ownerWorld{system: system, psStart: recordedIWDPStart}, probe, simRecord(probe.pid, tok, recordedIWDPStart, ""))
						extra, calls := withoutPidfdFunctions(t, removed)
						env := append(slices.Clone(run.env), extra...)
						word, term := "verified", []int{sigterm}
						if gone {
							word, term = "gone", nil
						}
						if via == "--terminate" {
							stdout, stderr, code := runOwnerCheck(t, env, "--terminate", run.record)
							if got, _, _ := strings.Cut(stdout, ": "); got != word || code != map[string]int{"verified": 0, "gone": 3}[word] || stderr != "" {
								t.Fatalf("--terminate: exit %d, stdout %q, stderr %q; want %s", code, stdout, stderr, word)
							}
							if !gone && !probe.awaitSignal(t) {
								t.Fatal("the verified iwdp did not get SIGTERM")
							}
							checkPidfdCalls(t, calls(), removed, probe.pid, word, term...)
							return
						}
						_, stderr, code := runSimScript(t, env, "--teardown")
						if code != 0 || run.recordKept(t) {
							t.Fatalf("--teardown: exit %d, record kept %v, stderr %q; want exit 0 and the record removed", code, run.recordKept(t), stderr)
						}
						if !gone && !probe.signaled() {
							t.Fatal("the teardown did not stop the verified iwdp")
						}
						// The teardown runs the helper more than once: SIGTERM
						// once, then checks of the exited process (gone) and
						// --forget. Each run without os.pidfd_open (removed,
						// or missing in this Python) opens a pidfd of the pid
						// with the system call.
						c := calls()
						if runtime.GOOS == "linux" && c.procs > 0 && !c.hasOpen {
							if len(c.opens) == 0 || slices.ContainsFunc(c.opens, func(pid int) bool { return pid != probe.pid }) {
								t.Errorf("the teardown opened pidfds for %v; want only %d", c.opens, probe.pid)
							}
							c.opens = []int{probe.pid}
						}
						checkPidfdCalls(t, c, removed, probe.pid, word, term...)
					})
				}
			}
		}
	}
}

// TestOwnerCheckRefusesWhenThePidfdFallbackFails: on a Linux kernel,
// when a python3 without the pidfd functions cannot open a pidfd through
// the system call either, 'iwdp-owner-check.sh --terminate' reports
// cannot-check and signals nothing, and never falls back to a signal by
// pid (review WS3 round 8): a kernel without pidfd_open (ENOSYS) and
// another failure (EMFILE). The helper makes the system calls only on the
// machines whose table has pidfd_open 434 and pidfd_send_signal 424 (its
// allowlist, from Go's golang.org/x/sys/unix tables); on alpha, mips,
// mips64, an unknown machine and x86_64 with 4-byte pointers (x32 or a
// 32-bit process) it makes none and reports cannot-check. On riscv64, a
// machine of the allowlist other than x86_64, it verifies and signals
// through the system calls (the numbers of this test host's kernel are the
// same). On another kernel the fallback is never used: it verifies and
// signals by pid.
func TestOwnerCheckRefusesWhenThePidfdFallbackFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seams   []string
		opens   bool // it makes the pidfd_open system call
		allowed bool // the call succeeds: verified, with SIGTERM through the system call
	}{
		{"ENOSYS", []string{"OWNER_CHECK_PIDFD_OPEN_ERRNO=" + strconv.Itoa(int(syscall.ENOSYS))}, true, false},
		{"EMFILE", []string{"OWNER_CHECK_PIDFD_OPEN_ERRNO=" + strconv.Itoa(int(syscall.EMFILE))}, true, false},
		{"alpha", []string{"OWNER_CHECK_MACHINE=alpha"}, false, false},
		{"mips", []string{"OWNER_CHECK_MACHINE=mips"}, false, false},
		{"mips64", []string{"OWNER_CHECK_MACHINE=mips64"}, false, false},
		{"an unknown machine", []string{"OWNER_CHECK_MACHINE=z80"}, false, false},
		{"x86_64 with 4-byte pointers", []string{"OWNER_CHECK_MACHINE=x86_64", "OWNER_CHECK_POINTER_SIZE=4"}, false, false},
		{"riscv64 (allowed)", []string{"OWNER_CHECK_MACHINE=riscv64"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := randomHex(t, 16)
			probe := startOwnerProbe(t, probeSpec{argv0: iwdpProgram, env: []string{simTokenEnv + "=" + tok}, exitOnTerm: true})
			system := "Darwin" // a macOS host has no /proc
			if runtime.GOOS == "linux" {
				system = "Linux"
			}
			run := prepareOwnerRun(t, ownerWorld{system: system, psStart: recordedIWDPStart}, probe, simRecord(probe.pid, tok, recordedIWDPStart, ""))
			extra, calls := withoutPidfdFunctions(t, removeBoth)
			env := append(slices.Clone(run.env), append(extra, tc.seams...)...)
			stdout, stderr, code := runOwnerCheck(t, env, "--terminate", run.record)
			c := calls()
			if runtime.GOOS != "linux" {
				if code != 0 || !probe.awaitSignal(t) || len(c.opens)+len(c.sends) != 0 {
					t.Fatalf("on %s: exit %d, stdout %q, stderr %q, pidfd system calls %+v; want verified, SIGTERM by pid and no pidfd system call", runtime.GOOS, code, stdout, stderr, c)
				}
				return
			}
			if tc.allowed {
				if code != 0 || !strings.HasPrefix(stdout, "verified: ") || stderr != "" || !probe.awaitSignal(t) {
					t.Fatalf("--terminate: exit %d, stdout %q, stderr %q, probe signaled %v; want verified and SIGTERM", code, stdout, stderr, probe.signaled())
				}
				checkPidfdCalls(t, c, removeBoth, probe.pid, "verified", int(syscall.SIGTERM))
				return
			}
			if code != 2 || stdout != "cannot-check: this system cannot hold the process while it checks it\n" || stderr != "" {
				t.Fatalf("--terminate: exit %d, stdout %q, stderr %q; want exit 2 and cannot-check: this system cannot hold the process", code, stdout, stderr)
			}
			if probe.signaled() || len(c.kills)+len(c.sends) != 0 {
				t.Fatalf("the check signaled: probe signaled %v, signals by pid %v, pidfd signals %v; want none", probe.signaled(), c.kills, c.sends)
			}
			var wantOpens []int
			if tc.opens {
				wantOpens = []int{probe.pid}
			}
			if !slices.Equal(c.opens, wantOpens) {
				t.Fatalf("pidfd_open system calls for %v; want %v", c.opens, wantOpens)
			}
		})
	}
}

// TestManualStopUsesOnlyTheIdentityCheck: the manual stop of
// docs/device-runbook.md has two conditions, "you started it yourself and
// nobody else uses it" (review WS3 round 4, r1 F1 and r2 F14) and a
// "verified" from 'scripts/iwdp-owner-check.sh --terminate', which sends
// the signal itself, then the plain check until "gone", then
// 'scripts/iwdp-owner-check.sh --forget', which removes the record only
// while the path still names the record that it read, never a removal by
// hand (review WS3 round 5, r2). It has no
// listener check: a listener check can accept a socket that does not
// prove ownership (ipv6only-listener.py, review WS3 round 3 F8), and
// identity is what makes a stop safe. Nobody reads, types or prints a
// start token by hand (review WS3 round 3 F7). Every other document that
// tells how to stop an iwdp by hand states the same conditions and steps
// and names that section: by a Markdown link, or by its path and name
// where no link renders (CLAUDE.md's code block, and the embedded skill
// that 'agent-ios skills get' serves outside the repository).
func TestManualStopUsesOnlyTheIdentityCheck(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", path))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	runbook := read("docs/device-runbook.md")
	_, section, ok := strings.Cut(runbook, "### Stopping an iwdp by hand\n")
	if !ok {
		t.Fatal("docs/device-runbook.md has no section Stopping an iwdp by hand")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	for _, want := range []string{"You started this iwdp yourself, and nobody else uses it", "scripts/iwdp-owner-check.sh --terminate <record>", "until it reports `gone`", "remove the record with `scripts/iwdp-owner-check.sh --forget <record>`, never by hand"} {
		if !strings.Contains(section, want) {
			t.Errorf("the manual stop does not say %q:\n%s", want, section)
		}
	}
	// The runbook's safety rule forbids pkill, killall, kill and every
	// other script or command, and names each of the three tools that the
	// same page tells readers to use, with its scope; the manual stop with
	// both conditions. A rule without one of them forbids a procedure of
	// the page itself (review WS3 rounds 5 and 6, rename reviewer).
	rule := ""
	for _, line := range strings.Split(runbook, "\n") {
		if strings.HasPrefix(line, "- Never run `pkill`") {
			rule = line
		}
	}
	for _, want := range []string{
		"- Never run `pkill`, `killall` or `kill` on `ios_webkit_debug_proxy`, and never run any other script or command that stops or restarts it.",
		"Only three tools may stop an iwdp, and each proves the identity of that iwdp before it signals it:",
		"`agent-ios restart-iwdp`, only for an iwdp that agent-ios started and recorded",
		"the Simulator teardown (`make sim-teardown` or `scripts/sim-setup.sh --teardown`), only for the iwdp that setup started, under the run lease",
		"`scripts/iwdp-owner-check.sh --terminate`, only for a stop by hand of an iwdp that you started yourself and nobody else uses, as [Stopping an iwdp by hand](#stopping-an-iwdp-by-hand) describes",
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("the safety rule of docs/device-runbook.md does not say %q: %q", want, rule)
		}
	}
	// The teardown paragraph states what --forget removes and keeps, from
	// its own read, and that the next teardown does not check a record kept
	// under a private name (review WS3 round 6, rename reviewer and
	// stop-safety reviewer).
	for _, want := range []string{
		"That reads the record again and removes it only when the process that this read names is gone and the path still names the same file with the same content: a record that names a live process stays, and so does one that changed after that read.",
		"A record that another program wrote over the path between the check and that read is removed when the process that it names is gone, too.",
		"the changed record stays under a private name in the same directory (`.iwdp.pid.forget-<hex>`), which the warning names on one line, escaped: every byte outside printable ASCII, the apostrophe and the backslash as `\\0ooo` (a backslash, a zero and three octal digits), so that the name holds no apostrophe and is safe inside single quotes; the command `printf '%b' '<name>'`, with the name pasted between the quotes, gives the name back in bash, dash and zsh.",
		"The next teardown does not check that file, and the next setup does not refuse because of it: check it by hand as [Stopping an iwdp by hand](#stopping-an-iwdp-by-hand) describes",
		"To check such a private file, move it under its original name (without the leading `.` and the `.forget-<hex>` suffix) into a new directory that only you can write, and follow these steps for it.",
		"It checks the record before the process: a regular file of yours that group and others cannot write, with a valid pid, a start time and a start token. Any other record is `cannot-check`, also when no process has its pid, so it is never `gone` and never removed.",
	} {
		if !strings.Contains(runbook, want) {
			t.Errorf("docs/device-runbook.md does not say %q", want)
		}
	}
	for _, bad := range []string{"which keeps a record that changed after the check"} {
		if strings.Contains(runbook, bad) {
			t.Errorf("docs/device-runbook.md still says %q", bad)
		}
	}
	if want := "It removes the record only with `scripts/iwdp-owner-check.sh --forget`, which reads it again and removes it only when the process that this read names is gone and the file did not change after that read; a changed record that `--forget` keeps under a private name is not checked by the next teardown"; !strings.Contains(read("README.md"), want) {
		t.Errorf("README.md does not say %q", want)
	}
	// A listener check, or a command that puts a token in an argument or
	// prints it, in the procedure itself.
	for _, bad := range []string{"ss -", "lsof", "sport", "LISTEN", "listening socket", "grep -F", "kill ", "<token>", "may delete the record", "`rm ", "rm -f"} {
		if strings.Contains(section, bad) {
			t.Errorf("the manual stop of docs/device-runbook.md contains %q:\n%s", bad, section)
		}
	}
	// The earlier procedure's steps, in any document.
	for _, path := range []string{"docs/device-runbook.md", "README.md", "CLAUDE.md", "docs/guide.md", "docs/migration.md", "docs/known-limits.md", "docs/cdp.md", "skill-data/agent-ios/SKILL.md"} {
		text := read(path)
		for _, bad := range []string{"ss -Hltnp", "-sTCP:LISTEN", "grep -Fx", "kill <pid>"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s contains %q, a step of the earlier stop by hand", path, bad)
			}
		}
	}
	// The restart refusal states both conditions of the first step and
	// the steps of the stop.
	src := read("internal/proxy/restart.go")
	for _, want := range []string{"only if you started it and nobody else uses it", "'scripts/iwdp-owner-check.sh --terminate <record>', then 'scripts/iwdp-owner-check.sh <record>' until it reports gone", "docs/device-runbook.md (Stopping an iwdp by hand)"} {
		if !strings.Contains(src, want) {
			t.Errorf("the restart refusal in internal/proxy/restart.go does not say %q", want)
		}
	}
	// The other documents: both conditions, else ask; the stop with
	// --terminate, then the plain check until gone; the section.
	steps := []string{
		"nobody else uses it",
		"ask whoever started it, or the user",
		"`scripts/iwdp-owner-check.sh --terminate <record>`",
		"`scripts/iwdp-owner-check.sh <record>` until it reports `gone`",
	}
	for path, ref := range map[string][]string{
		"README.md":            {"[Stopping an iwdp by hand](docs/device-runbook.md#stopping-an-iwdp-by-hand)"},
		"CLAUDE.md":            {"(docs/device-runbook.md, Stopping an iwdp by hand)", "](docs/device-runbook.md#stopping-an-iwdp-by-hand)"},
		"docs/guide.md":        {"[Stopping an iwdp by hand](device-runbook.md#stopping-an-iwdp-by-hand)"},
		"docs/migration.md":    {"[Stopping an iwdp by hand](device-runbook.md#stopping-an-iwdp-by-hand)"},
		"docs/cdp.md":          {"[Stopping an iwdp by hand](device-runbook.md#stopping-an-iwdp-by-hand)"},
		"docs/known-limits.md": {"[Stopping an iwdp by hand](device-runbook.md#stopping-an-iwdp-by-hand)"},
		// The guide of the device and its faults, which the core guide names.
		"skill-data/agent-ios-device/SKILL.md": {"docs/device-runbook.md (Stopping an iwdp by hand)"},
	} {
		text := read(path)
		for _, want := range append(slices.Clone(steps), ref...) {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q", path, want)
			}
		}
	}
	// Other stop advice in the scripts and docs states both conditions.
	for _, c := range []struct{ path, want string }{
		{"scripts/sim-setup.sh", "stop it only when you started it yourself and nobody else uses it"},
		{"docs/device-runbook.md", "whoever started that iwdp, and only when nobody else uses it, stops it once"},
		// The restart refusal case of the runbook (review WS3 round 5,
		// rename reviewer).
		{"docs/device-runbook.md", "Do not stop that iwdp yourself unless you started it and nobody else uses it, and then only as [Stopping an iwdp by hand](#stopping-an-iwdp-by-hand) describes"},
		{"docs/migration.md", "Whoever started that iwdp, and only when nobody else uses it, stops it once"},
	} {
		if !strings.Contains(read(c.path), c.want) {
			t.Errorf("%s does not say %q", c.path, c.want)
		}
	}
	// The make sim-teardown line of CLAUDE.md's code block names the
	// helper's --terminate and the section by path and name.
	for _, line := range strings.Split(read("CLAUDE.md"), "\n") {
		if strings.HasPrefix(line, "make sim-teardown ") && (!strings.Contains(line, "scripts/iwdp-owner-check.sh --terminate") || !strings.Contains(line, "(docs/device-runbook.md, Stopping an iwdp by hand)")) {
			t.Errorf("CLAUDE.md's make sim-teardown line does not name scripts/iwdp-owner-check.sh --terminate and docs/device-runbook.md, Stopping an iwdp by hand: %s", line)
		}
	}
}

// TestOwnerCheckReadsTheRecordOnce: iwdp-owner-check.sh decides from one
// read of the record, and --terminate signals the process of that read,
// even when another program of this user replaces the record during the
// check (helper-record-race.sh, review WS3 round 4, r1 F9 and r2 F11).
// The replacement happens right after the helper read the record (at its
// uname call, before step 2, the existence) or at its first ps call (the start time,
// step 3); for the earlier helper also at its "kill -0" between two reads
// (BASH_ENV), whichever comes first.
//   - Two records whose fields would pass together, but neither alone:
//     the first names the iwdp with another start time, the second a
//     decoy with the right one. The earlier helper took the pid from its
//     first read and the rest from its second, and reported "verified".
//   - A valid record replaced by one that names a decoy: --terminate
//     signals the verified process, never the decoy.
func TestOwnerCheckReadsTheRecordOnce(t *testing.T) {
	systems := []string{"Darwin"}
	if runtime.GOOS == "linux" {
		systems = append(systems, "Linux")
	}
	for _, combo := range []struct{ system, at string }{{"Darwin", "uname"}, {"Darwin", "ps"}, {"Linux", "uname"}, {"Linux", "ps"}} {
		system, at := combo.system, combo.at
		if !slices.Contains(systems, system) {
			continue
		}
		for _, tc := range []struct {
			name       string
			firstStart string // the start time in the first record; ps shows recordedIWDPStart for the iwdp
			wantWord   string
			wantCode   int
			wantSignal bool // with --terminate
		}{
			{name: "fields of two records never combine", firstStart: otherIWDPStart, wantWord: "mismatch", wantCode: 1},
			{name: "--terminate signals the process of its one read", firstStart: recordedIWDPStart, wantWord: "verified", wantCode: 0, wantSignal: true},
		} {
			t.Run(system+", replaced at "+at+": "+tc.name, func(t *testing.T) {
				tok := randomHex(t, 16)
				iwdp := startOwnerProbe(t, probeSpec{argv0: iwdpProgram, env: []string{simTokenEnv + "=" + tok}, exitOnTerm: true})
				decoy := startOwnerProbe(t, probeSpec{argv0: iwdpProgram, env: []string{simTokenEnv + "=" + tok}, exitOnTerm: true})
				first := simRecord(iwdp.pid, tok, tc.firstStart, "")
				second := simRecord(decoy.pid, tok, recordedIWDPStart, "")
				swapTo := filepath.Join(t.TempDir(), "second-record")
				if err := os.WriteFile(swapTo, []byte(second+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				run := prepareOwnerRun(t, ownerWorld{system: system, psStart: recordedIWDPStart}, iwdp, first, "FAKE_SWAP_TO="+swapTo, "FAKE_SWAP_AT_UNAME="+map[string]string{"uname": "1"}[at])
				for _, mode := range [][]string{{}, {"--terminate"}} {
					writeSimState(t, run.sim, "iwdp.pid", first)
					if err := os.Remove(swapTo + ".done"); err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					stdout, stderr, code := runOwnerCheck(t, run.env, append(mode, run.record)...)
					word, _, _ := strings.Cut(strings.TrimSuffix(stdout, "\n"), ": ")
					if _, err := os.Stat(swapTo + ".done"); err != nil {
						t.Fatalf("%q: the record was not replaced during the check (%v); stdout %q", mode, err, stdout)
					}
					if got, err := os.ReadFile(run.record); err != nil || strings.TrimSpace(string(got)) != second {
						t.Fatalf("%q: the record holds %q (%v); want the second record", mode, got, err)
					}
					if word != tc.wantWord || code != tc.wantCode || strings.Count(stdout, "\n") != 1 || stderr != "" {
						t.Fatalf("%q with the record replaced during the check: exit %d, stdout %q, stderr %q; want exit %d and %q, the result of the first record", mode, code, stdout, stderr, tc.wantCode, tc.wantWord)
					}
					if decoy.signaled() {
						t.Fatalf("%q signaled the decoy that the replaced record names", mode)
					}
					want := tc.wantSignal && len(mode) == 1
					sent := strings.Contains(stdout, "SIGTERM was sent")
					if got := iwdp.signaled() || sent && iwdp.awaitSignal(t, decoy); got != want {
						t.Fatalf("%q: the verified iwdp got SIGTERM: %v", mode, got)
					}
				}
			})
		}
	}
}

// TestOwnerCheckMalformedRecords: for every malformed record,
// iwdp-owner-check.sh prints exactly one line "cannot-check: <reason>" on
// stdout, nothing on stderr (no traceback), exits 2 as its header
// documents, and signals nothing and removes nothing, also with
// --terminate and --forget (long-pid.record, review WS3 round 4, r2 F13).
// The file name decides the kind and the format of a record: content in
// the other format, or an unknown name, is cannot-check with that reason
// (review WS3 round 5, r1 F12; the earlier helper chose the format by the
// content and verified a line record under an agent-ios name with the
// Simulator's variable). The records name the pid of a live probe
// that runs ios_webkit_debug_proxy with the recorded token and start
// time, so a record that the helper wrongly read as valid would verify
// it. Then, over generated edits of a record with a wrong token (so that
// none verifies): one line with a result word, the exit code of that
// word, nothing on stderr and no signal.
func TestOwnerCheckMalformedRecords(t *testing.T) {
	tok := randomHex(t, 16)
	probe := startOwnerProbe(t, probeSpec{argv0: iwdpProgram, env: []string{"AGENT_IOS_START_TOKEN=" + tok, "IWDP_MCP_START_TOKEN=" + tok, simTokenEnv + "=" + tok}, exitOnTerm: true})
	run := prepareOwnerRun(t, ownerWorld{system: "Darwin", psStart: recordedIWDPStart}, probe, "")
	pid := strconv.Itoa(probe.pid)
	const start = recordedIWDPStart
	jsonRec := func(pidField string) string {
		return fmt.Sprintf(`{"pid":%s,"start":%q,"token":%q}`, pidField, start, tok)
	}
	simLine := func(pidField string) string { return pidField + " " + tok + " " + start }
	// digits returns pid in the decimal digits that start at zero.
	digits := func(zero rune) string {
		return strings.Map(func(r rune) rune { return zero + (r - '0') }, pid)
	}
	long := strings.Repeat("9", 5000)
	const jsonName, simName = "agent-ios-started-0-127.0.0.1:9221.json", "iwdp.pid"
	cases := []struct{ name, file, text string }{
		{"JSON: a pid of 5000 digits", jsonName, jsonRec(long)},
		{"JSON: a pid string of 5000 digits", jsonName, jsonRec(`"` + long + `"`)},
		{"JSON: a negative pid", jsonName, jsonRec("-" + pid)},
		{"JSON: a pid with a plus sign", jsonName, jsonRec("+" + pid)},
		{"JSON: a pid of 2^31", jsonName, jsonRec("2147483648")},
		{"JSON: a pid of 2^32 + pid", jsonName, jsonRec(strconv.Itoa(1<<32 + probe.pid))},
		{"JSON: a pid of 11 digits", jsonName, jsonRec("99999999999")},
		{"JSON: a float pid", jsonName, jsonRec(pid + ".0")},
		{"JSON: a pid with an exponent", jsonName, jsonRec(pid + "e0")},
		{"JSON: a pid of 1e400", jsonName, jsonRec("1e400")},
		{"JSON: a pid of NaN", jsonName, jsonRec("NaN")},
		{"JSON: a pid of Infinity", jsonName, jsonRec("Infinity")},
		{"JSON: a pid of true", jsonName, jsonRec("true")},
		{"JSON: a pid of null", jsonName, jsonRec("null")},
		{"JSON: a pid in an array", jsonName, jsonRec("[" + pid + "]")},
		{"JSON: a pid in an object", jsonName, jsonRec(`{"pid":` + pid + "}")},
		{"JSON: an empty pid string", jsonName, jsonRec(`""`)},
		{"JSON: a pid string with spaces", jsonName, jsonRec(`" ` + pid + ` "`)},
		{"JSON: a pid string with a NUL", jsonName, jsonRec(`"` + pid + `\u0000"`)},
		{"JSON: a pid in Arabic-Indic digits", jsonName, jsonRec(`"` + digits('\u0660') + `"`)},
		{"JSON: a pid in fullwidth digits", jsonName, jsonRec(`"` + digits('\uff10') + `"`)},
		{"JSON: a token of another type", jsonName, fmt.Sprintf(`{"pid":%s,"start":%q,"token":[%q]}`, pid, start, tok)},
		{"JSON: a start time of another type", jsonName, fmt.Sprintf(`{"pid":%s,"start":1,"token":%q}`, pid, tok)},
		{"JSON: an array", jsonName, "[" + jsonRec(pid) + "]"},
		{"JSON: a string", jsonName, `"{\"pid\":1}"`},
		{"JSON: deep nesting", jsonName, `{"a":` + strings.Repeat("[", 200000) + strings.Repeat("]", 200000) + "}"},
		{"JSON: a NUL byte", jsonName, jsonRec(pid)[:5] + "\x00" + jsonRec(pid)[5:]},
		{"JSON: not UTF-8", jsonName, strings.TrimSuffix(jsonRec(pid), "}") + `,"x":"` + "\xff" + `"}`},
		{"JSON: over 64 KiB", jsonName, jsonRec(pid) + strings.Repeat(" ", 64*1024)},
		{"Simulator: a pid of 5000 digits (long-pid.record)", simName, simLine(long)},
		{"Simulator: a negative pid", simName, simLine("-" + pid)},
		{"Simulator: a pid with a plus sign", simName, simLine("+" + pid)},
		{"Simulator: a pid in Arabic-Indic digits", simName, simLine(digits('\u0660'))},
		{"Simulator: a pid in fullwidth digits", simName, simLine(digits('\uff10'))},
		{"Simulator: a hexadecimal pid", simName, simLine("0x" + strconv.FormatInt(int64(probe.pid), 16))},
		{"Simulator: a pid of 2^31", simName, simLine("2147483648")},
		{"Simulator: a pid of 2^32 + pid", simName, simLine(strconv.Itoa(1<<32 + probe.pid))},
		{"Simulator: a pid of 11 digits", simName, simLine("99999999999")},
		{"Simulator: an empty record", simName, ""},
		{"Simulator: only whitespace", simName, " \t \n"},
		{"Simulator: an empty first line", simName, "\n" + simLine(pid)},
		{"Simulator: a NUL after the pid", simName, simLine(pid + "\x00")},
		{"Simulator: a NUL before the pid", simName, "\x00" + simLine(pid)},
		{"Simulator: only a pid", simName, pid},
		{"Simulator: no start time", simName, pid + " " + tok},
		{"Simulator: not UTF-8", simName, simLine(pid) + "\xff"},
		{"Simulator: over 64 KiB", simName, simLine(pid) + "\n" + strings.Repeat("x", 64*1024)},
	}
	// why is the reason that a case must give, where it matters: the
	// cross-format records are valid in the other format, so the earlier
	// helper verified most of them, and gave another reason for the rest.
	why := map[string]string{}
	const wrongFormat, unknownName = "the content of the record does not match the format of its file name", "its file name is neither iwdp.pid nor agent-ios-started-*.json nor iwdp-mcp-started-*.json"
	for _, c := range []struct{ name, file, text, why string }{
		{"cross-format: JSON content under iwdp.pid", simName, jsonRec(pid), wrongFormat},
		{"cross-format: a line record under agent-ios-started-*.json", jsonName, simLine(pid), wrongFormat},
		{"cross-format: a line record under iwdp-mcp-started-*.json", "iwdp-mcp-started-0-127.0.0.1:9221.json", simLine(pid), wrongFormat},
		{"cross-format: JSON content under an unknown name", "record.json", jsonRec(pid), unknownName},
		{"cross-format: a line record under an unknown name", "record.pid", simLine(pid), unknownName},
		{"cross-format: JSON content under agent-ios-started-*.pid", "agent-ios-started-0-127.0.0.1:9221.pid", jsonRec(pid), unknownName},
	} {
		cases = append(cases, struct{ name, file, text string }{c.name, c.file, c.text})
		why[c.name] = c.why
	}
	dir := t.TempDir()
	write := func(file, text string) string {
		t.Helper()
		sub, err := os.MkdirTemp(dir, "")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(sub, file)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := write(tc.file, tc.text)
			for _, mode := range [][]string{{}, {"--terminate"}, {"--forget"}} {
				stdout, stderr, code := runOwnerCheck(t, run.env, append(mode, record)...)
				if code != 2 || !strings.HasPrefix(stdout, "cannot-check: ") || !strings.Contains(stdout, why[tc.name]) || strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") || stderr != "" {
					t.Fatalf("%q: exit %d, stdout %q, stderr %q; want exit 2, one line \"cannot-check: <reason>\" with a reason that says %q, and no stderr", mode, code, stdout, stderr, why[tc.name])
				}
				if _, err := os.Lstat(record); err != nil {
					t.Fatalf("%q removed the record: %v", mode, err)
				}
				if strings.Contains(stdout, tok) {
					t.Fatalf("%q: the output holds the token: %q", mode, stdout)
				}
				if probe.signaled() {
					t.Fatalf("%q signaled the probe that the record names", mode)
				}
			}
		})
	}

	t.Run("generated edits", func(t *testing.T) {
		pool := []string{"\x00", "\xff", "\u0660", "\uff10", "-", "+", ".0", "e9", "1e400", "99999999999", long, "{", "}", "[", "]", "\"", ":", ",", "null", "true", " ", "\t", "\n", "\u00e9"}
		seen := map[string]int{}
		prop := func(simKind bool, ops []uint32) bool {
			wrong := randomHex(t, 16)
			text := []byte(fmt.Sprintf(`{"pid":%d,"pgid":%d,"addr":"127.0.0.1:9221","start":%q,"token":%q,"writer":"agent-ios"}`, probe.pid, probe.pid, start, wrong))
			file := jsonName
			if simKind {
				text, file = []byte(simRecord(probe.pid, wrong, start, "")), simName
			}
			// One to three edits, so that many records stay close to
			// valid. Each op: bits 0-7 a byte or a pool index, bits 8-23
			// a position, bits 24-31 the edit.
			for _, op := range ops[:min(len(ops), 1+len(ops)%3)] {
				b, pos := byte(op), int(op>>8&0xffff)%(len(text)+1)
				switch op >> 24 % 6 {
				case 0: // insert a byte
					text = append(text[:pos:pos], append([]byte{b}, text[pos:]...)...)
				case 1: // delete a byte
					if pos < len(text) {
						text = append(text[:pos:pos], text[pos+1:]...)
					}
				case 2: // replace a byte
					if pos < len(text) {
						text[pos] = b
					}
				case 3: // insert a string of the pool
					text = append(text[:pos:pos], append([]byte(pool[int(b)%len(pool)]), text[pos:]...)...)
				case 4: // cut
					text = text[:pos]
				case 5: // repeat the tail
					text = append(text, text[pos:]...)
				}
			}
			record := write(file, string(text))
			stdout, stderr, code := runOwnerCheck(t, run.env, "--terminate", record)
			word, _, _ := strings.Cut(stdout, ": ")
			want, known := map[string]int{"mismatch": 1, "cannot-check": 2, "gone": 3}[word]
			seen[word]++
			if !known || code != want || strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") || stderr != "" || probe.signaled() {
				t.Errorf("record %q: exit %d, stdout %q, stderr %q, probe signaled %v; want one line with mismatch, cannot-check or gone, its exit code, no stderr and no signal", text, code, stdout, stderr, probe.signaled())
				return false
			}
			return true
		}
		cfg := &quick.Config{MaxCount: 2 * teardownPropertyCount(t), Rand: rand.New(rand.NewSource(teardownPropertySeed(t)))}
		if err := quick.Check(prop, cfg); err != nil {
			t.Fatal(err)
		}
		t.Logf("generated results: %v", seen)
	})
}

// TestOwnerCheckForgetKeepsAReplacedRecord: 'iwdp-owner-check.sh --forget
// RECORD', the removal after "gone" of the teardown and of the manual stop,
// signals nothing and removes the record only when the process of its one
// read is gone and the path still names that record (review WS3 round 5,
// r2). Another program of this user writes the record of a live decoy over
// the path right after the helper read it (at its uname call): the record
// stays, with the decoy's content, and the reason says why.
func TestOwnerCheckForgetKeepsAReplacedRecord(t *testing.T) {
	for _, tc := range []struct {
		name           string
		gone, replaced bool
		public         bool // others can write the record
		noFields       bool // the record holds only the pid
		wantWord       string
		wantCode       int
		wantWhy        string
		wantKept       bool
	}{
		{name: "a gone process", gone: true, wantWord: "gone", wantCode: 3, wantWhy: "the record was removed"},
		{name: "a gone process, the record replaced after the read by a live decoy's", gone: true, replaced: true, wantWord: "cannot-check", wantCode: 2, wantWhy: "changed after it was read; it was kept", wantKept: true},
		{name: "a live, verified process", wantWord: "verified", wantCode: 0, wantWhy: "the recorded ios_webkit_debug_proxy", wantKept: true},
		// Review WS3 round 6, stop-safety reviewer: the helper checked the
		// process before the record, so an invalid record of a gone process
		// was "gone", and --forget removed it.
		{name: "a gone process, a record that others can write", gone: true, public: true, wantWord: "cannot-check", wantCode: 2, wantWhy: "not a private file", wantKept: true},
		{name: "a gone process, a record with only the pid", gone: true, noFields: true, wantWord: "cannot-check", wantCode: 2, wantWhy: "no start time or no start token", wantKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, decoyTok := randomHex(t, 16), randomHex(t, 16)
			probe := startOwnerProbe(t, probeSpec{argv0: iwdpProgram, env: []string{simTokenEnv + "=" + tok}, exitOnTerm: true})
			decoy := startOwnerProbe(t, probeSpec{argv0: iwdpProgram, env: []string{simTokenEnv + "=" + decoyTok}, exitOnTerm: true})
			first := simRecord(probe.pid, tok, recordedIWDPStart, "")
			second := simRecord(decoy.pid, decoyTok, recordedIWDPStart, "")
			swapTo := filepath.Join(t.TempDir(), "second-record")
			if err := os.WriteFile(swapTo, []byte(second+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var extra []string
			if tc.replaced {
				extra = []string{"FAKE_SWAP_TO=" + swapTo, "FAKE_SWAP_AT_UNAME=1"}
			}
			if tc.noFields {
				first = strconv.Itoa(probe.pid)
			}
			run := prepareOwnerRun(t, ownerWorld{system: "Darwin", psStart: recordedIWDPStart}, probe, first, extra...)
			if tc.public {
				if err := os.Chmod(run.record, 0o606); err != nil {
					t.Fatal(err)
				}
			}
			if tc.gone {
				probe.kill(t)
			}
			stdout, stderr, code := runOwnerCheck(t, run.env, "--forget", run.record)
			word, why, _ := strings.Cut(strings.TrimSuffix(stdout, "\n"), ": ")
			if word != tc.wantWord || code != tc.wantCode || !strings.Contains(why, tc.wantWhy) || strings.Count(stdout, "\n") != 1 || stderr != "" {
				t.Fatalf("--forget: exit %d, stdout %q, stderr %q; want exit %d and one line %q with a reason that says %q", code, stdout, stderr, tc.wantCode, tc.wantWord, tc.wantWhy)
			}
			if _, err := os.Stat(swapTo + ".done"); tc.replaced && err != nil {
				t.Fatalf("the record was not replaced during the check: %v", err)
			}
			if got := run.recordKept(t); got != tc.wantKept {
				t.Fatalf("record kept: %v; want %v", got, tc.wantKept)
			}
			if tc.public || tc.noFields {
				if got, err := os.ReadFile(run.record); err != nil || strings.TrimSpace(string(got)) != first {
					t.Fatalf("the record holds %q (%v); want it unchanged", got, err)
				}
			}
			if tc.replaced {
				if got, err := os.ReadFile(run.record); err != nil || strings.TrimSpace(string(got)) != second {
					t.Fatalf("the record holds %q (%v); want the decoy's record", got, err)
				}
			}
			entries, err := os.ReadDir(filepath.Dir(run.record))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".iwdp.pid.forget-") {
					t.Fatalf("--forget left %s", e.Name())
				}
			}
			if probe.signaled() || decoy.signaled() {
				t.Fatalf("--forget signaled a process: the recorded one %v, the decoy %v", probe.signaled(), decoy.signaled())
			}
			for _, secret := range []string{tok, decoyTok, probe.marker, decoy.marker} {
				if strings.Contains(stdout, secret) {
					t.Fatalf("the output holds %q: %s", secret, stdout)
				}
			}
		})
	}
}

// TestOwnerCheckForgetNamesTheKeptFileOnOneLine: when --forget keeps a
// changed record under a private name, its one result line names that
// file escaped (every byte outside printable ASCII, the apostrophe and
// the backslash as \0ooo), so a newline in the directory does not break
// the line (review WS3 round 6, stop-safety reviewer: the one-line guard
// replaced the reason with "the check failed unexpectedly", and the kept
// path was lost). The documented decoding, the command that
// docs/device-runbook.md gives with the reported name pasted between its
// single quotes and run by bash, gives the exact path back and runs
// nothing, for a directory with an apostrophe, $( ), a backtick, a
// newline and backslashes, also before the n and the 0 that printf %b
// would read as escapes, and a digit after an escaped byte (review WS3
// round 8, stop-safety reviewer: an apostrophe ended the quotes, and the
// $( ) after it ran).
func TestOwnerCheckForgetNamesTheKeptFileOnOneLine(t *testing.T) {
	runbook, err := os.ReadFile("../docs/device-runbook.md")
	if err != nil {
		t.Fatal(err)
	}
	const documented = "printf '%b' '<name>'"
	if !strings.Contains(string(runbook), "`"+documented+"`") {
		t.Fatalf("docs/device-runbook.md does not give the decoding command %q", documented)
	}
	probe := startOwnerProbe(t, probeSpec{argv0: iwdpProgram, exitOnTerm: true})
	probe.kill(t)
	// A substitution that runs creates its marker in work, the shell's
	// directory.
	work := t.TempDir()
	dir := filepath.Join(t.TempDir(), "dir'$(touch subst)'`touch tick`\nnew\\line\\n\\0101-\u00e9")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "iwdp.pid")
	if err := os.WriteFile(record, []byte(simRecord(probe.pid, randomHex(t, 16), recordedIWDPStart, "")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runOwnerCheck(t, append(os.Environ(), forgetRaceEnv(t)), "--forget", record)
	const marker = "; it was kept as "
	line := strings.TrimSuffix(stdout, "\n")
	_, escaped, ok := strings.Cut(line, marker)
	if code != 2 || !strings.HasPrefix(line, "cannot-check: ") || !ok || strings.Contains(line, "\n") || stderr != "" {
		t.Fatalf("--forget: exit %d, stdout %q, stderr %q; want exit 2 and one line \"cannot-check: ...%s<escaped path>\"", code, stdout, stderr, marker)
	}
	// Every shell of bash, sh, dash and zsh that is installed decodes it
	// the same way; bash is required.
	var kept string
	for _, shell := range []string{"bash", "sh", "dash", "zsh"} {
		if _, err := exec.LookPath(shell); err != nil {
			if shell == "bash" {
				t.Fatal(err)
			}
			continue
		}
		decode := exec.CommandContext(t.Context(), shell, "-c", strings.Replace(documented, "<name>", escaped, 1))
		decode.Dir = work
		out, err := decode.Output()
		if err != nil {
			t.Fatalf("%s: the documented decoding of %q: %v", shell, escaped, err)
		}
		for _, marker := range []string{"subst", "tick"} {
			if _, err := os.Lstat(filepath.Join(work, marker)); err == nil {
				t.Errorf("%s: the documented decoding of %q ran a command (%s exists)", shell, escaped, marker)
			}
		}
		if kept != "" && string(out) != kept {
			t.Errorf("%s decodes %q as %q; bash as %q", shell, escaped, out, kept)
		}
		kept = string(out)
		if filepath.Dir(kept) != dir || !strings.HasPrefix(filepath.Base(kept), ".iwdp.pid.forget-") {
			t.Fatalf("%s: the documented decoding of %q gives %q; want a private name in %q", shell, escaped, kept, dir)
		}
	}
	if got, err := os.ReadFile(kept); err != nil || string(got) != forgetRaceChanged {
		t.Fatalf("the reported file %q holds %q (%v); want the changed record", kept, got, err)
	}
	if got, err := os.ReadFile(record); err != nil || string(got) != forgetRaceNew {
		t.Fatalf("the record holds %q (%v); want the new record", got, err)
	}
}
