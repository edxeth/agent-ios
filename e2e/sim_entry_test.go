package e2e_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// These tests run the real simulator entry points around
// scripts/sim-setup.sh, the Makefile targets sim-setup and test-e2e and
// the CI setup step of .github/workflows/test.yml, on any host. A fake
// setup script and a fake go stand in for the Simulator and the tests;
// nothing contacts a device, an iwdp or a Simulator.

const (
	freshWSURL   = "ws://localhost:9222/devtools/page/7"
	freshPageURL = "https://example.com/?iwdp-sim=fresh"
	freshToken   = "0123456789abcdef0123456789abcdef"
)

// staleSimEnv are inherited URLs of an earlier setup: an entry point
// must never run the tests with them.
var staleSimEnv = []string{"IWDP_SIM_WS_URL=ws://stale.invalid/devtools/page/1", "IWDP_SIM_PAGE_URL=https://stale.invalid/", "IWDP_SIM_RUN_TOKEN=ffffffffffffffffffffffffffffffff"}

// fakeSetups are the setup outcomes that every entry point must handle:
// the body of a fake scripts/sim-setup.sh for a setup run (its teardown
// only logs). wantOK is whether the entry point may go on to the tests.
var fakeSetups = []struct {
	name, body string
	wantOK     bool
}{
	{"setup fails", "echo 'setup error' >&2; exit 3", false},
	{"setup prints nothing", "exit 0", false},
	{"setup succeeds", "echo \"export IWDP_SIM_WS_URL='" + freshWSURL + "'\"; echo \"export IWDP_SIM_PAGE_URL='" + freshPageURL + "'\"; echo \"export IWDP_SIM_RUN_TOKEN='" + freshToken + "'\"", true},
}

// simEntryDir makes a working directory with a fake scripts/sim-setup.sh
// (setupBody for a setup run; "--teardown" is logged) and a bin directory
// with a fake go that logs its arguments and the two URLs. It returns the
// directory, the environment (with staleSimEnv) and the log path.
func simEntryDir(t *testing.T, setupBody string) (dir string, env []string, logPath string) {
	t.Helper()
	dir = t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	files := map[string]string{
		"scripts/sim-setup.sh": "#!/bin/sh\ncase \"$1\" in --teardown*) echo \"teardown $*\" >> \"$CALLS_LOG\"; exit 0 ;; esac\necho setup >> \"$CALLS_LOG\"\n" + setupBody + "\n",
		"bin/go":               "#!/bin/sh\necho \"go $* WS=$IWDP_SIM_WS_URL PAGE=$IWDP_SIM_PAGE_URL\" >> \"$CALLS_LOG\"\n",
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"), "CALLS_LOG="+logPath)
	return dir, append(env, staleSimEnv...), logPath
}

func readCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// TestMakeSimTargetsCheckSetup runs the Makefile's sim-setup and test-e2e
// targets: a failed setup, an empty output or stale inherited URLs fail
// the target without running the tests; a good setup runs the tests with
// its fresh URLs, and test-e2e then tears down (review F6, round 3).
func TestMakeSimTargetsCheckSetup(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Fatalf("the Makefile tests need make on PATH: %v", err)
	}
	makefile, err := filepath.Abs("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"sim-setup", "test-e2e"} {
		for _, tc := range fakeSetups {
			t.Run(target+"/"+tc.name, func(t *testing.T) {
				dir, env, logPath := simEntryDir(t, tc.body)
				cmd := exec.CommandContext(t.Context(), "make", "-s", "-f", makefile, target)
				cmd.Dir, cmd.Env = dir, env
				out, err := cmd.CombinedOutput()
				calls := readCalls(t, logPath)
				if (err == nil) != tc.wantOK {
					t.Fatalf("make %s: error %v; want success %v\noutput:\n%s\ncalls:\n%s", target, err, tc.wantOK, out, calls)
				}
				if strings.Contains(calls, "--teardown\n") || (!tc.wantOK && strings.Contains(calls, "teardown --teardown-run "+freshToken)) {
					t.Fatalf("make %s ran a teardown that is not this run's:\n%s", target, calls)
				}
				if strings.Contains(calls+string(out), "stale.invalid") || strings.Contains(calls, "ffffffff") {
					t.Fatalf("make %s used the inherited URLs:\n%s%s", target, out, calls)
				}
				ranTests := strings.Contains(calls, "go test")
				if target == "test-e2e" {
					wantGo := "go test -tags=simulator ./e2e/ -v -count=1 -timeout=300s WS=" + freshWSURL + " PAGE=" + freshPageURL
					switch {
					case tc.wantOK && !strings.Contains(calls, wantGo):
						t.Fatalf("calls:\n%s\nwant %q", calls, wantGo)
					case !tc.wantOK && ranTests:
						t.Fatalf("make test-e2e ran the tests after a bad setup:\n%s", calls)
					case tc.wantOK && !strings.HasSuffix(calls, "teardown --teardown-run "+freshToken+"\n"):
						t.Fatalf("calls:\n%s\nwant a teardown of this run's token after the tests", calls)
					}
				} else if tc.wantOK && !strings.Contains(string(out), freshWSURL) {
					t.Fatalf("make sim-setup printed %q; want the fresh URLs", out)
				}
			})
		}
	}
}

// ciStep is one step of the e2e job of .github/workflows/test.yml: its
// id, its env entries (KEY=VALUE, expressions unresolved) and its run
// commands.
type ciStep struct {
	id  string
	env []string
	run string
}

// ciWorkflowStep reads the step name of .github/workflows/test.yml with a
// minimal reader for that file's layout (step keys indented 8 spaces,
// their blocks 10): the keys id, env and run (a one-line run or a
// "run: |" block); the other keys are ignored.
func ciWorkflowStep(t *testing.T, name string) ciStep {
	t.Helper()
	b, err := os.ReadFile("../.github/workflows/test.yml")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), "      - name: "+name+"\n")
	if !ok {
		t.Fatalf("test.yml has no step %q", name)
	}
	const keyIndent, blockIndent = "        ", "          "
	var step ciStep
	var run []string
	key := ""
lines:
	for _, line := range strings.Split(rest, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			if key == "run" {
				run = append(run, "")
			}
		case strings.HasPrefix(line, blockIndent):
			switch key {
			case "run":
				run = append(run, strings.TrimPrefix(line, blockIndent))
			case "env":
				k, v, ok := strings.Cut(trimmed, ": ")
				if !ok {
					t.Fatalf("step %q: env line %q is not KEY: value", name, line)
				}
				step.env = append(step.env, k+"="+v)
			}
		case strings.HasPrefix(line, keyIndent):
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			k, v, _ := strings.Cut(trimmed, ":")
			key, v = k, strings.TrimSpace(v)
			switch k {
			case "id":
				step.id = v
			case "run":
				if v != "|" {
					step.run, key = v, ""
				}
			}
		default: // the next step or job
			break lines
		}
	}
	if run != nil {
		step.run = strings.TrimRight(strings.Join(run, "\n"), "\n")
	}
	if step.run == "" {
		t.Fatalf("step %q has no run commands", name)
	}
	return step
}

// ciSimSetupStepName is the CI step that boots the Simulator.
const ciSimSetupStepName = "Boot iOS Simulator and start iwdp"

// readGitHubFile reads a GITHUB_ENV or GITHUB_OUTPUT file (KEY=VALUE
// lines) as a map; a later line wins, as on GitHub. A missing file is
// empty.
func readGitHubFile(t *testing.T, path string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, line := range strings.Split(readCalls(t, path), "\n") {
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: line %q is not KEY=VALUE", path, line)
		}
		m[k] = v
	}
	return m
}

// TestCISimSetupStepChecksSetup runs the CI setup step as GitHub runs it
// (bash -e): a failed setup, an empty output or stale inherited URLs fail
// the step and export nothing; a good setup exports its fresh URLs and
// its run token, and only a good setup sets the step output run_token,
// which the teardown step uses (review F6, round 3; round 5).
func TestCISimSetupStepChecksSetup(t *testing.T) {
	step := ciWorkflowStep(t, ciSimSetupStepName)
	tokenOnly := "echo \"export IWDP_SIM_RUN_TOKEN='" + freshToken + "'\""
	cases := append(fakeSetups[:len(fakeSetups):len(fakeSetups)], struct {
		name, body string
		wantOK     bool
	}{"setup prints a token but no URLs", tokenOnly, false})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, env, logPath := simEntryDir(t, tc.body)
			githubEnv, githubOutput := filepath.Join(dir, "github_env"), filepath.Join(dir, "github_output")
			cmd := exec.CommandContext(t.Context(), "bash", "-e", "-c", step.run)
			cmd.Dir, cmd.Env = dir, append(env, "GITHUB_ENV="+githubEnv, "GITHUB_OUTPUT="+githubOutput)
			out, err := cmd.CombinedOutput()
			exported, outputs, calls := readCalls(t, githubEnv), readCalls(t, githubOutput), readCalls(t, logPath)
			if (err == nil) != tc.wantOK {
				t.Fatalf("CI step: error %v; want success %v\noutput:\n%s\nGITHUB_ENV:\n%s", err, tc.wantOK, out, exported)
			}
			wantEnv, wantOutputs := "", ""
			if tc.wantOK {
				wantEnv = "IWDP_SIM_RUN_TOKEN=" + freshToken + "\nIWDP_SIM_WS_URL=" + freshWSURL + "\nIWDP_SIM_PAGE_URL=" + freshPageURL + "\n"
				wantOutputs = "run_token=" + freshToken + "\n"
			}
			if exported != wantEnv {
				t.Fatalf("GITHUB_ENV = %q; want %q", exported, wantEnv)
			}
			if outputs != wantOutputs {
				t.Fatalf("GITHUB_OUTPUT = %q; want %q", outputs, wantOutputs)
			}
			// A lease that this setup took, without the URLs, is torn
			// down by the step itself: no step output names it.
			if tc.body == tokenOnly && !strings.Contains(calls, "teardown --teardown-run "+freshToken+"\n") {
				t.Fatalf("calls:\n%s\nwant a teardown of this setup's token after its output had no URLs", calls)
			}
			if strings.Contains(calls, "ffffffff") || strings.Contains(calls, "--teardown\n") {
				t.Fatalf("the CI step ran a teardown that is not this run's:\n%s", calls)
			}
		})
	}
}

// TestSimDocsRunOnlyMakeTargets: the README's examples and the Makefile's
// comments run the Simulator setup only through make test-e2e and make
// sim-setup, which check it; a hand-written setup command line can hide a
// failed setup or reuse stale URLs (review round 3).
func TestSimDocsRunOnlyMakeTargets(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	inFence, sawMake := false, false
	for i, line := range strings.Split(string(readme), "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			continue
		}
		if strings.Contains(line, "sim-setup.sh") {
			t.Errorf("README.md:%d runs the setup script directly: %s", i+1, line)
		}
		sawMake = sawMake || strings.TrimSpace(line) == "make test-e2e"
	}
	if !sawMake {
		t.Error("README.md has no example line make test-e2e")
	}
	makefile, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(makefile), "\n") {
		if strings.HasPrefix(line, "#") && strings.Contains(line, "sim-setup.sh") {
			t.Errorf("Makefile:%d: a comment runs the setup script directly: %s", i+1, line)
		}
	}
}

// stepOutputRE matches the one expression form that the CI teardown
// step may use: an output of an earlier step.
var stepOutputRE = regexp.MustCompile(`^\$\{\{ steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+) \}\}$`)

// TestCITeardownStepActsOnlyOnItsOwnLease runs the real CI teardown step
// with the real setup script and a fake simctl, in the environment that
// GitHub gives it: the runner's environment, which holds an inherited
// IWDP_SIM_RUN_TOKEN of an earlier run that is still active, then the
// GITHUB_ENV entries of the earlier steps, then the step's own env:
// lines with steps.<setup id>.outputs.* taken from the setup step's
// GITHUB_OUTPUT. When this job's setup was refused, never ran or names
// another run's lease, the earlier run's Simulator stays booted and its
// lease stays; with this job's own token the step shuts down the
// Simulator that this job booted (review round 4, F16; round 5).
func TestCITeardownStepActsOnlyOnItsOwnLease(t *testing.T) {
	setup, teardown := ciWorkflowStep(t, ciSimSetupStepName), ciWorkflowStep(t, "Teardown simulator")
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// earlierRun boots the fake Simulator for an earlier run that is still
	// active and returns the runner environment, with that run's token
	// inherited, the fake Simulator, the token and the lease file.
	earlierRun := func(t *testing.T) (env []string, sim *fakeSim, earlier, lease string) {
		t.Helper()
		env, sim = simScriptEnv(t, "Shutdown")
		stdout, stderr, code := runSimScript(t, env, "--boot-simulator")
		if code != 0 {
			t.Fatalf("the earlier run's setup exited %d: %s", code, stderr)
		}
		earlier = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(stdout), "export IWDP_SIM_RUN_TOKEN='"), "'")
		if !runTokenRE.MatchString(earlier) {
			t.Fatalf("setup printed %q; want the export of its run token", stdout)
		}
		lease = filepath.Join(sim.stateDir, "iwdp-sim-"+strconv.Itoa(os.Getuid()), "lease")
		return append(env, "IWDP_SIM_RUN_TOKEN="+earlier), sim, earlier, lease
	}
	// githubFiles returns a GITHUB_ENV and a GITHUB_OUTPUT file with the
	// given contents.
	githubFiles := func(t *testing.T, envText, outputText string) (githubEnv, githubOutput string) {
		t.Helper()
		dir := t.TempDir()
		githubEnv, githubOutput = filepath.Join(dir, "github_env"), filepath.Join(dir, "github_output")
		for path, text := range map[string]string{githubEnv: envText, githubOutput: outputText} {
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return githubEnv, githubOutput
	}
	// runTeardown runs the teardown step in env after the earlier steps
	// wrote githubEnv and githubOutput (the setup step's outputs).
	runTeardown := func(t *testing.T, env []string, githubEnv, githubOutput string) {
		t.Helper()
		stepEnv := append([]string(nil), env...)
		for k, v := range readGitHubFile(t, githubEnv) {
			stepEnv = append(stepEnv, k+"="+v)
		}
		outputs := readGitHubFile(t, githubOutput)
		for _, kv := range teardown.env {
			k, v, _ := strings.Cut(kv, "=")
			if m := stepOutputRE.FindStringSubmatch(v); m != nil {
				v = "" // GitHub's value of an output that no step set
				if m[1] == setup.id {
					v = outputs[m[2]]
				}
			} else if strings.Contains(v, "${{") {
				t.Fatalf("the teardown step's env %s uses an expression that this test cannot resolve: %s", k, v)
			}
			stepEnv = append(stepEnv, k+"="+v)
		}
		cmd := exec.CommandContext(t.Context(), "bash", "-e", "-c", teardown.run)
		cmd.Dir, cmd.Env = repo, stepEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("CI teardown step: %v\n%s", err, out)
		}
	}
	wantEarlierRunKept := func(t *testing.T, sim *fakeSim, earlier, lease string) {
		t.Helper()
		if got := sim.simState(t); got != "Booted" || strings.Contains(sim.calls(t), "simctl shutdown") {
			t.Fatalf("the CI teardown stopped the earlier run's Simulator (state %s):\n%s", got, sim.calls(t))
		}
		if b, err := os.ReadFile(lease); err != nil || strings.TrimSpace(string(b)) != earlier {
			t.Fatalf("the earlier run's lease is gone or changed: %q, %v", b, err)
		}
	}

	t.Run("setup refused", func(t *testing.T) {
		env, sim, earlier, lease := earlierRun(t)
		githubEnv, githubOutput := githubFiles(t, "", "")
		cmd := exec.CommandContext(t.Context(), "bash", "-e", "-c", setup.run)
		cmd.Dir, cmd.Env = repo, append(env, "GITHUB_ENV="+githubEnv, "GITHUB_OUTPUT="+githubOutput)
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "holds the lease") {
			t.Fatalf("the CI setup step: error %v; want the refusal of the earlier run's lease\n%s", err, out)
		}
		runTeardown(t, env, githubEnv, githubOutput)
		wantEarlierRunKept(t, sim, earlier, lease)
	})
	t.Run("setup skipped", func(t *testing.T) {
		env, sim, earlier, lease := earlierRun(t)
		githubEnv, githubOutput := githubFiles(t, "", "")
		runTeardown(t, env, githubEnv, githubOutput)
		wantEarlierRunKept(t, sim, earlier, lease)
	})
	t.Run("another run's token", func(t *testing.T) {
		const other = "ffffffffffffffffffffffffffffffff"
		env, sim, earlier, lease := earlierRun(t)
		githubEnv, githubOutput := githubFiles(t, "IWDP_SIM_RUN_TOKEN="+other+"\n", "run_token="+other+"\n")
		runTeardown(t, env, githubEnv, githubOutput)
		wantEarlierRunKept(t, sim, earlier, lease)
	})
	t.Run("own token", func(t *testing.T) {
		env, sim, earlier, lease := earlierRun(t)
		// As a good setup of this job writes them (TestCISimSetupStepChecksSetup).
		githubEnv, githubOutput := githubFiles(t, "IWDP_SIM_RUN_TOKEN="+earlier+"\n", "run_token="+earlier+"\n")
		runTeardown(t, env, githubEnv, githubOutput)
		if got := sim.simState(t); got != "Shutdown" {
			t.Fatalf("the CI teardown with the run's own token left its Simulator %s; want Shutdown", got)
		}
		if _, err := os.Stat(lease); !os.IsNotExist(err) {
			t.Fatalf("the CI teardown with the run's own token kept its lease: %v", err)
		}
	})
}

// TestSimDocsReserveSimTeardownForACrashedRun: where the docs tell the
// user what to do about an earlier run, make sim-teardown is only the
// recovery after a crashed run; an earlier run that holds the lease may
// still be active (review round 4, F15).
func TestSimDocsReserveSimTeardownForACrashedRun(t *testing.T) {
	for _, path := range []string{"../README.md", "../docs/device-runbook.md"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "make sim-teardown") && strings.Contains(line, "earlier") && !strings.Contains(line, "crashed run") {
				t.Errorf("%s:%d tells the user to tear down an earlier run that may still be active: %s", path, i+1, line)
			}
		}
	}
}
