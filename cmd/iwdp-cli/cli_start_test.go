package main

import (
	"bytes"
	"errors"
	"fmt"
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
//
// The same process also writes, to the file's path plus exitFileSuffix,
// how long its command ran: the time from the start that current.begin,
// or for a registry command current.watch or current.started, recorded
// (when the command's deadline is fixed) to the call of os.Exit by exits,
// measured on the process's monotonic clock.
const startFileEnv = "IWDP_CLI_TEST_START_FILE"

// exitFileSuffix names the file of the command's run time (startFileEnv).
const exitFileSuffix = ".exit"

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
	exit := exits.exit
	exits.exit = func(code int) {
		current.mu.Lock()
		start := current.start
		current.mu.Unlock()
		if !start.IsZero() {
			_ = os.WriteFile(path+exitFileSuffix, []byte(strconv.FormatInt(int64(time.Since(start)), 10)), 0o600)
		}
		exit(code)
	}
}

// cliTimes are the run times of one iwdp-cli process (runCLITimed).
type cliTimes struct {
	// fromInit is the time from the process's package initialization to
	// its exit as the test saw it: it excludes process creation and the
	// Go runtime start, and includes the process teardown and the test's
	// wait for it.
	fromInit time.Duration
	// command is the time from the command's start (current.begin, when
	// its deadline is fixed) to the process's call of os.Exit, as the
	// process measured it.
	command time.Duration
}

// runCLITimed runs iwdp-cli like runCLI and also returns its run times
// (cliTimes).
func runCLITimed(t *testing.T, env []string, args ...string) (cliResult, cliTimes) {
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
	readNs := func(path, what string) int64 {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("iwdp-cli %v did not record %s: %v", args, what, err)
		}
		ns, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			t.Fatalf("%s %q of iwdp-cli %v: %v", what, raw, args, err)
		}
		return ns
	}
	initNs := readNs(startFile, "its start")
	cmdNs := readNs(startFile+exitFileSuffix, fmt.Sprintf("its command time (%s)", exitFileSuffix))
	return res, cliTimes{fromInit: end.Sub(time.Unix(0, initNs)), command: time.Duration(cmdNs)}
}
