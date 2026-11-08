package cdp

import (
	"context"
	"errors"
	"strconv"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/runtime"
)

// The Accessibility domain (design docs/design/agentic-parity.md 5.3,
// tier 1). The tree is W3b's snapshot walk in full mode (40-snapshot.js
// walk with handles instead of refs; 65-cdp.js cdpAX), so its roles and
// names are the snapshot's. An AX node's nodeId and backendDOMNodeId are
// the node's backendNodeId (handles.go): a client resolves it with
// DOM.resolveNode{backendNodeId} in one runtime call. enable and disable
// have no effect.

func init() {
	register(
		Method{Name: "Accessibility.enable", Levels: OnPage},
		Method{Name: "Accessibility.disable", Levels: OnPage},
		Method{Name: "Accessibility.getFullAXTree", Levels: OnPage, Handle: axFullTree},
		Method{Name: "Accessibility.getPartialAXTree", Levels: OnPage, Handle: axPartialTree},
		Method{Name: "Accessibility.queryAXTree", Levels: OnPage, Handle: axQuery},
	)
}

// axSrc is a node of the full-mode snapshot walk.
type axSrc struct {
	H        int64   `json:"h"`
	Role     string  `json:"role"`
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Level    int     `json:"level"`
	Checked  string  `json:"checked"`
	Expanded *bool   `json:"expanded"`
	Selected bool    `json:"selected"`
	Disabled bool    `json:"disabled"`
	Required bool    `json:"required"`
	Fo       *bool   `json:"fo"`
	Fd       *bool   `json:"fd"`
	Children []axSrc `json:"children"`
}

// axTree is the answer of cdpAX.
type axTree struct {
	Doc   string  `json:"doc"`
	H     int64   `json:"h"`
	Title string  `json:"title"`
	Tree  []axSrc `json:"tree"`
}

func axValue(typ string, v any) map[string]any { return map[string]any{"type": typ, "value": v} }

// axNode builds a CDP AXNode (without childIds and parentId) from a
// snapshot node with backendNodeId id.
func axNode(id int64, n axSrc) map[string]any {
	var props []any
	prop := func(name, typ string, v any) {
		props = append(props, map[string]any{"name": name, "value": axValue(typ, v)})
	}
	if n.Checked != "" {
		prop("checked", "tristate", n.Checked)
	}
	if n.Expanded != nil {
		prop("expanded", "booleanOrUndefined", *n.Expanded)
	}
	if n.Selected {
		prop("selected", "booleanOrUndefined", true)
	}
	if n.Disabled {
		prop("disabled", "boolean", true)
	}
	if n.Required {
		prop("required", "boolean", true)
	}
	if n.Level > 0 {
		prop("level", "integer", n.Level)
	}
	if n.Fo != nil && *n.Fo {
		prop("focusable", "booleanOrUndefined", true)
	}
	if n.Fd != nil && *n.Fd {
		prop("focused", "booleanOrUndefined", true)
	}
	out := map[string]any{
		"nodeId": strconv.FormatInt(id, 10), "ignored": false, "backendDOMNodeId": id,
		"role": axValue("role", n.Role), "name": axValue("computedString", n.Name),
	}
	if n.Value != "" {
		out["value"] = axValue("string", n.Value)
	}
	if props != nil {
		out["properties"] = props
	}
	return out
}

// axNodes flattens a tree in pre-order, the root (RootWebArea, the
// document) first, as Chrome does (Puppeteer takes the first node as the
// root). depth < 0 is the whole tree; else the levels below the root.
// byID collects every node with its source and parent id.
type axFlat struct {
	nodes  []map[string]any
	src    map[int64]axSrc
	parent map[int64]int64
	kids   map[int64][]int64
}

func (p *pageCall) axFlatten(t axTree, depth int) *axFlat {
	f := &axFlat{src: map[int64]axSrc{}, parent: map[int64]int64{}, kids: map[int64][]int64{}}
	root := axSrc{H: t.H, Role: "RootWebArea", Name: t.Title, Children: t.Tree}
	var walk func(n axSrc, parent int64, level int)
	walk = func(n axSrc, parent int64, level int) {
		id := p.backendID(t.Doc, n.H)
		node := axNode(id, n)
		if parent != 0 {
			node["parentId"] = strconv.FormatInt(parent, 10)
		}
		f.nodes = append(f.nodes, node)
		f.src[id], f.parent[id] = n, parent
		if depth >= 0 && level >= depth {
			return
		}
		ids := []string{}
		for _, c := range n.Children {
			cid := p.backendID(t.Doc, c.H)
			ids = append(ids, strconv.FormatInt(cid, 10))
			f.kids[id] = append(f.kids[id], cid)
			walk(c, id, level+1)
		}
		node["childIds"] = ids
	}
	walk(root, 0, 0)
	return f
}

func (p *pageCall) axTree() (axTree, error) {
	var t axTree
	err := p.rt.Call(p.ctx, "cdpAX", nil, &t)
	return t, err
}

// axFullTree returns the page's tree. frameId other than the main frame
// is refused: same-origin child frames are part of the main tree (design
// 3.2), and other frames are not reached.
func axFullTree(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Depth   *int   `json:"depth"`
		FrameID string `json:"frameId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	depth := -1
	if p.Depth != nil {
		depth = *p.Depth
	}
	var nodes []map[string]any
	err := call.session.withPage(ctx, call.Method, core.ClassRead, func(pc *pageCall) error {
		if p.FrameID != "" {
			ft, err := pc.frameTree()
			if err != nil {
				return err
			}
			if ft.Frame.ID != p.FrameID {
				return serverError("Accessibility.getFullAXTree: frame %q is not the main frame; the tree of the main frame includes its same-origin frames", p.FrameID)
			}
		}
		t, err := pc.axTree()
		if err != nil {
			return err
		}
		nodes = pc.axFlatten(t, depth).nodes
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"nodes": nodes}, nil, nil
}

// axPartialTree returns the node's AX node first, then with fetchRelatives
// (default true) its ancestors (nearest first), its siblings and its
// children. A node that the tree does not expose is one ignored node.
func axPartialTree(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		nodeParams
		FetchRelatives *bool `json:"fetchRelatives"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	relatives := p.FetchRelatives == nil || *p.FetchRelatives
	var nodes []map[string]any
	err := call.session.withPage(ctx, call.Method, core.ClassRead, func(pc *pageCall) error {
		k, err := pc.handleOf(p.nodeParams)
		if err != nil {
			return err
		}
		t, err := pc.axTree()
		if err != nil {
			return err
		}
		id := pc.backendID(k.doc, k.h)
		f := pc.axFlatten(t, -1)
		byID := map[int64]map[string]any{}
		for _, n := range f.nodes {
			byID[n["backendDOMNodeId"].(int64)] = n
		}
		self, ok := byID[id]
		if !ok || k.doc != t.Doc {
			nodes = []map[string]any{{"nodeId": strconv.FormatInt(id, 10), "ignored": true, "backendDOMNodeId": id, "role": axValue("role", "none")}}
			return nil
		}
		nodes = []map[string]any{self}
		if !relatives {
			return nil
		}
		for a := f.parent[id]; a != 0; a = f.parent[a] {
			nodes = append(nodes, byID[a])
		}
		for _, sib := range f.kids[f.parent[id]] {
			if sib != id {
				nodes = append(nodes, byID[sib])
			}
		}
		for _, c := range f.kids[id] {
			nodes = append(nodes, byID[c])
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"nodes": nodes}, nil, nil
}

// axQuery returns the rendered elements at or below the node in the flat
// tree (open shadow roots included) whose role and accessible name match
// (65-cdp.js cdpQueryAX over the locator's elements).
func axQuery(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		nodeParams
		AccessibleName *string `json:"accessibleName"`
		Role           string  `json:"role"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	var nodes []map[string]any
	err := call.session.withPage(ctx, call.Method, core.ClassRead, func(pc *pageCall) error {
		k, err := pc.handleOf(p.nodeParams)
		if err != nil {
			return err
		}
		args := map[string]any{"doc": k.doc, "h": k.h}
		if p.AccessibleName != nil {
			args["name"] = *p.AccessibleName
		}
		if p.Role != "" {
			args["role"] = p.Role
		}
		var res struct {
			Doc   string  `json:"doc"`
			Nodes []axSrc `json:"nodes"`
		}
		if err := pc.rt.Call(pc.ctx, "cdpQueryAX", args, &res); err != nil {
			if isNoNode(err) {
				return errNoNode
			}
			return err
		}
		nodes = []map[string]any{}
		for _, n := range res.Nodes {
			nodes = append(nodes, axNode(pc.backendID(res.Doc, n.H), n))
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"nodes": nodes}, nil, nil
}

// isNoNode reports the runtime's refusal of an unknown handle.
func isNoNode(err error) bool {
	var re *runtime.Error
	return errors.As(err, &re) && re.Code == "no_node"
}
