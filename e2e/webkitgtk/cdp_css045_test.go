//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
)

// cssCall runs method on session and returns its reply and the receive
// sequence number of the reply, to order it against events.
func cssCall(t *testing.T, c *cdptest.Client, session, method string, params any) (json.RawMessage, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, seq, err := c.CallSeq(ctx, session, method, params)
	if err != nil {
		t.Fatalf("CDP %s: %v", method, err)
	}
	return res, seq
}

// cssFail runs method on session and returns its CDP error, failing the
// test on success.
func cssFail(t *testing.T, c *cdptest.Client, session, method string, params any) *cdptest.Error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := c.Call(ctx, session, method, params)
	if err == nil {
		t.Fatalf("%s succeeded with %s, want an error", method, res)
	}
	e, ok := err.(*cdptest.Error)
	if !ok {
		t.Fatalf("%s: %v, want a CDP error reply", method, err)
	}
	return e
}

// cssEvents are the events of method on session, received after seq after
// and before seq before (0: no bound).
func cssEvents(c *cdptest.Client, session, method string, after, before int) []cdptest.Event {
	var out []cdptest.Event
	for _, e := range c.Events() {
		if e.SessionID == session && e.Method == method && e.Seq > after && (before == 0 || e.Seq < before) {
			out = append(out, e)
		}
	}
	return out
}

// cssRule is the part of Chrome's CSS.CSSRule that the test reads.
type cssRule struct {
	StyleSheetID string `json:"styleSheetId"`
	Origin       string `json:"origin"`
	SelectorList struct {
		Text      string `json:"text"`
		Selectors []struct {
			Text  string `json:"text"`
			Range *struct {
				StartLine int `json:"startLine"`
			} `json:"range"`
		} `json:"selectors"`
	} `json:"selectorList"`
	Style     cssStyle `json:"style"`
	RuleTypes []string `json:"ruleTypes"`
	Media     []struct {
		Text string `json:"text"`
	} `json:"media"`
}

type cssStyle struct {
	StyleSheetID  string `json:"styleSheetId"`
	CSSProperties []struct {
		Name      string `json:"name"`
		Value     string `json:"value"`
		Important bool   `json:"important"`
		Range     *struct {
			StartLine int `json:"startLine"`
		} `json:"range"`
	} `json:"cssProperties"`
}

// prop is the value of the declared (ranged) property name of s; "" when
// s has none.
func (s cssStyle) prop(name string) (value string, important bool) {
	for _, p := range s.CSSProperties {
		if p.Name == name && p.Range != nil {
			return p.Value, p.Important
		}
	}
	return "", false
}

// Parity 0.45, chrome-devtools-mcp 1.14.0 get_css_styles through the CDP
// endpoint on WebKitGTK (WP-CSSBRIDGE; DevTools' DOMModel and CSSModel as
// McpPage.getCssStylesForUid drives them): on a session that sent
// CSS.enable, the exact sequence DOM.enable, CSS.enable, DOM.getDocument,
// DOM.pushNodesByBackendIdsToFrontend, CSS.getMatchedStylesForNode,
// CSS.getEnvironmentVariables and CSS.getComputedStyleForNode works with
// node ids that are backendNodeIds: DOM.setChildNodes for the node's path
// arrive before the push's reply, a CSS.styleSheetAdded header for the
// document's <style> arrives before the matched styles, and the matched
// styles have Chrome's shapes. Expected values come from the fixture
// (e2e/fixtures/runtime/css.html: the rule lines inside its <style>, the
// @media rule, the !important declaration, the inline styles of #target
// and #wrap, and its ancestors) and from the CDP definitions.
// CSS.getEnvironmentVariables stays untranslated (WebKit has no env()
// list; DevTools reads its error as no variables): the endpoint answers
// "wasn't found".
//
// A second session of the same page without CSS.enable keeps WebKit's
// node ids and gets no DOM or CSS events, and neither session's
// DOM.getDocument disturbs the other's node ids.
func TestCDPCSSBridgeDevToolsSequenceOnWebKitGTK(t *testing.T) {
	p := cdp045Client(t, "css.html")
	pc, plain := p.c, p.page // the session without CSS.enable
	// The DevTools session: a page attachment of the same logical session
	// on a second connection.
	r := cli(t, "--session", "s1", "get", "cdp-url")
	if r.code != 0 {
		t.Fatalf("get cdp-url: exit %d, stderr %q", r.code, r.stderr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := cdptest.Dial(ctx, strings.TrimSpace(r.stdout), nil)
	if err != nil {
		t.Fatalf("dialing the printed URL again: %v", err)
	}
	t.Cleanup(c.Close)
	gtkCall(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
	tab, _ := gtkLastAttach(t, c)
	devtools := &cdpPage{c: c, tab: tab}
	devtools.attachPage(t)
	css := devtools.page

	// The plain session reads WebKit's tree first and keeps the node id of
	// #target.
	var wipDoc struct {
		Root struct {
			NodeID        int64 `json:"nodeId"`
			BackendNodeID int64 `json:"backendNodeId"`
		} `json:"root"`
	}
	if err := json.Unmarshal(p.call(t, "DOM.getDocument", map[string]any{"depth": 0}), &wipDoc); err != nil {
		t.Fatal(err)
	}
	if wipDoc.Root.NodeID == 0 || wipDoc.Root.BackendNodeID != 0 {
		t.Fatalf("plain session's getDocument root %+v, want WebKit's tree (a nodeId, no backendNodeId)", wipDoc.Root)
	}
	wipTarget := p.nodeID(t, "#target")
	var plainDescribed struct {
		Node struct {
			BackendNodeID int64 `json:"backendNodeId"`
		} `json:"node"`
	}
	_ = json.Unmarshal(p.call(t, "DOM.describeNode", map[string]any{"nodeId": wipTarget}), &plainDescribed)
	backend := plainDescribed.Node.BackendNodeID
	if backend == 0 {
		t.Fatalf("describeNode of WebKit node %d gave no backendNodeId", wipTarget)
	}
	plainMark := pc.Seq()

	// DevTools' sequence on its own session.
	cssCall(t, c, css, "DOM.enable", map[string]any{})
	cssCall(t, c, css, "CSS.enable", nil)
	rawDoc, _ := cssCall(t, c, css, "DOM.getDocument", map[string]any{})
	var doc struct {
		Root struct {
			NodeID        int64 `json:"nodeId"`
			BackendNodeID int64 `json:"backendNodeId"`
			NodeName      string
			Children      []struct {
				NodeID        int64 `json:"nodeId"`
				BackendNodeID int64 `json:"backendNodeId"`
				LocalName     string
			}
		} `json:"root"`
	}
	if err := json.Unmarshal(rawDoc, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Root.NodeName != "#document" || doc.Root.NodeID == 0 || doc.Root.NodeID != doc.Root.BackendNodeID {
		t.Fatalf("CSS session's getDocument root %+v, want the document with nodeId = backendNodeId", doc.Root)
	}
	var html int64
	for _, ch := range doc.Root.Children {
		if ch.NodeID != ch.BackendNodeID {
			t.Errorf("child %+v: nodeId differs from backendNodeId", ch)
		}
		if ch.LocalName == "html" {
			html = ch.NodeID
		}
	}
	if html == 0 {
		t.Fatalf("getDocument's default depth 1 gave no html child: %s", rawDoc)
	}

	before := c.Seq()
	rawPush, pushSeq := cssCall(t, c, css, "DOM.pushNodesByBackendIdsToFrontend", map[string]any{"backendNodeIds": []int64{backend}})
	var pushed struct {
		NodeIDs []int64 `json:"nodeIds"`
	}
	_ = json.Unmarshal(rawPush, &pushed)
	if !slices.Equal(pushed.NodeIDs, []int64{backend}) {
		t.Fatalf("push of %d answered %s, want [%d]", backend, rawPush, backend)
	}
	// The path document > html > body > div#wrap > p#target, top down,
	// all before the reply (DOMModel.nodeForId reads it at the reply).
	type kid struct {
		NodeID     int64    `json:"nodeId"`
		LocalName  string   `json:"localName"`
		Attributes []string `json:"attributes"`
	}
	var parents []int64
	var last []kid
	for _, e := range cssEvents(c, css, "DOM.setChildNodes", before, pushSeq) {
		var sc struct {
			ParentID int64 `json:"parentId"`
			Nodes    []kid `json:"nodes"`
		}
		_ = json.Unmarshal(e.Params, &sc)
		parents = append(parents, sc.ParentID)
		last = sc.Nodes
	}
	if late := cssEvents(c, css, "DOM.setChildNodes", pushSeq, 0); len(late) != 0 {
		t.Errorf("%d setChildNodes after the push's reply", len(late))
	}
	if len(parents) != 4 || parents[0] != doc.Root.NodeID || parents[1] != html {
		t.Fatalf("setChildNodes parents %v, want 4 from the document %d and html %d down", parents, doc.Root.NodeID, html)
	}
	if i := slices.IndexFunc(last, func(k kid) bool { return k.NodeID == backend }); i < 0 || last[i].LocalName != "p" || !slices.Contains(last[i].Attributes, "target") {
		t.Fatalf("the last setChildNodes %+v lacks p#target with nodeId %d", last, backend)
	}

	before = c.Seq()
	rawMatched, matchedSeq := cssCall(t, c, css, "CSS.getMatchedStylesForNode", map[string]any{"nodeId": backend})
	var matched struct {
		InlineStyle     *cssStyle `json:"inlineStyle"`
		MatchedCSSRules []struct {
			Rule              cssRule `json:"rule"`
			MatchingSelectors []int   `json:"matchingSelectors"`
		} `json:"matchedCSSRules"`
		PseudoElements []json.RawMessage `json:"pseudoElements"`
		Inherited      []struct {
			InlineStyle     *cssStyle `json:"inlineStyle"`
			MatchedCSSRules []struct {
				Rule cssRule `json:"rule"`
			} `json:"matchedCSSRules"`
		} `json:"inherited"`
	}
	if err := json.Unmarshal(rawMatched, &matched); err != nil {
		t.Fatalf("decoding the matched styles %s: %v", rawMatched, err)
	}
	if matched.InlineStyle == nil {
		t.Errorf("no inlineStyle: %s", rawMatched)
	} else if v, _ := matched.InlineStyle.prop("font-weight"); v != "700" {
		t.Errorf("inline font-weight %q, want 700", v)
	}
	rules := map[string]cssRule{}
	var order []string
	for _, m := range matched.MatchedCSSRules {
		key := m.Rule.SelectorList.Text + "/" + m.Rule.Origin
		if len(m.Rule.RuleTypes) > 0 {
			key += "/" + strings.Join(m.Rule.RuleTypes, ",")
		}
		rules[key] = m.Rule
		order = append(order, key)
		if !slices.Equal(m.MatchingSelectors, []int{0}) {
			t.Errorf("rule %s matchingSelectors %v, want [0]", key, m.MatchingSelectors)
		}
	}
	// Lowest precedence first, as Chrome lists them.
	wantOrder := []string{"p/user-agent", ".box/regular", "#target/regular", "#target/regular/MediaRule", "#target.box/regular", "p/regular"}
	for _, k := range wantOrder {
		if _, ok := rules[k]; !ok {
			t.Errorf("no matched rule %s; got %v", k, order)
		}
	}
	if len(order) == 0 || order[0] != "p/user-agent" {
		t.Errorf("rule order %v, want the user agent rule first", order)
	}
	ua := rules["p/user-agent"]
	if ua.StyleSheetID != "" || ua.Style.StyleSheetID != "" {
		t.Errorf("the user agent rule names a style sheet %q/%q, want none", ua.StyleSheetID, ua.Style.StyleSheetID)
	}
	sheet := rules["#target/regular"].StyleSheetID
	if sheet == "" {
		t.Fatalf("#target has no styleSheetId: %s", rawMatched)
	}
	// Lines inside the <style> element: :root is line 1, .box 3, #target 4,
	// @media 5, #target.box 6, p 7.
	for key, line := range map[string]int{".box/regular": 3, "#target/regular": 4, "#target/regular/MediaRule": 5, "#target.box/regular": 6, "p/regular": 7} {
		r := rules[key]
		if r.StyleSheetID != sheet || r.Style.StyleSheetID != sheet {
			t.Errorf("%s: styleSheetId %q (style %q), want %q", key, r.StyleSheetID, r.Style.StyleSheetID, sheet)
		}
		if len(r.SelectorList.Selectors) == 0 || r.SelectorList.Selectors[0].Range == nil || r.SelectorList.Selectors[0].Range.StartLine != line {
			t.Errorf("%s: first selector %+v, want a range on line %d", key, r.SelectorList.Selectors, line)
		}
	}
	if m := rules["#target/regular/MediaRule"].Media; len(m) != 1 || m[0].Text != "(min-width: 1px)" {
		t.Errorf("@media rule media %+v, want (min-width: 1px)", m)
	}
	if v, imp := rules[".box/regular"].Style.prop("text-decoration"); v != "underline" || !imp {
		t.Errorf(".box text-decoration %q important %v, want underline, important", v, imp)
	}
	if v, imp := rules["#target/regular"].Style.prop("color"); v != "var(--brand)" || imp {
		t.Errorf("#target color %q important %v, want var(--brand)", v, imp)
	}
	// One inherited entry per ancestor element: div#wrap (its inline
	// letter-spacing), body, html.
	if len(matched.Inherited) != 3 {
		t.Fatalf("%d inherited entries, want 3 (div, body, html): %s", len(matched.Inherited), rawMatched)
	}
	if in := matched.Inherited[0].InlineStyle; in == nil {
		t.Error("the div's inherited entry has no inlineStyle")
	} else if v, _ := in.prop("letter-spacing"); v != "1px" {
		t.Errorf("the div's inline letter-spacing %q, want 1px", v)
	}
	var bodyRule bool
	for _, m := range matched.Inherited[1].MatchedCSSRules {
		if m.Rule.SelectorList.Text == "body" && m.Rule.Origin == "regular" {
			bodyRule = true
			if v, _ := m.Rule.Style.prop("font-size"); v != "20px" {
				t.Errorf("body font-size %q, want 20px", v)
			}
		}
	}
	if !bodyRule {
		t.Error("the body's inherited entry lacks the author body rule")
	}
	if matched.PseudoElements == nil {
		t.Error("no pseudoElements list")
	}
	// The document's <style> was announced once, before the reply, as an
	// inline sheet without URL (DevTools prints "<style>:line").
	added := cssEvents(c, css, "CSS.styleSheetAdded", before, matchedSeq)
	var headers []map[string]any
	for _, e := range added {
		var h struct {
			Header map[string]any `json:"header"`
		}
		_ = json.Unmarshal(e.Params, &h)
		headers = append(headers, h.Header)
	}
	if len(headers) != 1 || headers[0]["styleSheetId"] != sheet || headers[0]["isInline"] != true || headers[0]["sourceURL"] != "" || headers[0]["origin"] != "regular" || headers[0]["frameId"] == "" {
		t.Fatalf("styleSheetAdded before the matched styles: %v, want one inline header of sheet %s", headers, sheet)
	}

	if e := cssFail(t, c, css, "CSS.getEnvironmentVariables", map[string]any{}); !strings.Contains(e.Message, "wasn't found") {
		t.Errorf("CSS.getEnvironmentVariables: %v, want the endpoint's \"wasn't found\"", e)
	}

	rawComputed, _ := cssCall(t, c, css, "CSS.getComputedStyleForNode", map[string]any{"nodeId": backend})
	var computed struct {
		ComputedStyle []struct{ Name, Value string } `json:"computedStyle"`
	}
	_ = json.Unmarshal(rawComputed, &computed)
	got := map[string]string{}
	for _, p := range computed.ComputedStyle {
		got[p.Name] = p.Value
	}
	for name, want := range map[string]string{"color": "rgb(255, 0, 0)", "font-weight": "700", "font-size": "20px", "letter-spacing": "1px", "margin-top": "4px"} {
		if got[name] != want {
			t.Errorf("computed %s = %q, want %q", name, got[name], want)
		}
	}
	// A second read announces no sheet again.
	before = c.Seq()
	cssCall(t, c, css, "CSS.getMatchedStylesForNode", map[string]any{"nodeId": backend})
	if n := len(cssEvents(c, css, "CSS.styleSheetAdded", before, 0)); n != 0 {
		t.Errorf("the second read announced %d sheets, want 0", n)
	}
	// Methods that WebKit answers with its own ids are refused on the CSS
	// session.
	if e := cssFail(t, c, css, "DOM.querySelector", map[string]any{"nodeId": doc.Root.NodeID, "selector": "#target"}); !strings.Contains(e.Message, "CSS.enable") {
		t.Errorf("querySelector on the CSS session: %v", e)
	}

	// The plain session: no DOM or CSS events, its WebKit node id still
	// names #target after the CSS session's getDocument and reads.
	for _, ev := range []string{"DOM.setChildNodes", "DOM.documentUpdated", "CSS.styleSheetAdded"} {
		if n := len(cssEvents(pc, plain, ev, 0, 0)); n != 0 {
			t.Errorf("the session without CSS.enable got %d %s", n, ev)
		}
	}
	var again struct {
		Node struct {
			BackendNodeID int64    `json:"backendNodeId"`
			LocalName     string   `json:"localName"`
			Attributes    []string `json:"attributes"`
		} `json:"node"`
	}
	_ = json.Unmarshal(p.call(t, "DOM.describeNode", map[string]any{"nodeId": wipTarget}), &again)
	if again.Node.BackendNodeID != backend || again.Node.LocalName != "p" || !slices.Contains(again.Node.Attributes, "target") {
		t.Errorf("after the CSS session's sequence, WebKit node %d describes as %+v, want p with backendNodeId %d", wipTarget, again.Node, backend)
	}
	if e := cssFail(t, pc, plain, "CSS.getMatchedStylesForNode", map[string]any{"nodeId": wipTarget}); e.Message != "CSS agent was not enabled" {
		t.Errorf("CSS on the plain session: %v", e)
	}
	if e := cssFail(t, pc, plain, "DOM.pushNodesByBackendIdsToFrontend", map[string]any{"backendNodeIds": []int64{backend}}); !strings.Contains(e.Message, "CSS.enable") {
		t.Errorf("push on the plain session: %v", e)
	}
	if n := len(cssEvents(pc, plain, "DOM.setChildNodes", plainMark, 0)); n != 0 {
		t.Errorf("the plain session got %d setChildNodes", n)
	}

	// The other way round: the plain session's WebKit getDocument (which
	// makes WebKit forget its node ids) leaves the CSS session's ids
	// valid.
	p.call(t, "DOM.getDocument", map[string]any{"depth": 0})
	rawAgain, _ := cssCall(t, c, css, "CSS.getMatchedStylesForNode", map[string]any{"nodeId": backend})
	if !strings.Contains(string(rawAgain), `"text":"#target.box"`) {
		t.Errorf("after the plain session's getDocument the CSS session read %.300s, want #target's rules", rawAgain)
	}
}
