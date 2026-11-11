package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Review round 1 of W1c-5: a record must be authenticated (F2) and name
// the process incarnation that this program started (F3). Every refused
// case leaves the foreign fake iwdp alive and running.

// Review F2 (reviewer's TestReviewRejectsWorldWritableRecord): a record
// that others can write is not trusted.
func TestReviewRejectsWorldWritableRecord(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	foreign := foreignIWDP(t, addr)
	writeRecord(t, addr, foreign, foreign)
	if err := os.Chmod(recordPath(addr), 0o666); err != nil {
		t.Fatal(err)
	}
	useStd(t, testStarter(addr, 2*time.Second))
	err := Restart(context.Background())
	if !alive(foreign) {
		t.Fatalf("a writable record authorized stopping the foreign listener %d; Restart returned %v", foreign, err)
	}
	checkUntouched(t, f, addr, foreign)
}

// Review F3 (reviewer's TestReviewRejectsReusedProcessIdentity): a record
// with only the numeric pid and group of the current listener (a reused
// pid) does not name the process incarnation that this program started.
func TestReviewRejectsReusedProcessIdentity(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	foreign := foreignIWDP(t, addr)
	writeRecord(t, addr, foreign, foreign)
	useStd(t, testStarter(addr, 2*time.Second))
	err := Restart(context.Background())
	if !alive(foreign) {
		t.Fatalf("numeric PID/PGID reuse state authorized stopping the foreign listener %d; Restart returned %v", foreign, err)
	}
	checkUntouched(t, f, addr, foreign)
}

// F3: a forged record that names the foreign listener with its true, public
// start time and a made-up token is refused: the foreign process does not
// carry the token that Starter gives the iwdp it starts.
func TestRestartRefusesAForgedRecordOfAForeignListener(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	foreign := foreignIWDP(t, addr)
	id, err := readIdentity(foreign)
	if err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"pid":%d,"pgid":%d,"addr":%q,"start":%q,"token":"0123456789abcdef0123456789abcdef","writer":%q}`, foreign, foreign, addr, id.start, recordWriter)
	if err := os.WriteFile(recordPath(addr), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	useStd(t, testStarter(addr, 2*time.Second))
	err = Restart(context.Background())
	if !alive(foreign) {
		t.Fatalf("a forged record with the foreign listener's start time stopped it (%d); Restart returned %v", foreign, err)
	}
	checkUntouched(t, f, addr, foreign)
	if err == nil || !strings.Contains(err.Error(), "restart refused") {
		t.Errorf("Restart with a forged record = %v, want a refusal", err)
	}
	// The refusal states the ownership rule: only an iwdp that iwdp-cli
	// started and recorded can be stopped (review WS1 S2 F1).
	const rule = "not an ios_webkit_debug_proxy that iwdp-cli started and recorded"
	if err != nil && !strings.Contains(err.Error(), rule) {
		t.Errorf("the refusal %q does not state the ownership rule %q", err, rule)
	}
}

// fakeStubbornEnv makes TestFakeStubbornIWDPProcess serve the device list
// on the address it names while it ignores SIGTERM; fakeStubbornChildEnv
// names the file where it writes the pid of its child.
const (
	fakeStubbornEnv      = "IWDP_PROXY_TEST_FAKE_STUBBORN"
	fakeStubbornChildEnv = "IWDP_PROXY_TEST_FAKE_STUBBORN_CHILD"
)

// TestFakeStubbornIWDPProcess is the body of the stubborn fake iwdp: it
// ignores SIGTERM, starts a child (sleep) in its process group, writes the
// child's pid, and serves an empty device list until it is killed or its
// test process ends (exitWithParent, which also kills the child). Without
// the variable it returns at once.
func TestFakeStubbornIWDPProcess(t *testing.T) {
	addr := os.Getenv(fakeStubbornEnv)
	if addr == "" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	child := exec.Command("sleep", "300")
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "stubborn fake:", err)
		os.Exit(3)
	}
	exitWithParent(func() { _ = child.Process.Kill() })
	if err := os.WriteFile(os.Getenv(fakeStubbornChildEnv), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "stubborn fake:", err)
		os.Exit(3)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stubborn fake:", err)
		os.Exit(3)
	}
	err = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	fmt.Fprintln(os.Stderr, "stubborn fake:", err)
	os.Exit(3)
}

// installStubbornIWDP makes the fake iwdp f the stubborn one; it returns
// the file of its child's pid. The test stops both by their pids.
func installStubbornIWDP(t *testing.T, f *fakeIWDP, addr string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	childFile := filepath.Join(f.dir, "stubborn-child.pid")
	script := "#!/bin/sh\necho \"$$ $*\" >> " + q(filepath.Join(f.dir, "starts.log")) + "\n" +
		"exec env " + fakeParent() + " " + fakeStubbornEnv + "=" + q(addr) + " " + fakeStubbornChildEnv + "=" + q(childFile) + " " + q(exe) + " -test.run='^TestFakeStubbornIWDPProcess$'\n"
	if err := os.WriteFile(filepath.Join(f.dir, "ios_webkit_debug_proxy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if data, err := os.ReadFile(childFile); err == nil {
			if pid, err := strconv.Atoi(string(data)); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	return childFile
}

// startStubborn starts the stubborn fake iwdp through s and returns its
// fake set and the pids of its group: the fake and its child.
func startStubborn(t *testing.T, s *Starter, addr string) (*fakeIWDP, []int) {
	t.Helper()
	f := installFakeIWDP(t, fakeServe, addr)
	childFile := installStubbornIWDP(t, f, addr)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start of the stubborn fake: %v", err)
	}
	waitFile(t, childFile)
	group := append(f.pids(), readPIDs(t, childFile)...)
	if len(group) != 2 {
		t.Fatalf("the stubborn fake's processes = %v, want the fake and its child", group)
	}
	return f, group
}

// L02: an iwdp of this program that ignores SIGTERM and keeps its listener
// is stopped with SIGKILL, its whole process group, before the new start.
func TestRestartStopsAStubbornIWDP(t *testing.T) {
	addr := freeAddr(t)
	s := testStarter(addr, 10*time.Second)
	useStd(t, s)
	_, group := startStubborn(t, s, addr)
	var signals []syscall.Signal
	s.kill = func(pid int, sig syscall.Signal) error {
		signals = append(signals, sig)
		return syscall.Kill(pid, sig)
	}
	// The next start serves normally, so Restart can finish.
	installFakeIWDP(t, fakeServe, addr)
	if err := Restart(context.Background()); err != nil {
		t.Fatalf("Restart of a stubborn iwdp: %v", err)
	}
	for _, pid := range group {
		if alive(pid) {
			t.Errorf("process %d of the stubborn iwdp is still running after Restart", pid)
		}
	}
	if fmt.Sprint(signals) != fmt.Sprint([]syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Errorf("Restart sent %v, want SIGTERM then SIGKILL", signals)
	}
	if err := CheckListing(context.Background(), addr); err != nil {
		t.Errorf("the restarted iwdp does not answer: %v", err)
	}
}

// F3: when the recorded process's identity no longer matches after
// SIGTERM (its pid could have been reused), no SIGKILL is sent and Restart
// fails.
func TestRestartSendsNoSIGKILLWhenTheIdentityChanges(t *testing.T) {
	addr := freeAddr(t)
	s := testStarter(addr, 10*time.Second)
	useStd(t, s)
	_, group := startStubborn(t, s, addr)
	var termSent atomic.Bool
	var signals []syscall.Signal
	s.kill = func(pid int, sig syscall.Signal) error {
		signals = append(signals, sig)
		if sig == syscall.SIGTERM {
			termSent.Store(true)
		}
		return syscall.Kill(pid, sig)
	}
	s.identify = func(pid int) (identity, error) {
		id, err := readIdentity(pid)
		if termSent.Load() {
			id.start += "0" // another incarnation with the same pid
		}
		return id, err
	}
	err := Restart(context.Background())
	if !errors.Is(err, ErrStopFailed) || !strings.Contains(err.Error(), "no SIGKILL was sent") {
		t.Fatalf("Restart with a changed identity after SIGTERM = %v, want no SIGKILL", err)
	}
	if fmt.Sprint(signals) != fmt.Sprint([]syscall.Signal{syscall.SIGTERM}) {
		t.Errorf("Restart sent %v, want only SIGTERM", signals)
	}
	for _, pid := range group {
		if !alive(pid) {
			t.Errorf("process %d was killed although its identity changed", pid)
		}
	}
}

// F3: a record of this program's own iwdp with another start time or
// another token does not name the live process: refused, nothing signaled.
// The rule is the same with this system's reader and with darwin's
// reader (review round 2, F9: the token-only mismatch is refused on
// darwin too).
func TestRestartRefusesARecordWithAnotherIdentity(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*record)
	}{
		{"another start time", func(r *record) { r.Start += "0" }},
		{"another token", func(r *record) { r.Token = newToken() }},
		{"no token", func(r *record) { r.Token = "" }},
	} {
		for reader, identify := range identityReaders() {
			t.Run(c.name+"/"+reader, func(t *testing.T) {
				addr := freeAddr(t)
				f := installFakeIWDP(t, fakeServe, addr)
				s := testStarter(addr, 10*time.Second)
				s.identify = identify
				useStd(t, s)
				if err := Start(context.Background()); err != nil {
					t.Fatalf("Start: %v", err)
				}
				rec := *s.own
				c.change(&rec)
				s.own = &rec // the in-memory record follows the same rules
				data, err := json.Marshal(rec)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(recordPath(addr), data, 0o600); err != nil {
					t.Fatal(err)
				}
				s.kill = func(pid int, sig syscall.Signal) error {
					t.Errorf("Restart sent %v to %d", sig, pid)
					return nil
				}
				err = Restart(context.Background())
				if err == nil || !strings.Contains(err.Error(), "restart refused") {
					t.Fatalf("Restart with %s = %v, want a refusal", c.name, err)
				}
				if err := os.Remove(recordPath(addr)); err != nil {
					t.Fatal(err)
				}
				if err := Restart(context.Background()); err == nil || !strings.Contains(err.Error(), "restart refused") {
					t.Fatalf("Restart with only the in-memory record (%s) = %v, want a refusal", c.name, err)
				}
				if n := len(f.starts()); n != 1 {
					t.Errorf("ios_webkit_debug_proxy ran %d times, want 1", n)
				}
				for _, pid := range f.pids() {
					if !alive(pid) {
						t.Errorf("the started iwdp %d was stopped", pid)
					}
				}
			})
		}
	}
}

// F2: a record directory that others can write, or a record that is not
// a regular file, is not trusted: refused, nothing signaled. The record
// itself is this program's valid record, so only the storage decides.
func TestRestartRefusesAnUnsafeRecordStore(t *testing.T) {
	for _, c := range []struct {
		name   string
		damage func(t *testing.T, path string)
		want   string
	}{
		{"writable directory", func(t *testing.T, path string) {
			dir := filepath.Dir(path)
			if err := os.Chmod(dir, 0o777); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		}, "not private"},
		{"group-writable record", func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o620); err != nil {
				t.Fatal(err)
			}
		}, "others must not have"},
		{"FIFO record", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			addr := freeAddr(t)
			f := installFakeIWDP(t, fakeServe, addr)
			s := testStarter(addr, 10*time.Second)
			useStd(t, s)
			if err := Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			c.damage(t, recordPath(addr))
			s.kill = func(pid int, sig syscall.Signal) error {
				t.Errorf("Restart sent %v to %d", sig, pid)
				return nil
			}
			err := Restart(context.Background())
			if err == nil || !strings.Contains(err.Error(), "restart refused") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Restart with a %s = %v, want a refusal that says %q", c.name, err, c.want)
			}
			if n := len(f.starts()); n != 1 {
				t.Errorf("ios_webkit_debug_proxy ran %d times, want 1", n)
			}
		})
	}
}

// F2: recordDir uses XDG_RUNTIME_DIR only when it is this user's and others
// cannot write it; else a 0700 directory of this user in the temporary
// directory, which must pass the same check.
func TestRecordDirIsPrivate(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	fallback := filepath.Join(tmp, fmt.Sprintf("iwdp-cli-%d", os.Getuid()))
	xdg := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	if dir, err := recordDir(); err != nil || dir != filepath.Join(xdg, "iwdp-cli") {
		t.Fatalf("recordDir with a private XDG_RUNTIME_DIR = %q, %v; want %s/iwdp-cli", dir, err, xdg)
	}
	if err := os.Chmod(xdg, 0o777); err != nil {
		t.Fatal(err)
	}
	dir, err := recordDir()
	if err != nil || dir != fallback {
		t.Fatalf("recordDir with a writable XDG_RUNTIME_DIR = %q, %v; want %s", dir, err, fallback)
	}
	if fi, err := os.Lstat(fallback); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the fallback directory: %v, %v; want mode 0700", fi, err)
	}
	if err := os.Chmod(fallback, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := recordDir(); err == nil {
		t.Error("recordDir accepted a fallback directory with mode 0755")
	}
	if err := os.Remove(fallback); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), fallback); err != nil {
		t.Fatal(err)
	}
	if _, err := recordDir(); err == nil {
		t.Error("recordDir accepted a symlink")
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", filepath.Join(tmp, "missing"))
	if _, err := recordDir(); err == nil {
		t.Error("recordDir succeeded in a missing temporary directory")
	}
}

// F2: checkPrivate refuses another user's file ("/" belongs to root, or
// to the overflow uid inside the test namespace) and the wrong file type.
func TestCheckPrivateRefusesAnotherUsersFile(t *testing.T) {
	if err := checkPrivate("/", true, 0o022); err == nil || !strings.Contains(err.Error(), "not owned by this user") {
		t.Errorf("checkPrivate(/) = %v, want the owner error", err)
	}
	if err := checkPrivate(t.TempDir(), false, 0o022); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("checkPrivate(a directory as a file) = %v, want the type error", err)
	}
	if err := checkPrivate(filepath.Join(t.TempDir(), "missing"), true, 0o022); err == nil {
		t.Error("checkPrivate of a missing path succeeded")
	}
}

// F2: when the record directory is unsafe, a start stops the iwdp it
// cannot record (no unrecorded iwdp of this program is left).
func TestStartFailsWithAnUnsafeRecordDirectory(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	t.Setenv("XDG_RUNTIME_DIR", "")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	if err := os.Mkdir(filepath.Join(tmp, fmt.Sprintf("iwdp-cli-%d", os.Getuid())), 0o755); err != nil {
		t.Fatal(err)
	}
	err := testStarter(addr, 10*time.Second).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "recording the started ios_webkit_debug_proxy") {
		t.Fatalf("Start with an unsafe record directory = %v, want the recording error", err)
	}
	for _, pid := range f.pids() {
		if alive(pid) {
			t.Errorf("the unrecorded iwdp %d is still running", pid)
		}
	}
}

// F5: a SIGTERM that the system refuses (permission denied) fails the
// restart; no SIGKILL follows and no new iwdp is started.
func TestRestartReportsARefusedSignal(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	s := testStarter(addr, 10*time.Second)
	useStd(t, s)
	if err := Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var signals []syscall.Signal
	s.kill = func(_ int, sig syscall.Signal) error {
		signals = append(signals, sig)
		return syscall.EPERM
	}
	err := Restart(context.Background())
	if !errors.Is(err, ErrStopFailed) || !errors.Is(err, syscall.EPERM) || !strings.Contains(err.Error(), "stopping ios_webkit_debug_proxy") {
		t.Fatalf("Restart with a refused SIGTERM = %v, want the EPERM error", err)
	}
	if fmt.Sprint(signals) != fmt.Sprint([]syscall.Signal{syscall.SIGTERM}) {
		t.Errorf("Restart sent %v, want only SIGTERM", signals)
	}
	if n := len(f.starts()); n != 1 {
		t.Errorf("ios_webkit_debug_proxy ran %d times, want 1", n)
	}
}

// F3: the identity readers on this system. readIdentity of this process
// finds no token (it was not started with one); psStart reads a start
// time; both fail for a gone process. Linux's /proc reader is tested in
// identity_linux_test.go, darwin's environment reader with injected
// buffers in restart_r2_w1c5_test.go.
func TestIdentityReaders(t *testing.T) {
	if id, err := readIdentity(os.Getpid()); err != nil || id.start == "" || id.token != "" {
		t.Errorf("readIdentity of this process = %+v, %v; want a start time and no token", id, err)
	}
	if start, err := psStart(os.Getpid()); err != nil || start == "" {
		t.Errorf("psStart of this process = %q, %v; want a start time", start, err)
	}
	gone := deadPID(t)
	if id, err := readIdentity(gone); err == nil {
		t.Errorf("readIdentity of a gone process = %+v, want an error", id)
	}
	if start, err := psStart(gone); err == nil {
		t.Errorf("psStart of a gone process = %q, want an error", start)
	}
}

// F3: matches accepts only the recorded start time together with the
// recorded token, on every system (review round 2: a record alone never
// authorizes a signal); a record without a start time or a token matches
// nothing.
func TestRecordMatchesOnlyItsIncarnation(t *testing.T) {
	rec := record{Start: "100", Token: "tok"}
	for _, c := range []struct {
		id   identity
		want bool
	}{
		{identity{start: "100", token: "tok"}, true},
		{identity{start: "101", token: "tok"}, false},
		{identity{start: "100", token: "other"}, false},
		{identity{start: "100"}, false}, // no token: not started by this program
		{identity{start: "101"}, false},
	} {
		if got := rec.matches(c.id); got != c.want {
			t.Errorf("matches(%+v) = %v, want %v", c.id, got, c.want)
		}
	}
	if (&record{Token: "tok"}).matches(identity{token: "tok"}) {
		t.Error("a record without a start time matched")
	}
	if (&record{Start: "100"}).matches(identity{start: "100"}) {
		t.Error("a record without a token matched a process without one")
	}
}

// F3: the live recorded iwdp (identity matches) whose group no longer owns
// the listener (another process took it) is not signaled: refused.
func TestRestartRefusesWhenTheRecordedIWDPNoLongerListens(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	s := testStarter(addr, 10*time.Second)
	useStd(t, s)
	if err := Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.listens = func(context.Context, int, netip.AddrPort) (listener, error) { return listenerForeign, nil }
	s.kill = func(pid int, sig syscall.Signal) error {
		t.Errorf("Restart sent %v to %d", sig, pid)
		return nil
	}
	err := Restart(context.Background())
	if err == nil || !strings.Contains(err.Error(), "restart refused") || !strings.Contains(err.Error(), "no longer owns it") {
		t.Fatalf("Restart when another process holds the listener = %v, want a refusal", err)
	}
	if n := len(f.starts()); n != 1 {
		t.Errorf("ios_webkit_debug_proxy ran %d times, want 1", n)
	}
}
