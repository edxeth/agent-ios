package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/tools"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// eval evaluates JavaScript in the session's page (the agent-browser
// command; the output of the legacy W0 eval, which agents and the skill
// rely on): a string prints as is, any other value as indented JSON, so a
// number or a boolean prints raw on one line. A returned Promise is
// awaited. A thrown value or a rejected Promise is the command's error,
// with its message and the JavaScript stack where WebKit gives one
// (tools.JSException), and exits 1. After "frame <iframe>" the expression
// runs in the selected frame's execution context (frame.go), also in a
// cross-origin frame, and the JSON data's "origin" is that frame's URL, as
// agent-browser's data.origin; without a frame it is the page's URL.
func init() {
	Register(&Command{
		Name:    "eval",
		Summary: "Evaluate a JavaScript expression in the page and print the result, awaiting a returned Promise; the page is the session's, or the tab id or ws:// URL of --page. A string prints as is, other values as JSON; a thrown value exits 1 with its message and stack.",
		Args:    []Arg{{Name: "expression", Help: "the JavaScript expression (quote it for the shell)", Required: true}},
		Flags: []Flag{
			{Name: "await", Type: FlagBool, Help: "accepted for the older command line; eval always awaits a returned Promise"},
		},
		Page:    LeasedPage,
		Mutates: true, // the expression can change the page
		Output:  OutputText,
		Run:     runEval,
	})
}

// evalData is the --json form of an eval result: the value's JSON, or its
// description when it has none (undefined, a function, a symbol).
type evalData struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype,omitempty"`
	Value       json.RawMessage `json:"value,omitempty"`
	Description string          `json:"description,omitempty"`
	// Origin is the URL of the document the expression ran in, read
	// after the evaluation; omitted when it could not be read (the
	// content boundary then names the origin "unknown").
	Origin string `json:"origin,omitempty"`
}

// evalOriginExpr reads the URL of the evaluation's document.
const evalOriginExpr = "location.href"

func runEval(ctx context.Context, env Env, in Input) (Result, error) {
	ps, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return Result{}, err
	}
	if oe, ok := env.(interface{ Op() *core.Op }); ok && oe.Op() != nil && ps.Runtime != nil && ps.Refs != nil {
		// The session's frame scope (frame.go): the operation's caller
		// sends the evaluation to the frame's execution context.
		if _, err := openRuntime(ctx, env); err != nil {
			return Result{}, err
		}
	}
	res, err := tools.EvaluateScriptWithOptions(ctx, c, in.Args[0], tools.EvalOptions{ReturnByValue: true, AwaitPromise: true})
	if err != nil {
		return Result{}, err // a *tools.JSException carries the message and the stack
	}
	text, err := evalText(res.Result)
	if err != nil {
		return Result{}, err
	}
	data := evalData{Type: res.Result.Type, Subtype: res.Result.Subtype, Value: res.Result.Value}
	if res.Result.Value == nil {
		data.Description = res.Result.Description
	}
	data.Origin = evalOrigin(ctx, c)
	return Result{Text: text, Data: data}, nil
}

// evalOrigin returns the URL of the document that c evaluates in, or ""
// when it cannot be read (the expression navigated, or the frame went).
func evalOrigin(ctx context.Context, c webkit.Caller) string {
	raw, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": evalOriginExpr, "returnByValue": true})
	if err != nil {
		return ""
	}
	s, err := evalValue[string](raw)
	if err != nil {
		return ""
	}
	return s
}

// evalText renders a result as the legacy eval printed it.
func evalText(r webkit.RemoteObject) (string, error) {
	switch {
	case r.Value != nil:
		var v any
		if err := json.Unmarshal(r.Value, &v); err != nil {
			return "", fmt.Errorf("decoding the %s result: %w", r.Type, err)
		}
		if s, ok := v.(string); ok {
			// The legacy eval printed a string with fmt.Println. The
			// result writer ends a text with one newline unless it ends
			// with one, so an empty string, or one that ends with a
			// newline, gets that newline here: "" prints one empty line.
			if s == "" || strings.HasSuffix(s, "\n") {
				return s + "\n", nil
			}
			return s, nil
		}
		out, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return "", fmt.Errorf("encoding the %s result: %w", r.Type, err)
		}
		return string(out), nil
	case r.Description != "":
		return r.Description, nil
	}
	return "(" + r.Type + ")", nil
}
