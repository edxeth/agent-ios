package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startFileEnv names a file in which a CLI process of the test binary
// writes the wall time (Unix nanoseconds) at which its package
// initialization ran, just before TestMain hands over to main(). Process
// creation, loading and the Go runtime start come before that time.
const startFileEnv = "IWDP_CLI_TEST_START_FILE"

func init() {
	if os.Getenv(runMainEnv) != "1" {
		return
	}
	path := os.Getenv(startFileEnv)
	if path == "" {
		return
	}
	now := time.Now().UnixNano()
	// O_EXCL: only the first process writes it; a daemon that this CLI
	// process might start inherits the variable and must not overwrite it.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(strconv.FormatInt(now, 10))
	_ = f.Close()
}

// runCLIFromInit runs iwdp-cli like runCLI and also returns the time from
// the process's package initialization (startFileEnv) to its exit as the
// test saw it. The time excludes process creation and the Go runtime start,
// which come before the command's deadline exists.
func runCLIFromInit(t *testing.T, env []string, args ...string) (cliResult, time.Duration) {
	t.Helper()
	startFile := filepath.Join(t.TempDir(), "start")
	cmd := cliCommand(t, "", with(env, startFileEnv+"="+startFile), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	end := time.Now()
	res := cliResult{stdout: stdout.String(), stderr: stderr.String(), elapsed: end.Sub(start)}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running iwdp-cli %v: %v", args, err)
	}
	raw, err := os.ReadFile(startFile)
	if err != nil {
		t.Fatalf("iwdp-cli %v did not record its start: %v", args, err)
	}
	ns, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		t.Fatalf("start time %q of iwdp-cli %v: %v", raw, args, err)
	}
	return res, end.Sub(time.Unix(0, ns))
}
