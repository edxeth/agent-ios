package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/edxeth/agent-ios/internal/devtools"
)

// The state group (design docs/design/agentic-parity.md, section 4.2;
// agent-browser 0.45 "state"): the cookies and storage of the page's
// current origin as a JSON file (devtools.State). The file holds live
// credentials: it is written with mode 0600, and the output shows only
// counts.
//
// A state is named or a path. A NAME is letters, digits, '_' and '-' only
// (the file name without ".json", as state list shows it); it is the file
// <name>.json in the state directory, $XDG_STATE_HOME/agent-ios/states
// (else ~/.local/state/agent-ios/states), mode 0700. Any other argument,
// such as ./login, login.json or /tmp/x.json, is a path relative to the
// caller's directory. state save and state load take a name or a path,
// state show too; state rename and state clear take names only and touch
// only the state directory.
func init() {
	nameOrPath := "a state name (letters, digits, _ and -: the file <name>.json in the state directory) or a file path (anything else, such as ./login or login.json)"
	Register(&Command{
		Name:    "state save",
		Summary: "Save the cookies, localStorage and sessionStorage of the page's current origin as a named state or to a JSON file (mode 0600; it holds live credentials).",
		Args:    []Arg{{Name: "name-or-path", Help: nameOrPath + "; an existing file is overwritten", Required: true}},
		Page:    LeasedPage,
		Output:  OutputText,
		Run:     runStateSave,
	})
	Register(&Command{
		Name:    "state load",
		Summary: "Restore a state written by state save (a name or a path): the page must be at the state's origin; its cookies and storage are replaced by the state's.",
		Args:    []Arg{{Name: "name-or-path", Help: nameOrPath, Required: true}},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runStateLoad,
	})
	Register(&Command{
		Name:    "state list",
		Summary: "List the named states in the state directory ($XDG_STATE_HOME/agent-ios/states): file, size and date.",
		Output:  OutputText,
		Run:     runStateList,
	})
	Register(&Command{
		Name:    "state show",
		Summary: "Show what a state holds without its values: the number of cookies and origins, size and date.",
		Args:    []Arg{{Name: "name-or-path", Help: nameOrPath, Required: true}},
		Output:  OutputText,
		Run:     runStateShow,
	})
	Register(&Command{
		Name:    "state rename",
		Summary: "Rename a named state in the state directory; refuses to overwrite an existing state.",
		Args: []Arg{
			{Name: "old-name", Help: "the state to rename (a name; a trailing .json is ignored)", Required: true},
			{Name: "new-name", Help: "its new name (letters, digits, _ and -; a trailing .json is ignored)", Required: true},
		},
		Output: OutputText,
		Run:    runStateRename,
	})
	Register(&Command{
		Name: "state clear",
		Summary: "Delete the named state <name>, or with --all every named state (<name>.json) in the state directory. " +
			"Nothing outside that directory is deleted: no path, no other file, no subdirectory, no symlink target.",
		Args:   []Arg{{Name: "name", Help: "the state to delete (a name; a trailing .json is ignored); or --all"}},
		Flags:  []Flag{{Name: "all", Short: "a", Type: FlagBool, Help: "delete every named state in the state directory"}},
		Output: OutputText,
		Check: func(in Input) error {
			if arg(in, 0) != "" && flagBool(in, "all") {
				return usagef("give a state name or --all, not both")
			}
			return nil
		},
		Run: runStateClear,
	})
}

// maxStateFile bounds a state file.
const maxStateFile = 64 << 20

// stateNameRE is a state name: the file name of a named state without
// ".json".
var stateNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// stateDir is the directory of named states: $XDG_STATE_HOME/agent-ios/states,
// else ~/.local/state/agent-ios/states (the directory of the daemon log,
// internal/daemon/paths.go).
func stateDir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "agent-ios", "states"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("the state directory: no XDG_STATE_HOME and no home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "agent-ios", "states"), nil
}

// makeStateDir creates the state directory with mode 0700, or narrows
// the mode of an existing one to 0700. A symlink or a non-directory in
// its place is refused.
func makeStateDir() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating the state directory: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("the state directory: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("the state directory %s is not a directory (a symlink is refused)", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", fmt.Errorf("the state directory: %w", err)
		}
	}
	return dir, nil
}

// statePath returns the file of a name-or-path argument and whether it
// is a named state.
func statePath(env Env, s string) (string, bool, error) {
	if !stateNameRE.MatchString(s) {
		return env.ResolvePath(s), false, nil
	}
	dir, err := stateDir()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(dir, s+".json"), true, nil
}

// stateName checks a name argument of state rename and state clear; a
// trailing ".json" is dropped.
func stateName(cmd, s string) (string, error) {
	n := strings.TrimSuffix(s, ".json")
	if !stateNameRE.MatchString(n) {
		return "", usagef("%s %q: a state name has 1 to 128 letters, digits, _ and - (state list shows the names as <name>.json)", cmd, s)
	}
	return n, nil
}

func runStateSave(ctx context.Context, env Env, in Input) (Result, error) {
	path, named, err := statePath(env, arg(in, 0))
	if err != nil {
		return Result{}, err
	}
	_, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return Result{}, err
	}
	st, err := devtools.SaveState(ctx, c)
	if err != nil {
		return Result{}, err
	}
	data, err := devtools.EncodeState(st)
	if err != nil {
		return Result{}, err
	}
	if named {
		if _, err := makeStateDir(); err != nil {
			return Result{}, err
		}
	}
	if err := devtools.WritePrivateFile(path, data); err != nil {
		return Result{}, err
	}
	o := st.Origins[0]
	return Result{
		Text: fmt.Sprintf("Saved the state of %s to %s: %s, %s and %s (mode 0600; the file holds live credentials, delete it after use).",
			o.Origin, path, nounCount(len(st.Cookies), "cookie"), nounCount(len(o.LocalStorage), "localStorage item"), nounCount(len(o.SessionStorage), "sessionStorage item")),
		Data: map[string]any{"saved": true, "path": path, "origin": o.Origin, "cookies": len(st.Cookies), "localStorage": len(o.LocalStorage), "sessionStorage": len(o.SessionStorage)},
	}, nil
}

// readStateFile reads and decodes a state file of at most maxStateFile
// bytes.
func readStateFile(path string) (devtools.State, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return devtools.State{}, nil, fmt.Errorf("reading the state file: %w", err)
	}
	fi, err := f.Stat()
	var data []byte
	if err == nil {
		data, err = io.ReadAll(io.LimitReader(f, maxStateFile+1))
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return devtools.State{}, nil, fmt.Errorf("reading the state file %s: %w", path, err)
	}
	if len(data) > maxStateFile {
		return devtools.State{}, nil, fmt.Errorf("the state file %s is larger than %d bytes", path, maxStateFile)
	}
	st, err := devtools.DecodeState(data)
	if err != nil {
		return devtools.State{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	return st, fi, nil
}

func runStateLoad(ctx context.Context, env Env, in Input) (Result, error) {
	path, _, err := statePath(env, arg(in, 0))
	if err != nil {
		return Result{}, err
	}
	st, _, err := readStateFile(path)
	if err != nil {
		return Result{}, err
	}
	_, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return Result{}, err
	}
	res, err := devtools.LoadState(ctx, c, st)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Text: fmt.Sprintf("Loaded the state of %s from %s: %s set (%d removed), %s and %s.", res.Origin, path,
			nounCount(res.CookiesSet, "cookie"), res.CookiesRemoved, nounCount(res.LocalStorage, "localStorage item"), nounCount(res.SessionStorage, "sessionStorage item")),
		Data: res,
	}, nil
}

// stateFileInfo is one file of state list (agent-browser's JSON members;
// agent-ios never encrypts a state file).
type stateFileInfo struct {
	Filename  string `json:"filename"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Modified  int64  `json:"modified"` // Unix seconds
	Encrypted bool   `json:"encrypted"`
}

func fileInfoOf(path string, fi os.FileInfo) stateFileInfo {
	return stateFileInfo{Filename: filepath.Base(path), Path: path, Size: fi.Size(), Modified: fi.ModTime().Unix()}
}

// namedStates returns the named states in dir: the regular files
// <name>.json, by name. A missing directory has none.
func namedStates(dir string) ([]stateFileInfo, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the state directory: %w", err)
	}
	var out []stateFileInfo
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !stateNameRE.MatchString(name) || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted meanwhile
		}
		if err != nil {
			return nil, fmt.Errorf("reading the state directory: %w", err)
		}
		out = append(out, fileInfoOf(filepath.Join(dir, e.Name()), fi))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out, nil
}

func runStateList(context.Context, Env, Input) (Result, error) {
	dir, err := stateDir()
	if err != nil {
		return Result{}, err
	}
	files, err := namedStates(dir)
	if err != nil {
		return Result{}, err
	}
	if files == nil {
		files = []stateFileInfo{}
	}
	return Result{Text: stateListText(dir, files), Data: map[string]any{"files": files, "directory": dir}}, nil
}

// stateListText is the text of state list (agent-browser's form).
func stateListText(dir string, files []stateFileInfo) string {
	var b strings.Builder
	b.WriteString("Saved states in " + dir)
	if len(files) == 0 {
		b.WriteString("\n  No state files found")
	}
	for _, f := range files {
		fmt.Fprintf(&b, "\n  %s (%s, %s)", f.Filename, stateSize(f.Size), stateDate(f.Modified))
	}
	return b.String()
}

// stateSize is "<n>B", or "<x.y>KB" above 1024 bytes.
func stateSize(n int64) string {
	if n > 1024 {
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%dB", n)
}

// stateDate is the UTC date YYYY-MM-DD of Unix seconds.
func stateDate(sec int64) string { return time.Unix(sec, 0).UTC().Format("2006-01-02") }

func runStateShow(_ context.Context, env Env, in Input) (Result, error) {
	path, _, err := statePath(env, arg(in, 0))
	if err != nil {
		return Result{}, err
	}
	st, fi, err := readStateFile(path)
	if err != nil {
		return Result{}, err
	}
	info := fileInfoOf(path, fi)
	cookies, origins := len(st.Cookies), len(st.Origins)
	data := map[string]any{
		"filename": info.Filename, "path": info.Path, "size": info.Size, "modified": info.Modified, "encrypted": false,
		"summary":     fmt.Sprintf("%d cookies, %d origins", cookies, origins),
		"cookieCount": cookies, "originCount": origins,
	}
	text := fmt.Sprintf("State file summary:\n  Cookies: %d\n  Origins: %d", cookies, origins)
	return Result{Text: text, Data: data}, nil
}

func runStateRename(_ context.Context, _ Env, in Input) (Result, error) {
	from, err := stateName("state rename", arg(in, 0))
	if err != nil {
		return Result{}, err
	}
	to, err := stateName("state rename", arg(in, 1))
	if err != nil {
		return Result{}, err
	}
	dir, err := stateDir()
	if err != nil {
		return Result{}, err
	}
	if err := checkStateDir(dir); err != nil {
		return Result{}, err
	}
	src, dst := filepath.Join(dir, from+".json"), filepath.Join(dir, to+".json")
	fi, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return Result{}, fmt.Errorf("state %s does not exist in %s; state list shows the names", from, dir)
	}
	if err != nil {
		return Result{}, fmt.Errorf("state rename: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return Result{}, fmt.Errorf("state rename: %s is not a regular file", src)
	}
	if from == to {
		return Result{}, usagef("state rename: the old and the new name are both %q", from)
	}
	// A hard link fails when dst exists, so an existing state is never
	// overwritten, also by a concurrent save; then the old name goes.
	if err := os.Link(src, dst); errors.Is(err, fs.ErrExist) {
		return Result{}, fmt.Errorf("state %s exists already; delete it first (state clear %s) or choose another name", to, to)
	} else if err != nil {
		return Result{}, fmt.Errorf("state rename: %w", err)
	}
	if err := os.Remove(src); err != nil {
		return Result{}, fmt.Errorf("state rename: %s is now also %s, but removing the old name failed: %w", from, to, err)
	}
	return Result{Text: fmt.Sprintf("Renamed %s -> %s", from, to), Data: map[string]any{"renamed": true, "from": from, "to": to}}, nil
}

// checkStateDir refuses a state directory that is a symlink or not a
// directory, so that state clear and state rename never act on files
// elsewhere. A missing directory passes: it holds no state.
func checkStateDir(dir string) error {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("the state directory: %w", err)
	case !fi.IsDir():
		return fmt.Errorf("the state directory %s is not a directory (a symlink is refused); nothing was changed", dir)
	}
	return nil
}

func runStateClear(_ context.Context, _ Env, in Input) (Result, error) {
	if arg(in, 0) == "" && !flagBool(in, "all") {
		return Result{}, usagef("state clear needs a state name or --all")
	}
	dir, err := stateDir()
	if err != nil {
		return Result{}, err
	}
	if err := checkStateDir(dir); err != nil {
		return Result{}, err
	}
	var paths []string
	if flagBool(in, "all") {
		files, err := namedStates(dir)
		if err != nil {
			return Result{}, err
		}
		for _, f := range files {
			paths = append(paths, f.Path)
		}
	} else {
		name, err := stateName("state clear", arg(in, 0))
		if err != nil {
			return Result{}, err
		}
		p := filepath.Join(dir, name+".json")
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return Result{}, fmt.Errorf("state %s does not exist in %s; state list shows the names", name, dir)
		}
		if err != nil {
			return Result{}, fmt.Errorf("state clear: %w", err)
		}
		if !fi.Mode().IsRegular() {
			return Result{}, fmt.Errorf("state clear: %s is not a regular file; it is left alone", p)
		}
		paths = append(paths, p)
	}
	deleted := 0
	for _, p := range paths {
		if err := os.Remove(p); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return Result{}, fmt.Errorf("state clear: deleted %d, then: %w", deleted, err)
		}
		deleted++
	}
	return Result{Text: fmt.Sprintf("Cleared %s in %s", nounCount(deleted, "state file"), dir), Data: map[string]any{"deleted": deleted}}, nil
}
