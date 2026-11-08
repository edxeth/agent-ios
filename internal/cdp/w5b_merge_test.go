package cdp

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// wkConsole emits console messages on a fake page as WebKit's
// InspectorConsoleAgent does: a message equal to the previous one is
// Console.messageRepeatCountUpdated with the new count, not messageAdded.
type wkConsole struct {
	f     *fakewebkit.Server
	mu    sync.Mutex
	last  string
	count int
}

func (k *wkConsole) emit(message map[string]any) {
	b, _ := json.Marshal(message)
	k.mu.Lock()
	defer k.mu.Unlock()
	if string(b) == k.last {
		k.count++
		k.f.Emit("Console.messageRepeatCountUpdated", map[string]any{"count": k.count})
		return
	}
	k.last, k.count = string(b), 1
	k.f.Emit("Console.messageAdded", map[string]any{"message": message})
}

// dialogMessage is the marked console.debug message of 80-dialogs.js for
// an automatic answer.
func dialogMessage(typ, msg string, result any) map[string]any {
	u, _ := json.Marshal(map[string]any{"id": "", "type": typ, "message": msg, "result": result})
	text := dialogMarker + string(u)
	return map[string]any{
		"source": "console-api", "level": "debug", "type": "log", "text": text,
		"parameters": []any{map[string]any{"type": "string", "value": text}},
	}
}

// W5b merge (design 5.2 "Dialogs"): a dialog that a CDP Input event opens
// is answered in the page by the session's policy (W5b-2: the step
// carries it and the use goes to the session's dialog bridge), and the
// page's marked console message (80-dialogs.js) becomes exactly one
// Log.entryAdded warning on the attachment that sent the event (W5b-1).
// A second equal dialog is a second warning, although WebKit reports its
// message as a repeat count update.
func TestInputDialogGivesOneLogWarning(t *testing.T) {
	d := &fakeDialogs{policy: "accept"}
	w := newW5b2Page(t, func(o *Options) { o.Dialogs = d })
	con := &wkConsole{f: w.f}
	w.pf.mu.Lock()
	w.pf.actValue = func(map[string]any) any {
		con.emit(dialogMessage("confirm", "Sure?", true))
		return map[string]any{"d": []string{}, "dlg": []any{map[string]any{"id": "", "type": "confirm", "message": "Sure?", "result": true}}}
	}
	w.pf.mu.Unlock()
	w.call(t, "Log.enable", nil)
	const want = `dialog confirm "Sure?" answered true by the session's dialog policy; `
	// Round 1 is one tap; round 2 is two taps in a row, whose second
	// dialog message WebKit reports as a repeat count update.
	for round, taps := range []int{1, 2} {
		seq := w.c.Seq()
		for range taps {
			w.call(t, "Input.synthesizeTapGesture", map[string]any{"x": 1, "y": 1})
		}
		// A later Log entry: the attachment's events are in order, so
		// every warning of the taps comes before it.
		barrier := fmt.Sprintf("barrier %d", round)
		con.emit(map[string]any{"source": "network", "level": "error", "text": barrier})
		waitEvent(t, w.c, seq, w.page, "Log.entryAdded", func(p json.RawMessage) bool { return strings.Contains(string(p), barrier) })
		var dialogs []string
		for _, e := range sessionEvents(w.c, seq, w.page, "Log.entryAdded") {
			en := decode[logEntry](t, e.Params).Entry
			if strings.Contains(en.Text, barrier) {
				break
			}
			if en.Source != "other" || en.Level != "warning" || !strings.HasPrefix(en.Text, want) {
				t.Fatalf("round %d: Log entry %+v, want the dialog warning %q", round, en, want)
			}
			dialogs = append(dialogs, en.Text)
		}
		if len(dialogs) != taps {
			t.Fatalf("round %d: %d dialog warnings for %d tapped dialogs %q, want exactly one per dialog", round, len(dialogs), taps, dialogs)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.uses) != 3 {
		t.Fatalf("the session's dialog bridge got %d uses, want 3", len(d.uses))
	}
}

// consoleRepeated: each repeat count update routes the target's last
// console message again (a dialog warning, a console API call, a
// binding call); an update for a target whose last message the
// attachment did not see, after a clear or a reset, emits nothing.
func TestConsoleRepeatCountUpdates(t *testing.T) {
	var sent []string
	ps := newPageModel("PAGE", func(method string, params any) {
		b, _ := json.Marshal(params)
		sent = append(sent, method+" "+string(b))
	})
	ps.setEnabled(domLog, true)
	ps.setEnabled(domRuntime, true)
	ps.bindings["ping"] = "tok"
	added := func(target string, m map[string]any) {
		b, _ := json.Marshal(map[string]any{"message": m})
		ps.dispatch(target, "Console.messageAdded", b)
	}
	repeat := func(target string, n int) {
		ps.dispatch(target, "Console.messageRepeatCountUpdated", json.RawMessage(fmt.Sprintf(`{"count":%d}`, n)))
	}
	count := func(method, part string) int {
		n := 0
		for _, s := range sent {
			if strings.HasPrefix(s, method+" ") && strings.Contains(s, part) {
				n++
			}
		}
		return n
	}
	bind := bindMarker + `{"n":"ping","k":"tok","p":"x"}`

	added("T1", dialogMessage("alert", "Hi", nil))
	repeat("T1", 2)
	repeat("T1", 3)
	if n := count("Log.entryAdded", `dialog alert \"Hi\" answered accepted`); n != 3 {
		t.Fatalf("%d dialog warnings for a message and two repeats, want 3: %v", n, sent)
	}
	added("T1", map[string]any{"source": "console-api", "level": "info", "type": "log", "text": "hello", "parameters": []any{map[string]any{"type": "string", "value": "hello"}}})
	repeat("T1", 2)
	added("T1", map[string]any{"source": "console-api", "level": "debug", "type": "log", "text": bind})
	repeat("T1", 2)
	if n := count("Runtime.consoleAPICalled", "hello"); n != 2 {
		t.Fatalf("%d consoleAPICalled for a console.log and its repeat, want 2: %v", n, sent)
	}
	if n := count("Runtime.bindingCalled", `"payload":"x"`); n != 2 {
		t.Fatalf("%d bindingCalled for a binding call and its repeat, want 2: %v", n, sent)
	}

	// Another target's repeat names its own last message, never T1's.
	sent = nil
	repeat("T2", 2)
	if len(sent) != 0 {
		t.Fatalf("a repeat of a target without a message sent %v", sent)
	}
	// A clear forgets the target's last message.
	added("T1", dialogMessage("alert", "Hi", nil))
	ps.dispatch("T1", "Console.messagesCleared", json.RawMessage(`{}`))
	sent = nil
	repeat("T1", 2)
	if len(sent) != 0 {
		t.Fatalf("a repeat after messagesCleared sent %v", sent)
	}
	// A reset (lost events) forgets it too.
	added("T1", dialogMessage("alert", "Hi", nil))
	ps.mu.Lock()
	ps.resetLocked()
	ps.mu.Unlock()
	sent = nil
	repeat("T1", 2)
	if n := count("Log.entryAdded", "dialog"); n != 0 {
		t.Fatalf("a repeat after a reset sent %v", sent)
	}
	added("T1", dialogMessage("alert", "Hi", nil))
	repeat("T1", 2)
	if n := count("Log.entryAdded", "dialog"); n != 2 {
		t.Fatalf("%d dialog warnings for a message after the reset and its repeat, want 2: %v", n, sent)
	}
	// A tree read (seedLocked, W5b-1 round 6) is the truth for the frames
	// only: it adds and removes child frames and keeps the console record,
	// which a tree does not carry, so the next repeat still names it.
	ps.setEnabled(domPage, true)
	main := wipFrame{ID: "main", LoaderID: "0.1", URL: "https://one.example/"}
	child := wipTree{Frame: wipFrame{ID: "c1", ParentID: "main", LoaderID: "c.1", URL: "https://one.example/c"}}
	sent = nil
	ps.mu.Lock()
	ps.seedLocked("T1", wipTree{Frame: main})
	ps.seedLocked("T1", wipTree{Frame: main, ChildFrames: []wipTree{child}})
	ps.seedLocked("T1", wipTree{Frame: main})
	ps.mu.Unlock()
	if count("Page.frameAttached", "c1") != 1 || count("Page.frameDetached", "c1") != 1 {
		t.Fatalf("the tree reads sent %v, want c1 attached and detached", sent)
	}
	sent = nil
	repeat("T1", 3)
	if n := count("Log.entryAdded", "dialog"); n != 1 {
		t.Fatalf("%d dialog warnings for a repeat after tree reads, want 1: %v", n, sent)
	}
	// The kept messages are bounded by maxConsoleTargets.
	for i := 0; i < 3*maxConsoleTargets; i++ {
		added(fmt.Sprintf("W%d", i), map[string]any{"source": "other", "level": "info", "text": "m"})
	}
	ps.mu.Lock()
	n := len(ps.lastConsole)
	ps.mu.Unlock()
	if n > maxConsoleTargets {
		t.Fatalf("%d targets' last messages kept, want at most %d", n, maxConsoleTargets)
	}
}

// pageTargetID is the target id of the page attachment w.page.
func (w *w5b2Page) pageTargetID(t *testing.T) string {
	t.Helper()
	for _, e := range eventsOf(w.c, "Target.attachedToTarget") {
		if a := decode[attached](t, e.Params); a.SessionID == w.page {
			return a.TargetInfo.TargetID
		}
	}
	t.Fatalf("no attachedToTarget for the page session %s", w.page)
	return ""
}

// W5b merge (design 5.2, P2 item 1): the main frame's CDP id is the page
// target id in every domain. W5b-1's Page domain names it so; W5b-2's
// DOM.getNodeForLocation and Accessibility.getFullAXTree{frameId} read
// WIP's frame tree, whose main frame id (here "wip-main") the client
// never sees. Child frames keep their WIP ids in both.
func TestMainFrameIDAgreesAcrossDomains(t *testing.T) {
	w := newW5b2Page(t)
	target := w.pageTargetID(t)
	w.f.Result("Page.getResourceTree", map[string]any{"frameTree": map[string]any{
		"frame":       map[string]any{"id": "wip-main", "loaderId": "0.1", "url": "https://one.example/", "securityOrigin": "https://one.example", "mimeType": "text/html"},
		"childFrames": []any{map[string]any{"frame": map[string]any{"id": "wip-child", "parentId": "wip-main", "loaderId": "0.2", "url": "https://one.example/f", "securityOrigin": "https://one.example", "mimeType": "text/html"}}},
	}})
	tree := decode[struct {
		FrameTree struct {
			Frame       struct{ ID string }
			ChildFrames []struct{ Frame struct{ ID string } }
		}
	}](t, w.call(t, "Page.getFrameTree", nil)).FrameTree
	if tree.Frame.ID != target || len(tree.ChildFrames) != 1 || tree.ChildFrames[0].Frame.ID != "wip-child" {
		t.Fatalf("Page.getFrameTree %+v, want the main frame %s and the child wip-child", tree, target)
	}
	pageScript{{jsNodeAt, pageAnswer(map[string]any{"doc": "doc-1", "h": 21})}}.on(w.pf)
	hit := decode[struct{ FrameID string }](t, w.call(t, "DOM.getNodeForLocation", map[string]any{"x": 1, "y": 1}))
	if hit.FrameID != target {
		t.Fatalf("DOM.getNodeForLocation frameId %q, want the main frame of Page.getFrameTree %q", hit.FrameID, target)
	}
	w.pf.setFn("cdpAX", func(json.RawMessage) any { return axFixture() })
	w.call(t, "Accessibility.getFullAXTree", map[string]any{"frameId": target})
	for _, f := range []string{"wip-main", "wip-child"} {
		if e := w.fail(t, "Accessibility.getFullAXTree", map[string]any{"frameId": f}); !strings.Contains(e.Message, "is not the main frame") {
			t.Fatalf("getFullAXTree{frameId: %s}: %+v, want the not-main-frame error", f, e)
		}
	}
}

// W5b merge (design 5.3; W5b-1's fail-closed context rule): DOM.resolveNode
// with an executionContextId resolves only while that context exists in
// the attachment's document. After a navigation replaced the document,
// the old id fails with Chrome's unknown-context error and nothing is
// evaluated; the new document's context resolves.
func TestResolveNodeContextFailsClosed(t *testing.T) {
	_, fp, c, sid, s := attachWithPriorContexts(t, false)
	main := s.targetID()
	old := contextOf(t, c, sid, main)
	backend := handlesOf(s.pg).id("doc-1", 5)
	fp.mu.Lock()
	fp.eval = func(expr string) (any, bool) {
		if strings.Contains(expr, "I._.byHandle(\"doc-1\",5)") {
			return map[string]any{"result": map[string]any{"type": "object", "subtype": "node", "objectId": "obj-5"}}, true
		}
		return nil, false
	}
	fp.mu.Unlock()
	resolve := func(ctxID int) (json.RawMessage, error) {
		return c.Call(ctxFor(t, 5*time.Second), sid, "DOM.resolveNode", map[string]any{"backendNodeId": backend, "executionContextId": ctxID})
	}
	if res, err := resolve(old); err != nil || !strings.Contains(string(res), "obj-5") {
		t.Fatalf("resolveNode in the current context: %s, %v", res, err)
	}
	// A utility world (Puppeteer's adoptBackendNode) is an alias of the
	// main world: its context resolves too.
	alias := decode[struct{ ExecutionContextID int }](t, call(t, c, sid, "Page.createIsolatedWorld", map[string]any{"frameId": main, "worldName": "pptr"})).ExecutionContextID
	if res, err := resolve(alias); err != nil || !strings.Contains(string(res), "obj-5") {
		t.Fatalf("resolveNode in the utility world %d: %s, %v", alias, res, err)
	}
	seq := c.Seq()
	fp.navigate("https://one.example/next")
	waitEvent(t, c, seq, sid, "Runtime.executionContextCreated", nil)
	now := 0
	for _, x := range contextsOf(t, c, seq, sid) {
		if x.AuxData.FrameID == main && x.AuxData.IsDefault {
			now = x.ID
		}
	}
	if now == 0 || now == old {
		t.Fatalf("the new document's default context is %d (old %d)", now, old)
	}
	n := len(fp.expressions())
	for _, stale := range []int{old, alias} {
		if _, err := resolve(stale); err == nil || !strings.Contains(err.Error(), "Cannot find context with specified id") {
			t.Fatalf("resolveNode with the replaced document's context %d: %v, want the unknown-context error", stale, err)
		}
	}
	if got := fp.expressions()[n:]; len(got) != 0 {
		t.Fatalf("resolveNode with a stale context evaluated %q", got)
	}
	if res, err := resolve(now); err != nil || !strings.Contains(string(res), "obj-5") {
		t.Fatalf("resolveNode in the new document's context: %s, %v", res, err)
	}
}

// W5b final integration (W5b-1 round 6 with W5b-2): a tree read now
// announces the child frames that it adds (Page.frameAttached) and removes
// the ones that it lacks (Page.frameDetached), events.go seedLocked. The
// frame ids of these events are the ids that W5b-2's domains accept: the
// announced child's frameId finds its owner with DOM.getFrameOwner, its
// parentFrameId is the main frame that Accessibility.getFullAXTree takes
// (a child frame is refused, design 3.2), and a detached frame is no
// longer found (Chrome: "Frame with the given id was not found."). Input
// takes no frame id and keeps dispatching.
func TestSeededFrameIDsWorkInW5b2Domains(t *testing.T) {
	w := newW5b2Page(t)
	target := w.pageTargetID(t)
	owner := w.backendOf(t, "doc-1", 11)
	mainFrame := map[string]any{"id": "wip-main", "loaderId": "0.1", "url": "https://one.example/", "securityOrigin": "https://one.example", "mimeType": "text/html"}
	childFrame := map[string]any{"id": "wip-child", "parentId": "wip-main", "loaderId": "0.2", "url": "https://one.example/f", "securityOrigin": "https://one.example", "mimeType": "text/html"}
	setTree := func(children ...any) {
		w.f.Result("Page.getResourceTree", map[string]any{"frameTree": map[string]any{"frame": mainFrame, "childFrames": children}})
	}
	setTree()
	w.call(t, "Page.enable", nil)
	pageScript{{jsFrameOwner, pageAnswer(map[string]any{"doc": "doc-1", "h": 11})}}.on(w.pf)
	w.pf.setFn("cdpAX", func(json.RawMessage) any { return axFixture() })
	mouse := func(x int) {
		t.Helper()
		n := len(w.pf.steps())
		w.call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": 1})
		if got := w.pf.steps()[n:]; len(got) != 1 || got[0]["op"] != "move" || got[0]["x"] != float64(x) {
			t.Fatalf("Input.dispatchMouseEvent to x=%d dispatched %v, want one move", x, got)
		}
	}

	setTree(map[string]any{"frame": childFrame})
	seq := w.c.Seq()
	w.call(t, "Page.getFrameTree", nil)
	att := decode[struct{ FrameID, ParentFrameID string }](t, waitEvent(t, w.c, seq, w.page, "Page.frameAttached", nil).Params)
	if att.FrameID != "wip-child" || att.ParentFrameID != target {
		t.Fatalf("frameAttached %+v, want wip-child under the main frame %s", att, target)
	}
	got := decode[struct{ BackendNodeID int64 }](t, w.call(t, "DOM.getFrameOwner", map[string]any{"frameId": att.FrameID}))
	if got.BackendNodeID != owner {
		t.Fatalf("DOM.getFrameOwner of the announced frame %s = %d, want %d", att.FrameID, got.BackendNodeID, owner)
	}
	if ran := w.pf.pageFns(); string(ran[len(ran)-1].args) != `{"path":[0]}` {
		t.Fatalf("the frame owner ran with %s, want path [0]", ran[len(ran)-1].args)
	}
	w.call(t, "Accessibility.getFullAXTree", map[string]any{"frameId": att.ParentFrameID})
	if e := w.fail(t, "Accessibility.getFullAXTree", map[string]any{"frameId": att.FrameID}); !strings.Contains(e.Message, "is not the main frame") {
		t.Fatalf("getFullAXTree{frameId: %s}: %+v, want the not-main-frame error", att.FrameID, e)
	}
	mouse(5)

	setTree()
	seq = w.c.Seq()
	w.call(t, "Page.getFrameTree", nil)
	det := decode[struct{ FrameID string }](t, waitEvent(t, w.c, seq, w.page, "Page.frameDetached", nil).Params)
	if det.FrameID != att.FrameID {
		t.Fatalf("frameDetached %+v, want %s", det, att.FrameID)
	}
	if e := w.fail(t, "DOM.getFrameOwner", map[string]any{"frameId": det.FrameID}); e.Message != "Frame with the given id was not found." {
		t.Fatalf("DOM.getFrameOwner of the detached frame %s: %+v", det.FrameID, e)
	}
	w.call(t, "Accessibility.getFullAXTree", map[string]any{"frameId": target})
	mouse(6)
}
