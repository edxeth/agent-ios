//go:build cdpcompat

package cdpcompat

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lifetimeReady is the line that the child prints when its MiniBrowser
// lists the page.
const lifetimeReady = "lifetime-helper: ready"

// lifetimeReportMargin is the part of the test's time that is left for
// the report of processes that did not end.
const lifetimeReportMargin = 10 * time.Second

// A test process can end without its cleanup: at a test timeout, or when
// someone kills it. The browser that it started must end with it, also
// WebKit's web and network processes and broadwayd. A child test process
// starts broadwayd and MiniBrowser as the client scenarios do
// (startMiniBrowser) and waits; this test kills it with SIGKILL and checks
// that every process the child started ends.
func TestBrowserEndsWithItsTestProcess(t *testing.T) {
	if os.Getenv(helperVar) == lifetimeHelperMode {
		lifetimeHelper(t)
		return
	}
	d, ok := t.Deadline()
	if !ok {
		t.Fatal("the test needs a deadline (go test -timeout) for its child process")
	}
	cmd := exec.Command(selfBinary(t), "-test.run=^TestBrowserEndsWithItsTestProcess$", "-test.count=1", "-test.timeout="+time.Until(d).String())
	// TMPDIR: the child's temp dirs (the DPI shim) go away with this
	// test's, also after the kill.
	cmd.Env = append(os.Environ(), helperVar+"="+lifetimeHelperMode, "TMPDIR="+t.TempDir())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// The child ends with this process too, so a failed run leaves nothing.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child test process: %v", err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if sc.Text() == lifetimeReady {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	if !<-ready {
		_ = cmd.Wait()
		t.Fatalf("the child test process ended before its MiniBrowser listed the page; its output:\n%s", stderr.String())
	}

	// The child's descendants, once WebKit's web and network processes
	// run: broadwayd, MiniBrowser and the processes that MiniBrowser
	// started.
	var procs []procInfo
	for {
		procs = descendants(t, cmd.Process.Pid)
		if hasComm(procs, "broadwayd") && hasComm(procs, "MiniBrowser") && hasComm(procs, "WebKitWebProces") && hasComm(procs, "WebKitNetworkPr") {
			break
		}
		if time.Now().After(d) {
			t.Fatalf("the child's processes %v lack broadwayd, MiniBrowser, WebKitWebProcess or WebKitNetworkProcess", procs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("the child test process %d started %v", cmd.Process.Pid, procs)

	killed = true
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the child test process: %v", err)
	}
	_ = cmd.Wait()
	// Wait for the ends until shortly before the test's deadline, so that
	// a failure still names the survivors.
	end := d.Add(-lifetimeReportMargin)
	for {
		var alive []procInfo
		for _, p := range procs {
			if p.alive() {
				alive = append(alive, p)
			}
		}
		if len(alive) == 0 {
			return
		}
		if time.Now().After(end) {
			for _, p := range alive {
				_ = syscall.Kill(p.pid, syscall.SIGKILL) // by PID: they are this test's leftovers
			}
			t.Fatalf("these processes of the killed child test process still run: %v", alive)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lifetimeHelper is the child process of TestBrowserEndsWithItsTestProcess:
// it starts broadwayd and MiniBrowser on a fixture page, prints
// lifetimeReady when MiniBrowser lists the page, and waits to be killed.
func lifetimeHelper(t *testing.T) {
	startMiniBrowser(t, startPage)
	fmt.Println(lifetimeReady)
	d, _ := t.Deadline() // the parent passes its own deadline
	time.Sleep(time.Until(d))
}

// procInfo is one process: its id, its command name and its start time
// (/proc/<pid>/stat field 22), which tells it from a later process with
// the same id.
type procInfo struct {
	pid   int
	comm  string
	start string
}

func (p procInfo) String() string { return fmt.Sprintf("%d (%s)", p.pid, p.comm) }

// alive reports whether p still runs: its /proc entry exists with p's
// start time and it is not a zombie (a zombie has ended and waits for
// its parent's wait).
func (p procInfo) alive() bool {
	st, ok := readStat(p.pid)
	return ok && st.start == p.start && st.state != "Z"
}

// procStat is the part of /proc/<pid>/stat that the test reads.
type procStat struct {
	comm, state, start string
	ppid               int
}

// readStat reads /proc/<pid>/stat (proc(5)); ok is false when the
// process is gone. The command name is in parentheses and can hold
// spaces and parentheses, so the fields after it are read from its
// last ")".
func readStat(pid int) (procStat, bool) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, false
	}
	s := string(b)
	open, closing := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || closing < open {
		return procStat{}, false
	}
	rest := strings.Fields(s[closing+1:]) // rest[0] is field 3 (state)
	if len(rest) < 20 {
		return procStat{}, false
	}
	ppid, err := strconv.Atoi(rest[1])
	if err != nil {
		return procStat{}, false
	}
	return procStat{comm: s[open+1 : closing], state: rest[0], ppid: ppid, start: rest[19]}, true
}

// descendants returns every running descendant of pid.
func descendants(t *testing.T, pid int) []procInfo {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	children := map[int][]procInfo{}
	for _, e := range entries {
		id, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if st, ok := readStat(id); ok && st.state != "Z" {
			children[st.ppid] = append(children[st.ppid], procInfo{pid: id, comm: st.comm, start: st.start})
		}
	}
	var out []procInfo
	queue := []int{pid}
	for len(queue) > 0 {
		for _, c := range children[queue[0]] {
			out = append(out, c)
			queue = append(queue, c.pid)
		}
		queue = queue[1:]
	}
	return out
}

func hasComm(procs []procInfo, comm string) bool {
	for _, p := range procs {
		if p.comm == comm {
			return true
		}
	}
	return false
}
