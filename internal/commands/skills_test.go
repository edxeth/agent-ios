package commands

import (
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/nnemirovsky/iwdp-cli/internal/skills"
)

// repoSkill reads the guide skill-data/<name>/SKILL.md from the repository
// checkout; skills/<name>/SKILL.md is only its discovery stub.
func repoSkill(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "skill-data", name, skills.FileName))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Design 4.2: "skills get|list|path" print the skills embedded in the
// binary; they need no daemon or page.
func TestSkillsCommandsAreLocal(t *testing.T) {
	for _, n := range []string{"skills list", "skills get", "skills path"} {
		c, words := Lookup(strings.Fields(n))
		if c == nil || words != 2 || c.Name != n {
			t.Fatalf("Lookup(%q) = %v, %d", n, c, words)
		}
		if !c.Local || c.Page != NoPage {
			t.Errorf("%s: Local %v, page need %v; want a local command without a page", n, c.Local, c.Page)
		}
	}
	if c, _ := Lookup([]string{"skills"}); c == nil || c.Name != "skills list" {
		t.Fatalf("bare skills = %v, want skills list (agent-browser's default)", c)
	}
}

func TestSkillsGetPrintsTheRepositorySkill(t *testing.T) {
	want := repoSkill(t, "iwdp")
	code, out, errOut, _ := runCLI("skills", "get", "iwdp")
	if code != ExitOK || out != want {
		t.Fatalf("skills get iwdp: exit %d, stderr %q; stdout differs from skill-data/iwdp/SKILL.md:\n%s", code, errOut, out)
	}
	// Several names print several whole skills in order; no name prints
	// every skill (the repository has one).
	code, out, _, _ = runCLI("skills", "get", "iwdp", "iwdp")
	if code != ExitOK || !strings.HasPrefix(out, want) || !strings.HasSuffix(out, want) || strings.Count(out, "name: iwdp\n") != 2 {
		t.Fatalf("skills get iwdp iwdp: exit %d, stdout %q; want the skill twice", code, out)
	}
	for _, args := range [][]string{{"skills", "get"}, {"skills", "get", "--all"}} {
		code, out, _, _ = runCLI(args...)
		if code != ExitOK || strings.Count(out, "---\nname: ") != 1 || !strings.Contains(out, "---\nname: iwdp\n") {
			t.Fatalf("%v: exit %d, want every skill, got %q", args, code, out)
		}
	}
	code, out, _, _ = runCLI("--json", "skills", "get", "iwdp")
	var got []skills.Skill
	if code != ExitOK || json.Unmarshal([]byte(out), &got) != nil || len(got) != 1 || got[0].Name != "iwdp" || got[0].Content != want || got[0].Description == "" {
		t.Fatalf("--json skills get iwdp: exit %d, stdout %q", code, out)
	}
	code, _, errOut, _ = runCLI("skills", "get", "nope")
	if code != ExitError || !strings.Contains(errOut, `unknown skill "nope"`) {
		t.Fatalf("skills get nope: exit %d, stderr %q; want exit 1 naming the skill", code, errOut)
	}
}

func TestSkillsListShowsNamesAndShortDescriptions(t *testing.T) {
	code, out, _, _ := runCLI("skills", "list")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != ExitOK || len(lines) != 1 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "iwdp ") {
		t.Fatalf("skills list: exit %d, stdout %q; want one line per skill", code, out)
	}
	for _, l := range lines {
		if n := len([]rune(l)); n > 2+len("iwdp")+2+listDescriptionRunes+3 {
			t.Errorf("line of %d runes is longer than the cut description allows: %q", n, l)
		}
	}
	code, out, _, _ = runCLI("--json", "skills")
	var got []skills.Skill
	if code != ExitOK || json.Unmarshal([]byte(out), &got) != nil || len(got) != 1 || got[0].Content != "" || got[0].Description == "" {
		t.Fatalf("--json skills: exit %d, stdout %q; want names and whole descriptions without content", code, out)
	}
}

func TestShortenCutsAtAWordBoundary(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"one two three four", 10, "one two..."},
		{"one, two three", 6, "one..."},
		{"abcdefghij", 4, "abcd..."},
		{"üüü ööö", 5, "üüü..."},
	} {
		if got := shorten(c.in, c.n); got != c.want {
			t.Errorf("shorten(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestSkillsPathInstallsIntoTheUserCache(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", t.TempDir()) // os.UserCacheDir reads XDG_CACHE_HOME first
	code, out, errOut, _ := runCLI("skills", "path", "iwdp")
	dir := strings.TrimSpace(out)
	if code != ExitOK || !strings.HasPrefix(dir, filepath.Join(cache, "iwdp", "skills")+string(filepath.Separator)) || filepath.Base(dir) != "iwdp" {
		t.Fatalf("skills path iwdp: exit %d, stdout %q, stderr %q; want <cache>/iwdp/skills/<version>/iwdp", code, out, errOut)
	}
	if got, err := os.ReadFile(filepath.Join(dir, skills.FileName)); err != nil || string(got) != repoSkill(t, "iwdp") {
		t.Fatalf("%s/SKILL.md: %v; want the repository skill", dir, err)
	}
	code, out, _, _ = runCLI("skills", "path")
	if all := strings.TrimSpace(out); code != ExitOK || all != filepath.Dir(dir) {
		t.Fatalf("skills path: exit %d, stdout %q; want %s", code, out, filepath.Dir(dir))
	}
	code, _, errOut, _ = runCLI("skills", "path", "nope")
	if code != ExitError || !strings.Contains(errOut, `unknown skill "nope"`) {
		t.Fatalf("skills path nope: exit %d, stderr %q", code, errOut)
	}
}

// A failure to read the embedded skills ends "skills list" and "skills
// get" with exit 1 and the cause on stderr, never an empty list.
func TestSkillsListAndGetReportAReadFailure(t *testing.T) {
	cause := errors.New("reading the embedded skill iwdp/SKILL.md: broken")
	oldList, oldAll := skillsList, skillsAll
	skillsList = func() ([]skills.Skill, error) { return nil, cause }
	skillsAll = func() ([]skills.Skill, error) { return nil, cause }
	t.Cleanup(func() { skillsList, skillsAll = oldList, oldAll })
	for _, args := range [][]string{{"skills", "list"}, {"skills", "get"}, {"skills", "get", "--all"}} {
		code, out, errOut, _ := runCLI(args...)
		if code != ExitError || out != "" || !strings.Contains(errOut, cause.Error()) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want exit 1 with the cause on stderr", args, code, out, errOut)
		}
	}
}

// "skills path" fails with exit 1 and a message that names the step when
// there is no user cache directory, or when the cache directory cannot
// hold the skills.
func TestSkillsPathReportsCacheFailures(t *testing.T) {
	t.Run("no cache directory", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "")
		t.Setenv("HOME", "") // os.UserCacheDir needs one of the two
		code, out, errOut, _ := runCLI("skills", "path")
		if code != ExitError || out != "" || !strings.Contains(errOut, "finding the user cache directory for the skills") {
			t.Fatalf("skills path without HOME: exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})
	t.Run("cache is a file", func(t *testing.T) {
		cache := filepath.Join(t.TempDir(), "cache")
		if err := os.WriteFile(cache, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_CACHE_HOME", cache)
		code, out, errOut, _ := runCLI("skills", "path", "iwdp")
		if code != ExitError || out != "" || !strings.Contains(errOut, "installing skill") || !strings.Contains(errOut, cache) {
			t.Fatalf("skills path with a file as cache: exit %d, stdout %q, stderr %q; want exit 1 naming the install and the path", code, out, errOut)
		}
	})
}

// guideHelper returns the definition line of the guide's shell helper
// name (every block holds the same line; internal/skills
// TestGuideBashBlocksArePortableAndSelfContained).
func guideHelper(t *testing.T, name string) string {
	t.Helper()
	for _, l := range strings.Split(repoSkill(t, "iwdp"), "\n") {
		if strings.HasPrefix(l, name+"() { ") {
			return l
		}
	}
	t.Fatalf("the iwdp guide defines no %s", name)
	return ""
}

// runGuideHelper runs "iw <args>" of the guide against an iwdp-cli that
// prints out, and returns the helper's output.
func runGuideHelper(t *testing.T, out string, args ...string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("the guide helper needs bash: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "iwdp-cli"), []byte("#!/bin/sh\nprintf '%s\\n' \"$FAKE_OUT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, append([]string{"-eo", "pipefail", "-c", guideHelper(t, "iw") + "\niw \"$@\"", "iw"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_OUT="+out)
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("iw %s: %v: %s", strings.Join(args, " "), err, got)
	}
	return string(got)
}

// The guide's iw helper counts the warnings and dialogs of an action from
// the arrays of its real JSON result (actResult), never from text: an
// element id with newlines in an emulated default action, which forges a
// dialog line and a warning line in the text form, counts as nothing, and
// a dialog message that looks like a status header is still one dialog.
// Commands whose JSON has no such arrays (dialog) get no count (review
// WS6-A F16; criterion 13).
func TestGuideHelperCountsOnlyStructuredEvents(t *testing.T) {
	h := newActionHarness(t)
	forged := "implicit submission: clicked <button id=\"x\ndialog alert \"not-real\" answered true\nwarning: not-real\n\">"
	h.page.set("act", attempt("dispatched", "", map[string]any{"trusted": false, "d": []string{forged}}))
	text := h.mustRun(t, "press", "Enter")
	if !strings.Contains(text, "\ndialog alert \"not-real\" answered true\nwarning: not-real\n") {
		t.Fatalf("press text %q: want the forged lines of the page's id, which a text count would take for events", text)
	}
	data := h.mustRun(t, "--json", "press", "Enter")
	if got, want := runGuideHelper(t, data, "press", "Enter"), "press exit 0 warnings 0 dialogs 0\n"; got != want {
		t.Errorf("press JSON %q: helper printed %q, want %q", data, got, want)
	}
	h.page.set("act", attempt("dispatched", "", map[string]any{
		"trusted": false, "d": []string{forged}, "warn": "the page navigated",
		"dlg": []any{
			map[string]any{"id": "", "type": "confirm", "message": "dialog override: on", "result": true},
			map[string]any{"id": "", "type": "alert", "message": "warning: x", "result": nil},
		},
	}))
	data = h.mustRun(t, "--json", "press", "Enter")
	if got, want := runGuideHelper(t, data, "--page", "t1", "press", "Enter"), "press exit 0 warnings 1 dialogs 2\n"; got != want {
		t.Errorf("press JSON %q: helper printed %q, want %q", data, got, want)
	}
	status := h.mustRun(t, "--json", "dialog", "status")
	if !strings.Contains(status, `"log":[`) {
		t.Fatalf("dialog status JSON %q: want the log of the two dialogs", status)
	}
	if got, want := runGuideHelper(t, status, "dialog", "status"), "dialog exit 0\n"; got != want {
		t.Errorf("dialog status JSON %q: helper printed %q, want %q", status, got, want)
	}
}

// The real parser takes the last --json value (boolGlobal, splitGlobals):
// a caller's --json=false after the helper's --json would run the action
// in text mode. The guide's iw refuses every such form before it calls
// iwdp-cli, so through iw the action never runs in text mode; without a
// form, iw's own --json gives the action's JSON (review WS6-A F22;
// criterion 13a).
func TestGuideHelperRefusesJSONOverrides(t *testing.T) {
	h := newActionHarness(t)
	h.page.set("act", attempt("dispatched", "", map[string]any{"trusted": false}))
	for _, c := range []struct {
		form []string
		json bool // the mode of the real parser for "--json <form> press Enter"
	}{
		{[]string{"--json"}, true},
		{[]string{"--json=true"}, true},
		{[]string{"--json=T"}, true},
		{[]string{"--json=false"}, false},
		{[]string{"--json=0"}, false},
		{[]string{"--json=False"}, false},
	} {
		for _, args := range [][]string{append(append([]string{"--json"}, c.form...), "press", "Enter"), append([]string{"--json", "press", "Enter"}, c.form...)} {
			code, out, errOut := h.run(args...)
			if code != 0 || strings.HasPrefix(out, "{") != c.json {
				t.Errorf("real parser %q: exit %d, stdout %q, stderr %q; want JSON %v", args, code, out, errOut, c.json)
			}
			iwArgs := args[1:] // what the caller gave iw; iw adds the first --json
			got, code, calls := runGuideHelperStatus(t, "{}", iwArgs...)
			if code != 2 || got != "iw: usage: iw sets the output format itself; remove --json and --content-boundaries\n" || calls != 0 {
				t.Errorf("iw %q: exit %d, output %q, %d iwdp-cli calls; want the refusal, exit 2, no call", iwArgs, code, got, calls)
			}
		}
	}
	// The reviewer's lines of round 7: the CLI reads the first word after
	// --session as its value, so "--" there ends no options and the later
	// --json=false runs the action in text mode; a value "--json=false"
	// leaves JSON on. iw refuses both, because it parses none of the
	// caller's words (criterion 13b).
	for _, c := range []struct {
		args []string
		json bool
	}{
		{[]string{"--json", "--session", "--", "press", "Enter", "--json=false"}, false},
		{[]string{"--json", "--session", "--json=false", "press", "Enter"}, true},
	} {
		code, out, errOut := h.run(c.args...)
		if code != 0 || strings.HasPrefix(out, "{") != c.json {
			t.Errorf("real parser %q: exit %d, stdout %q, stderr %q; want JSON %v", c.args, code, out, errOut, c.json)
		}
		got, code, calls := runGuideHelperStatus(t, "{}", c.args[1:]...)
		if code != 2 || got != "iw: usage: iw sets the output format itself; remove --json and --content-boundaries\n" || calls != 0 {
			t.Errorf("iw %q: exit %d, output %q, %d iwdp-cli calls; want the refusal, exit 2, no call", c.args[1:], code, got, calls)
		}
	}
	code, out, _ := h.run("--json", "press", "Enter")
	if code != 0 || !strings.HasPrefix(out, "{") {
		t.Fatalf("real parser --json press Enter: exit %d, stdout %q; want JSON", code, out)
	}
	if got, code, calls := runGuideHelperStatus(t, out, "press", "Enter"); code != 0 || got != "press exit 0 warnings 0 dialogs 0\n" || calls != 1 {
		t.Errorf("iw press Enter: exit %d, output %q, %d calls; want the counts of the JSON result", code, got, calls)
	}
}

// runGuideHelperStatus is runGuideHelper that also returns the exit status
// and the number of iwdp-cli calls.
func runGuideHelperStatus(t *testing.T, out string, args ...string) (string, int, int) {
	t.Helper()
	return runHelperStatus(t, "iw", out, args...)
}

// runHelperStatus runs "<name> <args>" of the guide's helper name against
// an iwdp-cli that prints out, and returns the helper's output, its exit
// status and the number of iwdp-cli calls.
func runHelperStatus(t *testing.T, name, out string, args ...string) (string, int, int) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	if err := os.WriteFile(filepath.Join(dir, "iwdp-cli"), []byte("#!/bin/sh\necho call >> \""+log+"\"\nprintf '%s\\n' \"$FAKE_OUT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{"-eo", "pipefail", "-c", guideHelper(t, name) + "\n" + name + " \"$@\"", name}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_OUT="+out)
	got, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(got), code, strings.Count(string(data), "call\n")
}

// helpRefusal is the usage line of the guide's helper name for a help
// switch.
func helpRefusal(name string) string {
	return name + ": usage: " + name + " runs no help; read it with iwdp-cli <command> --help\n"
}

// The real CLI prints help and exits 0 for -h or --help anywhere among the
// global options, also with a command; through a helper, that help would
// be labeled with the switch, and iw's filter would fail on the help text.
// Both helpers refuse a help switch before any iwdp-cli call (review WS6-A
// F24; criterion 10a).
func TestGuideHelpersRefuseHelpSwitches(t *testing.T) {
	h := newActionHarness(t)
	for _, c := range []struct {
		name string
		args []string // the caller's words; iw adds --json
	}{
		{"iw", []string{"-h", "press", "Enter"}},
		{"iw", []string{"press", "Enter", "--help"}},
		{"iw", []string{"--session", "x", "-h", "press", "Enter"}},
		{"iwr", []string{"-h", "get", "text", "body"}},
		{"iwr", []string{"get", "text", "body", "--help"}},
	} {
		args := c.args
		if c.name == "iw" {
			args = append([]string{"--json"}, args...)
		}
		code, out, errOut := h.run(args...)
		if code != 0 || !strings.HasPrefix(out, "Usage: iwdp-cli ") {
			t.Errorf("real CLI %q: exit %d, stdout %q, stderr %q; want its help, exit 0", args, code, out, errOut)
		}
		got, code, calls := runHelperStatus(t, c.name, out, c.args...)
		if code != 2 || got != helpRefusal(c.name) || calls != 0 {
			t.Errorf("%s %q: exit %d, output %q, %d iwdp-cli calls; want the refusal, exit 2, no call", c.name, c.args, code, got, calls)
		}
	}
}

// iwTokens are the words of the generated helper lines: the words that set
// the output format, the help switches, "--", the global options with and
// without a value, "--session=--", plain values and command words
// (criteria 10a and 13b).
var iwTokens = []string{
	"--", "--json", "--json=false", "--json=true", "--content-boundaries", "--content-boundaries=false",
	"-h", "--help", "--session", "--page", "--session=--", "--timeout", "--takeover",
	"x", "t1", "5", "Enter", "@e1", "press", "dialog", "status", "tab", "list",
}

// iwOptions are the global options that the generated lines put before
// the command, most with a value; the values include "--", help switches
// and words that set the output format, which the CLI reads as values
// there.
var iwOptions = [][]string{
	{"--session", "x"},
	{"--session", "--"},
	{"--session", "--json=false"},
	{"--session", "-h"},
	{"--session=--"},
	{"--page", "t1"},
	{"--page", "--"},
	{"--page", "--json"},
	{"--page", "--help"},
	{"--timeout", "5"},
	{"--takeover"},
	{"--json"},
	{"--json=false"},
	{"--content-boundaries"},
	{"-h"},
	{"--"},
}

// iwPaths are command paths of the registry, of one, two and three words.
var iwPaths = [][]string{
	{"press", "Enter"},
	{"click", "@e1"},
	{"dialog", "status"},
	{"dialog", "accept"},
	{"tab", "list"},
	{"tab", "t1"},
	{"session", "info"},
	{"device", "reserve"},
	{"network", "har", "start"},
}

// iwLine is a generated argument list of a helper (testing/quick
// Generator): up to three global options or single tokens, then, mostly,
// a command path, then up to three tokens.
type iwLine []string

func (iwLine) Generate(r *rand.Rand, _ int) reflect.Value {
	var l []string
	for range r.Intn(4) {
		if r.Intn(4) == 0 {
			l = append(l, iwTokens[r.Intn(len(iwTokens))])
		} else {
			l = append(l, iwOptions[r.Intn(len(iwOptions))]...)
		}
	}
	if r.Intn(5) != 0 {
		l = append(l, iwPaths[r.Intn(len(iwPaths))]...)
	}
	for range r.Intn(4) {
		l = append(l, iwTokens[r.Intn(len(iwTokens))])
	}
	return reflect.ValueOf(iwLine(l))
}

// formatWord reports whether w sets the output format: criterion 13b
// names --json and --content-boundaries, alone or with "=".
func formatWord(w string) bool {
	return w == "--json" || w == "--content-boundaries" || strings.HasPrefix(w, "--json=") || strings.HasPrefix(w, "--content-boundaries=")
}

// helpWord reports whether w is a help switch (criterion 10a).
func helpWord(w string) bool { return w == "-h" || w == "--help" }

// The guide's helpers against the real parser, for generated lines of
// tricky words (review WS6-A F22 and F24; criteria 10, 10a and 13b).
//   - iw refuses a line exactly when it holds a word that sets the output
//     format or a help switch, and iwr exactly when it holds a help
//     switch, wherever the word stands: exit 2, the usage line of that
//     class of word, and no iwdp-cli call.
//   - A helper passes any other line on unchanged, iw after its own
//     "--json". When Parse accepts that call, it finds a command and no
//     help request, iw's call has JSON on and content boundaries off, and
//     the helper's status label is the first word of the command path.
func TestGuideIwAgreesWithTheParser(t *testing.T) {
	noEnv := func(string) string { return "" }
	for _, h := range []struct {
		name    string
		prefix  []string                     // the words that the helper adds
		refuse  map[string]func(string) bool // usage line -> the words it refuses
		content string                       // the stdout that the helper prints before its label
	}{
		{"iw", []string{"--json"}, map[string]func(string) bool{
			"iw: usage: iw sets the output format itself; remove --json and --content-boundaries\n": formatWord,
			helpRefusal("iw"): helpWord,
		}, ""},
		{"iwr", nil, map[string]func(string) bool{helpRefusal("iwr"): helpWord}, "{}\n"},
	} {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			def := guideHelper(t, h.name)
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			// The fake logs each call as "call", then NUL and each
			// argument; the generated words hold no NUL and no newline.
			fake := "#!/bin/sh\n{ printf call; for a do printf '\\0%s' \"$a\"; done; printf '\\n'; } >> \"" + log + "\"\nprintf '{}\\n'\n"
			if err := os.WriteFile(filepath.Join(dir, "iwdp-cli"), []byte(fake), 0o700); err != nil {
				t.Fatal(err)
			}
			env := append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			refusedWord := func(w string) bool {
				for _, is := range h.refuse {
					if is(w) {
						return true
					}
				}
				return false
			}
			var refused, accepted, parsed int
			prop := func(l iwLine) bool {
				if err := os.Remove(log); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				cmd := exec.Command("bash", append([]string{"-eo", "pipefail", "-c", def + "\n" + h.name + " \"$@\"", h.name}, l...)...)
				cmd.Env = env
				out, err := cmd.CombinedOutput()
				code := 0
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					code = exit.ExitCode()
				} else if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(log)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				var calls [][]string
				for _, c := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
					if c != "" {
						calls = append(calls, strings.Split(c, "\x00")[1:])
					}
				}
				if is, ok := h.refuse[string(out)]; ok && code == 2 {
					refused++
					if len(calls) != 0 || !slices.ContainsFunc(l, is) {
						t.Logf("%s %q: refused with %q and calls %q; want that usage line only for a word of its class, and no call", h.name, l, out, calls)
						return false
					}
					return true
				}
				// The helper accepted the line: the parser's view of the
				// call comes first, then the refusal rule.
				accepted++
				want := append(slices.Clone(h.prefix), l...)
				if len(calls) != 1 || !slices.Equal(calls[0], want) {
					t.Logf("%s %q: exit %d, output %q, calls %q; want one call %q", h.name, l, code, out, calls, want)
					return false
				}
				if inv, err := Parse(calls[0], noEnv); err == nil { // else the CLI runs nothing
					parsed++
					g := inv.Input.Global
					if inv.Cmd == nil || inv.Help {
						t.Logf("%s %q: Parse gives help %v, group %q, command %v; want a command and no help", h.name, l, inv.Help, inv.Group, inv.Cmd != nil)
						return false
					}
					if h.name == "iw" && (!g.JSON || g.ContentBoundaries) {
						t.Logf("iw %q: Parse of %q gives JSON %v, content boundaries %v; want JSON on, boundaries off", l, inv.Cmd.Name, g.JSON, g.ContentBoundaries)
						return false
					}
					if label := h.content + strings.Fields(inv.Cmd.Name)[0] + " exit 0\n"; code != 0 || string(out) != label {
						t.Logf("%s %q: exit %d, output %q; want %q for the command %q", h.name, l, code, out, label, inv.Cmd.Name)
						return false
					}
				}
				if slices.ContainsFunc(l, refusedWord) {
					t.Logf("%s %q: accepted, exit %d, output %q; want a refusal, exit 2, no call", h.name, l, code, out)
					return false
				}
				return true
			}
			if err := quick.Check(prop, &quick.Config{MaxCount: iwLines}); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d lines: %d refused, %d accepted, %d of them parsed", h.name, iwLines, refused, accepted, parsed)
			// The floors keep the property from passing on lines that
			// never reach the parser or never parse.
			if refused < iwLines/10 || accepted < iwLines/10 || parsed < iwLines/20 {
				t.Errorf("%s: %d refused, %d accepted, %d parsed of %d lines; want at least %d refused, %d accepted and %d parsed", h.name, refused, accepted, parsed, iwLines, iwLines/10, iwLines/10, iwLines/20)
			}
		})
	}
}

// iwLines is the number of generated lines per helper; each runs bash,
// and iw runs python3 for an accepted one (10 to 25 s for iw, by load). About 10%
// of iw's lines parse, so its parsed floor (iwLines/20) lies about four
// standard deviations below the mean.
const iwLines = 500
