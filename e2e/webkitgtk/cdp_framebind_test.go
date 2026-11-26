//go:build webkitgtk

package webkitgtk

import (
	"encoding/json"
	"testing"
)

// FIX-N (chrome-devtools-mcp 1.14.0 wait_for on a page with iframes):
// Puppeteer adds its utility world's bindings by executionContextName
// and calls them in every frame, and its aria/ query resolves the found
// node in the frame's utility world (DOM.resolveNode{backendNodeId,
// executionContextId}) and passes it to a call there. In the real page:
// the binding exists in the child frame, a call there names the child's
// context of the world, a call in the main frame names the main frame's,
// and a node resolved for the child's world is a valid argument of a call
// in that world.
func TestCDPFrameBindingsOnWebKitGTK(t *testing.T) {
	p := cdp045Client(t, "cdp045.html")
	p.waitChild(t)
	var tree struct {
		FrameTree struct {
			Frame       struct{ ID string }
			ChildFrames []struct {
				Frame struct{ ID string }
			}
		}
	}
	if err := json.Unmarshal(p.call(t, "Page.getFrameTree", nil), &tree); err != nil || len(tree.FrameTree.ChildFrames) != 1 {
		t.Fatalf("frame tree %+v (%v), want one child frame", tree, err)
	}
	world := func(frame string) int {
		var w struct{ ExecutionContextID int }
		if err := json.Unmarshal(p.call(t, "Page.createIsolatedWorld", map[string]any{"frameId": frame, "worldName": "fixn"}), &w); err != nil || w.ExecutionContextID == 0 {
			t.Fatalf("createIsolatedWorld in frame %s: %+v (%v)", frame, w, err)
		}
		return w.ExecutionContextID
	}
	mainWorld, childWorld := world(tree.FrameTree.Frame.ID), world(tree.FrameTree.ChildFrames[0].Frame.ID)
	p.call(t, "Runtime.addBinding", map[string]any{"name": "fixnBind", "executionContextName": "fixn"})
	for _, tc := range []struct {
		from    string
		context int
	}{{"child", childWorld}, {"main", mainWorld}} {
		seq := p.c.Seq()
		gtkValue(t, p.c, p.page, "fixnBind('from "+tc.from+"'), 1", tc.context)
		var b struct {
			Name, Payload      string
			ExecutionContextID int
		}
		if err := json.Unmarshal(gtkEvent(t, p.c, seq, p.page, "Runtime.bindingCalled", nil), &b); err != nil || b.Payload != "from "+tc.from || b.ExecutionContextID != tc.context {
			t.Errorf("call from the %s frame: bindingCalled %+v (%v), want context %d (main world %d, child world %d)", tc.from, b, err, tc.context, mainWorld, childWorld)
		}
	}

	var ev struct {
		Result struct{ ObjectID string }
	}
	_ = json.Unmarshal(p.call(t, "Runtime.evaluate", map[string]any{"expression": "document.getElementById('inner')", "contextId": childWorld}), &ev)
	var d struct {
		Node struct {
			BackendNodeID int64 `json:"backendNodeId"`
		}
	}
	if err := json.Unmarshal(p.call(t, "DOM.describeNode", map[string]any{"objectId": ev.Result.ObjectID}), &d); err != nil || d.Node.BackendNodeID == 0 {
		t.Fatalf("describeNode of the child's #inner: %+v (%v)", d, err)
	}
	var ro struct {
		Object struct{ ObjectID string }
	}
	if err := json.Unmarshal(p.call(t, "DOM.resolveNode", map[string]any{"backendNodeId": d.Node.BackendNodeID, "executionContextId": childWorld}), &ro); err != nil || ro.Object.ObjectID == "" {
		t.Fatalf("resolveNode in the child world: %+v (%v)", ro, err)
	}
	var cf struct {
		Result struct{ Value string }
	}
	if err := json.Unmarshal(p.call(t, "Runtime.callFunctionOn", map[string]any{
		"functionDeclaration": "(e) => e.id + '@' + e.ownerDocument.title", "executionContextId": childWorld,
		"arguments": []any{map[string]any{"objectId": ro.Object.ObjectID}}, "returnByValue": true,
	}), &cf); err != nil || cf.Result.Value != "inner@"+p.evalString(t, "document.getElementById('child').contentDocument.title") {
		t.Fatalf("callFunctionOn in the child world with the resolved node: %+v (%v)", cf, err)
	}
}
