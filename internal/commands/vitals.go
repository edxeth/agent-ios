package commands

import (
	"context"

	"github.com/edxeth/agent-ios/internal/devtools"
)

// vitals (design docs/design/agentic-parity.md, section 4.2): Web Vitals
// of the page's current document. LCP, CLS and INP are reported as
// unavailable where the engine lacks their entry types (MAP 1.12).
func init() {
	Register(&Command{
		Name:      "vitals",
		Summary:   "Show the page's Web Vitals: LCP, CLS, TTFB, FCP, INP, DOMContentLoaded and load (unavailable ones say why).",
		Page:      LeasedPage,
		RetrySafe: true,
		Output:    OutputText,
		Run:       runVitals,
	})
}

func runVitals(ctx context.Context, env Env, _ Input) (Result, error) {
	_, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return Result{}, err
	}
	v, err := devtools.ReadVitals(ctx, c)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: v.Text(), Data: v}, nil
}
