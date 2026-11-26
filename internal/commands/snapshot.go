package commands

import (
	"context"

	"github.com/edxeth/agent-ios/internal/runtime"
)

// snapshot: the accessibility snapshot with @eN refs (design
// docs/design/agentic-parity.md, section 3.2; agent-browser "snapshot").
func init() {
	Register(&Command{
		Name:    "snapshot",
		Summary: "Print the page's accessibility tree with @eN refs for later commands.",
		Flags: []Flag{
			{Name: "interactive", Short: "i", Type: FlagBool, Help: "show only elements with refs"},
			{Name: "compact", Short: "c", Type: FlagBool, Help: "keep lines with refs, values or text and their ancestors"},
			{Name: "depth", Short: "d", Type: FlagInt, Help: "deepest level shown (0 = top level only)"},
			{Name: "selector", Short: "s", Type: FlagString, Help: "scope the snapshot to every element matching a CSS selector or locator, or to a @ref"},
			{Name: "urls", Short: "u", Type: FlagBool, Help: "add url= to links"},
		},
		Page:    LeasedPage,
		Mutates: true, // ref allocation counts as a mutation (design 2.11)
		Output:  OutputText,
		Run:     runSnapshot,
	})
}

type snapshotValue struct {
	Tree  []runtime.Node `json:"tree"`
	URL   string         `json:"url"`
	Title string         `json:"title"`
}

type refInfo struct {
	Role string `json:"role"`
	Name string `json:"name"`
}

func runSnapshot(ctx context.Context, env Env, in Input) (Result, error) {
	opts := runtime.RenderOptions{Interactive: flagBool(in, "interactive"), Compact: flagBool(in, "compact"), URLs: flagBool(in, "urls")}
	if d, ok := in.Flags["depth"].(int); ok {
		if d < 0 {
			return Result{}, usagef("snapshot: --depth %d: want 0 or more", d)
		}
		opts.Depth, opts.DepthSet = d, true
	}
	p, err := openRuntime(ctx, env)
	if err != nil {
		return Result{}, err
	}
	args := map[string]any{}
	if sel := flagString(in, "selector"); sel != "" {
		spec, err := p.target(ctx, sel)
		if err != nil {
			return Result{}, err
		}
		args["scope"] = spec
	}
	var v snapshotValue
	var a allocated
	warnings, gen, err := p.allocate(ctx, "snapshot", args, &struct {
		*snapshotValue
		*allocated
	}{&v, &a})
	if err != nil {
		return Result{}, err
	}
	text := runtime.Render(v.Tree, opts)
	recordSnapshot(ctx, env, p, in, text)
	refMap := make(map[string]refInfo, len(a.Refs))
	for _, r := range a.Refs {
		refMap[r.Ref] = refInfo{Role: r.Role, Name: r.Name}
	}
	return Result{
		Text: withWarnings(text, warnings),
		Data: map[string]any{"snapshot": text, "tree": v.Tree, "url": v.URL, "title": v.Title, "generation": gen, "refs": refMap, "warnings": warnings},
	}, nil
}
