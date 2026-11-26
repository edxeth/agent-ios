package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/devtools/cssrules"
)

// css: the CSS rules that match an element and its computed style, read
// from the WebKit CSS domain (chrome-devtools-mcp 1.14.0 get_css_styles;
// parity notes in /home/devkit/.pi/artifacts/parity-045/notes/WP-CSS.md).
//
// Only the node methods CSS.getMatchedStylesForNode,
// CSS.getInlineStylesForNode and CSS.getComputedStyleForNode are sent, each
// with its own deadline (cssrules.CallDeadline). They answer without
// CSS.enable, which is never sent: through ios-webkit-debug-proxy
// CSS.enable, CSS.getAllStyleSheets and CSS.getStyleSheetText hang and
// stall the page connection (docs/webkit-protocol.md). The node id comes
// from DOM.requestNode on the element that the runtime resolves
// (cssrules.Open). Both commands are serialized on the page (Mutates):
// they only read, but a WIP node id lives only until the next
// DOM.getDocument, so no other work of the page runs between the lookup
// and the reads.
//
// The cascade (which declaration wins) is computed here as DevTools'
// CSSMatchedStyles does it, because WebKit marks only declarations that a
// later one of the same rule overrides ("inactive").
func init() {
	Register(&Command{
		Name:    "css matched",
		Summary: "Show the CSS rules that match an element (selector, source, declarations; overridden ones marked [overloaded]) with inline and inherited styles, from the CSS domain.",
		Args:    []Arg{targetArg("element")},
		Flags: []Flag{
			{Name: "computed", Type: FlagString, Help: "comma-separated properties to add to the Computed styles entry, which always has display, width, height and the properties of the matched and inherited rules (a shorthand adds the properties whose names start with it and -)"},
			{Name: "page-size", Type: FlagInt, Default: "10", Help: "entries per page (the Computed styles entry, element.style and each rule count as one)"},
			{Name: "page-index", Type: FlagInt, Default: "0", Help: "page to show, from 0"},
		},
		Page:      LeasedPage,
		Mutates:   true, // runs alone: see above
		RetrySafe: true,
		Output:    OutputText,
		Check:     checkCSSMatched,
		Run:       runCSSMatched,
	})
	Register(&Command{
		Name:    "css computed",
		Summary: "Show an element's computed style as the CSS domain reports it (all longhands and custom properties; get styles reads getComputedStyle in the page instead).",
		Args: []Arg{
			targetArg("element"),
			{Name: "properties", Help: "properties to show; a shorthand such as margin also shows each property whose name starts with it and - (margin-top, margin-block-start); custom properties after --; default: all", Variadic: true},
		},
		Page:      LeasedPage,
		Mutates:   true, // runs alone: see above
		RetrySafe: true,
		Output:    OutputText,
		Run:       runCSSComputed,
	})
}

func checkCSSMatched(in Input) error {
	// chrome-devtools-mcp's pageSize is a positive integer (zod
	// .positive()), so 0 is refused like a negative size.
	if v, ok := in.Flags["page-size"].(int); ok && v <= 0 {
		return usagef("css matched --page-size %d: want a positive number", v)
	}
	if v, ok := in.Flags["page-index"].(int); ok && v < 0 {
		return usagef("css matched --page-index %d: want 0 or more", v)
	}
	return nil
}

// cssElement is the element a css command inspects.
type cssElement struct {
	// node is the element's WIP node, pinned to its target and document
	// (cssrules.Node).
	node *cssrules.Node
	// chain holds DevTools' simple selector (DOMNode.simpleSelector) of
	// the element, then of each parent element up to the root.
	chain []string
	// docURL is the URL of the element's document: WebKit gives the rules
	// of its <style> elements this source URL, with lines counted in the
	// style sheet, not in the document.
	docURL string
}

// cssElementScript returns the simple selectors of this element and of
// its parent elements, as DevTools' DOMNode.simpleSelector writes them,
// and the URL of its document.
const cssElementScript = `function(){var o=[];for(var e=this;e&&e.nodeType===1;e=e.parentElement){o.push(s(e))}return{c:o,u:this.ownerDocument.URL};` +
	`function s(e){var n=e.localName,i=e.getAttribute("id"),c=e.getAttribute("class"),t=e.getAttribute("type");` +
	`if(n==="input"&&t&&!i&&!c)return n+'[type="'+CSS.escape(t)+'"]';if(i)return n+"#"+CSS.escape(i);` +
	`if(c&&c.trim())return(n==="div"?"":n)+"."+c.trim().split(/\s+/).map(function(x){return CSS.escape(x)}).join(".");return n}}`

// openCSSElement resolves the target to an element and its protocol node
// (review findings F8 and F9): the runtime picks the element (as hover
// --css does, nodeOf) into an object group of this read only, and
// cssrules.Open maps the object to a node. Every send goes to the target
// that was active at the start (in a frame's execution context, the
// operation pins its sends to that context's target itself), so a node id
// never reaches a target that a commit put in its place. The group is
// released as soon as the node is known, also when the read fails or is
// canceled: through the operation's cleanup caller, which the lease fences
// and which still sends after the operation's context ended. The caller
// closes the element.
func openCSSElement(ctx context.Context, env Env, target string) (*cssElement, error) {
	p, err := openRuntime(ctx, env)
	if err != nil {
		return nil, err
	}
	ps, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return nil, err
	}
	spec, err := p.target(ctx, target)
	if err != nil {
		return nil, err
	}
	if err := p.rt.Call(ctx, "ping", nil, nil); err != nil {
		return nil, err
	}
	conn := cssrules.Conn{C: c, Watch: ps.Caller}
	cleanup := ps.Caller
	if oe, ok := env.(interface{ Op() *core.Op }); ok && oe.Op() != nil {
		cleanup = oe.Op().CleanupCaller()
		if oe.Op().ExecutionContext().ID == 0 {
			conn.Target = cssrules.ActiveTarget(ps.Caller)
		}
	} else {
		conn.Target = cssrules.ActiveTarget(ps.Caller)
	}
	pinned := cssrules.Pin(ctx, conn.Target)
	group := cssrules.ObjectGroup()
	defer func() {
		// The group holds the element; free it, also after a failure or a
		// cancellation (cssrules.Release ignores ctx's end).
		_ = cssrules.Release(ctx, cleanup, conn.Target, group)
	}()
	args, err := json.Marshal(map[string]any{"t": spec})
	if err != nil {
		return nil, err
	}
	expr := fmt.Sprintf(`(function(){var r=window.__iwdp.call("pick",%s);if(!r.ok){throw new Error(r.error.code+": "+r.error.message)}return r.value})()`, args)
	raw, err := c.Send(pinned, "Runtime.evaluate", map[string]any{"expression": expr, "objectGroup": group})
	if err != nil {
		return nil, err
	}
	var ev struct {
		Result struct {
			ObjectID    string `json:"objectId"`
			Description string `json:"description"`
		} `json:"result"`
		WasThrown bool `json:"wasThrown"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, fmt.Errorf("resolving the target: %w", err)
	}
	if ev.WasThrown || ev.Result.ObjectID == "" {
		return nil, fmt.Errorf("resolving the target: %s", strings.TrimPrefix(ev.Result.Description, "Error: "))
	}
	el := &cssElement{}
	raw, err = c.Send(pinned, "Runtime.callFunctionOn", map[string]any{
		"objectId": ev.Result.ObjectID, "functionDeclaration": cssElementScript, "returnByValue": true,
	})
	if err != nil {
		return nil, err
	}
	var chain struct {
		Result struct {
			Value struct {
				Chain []string `json:"c"`
				URL   string   `json:"u"`
			} `json:"value"`
			Description string `json:"description"`
		} `json:"result"`
		WasThrown bool `json:"wasThrown"`
	}
	if err := json.Unmarshal(raw, &chain); err != nil {
		return nil, fmt.Errorf("reading the element's selectors: %w", err)
	}
	if chain.WasThrown || len(chain.Result.Value.Chain) == 0 {
		return nil, fmt.Errorf("reading the element's selectors: %s", chain.Result.Description)
	}
	el.chain, el.docURL = chain.Result.Value.Chain, chain.Result.Value.URL
	if el.node, err = cssrules.Open(ctx, conn, ev.Result.ObjectID); err != nil {
		return nil, err
	}
	return el, nil
}

// close ends the element's document watch.
func (e *cssElement) close() { e.node.Close() }

// call sends one CSS node method with the element's node id and params
// under cssrules.CallDeadline and decodes its reply into out.
func (e *cssElement) call(ctx context.Context, method string, params map[string]any, out any) error {
	return e.node.CallInto(ctx, method, params, out)
}

// WebKit Inspector Protocol shapes (cssrules, shared with the CDP
// endpoint's CSS domain).
type (
	wipCSSProperty   = cssrules.Property
	wipCSSStyle      = cssrules.Style
	wipCSSGrouping   = cssrules.Grouping
	wipCSSRule       = cssrules.Rule
	wipRuleMatch     = cssrules.RuleMatch
	wipMatchedStyles = cssrules.Matched
	wipInlineStyles  = cssrules.Inline
	wipComputedStyle = cssrules.Computed
)

// The output, in chrome-devtools-mcp's StructuredCssStyles form
// (CssFormatter.ts).
type cssProperty struct {
	Name          string `json:"name"`
	Value         string `json:"value"`
	ResolvedValue string `json:"resolvedValue,omitempty"`
	Status        string `json:"status"` // active, overloaded, invalid, disabled
	Important     bool   `json:"important,omitempty"`
}

type cssAncestor struct {
	Type  string `json:"type"` // media, supports, layer, container, scope, starting-style, nesting
	Query string `json:"query,omitempty"`
	Name  string `json:"name,omitempty"`
}

type cssRuleOut struct {
	Type              string            `json:"type"` // computed, inline, attributes, matched, inherited, pseudo
	Selector          string            `json:"selector,omitempty"`
	PseudoType        string            `json:"pseudoType,omitempty"`
	Node              *cssNodeOut       `json:"node,omitempty"`
	MatchingSelectors []string          `json:"matchingSelectors,omitempty"`
	Source            string            `json:"source,omitempty"`
	SourceURL         string            `json:"sourceURL,omitempty"`
	Line              int               `json:"line,omitempty"`
	IsUserAgent       bool              `json:"isUserAgent,omitempty"`
	Ancestors         []cssAncestor     `json:"ancestors,omitempty"`
	Properties        []cssProperty     `json:"properties,omitempty"`
	Computed          []computedEntry   `json:"-"`
	ComputedMap       map[string]string `json:"computed,omitempty"`
}

type cssNodeOut struct {
	Selector string `json:"selector"`
}

type computedEntry struct{ Name, Value string }

// cascade levels and their declarations (DevTools NodeCascade).
type cStyle struct {
	kind  string // inline, attributes, matched
	rule  *wipCSSRule
	match *wipRuleMatch
	style *wipCSSStyle
	props []*cProp
}

type cProp struct {
	p         *wipCSSProperty
	st        *cStyle
	longhands []*cProp
	state     int // 0 none, 1 active, 2 overloaded
}

const (
	stNone = iota
	stActive
	stOverloaded
)

func newCStyle(kind string, m *wipRuleMatch, s *wipCSSStyle) *cStyle {
	cs := &cStyle{kind: kind, match: m, style: s}
	if m != nil {
		cs.rule = &m.Rule
	}
	var last *cProp
	for i := range s.CSSProperties {
		p := &s.CSSProperties[i]
		cp := &cProp{p: p, st: cs}
		// In a style with source ranges, the declarations without a range
		// that follow a written one are its longhands: WebKit lists a
		// shorthand's longhands right after it, without a range.
		if len(s.Range) != 0 {
			if len(p.Range) != 0 {
				last = cp
			} else if last != nil {
				last.longhands = append(last.longhands, cp)
			}
		}
		cs.props = append(cs.props, cp)
	}
	return cs
}

// leading reports whether a declaration is written in the style (not a
// longhand that the browser derived), as DevTools' leadingProperties.
func (cs *cStyle) leading(cp *cProp) bool {
	if len(cs.style.Range) != 0 {
		return len(cp.p.Range) != 0
	}
	return !cp.p.Implicit
}

func (cp *cProp) parsedOk() bool  { return cp.p.ParsedOk == nil || *cp.p.ParsedOk }
func (cp *cProp) important() bool { return cp.p.Priority == "important" }
func (cp *cProp) activeInStyle() bool {
	return cp.parsedOk() && cp.p.Status != "inactive" && cp.p.Status != "disabled"
}

func (cs *cStyle) userAgent() bool { return cs.rule != nil && cs.rule.Origin == "user-agent" }

func (cs *cStyle) layer() string {
	if cs.rule == nil {
		return ""
	}
	var l []string
	for _, g := range cs.rule.Groupings {
		if g.Type == "layer-rule" || g.Type == "layer-import-rule" {
			l = append(l, g.Text)
		}
	}
	return strings.Join(l, "/")
}

// computeLevel runs DevTools' NodeCascade.computeActiveProperties on one
// element's styles, ordered from the highest precedence down.
// key gives the name that two declarations conflict by (cascadeKey).
func computeLevel(styles []*cStyle, inherited bool, key func(string) string) []*cProp {
	active := map[string]*cProp{}
	var order []*cProp
	set := func(cp *cProp, name string) {
		a := active[name]
		if a != nil && shouldOverload(a, cp) {
			cp.state = stOverloaded
			return
		}
		if a != nil {
			a.state = stOverloaded
		}
		cp.state = stActive
		if a == nil {
			order = append(order, cp)
		} else {
			for i, o := range order {
				if o == a {
					order[i] = cp
				}
			}
		}
		active[name] = cp
	}
	for i := len(styles) - 1; i >= 0; i-- {
		st := styles[i]
		for _, cp := range st.props {
			if inherited && !cssInherited(cp.p.Name) {
				continue
			}
			if len(st.style.Range) != 0 && len(cp.p.Range) == 0 {
				continue
			}
			if !cp.activeInStyle() {
				cp.state = stOverloaded
				continue
			}
			set(cp, key(cp.p.Name))
			for _, l := range cp.longhands {
				set(l, key(l.p.Name))
			}
		}
	}
	return order
}

func shouldOverload(active, higher *cProp) bool {
	if active.important() != higher.important() {
		return active.important()
	}
	if !active.important() {
		return false
	}
	if ua, hua := active.st.userAgent(), higher.st.userAgent(); ua != hua {
		return ua
	}
	if active.st.rule != nil && higher.st.rule != nil {
		return active.st.layer() != higher.st.layer()
	}
	return false
}

// computeCascade runs DevTools' DOMInheritanceCascade over the levels
// (the element first, then each ancestor): a property active at a nearer
// level overloads the same property further up, and a shorthand none of
// whose longhands won is overloaded.
func computeCascade(levels [][]*cStyle, key func(string) string) {
	winners := map[string]*cProp{}
	for i, styles := range levels {
		for _, cp := range computeLevel(styles, i > 0, key) {
			if cp.state != stActive {
				continue
			}
			k := key(cp.p.Name)
			if winners[k] != nil {
				cp.state = stOverloaded
				continue
			}
			winners[k] = cp
		}
	}
	for _, cp := range winners {
		if len(cp.longhands) == 0 {
			continue
		}
		own := false
		for _, l := range cp.longhands {
			if w := winners[key(l.p.Name)]; w != nil && w.st == cp.st {
				own = true
				break
			}
		}
		if !own {
			cp.state = stOverloaded
		}
	}
}

// computedValue is the element's computed value of name, or "".
func computedValue(comp *wipComputedStyle, name string) string {
	for _, c := range comp.ComputedStyle {
		if c.Name == name {
			return strings.TrimSpace(c.Value)
		}
	}
	return ""
}

// cascadeKey returns the name by which two declarations of an element
// conflict (review finding F10): a flow-relative (logical) property
// names the physical property that it sets in the element's writing mode
// and direction, so margin-inline-start and margin-left conflict in a
// horizontal left-to-right element, as in the browser's cascade. Other
// names are themselves. An unknown writing mode is horizontal-tb, an
// unknown direction ltr.
func cascadeKey(writingMode, direction string) func(string) string {
	vertical := writingMode != "" && writingMode != "horizontal-tb" && !strings.HasPrefix(writingMode, "lr") && !strings.HasPrefix(writingMode, "rl")
	side := map[string]string{"block-start": "top", "block-end": "bottom", "inline-start": "left", "inline-end": "right"}
	switch writingMode {
	case "vertical-rl", "sideways-rl", "tb", "tb-rl":
		side = map[string]string{"block-start": "right", "block-end": "left", "inline-start": "top", "inline-end": "bottom"}
	case "vertical-lr", "tb-lr":
		side = map[string]string{"block-start": "left", "block-end": "right", "inline-start": "top", "inline-end": "bottom"}
	case "sideways-lr":
		side = map[string]string{"block-start": "left", "block-end": "right", "inline-start": "bottom", "inline-end": "top"}
	}
	if direction == "rtl" {
		side["inline-start"], side["inline-end"] = side["inline-end"], side["inline-start"]
	}
	size := map[string]string{"inline": "width", "block": "height"}
	axis := map[string]string{"inline": "x", "block": "y"}
	if vertical {
		size = map[string]string{"inline": "height", "block": "width"}
		axis = map[string]string{"inline": "y", "block": "x"}
	}
	corner := func(block, inline string) string {
		v, h := side["block-"+block], side["inline-"+inline]
		if v == "left" || v == "right" { // vertical: the block side is horizontal
			v, h = h, v
		}
		return v + "-" + h
	}
	return func(name string) string {
		if strings.HasPrefix(name, "--") {
			return name
		}
		for _, f := range []string{"margin-", "padding-", "scroll-margin-", "scroll-padding-", "border-"} {
			rest, ok := strings.CutPrefix(name, f)
			if !ok {
				continue
			}
			for _, l := range []string{"block-start", "block-end", "inline-start", "inline-end"} {
				if tail, ok := strings.CutPrefix(rest, l); ok && (tail == "" || f == "border-" && (tail == "-width" || tail == "-style" || tail == "-color")) {
					return f + side[l] + tail
				}
			}
			if f == "border-" {
				for _, b := range []string{"start", "end"} {
					for _, i := range []string{"start", "end"} {
						if rest == b+"-"+i+"-radius" {
							return "border-" + corner(b, i) + "-radius"
						}
					}
				}
			}
		}
		if l, ok := strings.CutPrefix(name, "inset-"); ok && side[l] != "" {
			return side[l]
		}
		for _, d := range []string{"inline", "block"} {
			switch name {
			case d + "-size", "min-" + d + "-size", "max-" + d + "-size":
				return strings.Replace(name, d+"-size", size[d], 1)
			case "contain-intrinsic-" + d + "-size":
				return "contain-intrinsic-" + size[d]
			case "overflow-" + d:
				return "overflow-" + axis[d]
			case "overscroll-behavior-" + d:
				return "overscroll-behavior-" + axis[d]
			}
		}
		return name
	}
}

// cssInherited reports whether a property is inherited by default: custom
// properties and the inherited properties of CSS (DevTools reads them from
// its CSS metadata; this is the same list for the standard properties).
func cssInherited(name string) bool {
	if strings.HasPrefix(name, "--") {
		return true
	}
	return inheritedCSS[strings.TrimPrefix(name, "-webkit-")]
}

var inheritedCSS = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Fields(`border-collapse border-spacing caption-side color color-scheme cursor direction
		dominant-baseline empty-cells fill fill-opacity fill-rule font font-family font-feature-settings font-kerning
		font-language-override font-optical-sizing font-palette font-size font-size-adjust font-stretch font-style
		font-synthesis font-synthesis-small-caps font-synthesis-style font-synthesis-weight font-variant
		font-variant-alternates font-variant-caps font-variant-east-asian font-variant-emoji font-variant-ligatures
		font-variant-numeric font-variant-position font-variation-settings font-weight forced-color-adjust hanging-punctuation
		hyphenate-character hyphens image-orientation image-rendering letter-spacing line-break line-height list-style
		list-style-image list-style-position list-style-type marker marker-end marker-mid marker-start math-depth math-style
		orphans overflow-wrap paint-order pointer-events print-color-adjust quotes ruby-align ruby-position shape-rendering
		speak stroke stroke-dasharray stroke-dashoffset stroke-linecap stroke-linejoin stroke-miterlimit stroke-opacity
		stroke-width tab-size text-align text-align-last text-anchor text-combine-upright text-decoration-skip-ink
		text-emphasis text-emphasis-color text-emphasis-position text-emphasis-style text-indent text-justify
		text-orientation text-rendering text-shadow text-size-adjust text-spacing-trim text-transform text-underline-offset
		text-underline-position text-wrap text-wrap-style visibility white-space white-space-collapse widows word-break
		word-spacing word-wrap writing-mode -webkit-text-fill-color -webkit-text-stroke -webkit-text-stroke-color
		-webkit-text-stroke-width -webkit-text-security -webkit-tap-highlight-color -webkit-user-modify -webkit-line-break
		-webkit-text-combine -webkit-rtl-ordering -webkit-nbsp-mode -webkit-locale -webkit-touch-callout`) {
		m[strings.TrimPrefix(n, "-webkit-")] = true
	}
	return m
}()

// cssMatchedReport is the result of css matched, before pagination.
type cssMatchedReport struct {
	Element struct {
		Target   string `json:"target"`
		Selector string `json:"selector"`
	} `json:"element"`
	Rules      []cssRuleOut  `json:"rules"`
	Total      int           `json:"total"` // entries before pagination
	Pagination cssPagination `json:"pagination"`
	Warnings   []string      `json:"warnings,omitempty"`
}

type cssPagination struct {
	CurrentPage     int  `json:"currentPage"`
	TotalPages      int  `json:"totalPages"`
	HasNextPage     bool `json:"hasNextPage"`
	HasPreviousPage bool `json:"hasPreviousPage"`
	StartIndex      int  `json:"startIndex"`
	EndIndex        int  `json:"endIndex"`
	InvalidPage     bool `json:"invalidPage"`
}

func runCSSMatched(ctx context.Context, env Env, in Input) (Result, error) {
	target := arg(in, 0)
	el, err := openCSSElement(ctx, env, target)
	if err != nil {
		return Result{}, err
	}
	defer el.close()
	var matched wipMatchedStyles
	if err := el.call(ctx, "CSS.getMatchedStylesForNode", cssrules.MatchedParams, &matched); err != nil {
		return Result{}, err
	}
	var inline wipInlineStyles
	if err := el.call(ctx, "CSS.getInlineStylesForNode", nil, &inline); err != nil {
		return Result{}, err
	}
	var computed wipComputedStyle
	if err := el.call(ctx, "CSS.getComputedStyleForNode", nil, &computed); err != nil {
		return Result{}, err
	}
	rep := buildCSSMatched(target, el, &matched, &inline, &computed, splitList(flagString(in, "computed")))
	page := paginateCSS(&rep, flagInt(in, "page-size", 10), flagInt(in, "page-index", 0))
	return Result{Text: formatCSSMatched(&rep, page), Data: rep}, nil
}

// importantSuffix is a "!important" that a value may carry (DevTools
// strips it the same way, CssFormatter formatStructuredProperty).
var importantSuffix = regexp.MustCompile(`(?i)\s*!\s*important$`)

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// buildCSSMatched turns the three WebKit replies into chrome-devtools-mcp's
// cascade entries: the computed styles, the element's own styles from the
// highest precedence down, its ancestors' inherited styles, then pseudo
// elements (CssFormatter.collectRules).
func buildCSSMatched(target string, el *cssElement, m *wipMatchedStyles, in *wipInlineStyles, comp *wipComputedStyle, extra []string) cssMatchedReport {
	var rep cssMatchedReport
	chain := el.chain
	rep.Element.Target = target
	if len(chain) > 0 {
		rep.Element.Selector = chain[0]
	}
	// The element's own level (CSSMatchedStyles.buildMainCascade): inline,
	// author rules from the last down, attributes style, user agent rules.
	var own []*cStyle
	if in.InlineStyle != nil {
		own = append(own, newCStyle("inline", nil, in.InlineStyle))
	}
	attrs := false
	addAttrs := func() {
		if !attrs && in.AttributesStyle != nil && len(in.AttributesStyle.CSSProperties) > 0 {
			own = append(own, newCStyle("attributes", nil, in.AttributesStyle))
		}
		attrs = true
	}
	for i := len(m.MatchedCSSRules) - 1; i >= 0; i-- {
		rm := &m.MatchedCSSRules[i]
		if rm.Rule.Origin == "user-agent" || rm.Rule.Origin == "user" {
			addAttrs()
		}
		own = append(own, newCStyle("matched", rm, &rm.Rule.Style))
	}
	addAttrs()
	levels := [][]*cStyle{own}
	for i := range m.Inherited {
		e := &m.Inherited[i]
		var lv []*cStyle
		if e.InlineStyle != nil {
			lv = append(lv, newCStyle("inline", nil, e.InlineStyle))
		}
		for j := len(e.MatchedCSSRules) - 1; j >= 0; j-- {
			rm := &e.MatchedCSSRules[j]
			lv = append(lv, newCStyle("matched", rm, &rm.Rule.Style))
		}
		levels = append(levels, lv)
	}
	key := cascadeKey(computedValue(comp, "writing-mode"), computedValue(comp, "direction"))
	computeCascade(levels, key)

	vars := map[string]string{}
	for _, c := range comp.ComputedStyle {
		if strings.HasPrefix(c.Name, "--") {
			vars[c.Name] = strings.TrimSpace(c.Value)
		}
	}
	if len(m.Inherited) != len(chain)-1 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("WebKit reported %d inherited entries for %d ancestors; inherited styles are named by their position", len(m.Inherited), len(chain)-1))
	}

	rep.Rules = append(rep.Rules, computedRule(levels, comp, extra))
	for _, st := range own {
		if r, ok := styleRule(st, false, vars, el.docURL); ok {
			rep.Rules = append(rep.Rules, r)
		}
	}
	for i, lv := range levels[1:] {
		node := fmt.Sprintf("ancestor %d", i+1)
		if i+1 < len(chain) && len(m.Inherited) == len(chain)-1 {
			node = chain[i+1]
		}
		for _, st := range lv {
			r, ok := styleRule(st, true, nil, el.docURL)
			if !ok {
				continue
			}
			r.Node = &cssNodeOut{Selector: node}
			if r.Type == "inline" {
				r.Selector = "element.style"
			}
			r.Type = "inherited"
			rep.Rules = append(rep.Rules, r)
		}
	}
	for _, pe := range m.PseudoElements {
		var lv []*cStyle
		for j := len(pe.Matches) - 1; j >= 0; j-- {
			rm := &pe.Matches[j]
			lv = append(lv, newCStyle("matched", rm, &rm.Rule.Style))
		}
		computeCascade([][]*cStyle{lv}, key)
		for _, st := range lv {
			if r, ok := styleRule(st, false, nil, el.docURL); ok {
				r.Type, r.PseudoType = "pseudo", "::"+pe.PseudoID
				rep.Rules = append(rep.Rules, r)
			}
		}
	}
	return rep
}

// computedRule is the Computed styles entry: display, width, height, the
// properties that the element's styles declare, the inherited ones that
// its ancestors' styles declare, and the extra ones (a shorthand adds its
// longhands), in WebKit's order.
func computedRule(levels [][]*cStyle, comp *wipComputedStyle, extra []string) cssRuleOut {
	want := map[string]bool{"display": true, "width": true, "height": true}
	for i, lv := range levels {
		for _, st := range lv {
			for _, cp := range st.props {
				if i == 0 || cssInherited(cp.p.Name) {
					want[cp.p.Name] = true
				}
			}
		}
	}
	r := cssRuleOut{Type: "computed", ComputedMap: map[string]string{}}
	extra = expandProperties(extra, comp)
	for _, c := range comp.ComputedStyle {
		if want[c.Name] || slices.Contains(extra, c.Name) {
			r.Computed = append(r.Computed, computedEntry{c.Name, c.Value})
			r.ComputedMap[c.Name] = c.Value
		}
	}
	return r
}

// expandProperties returns the computed properties that names ask for:
// each name itself, and for a shorthand (cssShorthands, or a name that
// the computed style lacks) the computed longhands whose names start with
// it and "-" (margin-top and the others for margin), as DevTools adds a
// shorthand's longhands from its CSS metadata. Names other than custom
// properties are lowercased.
func expandProperties(names []string, comp *wipComputedStyle) []string {
	has := map[string]bool{}
	for _, c := range comp.ComputedStyle {
		has[c.Name] = true
	}
	var out []string
	for _, n := range names {
		if !strings.HasPrefix(n, "--") {
			n = strings.ToLower(n)
		}
		if has[n] {
			out = append(out, n)
			if !cssShorthands[n] {
				continue
			}
		}
		for _, c := range comp.ComputedStyle {
			if strings.HasPrefix(c.Name, n+"-") {
				out = append(out, c.Name)
			}
		}
	}
	return out
}

// cssShorthands are the shorthands whose longhands all start with their
// name and "-"; a name such as color, whose longer names (color-scheme)
// are other properties, is not one.
var cssShorthands = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Fields(`animation background border border-block border-block-end border-block-start
		border-bottom border-color border-image border-inline border-inline-end border-inline-start border-left
		border-radius border-right border-style border-top border-width column-rule columns container flex flex-flow
		font font-synthesis font-variant gap grid grid-area grid-column grid-row grid-template inset inset-block
		inset-inline list-style margin margin-block margin-inline mask outline overflow overscroll-behavior padding
		padding-block padding-inline place-content place-items place-self scroll-margin scroll-padding text-decoration
		text-emphasis transition`) {
		m[n] = true
	}
	return m
}()

// styleRule renders one style; ok is false when it shows nothing (no
// declarations, or no inherited ones for an ancestor's style).
func styleRule(st *cStyle, inherited bool, vars map[string]string, docURL string) (cssRuleOut, bool) {
	var r cssRuleOut
	switch st.kind {
	case "inline":
		r.Type, r.Selector = "inline", "element.style"
	case "attributes":
		r.Type, r.Selector = "attributes", "[attributes style]"
	default:
		r.Type = "matched"
		rule := st.rule
		r.Selector = rule.SelectorList.Text
		if len(rule.SelectorList.Selectors) > 1 {
			for _, i := range st.match.MatchingSelectors {
				if i >= 0 && i < len(rule.SelectorList.Selectors) {
					r.MatchingSelectors = append(r.MatchingSelectors, rule.SelectorList.Selectors[i].Text)
				}
			}
		}
		r.Source, r.SourceURL, r.Line = cssSource(rule, docURL)
		r.IsUserAgent = rule.Origin == "user-agent"
		r.Ancestors = cssAncestors(rule.Groupings)
	}
	for _, cp := range st.props {
		if !st.leading(cp) || (inherited && !cssInherited(cp.p.Name)) {
			continue
		}
		r.Properties = append(r.Properties, cssPropertyOut(cp, vars))
	}
	return r, len(r.Properties) > 0
}

func cssPropertyOut(cp *cProp, vars map[string]string) cssProperty {
	p := cssProperty{Name: cp.p.Name, Value: strings.TrimSpace(cp.p.Value), Important: cp.important(), Status: "active"}
	if p.Important {
		p.Value = importantSuffix.ReplaceAllString(p.Value, "")
	}
	switch {
	case !cp.parsedOk():
		p.Status = "invalid"
	case cp.p.Status == "disabled":
		p.Status = "disabled"
	case cp.state == stOverloaded:
		p.Status = "overloaded"
	}
	if vars != nil && p.Status != "invalid" && p.Status != "disabled" && strings.Contains(strings.ToLower(p.Value), "var(") {
		if v, ok := resolveVars(p.Value, vars, 0); ok && strings.TrimSpace(v) != p.Value {
			p.ResolvedValue = strings.TrimSpace(v)
		}
	}
	return p
}

// resolveVars replaces each var(--name[, fallback]) in s with the custom
// property's computed value on the element, or its resolved fallback. ok
// is false when a reference cannot be resolved. It reads s as CSS tokens
// (review finding F11): var( inside a string or a comment, or as the end
// of a longer name (myvar(), is text and stays as it is; a function name
// is case-insensitive; parentheses and commas inside strings do not end
// a reference or its fallback.
func resolveVars(s string, vars map[string]string, depth int) (string, bool) {
	if depth > 16 {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if n := cssSkip(s, i); n > i {
			b.WriteString(s[i:n])
			i = n
			continue
		}
		if !cssFuncAt(s, i, "var") {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := cssClose(s, i+4)
		if end < 0 {
			return "", false
		}
		inner := s[i+4 : end]
		name, fallback, hasFallback := inner, "", false
		if c := cssTopComma(inner); c >= 0 {
			name, fallback, hasFallback = inner[:c], inner[c+1:], true
		}
		name = strings.TrimSpace(name)
		if !strings.HasPrefix(name, "--") {
			return "", false
		}
		v, ok := vars[name]
		if !ok || v == "" {
			if !hasFallback {
				return "", false
			}
			if v, ok = resolveVars(strings.TrimSpace(fallback), vars, depth+1); !ok {
				return "", false
			}
		}
		b.WriteString(v)
		i = end + 1
	}
	return b.String(), true
}

// cssSkip returns the end of the string, comment or escape that starts at
// i in s, or i when none starts there. An unterminated string or comment
// ends at the end of s, as in CSS.
func cssSkip(s string, i int) int {
	switch {
	case s[i] == '"' || s[i] == '\'':
		q := s[i]
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case q:
				return j + 1
			}
		}
		return len(s)
	case strings.HasPrefix(s[i:], "/*"):
		if e := strings.Index(s[i+2:], "*/"); e >= 0 {
			return i + 2 + e + 2
		}
		return len(s)
	case s[i] == '\\':
		return min(i+2, len(s))
	}
	return i
}

// cssFuncAt reports whether the function token name( starts at i: the
// name, case aside, and no name character (or escape) right before it.
func cssFuncAt(s string, i int, name string) bool {
	if len(s)-i < len(name)+1 || !strings.EqualFold(s[i:i+len(name)], name) || s[i+len(name)] != '(' {
		return false
	}
	if i == 0 {
		return true
	}
	c := s[i-1]
	nameChar := c == '-' || c == '_' || c == '\\' || c >= 0x80 || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	return !nameChar
}

// cssClose returns the index of the parenthesis that closes the one open
// before i, skipping strings, comments and escapes; -1 when none does.
func cssClose(s string, i int) int {
	level := 1
	for i < len(s) {
		if n := cssSkip(s, i); n > i {
			i = n
			continue
		}
		switch s[i] {
		case '(':
			level++
		case ')':
			if level--; level == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

// cssTopComma returns the index of the first comma of s outside
// parentheses, strings and comments, or -1.
func cssTopComma(s string) int {
	level := 0
	for i := 0; i < len(s); {
		if n := cssSkip(s, i); n > i {
			i = n
			continue
		}
		switch s[i] {
		case '(':
			level++
		case ')':
			level--
		case ',':
			if level == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

// cssSource is the rule's source as chrome-devtools-mcp writes it
// (getSourceLocation): "user agent stylesheet", or the file name and the
// 1-based line, "index" for a document at its root path. A rule of a
// <style> element of the document is "<style>:line" with the line in that
// style sheet: WebKit counts its lines from the start of the element's
// text, and the offset of the element in the document is only in the
// style sheet headers, which need CSS.enable (DevTools adds it and writes
// the document's name and line).
func cssSource(rule *wipCSSRule, docURL string) (text, sourceURL string, line int) {
	switch rule.Origin {
	case "user-agent":
		return "user agent stylesheet", "", 0
	case "user":
		return "injected stylesheet", "", 0
	case "inspector":
		return "via inspector", "", 0
	}
	line = rule.SourceLine + 1
	if rule.SourceURL == "" || rule.SourceURL == docURL {
		return fmt.Sprintf("<style>:%d", line), rule.SourceURL, line
	}
	return fmt.Sprintf("%s:%d", cssFileName(rule.SourceURL), line), rule.SourceURL, line
}

func cssFileName(raw string) string {
	switch {
	case strings.HasPrefix(raw, "data:"):
		return "data-uri"
	case strings.HasPrefix(raw, "blob:"):
		return "blob"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if b := path.Base(u.Path); b != "/" && b != "." && b != "" {
		return b
	}
	return "index"
}

// cssAncestors turns WebKit's groupings (innermost first) into the
// enclosing rules, outermost first.
func cssAncestors(gs []wipCSSGrouping) []cssAncestor {
	var out []cssAncestor
	for i := len(gs) - 1; i >= 0; i-- {
		g := gs[i]
		switch g.Type {
		case "media-rule", "media-import-rule", "media-link-node", "media-style-node":
			if g.Text != "" {
				out = append(out, cssAncestor{Type: "media", Query: g.Text})
			}
		case "supports-rule":
			out = append(out, cssAncestor{Type: "supports", Query: g.Text})
		case "layer-rule", "layer-import-rule":
			out = append(out, cssAncestor{Type: "layer", Name: g.Text})
		case "container-rule":
			out = append(out, cssAncestor{Type: "container", Query: g.Text})
		case "scope-rule":
			out = append(out, cssAncestor{Type: "scope", Query: g.Text})
		case "starting-style-rule":
			out = append(out, cssAncestor{Type: "starting-style"})
		case "style-rule":
			if g.Text != "" {
				out = append(out, cssAncestor{Type: "nesting", Query: g.Text})
			}
		}
	}
	return out
}

// paginateCSS keeps one page of rep.Rules (chrome-devtools-mcp paginate):
// an index past the end shows the first page.
func paginateCSS(rep *cssMatchedReport, size, idx int) cssPagination {
	if size <= 0 {
		size = 10
	}
	n := len(rep.Rules)
	rep.Total = n
	total := max(1, (n+size-1)/size)
	p := cssPagination{TotalPages: total}
	if idx >= total {
		p.InvalidPage = true
		idx = 0
	}
	p.CurrentPage = idx
	p.StartIndex = idx * size
	p.EndIndex = min(n, p.StartIndex+size)
	p.HasNextPage = idx+1 < total
	p.HasPreviousPage = idx > 0
	rep.Rules = rep.Rules[p.StartIndex:p.EndIndex]
	rep.Pagination = p
	return p
}

// formatCSSMatched writes chrome-devtools-mcp's text form (McpResponse
// pagination lines, then CssFormatter.toString).
func formatCSSMatched(rep *cssMatchedReport, p cssPagination) string {
	var b strings.Builder
	if p.InvalidPage {
		b.WriteString("Invalid page number provided. Showing first page.\n")
	}
	fmt.Fprintf(&b, "Showing %d-%d of %d (Page %d of %d).\n", displayStart(p), p.EndIndex, rep.Total, p.CurrentPage+1, p.TotalPages)
	if p.HasNextPage {
		fmt.Fprintf(&b, "Next page: %d\n", p.CurrentPage+1)
	}
	if p.HasPreviousPage {
		fmt.Fprintf(&b, "Previous page: %d\n", p.CurrentPage-1)
	}
	fmt.Fprintf(&b, "Styles for %s (target: %q):", rep.Element.Selector, rep.Element.Target)
	if len(rep.Rules) == 0 {
		b.WriteString("\n\n  (no styles)")
	}
	w := &cssWriter{indent: 2}
	for _, r := range rep.Rules {
		w.line("")
		switch r.Type {
		case "computed":
			w.line("Computed styles:")
			w.indent += 2
			for _, c := range r.Computed {
				w.line(c.Name + ": " + c.Value + ";")
			}
			w.indent -= 2
		case "inherited":
			w.line("Inherited from " + r.Node.Selector + ":")
			w.indent += 2
			w.rule(r)
			w.indent -= 2
		case "pseudo":
			w.line("/* Pseudo " + r.PseudoType + " element */")
			w.rule(r)
		default:
			w.rule(r)
		}
	}
	if len(w.lines) > 0 {
		b.WriteString("\n" + strings.Join(w.lines, "\n"))
	}
	return withWarnings(b.String(), rep.Warnings)
}

func displayStart(p cssPagination) int {
	if p.EndIndex == 0 {
		return 0
	}
	return p.StartIndex + 1
}

type cssWriter struct {
	lines  []string
	indent int
}

func (w *cssWriter) line(s string) {
	if s == "" {
		w.lines = append(w.lines, "")
		return
	}
	w.lines = append(w.lines, strings.Repeat(" ", w.indent)+s)
}

func (w *cssWriter) rule(r cssRuleOut) {
	n := 0
	for _, a := range r.Ancestors {
		w.line(ancestorHeader(a) + " {")
		w.indent += 2
		n++
	}
	h := r.Selector
	if r.Source != "" {
		h += " (" + r.Source + ")"
	}
	w.line(h + " {")
	w.indent += 2
	for _, p := range r.Properties {
		w.line(propertyLine(p))
	}
	w.indent -= 2
	w.line("}")
	for range n {
		w.indent -= 2
		w.line("}")
	}
}

func ancestorHeader(a cssAncestor) string {
	switch a.Type {
	case "layer":
		if a.Name != "" {
			return "@layer " + a.Name
		}
		return "@layer"
	case "scope":
		if a.Query != "" {
			return "@scope " + a.Query
		}
		return "@scope"
	case "starting-style":
		return "@starting-style"
	case "nesting":
		return a.Query
	}
	return "@" + a.Type + " " + a.Query
}

// propertyLine writes one declaration as chrome-devtools-mcp does
// (formatPropertyLine): a status tag unless active, !important, and the
// resolved var() value as a comment.
func propertyLine(p cssProperty) string {
	s := ""
	if p.Status != "active" {
		s = "[" + p.Status + "] "
	}
	s += p.Name + ": " + p.Value
	if p.Important {
		s += " !important"
	}
	s += ";"
	if p.ResolvedValue != "" {
		s += " /* " + strings.ReplaceAll(p.ResolvedValue, "*/", `*\/`) + " */"
	}
	return s
}

func runCSSComputed(ctx context.Context, env Env, in Input) (Result, error) {
	el, err := openCSSElement(ctx, env, arg(in, 0))
	if err != nil {
		return Result{}, err
	}
	defer el.close()
	var computed wipComputedStyle
	if err := el.call(ctx, "CSS.getComputedStyleForNode", nil, &computed); err != nil {
		return Result{}, err
	}
	names := in.Args[1:]
	want := expandProperties(names, &computed)
	var lines []string
	m := map[string]string{}
	for _, c := range computed.ComputedStyle {
		if len(names) > 0 && !slices.Contains(want, c.Name) {
			continue
		}
		lines = append(lines, c.Name+": "+c.Value)
		m[c.Name] = c.Value
	}
	if len(names) > 0 && len(lines) == 0 {
		return Result{}, errors.New("css computed: the element's computed style has none of " + strings.Join(names, ", "))
	}
	return Result{Text: strings.Join(lines, "\n"), Data: map[string]any{"selector": el.chain[0], "properties": m}}, nil
}
