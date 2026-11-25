package e2e_test

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// probeLifetimeEnv makes TestOwnerProbeEndsWithItsTestProcess run as the
// child that starts the probe.
const probeLifetimeEnv = "AGENT_IOS_E2E_PROBE_LIFETIME_CHILD"

// A test process can end without its cleanup: at a test timeout, or when
// someone kills it. The owner probes that it started must end with it; 96
// probes of killed runs once ran for a day. A child test process starts a
// probe that ignores SIGTERM and prints its pid; this test kills the child
// with SIGKILL and checks that the probe ends.
func TestOwnerProbeEndsWithItsTestProcess(t *testing.T) {
	if os.Getenv(probeLifetimeEnv) != "" {
		p := startOwnerProbe(t, probeSpec{})
		if _, err := os.Stdout.WriteString("probe " + strconv.Itoa(p.pid) + "\n"); err != nil {
			t.Fatal(err)
		}
		select {} // until the parent kills this process
	}
	d, ok := t.Deadline()
	if !ok {
		t.Fatal("the test needs a deadline (go test -timeout) for its child process")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestOwnerProbeEndsWithItsTestProcess$", "-test.count=1", "-test.timeout="+time.Until(d).String())
	cmd.Env = append(os.Environ(), probeLifetimeEnv+"=1")
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
	pid := 0
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "probe "); ok {
			pid, _ = strconv.Atoi(v)
			break
		}
	}
	if pid <= 0 {
		_ = cmd.Wait()
		t.Fatalf("the child test process started no probe; its output:\n%s", stderr.String())
	}
	killed = true
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the child test process: %v", err)
	}
	_ = cmd.Wait()
	for probeRuns(pid) {
		if time.Now().After(d.Add(-2 * time.Second)) {
			_ = syscall.Kill(pid, syscall.SIGKILL) // by pid: this test's leftover
			t.Fatalf("the probe %d of the killed child test process still runs", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// probeRuns reports whether process pid runs and is no zombie.
func probeRuns(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}
