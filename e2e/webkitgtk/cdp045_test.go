//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
)

// cdp045Client starts the daemon and the CDP endpoint on the fixture page
// and returns a CDP client of session s1 with a page attachment, as
// Puppeteer makes it.
func cdp045Client(t *testing.T, page string) *cdpPage {
	t.Helper()
	addr := runtimeBrowser(t, page)
	if r := cli(t, "daemon", "start", "--transport", "webkitgtk", "--addr", addr); r.code != 0 {
		t.Fatalf("daemon start: exit %d, stderr %q", r.code, r.stderr)
	}
	t.Cleanup(func() {
		if r := cli(t, "daemon", "stop"); r.code != 0 {
			t.Errorf("daemon stop: exit %d, stderr %q", r.code, r.stderr)
		}
	})
	port := strings.TrimPrefix(freeAddr(t), "127.0.0.1:")
	if r := cli(t, "cdp", "start", "--port", port); r.code != 0 {
		t.Fatalf("cdp start --port %s: exit %d, stderr %q", port, r.code, r.stderr)
	}
	r := cli(t, "--session", "s1", "get", "cdp-url")
	if r.code != 0 {
		t.Fatalf("get cdp-url: exit %d, stderr %q", r.code, r.stderr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := cdptest.Dial(ctx, strings.TrimSpace(r.stdout), nil)
	if err != nil {
		t.Fatalf("dialing the printed URL: %v", err)
	}
	t.Cleanup(c.Close)
	gtkCall(t, c, "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}},
	})
	tab, _ := gtkLastAttach(t, c)
	p := &cdpPage{c: c, tab: tab}
	p.attachPage(t)
	p.call(t, "Page.enable", nil)
	p.call(t, "Runtime.enable", nil)
	return p
}

// quadsOf is DOM.getContentQuads of the node that params name.
func (p *cdpPage) quadsOf(t *testing.T, params map[string]any) [][]float64 {
	t.Helper()
	var r struct{ Quads [][]float64 }
	if err := json.Unmarshal(p.call(t, "DOM.getContentQuads", params), &r); err != nil {
		t.Fatal(err)
	}
	return r.Quads
}

// Parity 0.45 (root request m-261201e2; agent-browser 0.45.0 "frame #child"
// then "click #inner"): a node that a client found in an iframe's execution
// context keeps working through the DOM methods, and its geometry is in
// the main frame's coordinates, as Chrome gives it. Expected values come
// from the fixtures' CSS (e2e/fixtures/runtime/cdp045.html): the iframe's
// content box starts at (45, 65), the button is at (10, 20) in it and is
// 80x30, so its quad is (55, 85) to (135, 115).
func TestCDP045IframeNodesOnWebKitGTK(t *testing.T) {
	p := cdp045Client(t, "cdp045.html")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for p.evalString(t, "(function(){var d=document.getElementById('child').contentDocument;return d&&d.readyState==='complete'&&d.getElementById('inner')?'ready':''})()") != "ready" {
		if ctx.Err() != nil {
			t.Fatal("the iframe did not load")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var tree struct {
		FrameTree struct {
			ChildFrames []struct {
				Frame struct{ ID string }
			}
		}
	}
	if err := json.Unmarshal(p.call(t, "Page.getFrameTree", nil), &tree); err != nil || len(tree.FrameTree.ChildFrames) != 1 {
		t.Fatalf("frame tree %+v (%v), want one child frame", tree, err)
	}
	child := tree.FrameTree.ChildFrames[0].Frame.ID
	var world struct{ ExecutionContextID int64 }
	_ = json.Unmarshal(p.call(t, "Page.createIsolatedWorld", map[string]any{"frameId": child, "worldName": "cdp045"}), &world)
	if world.ExecutionContextID == 0 {
		t.Fatal("createIsolatedWorld in the child frame gave no context")
	}
	var ev struct {
		Result struct{ ObjectID string }
	}
	_ = json.Unmarshal(p.call(t, "Runtime.evaluate", map[string]any{"expression": "document.getElementById('inner')", "contextId": world.ExecutionContextID}), &ev)
	if ev.Result.ObjectID == "" {
		t.Fatal("evaluate in the child frame's context gave no object for #inner")
	}
	want := []float64{55, 85, 135, 85, 135, 115, 55, 115}
	checkQuads := func(how string, q [][]float64) {
		t.Helper()
		if len(q) != 1 || !sameQuad(q[0], want) {
			t.Errorf("getContentQuads by %s = %v, want [%v]", how, q, want)
		}
	}
	checkQuads("objectId", p.quadsOf(t, map[string]any{"objectId": ev.Result.ObjectID}))

	var d struct {
		Node struct {
			BackendNodeID int64 `json:"backendNodeId"`
			LocalName     string
		}
	}
	_ = json.Unmarshal(p.call(t, "DOM.describeNode", map[string]any{"objectId": ev.Result.ObjectID}), &d)
	if d.Node.BackendNodeID == 0 || d.Node.LocalName != "button" {
		t.Fatalf("describeNode of #inner = %+v", d.Node)
	}
	backend := d.Node.BackendNodeID
	checkQuads("backendNodeId", p.quadsOf(t, map[string]any{"backendNodeId": backend}))
	if m := p.boxModel(t, map[string]any{"backendNodeId": backend}); !sameQuad(m.Border, want) || m.Width != 80 || m.Height != 30 {
		t.Errorf("getBoxModel by backendNodeId = %+v, want border %v", m, want)
	}
	p.call(t, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": backend})
	var again struct {
		Node struct {
			BackendNodeID int64 `json:"backendNodeId"`
		}
	}
	_ = json.Unmarshal(p.call(t, "DOM.describeNode", map[string]any{"backendNodeId": backend}), &again)
	if again.Node.BackendNodeID != backend {
		t.Errorf("describeNode by backendNodeId %d gave %d", backend, again.Node.BackendNodeID)
	}

	// requestNode gives a WIP nodeId for the iframe's object; the DOM
	// methods take it.
	var rn struct {
		NodeID int64 `json:"nodeId"`
	}
	_ = json.Unmarshal(p.call(t, "DOM.requestNode", map[string]any{"objectId": ev.Result.ObjectID}), &rn)
	if rn.NodeID == 0 {
		t.Fatal("requestNode of #inner gave no nodeId")
	}
	checkQuads("nodeId", p.quadsOf(t, map[string]any{"nodeId": rn.NodeID}))
	var byID struct {
		Node struct {
			BackendNodeID int64 `json:"backendNodeId"`
		}
	}
	_ = json.Unmarshal(p.call(t, "DOM.describeNode", map[string]any{"nodeId": rn.NodeID}), &byID)
	if byID.Node.BackendNodeID != backend {
		t.Errorf("describeNode by nodeId gave backendNodeId %d, want %d", byID.Node.BackendNodeID, backend)
	}

	// resolveNode{backendNodeId, executionContextId} in the child frame's
	// world gives the same element (agent-browser's click check).
	var ro struct{ Object struct{ ObjectID string } }
	_ = json.Unmarshal(p.call(t, "DOM.resolveNode", map[string]any{"backendNodeId": backend, "executionContextId": world.ExecutionContextID}), &ro)
	if ro.Object.ObjectID == "" {
		t.Fatal("resolveNode of #inner's backendNodeId gave no object")
	}
	var same struct{ Result struct{ Value bool } }
	_ = json.Unmarshal(p.call(t, "Runtime.callFunctionOn", map[string]any{"objectId": ro.Object.ObjectID, "functionDeclaration": "function(){return this.id==='inner'&&this.ownerDocument.title==='CDP 045 child'}", "returnByValue": true}), &same)
	if !same.Result.Value {
		t.Error("resolveNode of #inner's backendNodeId is not #inner of the child document")
	}

	// getFrameOwner names the iframe element; the hit test at the
	// button's center gives the button's backendNodeId.
	var owner struct {
		BackendNodeID int64 `json:"backendNodeId"`
	}
	_ = json.Unmarshal(p.call(t, "DOM.getFrameOwner", map[string]any{"frameId": child}), &owner)
	var od struct{ Node struct{ LocalName string } }
	_ = json.Unmarshal(p.call(t, "DOM.describeNode", map[string]any{"backendNodeId": owner.BackendNodeID}), &od)
	if owner.BackendNodeID == 0 || od.Node.LocalName != "iframe" {
		t.Errorf("getFrameOwner of the child frame = %d (%q), want the iframe element", owner.BackendNodeID, od.Node.LocalName)
	}

	// A click at the quad's center reaches the button in the iframe.
	x, y := (want[0]+want[2])/2, (want[1]+want[5])/2
	p.call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1})
	p.call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1})
	if got := p.evalString(t, "document.getElementById('child').contentDocument.getElementById('inner').textContent"); got != "inner clicked" {
		t.Errorf("after a click at (%v, %v) the button says %q, want inner clicked", x, y, got)
	}
}

// evalAwait evaluates expr (a promise of a string) in the page.
func (p *cdpPage) evalAwait(t *testing.T, expr string) string {
	t.Helper()
	var r struct {
		Result struct{ Value string }
	}
	if err := json.Unmarshal(p.call(t, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}), &r); err != nil {
		t.Fatal(err)
	}
	return r.Result.Value
}

// failCall runs method and returns its CDP error message, failing the test
// on success.
func (p *cdpPage) failCall(t *testing.T, method string, params any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := p.c.Call(ctx, p.page, method, params)
	if err == nil {
		t.Fatalf("%s succeeded with %s, want an error", method, res)
	}
	return err.Error()
}

// Parity 0.45 (chrome-devtools-mcp 1.14.0 emulate and take_screenshot
// omitBackground through puppeteer-core 25.18.1): the methods that the
// bridge added, through a real CDP connection on WebKitGTK. Expected
// results come from the CDP definitions and the bridge's documented
// limits (docs/cdp.md), not from a browser run.
func TestCDP045EmulationAndNetworkOnWebKitGTK(t *testing.T) {
	p := cdp045Client(t, "cdp045.html")
	const fetchChild = "fetch('cdp045-child.html',{cache:'no-store'}).then(function(r){return 'ok '+r.status},function(e){return 'failed '+e.name})"

	t.Run("CPU throttling rate 1 only", func(t *testing.T) {
		p.call(t, "Emulation.setCPUThrottlingRate", map[string]any{"rate": 1})
		if msg := p.failCall(t, "Emulation.setCPUThrottlingRate", map[string]any{"rate": 4}); !strings.Contains(msg, "CPU throttling cannot be emulated") {
			t.Fatalf("rate 4: %s", msg)
		}
	})

	t.Run("default background color", func(t *testing.T) {
		p.call(t, "Emulation.setDefaultBackgroundColorOverride", map[string]any{})
		if msg := p.failCall(t, "Emulation.setDefaultBackgroundColorOverride", map[string]any{"color": map[string]any{"r": 0, "g": 0, "b": 0, "a": 0}}); !strings.Contains(msg, "transparent background") {
			t.Fatalf("a transparent color: %s", msg)
		}
	})

	t.Run("geolocation override and clear", func(t *testing.T) {
		const read = "new Promise(function(res){navigator.geolocation.getCurrentPosition(function(p){res(p.coords.latitude+','+p.coords.longitude)},function(e){res('error '+e.code)})})"
		p.call(t, "Emulation.clearGeolocationOverride", nil) // nothing set: no effect
		p.call(t, "Emulation.setGeolocationOverride", map[string]any{"latitude": 48.5, "longitude": 9.25, "accuracy": 10})
		if got := p.evalAwait(t, read); got != "48.5,9.25" {
			t.Fatalf("position with the override: %q", got)
		}
		p.call(t, "Emulation.clearGeolocationOverride", nil)
		if got := p.evalString(t, "String(Geolocation.prototype.getCurrentPosition===navigator.geolocation.getCurrentPosition)+' '+String(navigator.geolocation instanceof Geolocation)"); got != "true true" {
			t.Fatalf("after the clear navigator.geolocation is not the page's own: %q", got)
		}
	})

	t.Run("offline through interception", func(t *testing.T) {
		rule := func(offline bool, download float64) map[string]any {
			return map[string]any{"matchedNetworkConditions": []any{map[string]any{"urlPattern": "", "offline": offline, "latency": 0, "downloadThroughput": download, "uploadThroughput": -1}}}
		}
		p.call(t, "Network.enable", nil)
		p.call(t, "Network.overrideNetworkState", map[string]any{"offline": true, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1})
		p.call(t, "Network.emulateNetworkConditionsByRule", rule(true, -1))
		if got := p.evalAwait(t, fetchChild); got != "failed TypeError" {
			t.Fatalf("fetch while offline: %q", got)
		}
		p.call(t, "Network.overrideNetworkState", map[string]any{"offline": false, "latency": 0, "downloadThroughput": -1, "uploadThroughput": -1})
		p.call(t, "Network.emulateNetworkConditionsByRule", rule(false, -1))
		if got := p.evalAwait(t, fetchChild); got != "ok 200" {
			t.Fatalf("fetch after offline off: %q", got)
		}
	})
}

// waitChild waits until the fixture's iframe loaded its button.
func (p *cdpPage) waitChild(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for p.evalString(t, "(function(){var d=document.getElementById('child').contentDocument;return d&&d.readyState==='complete'&&d.getElementById('inner')?'ready':''})()") != "ready" {
		if ctx.Err() != nil {
			t.Fatal("the iframe did not load")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// nodeAt is the local name and frame id of DOM.getNodeForLocation at (x, y).
func (p *cdpPage) nodeAt(t *testing.T, x, y float64) (name, frame string) {
	t.Helper()
	var loc struct {
		BackendNodeID int64  `json:"backendNodeId"`
		FrameID       string `json:"frameId"`
	}
	if err := json.Unmarshal(p.call(t, "DOM.getNodeForLocation", map[string]any{"x": x, "y": y}), &loc); err != nil {
		t.Fatal(err)
	}
	var d struct {
		Node struct {
			LocalName string `json:"localName"`
		}
	}
	if err := json.Unmarshal(p.call(t, "DOM.describeNode", map[string]any{"backendNodeId": loc.BackendNodeID}), &d); err != nil {
		t.Fatal(err)
	}
	return d.Node.LocalName, loc.FrameID
}

// Parity 0.45 review F7 and F8 (agent-browser 0.45 checks a click point
// with DOM.getNodeForLocation; Chrome's quads and hit tests follow CSS
// transforms): with the iframe scaled or translated, the button's quad,
// the hit test and a click at its center follow the transform; a
// transform that cannot be mapped exactly (a rotation) is refused.
// Expected values come from the fixture's CSS: the iframe's border box
// starts at (40, 60) with a 5px border, the button is at (10, 20) in it
// and is 80x30.
func TestCDP045TransformedIframeOnWebKitGTK(t *testing.T) {
	p := cdp045Client(t, "cdp045.html")
	p.waitChild(t)
	var tree struct {
		FrameTree struct {
			ChildFrames []struct {
				Frame struct{ ID string }
			}
		}
	}
	if err := json.Unmarshal(p.call(t, "Page.getFrameTree", nil), &tree); err != nil || len(tree.FrameTree.ChildFrames) != 1 {
		t.Fatalf("frame tree %+v (%v), want one child frame", tree, err)
	}
	child := tree.FrameTree.ChildFrames[0].Frame.ID
	inner := func() string {
		var ev struct{ Result struct{ ObjectID string } }
		_ = json.Unmarshal(p.call(t, "Runtime.evaluate", map[string]any{"expression": "document.getElementById('child').contentDocument.getElementById('inner')"}), &ev)
		if ev.Result.ObjectID == "" {
			t.Fatal("no object for #inner")
		}
		return ev.Result.ObjectID
	}
	for _, tc := range []struct {
		name, transform string
		want            []float64
	}{
		{"none", "none", []float64{55, 85, 135, 85, 135, 115, 55, 115}},
		{"scale(2)", "scale(2)", []float64{70, 110, 230, 110, 230, 170, 70, 170}},
		{"translate", "translate(30px, 40px)", []float64{85, 125, 165, 125, 165, 155, 85, 155}},
		{"scale and translate", "translate(10px, 0) scale(0.5, 2)", []float64{57.5, 110, 97.5, 110, 97.5, 170, 57.5, 170}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.evalString(t, "(function(){var f=document.getElementById('child');f.style.transformOrigin='0 0';f.style.transform='"+tc.transform+"';f.contentDocument.getElementById('inner').textContent='Inner';return 'ok'})()")
			if q := p.quadsOf(t, map[string]any{"objectId": inner()}); len(q) != 1 || !sameQuad(q[0], tc.want) {
				t.Errorf("getContentQuads = %v, want [%v]", q, tc.want)
			}
			if m := p.boxModel(t, map[string]any{"objectId": inner()}); !sameQuad(m.Border, tc.want) {
				t.Errorf("getBoxModel border = %v, want %v", m.Border, tc.want)
			}
			x, y := (tc.want[0]+tc.want[2])/2, (tc.want[1]+tc.want[5])/2
			if name, frame := p.nodeAt(t, x, y); name != "button" || frame != child {
				t.Errorf("getNodeForLocation(%v, %v) = %q in frame %q, want the button in %q", x, y, name, frame, child)
			}
			p.call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1})
			p.call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1})
			if got := p.evalString(t, "document.getElementById('child').contentDocument.getElementById('inner').textContent"); got != "inner clicked" {
				t.Errorf("after a click at (%v, %v) the button says %q", x, y, got)
			}
		})
	}
	t.Run("rotation refused", func(t *testing.T) {
		p.evalString(t, "(function(){document.getElementById('child').style.transform='rotate(10deg)';return 'ok'})()")
		if msg := p.failCall(t, "DOM.getContentQuads", map[string]any{"objectId": inner()}); !strings.Contains(msg, "transform other than scale and translate") {
			t.Errorf("getContentQuads under a rotation: %s", msg)
		}
		if msg := p.failCall(t, "DOM.getNodeForLocation", map[string]any{"x": 95, "y": 100}); !strings.Contains(msg, "transform other than scale and translate") {
			t.Errorf("getNodeForLocation under a rotation: %s", msg)
		}
	})
}

// Parity 0.45 review F5: geolocation overrides of three attachments, the
// last set wins (a replaced override becomes the latest); clearing another
// attachment's override keeps the latest surviving one in the current
// document and in a new one.
func TestCDP045GeoClearKeepsLatestOnWebKitGTK(t *testing.T) {
	p := cdp045Client(t, "cdp045.html")
	a := p.page
	var target string
	for _, ev := range p.c.Events() {
		if ev.Method != "Target.attachedToTarget" {
			continue
		}
		var at struct {
			SessionID  string `json:"sessionId"`
			TargetInfo struct {
				TargetID string `json:"targetId"`
			} `json:"targetInfo"`
		}
		_ = json.Unmarshal(ev.Params, &at)
		if at.SessionID == a {
			target = at.TargetInfo.TargetID
		}
	}
	attach := func() string {
		var x struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(gtkCall(t, p.c, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}), &x)
		if x.SessionID == "" {
			t.Fatal("attachToTarget gave no session")
		}
		return x.SessionID
	}
	b, c := attach(), attach()
	set := func(id string, lat float64) {
		p.page = id
		p.call(t, "Emulation.setGeolocationOverride", map[string]any{"latitude": lat, "longitude": 0, "accuracy": 1})
	}
	set(a, 1)
	set(b, 2)
	set(c, 3)
	set(b, 4)
	const read = "new Promise(function(res){navigator.geolocation.getCurrentPosition(function(p){res(String(p.coords.latitude))},function(e){res('error '+e.code)})})"
	p.page = a
	if got := p.evalAwait(t, read); got != "4" {
		t.Fatalf("before the clear: latitude %q, want 4", got)
	}
	p.call(t, "Emulation.clearGeolocationOverride", nil)
	if got := p.evalAwait(t, read); got != "4" {
		t.Fatalf("after clearing another attachment's override: latitude %q, want the latest survivor 4", got)
	}
	p.call(t, "Page.reload", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for p.evalString(t, "document.readyState") != "complete" {
		if ctx.Err() != nil {
			t.Fatal("the reload did not complete")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := p.evalAwait(t, read); got != "4" {
		t.Fatalf("in a new document: latitude %q, want the latest survivor 4", got)
	}
}
