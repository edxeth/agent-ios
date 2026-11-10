package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// get: page and element getters (design docs/design/agentic-parity.md,
// section 4.2 rows of get; agent-browser "get"). Styles come from
// getComputedStyle in the page, never from the CSS domain.
func init() {
	Register(&Command{
		Name:    "get",
		Summary: "Get url, title, or text, html, value, attr, count, box or styles of a target (@ref, CSS selector or locator).",
		Args: []Arg{
			{Name: "what", Help: "url, title, text, html, value, attr, count, box or styles", Required: true},
			{Name: "target", Help: "@ref, CSS selector or locator such as text=Save (not for url and title)"},
			{Name: "name", Help: "attr: the attribute name; styles: comma-separated properties (default: a common set)"},
		},
		Page:      LeasedPage,
		RetrySafe: true,
		Output:    OutputText,
		Run:       runGet,
	})
}

// getTargets says which getters take a target (true) and which do not.
var getTargets = map[string]bool{
	"url": false, "title": false,
	"text": true, "html": true, "value": true, "attr": true, "count": true, "box": true, "styles": true,
}

// box is the value of "get box": the bounding box in CSS pixels of the
// viewport, with the runtime's visibility and enabled state.
type box struct {
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Width   float64 `json:"width"`
	Height  float64 `json:"height"`
	Visible bool    `json:"visible"`
	Enabled bool    `json:"enabled"`
}

func runGet(ctx context.Context, env Env, in Input) (Result, error) {
	what, target, name := arg(in, 0), arg(in, 1), arg(in, 2)
	needs, known := getTargets[what]
	switch {
	case !known:
		return Result{}, usagef("get %q: want url, title, text, html, value, attr, count, box or styles", what)
	case needs && target == "":
		return Result{}, usagef("get %s needs a target: @ref, CSS selector or locator", what)
	case !needs && target != "":
		return Result{}, usagef("get %s takes no target", what)
	case what == "attr" && name == "":
		return Result{}, usagef("get attr needs an attribute name: get attr <target> <name>")
	case name != "" && what != "attr" && what != "styles":
		return Result{}, usagef("get %s takes no third argument", what)
	}
	p, err := openRuntime(ctx, env)
	if err != nil {
		return Result{}, err
	}
	args := map[string]any{"what": what}
	if needs {
		spec, err := p.target(ctx, target)
		if err != nil {
			return Result{}, err
		}
		args["target"] = spec
	}
	if name != "" {
		args["name"] = name
	}
	var raw json.RawMessage
	warnings, err := p.rt.CallWarn(ctx, "get", args, &raw)
	if err != nil {
		return Result{}, err
	}
	text, data, err := formatGet(what, raw)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: withWarnings(text, warnings), Data: map[string]any{"value": data, "warnings": warnings}}, nil
}

// formatGet turns the page value of a getter into its text and JSON forms.
func formatGet(what string, raw json.RawMessage) (string, any, error) {
	bad := func(err error) (string, any, error) {
		return "", nil, fmt.Errorf("get %s: decoding the page value %s: %w", what, raw, err)
	}
	switch what {
	case "count":
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return bad(err)
		}
		return strconv.Itoa(n), n, nil
	case "box":
		var b box
		if err := json.Unmarshal(raw, &b); err != nil {
			return bad(err)
		}
		data, err := json.Marshal(b)
		if err != nil {
			return bad(err)
		}
		return string(data), b, nil
	case "styles":
		var pairs [][2]string
		if err := json.Unmarshal(raw, &pairs); err != nil {
			return bad(err)
		}
		lines := make([]string, len(pairs))
		m := make(map[string]string, len(pairs))
		for i, kv := range pairs {
			lines[i] = kv[0] + ": " + kv[1]
			m[kv[0]] = kv[1]
		}
		return strings.Join(lines, "\n"), m, nil
	case "attr":
		// A missing attribute is null: no text, and null in JSON.
		var s *string
		if err := json.Unmarshal(raw, &s); err != nil {
			return bad(err)
		}
		if s == nil {
			return "", nil, nil
		}
		return *s, *s, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return bad(err)
	}
	return s, s, nil
}
