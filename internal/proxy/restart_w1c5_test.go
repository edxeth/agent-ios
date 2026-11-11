package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/quick"
	"time"
)

// Hermetic tests of the iwdp restart (decision L02): Restart stops only an
// ios_webkit_debug_proxy that this program started, recorded by
// proxy.Starter and checked against the listening socket's owner, and
// never runs a pattern kill. Every iwdp here is a fake on a private
// loopback address; the tests run inside unshare -rn.

// recordPath is where Starter records the iwdp it started for addr: a
// file in the private directory iwdp-cli of XDG_RUNTIME_DIR (private to the
// test after installFakeIWDP, else TestMain's), named like the start lock.
// It creates the directory.
func recordPath(addr string) string {
	dir := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "iwdp-cli")
	_ = os.Mkdir(dir, 0o700)
	return filepath.Join(dir, fmt.Sprintf("iwdp-cli-started-%d-%s.json", os.Getuid(), strings.ReplaceAll(addr, ":", "_")))
}

// writeRecord writes a record of an iwdp with pid and process group pgid
// on addr, as another process of this program would.
func writeRecord(t *testing.T, addr string, pid, pgid int) {
	t.Helper()
	// The process's true start time when it runs, and a token that no
	// process of the test carries: a record of a reused pid.
	start := "1"
	if id, err := readIdentity(pid); err == nil {
		start = id.start
	}
	data := fmt.Sprintf(`{"pid":%d,"pgid":%d,"addr":%q,"start":%q,"token":"forged-token","writer":%q}`, pid, pgid, addr, start, recordWriter)
	if err := os.WriteFile(recordPath(addr), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// useStd makes s the Starter of Start, Restart and Status for this test.
func useStd(t *testing.T, s *Starter) {
	t.Helper()
	old := std
	t.Cleanup(func() { std = old })
	std = s
}

// pkillCalls returns the calls of the package's fake pkill so far.
func pkillCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(guardBin, "calls.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "pkill") && !strings.Contains(line, "--guard-check") {
			calls = append(calls, line)
		}
	}
	return calls
}

// foreignIWDP runs a fake ios_webkit_debug_proxy that this program did not
// start (the user's or another agent's): a process named
// ios_webkit_debug_proxy in its own process group, serving the device list
// on addr. It returns its pid; the test stops it by that pid, and it ends
// with this test process (exitWithParent).
func foreignIWDP(t *testing.T, addr string) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "ios_webkit_debug_proxy")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link, "-test.run=^TestFakeIWDPProcess$")
	cmd.Env = append(os.Environ(), fakeServeEnv+"="+addr, fakeParent())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if getListing(context.Background(), addr) == nil {
			return cmd.Process.Pid
		}
	}
	t.Fatalf("the foreign fake iwdp did not serve %s", addr)
	return 0
}

// sleeper runs a process in its own process group that holds no socket
// and returns its pid; the test stops it by that pid.
func sleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// deadPID returns the pid of a process that has exited and been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// checkUntouched fails the test unless every pid in pids is alive, addr
// still answers as iwdp, the fake ran no start and no pkill ran.
func checkUntouched(t *testing.T, f *fakeIWDP, addr string, pids ...int) {
	t.Helper()
	for _, pid := range pids {
		if !alive(pid) {
			t.Errorf("process %d was stopped; Restart must never signal a process it did not start", pid)
		}
	}
	if err := CheckListing(context.Background(), addr); err != nil {
		t.Errorf("the iwdp on %s no longer answers: %v", addr, err)
	}
	if got := f.starts(); len(got) != 0 {
		t.Errorf("Restart ran ios_webkit_debug_proxy %d times (%q) while another process owned %s", len(got), got, addr)
	}
	if calls := pkillCalls(t); len(calls) != 0 {
		t.Errorf("Restart ran pkill: %q", calls)
	}
}

// L02: a restart of the iwdp that this program started stops that
// process group and starts a new iwdp. Another Starter (another process of
// this program) finds it through the record file; the Starter that
// started it also without the file.
func TestRestartRestartsTheIWDPThisProgramStarted(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	useStd(t, testStarter(addr, 10*time.Second))
	if err := Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	restart := func(step string, wantStarts int) {
		t.Helper()
		before := f.pids()
		if err := Restart(context.Background()); err != nil {
			t.Fatalf("%s: Restart of the iwdp this program started: %v", step, err)
		}
		for _, pid := range before {
			if alive(pid) {
				t.Errorf("%s: the old iwdp %d is still running after Restart", step, pid)
			}
		}
		pids := f.pids()
		if len(pids) != wantStarts {
			t.Fatalf("%s: ios_webkit_debug_proxy ran %d times, want %d", step, len(pids), wantStarts)
		}
		if err := CheckListing(context.Background(), addr); err != nil {
			t.Fatalf("%s: the restarted iwdp does not answer: %v", step, err)
		}
		data, err := os.ReadFile(recordPath(addr))
		want := fmt.Sprintf(`"pid":%d,"pgid":%d`, pids[len(pids)-1], pids[len(pids)-1])
		if err != nil || !strings.Contains(string(data), want) {
			t.Errorf("%s: record = %q, %v; want the new iwdp (%s)", step, data, err, want)
		}
	}
	restart("same Starter", 2)
	useStd(t, testStarter(addr, 10*time.Second))
	restart("another Starter, through the record file", 3)
	if err := os.Remove(recordPath(addr)); err != nil {
		t.Fatal(err)
	}
	restart("the Starter that started it, without the record file", 4)
	if calls := pkillCalls(t); len(calls) != 0 {
		t.Errorf("Restart ran pkill: %q", calls)
	}
}

// WS2b (the MCP server is removed): an iwdp that an earlier iwdp-mcp
// started has a record without the writer field. Restart counts it as
// another program's iwdp: it refuses with the reason and stops nothing, so
// the user stops it by hand once (docs/migration.md).
func TestRestartRefusesARecordWithoutTheWriter(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	useStd(t, testStarter(addr, 10*time.Second))
	if err := Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	data, err := os.ReadFile(recordPath(addr))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("record %q: %v", data, err)
	}
	if rec["writer"] != recordWriter {
		t.Fatalf("the record of a start = %s, want the writer %q", data, recordWriter)
	}
	delete(rec, "writer")
	old, _ := json.Marshal(rec) // strings and numbers always encode
	if err := os.WriteFile(recordPath(addr), old, 0o600); err != nil {
		t.Fatal(err)
	}
	// Another process of iwdp-cli finds the iwdp only through the file.
	useStd(t, testStarter(addr, 10*time.Second))
	pids := f.pids()
	err = Restart(context.Background())
	if err == nil || !strings.Contains(err.Error(), "restart refused") || !strings.Contains(err.Error(), "not written by iwdp-cli") {
		t.Fatalf("Restart with the record %s = %v, want a refusal that names the missing writer", old, err)
	}
	for _, pid := range pids {
		if !alive(pid) {
			t.Errorf("the iwdp %d of the record without the writer was stopped", pid)
		}
	}
	if n := len(f.pids()); n != len(pids) {
		t.Errorf("ios_webkit_debug_proxy ran %d times, want %d (no new start)", n, len(pids))
	}
	if err := CheckListing(context.Background(), addr); err != nil {
		t.Errorf("the iwdp on %s no longer answers: %v", addr, err)
	}
	if calls := pkillCalls(t); len(calls) != 0 {
		t.Errorf("Restart ran pkill: %q", calls)
	}
}

// The record of a start replaces a record file left at the same path, mode
// included (saveRecord: a new 0600 file, renamed over the old one). A
// group-writable file left behind (by hand, or by an older program) would
// otherwise stay group-writable, and every later restart would refuse it.
func TestStartReplacesARecordLeftWithAnotherMode(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	if err := os.WriteFile(recordPath(addr), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(recordPath(addr), 0o620); err != nil {
		t.Fatal(err)
	}
	useStd(t, testStarter(addr, 10*time.Second))
	if err := Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fi, err := os.Lstat(recordPath(addr))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the record after Start: %v, %v; want mode 0600", fi, err)
	}
	if err := Restart(context.Background()); err != nil {
		t.Fatalf("Restart of the iwdp that Start recorded: %v", err)
	}
	if n := len(f.starts()); n != 2 {
		t.Errorf("ios_webkit_debug_proxy ran %d times, want 2 (the start and the restart)", n)
	}
}

// L02: an iwdp that this program did not start is never signaled, also
// when its command name is ios_webkit_debug_proxy. Restart refuses and
// names the address, the owner pid, the checked stop of the runbook and
// the manual start command, and prints no kill command (WS3 round 3).
func TestRestartNeverSignalsAnotherProgramsIWDP(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	foreign := foreignIWDP(t, addr)
	useStd(t, testStarter(addr, 2*time.Second))
	err := Restart(context.Background())
	checkUntouched(t, f, addr, foreign)
	if err == nil {
		t.Fatal("Restart succeeded against an iwdp that this program did not start")
	}
	t.Logf("refusal: %v", err)
	for _, want := range []string{addr, "pid " + strconv.Itoa(foreign), "Stopping an iwdp by hand", wantManualIWDP()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "kill") {
		t.Errorf("refusal %q prints a kill command", err)
	}
}

// L02: a record that no longer owns the listening socket is stale. Restart
// refuses, and signals neither the recorded process (alive or not) nor the
// owner.
func TestRestartRefusesAStaleRecord(t *testing.T) {
	for _, c := range []struct {
		name string
		pid  func(t *testing.T) int
	}{
		{"recorded process alive", sleeper},
		{"recorded process gone", deadPID},
	} {
		t.Run(c.name, func(t *testing.T) {
			addr := freeAddr(t)
			f := installFakeIWDP(t, fakeFail, addr)
			foreign := foreignIWDP(t, addr)
			recorded := c.pid(t)
			writeRecord(t, addr, recorded, recorded)
			useStd(t, testStarter(addr, 2*time.Second))
			err := Restart(context.Background())
			if err == nil {
				t.Fatal("Restart succeeded with a stale record")
			}
			for _, want := range []string{addr, "pid " + strconv.Itoa(foreign), strconv.Itoa(recorded), wantManualIWDP()} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
			pids := []int{foreign}
			if alive(recorded) {
				pids = append(pids, recorded)
			}
			checkUntouched(t, f, addr, pids...)
		})
	}
}

// L02: a record that cannot name a process group of an iwdp is refused
// without a signal: unreadable, for another address, or a group that
// would signal every process (1), this process's group (0, or its own
// number), or none. The listing here is served by this test process.
func TestRestartRefusesAnInvalidRecord(t *testing.T) {
	self := syscall.Getpgrp()
	for _, c := range []struct {
		name string
		data func(addr string, other int) string
	}{
		{"not JSON", func(string, int) string { return "pid=1" }},
		{"another address", func(_ string, other int) string {
			return fmt.Sprintf(`{"pid":%d,"pgid":%d,"addr":"127.0.0.1:1"}`, other, other)
		}},
		{"group 0", func(addr string, other int) string { return fmt.Sprintf(`{"pid":%d,"pgid":0,"addr":%q}`, other, addr) }},
		{"group 1", func(addr string, _ int) string { return fmt.Sprintf(`{"pid":1,"pgid":1,"addr":%q}`, addr) }},
		{"negative group", func(addr string, other int) string {
			return fmt.Sprintf(`{"pid":%d,"pgid":%d,"addr":%q}`, other, -other, addr)
		}},
		{"this process's group", func(addr string, _ int) string {
			return fmt.Sprintf(`{"pid":%d,"pgid":%d,"addr":%q}`, self, self, addr)
		}},
		{"pid is not the group leader", func(addr string, other int) string {
			return fmt.Sprintf(`{"pid":%d,"pgid":%d,"addr":%q}`, os.Getpid(), other, addr)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			addr := freeAddr(t)
			f := installFakeIWDP(t, fakeFail, addr)
			serveListing(t, addr)
			other := sleeper(t)
			data := c.data(addr, other)
			if err := os.WriteFile(recordPath(addr), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			useStd(t, testStarter(addr, 2*time.Second))
			err := Restart(context.Background())
			if err == nil || !strings.Contains(err.Error(), addr) || !strings.Contains(err.Error(), wantManualIWDP()) {
				t.Fatalf("Restart with the record %s = %v, want a refusal naming %s and the manual command", data, err, addr)
			}
			checkUntouched(t, f, addr, other)
		})
	}
}

// L02, crash recovery: when nothing owns the listing address there is
// nothing to stop, and Restart starts iwdp. A stale record's process is
// left alone.
func TestRestartStartsIWDPWhenNothingListens(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	recorded := sleeper(t)
	writeRecord(t, addr, recorded, recorded)
	useStd(t, testStarter(addr, 10*time.Second))
	if err := Restart(context.Background()); err != nil {
		t.Fatalf("Restart with nothing on %s: %v", addr, err)
	}
	if !alive(recorded) {
		t.Errorf("the stale record's process %d was stopped", recorded)
	}
	if n := len(f.starts()); n != 1 {
		t.Errorf("ios_webkit_debug_proxy ran %d times, want 1", n)
	}
	if err := CheckListing(context.Background(), addr); err != nil {
		t.Errorf("the started iwdp does not answer: %v", err)
	}
}

// readPIDs reads one pid per line.
func readPIDs(t *testing.T, path string) []int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, f := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	return pids
}

// L02: when the owner lookup fails, the refusal still names the address
// and says that the owner is unknown. The lookup fails on both systems:
// Linux's socket table is missing, and darwin's lsof (a fake first on
// PATH) fails.
func TestRestartRefusalWithAnUnknownOwner(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	serveListing(t, addr)
	old := socketTables
	t.Cleanup(func() { socketTables = old })
	socketTables.tcp = filepath.Join(t.TempDir(), "missing")
	lsofDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(lsofDir, "lsof"), []byte("#!/bin/sh\necho 'lsof: no socket table' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", lsofDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	useStd(t, testStarter(addr, 2*time.Second))
	err := Restart(context.Background())
	if err == nil || !strings.Contains(err.Error(), addr) || !strings.Contains(err.Error(), "owner pid unknown") {
		t.Fatalf("Restart with a failing owner lookup = %v, want a refusal naming %s and an unknown owner", err, addr)
	}
	checkUntouched(t, f, addr)
}

// Restart needs a numeric listing address: the ownership check compares
// socket addresses.
func TestRestartNeedsAnIPAddress(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	serveListing(t, addr)
	_, port, _ := net.SplitHostPort(addr)
	useStd(t, testStarter("localhost:"+port, 2*time.Second))
	err := Restart(context.Background())
	if err == nil || !strings.Contains(err.Error(), "localhost:"+port) {
		t.Fatalf("Restart on a host name = %v, want an error naming the address", err)
	}
	checkUntouched(t, f, addr)
}

// L02: a record file that is a symbolic link (a shared temporary
// directory) is not read; Restart refuses.
func TestRestartRefusesASymlinkRecord(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	serveListing(t, addr)
	target := filepath.Join(t.TempDir(), "record.json")
	other := sleeper(t)
	if err := os.WriteFile(target, []byte(fmt.Sprintf(`{"pid":%d,"pgid":%d,"addr":%q}`, other, other, addr)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, recordPath(addr)); err != nil {
		t.Fatal(err)
	}
	useStd(t, testStarter(addr, 2*time.Second))
	err := Restart(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reading the iwdp record") || !strings.Contains(err.Error(), addr) {
		t.Fatalf("Restart with a symlinked record = %v, want a refusal that names the unreadable record and %s", err, addr)
	}
	checkUntouched(t, f, addr, other)
}

// L02: every iwdp this program starts has a record. When the record
// cannot be written, the started iwdp is stopped and Start fails, so no
// unrecorded iwdp of this program is left that a restart would refuse.
func TestStartStopsAnIWDPItCannotRecord(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	// A directory that is not empty cannot be replaced by the record file.
	if err := os.MkdirAll(filepath.Join(recordPath(addr), "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(recordPath(addr)) })
	err := testStarter(addr, 10*time.Second).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "recording the started ios_webkit_debug_proxy") {
		t.Fatalf("Start with an unwritable record = %v, want the recording error", err)
	}
	for _, pid := range f.pids() {
		if alive(pid) {
			t.Errorf("the unrecorded iwdp %d is still running", pid)
		}
	}
	if left, _ := filepath.Glob(recordPath(addr) + ".*"); len(left) != 0 {
		t.Errorf("temporary record files are left: %v", left)
	}
}

// quickSeed fixes the random source of the property tests; 0 picks a new
// seed per run. Each property logs its seed, so a failure can be run
// again with -args -proxy.quickseed=<seed>.
var quickSeed = flag.Int64("proxy.quickseed", 0, "seed of the property tests (0: a new seed per run)")

// quickConfig is a testing/quick configuration of n cases with the seed of
// quickSeed, which it logs.
func quickConfig(t *testing.T, n int) *quick.Config {
	t.Helper()
	seed := *quickSeed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	t.Logf("property seed %d (run again with -args -proxy.quickseed=%d)", seed, seed)
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

// L02, property over broadly generated records, most of them rejected: a
// record that problem accepts names a process group whose signal reaches
// only that group: its number is above 1, it is not this process's group,
// its leader is the recorded pid, it is for the Starter's address, and it
// holds the identity. TestRecordProblemAcceptsEverySafeRecord generates
// the accepted records.
func TestRecordProblemAcceptsOnlyASafeGroup(t *testing.T) {
	const addr = "127.0.0.1:9221"
	self := syscall.Getpgrp()
	accepted, rejected := 0, 0
	prop := func(pid, pgid int32, sameAddr, useSelf, hasStart, hasToken, hasWriter bool) bool {
		rec := record{PID: int(pid % 64), PGID: int(pgid % 64), Addr: "127.0.0.1:1"}
		if sameAddr {
			rec.Addr = addr
		}
		if useSelf {
			rec.PGID = self
		}
		if hasStart {
			rec.Start = "100"
		}
		if hasToken {
			rec.Token = "tok"
		}
		if hasWriter {
			rec.Writer = recordWriter
		}
		if rec.problem(addr) != "" {
			rejected++
			return true
		}
		accepted++
		return rec.PGID > 1 && rec.PGID != self && rec.PID == rec.PGID && rec.Addr == addr && rec.Start != "" && rec.Token != "" && rec.Writer == recordWriter
	}
	if err := quick.Check(prop, quickConfig(t, 20000)); err != nil {
		t.Fatal(err)
	}
	// Most generated records are unsafe; an accepted one is a bonus here.
	t.Logf("%d records rejected, %d accepted", rejected, accepted)
	if rejected == 0 {
		t.Fatal("no generated record was rejected; the property was never tested on an unsafe record")
	}
	if p := (&record{PID: 4242, PGID: 4242, Addr: addr, Start: "100", Token: "tok", Writer: recordWriter}).problem(addr); p != "" {
		t.Fatalf("a valid record has the problem %q", p)
	}
}

// L02, property over constructed safe records: problem accepts every
// record of a process group above 1 that is not this process's group, led
// by the recorded pid, for the Starter's address and with the identity;
// and each single unsafe change of such a record is rejected.
func TestRecordProblemAcceptsEverySafeRecord(t *testing.T) {
	const addr = "127.0.0.1:9221"
	self := syscall.Getpgrp()
	const groups = 1 << 22 // above any pid_max of Linux and darwin
	accepted := 0
	prop := func(n uint32, start, token string) bool {
		pgid := 2 + int(n%groups)
		if pgid >= self {
			pgid++ // skip this process's group
		}
		rec := record{PID: pgid, PGID: pgid, Addr: addr, Start: "s" + start, Token: "t" + token, Writer: recordWriter}
		if p := rec.problem(addr); p != "" {
			t.Logf("the safe record %+v has the problem %q", rec, p)
			return false
		}
		accepted++
		for name, change := range map[string]func(*record){
			"group 1":            func(r *record) { r.PID, r.PGID = 1, 1 },
			"group 0":            func(r *record) { r.PID, r.PGID = 0, 0 },
			"negative group":     func(r *record) { r.PID, r.PGID = -pgid, -pgid },
			"this process group": func(r *record) { r.PID, r.PGID = self, self },
			"pid not the leader": func(r *record) { r.PID = pgid + 1 },
			"another address":    func(r *record) { r.Addr = "127.0.0.1:1" },
			"no start time":      func(r *record) { r.Start = "" },
			"no token":           func(r *record) { r.Token = "" },
			"no writer":          func(r *record) { r.Writer = "" },
		} {
			bad := rec
			change(&bad)
			if bad.problem(addr) == "" {
				t.Logf("the record %+v (%s) was accepted", bad, name)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(t, 2000)); err != nil {
		t.Fatal(err)
	}
	if accepted == 0 {
		t.Fatal("no generated record was accepted; the property was never tested on an accepted record")
	}
}

// Restart takes the start lock: while another process starts or restarts
// iwdp, it waits, and a caller that gives up gets the wait's error and
// stops nothing.
func TestRestartWaitsForTheStartLock(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	foreign := foreignIWDP(t, addr)
	unlock, err := lockStart(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	useStd(t, testStarter(addr, 2*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = Restart(ctx)
	if err == nil || !strings.Contains(err.Error(), "waiting for another process's iwdp start") {
		t.Fatalf("Restart while another start holds the lock = %v, want the lock wait error", err)
	}
	checkUntouched(t, f, addr, foreign)
}

// When the ownership check fails, Restart stops nothing and says so.
func TestRestartStopsNothingWhenTheOwnerCheckFails(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	s := testStarter(addr, 10*time.Second)
	useStd(t, s)
	if err := Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.listens = func(context.Context, int, netip.AddrPort) (listener, error) {
		return 0, errors.New("no socket table")
	}
	err := Restart(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no socket table") || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Fatalf("Restart with a failing ownership check = %v, want that error and nothing stopped", err)
	}
	for _, pid := range f.pids() {
		if !alive(pid) {
			t.Errorf("the started iwdp %d was stopped although the ownership check failed", pid)
		}
	}
	if n := len(f.starts()); n != 1 {
		t.Errorf("ios_webkit_debug_proxy ran %d times, want 1", n)
	}
}
