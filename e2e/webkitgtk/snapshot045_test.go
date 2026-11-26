//go:build webkitgtk

package webkitgtk

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// Expected values of this file come from agent-browser 0.45.0's own tests
// (cli/src/native/e2e_tests.rs; the test is named per case) and the
// contract of docs agent-browser-045-contract.md sections 4 and 5; the
// page is e2e/fixtures/runtime/snapshot045.html.

// snapshot045Ready waits for the fifth level of the nested iframe chain.
const snapshot045Ready = `(function () {
	var d = document;
	for (var k = 1; k <= 5; k++) {
		var f = d.querySelector('iframe[name="level' + k + '"]');
		d = f && f.contentDocument;
		if (!d || d.readyState !== 'complete' || !d.body) { return false; }
	}
	return !!d.querySelector('button') && !!document.querySelector('iframe[name="results-frame"]').contentDocument.querySelector('button');
})()`

var anyRef = regexp.MustCompile(`ref=e\d+`)

// lineWith returns the first line of text that contains s.
func lineWith(text, s string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, s) {
			return l
		}
	}
	return ""
}

// #2578: hints on controls whose label is empty or shared, after the
// attribute group; none on unique labels (e2e_snapshot_hints_ambiguous_controls).
func TestSnapshot045Hints(t *testing.T) {
	navigate(t, "snapshot045.html", snapshot045Ready)
	ps := pageSession("d1", "snap045-hints", 1)
	got := anyRef.ReplaceAllString(mustRun(t, ps, "snapshot", "-i", "-s", "#hints"), "ref=eN")
	want := `- link [ref=eN] hint="img:cart"
- button [ref=eN] hint="icon:icon-trash; in:Blue mug"
- button [ref=eN] hint="icon:fa-heart; in:Blue mug"
- button [ref=eN] hint="img:pencil; id:edit-btn; in:Blue mug"
- textbox "Email" [ref=eN] hint="name:login-email"
- textbox "Email" [ref=eN] hint="name:newsletter-email"
- button "Save" [ref=eN]
- generic "Edit" [ref=eN] hint="in:Invoice 1" clickable [onclick]
- generic "Edit" [ref=eN] hint="in:Invoice 2" clickable [onclick]
- generic "Archive" [ref=eN] clickable [onclick]`
	if got != want {
		t.Fatalf("snapshot -i -s #hints:\n%s\nwant:\n%s", got, want)
	}
	// On in every mode: the full snapshot has the same hints.
	full := mustRun(t, ps, "snapshot")
	if l := lineWith(full, "name:login-email"); !strings.Contains(l, `textbox "Email" [ref=`) || !strings.Contains(l, `] hint="name:login-email"`) {
		t.Fatalf("full snapshot hint line %q:\n%s", l, full)
	}
	// Labels are counted per document: the "Save" button of the iframe
	// does not make the top document's "Save" shared.
	if l := lineWith(full, `button "Save"`); strings.Contains(l, "hint=") {
		t.Fatalf("Save in the top document got a hint: %q", l)
	}
	// --json: the hint is in the snapshot text; the refs map keeps the empty name.
	js := mustRun(t, ps, "--json", "snapshot", "-i", "-s", "#hints")
	if !strings.Contains(js, `hint=\"img:cart\"`) || strings.Contains(js, `"name":"img:cart"`) {
		t.Fatalf("--json snapshot:\n%s", js)
	}
}

// #2688: <summary>, a native control with a non-interactive role and a
// clickable in an open shadow root get refs, one per control, and the scan
// changes nothing in the page
// (e2e_snapshot_cursor_scan_covers_native_and_shadow_elements).
func TestSnapshot045CursorScan(t *testing.T) {
	navigate(t, "snapshot045.html", snapshot045Ready)
	ps := pageSession("d1", "snap045-scan", 1)
	eval(t, "window.__mutations = 0; 1")
	got := mustRun(t, ps, "snapshot", "-i", "-s", "#scan")
	for _, label := range []string{"Advanced", "Menu button", "Menu link", "Plain button", "Shadow clickable", "Grouped", "Pick colour"} {
		if !strings.Contains(lineWith(got, label), "ref=e") {
			t.Errorf("%q has no ref:\n%s", label, got)
		}
	}
	if n := len(anyRef.FindAllString(got, -1)); n != 7 {
		t.Errorf("%d refs, want 7:\n%s", n, got)
	}
	if l := lineWith(got, "Plain button"); strings.Contains(l, "clickable") {
		t.Errorf("a natively interactive button carries cursor hints: %q", l)
	}
	if m := string(eval(t, "window.__mutations")); m != "0" {
		t.Errorf("the snapshot changed the page: %s mutation records", m)
	}
	// e2e_snapshot_cursor_scan_ignores_inherited_pointer_and_body and
	// e2e_snapshot_cursor_scan_skips_hrefless_link_wrapping_control.
	got = mustRun(t, ps, "snapshot", "-i", "-s", "#inherit")
	for _, s := range []string{"Inherited pointer inner", "Slotted text"} {
		if strings.Contains(got, s) {
			t.Errorf("%q, which only inherits cursor:pointer, got a line:\n%s", s, got)
		}
	}
	if l := lineWith(got, "Wrapped"); !strings.HasPrefix(l, `- button "Wrapped" [ref=`) || strings.Count(got, "Wrapped") != 1 {
		t.Errorf("the link around a button got its own ref:\n%s", got)
	}
	if !strings.Contains(lineWith(got, "Bare anchor"), "ref=e") {
		t.Errorf("the bare anchor has no ref:\n%s", got)
	}
}

// #2688: snapshot -s covers every match in page order, a match nested in
// another once, a hidden match silently drops, and no match is an error
// (e2e_snapshot_selector_scopes_to_all_matches, _matches_skip_nested).
func TestSnapshot045SelectorEveryMatch(t *testing.T) {
	navigate(t, "snapshot045.html", snapshot045Ready)
	ps := pageSession("d1", "snap045-scope", 1)
	got := anyRef.ReplaceAllString(mustRun(t, ps, "snapshot", "-s", ".item"), "ref=eN")
	if want := "- button \"First\" [ref=eN]\n- button \"Second\" [ref=eN]\n- button \"Third\" [ref=eN]"; got != want {
		t.Fatalf("snapshot -s .item:\n%s\nwant:\n%s", got, want)
	}
	code, _, errOut := run(t, ps, "snapshot", "-s", ".nothing")
	if code == 0 || !strings.Contains(errOut, "Selector '.nothing' did not match any element") {
		t.Fatalf("snapshot -s .nothing: exit %d, %q", code, errOut)
	}
	code, _, errOut = run(t, ps, "snapshot", "-s", "div[")
	if code == 0 || !strings.Contains(errOut, "Invalid selector 'div[': ") {
		t.Fatalf("snapshot -s div[: exit %d, %q", code, errOut)
	}
}

// #2739 and #2737: an iframe without a name prints its name attribute; an
// iframe with neither prints no label; nested same-origin iframes are
// walked four levels deep (MAX_IFRAME_SNAPSHOT_DEPTH).
func TestSnapshot045Iframes(t *testing.T) {
	navigate(t, "snapshot045.html", snapshot045Ready)
	ps := pageSession("d1", "snap045-frames", 1)
	got := mustRun(t, ps, "snapshot", "-i", "-s", "#frames")
	if !strings.HasPrefix(lineWith(got, "results-frame"), `- Iframe "results-frame" [ref=`) {
		t.Errorf("iframe with a name attribute:\n%s", got)
	}
	if !regexp.MustCompile(`(?m)^- Iframe \[ref=e\d+\]$`).MatchString(got) {
		t.Errorf("no unlabelled iframe line:\n%s", got)
	}
	for k := 1; k <= 4; k++ {
		if !strings.Contains(got, "button \"Level "+string(rune('0'+k))+"\" [ref=") {
			t.Errorf("Level %d (iframe depth %d) is missing:\n%s", k, k, got)
		}
	}
	if strings.Contains(got, "Level 5") {
		t.Errorf("the fifth nested iframe was walked:\n%s", got)
	}
	// The ref of the deepest walked level resolves.
	ref := refNamed(t, got, `button "Level 4"`)
	if txt := mustRun(t, ps, "get", "text", "@"+ref); txt != "Level 4" {
		t.Errorf("get text @%s = %q", ref, txt)
	}
	// The refs map keeps the iframe's empty name (presentation only).
	if js := mustRun(t, ps, "--json", "snapshot", "-i", "-s", "#frames"); strings.Contains(js, `"name":"results-frame"`) {
		t.Errorf("the refs map took the iframe's display name:\n%s", js)
	}
}

// #2685: a ref from a replaced document fails with agent-browser 0.45.0's
// message (element.rs:564) under agent-ios's stale_ref code, for the top
// document after a reload and for a ref inside a nested iframe after the
// reload replaced it.
func TestSnapshot045ReplacedDocumentRef(t *testing.T) {
	navigate(t, "snapshot045.html", snapshot045Ready)
	ps := pageSession("d1", "snap045-stale", 1)
	got := mustRun(t, ps, "snapshot", "-i")
	top, deep := refNamed(t, got, `button "Save"`), refNamed(t, got, `button "Level 3"`)
	eval(t, "location.reload(); 1")
	navigate(t, "snapshot045.html", snapshot045Ready)
	for _, ref := range []string{top, deep} {
		code, _, errOut := run(t, ps, "get", "text", "@"+ref)
		want := "stale_ref: Ref @" + ref + " is from a document that is no longer loaded (the page navigated or the frame was removed). Take a new snapshot and use a fresh ref."
		if code == 0 || !strings.Contains(errOut, want) {
			t.Errorf("@%s after the reload: exit %d, %q", ref, code, errOut)
		}
	}
}

// level4 is an expression for the document of the fourth nested frame of
// snapshot045.html.
const level4 = "(function () { var d = document; for (var k = 1; k <= 4; k++) { d = d.querySelector('iframe[name=\"level' + k + '\"]').contentDocument; } return d; })()"

// Review F1 (runtime-input): a detached ref inside a nested frame is found
// again by role and name only in the ref's own document: a replacement in
// that frame resolves and takes the click, a top-document button of the
// same role and name is never taken, and once the frame is gone the ref is
// stale.
func TestSnapshot045NestedRefRecoversInItsOwnDocument(t *testing.T) {
	navigate(t, "snapshot045.html", snapshot045Ready)
	ps, _ := actionSession(t, hooks{})
	ref := refNamed(t, mustRun(t, ps, "snapshot", "-i", "-s", "#frames"), `button "Level 4"`)
	replace := level4 + ".querySelector('button').outerHTML = '<button onclick=\"top.frameClicks++\">Level 4</button>'; "
	eval(t, "window.frameClicks = 0; window.wrongClicks = 0; "+replace+"var a = document.createElement('button'); a.textContent = 'Level 4'; a.style.cssText = 'position:fixed;left:0;top:0;z-index:1000'; "+
		"a.onclick = function () { window.wrongClicks++; }; document.body.appendChild(a); true")
	code, _, errOut := act(context.Background(), ps, "--timeout", "3s", "click", "@"+ref)
	if wrong, right := pageString(t, "String(window.wrongClicks)"), pageString(t, "String(window.frameClicks)"); code != 0 || wrong != "0" || right != "1" {
		t.Fatalf("click @%s after a replacement in its frame and a same-named top button: exit %d, err %q, top clicks %s, frame clicks %s; want the frame's button once", ref, code, errOut, wrong, right)
	}
	eval(t, replace+"true")
	if code, out, errOut := run(t, ps, "get", "text", "@"+ref); code != 0 || strings.Split(out, "\n")[0] != "Level 4" {
		t.Fatalf("get text @%s after its button was replaced in its frame again: exit %d, out %q, err %q", ref, code, out, errOut)
	}
	eval(t, "document.getElementById('nest').remove(); true")
	code, _, errOut = run(t, ps, "get", "text", "@"+ref)
	if want := "stale_ref: Ref @" + ref + " is from a document that is no longer loaded"; code == 0 || !strings.Contains(errOut, want) {
		t.Fatalf("get text @%s after its frame was removed: exit %d, err %q; want %q", ref, code, errOut, want)
	}
}
