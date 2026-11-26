//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestCSSNodeMethodsAnswerWithoutEnable is the WebKitGTK half of WP-CSS
// step 1: CSS.getMatchedStylesForNode, CSS.getInlineStylesForNode and
// CSS.getComputedStyleForNode answer on a node from DOM.requestNode
// without CSS.enable, each within a short deadline. It logs the replies
// and timings (go test -v) for the notes.
func TestCSSNodeMethodsAnswerWithoutEnable(t *testing.T) {
	navigate(t, "css.html", "")
	send := func(method string, params map[string]any) json.RawMessage {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		raw, err := sess.Send(ctx, method, params)
		if err != nil {
			t.Fatalf("%s: %v (after %s)", method, err, time.Since(start))
		}
		t.Logf("%s answered in %s: %.6000s", method, time.Since(start), raw)
		return raw
	}
	var ev struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(send("Runtime.evaluate", map[string]any{"expression": "document.getElementById('target')"}), &ev); err != nil || ev.Result.ObjectID == "" {
		t.Fatalf("no object id for #target: %v", err)
	}
	send("DOM.getDocument", map[string]any{"depth": 0})
	var n struct {
		NodeID int `json:"nodeId"`
	}
	if err := json.Unmarshal(send("DOM.requestNode", map[string]any{"objectId": ev.Result.ObjectID}), &n); err != nil || n.NodeID == 0 {
		t.Fatalf("DOM.requestNode gave no node id: %v", err)
	}
	send("CSS.getMatchedStylesForNode", map[string]any{"nodeId": n.NodeID, "includePseudo": true, "includeInherited": true})
	send("CSS.getInlineStylesForNode", map[string]any{"nodeId": n.NodeID})
	send("CSS.getComputedStyleForNode", map[string]any{"nodeId": n.NodeID})
}

// cssMatchedCascade is the part of "css matched #target" after the
// Computed styles entry on css.html, written from the CSS cascade of the
// fixture: inline style first, then the rules from the highest
// specificity and the latest down, the user agent rule last; .box's color
// loses to #target, its padding to the later @media rule; #target's
// text-decoration loses to .box's !important one; the ancestors' styles
// show only inherited properties (body's margin is left out) and body's
// color loses to the element's own; var() values are resolved with the
// element's custom properties, a missing one with its fallback. Lines of
// <style> rules count from the start of the style element's text.
const cssMatchedCascade = `  element.style {
    font-weight: 700;
  }

  #target.box (<style>:7) {
    margin-top: 4px;
  }

  @media (min-width: 1px) {
    #target (<style>:6) {
      padding: 3px;
    }
  }

  #target (<style>:5) {
    color: var(--brand); /* rgb(255, 0, 0) */
    border: 2px solid var(--missing, rgb(0, 0, 255)); /* 2px solid rgb(0, 0, 255) */
    [overloaded] text-decoration: none;
  }

  .box (<style>:4) {
    [overloaded] color: blue;
    [overloaded] padding: 1px;
    text-decoration: underline !important;
  }

  p (<style>:8) {
    margin: 0;
  }

  p (user agent stylesheet) {
    display: block;
    [overloaded] margin-block-start: 1em;
    [overloaded] margin-inline-start: 0px;
  }

  Inherited from div#wrap:
    element.style {
      letter-spacing: 1px;
    }

  Inherited from body:
    body (<style>:3) {
      font-size: 20px;
      [overloaded] color: green;
    }

  Inherited from html:
    :root (<style>:2) {
      --brand: rgb(255, 0, 0);
    }

  Inherited from html:
    html (user agent stylesheet) {
      [overloaded] color: canvastext;
    }`

// TestCSSMatched: css matched prints the cascade of the element as
// chrome-devtools-mcp's get_css_styles does (overridden declarations
// marked [overloaded], var() resolved, @media wrappers, inline and
// inherited styles), after a Computed styles entry with the computed
// values of the declared properties and the --computed extras.
func TestCSSMatched(t *testing.T) {
	navigate(t, "css.html", "")
	ps := pageSession("css045", "s1", 1)
	out := mustRun(t, ps, "css", "matched", "#target", "--page-size", "30", "--computed", "z-index")
	head, cascade, ok := strings.Cut(out, "\n\n  element.style {")
	if !ok {
		t.Fatalf("no element.style entry in:\n%s", out)
	}
	if got := "  element.style {" + cascade; got != cssMatchedCascade {
		t.Errorf("cascade:\n%s\nwant:\n%s", got, cssMatchedCascade)
	}
	if !strings.HasPrefix(head, "Showing 1-12 of 12 (Page 1 of 1).\nStyles for p#target (target: \"#target\"):\n\n  Computed styles:\n") {
		t.Errorf("header:\n%s", head)
	}
	for _, line := range []string{"color: rgb(255, 0, 0);", "display: block;", "letter-spacing: 1px;", "font-size: 20px;", "padding-top: 3px;", "margin-top: 4px;", "--brand: rgb(255, 0, 0);", "z-index: auto;"} {
		if !strings.Contains(head, "\n    "+line+"\n") {
			t.Errorf("Computed styles lack %q:\n%s", line, head)
		}
	}
	if strings.Contains(head, "z-index: auto;") && strings.Contains(mustRun(t, ps, "css", "matched", "#target", "--page-size", "30"), "z-index") {
		t.Error("z-index is in Computed styles without --computed z-index")
	}
}

// TestCSSMatchedPages: --page-size and --page-index page through the
// entries (the Computed styles entry counts as one), with
// chrome-devtools-mcp's pagination lines; an index past the end shows the
// first page and says so.
func TestCSSMatchedPages(t *testing.T) {
	navigate(t, "css.html", "")
	ps := pageSession("css045", "s1", 1)
	out := mustRun(t, ps, "css", "matched", "#target", "--page-size", "5", "--page-index", "1")
	want := "Showing 6-10 of 12 (Page 2 of 3).\nNext page: 2\nPrevious page: 0\nStyles for p#target (target: \"#target\"):\n\n  .box (<style>:4) {"
	if !strings.HasPrefix(out, want) {
		t.Errorf("page 2:\n%s\nwant prefix:\n%s", out, want)
	}
	var rep struct {
		Rules []struct {
			Type     string `json:"type"`
			Selector string `json:"selector"`
		} `json:"rules"`
		Total      int `json:"total"`
		Pagination struct {
			InvalidPage bool `json:"invalidPage"`
			CurrentPage int  `json:"currentPage"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, ps, "--json", "css", "matched", "#target", "--page-size", "5", "--page-index", "9")), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Pagination.InvalidPage || rep.Pagination.CurrentPage != 0 || rep.Total != 12 || len(rep.Rules) != 5 || rep.Rules[0].Type != "computed" {
		t.Errorf("index past the end: %+v", rep)
	}
}

// TestCSSComputed: css computed prints the CSS domain's computed values
// in WebKit's order; a shorthand name also gives each property whose name
// starts with it and "-", a custom property its value, color only itself
// (not color-scheme), and a name that matches nothing fails.
func TestCSSComputed(t *testing.T) {
	navigate(t, "css.html", "")
	ps := pageSession("css045", "s1", 1)
	out := mustRun(t, ps, "css", "computed", "#target", "--", "color", "padding", "--brand")
	want := "color: rgb(255, 0, 0)\npadding-block-end: 3px\npadding-block-start: 3px\npadding-bottom: 3px\npadding-inline-end: 3px\npadding-inline-start: 3px\npadding-left: 3px\npadding-right: 3px\npadding-top: 3px\n--brand: rgb(255, 0, 0)\npadding: 3px\npadding-block: 3px\npadding-inline: 3px"
	if out != want {
		t.Errorf("css computed:\n%s\nwant:\n%s", out, want)
	}
	if code, _, errOut := run(t, ps, "css", "computed", "#target", "no-such-property"); code != 1 || !strings.Contains(errOut, "no-such-property") {
		t.Errorf("unknown property: exit %d: %s", code, errOut)
	}
}
