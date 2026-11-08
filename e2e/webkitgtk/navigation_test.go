//go:build webkitgtk

package webkitgtk

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

const agentA = "agent-a"

// eventually polls cond (a JavaScript expression) until it is true.
func (e *navEnv) eventually(session, cond string, limit time.Duration) {
	e.t.Helper()
	for end := time.Now().Add(limit); ; time.Sleep(100 * time.Millisecond) {
		if e.js(session, "!!("+cond+")") == "true" {
			return
		}
		if time.Now().After(end) {
			e.t.Fatalf("%s: not true within %v", cond, limit)
		}
	}
}

// swapped reports whether the tap saw a committed process swap.
func (e *navEnv) swaps() int {
	n := 0
	for _, r := range e.tap.records() {
		if r.Method == "Target.didCommitProvisionalTarget" {
			n++
		}
	}
	return n
}

// 7.6 item 9: open works on WebKitGTK, which has no Page.navigate, and
// returns after load; back, forward, reload and pushstate move the page.
func TestOpenBackForwardReloadPushstate(t *testing.T) {
	e := newNavEnv(t, "nav.html", false)
	target := e.sites.b + "/second.html"
	out := e.must(agentA, "open", target)
	if !strings.HasPrefix(out, "navigated to "+target) {
		t.Fatalf("open: %q", out)
	}
	if got := e.js(agentA, "location.href + ' ' + document.readyState"); got != fmt.Sprintf("%q", target+" complete") {
		t.Fatalf("after open the page is %s, want %s complete", got, target)
	}
	if e.swaps() == 0 {
		t.Fatal("open between the two sites made no process swap; the test does not cover the swap")
	}
	for _, r := range e.tap.records() {
		if r.Out && r.Method == "Page.navigate" {
			t.Fatal("open sent Page.navigate")
		}
	}
	// History moves stay on one site: a back across the sites returns to
	// the earlier target, whose setup fails with "Page domain already
	// enabled" in the core (a W1 finding, docs/notes/w3b.md).
	e.must(agentA, "open", e.sites.b+"/nav.html")
	back := e.must(agentA, "back")
	if !strings.HasPrefix(back, "navigated to "+target) || e.js(agentA, "location.href") != fmt.Sprintf("%q", target) {
		t.Fatalf("back: %q", back)
	}
	if fwd := e.must(agentA, "forward"); !strings.HasPrefix(fwd, "navigated to "+e.sites.b+"/nav.html") {
		t.Fatalf("forward: %q", fwd)
	}
	e.must(agentA, "back")
	e.js(agentA, "window.__mark = 1")
	if out := e.must(agentA, "reload"); !strings.HasPrefix(out, "navigated to "+target) {
		t.Fatalf("reload: %q", out)
	}
	if got := e.js(agentA, "String(window.__mark) + ' ' + document.readyState"); got != "\"undefined complete\"" {
		t.Fatalf("after reload the page shows %s, want a new, loaded document", got)
	}
	if out := e.must(agentA, "pushstate", "/pushed?x=1"); !strings.Contains(out, "/pushed?x=1 (history.pushState)") {
		t.Fatalf("pushstate: %q", out)
	}
	if got := e.js(agentA, "location.pathname + location.search + ' ' + document.title"); got != "\"/pushed?x=1 Second\"" {
		t.Fatalf("after pushstate the page is %s, want the same document at /pushed?x=1", got)
	}
}

// 7.6 item 6: every wait form succeeds, and times out with exit 3 and the
// last state; wait --url succeeds across a real cross-site process swap.
func TestWaitFormsOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "nav.html", false)
	e.must(agentA, "open", e.sites.a+"/nav.html")
	if out := e.must(agentA, "wait", "200"); out != "waited 200 ms" {
		t.Fatalf("wait 200: %q", out)
	}
	e.js(agentA, "laterElement(400)")
	e.must(agentA, "wait", "#late")
	if e.js(agentA, "!!document.getElementById('late')") != "true" {
		t.Fatal("wait #late returned before the element existed")
	}
	e.js(agentA, "laterRemoveBanner(300)")
	e.must(agentA, "wait", "#banner", "--state", "detached")
	if e.js(agentA, "!!document.getElementById('banner')") != "false" {
		t.Fatal("wait --state detached returned while #banner was attached")
	}
	e.must(agentA, "wait", "--text", "Ready later")
	e.js(agentA, "laterFlag(300)")
	e.must(agentA, "wait", "--fn", "window.flag === 1")
	e.must(agentA, "wait", "--load", "load")

	for _, c := range []struct {
		argv []string
		last string
	}{
		{[]string{"wait", "#never", "--timeout", "1500ms"}, "no element matches"},
		{[]string{"wait", "--text", "Never there", "--timeout", "1500ms"}, "is not on the page"},
		{[]string{"wait", "--fn", "window.never === 1", "--timeout", "1500ms"}, "the expression is falsy"},
		{[]string{"wait", "--url", "**/never.html", "--timeout", "1500ms"}, "URL is still " + e.sites.a + "/nav.html"},
	} {
		start := time.Now()
		_, code, msg := e.run(agentA, c.argv...)
		if code != 3 || !strings.Contains(msg, c.last) || time.Since(start) > 3*time.Second {
			t.Fatalf("%v: exit %d after %v, %q; want exit 3 with %q", c.argv, code, time.Since(start), msg, c.last)
		}
	}

	swaps := e.swaps()
	e.js(agentA, fmt.Sprintf("laterGo(%q, 600)", e.sites.b+"/second.html"))
	if out := e.must(agentA, "wait", "--url", "**/second.html"); !strings.HasPrefix(out, "done: --url") {
		t.Fatalf("wait --url: %q", out)
	}
	if got := e.js(agentA, "location.href"); got != fmt.Sprintf("%q", e.sites.b+"/second.html") {
		t.Fatalf("after wait --url the page is %s", got)
	}
	if e.swaps() == swaps {
		t.Fatal("the navigation during wait --url was not a process swap")
	}
}

// Design 3.6 (W3b final round, leftover (a)): "wait --load networkidle"
// waits on the daemon's network tracker until no request has been in
// flight for 500 ms. The lease's start began the capture (W4 lease
// hooks), so the wait does not start it; a fetch in flight keeps the wait
// until 500 ms after its response; a wait that ends first is exit 3 with
// the request in flight.
func TestWaitNetworkIdleOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "nav.html", false)
	e.must(agentA, "open", e.sites.a+"/nav.html")
	if out := e.must(agentA, "wait", "--load", "networkidle"); out != "done: --load networkidle" {
		t.Fatalf("first wait --load networkidle: %q", out)
	}
	e.js(agentA, "slowFetch(1200)")
	start := time.Now()
	out := e.must(agentA, "wait", "--load", "networkidle")
	ended := time.Now()
	e.recMu.Lock()
	done := e.slowDone
	e.recMu.Unlock()
	if out != "done: --load networkidle" || done.IsZero() || ended.Sub(done) < 500*time.Millisecond {
		t.Fatalf("wait --load networkidle with a fetch in flight: %q after %v; the fetch answered at %v, %v before the wait ended; want the wait to end 500 ms after it",
			out, ended.Sub(start), done.Sub(start), ended.Sub(done))
	}
	e.js(agentA, "slowFetch(8000)")
	startTimeout := time.Now()
	_, code, msg := e.run(agentA, "wait", "--load", "networkidle", "--timeout", "1500ms")
	if code != 3 || !strings.Contains(msg, "1 request(s) still in flight") || time.Since(startTimeout) > 3*time.Second {
		t.Fatalf("wait --load networkidle --timeout 1500ms with a fetch in flight: exit %d after %v, %q; want exit 3 naming it", code, time.Since(startTimeout), msg)
	}
}

// Design 3.6 (review finding 4): a ref whose element the page removes
// satisfies --state detached and --state hidden; a ref the session never
// got stays an error.
func TestWaitForRemovedRefOnWebKitGTK(t *testing.T) {
	e := newNavEnv(t, "annotate.html", false)
	snap := e.must(agentA, "snapshot")
	ref := func(role, name string) string {
		m := regexp.MustCompile(role + ` "` + name + `" \[ref=(e[0-9]+)`).FindStringSubmatch(snap)
		if m == nil {
			t.Fatalf("no %s %q in the snapshot:\n%s", role, name, snap)
		}
		return "@" + m[1]
	}
	save, help := ref("button", "Save"), ref("link", "Help")
	e.js(agentA, "setTimeout(function () { document.getElementById('save').remove(); }, 300); true")
	if out, code, msg := e.run(agentA, "wait", save, "--state", "detached", "--timeout", "3s"); code != 0 {
		t.Fatalf("wait %s --state detached: exit %d, %q %q", save, code, out, msg)
	}
	if got := e.js(agentA, "document.getElementById('save') === null"); got != "true" {
		t.Fatalf("the wait ended while the button is still in the page (%s)", got)
	}
	e.js(agentA, "setTimeout(function () { document.querySelector('a').remove(); }, 300); true")
	if out, code, msg := e.run(agentA, "wait", help, "--state", "hidden", "--timeout", "3s"); code != 0 {
		t.Fatalf("wait %s --state hidden: exit %d, %q %q", help, code, out, msg)
	}
	if got := e.js(agentA, "document.querySelector('a') === null"); got != "true" {
		t.Fatalf("the wait ended while the link is still in the page (%s)", got)
	}
	if _, code, msg := e.run(agentA, "wait", "@e999", "--state", "detached", "--timeout", "2s"); code == 0 || !strings.Contains(msg, "@e999") {
		t.Fatalf("wait for a ref no snapshot produced: exit %d, %q; want its ref error", code, msg)
	}
}
