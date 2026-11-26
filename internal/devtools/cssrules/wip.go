package cssrules

import "encoding/json"

// WebKit Inspector Protocol shapes of the CSS node methods (CSS.json, iOS
// 16 to 27). Ranges stay raw: they are passed on unchanged.

// Property is a WIP CSS.CSSProperty.
type Property struct {
	Name     string          `json:"name"`
	Value    string          `json:"value"`
	Priority string          `json:"priority"`
	Implicit bool            `json:"implicit"`
	Text     string          `json:"text"`
	ParsedOk *bool           `json:"parsedOk"`
	Status   string          `json:"status"` // active, inactive, disabled, style
	Range    json.RawMessage `json:"range"`
}

// ShorthandEntry is a WIP CSS.ShorthandEntry.
type ShorthandEntry struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Priority string `json:"priority"`
}

// StyleID names a style in its style sheet.
type StyleID struct {
	StyleSheetID string `json:"styleSheetId"`
	Ordinal      int    `json:"ordinal"`
}

// Style is a WIP CSS.CSSStyle.
type Style struct {
	StyleID          *StyleID         `json:"styleId"`
	CSSProperties    []Property       `json:"cssProperties"`
	ShorthandEntries []ShorthandEntry `json:"shorthandEntries"`
	CSSText          string           `json:"cssText"`
	Range            json.RawMessage  `json:"range"`
}

// Grouping is a WIP CSS.Grouping (an enclosing @media, @supports, @layer,
// @container, @scope, @starting-style or style rule).
type Grouping struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	SourceURL string `json:"sourceURL"`
}

// Selector is a WIP CSS.CSSSelector.
type Selector struct {
	Text        string `json:"text"`
	Specificity []int  `json:"specificity,omitempty"`
}

// SelectorList is a WIP CSS.SelectorList.
type SelectorList struct {
	Selectors []Selector      `json:"selectors"`
	Text      string          `json:"text"`
	Range     json.RawMessage `json:"range"`
}

// Rule is a WIP CSS.CSSRule.
type Rule struct {
	RuleID       *StyleID     `json:"ruleId"`
	SelectorList SelectorList `json:"selectorList"`
	SourceURL    string       `json:"sourceURL"`
	SourceLine   int          `json:"sourceLine"`
	Origin       string       `json:"origin"` // user, user-agent, author, inspector
	Style        Style        `json:"style"`
	Groupings    []Grouping   `json:"groupings"`
}

// RuleMatch is a WIP CSS.RuleMatch.
type RuleMatch struct {
	Rule              Rule  `json:"rule"`
	MatchingSelectors []int `json:"matchingSelectors"`
}

// PseudoMatches are the rules of one pseudo element.
type PseudoMatches struct {
	PseudoID string      `json:"pseudoId"`
	Matches  []RuleMatch `json:"matches"`
}

// InheritedEntry is the styles of one ancestor element.
type InheritedEntry struct {
	InlineStyle     *Style      `json:"inlineStyle"`
	MatchedCSSRules []RuleMatch `json:"matchedCSSRules"`
}

// Matched is the reply of CSS.getMatchedStylesForNode: rules lowest
// precedence first, one inherited entry per ancestor element.
type Matched struct {
	MatchedCSSRules []RuleMatch      `json:"matchedCSSRules"`
	PseudoElements  []PseudoMatches  `json:"pseudoElements"`
	Inherited       []InheritedEntry `json:"inherited"`
}

// Inline is the reply of CSS.getInlineStylesForNode.
type Inline struct {
	InlineStyle     *Style `json:"inlineStyle"`
	AttributesStyle *Style `json:"attributesStyle"`
}

// ComputedProperty is one computed value.
type ComputedProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Computed is the reply of CSS.getComputedStyleForNode.
type Computed struct {
	ComputedStyle []ComputedProperty `json:"computedStyle"`
}

// MatchedParams are the parameters of CSS.getMatchedStylesForNode that
// both readers send: pseudo elements and inherited styles included.
var MatchedParams = map[string]any{"includePseudo": true, "includeInherited": true}
