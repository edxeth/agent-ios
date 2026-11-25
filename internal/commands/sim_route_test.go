package commands

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/sim"
)

// fakeSSHHost points the simulator configuration at a fake ssh that logs
// each call to the returned file; ssh -G starts a child that keeps its
// pipes for 3 s (a slow Match exec), every other call fails at once. The
// state directory is private and short.
func fakeSSHHost(t *testing.T) (log, state string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	path, err := sim.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"ssh":"mac","device":"`+"E485E2B7-A0F9-413B-AB85-EFF4F63398F5"+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log = filepath.Join(bin, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$*\" in *' -G '*) sleep 3 & wait ;; esac\necho 'ssh: connect: refused' >&2\nexit 255\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	state, err = os.MkdirTemp("/tmp", "iwdp-t")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	prev := SimStateDir
	SimStateDir = func() (string, error) { return state, nil }
	t.Cleanup(func() { SimStateDir = prev })
	return log, state
}

// F7: reading the route (ssh -G, which can run a slow Match exec whose
// child keeps ssh's pipes) does not hold a command past its deadline.
func TestSimRouteDoesNotOutliveTheDeadline(t *testing.T) {
	fakeSSHHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	s, err := newSim(nil)
	if err == nil {
		_, err = s.Booted(ctx, "")
	}
	if took := time.Since(start); took > 600*time.Millisecond || err == nil {
		t.Fatalf("a 100 ms command took %s: %v", took, err)
	}
}

// F7: sim up --url with a bad URL reports the URL, before any ssh call,
// as at 2127105c.
func TestSimUpBadURLBeforeAnySSH(t *testing.T) {
	log, _ := fakeSSHHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := newSim(nil)
	if err == nil {
		_, err = s.Up(ctx, "", "not-a-url", true)
	}
	if err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("sim up --url not-a-url: %v, want the URL error", err)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Fatalf("ssh ran before the URL check: %s", b)
	}
}

// F7: sim down stops the forward first, before any ssh call, as at
// 2127105c: a slow ssh configuration does not keep the forward running.
func TestSimDownStopsTheForwardFirst(t *testing.T) {
	log, state := fakeSSHHost(t)
	rec, _ := json.Marshal(map[string]any{"pid": 999999999, "start": "s", "token": "t", "socket": "/x"})
	if err := os.WriteFile(filepath.Join(state, "sim-forward.json"), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	s, err := newSim(nil)
	if err == nil {
		_, err = s.Down(ctx, "", false)
	}
	if _, serr := os.Stat(filepath.Join(state, "sim-forward.json")); !errors.Is(serr, os.ErrNotExist) {
		b, _ := os.ReadFile(log)
		t.Fatalf("sim down left the forward's record (%v; ssh calls %q): %v", serr, b, err)
	}
}
