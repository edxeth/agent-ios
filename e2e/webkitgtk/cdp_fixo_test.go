//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// FIX-O: Runtime.callFunctionOn with an executionContextId, now one WIP
// round trip (one evaluate with value arguments, one callFunctionOn on an
// argument object of the context), keeps Chrome's semantics on real
// WebKit: the declaration runs in the context's frame at global scope
// with an undefined receiver (an arrow's this is the global object, an
// arguments name is not shadowed), objects of the context work as
// arguments, a thrown error is exceptionDetails, a promise is awaited,
// and an argument object of another frame is refused. Expected values
// come from the fixture (e2e/fixtures/runtime/cdp045.html: the iframe's
// button #inner) and from V8's callFunctionOn without an objectId.
func TestCDPCallFunctionOnInContextOnWebKitGTK(t *testing.T) {
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
			Frame       struct{ ID string }
			ChildFrames []struct {
				Frame struct{ ID string }
			}
		}
	}
	if err := json.Unmarshal(p.call(t, "Page.getFrameTree", nil), &tree); err != nil || len(tree.FrameTree.ChildFrames) != 1 {
		t.Fatalf("frame tree %+v (%v), want one child frame", tree, err)
	}
	world := func(frame string) int64 {
		var w struct{ ExecutionContextID int64 }
		_ = json.Unmarshal(p.call(t, "Page.createIsolatedWorld", map[string]any{"frameId": frame, "worldName": "fixo"}), &w)
		if w.ExecutionContextID == 0 {
			t.Fatalf("createIsolatedWorld in frame %s gave no context", frame)
		}
		return w.ExecutionContextID
	}
	child, main := world(tree.FrameTree.ChildFrames[0].Frame.ID), world(tree.FrameTree.Frame.ID)
	var ev struct {
		Result struct{ ObjectID string }
	}
	_ = json.Unmarshal(p.call(t, "Runtime.evaluate", map[string]any{"expression": "document.getElementById('inner')", "contextId": child}), &ev)
	if !strings.Contains(ev.Result.ObjectID, "injectedScriptId") {
		t.Fatalf("the object id %q does not name its injected script; the one-call path needs it", ev.Result.ObjectID)
	}
	byValue := func(params map[string]any) string {
		t.Helper()
		params["returnByValue"] = true
		var r struct {
			Result           struct{ Value any }
			ExceptionDetails any
		}
		raw := p.call(t, "Runtime.callFunctionOn", params)
		if err := json.Unmarshal(raw, &r); err != nil || r.ExceptionDetails != nil {
			t.Fatalf("callFunctionOn %v = %s (%v)", params, raw, err)
		}
		b, _ := json.Marshal(r.Result.Value)
		return string(b)
	}
	decl := "(a, b) => [a, b, this === globalThis, typeof arguments, document.querySelector('#inner') !== null]\n//# sourceURL=pptr:fixo\n"
	if got, want := byValue(map[string]any{"executionContextId": child, "functionDeclaration": decl, "arguments": []any{map[string]any{"value": 1}, map[string]any{"value": "x"}}}), `[1,"x",true,"undefined",true]`; got != want {
		t.Errorf("callFunctionOn with values = %s, want %s", got, want)
	}
	decl = "function(el, s) { return [el.id, s, this === globalThis, (() => this === globalThis)(), typeof arguments[1]]; }"
	if got, want := byValue(map[string]any{"executionContextId": child, "functionDeclaration": decl, "arguments": []any{map[string]any{"objectId": ev.Result.ObjectID}, map[string]any{"value": "s"}}}), `["inner","s",true,true,"string"]`; got != want {
		t.Errorf("callFunctionOn with an object of the context = %s, want %s", got, want)
	}
	if got, want := byValue(map[string]any{"executionContextId": child, "functionDeclaration": "el => [this === globalThis, typeof arguments, el.id]", "arguments": []any{map[string]any{"objectId": ev.Result.ObjectID}}}), `[true,"undefined","inner"]`; got != want {
		t.Errorf("arrow callFunctionOn with an object of the context = %s, want %s", got, want)
	}
	if got, want := byValue(map[string]any{"executionContextId": child, "functionDeclaration": "async el => el.id + '!'", "arguments": []any{map[string]any{"objectId": ev.Result.ObjectID}}, "awaitPromise": true}), `"inner!"`; got != want {
		t.Errorf("awaited callFunctionOn = %s, want %s", got, want)
	}
	var thrown struct {
		ExceptionDetails *struct{ Text string }
	}
	_ = json.Unmarshal(p.call(t, "Runtime.callFunctionOn", map[string]any{"executionContextId": child, "functionDeclaration": "el => { throw new Error('fixo ' + el.id); }", "arguments": []any{map[string]any{"objectId": ev.Result.ObjectID}}}), &thrown)
	if thrown.ExceptionDetails == nil {
		t.Error("a throwing callFunctionOn has no exceptionDetails")
	}
	var handle struct {
		Result struct{ ObjectID string }
	}
	_ = json.Unmarshal(p.call(t, "Runtime.callFunctionOn", map[string]any{"executionContextId": child, "functionDeclaration": "el => el.ownerDocument.body", "arguments": []any{map[string]any{"objectId": ev.Result.ObjectID}}}), &handle)
	if got := byValue(map[string]any{"objectId": handle.Result.ObjectID, "functionDeclaration": "function() { return this.contains(document.getElementById('inner')); }"}); got != "true" {
		t.Errorf("the returned object of the child's body is %s, want the body of the frame", got)
	}
	if msg := p.failCall(t, "Runtime.callFunctionOn", map[string]any{"executionContextId": main, "functionDeclaration": "el => el.id", "arguments": []any{map[string]any{"objectId": ev.Result.ObjectID}}}); !strings.Contains(msg, "same JavaScript world") {
		t.Errorf("an object of the child frame in the main frame's context: %q, want WebKit's same-world refusal", msg)
	}
}
