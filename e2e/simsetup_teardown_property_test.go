package e2e_test

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

// teardownWorld is one generated combination of an iwdp record and the
// process that its pid names, for TestSimTeardownSignalsOnlyAVerifiedIWDPProperty.
type teardownWorld struct {
	Linux        bool   // the system: Linux (pidfd, /proc) or macOS (signal by pid, KERN_PROCARGS2 built from the probe's /proc)
	Kind         string // the record: "sim" (scripts/sim-setup.sh's iwdp.pid), "cli" (iwdp-cli-started-*), "mcp" (iwdp-mcp-started-*) or "other" (a JSON record with another name)
	Validity     string // the record: "valid", "public" (others can write it) or "no-fields" (only the pid)
	Existence    string // "alive", "gone" (the probe was killed) or "not-permitted" (the helper's test seam)
	StartMatches bool   // ps shows the recorded start time
	TokenIn      string // where the process carries the recorded token: "kind" (the variable of the record's kind), "other-var" (another variable) or "none"
	IsIWDP       bool   // the program is ios_webkit_debug_proxy
	ExitsOnTerm  bool   // SIGTERM ends the process; else it ignores SIGTERM
	ReplacedAt   string // "" or when another program replaces the record with one that names a decoy: "uname" (right after the helper read it) or "ps" (at its start-time step)
}

// kindVariable is the variable that carries the start token for each
// record kind (docs/device-runbook.md, Stopping an iwdp by hand, and the
// header of scripts/iwdp-owner-check.sh); a JSON record of another name
// has none.
var kindVariable = map[string]string{"sim": simTokenEnv, "cli": "IWDP_CLI_START_TOKEN", "mcp": "IWDP_MCP_START_TOKEN"}

// kindFile is the record's file name for each kind.
var kindFile = map[string]string{
	"sim": "iwdp.pid", "cli": "iwdp-cli-started-0-127.0.0.1:9221.json",
	"mcp": "iwdp-mcp-started-0-127.0.0.1:9221.json", "other": "iwdp-started-0-127.0.0.1:9221.json",
}

// likely is true with probability 3/4, so that the generated worlds
// often reach "verified", which needs every check to pass.
func likely(r *rand.Rand) bool { return r.Intn(4) != 0 }

// Generate makes teardownWorld a quick.Generator.
func (teardownWorld) Generate(r *rand.Rand, _ int) reflect.Value {
	w := teardownWorld{
		Linux:        runtime.GOOS == "linux" && r.Intn(2) == 0,
		Kind:         []string{"sim", "sim", "cli", "mcp", "other"}[r.Intn(5)],
		Validity:     []string{"valid", "valid", "valid", "valid", "public", "no-fields"}[r.Intn(6)],
		Existence:    []string{"alive", "alive", "alive", "gone", "not-permitted"}[r.Intn(5)],
		StartMatches: likely(r),
		TokenIn:      []string{"kind", "kind", "kind", "kind", "kind", "kind", "other-var", "none"}[r.Intn(8)],
		IsIWDP:       likely(r),
		ExitsOnTerm:  r.Intn(2) == 0,
		ReplacedAt:   []string{"", "uname", "ps"}[r.Intn(3)],
	}
	return reflect.ValueOf(w)
}

// enumeratedTeardownWorlds are the worlds that the property checks on
// every run, before the generated ones (review WS3 round 8: with the
// Validity dimension, 30 generated worlds drew the worlds that catch
// mutations M6, R1 and F1 only for some seeds). They hold every
// combination of the dimensions that decide which steps the helper, the
// teardown and --forget reach and what a replacement of the record
// changes: Kind, Validity, Existence and ReplacedAt, and for a live
// process ExitsOnTerm. Every other dimension passes its check (the start
// time, the token in the kind's variable, the program), so that a live
// process reaches "verified" and a mutation that changes a later step
// shows; the generated worlds vary them. Two classes are reduced, because
// the helper ends at the record for them, before uname, so ReplacedAt
// cannot act: a record of another kind (one world per Existence) and an
// invalid record (every Kind and Existence, ReplacedAt "uname", so a
// mutation that checks on meets the replacement). Linux comes from r, as
// in Generate. M6 (re-read after step 1) needs a valid, live, verified
// record replaced at uname; R1 (teardown removes without --forget) a valid
// Simulator record of a gone process replaced at uname; F1 (--forget
// without the comparison) the same for a JSON record; V1 (the record
// checked after the existence) an invalid record of a gone process.
func enumeratedTeardownWorlds(r *rand.Rand) []teardownWorld {
	var worlds []teardownWorld
	add := func(kind, validity, existence, replacedAt string, exitsOnTerm bool) {
		worlds = append(worlds, teardownWorld{
			Linux: runtime.GOOS == "linux" && r.Intn(2) == 0, Kind: kind, Validity: validity, Existence: existence,
			StartMatches: true, TokenIn: "kind", IsIWDP: true, ExitsOnTerm: exitsOnTerm, ReplacedAt: replacedAt,
		})
	}
	existences := []string{"alive", "gone", "not-permitted"}
	for _, kind := range []string{"sim", "cli", "mcp"} {
		for _, existence := range existences {
			for _, replacedAt := range []string{"", "uname", "ps"} {
				add(kind, "valid", existence, replacedAt, true)
				if existence == "alive" {
					add(kind, "valid", existence, replacedAt, false)
				}
			}
			for _, validity := range []string{"public", "no-fields"} {
				add(kind, validity, existence, "uname", true)
			}
		}
	}
	for _, existence := range existences {
		add("other", "valid", existence, "uname", true)
	}
	return worlds
}

// verdict is the model of iwdp-owner-check.sh for w: its result word,
// from the documented record kinds and order of checks, for the record as
// it was when the check began.
func (w teardownWorld) verdict() string {
	switch {
	case w.Kind == "other":
		return "cannot-check"
	case w.Validity != "valid": // the record is checked before the process
		return "cannot-check"
	case w.Existence == "gone":
		return "gone"
	case w.Existence == "not-permitted":
		return "cannot-check"
	case !w.StartMatches:
		return "mismatch"
	case w.TokenIn != "kind", !w.IsIWDP:
		return "mismatch"
	}
	return "verified"
}

// replacedDuringCheck reports whether the record is replaced while a check
// runs. The fake uname replaces it when the helper asks for the system,
// after it read the record and its kind; the fake ps when the helper reads
// the start time (step 3), after the process's existence.
func (w teardownWorld) replacedDuringCheck() bool {
	if w.Kind == "other" || w.Validity != "valid" { // the checks end before uname
		return false
	}
	switch w.ReplacedAt {
	case "uname":
		return true
	case "ps":
		return w.Existence == "alive"
	}
	return false
}

// forgetVerdict is the model of 'iwdp-owner-check.sh --forget' for w, run
// on the original record when its process is gone (goneNow) or not: its
// result word, and whether it removes the record. It removes the record
// only when the process of its one read is gone and the path still names
// that record; the fake uname replaces the record right after that read.
func (w teardownWorld) forgetVerdict(goneNow bool) (string, bool) {
	switch {
	case w.Kind == "other", w.Validity != "valid":
		return "cannot-check", false
	case goneNow && w.ReplacedAt == "uname":
		return "cannot-check", false
	case goneNow:
		return "gone", true
	}
	return w.verdict(), false
}

// teardownPropertyCount is the number of generated worlds, after the
// enumerated ones: IWDP_TEARDOWN_PROPERTY_COUNT, else 30.
func teardownPropertyCount(t *testing.T) int {
	if s := os.Getenv("IWDP_TEARDOWN_PROPERTY_COUNT"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("IWDP_TEARDOWN_PROPERTY_COUNT=%q: want a positive integer", s)
		}
		return n
	}
	return 30
}

// teardownPropertySeed is the seed of the generated worlds:
// IWDP_TEARDOWN_PROPERTY_SEED, else the time. It is logged, so a failing
// run can be repeated.
func teardownPropertySeed(t *testing.T) int64 {
	seed := time.Now().UnixNano()
	if s := os.Getenv("IWDP_TEARDOWN_PROPERTY_SEED"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("IWDP_TEARDOWN_PROPERTY_SEED=%q: want an integer", s)
		}
		seed = n
	}
	t.Logf("generated worlds: IWDP_TEARDOWN_PROPERTY_SEED=%d", seed)
	return seed
}

// teardownRecord is the text of a record of kind for pid and token.
// Without a token it holds only the pid.
func teardownRecord(kind string, pid int, token string) string {
	if token == "" {
		if kind == "sim" {
			return strconv.Itoa(pid)
		}
		return fmt.Sprintf(`{"pid":%d}`, pid)
	}
	if kind == "sim" {
		return simRecord(pid, token, recordedIWDPStart, "")
	}
	return fmt.Sprintf(`{"pid":%d,"start":%q,"token":%q}`, pid, recordedIWDPStart, token)
}

// TestSimTeardownSignalsOnlyAVerifiedIWDPProperty: over generated records
// and processes (the record's kind and validity, review WS3 round 6:
// an invalid record is cannot-check, also when its process is gone, so it
// is never removed; start time, token and program each
// match or not; the process exits on SIGTERM or ignores it; it is alive,
// gone or of another user; another program replaces the record during the
// check or not):
//   - iwdp-owner-check.sh reports the result of its documented checks for
//     the record as it was when the check began;
//   - the Simulator teardown (for the Simulator's record) and
//     'iwdp-owner-check.sh --terminate' (the manual stop, for the JSON
//     records) signal the process if and only if the check verifies it,
//     and never the decoy that a replaced record names;
//   - teardown removes the record if and only if the process of the
//     record that it read is gone (before the teardown, or after its
//     SIGTERM) and the path still names that record (review WS3 round 5,
//     r2: it removed a record that had replaced the one it read);
//     --terminate never removes a record, and --forget (the last step of
//     the manual stop) removes it under the same condition, for its own
//     read;
//   - no output holds the start token or the process's command line or
//     environment, and no command gets the token as an argument.
//
// Each world is a real probe process with the fakes of
// TestSimTeardownStopsOnlyAVerifiedIWDP; "gone" is a killed probe and
// "not-permitted" the helper's test seam, so a live probe also shows that
// nothing signals it then.
func TestSimTeardownSignalsOnlyAVerifiedIWDPProperty(t *testing.T) {
	seen := map[string]int{} // the generated verdicts, logged
	prop := func(w teardownWorld) bool {
		token := randomHex(t, 16)
		env := []string{"FOREIGN_SECRET=" + randomHex(t, 8)}
		probeToken := randomHex(t, 16)
		switch w.TokenIn {
		case "kind":
			v := kindVariable[w.Kind]
			if v == "" {
				v = "IWDP_CLI_START_TOKEN"
			}
			env = append(env, v+"="+token)
		case "other-var":
			v := "IWDP_CLI_START_TOKEN"
			if w.Kind == "cli" {
				v = "IWDP_MCP_START_TOKEN"
			}
			env = append(env, v+"="+token)
		default:
			env = append(env, kindVariable["sim"]+"="+probeToken, "IWDP_CLI_START_TOKEN="+probeToken)
		}
		argv0 := "python3"
		if w.IsIWDP {
			argv0 = iwdpProgram
		}
		probe := startOwnerProbe(t, probeSpec{argv0: argv0, env: env, exitOnTerm: w.ExitsOnTerm})
		world := ownerWorld{system: "Darwin", psStart: recordedIWDPStart, notPermitted: w.Existence == "not-permitted"}
		if w.Linux {
			world.system = "Linux"
		}
		if !w.StartMatches {
			world.psStart = otherIWDPStart
		}
		if w.Existence == "gone" {
			probe.kill(t)
		}
		recordToken := token
		if w.Validity == "no-fields" {
			recordToken = ""
		}
		original := teardownRecord(w.Kind, probe.pid, recordToken)
		run := prepareOwnerRun(t, world, probe, "")
		record := run.record
		if w.Kind != "sim" {
			record = filepath.Join(t.TempDir(), kindFile[w.Kind])
		}
		// The decoy is an iwdp with a token of its own, which the
		// replacement record names: a check that mixed the two records'
		// fields would see another token.
		var decoy *probeProcess
		decoyToken := randomHex(t, 16)
		swapTo := filepath.Join(t.TempDir(), "replacement")
		if w.ReplacedAt != "" {
			v := kindVariable[w.Kind]
			if v == "" {
				v = "IWDP_CLI_START_TOKEN"
			}
			decoy = startOwnerProbe(t, probeSpec{argv0: iwdpProgram, env: []string{v + "=" + decoyToken}, exitOnTerm: true})
			if err := os.WriteFile(swapTo, []byte(teardownRecord(w.Kind, decoy.pid, decoyToken)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			run.env = append(run.env, "FAKE_SWAP_TO="+swapTo, "FAKE_SWAP_RECORD="+record)
			if w.ReplacedAt == "uname" {
				run.env = append(run.env, "FAKE_SWAP_AT_UNAME=1")
			}
		}
		// reset writes the original record and arms the replacement again.
		reset := func() {
			if err := os.WriteFile(record, []byte(original+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			mode := os.FileMode(0o600)
			if w.Validity == "public" {
				mode = 0o606
			}
			if err := os.Chmod(record, mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(swapTo + ".done"); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		helper := func(args ...string) (string, int) {
			check := exec.CommandContext(t.Context(), "bash", append([]string{"../scripts/iwdp-owner-check.sh"}, args...)...)
			check.Env = run.env
			out, _ := check.CombinedOutput()
			return string(out), check.ProcessState.ExitCode()
		}

		want := w.verdict()
		wantCode := map[string]int{"verified": 0, "mismatch": 1, "cannot-check": 2, "gone": 3}[want]
		ok := true
		fail := func(format string, args ...any) {
			t.Errorf("%+v: "+format, append([]any{w}, args...)...)
			ok = false
		}
		reset()
		out, code := helper(record)
		if word, _, _ := strings.Cut(out, ":"); word != want || code != wantCode {
			fail("iwdp-owner-check.sh printed %q, exit %d; want %s", out, code, want)
		}
		if _, err := os.Stat(swapTo + ".done"); w.replacedDuringCheck() && err != nil {
			fail("the record was not replaced during the check: %v", err)
		}
		reset()
		output := out
		// sent: the helper says that it sent SIGTERM, which the probe's
		// handler records soon after; teardown waits for the process
		// itself.
		sent := false
		if w.Kind == "sim" {
			_, stderr, code := runSimScript(t, run.env, "--teardown")
			output += stderr
			if code != 0 {
				fail("--teardown exited %d: %s", code, stderr)
			}
			processGone := want == "gone" || (want == "verified" && w.ExitsOnTerm)
			wantRemoved := processGone && !w.replacedDuringCheck()
			if got := !run.recordKept(t); got != wantRemoved {
				fail("record removed %v; want %v (process gone %v, record replaced %v); stderr: %s", got, wantRemoved, processGone, w.replacedDuringCheck(), stderr)
			}
		} else {
			out, code := helper("--terminate", record)
			output += out
			sent = strings.Contains(out, "SIGTERM was sent")
			if word, _, _ := strings.Cut(out, ":"); word != want || code != wantCode {
				fail("iwdp-owner-check.sh --terminate printed %q, exit %d; want %s", out, code, want)
			}
			if _, err := os.Lstat(record); err != nil {
				fail("the helper removed the record: %v", err)
			}
			// The last step of the manual stop, on the original record
			// with the replacement armed again, once the process that got
			// SIGTERM has exited.
			goneNow := want == "gone" || (want == "verified" && w.ExitsOnTerm)
			if sent && w.ExitsOnTerm && !probe.awaitExit(t) {
				fail("the probe did not exit after SIGTERM")
			}
			reset()
			out, code = helper("--forget", record)
			output += out
			wantForget, wantRemoved := w.forgetVerdict(goneNow)
			if word, _, _ := strings.Cut(out, ":"); word != wantForget || code != map[string]int{"verified": 0, "mismatch": 1, "cannot-check": 2, "gone": 3}[wantForget] {
				fail("iwdp-owner-check.sh --forget printed %q, exit %d; want %s", out, code, wantForget)
			}
			if _, err := os.Lstat(record); (err == nil) == wantRemoved {
				fail("--forget: record removed %v; want %v (process gone %v); output: %s", err != nil, wantRemoved, goneNow, out)
			}
			if aside, _ := filepath.Glob(filepath.Join(filepath.Dir(record), ".*.forget-*")); len(aside) != 0 {
				fail("--forget left %v", aside)
			}
			seen[fmt.Sprintf("--forget %s, removed %v, replaced at %q", wantForget, wantRemoved, w.ReplacedAt)]++
		}
		if got := probe.signaled() || sent && probe.awaitSignal(t, decoy); got != (want == "verified") {
			fail("signaled %v, but the check said %s; output: %s", got, want, output)
		}
		if decoy != nil && decoy.signaled() {
			fail("signaled the decoy that the replaced record names; output: %s", output)
		}
		for _, secret := range append([]string{token, probeToken, decoyToken, probe.marker}, env...) {
			if strings.Contains(output, secret) {
				fail("the output holds %q:\n%s", secret, output)
			}
		}
		if calls := run.sim.calls(t); strings.Contains(calls, token) || strings.Contains(calls, decoyToken) {
			fail("a command got the start token as an argument:\n%s", calls)
		}
		replaced := "not replaced"
		if w.replacedDuringCheck() {
			replaced = "replaced at " + w.ReplacedAt
		}
		seen[fmt.Sprintf("%s %s %s, %s", w.Validity, w.Kind, want, replaced)]++
		return ok
	}
	cfg := &quick.Config{MaxCount: teardownPropertyCount(t), Rand: rand.New(rand.NewSource(teardownPropertySeed(t)))}
	enumerated := enumeratedTeardownWorlds(cfg.Rand)
	for i, w := range enumerated {
		if !prop(w) {
			t.Errorf("enumerated world #%d of %d failed: %+v", i+1, len(enumerated), w)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Fatal(err)
	}
	t.Logf("generated verdicts: %v", seen)
}
