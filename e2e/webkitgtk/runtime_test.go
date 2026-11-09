//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	iwdpruntime "github.com/nnemirovsky/iwdp-mcp/internal/runtime"
)

// 7.5 test 2: each fixture's snapshot equals its hand-written expected text.
func TestSnapshotFixtures(t *testing.T) {
	for _, c := range []struct {
		page, wait, expected string
		args                 []string
	}{
		{page: "forms.html", expected: "forms.snapshot.txt"},
		{page: "forms.html", expected: "forms.interactive.txt", args: []string{"-i", "-u"}},
		{page: "aria.html", expected: "aria.snapshot.txt"},
		{page: "shadow.html", expected: "shadow.snapshot.txt"},
		{page: "hidden.html", expected: "hidden.snapshot.txt"},
		{
			page: "iframe.html", expected: "iframe.snapshot.txt",
			wait: "document.getElementById('child').contentDocument && document.getElementById('child').contentDocument.readyState === 'complete' && !!document.getElementById('child').contentDocument.querySelector('button')",
		},
	} {
		t.Run(c.expected, func(t *testing.T) {
			navigate(t, c.page, c.wait)
			ps := pageSession("d1", "snap-"+c.expected, 1)
			got := mustRun(t, ps, append([]string{"snapshot"}, c.args...)...)
			if want := readFixture(t, c.expected); got != want {
				t.Fatalf("snapshot of %s:\n%s\n\nwant (%s):\n%s", c.page, got, c.expected, want)
			}
		})
	}
}

// 7.5 test 3: the runtime's visibility equals the hand-written table, with
// Element.prototype.checkVisibility present and deleted (Safari 16.6 has
// none).
func TestVisibilityOracle(t *testing.T) {
	navigate(t, "hidden.html", "")
	var rows [][2]string
	for _, l := range strings.Split(readFixture(t, "hidden.expected.txt"), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 || strings.HasPrefix(l, "#") {
			continue
		}
		rows = append(rows, [2]string{f[0], f[1]})
	}
	if len(rows) < 20 {
		t.Fatalf("expected table has %d rows", len(rows))
	}
	ps := pageSession("d1", "visibility", 1)
	check := func(mode string) {
		for _, r := range rows {
			want := map[string]string{"visible": "true", "hidden": "false"}[r[1]]
			if got := mustRun(t, ps, "is", "visible", "#"+r[0]); got != want {
				t.Errorf("%s: is visible #%s = %s, want %s", mode, r[0], got, want)
			}
		}
	}
	if string(eval(t, "typeof Element.prototype.checkVisibility")) != `"function"` {
		t.Fatal("this WebKitGTK has no checkVisibility; the present-mode run would not test it")
	}
	check("checkVisibility present")
	eval(t, "delete Element.prototype.checkVisibility; typeof Element.prototype.checkVisibility")
	check("checkVisibility deleted")
}

var refPattern = regexp.MustCompile(`ref=e(\d+)`)

func refsOf(text string) []int {
	var out []int
	for _, m := range refPattern.FindAllStringSubmatch(text, -1) {
		var n int
		fmt.Sscan(m[1], &n)
		out = append(out, n)
	}
	return out
}

// 7.5 test 4: ref lifetime.
func TestRefLifetime(t *testing.T) {
	navigate(t, "forms.html", "")
	ps := pageSession("d1", "lifetime", 1)
	first := mustRun(t, ps, "snapshot", "-i")
	if again := mustRun(t, ps, "snapshot", "-i"); again != first {
		t.Fatalf("the same elements changed refs between snapshots:\n%s\n---\n%s", first, again)
	}
	// A replaced element that was unique by role and name resolves with a warning.
	eval(t, "var b = document.getElementById('signin'); b.replaceWith(b.cloneNode(true)); 1")
	code, out, errOut := run(t, ps, "get", "text", "@e2")
	if code != 0 || !strings.HasPrefix(out, "Sign In") || !strings.Contains(out, "warning: @e2") {
		t.Fatalf("replaced unique element: exit %d, %q, %q", code, out, errOut)
	}
	// A replaced element whose role and name were not unique is stale.
	eval(t, "var s = document.querySelector('#a .save'); s.replaceWith(s.cloneNode(true)); 1")
	if code, _, errOut := run(t, ps, "get", "text", "@e14"); code == 0 || !strings.Contains(errOut, "stale_ref") {
		t.Fatalf("replaced duplicate: exit %d, %q", code, errOut)
	}
	// After a navigation the old ref fails with the "page navigated" message.
	before := sess.Generation()
	navigate(t, "forms.html", "")
	if sess.Generation() == before {
		t.Fatal("the navigation did not change the document generation")
	}
	if code, _, errOut := run(t, ps, "get", "text", "@e1"); code == 0 || !strings.Contains(errOut, "page navigated since the snapshot that produced @e1; run snapshot") {
		t.Fatalf("ref after navigation: exit %d, %q", code, errOut)
	}
	// Ids never repeat in a session, also across navigations.
	maxBefore := 0
	for _, n := range refsOf(first) {
		maxBefore = max(maxBefore, n)
	}
	for _, n := range refsOf(mustRun(t, ps, "snapshot", "-i")) {
		if n <= maxBefore {
			t.Fatalf("ref e%d was handed out again after the navigation (earlier max e%d)", n, maxBefore)
		}
	}
}

// pageStats reads the page runtime's own view: the refs in its map and
// its owner.
func pageStats(t *testing.T) (refs int, owner string) {
	var v struct {
		Refs  int             `json:"refs"`
		Owner json.RawMessage `json:"owner"`
	}
	raw := eval(t, "window.__iwdp.call('stats', {}).value")
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("stats %s: %v", raw, err)
	}
	return v.Refs, string(v.Owner)
}

// 7.5 test 5: session A takes a snapshot; session B acquires the page.
// The reset runs before B's first call, A's refs are unknown_ref for B, and
// the page map holds only B's refs. Release clears the page maps.
func TestOwnershipHandoff(t *testing.T) {
	navigate(t, "forms.html", "")
	a := pageSession("d1", "A", 1)
	aRefs := refsOf(mustRun(t, a, "snapshot", "-i"))
	b := pageSession("d1", "B", 2)
	code, _, errOut := run(t, b, "get", "text", "@e3")
	if code == 0 || !strings.Contains(errOut, "unknown_ref") || !strings.Contains(errOut, "no snapshot produced @e3") {
		t.Fatalf("A's ref for B: exit %d, %q", code, errOut)
	}
	if n, owner := pageStats(t); n != 0 || !strings.Contains(owner, `"session":"B"`) {
		t.Fatalf("after B's first call the page holds %d refs, owner %s; want 0 and B", n, owner)
	}
	// The page reported the reset to B's table: its counter moved past A's refs.
	if next := b.Refs.Next(); next <= 17 {
		t.Fatalf("B's ref counter is %d after the reset; A had e1 to e17", next)
	}
	bRefs := refsOf(mustRun(t, b, "snapshot", "-i"))
	if n, _ := pageStats(t); n != len(bRefs) {
		t.Fatalf("page map holds %d refs, B's snapshot has %d", n, len(bRefs))
	}
	for _, n := range bRefs {
		for _, m := range aRefs {
			if n == m {
				t.Fatalf("B got ref e%d that A already had", n)
			}
		}
	}
	// Release of B's epoch restores the page and clears its maps.
	var released bool
	if err := b.Runtime.Call(context.Background(), "release", map[string]any{"epoch": 2}, &released); err != nil || !released {
		t.Fatalf("release = %v, %v", released, err)
	}
	if n, owner := pageStats(t); n != 0 || owner != "null" {
		t.Fatalf("after release the page holds %d refs, owner %s", n, owner)
	}
}

// 7.5 test 5: daemon restart against an unchanged document.
func TestDaemonRestart(t *testing.T) {
	navigate(t, "forms.html", "")
	old := pageSession("daemon-1", "s", 1)
	mustRun(t, old, "snapshot", "-i")
	restarted := pageSession("daemon-2", "s", 1)
	code, _, errOut := run(t, restarted, "get", "text", "@e3")
	if code == 0 || !strings.Contains(errOut, "daemon restarted; run snapshot") {
		t.Fatalf("old ref after a daemon restart: exit %d, %q", code, errOut)
	}
	for _, n := range refsOf(mustRun(t, restarted, "snapshot", "-i")) {
		if n <= 17 {
			t.Fatalf("the restarted daemon handed out e%d again", n)
		}
	}
}

// 7.5 test 6: two snapshots of one session in parallel get disjoint ref
// ranges, and every returned ref resolves to its element.
func TestConcurrentSnapshots(t *testing.T) {
	navigate(t, "forms.html", "")
	ps := pageSession("d1", "concurrent", 1)
	var wg sync.WaitGroup
	outs := make([]string, 2)
	for i, scope := range []string{"#a", "#b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i] = mustRun(t, ps, "snapshot", "-s", scope)
		}()
	}
	wg.Wait()
	ra, rb := refsOf(outs[0]), refsOf(outs[1])
	if len(ra) != 2 || len(rb) != 2 {
		t.Fatalf("scoped snapshots:\n%s\n%s", outs[0], outs[1])
	}
	all := append(append([]int{}, ra...), rb...)
	sort.Ints(all)
	for i := 1; i < len(all); i++ {
		if all[i] == all[i-1] {
			t.Fatalf("ref ranges overlap: %v and %v", ra, rb)
		}
	}
	for i, n := range ra {
		if got := mustRun(t, ps, "get", "text", fmt.Sprintf("@e%d", n)); got != "Save" {
			t.Errorf("@e%d (Save %d) resolves to %q", n, i, got)
		}
	}
	for i, n := range rb {
		if got, want := mustRun(t, ps, "get", "text", fmt.Sprintf("@e%d", n)), []string{"One", "Two"}[i]; got != want {
			t.Errorf("@e%d resolves to %q, want %q", n, got, want)
		}
	}
}

// 7.5 test 7: install is idempotent, and a page that overrides
// getBoundingClientRect after install does not change results.
func TestInstallIdempotentAndBuiltinsCaptured(t *testing.T) {
	navigate(t, "probes.html", "")
	ps := pageSession("d1", "install", 1)
	box := mustRun(t, ps, "get", "box", "#box")
	if box != `{"x":10,"y":20,"width":100,"height":50,"visible":true,"enabled":true}` {
		t.Fatalf("get box #box = %s", box)
	}
	ref := refsOf(mustRun(t, ps, "find", "testid", "go-link"))
	eval(t, "window.__iwdp.__mark = 'kept'; 1")
	eval(t, iwdpruntime.Source())
	if got := string(eval(t, "window.__iwdp.__mark")); got != `"kept"` {
		t.Fatalf("a second install replaced the runtime (marker %s)", got)
	}
	if got := mustRun(t, ps, "get", "text", fmt.Sprintf("@e%d", ref[0])); got != "Go" {
		t.Fatalf("ref after the second install resolves to %q", got)
	}
	eval(t, "Element.prototype.getBoundingClientRect = function () { return {x: 0, y: 0, width: 0, height: 0, top: 0, left: 0, right: 0, bottom: 0}; }; Element.prototype.getClientRects = function () { return []; }; 1")
	if got := mustRun(t, ps, "get", "box", "#box"); got != box {
		t.Fatalf("get box after the page overrode getBoundingClientRect = %s, want %s", got, box)
	}
	if got := mustRun(t, ps, "is", "visible", "#box"); got != "true" {
		t.Fatalf("is visible after the override = %s", got)
	}
}

// 7.5 test 8: the getters replace the hand-written probes of AGG [318],
// [319], [331] and [335].
func TestProbeReplacements(t *testing.T) {
	navigate(t, "probes.html", "")
	ps := pageSession("d1", "probes", 1)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"get", "title"}, "Probes"},
		{[]string{"get", "url"}, origin + "/probes.html"},
		{[]string{"get", "text", "#para"}, "Hello world"},
		{[]string{"get", "html", "#para"}, "Hello <b>world</b>"},
		{[]string{"get", "value", "#field"}, "typed"},
		{[]string{"get", "attr", "#link", "href"}, "#x"},
		{[]string{"get", "count", "input"}, "5"},
		{[]string{"get", "styles", "#box", "position,background-color,width"}, "position: absolute\nbackground-color: rgb(255, 0, 0)\nwidth: 100px"},
		{[]string{"is", "visible", "#covered"}, "true"},
		{[]string{"is", "enabled", "#covered"}, "true"},
		{[]string{"is", "enabled", "#in-disabled-fieldset"}, "false"},
		{[]string{"is", "editable", "#field"}, "true"},
		{[]string{"is", "editable", "#ro"}, "false"},
		{[]string{"is", "checked", "#checked"}, "true"},
		{[]string{"is", "focused", "#field"}, "false"},
		{[]string{"is", "visible", "#nothing-matches"}, "false"},
	} {
		if got := mustRun(t, ps, c.args...); got != c.want {
			t.Errorf("%s = %q, want %q", strings.Join(c.args, " "), got, c.want)
		}
	}
	// hit-test: the element at a point, its box and ref.
	hit := mustRun(t, ps, "hit-test", "50", "45")
	if !strings.Contains(hit, `<div id="box">`) || !strings.Contains(hit, "x=10 y=20 width=100 height=50") || len(refsOf(hit)) != 1 {
		t.Fatalf("hit-test 50 45 = %q", hit)
	}
	if hit := mustRun(t, ps, "hit-test", "30", "110"); !strings.Contains(hit, `<div id="cover">`) {
		t.Fatalf("hit-test over the covered button = %q, want the cover", hit)
	}
	// find by every locator kind returns the element with a ref.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"find", "role", "link", "--name", "Go"}, `- link "Go" [ref=`},
		{[]string{"find", "text", "Under"}, `- button "Under" [ref=`},
		{[]string{"find", "label", "Name"}, `- textbox "Name" [ref=`},
		{[]string{"find", "placeholder", "Your name"}, `- textbox "Name" [ref=`},
		{[]string{"find", "alt", "Picture"}, `- img "Picture" [ref=`},
		{[]string{"find", "title", "Go there"}, `- link "Go" [ref=`},
		{[]string{"find", "testid", "go-link"}, `- link "Go" [ref=`},
		{[]string{"find", "first", "input"}, `- textbox [ref=`},
		// agent-browser's nth is 0-based (agent-browser 0.41.2 "find --help":
		// "nth <index> <selector>  Nth matching element (0-based)"), and a
		// negative index counts from the end (its handle_nth).
		{[]string{"find", "nth", "1", "input"}, `- textbox [ref=`},
		{[]string{"find", "nth", "--", "-1", "input"}, `- textbox "Name" [ref=`},
	} {
		if got := mustRun(t, ps, c.args...); !strings.HasPrefix(got, c.want) {
			t.Errorf("%s = %q, want prefix %q", strings.Join(c.args, " "), got, c.want)
		}
	}
	if got := mustRun(t, ps, "find", "nth", "1", "input"); !strings.HasSuffix(got, ": fixed") {
		t.Errorf("find nth 1 input = %q, want the read-only field (the second input)", got)
	}
	if code, _, errOut := run(t, ps, "find", "text", "no such text anywhere"); code == 0 || !strings.Contains(errOut, "not_found") {
		t.Errorf("find without a match: exit %d, %q", code, errOut)
	}
	if code, _, errOut := run(t, ps, "get", "text", "input"); code == 0 || !strings.Contains(errOut, "ambiguous") || !strings.Contains(errOut, "5") {
		t.Errorf("ambiguous selector: exit %d, %q", code, errOut)
	}
	if out := mustRun(t, ps, "read"); !strings.Contains(out, "Hello world") || !strings.Contains(out, "Under") {
		t.Errorf("read = %q", out)
	}
}

// The operation fence of design 2.6 and 3.1, as the page runtime enforces
// it for W3b's attempt functions, driven by W1a's attempt loop.
func TestAttemptFence(t *testing.T) {
	navigate(t, "forms.html", "")
	ps := pageSession("d1", "fence", 3)
	if err := ps.Runtime.Call(context.Background(), "stats", nil, nil); err != nil {
		t.Fatal(err)
	}
	eval(t, `window.__iwdp.def("w3aStep", function (a) {
		return window.__iwdp.attempt(a, function () { return ""; }, function () {
			window.__w3aSteps = (window.__w3aSteps || []).concat([a.step]);
			return a.step;
		});
	}); 1`)
	sched := core.NewScheduler(sess, core.SchedulerOptions{Runtime: ps.Runtime.RuntimeCaller(), Generation: sess.Generation})
	defer sched.Close()
	sched.SetLease("fence", 3)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	op, err := sched.Begin(ctx, core.OpSpec{Name: "fence", Class: core.ClassMutate, Session: "fence", Epoch: 3})
	if err != nil {
		t.Fatal(err)
	}
	res, err := op.RunSteps(core.Action{Name: "test", Fn: "w3aStep", Steps: 3})
	op.End()
	if err != nil || res.Acked != 3 {
		t.Fatalf("RunSteps = %+v, %v", res, err)
	}
	if got := string(eval(t, "window.__w3aSteps")); got != "[1,2,3]" {
		t.Fatalf("page ran steps %s, want [1,2,3]", got)
	}
	var clock struct {
		Now float64 `json:"now"`
		Doc string  `json:"doc"`
	}
	if err := ps.Runtime.Call(ctx, "clock", nil, &clock); err != nil {
		t.Fatal(err)
	}
	attempt := func(mut func(a *core.AttemptArgs)) core.AttemptReply {
		a := core.AttemptArgs{OpID: "op-x", Epoch: 3, Doc: clock.Doc, Step: 1, Steps: 1, AttemptEnd: clock.Now + 60000, Deadline: clock.Now + 60000}
		mut(&a)
		var r core.AttemptReply
		if err := ps.Runtime.Call(ctx, "w3aStep", a, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, c := range []struct {
		name   string
		mut    func(a *core.AttemptArgs)
		status string
	}{
		{"attempt end passed", func(a *core.AttemptArgs) { a.AttemptEnd = clock.Now }, core.StatusExpired},
		{"deadline passed", func(a *core.AttemptArgs) { a.Deadline = clock.Now }, core.StatusExpired},
		{"other document", func(a *core.AttemptArgs) { a.Doc = "other" }, core.StatusRefused},
		{"older epoch", func(a *core.AttemptArgs) { a.Epoch = 2 }, core.StatusRefused},
		{"not the next step", func(a *core.AttemptArgs) { a.Step = 2 }, core.StatusRefused},
		{"first step", func(a *core.AttemptArgs) {}, core.StatusDispatched},
		{"same step again", func(a *core.AttemptArgs) {}, core.StatusRefused},
	} {
		if r := attempt(c.mut); r.Status != c.status || r.Doc != clock.Doc {
			t.Errorf("%s: status %q (%s), doc %q; want %q", c.name, r.Status, r.Reason, r.Doc, c.status)
		}
	}
	if err := ps.Runtime.Call(ctx, "cancel", map[string]string{"opId": "op-y"}, nil); err != nil {
		t.Fatal(err)
	}
	if r := attempt(func(a *core.AttemptArgs) { a.OpID = "op-y" }); r.Status != core.StatusRefused {
		t.Errorf("canceled op: status %q", r.Status)
	}
}

// refNamed returns the ref of the snapshot line that starts with line, such
// as `button "Sign In"`.
func refNamed(t *testing.T, snapshot, line string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*- ` + regexp.QuoteMeta(line) + ` \[(?:[^\]]*, )?ref=(e\d+)`).FindStringSubmatch(snapshot)
	if m == nil {
		t.Fatalf("no line %s with a ref in:\n%s", line, snapshot)
	}
	return m[1]
}

// Design 3.1 and 3.3: refs of an earlier owner are unknown, never rebound
// by role and name, also when the session keeps its ref table across a new
// lease epoch and when it reacquires the page after another session.
func TestOwnerResetMakesOldRefsUnknown(t *testing.T) {
	navigate(t, "forms.html", "")
	ctx := context.Background()
	ps := pageSession("d1", "S", 1)
	ref := refNamed(t, mustRun(t, ps, "snapshot", "-i"), `button "Sign In"`)
	// The same session under lease epoch 2, with its table.
	ps.Runtime.SetOwner(iwdpruntime.Owner{Daemon: "d1", Session: "S", Epoch: 2})
	code, out, errOut := run(t, ps, "get", "text", "@"+ref)
	if code == 0 || !strings.Contains(errOut, "unknown_ref: @"+ref+": the lease of this page changed since the snapshot that produced it; run snapshot") {
		t.Fatalf("ref of lease epoch 1 under epoch 2: exit %d, %q, %q", code, out, errOut)
	}
	// The page itself: the spec the table had, in the call that brings the
	// next epoch, is not resolved by role and name.
	ps.Runtime.SetOwner(iwdpruntime.Owner{Daemon: "d1", Session: "S", Epoch: 3})
	ref2 := refNamed(t, mustRun(t, ps, "snapshot", "-i"), `button "Sign In"`)
	ps.Runtime.SetOwner(iwdpruntime.Owner{Daemon: "d1", Session: "S", Epoch: 4})
	var got string
	err := ps.Runtime.Call(ctx, "get", map[string]any{
		"what":   "text",
		"target": map[string]any{"ref": ref2, "role": "button", "name": "Sign In", "unique": true},
	}, &got)
	var rerr *iwdpruntime.Error
	if !errors.As(err, &rerr) || rerr.Code != "unknown_ref" || !strings.Contains(rerr.Message, "the lease of this page changed") {
		t.Fatalf("page resolve of an epoch-3 ref under epoch 4 = %q, %v; want unknown_ref", got, err)
	}
	// Session A reacquires the page after session B owned it.
	a := pageSession("d1", "A", 10)
	aRef := refNamed(t, mustRun(t, a, "snapshot", "-i"), `button "Sign In"`)
	mustRun(t, pageSession("d1", "B", 11), "get", "title")
	a.Runtime.SetOwner(iwdpruntime.Owner{Daemon: "d1", Session: "A", Epoch: 12})
	if code, out, errOut := run(t, a, "get", "text", "@"+aRef); code == 0 || !strings.Contains(errOut, "unknown_ref") {
		t.Fatalf("A's ref after B owned the page: exit %d, %q, %q", code, out, errOut)
	}
}

// Design 3.3: a frame document change counts as a new generation. The top
// document stays, so the session's generation does not change; the page
// must see that the element's frame now shows another document, although
// the old element stays connected to its old document.
func TestFrameNavigationMakesRefsStale(t *testing.T) {
	frameReady := "document.getElementById('child').contentDocument && document.getElementById('child').contentDocument.readyState === 'complete' && !!document.getElementById('child').contentDocument.querySelector('button')"
	navigate(t, "iframe.html", frameReady)
	ps := pageSession("d1", "frame", 1)
	ref := refNamed(t, mustRun(t, ps, "snapshot"), `button "In frame"`)
	if got := mustRun(t, ps, "get", "html", "@"+ref); got != "In frame" {
		t.Fatalf("get html @%s before the frame navigated = %q", ref, got)
	}
	// A scoped snapshot of a frame element follows the iframe's exposure:
	// shown while the iframe is, empty while the iframe is display none.
	if got, want := mustRun(t, ps, "snapshot", "-s", "@"+ref), `- button "In frame" [ref=`+ref+`]`; got != want {
		t.Fatalf("snapshot -s @%s = %q, want %q", ref, got, want)
	}
	eval(t, "document.getElementById('child').style.display = 'none'; 1")
	if got := mustRun(t, ps, "snapshot", "-s", "@"+ref); got != "(empty page)" {
		t.Fatalf("snapshot -s @%s inside a display:none iframe = %q", ref, got)
	}
	eval(t, "document.getElementById('child').style.display = ''; 1")
	// The frame's document does not inherit the iframe's visibility, but the
	// page walk does not enter a visibility:hidden iframe.
	eval(t, "document.getElementById('child').style.visibility = 'hidden'; 1")
	if got := mustRun(t, ps, "snapshot", "-s", "@"+ref); got != "(empty page)" {
		t.Fatalf("snapshot -s @%s inside a visibility:hidden iframe = %q", ref, got)
	}
	if got := mustRun(t, ps, "snapshot"); got != "(empty page)" {
		t.Fatalf("page snapshot with a visibility:hidden iframe = %q", got)
	}
	eval(t, "document.getElementById('child').style.visibility = ''; 1")
	gen := sess.Generation()
	eval(t, `window.__oldFrameButton = document.getElementById('child').contentDocument.querySelector('button'); document.getElementById('child').srcdoc = '<button>New frame</button>'; 1`)
	newFrame := frameReady + " && document.getElementById('child').contentDocument.querySelector('button').textContent === 'New frame'"
	for deadline := time.Now().Add(10 * time.Second); string(eval(t, "!!("+newFrame+")")) != "true"; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the frame did not show its new document within 10 s")
		}
	}
	if string(eval(t, "window.__oldFrameButton.isConnected")) != "true" || sess.Generation() != gen {
		t.Fatalf("precondition: the old button is connected (%s) and the top document generation is unchanged (%d, was %d)",
			eval(t, "window.__oldFrameButton.isConnected"), sess.Generation(), gen)
	}
	code, out, errOut := run(t, ps, "get", "html", "@"+ref)
	if code == 0 || !strings.Contains(errOut, "stale_ref: frame navigated since the snapshot that produced @"+ref+"; run snapshot") {
		t.Fatalf("ref into the navigated frame: exit %d, %q, %q", code, out, errOut)
	}
	if got := mustRun(t, ps, "snapshot", "-i"); !strings.Contains(got, `button "New frame"`) {
		t.Fatalf("snapshot after the frame navigated:\n%s", got)
	}
}

// Design 3.1: the owner epoch never moves back within one daemon. An owned
// call of an older epoch is refused before it changes anything, also a
// release, and also after the newer owner released the page; another
// daemon (a restart) may take the page.
func TestOwnerEpochRollbackRefused(t *testing.T) {
	navigate(t, "forms.html", "")
	ctx := context.Background()
	newer := pageSession("d1", "S", 2)
	mustRun(t, newer, "snapshot", "-i")
	refsBefore, _ := pageStats(t)
	older := pageSession("d1", "S", 1)
	if code, out, errOut := run(t, older, "get", "title"); code == 0 || !strings.Contains(errOut, "stale_owner") {
		t.Fatalf("call of epoch 1 after epoch 2: exit %d, %q, %q", code, out, errOut)
	}
	if n, owner := pageStats(t); n != refsBefore || !strings.Contains(owner, `"epoch":2`) {
		t.Fatalf("after the epoch-1 call the page holds %d refs (had %d), owner %s; want them kept and epoch 2", n, refsBefore, owner)
	}
	var released bool
	err := older.Runtime.Call(ctx, "release", map[string]any{"epoch": 1}, &released)
	var rerr *iwdpruntime.Error
	if !errors.As(err, &rerr) || rerr.Code != "stale_owner" {
		t.Fatalf("release of epoch 1 = %v, %v; want stale_owner", released, err)
	}
	if _, owner := pageStats(t); !strings.Contains(owner, `"epoch":2`) {
		t.Fatalf("after the epoch-1 release the owner is %s", owner)
	}
	if err := newer.Runtime.Call(ctx, "release", map[string]any{"epoch": 2}, &released); err != nil || !released {
		t.Fatalf("release of epoch 2 = %v, %v", released, err)
	}
	if code, out, errOut := run(t, older, "get", "title"); code == 0 || !strings.Contains(errOut, "stale_owner") {
		t.Fatalf("call of epoch 1 after epoch 2 released: exit %d, %q, %q", code, out, errOut)
	}
	if got := mustRun(t, pageSession("d2", "S", 1), "get", "title"); got != "Forms" {
		t.Fatalf("another daemon's call = %q", got)
	}
}

// Design 3.1: page code never dispatches after its call settled. An
// attempt's context reports live() false once the call returned, threw,
// resolved or rejected, so a callback the step left behind does nothing.
func TestAttemptContextClosesWhenTheCallSettles(t *testing.T) {
	navigate(t, "forms.html", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ps := pageSession("d1", "settle", 1)
	var clock struct {
		Now float64 `json:"now"`
		Doc string  `json:"doc"`
	}
	if err := ps.Runtime.Call(ctx, "clock", nil, &clock); err != nil {
		t.Fatal(err)
	}
	eval(t, `window.__iwdp.def("w3aKeep", function (a) {
		return window.__iwdp.attempt(a, null, function (ctx) {
			window.__w3aCtx = window.__w3aCtx || {};
			window.__w3aCtx[a.opId] = ctx;
			switch (a.args.mode) {
			case "throw": throw new Error("dispatch failed");
			case "reject": return Promise.reject(new Error("dispatch rejected"));
			case "resolve": return Promise.resolve(1);
			}
			return 1;
		});
	}); 1`)
	for _, c := range []struct {
		mode    string
		async   bool
		wantErr string
	}{
		{"return", false, ""},
		{"resolve", true, ""},
		{"reject", true, "dispatch rejected"},
		{"throw", false, "dispatch failed"},
	} {
		a := core.AttemptArgs{
			OpID: "settle-" + c.mode, Epoch: 1, Doc: clock.Doc, Step: 1, Steps: 1,
			AttemptEnd: clock.Now + 60000, Deadline: clock.Now + 60000, Args: map[string]string{"mode": c.mode},
		}
		var r core.AttemptReply
		call := ps.Runtime.Call
		if c.async {
			call = ps.Runtime.CallAsync
		}
		err := call(ctx, "w3aKeep", a, &r)
		if c.wantErr == "" && (err != nil || r.Status != core.StatusDispatched) {
			t.Fatalf("%s: %+v, %v", c.mode, r, err)
		}
		if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Fatalf("%s: error %v, want %q", c.mode, err, c.wantErr)
		}
		if live := string(eval(t, fmt.Sprintf("window.__w3aCtx[%q].live()", a.OpID))); live != "false" {
			t.Errorf("%s: the attempt context is still live after its call settled", c.mode)
		}
	}
}

// Design 3.2: an accessible name from content follows the flat tree, so a
// button in a shadow root is named by the light-DOM text assigned to its
// slot, as the snapshot shows it.
func TestSlottedContentNamesTheButton(t *testing.T) {
	navigate(t, "shadow.html", "")
	ps := pageSession("d1", "slots", 1)
	if got := mustRun(t, ps, "find", "role", "button", "--name", "Save Slotted", "--exact"); !strings.HasPrefix(got, `- button "Save Slotted" [ref=`) {
		t.Fatalf("find role button --name 'Save Slotted' --exact = %q", got)
	}
	if got := mustRun(t, ps, "get", "count", `role=button[name="Save Slotted"]`); got != "1" {
		t.Fatalf("get count of the slotted button locator = %q", got)
	}
}

// Design 3.2: a scoped snapshot never shows what the page snapshot hides.
// A scope root whose flat-tree ancestor excludes its subtree (display
// none, aria-hidden, content-visibility hidden, the content of a closed
// details element) gives an empty tree; a visible scope root keeps its
// node and ref, and a visibility:visible override inside a visibility:hidden
// parent stays, as in the page snapshot (hidden.snapshot.txt).
func TestScopedSnapshotHidesWhatThePageHides(t *testing.T) {
	navigate(t, "hidden.html", "")
	ps := pageSession("d1", "scoped", 1)
	for _, c := range []struct{ scope, want string }{
		{"#v-summary", `- button "more" [expanded=false, ref=e1]`},
		{"#v-visible-in-hidden", `- StaticText "shown again"`},
		{"#h-details-button", "(empty page)"},
		{"#h-details-closed", "(empty page)"},
		{"#h-button-in-none", "(empty page)"},
		{"#v-button-aria-hidden", "(empty page)"},
		{"#h-cv-child", "(empty page)"},
		{"#h-in-hidden-shadow", "(empty page)"},
	} {
		if got := mustRun(t, ps, "snapshot", "-s", c.scope); got != c.want {
			t.Errorf("snapshot -s %s:\n%s\nwant:\n%s", c.scope, got, c.want)
		}
	}
}

// Design 3.2: the snapshot walks the flat tree. A light-DOM child of a
// shadow host whose shadow root has no slot is not in the flat tree, so a
// scoped snapshot of it is empty; a shadow-tree element is shown.
func TestScopedSnapshotFollowsTheFlatTree(t *testing.T) {
	navigate(t, "shadow.html", "")
	ps := pageSession("d1", "scoped-flat", 1)
	for _, c := range []struct{ scope, want string }{
		{"#inner", `- button "Shadow button" [ref=e1]`},
		{"#unassigned", "(empty page)"},
	} {
		if got := mustRun(t, ps, "snapshot", "-s", c.scope); got != c.want {
			t.Errorf("snapshot -s %s:\n%s\nwant:\n%s", c.scope, got, c.want)
		}
	}
}

// scopedSnapshotProperty checks every element of the page (document, open
// shadow roots, same-origin iframes one level deep) against the page
// snapshot, in the page with the runtime's walk (the function behind
// snapshot and snapshot -s). An element is in the page snapshot when
// marking it aria-hidden="true", which removes an element and its subtree
// from the snapshot (design 3.2), changes the page tree. The property:
// the scoped tree of an element is empty exactly when the element is not
// in the page snapshot, and otherwise it occurs as a run of siblings, with
// the same refs, in the page tree. The body and html of the top document
// stand for the whole page and are checked to give the page tree.
const scopedSnapshotProperty = `(function () {
	var walk = window.__iwdp._.walk, J = JSON.stringify;
	function tree(root) { return walk(root, 1).tree; }
	function hasRun(list, run) {
		for (var i = 0; i + run.length <= list.length; i++) {
			if (J(list.slice(i, i + run.length)) === J(run)) { return true; }
		}
		for (var j = 0; j < list.length; j++) {
			if (list[j].children && hasRun(list[j].children, run)) { return true; }
		}
		return false;
	}
	var els = [];
	function collect(root, depth) {
		var list = root.querySelectorAll("*");
		for (var i = 0; i < list.length; i++) {
			var e = list[i];
			els.push(e);
			if (e.shadowRoot) { collect(e.shadowRoot, depth); }
			if (depth === 0 && e.localName === "iframe" && e.contentDocument) { collect(e.contentDocument, 1); }
		}
	}
	collect(document, 0);
	function desc(e) {
		return "<" + e.localName + (e.id ? "#" + e.id : "") + (e.ownerDocument !== document ? " in a frame" : "") + "> " + JSON.stringify((e.textContent || "").trim().slice(0, 30));
	}
	var bad = [], present = 0;
	if (J(tree(document.body)) !== J(tree(null)) || J(tree(document.documentElement)) !== J(tree(null))) {
		bad.push("the scoped tree of body or html is not the page tree");
	}
	els.forEach(function (e) {
		if (e === document.body || e === document.documentElement) { return; }
		var whole = tree(null);
		var had = e.getAttribute("aria-hidden");
		e.setAttribute("aria-hidden", "true");
		var hiddenTree = J(tree(null));
		if (had === null) { e.removeAttribute("aria-hidden"); } else { e.setAttribute("aria-hidden", had); }
		var inPage = hiddenTree !== J(whole);
		var s = tree(e);
		if (inPage) { present++; }
		if (inPage && s.length === 0) {
			bad.push(desc(e) + " is in the page snapshot, but its scoped snapshot is empty");
		} else if (!inPage && s.length > 0) {
			bad.push(desc(e) + " is not in the page snapshot, but its scoped snapshot is " + J(s));
		} else if (s.length > 0 && !hasRun(whole, s)) {
			bad.push(desc(e) + ": its scoped snapshot " + J(s) + " is not a run of siblings in the page tree");
		}
	});
	return { elements: els.length, present: present, bad: bad };
})()`

// Design 3.2: a scoped snapshot shows exactly what the page snapshot shows
// of the scope element, for every element of every fixture, also with the
// iframe hidden by visibility or display.
func TestScopedSnapshotEqualsPageSubtree(t *testing.T) {
	frameReady := "document.getElementById('child').contentDocument && document.getElementById('child').contentDocument.readyState === 'complete' && !!document.getElementById('child').contentDocument.querySelector('button')"
	for _, c := range []struct{ page, wait, setup string }{
		{page: "forms.html"},
		{page: "aria.html"},
		{page: "shadow.html"},
		{page: "hidden.html"},
		{page: "probes.html"},
		{page: "iframe.html", wait: frameReady},
		{page: "iframe.html", wait: frameReady, setup: "document.getElementById('child').style.visibility = 'hidden'"},
		{page: "iframe.html", wait: frameReady, setup: "document.getElementById('child').style.display = 'none'"},
	} {
		t.Run(c.page+" "+c.setup, func(t *testing.T) {
			navigate(t, c.page, c.wait)
			mustRun(t, pageSession("d1", "property", 1), "snapshot") // installs the runtime
			if c.setup != "" {
				eval(t, c.setup+"; 1")
			}
			var res struct {
				Elements, Present int
				Bad               []string
			}
			if raw := eval(t, scopedSnapshotProperty); json.Unmarshal(raw, &res) != nil || res.Elements < 3 {
				t.Fatalf("property check returned %s", raw)
			}
			for _, b := range res.Bad {
				t.Error(b)
			}
			t.Logf("%d elements, %d in the page snapshot", res.Elements, res.Present)
		})
	}
}
