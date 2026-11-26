package cdp

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/devtools/cssrules"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// cssPage is a page whose runtime describes a small tree for jsTree
// (document 1 > html 2 > body 3 > p 4, handles of document "doc-1") and
// whose WebKit answers the CSS node methods for WIP node 42.
func cssPage(t *testing.T) (*w5b2Page, int64) {
	t.Helper()
	w := newW5b2Page(t)
	p := w.backendOf(t, "doc-1", 4)
	el := func(h int, name string, kids int) map[string]any {
		return map[string]any{"h": h, "nodeType": 1, "nodeName": strings.ToUpper(name), "localName": name, "nodeValue": "", "childNodeCount": kids, "attributes": []string{}}
	}
	pageScript{
		{jsTree, func(this string, args json.RawMessage) any {
			if strings.Contains(string(args), `"path"`) {
				return map[string]any{"v": map[string]any{"doc": "doc-1", "h": 4, "steps": []any{
					map[string]any{"h": 1, "kids": []any{el(2, "html", 1)}},
					map[string]any{"h": 2, "kids": []any{el(3, "body", 1)}},
					map[string]any{"h": 3, "kids": []any{el(4, "p", 0)}},
				}}}
			}
			return map[string]any{"v": map[string]any{"doc": "doc-1", "n": map[string]any{
				"h": 1, "nodeType": 9, "nodeName": "#document", "localName": "", "nodeValue": "", "childNodeCount": 1,
				"documentURL": "https://one.example/", "children": []any{el(2, "html", 1)},
			}}}
		}},
		{jsDocURL, pageAnswer("https://one.example/")},
	}.on(w.pf)
	w.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Expression  string `json:"expression"`
			ObjectGroup string `json:"objectGroup"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if strings.Contains(p.Expression, "return I._.byHandle(") { // byReference
			return map[string]any{"result": map[string]any{"type": "object", "subtype": "node", "objectId": "obj-in-" + p.ObjectGroup}}, nil
		}
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil {
			return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"ok": true, "value": true}}}, nil
		}
		this := "null"
		if m := byHandleCall.FindStringSubmatch(p.Expression); m != nil {
			this = m[1] + "#" + m[2]
		}
		var a json.RawMessage
		if m := evalArgs.FindStringSubmatch(p.Expression); m != nil {
			a = json.RawMessage(m[1])
		}
		return w.pf.answer(p.Expression, this, a), nil
	})
	w.f.Result("DOM.requestNode", map[string]any{"nodeId": 42})
	w.f.Result("Runtime.releaseObjectGroup", map[string]any{})
	style := map[string]any{
		"styleId": map[string]any{"styleSheetId": "7", "ordinal": 1},
		"cssProperties": []any{
			map[string]any{"name": "margin", "value": "0", "text": "margin: 0;", "range": map[string]int{"startLine": 7, "startColumn": 4, "endLine": 7, "endColumn": 14}, "status": "active"},
			map[string]any{"name": "margin-top", "value": "0px"},
			map[string]any{"name": "color", "value": "red", "priority": "important", "text": "color: red !important;", "range": map[string]int{"startLine": 7, "startColumn": 15, "endLine": 7, "endColumn": 37}},
		},
		"shorthandEntries": []any{map[string]any{"name": "margin", "value": "0px"}},
		"cssText":          " margin: 0; color: red !important; ",
		"range":            map[string]int{"startLine": 7, "startColumn": 3, "endLine": 7, "endColumn": 39},
	}
	rule := func(origin, url string, sheet any) map[string]any {
		r := map[string]any{
			"selectorList": map[string]any{"selectors": []any{map[string]any{"text": "p", "specificity": []int{0, 0, 1}}}, "text": "p", "range": map[string]int{"startLine": 7, "startColumn": 0, "endLine": 7, "endColumn": 1}},
			"sourceLine":   7, "origin": origin, "style": style, "sourceURL": url,
			"groupings": []any{map[string]any{"type": "media-rule", "text": "(min-width: 1px)"}},
		}
		if sheet != nil {
			r["ruleId"] = map[string]any{"styleSheetId": sheet, "ordinal": 1}
		}
		return r
	}
	w.f.Result("CSS.getMatchedStylesForNode", map[string]any{
		"matchedCSSRules": []any{
			map[string]any{"rule": rule("user-agent", "", nil), "matchingSelectors": []int{0}},
			map[string]any{"rule": rule("author", "https://one.example/", "7"), "matchingSelectors": []int{0}},
		},
		"pseudoElements": []any{},
		"inherited":      []any{map[string]any{"matchedCSSRules": []any{}}, map[string]any{"matchedCSSRules": []any{}}},
	})
	w.f.Result("CSS.getInlineStylesForNode", map[string]any{"inlineStyle": map[string]any{"cssProperties": []any{}, "shorthandEntries": []any{}}})
	w.f.Result("CSS.getComputedStyleForNode", map[string]any{"computedStyle": []any{map[string]any{"name": "display", "value": "block"}}})
	return w, p
}

// get_css_styles through the endpoint (chrome-devtools-mcp 1.14.0
// McpPage.getCssStylesForUid, DevTools CSSModel and DOMModel): the method
// sequence DevTools sends works on an attachment that enabled CSS, with
// node ids of its own (the backendNodeIds), DOM.setChildNodes for the
// node's path before the push's reply, a CSS.styleSheetAdded header before
// the first reply that names a sheet, Chrome's rule shapes, and WIP's CSS
// node methods on WIP's node id. CSS.enable never reaches WebKit, nor does
// any style sheet method.
func TestCSSBridgeDevToolsSequence(t *testing.T) {
	w, p := cssPage(t)
	m := w.mark()
	w.call(t, "CSS.enable", map[string]any{})
	w.call(t, "DOM.enable", map[string]any{})
	doc := decode[struct {
		Root struct {
			NodeID, BackendNodeID int64
			Children              []struct{ NodeID, BackendNodeID int64 }
		}
	}](t, w.call(t, "DOM.getDocument", map[string]any{}))
	if doc.Root.NodeID == 0 || doc.Root.NodeID != doc.Root.BackendNodeID || len(doc.Root.Children) != 1 || doc.Root.Children[0].NodeID != doc.Root.Children[0].BackendNodeID {
		t.Fatalf("getDocument of a CSS attachment: %+v, want node ids equal to backendNodeIds", doc)
	}
	html := doc.Root.Children[0].NodeID
	before := len(w.c.Events())
	pushed := decode[struct{ NodeIDs []int64 }](t, w.call(t, "DOM.pushNodesByBackendIdsToFrontend", map[string]any{"backendNodeIds": []int64{p}}))
	if !slices.Equal(pushed.NodeIDs, []int64{p}) {
		t.Fatalf("push answered %v, want [%d]", pushed.NodeIDs, p)
	}
	var parents []int64
	for _, e := range w.c.Events()[before:] {
		if e.Method == "DOM.setChildNodes" {
			parents = append(parents, decode[struct{ ParentID int64 }](t, e.Params).ParentID)
		}
	}
	if len(parents) != 3 || parents[0] != doc.Root.NodeID || parents[1] != html {
		t.Fatalf("setChildNodes parents %v, want the path from the document (%d, %d, body)", parents, doc.Root.NodeID, html)
	}
	before = len(w.c.Events())
	matched := w.call(t, "CSS.getMatchedStylesForNode", map[string]any{"nodeId": p})
	var sheets []json.RawMessage
	for _, e := range w.c.Events()[before:] {
		if e.Method == "CSS.styleSheetAdded" {
			sheets = append(sheets, e.Params)
		}
	}
	if len(sheets) != 1 || !strings.Contains(string(sheets[0]), `"styleSheetId":"7"`) || !strings.Contains(string(sheets[0]), `"isInline":true`) || !strings.Contains(string(sheets[0]), `"sourceURL":""`) {
		t.Fatalf("styleSheetAdded before the reply: %s, want sheet 7 as an inline sheet of the document", sheets)
	}
	for _, want := range []string{
		`"origin":"regular"`, `"origin":"user-agent"`, `"styleSheetId":"7"`, `"ruleTypes":["MediaRule"]`, `"media":[{"source":"mediaRule","text":"(min-width: 1px)"}]`,
		`"longhandProperties":[{"important":false,"name":"margin-top","value":"0px"}]`, `"important":true,"name":"color"`, `"specificity":{"a":0,"b":0,"c":1}`, `"inherited":[{"matchedCSSRules":[]},{"matchedCSSRules":[]}]`,
	} {
		if !strings.Contains(string(matched), want) {
			t.Errorf("getMatchedStylesForNode reply lacks %s:\n%s", want, matched)
		}
	}
	w.call(t, "CSS.getMatchedStylesForNode", map[string]any{"nodeId": p})
	if n := len(eventsOf(w.c, "CSS.styleSheetAdded")); n != 1 {
		t.Errorf("sheet 7 announced %d times, want once", n)
	}
	comp := w.call(t, "CSS.getComputedStyleForNode", map[string]any{"nodeId": p})
	if string(comp) != `{"computedStyle":[{"name":"display","value":"block"}]}` {
		t.Errorf("computed style %s", comp)
	}
	sent := w.sentSince(m)
	for _, s := range sent {
		for _, never := range []string{"CSS.enable", "CSS.disable", "CSS.getAllStyleSheets", "CSS.getStyleSheetText", "DOM.getDocument", "DOM.enable"} {
			if strings.HasPrefix(s, never+" ") {
				t.Errorf("WebKit received %s", s)
			}
		}
	}
	var css, groups, released []string
	for _, rec := range w.f.Received()[m:] {
		switch rec.Method {
		case "CSS.getMatchedStylesForNode", "CSS.getInlineStylesForNode", "CSS.getComputedStyleForNode":
			css = append(css, rec.Method+" "+string(rec.Params))
		case "DOM.requestNode":
			groups = append(groups, string(rec.Params))
		case "Runtime.releaseObjectGroup":
			released = append(released, string(rec.Params))
		}
	}
	wantCSS := []string{
		`CSS.getMatchedStylesForNode {"includeInherited":true,"includePseudo":true,"nodeId":42}`, `CSS.getInlineStylesForNode {"nodeId":42}`,
		`CSS.getMatchedStylesForNode {"includeInherited":true,"includePseudo":true,"nodeId":42}`, `CSS.getInlineStylesForNode {"nodeId":42}`,
		`CSS.getComputedStyleForNode {"nodeId":42}`,
	}
	if !slices.Equal(css, wantCSS) {
		t.Errorf("WebKit's CSS calls %v, want %v", css, wantCSS)
	}
	// Each read used its own object group and released it.
	if len(groups) != 3 || len(released) != 3 || groups[0] == groups[1] {
		t.Errorf("requestNode objects %v, releases %v: want one group per read, each released", groups, released)
	}
	for i, g := range groups {
		var a struct{ ObjectID string }
		_ = json.Unmarshal([]byte(g), &a)
		if !strings.Contains(released[i], strings.TrimPrefix(a.ObjectID, "obj-in-")) {
			t.Errorf("read %d requested %s but released %s", i, g, released[i])
		}
	}
}

// Other attachments are unaffected: without CSS.enable the CSS methods
// and the push are refused without a send, DOM.getDocument still answers
// WebKit's tree with its node ids, and no DOM event reaches them when a
// CSS attachment of the same page pushes nodes.
func TestCSSBridgeOptInPerAttachment(t *testing.T) {
	w, p := cssPage(t)
	m := w.mark()
	if e := w.fail(t, "CSS.getMatchedStylesForNode", map[string]any{"nodeId": 1}); e.Message != "CSS agent was not enabled" {
		t.Errorf("before CSS.enable: %v", e)
	}
	if e := w.fail(t, "DOM.pushNodesByBackendIdsToFrontend", map[string]any{"backendNodeIds": []int{1}}); !strings.Contains(e.Message, "CSS.enable") {
		t.Errorf("push before CSS.enable: %v", e)
	}
	if s := w.sentSince(m); len(s) != 0 {
		t.Errorf("refused calls sent %v", s)
	}
	w.f.Result("DOM.getDocument", map[string]any{"root": map[string]any{"nodeId": 1, "nodeType": 9}})
	if got := string(w.call(t, "DOM.getDocument", nil)); got != `{"root":{"backendNodeId":0,"nodeId":1,"nodeType":9}}` {
		t.Errorf("getDocument without CSS.enable: %s", got)
	}
	// A second attachment of the same page enables CSS and pushes.
	c2 := dial(t, w.r.issue(t, "s1", "One", ScopePage).WS)
	_, page2 := puppeteerAttach(t, c2)
	call(t, c2, page2, "CSS.enable", map[string]any{})
	call(t, c2, page2, "DOM.getDocument", map[string]any{})
	call(t, c2, page2, "DOM.pushNodesByBackendIdsToFrontend", map[string]any{"backendNodeIds": []int64{p}})
	if n := len(eventsOf(c2, "DOM.setChildNodes")); n != 3 {
		t.Errorf("the CSS attachment got %d setChildNodes, want 3", n)
	}
	time.Sleep(50 * time.Millisecond) // events of the other attachment would have arrived by now
	for _, ev := range []string{"DOM.setChildNodes", "DOM.documentUpdated", "CSS.styleSheetAdded"} {
		if n := len(eventsOf(w.c, ev)); n != 0 {
			t.Errorf("the attachment without CSS.enable got %d %s", n, ev)
		}
	}
	// On the CSS attachment, a nodeId is the backendNodeId and methods
	// that need WebKit's ids are refused.
	if _, err := c2.Call(testCtx(t), page2, "DOM.querySelector", map[string]any{"nodeId": p, "selector": "p"}); err == nil || !strings.Contains(err.Error(), "CSS.enable") {
		t.Errorf("querySelector on the CSS attachment: %v", err)
	}
	obj := decode[struct{ Object map[string]any }](t, call(t, c2, page2, "DOM.resolveNode", map[string]any{"nodeId": p}))
	if obj.Object["objectId"] == nil {
		t.Errorf("resolveNode{nodeId} on the CSS attachment: %v", obj.Object)
	}
}

// Each WIP CSS call has its own deadline (cssrules.CallDeadline), so a
// stalled CSS domain fails the command with its reason quickly.
func TestCSSBridgeCallDeadline(t *testing.T) {
	old := cssrules.CallDeadline
	cssrules.CallDeadline = 100 * time.Millisecond
	t.Cleanup(func() { cssrules.CallDeadline = old })
	w, p := cssPage(t)
	w.f.Stall("CSS.getComputedStyleForNode")
	w.call(t, "CSS.enable", nil)
	e := w.fail(t, "CSS.getComputedStyleForNode", map[string]any{"nodeId": p})
	if !strings.Contains(e.Message, "CSS.getComputedStyleForNode got no reply within 100ms") {
		t.Fatalf("stalled call: %v", e)
	}
	if n := len(w.f.ReceivedMethod("Runtime.releaseObjectGroup")); n != 1 {
		t.Errorf("%d releases after the stall, want 1", n)
	}
}
