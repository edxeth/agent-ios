package commands

import (
	"context"
	"strconv"
)

// is: element state checks (design docs/design/agentic-parity.md, section
// 4.2; agent-browser "is"). Visibility uses the runtime's own rule (3.4).
func init() {
	Register(&Command{
		Name:    "is",
		MCPName: "is",
		Profile: ProfileCore,
		Summary: "Print true or false: is a target visible, enabled, checked, editable or focused.",
		Args: []Arg{
			{Name: "state", Help: "visible, enabled, checked, editable or focused", Required: true},
			{Name: "target", Help: "@ref, CSS selector or locator such as label=Email", Required: true},
		},
		Page:      LeasedPage,
		RetrySafe: true,
		Output:    OutputText,
		Run:       runIs,
	})
}

var isStates = map[string]bool{"visible": true, "enabled": true, "checked": true, "editable": true, "focused": true}

func runIs(ctx context.Context, env Env, in Input) (Result, error) {
	state, target := arg(in, 0), arg(in, 1)
	if !isStates[state] {
		return Result{}, usagef("is %q: want visible, enabled, checked, editable or focused", state)
	}
	p, err := openRuntime(ctx, env)
	if err != nil {
		return Result{}, err
	}
	spec, err := p.target(ctx, target)
	if err != nil {
		return Result{}, err
	}
	var v bool
	warnings, err := p.rt.CallWarn(ctx, "is", map[string]any{"state": state, "target": spec}, &v)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: withWarnings(strconv.FormatBool(v), warnings), Data: map[string]any{"value": v, "warnings": warnings}}, nil
}
