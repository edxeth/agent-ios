package cdp

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// readPageShims reads the page compatibility shims (page_shims.js) as a
// test fixture, independent of how the package embeds them.
func readPageShims(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("page_shims.js")
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

// bootSources returns the sources of the Page.setBootstrapScript calls in
// sent (lines of w5b2Page.sentSince).
func bootSources(t *testing.T, sent []string) []string {
	t.Helper()
	var out []string
	for _, s := range sent {
		params, ok := strings.CutPrefix(s, "Page.setBootstrapScript ")
		if !ok {
			continue
		}
		var p struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal([]byte(params), &p); err != nil {
			t.Fatalf("Page.setBootstrapScript params %s: %v", params, err)
		}
		out = append(out, p.Source)
	}
	return out
}

// clipped shortens each line of sent to its first 200 bytes, so a
// failure does not print the agent runtime's bundle.
func clipped(sent []string) string {
	out := make([]string, len(sent))
	for i, s := range sent {
		if len(s) > 200 {
			s = s[:200] + "..."
		}
		out[i] = s
	}
	return strings.Join(out, "\n")
}

// docs/cdp.md "Page compatibility shims": a page attachment installs the
// shims in the current document (Runtime.evaluate) and as a bootstrap
// part of its own desired-state owner, which runs before the client's
// Page.addScriptToEvaluateOnNewDocument scripts; its detach sends the
// bootstrap script without the part.
func TestPageShimsInstalledAtAttachAndReleasedAtDetach(t *testing.T) {
	shims := readPageShims(t)
	w := newW5b2Page(t)
	sent := w.sentSince(0)
	expr, _ := json.Marshal(shims)
	var evaluated bool
	for _, s := range sent {
		evaluated = evaluated || strings.HasPrefix(s, "Runtime.evaluate ") && strings.Contains(s, string(expr))
	}
	boots := bootSources(t, sent)
	if !evaluated || len(boots) == 0 || !strings.Contains(boots[len(boots)-1], shims) {
		t.Fatalf("at attach the page received:\n%s\nwant the shims evaluated in the current document and in the bootstrap script", clipped(only(sent, "Runtime.evaluate", "Page.setBootstrapScript")))
	}
	m := w.mark()
	w.call(t, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.__clientInit = 1"})
	boots = bootSources(t, w.sentSince(m))
	if len(boots) == 0 {
		t.Fatal("Page.addScriptToEvaluateOnNewDocument sent no bootstrap script")
	}
	last := boots[len(boots)-1]
	if i, j := strings.Index(last, shims), strings.Index(last, "__clientInit"); i < 0 || j < 0 || i > j {
		t.Fatalf("shims at byte %d, client script at byte %d of the bootstrap script; want the shims before the client's script", i, j)
	}
	m = w.mark()
	w.detachPage(t)
	boots = bootSources(t, w.sentSince(m))
	if len(boots) == 0 || strings.Contains(boots[len(boots)-1], "__iwdpShim") {
		t.Fatalf("after the detach the page received:\n%s\nwant a bootstrap script without the shims", clipped(w.sentSince(m)))
	}
}

// A tab attachment does not install the shims (it runs no page code); an
// install that fails refuses the page attachment with the reason.
func TestPageShimsOnlyForPagesAndAFailedInstallRefusesTheAttach(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	newPageFake(f)
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if strings.Contains(string(c.Params), "__iwdpShim") {
			return nil, &fakewebkit.Error{Code: -32000, Message: "evaluation refused"}
		}
		return value(0), nil
	})
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
	att := eventsOf(c, "Target.attachedToTarget")
	tab := decode[attached](t, att[len(att)-1].Params).SessionID
	for _, rec := range f.Received() {
		if strings.Contains(string(rec.Params), "__iwdpShim") {
			t.Fatalf("the tab attachment sent %s %s; want no shims", rec.Method, rec.Params)
		}
	}
	_, err := c.Call(ctxFor(t, 10*time.Second), tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	if err == nil || !strings.Contains(err.Error(), "page compatibility shims") || !strings.Contains(err.Error(), "evaluation refused") {
		t.Fatalf("page attach with a failing shim install: %v; want an error that names the shims and the page's reason", err)
	}
	for _, e := range eventsOf(c, "Target.attachedToTarget") {
		if decode[attached](t, e.Params).TargetInfo.Type == "page" {
			t.Fatalf("a page attachment was announced: %s", e.Params)
		}
	}
}

// docs/cdp.md "Page compatibility shims", When: a failed install refuses
// the attachment with the reason, also when the page refuses the
// bootstrap script or the bootstrap part cannot be set.
func TestPageShimsFailedBootstrapRefusesTheAttach(t *testing.T) {
	for _, c := range []struct {
		name, session, want string
		refuseBoot          bool
	}{
		// The page refuses the Page.setBootstrapScript with the shims.
		{name: "bootstrap script refused", session: "s1", want: "Page.setBootstrapScript: bootstrap refused", refuseBoot: true},
		// The part name holds the session name, and a bootstrap part name
		// must be one line (core.BootstrapParts.Set).
		{name: "session name with a line break", session: "s\n1", want: "must be one non-empty line"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			f := r.fake["One"]
			newPageFake(f)
			if c.refuseBoot {
				// Only the bootstrap script with the shims: the page
				// connection's own setup sends one without them.
				f.Handle("Page.setBootstrapScript", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
					if strings.Contains(string(c.Params), "__iwdpShim") {
						return nil, &fakewebkit.Error{Code: -32000, Message: "bootstrap refused"}
					}
					return map[string]any{}, nil
				})
			}
			cl := dial(t, r.issue(t, c.session, "One", ScopePage).WS)
			call(t, cl, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
			att := eventsOf(cl, "Target.attachedToTarget")
			tab := decode[attached](t, att[len(att)-1].Params).SessionID
			_, err := cl.Call(ctxFor(t, 10*time.Second), tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
			if err == nil || !strings.Contains(err.Error(), "page compatibility shims") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("page attach: %v; want an error that names the shims and %q", err, c.want)
			}
			for _, e := range eventsOf(cl, "Target.attachedToTarget") {
				if decode[attached](t, e.Params).TargetInfo.Type == "page" {
					t.Fatalf("a page attachment was announced: %s", e.Params)
				}
			}
		})
	}
}
