package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
)

// errors: uncaught errors and unhandled rejections with their stacks
// (design docs/design/agentic-parity.md, section 4.2 "errors", 7.7 test
// 4): WebKit's javascript error messages plus the bootstrap hooks of
// window "error" and "unhandledrejection", which the first run installs
// for new documents and the current one.
func init() {
	Register(&Command{
		Name:    "errors",
		MCPName: "get_errors",
		Profile: ProfileDevtools,
		Summary: "Print uncaught errors and unhandled promise rejections with their stacks (the first run installs the error hooks).",
		Flags: []Flag{
			{Name: "clear", Type: FlagBool, Help: "empty the error list after printing"},
		},
		Page:    LeasedPage,
		Mutates: true, // installs the error hooks
		Output:  OutputText,
		Run:     runErrors,
	})
}

func runErrors(ctx context.Context, env Env, in Input) (Result, error) {
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	k, err := d.p.StartErrors(ctx, d.c, d.session)
	if err != nil {
		return Result{}, err
	}
	errs := k.Errors()
	if flagBool(in, "clear") {
		k.ClearErrors()
	}
	if len(errs) == 0 {
		return Result{Text: "No errors.", Data: map[string]any{"errors": []devtools.PageError{}}}, nil
	}
	var b strings.Builder
	for _, e := range errs {
		fmt.Fprintf(&b, "#%d %s: %s", e.ID, e.Kind, e.Message)
		if e.URL != "" {
			fmt.Fprintf(&b, " (%s:%d:%d)", e.URL, e.Line, e.Column)
		}
		b.WriteByte('\n')
		if e.Stack != "" {
			b.WriteString(indent(e.Stack))
			b.WriteByte('\n')
		}
	}
	return Result{Text: strings.TrimRight(b.String(), "\n"), Data: map[string]any{"errors": errs}}, nil
}

// indent puts four spaces before every line of s that has none.
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "    ") {
			lines[i] = "    " + strings.TrimLeft(l, " ")
		}
	}
	return strings.Join(lines, "\n")
}
