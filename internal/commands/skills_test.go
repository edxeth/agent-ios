package commands

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/skills"
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

// skills get prints each guide without its front matter (an agent that
// runs it has chosen the skill already); --json keeps the whole SKILL.md.
func TestSkillsGetPrintsTheRepositorySkill(t *testing.T) {
	file := repoSkill(t, "agent-ios")
	_, want, ok := strings.Cut(file, "\n---\n")
	want = strings.TrimLeft(want, "\n")
	if !ok || !strings.HasPrefix(file, "---\nname: agent-ios\n") || !strings.HasPrefix(want, "# ") {
		t.Fatalf("skill-data/agent-ios/SKILL.md has no front matter before its guide")
	}
	code, out, errOut, _ := runCLI("skills", "get", "agent-ios")
	if code != ExitOK || out != want || strings.Contains(out, "name: agent-ios\n") {
		t.Fatalf("skills get agent-ios: exit %d, stderr %q; stdout is not skill-data/agent-ios/SKILL.md without its front matter:\n%s", code, errOut, out)
	}
	// Several names print several whole skills in order; no name prints
	// every skill.
	code, out, _, _ = runCLI("skills", "get", "agent-ios", "agent-ios")
	if code != ExitOK || !strings.HasPrefix(out, want) || !strings.HasSuffix(out, want) || strings.Count(out, "# iOS Safari with agent-ios\n") != 2 {
		t.Fatalf("skills get agent-ios agent-ios: exit %d, stdout %q; want the skill twice", code, out)
	}
	for _, args := range [][]string{{"skills", "get"}, {"skills", "get", "--all"}} {
		code, out, _, _ = runCLI(args...)
		if code != ExitOK || strings.Contains(out, "---\nname: ") || strings.Count(out, "\n# ") < 3 || !strings.HasPrefix(out, want) {
			t.Fatalf("%v: exit %d, want every skill, got %q", args, code, out)
		}
	}
	code, out, _, _ = runCLI("--json", "skills", "get", "agent-ios")
	var got []skills.Skill
	if code != ExitOK || json.Unmarshal([]byte(out), &got) != nil || len(got) != 1 || got[0].Name != "agent-ios" || got[0].Content != file || got[0].Description == "" {
		t.Fatalf("--json skills get agent-ios: exit %d, stdout %q", code, out)
	}
	code, _, errOut, _ = runCLI("skills", "get", "nope")
	if code != ExitError || !strings.Contains(errOut, `unknown skill "nope"`) {
		t.Fatalf("skills get nope: exit %d, stderr %q; want exit 1 naming the skill", code, errOut)
	}
}

func TestSkillsListShowsNamesAndShortDescriptions(t *testing.T) {
	code, out, _, _ := runCLI("skills", "list")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// The core guide first, then the guides that it names.
	if code != ExitOK || len(lines) != 4 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "agent-ios ") {
		t.Fatalf("skills list: exit %d, stdout %q; want one line per skill, the core guide first", code, out)
	}
	for _, l := range lines {
		if n := len([]rune(l)); n > 2+len("agent-ios-devtools")+2+listDescriptionRunes+3 {
			t.Errorf("line of %d runes is longer than the cut description allows: %q", n, l)
		}
	}
	code, out, _, _ = runCLI("--json", "skills")
	var got []skills.Skill
	if code != ExitOK || json.Unmarshal([]byte(out), &got) != nil || len(got) != 4 || got[0].Content != "" || got[0].Description == "" {
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
	code, out, errOut, _ := runCLI("skills", "path", "agent-ios")
	dir := strings.TrimSpace(out)
	if code != ExitOK || !strings.HasPrefix(dir, filepath.Join(cache, "agent-ios", "skills")+string(filepath.Separator)) || filepath.Base(dir) != "agent-ios" {
		t.Fatalf("skills path agent-ios: exit %d, stdout %q, stderr %q; want <cache>/agent-ios/skills/<version>/agent-ios", code, out, errOut)
	}
	if got, err := os.ReadFile(filepath.Join(dir, skills.FileName)); err != nil || string(got) != repoSkill(t, "agent-ios") {
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
	cause := errors.New("reading the embedded skill agent-ios/SKILL.md: broken")
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
		code, out, errOut, _ := runCLI("skills", "path", "agent-ios")
		if code != ExitError || out != "" || !strings.Contains(errOut, "installing skill") || !strings.Contains(errOut, cache) {
			t.Fatalf("skills path with a file as cache: exit %d, stdout %q, stderr %q; want exit 1 naming the install and the path", code, out, errOut)
		}
	})
}
