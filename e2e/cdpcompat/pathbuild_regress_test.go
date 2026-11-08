//go:build cdpcompat

package cdpcompat

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeAgentBrowserVar makes the test binary act as a fake agent-browser
// (TestMain): a native binary whose behavior a test chooses. Its value is
// "<mode>:<file>":
//   - record: append one JSON line {args, home, dir} per run to file;
//   - hang: at --version or --cdp, start a child that holds stdout, write
//     both pids to file, print the version and never end; any other
//     command (close) only prints;
//   - linger: as hang, but the leader ends after the version (the child
//     keeps stdout open);
//
// hang and linger also append every run, as record does, to file.runs.
//   - sleep: sleep (the child of hang and linger).
//
// hang and sleep end by themselves after fakeHang, so a failed test
// leaves no process behind for long.
//
// Every mode prints a version line for --version.
const fakeAgentBrowserVar = "CDPCOMPAT_FAKE_AGENT_BROWSER"

// fakeVersion is the --version line of the fake.
const fakeVersion = "agent-browser 0.0.0-fake"

// fakeHang is how long hang and sleep wait: far longer than the shortened
// metadataTimeout of TestMetadataCommandsAreBounded.
const fakeHang = time.Minute

// fakeRun is one record of the fake in record mode.
type fakeRun struct {
	Args []string `json:"args"`
	Home string   `json:"home"`
	Dir  string   `json:"dir"`
}

func fakeAgentBrowser(mode string) int {
	kind, file, _ := strings.Cut(mode, ":")
	version := slices.Contains(os.Args[1:], "--version")
	switch kind {
	case "sleep":
		time.Sleep(fakeHang)
		return 0
	case "record":
		dir, _ := os.Getwd()
		line, err := json.Marshal(fakeRun{Args: os.Args[1:], Home: os.Getenv("HOME"), Dir: dir})
		if err == nil {
			err = appendLine(file, line)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "fake agent-browser: %v\n", err)
			return 3
		}
	case "hang", "linger":
		line, err := json.Marshal(fakeRun{Args: os.Args[1:], Home: os.Getenv("HOME")})
		if err == nil {
			err = appendLine(file+".runs", line)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "fake agent-browser: %v\n", err)
			return 3
		}
		if !version && !slices.Contains(os.Args[1:], "--cdp") {
			break
		}
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "fake agent-browser: %v\n", err)
			return 3
		}
		child := exec.Command(self)
		child.Env = append(os.Environ(), fakeAgentBrowserVar+"=sleep:")
		child.Stdout = os.Stdout // the child holds the output pipe
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "fake agent-browser: %v\n", err)
			return 3
		}
		if err := os.WriteFile(file, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), child.Process.Pid)), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "fake agent-browser: %v\n", err)
			return 3
		}
		fmt.Println(fakeVersion)
		if kind == "hang" {
			time.Sleep(fakeHang)
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "fake agent-browser: unknown mode %q\n", mode)
		return 3
	}
	if version {
		fmt.Println(fakeVersion)
	}
	return 0
}

func appendLine(file string, line []byte) error {
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// selfBinary returns the test binary (the fake agent-browser), with
// symlinks resolved.
func selfBinary(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		t.Fatal(err)
	}
	return self
}

// pathFixture is a directory to put first on PATH with one agent-browser.
func pathFixture(t *testing.T, write func(path string) error) string {
	t.Helper()
	dir := t.TempDir()
	if err := write(filepath.Join(dir, "agent-browser")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func symlinkTo(target string) func(string) error {
	return func(path string) error { return os.Symlink(target, path) }
}

// helpDelegatingScript is a wrapper script that runs pinned for --help and
// other for every other command (the reviewer's wrapper of F8), and
// creates marker whenever it runs.
func helpDelegatingScript(marker, pinned, other string) func(string) error {
	return func(path string) error {
		script := fmt.Sprintf("#!/bin/sh\n: > '%s'\nfor a in \"$@\"; do\n  if [ \"$a\" = --help ]; then exec '%s' \"$@\"; fi\ndone\nexec '%s' \"$@\"\n", marker, pinned, other)
		return os.WriteFile(path, []byte(script), 0o700)
	}
}

// F8 of the W5c-2a round-1 review: the build that the PATH scenarios run
// comes from what PATH names and an explicit mapping, never from what a
// script runs for --help. The script on PATH runs the pinned binary for
// --help and another build (the fake) for everything else; the resolver
// never runs it (its marker stays absent), a script needs the mapping
// pathMappingVar, and only a binary with the pinned SHA-256 deduplicates.
func TestPathAgentBrowserResolution(t *testing.T) {
	pinned := pinnedAgentBrowser(t)
	self := selfBinary(t)
	t.Setenv(fakeAgentBrowserVar, "record:"+filepath.Join(t.TempDir(), "runs.jsonl"))
	marker := filepath.Join(t.TempDir(), "script-ran")
	userPath := os.Getenv("PATH")
	empty := t.TempDir()
	native := pathFixture(t, symlinkTo(self))
	pinnedLink := pathFixture(t, symlinkTo(pinned.Binary))
	scriptDir := pathFixture(t, helpDelegatingScript(marker, pinned.Binary, self))
	script := filepath.Join(scriptDir, "agent-browser")
	scriptReal, err := filepath.EvalSymlinks(script)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, path string
		mapping    *string // nil: unset
		// want: the binary, run and a note fragment; or an error with
		// every fragment of wantErr.
		wantBinary, wantNote string
		wantRun              bool
		wantScript           bool // the record names the script and mapping
		wantErr              []string
	}{
		{name: "no agent-browser", path: empty, wantNote: "no agent-browser on PATH"},
		{name: "mapping without agent-browser", path: empty, mapping: &self, wantErr: []string{pathMappingVar, "PATH has no agent-browser", "unset"}},
		{name: "native symlink", path: native + ":" + userPath, wantBinary: self, wantRun: true},
		{name: "native pinned binary", path: pinnedLink + ":" + userPath, wantBinary: pinned.Binary, wantNote: "pinned SHA-256"},
		{name: "native binary with a mapping", path: native + ":" + userPath, mapping: &self, wantErr: []string{"native binary", "unset " + pathMappingVar}},
		{name: "script without mapping", path: scriptDir + ":" + userPath, wantErr: []string{scriptReal, "is the script", "Set " + pathMappingVar, "absolute path of the native agent-browser binary"}},
		{name: "script with an empty mapping", path: scriptDir + ":" + userPath, mapping: new(string), wantErr: []string{"Set " + pathMappingVar}},
		{name: "script mapped to another build", path: scriptDir + ":" + userPath, mapping: &self, wantBinary: self, wantRun: true, wantScript: true},
		{name: "script mapped to the pinned binary", path: scriptDir + ":" + userPath, mapping: &pinned.Binary, wantBinary: pinned.Binary, wantNote: "pinned SHA-256", wantScript: true},
		{name: "relative mapping", path: scriptDir + ":" + userPath, mapping: ptr("agent-browser"), wantErr: []string{"not an absolute path"}},
		{name: "mapping to a script", path: scriptDir + ":" + userPath, mapping: &script, wantErr: []string{pathMappingVar, "not an ELF binary"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PATH", c.path)
			if c.mapping != nil {
				t.Setenv(pathMappingVar, *c.mapping)
			} else {
				unsetEnv(t, pathMappingVar)
			}
			b, run, err := resolvePathBuild(pinned)
			if len(c.wantErr) > 0 {
				for _, frag := range c.wantErr {
					if err == nil || !strings.Contains(err.Error(), frag) {
						t.Errorf("error %v, want one that contains %q", err, frag)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b.Source != "PATH" || b.Binary != c.wantBinary || run != c.wantRun || !strings.Contains(b.Note, c.wantNote) {
				t.Errorf("got %+v, run %v; want binary %q, run %v, a note with %q", b, run, c.wantBinary, c.wantRun, c.wantNote)
			}
			if c.wantBinary == "" {
				return
			}
			if want := map[string]string{self: "", pinned.Binary: pinned.SHA256}[c.wantBinary]; want != "" && b.SHA256 != want {
				t.Errorf("SHA-256 %s, want %s", b.SHA256, want)
			}
			if c.wantScript && (b.Script != scriptReal || b.Mapping != *c.mapping || b.Entry != script) {
				t.Errorf("record %+v; want script %s, mapping %s, entry %s", b, scriptReal, *c.mapping, script)
			}
			if !c.wantScript && (b.Script != "" || b.Mapping != "") {
				t.Errorf("record %+v names a script or mapping for a native binary", b)
			}
		})
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("the resolver ran the script on PATH (%s exists)", marker)
	}
}

func ptr(s string) *string { return &s }

// unsetEnv unsets k for the test and restores it at cleanup.
func unsetEnv(t *testing.T, k string) {
	t.Helper()
	t.Setenv(k, "") // registers the restore
	if err := os.Unsetenv(k); err != nil {
		t.Fatal(err)
	}
}

// F8: with the mapping, the scenarios run the mapped binary with the
// driver's home, working directory and configuration, and never the
// script on PATH.
func TestPathScriptRunsMappedBinary(t *testing.T) {
	pinned := pinnedAgentBrowser(t)
	self := selfBinary(t)
	runs := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv(fakeAgentBrowserVar, "record:"+runs)
	marker := filepath.Join(t.TempDir(), "script-ran")
	scriptDir := pathFixture(t, helpDelegatingScript(marker, pinned.Binary, pinned.Binary))
	mapping := filepath.Join(t.TempDir(), "agent-browser")
	if err := os.Symlink(self, mapping); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", scriptDir+":"+os.Getenv("PATH"))
	t.Setenv(pathMappingVar, mapping)
	b, run := pathAgentBrowser(t, pinned)
	if !run || b.Binary != self || b.Version != fakeVersion {
		t.Fatalf("build %+v, run %v; want the mapped binary %s (%s)", b, run, self, fakeVersion)
	}
	e := startDaemon(t)
	url := "ws://127.0.0.1:9/devtools/browser/x"
	if code, out, _ := e.runAgentBrowser(t, b.Binary, "path-mapped", url, "get", "url"); code != 0 {
		t.Fatalf("mapped binary: exit %d: %s", code, out)
	}
	var scenario []fakeRun
	for _, r := range readRuns(t, runs) {
		if slices.Contains(r.Args, "--cdp") {
			scenario = append(scenario, r)
		}
	}
	if len(scenario) != 1 {
		t.Fatalf("the mapped binary ran %d scenario commands, want 1: %+v", len(scenario), scenario)
	}
	r := scenario[0]
	if len(r.Args) != 8 || r.Args[0] != "--config" || r.Args[1] != agentBrowserConfig || r.Args[2] != "--session" ||
		!strings.HasPrefix(r.Args[3], "cdpc-") || !slices.Equal(r.Args[4:], []string{"--cdp", url, "get", "url"}) {
		t.Errorf("scenario arguments %q; want --config %s --session cdpc-... --cdp %s get url", r.Args, agentBrowserConfig, url)
	}
	if r.Home != clientHome || r.Dir != clientHome {
		t.Errorf("scenario ran with HOME %s in %s, want the driver's home %s for both", r.Home, r.Dir, clientHome)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("the script on PATH ran (%s exists)", marker)
	}
}

func readRuns(t *testing.T, file string) []fakeRun {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []fakeRun
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r fakeRun
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// F7 of the W5c-2a round-1 review: every metadata command of the build
// description has a deadline. A --version that never ends fails after
// metadataTimeout, and its whole process group ends (the leader and a
// child that holds the output pipe). A --version whose child keeps the
// pipe open after the leader exited (a wrapper that does not exec) ends
// at once with its output, and the child ends too.
func TestMetadataCommandsAreBounded(t *testing.T) {
	self := selfBinary(t)
	saved := metadataTimeout
	metadataTimeout = time.Second
	t.Cleanup(func() { metadataTimeout = saved })

	t.Run("hanging version", func(t *testing.T) {
		pids := filepath.Join(t.TempDir(), "pids")
		t.Setenv(fakeAgentBrowserVar, "hang:"+pids)
		start := time.Now()
		_, err := describeBuild("PATH", self, self)
		took := time.Since(start)
		if err == nil || !strings.Contains(err.Error(), "did not end within 1s") || !strings.Contains(err.Error(), "process group was killed") {
			t.Errorf("describeBuild of a hanging --version: %v, want the timeout", err)
		}
		if limit := metadataTimeout + pipeDrain; took > limit {
			t.Errorf("describeBuild took %v, want at most %v", took, limit)
		}
		requireGone(t, pids)
	})
	t.Run("child holds the output", func(t *testing.T) {
		pids := filepath.Join(t.TempDir(), "pids")
		t.Setenv(fakeAgentBrowserVar, "linger:"+pids)
		start := time.Now()
		b, err := describeBuild("PATH", self, self)
		took := time.Since(start)
		if err != nil || b.Version != fakeVersion {
			t.Errorf("describeBuild: %+v, %v; want version %q", b, err, fakeVersion)
		}
		// The group kill closes the pipe, not the pipeDrain bound.
		if took >= pipeDrain {
			t.Errorf("describeBuild took %v, want less than %v", took, pipeDrain)
		}
		requireGone(t, pids)
	})
}

// F9 of the W5c-2a round-2 review (the reviewer's reproduction
// TestReviewScenarioTimeoutEndsItsChild): a client run that outlives its
// timeout ends with its whole process group, also when a child that the
// client started holds the output pipe. The fake agent-browser starts
// such a child at --cdp and never ends; runClient must return soon after
// the 100 ms timeout, report it, keep the log, and leave no process.
func TestScenarioTimeoutEndsItsChild(t *testing.T) {
	self := selfBinary(t)
	pids := filepath.Join(t.TempDir(), "pids")
	t.Setenv(fakeAgentBrowserVar, "hang:"+pids)
	e := &endpoint{}
	const timeout = 100 * time.Millisecond
	type outcome struct {
		code     int
		out      string
		timedOut bool
		err      error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		code, out, timedOut, err := e.runClient("scenario-timeout.log", clientHome, timeout, nil, self,
			"--config", agentBrowserConfig, "--session", "cdpc-timeout", "--cdp", "ws://127.0.0.1:9/x", "get", "url")
		done <- outcome{code, out, timedOut, err}
	}()
	var o outcome
	select {
	case o = <-done:
	case <-time.After(timeout + pipeDrain + time.Second):
		killOwnedChild(t, pids, self)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("the client runner is still blocked after the owned child ended")
		}
		t.Fatalf("a %v client timeout was still blocked after %v: the child kept the output open", timeout, time.Since(start))
	}
	took := time.Since(start)
	if !o.timedOut || o.code != -1 || o.err != nil || !strings.Contains(o.out, fakeVersion) {
		t.Errorf("runClient: exit %d, timed out %v, error %v, output %q; want a timeout (exit -1) with the output so far", o.code, o.timedOut, o.err, o.out)
	}
	// The group kill closes the pipe at once, not the pipeDrain bound.
	if took >= timeout+pipeDrain {
		t.Errorf("runClient took %v, want less than %v", took, timeout+pipeDrain)
	}
	log, err := os.ReadFile(filepath.Join(logDir, "scenario-timeout.log"))
	if err != nil || !strings.Contains(string(log), "timed out after 100ms: true") || !strings.Contains(string(log), fakeVersion) {
		t.Errorf("log %q (%v): want the timeout and the output", log, err)
	}
	requireGone(t, pids)
}

// killOwnedChild ends the child that the fake recorded in pids, after it
// checked that the process still runs the test binary self.
func killOwnedChild(t *testing.T, pids, self string) {
	t.Helper()
	data, err := os.ReadFile(pids)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		t.Fatalf("bad owned process record: %q", data)
	}
	child, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatal(err)
	}
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", child)); err != nil || exe != self {
		t.Fatalf("refusing to kill process %d: it runs %q (%v), not %s", child, exe, err, self)
	}
	if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
}

// helperVar selects a helper mode of a test in a child test process, and
// helperFakeVar carries the fake's mode for the helper's clients (the
// helper itself must not run as the fake).
const (
	helperVar     = "CDPCOMPAT_TEST_HELPER"
	helperFakeVar = "CDPCOMPAT_TEST_HELPER_FAKE"
)

// F9: after a scenario timeout, runAgentBrowser still closes its own
// session. A child test process runs runAgentBrowser with the hanging
// fake and a 200 ms client timeout; it must fail with the timeout, and
// the fake must then see the close of the same session.
func TestScenarioTimeoutStillClosesSession(t *testing.T) {
	self := selfBinary(t)
	if os.Getenv(helperVar) == "scenario-timeout" {
		t.Setenv(fakeAgentBrowserVar, os.Getenv(helperFakeVar))
		clientTimeout = 200 * time.Millisecond
		e := startDaemon(t)
		e.runAgentBrowser(t, self, "helper-timeout", "ws://127.0.0.1:9/devtools/browser/x", "get", "url")
		return
	}
	pids := filepath.Join(t.TempDir(), "pids")
	start := time.Now()
	cmd := exec.Command(self, "-test.run=^TestScenarioTimeoutStillClosesSession$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), helperVar+"=scenario-timeout", helperFakeVar+"=hang:"+pids, "CDPCOMPAT_LOG_DIR="+t.TempDir())
	out := &cappedBuffer{limit: clientOutputLimit}
	cmd.Stdout, cmd.Stderr = out, out
	if timedOut, err := runOwned(cmd, 2*time.Minute, true); timedOut || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 1 {
		t.Fatalf("helper: %v (timed out %v); want exit 1 with the timeout; output:\n%s", err, timedOut, out)
	}
	if !strings.Contains(out.String(), "did not end within 200ms; its process group was killed") {
		t.Errorf("helper output lacks the timeout failure:\n%s", out)
	}
	// The helper builds iwdp-cli and starts a daemon; the fake would hang
	// for fakeHang if the timeout did not end it.
	if took := time.Since(start); took >= fakeHang/2 {
		t.Errorf("the helper took %v, want less than %v", took, fakeHang/2)
	}
	runs := readRuns(t, pids+".runs")
	session := func(r fakeRun) string {
		if i := slices.Index(r.Args, "--session"); i >= 0 && i+1 < len(r.Args) {
			return r.Args[i+1]
		}
		return ""
	}
	if len(runs) != 2 || !slices.Contains(runs[0].Args, "--cdp") || runs[1].Args[len(runs[1].Args)-1] != "close" ||
		session(runs[0]) == "" || session(runs[0]) != session(runs[1]) {
		t.Errorf("the fake saw %+v; want the --cdp run, then the close of the same session", runs)
	}
	requireGone(t, pids)
}

// requireGone fails unless every pid in file has ended within a second (it
// no longer exists, or it is a zombie that its new parent has not reaped).
// It kills a process that this test's fake started and that still runs.
func requireGone(t *testing.T, file string) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for !processEnded(pid) {
			if time.Now().After(deadline) {
				t.Errorf("process %d of the command is still running", pid)
				_ = syscall.Kill(pid, syscall.SIGKILL)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func processEnded(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	_, rest, _ := strings.Cut(string(stat), ") ")
	return strings.HasPrefix(rest, "Z")
}
