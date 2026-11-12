package broadwayshm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Tests of the log and of End's wait that need neither a C compiler nor
// /dev/shm; the tests of the library and of the removal are in
// broadwayshm_shim_linux_test.go, with the WebKitGTK suite's build tag.

// startTime returns field 22 of /proc/<pid>/stat.
func startTime(t *testing.T, pid int) string {
	t.Helper()
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(stat)
	return strings.Fields(s[strings.LastIndexByte(s, ')')+1:])[19]
}

// markLine is the log line of a process that loaded the library.
func markLine(t *testing.T, pid int) string {
	t.Helper()
	return fmt.Sprintf("@%d %s\n", pid, startTime(t, pid))
}

// recorderDir makes a recorder's own directory in dir, as NewRecorder
// does (its owner file), without compiling the library.
func recorderDir(t *testing.T, dir, name string) string {
	t.Helper()
	sub := filepath.Join(dir, dirPrefix+name)
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ownerFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return sub
}

// startSleep starts sleep and returns it; the test ends it.
func startSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "60")
	c.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	return c
}

func TestReadLog(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		t.Helper()
		p := filepath.Join(dir, "x.log")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	rec, err := ReadLog(write("@12 99\n+1 2 /bdw-a\n@13 100\n+3 4 /bdw-b\n"))
	want := Record{Procs: []Marked{{12, "99"}, {13, "100"}}, Created: []Created{{Dir + "/bdw-a", 1, 2}, {Dir + "/bdw-b", 3, 4}}}
	if err != nil || !reflect.DeepEqual(rec, want) {
		t.Fatalf("ReadLog = %+v, %v; want %+v", rec, err, want)
	}
	for _, bad := range []string{
		"",                          // no process loaded the library
		"+1 2 /bdw-a\n",             // the same
		"@12\n",                     // no start time
		"@12 x\n",                   // not a start time
		"@12 9\n+1 2 /etc/passwd\n", // not a Broadway name
		"@12 9\n+1 2 /bdw-\n",
		"@12 9\n+1 2 /bdw-a/../x\n",
		"@12 9\n+x 2 /bdw-a\n",
		"@12 9\n-/bdw-a\n",
		"@12 9\n+1 2 /bdw-" + strings.Repeat("a", 70000) + "\n+3 4 /bdw-b\n", // too long
	} {
		if rec, err := ReadLog(write(bad)); err == nil {
			t.Errorf("ReadLog of %.40q = %+v, nil; want an error", bad, rec)
		}
	}
	if _, err := ReadLog(filepath.Join(dir, "absent.log")); err == nil || !strings.Contains(err.Error(), "did not run") {
		t.Fatalf("ReadLog of a missing log = %v, want an error that the recorder did not run", err)
	}
}

func TestRecorderEnvAndPaths(t *testing.T) {
	rec := &Recorder{Lib: "/x/shm.so", Log: "/x/shm.log"}
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	got := rec.Env([]string{"A=1", "LD_PRELOAD=/x/dpi.so"})
	want := []string{"A=1", "LD_PRELOAD=/x/dpi.so /x/shm.so", LogEnv + "=/x/shm.log", PIDNSEnv + "=" + ns}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Env = %q, want %q", got, want)
	}
	// LD_PRELOAD splits at spaces and colons: such a directory is refused
	// before anything is built.
	for _, d := range []string{"has space", "has:colon", "has\ttab"} {
		dir := filepath.Join(t.TempDir(), d)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if r, err := NewRecorder(dir); err == nil || !strings.Contains(err.Error(), "LD_PRELOAD") {
			t.Errorf("NewRecorder(%q) = %+v, %v; want a refusal", dir, r, err)
		}
	}
	// Keep removes the directory only after a complete cleanup.
	dir := t.TempDir()
	keep := filepath.Join(dir, "kept")
	if err := os.Mkdir(keep, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Keep(keep, fmt.Errorf("cleanup failed")); err == nil || !strings.Contains(err.Error(), keep) {
		t.Fatalf("Keep after a failed cleanup = %v, want the error with the directory", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("Keep removed the directory after a failed cleanup: %v", err)
	}
	if err := Keep(keep, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); !os.IsNotExist(err) {
		t.Fatalf("Keep after a complete cleanup left the directory (%v)", err)
	}
}

// End waits for each marked process before it reads the log for the last
// time: a record that a process appends during the wait is read. A
// reused pid (another start time) is not waited for. A process that does
// not end within Wait is an error, and nothing is removed.
func TestEndWaitsAndRereads(t *testing.T) {
	dir := t.TempDir()
	sub := recorderDir(t, dir, "w")
	log := filepath.Join(sub, logPrefix+"w"+logSuffix)
	writer, reused := startSleep(t), startSleep(t)
	content := markLine(t, writer.Process.Pid) + fmt.Sprintf("@%d 1\n", reused.Process.Pid)
	if err := os.WriteFile(log, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ended := make(chan time.Time, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		// A record appended during the wait, with a malformed name: End
		// must read it after the wait and fail on it.
		f, _ := os.OpenFile(log, os.O_WRONLY|os.O_APPEND, 0)
		_, _ = f.WriteString("+1 2 not-a-name\n")
		_ = f.Close()
		_ = writer.Process.Kill()
		_ = writer.Wait()
		ended <- time.Now()
	}()
	err := EndIn(dir)
	returned := time.Now()
	if at := <-ended; returned.Before(at) {
		t.Fatal("End returned before the marked process ended")
	}
	if err == nil || !strings.Contains(err.Error(), "not-a-name") {
		t.Fatalf("EndIn = %v, want the error of the record appended during the wait", err)
	}

	stuck := startSleep(t)
	if _, err := os.Stat(sub); err != nil { // kept by the failed EndIn
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte(markLine(t, stuck.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := End(log); err == nil || !strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("End with a process that does not end = %v, want an error", err)
	}
}

// A process whose state cannot be read is not taken for ended.
func TestRunsReportsUnreadableState(t *testing.T) {
	old := procFS
	t.Cleanup(func() { procFS = old })
	procFS = t.TempDir()
	if err := os.MkdirAll(filepath.Join(procFS, "42", "stat"), 0o700); err != nil { // a directory: unreadable as a file
		t.Fatal(err)
	}
	if alive, err := runs(Marked{PID: 42, Start: "1"}); err == nil {
		t.Fatalf("runs of an unreadable stat = %v, nil; want an error", alive)
	}
	if alive, err := runs(Marked{PID: 43, Start: "1"}); alive || err != nil {
		t.Fatalf("runs of a gone process = %v, %v; want false, nil", alive, err)
	}
}

// EndIn reads its directory literally (glob characters in the path are
// names) and fails without a log or on a directory it cannot read.
func TestEndInReadsTheDirectoryLiterally(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "[x]?*")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EndIn(dir); err == nil || !strings.Contains(err.Error(), "holds no log") {
		t.Fatalf("EndIn without a log = %v, want an error", err)
	}
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	sub := recorderDir(t, dir, "[a]")
	// A directory of another program with a matching name, and a file.
	backup := filepath.Join(dir, dirPrefix+"backup")
	if err := os.Mkdir(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "data"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(sub, logPrefix+"a"+logSuffix)
	if err := os.WriteFile(log, []byte(fmt.Sprintf("@%d 1\n", gone.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EndIn(dir); err != nil {
		t.Fatalf("EndIn in %q: %v", dir, err)
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatalf("EndIn kept the recorder directory of an ended recording (%v)", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("EndIn removed the given directory: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(backup, "data")); err != nil || string(data) != "keep" {
		t.Fatalf("EndIn touched %s, which no recorder made: %q, %v", backup, data, err)
	}
	if err := EndIn(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("EndIn of a missing directory: no error")
	}
}
