package proxy

import (
	"context"
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

// Device check 4: a serve fake (TestFakeIWDPProcess) runs in its own
// process group, so a test binary that is killed (a go test timeout, a
// killed CI job) does not take it along; one ran for 6 hours. Each fake
// gets the pid of the test process that starts it (fakeParentEnv) and ends
// when that process ends (exitWithParent).

// Variables of TestFakeIWDPParentProcess: how it starts the fake (one of
// fakeStarts), the fake's listing address, and the file for the fake's
// pid.
const (
	fakeParentHowEnv  = "AGENT_IOS_PROXY_TEST_PARENT_HOW"
	fakeParentAddrEnv = "AGENT_IOS_PROXY_TEST_PARENT_ADDR"
	fakeParentPIDEnv  = "AGENT_IOS_PROXY_TEST_PARENT_PID_FILE"
)

// fakeStarts are the ways the tests start a fake that runs the test
// binary: startServer (ownership_test.go), foreignIWDP
// (restart_w1c5_test.go), a Starter with the serve fake on PATH
// (installFakeIWDP, start_test.go) and the stubborn fake with its child
// (startStubborn, restart_r1_w1c5_test.go), and the shell fakes hang and
// stubborn with their sleep child (installFakeIWDP), which never serve.
var fakeStarts = []string{"server", "foreign", "starter", "stubborn", "hang", "shell-stubborn"}

// fakeEndBudget is how soon a fake must end after its test process ended.
// It is the behavior under test: "within a few seconds".
const fakeEndBudget = 5 * time.Second

// TestFakeIWDPParentProcess is the body of a test process that starts a
// fake, writes the pids of the fake's processes and waits to be killed. Without the
// variables it returns at once.
func TestFakeIWDPParentProcess(t *testing.T) {
	how := os.Getenv(fakeParentHowEnv)
	if how == "" {
		return
	}
	addr := os.Getenv(fakeParentAddrEnv)
	var pids []int
	switch how {
	case "server":
		pids = []int{startServer(t, addr)}
	case "foreign":
		pids = []int{foreignIWDP(t, addr)}
	case "starter":
		f := installFakeIWDP(t, fakeServe, addr)
		if err := testStarter(addr, 10*time.Second).Start(context.Background()); err != nil {
			t.Fatalf("Start of the serve fake: %v", err)
		}
		pids = f.pids()
		if len(pids) != 1 {
			t.Fatalf("started fakes %v, want 1", pids)
		}
	case "stubborn":
		_, pids = startStubborn(t, testStarter(addr, 10*time.Second), addr)
	case "hang", "shell-stubborn":
		mode := fakeHang
		if how == "shell-stubborn" {
			mode = fakeStubborn
		}
		f := installFakeIWDP(t, mode, addr)
		// The fake never serves, so Start waits until this process is
		// killed; the pids are the fake shell's and its child's.
		go func() { _ = testStarter(addr, time.Minute).Start(context.Background()) }()
		for len(pids) < 2 {
			time.Sleep(10 * time.Millisecond)
			pids = f.pids()
		}
	default:
		t.Fatalf("unknown fake start %q", how)
	}
	text := strings.Trim(fmt.Sprint(pids), "[]")
	if err := os.WriteFile(os.Getenv(fakeParentPIDEnv), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Minute) // until TestFakeIWDPEndsWithItsTestProcess kills this process
}

// A serve fake ends within fakeEndBudget after the test process that
// started it was killed, however the test started it.
func TestFakeIWDPEndsWithItsTestProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, how := range fakeStarts {
		t.Run(how, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "fake.pid")
			parent := exec.Command(exe, "-test.run=^TestFakeIWDPParentProcess$")
			parent.Env = append(os.Environ(), fakeParentHowEnv+"="+how, fakeParentAddrEnv+"="+freeAddr(t), fakeParentPIDEnv+"="+pidFile)
			var out strings.Builder
			parent.Stdout, parent.Stderr = &out, &out
			if err := parent.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- parent.Wait() }()
			t.Cleanup(func() {
				_ = parent.Process.Kill()
				if err := <-exited; err != nil {
					t.Logf("the parent test process: %v", err) // the kill ends it with an error
				}
			})
			var pids []int
			for pids == nil {
				select {
				case err := <-exited:
					exited <- err
					t.Fatalf("the parent test process ended before it started the fake: %v\n%s", err, out.String())
				case <-time.After(10 * time.Millisecond):
				}
				if data, err := os.ReadFile(pidFile); err == nil && len(data) > 0 {
					for _, f := range strings.Fields(string(data)) {
						pid, err := strconv.Atoi(f)
						if err != nil {
							t.Fatal(err)
						}
						pids = append(pids, pid)
					}
				}
			}
			for _, pid := range pids {
				if !alive(pid) {
					t.Fatalf("the fake's process %d is not running while its test process runs", pid)
				}
			}
			_ = parent.Process.Signal(syscall.SIGKILL)
			exited <- <-exited
			end := time.Now().Add(fakeEndBudget)
			for _, pid := range pids {
				for alive(pid) {
					if time.Now().After(end) {
						for _, p := range pids {
							_ = syscall.Kill(p, syscall.SIGKILL) // by its pid, never by name
						}
						t.Fatalf("the fake's process %d still ran %s after its test process was killed", pid, fakeEndBudget)
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		})
	}
}
