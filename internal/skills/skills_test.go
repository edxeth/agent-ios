package skills

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"testing/quick"
)

// repoSkills reads skill-data/<name>/SKILL.md from the repository
// checkout: the guides that the binary must embed (design 7.9 "embedded
// skills").
func repoSkills(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "skill-data", "*", FileName))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no skill-data/*/%s in the repository: %v", FileName, err)
	}
	out := map[string]string{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(filepath.Dir(p))] = string(data)
	}
	return out
}

// Every repository skill is embedded with its exact content, and nothing
// else is: "skills get" prints what the repository ships.
func TestEmbeddedSkillsAreTheRepositorySkills(t *testing.T) {
	want := repoSkills(t)
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range list {
		names = append(names, s.Name)
		if s.Content != "" {
			t.Errorf("List gave the content of %s; it lists names and descriptions only", s.Name)
		}
		if s.Description == "" {
			t.Errorf("skill %s has no description", s.Name)
		}
	}
	wantNames := make([]string, 0, len(want))
	for n := range want {
		wantNames = append(wantNames, n)
	}
	slices.Sort(wantNames)
	if !slices.Equal(names, wantNames) {
		t.Fatalf("embedded skills %v, repository skills %v", names, wantNames)
	}
	for n, content := range want {
		s, err := Get(n)
		if err != nil {
			t.Fatal(err)
		}
		if s.Content != content {
			t.Errorf("embedded %s differs from skill-data/%s/%s", n, n, FileName)
		}
	}
}

// maxStubWords bounds a discovery stub. Its model, agent-browser's stub,
// has about 450 words with a list of eight guides; an iwdp-cli stub points
// at one guide, so 350 words hold every required point, while a copied
// guide section (the safety rules alone have more than 900 words) does not
// fit. Guide text in a stub would drift from the binary's guide.
const maxStubWords = 350

// fenceRE matches the opening or closing line of a Markdown fenced code
// block in either form (CommonMark: up to three spaces, then at least
// three backticks or tildes).
var fenceRE = regexp.MustCompile("^ {0,3}(```|~~~)")

// Design 7.9 (skill layout, agent-browser's): skills/<name>/SKILL.md is
// the only file of its directory, a discovery stub that agents find; the
// guide is skill-data/<name>/SKILL.md, which the binary embeds. A stub
// triggers like its guide (same name and description), names the command
// that prints the guide, and has no code block: the guide's examples run
// in the WebKitGTK doc test, a stub's would not.
func TestSkillStubsPointAtTheirGuides(t *testing.T) {
	root := filepath.Join("..", "..")
	stubs := 0
	err := filepath.WalkDir(filepath.Join(root, "skills"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(filepath.Join(root, "skills"), p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		switch {
		case rel == ".":
			return nil
		case len(parts) == 1 && d.IsDir():
			return nil
		case len(parts) != 2 || d.IsDir() || parts[1] != FileName:
			// Pi finds SKILL.md files below a skill directory too, and a
			// nested one would collide with the stub.
			t.Errorf("skills/%s: skills/ holds only <name>/%s stubs", filepath.ToSlash(rel), FileName)
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		stubs++
		name := parts[0]
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		stub, err := parse(name, string(data))
		if err != nil {
			t.Errorf("stub: %v", err)
			return nil
		}
		guideData, err := os.ReadFile(filepath.Join(root, "skill-data", name, FileName))
		if err != nil {
			t.Errorf("stub skills/%s/%s has no guide skill-data/%s/%s: %v", name, FileName, name, FileName, err)
			return nil
		}
		guide, err := parse(name, string(guideData))
		if err != nil {
			t.Errorf("guide: %v", err)
			return nil
		}
		if stub.Description != guide.Description {
			t.Errorf("skills/%s/%s: description %q differs from the guide's %q", name, FileName, stub.Description, guide.Description)
		}
		if want := "iwdp-cli skills get " + name; !strings.Contains(stub.Content, want) {
			t.Errorf("skills/%s/%s does not name %q", name, FileName, want)
		}
		for i, line := range strings.Split(stub.Content, "\n") {
			if fenceRE.MatchString(line) {
				t.Errorf("skills/%s/%s:%d: a fenced code block; examples belong in the guide", name, FileName, i+1)
			}
		}
		if n := len(strings.Fields(stub.Content)); n > maxStubWords {
			t.Errorf("skills/%s/%s has %d words, more than %d: a stub points at the guide and does not copy it", name, FileName, n, maxStubWords)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stubs == 0 {
		t.Fatalf("no skills/*/%s stub in the repository", FileName)
	}
}

// readRepo reads a file of the repository checkout by its slash path.
func readRepo(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The iwdp stub states each instruction that design 7.9 (skill layout)
// and the WS6 brief require of it, in this wording. The owner dropped the
// install hint from the stub (2026-11-24): this local fork is installed
// from its checkout.
func TestIwdpStubStatesTheRequiredInstructions(t *testing.T) {
	stub := readRepo(t, "skills/iwdp/SKILL.md")
	for _, c := range []struct {
		what string
		want []string
	}{
		{"it is a discovery stub, not the guide", []string{"This file is a discovery stub, not the guide."}},
		{"load the guide before any other iwdp-cli command", []string{"Before you run any other `iwdp-cli` command, load the guide that matches the installed binary", "Run `iwdp-cli skills get iwdp`."}},
		{"read the guide by line range", []string{"read it by line range"}},
		{"--help prints one command's flags and never connects", []string{"`iwdp-cli <command> --help` prints the flags of one command and never connects to a page."}},
		{"what iwdp-cli is", []string{"Safari speaks the WebKit Inspector Protocol", "CDP clients such as agent-browser", "`iwdp-cli get cdp-url`"}},
		{"why it only points at the CLI", []string{"The CLI serves the guide of its own version.", "so it only points at the CLI"}},
	} {
		for _, w := range c.want {
			if !strings.Contains(stub, w) {
				t.Errorf("skills/iwdp/SKILL.md does not state %s: no %q", c.what, w)
			}
		}
	}
}

func TestGetUnknownSkillNamesTheKnownOnes(t *testing.T) {
	_, err := Get("no-such-skill")
	if err == nil || !strings.Contains(err.Error(), `unknown skill "no-such-skill"`) || !strings.Contains(err.Error(), "iwdp") {
		t.Fatalf("Get(no-such-skill) = %v, want an error that names the known skills", err)
	}
}

func withSource(t *testing.T, m fstest.MapFS) {
	t.Helper()
	old := source
	source = m
	t.Cleanup(func() { source = old })
}

func skillFile(name, desc, body string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body)}
}

// A malformed skill file is an error that names the skill and the defect,
// never a skill with an empty name or description.
func TestParseRejectsBadFrontMatter(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"no start", "name: a\n---\nbody", "does not start with a front matter line"},
		{"no end", "---\nname: a\ndescription: d\nbody", "has no closing line"},
		{"other name", "---\nname: b\ndescription: d\n---\n", `front matter name "b" differs`},
		{"no description", "---\nname: a\ndescription:   \n---\n", "has no description"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withSource(t, fstest.MapFS{"a/SKILL.md": {Data: []byte(c.content)}})
			if _, err := List(); err == nil || !strings.Contains(err.Error(), "skill a") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("List() error = %v, want one about skill a that says %q", err, c.want)
			}
		})
	}
	withSource(t, fstest.MapFS{})
	if _, err := List(); err == nil || !strings.Contains(err.Error(), "no skills are embedded") {
		t.Fatalf("List() with no skills = %v", err)
	}
}

// Property (parsing): for any skill name of word characters and any
// one-line description with text, parse returns that name and the trimmed
// description, whatever the body holds, colons in the description
// included.
func TestParseFrontMatterProperty(t *testing.T) {
	const nameChars = "abcdefghijklmnopqrstuvwxyz0123456789-"
	f := func(rawName []byte, desc, body string) bool {
		var b strings.Builder
		for _, c := range rawName {
			b.WriteByte(nameChars[int(c)%len(nameChars)])
		}
		name := "s" + b.String()
		desc = strings.NewReplacer("\n", " ", "\r", " ").Replace(desc) + ": x"
		s, err := parse(name, "---\nname: "+name+"\ndescription: "+desc+"\n---\n"+body)
		return err == nil && s.Name == name && s.Description == strings.TrimSpace(desc)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

// Install writes each skill once, privately, below a directory named by
// the skills version; a damaged file is replaced; another skills version
// gets another directory.
func TestInstallWritesPrivateFilesPerVersion(t *testing.T) {
	withSource(t, fstest.MapFS{
		"one/SKILL.md": skillFile("one", "first", "body one\n"),
		"two/SKILL.md": skillFile("two", "second", "body two\n"),
	})
	root := filepath.Join(t.TempDir(), "cache")
	dir, err := Install(root)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := Version()
	if dir != filepath.Join(root, v) || len(v) != versionHexDigits {
		t.Fatalf("Install dir %s, version %q; want <root>/<12 hex digits>", dir, v)
	}
	for _, n := range []string{"one", "two"} {
		p := filepath.Join(dir, n, FileName)
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", p, st.Mode().Perm())
		}
		dst, _ := os.Stat(filepath.Dir(p))
		if dst.Mode().Perm() != 0o700 {
			t.Errorf("%s has mode %v, want 0700", filepath.Dir(p), dst.Mode().Perm())
		}
		got, _ := os.ReadFile(p)
		s, _ := Get(n)
		if string(got) != s.Content {
			t.Errorf("%s holds %q, want the skill content", p, got)
		}
	}
	// A damaged file is written again; an intact one is left alone.
	damaged := filepath.Join(dir, "one", FileName)
	if err := os.WriteFile(damaged, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	intact := filepath.Join(dir, "two", FileName)
	before, _ := os.Stat(intact)
	if again, err := Install(root); err != nil || again != dir {
		t.Fatalf("second Install = %s, %v; want %s", again, err, dir)
	}
	if got, _ := os.ReadFile(damaged); !strings.Contains(string(got), "body one") {
		t.Fatalf("damaged file holds %q after Install, want the skill", got)
	}
	if after, _ := os.Stat(intact); !os.SameFile(before, after) {
		t.Error("Install rewrote a file whose content was right")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*", ".skill-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	// Changed content: a new version directory, the old one untouched.
	withSource(t, fstest.MapFS{
		"one/SKILL.md": skillFile("one", "first", "body one, changed\n"),
		"two/SKILL.md": skillFile("two", "second", "body two\n"),
	})
	next, err := Install(root)
	if err != nil || next == dir {
		t.Fatalf("Install after a change = %s, %v; want a new directory beside %s", next, err, dir)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "one", FileName)); !strings.Contains(string(got), "body one\n") {
		t.Errorf("the old version's file changed to %q", got)
	}
}

// The version covers names and contents: moving text from one skill to
// another changes it.
func TestVersionCoversNamesAndContents(t *testing.T) {
	withSource(t, fstest.MapFS{"ab/SKILL.md": skillFile("ab", "d", "c")})
	v1, err := Version()
	if err != nil {
		t.Fatal(err)
	}
	withSource(t, fstest.MapFS{"ab/SKILL.md": skillFile("ab", "d", "cc")})
	if v2, _ := Version(); v2 == v1 {
		t.Fatalf("Version %s did not change with the content", v1)
	}
}

// Every function that reads the skills reports a broken source; Install
// then writes nothing.
func TestBrokenSourceFailsEveryReader(t *testing.T) {
	withSource(t, fstest.MapFS{})
	const want = "no skills are embedded"
	if _, err := Get("one"); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Get = %v, want %q", err, want)
	}
	if _, err := All(); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("All = %v, want %q", err, want)
	}
	if _, err := Version(); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Version = %v, want %q", err, want)
	}
	root := filepath.Join(t.TempDir(), "cache")
	if dir, err := Install(root); err == nil || dir != "" || !strings.Contains(err.Error(), want) {
		t.Errorf("Install = %q, %v; want no directory and %q", dir, err, want)
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Install with no skills made %s (stat: %v)", root, err)
	}
}

// faultFS is a skill source whose Glob or ReadFile fails.
type faultFS struct {
	fstest.MapFS
	globErr, readErr error
}

func (f faultFS) Glob(pattern string) ([]string, error) {
	if f.globErr != nil {
		return nil, f.globErr
	}
	return f.MapFS.Glob(pattern)
}

func (f faultFS) ReadFile(name string) ([]byte, error) {
	if f.readErr != nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: f.readErr}
	}
	return f.MapFS.ReadFile(name)
}

// A source that cannot list or read its files gives an error that says
// which step failed and keeps the cause.
func TestLoadNamesTheFailedStep(t *testing.T) {
	cause := errors.New("disk gone")
	files := fstest.MapFS{"one/SKILL.md": skillFile("one", "first", "body\n")}
	for _, c := range []struct {
		name string
		src  faultFS
		want string
	}{
		{"glob", faultFS{MapFS: files, globErr: cause}, "listing the embedded skills"},
		{"read", faultFS{MapFS: files, readErr: cause}, "reading the embedded skill one/SKILL.md"},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := source
			source = c.src
			t.Cleanup(func() { source = old })
			_, err := List()
			if !errors.Is(err, cause) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("List() = %v, want %q wrapping %q", err, c.want, cause)
			}
		})
	}
}

// faultyTemp is a real temporary file whose Write or Close fails.
type faultyTemp struct {
	*os.File
	writeErr, closeErr error
}

func (f faultyTemp) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.File.Write(p)
}

func (f faultyTemp) Close() error {
	err := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

func withCreateTemp(t *testing.T, fn func(dir, pattern string) (tempFile, error)) {
	t.Helper()
	old := createTemp
	createTemp = fn
	t.Cleanup(func() { createTemp = old })
}

// A failed write step makes Install fail with the skill name and the
// cause, keeps the old file, and leaves no temporary file: a reader never
// sees a partial skill.
func TestInstallWriteFailuresKeepTheOldFile(t *testing.T) {
	withSource(t, fstest.MapFS{"one/SKILL.md": skillFile("one", "first", "body one\n")})
	v, err := Version()
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("device full")
	for _, c := range []struct {
		name string
		fn   func(dir, pattern string) (tempFile, error)
	}{
		{"create", func(string, string) (tempFile, error) { return nil, cause }},
		{"write", func(dir, pattern string) (tempFile, error) {
			f, err := os.CreateTemp(dir, pattern)
			return faultyTemp{File: f, writeErr: cause}, err
		}},
		{"close", func(dir, pattern string) (tempFile, error) {
			f, err := os.CreateTemp(dir, pattern)
			return faultyTemp{File: f, closeErr: cause}, err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			old := filepath.Join(root, v, "one", FileName)
			if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			withCreateTemp(t, c.fn)
			dir, err := Install(root)
			if dir != "" || !errors.Is(err, cause) || !strings.Contains(err.Error(), "installing skill one") {
				t.Fatalf("Install = %q, %v; want no directory and an error about skill one that wraps %q", dir, err, cause)
			}
			if got, _ := os.ReadFile(old); string(got) != "old" {
				t.Errorf("the old file holds %q after a failed Install, want it unchanged", got)
			}
			if left, _ := filepath.Glob(filepath.Join(root, v, "one", ".skill-*")); len(left) != 0 {
				t.Errorf("temporary files left: %v", left)
			}
		})
	}
}

// The file system refuses the install: a file where the cache directory
// should be, or a directory where SKILL.md should be. Install fails with
// the skill name and the system error and leaves no temporary file.
func TestInstallReportsFileSystemRefusals(t *testing.T) {
	withSource(t, fstest.MapFS{"one/SKILL.md": skillFile("one", "first", "body one\n")})
	v, err := Version()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("root is a file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "cache")
		if err := os.WriteFile(root, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		dir, err := Install(root)
		if dir != "" || !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), "installing skill one") {
			t.Fatalf("Install under a file = %q, %v; want an error about skill one that wraps ENOTDIR", dir, err)
		}
	})
	t.Run("SKILL.md is a directory", func(t *testing.T) {
		root := t.TempDir()
		blocker := filepath.Join(root, v, "one", FileName, "keep")
		if err := os.MkdirAll(blocker, 0o700); err != nil {
			t.Fatal(err)
		}
		dir, err := Install(root)
		var linkErr *os.LinkError
		if dir != "" || !errors.As(err, &linkErr) || !strings.Contains(err.Error(), "installing skill one") {
			t.Fatalf("Install over a directory = %q, %v; want an error about skill one from the rename", dir, err)
		}
		if _, err := os.Stat(blocker); err != nil {
			t.Errorf("Install removed the directory in its way: %v", err)
		}
		if left, _ := filepath.Glob(filepath.Join(root, v, "one", ".skill-*")); len(left) != 0 {
			t.Errorf("temporary files left: %v", left)
		}
	})
}
