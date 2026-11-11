#!/usr/bin/env bash
#
# iwdp-owner-check.sh [--terminate | --forget] RECORD tells whether the
# live process that the iwdp record RECORD names is the
# ios_webkit_debug_proxy that the record's writer started. With
# --terminate it also sends SIGTERM to that process, and only to it, in the
# same step: the Simulator teardown (scripts/sim-setup.sh) and the manual
# stop of docs/device-runbook.md (Stopping an iwdp by hand) never send a
# signal themselves. With --forget it signals nothing and, on "gone", also
# removes RECORD, but only when the path still names the record that it
# read (see below); the teardown and the manual stop never remove a
# record themselves.
#
# The file name of RECORD decides its kind, its format and the variable
# that carries the token, and the content must have that format:
# - iwdp-cli-started-*.json (iwdp-cli) and iwdp-mcp-started-*.json (a
#   version before the rename to iwdp-cli): JSON with "pid", "start" and
#   "token"; the token is in IWDP_CLI_START_TOKEN or IWDP_MCP_START_TOKEN.
#   The "writer" mark cannot tell them apart (both versions wrote
#   "iwdp-cli");
# - iwdp.pid, the record of scripts/sim-setup.sh: a first line
#   "<pid> <token> <start>"; the token is in IWDP_SIM_START_TOKEN.
# Another file name, or content in the other format, is cannot-check.
# The pid is a decimal of 1 to 10 digits, above 1 and below 2^31. A start
# of digits only is the Linux start time (field 22 of /proc/<pid>/stat,
# clock ticks after boot); any other start is the text of
# 'ps -o lstart=' in the C locale, with single spaces.
#
# The record is read once; every check uses the values of that one read.
# The checks, in this order:
# 1. the record: a regular file of this user that group and others
#    cannot write, with a valid pid, a start time and a start token.
#    Any other record is cannot-check, so only a valid record can be
#    "gone", and only a valid record can be removed (--forget);
# 2. existence: on a Linux kernel (os.uname, never the external uname) it
#    opens a pidfd for the pid (it stays bound to that process); on a
#    macOS kernel it sends signal 0. No process is "gone"; one that this
#    user may not signal is not this user's iwdp and cannot be checked.
#    A Python without os.pidfd_open and signal.pidfd_send_signal (the
#    standalone builds that uv and mise install) makes the same Linux
#    system calls through ctypes; a kernel without them (ENOSYS), or any
#    other failure to open the pidfd, is cannot-check, never a signal by
#    pid;
# 3. the start time of the process equals the recorded one;
# 4. the environment of the process holds the entry <variable>=<token>,
#    read with exact boundaries between the arguments and the
#    environment: Linux /proc/<pid>/environ (NUL-separated entries);
#    macOS the sysctl KERN_PROCARGS2 buffer (procargs_env). When the
#    boundaries cannot be proven, the result is cannot-check;
# 5. the program (the base name of the first argument, from the same
#    source) is ios_webkit_debug_proxy;
# 6. Linux: the pidfd's process still runs, so steps 3 to 5 read it and
#    not a process that reused the pid.
# With --terminate and "verified", SIGTERM goes on a Linux kernel through
# the pidfd, so it reaches only the checked process; on a macOS kernel by
# pid right after the checks (docs/known-limits.md).
#
# With --forget and "gone", the record is removed only when the path still
# names exactly the record of the one read: the same file (device and
# inode) with the same content. The path is first renamed to a private
# name in the same directory, so that no other record can take the path
# while the file is compared; the file under the private name is deleted
# only on a match. On a mismatch the file goes back to the path only when
# the path is still free (a hard link, which never replaces a file), and
# the result is cannot-check; when another file took the path in the
# meantime, the changed file stays under its private name, which the
# reason gives escaped on one line: every byte outside printable ASCII,
# the apostrophe and the backslash as \0ooo, a backslash, a zero and its
# three octal digits (POSIX printf %b; bash, dash and zsh decode it).
# The escaped name holds no apostrophe, so it is safe inside single
# quotes, where nothing else is special to the shell: printf '%b' '<name>'
# recovers the path. Nothing
# checks it again by itself (docs/device-runbook.md, Stopping an iwdp by
# hand, step 3, says how to check it). --forget makes its own read: a
# record that replaced the one of an earlier check before that read is
# removed when the process that it names is gone. A writer that already
# holds the record open can still change it after the comparison; scripts/sim-setup.sh and iwdp-cli
# replace their records by rename and never write one in place.
#
# It prints one line on stdout, "<result>: <reason>", and exits with the
# result's code:
#   0 verified      the process is the recorded iwdp (with --terminate:
#                   and SIGTERM was sent to it)
#   1 mismatch      a process with the pid runs, but it is not the recorded iwdp
#   2 cannot-check  the record or the process cannot be read or checked;
#                   also every unexpected failure
#   3 gone          no process has the recorded pid (with --forget: and
#                   the record was removed, or no longer exists)
#   64              usage error (on stderr)
# The reason is a fixed text, except for the escaped private name of a kept
# file (--forget): no token, start time, command line or environment of any
# process. The token stays in this script's Python
# process: it is read from RECORD and never passed in an argument.
#
# Test seams, each of which can only keep a signal from being sent and
# never supplies process data: IWDP_OWNER_CHECK_TEST_NOT_PERMITTED=1 makes
# step 2 report a process that this user may not signal. The external
# 'uname -s' selects only the readers of the process data, never the
# process handle or the signal. When it says Darwin but the kernel
# (os.uname) is Linux, which only a fake uname on PATH causes, the
# KERN_PROCARGS2 buffer is built from the real /proc/<pid>/cmdline and
# environ of that process (linux_procargs), so the macOS parser reads the
# real process's own data, and the pidfd still holds and signals it.
set -uo pipefail
export LC_ALL=C

mode=check
if [ "$#" -eq 2 ]; then
  case "$1" in
    --terminate) mode=terminate; shift ;;
    --forget) mode=forget; shift ;;
  esac
fi
if [ "$#" -ne 1 ] || [ "${1#-}" != "$1" ]; then
  echo "usage: $0 [--terminate | --forget] RECORD" >&2
  exit 64
fi

# The Python part prints the result line; this shell passes it on only
# when it is one line whose word matches the exit code, else cannot-check.
out=$(python3 - "$mode" "$1" <<'PY'
import errno, json, os, re, secrets, signal, stat, struct, subprocess, sys

MISMATCH, CANNOT, GONE = 1, 2, 3
WORDS = {0: "verified", MISMATCH: "mismatch", CANNOT: "cannot-check", GONE: "gone"}
PROGRAM = b"ios_webkit_debug_proxy"
MAX_RECORD = 64 * 1024
PID_RE = re.compile(r"[0-9]{1,10}")  # ASCII digits only; bounded before int()
PID_LIMIT = 2**31
NOT_PERMITTED_TEST = "IWDP_OWNER_CHECK_TEST_NOT_PERMITTED"
SIM_RECORD = "iwdp.pid"  # the file name that scripts/sim-setup.sh writes
CTL_KERN, KERN_ARGMAX, KERN_PROCARGS2 = 1, 8, 49  # macOS <sys/sysctl.h>
# The Linux system call numbers of pidfd_open and pidfd_send_signal, and
# the machines (os.uname().machine) whose system call table has them: the
# tables of 386, amd64, arm, arm64, loong64, ppc, ppc64, ppc64le, riscv64,
# s390x and sparc64 in Go's golang.org/x/sys/unix (zsysnum_linux_*.go,
# v0.45.0). Other tables number them differently (MIPS o32 4434 and 4424,
# n64 5434 and 5424, n32 a range of its own; alpha and ia64 others again),
# so on every other machine the fallback makes no system call.
SYS_PIDFD_OPEN, SYS_PIDFD_SEND_SIGNAL = 434, 424
SHARED_TABLE_MACHINES = re.compile(
    r"x86_64|i[3-6]86|aarch64|arm64|armv[0-9][0-9a-z]*|loongarch64|ppc|ppc64|ppc64le|riscv64|s390x|sparc64")


class Result(Exception):
    def __init__(self, code, reason):
        super().__init__(reason)
        self.code, self.reason = code, reason


def read_file(path):
    """Returns (bytes, file status) of one read of the regular file path,
    opened without following a symlink and without blocking on a FIFO."""
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except OSError as e:
        raise Result(CANNOT, "the record cannot be opened (" + errno.errorcode.get(e.errno, "error") + ")")
    with os.fdopen(fd, "rb") as f:
        st = os.fstat(f.fileno())
        if not stat.S_ISREG(st.st_mode):
            raise Result(CANNOT, "the record is not a regular file")
        data = f.read(MAX_RECORD + 1)
    if len(data) > MAX_RECORD:
        raise Result(CANNOT, "the record is too large")
    return data, st


def read_record(path):
    """Returns (text, bytes, file status) of the one read of the record."""
    data, st = read_file(path)
    try:
        return data.decode(), data, st
    except UnicodeDecodeError:
        raise Result(CANNOT, "the record is not text")


def parse_pid(value):
    """Returns the pid of a record field, or None unless it is a bounded
    decimal (a string of 1 to 10 ASCII digits, or a JSON integer) in range."""
    if isinstance(value, bool):
        return None
    if isinstance(value, str):
        if not PID_RE.fullmatch(value):
            return None
        value = int(value)
    if not isinstance(value, int) or not 1 < value < PID_LIMIT:
        return None
    return value


def parse(text, name):
    """Returns (pid, start, token, token variable) of the record text. The
    file name decides the kind and the format, and the text must have that
    format; pid is None and start or token "" when the record lacks them."""
    if name == SIM_RECORD:
        var, is_json = "IWDP_SIM_START_TOKEN", False
    elif name.startswith("iwdp-cli-started-") and name.endswith(".json"):
        var, is_json = "IWDP_CLI_START_TOKEN", True
    elif name.startswith("iwdp-mcp-started-") and name.endswith(".json"):
        var, is_json = "IWDP_MCP_START_TOKEN", True
    else:
        raise Result(CANNOT, "the kind of the record is unclear (its file name is neither iwdp.pid nor iwdp-cli-started-*.json nor iwdp-mcp-started-*.json)")
    if text.lstrip().startswith("{") != is_json:
        raise Result(CANNOT, "the content of the record does not match the format of its file name")
    if is_json:
        try:
            rec = json.loads(text, parse_int=lambda s: int(s) if len(s.lstrip("-")) <= 10 else None,
                             parse_float=lambda s: None, parse_constant=lambda s: None)
        except (ValueError, RecursionError):
            raise Result(CANNOT, "the record is not valid JSON")
        if not isinstance(rec, dict):
            raise Result(CANNOT, "the record is not a JSON object")
        pid, start, token = parse_pid(rec.get("pid")), rec.get("start"), rec.get("token")
        start = start if isinstance(start, str) else ""
        token = token if isinstance(token, str) else ""
    else:
        fields = (text.splitlines() or [""])[0].split(None, 2)
        fields += [""] * (3 - len(fields))
        pid = parse_pid(fields[0])
        token, start = fields[1], fields[2]
    return pid, " ".join(start.split()), token, var


def run(argv):
    """Returns the stdout of argv, or None when it fails or prints nothing."""
    try:
        p = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                           stderr=subprocess.DEVNULL, timeout=10)
    except (OSError, subprocess.SubprocessError):
        return None
    out = p.stdout.decode(errors="replace")
    return out if p.returncode == 0 and out.strip() else None


def proc_file(pid, name):
    try:
        with open(f"/proc/{pid}/{name}", "rb") as f:
            return f.read()
    except OSError:
        return None


def procargs_env(buf):
    """Returns (arguments, environment) of a KERN_PROCARGS2 buffer (macOS):
    the argument count as a native-endian 32-bit integer, the executable
    path, NUL padding, the arguments, the environment, NUL padding and
    macOS's own strings (pfz=..., ptr_munge=...), each string
    NUL-terminated. No empty string reliably ends the environment, so the
    environment is every nonempty string after the arguments; macOS's own
    strings never name a start-token variable. It raises ValueError when
    the buffer ends inside a string, so the boundaries are not proven. The
    same rules as procargsEnv in internal/proxy/restart.go."""
    if len(buf) < 4:
        raise ValueError("no argument count")
    (argc,) = struct.unpack("=I", buf[:4])
    rest = buf[4:]

    def take(rest):
        i = rest.find(b"\0")
        if i < 0:
            raise ValueError("the buffer ends inside a string")
        return rest[:i], rest[i + 1:]

    _, rest = take(rest)  # the executable path
    rest = rest.lstrip(b"\0")
    args = []
    for _ in range(argc):  # each argument takes at least its NUL
        arg, rest = take(rest)
        args.append(arg)
    env = []
    while rest:
        kv, rest = take(rest)
        if kv:
            env.append(kv)
    return args, env


def kern_procargs2(pid):
    """Reads the KERN_PROCARGS2 buffer of process pid with sysctl (macOS;
    only for this user's processes)."""
    import ctypes
    libc = ctypes.CDLL(None, use_errno=True)
    argmax, size = ctypes.c_int(0), ctypes.c_size_t(ctypes.sizeof(ctypes.c_int))
    if libc.sysctl((ctypes.c_int * 2)(CTL_KERN, KERN_ARGMAX), 2, ctypes.byref(argmax), ctypes.byref(size), None, 0) != 0:
        raise OSError(ctypes.get_errno(), "sysctl KERN_ARGMAX")
    buf, size = ctypes.create_string_buffer(argmax.value), ctypes.c_size_t(argmax.value)
    if libc.sysctl((ctypes.c_int * 3)(CTL_KERN, KERN_PROCARGS2, pid), 3, buf, ctypes.byref(size), None, 0) != 0:
        raise OSError(ctypes.get_errno(), "sysctl KERN_PROCARGS2")
    return buf.raw[:size.value]


def linux_procargs(pid):
    """Builds a KERN_PROCARGS2 buffer from the real /proc files of process
    pid on a Linux kernel (the test path of the header)."""
    exe, cmdline, environ = os.readlink(f"/proc/{pid}/exe"), proc_file(pid, "cmdline"), proc_file(pid, "environ")
    if cmdline is None or environ is None:
        raise OSError(errno.EACCES, "/proc")
    split = lambda b: b[:-1].split(b"\0") if b else []
    args, env = split(cmdline), split(environ)
    strings = lambda items: b"".join(s + b"\0" for s in items)
    return (struct.pack("=I", len(args)) + exe.encode() + b"\0" + b"\0" * 3 + strings(args)
            + strings(env) + b"\0" * 3 + b"ptr_munge=\0")


def darwin_procargs(pid):
    kernel = os.uname().sysname
    if kernel == "Darwin":
        return kern_procargs2(pid)
    if kernel == "Linux":
        return linux_procargs(pid)
    raise OSError(errno.ENOSYS, kernel)


def linux_syscall(number, argtypes, *args):
    """Calls the Linux system call number with args through libc's
    syscall function (ctypes, with the argument types argtypes) and
    returns its result; it raises OSError with errno when it fails."""
    import ctypes
    machine = os.uname().machine
    # x86_64 with 4-byte pointers is the x32 ABI, whose system call numbers
    # carry another bit, or a 32-bit process: it is refused as well.
    if not SHARED_TABLE_MACHINES.fullmatch(machine) or (machine == "x86_64" and ctypes.sizeof(ctypes.c_void_p) != 8):
        raise OSError(errno.ENOSYS, "no known pidfd system call numbers on this machine")
    call = ctypes.CDLL(None, use_errno=True)["syscall"]  # a new object: its own argtypes
    call.restype, call.argtypes = ctypes.c_long, (ctypes.c_long,) + argtypes
    ret = call(number, *args)
    if ret < 0:
        err = ctypes.get_errno()
        raise OSError(err, os.strerror(err))
    return ret


def pidfd_open(pid):
    """Returns a pidfd of process pid: os.pidfd_open where this Python has
    it, else the system call. Standalone Python builds (uv, mise) on Linux
    lack os.pidfd_open and signal.pidfd_send_signal. ESRCH (no process)
    raises ProcessLookupError; ENOSYS and every other failure an OSError,
    ImportError or AttributeError."""
    if hasattr(os, "pidfd_open"):
        return os.pidfd_open(pid)
    import ctypes
    return linux_syscall(SYS_PIDFD_OPEN, (ctypes.c_int, ctypes.c_uint), pid, 0)


def pidfd_send_signal(fd, sig):
    """Sends sig to the process of the pidfd fd: signal.pidfd_send_signal
    where this Python has it, else the system call (no siginfo, no flags)."""
    if hasattr(signal, "pidfd_send_signal"):
        signal.pidfd_send_signal(fd, sig)
        return
    import ctypes
    linux_syscall(SYS_PIDFD_SEND_SIGNAL, (ctypes.c_int, ctypes.c_int, ctypes.c_void_p, ctypes.c_uint), fd, int(sig), None, 0)


class Process:
    """The process that the record's pid names, held from step 2 on."""

    def __init__(self, pid, kernel):
        self.pid, self.fd = pid, None
        if os.environ.get(NOT_PERMITTED_TEST):
            raise Result(CANNOT, "a process with the recorded pid runs, but this user may not signal it")
        if kernel == "Linux":
            try:
                self.fd = pidfd_open(pid)
            except ProcessLookupError:
                raise Result(GONE, "no process has the recorded pid")
            except (ImportError, AttributeError, OSError):
                raise Result(CANNOT, "this system cannot hold the process while it checks it")
        self.signal(0)

    def signal(self, sig):
        try:
            if self.fd is not None:
                pidfd_send_signal(self.fd, sig)
            else:
                os.kill(self.pid, sig)
        except ProcessLookupError:
            raise Result(GONE, "no process has the recorded pid")
        except PermissionError:
            raise Result(CANNOT, "a process with the recorded pid runs, but this user may not signal it")


def escaped_path(path):
    """Returns path on one line, as the header documents: every byte
    outside printable ASCII, the apostrophe and the backslash as \\0ooo
    (always three octal digits, so a following digit stays itself)."""
    out = []
    for b in os.fsencode(path):
        if 0x20 <= b < 0x7F and b not in (0x27, 0x5C):
            out.append(chr(b))
        else:
            out.append("\\0%03o" % b)
    return "".join(out)


def forget(path, data, st):
    """Removes the record at path, whose process is gone, only when path
    still names the file of the one read (st) with its content (data); see
    the header. It always raises the Result."""
    directory, name = os.path.split(path)
    aside = os.path.join(directory, "." + name + ".forget-" + secrets.token_hex(8))
    try:
        os.rename(path, aside)
    except FileNotFoundError:
        raise Result(GONE, "no process has the recorded pid, and the record no longer exists")
    except OSError:
        raise Result(CANNOT, "no process has the recorded pid, but the record cannot be moved aside for the comparison; it was kept")
    try:
        now, now_st = read_file(aside)
        same = (now_st.st_dev, now_st.st_ino) == (st.st_dev, st.st_ino) and now == data
    except Result:
        same = False
    kept = "; it was kept as " + escaped_path(aside)
    if same:
        try:
            os.unlink(aside)
        except OSError:
            raise Result(CANNOT, "no process has the recorded pid, but the record cannot be removed" + kept)
        raise Result(GONE, "no process has the recorded pid; the record was removed")
    try:
        os.link(aside, path, follow_symlinks=False)
    except (OSError, NotImplementedError):
        raise Result(CANNOT, "no process has the recorded pid, but the record changed after it was read, and another file took its path" + kept)
    try:
        os.unlink(aside)
    except OSError:
        raise Result(CANNOT, "no process has the recorded pid, but the record changed after it was read; it was kept, also as " + escaped_path(aside))
    raise Result(CANNOT, "no process has the recorded pid, but the record changed after it was read; it was kept")


def check(mode, path):
    text, data, st = read_record(path)
    try:
        identify(mode, path, text, st)
    except Result as r:
        if mode == "forget" and r.code == GONE:
            forget(path, data, st)
        raise


def identify(mode, path, text, st):
    """Runs the checks of the header on the record of the one read (text,
    file status st) and raises the Result; with mode "terminate" it sends
    SIGTERM on "verified"."""
    pid, start, token, var = parse(text, os.path.basename(path))
    if pid is None:
        raise Result(CANNOT, "the record names no pid (a decimal of at most 10 digits above 1)")
    # 1. The record, before the process: an invalid record is never
    # "gone", so --forget never removes one.
    if st.st_uid != os.getuid() or st.st_mode & 0o022:
        raise Result(CANNOT, "the record is not a private file of this user (group or others can write it, or another user owns it)")
    if not start or not token:
        raise Result(CANNOT, "the record holds no start time or no start token")
    # 2. Existence. The process handle and the signal follow the real kernel. The
    # external uname selects only the readers of the process data below
    # (the test seam of the header); each of them reads the real process
    # or fails closed with cannot-check, and none of them signals.
    kernel = os.uname().sysname
    system = (run(["uname", "-s"]) or "").strip()
    if kernel not in ("Linux", "Darwin") or system not in ("Linux", "Darwin"):
        raise Result(CANNOT, "this script cannot read processes on this system")
    proc = Process(pid, kernel)

    # 3. The start time. system may follow the seam: /proc exists only on a
    # Linux kernel (elsewhere the read fails: cannot-check), and ps reads
    # the real process on either kernel.
    if start.isdigit():
        stat_text = proc_file(pid, "stat") if system == "Linux" else None
        fields = stat_text.decode(errors="replace").rpartition(")")[2].split() if stat_text else []
        if len(fields) < 20:
            raise Result(CANNOT, "the start time of the process cannot be read")
        live_start = fields[19]  # field 22; the fields after "(comm)" start at field 3
    else:
        live_start = " ".join((run(["ps", "-p", str(pid), "-o", "lstart="]) or "").split())
        if not live_start:
            raise Result(CANNOT, "the start time of the process cannot be read")
    if live_start != start:
        raise Result(MISMATCH, "the process started at another time than the recorded one (a reused pid)")

    # 4 and 5. The environment and the program, with exact boundaries.
    # system may follow the seam: both readers read the real process
    # (darwin_procargs by the kernel), or fail closed with cannot-check.
    if system == "Linux":
        environ, cmdline = proc_file(pid, "environ"), proc_file(pid, "cmdline")
        if environ is None or cmdline is None:
            raise Result(CANNOT, "the environment or the arguments of the process cannot be read")
        env, args = environ.split(b"\0"), cmdline.split(b"\0")
    else:
        try:
            args, env = procargs_env(darwin_procargs(pid))
        except (OSError, ValueError):
            raise Result(CANNOT, "the arguments and the environment of the process cannot be read with exact boundaries")
    if (var + "=" + token).encode() not in env:
        raise Result(MISMATCH, "the process does not carry the recorded start token")
    if not args or os.path.basename(args[0]) != PROGRAM:
        raise Result(MISMATCH, "the process is not ios_webkit_debug_proxy")

    # 6. The checked process still runs (Linux: the pidfd's process).
    proc.signal(0)
    if mode == "terminate":
        proc.signal(signal.SIGTERM)
        raise Result(0, "the process is the recorded ios_webkit_debug_proxy; SIGTERM was sent to it")
    raise Result(0, "the process is the recorded ios_webkit_debug_proxy")


def main():
    try:
        check(sys.argv[1], sys.argv[2])
    except Result as r:
        code, reason = r.code, r.reason
    except BaseException:  # never a traceback, never a signal after it
        code, reason = CANNOT, "the check failed unexpectedly"
    print(f"{WORDS[code]}: {reason}")
    sys.exit(code)


if __name__ == "__main__":
    main()
PY
)
rc=$?
case "$rc:$out" in
  *$'\n'*) ;;
  "0:verified: "* | "1:mismatch: "* | "2:cannot-check: "* | "3:gone: "*)
    printf '%s\n' "$out"
    exit "$rc"
    ;;
esac
echo "cannot-check: the check failed unexpectedly"
exit 2
