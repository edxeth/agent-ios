package cssrules

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Translation of the WIP replies to Chrome's CSS domain shapes, for the
// CDP endpoint (internal/cdp/css_domain.go). DevTools' CSSModel computes
// the cascade, var() values and the source locations itself from these
// shapes, so the translation keeps every declaration, range and text that
// WebKit gives.

// Sheet is a style sheet that a translated reply names by styleSheetId;
// the endpoint announces it (CSS.styleSheetAdded) before the reply,
// because WIP gives no headers without CSS.enable.
type Sheet struct {
	ID        string
	SourceURL string // WIP's sourceURL of the rule; the document's URL for a <style> element
	Origin    string // Chrome's origin
}

// CDPOrigin is Chrome's StyleSheetOrigin of a WIP origin.
func CDPOrigin(o string) string {
	switch o {
	case "author":
		return "regular"
	case "user":
		return "injected"
	}
	return o // user-agent, inspector
}

// importantSuffix is a "!important" that a value may carry.
var importantSuffix = regexp.MustCompile(`(?i)\s*!\s*important$`)

// translator collects the style sheets of one reply.
type translator struct {
	sheets []Sheet
	seen   map[string]bool
}

func (t *translator) sheet(id, url, origin string) {
	if id == "" || t.seen[id] {
		return
	}
	if t.seen == nil {
		t.seen = map[string]bool{}
	}
	t.seen[id] = true
	t.sheets = append(t.sheets, Sheet{ID: id, SourceURL: url, Origin: origin})
}

func cdpProperty(p *Property) map[string]any {
	important := p.Priority == "important"
	v := p.Value
	if important {
		v = importantSuffix.ReplaceAllString(v, "")
	}
	out := map[string]any{"name": p.Name, "value": v, "important": important, "implicit": p.Implicit, "disabled": p.Status == "disabled"}
	if p.ParsedOk != nil {
		out["parsedOk"] = *p.ParsedOk
	}
	if p.Text != "" {
		out["text"] = p.Text
	}
	if len(p.Range) != 0 {
		out["range"] = p.Range
	}
	return out
}

// cdpStyle translates a style; sheet is the styleSheetId it gets ("" for
// none). In a style with source ranges, WebKit lists a shorthand's
// longhands right after it without a range: they become the shorthand's
// longhandProperties too, which DevTools' cascade needs (it reads only
// the declarations with a range of such a style).
func cdpStyle(s *Style, sheet string) map[string]any {
	props := make([]any, 0, len(s.CSSProperties))
	var last map[string]any
	for i := range s.CSSProperties {
		p := &s.CSSProperties[i]
		cp := cdpProperty(p)
		props = append(props, cp)
		if len(s.Range) == 0 {
			continue
		}
		if len(p.Range) != 0 {
			last = cp
		} else if last != nil {
			l := map[string]any{"name": cp["name"], "value": cp["value"], "important": cp["important"]}
			lh, _ := last["longhandProperties"].([]any)
			last["longhandProperties"] = append(lh, l)
		}
	}
	shorts := make([]any, 0, len(s.ShorthandEntries))
	for _, e := range s.ShorthandEntries {
		shorts = append(shorts, map[string]any{"name": e.Name, "value": importantSuffix.ReplaceAllString(e.Value, ""), "important": e.Priority == "important"})
	}
	out := map[string]any{"cssProperties": props, "shorthandEntries": shorts}
	if sheet != "" {
		out["styleSheetId"] = sheet
	}
	if s.CSSText != "" {
		out["cssText"] = s.CSSText
	}
	if len(s.Range) != 0 {
		out["range"] = s.Range
	}
	return out
}

// line is a range of one line.
func line(n int) json.RawMessage {
	b, _ := json.Marshal(map[string]int{"startLine": n, "startColumn": 0, "endLine": n, "endColumn": 0})
	return b
}

func (t *translator) rule(r *Rule) map[string]any {
	origin := CDPOrigin(r.Origin)
	sheet := ""
	if r.RuleID != nil && r.Origin != "user-agent" {
		sheet = r.RuleID.StyleSheetID
		t.sheet(sheet, r.SourceURL, origin)
	}
	sels := make([]any, 0, len(r.SelectorList.Selectors))
	for i, s := range r.SelectorList.Selectors {
		cs := map[string]any{"text": s.Text}
		if len(s.Specificity) == 3 {
			cs["specificity"] = map[string]int{"a": s.Specificity[0], "b": s.Specificity[1], "c": s.Specificity[2]}
		}
		// DevTools reads a rule's line from its first selector's range
		// (CSSStyleRule.lineNumberInSource); WIP has the range of the
		// whole list only.
		if i == 0 && sheet != "" {
			if len(r.SelectorList.Range) != 0 {
				cs["range"] = r.SelectorList.Range
			} else {
				cs["range"] = line(r.SourceLine)
			}
		}
		sels = append(sels, cs)
	}
	out := map[string]any{
		"selectorList": map[string]any{"selectors": sels, "text": r.SelectorList.Text},
		"origin":       origin,
		"style":        cdpStyle(&r.Style, sheet),
	}
	if sheet != "" {
		out["styleSheetId"] = sheet
	}
	groupings(out, r.Groupings)
	return out
}

// groupings adds Chrome's enclosing rules: ruleTypes innermost first, as
// WIP lists its groupings, with the entries of each kind in the same
// order.
func groupings(out map[string]any, gs []Grouping) {
	var types []string
	add := func(key string, v any, typ string) {
		l, _ := out[key].([]any)
		out[key] = append(l, v)
		types = append(types, typ)
	}
	for _, g := range gs {
		switch g.Type {
		case "media-rule", "media-import-rule", "media-link-node", "media-style-node":
			if g.Text != "" {
				add("media", map[string]any{"text": g.Text, "source": "mediaRule"}, "MediaRule")
			}
		case "supports-rule":
			add("supports", map[string]any{"text": g.Text, "active": true}, "SupportsRule")
		case "layer-rule", "layer-import-rule":
			add("layers", map[string]any{"text": g.Text}, "LayerRule")
		case "container-rule":
			add("containerQueries", map[string]any{"text": g.Text}, "ContainerRule")
		case "scope-rule":
			add("scopes", map[string]any{"text": g.Text}, "ScopeRule")
		case "starting-style-rule":
			add("startingStyles", map[string]any{}, "StartingStyleRule")
		case "style-rule":
			if g.Text != "" {
				l, _ := out["nestingSelectors"].([]string)
				out["nestingSelectors"] = append(l, g.Text)
				types = append(types, "StyleRule")
			}
		}
	}
	if len(types) > 0 {
		out["ruleTypes"] = types
	}
}

func (t *translator) matches(ms []RuleMatch) []any {
	out := make([]any, 0, len(ms))
	for i := range ms {
		sel := ms[i].MatchingSelectors
		if sel == nil {
			sel = []int{}
		}
		out = append(out, map[string]any{"rule": t.rule(&ms[i].Rule), "matchingSelectors": sel})
	}
	return out
}

// MatchedToCDP is Chrome's CSS.getMatchedStylesForNode result from WIP's
// getMatchedStylesForNode and getInlineStylesForNode replies, and the
// style sheets that it names.
func MatchedToCDP(m *Matched, in *Inline) (map[string]any, []Sheet) {
	t := &translator{}
	out := map[string]any{"matchedCSSRules": t.matches(m.MatchedCSSRules)}
	if in.InlineStyle != nil {
		out["inlineStyle"] = cdpStyle(in.InlineStyle, "")
	}
	if in.AttributesStyle != nil && len(in.AttributesStyle.CSSProperties) > 0 {
		out["attributesStyle"] = cdpStyle(in.AttributesStyle, "")
	}
	pseudo := make([]any, 0, len(m.PseudoElements))
	for i := range m.PseudoElements {
		pe := &m.PseudoElements[i]
		pseudo = append(pseudo, map[string]any{"pseudoType": pe.PseudoID, "matches": t.matches(pe.Matches)})
	}
	out["pseudoElements"] = pseudo
	inh := make([]any, 0, len(m.Inherited))
	for i := range m.Inherited {
		e := &m.Inherited[i]
		entry := map[string]any{"matchedCSSRules": t.matches(e.MatchedCSSRules)}
		if e.InlineStyle != nil {
			entry["inlineStyle"] = cdpStyle(e.InlineStyle, "")
		}
		inh = append(inh, entry)
	}
	out["inherited"] = inh
	return out, t.sheets
}

// InlineToCDP is Chrome's CSS.getInlineStylesForNode result.
func InlineToCDP(in *Inline) map[string]any {
	out := map[string]any{}
	if in.InlineStyle != nil {
		out["inlineStyle"] = cdpStyle(in.InlineStyle, "")
	}
	if in.AttributesStyle != nil && len(in.AttributesStyle.CSSProperties) > 0 {
		out["attributesStyle"] = cdpStyle(in.AttributesStyle, "")
	}
	return out
}

// IsDocumentSheet reports whether a rule's sourceURL is its document's
// own <style> element: WebKit gives those the document's URL.
func IsDocumentSheet(sourceURL, docURL string) bool {
	return sourceURL == "" || strings.TrimSpace(sourceURL) == docURL
}
