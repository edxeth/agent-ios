package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/devtools"
)

// network har start and stop: a HAR 1.2 file of the requests that the
// network log sees between them (design docs/design/agentic-parity.md,
// section 4.2 "network har", 7.7 test 2). The file is written with mode
// 0600 and secret values are masked unless --show-secrets (design 7.9).
func init() {
	Register(&Command{
		Name:    "network har start",
		Summary: "Start recording a HAR of the requests from now on; 'network har stop' writes it.",
		Flags: []Flag{
			{Name: "content", Type: FlagEnum, Enum: []string{"all", "none"}, Default: "all", Help: "all: add the response bodies at stop; none: headers and timings only"},
		},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run: func(ctx context.Context, env Env, in Input) (Result, error) {
			d, err := devtoolsForCommand(ctx, env)
			if err != nil {
				return Result{}, err
			}
			n, note, err := netlogFor(ctx, d)
			if err != nil {
				return Result{}, err
			}
			if err := n.StartHAR(flagString(in, "content")); err != nil {
				return Result{}, err
			}
			text := "HAR recording started; run 'network har stop [path]' to write it."
			if note != "" {
				text = note + "\n" + text
			}
			return Result{Text: text}, nil
		},
	})
	Register(&Command{
		Name:    "network har stop",
		Summary: "Stop the HAR recording and write it as HAR 1.2 JSON (mode 0600; secret values masked unless --show-secrets).",
		Args:    []Arg{{Name: "path", Help: "file to write (default network-<time>.har in the current directory)"}},
		Flags: []Flag{
			{Name: "show-secrets", Type: FlagBool, Help: "keep secret header, cookie, query and field values"},
		},
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputFile,
		Run:     runHARStop,
	})
}

func runHARStop(ctx context.Context, env Env, in Input) (Result, error) {
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	n := d.p.Netlog(d.session)
	if n == nil {
		return Result{}, fmt.Errorf("no HAR recording runs; start one with 'network har start'")
	}
	show := flagBool(in, "show-secrets")
	har, err := n.StopHAR(ctx, d.c, devtools.HAROptions{Creator: devtools.HARCreator{Name: "iwdp-cli", Version: "dev"}, ShowValues: show})
	if err != nil {
		return Result{}, err
	}
	data, err := json.MarshalIndent(har, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("network har stop: encoding the HAR: %w", err)
	}
	path := arg(in, 0)
	if path == "" {
		path = "network-" + time.Now().Format("20060102-150405") + ".har"
	}
	path = env.ResolvePath(path)
	if err := devtools.WritePrivateFile(path, data); err != nil {
		return Result{}, err
	}
	return Result{
		Text: harStopText(len(har.Log.Entries), path, show),
		Data: map[string]any{"path": path, "entries": len(har.Log.Entries)},
	}, nil
}

// harStopText is the text of network har stop; show says whether secret
// values were kept. Private mode makes it from the members that it prints
// (privateReads).
func harStopText(entries int, path string, show bool) string {
	masked := "secret values masked; --show-secrets keeps them"
	if show {
		masked = "secret values kept"
	}
	return fmt.Sprintf("Wrote %d entries to %s (%s).", entries, path, masked)
}
