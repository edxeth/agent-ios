package commands

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
)

// profiler start and profiler stop (design docs/design/agentic-parity.md,
// section 4.2): ScriptProfiler sampling converted to a .cpuprofile (CDP
// Profiler.Profile, devtools.ConvertSamples) written with mode 0600. The
// profile belongs to the session that holds the lease; a release stops it.
func init() {
	Register(&Command{
		Name:    "profiler start",
		Summary: "Start sampling the page's JavaScript with the WebKit ScriptProfiler (profiler stop writes the .cpuprofile).",
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runProfilerStart,
	})
	Register(&Command{
		Name:    "profiler stop",
		Summary: "Stop sampling and write the CPU profile (.cpuprofile, Chrome DevTools format, mode 0600) to path, or to a new file in the temp directory.",
		Args:    []Arg{{Name: "path", Help: "the profile file to write (default: a new iwdp-profile-*.cpuprofile in the temp directory)"}},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runProfilerStop,
	})
}

func runProfilerStart(ctx context.Context, env Env, _ Input) (Result, error) {
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	if err := d.p.StartProfiler(ctx, d.c, d.session); err != nil {
		return Result{}, err
	}
	return Result{Text: "CPU profiling started; run 'profiler stop [path]' to write the profile.", Data: map[string]any{"profiling": true}}, nil
}

func runProfilerStop(ctx context.Context, env Env, in Input) (Result, error) {
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	prof, err := d.p.StopProfiler(ctx, d.c, d.session)
	if err != nil {
		return Result{}, err
	}
	data, err := json.Marshal(prof)
	if err != nil {
		return Result{}, fmt.Errorf("encoding the profile: %w", err)
	}
	path, err := outputPath(env, arg(in, 0), "iwdp-profile-*.cpuprofile")
	if err != nil {
		return Result{}, err
	}
	if err := devtools.WritePrivateFile(path, data); err != nil {
		return Result{}, err
	}
	return Result{
		Text: fmt.Sprintf("CPU profile written to %s: %s, %s.", path, nounCount(len(prof.Samples), "sample"), nounCount(len(prof.Nodes), "node")),
		Data: map[string]any{"path": path, "samples": len(prof.Samples), "nodes": len(prof.Nodes)},
	}, nil
}
