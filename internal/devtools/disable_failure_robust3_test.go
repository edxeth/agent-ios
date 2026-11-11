package devtools

import (
	"context"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Design 2.4 (reference count): a domain's disable is sent when its last
// owner gives it up. When that disable fails, the domain is still enabled
// in the page, so the owner keeps it: a later release of the owner (here
// the lease release, core.DesiredState.Release) finds the disable still
// owed and sends it again. The CDP endpoint keeps the domain the same way
// (internal/cdp releaseTimeline, TestCDPTracingEndTimelineDisableFailure).
// Each case gives up the last owner of its domain through a different
// devtools command.
func TestFailedDisableKeepsTheDomainForTheRelease(t *testing.T) {
	cases := []struct {
		name, domain string
		start, stop  func(ctx context.Context, p *Page, c webkit.Caller) error
	}{
		{
			name: "trace stop", domain: "Timeline",
			start: func(ctx context.Context, p *Page, c webkit.Caller) error { return p.StartTrace(ctx, c, "s1", false) },
			stop: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, _, err := p.StopTrace(ctx, c, "s1")
				return err
			},
		},
		{
			name: "route removal", domain: "Network",
			start: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/api/*", Abort: true})
				return err
			},
			stop: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.RemoveRoutes(ctx, c, "s1", "")
				return err
			},
		},
		{
			name: "headers cleared", domain: "Network",
			start: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.SetHeaders(ctx, c, "s1", map[string]string{"X-Test": "one"})
				return err
			},
			stop: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.SetHeaders(ctx, c, "s1", nil)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.must(t, tc.start)
			p := r.page(t)
			if !p.ps.State.Needs(tc.domain) {
				t.Fatalf("setup: %s has no owner after the start", tc.domain)
			}
			disable := tc.domain + ".disable"
			const refusal = "deliberate disable refusal"
			r.f.Handle(disable, func(fakewebkit.Call) (any, *fakewebkit.Error) {
				r.f.Handle(disable, nil)
				return nil, &fakewebkit.Error{Code: -32000, Message: refusal}
			})
			mark := lastSeq(r.f)
			if err := r.do(t, tc.stop); err == nil || !strings.Contains(err.Error(), refusal) {
				t.Fatalf("%s with a failing %s: %v", tc.name, disable, err)
			}
			if got := len(receivedAfter(r.f, disable, mark)); got != 1 {
				t.Fatalf("setup: %s sent %s %d times, want 1", tc.name, disable, got)
			}
			if !p.ps.State.Needs(tc.domain) {
				t.Fatalf("the failed %s gave up the last owner of %s, so nothing sends the disable again", disable, tc.domain)
			}
			mark = lastSeq(r.f)
			r.release(t)
			if got := len(receivedAfter(r.f, disable, mark)); got != 1 {
				t.Fatalf("the lease release sent %s %d times, want 1 (the disable still owed)", disable, got)
			}
			if p.ps.State.Needs(tc.domain) {
				t.Fatalf("%s still has an owner after the release", tc.domain)
			}
		})
	}
}
