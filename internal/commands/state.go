package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/edxeth/agent-ios/internal/devtools"
)

// state save and state load (design docs/design/agentic-parity.md,
// section 4.2): the cookies and storage of the page's current origin as a
// JSON file (devtools.State). The file holds live credentials: it is
// written with mode 0600, and the output shows only counts.
func init() {
	Register(&Command{
		Name:    "state save",
		Summary: "Save the cookies, localStorage and sessionStorage of the page's current origin to a JSON file (mode 0600; it holds live credentials).",
		Args:    []Arg{{Name: "path", Help: "the state file to write", Required: true}},
		Page:    LeasedPage,
		Output:  OutputText,
		Run:     runStateSave,
	})
	Register(&Command{
		Name:    "state load",
		Summary: "Restore a state file written by state save: the page must be at the file's origin; its cookies and storage are replaced by the file's.",
		Args:    []Arg{{Name: "path", Help: "the state file to read", Required: true}},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runStateLoad,
	})
}

// maxStateFile bounds a state file.
const maxStateFile = 64 << 20

func runStateSave(ctx context.Context, env Env, in Input) (Result, error) {
	path := env.ResolvePath(arg(in, 0))
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
	if err := devtools.WritePrivateFile(path, data); err != nil {
		return Result{}, err
	}
	o := st.Origins[0]
	return Result{
		Text: fmt.Sprintf("Saved the state of %s to %s: %s, %s and %s (mode 0600; the file holds live credentials, delete it after use).",
			o.Origin, path, nounCount(len(st.Cookies), "cookie"), nounCount(len(o.LocalStorage), "localStorage item"), nounCount(len(o.SessionStorage), "sessionStorage item")),
		Data: map[string]any{"path": path, "origin": o.Origin, "cookies": len(st.Cookies), "localStorage": len(o.LocalStorage), "sessionStorage": len(o.SessionStorage)},
	}, nil
}

func runStateLoad(ctx context.Context, env Env, in Input) (Result, error) {
	path := env.ResolvePath(arg(in, 0))
	f, err := os.Open(path)
	if err != nil {
		return Result{}, fmt.Errorf("reading the state file: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxStateFile+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return Result{}, fmt.Errorf("reading the state file %s: %w", path, err)
	}
	if len(data) > maxStateFile {
		return Result{}, fmt.Errorf("the state file %s is larger than %d bytes", path, maxStateFile)
	}
	st, err := devtools.DecodeState(data)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", path, err)
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
