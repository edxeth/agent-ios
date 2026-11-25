//go:build webkitgtk

package webkitgtk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
)

// cdpPage is a CDP client of session s1 on the fixture page through the
// daemon, with a page attachment as Puppeteer makes it.
type cdpPage struct {
	c         *cdptest.Client
	tab, page string
}

// attachPage attaches a new page session below the tab.
func (p *cdpPage) attachPage(t *testing.T) {
	t.Helper()
	gtkCall(t, p.c, p.tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true})
	page, kind := gtkLastAttach(t, p.c)
	if kind != "page" || page == p.page {
		t.Fatalf("tab auto-attach attached a %q in session %q, want a new page session", kind, page)
	}
	p.page = page
}

func (p *cdpPage) call(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	return gtkCall(t, p.c, p.page, method, params)
}

// evalString evaluates expr in the page and returns its string value.
func (p *cdpPage) evalString(t *testing.T, expr string) string {
	t.Helper()
	var r struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(p.call(t, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true}), &r); err != nil {
		t.Fatal(err)
	}
	return r.Result.Value
}

// nodeID is the WIP nodeId of the element that selector finds, through
// DOM.getDocument and DOM.querySelector as agent-browser does.
func (p *cdpPage) nodeID(t *testing.T, selector string) int64 {
	t.Helper()
	var doc struct {
		Root struct {
			NodeID int64 `json:"nodeId"`
		} `json:"root"`
	}
	_ = json.Unmarshal(p.call(t, "DOM.getDocument", map[string]any{"depth": 0}), &doc)
	var q struct {
		NodeID int64 `json:"nodeId"`
	}
	_ = json.Unmarshal(p.call(t, "DOM.querySelector", map[string]any{"nodeId": doc.Root.NodeID, "selector": selector}), &q)
	if q.NodeID == 0 {
		t.Fatalf("DOM.querySelector %s found no node", selector)
	}
	return q.NodeID
}

// boxModel is CDP's DOM.BoxModel.
type boxModel struct {
	Content, Padding, Border, Margin []float64
	Width, Height                    float64
}

func (p *cdpPage) boxModel(t *testing.T, params map[string]any) boxModel {
	t.Helper()
	var r struct{ Model boxModel }
	if err := json.Unmarshal(p.call(t, "DOM.getBoxModel", params), &r); err != nil {
		t.Fatal(err)
	}
	return r.Model
}

// click clicks at the center of the element with nodeId id as Puppeteer
// does: a move, a press and a release with click count 1.
func (p *cdpPage) click(t *testing.T, id int64) {
	t.Helper()
	b := p.boxModel(t, map[string]any{"nodeId": id}).Border
	x, y := (b[0]+b[2])/2, (b[1]+b[5])/2
	p.call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": y})
	p.call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1})
	p.call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1})
}

func sameQuad(got, want []float64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if d := got[i] - want[i]; d > 0.01 || d < -0.01 {
			return false
		}
	}
	return true
}

// W5b-2 on WebKitGTK (design 5.3 DOM, Accessibility, Input and Emulation;
// 5.2 "Dialogs"): the page JavaScript of these domains runs in a real
// browser through the daemon and a CDP client of session s1. Expected
// values come from the fixture's markup and CSS (e2e/fixtures/runtime/
// cdp.html) and the CDP definitions, not from a browser run.
func TestCDPDomainsOnWebKitGTK(t *testing.T) {
	addr := runtimeBrowser(t, "cdp.html")
	// The daemon reaches the browser through a forwarding proxy that can
	// stall the inspector sockets (review finding F7: same-document
	// recovery).
	addr, stallInspector := inspectorProxy(t, addr)
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
	if r := cli(t, "--session", "s1", "snapshot"); r.code != 0 {
		t.Fatalf("snapshot: exit %d, stderr %q", r.code, r.stderr)
	}
	r := cli(t, "--session", "s1", "get", "cdp-url")
	ws := strings.TrimSpace(r.stdout)
	if r.code != 0 {
		t.Fatalf("get cdp-url: exit %d, stderr %q", r.code, r.stderr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := cdptest.Dial(ctx, ws, nil)
	if err != nil {
		t.Fatalf("dialing the printed URL: %v", err)
	}
	defer c.Close()
	gtkCall(t, c, "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}},
	})
	tab, _ := gtkLastAttach(t, c)
	p := &cdpPage{c: c, tab: tab}
	p.attachPage(t)

	t.Run("accessibility tree", func(t *testing.T) {
		var res struct {
			Nodes []struct {
				BackendDOMNodeID int64 `json:"backendDOMNodeId"`
				Role, Name       struct{ Value any }
				Properties       []struct {
					Name  string
					Value struct{ Value any }
				}
			}
		}
		if err := json.Unmarshal(p.call(t, "Accessibility.getFullAXTree", map[string]any{}), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Nodes) == 0 || res.Nodes[0].Role.Value != "RootWebArea" || res.Nodes[0].Name.Value != "CDP W5b-2" {
			t.Fatalf("first AX node %+v, want the RootWebArea named by the title", res.Nodes)
		}
		props := map[string]map[string]any{}
		var agree int64
		for _, n := range res.Nodes {
			key := n.Role.Value.(string) + " " + n.Name.Value.(string)
			props[key] = map[string]any{}
			for _, pr := range n.Properties {
				props[key][pr.Name] = pr.Value.Value
			}
			if key == "checkbox Agree" {
				agree = n.BackendDOMNodeID
			}
		}
		if props["heading Form"]["level"] != float64(1) || props["checkbox Agree"]["checked"] != "true" || props["checkbox Agree"]["focusable"] != true ||
			props["button Go"]["focusable"] != true || props["textbox Name"] == nil {
			t.Fatalf("AX nodes and properties %v", props)
		}
		// backendDOMNodeId is the element's backendNodeId (design 5.2):
		// DOM.describeNode and DOM.resolveNode take it.
		var d struct {
			Node struct {
				BackendNodeID int64    `json:"backendNodeId"`
				LocalName     string   `json:"localName"`
				Attributes    []string `json:"attributes"`
			} `json:"node"`
		}
		_ = json.Unmarshal(p.call(t, "DOM.describeNode", map[string]any{"backendNodeId": agree}), &d)
		if d.Node.BackendNodeID != agree || d.Node.LocalName != "input" || !strings.Contains(strings.Join(d.Node.Attributes, " "), "id agree") {
			t.Fatalf("describeNode of the checkbox's backendDOMNodeId %d: %+v", agree, d.Node)
		}
		var ro struct {
			Object struct {
				ClassName string `json:"className"`
				ObjectID  string `json:"objectId"`
			} `json:"object"`
		}
		_ = json.Unmarshal(p.call(t, "DOM.resolveNode", map[string]any{"backendNodeId": agree}), &ro)
		if ro.Object.ClassName != "HTMLInputElement" || ro.Object.ObjectID == "" {
			t.Fatalf("resolveNode of the checkbox's backendDOMNodeId %d: %+v", agree, ro.Object)
		}
	})

	t.Run("box model", func(t *testing.T) {
		// #box: left 10, top 200, margin 4, border-box 100x50, border 2,
		// padding 3, so the border box starts at (14, 204).
		m := p.boxModel(t, map[string]any{"nodeId": p.nodeID(t, "#box")})
		if !sameQuad(m.Border, []float64{14, 204, 114, 204, 114, 254, 14, 254}) || !sameQuad(m.Padding, []float64{16, 206, 112, 206, 112, 252, 16, 252}) ||
			!sameQuad(m.Content, []float64{19, 209, 109, 209, 109, 249, 19, 249}) || !sameQuad(m.Margin, []float64{10, 200, 118, 200, 118, 258, 10, 258}) ||
			m.Width != 100 || m.Height != 50 {
			t.Fatalf("box model %+v", m)
		}
	})

	t.Run("click changes the page", func(t *testing.T) {
		p.click(t, p.nodeID(t, "#go"))
		if got := p.evalString(t, "T.events.join(' ') + ' | ' + document.getElementById('go').textContent"); got != "pointerdown:false mousedown:false pointerup:false mouseup:false click:false | Clicked 1" {
			t.Fatalf("after a CDP click: %q", got)
		}
	})

	t.Run("typing into an input", func(t *testing.T) {
		p.call(t, "DOM.focus", map[string]any{"nodeId": p.nodeID(t, "#name")})
		key := func(typ, k, code, text string, vk int) {
			p.call(t, "Input.dispatchKeyEvent", map[string]any{"type": typ, "key": k, "code": code, "text": text, "windowsVirtualKeyCode": vk})
		}
		key("keyDown", "a", "KeyA", "a", 65)
		key("keyUp", "a", "KeyA", "", 65)
		p.call(t, "Input.insertText", map[string]any{"text": "bc"})
		key("rawKeyDown", "Backspace", "Backspace", "", 8)
		key("keyUp", "Backspace", "Backspace", "", 8)
		if got := p.evalString(t, "document.getElementById('name').value"); got != "ab" {
			t.Fatalf("value after typing a, inserting bc and Backspace: %q, want ab", got)
		}
	})

	t.Run("setFileInputFiles", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "note.txt")
		if err := os.WriteFile(f, []byte("hello cdp"), 0o600); err != nil {
			t.Fatal(err)
		}
		p.call(t, "DOM.setFileInputFiles", map[string]any{"nodeId": p.nodeID(t, "#file"), "files": []string{f}})
		if got := p.evalString(t, "T.files.join(',') + ' ' + document.getElementById('file').files.length"); got != "note.txt:9 1" {
			t.Fatalf("after setFileInputFiles: %q, want one change with note.txt of 9 bytes", got)
		}
	})

	t.Run("dialog policy answers a CDP click", func(t *testing.T) {
		ask := p.nodeID(t, "#ask")
		p.click(t, ask)
		if got := p.evalString(t, "T.answer"); got != "false" {
			t.Fatalf("an unarmed confirm opened by a CDP click returned %q, want false (session default)", got)
		}
		if r := cli(t, "--session", "s1", "dialog", "default", "accept"); r.code != 0 {
			t.Fatalf("dialog default accept: exit %d, stderr %q", r.code, r.stderr)
		}
		p.click(t, ask)
		if got := p.evalString(t, "T.answer"); got != "true" {
			t.Fatalf("confirm after dialog default accept returned %q, want true", got)
		}
		if r := cli(t, "--session", "s1", "dialog", "status"); r.code != 0 || !strings.Contains(r.stdout, `dialog confirm "Sure?" answered true (no answer armed: session default)`) {
			t.Fatalf("dialog status: exit %d, stdout %q", r.code, r.stdout)
		}
	})

	// W5b merge (5.2 "Dialogs"): the dialog of a CDP Input click is
	// answered by the session's policy and reported as one Log.entryAdded
	// warning on the attachment that sent the Input. Two equal dialogs in
	// a row are two reports, although WebKit reports the second console
	// message as a repeat count update. Each round ends with a barrier, a
	// dialog of a page script with another message: the session's events
	// are in order, so the round's reports come before the barrier's.
	t.Run("CDP click dialog gives one Log warning", func(t *testing.T) {
		if r := cli(t, "--session", "s1", "dialog", "default", "accept"); r.code != 0 {
			t.Fatalf("dialog default accept: exit %d, stderr %q", r.code, r.stderr)
		}
		p.call(t, "Log.enable", nil)
		barrier := func(name string) int {
			t.Helper()
			if got := p.evalString(t, "String(confirm('"+name+"'))"); got != "true" {
				t.Fatalf("confirm(%q) returned %q, want true (session default accept)", name, got)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			e, err := p.c.WaitEvent(ctx, 0, "Log.entryAdded", func(e cdptest.Event) bool {
				return e.SessionID == p.page && strings.Contains(string(e.Params), name)
			})
			if err != nil {
				t.Fatalf("the Log warning of the barrier dialog %s: %v", name, err)
			}
			return e.Seq
		}
		ask := p.nodeID(t, "#ask")
		from := p.c.Seq()
		for round, clicks := range []int{1, 2} {
			for i := range clicks {
				p.call(t, "Runtime.evaluate", map[string]any{"expression": "T.answer = 'none'"})
				p.click(t, ask)
				if got := p.evalString(t, "T.answer"); got != "true" {
					t.Fatalf("round %d click %d: confirm opened by a CDP click returned %q, want true", round, i+1, got)
				}
			}
			to := barrier(fmt.Sprintf("Barrier %d", round))
			var texts []string
			for _, e := range p.c.Events() {
				if e.Seq > from && e.Seq < to && e.Method == "Log.entryAdded" && e.SessionID == p.page {
					var v struct {
						Entry struct{ Source, Level, Text string }
					}
					if err := json.Unmarshal(e.Params, &v); err != nil {
						t.Fatal(err)
					}
					if v.Entry.Source != "other" || v.Entry.Level != "warning" || !strings.HasPrefix(v.Entry.Text, `dialog confirm "Sure?" answered true by the session's dialog policy`) {
						t.Fatalf("round %d: Log entry %+v, want the warning of the clicked confirm", round, v.Entry)
					}
					texts = append(texts, v.Entry.Text)
				}
			}
			if len(texts) != clicks {
				t.Fatalf("round %d: Log warnings of %d CDP click dialogs: %q, want exactly one per dialog", round, clicks, texts)
			}
			from = to
		}
	})

	t.Run("emulated media removed after detach", func(t *testing.T) {
		const q = "[matchMedia('(prefers-color-scheme: dark)').matches, matchMedia('print').matches].join()"
		before := p.evalString(t, q)
		p.call(t, "Emulation.setEmulatedMedia", map[string]any{"media": "print", "features": []any{map[string]any{"name": "prefers-color-scheme", "value": "dark"}}})
		if got := p.evalString(t, q); got != "true,true" {
			t.Fatalf("with print and dark emulated: %q", got)
		}
		gtkCall(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": p.page})
		p.attachPage(t)
		if got := p.evalString(t, q); got != before || got == "true,true" {
			t.Fatalf("after the detach: %q, want the page's own %q", got, before)
		}
	})

	// The regressions of the W5b-2 review (findings F1 to F4): the
	// reviewer's reproductions, with their assertions unchanged.

	// sibling is a page attachment of session s1 on a second CDP
	// connection, as a second Puppeteer client makes it.
	sibling := func(t *testing.T) *cdpPage {
		t.Helper()
		c2, _, err := cdptest.Dial(ctx, ws, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c2.Close)
		gtkCall(t, c2, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
		tab2, _ := gtkLastAttach(t, c2)
		p2 := &cdpPage{c: c2, tab: tab2}
		p2.attachPage(t)
		return p2
	}

	// F1: the page keeps each attachment's press targets apart (design
	// 5.3 Input: the button state is kept per CDP session).
	t.Run("sibling release must not erase press", func(t *testing.T) {
		p2 := sibling(t)
		_ = p.evalString(t, "T.clicks=0;'ok'")
		b := p.boxModel(t, map[string]any{"nodeId": p.nodeID(t, "#go")}).Border
		x, y := (b[0]+b[2])/2, (b[1]+b[5])/2
		ev := func(pp *cdpPage, typ string) {
			pp.call(t, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "clickCount": 1})
		}
		ev(p, "mousePressed")
		ev(p2, "mouseReleased")
		ev(p, "mouseReleased")
		if got := p.evalString(t, "String(T.clicks)"); got != "1" {
			t.Errorf("press A, release B, release A: clicks=%s, want 1", got)
		}
	})

	// F1, the same class for touch: a sibling's touchEnd finds no touch of
	// its own and leaves the other attachment's touch active.
	t.Run("sibling touchEnd must not end another touch", func(t *testing.T) {
		p2 := sibling(t)
		b := p.boxModel(t, map[string]any{"nodeId": p.nodeID(t, "#go")}).Border
		x, y := (b[0]+b[2])/2, (b[1]+b[5])/2
		p.call(t, "Input.dispatchTouchEvent", map[string]any{"type": "touchStart", "touchPoints": []any{map[string]any{"x": x, "y": y}}})
		if _, err := p2.c.Call(ctx, p2.page, "Input.dispatchTouchEvent", map[string]any{"type": "touchEnd", "touchPoints": []any{}}); err == nil || !strings.Contains(err.Error(), "no touch is active") {
			t.Errorf("touchEnd of B after a touchStart of A: %v, want the error no touch is active", err)
		}
		if _, err := p.c.Call(ctx, p.page, "Input.dispatchTouchEvent", map[string]any{"type": "touchEnd", "touchPoints": []any{}}); err != nil {
			t.Errorf("touchEnd of A after B's touchEnd: %v, want A's touch still active", err)
		}
	})

	// F1: the detach of an attachment that pressed drops its press state in
	// the page (65-cdp.js cdpDrop, called with its CDP session id, says
	// whether the session had state); a second drop finds none.
	t.Run("detach drops the attachment's press state", func(t *testing.T) {
		_ = p.evalString(t, "var o=__iwdp._.cdpDrop;T.dropped=[];__iwdp._.cdpDrop=function(a){var had=o(a);T.dropped.push(a+':'+had);return had};'ok'")
		b := p.boxModel(t, map[string]any{"nodeId": p.nodeID(t, "#go")}).Border
		p.call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": (b[0] + b[2]) / 2, "y": (b[1] + b[5]) / 2, "button": "left", "clickCount": 1})
		old := p.page
		gtkCall(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": p.page})
		p.attachPage(t)
		if got := p.evalString(t, "T.dropped.join(',')"); got != old+":true" {
			t.Errorf("cdpDrop calls after the detach: %q, want the detached session %q with state", got, old+":true")
		}
		if got := p.evalString(t, "String(__iwdp._.cdpDrop('"+old+"'))"); got != "false" {
			t.Errorf("a second cdpDrop of the detached session found state: %s, want false", got)
		}
	})

	// F2: detaching a CDP geolocation override keeps the session's CLI
	// override (design 5.2: a detach removes only the attachment's state).
	t.Run("geolocation survives detach of overlay", func(t *testing.T) {
		if r := cli(t, "--session", "s1", "set", "geo", "12", "34"); r.code != 0 {
			t.Fatalf("set geo: %+v", r)
		}
		p.call(t, "Emulation.setGeolocationOverride", map[string]any{"latitude": 56, "longitude": 78})
		gtkCall(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": p.page})
		p.attachPage(t)
		if got := p.evalString(t, "String(Object.prototype.hasOwnProperty.call(navigator.geolocation,'__iwdpGeo'))"); got != "true" {
			t.Errorf("CLI geolocation shim present after CDP detach=%s, want true", got)
		}
	})

	// F4: a supplied features list replaces the attachment's feature set.
	t.Run("empty feature list clears previous values", func(t *testing.T) {
		baseline := p.evalString(t, "String(matchMedia('(prefers-color-scheme: dark)').matches)")
		p.call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []any{map[string]any{"name": "prefers-color-scheme", "value": "light"}}})
		p.call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []any{map[string]any{"name": "prefers-color-scheme", "value": map[bool]string{true: "light", false: "dark"}[baseline == "true"]}}})
		p.call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []any{}})
		if got := p.evalString(t, "String(matchMedia('(prefers-color-scheme: dark)').matches)"); got != baseline {
			t.Errorf("empty features left dark=%s, want baseline %s", got, baseline)
		}
		gtkCall(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": p.page})
		p.attachPage(t)
	})

	// F4: an omitted media field changes no media type (pinned Puppeteer
	// sends media and features separately, EmulationManager.ts:477-532).
	t.Run("feature only call preserves media type", func(t *testing.T) {
		p.call(t, "Emulation.setEmulatedMedia", map[string]any{"media": "print"})
		p.call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []any{map[string]any{"name": "prefers-color-scheme", "value": "dark"}}})
		if got := p.evalString(t, "String(matchMedia('print').matches)"); got != "true" {
			t.Errorf("feature-only call changed print media to %s, want true", got)
		}
		gtkCall(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": p.page})
		p.attachPage(t)
	})

	// F3: queryAXTree walks the flat tree, as the snapshot does.
	t.Run("query AX includes shadow descendants", func(t *testing.T) {
		_ = p.evalString(t, "var host=document.createElement('div');document.body.appendChild(host);host.attachShadow({mode:'open'}).innerHTML='<button>ShadowUnique</button>'; 'ok'")
		var obj struct {
			Result struct {
				ObjectID string `json:"objectId"`
			}
		}
		_ = json.Unmarshal(p.call(t, "Runtime.evaluate", map[string]any{"expression": "document"}), &obj)
		var q struct{ Nodes []any }
		_ = json.Unmarshal(p.call(t, "Accessibility.queryAXTree", map[string]any{"objectId": obj.Result.ObjectID, "role": "button", "accessibleName": "ShadowUnique"}), &q)
		if len(q.Nodes) != 1 {
			t.Errorf("query AX shadow button returned %d nodes, want 1", len(q.Nodes))
		}
	})

	// F7 (closure review): a stall of the inspector socket, then the
	// ordinary same-document recovery of the logical session, keeps the
	// attachment's input cleanup; its later detach still drops the press
	// (mouse) or touch state in the live page. The reviewer's reproduction
	// with its assertion unchanged, for a press and for a touchstart.
	recoverThenDetach := func(t *testing.T, start func(x, y float64)) string {
		t.Helper()
		b := p.boxModel(t, map[string]any{"nodeId": p.nodeID(t, "#go")}).Border
		start((b[0]+b[2])/2, (b[1]+b[5])/2)
		old := p.page
		stallInspector()
		r := cli(t, "--session", "s1", "--timeout", "250ms", "eval", "1")
		t.Logf("stalled CLI eval exit=%d, stderr=%s", r.code, r.stderr)
		if r.code == 0 {
			t.Fatal("fixture: stalled inspector answered")
		}
		for end := time.Now().Add(10 * time.Second); ; {
			r = cli(t, "--session", "s1", "--timeout", "1s", "eval", "1")
			if r.code == 0 {
				break
			}
			if time.Now().After(end) {
				t.Fatalf("fixture: recovery did not complete: %+v", r)
			}
			time.Sleep(20 * time.Millisecond)
		}
		gtkCall(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": old})
		p.attachPage(t)
		return old
	}
	t.Run("detach drops press after recovery", func(t *testing.T) {
		old := recoverThenDetach(t, func(x, y float64) {
			p.call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1})
		})
		if got := p.evalString(t, "String(__iwdp._.cdpDrop('"+old+"'))"); got != "false" {
			t.Errorf("detached attachment still has press state after same-document recovery: cdpDrop=%s, want false", got)
		}
	})
	t.Run("detach drops touch after recovery", func(t *testing.T) {
		old := recoverThenDetach(t, func(x, y float64) {
			p.call(t, "Input.dispatchTouchEvent", map[string]any{"type": "touchStart", "touchPoints": []any{map[string]any{"x": x, "y": y}}})
		})
		if got := p.evalString(t, "String(__iwdp._.cdpDrop('"+old+"'))"); got != "false" {
			t.Errorf("detached attachment still has touch state after same-document recovery: cdpDrop=%s, want false", got)
		}
	})
	// W5b merge: the ids of a replaced document fail after W5b-1's
	// Page.reload (the connection was also replaced by the recoveries
	// above): a WIP nodeId and a backendNodeId of the old document, and
	// an execution context of the old document for DOM.resolveNode (the
	// fail-closed context rule). The new document's nodes and Input work.
	t.Run("stale ids after a reload", func(t *testing.T) {
		p.call(t, "Page.enable", nil)
		seq := p.c.Seq()
		p.call(t, "Runtime.enable", nil)
		mainContext := func(after int) int {
			t.Helper()
			id := 0
			for _, e := range p.c.Events() {
				if e.Seq > after && e.SessionID == p.page && e.Method == "Runtime.executionContextCreated" {
					var v struct {
						Context struct {
							ID      int
							AuxData struct{ IsDefault bool }
						}
					}
					if json.Unmarshal(e.Params, &v) == nil && v.Context.AuxData.IsDefault {
						id = v.Context.ID
					}
				}
			}
			if id == 0 {
				t.Fatal("no default execution context announced")
			}
			return id
		}
		backendOf := func(nodeID int64) int64 {
			t.Helper()
			var d struct{ Node struct{ BackendNodeID int64 } }
			if err := json.Unmarshal(p.call(t, "DOM.describeNode", map[string]any{"nodeId": nodeID}), &d); err != nil || d.Node.BackendNodeID == 0 {
				t.Fatalf("describeNode %d: %+v (%v)", nodeID, d, err)
			}
			return d.Node.BackendNodeID
		}
		oldCtx := mainContext(seq)
		oldNode := p.nodeID(t, "#go")
		oldBackend := backendOf(oldNode)
		seq = p.c.Seq()
		p.call(t, "Page.reload", nil)
		gtkEvent(t, p.c, seq, p.page, "Page.loadEventFired", nil)
		newCtx := mainContext(seq)
		fail := func(method string, params map[string]any) string {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res, err := p.c.Call(ctx, p.page, method, params)
			if err == nil {
				t.Fatalf("%s %v after the reload succeeded: %s", method, params, res)
			}
			return err.Error()
		}
		fail("DOM.getBoxModel", map[string]any{"nodeId": oldNode})
		if e := fail("DOM.describeNode", map[string]any{"backendNodeId": oldBackend}); !strings.Contains(e, "No node with given id found") {
			t.Fatalf("describeNode of the old document's backendNodeId: %s", e)
		}
		newBackend := backendOf(p.nodeID(t, "#go"))
		if e := fail("DOM.resolveNode", map[string]any{"backendNodeId": newBackend, "executionContextId": oldCtx}); !strings.Contains(e, "Cannot find context with specified id") {
			t.Fatalf("resolveNode with the old document's context %d: %s", oldCtx, e)
		}
		var ro struct{ Object struct{ ObjectID string } }
		if err := json.Unmarshal(p.call(t, "DOM.resolveNode", map[string]any{"backendNodeId": newBackend, "executionContextId": newCtx}), &ro); err != nil || ro.Object.ObjectID == "" {
			t.Fatalf("resolveNode in the new document's context: %+v (%v)", ro, err)
		}
		p.click(t, p.nodeID(t, "#go"))
		if got := p.evalString(t, "String(T.clicks)"); got != "1" {
			t.Fatalf("T.clicks after a CDP click in the reloaded page = %q, want 1", got)
		}
	})
}

// inspectorLink is one proxied connection of inspectorProxy.
type inspectorLink struct {
	down, up net.Conn
	stall    atomic.Bool
}

// inspectorProxy forwards TCP connections to target and returns its
// address and a function that stalls the inspector sockets open at that
// moment (requests whose first line names /socket/): their browser-to-
// daemon bytes are dropped from then on, as a stalled page would. New
// connections forward normally. The reviewer's fixture (W5b-2 closure
// review, F7).
func inspectorProxy(t *testing.T, target string) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	active := map[*inspectorLink]bool{}
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for c := range active {
			_ = c.down.Close()
			_ = c.up.Close()
		}
	})
	go func() {
		for {
			down, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = down.Close() }()
				up, err := net.DialTimeout("tcp", target, time.Second)
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				br := bufio.NewReader(down)
				first, err := br.ReadString('\n')
				if err != nil {
					return
				}
				c := &inspectorLink{down: down, up: up}
				if strings.Contains(first, " /socket/") {
					mu.Lock()
					active[c] = true
					mu.Unlock()
					defer func() {
						mu.Lock()
						delete(active, c)
						mu.Unlock()
					}()
				}
				if _, err = io.WriteString(up, first); err != nil {
					return
				}
				go func() {
					defer func() { _ = down.Close() }()
					buf := make([]byte, 32<<10)
					for {
						n, err := up.Read(buf)
						if n > 0 && !c.stall.Load() {
							if _, e := down.Write(buf[:n]); e != nil {
								return
							}
						}
						if err != nil {
							return
						}
					}
				}()
				_, _ = io.Copy(up, br)
			}()
		}
	}()
	return l.Addr().String(), func() {
		mu.Lock()
		defer mu.Unlock()
		for c := range active {
			c.stall.Store(true)
		}
	}
}
