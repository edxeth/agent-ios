package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/quick"
	"time"
)

// Review round 2 of W1c-5: a failed stop never counts as a restart (F8);
// darwin authenticates a record by the start token in the live process's
// environment, read with sysctl KERN_PROCARGS2, and refuses when it cannot
// read it (F3). darwin's reader runs here with injected KERN_PROCARGS2
// buffers: nativeProcargs builds them from /proc on Linux
// (identity_linux_test.go) and reads the system's own on darwin
// (identity_darwin_test.go).

// F8: when the SIGKILL escalation fails (the system refuses it) or the
// process group does not end after it, Restart fails with ErrStopFailed.
// It keeps the record file and the in-memory record, starts no new iwdp,
// and the old iwdp keeps serving: a later restart can try again.
func TestRestartFailsWhenTheEscalationFails(t *testing.T) {
	for _, c := range []struct {
		name string
		kill func(pid int, sig syscall.Signal) error // SIGKILL only
	}{
		{"SIGKILL refused", func(int, syscall.Signal) error { return syscall.EPERM }},
		{"group does not end", func(int, syscall.Signal) error { return nil }}, // the signal is lost
	} {
		t.Run(c.name, func(t *testing.T) {
			addr := freeAddr(t)
			s := testStarter(addr, 10*time.Second)
			useStd(t, s)
			f, group := startStubborn(t, s, addr)
			before, err := os.ReadFile(recordPath(addr))
			if err != nil {
				t.Fatal(err)
			}
			own := *s.own
			var signals []syscall.Signal
			s.kill = func(pid int, sig syscall.Signal) error {
				signals = append(signals, sig)
				if sig == syscall.SIGKILL {
					return c.kill(pid, sig)
				}
				return syscall.Kill(pid, sig)
			}
			err = Restart(context.Background())
			if !errors.Is(err, ErrStopFailed) || !strings.Contains(err.Error(), "SIGKILL") {
				t.Fatalf("Restart with %s = %v, want ErrStopFailed that names SIGKILL", c.name, err)
			}
			if c.name == "SIGKILL refused" && !errors.Is(err, syscall.EPERM) {
				t.Errorf("Restart = %v, want the EPERM of the SIGKILL", err)
			}
			if fmt.Sprint(signals) != fmt.Sprint([]syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
				t.Errorf("Restart sent %v, want SIGTERM then SIGKILL", signals)
			}
			for _, pid := range group {
				if !alive(pid) {
					t.Errorf("process %d of the old iwdp is gone; the test's kill never delivered SIGKILL", pid)
				}
			}
			if after, err := os.ReadFile(recordPath(addr)); err != nil || string(after) != string(before) {
				t.Errorf("record after the failed stop = %q, %v; want it kept: %q", after, err, before)
			}
			if s.own == nil || *s.own != own {
				t.Errorf("in-memory record after the failed stop = %+v, want it kept: %+v", s.own, own)
			}
			if n := len(f.starts()); n != 1 {
				t.Errorf("ios_webkit_debug_proxy ran %d times, want 1 (no start after a failed stop)", n)
			}
			if err := CheckListing(context.Background(), addr); err != nil {
				t.Errorf("the old iwdp no longer answers: %v", err)
			}
		})
	}
}

// F8: after the recorded iwdp was stopped, only a new start completes the
// restart. When another process serves the address by then, Restart fails
// and does not report that process as the restarted iwdp.
func TestRestartAfterAStopNeverAcceptsAnotherServer(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	s := testStarter(addr, 10*time.Second)
	useStd(t, s)
	if err := Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.kill = func(pid int, sig syscall.Signal) error {
		err := syscall.Kill(pid, sig)
		// The group exits on SIGTERM; then another server takes the address.
		for end := time.Now().Add(5 * time.Second); portFree(context.Background(), addr) != nil; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("%s is still in use after SIGTERM", addr)
			}
		}
		serveListing(t, addr)
		return err
	}
	err := Restart(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no new ios_webkit_debug_proxy was started") || !strings.Contains(err.Error(), addr) {
		t.Fatalf("Restart when another server took the address after the stop = %v, want an error naming %s", err, addr)
	}
	if n := len(f.starts()); n != 1 {
		t.Errorf("ios_webkit_debug_proxy ran %d times, want 1", n)
	}
}

// procargsBuffer builds a KERN_PROCARGS2 buffer as darwin lays it out: the
// argument count (native-endian 32 bits), the executable path, pad NULs,
// the arguments, the environment, pad NULs and darwin's own strings, each
// string NUL-terminated (xnu bsd/kern/kern_exec.c exec_copyout_strings,
// bsd/kern/kern_sysctl.c sysctl_procargsx).
func procargsBuffer(path string, args, env, apple []string, pad int) []byte {
	buf := binary.NativeEndian.AppendUint32(nil, uint32(len(args)))
	buf = append(append(buf, path...), 0)
	buf = append(buf, make([]byte, pad)...)
	for _, a := range args {
		buf = append(append(buf, a...), 0)
	}
	for _, e := range env {
		buf = append(append(buf, e...), 0)
	}
	buf = append(buf, make([]byte, pad)...)
	for _, a := range apple {
		buf = append(append(buf, a...), 0)
	}
	return buf
}

// darwinReader is darwin's identity reader (argsIdentity: ps for the start
// time) with the KERN_PROCARGS2 reader procargs.
func darwinReader(procargs func(int) ([]byte, error)) func(int) (identity, error) {
	return func(pid int) (identity, error) { return argsIdentity(pid, psStart, procargs) }
}

// identityReaders are the identity readers whose rules a restart test
// checks: this system's (readIdentity) and darwin's on this system's
// processes (darwinReader with nativeProcargs).
func identityReaders() map[string]func(int) (identity, error) {
	return map[string]func(int) (identity, error){
		"native": readIdentity,
		"darwin": darwinReader(nativeProcargs),
	}
}

// F3 (darwin): with darwin's reader, Restart restarts the iwdp that this
// program started: its record holds the ps start time, and the live
// process carries the recorded token in its environment.
func TestRestartWithTheDarwinReaderRestartsItsIWDP(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	s := testStarter(addr, 10*time.Second)
	s.identify = darwinReader(nativeProcargs)
	useStd(t, s)
	if err := Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	old := f.pids()
	if err := Restart(context.Background()); err != nil {
		t.Fatalf("Restart with darwin's reader: %v", err)
	}
	for _, pid := range old {
		if alive(pid) {
			t.Errorf("the old iwdp %d is still running after Restart", pid)
		}
	}
	if n := len(f.starts()); n != 2 {
		t.Errorf("ios_webkit_debug_proxy ran %d times, want 2", n)
	}
}

// F3 (darwin; the reviewer's TestReviewDarwinRejectsForgedRecord): a
// private record that another program of this user forged, naming the
// foreign listener with its true start time and a made-up token, is
// refused with darwin's reader: the foreign process does not carry the
// token.
func TestRestartWithTheDarwinReaderRefusesAForgedRecord(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	foreign := foreignIWDP(t, addr)
	start, err := psStart(foreign)
	if err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"pid":%d,"pgid":%d,"addr":%q,"start":%q,"token":"made-up-token","writer":%q}`, foreign, foreign, addr, start, recordWriter)
	if err := os.WriteFile(recordPath(addr), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	s := testStarter(addr, 2*time.Second)
	s.identify = darwinReader(nativeProcargs)
	useStd(t, s)
	err = Restart(context.Background())
	if !alive(foreign) {
		t.Fatalf("darwin's reader accepted a forged private record and stopped the foreign listener %d; Restart returned %v", foreign, err)
	}
	checkUntouched(t, f, addr, foreign)
	if err == nil || !strings.Contains(err.Error(), "restart refused") || !strings.Contains(err.Error(), "start token differs") {
		t.Errorf("Restart with a forged record = %v, want a refusal for the token", err)
	}
}

// F3 (darwin): when the environment of the recorded iwdp cannot be read
// (sysctl KERN_PROCARGS2 is denied or fails, its buffer is malformed, or
// darwin leaves out the environment of a restricted process), Restart
// refuses and signals nothing, also for the iwdp this program started
// and recorded in this process.
func TestRestartWithTheDarwinReaderFailsClosed(t *testing.T) {
	for _, c := range []struct {
		name     string
		procargs func(pid int) ([]byte, error)
		want     string
	}{
		{"denied", func(int) ([]byte, error) { return nil, syscall.EPERM }, "KERN_PROCARGS2"},
		{"another user's process", func(int) ([]byte, error) { return nil, syscall.EINVAL }, "KERN_PROCARGS2"},
		{"malformed buffer", func(int) ([]byte, error) { return []byte{1, 0}, nil }, "too few"},
		{"no environment", func(pid int) ([]byte, error) {
			return procargsBuffer("/usr/local/bin/ios_webkit_debug_proxy", []string{"ios_webkit_debug_proxy"}, nil, nil, 0), nil
		}, "start token differs"},
	} {
		t.Run(c.name, func(t *testing.T) {
			addr := freeAddr(t)
			f := installFakeIWDP(t, fakeServe, addr)
			s := testStarter(addr, 10*time.Second)
			s.identify = darwinReader(nativeProcargs)
			useStd(t, s)
			if err := Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			s.identify = darwinReader(c.procargs)
			s.kill = func(pid int, sig syscall.Signal) error {
				t.Errorf("Restart sent %v to %d", sig, pid)
				return nil
			}
			err := Restart(context.Background())
			if err == nil || !strings.Contains(err.Error(), "restart refused") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Restart when the environment read gives %s = %v, want a refusal that says %q", c.name, err, c.want)
			}
			for _, pid := range f.pids() {
				if !alive(pid) {
					t.Errorf("the started iwdp %d was stopped", pid)
				}
			}
			if n := len(f.starts()); n != 1 {
				t.Errorf("ios_webkit_debug_proxy ran %d times, want 1", n)
			}
		})
	}
}

// F3 (darwin): argsIdentity takes the start time from its start reader and
// the token from the environment in the KERN_PROCARGS2 buffer, never from
// the arguments; every failed read is an error.
func TestArgsIdentity(t *testing.T) {
	start := func(int) (string, error) { return "Sun Nov 8 06:14:04 2026", nil }
	buf := procargsBuffer("/opt/homebrew/bin/ios_webkit_debug_proxy",
		[]string{"ios_webkit_debug_proxy", "-F", tokenEnv + "=from-an-argument"},
		[]string{"HOME=/Users/u", tokenEnv + "=tok-1", "PATH=/usr/bin"},
		[]string{"MallocNanoZone=1", "ptr_munge=0x1"}, 5)
	id, err := argsIdentity(7, start, func(int) ([]byte, error) { return buf, nil })
	if err != nil || id != (identity{start: "Sun Nov 8 06:14:04 2026", token: "tok-1"}) {
		t.Errorf("argsIdentity = %+v, %v; want the start time and tok-1", id, err)
	}
	argOnly := procargsBuffer("/bin/x", []string{"x", tokenEnv + "=forged"}, []string{"HOME=/"}, nil, 0)
	if id, err := argsIdentity(7, start, func(int) ([]byte, error) { return argOnly, nil }); err != nil || id.token != "" {
		t.Errorf("argsIdentity with the token only in an argument = %+v, %v; want no token", id, err)
	}
	if _, err := argsIdentity(7, func(int) (string, error) { return "", errors.New("ps found no process 7") }, nil); err == nil || !strings.Contains(err.Error(), "ps found no process") {
		t.Errorf("argsIdentity with a failing start reader = %v, want its error", err)
	}
	_, err = argsIdentity(7, start, func(int) ([]byte, error) { return nil, syscall.EPERM })
	if !errors.Is(err, syscall.EPERM) || !strings.Contains(err.Error(), "KERN_PROCARGS2") {
		t.Errorf("argsIdentity with a denied read = %v, want the EPERM", err)
	}
}

// F3 (darwin): procargsEnv parses darwin's layout and fails on a buffer
// that ends inside a string.
func TestProcargsEnvExamples(t *testing.T) {
	buf := procargsBuffer("/bin/sleep", []string{"sleep", "300"}, []string{"A=1", "B="}, []string{"pfz=0x1"}, 2)
	if env, err := procargsEnv(buf); err != nil || !slices.Equal(env, []string{"A=1", "B=", "pfz=0x1"}) {
		t.Errorf("procargsEnv = %q, %v; want the environment and darwin's strings", env, err)
	}
	if env, err := procargsEnv(procargsBuffer("/bin/sleep", []string{"sleep"}, nil, nil, 0)); err != nil || len(env) != 0 {
		t.Errorf("procargsEnv without an environment = %q, %v; want none", env, err)
	}
	for name, bad := range map[string][]byte{
		"empty":                {},
		"short count":          {1, 0, 0},
		"unterminated path":    append(binary.NativeEndian.AppendUint32(nil, 0), "/bin/x"...),
		"missing argument":     procargsBuffer("/bin/x", []string{"x"}, nil, nil, 0)[:len(procargsBuffer("/bin/x", []string{"x"}, nil, nil, 0))-1],
		"count above the args": append(binary.NativeEndian.AppendUint32(nil, 3), "/bin/x\x00x\x00"...),
		"unterminated env":     append(procargsBuffer("/bin/x", []string{"x"}, nil, nil, 0), "A=1"...),
	} {
		if env, err := procargsEnv(bad); err == nil {
			t.Errorf("procargsEnv(%s) = %q, want an error", name, env)
		}
	}
}

// F3 (darwin), property: for every buffer in darwin's layout, procargsEnv
// returns exactly the nonempty strings after the arguments (the
// environment, then darwin's strings), whatever the arguments hold; and
// for any bytes it returns without panicking, only nonempty strings
// without NUL when it succeeds.
func TestProcargsEnvProperty(t *testing.T) {
	clean := func(prefix string, in []string) []string {
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = prefix + strings.ReplaceAll(s, "\x00", "_")
		}
		return out
	}
	layout := func(path string, args, env, apple []string, pad uint8) bool {
		args = clean("a", args) // an empty argument would read as padding
		env, apple = clean("E", env), clean("p", apple)
		got, err := procargsEnv(procargsBuffer("/"+strings.ReplaceAll(path, "\x00", "_"), args, env, apple, int(pad%8)))
		want := append(slices.Clone(env), apple...)
		if err != nil || !slices.Equal(got, want) {
			t.Logf("procargsEnv(args %q, env %q, apple %q) = %q, %v", args, env, apple, got, err)
			return false
		}
		return true
	}
	if err := quick.Check(layout, quickConfig(t, 2000)); err != nil {
		t.Fatal(err)
	}
	anyBytes := func(buf []byte) bool {
		env, err := procargsEnv(buf)
		if err != nil {
			return true
		}
		for _, s := range env {
			if s == "" || strings.Contains(s, "\x00") {
				return false
			}
		}
		return true
	}
	if err := quick.Check(anyBytes, quickConfig(t, 5000)); err != nil {
		t.Fatal(err)
	}
}

// F8: a group whose processes have all exited, but wait as zombies for
// their parent to reap them, is gone: it holds no socket and runs no
// code. A group with a live process is not gone. A failing ps keeps the
// group (it is not known to be gone).
func TestGroupGoneCountsZombiesAsGone(t *testing.T) {
	zombie := exec.Command("true")
	zombie.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	// Not reaped until the end of the test: a zombie of this process.
	t.Cleanup(func() { _ = zombie.Wait() })
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if live, err := groupLive(zombie.Process.Pid); err == nil && !live {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("process %d did not become a zombie", zombie.Process.Pid)
		}
	}
	if err := syscall.Kill(-zombie.Process.Pid, 0); err != nil {
		t.Fatalf("the zombie's group %d is not visible to kill(0): %v", zombie.Process.Pid, err)
	}
	if !groupGone(zombie.Process.Pid, 50*time.Millisecond) {
		t.Errorf("groupGone of a group of zombies only = false, want true")
	}
	live := sleeper(t)
	if groupGone(live, 50*time.Millisecond) {
		t.Errorf("groupGone of a group with a live process = true, want false")
	}
	if ok, err := groupLive(live); err != nil || !ok {
		t.Errorf("groupLive of a live group = %v, %v; want true", ok, err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := groupLive(zombie.Process.Pid); err == nil {
		t.Error("groupLive with a failing ps succeeded")
	}
	if groupGone(zombie.Process.Pid, 50*time.Millisecond) {
		t.Error("groupGone with a failing ps = true, want false")
	}
}
