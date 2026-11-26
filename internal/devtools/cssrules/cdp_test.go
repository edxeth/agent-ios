package cssrules

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// The translation of WIP's CSS node replies to Chrome's CSS domain shapes
// (MatchedToCDP, InlineToCDP), which the CDP endpoint answers to
// chrome-devtools-mcp's get_css_styles (DevTools CSSModel). Expected
// values come from the CDP definitions of CSS.CSSRule, CSSStyle,
// CSSProperty and RuleMatch (Chrome's protocol, as DevTools 1.14.0
// registers it) and from the recorded replies themselves, not from the
// translation's output.

// recorded are WIP replies of CSS.getMatchedStylesForNode (includePseudo,
// includeInherited) and CSS.getInlineStylesForNode for p#target of the
// same page (e2e/fixtures/runtime/css.html): from Safari on the iOS 26.5
// simulator (WP-CSS evidence ios-26.5-sim-replies.json; that probe page
// lacks the text-decoration declarations) and from WebKitGTK 2.54.3
// (TestCSSNodeMethodsAnswerWithoutEnable, 2026-11-26).
var recorded = []struct {
	file, sheet, url string
	textDecoration   bool // the page has .box's and #target's text-decoration
}{
	{"testdata/ios-26.5-sim.json", "7", "http://10.0.2.2:18745/wpcss-probe.html", false},
	{"testdata/webkitgtk-2.54.json", "2", "http://127.0.0.1:42623/css.html", true},
}

// Chrome's shapes, as far as the tests read them.
type (
	cdpRange struct {
		StartLine   int `json:"startLine"`
		StartColumn int `json:"startColumn"`
		EndLine     int `json:"endLine"`
		EndColumn   int `json:"endColumn"`
	}
	cdpProp struct {
		Name               string    `json:"name"`
		Value              string    `json:"value"`
		Important          bool      `json:"important"`
		Implicit           bool      `json:"implicit"`
		Disabled           bool      `json:"disabled"`
		ParsedOk           *bool     `json:"parsedOk"`
		Text               string    `json:"text"`
		Range              *cdpRange `json:"range"`
		LonghandProperties []cdpProp `json:"longhandProperties"`
	}
	cdpStyleOut struct {
		StyleSheetID     *string `json:"styleSheetId"`
		CSSProperties    []cdpProp
		ShorthandEntries []struct {
			Name      string `json:"name"`
			Value     string `json:"value"`
			Important bool   `json:"important"`
		}
		CSSText string    `json:"cssText"`
		Range   *cdpRange `json:"range"`
	}
	cdpRuleOut struct {
		StyleSheetID *string `json:"styleSheetId"`
		Origin       string  `json:"origin"`
		SelectorList struct {
			Text      string
			Selectors []struct {
				Text        string
				Specificity *struct{ A, B, C int }
				Range       *cdpRange
			}
		}
		Style            cdpStyleOut
		RuleTypes        []string `json:"ruleTypes"`
		Media            []map[string]any
		Supports         []map[string]any
		Layers           []map[string]any
		ContainerQueries []map[string]any
		Scopes           []map[string]any
		StartingStyles   []map[string]any
		NestingSelectors []string
	}
	cdpMatch struct {
		Rule              cdpRuleOut
		MatchingSelectors []int
	}
	cdpMatched struct {
		InlineStyle     *cdpStyleOut
		AttributesStyle *cdpStyleOut
		MatchedCSSRules []cdpMatch
		PseudoElements  []struct {
			PseudoType string
			Matches    []cdpMatch
		}
		Inherited []struct {
			InlineStyle     *cdpStyleOut
			MatchedCSSRules []cdpMatch
		}
	}
)

// translate runs MatchedToCDP and reads its result through JSON, as the
// endpoint sends it.
func translate(t *testing.T, m *Matched, in *Inline) (cdpMatched, map[string]any, []Sheet) {
	t.Helper()
	out, sheets := MatchedToCDP(m, in)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var typed cdpMatched
	var generic map[string]any
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	_ = json.Unmarshal(raw, &generic)
	return typed, generic, sheets
}

func loadRecorded(t *testing.T, file string) (*Matched, *Inline) {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Matched Matched `json:"CSS.getMatchedStylesForNode"`
		Inline  Inline  `json:"CSS.getInlineStylesForNode"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return &r.Matched, &r.Inline
}

func prop(s cdpStyleOut, name string) (cdpProp, bool) {
	for _, p := range s.CSSProperties {
		if p.Name == name {
			return p, true
		}
	}
	return cdpProp{}, false
}

func names(ps []cdpProp) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

// Recorded WIP replies of iOS 26.5 and WebKitGTK translate to Chrome's
// shapes: origins (author -> regular), one style sheet per author rule
// with the sheet named on rule and style, ranges passed on, the first
// selector's range (DevTools' line of a rule), every declaration kept in
// order with its importance stripped from the value, a shorthand's
// longhandProperties holding only its own longhands, @media as ruleTypes
// and media, one inherited entry per ancestor with its inline style, and
// the style sheets the reply names.
func TestMatchedToCDPRecordedReplies(t *testing.T) {
	for _, rec := range recorded {
		t.Run(rec.file, func(t *testing.T) {
			m, in := loadRecorded(t, rec.file)
			got, generic, sheets := translate(t, m, in)

			if want := []Sheet{{ID: rec.sheet, SourceURL: rec.url, Origin: "regular"}}; !reflect.DeepEqual(sheets, want) {
				t.Errorf("sheets %+v, want %+v", sheets, want)
			}
			var sels, origins []string
			for _, r := range got.MatchedCSSRules {
				sels = append(sels, r.Rule.SelectorList.Text)
				origins = append(origins, r.Rule.Origin)
				if !slices.Equal(r.MatchingSelectors, []int{0}) {
					t.Errorf("%s matchingSelectors %v, want [0]", r.Rule.SelectorList.Text, r.MatchingSelectors)
				}
			}
			if want := []string{"p", "p", ".box", "#target", "#target", "#target.box"}; !slices.Equal(sels, want) {
				t.Fatalf("rules %v, want %v (WIP's order, lowest precedence first)", sels, want)
			}
			if want := []string{"user-agent", "regular", "regular", "regular", "regular", "regular"}; !slices.Equal(origins, want) {
				t.Errorf("origins %v, want %v", origins, want)
			}
			// Every declaration is kept, in WIP's order.
			for i, r := range got.MatchedCSSRules {
				var wipNames []string
				for _, p := range m.MatchedCSSRules[i].Rule.Style.CSSProperties {
					wipNames = append(wipNames, p.Name)
				}
				if !slices.Equal(names(r.Rule.Style.CSSProperties), wipNames) {
					t.Errorf("rule %d properties %v, want WIP's %v", i, names(r.Rule.Style.CSSProperties), wipNames)
				}
			}

			ua := got.MatchedCSSRules[0].Rule
			if ua.StyleSheetID != nil || ua.Style.StyleSheetID != nil || ua.SelectorList.Selectors[0].Range != nil {
				t.Errorf("user agent rule names a sheet or a range: %+v", ua)
			}
			if sp := ua.SelectorList.Selectors[0].Specificity; sp == nil || *sp != (struct{ A, B, C int }{0, 0, 1}) {
				t.Errorf("p specificity %+v, want {0 0 1}", sp)
			}
			if p, _ := prop(ua.Style, "margin-block-end"); !p.Implicit {
				t.Errorf("UA margin-block-end %+v, want implicit", p)
			}
			if len(ua.Style.ShorthandEntries) != 2 || ua.Style.ShorthandEntries[0].Name != "margin-block" || ua.Style.ShorthandEntries[0].Value != "1em" {
				t.Errorf("UA shorthandEntries %+v", ua.Style.ShorthandEntries)
			}

			p := got.MatchedCSSRules[1].Rule
			if p.StyleSheetID == nil || *p.StyleSheetID != rec.sheet || p.Style.StyleSheetID == nil || *p.Style.StyleSheetID != rec.sheet {
				t.Errorf("author p sheet %v / style %v, want %s", p.StyleSheetID, p.Style.StyleSheetID, rec.sheet)
			}
			if r := p.SelectorList.Selectors[0].Range; r == nil || *r != (cdpRange{7, 0, 7, 1}) {
				t.Errorf("author p selector range %+v, want line 7 columns 0-1", r)
			}
			if p.Style.Range == nil || *p.Style.Range != (cdpRange{7, 3, 7, 15}) || p.Style.CSSText != " margin: 0; " {
				t.Errorf("author p style range %+v text %q", p.Style.Range, p.Style.CSSText)
			}
			margin, _ := prop(p.Style, "margin")
			if margin.Value != "0" || margin.Text != "margin: 0;" || margin.Range == nil || *margin.Range != (cdpRange{7, 4, 7, 14}) || margin.Important || margin.Disabled || margin.Implicit {
				t.Errorf("author p margin %+v", margin)
			}
			// margin is the style's only shorthand: the four longhands WIP
			// lists after the declarations are its own.
			if lh := names(margin.LonghandProperties); !slices.Equal(lh, []string{"margin-top", "margin-right", "margin-bottom", "margin-left"}) {
				t.Errorf("margin longhandProperties %v, want its four longhands", lh)
			} else if margin.LonghandProperties[0].Value != "0px" {
				t.Errorf("margin-top longhand %+v, want 0px", margin.LonghandProperties[0])
			}

			media := got.MatchedCSSRules[4].Rule
			if !slices.Equal(media.RuleTypes, []string{"MediaRule"}) || len(media.Media) != 1 || media.Media[0]["text"] != "(min-width: 1px)" || media.Media[0]["source"] != "mediaRule" {
				t.Errorf("@media rule ruleTypes %v media %v", media.RuleTypes, media.Media)
			}
			if r := media.SelectorList.Selectors[0].Range; r == nil || *r != (cdpRange{5, 26, 5, 33}) {
				t.Errorf("@media #target selector range %+v", r)
			}
			if plain := got.MatchedCSSRules[3].Rule; plain.RuleTypes != nil || plain.Media != nil {
				t.Errorf("#target outside @media has ruleTypes %v media %v", plain.RuleTypes, plain.Media)
			}

			// A longhand belongs to its own shorthand only (CSS.CSSProperty
			// longhandProperties: "Parsed longhand components of this
			// property"): WIP lists the longhands of all shorthands after
			// all declarations.
			own := map[string]string{"margin": "margin-", "padding": "padding-", "border": "border-", "text-decoration": "text-decoration-"}
			check := func(where string, s cdpStyleOut) {
				for _, pr := range s.CSSProperties {
					for _, l := range pr.LonghandProperties {
						if pre, ok := own[pr.Name]; !ok || !strings.HasPrefix(l.Name, pre) {
							t.Errorf("%s: %s lists %s as its longhand", where, pr.Name, l.Name)
						}
					}
				}
			}
			for _, r := range got.MatchedCSSRules {
				check(r.Rule.SelectorList.Text, r.Rule.Style)
			}
			if rec.textDecoration {
				box := got.MatchedCSSRules[2].Rule
				td, _ := prop(box.Style, "text-decoration")
				if td.Value != "underline" || !td.Important {
					t.Errorf(".box text-decoration %+v, want underline, important", td)
				}
				if tdl, _ := prop(box.Style, "text-decoration-line"); !tdl.Important || tdl.Value != "underline" {
					t.Errorf(".box text-decoration-line %+v, want important underline", tdl)
				}
			}

			// One inherited entry per ancestor: div#wrap, body, html.
			if len(got.Inherited) != 3 {
				t.Fatalf("%d inherited entries, want 3", len(got.Inherited))
			}
			div := got.Inherited[0]
			if div.InlineStyle == nil || div.InlineStyle.StyleSheetID != nil {
				t.Fatalf("div inline style %+v, want one without a sheet", div.InlineStyle)
			}
			if ls, _ := prop(*div.InlineStyle, "letter-spacing"); ls.Value != "1px" {
				t.Errorf("div letter-spacing %+v", ls)
			}
			if r := div.MatchedCSSRules; len(r) != 1 || r[0].Rule.Origin != "user-agent" || !slices.Equal(r[0].MatchingSelectors, []int{3}) {
				t.Errorf("div rules %+v, want the UA block rule matching its selector 3", r)
			}
			var bodySel, htmlSel []string
			for _, r := range got.Inherited[1].MatchedCSSRules {
				bodySel = append(bodySel, r.Rule.SelectorList.Text+"/"+r.Rule.Origin)
			}
			for _, r := range got.Inherited[2].MatchedCSSRules {
				htmlSel = append(htmlSel, r.Rule.SelectorList.Text+"/"+r.Rule.Origin)
			}
			if !slices.Equal(bodySel, []string{"body/user-agent", "body/regular"}) || !slices.Equal(htmlSel, []string{"html/user-agent", ":root/user-agent", ":root/regular"}) {
				t.Errorf("inherited rules body %v html %v", bodySel, htmlSel)
			}
			if got.Inherited[2].InlineStyle != nil {
				t.Errorf("html has an inline style %+v", got.Inherited[2].InlineStyle)
			}
			if pe, ok := generic["pseudoElements"].([]any); !ok || len(pe) != 0 {
				t.Errorf("pseudoElements %v, want an empty list", generic["pseudoElements"])
			}

			if got.InlineStyle == nil {
				t.Fatal("no inlineStyle")
			}
			if fw, _ := prop(*got.InlineStyle, "font-weight"); fw.Value != "700" || fw.Range == nil {
				t.Errorf("inline font-weight %+v", fw)
			}
			if _, ok := generic["attributesStyle"]; ok {
				t.Errorf("attributesStyle %v for an element without presentational attributes", generic["attributesStyle"])
			}
		})
	}
}

// CDP StyleSheetOrigin: WIP's author is Chrome's regular, user is
// injected; user-agent and inspector keep their names.
func TestCDPOrigin(t *testing.T) {
	for wip, want := range map[string]string{"author": "regular", "user": "injected", "user-agent": "user-agent", "inspector": "inspector"} {
		if got := CDPOrigin(wip); got != want {
			t.Errorf("CDPOrigin(%q) = %q, want %q", wip, got, want)
		}
	}
}

// WIP groupings become Chrome's enclosing rules, innermost first, with
// ruleTypes in the same order; a media grouping without text (a <style
// media> without the attribute) and unknown kinds add nothing.
func TestGroupingsToCDP(t *testing.T) {
	m := &Matched{MatchedCSSRules: []RuleMatch{{
		MatchingSelectors: []int{0},
		Rule: Rule{
			Origin: "author", RuleID: &StyleID{StyleSheetID: "s1"}, SourceURL: "https://a.example/a.css", SourceLine: 9,
			SelectorList: SelectorList{Selectors: []Selector{{Text: ".a"}, {Text: ".b"}}, Text: ".a, .b"},
			Groupings: []Grouping{
				{Type: "media-rule", Text: "(width > 1px)"},
				{Type: "supports-rule", Text: "(display: grid)"},
				{Type: "layer-rule", Text: "base"},
				{Type: "container-rule", Text: "(width > 2px)"},
				{Type: "scope-rule", Text: "(.root)"},
				{Type: "starting-style-rule"},
				{Type: "style-rule", Text: ".parent"},
				{Type: "media-style-node"},
				{Type: "font-feature-values-rule", Text: "x"},
			},
		},
	}}}
	got, _, sheets := translate(t, m, &Inline{})
	r := got.MatchedCSSRules[0].Rule
	if want := []string{"MediaRule", "SupportsRule", "LayerRule", "ContainerRule", "ScopeRule", "StartingStyleRule", "StyleRule"}; !slices.Equal(r.RuleTypes, want) {
		t.Errorf("ruleTypes %v, want %v", r.RuleTypes, want)
	}
	if len(r.Media) != 1 || r.Media[0]["text"] != "(width > 1px)" || r.Media[0]["source"] != "mediaRule" {
		t.Errorf("media %v", r.Media)
	}
	if len(r.Supports) != 1 || r.Supports[0]["text"] != "(display: grid)" || r.Supports[0]["active"] != true {
		t.Errorf("supports %v", r.Supports)
	}
	if len(r.Layers) != 1 || r.Layers[0]["text"] != "base" || len(r.ContainerQueries) != 1 || r.ContainerQueries[0]["text"] != "(width > 2px)" ||
		len(r.Scopes) != 1 || r.Scopes[0]["text"] != "(.root)" || len(r.StartingStyles) != 1 || !slices.Equal(r.NestingSelectors, []string{".parent"}) {
		t.Errorf("layers %v containers %v scopes %v starting %v nesting %v", r.Layers, r.ContainerQueries, r.Scopes, r.StartingStyles, r.NestingSelectors)
	}
	// Without a selector list range, the first selector gets the rule's
	// source line (DevTools reads a rule's line from it); the others none.
	if s := r.SelectorList.Selectors; s[0].Range == nil || *s[0].Range != (cdpRange{9, 0, 9, 0}) || s[1].Range != nil {
		t.Errorf("selector ranges %+v %+v, want line 9 on the first only", s[0].Range, s[1].Range)
	}
	if want := []Sheet{{ID: "s1", SourceURL: "https://a.example/a.css", Origin: "regular"}}; !reflect.DeepEqual(sheets, want) {
		t.Errorf("sheets %+v", sheets)
	}
}

// Declarations: a disabled one (status disabled) is disabled, parsedOk
// passes on, !important leaves the value (any spacing and case) and sets
// important; a missing matchingSelectors is an empty list; pseudo element
// rules keep their pseudo type and name their sheets; a user rule is
// injected; a sheet named twice is listed once.
func TestDeclarationsAndPseudoToCDP(t *testing.T) {
	no := false
	style := Style{CSSProperties: []Property{
		{Name: "color", Value: "red !important", Priority: "important"},
		{Name: "width", Value: "1px  ! IMPORTANT", Priority: "important"},
		{Name: "height", Value: "2px", Status: "disabled"},
		{Name: "zoom", Value: "x", ParsedOk: &no},
	}}
	rule := func(origin, sheet string) Rule {
		return Rule{
			Origin: origin, RuleID: &StyleID{StyleSheetID: sheet}, SourceURL: "https://a.example/" + sheet + ".css",
			SelectorList: SelectorList{Selectors: []Selector{{Text: "::before"}}, Text: "::before"}, Style: style,
		}
	}
	m := &Matched{
		MatchedCSSRules: []RuleMatch{{Rule: rule("user", "u1")}, {Rule: rule("author", "a1")}},
		PseudoElements:  []PseudoMatches{{PseudoID: "before", Matches: []RuleMatch{{Rule: rule("author", "a2"), MatchingSelectors: []int{0}}, {Rule: rule("author", "a1")}}}},
	}
	got, _, sheets := translate(t, m, &Inline{AttributesStyle: &Style{CSSProperties: []Property{{Name: "width", Value: "10px"}}}})
	if got.MatchedCSSRules[0].Rule.Origin != "injected" || got.MatchedCSSRules[0].MatchingSelectors == nil || len(got.MatchedCSSRules[0].MatchingSelectors) != 0 {
		t.Errorf("user rule %+v", got.MatchedCSSRules[0])
	}
	s := got.MatchedCSSRules[1].Rule.Style
	color, _ := prop(s, "color")
	width, _ := prop(s, "width")
	height, _ := prop(s, "height")
	zoom, _ := prop(s, "zoom")
	if color.Value != "red" || !color.Important || width.Value != "1px" || !width.Important || !height.Disabled || height.Important || zoom.ParsedOk == nil || *zoom.ParsedOk {
		t.Errorf("declarations color %+v width %+v height %+v zoom %+v", color, width, height, zoom)
	}
	if color.ParsedOk != nil {
		t.Errorf("parsedOk %v invented for color", *color.ParsedOk)
	}
	if len(got.PseudoElements) != 1 || got.PseudoElements[0].PseudoType != "before" || len(got.PseudoElements[0].Matches) != 2 {
		t.Fatalf("pseudo elements %+v", got.PseudoElements)
	}
	var ids []string
	for _, sh := range sheets {
		ids = append(ids, sh.ID)
	}
	if !slices.Equal(ids, []string{"u1", "a1", "a2"}) {
		t.Errorf("sheets %v, want u1 a1 a2 once each", ids)
	}
	if got.AttributesStyle == nil || len(got.AttributesStyle.CSSProperties) != 1 {
		t.Errorf("attributesStyle %+v", got.AttributesStyle)
	}
	if got.Inherited == nil {
		t.Error("no inherited list")
	}
}

// A user agent rule names no style sheet, also when WIP gives it a rule
// id: DevTools prints "user agent stylesheet" from the origin, and the
// endpoint announces no header for it.
func TestUserAgentRuleNamesNoSheet(t *testing.T) {
	m := &Matched{MatchedCSSRules: []RuleMatch{{Rule: Rule{
		Origin: "user-agent", RuleID: &StyleID{StyleSheetID: "ua1"}, SourceLine: 3,
		SelectorList: SelectorList{Selectors: []Selector{{Text: "p"}}, Text: "p"},
		Style:        Style{CSSProperties: []Property{{Name: "display", Value: "block"}}},
	}}}}
	got, generic, sheets := translate(t, m, &Inline{})
	r := got.MatchedCSSRules[0].Rule
	if r.StyleSheetID != nil || r.Style.StyleSheetID != nil || r.SelectorList.Selectors[0].Range != nil || len(sheets) != 0 {
		t.Errorf("user agent rule %v, sheets %v: want no sheet and no range", generic["matchedCSSRules"], sheets)
	}
}

// A rule's sourceURL names the document's own <style> when it is empty or
// the document's URL (WebKit gives a <style> rule its document's URL).
func TestIsDocumentSheet(t *testing.T) {
	doc := "https://a.example/p.html"
	for _, c := range []struct {
		url  string
		want bool
	}{{"", true}, {doc, true}, {" " + doc + "\n", true}, {"https://a.example/a.css", false}, {doc + "#x", false}} {
		if got := IsDocumentSheet(c.url, doc); got != c.want {
			t.Errorf("IsDocumentSheet(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// genStyle is a WIP style as WebKit lists a ranged one: its declarations
// (each with a range; some are shorthands), then the longhands of each
// shorthand without a range, in shorthand order. owner maps each longhand
// to the shorthand it belongs to. Names do not share prefixes, so only
// the listing tells which longhand is whose.
type genStyle struct {
	style Style
	owner map[string]string
}

func (genStyle) Generate(r *rand.Rand, _ int) reflect.Value {
	g := genStyle{owner: map[string]string{}}
	rg := json.RawMessage(`{"startLine":0,"startColumn":0,"endLine":0,"endColumn":1}`)
	g.style.Range = rg
	var longhands []Property
	for i := range r.Intn(6) {
		name := fmt.Sprintf("decl%d", i)
		g.style.CSSProperties = append(g.style.CSSProperties, Property{Name: name, Value: "v", Range: rg})
		if r.Intn(2) == 0 {
			continue // not a shorthand
		}
		g.style.ShorthandEntries = append(g.style.ShorthandEntries, ShorthandEntry{Name: name, Value: "v"})
		for k := range 1 + r.Intn(4) {
			l := fmt.Sprintf("part%d_%d", i, k)
			g.owner[l] = name
			longhands = append(longhands, Property{Name: l, Value: fmt.Sprint(k), Implicit: k > 0})
		}
	}
	g.style.CSSProperties = append(g.style.CSSProperties, longhands...)
	return reflect.ValueOf(g)
}

// GoString keeps quick's counterexamples short.
func (g genStyle) GoString() string {
	var b strings.Builder
	for _, p := range g.style.CSSProperties {
		fmt.Fprintf(&b, "%s(ranged=%v) ", p.Name, len(p.Range) != 0)
	}
	fmt.Fprintf(&b, "shorthands %v", g.style.ShorthandEntries)
	return b.String()
}

// For any ranged WIP style: every declaration is kept in order, a
// declaration's longhandProperties hold only its own longhands (with
// their values), and a style with one shorthand gives it all of its
// longhands, in order.
func TestStyleLonghandsProperty(t *testing.T) {
	prop := func(g genStyle) bool {
		out := cdpStyle(&g.style, "s")
		props := out["cssProperties"].([]any)
		if len(props) != len(g.style.CSSProperties) {
			return false
		}
		var shorthands []string
		for _, e := range g.style.ShorthandEntries {
			shorthands = append(shorthands, e.Name)
		}
		for i, p := range props {
			pm := p.(map[string]any)
			if pm["name"] != g.style.CSSProperties[i].Name {
				return false
			}
			lh, _ := pm["longhandProperties"].([]any)
			var got []string
			for _, l := range lh {
				lm := l.(map[string]any)
				n := lm["name"].(string)
				if g.owner[n] != pm["name"] {
					t.Logf("%s listed as a longhand of %s; it belongs to %q", n, pm["name"], g.owner[n])
					return false
				}
				got = append(got, n)
			}
			if len(shorthands) == 1 && pm["name"] == shorthands[0] {
				var want []string
				for _, q := range g.style.CSSProperties {
					if g.owner[q.Name] == shorthands[0] {
						want = append(want, q.Name)
					}
				}
				if !slices.Equal(got, want) {
					t.Logf("the only shorthand %s has longhands %v, want %v", shorthands[0], got, want)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
		t.Error(err)
	}
}

// For any value, a declaration with priority important gives Chrome's
// value without the "!important" that WIP appends, and important true.
func TestImportantValueProperty(t *testing.T) {
	prop := func(words []uint8, spaces uint8, upper bool) bool {
		var b strings.Builder
		for i, w := range words {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteByte('a' + w%26)
		}
		v := b.String()
		if v == "" {
			v = "x"
		}
		bang := strings.Repeat(" ", int(spaces%3)) + "!" + strings.Repeat(" ", int(spaces/3%2)) + "important"
		if upper {
			bang = strings.ToUpper(bang)
		}
		out := cdpProperty(&Property{Name: "n", Value: v + " " + bang, Priority: "important"})
		return out["value"] == v && out["important"] == true
	}
	if err := quick.Check(prop, nil); err != nil {
		t.Error(err)
	}
}
