package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
)

// Design 5.3 Input: each CDP mouse event is one attempt-loop step of the
// cdpMouse dispatcher; the buttons are kept per CDP session, so a release
// clicks only after a press of the same session.
func TestInputButtonsPerSession(t *testing.T) {
	r := newRig(t)
	pf := newPageFake(r.fake["One"])
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	c2 := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page2 := puppeteerAttach(t, c2)
	call(t, c, page, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": 5, "y": 6, "button": "left", "clickCount": 1})
	call(t, c2, page2, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": 5, "y": 6, "button": "left", "clickCount": 1})
	call(t, c, page, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": 5, "y": 6, "button": "left", "clickCount": 1})
	acts := pf.steps()
	want := []string{"cdpMouse down bs=1 cl=false", "cdpMouse up bs=0 cl=false", "cdpMouse up bs=0 cl=true"}
	if len(acts) != len(want) {
		t.Fatalf("steps %v", acts)
	}
	for i, a := range acts {
		if got := fmt.Sprintf("%v %v bs=%v cl=%v", a["k"], a["op"], a["bs"], a["cl"]); got != want[i] || a["f"] != true {
			t.Fatalf("step %d = %q (%v), want %q", i, got, a, want[i])
		}
	}
}

// Review finding F1 (W5b-2 review; design 5.3 Input: the button state is
// kept per CDP session): each cdpMouse and cdpTouch step names its
// attachment (a), so the page keeps the press targets and touches of
// sibling attachments apart. The detach of an attachment that pressed or
// started a touch drops that state in the page (jsInputDrop with its CDP
// session id); an attachment that only released or moved sends no drop.
func TestInputPageStateIsPerAttachment(t *testing.T) {
	r := newRig(t)
	pf := newPageFake(r.fake["One"])
	attach := func() (*cdptest.Client, string) {
		c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
		_, page := puppeteerAttach(t, c)
		return c, page
	}
	c, page := attach()
	c2, page2 := attach()
	c3, page3 := attach()
	mouse := func(c *cdptest.Client, page, typ string) {
		call(t, c, page, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": 5, "y": 6, "button": "left", "clickCount": 1})
	}
	mouse(c, page, "mousePressed")
	mouse(c2, page2, "mouseReleased")
	mouse(c2, page2, "mouseMoved")
	call(t, c3, page3, "Input.dispatchTouchEvent", map[string]any{"type": "touchStart", "touchPoints": []any{map[string]any{"x": 1, "y": 2}}})
	call(t, c3, page3, "Input.emulateTouchFromMouseEvent", map[string]any{"type": "mousePressed", "x": 1, "y": 2, "button": "left"})
	jsonID := func(id string) string { b, _ := json.Marshal(id); return string(b) }
	sameLines(t, "steps", stepKeys(pf.steps(), "k", "op", "a"), []string{
		`k="cdpMouse" op="down" a=` + jsonID(page),
		`k="cdpMouse" op="up" a=` + jsonID(page2),
		`k="cdpMouse" op="move" a=` + jsonID(page2),
		`k="cdpTouch" op="start" a=` + jsonID(page3),
		`k="cdpTouch" op="start" a=` + jsonID(page3),
	})
	drops := func(mark int) []string {
		var out []string
		for _, rec := range r.fake["One"].Received()[mark:] {
			var p struct{ Expression string }
			if rec.Method == "Runtime.evaluate" && json.Unmarshal(rec.Params, &p) == nil && strings.Contains(p.Expression, "cdpDrop") {
				out = append(out, p.Expression)
			}
		}
		return out
	}
	detach := func(c *cdptest.Client, page string) []string {
		mark := len(r.fake["One"].Received())
		call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": page})
		if _, err := c.WaitEvent(ctxFor(t, 5*time.Second), 0, "Target.detachedFromTarget", nil); err != nil {
			t.Fatal(err)
		}
		return drops(mark)
	}
	if got := detach(c2, page2); len(got) != 0 {
		t.Fatalf("the detach of an attachment that never pressed sent %v", got)
	}
	for _, a := range []struct {
		c    *cdptest.Client
		page string
	}{{c, page}, {c3, page3}} {
		if got, want := detach(a.c, a.page), fmt.Sprintf(jsInputDrop, jsonID(a.page)); len(got) != 1 || got[0] != want {
			t.Fatalf("the detach of %s sent the drops %v, want [%s]", a.page, got, want)
		}
	}
}

// Review finding F7 (W5b-2 closure review): the input cleanup entry lives
// as long as the attachment owner. A same-page reconnect (background
// recovery after a stalled connection) keeps it, so the later detach still
// sends cdpDrop. The reviewer's reproduction, with its assertion
// unchanged.
func TestInputCleanupSurvivesReconnect(t *testing.T) {
	w := newW5b2Page(t)
	w.call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": 5, "y": 6, "button": "left", "clickCount": 1})
	_, s := w.r.pageSession(t, w.page)
	key := "cdp:input:" + string(s.owner)
	if !s.pg.PS.State.HasEntry(key) {
		t.Fatal("fixture: press cleanup entry absent")
	}

	w.f.StallConnection(1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	mo, err := w.r.m.BeginLeased(ctx, s.pg, "s1", s.epoch, "test stall", core.ClassRead)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_, err = w.r.m.Raw(ctx, mo.ID, "Runtime.evaluate", json.RawMessage(`{"expression":"1"}`))
	w.r.m.End(mo.ID)
	cancel()
	if err == nil {
		t.Fatal("fixture: stall call succeeded")
	}
	eventually(t, 10*time.Second, "background recovery ends", func() bool { return strings.Contains(w.r.log.text(), "background recovery reconnected") })
	w.call(t, "Runtime.evaluate", map[string]any{"expression": "1"})

	mark := w.mark()
	w.detachPage(t)
	for _, rec := range w.f.Received()[mark:] {
		if rec.Method == "Runtime.evaluate" && strings.Contains(string(rec.Params), "cdpDrop") {
			return
		}
	}
	t.Fatal("detach after same-page reconnect sent no cdpDrop; the cleanup entry was discarded while page-side press state can survive")
}

// stepKeys renders the given fields of each act step, in order.
func stepKeys(steps []map[string]any, keys ...string) []string {
	var out []string
	for _, s := range steps {
		var parts []string
		for _, k := range keys {
			b, _ := json.Marshal(s[k])
			parts = append(parts, k+"="+string(b))
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out
}

func sameLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s:\n%s\nwant:\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Design 5.3 Input dispatchMouseEvent, with CDP's parameters (Chrome
// protocol Input.dispatchMouseEvent: type, x, y, modifiers Alt 1 Ctrl 2
// Meta 4 Shift 8, button, buttons, clickCount, deltaX, deltaY): a double
// click as Puppeteer sends it (move, then press and release with click
// counts 1 and 2), the right and middle buttons with their DOM numbers
// and buttons bits (UI Events), a client buttons value, modifiers and the
// wheel. Each event is one forced step; a release clicks (cl) only after
// the session's press of that button.
func TestInputMouseSequences(t *testing.T) {
	w := newW5b2Page(t)
	ev := func(p map[string]any) { w.call(t, "Input.dispatchMouseEvent", p) }
	ev(map[string]any{"type": "mouseMoved", "x": 10, "y": 20})
	ev(map[string]any{"type": "mousePressed", "x": 10, "y": 20, "button": "left", "clickCount": 1})
	ev(map[string]any{"type": "mouseReleased", "x": 10, "y": 20, "button": "left", "clickCount": 1})
	ev(map[string]any{"type": "mousePressed", "x": 10, "y": 20, "button": "left", "clickCount": 2})
	ev(map[string]any{"type": "mouseReleased", "x": 10, "y": 20, "button": "left", "clickCount": 2})
	ev(map[string]any{"type": "mousePressed", "x": 1, "y": 2, "button": "right", "clickCount": 1, "modifiers": 8})
	ev(map[string]any{"type": "mousePressed", "x": 1, "y": 2, "button": "middle", "clickCount": 1})
	ev(map[string]any{"type": "mouseReleased", "x": 1, "y": 2, "button": "middle", "clickCount": 1})
	ev(map[string]any{"type": "mouseReleased", "x": 1, "y": 2, "button": "right", "clickCount": 1, "buttons": 0})
	ev(map[string]any{"type": "mouseMoved", "x": 3, "y": 4, "buttons": 3})
	ev(map[string]any{"type": "mouseWheel", "x": 3, "y": 4, "deltaX": 5, "deltaY": -40})
	sameLines(t, "mouse steps", stepKeys(w.pf.steps(), "k", "op", "x", "y", "b", "bs", "n", "m", "cl", "dx", "dy", "f"), []string{
		`k="cdpMouse" op="move" x=10 y=20 b=0 bs=0 n=0 m=0 cl=false dx=0 dy=0 f=true`,
		`k="cdpMouse" op="down" x=10 y=20 b=0 bs=1 n=1 m=0 cl=false dx=0 dy=0 f=true`,
		`k="cdpMouse" op="up" x=10 y=20 b=0 bs=0 n=1 m=0 cl=true dx=0 dy=0 f=true`,
		`k="cdpMouse" op="down" x=10 y=20 b=0 bs=1 n=2 m=0 cl=false dx=0 dy=0 f=true`,
		`k="cdpMouse" op="up" x=10 y=20 b=0 bs=0 n=2 m=0 cl=true dx=0 dy=0 f=true`,
		`k="cdpMouse" op="down" x=1 y=2 b=2 bs=2 n=1 m=8 cl=false dx=0 dy=0 f=true`,
		`k="cdpMouse" op="down" x=1 y=2 b=1 bs=6 n=1 m=0 cl=false dx=0 dy=0 f=true`,
		`k="cdpMouse" op="up" x=1 y=2 b=1 bs=2 n=1 m=0 cl=true dx=0 dy=0 f=true`,
		`k="cdpMouse" op="up" x=1 y=2 b=2 bs=0 n=1 m=0 cl=true dx=0 dy=0 f=true`,
		`k="cdpMouse" op="move" x=3 y=4 b=0 bs=3 n=0 m=0 cl=false dx=0 dy=0 f=true`,
		`k="cdpMouse" op="wheel" x=3 y=4 b=0 bs=0 n=0 m=0 cl=false dx=5 dy=-40 f=true`,
	})
}

// An unknown mouse event type or button is a CDP invalid-params error, and
// nothing is dispatched (Chrome answers -32602 for them).
func TestInputMouseRejectsUnknownTypeAndButton(t *testing.T) {
	w := newW5b2Page(t)
	for _, p := range []map[string]any{
		{"type": "mouseClicked", "x": 1, "y": 1},
		{"type": "mousePressed", "x": 1, "y": 1, "button": "sideways"},
	} {
		if e := w.fail(t, "Input.dispatchMouseEvent", p); e.Code != codeInvalidParams || !strings.Contains(e.Message, "Invalid parameters") {
			t.Fatalf("%v: %+v, want invalid params", p, e)
		}
		if e := w.fail(t, "Input.emulateTouchFromMouseEvent", p); e.Code != codeInvalidParams {
			t.Fatalf("emulateTouchFromMouseEvent %v: %+v, want invalid params", p, e)
		}
	}
	if s := w.pf.steps(); len(s) != 0 {
		t.Fatalf("refused events dispatched %v", s)
	}
}

// Design 5.3 Input dispatchKeyEvent and 3.5: keyDown and rawKeyDown run
// everything before keyup (phase kd; rawKeyDown inserts no text), char
// the keypress and insertion (ch), keyUp the keyup (up). The key carries
// CDP's key, code, keyCode (windowsVirtualKeyCode, else
// nativeVirtualKeyCode), charCode (the first character of text) and
// keyIdentifier (given, else U+XXXX of a one-character key, else the key
// name); a control character in text (Enter's "\r") inserts nothing, its
// default action is the key's. Modifiers: Alt 1, Ctrl 2, Meta 4, Shift 8.
func TestInputKeyPhases(t *testing.T) {
	w := newW5b2Page(t)
	key := func(p map[string]any) { w.call(t, "Input.dispatchKeyEvent", p) }
	key(map[string]any{"type": "keyDown", "key": "a", "code": "KeyA", "text": "a", "windowsVirtualKeyCode": 65})
	key(map[string]any{"type": "keyUp", "key": "a", "code": "KeyA", "windowsVirtualKeyCode": 65})
	key(map[string]any{"type": "rawKeyDown", "key": "b", "code": "KeyB", "text": "b", "nativeVirtualKeyCode": 66, "modifiers": 2 | 8})
	key(map[string]any{"type": "char", "text": "b", "modifiers": 1 | 4})
	key(map[string]any{"type": "keyDown", "key": "Enter", "code": "Enter", "text": "\r", "windowsVirtualKeyCode": 13})
	key(map[string]any{"type": "keyDown", "key": "x", "keyIdentifier": "U+0058", "windowsVirtualKeyCode": 88, "nativeVirtualKeyCode": 7})
	sameLines(t, "key steps", stepKeys(w.pf.steps(), "k", "ph", "keys", "m", "f"), []string{
		`k="press" ph="kd" keys=[{"cc":97,"code":"KeyA","kc":65,"key":"a","ki":"U+0041","text":"a"}] m={"a":false,"c":false,"m":false,"s":false} f=true`,
		`k="press" ph="up" keys=[{"cc":0,"code":"KeyA","kc":65,"key":"a","ki":"U+0041","text":""}] m={"a":false,"c":false,"m":false,"s":false} f=true`,
		`k="press" ph="kd" keys=[{"cc":98,"code":"KeyB","kc":66,"key":"b","ki":"U+0042","text":""}] m={"a":false,"c":true,"m":false,"s":true} f=true`,
		`k="press" ph="ch" keys=[{"cc":98,"code":"","kc":0,"key":"b","ki":"U+0042","text":"b"}] m={"a":true,"c":false,"m":true,"s":false} f=true`,
		`k="press" ph="kd" keys=[{"cc":13,"code":"Enter","kc":13,"key":"Enter","ki":"Enter","text":""}] m={"a":false,"c":false,"m":false,"s":false} f=true`,
		`k="press" ph="kd" keys=[{"cc":0,"code":"","kc":88,"key":"x","ki":"U+0058","text":""}] m={"a":false,"c":false,"m":false,"s":false} f=true`,
	})
	if e := w.fail(t, "Input.dispatchKeyEvent", map[string]any{"type": "keyPress", "key": "a"}); e.Code != codeInvalidParams {
		t.Fatalf("unknown key event type: %+v", e)
	}
	if n := len(w.pf.steps()); n != 6 {
		t.Fatalf("the refused key event dispatched (%d steps)", n)
	}
}

// Design 5.3 Input insertText: one inserttext step (keyboard inserttext:
// beforeinput, insertion, input; no key events).
func TestInputInsertText(t *testing.T) {
	w := newW5b2Page(t)
	w.call(t, "Input.insertText", map[string]any{"text": "héllo 世界"})
	sameLines(t, "insertText steps", stepKeys(w.pf.steps(), "k", "text", "f"), []string{`k="inserttext" text="héllo 世界" f=true`})
}

// Design 5.3 Input dispatchTouchEvent: touchStart, touchMove, touchEnd
// and touchCancel are one cdpTouch step each with the touch points (ids
// kept); touchEnd and touchCancel may have no points; touchStart and
// touchMove need one (Chrome: "Must send a TouchStart first to start a
// new touch" and empty points are refused).
func TestInputTouchEvents(t *testing.T) {
	w := newW5b2Page(t)
	touch := func(p map[string]any) { w.call(t, "Input.dispatchTouchEvent", p) }
	touch(map[string]any{"type": "touchStart", "touchPoints": []any{map[string]any{"x": 1, "y": 2, "id": 7}}})
	touch(map[string]any{"type": "touchMove", "touchPoints": []any{map[string]any{"x": 3, "y": 4, "id": 7}, map[string]any{"x": 9, "y": 9}}})
	touch(map[string]any{"type": "touchEnd", "touchPoints": []any{}})
	touch(map[string]any{"type": "touchCancel"})
	sameLines(t, "touch steps", stepKeys(w.pf.steps(), "k", "op", "p", "f"), []string{
		`k="cdpTouch" op="start" p=[{"id":7,"x":1,"y":2}] f=true`,
		`k="cdpTouch" op="move" p=[{"id":7,"x":3,"y":4},{"x":9,"y":9}] f=true`,
		`k="cdpTouch" op="end" p=[] f=true`,
		`k="cdpTouch" op="cancel" p=[] f=true`,
	})
	for _, p := range []map[string]any{
		{"type": "touchStart", "touchPoints": []any{}},
		{"type": "touchMove"},
		{"type": "touchTap", "touchPoints": []any{map[string]any{"x": 1, "y": 1}}},
	} {
		if e := w.fail(t, "Input.dispatchTouchEvent", p); e.Code != codeInvalidParams {
			t.Fatalf("%v: %+v, want invalid params", p, e)
		}
	}
	if n := len(w.pf.steps()); n != 4 {
		t.Fatalf("refused touch events dispatched (%d steps)", n)
	}
}

// Design 5.3 Input emulateTouchFromMouseEvent: a press starts a touch at
// the point, a move while it is active moves it, a release ends it, and a
// wheel scrolls as the mouse wheel does. A move or release without an
// active touch dispatches nothing and succeeds.
func TestInputTouchFromMouse(t *testing.T) {
	w := newW5b2Page(t)
	ev := func(p map[string]any) { w.call(t, "Input.emulateTouchFromMouseEvent", p) }
	ev(map[string]any{"type": "mouseMoved", "x": 1, "y": 1, "button": "none"})
	ev(map[string]any{"type": "mousePressed", "x": 5, "y": 6, "button": "left", "clickCount": 1})
	ev(map[string]any{"type": "mouseMoved", "x": 7, "y": 8, "button": "left"})
	ev(map[string]any{"type": "mouseReleased", "x": 7, "y": 8, "button": "left"})
	ev(map[string]any{"type": "mouseReleased", "x": 7, "y": 8, "button": "left"})
	ev(map[string]any{"type": "mouseWheel", "x": 2, "y": 3, "button": "none", "deltaX": 0, "deltaY": 120})
	sameLines(t, "touch-from-mouse steps", stepKeys(w.pf.steps(), "k", "op", "p", "dy"), []string{
		`k="cdpTouch" op="start" p=[{"x":5,"y":6}] dy=null`,
		`k="cdpTouch" op="move" p=[{"x":7,"y":8}] dy=null`,
		`k="cdpTouch" op="end" p=[] dy=null`,
		`k="cdpMouse" op="wheel" p=null dy=120`,
	})
}

// Design 5.3 Input synthesizeTapGesture: tapCount taps at the point, one
// tapAt step each with its ordinal (default 1).
func TestInputTapGesture(t *testing.T) {
	w := newW5b2Page(t)
	w.call(t, "Input.synthesizeTapGesture", map[string]any{"x": 11, "y": 12, "tapCount": 2, "duration": 50})
	w.call(t, "Input.synthesizeTapGesture", map[string]any{"x": 1, "y": 2})
	sameLines(t, "tap steps", stepKeys(w.pf.steps(), "k", "x", "y", "n", "f"), []string{
		`k="tapAt" x=11 y=12 n=1 f=true`,
		`k="tapAt" x=11 y=12 n=2 f=true`,
		`k="tapAt" x=1 y=2 n=1 f=true`,
	})
}

// A failure that the dispatcher reports after its dispatch (the step's
// err, for example a touchMove with no active touch in 65-cdp.js) is the
// command's error.
func TestInputStepFailureIsTheCommandError(t *testing.T) {
	w := newW5b2Page(t)
	w.pf.mu.Lock()
	w.pf.actValue = func(map[string]any) any { return map[string]any{"err": "no touch is active", "d": []string{}} }
	w.pf.mu.Unlock()
	e := w.fail(t, "Input.dispatchTouchEvent", map[string]any{"type": "touchMove", "touchPoints": []any{map[string]any{"x": 1, "y": 1}}})
	if !strings.Contains(e.Message, "Input.dispatchTouchEvent: no touch is active") {
		t.Fatalf("step failure: %+v", e)
	}
}

// fakeDialogs is a Dialogs that records its calls.
type fakeDialogs struct {
	mu     sync.Mutex
	policy string
	err    error
	before []DialogPage
	uses   []DialogUse
	after  int
}

func (d *fakeDialogs) Before(_ context.Context, p DialogPage) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.before = append(d.before, p)
	return d.policy, d.err
}

func (d *fakeDialogs) After(_ DialogPage, uses []DialogUse) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.after++
	d.uses = append(d.uses, uses...)
}

// Design 5.2 "Dialogs" and 3.7: an Input event runs through the logical
// session's dialog bridge as a W3b action does. The bridge is prepared
// before the steps (for the session and its lease epoch, on the page's
// session), each step carries its policy (dp), and the dialogs that the
// steps answered go back to the bridge. A bridge that cannot be prepared
// fails the event before anything is dispatched.
func TestInputRunsThroughTheSessionDialogBridge(t *testing.T) {
	d := &fakeDialogs{policy: "accept"}
	w := newW5b2Page(t, func(o *Options) { o.Dialogs = d })
	w.pf.mu.Lock()
	w.pf.actValue = func(map[string]any) any {
		return map[string]any{"d": []string{}, "dlg": []any{map[string]any{"id": "", "type": "confirm", "message": "Sure?", "result": true}}}
	}
	w.pf.mu.Unlock()
	w.call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": 1, "y": 1, "button": "left", "clickCount": 1})
	w.call(t, "Input.synthesizeTapGesture", map[string]any{"x": 1, "y": 1, "tapCount": 2})
	steps := w.pf.steps()
	if len(steps) != 3 {
		t.Fatalf("steps %v", steps)
	}
	for i, s := range steps {
		if s["dp"] != "accept" {
			t.Fatalf("step %d carries dialog policy %v, want accept: %v", i, s["dp"], s)
		}
	}
	session, epoch := w.r.leaseOf(t, "One")
	d.mu.Lock()
	if len(d.before) != 2 || d.after != 2 {
		t.Fatalf("bridge prepared %d times and told %d times, want 2 and 2 (one per CDP event)", len(d.before), d.after)
	}
	for _, p := range d.before {
		if p.Session != session || p.Epoch != epoch || p.PS == nil || p.Caller == nil || p.Runtime == nil {
			t.Fatalf("bridge page %+v, want session %s epoch %d with page session, caller and runtime", p, session, epoch)
		}
	}
	want := DialogUse{Type: "confirm", Message: "Sure?", Result: true}
	if len(d.uses) != 3 || d.uses[0] != want || d.uses[2] != want {
		t.Fatalf("uses told to the bridge %+v, want three of %+v", d.uses, want)
	}
	d.err = errors.New("installing the dialog override for new documents: boom")
	d.mu.Unlock()
	if e := w.fail(t, "Input.insertText", map[string]any{"text": "x"}); !strings.Contains(e.Message, "boom") {
		t.Fatalf("bridge failure: %+v", e)
	}
	if n := len(w.pf.steps()); n != 3 {
		t.Fatalf("an event whose bridge failed dispatched (%d steps)", n)
	}
}
