package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
)

// trace start and trace stop (design docs/design/agentic-parity.md,
// section 4.2): a Timeline recording, with ScriptProfiler events, converted
// to Chrome trace JSON (devtools.ConvertTimeline) and written with mode
// 0600. The recording belongs to the session that holds the lease; a
// release stops it.
func init() {
	Register(&Command{
		Name:    "trace start",
		Summary: "Start a WebKit Timeline recording of the page for a Chrome trace file (trace stop writes it).",
		Flags: []Flag{
			{
				Name: "categories", Type: FlagString, Default: "timeline,script",
				Help: "comma-separated: timeline (Timeline records, always on) and script (JavaScript events of the ScriptProfiler)",
			},
		},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runTraceStart,
	})
	Register(&Command{
		Name:    "trace stop",
		Summary: "Stop the Timeline recording and write it as Chrome trace JSON (mode 0600) to path, or to a new file in the temp directory.",
		Args:    []Arg{{Name: "path", Help: "the trace file to write (default: a new iwdp-trace-*.json in the temp directory)"}},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run:     runTraceStop,
	})
}

func runTraceStart(ctx context.Context, env Env, in Input) (Result, error) {
	script := false
	for _, cat := range strings.Split(flagString(in, "categories"), ",") {
		switch strings.TrimSpace(cat) {
		case "timeline":
		case "script":
			script = true
		default:
			return Result{}, usagef("--categories: %q is not timeline or script", cat)
		}
	}
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	if err := d.p.StartTrace(ctx, d.c, d.session, script); err != nil {
		return Result{}, err
	}
	cats := "timeline"
	if script {
		cats += ",script"
	}
	return Result{
		Text: fmt.Sprintf("Trace recording started (categories %s); run 'trace stop [path]' to write it.", cats),
		Data: map[string]any{"recording": true, "categories": cats},
	}, nil
}

// outputPath resolves a command's output path, or creates a new private
// file in the temp directory from pattern (os.CreateTemp) when p is "".
func outputPath(env Env, p, pattern string) (string, error) {
	if p != "" {
		return env.ResolvePath(p), nil
	}
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", fmt.Errorf("creating an output file in the temp directory: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func runTraceStop(ctx context.Context, env Env, in Input) (Result, error) {
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	tr, records, err := d.p.StopTrace(ctx, d.c, d.session)
	if err != nil {
		return Result{}, err
	}
	data, err := json.Marshal(tr)
	if err != nil {
		return Result{}, fmt.Errorf("encoding the trace: %w", err)
	}
	path, err := outputPath(env, arg(in, 0), "iwdp-trace-*.json")
	if err != nil {
		return Result{}, err
	}
	if err := devtools.WritePrivateFile(path, data); err != nil {
		return Result{}, err
	}
	events := 0
	for _, e := range tr.TraceEvents {
		if e.Ph != "M" {
			events++
		}
	}
	return Result{
		Text: fmt.Sprintf("Trace written to %s: %s, %s.", path, nounCount(records, "Timeline record"), nounCount(events, "trace event")),
		Data: map[string]any{"path": path, "records": records, "events": events},
	}, nil
}
