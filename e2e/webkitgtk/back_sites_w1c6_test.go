//go:build webkitgtk

package webkitgtk

import (
	"fmt"
	"strings"
	"testing"
)

// W1c T03, the W3b finding "Back across two sites" (docs/notes/w3b.md):
// after an open from site a to site b, which swaps the web process, the
// back command returns to the earlier target from WebKit's back/forward
// cache. Its provisional setup sent Page.enable to that target, WebKit
// answered "Page domain already enabled", and back exited 3 after about
// 30 s with every later call refused. The core now resets the agents of a
// restored target first (resetCalls); backforward_test.go checks that with
// raw sends, this test checks the back command itself: exit 0, and the
// page is the loaded nav.html of site a. forward then returns to site b.
func TestBackCommandAcrossTwoSites(t *testing.T) {
	e := newNavEnv(t, "nav.html", false)
	first := e.sites.a + "/nav.html"
	second := e.sites.b + "/second.html"
	e.must(agentA, "open", second)
	if e.swaps() == 0 {
		t.Fatal("open between the two sites made no process swap; the test does not cover the swap")
	}
	out, code, msg := e.run(agentA, "back")
	if code != 0 {
		t.Fatalf("back across two sites: exit %d: %s", code, msg)
	}
	if !strings.HasPrefix(out, "navigated to "+first) {
		t.Fatalf("back: %q, want navigated to %s", out, first)
	}
	if got := e.js(agentA, "location.href + ' ' + document.readyState"); got != fmt.Sprintf("%q", first+" complete") {
		t.Fatalf("after back the page is %s, want %s complete", got, first)
	}
	out, code, msg = e.run(agentA, "forward")
	if code != 0 {
		t.Fatalf("forward across two sites: exit %d: %s", code, msg)
	}
	if !strings.HasPrefix(out, "navigated to "+second) {
		t.Fatalf("forward: %q, want navigated to %s", out, second)
	}
	if got := e.js(agentA, "location.href + ' ' + document.readyState"); got != fmt.Sprintf("%q", second+" complete") {
		t.Fatalf("after forward the page is %s, want %s complete", got, second)
	}
}
