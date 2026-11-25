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
// how long after the command's deadline exits called os.Exit, in
// nanoseconds (negative before the deadline): time.Since of the deadline
// of the command's context (ctx.Deadline() of the context that
// current.watch or current.started recorded, which
// commands.RunCLIStarted created). The deadline is the context's own, so a
// delay between its creation and its recording does not shift it, and it
// keeps its monotonic clock reading, so a step of the wall clock does not
// change the result.
const startFileEnv = "AGENT_IOS_TEST_START_FILE"

// exitFileSuffix names the file of the command's exit time after its
// deadline (startFileEnv).
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
		ctx := current.ctx
		current.mu.Unlock()
		if ctx != nil {
			if dl, ok := ctx.Deadline(); ok {
				past := time.Since(dl)
				_ = os.WriteFile(path+exitFileSuffix, []byte(strconv.FormatInt(int64(past), 10)), 0o600)
			}
		}
		exit(code)
	}
}

// cliTimes are the run times of one agent-ios process (runCLITimed).
type cliTimes struct {
	// fromInit is the time from the process's package initialization to
	// its exit as the test saw it: it excludes process creation and the
	// Go runtime start, and includes the process teardown and the test's
	// wait for it.
	fromInit time.Duration
	// pastDeadline is the time from the command's deadline (its
	// context's ctx.Deadline()) to the process's call of os.Exit, as the
	// process measured it on its monotonic clock; negative when it exited
	// before the deadline.
	pastDeadline time.Duration
}

// runCLITimed runs agent-ios like runCLI and also returns its run times
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
		t.Fatalf("running agent-ios %v: %v", args, err)
	}
	readNs := func(path, what string) int64 {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("agent-ios %v did not record %s: %v", args, what, err)
		}
		ns, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			t.Fatalf("%s %q of agent-ios %v: %v", what, raw, args, err)
		}
		return ns
	}
	initNs := readNs(startFile, "its start")
	pastNs := readNs(startFile+exitFileSuffix, fmt.Sprintf("its exit time after the deadline (%s)", exitFileSuffix))
	return res, cliTimes{fromInit: end.Sub(time.Unix(0, initNs)), pastDeadline: time.Duration(pastNs)}
}
