package commands

import (
	"context"
	"strconv"

	"github.com/edxeth/agent-ios/internal/runtime"
)

// find: semantic locators (design docs/design/agentic-parity.md, section
// 4.2; agent-browser "find"). This file is the locate part of W3a: it
// prints the matches as snapshot lines with refs. An action after the
// locator (click, fill, ...) runs on the single match (find_action.go, W3b).
func init() {
	Register(&Command{
		Name:    "find",
		Summary: "Find elements by role, text, label, placeholder, alt, title, testid, or first|last|nth of a selector; print them with refs.",
		Args: []Arg{
			{Name: "kind", Help: "role, text, label, placeholder, alt, title, testid, first, last or nth", Required: true},
			{Name: "value", Help: "the role, text or attribute value; for first and last the selector; for nth the 0-based index (negative counts from the end)", Required: true},
			{Name: "action", Variadic: true, Help: "nth: the selector first; then an optional action and its arguments (" + findActionList + ")"},
		},
		Flags: []Flag{
			{Name: "name", Type: FlagString, Help: "role only: accessible name (case-insensitive substring unless --exact)"},
			{Name: "exact", Type: FlagBool, Help: "exact, case-sensitive text match"},
		},
		Page:    LeasedPage,
		Mutates: true, // ref allocation counts as a mutation (design 2.11)
		Output:  OutputText,
		Run:     runFind,
	})
}

var findKinds = map[string]bool{
	"role": true, "text": true, "label": true, "placeholder": true, "alt": true, "title": true, "testid": true,
	"first": true, "last": true, "nth": true,
}

func runFind(ctx context.Context, env Env, in Input) (Result, error) {
	kind, value := arg(in, 0), arg(in, 1)
	rest := in.Args[min(2, len(in.Args)):]
	selector := ""
	if kind == "nth" && len(rest) > 0 {
		selector, rest = rest[0], rest[1:]
	}
	args := map[string]any{"kind": kind, "value": value}
	switch {
	case !findKinds[kind]:
		return Result{}, usagef("find %q: want role, text, label, placeholder, alt, title, testid, first, last or nth", kind)
	case kind == "nth":
		// agent-browser 0.41.2 "find --help": "nth <index> <selector>  Nth
		// matching element (0-based)"; a negative index counts from the end
		// (its handle_nth).
		idx, err := strconv.Atoi(value)
		if err != nil {
			return Result{}, usagef("find nth %q: the index must be an integer (0-based)", value)
		}
		if selector == "" {
			return Result{}, usagef("find nth needs a selector: find nth <index> <selector>")
		}
		args["value"], args["index"] = selector, idx
	}
	act, err := parseFindAction(rest)
	if err != nil {
		return Result{}, err
	}
	if n := flagString(in, "name"); n != "" {
		args["name"] = n
	}
	if flagBool(in, "exact") {
		args["exact"] = true
	}
	p, err := openRuntime(ctx, env)
	if err != nil {
		return Result{}, err
	}
	var v struct {
		Nodes []runtime.Node `json:"nodes"`
	}
	warnings, _, err := p.allocate(ctx, "find", args, &v)
	if err != nil {
		return Result{}, err
	}
	text := runtime.Render(v.Nodes, runtime.RenderOptions{})
	if act != nil {
		return act.run(ctx, env, in, v.Nodes, warnings)
	}
	return Result{Text: withWarnings(text, warnings), Data: map[string]any{"matches": v.Nodes, "warnings": warnings}}, nil
}
