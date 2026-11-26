package commands

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/devtools/cssrules"
)

// cssStyleJSON builds a WIP CSSStyle with source ranges: each "name:value"
// is a written declaration, each "+name:value" a longhand WebKit derived
// for the declaration before it (no range), and a "!" suffix on the value
// makes it !important.
func cssStyleJSON(decls ...string) wipCSSStyle {
	s := wipCSSStyle{Range: json.RawMessage(`{"startLine":0}`)}
	for _, d := range decls {
		derived := strings.HasPrefix(d, "+")
		name, value, _ := strings.Cut(strings.TrimPrefix(d, "+"), ":")
		p := wipCSSProperty{Name: name, Value: value, Status: "active"}
		if strings.HasSuffix(value, "!") {
			p.Value, p.Priority = strings.TrimSuffix(value, "!"), "important"
		}
		if !derived {
			p.Range = json.RawMessage(`{"startLine":0}`)
		} else {
			p.Status = ""
		}
		s.CSSProperties = append(s.CSSProperties, p)
	}
	return s
}

func ruleMatch(selector, origin string, line int, style wipCSSStyle, groupings ...wipCSSGrouping) wipRuleMatch {
	var m wipRuleMatch
	m.Rule.SelectorList.Text = selector
	m.Rule.SelectorList.Selectors = append(m.Rule.SelectorList.Selectors, cssrules.Selector{Text: selector})
	m.Rule.Origin, m.Rule.SourceLine, m.Rule.Style, m.Rule.Groupings = origin, line, style, groupings
	m.Rule.SourceURL = "https://example.test/css/site.css"
	return m
}

// statuses returns "selector name status" for each declaration shown.
func statuses(rep cssMatchedReport) []string {
	var out []string
	for _, r := range rep.Rules {
		for _, p := range r.Properties {
			out = append(out, r.Type+" "+r.Selector+" "+p.Name+" "+p.Status)
		}
	}
	return out
}

// TestCSSCascadeStatuses: the cascade marks the declarations that lose as
// [overloaded] as DevTools' CSSMatchedStyles does: a later or more
// specific rule wins, !important beats a normal declaration of a higher
// rule, a user agent !important beats an author !important, an inline
// style beats rules, a shorthand stays active while one longhand wins and
// is overloaded when none does, an ancestor's style counts only for
// inherited properties and loses to the element's own.
func TestCSSCascadeStatuses(t *testing.T) {
	m := &wipMatchedStyles{MatchedCSSRules: []wipRuleMatch{ // lowest precedence first, as WebKit sends them
		ruleMatch("p", "user-agent", 0, cssStyleJSON("display:block", "color:black!")),
		ruleMatch("p", "author", 1, cssStyleJSON("margin:0", "+margin-top:0px", "+margin-left:0px", "padding:1px", "+padding-top:1px", "+padding-left:1px")),
		ruleMatch(".a", "author", 2, cssStyleJSON("color:blue!", "width:1px")),
		ruleMatch("#t", "author", 3, cssStyleJSON("color:red", "margin-top:2px", "margin-left:2px", "padding-top:3px", "width:2px")),
	}}
	m.Inherited = append(m.Inherited, struct {
		InlineStyle     *wipCSSStyle   `json:"inlineStyle"`
		MatchedCSSRules []wipRuleMatch `json:"matchedCSSRules"`
	}{MatchedCSSRules: []wipRuleMatch{ruleMatch("body", "author", 4, cssStyleJSON("font-size:9px", "color:green", "margin:5px"))}})
	inline := cssStyleJSON("width:3px")
	rep := buildCSSMatched("#t", &cssElement{chain: []string{"p#t", "body"}}, m, &wipInlineStyles{InlineStyle: &inline}, &wipComputedStyle{}, nil)
	want := []string{
		"inline element.style width active",
		"matched #t color overloaded",
		"matched #t margin-top active",
		"matched #t margin-left active",
		"matched #t padding-top active",
		"matched #t width overloaded",
		"matched .a color overloaded",
		"matched .a width overloaded",
		"matched p margin overloaded",
		"matched p padding active",
		"matched p display active",
		"matched p color active",
		"inherited body font-size active",
		"inherited body color overloaded",
	}
	if got := statuses(rep); !reflect.DeepEqual(got, want) {
		t.Errorf("statuses:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestCSSCascadeLayers: between two author !important declarations of
// different @layer blocks, the one already active (the earlier layer, as
// the cascade orders important layers in reverse) wins.
func TestCSSCascadeLayers(t *testing.T) {
	m := &wipMatchedStyles{MatchedCSSRules: []wipRuleMatch{
		ruleMatch(".a", "author", 1, cssStyleJSON("color:blue!"), wipCSSGrouping{Type: "layer-rule", Text: "base"}),
		ruleMatch(".b", "author", 2, cssStyleJSON("color:red!"), wipCSSGrouping{Type: "layer-rule", Text: "top"}),
	}}
	rep := buildCSSMatched("x", &cssElement{chain: []string{"p"}}, m, &wipInlineStyles{}, &wipComputedStyle{}, nil)
	want := []string{"matched .b color overloaded", "matched .a color active"}
	if got := statuses(rep); !reflect.DeepEqual(got, want) {
		t.Errorf("statuses: %q, want %q", got, want)
	}
}

// TestCSSStatusesFromWebKit: a declaration WebKit could not parse is
// [invalid], a disabled one [disabled], and one that a later declaration
// of the same rule overrides (WebKit status inactive) [overloaded].
func TestCSSStatusesFromWebKit(t *testing.T) {
	st := cssStyleJSON("color:red", "width:1px", "height:1px", "color:blue")
	no := false
	st.CSSProperties[0].Status = "inactive"
	st.CSSProperties[1].ParsedOk = &no
	st.CSSProperties[2].Status = "disabled"
	m := &wipMatchedStyles{MatchedCSSRules: []wipRuleMatch{ruleMatch(".a", "author", 0, st)}}
	rep := buildCSSMatched("x", &cssElement{chain: []string{"p"}}, m, &wipInlineStyles{}, &wipComputedStyle{}, nil)
	want := []string{"matched .a color overloaded", "matched .a width invalid", "matched .a height disabled", "matched .a color active"}
	if got := statuses(rep); !reflect.DeepEqual(got, want) {
		t.Errorf("statuses: %q, want %q", got, want)
	}
}

// TestResolveVars: var() takes the element's computed custom property,
// a missing one its fallback (which may hold var() itself), and a missing
// one without a usable fallback leaves the value unresolved.
func TestResolveVars(t *testing.T) {
	vars := map[string]string{"--a": "red", "--b": "1px"}
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"var(--a)", "red", true},
		{"var(--b) solid var(--a)", "1px solid red", true},
		{"var(--x, var(--a))", "red", true},
		{"calc(var(--b) * 2)", "calc(1px * 2)", true},
		{"var(--x, rgb(0, 0, 255))", "rgb(0, 0, 255)", true},
		{"var(--x)", "", false},
		{"var(--x, var(--y))", "", false},
		{"var(--a", "", false},
	} {
		got, ok := resolveVars(c.in, vars, 0)
		if got != c.want || ok != c.ok {
			t.Errorf("resolveVars(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestCSSSource: sources read as chrome-devtools-mcp writes them: the
// file name and 1-based line, "index" for a root path, special origins by
// name; a rule of the document's own <style> elements is "<style>:line".
func TestCSSSource(t *testing.T) {
	doc := "https://example.test/page.html"
	for _, c := range []struct {
		url, origin, want string
	}{
		{"https://example.test/css/site.css?v=2", "author", "site.css:5"},
		{"https://example.test/", "author", "index:5"},
		{"data:text/css,p{}", "author", "data-uri:5"},
		{doc, "author", "<style>:5"},
		{"", "author", "<style>:5"},
		{"", "user-agent", "user agent stylesheet"},
		{"", "user", "injected stylesheet"},
		{"", "inspector", "via inspector"},
	} {
		got, _, _ := cssSource(&wipCSSRule{SourceURL: c.url, SourceLine: 4, Origin: c.origin}, doc)
		if got != c.want {
			t.Errorf("cssSource(%q, %s) = %q, want %q", c.url, c.origin, got, c.want)
		}
	}
}

// TestCSSAncestorsOutermostFirst: WebKit lists groupings innermost first;
// the output nests them outermost first.
func TestCSSAncestorsOutermostFirst(t *testing.T) {
	got := cssAncestors([]wipCSSGrouping{
		{Type: "media-rule", Text: "(min-width: 1px)"},
		{Type: "supports-rule", Text: "(display: grid)"},
		{Type: "layer-rule", Text: "base"},
	})
	want := []cssAncestor{{Type: "layer", Name: "base"}, {Type: "supports", Query: "(display: grid)"}, {Type: "media", Query: "(min-width: 1px)"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ancestors %+v, want %+v", got, want)
	}
}

// TestPaginateCSS: pages hold page-size entries, an index past the last
// page shows the first one and says so, and the text gives
// chrome-devtools-mcp's "Showing" and next/previous lines.
func TestPaginateCSS(t *testing.T) {
	mk := func(n int) *cssMatchedReport {
		rep := &cssMatchedReport{}
		for range n {
			rep.Rules = append(rep.Rules, cssRuleOut{Type: "inline", Selector: "element.style", Properties: []cssProperty{{Name: "a", Value: "b", Status: "active"}}})
		}
		return rep
	}
	rep := mk(7)
	p := paginateCSS(rep, 3, 1)
	if p.StartIndex != 3 || p.EndIndex != 6 || p.TotalPages != 3 || !p.HasNextPage || !p.HasPreviousPage || len(rep.Rules) != 3 || rep.Total != 7 {
		t.Errorf("page 1 of 7 by 3: %+v, %d rules", p, len(rep.Rules))
	}
	if txt := formatCSSMatched(rep, p); !strings.HasPrefix(txt, "Showing 4-6 of 7 (Page 2 of 3).\nNext page: 2\nPrevious page: 0\n") {
		t.Errorf("text:\n%s", txt)
	}
	rep = mk(5)
	p = paginateCSS(rep, 2, 4)
	if !p.InvalidPage || p.CurrentPage != 0 || p.StartIndex != 0 || len(rep.Rules) != 2 {
		t.Errorf("index past the end: %+v", p)
	}
	if txt := formatCSSMatched(rep, p); !strings.HasPrefix(txt, "Invalid page number provided. Showing first page.\nShowing 1-2 of 5 (Page 1 of 3).\nNext page: 1\nStyles for") {
		t.Errorf("text:\n%s", txt)
	}
	rep = mk(0)
	p = paginateCSS(rep, 10, 0)
	if txt := formatCSSMatched(rep, p); !strings.HasPrefix(txt, "Showing 0-0 of 0 (Page 1 of 1).") || !strings.HasSuffix(txt, "(no styles)") {
		t.Errorf("no entries:\n%s", txt)
	}
}

// TestPropertyLine: the status tag, !important and the resolved value
// comment (with "*/" escaped) as chrome-devtools-mcp formatPropertyLine.
func TestPropertyLine(t *testing.T) {
	got := propertyLine(cssProperty{Name: "content", Value: "var(--c)", ResolvedValue: "\"*/\"", Status: "overloaded", Important: true})
	if want := `[overloaded] content: var(--c) !important; /* "*\/" */`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestCSSMatchedChecksPaging: a page size below 1 (chrome-devtools-mcp's
// pageSize is a positive integer) or a negative page index is a usage
// error before any connection.
func TestCSSMatchedChecksPaging(t *testing.T) {
	for _, flags := range []map[string]any{{"page-size": -1}, {"page-size": 0}, {"page-index": -2}} {
		if err := checkCSSMatched(Input{Flags: flags}); err == nil {
			t.Errorf("%v: no usage error", flags)
		}
	}
	if err := checkCSSMatched(Input{Flags: map[string]any{"page-size": 5, "page-index": 2}}); err != nil {
		t.Error(err)
	}
}
