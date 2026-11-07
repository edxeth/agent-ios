package testutil

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DeadIWDPListing starts a private loopback HTTP server that stands in for
// iwdp's default device list (iwdp.DefaultListingAddr) and returns its
// host:port. It closes every connection without a reply, like an iwdp that
// is not running, so tests that set iwdp.DefaultListingAddr to it never
// read the device list of a real iwdp on 127.0.0.1:9221. The server lives
// until the process exits.
func DeadIWDPListing() string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no iwdp", http.StatusServiceUnavailable)
			return
		}
		c, _, err := hj.Hijack()
		if err != nil {
			http.Error(w, "no iwdp", http.StatusServiceUnavailable)
			return
		}
		_ = c.Close()
	}))
	return strings.TrimPrefix(srv.URL, "http://")
}

// FakeIWDPCommands are the executables through which code starts or stops
// the real ios_webkit_debug_proxy (internal/proxy Start, EnsureRunning and
// Restart run them by name).
var FakeIWDPCommands = []string{"ios_webkit_debug_proxy", "pkill"}

// InstallFakeIWDP writes fake FakeIWDPCommands into dir and puts dir first
// on this process's PATH, which binaries and daemons started by the tests
// inherit. Tests then never start, stop or restart the real iwdp: each fake
// appends its arguments to dir/calls.log and exits 1.
func InstallFakeIWDP(dir string) error {
	for _, name := range FakeIWDPCommands {
		script := "#!/bin/sh\necho \"" + name + " $*\" >> '" + filepath.Join(dir, "calls.log") + "'\nexit 1\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			return fmt.Errorf("fake %s: %w", name, err)
		}
	}
	return os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// CheckFakeIWDP returns an error unless every FakeIWDPCommands name
// resolves to its fake in dir and the fake records a call without running
// the real program (the guard test of each suite).
func CheckFakeIWDP(dir string) error {
	for _, name := range FakeIWDPCommands {
		p, err := exec.LookPath(name)
		if err != nil || p != filepath.Join(dir, name) {
			return fmt.Errorf("%s resolves to %q (%v), not the fake in %s", name, p, err, dir)
		}
		if err := exec.Command(p, "--guard-check").Run(); err == nil {
			return fmt.Errorf("the fake %s exited 0", name)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		return err
	}
	for _, name := range FakeIWDPCommands {
		if !strings.Contains(string(data), name+" --guard-check") {
			return fmt.Errorf("the fake %s did not record its call: %q", name, data)
		}
	}
	return nil
}
