package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/runtime"
	"github.com/nnemirovsky/iwdp-cli/internal/upload"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Design 5.2 "Node ids": DOM.describeNode gives the node's runtime handle
// as a stable backendNodeId, and DOM.resolveNode{backendNodeId} evaluates
// exactly that handle; an unknown number fails.
func TestBackendNodeIDIsTheRuntimeHandle(t *testing.T) {
	r := newRig(t)
	pf := newPageFake(r.fake["One"])
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	pf.setPage(func(src, this string, args json.RawMessage) any {
		return map[string]any{"v": map[string]any{"doc": "doc-1", "n": map[string]any{"h": 5, "nodeType": 1, "nodeName": "B"}}}
	})
	type d struct {
		Node struct{ BackendNodeID int64 } `json:"node"`
	}
	a := decode[d](t, call(t, c, page, "DOM.describeNode", map[string]any{"objectId": "o1"}))
	b := decode[d](t, call(t, c, page, "DOM.describeNode", map[string]any{"objectId": "o2"}))
	if a.Node.BackendNodeID == 0 || a.Node.BackendNodeID != b.Node.BackendNodeID {
		t.Fatalf("describeNode numbers %d and %d", a.Node.BackendNodeID, b.Node.BackendNodeID)
	}
	r.fake["One"].Handle("Runtime.evaluate", func(fc fakewebkit.Call) (any, *fakewebkit.Error) {
		if strings.Contains(string(fc.Params), `byHandle(\"doc-1\",5)`) {
			return map[string]any{"result": map[string]any{"type": "object", "objectId": "rt-5"}}, nil
		}
		return nil, &fakewebkit.Error{Code: -32000, Message: "unexpected"}
	})
	ro := decode[struct{ Object map[string]any }](t, call(t, c, page, "DOM.resolveNode", map[string]any{"backendNodeId": a.Node.BackendNodeID}))
	if ro.Object["objectId"] != "rt-5" {
		t.Fatalf("resolveNode = %v", ro.Object)
	}
	if _, err := c.Call(ctxFor(t, 5*time.Second), page, "DOM.resolveNode", map[string]any{"backendNodeId": 999999}); err == nil || !strings.Contains(err.Error(), "No node with given id found") {
		t.Fatalf("unknown number: %v", err)
	}
}

// pageScript answers the page functions of dom_domain.go by their source:
// the first entry whose function constant the call contains answers.
type pageScript []struct {
	fn string
	v  func(this string, args json.RawMessage) any
}

func (ps pageScript) on(pf *pageFake) {
	pf.setPage(func(src, this string, args json.RawMessage) any {
		for _, e := range ps {
			if strings.Contains(src, e.fn) {
				return e.v(this, args)
			}
		}
		return map[string]any{"e": "unscripted page function"}
	})
}

// pageAnswer answers v from a page function.
func pageAnswer(v any) func(string, json.RawMessage) any {
	return func(string, json.RawMessage) any { return map[string]any{"v": v} }
}

// backendOf returns the backendNodeId of handle h of document doc, as
// DOM.getNodeForLocation hands it out. It replaces the page script.
func (w *w5b2Page) backendOf(t *testing.T, doc string, h int) int64 {
	t.Helper()
	pageScript{{jsNodeAt, pageAnswer(map[string]any{"doc": doc, "h": h})}}.on(w.pf)
	w.f.Result("Page.getResourceTree", map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "main"}}})
	return decode[struct{ BackendNodeID int64 }](t, w.call(t, "DOM.getNodeForLocation", map[string]any{"x": 1, "y": 1})).BackendNodeID
}

// Design 5.3 DOM getDocument (CDP DOM.Node, WIP DOM.Node): WIP's tree is
// returned with the fields that CDP's Node has, through every subtree
// (children, contentDocument, shadowRoots, templateContent,
// pseudoElements); WIP-only fields are dropped. A WIP node has no
// backendNodeId and CDP requires the field, so it is 0: no pinned client
// reads it from a getDocument tree (docs/notes/w5.md, "W5b-2"). depth and
// pierce have no WIP parameter and are not sent.
func TestDOMGetDocumentConvertsWIPNodes(t *testing.T) {
	w := newW5b2Page(t)
	w.f.Result("DOM.getDocument", map[string]any{"root": map[string]any{
		"nodeId": 1, "nodeType": 9, "nodeName": "#document", "localName": "", "nodeValue": "", "childNodeCount": 1,
		"documentURL": "https://one.example/", "baseURL": "https://one.example/", "xmlVersion": "", "layoutFlags": []string{"rendered"},
		"children": []any{map[string]any{
			"nodeId": 2, "nodeType": 1, "nodeName": "HTML", "localName": "html", "attributes": []string{"lang", "en"}, "customElementState": "builtin",
			"shadowRoots":     []any{map[string]any{"nodeId": 3, "nodeType": 11, "shadowRootType": "open", "contentSecurityPolicyHash": "x"}},
			"templateContent": map[string]any{"nodeId": 4, "nodeType": 11},
			"pseudoElements":  []any{map[string]any{"nodeId": 5, "nodeType": 1, "pseudoType": "before"}},
			"contentDocument": map[string]any{"nodeId": 6, "nodeType": 9, "frameId": "f1", "children": []any{map[string]any{"nodeId": 7, "nodeType": 1, "name": "n", "value": "v"}}},
		}},
	}})
	m := w.mark()
	got := w.call(t, "DOM.getDocument", map[string]any{"depth": -1, "pierce": true})
	want := `{"root":{"backendNodeId":0,"baseURL":"https://one.example/","childNodeCount":1,"children":[{"attributes":["lang","en"],"backendNodeId":0,` +
		`"contentDocument":{"backendNodeId":0,"children":[{"backendNodeId":0,"name":"n","nodeId":7,"nodeType":1,"value":"v"}],"frameId":"f1","nodeId":6,"nodeType":9},` +
		`"localName":"html","nodeId":2,"nodeName":"HTML","nodeType":1,"pseudoElements":[{"backendNodeId":0,"nodeId":5,"nodeType":1,"pseudoType":"before"}],` +
		`"shadowRoots":[{"backendNodeId":0,"nodeId":3,"nodeType":11,"shadowRootType":"open"}],"templateContent":{"backendNodeId":0,"nodeId":4,"nodeType":11}}],` +
		`"documentURL":"https://one.example/","localName":"","nodeId":1,"nodeName":"#document","nodeType":9,"nodeValue":"","xmlVersion":""}}`
	if string(got) != want {
		t.Fatalf("getDocument:\n%s\nwant\n%s", got, want)
	}
	if s := w.sentSince(m); len(s) != 1 || s[0] != "DOM.getDocument " {
		t.Fatalf("the page received %v, want DOM.getDocument without params", s)
	}
}

// Design 5.3 DOM: querySelector, querySelectorAll, setAttributeValue and
// removeNode have the same parameters and results in CDP and WIP, so they
// go to WIP unchanged; focus and getOuterHTML with a nodeId go to WIP's
// methods of the same name.
func TestDOMPassThroughMethods(t *testing.T) {
	w := newW5b2Page(t)
	cases := []struct {
		method, params, result string
	}{
		{"DOM.querySelector", `{"nodeId":1,"selector":"#a"}`, `{"nodeId":7}`},
		{"DOM.querySelectorAll", `{"nodeId":1,"selector":"p"}`, `{"nodeIds":[8,9]}`},
		{"DOM.setAttributeValue", `{"name":"title","nodeId":7,"value":"t"}`, `{}`},
		{"DOM.removeNode", `{"nodeId":9}`, `{}`},
		{"DOM.focus", `{"nodeId":7}`, `{}`},
		{"DOM.getOuterHTML", `{"nodeId":7}`, `{"outerHTML":"<p id=\"a\"></p>"}`},
	}
	ran := len(w.pf.pageFns())
	for _, c := range cases {
		w.f.Result(c.method, json.RawMessage(c.result))
		m := w.mark()
		got := w.call(t, c.method, json.RawMessage(c.params))
		if jsonOf(t, decode[any](t, got)) != jsonOf(t, decode[any](t, json.RawMessage(c.result))) {
			t.Fatalf("%s = %s, want %s", c.method, got, c.result)
		}
		if s := w.sentSince(m); len(s) != 1 || s[0] != c.method+" "+c.params {
			t.Fatalf("%s: the page received %v, want the call unchanged", c.method, s)
		}
	}
	if n := len(w.pf.pageFns()) - ran; n != 0 {
		t.Fatalf("pass-through methods ran %d page functions", n)
	}
}

// Design 5.3 DOM describeNode (WIP has none): the node and its children to
// depth (default 1, -1 all) from the page, each with its backendNodeId; a
// frame owner gets the frameId of its frame from WIP's frame tree. A node
// is named by objectId (the page function runs on it), nodeId (WIP
// DOM.resolveNode, then the function, then the object is released) or
// backendNodeId (the runtime handle). nodeId is the given one, else 0, as
// in Chrome for a node never pushed to the client; no pinned client reads
// it (docs/notes/w5.md, "W5b-2").
func TestDOMDescribeNode(t *testing.T) {
	w := newW5b2Page(t)
	img := w.backendOf(t, "doc-1", 3)
	w.f.Result("Page.getResourceTree", map[string]any{"frameTree": map[string]any{
		"frame":       map[string]any{"id": "main"},
		"childFrames": []any{map[string]any{"frame": map[string]any{"id": "child-0"}}, map[string]any{"frame": map[string]any{"id": "child-1"}}},
	}})
	w.f.Result("DOM.resolveNode", map[string]any{"object": map[string]any{"type": "object", "objectId": "wip-obj"}})
	desc := map[string]any{"doc": "doc-1", "n": map[string]any{
		"h": 2, "nodeType": 1, "nodeName": "DIV", "localName": "div", "childNodeCount": 2, "attributes": []string{"id", "d"},
		"children": []any{map[string]any{"h": 3, "nodeType": 1, "nodeName": "IMG", "localName": "img", "childNodeCount": 0}, map[string]any{"h": 4, "nodeType": 1, "nodeName": "IFRAME", "fp": []int{1}}},
	}}
	pageScript{{jsDescribe, pageAnswer(desc)}}.on(w.pf)
	type node struct {
		NodeID, BackendNodeID int64
		NodeName, FrameID     string
		Children              []node
	}
	type res struct{ Node node }
	n := decode[res](t, w.call(t, "DOM.describeNode", map[string]any{"objectId": "o1"})).Node
	if n.NodeID != 0 || n.BackendNodeID == 0 || n.NodeName != "DIV" || len(n.Children) != 2 || n.Children[0].BackendNodeID != img ||
		n.Children[1].FrameID != "child-1" || n.Children[1].BackendNodeID == n.BackendNodeID || n.Children[0].NodeID != 0 {
		t.Fatalf("describeNode{objectId} = %+v (img %d)", n, img)
	}
	m := w.mark()
	byID := decode[res](t, w.call(t, "DOM.describeNode", map[string]any{"nodeId": 12, "depth": -1})).Node
	if byID.NodeID != 12 || byID.BackendNodeID != n.BackendNodeID {
		t.Fatalf("describeNode{nodeId} = %+v, want nodeId 12 and the same backendNodeId", byID)
	}
	if got := w.methodsSince(m); strings.Join(got, " ") != "DOM.resolveNode Runtime.callFunctionOn Runtime.releaseObject Page.getResourceTree" {
		t.Fatalf("describeNode{nodeId} sent %v", got)
	}
	byBackend := decode[res](t, w.call(t, "DOM.describeNode", map[string]any{"backendNodeId": img})).Node
	if byBackend.BackendNodeID != n.BackendNodeID { // the scripted page describes the same node
		t.Fatalf("describeNode{backendNodeId} = %+v", byBackend)
	}
	var thisOf, argsOf []string
	for _, f := range w.pf.pageFns() {
		if strings.Contains(f.src, jsDescribe) {
			thisOf, argsOf = append(thisOf, f.this), append(argsOf, string(f.args))
		}
	}
	if strings.Join(thisOf, " ") != `o1 wip-obj "doc-1"#3` || argsOf[0] != `{"depth":1}` || argsOf[1] != `{"depth":-1}` {
		t.Fatalf("describe ran on %v with %v", thisOf, argsOf)
	}
	if e := w.fail(t, "DOM.describeNode", map[string]any{}); e.Message != "Either nodeId, backendNodeId or objectId must be specified" {
		t.Fatalf("describeNode without a node: %+v", e)
	}
}

// Design 5.3 DOM resolveNode: a nodeId goes to WIP DOM.resolveNode with the
// object group, and its remote object is converted to CDP's (WIP's
// preview has another shape and is dropped); without a nodeId or
// backendNodeId it fails.
func TestDOMResolveNodeByNodeID(t *testing.T) {
	w := newW5b2Page(t)
	w.f.Result("DOM.resolveNode", map[string]any{"object": map[string]any{"type": "object", "subtype": "node", "className": "HTMLDivElement", "description": "div#a", "objectId": "obj-1", "preview": map[string]any{"lossless": false}}})
	m := w.mark()
	got := w.call(t, "DOM.resolveNode", map[string]any{"nodeId": 7, "objectGroup": "g"})
	if string(got) != `{"object":{"className":"HTMLDivElement","description":"div#a","objectId":"obj-1","subtype":"node","type":"object"}}` {
		t.Fatalf("resolveNode = %s", got)
	}
	if s := w.sentSince(m); len(s) != 1 || s[0] != `DOM.resolveNode {"nodeId":7,"objectGroup":"g"}` {
		t.Fatalf("the page received %v", s)
	}
	if e := w.fail(t, "DOM.resolveNode", map[string]any{"objectGroup": "g"}); e.Message != "Either nodeId or backendNodeId must be specified" {
		t.Fatalf("resolveNode without a node: %+v", e)
	}
}

// Design 5.3 DOM requestNode: WIP pushes a node only after DOM.getDocument
// on its connection (WebKit InspectorDOMAgent::pushNodePathToFrontend,
// "Document must have been requested"), and DOM.getDocument forgets the
// node ids given before, so it is sent only after that refusal, then the
// push again. Another error is the command's error.
func TestDOMRequestNodeRetriesAfterGetDocument(t *testing.T) {
	w := newW5b2Page(t)
	var documented atomic.Bool
	w.f.Handle("DOM.getDocument", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		documented.Store(true)
		return map[string]any{"root": map[string]any{"nodeId": 1}}, nil
	})
	w.f.Handle("DOM.requestNode", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if strings.Contains(string(c.Params), "bad") {
			return nil, &fakewebkit.Error{Code: -32000, Message: "Object id doesn't reference a Node"}
		}
		if !documented.Load() {
			return nil, &fakewebkit.Error{Code: -32000, Message: "Document must have been requested"}
		}
		return map[string]any{"nodeId": 42}, nil
	})
	m := w.mark()
	if got := w.call(t, "DOM.requestNode", map[string]any{"objectId": "o1"}); string(got) != `{"nodeId":42}` {
		t.Fatalf("requestNode = %s", got)
	}
	if got := w.methodsSince(m); strings.Join(got, " ") != "DOM.requestNode DOM.getDocument DOM.requestNode" {
		t.Fatalf("first requestNode sent %v", got)
	}
	m = w.mark()
	w.call(t, "DOM.requestNode", map[string]any{"objectId": "o2"})
	if got := w.methodsSince(m); strings.Join(got, " ") != "DOM.requestNode" {
		t.Fatalf("requestNode after the document sent %v, want no second getDocument", got)
	}
	m = w.mark()
	if e := w.fail(t, "DOM.requestNode", map[string]any{"objectId": "bad"}); !strings.Contains(e.Message, "doesn't reference a Node") {
		t.Fatalf("requestNode of a non-node: %+v", e)
	}
	if got := w.methodsSince(m); strings.Join(got, " ") != "DOM.requestNode" {
		t.Fatalf("a failed requestNode sent %v", got)
	}
}

// nodeFnScript answers the node page functions of TestDOMNodeFunctions.
func nodeFnScript(pf *pageFake) {
	pageScript{
		{jsBoxModel, func(this string, _ json.RawMessage) any {
			if this == "hidden" {
				return map[string]any{"e": "Could not compute box model."}
			}
			return map[string]any{"v": map[string]any{"content": []int{1, 1, 9, 1, 9, 9, 1, 9}, "width": 10, "height": 10}}
		}},
		{jsContentQuads, pageAnswer([][]int{{0, 0, 5, 0, 5, 5, 0, 5}})},
		{jsScrollIntoView, pageAnswer(nil)},
		{jsFocus, pageAnswer(nil)},
		{jsOuterHTML, pageAnswer("<b>x</b>")},
	}.on(pf)
}

// Design 5.3 DOM getBoxModel and getContentQuads use runtime rects (WIP has
// no box model method), scrollIntoViewIfNeeded scrolls in the page, focus
// and getOuterHTML without a nodeId run on the node in the page: each is
// its page function on the given node, and its result is CDP's field
// (model, quads, outerHTML; empty for scroll and focus). A function that
// throws gives its message as the CDP error.
func TestDOMNodeFunctions(t *testing.T) {
	w := newW5b2Page(t)
	b := w.backendOf(t, "doc-1", 9)
	nodeFnScript(w.pf)
	cases := []struct {
		method string
		params map[string]any
		want   string
		fn     string
		this   string
	}{
		{"DOM.getBoxModel", map[string]any{"objectId": "o1"}, `{"model":{"content":[1,1,9,1,9,9,1,9],"height":10,"width":10}}`, jsBoxModel, "o1"},
		{"DOM.getContentQuads", map[string]any{"backendNodeId": b}, `{"quads":[[0,0,5,0,5,5,0,5]]}`, jsContentQuads, `"doc-1"#9`},
		{"DOM.scrollIntoViewIfNeeded", map[string]any{"objectId": "o2", "rect": map[string]any{"x": 0, "y": 0, "width": 1, "height": 1}}, `{}`, jsScrollIntoView, "o2"},
		{"DOM.focus", map[string]any{"objectId": "o3"}, `{}`, jsFocus, "o3"},
		{"DOM.getOuterHTML", map[string]any{"backendNodeId": b}, `{"outerHTML":"<b>x</b>"}`, jsOuterHTML, `"doc-1"#9`},
	}
	for _, c := range cases {
		before := len(w.pf.pageFns())
		if got := w.call(t, c.method, c.params); jsonOf(t, decode[any](t, got)) != jsonOf(t, decode[any](t, json.RawMessage(c.want))) {
			t.Fatalf("%s = %s, want %s", c.method, got, c.want)
		}
		ran := w.pf.pageFns()[before:]
		if len(ran) != 1 || !strings.Contains(ran[0].src, c.fn) || ran[0].this != c.this {
			t.Fatalf("%s ran %+v, want its page function on %s", c.method, ran, c.this)
		}
	}
	if e := w.fail(t, "DOM.getBoxModel", map[string]any{"objectId": "hidden"}); e.Message != "Could not compute box model." {
		t.Fatalf("box model of a hidden node: %+v", e)
	}
	for _, m := range []string{"DOM.getBoxModel", "DOM.getContentQuads", "DOM.scrollIntoViewIfNeeded", "DOM.focus", "DOM.getOuterHTML"} {
		if e := w.fail(t, m, map[string]any{}); e.Message != "Either nodeId, backendNodeId or objectId must be specified" {
			t.Fatalf("%s without a node: %+v", m, e)
		}
	}
}

// A page without the runtime of this version (the page function answers
// the missing sentinel) gets the runtime installed (a runtime call, ping),
// and the page function runs once more.
func TestDOMPageFunctionInstallsTheRuntime(t *testing.T) {
	w := newW5b2Page(t)
	var calls atomic.Int32
	pageScript{{jsContentQuads, func(string, json.RawMessage) any {
		if calls.Add(1) == 1 {
			return runtime.MissingSentinel
		}
		return map[string]any{"v": [][]int{{1}}}
	}}}.on(w.pf)
	m := w.mark()
	if got := w.call(t, "DOM.getContentQuads", map[string]any{"objectId": "o1"}); string(got) != `{"quads":[[1]]}` || calls.Load() != 2 {
		t.Fatalf("getContentQuads = %s after %d page calls", got, calls.Load())
	}
	if got := strings.Join(w.methodsSince(m), " "); !strings.HasPrefix(got, "Runtime.callFunctionOn Runtime.evaluate") || !strings.HasSuffix(got, "Runtime.callFunctionOn") {
		t.Fatalf("the page received %s, want the function, the install and the function again", got)
	}
}

// Design 5.3 DOM getFrameOwner: the frame's path in WIP's frame tree
// (child indexes from the main frame) is followed in the page, and the
// owner element's handle is its backendNodeId. The main frame has no
// owner, and an unknown frame fails (Chrome: "Frame with the given id was
// not found.").
func TestDOMGetFrameOwner(t *testing.T) {
	w := newW5b2Page(t)
	owner := w.backendOf(t, "doc-1", 11)
	w.f.Result("Page.getResourceTree", map[string]any{"frameTree": map[string]any{
		"frame": map[string]any{"id": "main"},
		"childFrames": []any{
			map[string]any{"frame": map[string]any{"id": "a"}},
			map[string]any{"frame": map[string]any{"id": "b"}, "childFrames": []any{map[string]any{"frame": map[string]any{"id": "c"}}}},
		},
	}})
	pageScript{{jsFrameOwner, pageAnswer(map[string]any{"doc": "doc-1", "h": 11})}}.on(w.pf)
	got := decode[struct{ BackendNodeID int64 }](t, w.call(t, "DOM.getFrameOwner", map[string]any{"frameId": "c"}))
	if got.BackendNodeID != owner {
		t.Fatalf("getFrameOwner = %d, want %d", got.BackendNodeID, owner)
	}
	ran := w.pf.pageFns()
	if last := ran[len(ran)-1]; string(last.args) != `{"path":[1,0]}` || last.this != "null" {
		t.Fatalf("frame owner ran with %s on %s, want path [1,0]", last.args, last.this)
	}
	for _, f := range []string{"main", "nope"} {
		if e := w.fail(t, "DOM.getFrameOwner", map[string]any{"frameId": f}); e.Message != "Frame with the given id was not found." {
			t.Fatalf("getFrameOwner %s: %+v", f, e)
		}
	}
}

// Design 5.3 DOM getNodeForLocation: the page's hit test at the client
// point (runtime elementFromPoint) gives the node's backendNodeId and the
// main frame's CDP id, the page target id (as Page.getFrameTree names it,
// not WIP's "main-frame"); no node there fails.
func TestDOMGetNodeForLocation(t *testing.T) {
	w := newW5b2Page(t)
	w.f.Result("Page.getResourceTree", map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "main-frame"}}})
	pageScript{{jsNodeAt, func(_ string, a json.RawMessage) any {
		if string(a) == `{"x":0,"y":0}` {
			return map[string]any{"e": "No node found at given location"}
		}
		return map[string]any{"v": map[string]any{"doc": "doc-1", "h": 21}}
	}}}.on(w.pf)
	if e := w.fail(t, "DOM.getNodeForLocation", map[string]any{"x": 0, "y": 0}); e.Message != "No node found at given location" {
		t.Fatalf("no node: %+v", e)
	}
	got := decode[struct {
		BackendNodeID int64
		FrameID       string
	}](t, w.call(t, "DOM.getNodeForLocation", map[string]any{"x": 15.5, "y": 30, "includeUserAgentShadowDOM": true}))
	if got.FrameID != w.pageTargetID(t) {
		t.Fatalf("getNodeForLocation = %+v", got)
	}
	if ran := w.pf.pageFns(); string(ran[len(ran)-1].args) != `{"x":15.5,"y":30}` || ran[len(ran)-1].this != "null" {
		t.Fatalf("hit test ran with %s on %s", ran[len(ran)-1].args, ran[len(ran)-1].this)
	}
	if again := w.backendOf(t, "doc-1", 21); got.BackendNodeID == 0 || got.BackendNodeID != again {
		t.Fatalf("getNodeForLocation = %d, the same node again %d", got.BackendNodeID, again)
	}
	if other := w.backendOf(t, "doc-1", 22); other == got.BackendNodeID {
		t.Fatalf("another node got the same number %d", other)
	}
}

// Design 5.3 DOM setFileInputFiles: the host files are read and set on the
// input by W3b's upload dispatcher (one forced upload step on the node's
// handle with name, MIME type by extension and base64 content). A node by
// objectId gets its handle from the page first. A missing file or more
// than 8 MiB (design 3.5) fails before anything is dispatched.
func TestDOMSetFileInputFiles(t *testing.T) {
	w := newW5b2Page(t)
	dir := t.TempDir()
	txt, bin := filepath.Join(dir, "a.txt"), filepath.Join(dir, "blob.zzunknown")
	if err := os.WriteFile(txt, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	input := w.backendOf(t, "doc-1", 31)
	w.call(t, "DOM.setFileInputFiles", map[string]any{"backendNodeId": input, "files": []string{txt, bin}})
	pageScript{{jsHandle, pageAnswer(map[string]any{"doc": "doc-1", "h": 32})}}.on(w.pf)
	w.call(t, "DOM.setFileInputFiles", map[string]any{"objectId": "o-input", "files": []string{}})
	b64 := base64.StdEncoding.EncodeToString
	sameLines(t, "upload steps", stepKeys(w.pf.steps(), "k", "t", "files", "f"), []string{
		`k="upload" t={"doc":"doc-1","h":31} files=[{"b64":"` + b64([]byte("hello")) + `","name":"a.txt","type":"text/plain; charset=utf-8"},{"b64":"` + b64([]byte{0, 1, 2}) + `","name":"blob.zzunknown","type":"application/octet-stream"}] f=true`,
		`k="upload" t={"doc":"doc-1","h":32} files=[] f=true`,
	})
	big := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(big, make([]byte, upload.MaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if e := w.fail(t, "DOM.setFileInputFiles", map[string]any{"backendNodeId": input, "files": []string{big}}); !strings.Contains(e.Message, "the limit is 8388608 bytes (8 MiB)") {
		t.Fatalf("an upload over 8 MiB: %+v", e)
	}
	if e := w.fail(t, "DOM.setFileInputFiles", map[string]any{"backendNodeId": input, "files": []string{filepath.Join(dir, "missing")}}); !strings.Contains(e.Message, "reading "+filepath.Join(dir, "missing")) {
		t.Fatalf("a missing file: %+v", e)
	}
	if e := w.fail(t, "DOM.setFileInputFiles", map[string]any{"files": []string{txt}}); e.Message != "Either nodeId, backendNodeId or objectId must be specified" {
		t.Fatalf("no node: %+v", e)
	}
	if n := len(w.pf.steps()); n != 2 {
		t.Fatalf("refused uploads dispatched (%d steps)", n)
	}
}

// Review finding F5 (W5b-2 review): an upload refuses a FIFO (a special
// file whose read can block) without blocking, also when a writer holds it
// open with no data. The reviewer's reproduction, with its assertion
// unchanged.
func TestUploadDoesNotBlockOnFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	hold, err := os.OpenFile(fifo, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	params, _ := json.Marshal(map[string]any{"nodeId": 1, "files": []string{fifo, filepath.Join(dir, "missing")}})
	done := make(chan struct{})
	go func() { _, _, _ = domSetFileInputFiles(ctx, &Call{Params: params}); close(done) }()
	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):
		_ = hold.Close() // release this review-owned FIFO, so the handler can finish
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("handler failed to finish after FIFO was released")
		}
		t.Fatal("DOM.setFileInputFiles blocked on a FIFO beyond the operation deadline; reject special files before reading")
	}
}

// uploadErr runs DOM.setFileInputFiles for files on node 1 without a page:
// each case fails while reading, before the page is used.
func uploadErr(ctx context.Context, t *testing.T, files ...string) error {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"nodeId": 1, "files": files})
	_, _, err := domSetFileInputFiles(ctx, &Call{Params: params})
	if err == nil {
		t.Fatalf("DOM.setFileInputFiles %v succeeded, want a read error", files)
	}
	return err
}

// Review finding F5: a regular file over the 8 MiB limit (design 3.5) is
// refused after at most the limit plus one byte: a sparse file of 1 TiB is
// refused at once, where reading it whole would need 1 TiB of memory. A
// special file is named in the error.
func TestUploadRefusesALargeFileWithoutReadingIt(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "sparse.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 40); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := uploadErr(ctx, t, big); !strings.Contains(err.Error(), "the limit is 8388608 bytes (8 MiB)") {
		t.Fatalf("an upload of a 1 TiB sparse file: %v, want the limit error", err)
	}
	small := filepath.Join(dir, "half.bin")
	if err := os.WriteFile(small, make([]byte, upload.MaxBytes/2+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := uploadErr(ctx, t, small, small); !strings.Contains(err.Error(), "the limit is 8388608 bytes (8 MiB)") {
		t.Fatalf("two files of half the limit plus one byte: %v, want the limit error", err)
	}
	if err := uploadErr(ctx, t, dir); !strings.Contains(err.Error(), "reading "+dir+": not a regular file") {
		t.Fatalf("an upload of a directory: %v", err)
	}
}

// Review finding F5: the reads stop when the command's context ends.
func TestUploadStopsWhenCanceled(t *testing.T) {
	txt := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(txt, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := uploadErr(ctx, t, txt); !strings.Contains(err.Error(), "reading "+txt+": context canceled") {
		t.Fatalf("an upload with a canceled context: %v", err)
	}
}
