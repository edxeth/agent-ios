//go:build darwin

package proxy

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// nativeProcargs returns the KERN_PROCARGS2 buffer of process pid: on
// darwin the system's own (kernProcargs2).
func nativeProcargs(pid int) ([]byte, error) { return kernProcargs2(pid) }

// F3 (darwin): readIdentity reads the start time and, through sysctl
// KERN_PROCARGS2, the token of a process of this user that was started
// with one; a process without the token has none.
func TestReadIdentityReadsTheTokenOnDarwin(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	cmd.Env = append(os.Environ(), tokenEnv+"=tok-1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	var id identity
	var err error
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if id, err = readIdentity(cmd.Process.Pid); err == nil && id.token != "" {
			break
		}
	}
	if err != nil || id.start == "" || id.token != "tok-1" {
		t.Errorf("readIdentity = %+v, %v; want a start time and tok-1", id, err)
	}
	if id, err := readIdentity(os.Getpid()); err != nil || id.token != "" {
		t.Errorf("readIdentity of this process = %+v, %v; want no token", id, err)
	}
}
