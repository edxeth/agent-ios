//go:build webkitgtk

package webkitgtk

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Logpoints (parity-045 WP-LOGPOINTS; chrome-devtools-mcp 1.14.0
// set_logpoints): the logpoint commands through the daemon against a real
// WebKitGTK page. The expected locations come from the fixtures
// (e2e/fixtures/runtime/logpoint*.js and their hand-written source maps,
// README.md there); the values from the scripts' arithmetic.

// waitOutput runs agent-ios args until ok accepts its output, within the
// test's deadline or 15 s, and returns that output.
func waitOutput(t *testing.T, ok func(string) bool, args ...string) string {
	t.Helper()
	end := time.Now().Add(15 * time.Second)
	if dl, has := t.Deadline(); has && dl.Before(end) {
		end = dl
	}
	out := ""
	for time.Now().Before(end) {
		if out = mustCLI(t, args...); ok(out) {
			return out
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("agent-ios %s never gave the expected output; last:\n%s", strings.Join(args, " "), out)
	return ""
}

// waitCount waits until the page's counter expr has grown by n from its
// current value: the page runs, nothing pauses it.
func waitCount(t *testing.T, expr string, n int) int {
	t.Helper()
	start, err := strconv.Atoi(js(t, "String("+expr+")"))
	if err != nil {
		t.Fatal(err)
	}
	var now int
	waitOutput(t, func(out string) bool {
		now, _ = strconv.Atoi(strings.TrimSpace(out))
		return now >= start+n
	}, "eval", "String("+expr+")")
	return now
}

// A logpoint on a plain script: a line without code moves to the next
// one (the bound location is printed), the logged values come from the
// scope of the location, the page never pauses, an expression that throws
// logs a console error and still does not pause, the logpoint survives a
// reload, removal stops it, and the lease release removes the rest.
func TestLogpoint045PlainScript(t *testing.T) {
	devtoolsBrowser(t, origin+"/logpoint.html")
	mustCLI(t, "console") // the lease and its console capture

	out := mustCLI(t, "logpoint", "add", "logpoint.js:5", `"lp-hit", step, doubled`)
	if want := "1: logpoint.js:5 \"lp-hit\", step, doubled -> bound at " + origin + "/logpoint.js:6:3"; !strings.Contains(out, want) {
		t.Fatalf("logpoint add on the empty line 5:\n%s\nwant %q (the next line with code)", out, want)
	}
	hit := regexp.MustCompile(`lp-hit (\d+) (\d+)`)
	cons := waitOutput(t, func(o string) bool { return hit.MatchString(o) }, "console")
	for _, m := range hit.FindAllStringSubmatch(cons, -1) {
		step, _ := strconv.Atoi(m[1])
		doubled, _ := strconv.Atoi(m[2])
		if doubled != 2*step {
			t.Fatalf("logged %q: doubled is not 2*step; the values are not those of the location's scope", m[0])
		}
	}
	waitCount(t, "window.lpCount", 5) // a logpoint never pauses

	out = mustCLI(t, "logpoint", "add", "logpoint.js:4", "nosuchvar.x")
	if want := "2: logpoint.js:4 nosuchvar.x -> bound at " + origin + "/logpoint.js:4:3"; !strings.Contains(out, want) {
		t.Fatalf("logpoint add of a throwing expression:\n%s\nwant %q", out, want)
	}
	waitOutput(t, func(o string) bool { return strings.Contains(o, "nosuchvar") }, "console", "--level", "error")
	waitCount(t, "window.lpCount", 5) // the exception did not pause the page

	list := mustCLI(t, "logpoint", "list")
	for _, want := range []string{"1: logpoint.js:5", "2: logpoint.js:4", origin + "/logpoint.js:6:3", origin + "/logpoint.js:4:3"} {
		if !strings.Contains(list, want) {
			t.Fatalf("logpoint list:\n%s\nwant %q", list, want)
		}
	}
	if r := cli(t, "logpoint", "add", "logpoint.js:100", "1"); r.code == 0 || !strings.Contains(r.stderr, "no executable code") {
		t.Fatalf("logpoint add after the script's last line: exit %d, stderr %q; want a failure naming no executable code", r.code, r.stderr)
	}

	// Reload: the counter starts at 0 again, so "lp-hit 0 0" is a hit of
	// the new document.
	mustCLI(t, "console", "clear")
	mustCLI(t, "reload")
	waitOutput(t, func(o string) bool { return strings.Contains(o, "lp-hit 0 0") }, "console")

	// Removal: the page runs a few ticks after the removal, and none logs.
	mustCLI(t, "logpoint", "remove", "1")
	mustCLI(t, "console", "clear")
	waitCount(t, "window.lpCount", 3)
	if cons = mustCLI(t, "console"); strings.Contains(cons, "lp-hit") {
		t.Fatalf("console after logpoint remove 1:\n%s\nwant no lp-hit message", cons)
	}
	if out := mustCLI(t, "logpoint", "clear"); !strings.Contains(out, "Removed 1 logpoint(s).") {
		t.Fatalf("logpoint clear: %q", out)
	}
	if out := mustCLI(t, "logpoint", "list"); strings.TrimSpace(out) != "No logpoints." {
		t.Fatalf("logpoint list after clear: %q", out)
	}

	// The lease release removes the session's logpoints in the page.
	mustCLI(t, "logpoint", "add", "logpoint.js:6", `"lp-rel", window.lpCount`)
	waitOutput(t, func(o string) bool { return strings.Contains(o, "lp-rel") }, "console")
	mustCLI(t, "session", "release")
	if out := mustCLI(t, "logpoint", "list"); strings.TrimSpace(out) != "No logpoints." {
		t.Fatalf("logpoint list after the lease release: %q", out)
	}
	c0, _ := strconv.Atoi(js(t, "String(window.lpCount)"))
	waitCount(t, "window.lpCount", 3)
	rel := regexp.MustCompile(`lp-rel (\d+)`)
	for _, m := range rel.FindAllStringSubmatch(mustCLI(t, "console"), -1) {
		if n, _ := strconv.Atoi(m[1]); n > c0+1 {
			t.Fatalf("%q was logged after the lease release (counter %d at the release)", m[0], c0)
		}
	}
}

// Logpoints on original sources: an inline (data: URL) source map and a
// map file, located by path suffix; the bound location is printed in the
// original source and in the generated script.
func TestLogpoint045SourceMapped(t *testing.T) {
	devtoolsBrowser(t, origin+"/logpoint.html")
	mustCLI(t, "console")

	out := mustCLI(t, "logpoint", "add", "src/cart.ts:7", `"lp-sum", sum`)
	if want := "bound at " + origin + "/src/cart.ts:7:5 (generated " + origin + "/logpoint-mapped.js:5:5)"; !strings.Contains(out, want) {
		t.Fatalf("logpoint add src/cart.ts:7:\n%s\nwant %q", out, want)
	}
	waitOutput(t, func(o string) bool { return strings.Contains(o, "lp-sum 0") && strings.Contains(o, "lp-sum 2") }, "console")

	out = mustCLI(t, "logpoint", "add", "basket.ts:7", `"lp-basket", sum, items.length // a comment ends at the line end`)
	if want := "bound at " + origin + "/src/basket.ts:7:5 (generated " + origin + "/logpoint-mapped2.js:5:5)"; !strings.Contains(out, want) {
		t.Fatalf("logpoint add basket.ts:7 (map file):\n%s\nwant %q", out, want)
	}
	waitOutput(t, func(o string) bool { return strings.Contains(o, "lp-basket 2 2") }, "console")
	waitCount(t, "window.lpTotal2 + window.lpTotal", 0)
	if r := cli(t, "logpoint", "add", "nosuch.ts:1", "1"); r.code != 0 {
		t.Fatalf("a path suffix that matches no loaded script waits for one: exit %d %q", r.code, r.stderr)
	} else if !strings.Contains(r.stdout, "not bound yet") {
		t.Fatalf("logpoint add nosuch.ts:1: %q, want not bound yet", r.stdout)
	}
}

// A logpoint is desired state of its session: after the page connection
// drops and the daemon takes the page over, the new connection gets the
// breakpoint again and it logs.
func TestLogpoint045SurvivesAReconnect(t *testing.T) {
	p := devtoolsBrowser(t, origin+"/logpoint.html")
	mustCLI(t, "console")
	mustCLI(t, "logpoint", "add", "logpoint.js:6", `"lp-again", step`)
	waitOutput(t, func(o string) bool { return strings.Contains(o, "lp-again") }, "console")
	reconnect(t, p)
	mustCLI(t, "console", "clear")
	waitOutput(t, func(o string) bool { return strings.Contains(o, "lp-again") }, "console")
	if out := mustCLI(t, "logpoint", "list"); !strings.Contains(out, "1: logpoint.js:6") {
		t.Fatalf("logpoint list after the reconnect:\n%s", out)
	}
}
