package proxy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Device check 3, P3-D: the auto-started iwdp had no log (its stdout and
// stderr were /dev/null), and iwdp's stderr lines were the only evidence of
// the F1 fault. A started iwdp appends both to LogPath.

// waitLog waits up to 3 s for the iwdp log at path to contain want and
// returns the log.
func waitLog(t *testing.T, path, want string) string {
	t.Helper()
	var data []byte
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		data, _ = os.ReadFile(path)
		if strings.Contains(string(data), want) {
			break
		}
	}
	return string(data)
}

func TestStartWritesTheIWDPLog(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeChatty, addr)
	path, err := LogPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(os.Getenv("XDG_STATE_HOME"), "agent-ios", "iwdp.log"); path != want {
		t.Fatalf("LogPath() = %s, want %s", path, want)
	}
	if err := testStarter(addr, 10*time.Second).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.starts()); n != 1 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times, want 1", n)
	}
	if log := waitLog(t, path, "fake-stderr-line"); !strings.Contains(log, "fake-stderr-line") {
		t.Fatalf("the iwdp log %s = %q, want the started iwdp's stderr", path, log)
	}
	// iwdp's stdout is block-buffered when it is a file; with GNU stdbuf
	// on PATH the start makes it line-buffered, so a stdout line is there
	// while the iwdp runs. Without stdbuf (macOS) it appears at exit.
	if _, err := exec.LookPath("stdbuf"); err == nil {
		if log := waitLog(t, path, "fake-stdout-line"); !strings.Contains(log, "fake-stdout-line") {
			t.Fatalf("the iwdp log %s = %q, want the running iwdp's stdout line (stdbuf -oL)", path, log)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the iwdp log has mode %v, want 0600", perm)
	}
}

// The log is bounded: a start finds a log of maxIWDPLog bytes or more and
// keeps it as iwdp.log.1 (replacing an older one), so at most two logs
// exist; a smaller log is appended to.
func TestStartKeepsOnePreviousIWDPLog(t *testing.T) {
	addr := freeAddr(t)
	installFakeIWDP(t, fakeChatty, addr)
	path, err := LogPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := "old-run\n" + strings.Repeat("x", maxIWDPLog)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("older-run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := testStarter(addr, 10*time.Second).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	log := waitLog(t, path, "fake-stderr-line")
	if !strings.Contains(log, "fake-stderr-line") || strings.Contains(log, "old-run") {
		t.Fatalf("the iwdp log after the start = %.200q, want only the new run", log)
	}
	prev, err := os.ReadFile(path + ".1")
	if err != nil || string(prev) != old {
		t.Fatalf("iwdp.log.1 = %.40q (%v), want the previous log", prev, err)
	}
}

func TestStartAppendsToASmallIWDPLog(t *testing.T) {
	addr := freeAddr(t)
	installFakeIWDP(t, fakeChatty, addr)
	path, err := LogPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("earlier-run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := testStarter(addr, 10*time.Second).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if log := waitLog(t, path, "fake-stderr-line"); !strings.HasPrefix(log, "earlier-run\n") || !strings.Contains(log, "fake-stderr-line") {
		t.Fatalf("the iwdp log = %q, want the earlier run followed by the new one", log)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("a small log was rotated (iwdp.log.1: %v)", err)
	}
}

// A log that cannot be opened fails the start before any iwdp runs, and
// the error names the log.
func TestStartFailsWithoutAUsableIWDPLog(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", notDir)
	err := testStarter(addr, 10*time.Second).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "iwdp log") || !strings.Contains(err.Error(), "no ios_webkit_debug_proxy was started") {
		t.Fatalf("Start with an unusable log directory = %v, want the log error", err)
	}
	if n := len(f.starts()); n != 0 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times, want 0", n)
	}
}

// Without XDG_STATE_HOME the log is ~/.local/state/agent-ios/iwdp.log: a
// directory of agent-ios's own, because another tool that starts iwdp may
// write and truncate ~/.local/state/iwdp/iwdp.log.
func TestIWDPLogPathDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	path, err := LogPath()
	if want := filepath.Join(home, ".local", "state", "agent-ios", "iwdp.log"); err != nil || path != want {
		t.Fatalf("LogPath() = %s, %v; want %s", path, err, want)
	}
}
