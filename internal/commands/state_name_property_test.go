package commands

import (
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// stateArg is a generated name-or-path argument of the state commands:
// names of 1 to 129 name characters, and texts that mix them with ".",
// ".json", "/", "..", white space, a newline and non-ASCII letters.
type stateArg string

func (stateArg) Generate(r *rand.Rand, _ int) reflect.Value {
	const nameChars = "abcXYZ019_-"
	one := func() string { return string(nameChars[r.Intn(len(nameChars))]) }
	var b strings.Builder
	switch r.Intn(5) {
	case 0: // a name, or one character too long, around the limit
		for range 126 + r.Intn(4) {
			b.WriteString(one())
		}
	case 1: // the file name of a named state
		for range 1 + r.Intn(8) {
			b.WriteString(one())
		}
		b.WriteString(".json")
	default:
		tokens := []string{".", ".json", "json", "/", "..", " ", "\n", "é", "日", "-", "_"}
		for range r.Intn(12) {
			if r.Intn(3) == 0 {
				b.WriteString(tokens[r.Intn(len(tokens))])
			} else {
				b.WriteString(one())
			}
		}
	}
	return reflect.ValueOf(stateArg(b.String()))
}

// validStateName is the documented rule (state.go, FIX-F F7): a state
// name is 1 to 128 letters, digits, '_' and '-'; so it never ends in
// ".json". Anything else is a path.
func validStateName(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	const allowed = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
	for _, c := range s {
		if !strings.ContainsRune(allowed, c) {
			return false
		}
	}
	return true
}

// fileSafe reports whether s can be one file name in a directory.
func fileSafe(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.Contains(s, "/") && len(s) <= 250
}

// Review2 F18 (FIX-F F7): every state command reads a generated argument
// by the documented rule (validStateName). state save, load and show
// (statePath; show run as a command) take a name as <name>.json in the
// state directory and anything else as a path relative to the caller,
// even when the other reading exists; state rename and state clear refuse
// a non-name with a usage error and look a name up in the state
// directory; state list lists exactly the files <name>.json.
func TestStateNameOrPathRuleProperty(t *testing.T) {
	dir := stateHome(t)
	work := t.TempDir()
	prop := func(args [3]stateArg) bool {
		ok := true
		fail := func(format string, a ...any) { t.Logf(format, a...); ok = false }
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		cwd, err := os.MkdirTemp(work, "cwd")
		if err != nil {
			t.Fatal(err)
		}
		var listed []string
		for _, a := range args {
			s := string(a)
			valid := validStateName(s)
			// save, load and show: statePath.
			path, named, err := statePath(spEnv{dir: cwd}, s)
			want := spEnv{dir: cwd}.ResolvePath(s)
			if valid {
				want = filepath.Join(dir, s+".json")
			}
			if err != nil || named != valid || path != want {
				fail("statePath(%q) = %q, %v, %v; want %q, named %v", s, path, named, err, want, valid)
			}
			// rename and clear: a name, or a usage error.
			for _, cmd := range [][]string{{"state", "clear", "--", s}, {"state", "rename", "--", s, "x"}, {"state", "rename", "--", "x", s}} {
				code, _, errOut := spRun(nil, cwd, cmd...)
				switch {
				case valid && (code != ExitError || !strings.Contains(errOut, " does not exist in "+dir)):
					fail("%q: exit %d, %q; want the name looked up in the state directory", cmd, code, errOut)
				case !valid && code != ExitUsage:
					fail("%q: exit %d, %q; want a usage error", cmd, code, errOut)
				}
			}
		}
		// show, after the commands above ran on an empty state directory.
		for _, a := range args {
			s := string(a)
			valid := validStateName(s)
			want := spEnv{dir: cwd}.ResolvePath(s)
			if valid {
				want = filepath.Join(dir, s+".json")
			}
			if !fileSafe(s) {
				continue
			}
			// show, with both readings present: the rule picks one.
			other := filepath.Join(dir, strings.TrimSuffix(s, ".json")+".json")
			if valid {
				other = filepath.Join(cwd, s)
			}
			for _, p := range []string{want, other} {
				writeState(t, p, 1, 1)
				if n, ok := strings.CutSuffix(filepath.Base(p), ".json"); ok && filepath.Dir(p) == dir && validStateName(n) {
					listed = append(listed, n)
				}
			}
			code, out, errOut := spRun(nil, cwd, "--json", "state", "show", "--", s)
			if code != 0 || jsonOf(t, out)["path"] != want {
				fail("state show %q: exit %d, %q, %q; want path %s", s, code, out, errOut, want)
			}
		}
		// list: exactly the names among the files written to the state
		// directory; a file <arg>.json whose arg is not a name is not
		// listed.
		for _, a := range args {
			if s := string(a); fileSafe(s+".json") && !validStateName(s) {
				writeState(t, filepath.Join(dir, s+".json"), 1, 1)
			}
		}
		code, out, errOut := spRun(nil, cwd, "--json", "state", "list")
		var got []string
		if code == 0 {
			for _, f := range jsonOf(t, out)["files"].([]any) {
				got = append(got, f.(map[string]any)["name"].(string))
			}
		}
		// state list sorts by file name.
		slices.SortFunc(listed, func(a, b string) int { return strings.Compare(a+".json", b+".json") })
		listed = slices.Compact(listed)
		if code != 0 || !slices.Equal(got, listed) {
			fail("state list: exit %d, %q, names %q; want %q", code, errOut, got, listed)
		}
		return ok
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}
