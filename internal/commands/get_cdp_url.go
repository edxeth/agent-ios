package commands

import (
	"context"

	"github.com/nnemirovsky/agent-ios/internal/cdp"
	"github.com/nnemirovsky/agent-ios/internal/core"
)

// get cdp-url issues the CDP endpoint record of the caller's session and
// page (design docs/design/agentic-parity.md, sections 4.2, 5.1, 5.2 and
// 5.5). It prints one URL: the ws form, which every client uses verbatim
// (agent-browser needs it), or with --form http the discovery base for
// clients that append /json/version (Playwright and Puppeteer); --json
// returns both. Issuing takes no lease.
func init() {
	Register(&Command{
		Name: "get cdp-url",
		// The devtools profile keeps the core tool count of decision D2.
		Summary: "Print a CDP URL for this session's page (starts the CDP endpoint if needed). Clients act for this session; use the URL exactly as printed.",
		Flags: []Flag{
			{Name: "scope", Type: FlagEnum, Enum: []string{cdp.ScopePage, cdp.ScopeDevice}, Default: cdp.ScopePage, Help: "page: only this page; device: also the other pages of the devices reserved by --reservation"},
			{Name: "form", Type: FlagEnum, Enum: []string{"ws", "http"}, Default: "ws", Help: "ws: the browser WebSocket URL (every client; agent-browser --cdp, Puppeteer browserWSEndpoint, Playwright connectOverCDP); http: the discovery base for Playwright connectOverCDP and Puppeteer browserURL"},
		},
		ReadsPage: true,
		Run:       runGetCDPURL,
	})
}

func runGetCDPURL(ctx context.Context, env Env, in Input) (Result, error) {
	srv, err := cdpOf(env)
	if err != nil {
		return Result{}, err
	}
	if _, err := srv.StartDefault(nil); err != nil {
		return Result{}, err
	}
	g := in.Global
	ep, err := srv.Issue(ctx, cdp.IssueRequest{
		Session: g.Session, Reservation: g.Reservation, Scope: in.Flags["scope"].(string),
		Page: core.PageRequest{Transport: g.Transport, Page: g.Page},
	})
	if err != nil {
		return Result{}, err
	}
	text := ep.WS
	if in.Flags["form"] == "http" {
		text = ep.HTTP
	}
	return Result{Text: text, Data: ep}, nil
}
