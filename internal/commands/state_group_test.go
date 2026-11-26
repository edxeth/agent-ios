package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/devtools"
	"github.com/edxeth/agent-ios/internal/devtools/devtoolstest"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// Tests of the state group (agent-browser 0.45 contract, section 7, its
// intended contract; WP-DIFFSTATE brief: named states in
// $XDG_STATE_HOME/agent-ios/states, directory 0700, files 0600).

// stateHome points XDG_STATE_HOME at a new directory and returns the state
// directory under it.
func stateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", home)
	return filepath.Join(home, "agent-ios", "states")
}

// writeState writes a state file with n cookies and one origin, and sets
// its modification time to the Unix second mod.
func writeState(t *testing.T, path string, n int, mod int64) {
	t.Helper()
	st := devtools.State{Version: devtools.StateVersion, Origins: []devtools.StateOrigin{{Origin: "https://app.test"}}}
	for i := 0; i < n; i++ {
		st.Cookies = append(st.Cookies, devtools.StateCookie{Name: "c" + string(rune('a'+i)), Value: "v", Domain: "app.test", Path: "/"})
	}
	data, err := devtools.EncodeState(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Unix(mod, 0), time.Unix(mod, 0)); err != nil {
		t.Fatal(err)
	}
}

func jsonOf(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not JSON: %q: %v", out, err)
	}
	return m
}

// A name is the file <name>.json in the state directory (created 0700, an
// existing one narrowed to 0700; the file 0600); anything else is a path
// relative to the caller.
func TestStateSaveLoadNameOrPath(t *testing.T) {
	dir := stateHome(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := devtoolstest.New(t, "https://app.test")
	p.Cookies = []webkit.Cookie{{Name: "sid", Value: "s3cret", Domain: "app.test", Path: "/", Session: true, SameSite: "Lax"}}
	ps := &core.PageSession{Caller: p.Conn}
	cwd := t.TempDir()
	code, out, errOut := spRun(ps, cwd, "--json", "state", "save", "login")
	named := filepath.Join(dir, "login.json")
	if code != 0 || jsonOf(t, out)["path"] != named || jsonOf(t, out)["saved"] != true {
		t.Fatalf("state save login: exit %d, %q, %q; want path %s", code, out, errOut, named)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, named: 0o600} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: mode %v, %v; want %v", path, fi.Mode().Perm(), err, want)
		}
	}
	// ./login and login.json are paths in the caller's directory.
	for _, arg := range []string{"./login", "login.json"} {
		if code, _, errOut := spRun(ps, cwd, "state", "save", arg); code != 0 {
			t.Fatalf("state save %s: exit %d, %q", arg, code, errOut)
		}
		if _, err := os.Stat(filepath.Join(cwd, arg)); err != nil {
			t.Fatalf("state save %s did not write it in the caller's directory: %v", arg, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "login.json.json")); !os.IsNotExist(err) {
		t.Fatalf("login.json was taken for a name: %v", err)
	}
	p.Mu.Lock()
	p.Cookies = nil
	p.Mu.Unlock()
	code, out, errOut = spRun(ps, cwd, "state", "load", "login")
	if code != 0 || !strings.Contains(out, "from "+named+": 1 cookie set") {
		t.Fatalf("state load login: exit %d, %q, %q", code, out, errOut)
	}
	if code, _, errOut := spRun(ps, cwd, "state", "load", "nosuch"); code != ExitError || !strings.Contains(errOut, filepath.Join(dir, "nosuch.json")) {
		t.Fatalf("state load of a missing name: exit %d, %q", code, errOut)
	}
}

// state list: the named states only, agent-browser's text and JSON
// (filename, path, size, modified in Unix seconds, encrypted).
func TestStateList(t *testing.T) {
	dir := stateHome(t)
	code, out, errOut := spRun(nil, t.TempDir(), "state", "list")
	if want := "Saved states in " + dir + "\n  No state files found"; code != 0 || out != want {
		t.Fatalf("state list without a directory: exit %d, %q, %q; want %q", code, out, errOut, want)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeState(t, filepath.Join(dir, "demo-default.json"), 1, 1764000000)
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, make([]byte, 1536), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(big, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"notes.txt", "bad name.json", ".hidden.json"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(big, filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "demo-default.json"))
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut = spRun(nil, t.TempDir(), "state", "list")
	want := "Saved states in " + dir + "\n  big.json (1.5KB, 1970-01-01)\n  demo-default.json (" + stateSize(fi.Size()) + ", 2025-11-24)"
	if code != 0 || out != want {
		t.Fatalf("state list = exit %d,\n%s\n%s\nwant\n%s", code, out, errOut, want)
	}
	code, out, _ = spRun(nil, t.TempDir(), "--json", "state", "list")
	m := jsonOf(t, out)
	files, _ := m["files"].([]any)
	if code != 0 || m["directory"] != dir || len(files) != 2 {
		t.Fatalf("state list --json = %v", m)
	}
	f := files[1].(map[string]any)
	if f["filename"] != "demo-default.json" || f["path"] != filepath.Join(dir, "demo-default.json") || f["size"] != float64(fi.Size()) ||
		f["modified"] != 1764000000.0 || f["encrypted"] != false {
		t.Fatalf("state list entry = %v", f)
	}
}

// stateSize and stateDate follow agent-browser's list text: "<n>B", one
// decimal "KB" above 1024 bytes, UTC date.
func TestStateListFormats(t *testing.T) {
	for n, want := range map[int64]string{0: "0B", 244: "244B", 1024: "1024B", 1025: "1.0KB", 1536: "1.5KB"} {
		if got := stateSize(n); got != want {
			t.Errorf("stateSize(%d) = %q, want %q", n, got, want)
		}
	}
	for sec, want := range map[int64]string{1764000000: "2025-11-24", 0: "1970-01-01"} {
		if got := stateDate(sec); got != want {
			t.Errorf("stateDate(%d) = %q, want %q", sec, got, want)
		}
	}
}

// state show: counts, never values; by name or by path.
func TestStateShow(t *testing.T) {
	dir := stateHome(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(dir, "two.json")
	writeState(t, named, 2, 1764000000)
	code, out, errOut := spRun(nil, t.TempDir(), "state", "show", "two")
	if want := "State file summary:\n  Cookies: 2\n  Origins: 1"; code != 0 || out != want {
		t.Fatalf("state show two: exit %d, %q, %q; want %q", code, out, errOut, want)
	}
	cwd := t.TempDir()
	writeState(t, filepath.Join(cwd, "one.json"), 1, 1764000000)
	code, out, _ = spRun(nil, cwd, "--json", "state", "show", "one.json")
	m := jsonOf(t, out)
	if code != 0 || m["filename"] != "one.json" || m["path"] != filepath.Join(cwd, "one.json") || m["summary"] != "1 cookies, 1 origins" ||
		m["cookieCount"] != 1.0 || m["originCount"] != 1.0 || m["modified"] != 1764000000.0 || m["encrypted"] != false {
		t.Fatalf("state show --json one.json = %v", m)
	}
	if strings.Contains(out, "\"v\"") || m["state"] != nil {
		t.Fatalf("state show printed values: %s", out)
	}
	if err := os.WriteFile(filepath.Join(cwd, "bad.json"), []byte(`{"x":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := spRun(nil, cwd, "state", "show", "bad.json"); code != ExitError || !strings.Contains(errOut, "not an agent-ios state file") {
		t.Fatalf("state show of another file: exit %d, %q", code, errOut)
	}
}

// state rename renames a named state in the state directory and never
// overwrites one.
func TestStateRename(t *testing.T) {
	dir := stateHome(t)
	if code, _, errOut := spRun(nil, t.TempDir(), "state", "rename", "a", "b"); code != ExitError || !strings.Contains(errOut, "state a does not exist") {
		t.Fatalf("rename without states: exit %d, %q", code, errOut)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeState(t, filepath.Join(dir, "a.json"), 1, 1)
	writeState(t, filepath.Join(dir, "b.json"), 2, 1)
	code, out, _ := spRun(nil, t.TempDir(), "--json", "state", "rename", "a.json", "c")
	if m := jsonOf(t, out); code != 0 || m["renamed"] != true || m["from"] != "a" || m["to"] != "c" {
		t.Fatalf("rename a.json c: exit %d, %v", code, m)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.json")); !os.IsNotExist(err) {
		t.Fatalf("a.json still exists: %v", err)
	}
	code, out, _ = spRun(nil, t.TempDir(), "state", "rename", "c", "d")
	if code != 0 || out != "Renamed c -> d" {
		t.Fatalf("rename c d: exit %d, %q", code, out)
	}
	code, _, errOut := spRun(nil, t.TempDir(), "state", "rename", "d", "b")
	if code != ExitError || !strings.Contains(errOut, "state b exists already") {
		t.Fatalf("rename onto b: exit %d, %q", code, errOut)
	}
	if st, _, err := readStateFile(filepath.Join(dir, "b.json")); err != nil || len(st.Cookies) != 2 {
		t.Fatalf("b.json after a refused rename: %v cookies, %v", len(st.Cookies), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d.json")); err != nil {
		t.Fatalf("d.json after a refused rename: %v", err)
	}
	for _, args := range [][]string{{"d", "../x"}, {"../d", "x"}, {"d", "x/y"}} {
		if code, _, errOut := spRun(nil, t.TempDir(), append([]string{"state", "rename"}, args...)...); code != ExitUsage {
			t.Errorf("rename %v: exit %d, %q; want a usage error", args, code, errOut)
		}
	}
}

// state clear deletes one named state, or with --all every named state,
// and nothing else: no other file, subdirectory or symlink target, and
// nothing through a symlinked state directory.
func TestStateClear(t *testing.T) {
	dir := stateHome(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "target.json")
	writeState(t, target, 1, 1)
	if err := os.MkdirAll(filepath.Join(dir, "sub.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c"} {
		writeState(t, filepath.Join(dir, n+".json"), 1, 1)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{}, {"a", "--all"}, {"../a"}} {
		if code, _, errOut := spRun(nil, t.TempDir(), append([]string{"state", "clear"}, args...)...); code != ExitUsage {
			t.Errorf("state clear %v: exit %d, %q; want a usage error", args, code, errOut)
		}
	}
	code, out, _ := spRun(nil, t.TempDir(), "--json", "state", "clear", "a.json")
	if m := jsonOf(t, out); code != 0 || m["deleted"] != 1.0 {
		t.Fatalf("state clear a.json: exit %d, %v", code, m)
	}
	if code, _, errOut := spRun(nil, t.TempDir(), "state", "clear", "a"); code != ExitError || !strings.Contains(errOut, "state a does not exist") {
		t.Fatalf("clear of a missing state: exit %d, %q", code, errOut)
	}
	if code, _, errOut := spRun(nil, t.TempDir(), "state", "clear", "link"); code != ExitError || !strings.Contains(errOut, "not a regular file") {
		t.Fatalf("clear of a symlink: exit %d, %q", code, errOut)
	}
	code, out, _ = spRun(nil, t.TempDir(), "state", "clear", "--all")
	if code != 0 || out != "Cleared 2 state files in "+dir {
		t.Fatalf("state clear --all: exit %d, %q", code, out)
	}
	for _, kept := range []string{filepath.Join(dir, "sub.json"), filepath.Join(dir, "notes.txt"), filepath.Join(dir, "link.json"), target} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("state clear --all removed %s: %v", kept, err)
		}
	}
	// A state directory that is a symlink is refused.
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "agent-ios"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "agent-ios", "states")); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := spRun(nil, t.TempDir(), "state", "clear", "--all"); code != ExitError || !strings.Contains(errOut, "a symlink is refused") {
		t.Fatalf("clear --all through a symlinked directory: exit %d, %q", code, errOut)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the file behind the symlinked directory was deleted: %v", err)
	}
}
