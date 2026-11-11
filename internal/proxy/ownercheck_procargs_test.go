package proxy

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/quick"
)

// The macOS parser of scripts/iwdp-owner-check.sh (procargs_env) reads
// the KERN_PROCARGS2 buffer by the same rules as procargsEnv. These tests
// run the script's Python part as a module in a test process of their
// own (ownerCheckPython): they call procargs_env on buffers from
// procargsBuffer, and the script's main on crafted buffers, with the
// process readers replaced inside that test process only. The script
// itself takes no process data from its caller: its arguments are only
// the mode and the record path, and it reads the process from the kernel
// (sysctl KERN_PROCARGS2 on macOS, /proc on Linux; under a fake uname on a
// Linux kernel, the real /proc files of the recorded process).

const ownerCheckScript = "../../scripts/iwdp-owner-check.sh"

// ownerCheckDriver runs in the test's Python process. It reads a JSON
// request from stdin and prints the JSON answer:
//   - "parse": base64 buffers; for each, procargs_env's arguments and
//     environment (base64), or {"error": true} for its ValueError. Any
//     other exception is a traceback and fails the test;
//   - "check": cases {"buffer", "token"}; for each, the result line and
//     exit code of the script's main in mode "check" (it never signals)
//     for a private iwdp-cli record of this Python process (its pid, the
//     start time ownerCheckStart and the token), with uname answering
//     Darwin, ps answering the start time and darwin_procargs returning
//     the buffer;
//   - "terminate": cases {"buffer", "token"}; for each, the same in mode
//     "terminate" for a record of a child "sleep" of this Python process,
//     with spies on Process (whether it held a pidfd), on the script's own
//     wrapper pidfd_send_signal and on os.kill (the signals sent), the
//     child's exit status, the kernel that this process sees (os.uname,
//     as the script), whether this Python has os.pidfd_open and whether
//     it has signal.pidfd_send_signal, and the arguments of each call of the
//     script's linux_syscall (its system calls through ctypes). The child is
//     always killed by its pid and reaped, also when the setup fails.
const ownerCheckDriver = `import base64, contextlib, io, json, os, signal, sys, tempfile
sys.path.insert(0, sys.argv[1])
import owner_check as oc

syscalls = []
real_syscall = oc.linux_syscall
def spy_syscall(number, argtypes, *args):
    syscalls.append([number] + [a if isinstance(a, int) else None for a in args])
    return real_syscall(number, argtypes, *args)
oc.linux_syscall = spy_syscall

b64 = lambda b: base64.b64encode(b).decode()
req = json.load(sys.stdin)
answer = {"parse": [], "check": []}
for item in req["parse"]:
    try:
        args, env = oc.procargs_env(base64.b64decode(item))
    except ValueError:
        answer["parse"].append({"error": True})
        continue
    answer["parse"].append({"args": [b64(a) for a in args], "env": [b64(e) for e in env]})

start = req["start"]
oc.run = lambda argv: {"uname": "Darwin\n", "ps": start + "\n"}.get(argv[0])
for case in req["check"]:
    buf = base64.b64decode(case["buffer"])
    oc.darwin_procargs = lambda pid, buf=buf: buf
    path = os.path.join(tempfile.mkdtemp(), "iwdp-cli-started-0-127.0.0.1:9221.json")
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    os.write(fd, json.dumps({"pid": os.getpid(), "start": start, "token": case["token"]}).encode())
    os.close(fd)
    sys.argv = ["owner_check", "check", path]
    out = io.StringIO()
    code = None
    with contextlib.redirect_stdout(out):
        try:
            oc.main()
        except SystemExit as e:
            code = e.code
    answer["check"].append({"code": code, "line": out.getvalue()})
answer["terminate"] = []
for case in req.get("terminate", []):
    import signal, subprocess
    buf = base64.b64decode(case["buffer"])
    oc.darwin_procargs = lambda pid, buf=buf: buf
    held, sent, code, status = [], [], None, "still running"
    del syscalls[:]
    out = io.StringIO()
    real_process, real_kill, real_pidfd = oc.Process, os.kill, oc.pidfd_send_signal
    def restore():
        oc.Process, os.kill, oc.pidfd_send_signal = real_process, real_kill, real_pidfd
    child = subprocess.Popen(["sleep", "60"])
    try:
        path = os.path.join(tempfile.mkdtemp(), "iwdp-cli-started-0-127.0.0.1:9221.json")
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        os.write(fd, json.dumps({"pid": child.pid, "start": start, "token": case["token"]}).encode())
        os.close(fd)
        class SpyProcess(real_process):
            def __init__(self, *a):
                super().__init__(*a)
                held.append(self.fd is not None)
        def spy_pidfd(fd, sig, *a):
            sent.append(["pidfd", int(sig)])
            return real_pidfd(fd, sig, *a)
        def spy_kill(pid, sig):
            sent.append(["kill", int(sig)])
            return real_kill(pid, sig)
        oc.Process, os.kill, oc.pidfd_send_signal = SpyProcess, spy_kill, spy_pidfd
        sys.argv = ["owner_check", "terminate", path]
        try:
            with contextlib.redirect_stdout(out):
                try:
                    oc.main()
                except SystemExit as e:
                    code = e.code
        finally:
            restore()
        try:
            status = str(child.wait(timeout=10))
        except subprocess.TimeoutExpired:
            pass
    finally:
        # Always reap the child, by its pid, also when the setup failed.
        restore()
        if child.poll() is None:
            real_kill(child.pid, signal.SIGKILL)
            child.wait()
    answer["terminate"].append({"code": code, "line": out.getvalue(), "held": held, "sent": sent, "status": status, "kernel": os.uname().sysname,
                                "has_open": hasattr(os, "pidfd_open"), "has_send": hasattr(signal, "pidfd_send_signal"), "syscalls": list(syscalls)})
json.dump(answer, sys.stdout)
`

const ownerCheckStart = "Mon Nov 10 10:00:00 2026"

// ownerCheckPython writes the Python part of iwdp-owner-check.sh (its
// here-document) as the module owner_check.py into a new directory and
// returns that directory.
func ownerCheckPython(t *testing.T) string {
	t.Helper()
	script, err := os.ReadFile(ownerCheckScript)
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(script), "<<'PY'\n")
	body, _, ok2 := strings.Cut(after, "\nPY\n")
	if !ok || !ok2 {
		t.Fatalf("%s has no Python here-document <<'PY' ... PY", ownerCheckScript)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "owner_check.py"), []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ownerCheckAnswer is the driver's answer.
type ownerCheckAnswer struct {
	Parse []struct {
		Error     bool
		Args, Env []string // base64
	}
	Check []struct {
		Code *int
		Line string
	}
	Terminate []struct {
		Code   *int
		Line   string
		Held   []bool
		Sent   [][]any
		Status string
		Kernel string
		// HasOpen and HasSend: the Python has os.pidfd_open and
		// signal.pidfd_send_signal. Syscalls: the arguments of each
		// call of linux_syscall, the system call number first (None as
		// nil).
		HasOpen  bool `json:"has_open"`
		HasSend  bool `json:"has_send"`
		Syscalls [][]*int
	}
}

// noPidfdSite is a sitecustomize.py that removes the functions that
// $OWNER_CHECK_REMOVE names, a comma-separated subset of os.pidfd_open and
// signal.pidfd_send_signal: a Python like the standalone builds that uv
// and mise install, which lack both on Linux, or one that lacks only one.
const noPidfdSite = `import os, signal
_remove = os.environ["OWNER_CHECK_REMOVE"].split(",")
for module, name in ((os, "pidfd_open"), (signal, "pidfd_send_signal")):
    if name in _remove and hasattr(module, name):
        delattr(module, name)
`

// withoutPidfdFunctions writes noPidfdSite and returns the environment
// entries that load it into a Python process of the test only, with the
// functions names (pidfd_open, pidfd_send_signal) removed.
func withoutPidfdFunctions(t *testing.T, names ...string) []string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sitecustomize.py"), []byte(noPidfdSite), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"PYTHONPATH=" + dir, "OWNER_CHECK_REMOVE=" + strings.Join(names, ",")}
}

// ownerCheckCase is one crafted buffer for the check, with the token of
// the record.
type ownerCheckCase struct {
	Buffer []byte `json:"buffer"`
	Token  string `json:"token"`
}

// runOwnerCheckDriver runs ownerCheckDriver on the buffers and cases.
func runOwnerCheckDriver(t *testing.T, parse [][]byte, check []ownerCheckCase, terminate ...ownerCheckCase) ownerCheckAnswer {
	t.Helper()
	return runOwnerCheckDriverEnv(t, nil, parse, check, terminate...)
}

// runOwnerCheckDriverEnv is runOwnerCheckDriver with the extra
// environment entries env for the driver's Python process.
func runOwnerCheckDriverEnv(t *testing.T, env []string, parse [][]byte, check []ownerCheckCase, terminate ...ownerCheckCase) ownerCheckAnswer {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("the iwdp-owner-check.sh tests need python3: %v", err)
	}
	if parse == nil {
		parse = [][]byte{}
	}
	if check == nil {
		check = []ownerCheckCase{}
	}
	if terminate == nil {
		terminate = []ownerCheckCase{}
	}
	req, err := json.Marshal(map[string]any{"parse": parse, "check": check, "terminate": terminate, "start": ownerCheckStart})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), python, "-c", ownerCheckDriver, ownerCheckPython(t))
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin = strings.NewReader(string(req))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the Python part of iwdp-owner-check.sh: %v\n%s", err, stderr.String())
	}
	var a ownerCheckAnswer
	if err := json.Unmarshal(out, &a); err != nil {
		t.Fatalf("the driver's answer %q: %v", out, err)
	}
	if len(a.Parse) != len(parse) || len(a.Check) != len(check) || len(a.Terminate) != len(terminate) {
		t.Fatalf("the driver answered %d parses, %d checks and %d terminates; want %d, %d and %d", len(a.Parse), len(a.Check), len(a.Terminate), len(parse), len(check), len(terminate))
	}
	return a
}

// decoded returns the base64 strings as strings.
func decoded(t *testing.T, in []string) []string {
	t.Helper()
	out := make([]string, len(in))
	for i, s := range in {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = string(b)
	}
	return out
}

// TestOwnerCheckProcargsOnCraftedBuffers: on macOS iwdp-owner-check.sh
// takes the token only from the environment of the KERN_PROCARGS2 buffer,
// whose boundaries it must prove (review WS3 round 4, r1 F4 and r2 F12):
//   - the token only in an argument, or only in the executable path, is a
//     mismatch;
//   - a buffer without an environment is a mismatch;
//   - padding after the executable path and before macOS's own strings
//     changes nothing;
//   - a buffer that ends inside a string or before its last argument is
//     cannot-check;
//   - an argument count above the strings of the buffer is cannot-check;
//     one that takes the token entry for an argument is a mismatch.
func TestOwnerCheckProcargsOnCraftedBuffers(t *testing.T) {
	const tok = "tok-0123456789abcdef"
	entry := "IWDP_CLI_START_TOKEN=" + tok
	const path = "/opt/homebrew/bin/ios_webkit_debug_proxy"
	iwdpArgs := []string{"ios_webkit_debug_proxy", "-F", "-s", "127.0.0.1:27753"}
	env := []string{"HOME=/Users/u", entry, "PATH=/usr/bin"}
	apple := []string{"MallocNanoZone=1", "ptr_munge=0x1"}
	const pad = 3
	good := procargsBuffer(path, iwdpArgs, env, apple, pad)
	// argsEnd is the length of good up to the NUL of its last argument
	// (procargsBuffer pads after the environment as well).
	argsEnd := len(procargsBuffer(path, iwdpArgs, nil, nil, pad)) - pad
	withArgc := func(buf []byte, argc uint32) []byte {
		out := slices.Clone(buf)
		binary.NativeEndian.PutUint32(out, argc)
		return out
	}
	type crafted struct {
		name string
		buf  []byte
		want string
	}
	argOnly := procargsBuffer(path, append(slices.Clone(iwdpArgs), entry), []string{"HOME=/Users/u"}, apple, pad)
	cases := []crafted{
		{"the token in the environment", good, "verified"},
		{"the token only in an argument", argOnly, "mismatch"},
		{"the token only in the executable path", procargsBuffer(entry, iwdpArgs, []string{"HOME=/Users/u"}, apple, pad), "mismatch"},
		{"the token inside another variable", procargsBuffer(path, iwdpArgs, []string{"X" + entry}, apple, pad), "mismatch"},
		{"no environment", procargsBuffer(path, iwdpArgs, nil, apple, pad), "mismatch"},
		{"no environment and no strings of macOS", procargsBuffer(path, iwdpArgs, nil, nil, 0), "mismatch"},
		{"another program", procargsBuffer(path, []string{"/usr/bin/python3"}, []string{entry}, apple, pad), "mismatch"},
		// The padding NULs after the environment are empty strings.
		{"an argument count above the strings", withArgc(good, uint32(len(iwdpArgs)+len(env)+pad+len(apple)+1)), "cannot-check"},
		{"an argument count of 2^32-1", withArgc(good, 1<<32-1), "cannot-check"},
		{"an argument count that takes the token entry for an argument", withArgc(good, uint32(len(iwdpArgs)+2)), "mismatch"},
	}
	for _, n := range []int{0, 1, 2, 7, 16, 64} {
		cases = append(cases,
			crafted{fmt.Sprintf("%d padding bytes, with macOS's strings", n), procargsBuffer(path, iwdpArgs, []string{entry}, apple, n), "verified"},
			crafted{fmt.Sprintf("%d padding bytes, without macOS's strings", n), procargsBuffer(path, iwdpArgs, []string{entry}, nil, n), "verified"},
		)
	}
	// Every cut before the end of the arguments, and every later cut
	// inside a string.
	for n := 0; n < len(good); n++ {
		if n < argsEnd || good[n-1] != 0 {
			cases = append(cases, crafted{fmt.Sprintf("cut to %d of %d bytes", n, len(good)), good[:n], "cannot-check"})
		}
	}
	check := make([]ownerCheckCase, len(cases))
	for i, c := range cases {
		check[i] = ownerCheckCase{Buffer: c.buf, Token: tok}
	}
	answer := runOwnerCheckDriver(t, [][]byte{argOnly}, check)
	for i, c := range cases {
		got := answer.Check[i]
		word, _, _ := strings.Cut(strings.TrimSuffix(got.Line, "\n"), ": ")
		wantCode := map[string]int{"verified": 0, "mismatch": 1, "cannot-check": 2}[c.want]
		if word != c.want || got.Code == nil || *got.Code != wantCode || strings.Count(got.Line, "\n") != 1 {
			code := "none"
			if got.Code != nil {
				code = fmt.Sprint(*got.Code)
			}
			t.Errorf("%s: exit %s, line %q; want exit %d and one line %q", c.name, code, got.Line, wantCode, c.want)
		}
		if strings.Contains(got.Line, tok) {
			t.Errorf("%s: the line holds the token: %q", c.name, got.Line)
		}
	}
	// The parser itself: the token only in an argument stays an argument.
	p := answer.Parse[0]
	if p.Error || slices.Contains(decoded(t, p.Env), entry) || !slices.Contains(decoded(t, p.Args), entry) {
		t.Errorf("procargs_env of the token only in an argument = arguments %q, environment %q, error %v; want the entry among the arguments only", decoded(t, p.Args), decoded(t, p.Env), p.Error)
	}
}

// procargsInput is one generated KERN_PROCARGS2 buffer for
// TestOwnerCheckProcargsMatchesProcargsEnv: by Shape, a buffer in macOS's
// layout (procargsBuffer), that buffer cut, that buffer with another
// argument count, or raw bytes.
type procargsInput struct {
	Path             string
	Args, Env, Apple []string
	Pad              uint8
	Cut              uint16 // Shape 1: the length, modulo the buffer's length + 1
	Argc             uint32 // Shape 2: the argument count, modulo the number of strings + 3
	Raw              []byte // Shape 3: the buffer
	Shape            uint8  // modulo 4: 0 layout, 1 cut, 2 argument count, 3 raw bytes
}

// buffer returns the input's buffer and, for a buffer in macOS's layout,
// its arguments.
func (in procargsInput) buffer() (buf []byte, args []string, layout bool) {
	clean := func(prefix string, items []string) []string {
		out := make([]string, len(items))
		for i, s := range items {
			out[i] = prefix + strings.ReplaceAll(s, "\x00", "_")
		}
		return out
	}
	args = clean("a", in.Args) // an empty argument would read as padding
	buf = procargsBuffer("/"+strings.ReplaceAll(in.Path, "\x00", "_"), args, clean("E", in.Env), clean("p", in.Apple), int(in.Pad%17))
	switch in.Shape % 4 {
	case 1:
		return buf[:int(in.Cut)%(len(buf)+1)], nil, false
	case 2:
		binary.NativeEndian.PutUint32(buf, in.Argc%uint32(len(in.Args)+len(in.Env)+len(in.Apple)+3))
		return buf, nil, false
	case 3:
		return in.Raw, nil, false
	}
	return buf, args, true
}

// TestOwnerCheckProcargsMatchesProcargsEnv, property: on every generated
// buffer (macOS's layout, cut, with another argument count, or raw
// bytes), procargs_env of iwdp-owner-check.sh fails exactly when
// procargsEnv fails, and otherwise returns the same environment; on a
// buffer in macOS's layout it returns the arguments as written.
func TestOwnerCheckProcargsMatchesProcargsEnv(t *testing.T) {
	cfg := quickConfig(t, 400)
	inputs := make([]procargsInput, cfg.MaxCount)
	bufs := make([][]byte, cfg.MaxCount)
	for i := range inputs {
		v, ok := quick.Value(reflect.TypeFor[procargsInput](), cfg.Rand)
		if !ok {
			t.Fatal("quick.Value cannot generate a procargsInput")
		}
		inputs[i] = v.Interface().(procargsInput)
		bufs[i], _, _ = inputs[i].buffer()
	}
	answer := runOwnerCheckDriver(t, bufs, nil)
	seen := map[string]int{}
	for i, in := range inputs {
		buf, args, layout := in.buffer()
		goEnv, goErr := procargsEnv(buf)
		got := answer.Parse[i]
		seen[fmt.Sprintf("%s, error %v", []string{"layout", "cut", "argument count", "raw"}[in.Shape%4], goErr != nil)]++
		if got.Error != (goErr != nil) {
			t.Errorf("buffer %q: procargs_env error %v, procargsEnv error %v", buf, got.Error, goErr)
			continue
		}
		if goErr != nil {
			continue
		}
		if env := decoded(t, got.Env); len(env)+len(goEnv) > 0 && !slices.Equal(env, goEnv) {
			t.Errorf("buffer %q: procargs_env environment %q, procargsEnv %q", buf, env, goEnv)
		}
		if got := decoded(t, got.Args); layout && len(got)+len(args) > 0 && !slices.Equal(got, args) {
			t.Errorf("buffer %q: procargs_env arguments %q; want %q", buf, got, args)
		}
	}
	t.Logf("generated buffers: %v", seen)
}

// TestOwnerCheckTerminateSignalsThroughAPidfdOnLinux: the process handle
// and the signal of 'iwdp-owner-check.sh --terminate' follow the real
// kernel, never the external uname (review WS3 round 5, r1). Here uname
// says Darwin (the test seam that selects only the readers of the process
// data): on a Linux kernel the check still holds a pidfd and sends SIGTERM
// through it, and never by pid with os.kill; on a macOS kernel it signals
// by pid. The kernel is the one that the driver's Python process sees
// (os.uname), so a simulated macOS (no pidfd, os.uname saying Darwin)
// runs the macOS branch. The recorded process is a child sleep of the
// driver.
//
// It runs under this python3, under a python3 without os.pidfd_open and
// signal.pidfd_send_signal (noPidfdSite; the standalone builds of uv and
// mise, review WS3 round 8), and under one without only one of them. The
// helper chooses per function: for each function that the Python has, it
// uses that function and makes no system call for it; for each one that
// it lacks, it makes the system call through ctypes (the script's
// linux_syscall): pidfd_open (434), and pidfd_send_signal (424) with
// SIGTERM.
func TestOwnerCheckTerminateSignalsThroughAPidfdOnLinux(t *testing.T) {
	const tok = "tok-terminate-0123456789"
	const sysPidfdOpen, sysPidfdSendSignal = 434, 424 // the Linux system call numbers
	buf := procargsBuffer("/opt/homebrew/bin/ios_webkit_debug_proxy", []string{"ios_webkit_debug_proxy"}, []string{"IWDP_CLI_START_TOKEN=" + tok}, nil, 3)
	for _, python := range []struct {
		name   string
		remove []string // the functions that noPidfdSite removes; none: this python3 as it is
	}{
		{"this python3", nil},
		{"a python3 without the pidfd functions", []string{"pidfd_open", "pidfd_send_signal"}},
		{"a python3 without os.pidfd_open only", []string{"pidfd_open"}},
		{"a python3 without signal.pidfd_send_signal only", []string{"pidfd_send_signal"}},
	} {
		t.Run(python.name, func(t *testing.T) {
			var env []string
			if python.remove != nil {
				env = withoutPidfdFunctions(t, python.remove...)
			}
			answer := runOwnerCheckDriverEnv(t, env, nil, nil, ownerCheckCase{Buffer: buf, Token: tok})
			got := answer.Terminate[0]
			if got.Code == nil || *got.Code != 0 || !strings.HasPrefix(got.Line, "verified: ") || !strings.Contains(got.Line, "SIGTERM was sent") {
				code := "none"
				if got.Code != nil {
					code = fmt.Sprint(*got.Code)
				}
				t.Fatalf("--terminate: exit %s, line %q; want exit 0 and verified with SIGTERM sent", code, got.Line)
			}
			if got.Status != "-15" {
				t.Errorf("the recorded child ended with %q; want -15 (SIGTERM)", got.Status)
			}
			if python.remove != nil && (slices.Contains(python.remove, "pidfd_open") && got.HasOpen || slices.Contains(python.remove, "pidfd_send_signal") && got.HasSend) {
				t.Fatalf("noPidfdSite without %v: the python3 still has os.pidfd_open %v, signal.pidfd_send_signal %v", python.remove, got.HasOpen, got.HasSend)
			}
			sigterm := fmt.Sprint(int(syscall.SIGTERM))
			var viaPidfd, viaKill bool
			for _, s := range got.Sent {
				if fmt.Sprint(s[1]) == sigterm {
					viaPidfd = viaPidfd || s[0] == "pidfd"
					viaKill = viaKill || s[0] == "kill"
				}
			}
			// The system calls through linux_syscall: pidfd_open, and
			// pidfd_send_signal with SIGTERM, no siginfo and no flags.
			var opened, sent, termed bool
			for _, c := range got.Syscalls {
				if len(c) == 0 || c[0] == nil {
					continue
				}
				opened = opened || *c[0] == sysPidfdOpen
				sent = sent || *c[0] == sysPidfdSendSignal
				termed = termed || len(c) == 5 && *c[0] == sysPidfdSendSignal && c[2] != nil && fmt.Sprint(*c[2]) == sigterm && c[3] == nil && c[4] != nil && *c[4] == 0
			}
			// The kernel that the script's own process sees (os.uname),
			// decides, as the script's header documents.
			switch got.Kernel {
			case "Linux":
				if len(got.Held) != 1 || !got.Held[0] || !viaPidfd || viaKill {
					t.Errorf("on a Linux kernel with uname saying Darwin: pidfd held %v, signals %v; want a pidfd and SIGTERM only through it", got.Held, got.Sent)
				}
				// Each function decides its own path.
				if opened == got.HasOpen {
					t.Errorf("os.pidfd_open present %v: pidfd_open system call (%d) made %v; want it exactly when the function is missing (system calls %v)", got.HasOpen, sysPidfdOpen, opened, got.Syscalls)
				}
				if sent == got.HasSend || !got.HasSend && !termed {
					t.Errorf("signal.pidfd_send_signal present %v: pidfd_send_signal system calls (%d) made %v, with SIGTERM %v; want them exactly when the function is missing, then with SIGTERM (system calls %v)", got.HasSend, sysPidfdSendSignal, sent, termed, got.Syscalls)
				}
			case "Darwin":
				if len(got.Held) != 1 || got.Held[0] || !viaKill || viaPidfd || opened || sent {
					t.Errorf("on a macOS kernel: pidfd held %v, signals %v, system calls %v; want no pidfd and SIGTERM by pid", got.Held, got.Sent, got.Syscalls)
				}
			default:
				t.Errorf("the driver saw the kernel %q; want Linux or Darwin", got.Kernel)
			}
		})
	}
}
