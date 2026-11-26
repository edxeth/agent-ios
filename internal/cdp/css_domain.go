package cdp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/devtools/cssrules"
	"github.com/edxeth/agent-ios/internal/logsafe"
)

// The CSS domain for chrome-devtools-mcp's get_css_styles (DevTools
// CSSModel.getMatchedStyles and getComputedStyle on a node that
// DOMModel.pushNodesByBackendIdsToFrontend gave). Notes:
// /home/devkit/.pi/artifacts/parity-045/notes/WP-CSSBRIDGE.md.
//
// Opt-in per attachment: CSS.enable (answered here, never sent to WebKit,
// where it hangs through iwdp) switches the attachment's DOM domain to its
// own node ids, as DevTools' DOMModel needs them: a nodeId of such an
// attachment is the node's backendNodeId (the runtime handle number,
// handles.go), which stays valid for the node's document whatever other
// clients do, while a WIP nodeId is forgotten at every DOM.getDocument of
// the shared page connection. On such an attachment DOM.getDocument and
// DOM.requestNode answer the page's tree with these ids,
// DOM.pushNodesByBackendIdsToFrontend sends DOM.setChildNodes for the
// node's path before its reply, a new document sends DOM.documentUpdated,
// the other DOM methods read a nodeId as that backendNodeId, and the
// methods that need WIP's ids refuse. Attachments without CSS.enable keep
// WIP's node ids and get no DOM events: nothing changes for them.
//
// The CSS node methods resolve the node's handle to an object in an object
// group of their own, map it to a WIP node (cssrules.Open, which pins
// every send to the target that gave the id and fails after a document
// change), read WIP's CSS.getMatchedStylesForNode, getInlineStylesForNode
// or getComputedStyleForNode, each under cssrules.CallDeadline, and
// translate the replies to Chrome's shapes (cssrules.MatchedToCDP). The
// operation runs alone on the page (core.ClassExclusive), so no other work
// resets WIP's node ids in between. WIP gives no style sheet headers
// without CSS.enable: the first reply that names a sheet is preceded by a
// CSS.styleSheetAdded with the header known from the rule (its URL; a
// <style> element of the document is inline, without a URL, so DevTools
// writes "<style>:line" with the line inside the element).
// CSS.getAllStyleSheets and CSS.getStyleSheetText are never forwarded.

func init() {
	register(
		Method{Name: "CSS.enable", Levels: OnPage, Handle: cssEnable},
		Method{Name: "CSS.disable", Levels: OnPage, Handle: cssDisable},
		Method{Name: "CSS.getMatchedStylesForNode", Levels: OnPage, Handle: cssMatched},
		Method{Name: "CSS.getInlineStylesForNode", Levels: OnPage, Handle: cssInline},
		Method{Name: "CSS.getComputedStyleForNode", Levels: OnPage, Handle: cssComputed},
		Method{Name: "DOM.pushNodesByBackendIdsToFrontend", Levels: OnPage, Handle: domPushNodes},
	)
	registerEvents("CSS", "styleSheetAdded")
	registerEvents("DOM", "setChildNodes", "documentUpdated")
}

// cssState is the CSS opt-in of a page attachment (guarded by ps.mu).
type cssState struct {
	on     bool            // CSS.enable: own node ids and DOM events
	doc    string          // runtime document of the last DOM.getDocument; "" = none
	sheets map[string]bool // style sheets announced since CSS.enable or the last document
}

// errCSSOff is Chrome's error for a CSS method before CSS.enable.
var errCSSOff = serverError("CSS agent was not enabled")

// cssOn reports whether the attachment enabled CSS.
func (s *session) cssOn() bool {
	ps := s.page
	if ps == nil {
		return false
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.css.on
}

func cssEnable(_ context.Context, call *Call) (any, Later, error) {
	ps := call.session.page
	if ps == nil {
		return nil, nil, notFound(call.Method)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.css.on = true
	return map[string]any{}, nil, nil
}

// cssDisable forgets the announced style sheets (DevTools clears its
// headers at CSS.disable). The attachment keeps its own node ids: its DOM
// model may still hold them.
func cssDisable(_ context.Context, call *Call) (any, Later, error) {
	if ps := call.session.page; ps != nil {
		ps.mu.Lock()
		ps.css.sheets = nil
		ps.mu.Unlock()
	}
	return map[string]any{}, nil, nil
}

// cssNewDocumentLocked runs at a new document of the attachment
// (newDocumentLocked): the DOM model of a CSS attachment learns that its
// nodes are gone, and the style sheets are announced again.
func (ps *pageState) cssNewDocumentLocked() {
	if !ps.css.on {
		return
	}
	ps.css.sheets = nil
	if ps.css.doc != "" {
		ps.css.doc = ""
		ps.emitLocked("", "DOM.documentUpdated", map[string]any{})
	}
}

// ---- node ids of a CSS attachment ----

// jsTree describes nodes for DOMModel: d(n, depth) as jsDescribe, plus an
// open shadow root (shadowRoots) and a same-origin frame's document
// (contentDocument) of each element, at depth 0. With a.path, it answers
// the path of this node from the main document: for each ancestor whose
// next node on the path is one of its child nodes, the ancestor's handle
// and its children (DOM.setChildNodes); else the tree of the main
// document to a.depth.
const jsTree = `function(a,_){
function sub(n){var o=[];try{if(n.nodeType===1&&n.shadowRoot)o.push(["shadowRoots",n.shadowRoot])}catch(e){}try{if(n.nodeType===1&&n.contentDocument)o.push(["contentDocument",n.contentDocument])}catch(e){}return o}
function d(n,depth){var o={h:_.handle(n),nodeType:n.nodeType,nodeName:n.nodeName,localName:n.localName||"",nodeValue:n.nodeValue||"",childNodeCount:n.childNodes.length},i,s;
if(n.attributes){o.attributes=[];for(i=0;i<n.attributes.length;i++)o.attributes.push(n.attributes[i].name,n.attributes[i].value)}
if(n.nodeType===9){o.documentURL=n.URL;o.baseURL=n.baseURI;o.xmlVersion=""}
if(n.nodeType===10){o.publicId=n.publicId;o.systemId=n.systemId}
if(n.nodeType===11&&n.host){o.shadowRootType=n.mode==="closed"?"closed":"open"}
s=sub(n);for(i=0;i<s.length;i++){if(s[i][0]==="shadowRoots")o.shadowRoots=[d(s[i][1],0)];else o.contentDocument=d(s[i][1],0)}
if(depth!==0&&n.childNodes.length){o.children=[];for(i=0;i<n.childNodes.length;i++)o.children.push(d(n.childNodes[i],depth-1))}
return o}
function up(n){if(n.nodeType===11&&n.host)return [n.host,0];if(n.parentNode)return [n.parentNode,1];if(n.nodeType===9){try{if(n.defaultView&&n.defaultView.frameElement)return [n.defaultView.frameElement,0]}catch(e){}}return null}
if(!a.path)return {doc:_.doc(),n:d(W.document,a.depth)};
var steps=[],n=this,p;if(!n||!n.isConnected)throw new Error("Node is detached from document");
while((p=up(n))){if(p[1]){var k=[],i;for(i=0;i<p[0].childNodes.length;i++)k.push(d(p[0].childNodes[i],0));steps.unshift({h:_.handle(p[0]),kids:k})}n=p[0]}
if(n!==W.document)throw new Error("Node is not in the main document's tree");
return {doc:_.doc(),h:_.handle(this),steps:steps}}`

// treeNode is a node as jsTree describes it.
type treeNode = map[string]any

// cdpTreeNode converts a jsTree node: its handle becomes both nodeId and
// backendNodeId.
func (p *pageCall) cdpTreeNode(doc string, n treeNode) treeNode {
	h, _ := n["h"].(float64)
	delete(n, "h")
	id := p.backendID(doc, int64(h))
	n["nodeId"], n["backendNodeId"] = id, id
	if kids, ok := n["children"].([]any); ok {
		for _, c := range kids {
			if m, ok := c.(map[string]any); ok {
				p.cdpTreeNode(doc, m)
			}
		}
	}
	if l, ok := n["shadowRoots"].([]any); ok {
		for _, c := range l {
			if m, ok := c.(map[string]any); ok {
				p.cdpTreeNode(doc, m)
			}
		}
	}
	if m, ok := n["contentDocument"].(map[string]any); ok {
		p.cdpTreeNode(doc, m)
	}
	return n
}

// cssGetDocument is DOM.getDocument of a CSS attachment: the main
// document from the page, to depth (default 1, -1 all), with the
// attachment's node ids.
func cssGetDocument(ctx context.Context, call *Call) (any, Later, error) {
	var a struct {
		Depth *int `json:"depth"`
	}
	if err := call.decode(&a); err != nil {
		return nil, nil, err
	}
	depth := 1
	if a.Depth != nil {
		depth = *a.Depth
	}
	var root treeNode
	var doc string
	err := call.session.withPage(ctx, call.Method, core.ClassRead, func(p *pageCall) error {
		raw, err := p.run(nil, jsTree, map[string]any{"depth": depth})
		if err != nil {
			return err
		}
		var t struct {
			Doc string   `json:"doc"`
			N   treeNode `json:"n"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			return fmt.Errorf("decoding the document tree: %w", err)
		}
		if t.N == nil {
			return fmt.Errorf("decoding the document tree: the reply %.200s has no root node", raw)
		}
		doc, root = t.Doc, p.cdpTreeNode(t.Doc, t.N)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	ps := call.session.page
	ps.mu.Lock()
	ps.css.doc = doc
	ps.mu.Unlock()
	return map[string]any{"root": root}, nil, nil
}

// pushPath sends DOM.setChildNodes for the path of node np from the main
// document, top down, and returns its node id. A node of another document
// than the attachment's DOM model holds tells the model that its document
// is gone (DOM.documentUpdated) and gives 0.
func pushPath(ctx context.Context, call *Call, np nodeParams) (int64, error) {
	type step struct {
		H    int64      `json:"h"`
		Kids []treeNode `json:"kids"`
	}
	var path struct {
		Doc   string `json:"doc"`
		H     int64  `json:"h"`
		Steps []step `json:"steps"`
	}
	var events []map[string]any
	var id int64
	err := call.session.withPage(ctx, call.Method, core.ClassRead, func(p *pageCall) error {
		raw, err := p.run(&np, jsTree, map[string]any{"path": true})
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &path); err != nil {
			return fmt.Errorf("decoding a node path: %w", err)
		}
		for _, st := range path.Steps {
			kids := make([]any, 0, len(st.Kids))
			for _, k := range st.Kids {
				kids = append(kids, p.cdpTreeNode(path.Doc, k))
			}
			events = append(events, map[string]any{"parentId": p.backendID(path.Doc, st.H), "nodes": kids})
		}
		id = p.backendID(path.Doc, path.H)
		return nil
	})
	if err != nil {
		return 0, err
	}
	ps := call.session.page
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.css.doc != path.Doc {
		if ps.css.doc != "" {
			ps.css.doc = ""
			ps.emitLocked("", "DOM.documentUpdated", map[string]any{})
		}
		return 0, nil
	}
	for _, e := range events {
		ps.emitLocked("", "DOM.setChildNodes", e)
	}
	return id, nil
}

// domPushNodes is DOM.pushNodesByBackendIdsToFrontend of a CSS attachment:
// 0 for a node that is gone or not in the attachment's document.
func domPushNodes(ctx context.Context, call *Call) (any, Later, error) {
	if !call.session.cssOn() {
		return nil, nil, serverError("DOM.pushNodesByBackendIdsToFrontend needs CSS.enable on this session first: only then does its DOM domain use its own node ids")
	}
	var a struct {
		BackendNodeIDs []int64 `json:"backendNodeIds"`
	}
	if err := call.decode(&a); err != nil {
		return nil, nil, err
	}
	ids := make([]int64, 0, len(a.BackendNodeIDs))
	for _, b := range a.BackendNodeIDs {
		id, err := pushPath(ctx, call, nodeParams{BackendNodeID: b})
		if err != nil {
			if call.session.ctx.Err() != nil {
				return nil, nil, err
			}
			id = 0 // gone, detached or unknown, as Chrome answers
		}
		ids = append(ids, id)
	}
	return map[string]any{"nodeIds": ids}, nil, nil
}

// cssRequestNode is DOM.requestNode of a CSS attachment.
func cssRequestNode(ctx context.Context, call *Call) (any, Later, error) {
	var a struct {
		ObjectID string `json:"objectId"`
	}
	if err := call.decode(&a); err != nil {
		return nil, nil, err
	}
	id, err := pushPath(ctx, call, nodeParams{ObjectID: a.ObjectID})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"nodeId": id}, nil, nil
}

// ownNodeIDs wraps a DOM method that takes a node: on a CSS attachment,
// cssH answers when it is given, and a nodeId is read as the node's
// backendNodeId; other attachments get h unchanged.
func ownNodeIDs(h, cssH Handler) Handler {
	return func(ctx context.Context, call *Call) (any, Later, error) {
		if call.session == nil || !call.session.cssOn() {
			return h(ctx, call)
		}
		if cssH != nil {
			return cssH(ctx, call)
		}
		var p map[string]json.RawMessage
		if err := call.decode(&p); err != nil {
			return nil, nil, err
		}
		if raw, ok := p["nodeId"]; ok {
			var id int64
			if err := json.Unmarshal(raw, &id); err != nil {
				return nil, nil, &Error{Code: codeInvalidParams, Message: fmt.Sprintf("Invalid parameters: nodeId: %v", err)}
			}
			delete(p, "nodeId")
			if id != 0 {
				p["backendNodeId"] = raw
			}
			b, err := json.Marshal(p)
			if err != nil {
				return nil, nil, err
			}
			c := *call
			c.Params = b
			return h(ctx, &c)
		}
		return h(ctx, call)
	}
}

// wipNodeIDs wraps a DOM method that WebKit answers with its own node ids:
// a CSS attachment, whose ids are others, is refused.
func wipNodeIDs(h Handler) Handler {
	return ownNodeIDs(h, func(_ context.Context, call *Call) (any, Later, error) {
		return nil, nil, serverError("%s is not available on a session that sent CSS.enable: its node ids are backendNodeIds, and WebKit answers this method with its own", call.Method)
	})
}

// ---- CSS node methods ----

// jsDocURL is the URL of the node's document.
const jsDocURL = `function(){return (this.ownerDocument||this).URL}`

// cssRead runs read on the WIP node of the attachment's nodeId in one
// operation that runs alone on the page; docURL is the node's document.
func cssRead(ctx context.Context, call *Call, read func(ctx context.Context, n *cssrules.Node, docURL string) error) error {
	s := call.session
	ps := s.page
	if ps == nil || !s.cssOn() {
		return errCSSOff
	}
	var a struct {
		NodeID int64 `json:"nodeId"`
	}
	if err := call.decode(&a); err != nil {
		return err
	}
	if _, ok := handlesOf(s.pg).lookup(a.NodeID); !ok {
		return errNoNode
	}
	return s.withPage(ctx, call.Method, core.ClassExclusive, func(p *pageCall) error {
		target := cssrules.ActiveTarget(s.pg.PS.Caller)
		p.ctx = cssrules.Pin(p.ctx, target)
		group := cssrules.ObjectGroup()
		defer func() {
			if err := cssrules.Release(p.ctx, p.mo.Op.CleanupCaller(), target, group); err != nil {
				s.c.s.o.Logf("cdp: %s: releasing a CSS object group of %s session %s: %v", logsafe.ID(s.c.name), logsafe.ID(s.kind), logsafe.ID(shortID(s.id)), logsafe.Error(err))
			}
		}()
		obj, err := p.byReference(a.NodeID, group)
		if err != nil {
			return err
		}
		objectID, _ := obj["objectId"].(string)
		if objectID == "" {
			return errNoNode
		}
		r, err := p.callOn(objectID, jsDocURL, json.RawMessage("null"))
		if err != nil {
			return err
		}
		v, err := pageValue(r)
		if err != nil {
			return err
		}
		var docURL string
		if err := json.Unmarshal(v, &docURL); err != nil {
			return fmt.Errorf("decoding the node's document URL %s: %w", v, err)
		}
		n, err := cssrules.Open(p.ctx, cssrules.Conn{C: p.mo.Op.Caller(), Watch: s.pg.PS.Caller, Target: target}, objectID)
		if err != nil {
			return err
		}
		defer n.Close()
		return read(p.ctx, n, docURL)
	})
}

// announceSheetsLocked sends CSS.styleSheetAdded for each sheet the
// attachment has not seen. A sheet of the document's own <style> element
// is inline and has no URL.
func (ps *pageState) announceSheetsLocked(sheets []cssrules.Sheet, docURL string) {
	if ps.css.sheets == nil {
		ps.css.sheets = map[string]bool{}
	}
	for _, sh := range sheets {
		if ps.css.sheets[sh.ID] {
			continue
		}
		ps.css.sheets[sh.ID] = true
		inline := cssrules.IsDocumentSheet(sh.SourceURL, docURL)
		url := sh.SourceURL
		if inline {
			url = ""
		}
		ps.emitLocked("", "CSS.styleSheetAdded", map[string]any{"header": map[string]any{
			"styleSheetId": sh.ID, "frameId": ps.target, "sourceURL": url, "origin": sh.Origin, "title": "",
			"disabled": false, "isInline": inline, "isMutable": false, "isConstructed": false,
			"startLine": 0, "startColumn": 0, "length": 0, "endLine": 0, "endColumn": 0, "hasSourceURL": false,
		}})
	}
}

func cssMatched(ctx context.Context, call *Call) (any, Later, error) {
	var out map[string]any
	var sheets []cssrules.Sheet
	var doc string
	err := cssRead(ctx, call, func(ctx context.Context, n *cssrules.Node, docURL string) error {
		var m cssrules.Matched
		if err := n.CallInto(ctx, "CSS.getMatchedStylesForNode", cssrules.MatchedParams, &m); err != nil {
			return err
		}
		var in cssrules.Inline
		if err := n.CallInto(ctx, "CSS.getInlineStylesForNode", nil, &in); err != nil {
			return err
		}
		out, sheets = cssrules.MatchedToCDP(&m, &in)
		doc = docURL
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	ps := call.session.page
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.announceSheetsLocked(sheets, doc)
	return out, nil, nil
}

func cssInline(ctx context.Context, call *Call) (any, Later, error) {
	var out map[string]any
	err := cssRead(ctx, call, func(ctx context.Context, n *cssrules.Node, _ string) error {
		var in cssrules.Inline
		if err := n.CallInto(ctx, "CSS.getInlineStylesForNode", nil, &in); err != nil {
			return err
		}
		out = cssrules.InlineToCDP(&in)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, nil, nil
}

func cssComputed(ctx context.Context, call *Call) (any, Later, error) {
	var c cssrules.Computed
	err := cssRead(ctx, call, func(ctx context.Context, n *cssrules.Node, _ string) error {
		return n.CallInto(ctx, "CSS.getComputedStyleForNode", nil, &c)
	})
	if err != nil {
		return nil, nil, err
	}
	if c.ComputedStyle == nil {
		c.ComputedStyle = []cssrules.ComputedProperty{}
	}
	return map[string]any{"computedStyle": c.ComputedStyle}, nil, nil
}
