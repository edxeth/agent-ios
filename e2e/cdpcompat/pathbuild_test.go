//go:build cdpcompat

package cdpcompat

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// pathMappingVar names the native agent-browser binary that a script on
// PATH runs. The driver never runs a script on PATH and never infers
// which build a script runs: what a script runs for one command (for
// example --help) does not show what it runs for another, so the user
// states it (README.md, "agent-browser on PATH").
const pathMappingVar = "AGENT_IOS_CDPCOMPAT_PATH_AGENT_BROWSER"

// resolvePathBuild records the agent-browser that PATH names, which the
// user's agents run, and the binary that its scenario runs will execute.
// It returns run=true when those runs add something: false when PATH has
// no agent-browser, or when the binary has the pinned SHA-256.
//   - A native binary on PATH (an ELF file, or a symlink to one) runs
//     directly.
//   - A script on PATH (a wrapper or the npm launcher) needs the mapping
//     pathMappingVar: the absolute path of the native binary that the
//     script runs. That binary runs directly, in the client environment
//     (clientEnv) with the driver's home and configuration; the script
//     itself never runs. Without the mapping the error says what to set.
//   - A mapping without a script on PATH is an error, so that a stale
//     setting is never silently ignored.
func resolvePathBuild(pinned agentBrowserBuild) (agentBrowserBuild, bool, error) {
	mapping, mapped := os.LookupEnv(pathMappingVar)
	entry, err := exec.LookPath("agent-browser")
	if err != nil {
		if mapped {
			return agentBrowserBuild{}, false, fmt.Errorf("%s is set to %q, but PATH has no agent-browser (%v); unset %s, or put the script that runs that binary on PATH", pathMappingVar, mapping, err, pathMappingVar)
		}
		return agentBrowserBuild{Source: "PATH", Note: fmt.Sprintf("no agent-browser on PATH (%v); only the pinned runs ran", err)}, false, nil
	}
	real, err := filepath.EvalSymlinks(entry)
	if err != nil {
		return agentBrowserBuild{}, false, fmt.Errorf("agent-browser on PATH (%s): %w", entry, err)
	}
	bin, how, script := real, "PATH names the native binary", ""
	switch {
	case isELF(real) && mapped:
		return agentBrowserBuild{}, false, fmt.Errorf("agent-browser on PATH (%s) is the native binary %s, which the suite runs directly, but %s is set to %q; unset %s (it applies only to a script on PATH)", entry, real, pathMappingVar, mapping, pathMappingVar)
	case isELF(real):
	case !mapped || mapping == "":
		return agentBrowserBuild{}, false, fmt.Errorf("agent-browser on PATH (%s) is the script %s, not a native binary; the suite does not guess which build a script runs. Set %s to the absolute path of the native agent-browser binary that %s runs (e2e/cdpcompat/README.md, \"agent-browser on PATH\")", entry, real, pathMappingVar, real)
	case !filepath.IsAbs(mapping):
		return agentBrowserBuild{}, false, fmt.Errorf("%s=%q is not an absolute path; set it to the absolute path of the native agent-browser binary that %s runs", pathMappingVar, mapping, real)
	default:
		if bin, err = filepath.EvalSymlinks(mapping); err != nil {
			return agentBrowserBuild{}, false, fmt.Errorf("%s=%q: %w; set it to the native agent-browser binary that %s runs", pathMappingVar, mapping, err, real)
		}
		how, script = "the script on PATH runs the binary that "+pathMappingVar+" names", real
	}
	b, err := describeBuild("PATH", entry, bin)
	if err != nil {
		if script != "" {
			return b, false, fmt.Errorf("%s=%q (the binary that the script %s runs): %w", pathMappingVar, mapping, script, err)
		}
		return b, false, fmt.Errorf("agent-browser on PATH: %w", err)
	}
	b.Resolution, b.Script = how, script
	if script != "" {
		b.Mapping = mapping
	}
	if b.SHA256 == pinned.SHA256 {
		b.Note = "the binary that runs has the pinned SHA-256; the agent-browser runs ran once"
		return b, false, nil
	}
	return b, true, nil
}

// Limits of runBounded.
const (
	// metadataOutputLimit bounds what a metadata command's output keeps in
	// memory; the rest is read and dropped.
	metadataOutputLimit = 64 << 10
	// pipeDrain bounds the wait for a command's pipes after its process
	// group ended: a process that left the group can still hold them.
	pipeDrain = 2 * time.Second
)

// metadataTimeout bounds each metadata command of the build description
// (--version, git rev-parse, git describe). A variable: tests shorten it.
var metadataTimeout = 10 * time.Second

// runOwned runs cmd, whose Dir, Env, Stdout and Stderr the caller set, in
// its own process group, and returns whether timeout ended it and the
// error of exec.Cmd.Wait (an *exec.ExitError for an exit status other than
// 0). Every external command of the suite runs through it.
//   - When timeout ends first, it kills the whole group before it reaps
//     the leader: the unreaped leader keeps the group id in use, so the
//     signal reaches only this command's processes.
//   - With endGroup, it also kills the group when the leader exits by
//     itself, so nothing that the command started outlives it. Client runs
//     do not set it: agent-browser's session must live until its close.
//   - The pipes close at most pipeDrain after the leader exited or the
//     group was killed (exec.Cmd.WaitDelay), so a process that holds them,
//     in the group or not, cannot block the caller; Wait then reports
//     exec.ErrWaitDelay. Wait always reaps the leader.
func runOwned(cmd *exec.Cmd, timeout time.Duration, endGroup bool) (bool, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = pipeDrain
	if err := cmd.Start(); err != nil {
		return false, err
	}
	pgid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- waitExitNoReap(pgid) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	timedOut := false
	select {
	case err := <-exited:
		if err != nil {
			// The leader is not reaped, so the group id is still its own.
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			_ = cmd.Wait()
			return false, fmt.Errorf("waiting for %s: %w", cmd.Path, err)
		}
	case <-timer.C:
		timedOut = true
	}
	if timedOut || endGroup {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			_ = cmd.Wait()
			return timedOut, fmt.Errorf("ending the process group of %s: %w", cmd.Path, err)
		}
	}
	return timedOut, cmd.Wait()
}

// runBounded runs name with args in dir and env (nil: the test's own)
// through runOwned with endGroup, and returns its stdout. Each output
// keeps at most metadataOutputLimit bytes. A timeout, an exit status other
// than 0 and a pipe that stays open are errors.
func runBounded(timeout time.Duration, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	stdout, stderr := &cappedBuffer{limit: metadataOutputLimit}, &cappedBuffer{limit: metadataOutputLimit}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	timedOut, err := runOwned(cmd, timeout, true)
	line := strings.Join(append([]string{name}, args...), " ")
	switch {
	case timedOut:
		return nil, fmt.Errorf("%s did not end within %v; its process group was killed (stderr %q)", line, timeout, stderr.String())
	case err != nil:
		return nil, fmt.Errorf("%s: %w (stderr %q)", line, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// waitExitNoReap blocks until the process pid has exited, without reaping
// it (WNOWAIT): its pid, and so its process group id, stays in use until
// exec.Cmd.Wait reaps it.
func waitExitNoReap(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// cappedBuffer keeps the first limit bytes written to it and counts the
// dropped rest, so a writer is never blocked and memory stays bounded.
type cappedBuffer struct {
	bytes.Buffer
	limit   int
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	keep := min(len(p), max(c.limit-c.Len(), 0))
	c.Buffer.Write(p[:keep])
	c.dropped += len(p) - keep
	return len(p), nil
}
