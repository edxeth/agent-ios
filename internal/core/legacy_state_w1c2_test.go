package core

import (
	"context"
	"strings"
	"testing"
)

// W1c-2 (backlog C12): WebKit treats an empty user-agent override as no
// override (InspectorPageAgent::overrideUserAgent stores the value and
// applies it only when it is not empty), and internal/devtools already
// maps "" to a clear. A legacy {"value":""} is therefore the baseline, as
// a call without a value is: it takes no Page requirement, and its
// release sends nothing (a baseline value stays the owner's shared value
// until then, domain_owners.go).
func TestEmptyUserAgentOverrideIsNoOverride(t *testing.T) {
	for _, params := range []string{`{"value":""}`, `{}`} {
		t.Run(params, func(t *testing.T) {
			p, c, ps := ownersPage(t)
			from := len(p.wire)
			ownerCall(t, ps, c, "s1", "Page.overrideUserAgent", params)
			tg := p.live["page-0"]
			if tg.page || len(tg.overrides) != 0 {
				t.Fatalf("page-0 after %s: %s; want Page not enabled, no override", params, pageState(tg))
			}
			if sent := sentSince(p, from, "Page.enable"); len(sent) != 0 {
				t.Fatalf("%s sent %q; the baseline takes no Page requirement", params, sent)
			}
			if ps.State.Needs("Page") {
				t.Fatalf("Page is needed after %s; want no requirement", params)
			}
			from = len(p.wire)
			if _, err := releaseOwners(context.Background(), ps, "s1", c); err != nil {
				t.Fatal(err)
			}
			for _, w := range p.wire[from:] {
				if strings.Contains(w, "Page.") {
					t.Fatalf("the release of s1 sent %q; nothing was set", w)
				}
			}
		})
	}
	// An owner's real override stays the effective value next to it until
	// the empty one, the most recent value, replaces it; the empty one
	// never enables Page by itself.
	p, c, ps := ownersPage(t)
	ownerCall(t, ps, c, "s2", "Page.overrideUserAgent", `{"value":"ua-2"}`)
	ownerCall(t, ps, c, "s1", "Page.overrideUserAgent", `{"value":""}`)
	tg := p.live["page-0"]
	if !tg.page || len(tg.overrides) != 0 {
		t.Fatalf("page-0 after s1's empty override: %s; want Page kept for s2, no override", pageState(tg))
	}
	if _, err := releaseOwners(context.Background(), ps, "s2", c); err != nil {
		t.Fatal(err)
	}
	if tg.page || len(tg.overrides) != 0 {
		t.Fatalf("page-0 after s2's release: %s; want Page disabled (s1's empty value needs no Page)", pageState(tg))
	}
}
