//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// agent-browser 0.45 #2587 and #2611 (contract section 11): the pointer
// actions scroll the press point into view, let the layout settle, scroll
// again when reflow moved the point out, press the settled point, and
// refuse a point that would land on another element than the target, a
// label that activates it, or a frame it embeds. Page: click045.html.

func loadClick045(t *testing.T, arm string) {
	t.Helper()
	navigate(t, "click045.html", "!!window.T")
	eval(t, "scrollTo(0, 0); T.arm = "+strings.TrimSpace(mustJSON(t, arm))+"; true")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// click045Hits returns the page's record of target events, "type id ok".
func click045Hits(t *testing.T) []string {
	t.Helper()
	var hits []string
	pageJSON(t, "T.hits", &hits)
	return hits
}

// wantOnTarget checks that event type reached id and that every recorded
// event carried a client point on the target.
func wantOnTarget(t *testing.T, cmd, id, typ string) {
	t.Helper()
	hits := click045Hits(t)
	seen := false
	for _, h := range hits {
		if strings.HasSuffix(h, " false") {
			t.Fatalf("%s #%s: event %q carried a point off the target; events %q", cmd, id, h, hits)
		}
		seen = seen || h == typ+" "+id+" true"
	}
	if !seen {
		t.Fatalf("%s #%s: no %s on the target at its point; events %q", cmd, id, typ, hits)
	}
}

var click045Cmds = []struct{ cmd, typ string }{
	{"click", "click"}, {"dblclick", "dblclick"}, {"hover", "mouseover"}, {"tap", "click"}, // WebKitGTK has no Touch constructor: tap sends no touch events there
}

// An element half outside the viewport at the bottom or right edge (center
// outside), or straddling the bottom edge (center inside), or clipped by
// its scroll container, is scrolled in and pressed at a point on it.
func TestClick045ViewportEdge(t *testing.T) {
	for _, id := range []string{"bottom", "right", "edge", "clipped"} {
		for _, c := range click045Cmds {
			t.Run(id+"/"+c.cmd, func(t *testing.T) {
				loadClick045(t, "")
				ps, _ := actionSession(t, hooks{})
				mustAct(t, ps, c.cmd, "#"+id)
				wantOnTarget(t, c.cmd, id, c.typ)
			})
		}
	}
}

// An element that scroll-driven reflow moves 2000px down after it was
// scrolled into view is scrolled in again and pressed at its settled
// point, not at the point measured before the reflow.
func TestClick045ReflowAfterScroll(t *testing.T) {
	for _, c := range click045Cmds {
		t.Run(c.cmd, func(t *testing.T) {
			loadClick045(t, "moving")
			ps, _ := actionSession(t, hooks{})
			mustAct(t, ps, c.cmd, "#moving")
			wantOnTarget(t, c.cmd, "moving", c.typ)
		})
	}
}

// A cover that appears only after the target was scrolled into view (a
// sticky header shown on scroll) makes the action fail as "covered by" the
// cover, and the target gets no event.
func TestClick045CoveredAfterScroll(t *testing.T) {
	for _, c := range click045Cmds {
		t.Run(c.cmd, func(t *testing.T) {
			loadClick045(t, "late")
			ps, _ := actionSession(t, hooks{})
			code, _, errOut := act(context.Background(), ps, "--timeout", "2s", c.cmd, "#late")
			if code == 0 || !strings.Contains(errOut, `covered by <div id="banner">`) || strings.Contains(errOut, "may have run") {
				t.Fatalf("%s on a target covered after scrolling: exit %d, stderr %q; want a failure before dispatch that names the banner", c.cmd, code, errOut)
			}
			if hits := click045Hits(t); len(hits) != 0 {
				t.Fatalf("%s on a covered target: the target got events %q", c.cmd, hits)
			}
		})
	}
}

// A checkbox under the label that activates it takes the press (the hit
// is its label, which activates it), and so does the label itself; a
// link inside the label that covers the checkbox takes the press instead,
// so the click is refused.
func TestClick045LabelActivatesControl(t *testing.T) {
	loadClick045(t, "")
	ps, _ := actionSession(t, hooks{})
	checked := func() bool {
		var v bool
		pageJSON(t, "document.getElementById('cb').checked", &v)
		return v
	}
	for i, args := range [][]string{{"click", "#cb"}, {"tap", "#cb"}, {"check", "#cb"}, {"click", "#cbl"}} {
		mustAct(t, ps, args...)
		if want := i%2 == 0; checked() != want {
			t.Fatalf("after %s: checked %v, want %v", strings.Join(args, " "), !want, want)
		}
	}
	code, _, errOut := act(context.Background(), ps, "--timeout", "1500ms", "click", "#cb2")
	if code == 0 || !strings.Contains(errOut, `covered by <a id="terms">`) || string(eval(t, "document.getElementById('cb2').checked")) != "false" {
		t.Fatalf("click on a checkbox covered by a link in its label: exit %d, stderr %q; want covered by the link and no change", code, errOut)
	}
}

// Review F2 (runtime-input): a ref four frames deep is hit-tested in its
// own document and through every enclosing frame: the click lands on it at
// a point inside it; an element of the top document that covers the frame
// chain, or a hidden enclosing frame, refuses the click.
func TestClick045NestedFrameRef(t *testing.T) {
	navigate(t, "snapshot045.html", snapshot045Ready)
	ps, _ := actionSession(t, hooks{})
	ref := refNamed(t, mustRun(t, ps, "snapshot", "-i", "-s", "#frames"), `button "Level 4"`)
	eval(t, "window.hits = []; var b = "+level4+".querySelector('button'); "+
		"b.onclick = function (e) { top.hits.push(b.ownerDocument.elementFromPoint(e.clientX, e.clientY) === b); }; "+
		"document.body.style.paddingBottom = '3000px'; scrollTo(0, 1e6); true") // the frame chain starts above the viewport
	mustAct(t, ps, "click", "@"+ref)
	if got := pageString(t, "JSON.stringify(window.hits)"); got != "[true]" {
		t.Fatalf("click @%s (iframe depth 4): clicks with their point on the button %s, want [true]", ref, got)
	}
	eval(t, "var c = document.createElement('div'); c.id = 'cover'; c.style.cssText = 'position:fixed;left:0;top:0;width:100%;height:100%;z-index:1000'; document.body.appendChild(c); true")
	code, _, errOut := act(context.Background(), ps, "--timeout", "1500ms", "click", "@"+ref)
	if code == 0 || !strings.Contains(errOut, `covered by <div id="cover">`) || pageString(t, "JSON.stringify(window.hits)") != "[true]" {
		t.Fatalf("click @%s under a top-document cover: exit %d, err %q, hits %s; want covered by the cover and no click", ref, code, errOut, pageString(t, "JSON.stringify(window.hits)"))
	}
	eval(t, "document.getElementById('cover').remove(); document.getElementById('nest').contentDocument.querySelector('iframe').style.visibility = 'hidden'; true")
	code, _, errOut = act(context.Background(), ps, "--timeout", "1500ms", "click", "@"+ref)
	if code == 0 || pageString(t, "JSON.stringify(window.hits)") != "[true]" {
		t.Fatalf("click @%s inside a hidden frame: exit %d, err %q; want refused", ref, code, errOut)
	}
}

// Review F3 (runtime-input): inside a label that activates the target, a
// covering descendant that is interactive by agent-browser 0.45.0's
// BLOCKER_AT_JS (element.rs) takes the press, so the click is refused and
// the checkbox stays unchanged: a property-assigned onclick, details,
// embed, object, contenteditable, tabindex 0. tabindex -1 is not
// interactive there: the label still activates the checkbox.
func TestClick045LabelInteractiveDescendants(t *testing.T) {
	loadClick045(t, "")
	ps, _ := actionSession(t, hooks{})
	cover := func(js string) {
		eval(t, "var o = document.getElementById('cover'); if (o) { o.remove(); } document.getElementById('cb').checked = false; "+
			"var s = "+js+"; s.id = 'cover'; s.style.cssText = 'position:absolute;left:0;top:0;width:40px;height:24px;display:block;margin:0;padding:0'; "+
			"document.getElementById('cbl').appendChild(s); true")
	}
	for _, c := range []struct{ name, js string }{
		{"onclick property", "document.createElement('span'); s.onclick = function () {}"},
		{"details", "document.createElement('details')"},
		{"embed", "document.createElement('embed'); s.src = 'data:text/html,x'"}, // without a source an embed has no box
		{"object", "document.createElement('object')"},
		{"contenteditable", "document.createElement('span'); s.contentEditable = 'true'"},
		{"tabindex 0", "document.createElement('span'); s.tabIndex = 0"},
	} {
		cover(c.js)
		code, _, errOut := act(context.Background(), ps, "--timeout", "1s", "click", "#cb")
		if code == 0 || !strings.Contains(errOut, "covered by <") || string(eval(t, "document.getElementById('cb').checked")) != "false" {
			t.Fatalf("checkbox covered by a %s inside its label: exit %d, err %q, checked %s; want refused and unchanged", c.name, code, errOut, eval(t, "document.getElementById('cb').checked"))
		}
	}
	cover("document.createElement('span'); s.tabIndex = -1")
	mustAct(t, ps, "click", "#cb")
	if string(eval(t, "document.getElementById('cb').checked")) != "true" {
		t.Fatalf("checkbox under a tabindex -1 span inside its label was not checked")
	}
}
