package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"path/filepath"
	"strings"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/logcause"
	"github.com/edxeth/agent-ios/internal/logsafe"
	"github.com/edxeth/agent-ios/internal/runtime"
	"github.com/edxeth/agent-ios/internal/upload"
)

// The DOM domain (design docs/design/agentic-parity.md 5.3, tier 1). Node
// identity follows 5.2 (handles.go): a nodeId goes to WIP unchanged, a
// backendNodeId is resolved in the page through the runtime's handles,
// and an objectId is a WIP remote object. WIP has no box model, node
// description, hit test or frame owner method, so those run as small page
// functions on the node (the js* constants below); they are sent with
// each call, as page_seed.go and internal/commands/frame.go send theirs,
// and do not use the size budget of the installed bundle (design 3.1).
// The code that needs the runtime's state (handles, the snapshot walk,
// the event dispatchers) is in the bundle (internal/runtime/js/65-cdp.js).
// The domain sends no events (W5b-2 brief; DOM.documentUpdated and
// setChildNodes are not emitted).

func init() {
	register(
		// WIP has no DOM.enable; the domain needs no enabling.
		Method{Name: "DOM.enable", Levels: OnPage},
		Method{Name: "DOM.disable", Levels: OnPage},
		Method{Name: "DOM.getDocument", Levels: OnPage, Handle: domGetDocument},
		Method{Name: "DOM.querySelector", Levels: OnPage, Handle: passCall(core.ClassRead)},
		Method{Name: "DOM.querySelectorAll", Levels: OnPage, Handle: passCall(core.ClassRead)},
		Method{Name: "DOM.describeNode", Levels: OnPage, Handle: domDescribeNode},
		Method{Name: "DOM.resolveNode", Levels: OnPage, Handle: domResolveNode},
		Method{Name: "DOM.requestNode", Levels: OnPage, Handle: domRequestNode},
		Method{Name: "DOM.getBoxModel", Levels: OnPage, Handle: nodeFunc(core.ClassRead, jsBoxModel, "model")},
		Method{Name: "DOM.getContentQuads", Levels: OnPage, Handle: nodeFunc(core.ClassRead, jsContentQuads, "quads")},
		Method{Name: "DOM.scrollIntoViewIfNeeded", Levels: OnPage, Handle: nodeFunc(core.ClassMutate, jsScrollIntoView, "")},
		Method{Name: "DOM.focus", Levels: OnPage, Handle: byNodeID(core.ClassMutate, jsFocus, "")},
		Method{Name: "DOM.getOuterHTML", Levels: OnPage, Handle: byNodeID(core.ClassRead, jsOuterHTML, "outerHTML")},
		Method{Name: "DOM.setAttributeValue", Levels: OnPage, Handle: passCall(core.ClassMutate)},
		Method{Name: "DOM.removeNode", Levels: OnPage, Handle: passCall(core.ClassMutate)},
		Method{Name: "DOM.getFrameOwner", Levels: OnPage, Handle: domGetFrameOwner},
		Method{Name: "DOM.getNodeForLocation", Levels: OnPage, Handle: domGetNodeForLocation},
		Method{Name: "DOM.setFileInputFiles", Levels: OnPage, Handle: domSetFileInputFiles},
	)
}

// passCall sends the call to WIP unchanged (CDP and WIP share its
// parameters and result) in one operation of class, through the core's
// gates: the device's iOS command list and the methods that hang through
// Target routing (core.Manager.RawAs).
func passCall(class core.Class) Handler {
	return func(ctx context.Context, call *Call) (any, Later, error) {
		var res json.RawMessage
		err := call.session.do(ctx, call.Method, class, func(ctx context.Context, mo *core.ManagedOp) error {
			var err error
			res, err = call.session.c.s.o.Manager.RawAs(ctx, mo, call.session.owner, call.Method, call.Params)
			return err
		})
		if err != nil {
			return nil, nil, err
		}
		return res, nil, nil
	}
}

// pageCall is one CDP command's work in an operation of the page: its
// fenced caller and the page runtime with the lease holder as its owner.
type pageCall struct {
	s   *session
	ctx context.Context
	mo  *core.ManagedOp
	rt  *runtime.Runtime
}

// newPageCall returns the pageCall of operation mo. The runtime's owner is the
// lease holder and its epoch (design 3.1), as for a CLI command
// (internal/commands openRuntime), and the page's reset reports reach the
// session's ref table.
func (s *session) newPageCall(ctx context.Context, mo *core.ManagedOp) *pageCall {
	ps := s.pg.PS
	daemon := ps.DaemonID
	if daemon == "" {
		daemon = runtime.DaemonID
	}
	ps.Runtime.SetOwner(runtime.Owner{Daemon: daemon, Session: s.c.rec.session, Epoch: s.epoch})
	if t := ps.Refs; t != nil {
		ps.Runtime.OnReset(func(i runtime.ResetInfo) { t.NoteReset(i.Reason, i.MaxRef) })
	}
	return &pageCall{s: s, ctx: ctx, mo: mo, rt: ps.Runtime.Via(mo.Op.Caller())}
}

// withPage runs fn in one operation of class with its pageCall.
func (s *session) withPage(ctx context.Context, name string, class core.Class, fn func(p *pageCall) error) error {
	return s.do(ctx, name, class, func(ctx context.Context, mo *core.ManagedOp) error {
		return fn(s.newPageCall(ctx, mo))
	})
}

// send sends a WIP call in the operation.
func (p *pageCall) send(method string, params any) (json.RawMessage, error) {
	return p.mo.Op.Caller().Send(p.ctx, method, params)
}

// wipCall sends a WIP call in the operation mo and decodes its reply as
// an evaluation result. It sends to the page's active target, like the
// rest of a pageCall; the Runtime domain's context-bound calls use
// boundOp.call instead (runtime_domain.go).
func wipCall(ctx context.Context, mo *core.ManagedOp, method string, params any) (wipResult, error) {
	raw, err := mo.Op.Caller().Send(ctx, method, params)
	if err != nil {
		return wipResult{}, err
	}
	var r wipResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return wipResult{}, fmt.Errorf("decoding the WIP %s reply: %w", method, err)
	}
	return r, nil
}

// nodeParams names a node the CDP way: a nodeId (WIP), a backendNodeId
// (a runtime handle, handles.go) or an objectId (a WIP remote object).
type nodeParams struct {
	NodeID        int64  `json:"nodeId"`
	BackendNodeID int64  `json:"backendNodeId"`
	ObjectID      string `json:"objectId"`
}

func (np nodeParams) given() bool {
	return np.NodeID != 0 || np.BackendNodeID != 0 || np.ObjectID != ""
}

// Chrome's errors for node parameters.
var (
	errNoNodeParam = serverError("Either nodeId, backendNodeId or objectId must be specified")
	errNoNode      = serverError("No node with given id found")
)

// errMissing: the page has no runtime of this version (the call
// expression answered runtime.MissingSentinel).
var errMissing = errors.New("the agent runtime is not installed in the page")

// jsCall is the body of the page function that runs decl, a JavaScript
// function (a, _), with this = THIS, its argument ARGS and the runtime
// internals _; it answers {v: value} or {e: message}, or the missing
// sentinel when the runtime of this version is not installed.
const jsCall = `var I=window.__iwdp;if(!I||I.v!==%q)return %q;try{return {v:(%s).call(%s,%s,I._)}}catch(e){return {e:String(e&&e.message||e)}}`

// run runs decl in the page: on the node np (nil: with this = null), with
// args. The runtime is installed once when the page has none.
func (p *pageCall) run(np *nodeParams, decl string, args any) (json.RawMessage, error) {
	v, err := p.runOnce(np, decl, args)
	if !errors.Is(err, errMissing) {
		return v, err
	}
	if err := p.rt.Call(p.ctx, "ping", nil, nil); err != nil {
		return nil, err
	}
	v, err = p.runOnce(np, decl, args)
	if errors.Is(err, errMissing) {
		return nil, runtime.ErrNotInstalled
	}
	return v, err
}

func (p *pageCall) runOnce(np *nodeParams, decl string, args any) (json.RawMessage, error) {
	a, err := json.Marshal(args) // valid JavaScript (json escapes U+2028 and U+2029)
	if err != nil {
		return nil, fmt.Errorf("encoding the page function's argument: %w", err)
	}
	var r wipResult
	switch {
	case np == nil || np.BackendNodeID != 0:
		this := "null"
		if np != nil {
			k, ok := handlesOf(p.s.pg).lookup(np.BackendNodeID)
			if !ok {
				return nil, errNoNode
			}
			ks, _ := json.Marshal(k.doc)
			this = fmt.Sprintf("I._.byHandle(%s,%d)", ks, k.h)
		}
		r, err = wipCall(p.ctx, p.mo, "Runtime.evaluate", map[string]any{
			"expression": "(function(){" + fmt.Sprintf(jsCall, runtime.Version, runtime.MissingSentinel, decl, this, a) + "})()", "returnByValue": true,
		})
	case np.ObjectID != "":
		r, err = p.callOn(np.ObjectID, decl, a)
	case np.NodeID != 0:
		var obj string
		if obj, err = p.objectOf(np.NodeID); err != nil {
			return nil, err
		}
		r, err = p.callOn(obj, decl, a)
		p.release(obj)
	default:
		return nil, errNoNodeParam
	}
	if err != nil {
		return nil, err
	}
	return pageValue(r)
}

// callOn runs decl on the remote object obj.
func (p *pageCall) callOn(obj, decl string, args json.RawMessage) (wipResult, error) {
	return wipCall(p.ctx, p.mo, "Runtime.callFunctionOn", map[string]any{
		"objectId":            obj,
		"functionDeclaration": "function(a){" + fmt.Sprintf(jsCall, runtime.Version, runtime.MissingSentinel, decl, "this", "a") + "}",
		"arguments":           []any{map[string]any{"value": args}},
		"returnByValue":       true,
	})
}

// pageValue decodes the answer of jsCall.
func pageValue(r wipResult) (json.RawMessage, error) {
	if r.WasThrown {
		return nil, serverError("the page function threw %v", r.Result["description"])
	}
	if s, ok := r.Result["value"].(string); ok && s == runtime.MissingSentinel {
		return nil, errMissing
	}
	raw, err := json.Marshal(r.Result["value"])
	if err != nil {
		return nil, err
	}
	var out struct {
		V json.RawMessage `json:"v"`
		E *string         `json:"e"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decoding the page function's answer %s: %w", raw, err)
	}
	if out.E != nil {
		return nil, serverError("%s", *out.E)
	}
	return out.V, nil
}

// objectOf resolves WIP node nodeID to a remote object, which the caller
// releases.
func (p *pageCall) objectOf(nodeID int64) (string, error) {
	raw, err := p.send("DOM.resolveNode", map[string]any{"nodeId": nodeID})
	if err != nil {
		return "", err
	}
	var res struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.Object.ObjectID == "" {
		return "", fmt.Errorf("DOM.resolveNode of node %d gave no object: %s", nodeID, raw)
	}
	return res.Object.ObjectID, nil
}

// release releases a remote object that the client never saw.
func (p *pageCall) release(obj string) {
	if _, err := p.send("Runtime.releaseObject", map[string]any{"objectId": obj}); err != nil {
		p.s.c.s.o.Logf("cdp: %s: releasing a node object of %s session %s: %v", logsafe.ID(p.s.c.name), logsafe.ID(p.s.kind), logsafe.ID(shortID(p.s.id)), logsafe.Error(err))
	}
}

// handleOf returns the runtime handle of node np.
func (p *pageCall) handleOf(np nodeParams) (handleKey, error) {
	if np.BackendNodeID != 0 {
		if k, ok := handlesOf(p.s.pg).lookup(np.BackendNodeID); ok {
			return k, nil
		}
		return handleKey{}, errNoNode
	}
	if !np.given() {
		return handleKey{}, errNoNodeParam
	}
	raw, err := p.run(&np, jsHandle, nil)
	if err != nil {
		return handleKey{}, err
	}
	var k struct {
		Doc string `json:"doc"`
		H   int64  `json:"h"`
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return handleKey{}, fmt.Errorf("decoding a node handle %s: %w", raw, err)
	}
	return handleKey{doc: k.Doc, h: k.H}, nil
}

// backendID returns the backendNodeId of handle h of document doc.
func (p *pageCall) backendID(doc string, h int64) int64 { return handlesOf(p.s.pg).id(doc, h) }

// nodeFunc is a method that runs decl on the node of its params; the
// value is the result field key ("" for an empty result).
func nodeFunc(class core.Class, decl, key string) Handler {
	return func(ctx context.Context, call *Call) (any, Later, error) {
		var np nodeParams
		if err := call.decode(&np); err != nil {
			return nil, nil, err
		}
		if !np.given() {
			return nil, nil, errNoNodeParam
		}
		var v json.RawMessage
		err := call.session.withPage(ctx, call.Method, class, func(p *pageCall) error {
			var err error
			v, err = p.run(&np, decl, nil)
			return err
		})
		if err != nil {
			return nil, nil, err
		}
		if key == "" {
			return map[string]any{}, nil, nil
		}
		return map[string]any{key: v}, nil, nil
	}
}

// byNodeID is a method that WIP has with a nodeId only: a nodeId goes to
// WIP unchanged, and a backendNodeId or an objectId runs decl, the same
// operation as a page function.
func byNodeID(class core.Class, decl, key string) Handler {
	pass, onNode := passCall(class), nodeFunc(class, decl, key)
	return func(ctx context.Context, call *Call) (any, Later, error) {
		var np nodeParams
		if err := call.decode(&np); err != nil {
			return nil, nil, err
		}
		if np.NodeID != 0 {
			return pass(ctx, call)
		}
		return onNode(ctx, call)
	}
}

// ---- getDocument ----

// cdpNodeKeys are the WIP DOM.Node fields that CDP's DOM.Node has too.
var cdpNodeKeys = []string{
	"nodeId", "nodeType", "nodeName", "localName", "nodeValue", "childNodeCount", "attributes",
	"documentURL", "baseURL", "publicId", "systemId", "xmlVersion", "name", "value", "pseudoType",
	"shadowRootType", "frameId",
}

// cdpNodeTrees are its fields that hold nodes.
var cdpNodeTrees = []string{"children", "contentDocument", "shadowRoots", "templateContent", "pseudoElements"}

// toCDPNode converts a WIP DOM.Node. WIP nodes have no backendNodeId
// (CDP requires the field): a node of a WIP tree has 0, and DOM.describeNode
// gives a node's number. No pinned client reads backendNodeId from a
// getDocument tree: agent-browser 0.41.2 (snapshot.rs 886-957) takes the
// root nodeId, then DOM.querySelectorAll and DOM.describeNode{nodeId} for
// each node's backendNodeId; Puppeteer 25.16.0 and Playwright 1.65.0 never
// send DOM.getDocument (docs/notes/w5.md, "W5b-2"). Filling the field
// would cost a WIP DOM.resolveNode per node on every call, which
// agent-browser makes on every snapshot.
func toCDPNode(n map[string]any) map[string]any {
	out := map[string]any{"backendNodeId": 0}
	for _, k := range cdpNodeKeys {
		if v, ok := n[k]; ok {
			out[k] = v
		}
	}
	for _, k := range cdpNodeTrees {
		switch v := n[k].(type) {
		case map[string]any:
			out[k] = toCDPNode(v)
		case []any:
			l := make([]any, 0, len(v))
			for _, c := range v {
				if m, ok := c.(map[string]any); ok {
					l = append(l, toCDPNode(m))
				}
			}
			out[k] = l
		}
	}
	return out
}

// domGetDocument returns WIP's document tree. depth and pierce have no
// WIP equivalent and are ignored: WIP returns its own depth.
func domGetDocument(ctx context.Context, call *Call) (any, Later, error) {
	var raw json.RawMessage
	err := call.session.withPage(ctx, call.Method, core.ClassRead, func(p *pageCall) error {
		var err error
		raw, err = p.send("DOM.getDocument", nil)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	var res struct {
		Root map[string]any `json:"root"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.Root == nil {
		return nil, nil, logcause.Errorf("decoding the WIP DOM.getDocument reply %s: %v", raw, err)
	}
	return map[string]any{"root": toCDPNode(res.Root)}, nil, nil
}

// ---- requestNode and resolveNode ----

// wipNoDocument is WIP's refusal to push a node before the client asked
// for the document (WebKit InspectorDOMAgent::pushNodePathToFrontend).
const wipNoDocument = "Document must have been requested"

// domRequestNode maps a remote object to a WIP nodeId. WIP pushes a node
// only after DOM.getDocument on its connection, and DOM.getDocument
// forgets the node ids given before, so it is sent only after WIP refused
// the push for that reason.
func domRequestNode(ctx context.Context, call *Call) (any, Later, error) {
	var np struct {
		ObjectID string `json:"objectId"`
	}
	if err := call.decode(&np); err != nil {
		return nil, nil, err
	}
	var raw json.RawMessage
	err := call.session.withPage(ctx, call.Method, core.ClassRead, func(p *pageCall) error {
		var err error
		raw, err = p.send("DOM.requestNode", map[string]any{"objectId": np.ObjectID})
		if err != nil && strings.Contains(err.Error(), wipNoDocument) {
			if _, err = p.send("DOM.getDocument", nil); err == nil {
				raw, err = p.send("DOM.requestNode", map[string]any{"objectId": np.ObjectID})
			}
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return raw, nil, nil
}

// domResolveNode returns a remote object of a node: WIP DOM.resolveNode
// for a nodeId, the runtime handle for a backendNodeId (evaluated by
// reference in the main world). The CDP utility worlds are aliases of the
// main world (design 5.2), so an executionContextId does not change where
// the node is resolved; it must still name a context of the attachment's
// current document (runtime_domain.go wipContext), otherwise the call
// fails closed with Chrome's unknown-context error, as Runtime calls do.
func domResolveNode(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		nodeParams
		ObjectGroup        string `json:"objectGroup"`
		ExecutionContextID int    `json:"executionContextId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.NodeID == 0 && p.BackendNodeID == 0 {
		return nil, nil, serverError("Either nodeId or backendNodeId must be specified")
	}
	var obj map[string]any
	err := call.session.withPage(ctx, call.Method, core.ClassRead, func(pc *pageCall) error {
		if p.ExecutionContextID != 0 {
			if _, err := pc.s.wipContext(pc.ctx, p.ExecutionContextID, ""); err != nil {
				return err
			}
		}
		if p.NodeID != 0 {
			params := map[string]any{"nodeId": p.NodeID}
			if p.ObjectGroup != "" {
				params["objectGroup"] = p.ObjectGroup
			}
			raw, err := pc.send("DOM.resolveNode", params)
			if err != nil {
				return err
			}
			var res struct {
				Object map[string]any `json:"object"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("decoding the WIP DOM.resolveNode reply %s: %w", raw, err)
			}
			obj = res.Object
			return nil
		}
		var err error
		obj, err = pc.byReference(p.BackendNodeID, p.ObjectGroup)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"object": toCDP(obj)}, nil, nil
}

// jsByHandle evaluates to the node of a handle, or the missing sentinel.
const jsByHandle = `(function(){var I=window.__iwdp;if(!I||I.v!==%q)return %q;return I._.byHandle(%s,%d)})()`

// byReference evaluates the node of backendNodeId by reference.
func (p *pageCall) byReference(backendNodeID int64, group string) (map[string]any, error) {
	k, ok := handlesOf(p.s.pg).lookup(backendNodeID)
	if !ok {
		return nil, errNoNode
	}
	ks, _ := json.Marshal(k.doc)
	params := map[string]any{"expression": fmt.Sprintf(jsByHandle, runtime.Version, runtime.MissingSentinel, ks, k.h)}
	if group != "" {
		params["objectGroup"] = group
	}
	for attempt := 0; ; attempt++ {
		r, err := wipCall(p.ctx, p.mo, "Runtime.evaluate", params)
		if err != nil {
			return nil, err
		}
		if r.WasThrown {
			return nil, errNoNode
		}
		if s, _ := r.Result["value"].(string); s != runtime.MissingSentinel {
			return r.Result, nil
		}
		if attempt > 0 {
			return nil, runtime.ErrNotInstalled
		}
		if err := p.rt.Call(p.ctx, "ping", nil, nil); err != nil {
			return nil, err
		}
	}
}

// ---- describeNode ----

// domDescribeNode describes a node from the page (WIP has no
// describeNode): backendNodeId is the node's number, frameId the frame
// that a frame owner element shows, and nodeId the given one or 0 (as
// Chrome, which gives a nodeId only for nodes pushed to the client).
// depth defaults to 1 (-1: the whole subtree); pierce is ignored.
func domDescribeNode(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		nodeParams
		Depth *int `json:"depth"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if !p.given() {
		return nil, nil, errNoNodeParam
	}
	depth := 1
	if p.Depth != nil {
		depth = *p.Depth
	}
	var node map[string]any
	err := call.session.withPage(ctx, call.Method, core.ClassRead, func(pc *pageCall) error {
		raw, err := pc.run(&p.nodeParams, jsDescribe, map[string]any{"depth": depth})
		if err != nil {
			return err
		}
		var d struct {
			Doc string         `json:"doc"`
			N   map[string]any `json:"n"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return fmt.Errorf("decoding a node description: %w", err)
		}
		var frames *frameTree
		node, err = pc.cdpNode(d.Doc, d.N, &frames)
		if err == nil && p.NodeID != 0 {
			node["nodeId"] = p.NodeID
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"node": node}, nil, nil
}

// cdpNode converts a page node: h becomes backendNodeId and fp becomes
// frameId (frames is loaded at the first frame owner).
func (p *pageCall) cdpNode(doc string, n map[string]any, frames **frameTree) (map[string]any, error) {
	h, _ := n["h"].(float64)
	delete(n, "h")
	n["backendNodeId"] = p.backendID(doc, int64(h))
	if _, ok := n["nodeId"]; !ok {
		n["nodeId"] = 0
	}
	if fp, ok := n["fp"].([]any); ok {
		delete(n, "fp")
		if *frames == nil {
			ft, err := p.frameTree()
			if err != nil {
				return nil, err
			}
			*frames = ft
		}
		if f := (*frames).at(fp); f != nil {
			n["frameId"] = f.Frame.ID
		}
	}
	if kids, ok := n["children"].([]any); ok {
		for i, c := range kids {
			if m, ok := c.(map[string]any); ok {
				cn, err := p.cdpNode(doc, m, frames)
				if err != nil {
					return nil, err
				}
				kids[i] = cn
			}
		}
	}
	return n, nil
}

// frameTree is WIP's Page.getResourceTree frame tree, with the main
// frame's CDP id (pageCall.frameTree).
type frameTree struct {
	Frame struct {
		ID string `json:"id"`
	} `json:"frame"`
	ChildFrames []*frameTree `json:"childFrames"`
}

func (p *pageCall) frameTree() (*frameTree, error) {
	raw, err := p.send("Page.getResourceTree", nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		FrameTree *frameTree `json:"frameTree"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.FrameTree == nil {
		return nil, logcause.Errorf("decoding the WIP Page.getResourceTree reply: %v", err)
	}
	// The main frame's CDP id is the page target id, as the Page domain
	// names it (pageState.frameID); child frames keep their WIP ids.
	res.FrameTree.Frame.ID = p.s.targetID()
	return res.FrameTree, nil
}

// at follows a path of child-frame indexes. The page lists a window's
// frames in document order and WIP lists its child frames in the same
// order (the assumption of internal/commands frame.go, which checks the
// counts); nil when the path leaves the tree.
func (f *frameTree) at(path []any) *frameTree {
	for _, v := range path {
		i, ok := v.(float64)
		if !ok || int(i) < 0 || int(i) >= len(f.ChildFrames) {
			return nil
		}
		f = f.ChildFrames[int(i)]
	}
	return f
}

// pathTo returns the child-frame indexes from f to frame id.
func (f *frameTree) pathTo(id string) ([]int, bool) {
	if f.Frame.ID == id {
		return []int{}, true
	}
	for i, c := range f.ChildFrames {
		if p, ok := c.pathTo(id); ok {
			return append([]int{i}, p...), true
		}
	}
	return nil, false
}

// ---- getFrameOwner and getNodeForLocation ----

// pageHandle is a node handle as the page functions return it.
type pageHandle struct {
	Doc string `json:"doc"`
	H   int64  `json:"h"`
}

// domGetFrameOwner returns the element that shows a child frame: its
// path in WIP's frame tree is followed through the page's windows.
func domGetFrameOwner(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		FrameID string `json:"frameId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	var id int64
	err := call.session.withPage(ctx, call.Method, core.ClassRead, func(pc *pageCall) error {
		ft, err := pc.frameTree()
		if err != nil {
			return err
		}
		path, ok := ft.pathTo(p.FrameID)
		if !ok || len(path) == 0 {
			return serverError("Frame with the given id was not found.")
		}
		raw, err := pc.run(nil, jsFrameOwner, map[string]any{"path": path})
		if err != nil {
			return err
		}
		var h pageHandle
		if err := json.Unmarshal(raw, &h); err != nil {
			return fmt.Errorf("decoding a frame owner: %w", err)
		}
		id = pc.backendID(h.Doc, h.H)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"backendNodeId": id}, nil, nil
}

// domGetNodeForLocation hit-tests a client point in the main document
// (runtime elementFromPoint through open shadow roots, 50-actionability.js
// at). includeUserAgentShadowDOM and ignorePointerEventsNone are ignored:
// the hit test is the page's own.
func domGetNodeForLocation(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	var out map[string]any
	err := call.session.withPage(ctx, call.Method, core.ClassRead, func(pc *pageCall) error {
		raw, err := pc.run(nil, jsNodeAt, map[string]any{"x": p.X, "y": p.Y})
		if err != nil {
			return err
		}
		var h pageHandle
		if err := json.Unmarshal(raw, &h); err != nil {
			return fmt.Errorf("decoding a hit test: %w", err)
		}
		ft, err := pc.frameTree()
		if err != nil {
			return err
		}
		out = map[string]any{"backendNodeId": pc.backendID(h.Doc, h.H), "frameId": ft.Frame.ID}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, nil, nil
}

// ---- setFileInputFiles ----

// domSetFileInputFiles reads host files and sets them on a file input
// with W3b's upload dispatcher (55-actions.js upload: a DataTransfer of
// File objects, then input and change). The input is not checked for
// actionability: CDP sets files on hidden inputs too.
func domSetFileInputFiles(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		nodeParams
		Files []string `json:"files"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if !p.given() {
		return nil, nil, errNoNodeParam
	}
	type file struct {
		Name string `json:"name"`
		Type string `json:"type"`
		B64  string `json:"b64"`
	}
	files := []file{}
	total := 0
	for _, path := range p.Files {
		data, err := upload.Read(ctx, path, upload.MaxBytes-total)
		if err != nil {
			return nil, nil, serverError("DOM.setFileInputFiles: reading %s: %v", path, err)
		}
		if total += len(data); total > upload.MaxBytes {
			return nil, nil, serverError("DOM.setFileInputFiles: the files are %d bytes or more; the limit is %d bytes (8 MiB)", total, upload.MaxBytes)
		}
		name := filepath.Base(path)
		typ := mime.TypeByExtension(filepath.Ext(name))
		if typ == "" {
			typ = "application/octet-stream"
		}
		files = append(files, file{Name: name, Type: typ, B64: base64.StdEncoding.EncodeToString(data)})
	}
	s := call.session
	err := s.withPage(ctx, call.Method, core.ClassMutate, func(pc *pageCall) error {
		k, err := pc.handleOf(p.nodeParams)
		if err != nil {
			return err
		}
		_, err = pc.act(call.Method, 1, func(int) map[string]any {
			return map[string]any{"k": "upload", "t": map[string]any{"doc": k.doc, "h": k.h}, "files": files}
		})
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{}, nil, nil
}

// ---- page functions ----

// jsFrameOffset is offset(n): the client point of n's frame's viewport
// origin in the main frame (0, 0 in the main document).
const jsFrameOffset = `function offset(n){var w=n.ownerDocument.defaultView,x=0,y=0;for(;w&&w!==window&&w.frameElement;w=w.parent){var f=w.frameElement,r=f.getBoundingClientRect();x+=r.left+f.clientLeft;y+=r.top+f.clientTop}return {x:x,y:y}}`

// jsBoxModel is CDP's BoxModel from the border box and computed style;
// quads are client points of the main frame.
const jsBoxModel = `function(){` + jsFrameOffset + `var e=this;if(e.nodeType!==1||!e.getClientRects().length)throw new Error("Could not compute box model.");
var r=e.getBoundingClientRect(),s=e.ownerDocument.defaultView.getComputedStyle(e),o=offset(e);
function n(p){return parseFloat(s.getPropertyValue(p))||0}
function q(l,t,R,b){return [l+o.x,t+o.y,R+o.x,t+o.y,R+o.x,b+o.y,l+o.x,b+o.y]}
var bl=n("border-left-width"),bt=n("border-top-width"),br=n("border-right-width"),bb=n("border-bottom-width");
var pl=n("padding-left"),pt=n("padding-top"),pr=n("padding-right"),pb=n("padding-bottom");
return {border:q(r.left,r.top,r.right,r.bottom),padding:q(r.left+bl,r.top+bt,r.right-br,r.bottom-bb),
content:q(r.left+bl+pl,r.top+bt+pt,r.right-br-pr,r.bottom-bb-pb),
margin:q(r.left-n("margin-left"),r.top-n("margin-top"),r.right+n("margin-right"),r.bottom+n("margin-bottom")),
width:Math.round(r.width),height:Math.round(r.height)}}`

// jsContentQuads is one quad per client rect (a text node: of its range).
const jsContentQuads = `function(){` + jsFrameOffset + `var n=this,rs=[],out=[],i;
if(n.nodeType===3){var g=n.ownerDocument.createRange();g.selectNodeContents(n);rs=g.getClientRects()}else if(n.nodeType===1){rs=n.getClientRects()}
var o=n.nodeType===3||n.nodeType===1?offset(n):null;
for(i=0;i<rs.length;i++){var r=rs[i];out.push([r.left+o.x,r.top+o.y,r.right+o.x,r.top+o.y,r.right+o.x,r.bottom+o.y,r.left+o.x,r.bottom+o.y])}
if(!out.length)throw new Error("Could not compute content quads.");return out}`

// jsScrollIntoView scrolls the node's element into view when it is not
// visible (scrollIntoViewIfNeeded, else centered scrollIntoView). CDP's
// rect parameter is ignored.
const jsScrollIntoView = `function(){var e=this.nodeType===1?this:this.parentElement;
if(!e||!e.isConnected)throw new Error("Node is detached from document");
if(!e.getClientRects().length)throw new Error("Node does not have a layout object");
if(e.scrollIntoViewIfNeeded){e.scrollIntoViewIfNeeded(true)}else{e.scrollIntoView({block:"center",inline:"center"})}}`

// jsFocus focuses an element; one that does not take the focus fails, as
// in Chrome.
const jsFocus = `function(){var e=this;if(e.nodeType!==1||typeof e.focus!=="function")throw new Error("Element is not focusable");
e.focus();if(e.getRootNode().activeElement!==e)throw new Error("Element is not focusable")}`

// jsOuterHTML serializes a node: an element's outerHTML, a document's
// doctype and root element, any other node through XMLSerializer.
const jsOuterHTML = `function(){var n=this;if(n.nodeType===1)return n.outerHTML;
if(n.nodeType===9)return (n.doctype?"<!DOCTYPE "+n.doctype.name+">":"")+(n.documentElement?n.documentElement.outerHTML:"");
return new XMLSerializer().serializeToString(n)}`

// jsHandle is the node's handle.
const jsHandle = `function(a,_){return {doc:_.doc(),h:_.handle(this)}}`

// jsDescribe describes the node and its children to depth a.depth (-1:
// all) with their handles; a frame owner element gets fp, the indexes of
// its frame's window among its ancestors' frames from the main window.
const jsDescribe = `function(a,_){
function fp(n){var w=n.contentWindow,p=[];if(!w)return null;
for(;w!==window;){var u=w.parent,i=0;if(u===w)return null;while(i<u.frames.length&&u.frames[i]!==w)i++;if(i===u.frames.length)return null;p.unshift(i);w=u}return p}
function d(n,depth){var o={h:_.handle(n),nodeType:n.nodeType,nodeName:n.nodeName,localName:n.localName||"",nodeValue:n.nodeValue||"",childNodeCount:n.childNodes.length},i;
if(n.attributes){o.attributes=[];for(i=0;i<n.attributes.length;i++)o.attributes.push(n.attributes[i].name,n.attributes[i].value)}
if(n.nodeType===9){o.documentURL=n.URL;o.baseURL=n.baseURI;o.xmlVersion=""}
if(n.nodeType===10){o.publicId=n.publicId;o.systemId=n.systemId}
if(n.nodeType===1&&n.contentWindow){var p=fp(n);if(p)o.fp=p}
if(depth!==0&&n.childNodes.length){o.children=[];for(i=0;i<n.childNodes.length;i++)o.children.push(d(n.childNodes[i],depth-1))}
return o}
return {doc:_.doc(),n:d(this,a.depth)}}`

// jsFrameOwner is the handle of the element that shows the frame at
// a.path (child-frame indexes from the main window).
const jsFrameOwner = `function(a,_){var w=window,i;for(i=0;i<a.path.length;i++){w=w.frames[a.path[i]];if(!w)throw new Error("Frame with the given id was not found.")}
var fs=w.parent.document.querySelectorAll("iframe,frame,object,embed");for(i=0;i<fs.length;i++){if(fs[i].contentWindow===w)return {doc:_.doc(),h:_.handle(fs[i])}}
throw new Error("Frame with the given id was not found.")}`

// jsNodeAt is the handle of the element at client point (a.x, a.y).
const jsNodeAt = `function(a,_){var e=_.at(a.x,a.y);if(!e)throw new Error("No node found at given location");return {doc:_.doc(),h:_.handle(e)}}`
