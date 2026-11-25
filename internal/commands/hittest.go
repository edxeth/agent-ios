package commands

import (
	"context"
	"math"
	"strconv"

	"github.com/nnemirovsky/agent-ios/internal/runtime"
)

// hit-test: the element at a point (design docs/design/agentic-parity.md,
// section 4.2; asked for in AGG). The file is hittest.go because Go treats
// a file named hit_test.go as a test file.
func init() {
	Register(&Command{
		Name:    "hit-test",
		Summary: "Print the element at a viewport point (CSS pixels): its snapshot line with a ref, its tag and its box.",
		Args: []Arg{
			{Name: "x", Help: "viewport x in CSS pixels", Required: true},
			{Name: "y", Help: "viewport y in CSS pixels", Required: true},
		},
		Page:    LeasedPage,
		Mutates: true, // ref allocation counts as a mutation (design 2.11)
		Output:  OutputText,
		Run:     runHitTest,
	})
}

func parseCoord(name, s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, usagef("hit-test: %s %q is not a number", name, s)
	}
	return v, nil
}

func formatNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func runHitTest(ctx context.Context, env Env, in Input) (Result, error) {
	x, err := parseCoord("x", arg(in, 0))
	if err != nil {
		return Result{}, err
	}
	y, err := parseCoord("y", arg(in, 1))
	if err != nil {
		return Result{}, err
	}
	p, err := openRuntime(ctx, env)
	if err != nil {
		return Result{}, err
	}
	var v struct {
		Nodes   []runtime.Node `json:"nodes"`
		Element string         `json:"element"`
		Box     hitBox         `json:"box"`
	}
	warnings, _, err := p.allocate(ctx, "hitTest", map[string]any{"x": x, "y": y}, &v)
	if err != nil {
		return Result{}, err
	}
	text := hitTestText(v.Nodes, v.Element, v.Box)
	return Result{Text: withWarnings(text, warnings), Data: map[string]any{"match": v.Nodes, "element": v.Element, "box": v.Box, "warnings": warnings}}, nil
}

// hitBox is the box of the element at the point.
type hitBox struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// hitTestText is the text of hit-test without the warnings; private mode
// makes it from the members that it prints (privateReads).
func hitTestText(nodes []runtime.Node, element string, b hitBox) string {
	return runtime.Render(nodes, runtime.RenderOptions{}) + " " + element +
		" x=" + formatNum(b.X) + " y=" + formatNum(b.Y) + " width=" + formatNum(b.Width) + " height=" + formatNum(b.Height)
}
