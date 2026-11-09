package cdp

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// axFixture is the answer of the runtime's cdpAX for document doc-1:
// the full-mode snapshot walk of a page titled "Doc title".
func axFixture() map[string]any {
	return map[string]any{"doc": "doc-1", "h": 1, "title": "Doc title", "tree": []any{
		map[string]any{"h": 2, "role": "heading", "name": "Title", "level": 1, "children": []any{map[string]any{"h": 3, "role": "StaticText", "name": "Title"}}},
		map[string]any{"h": 4, "role": "checkbox", "name": "Agree", "checked": "mixed", "required": true, "disabled": true, "fo": true, "fd": false},
		map[string]any{"h": 5, "role": "button", "name": "Menu", "expanded": false, "fo": true, "fd": true},
		map[string]any{"h": 6, "role": "option", "name": "O", "selected": true, "value": "v1"},
	}}
}

type axNodeOut struct {
	NodeID           string `json:"nodeId"`
	Ignored          bool   `json:"ignored"`
	BackendDOMNodeID int64  `json:"backendDOMNodeId"`
	ParentID         string `json:"parentId"`
	ChildIDs         []string
	Role, Name       struct {
		Type  string `json:"type"`
		Value any    `json:"value"`
	}
	Value      json.RawMessage `json:"value"`
	Properties json.RawMessage `json:"properties"`
}

// axByName checks that every node's nodeId is its backendDOMNodeId and
// that no two nodes share one, and returns the nodes by name.
func axByName(t *testing.T, nodes []axNodeOut) map[string]axNodeOut {
	t.Helper()
	out, seen := map[string]axNodeOut{}, map[int64]bool{}
	for _, n := range nodes {
		if n.NodeID != strconv.FormatInt(n.BackendDOMNodeID, 10) || n.BackendDOMNodeID == 0 || seen[n.BackendDOMNodeID] {
			t.Fatalf("AX node %+v: nodeId must be its backendDOMNodeId, unique and not 0", n)
		}
		seen[n.BackendDOMNodeID] = true
		key := n.Name.Value.(string)
		if n.Role.Value == "StaticText" {
			key = "text:" + key
		}
		out[key] = n
	}
	return out
}

// Design 5.3 Accessibility getFullAXTree, from the runtime snapshot in
// full mode: the root (RootWebArea, named by the title) first, then the
// nodes in pre-order (Puppeteer takes the first node as the root). Each
// node has nodeId, role {type role}, name {type computedString},
// backendDOMNodeId (its backendNodeId, so nodeId is the same number),
// parentId and childIds, its value, and the properties checked
// (tristate), expanded and selected (booleanOrUndefined), disabled and
// required (boolean), level (integer), focusable and focused
// (booleanOrUndefined) in that order (CDP Accessibility.AXPropertyName).
func TestAXFullTree(t *testing.T) {
	w := newW5b2Page(t)
	w.pf.setFn("cdpAX", func(json.RawMessage) any { return axFixture() })
	nodes := decode[struct{ Nodes []axNodeOut }](t, w.call(t, "Accessibility.getFullAXTree", map[string]any{})).Nodes
	by := axByName(t, nodes)
	order := []string{}
	for _, n := range nodes {
		order = append(order, n.Role.Value.(string))
	}
	if strings.Join(order, " ") != "RootWebArea heading StaticText checkbox button option" {
		t.Fatalf("node order %v", order)
	}
	id := func(name string) string { return by[name].NodeID }
	root := by["Doc title"]
	if root.Role.Type != "role" || root.Name.Type != "computedString" || root.ParentID != "" || strings.Join(root.ChildIDs, ",") != strings.Join([]string{id("Title"), id("Agree"), id("Menu"), id("O")}, ",") {
		t.Fatalf("root %+v", root)
	}
	// The root is focusable, as Chrome's RootWebArea is. Puppeteer's
	// accessibility.snapshot() (puppeteer-core 25.16.0 cdp/Accessibility.ts
	// isInteresting) keeps a node without a control role only when it is
	// focusable or a named leaf; without the property it drops the root and
	// returns the root's first child as the whole snapshot.
	if string(root.Properties) != `[{"name":"focusable","value":{"type":"booleanOrUndefined","value":true}}]` {
		t.Fatalf("root properties %s, want focusable true", root.Properties)
	}
	if h := by["Title"]; h.ParentID != root.NodeID || strings.Join(h.ChildIDs, ",") != id("text:Title") || string(h.Properties) != `[{"name":"level","value":{"type":"integer","value":1}}]` {
		t.Fatalf("heading %+v %s", h, h.Properties)
	}
	if s := by["text:Title"]; s.ParentID != id("Title") || s.ChildIDs == nil || len(s.ChildIDs) != 0 || s.Properties != nil {
		t.Fatalf("static text %+v", s)
	}
	props := map[string]string{
		"Agree": `[{"name":"checked","value":{"type":"tristate","value":"mixed"}},{"name":"disabled","value":{"type":"boolean","value":true}},{"name":"required","value":{"type":"boolean","value":true}},{"name":"focusable","value":{"type":"booleanOrUndefined","value":true}}]`,
		"Menu":  `[{"name":"expanded","value":{"type":"booleanOrUndefined","value":false}},{"name":"focusable","value":{"type":"booleanOrUndefined","value":true}},{"name":"focused","value":{"type":"booleanOrUndefined","value":true}}]`,
		"O":     `[{"name":"selected","value":{"type":"booleanOrUndefined","value":true}}]`,
	}
	for name, want := range props {
		if n := by[name]; string(n.Properties) != want || n.ParentID != root.NodeID || n.Ignored {
			t.Fatalf("%s: properties %s parent %s, want %s under the root", name, n.Properties, n.ParentID, want)
		}
	}
	if v := string(by["O"].Value); v != `{"type":"string","value":"v1"}` {
		t.Fatalf("option value %s", v)
	}
	if by["Agree"].Value != nil {
		t.Fatalf("a node without a value has %s", by["Agree"].Value)
	}
	// The same page again gives the same numbers (design 5.2: a node keeps
	// its number in its document).
	again := axByName(t, decode[struct{ Nodes []axNodeOut }](t, w.call(t, "Accessibility.getFullAXTree", map[string]any{})).Nodes)
	for name, n := range by {
		if again[name].NodeID != n.NodeID {
			t.Fatalf("%s: %s, then %s", name, n.NodeID, again[name].NodeID)
		}
	}
}

// getFullAXTree depth limits the levels below the root (depth 1: the root
// and its children); a frameId other than the main frame's is refused
// (its same-origin content is part of the main tree, design 3.2), the
// main frame's CDP id (the page target id, as Page.getFrameTree names it)
// is accepted.
func TestAXFullTreeDepthAndFrame(t *testing.T) {
	w := newW5b2Page(t)
	w.pf.setFn("cdpAX", func(json.RawMessage) any { return axFixture() })
	w.f.Result("Page.getResourceTree", map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "main"}}})
	nodes := decode[struct{ Nodes []axNodeOut }](t, w.call(t, "Accessibility.getFullAXTree", map[string]any{"depth": 1, "frameId": w.pageTargetID(t)})).Nodes
	var roles []string
	for _, n := range nodes {
		roles = append(roles, n.Role.Value.(string))
	}
	if strings.Join(roles, " ") != "RootWebArea heading checkbox button option" {
		t.Fatalf("depth 1 nodes %v", roles)
	}
	if e := w.fail(t, "Accessibility.getFullAXTree", map[string]any{"frameId": "child"}); !strings.Contains(e.Message, "is not the main frame") {
		t.Fatalf("child frame: %+v", e)
	}
}

// Design 5.3 Accessibility getPartialAXTree (CDP: the node, then with
// fetchRelatives, default true, its ancestors, siblings and children): a
// node by backendNodeId or objectId; with fetchRelatives false only the
// node. A node that the tree does not expose is one ignored node.
func TestAXPartialTree(t *testing.T) {
	w := newW5b2Page(t)
	heading := w.backendOf(t, "doc-1", 2)
	w.pf.setFn("cdpAX", func(json.RawMessage) any { return axFixture() })
	names := func(nodes []axNodeOut) string {
		var l []string
		for _, n := range nodes {
			l = append(l, n.Role.Value.(string)+":"+n.Name.Value.(string))
		}
		return strings.Join(l, " ")
	}
	got := decode[struct{ Nodes []axNodeOut }](t, w.call(t, "Accessibility.getPartialAXTree", map[string]any{"backendNodeId": heading})).Nodes
	if names(got) != "heading:Title RootWebArea:Doc title checkbox:Agree button:Menu option:O StaticText:Title" || got[0].BackendDOMNodeID != heading {
		t.Fatalf("partial tree of the heading: %s", names(got))
	}
	pageScript{{jsHandle, pageAnswer(map[string]any{"doc": "doc-1", "h": 4})}}.on(w.pf)
	got = decode[struct{ Nodes []axNodeOut }](t, w.call(t, "Accessibility.getPartialAXTree", map[string]any{"objectId": "o-cb", "fetchRelatives": false})).Nodes
	if names(got) != "checkbox:Agree" {
		t.Fatalf("partial tree of the checkbox without relatives: %s", names(got))
	}
	pageScript{{jsHandle, pageAnswer(map[string]any{"doc": "doc-1", "h": 99})}}.on(w.pf)
	raw := w.call(t, "Accessibility.getPartialAXTree", map[string]any{"objectId": "o-hidden"})
	got = decode[struct{ Nodes []axNodeOut }](t, raw).Nodes
	if len(got) != 1 || !got[0].Ignored || got[0].Role.Value != "none" || got[0].BackendDOMNodeID == 0 {
		t.Fatalf("partial tree of an unexposed node: %s", raw)
	}
	if e := w.fail(t, "Accessibility.getPartialAXTree", map[string]any{}); e.Message != "Either nodeId, backendNodeId or objectId must be specified" {
		t.Fatalf("no node: %+v", e)
	}
}

// Design 5.3 Accessibility queryAXTree through the runtime locator: the
// root node's handle, the role and the accessible name (when given) go to
// cdpQueryAX, and each match is an AX node with its backendDOMNodeId. An
// unknown handle is CDP's "No node with given id found".
func TestAXQuery(t *testing.T) {
	w := newW5b2Page(t)
	root := w.backendOf(t, "doc-1", 1)
	var mu sync.Mutex
	var args []string
	w.pf.setFn("cdpQueryAX", func(a json.RawMessage) any {
		mu.Lock()
		args = append(args, string(a))
		mu.Unlock()
		if strings.Contains(string(a), "Gone") {
			return rtError{"no_node", "No node with given id found"}
		}
		return map[string]any{"doc": "doc-1", "nodes": []any{
			map[string]any{"h": 7, "role": "button", "name": "Save", "disabled": true},
			map[string]any{"h": 8, "role": "button", "name": "Save", "expanded": true},
		}}
	})
	got := decode[struct{ Nodes []axNodeOut }](t, w.call(t, "Accessibility.queryAXTree", map[string]any{"backendNodeId": root, "role": "button", "accessibleName": "Save"})).Nodes
	if len(got) != 2 || got[0].Role.Value != "button" || got[0].Name.Value != "Save" || string(got[0].Properties) != `[{"name":"disabled","value":{"type":"boolean","value":true}}]` || got[0].NodeID == got[1].NodeID {
		t.Fatalf("queryAXTree = %+v", got)
	}
	if again := w.backendOf(t, "doc-1", 7); again != got[0].BackendDOMNodeID {
		t.Fatalf("match number %d, the node's backendNodeId %d", got[0].BackendDOMNodeID, again)
	}
	w.call(t, "Accessibility.queryAXTree", map[string]any{"backendNodeId": root, "role": "link"})
	if e := w.fail(t, "Accessibility.queryAXTree", map[string]any{"backendNodeId": root, "accessibleName": "Gone"}); e.Message != "No node with given id found" {
		t.Fatalf("unknown handle: %+v", e)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(args) != 3 || args[0] != `{"doc":"doc-1","h":1,"name":"Save","role":"button"}` || args[1] != `{"doc":"doc-1","h":1,"role":"link"}` {
		t.Fatalf("cdpQueryAX args %v", args)
	}
}

// Design 5.3 and 5.4: the methods marked "no effect" (DOM.enable and
// disable, Accessibility.enable and disable, Emulation focus and touch
// emulation) return success and send nothing to the page.
func TestNoEffectMethods(t *testing.T) {
	w := newW5b2Page(t)
	m := w.mark()
	for _, method := range []string{"DOM.enable", "DOM.disable", "Accessibility.enable", "Accessibility.disable"} {
		if got := w.call(t, method, map[string]any{}); string(got) != "{}" {
			t.Fatalf("%s = %s", method, got)
		}
	}
	for _, method := range []string{"Emulation.setFocusEmulationEnabled", "Emulation.setTouchEmulationEnabled"} {
		if got := w.call(t, method, map[string]any{"enabled": true}); string(got) != "{}" {
			t.Fatalf("%s = %s", method, got)
		}
	}
	if s := w.sentSince(m); len(s) != 0 {
		t.Fatalf("no-effect methods sent %v", s)
	}
}
