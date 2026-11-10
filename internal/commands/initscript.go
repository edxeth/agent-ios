package commands

import (
	"context"
	"fmt"
	"os"
)

// addinitscript and removeinitscript: user scripts that run in every new
// document of the page as bootstrap parts (design
// docs/design/agentic-parity.md, sections 2.7 and 4.2). "Before page
// scripts" is partial while the daemon does not pause new targets on
// start (design 2.7 item 3).
func init() {
	Register(&Command{
		Name:    "addinitscript",
		Summary: "Run a script in every new document of the page, before page scripts when the page is paused on start (partial otherwise). Prints the script id.",
		Args:    []Arg{{Name: "script", Help: "JavaScript source (or use --file)"}},
		Flags: []Flag{
			{Name: "file", Type: FlagPath, Help: "read the script from this file"},
		},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runAddInitScript,
	})
	Register(&Command{
		Name:    "removeinitscript",
		Summary: "Remove an init script of this session by the id that addinitscript printed.",
		Args:    []Arg{{Name: "id", Help: "script id, for example init1", Required: true}},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run: func(ctx context.Context, env Env, in Input) (Result, error) {
			d, err := devtoolsForCommand(ctx, env)
			if err != nil {
				return Result{}, err
			}
			if err := d.p.RemoveInitScript(ctx, d.c, d.session, arg(in, 0)); err != nil {
				return Result{}, err
			}
			return Result{Text: "Removed init script " + arg(in, 0) + "."}, nil
		},
	})
}

func runAddInitScript(ctx context.Context, env Env, in Input) (Result, error) {
	src, file := arg(in, 0), flagString(in, "file")
	switch {
	case src != "" && file != "":
		return Result{}, usagef("addinitscript: give the script or --file, not both")
	case file != "":
		data, err := os.ReadFile(env.ResolvePath(file))
		if err != nil {
			return Result{}, fmt.Errorf("addinitscript: %w", err)
		}
		src = string(data)
	case src == "":
		return Result{}, usagef("addinitscript: give the script or --file")
	}
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	id, err := d.p.AddInitScript(ctx, d.c, d.session, src)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: id, Data: map[string]string{"id": id}}, nil
}
