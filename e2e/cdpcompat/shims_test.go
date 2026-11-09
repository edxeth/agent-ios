//go:build cdpcompat

package cdpcompat

import (
	"strings"
	"testing"
)

// docs/cdp.md "Page compatibility shims", device check 3 finding P1-A:
// Playwright's locator actions on a page without a native
// Element.prototype.checkVisibility (Safari 16.6, iOS 16.7). MiniBrowser
// starts on form-no-checkvisibility.html, which removes WebKitGTK's
// native method before any client attaches; Playwright then connects and,
// without a navigation, clicks with getByRole, fills with getByLabel and
// takes an ariaSnapshot through the endpoint's shim. Without the shim
// these steps fail with the device's "element.checkVisibility is not a
// function". The page keeps the shim after the client's detach.
func TestPlaywrightLocatorsWithoutNativeCheckVisibility(t *testing.T) {
	requireNode(t)
	const page = "/form-no-checkvisibility.html"
	g := startGTK(t, page)
	const session = "playwright-shims"
	u := g.clientSession(t, session)
	if got := strings.TrimSpace(cli(t, "--session", session, "eval", "typeof Element.prototype.checkVisibility")); got != "undefined" {
		t.Fatalf("before the attach the fixture's checkVisibility is %q, want undefined (the page removes the native method)", got)
	}
	run := g.runNode(t, "playwright-no-checkvisibility", "playwright-core", "noCheckVisibility", u.WS, g.site.origin)
	checkUnknown(t, "playwright-core", run.unknown)
	if got := pageState(t, session); got != (formState{URL: g.site.origin + page, Title: "CDP form", Button: "Clicked 1", Name: "Grace", Status: "name: Grace", Late: true}) {
		t.Errorf("the page after the scenario: %+v", got)
	}
	if got := strings.TrimSpace(cli(t, "--session", session, "eval", "Element.prototype.checkVisibility.__iwdpShim === true")); got != "true" {
		t.Errorf("after the client's detach the page's checkVisibility shim marker is %q, want true (the current document keeps the shim)", got)
	}
}
