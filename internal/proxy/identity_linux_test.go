//go:build linux

package proxy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// procStartTime is field 22 of /proc/<pid>/stat, the start time of the
// process in clock ticks after boot (proc(5)); it is public.
func procStartTime(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	return f[22-3] // the fields after the command name start at field 3
}

// nativeProcargs returns the arguments and exec environment of process pid
// as a KERN_PROCARGS2 buffer (procargsBuffer), built here from
// /proc/<pid>/cmdline and /proc/<pid>/environ, so darwin's reader
// (argsIdentity) runs on Linux against real processes.
func nativeProcargs(pid int) ([]byte, error) {
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil, err
	}
	split := func(b []byte) []string {
		if len(b) == 0 {
			return nil
		}
		return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	}
	args := split(cmdline)
	return procargsBuffer("/proc/exe", args, split(environ), []string{"ptr_munge=0x1"}, 3), nil
}

// F3: procIdentity (Linux) reads the start time and the token from /proc;
// a process without the token has none; it fails for a gone process and
// for short or unreadable /proc files.
func TestProcIdentityReadsStartAndToken(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	cmd.Env = append(os.Environ(), tokenEnv+"=tok-1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// Until its exec completes, the child shows this process's environment
	// (Restart reads the identity only of a started iwdp that listens).
	var id identity
	var err error
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if id, err = procIdentity(cmd.Process.Pid); err == nil && id.token != "" {
			break
		}
	}
	if err != nil || id.start != procStartTime(t, cmd.Process.Pid) || id.token != "tok-1" {
		t.Errorf("procIdentity = %+v, %v; want the start time and tok-1", id, err)
	}
	if id, err := procIdentity(os.Getpid()); err != nil || id.token != "" {
		t.Errorf("procIdentity of this process = %+v, %v; want no token", id, err)
	}
	if _, err := procIdentity(deadPID(t)); err == nil {
		t.Error("procIdentity of a gone process succeeded")
	}
	fake := t.TempDir()
	old := procFS
	t.Cleanup(func() { procFS = old })
	procFS = fake
	if err := os.MkdirAll(filepath.Join(fake, "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, "7", "stat"), []byte("7 (x) S 1 7"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := procIdentity(7); err == nil || !strings.Contains(err.Error(), "fields") {
		t.Errorf("procIdentity of a short stat = %v, want an error", err)
	}
	stat := "7 (x) S" + strings.Repeat(" 1", 30)
	if err := os.WriteFile(filepath.Join(fake, "7", "stat"), []byte(stat), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := procIdentity(7); err == nil {
		t.Error("procIdentity without an environ file succeeded")
	}
}

// Off darwin, kernProcargs2 is not available and always fails, so
// darwin's reader can never succeed here by accident.
func TestKernProcargs2IsDarwinOnly(t *testing.T) {
	if buf, err := kernProcargs2(os.Getpid()); err == nil || !strings.Contains(err.Error(), "KERN_PROCARGS2") {
		t.Errorf("kernProcargs2 on Linux = %q, %v; want an error", buf, err)
	}
}
