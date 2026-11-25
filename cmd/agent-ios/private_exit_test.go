package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	reg "github.com/nnemirovsky/agent-ios/internal/commands"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Tests of the end of a private run on every exit path of the process
// (processExit.finish): reviewer findings F33 and F35 of the checker-trim
// closure review. In private mode (--private, AGENT_IOS_PRIVATE=1) stderr holds
// exactly one status line, printed last, and nothing else: no argument,
// error text, page or log line. Each test runs the same line without the
// mode too, which prints what it printed before.

// privateSentinel stands for a page, an argument or an error text that
// private mode must not print.
const privateSentinel = "PRIVATE-SENTINEL"

// privateEnvOn turns private mode on through the variable.
const privateEnvOn = "AGENT_IOS_PRIVATE=1"

// wantStatus fails the test unless stderr is exactly one private status
// line of command with code: "<command> exit 0" for a success,
// "<command> exit <code>: <meaning> (private mode hides the message)" for
// a failure (commands.privateErrorLine). The meaning comes from the exit
// code alone, so the test checks only its form.
func wantStatus(t *testing.T, what, stderr, command string, code int) {
	t.Helper()
	if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
		t.Errorf("%s: stderr %q, want exactly one status line", what, stderr)
		return
	}
	head := command + " exit " + itoa(int64(code))
	if code == 0 {
		if stderr != head+"\n" {
			t.Errorf("%s: stderr %q, want %q", what, stderr, head+"\n")
		}
		return
	}
	if !strings.HasPrefix(stderr, head+": ") || !strings.HasSuffix(stderr, " (private mode hides the message)\n") {
		t.Errorf("%s: stderr %q, want %q followed by the meaning of the code", what, stderr, head+": ")
	}
}

// overview is the overview of all commands that main prints (printUsage).
func overview() string {
	var b bytes.Buffer
	if err := printUsage(&b); err != nil {
		panic(err) // a bytes.Buffer takes every write
	}
	return b.String()
}

// F33 and F35: the lines that main or the registry answers without running
// a command. "help PRIVATE-SENTINEL" repeated the unknown argument in
// private mode, and help ended without a status line.
func TestPrivateHelpAndUsageLinesEndWithOneStatusLine(t *testing.T) {
	click, _ := reg.Lookup([]string{"click"})
	network, _ := reg.GroupHelp([]string{"network"})
	for _, tc := range []struct {
		args           []string
		code           int
		stdout         string // with and without the mode
		plainErr       string // stderr without the mode
		command        string // of the private status line
		privateHasHelp bool   // private mode prints stdout as without it
	}{
		{nil, reg.ExitUsage, "", overview(), "agent-ios", false},
		{[]string{"help"}, 0, overview(), "", "agent-ios", true},
		{[]string{"-h", "help", "--help"}, 0, overview(), "", "agent-ios", true},
		{[]string{"help", "click"}, 0, reg.Help(click), "", "click", true},
		{[]string{"help", "click\n"}, 0, reg.Help(click), "", "click", true},
		{[]string{"help", "network"}, 0, network, "", "network", true},
		{[]string{"help", privateSentinel}, reg.ExitUsage, "", "Error: unknown command \"" + privateSentinel + "\" (run 'agent-ios --help')\n", "agent-ios", false},
		{[]string{"click", "--help"}, 0, reg.Help(click), "", "click", true},
		{[]string{privateSentinel}, reg.ExitUsage, "", "Error: unknown command \"" + privateSentinel + "\"\n", "agent-ios", false},
		{[]string{"click", "--" + privateSentinel}, reg.ExitUsage, "", "Error: click: unknown flag --" + privateSentinel + " (run 'agent-ios click --help')\n", "agent-ios", false},
	} {
		plain := runCLI(t, "", nil, tc.args...)
		if plain.code != tc.code || plain.stdout != tc.stdout || plain.stderr != tc.plainErr {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want exit %d, stdout %q, stderr %q", tc.args, plain.code, plain.stdout, plain.stderr, tc.code, tc.stdout, tc.plainErr)
		}
		private := runCLI(t, "", []string{privateEnvOn}, tc.args...)
		what := strings.Join(tc.args, " ") + " with " + privateEnvOn
		if private.code != tc.code || private.stdout != tc.stdout {
			t.Errorf("%s: exit %d, stdout %q; want exit %d, stdout %q", what, private.code, private.stdout, tc.code, tc.stdout)
		}
		wantStatus(t, what, private.stderr, tc.command, tc.code)
		if strings.Contains(private.stderr, privateSentinel) {
			t.Errorf("%s: stderr %q repeats the argument", what, private.stderr)
		}
	}
}

// blockRegistryCommand is a registry command whose client step ignores
// its context and never ends, so that only the watchdog ends it.
const blockRegistryCommand = "test private block"

func init() {
	reg.Register(&reg.Command{
		Name: blockRegistryCommand, Summary: "Test fixture: a client step that ignores its deadline.", Page: reg.AnyPage,
		ClientStep: func(context.Context, *reg.Input, io.Reader) error {
			time.Sleep(time.Hour) // ignores the context, as a blocking read does
			return nil
		},
		Run: func(context.Context, reg.Env, reg.Input) (reg.Result, error) { return reg.Result{}, nil },
	})
}

// F33 (reviewer's fifo repro, watchdog part): under AGENT_IOS_NO_DAEMON=1 the
// watchdog ends a command that ignores its deadline. Without the mode it
// prints the deadline message, which names the page; in private mode only
// the status line of exit 124.
func TestPrivateWatchdogPrintsOnlyItsStatusLine(t *testing.T) {
	args := append([]string{"--page", privateSentinel, "--timeout", "10ms"}, strings.Fields(blockRegistryCommand)...)
	plain := runCLI(t, "", []string{"AGENT_IOS_NO_DAEMON=1"}, args...)
	if plain.code != exitTimeout {
		t.Errorf("exit %d, want %d; stderr %q", plain.code, exitTimeout, plain.stderr)
	}
	for _, want := range []string{"Error: agent-ios " + blockRegistryCommand + " timed out after", "Page: " + privateSentinel, errWatchdog.Error()} {
		if !strings.Contains(plain.stderr, want) {
			t.Errorf("stderr without the mode lacks %q: %q", want, plain.stderr)
		}
	}
	for _, tc := range []struct {
		env  []string
		args []string
	}{
		{[]string{"AGENT_IOS_NO_DAEMON=1", privateEnvOn}, args},
		{[]string{"AGENT_IOS_NO_DAEMON=1"}, append([]string{"--private"}, args...)},
	} {
		private := runCLI(t, "", tc.env, tc.args...)
		what := strings.Join(append(tc.env, tc.args...), " ")
		if private.code != exitTimeout || private.stdout != "" {
			t.Errorf("%s: exit %d, stdout %q; want %d and no stdout", what, private.code, private.stdout, exitTimeout)
		}
		wantStatus(t, what, private.stderr, blockRegistryCommand, exitTimeout)
	}
}

// F33 and F35 (reviewer's TestReviewPrivateCleanupEmitsExtraLine): the
// cleanup of the in-process core at the exit can print warnings that name
// what it sent, and the core logs page keys and errors. Private mode
// prints neither: only the command's status line.
func TestPrivateCleanupPrintsNoWarning(t *testing.T) {
	run := func(env ...string) (*fakewebkit.Server, *runningCLI) {
		f := fakewebkit.New(fakewebkit.Options{})
		t.Cleanup(f.Close)
		f.DelayReplies("Console.disable", cleanupPastDeadline)
		args := append(strings.Fields(setRegistryCommand), "--page", f.URL, "--timeout", shortDeadline)
		p := startCLI(t, append([]string{"AGENT_IOS_NO_DAEMON=1"}, env...), args...)
		if code := p.wait(t, cleanupLimit); code != 0 {
			t.Fatalf("exit %d; stderr %q", code, p.stderr.String())
		}
		return f, p
	}
	_, plain := run()
	if s := plain.stderr.String(); !strings.Contains(s, "warning: cleanup of the page state") || !strings.Contains(s, "Console.disable") {
		t.Errorf("stderr without the mode %q: want the cleanup warning that names Console.disable", s)
	}
	f, private := run(privateEnvOn)
	if n := len(f.ReceivedMethod("Console.disable")); n != 1 {
		t.Errorf("private mode: Console.disable sent %d times, want 1: the cleanup must still run", n)
	}
	wantStatus(t, "private cleanup", private.stderr.String(), setRegistryCommand, 0)
	if s := private.stderr.String(); strings.Contains(s, f.URL) {
		t.Errorf("private stderr %q names the page", s)
	}
}

// F35 (reviewer's TestReviewPrivateInterruptedStatus): a signal ends a
// command with exit 130. In private mode it ends with the status line of
// that code, under AGENT_IOS_NO_DAEMON=1 and with the daemon; without the mode
// nothing changes: the in-process command prints no error, and a daemon
// client is ended by the signal itself.
func TestPrivateSignalEndsWithItsStatusLine(t *testing.T) {
	interrupt := func(env ...string) (*runningCLI, int) {
		f := fakewebkit.New(fakewebkit.Options{})
		t.Cleanup(f.Close)
		f.DelayReplies("Runtime.evaluate", cleanupPastDeadline)
		p := startCLI(t, env, append(strings.Fields(heldRegistryCommand), "--page", f.URL)...)
		eventually(t, cleanupLimit, "the registry command's Runtime.evaluate", func() bool {
			select {
			case <-p.done:
				t.Fatalf("the command exited %d before the signal; stderr %q", p.code, p.stderr.String())
			default:
			}
			return len(f.ReceivedMethod("Runtime.evaluate")) == 1
		})
		if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		code := p.wait(t, cleanupLimit)
		if slices.Contains(env, privateEnvOn) && strings.Contains(p.stderr.String(), f.URL) {
			t.Errorf("%q: stderr %q names the page", env, p.stderr.String())
		}
		return p, code
	}
	t.Run("in process", func(t *testing.T) {
		plain, code := interrupt("AGENT_IOS_NO_DAEMON=1")
		if s := plain.stderr.String(); code != exitInterrupted || strings.Contains(s, "Error:") || strings.Contains(s, " exit ") {
			t.Errorf("without the mode: exit %d, stderr %q; want %d without an error or status line", code, s, exitInterrupted)
		}
		private, code := interrupt("AGENT_IOS_NO_DAEMON=1", privateEnvOn)
		if code != exitInterrupted {
			t.Errorf("private: exit %d, want %d", code, exitInterrupted)
		}
		wantStatus(t, "private in process", private.stderr.String(), heldRegistryCommand, exitInterrupted)
	})
	t.Run("daemon", func(t *testing.T) {
		plain, code := interrupt()
		if code != -1 || plain.stderr.String() != "" || !strings.Contains(plain.cmd.ProcessState.String(), "interrupt") {
			t.Errorf("without the mode: exit %d (%s), stderr %q; want the end by the signal and no stderr", code, plain.cmd.ProcessState, plain.stderr.String())
		}
		private, code := interrupt(privateEnvOn)
		if code != exitInterrupted {
			t.Errorf("private: exit %d (%s), want %d", code, private.cmd.ProcessState, exitInterrupted)
		}
		wantStatus(t, "private daemon client", private.stderr.String(), heldRegistryCommand, exitInterrupted)
	})
}

// A daemon that cannot start (its socket directory cannot be made) is a
// failure of the command: without the mode its error names the
// directory; in private mode only the status line.
func TestPrivateDaemonStartFailurePrintsOnlyItsStatusLine(t *testing.T) {
	file := filepath.Join(t.TempDir(), privateSentinel)
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"XDG_RUNTIME_DIR=" + file}
	plain := runCLI(t, "", env, "eval", "1")
	if plain.code != reg.ExitError || !strings.HasPrefix(plain.stderr, "Error: ") || !strings.Contains(plain.stderr, privateSentinel) {
		t.Errorf("without the mode: exit %d, stderr %q; want exit 1 and the error that names the directory", plain.code, plain.stderr)
	}
	private := runCLI(t, "", append(env, privateEnvOn), "eval", "1")
	if private.code != reg.ExitError || private.stdout != "" {
		t.Errorf("private: exit %d, stdout %q", private.code, private.stdout)
	}
	wantStatus(t, "private daemon start", private.stderr, "eval", reg.ExitError)
}

// The ends that processExit decides itself (reviewer's
// TestReviewPrivateInterruptedStatus, and the watchdog and cleanup parts of
// F33 and F35), with a fake cleanup and exit: in private mode stderr gets
// only the status line of the recorded command, never the deadline
// message (which names the page), a cleanup warning or the warning of a
// cleanup that did not end. Without the mode the same ends print what the
// tests of cli_w1b_watchdog_test.go check, and nothing for a signal.
func TestPrivateEndsThatTheProcessDecides(t *testing.T) {
	failed := func(f *fakeExit) {
		f.cleanup = func(context.Context) error { return errors.New("Console.disable of ws://page: not sent") }
	}
	for name, tc := range map[string]struct {
		hang    bool
		prepare func(f *fakeExit)
		end     func(f *fakeExit, o reg.Outcome)
		code    int
		plain   []string // in stderr without the mode
	}{
		"signal": {
			end:  func(f *fakeExit, _ reg.Outcome) { close(f.release); go f.signalled() },
			code: exitInterrupted,
		},
		"watchdog": {
			end: func(f *fakeExit, _ reg.Outcome) {
				armWatchdog(time.Millisecond, f.processExit)
				<-f.started
				close(f.release)
			},
			code:  exitTimeout,
			plain: []string{"agent-ios eval timed out", "Page: ws://page", errWatchdog.Error()},
		},
		"main after a failed cleanup": {
			prepare: failed,
			end:     func(f *fakeExit, o reg.Outcome) { go f.finish(o) },
			plain:   []string{"warning: cleanup of the page state: Console.disable of ws://page: not sent"},
		},
		"main after a cleanup that did not end": {
			hang:  true,
			end:   func(f *fakeExit, o reg.Outcome) { go f.finish(o) },
			plain: []string{"warning: the cleanup of the page state did not end within"},
		},
		"signal after a cleanup that did not end": {
			hang:  true,
			end:   func(f *fakeExit, _ reg.Outcome) { go f.signalled() },
			code:  exitInterrupted,
			plain: []string{"did not end within"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, private := range []bool{false, true} {
				f := newFakeExit(t, 50*time.Millisecond, 50*time.Millisecond, tc.hang)
				if tc.prepare != nil {
					tc.prepare(f)
				}
				f.record("eval", private)
				tc.end(f, reg.Outcome{Command: "eval", Private: private})
				if code := f.exitCode(t); code != tc.code {
					t.Errorf("private %v: exit %d, want %d", private, code, tc.code)
				}
				s := f.stderr.String()
				if private {
					wantStatus(t, name, s, "eval", tc.code)
					continue
				}
				for _, want := range tc.plain {
					if !strings.Contains(s, want) {
						t.Errorf("without the mode: stderr %q lacks %q", s, want)
					}
				}
				if len(tc.plain) == 0 && s != "" {
					t.Errorf("without the mode: stderr %q, want none", s)
				}
			}
		})
	}
}

// --- F40 (checker-trim final review): help writes, the process boundary
// and "daemon run" ---

// startWithStdout starts agent-ios with args and env, with stdout the file
// out (closed here once the process has it); wait collects its end.
func startWithStdout(t *testing.T, out *os.File, env []string, args ...string) *runningCLI {
	t.Helper()
	r := &runningCLI{cmd: cliCommand(t, "", env, args...), stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan struct{})}
	r.cmd.Stdout, r.cmd.Stderr = out, r.stderr
	if err := r.cmd.Start(); err != nil {
		t.Fatalf("starting agent-ios %v: %v", args, err)
	}
	_ = out.Close()
	go func() {
		err := r.cmd.Wait()
		var exitErr *exec.ExitError
		switch {
		case errors.As(err, &exitErr):
			r.code = exitErr.ExitCode() // -1 when a signal ended it
		case err != nil:
			r.code = -2
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

// helpLines are the help lines of both kinds: those that main answers
// itself (runLine) and those that the registry answers (RunCLIOutcome),
// each with the command of its private status line.
var helpLines = []struct {
	args    []string
	command string
}{
	{[]string{"--help"}, "agent-ios"},
	{[]string{"help"}, "agent-ios"},
	{[]string{"help", "click"}, "click"},
	{[]string{"help", "network"}, "network"},
	{[]string{"click", "--help"}, "click"},
	{[]string{"network", "--help"}, "network"},
}

// F40, contract 1: a help text that cannot be written (stdout is open
// only for reading, so every write fails with EBADF) ended the run with
// exit 0. It ends with exit 1: without private mode with the error line of
// the write, in private mode with only the status line of exit 1.
func TestHelpThatCannotBeWrittenEndsWithExit1(t *testing.T) {
	for _, line := range helpLines {
		for _, env := range [][]string{nil, {privateEnvOn}} {
			out, err := os.Open(os.DevNull) // read only
			if err != nil {
				t.Fatal(err)
			}
			p := startWithStdout(t, out, env, line.args...)
			code := p.wait(t, time.Until(testDeadline(t)))
			stderr := p.stderr.String()
			what := strings.Join(append(env, line.args...), " ")
			if code != reg.ExitError {
				t.Errorf("%s: exit %d (%s), stderr %q; want exit %d", what, code, p.cmd.ProcessState, stderr, reg.ExitError)
				continue
			}
			if env != nil {
				wantStatus(t, what, stderr, line.command, reg.ExitError)
				continue
			}
			if !strings.HasPrefix(stderr, "Error: writing the help: ") || !strings.Contains(stderr, "bad file descriptor") || strings.Count(stderr, "\n") != 1 {
				t.Errorf("%s: stderr %q; want one line \"Error: writing the help: ...\" that names the error", what, stderr)
			}
		}
	}
}

// F40, contract 1 and 2: a help text whose stdout is a pipe without a
// reader. Without private mode the write ends the process by SIGPIPE, as
// Go does by default, and nothing changes. Private mode turns the signal
// into a write error at the process boundary, so the run ends with the
// status line of exit 1 instead of no line.
func TestPrivateHelpIntoAClosedPipeEndsWithExit1(t *testing.T) {
	for _, line := range helpLines {
		for _, env := range [][]string{nil, {privateEnvOn}} {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			_ = r.Close()
			p := startWithStdout(t, w, env, line.args...)
			code := p.wait(t, time.Until(testDeadline(t)))
			stderr := p.stderr.String()
			what := strings.Join(append(env, line.args...), " ")
			if env == nil {
				if code != -1 || stderr != "" || !strings.Contains(p.cmd.ProcessState.String(), "broken pipe") {
					t.Errorf("%s: %s, stderr %q; want the end by SIGPIPE and no stderr", what, p.cmd.ProcessState, stderr)
				}
				continue
			}
			if code != reg.ExitError {
				t.Errorf("%s: exit %d (%s), stderr %q; want exit %d", what, code, p.cmd.ProcessState, stderr, reg.ExitError)
				continue
			}
			wantStatus(t, what, stderr, line.command, reg.ExitError)
		}
	}
}

// smallStdout returns a stdout of minimal buffers for a process: a
// stream socket whose send and receive buffers are as small as the system
// allows (a few kilobytes), far smaller than the overview (17 kB). It
// returns the process's end and the test's end, which the test reads from
// as far as it wants: a write of the process blocks when the buffers are
// full, so the process cannot end before the test reads the rest.
func smallStdout(t *testing.T) (w, r *os.File) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The kernel raises 1 to its minimum.
	if err := syscall.SetsockoptInt(fds[1], syscall.SOL_SOCKET, syscall.SO_SNDBUF, 1); err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetsockoptInt(fds[0], syscall.SOL_SOCKET, syscall.SO_RCVBUF, 1); err != nil {
		t.Fatal(err)
	}
	r = os.NewFile(uintptr(fds[0]), "stdout read end")
	t.Cleanup(func() { _ = r.Close() })
	return os.NewFile(uintptr(fds[1]), "stdout write end"), r
}

// readWithin reads len(buf) bytes of r within the test's deadline.
func readWithin(t *testing.T, r io.Reader, buf []byte) {
	t.Helper()
	read := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(r, buf)
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Until(testDeadline(t))):
		t.Fatalf("no output within the test's deadline")
	}
}

// F40, contract 2 (reviewer's help_signal.py): a SIGINT while the
// overview blocks on stdout ended a private run without its status line,
// because the signal handler of private mode was installed only for a
// registry command. Private mode installs it at the process boundary, so
// the run ends with the status line of exit 130. Without private mode the
// signal ends the process, as it did.
func TestPrivateSignalWhileHelpBlocksOnStdout(t *testing.T) {
	const head = "agent-ios" // the start of the overview
	for _, env := range [][]string{nil, {privateEnvOn}} {
		w, r := smallStdout(t)
		p := startWithStdout(t, w, env, "--help")
		// The process writes its help only after the process boundary,
		// and the rest of the overview does not fit into the buffers.
		got := make([]byte, len(head))
		readWithin(t, r, got)
		if string(got) != head {
			t.Fatalf("%q: stdout starts with %q, want %q", env, got, head)
		}
		select {
		case <-p.done:
			t.Fatalf("%q: agent-ios --help ended (%s) although its stdout blocks; stderr %q", env, p.cmd.ProcessState, p.stderr.String())
		default:
		}
		if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		code := p.wait(t, time.Until(testDeadline(t)))
		stderr := p.stderr.String()
		if env == nil {
			if code != -1 || stderr != "" || !strings.Contains(p.cmd.ProcessState.String(), "interrupt") {
				t.Errorf("without private mode: %s, stderr %q; want the end by the signal and no stderr", p.cmd.ProcessState, stderr)
			}
			continue
		}
		const want = "agent-ios exit 130: interrupted by a signal (private mode hides the message)\n"
		if code != exitInterrupted || stderr != want {
			t.Errorf("private: exit %d (%s), stderr %q; want exit %d and %q", code, p.cmd.ProcessState, stderr, exitInterrupted, want)
		}
	}
}

// logDigits are the numbers of a daemon log line (time, pid, the digits of
// the directory), which differ between two runs.
var logDigits = regexp.MustCompile(`[0-9]+`)

// F40, contract 3: "daemon run" is the daemon, not a client command, so
// private mode never applies to it. With AGENT_IOS_PRIVATE=1 or --private a
// foreground daemon logs on stderr, prints no status line and exits after
// a signal exactly as without them. The daemon runs in the test's private
// directories (XDG_RUNTIME_DIR from cliCommand, AGENT_IOS_DAEMON_DIR below it),
// never the user's.
func TestDaemonRunIgnoresPrivateMode(t *testing.T) {
	dir := filepath.Join(testRuntime(t), "foreground")
	run := func(sig os.Signal, env []string, args ...string) (int, string) {
		p := startCLI(t, append([]string{"AGENT_IOS_DAEMON_DIR=" + dir}, env...), append(args, "daemon", "run")...)
		eventually(t, time.Until(testDeadline(t)), "the daemon's line \"listening on\"", func() bool {
			select {
			case <-p.done:
				t.Fatalf("%q %q: daemon run ended with %d before it listened; stderr %q", env, args, p.code, p.stderr.String())
			default:
			}
			return strings.Contains(p.stderr.String(), " listening on ")
		})
		if err := p.cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		code := p.wait(t, time.Until(testDeadline(t)))
		return code, logDigits.ReplaceAllString(p.stderr.String(), "N")
	}
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		baseCode, baseLog := run(sig, nil)
		if baseCode != 0 || !strings.Contains(baseLog, "shutting down: signal") || !strings.HasSuffix(baseLog, " stopped\n") {
			t.Fatalf("%v without private mode: exit %d, log %q; want exit 0 and the log of a shutdown by the signal", sig, baseCode, baseLog)
		}
		for _, tc := range []struct {
			env, args []string
		}{
			{[]string{privateEnvOn}, nil},
			{nil, []string{"--private"}},
		} {
			code, log := run(sig, tc.env, tc.args...)
			if code != baseCode || log != baseLog {
				t.Errorf("%v with %q %q: exit %d, stderr %q; want exit %d and the same log as without private mode %q", sig, tc.env, tc.args, code, log, baseCode, baseLog)
			}
		}
	}
}

// F40, contract 3: the client commands of the daemon stay in private
// mode: "daemon start", "daemon status" and "daemon stop" print no result
// (they are no reads) and only their status line. The daemon that "daemon
// start" starts gets AGENT_IOS_PRIVATE=1 with the client's environment and runs
// as without it (TestDaemonRunIgnoresPrivateMode).
func TestPrivateDaemonClientCommandsPrintOnlyTheirStatus(t *testing.T) {
	const notRunning = "The agent-ios daemon is not running.\n"
	for _, step := range []struct {
		args    []string
		running bool // the daemon runs after the step (daemon status without private mode)
	}{
		{[]string{"daemon", "start"}, true},
		{[]string{"daemon", "status"}, true},
		{[]string{"daemon", "stop"}, false},
		{[]string{"daemon", "status"}, false},
	} {
		res := runCLI(t, "", []string{privateEnvOn}, step.args...)
		what := strings.Join(step.args, " ")
		if res.code != 0 || res.stdout != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit 0 and no stdout", what, res.code, res.stdout, res.stderr)
		}
		wantStatus(t, what, res.stderr, what, 0)
		if plain := runCLI(t, "", nil, "daemon", "status"); plain.code != 0 || (plain.stdout != notRunning) == !step.running {
			t.Errorf("after %s: daemon status without private mode: exit %d, stdout %q; want the daemon running %v", what, plain.code, plain.stdout, step.running)
		}
	}
}

// emitRegistryCommand is a Local registry command that streams one line
// (Env.Emit) into a stdout that refuses every write (it is open only for
// reading), then succeeds; its result goes to the stdout of the test.
const emitRegistryCommand = "test private emit"

func init() {
	reg.Register(&reg.Command{
		Name: emitRegistryCommand, Summary: "Test fixture: a Local command whose streamed line cannot be written.", Local: true,
		Run: func(_ context.Context, env reg.Env, _ reg.Input) (reg.Result, error) {
			readOnly, err := os.Open(os.DevNull)
			if err != nil {
				return reg.Result{}, err
			}
			defer func() { _ = readOnly.Close() }()
			stdout := os.Stdout
			os.Stdout = readOnly
			defer func() { os.Stdout = stdout }()
			env.Emit("a streamed line")
			return reg.Result{}, nil
		},
	})
}

// F40, contract 1: a streamed line (Env.Emit, a line item of the daemon)
// that could not be written on stdout was dropped, and the run succeeded.
// It ends with exit 1: without private mode with the error line of the
// write, in private mode with the status line of exit 1.
func TestStreamedLineThatCannotBeWrittenEndsWithExit1(t *testing.T) {
	o := runRegistry(strings.Fields(emitRegistryCommand))
	if o.Code != reg.ExitError || !strings.HasPrefix(o.Line(), "Error: writing the output: ") || !strings.Contains(o.Line(), "bad file descriptor") {
		t.Errorf("exit %d, line %q; want exit %d and \"Error: writing the output: ...\" that names the error", o.Code, o.Line(), reg.ExitError)
	}
	o = runRegistry(append([]string{"--private"}, strings.Fields(emitRegistryCommand)...))
	if o.Code != reg.ExitError {
		t.Errorf("private: exit %d, want %d", o.Code, reg.ExitError)
	}
	wantStatus(t, "private", o.Line()+"\n", emitRegistryCommand, reg.ExitError)
}

// knownHelp is printCommandHelp into a buffer, whose writes never fail:
// it reports whether name is a command or a group.
func knownHelp(b *bytes.Buffer, name string) bool {
	known, _ := printCommandHelp(b, name)
	return known
}
