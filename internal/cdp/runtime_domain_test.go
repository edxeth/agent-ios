package cdp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Runtime.callFunctionOn (design 5.3): an objectId goes to WIP
// callFunctionOn; an execution context with an objectGroup (else see
// TestCallFunctionOnInContextRoundTrips) first turns the declaration into
// a function object by evaluate and calls it through a trampoline, then
// releases it; unserializable arguments are set in a wrapper; awaitPromise
// goes through Runtime.awaitPromise; a call without a target is refused.
func TestCallFunctionOn(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	fp.eval = func(expr string) (any, bool) {
		if strings.HasPrefix(expr, "(function") {
			return map[string]any{"result": map[string]any{"type": "function", "objectId": "fn-1"}}, true
		}
		return nil, false
	}
	f.Result("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"type": "number", "value": 7, "description": "7"}})
	res := decode[struct{ Result map[string]any }](t, call(t, c, sid, "Runtime.callFunctionOn", map[string]any{
		"objectId": "obj-9", "functionDeclaration": "function(a, b, c) { return 7; }",
		"arguments": []any{map[string]any{"value": "x"}, map[string]any{"unserializableValue": "NaN"}, map[string]any{"objectId": "obj-2"}},
	}))
	if res.Result["value"] != float64(7) {
		t.Fatalf("callFunctionOn = %v", res.Result)
	}
	cf := f.ReceivedMethod("Runtime.callFunctionOn")
	var p struct {
		ObjectID            string           `json:"objectId"`
		FunctionDeclaration string           `json:"functionDeclaration"`
		Arguments           []map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(cf[0].Params, &p)
	if p.ObjectID != "obj-9" || !strings.Contains(p.FunctionDeclaration, "a[1] = NaN;") || p.Arguments[0]["value"] != "x" || len(p.Arguments[1]) != 0 || p.Arguments[2]["objectId"] != "obj-2" {
		t.Fatalf("WIP callFunctionOn %s", cf[0].Params)
	}

	call(t, c, sid, "Page.enable", nil)
	fp.mu.Lock()
	fp.replay = true // the context call needs the main world proven (wipContext)
	fp.mu.Unlock()
	call(t, c, sid, "Runtime.enable", nil)
	main := contextsOf(t, c, 0, sid)[0].ID
	f.Result("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"type": "object", "className": "Promise", "objectId": "p-1"}})
	f.Result("Runtime.awaitPromise", map[string]any{"result": map[string]any{"type": "string", "value": "done"}})
	res = decode[struct{ Result map[string]any }](t, call(t, c, sid, "Runtime.callFunctionOn", map[string]any{
		"executionContextId": main, "functionDeclaration": "function() { return Promise.resolve('done'); }", "awaitPromise": true, "returnByValue": true,
		"objectGroup": "g1",
	}))
	if res.Result["value"] != "done" {
		t.Fatalf("awaited callFunctionOn = %v", res.Result)
	}
	cf = f.ReceivedMethod("Runtime.callFunctionOn")
	_ = json.Unmarshal(cf[len(cf)-1].Params, &p)
	if p.ObjectID != "fn-1" || p.FunctionDeclaration != applyFunction {
		t.Fatalf("callFunctionOn on a context sent %s, want the function object with the trampoline", cf[len(cf)-1].Params)
	}
	if rel := f.ReceivedMethod("Runtime.releaseObject"); len(rel) != 1 || !strings.Contains(string(rel[0].Params), "fn-1") {
		t.Fatalf("releaseObject calls %v, want the function object released", rel)
	}
	if ap := f.ReceivedMethod("Runtime.awaitPromise"); len(ap) != 1 || !strings.Contains(string(ap[0].Params), `"promiseObjectId":"p-1"`) {
		t.Fatalf("awaitPromise calls %v", ap)
	}
	for _, bad := range []map[string]any{
		{"functionDeclaration": "function() {}"},
		{"functionDeclaration": "function() {}", "executionContextId": 999},
		{"functionDeclaration": "function() {}", "objectId": "o", "arguments": []any{map[string]any{"unserializableValue": "alert(1)"}}},
	} {
		if _, err := c.Call(ctxFor(t, 5*time.Second), sid, "Runtime.callFunctionOn", bad); err == nil {
			t.Fatalf("callFunctionOn %v succeeded", bad)
		}
	}
}

// An awaited call does not hold the page while its promise is pending:
// Puppeteer's aria/ selector (chrome-devtools-mcp's wait_for) awaits a
// poller whose promise settles only after the client answered a binding
// call through Runtime.callFunctionOn and Runtime.getProperties. Here the
// fake answers the awaitPromise only after the page got getProperties and
// another callFunctionOn, sent while the await is pending.
func TestAwaitedCallLetsLaterCallsRun(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	f.Result("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"type": "object", "className": "Promise", "objectId": "p-1"}})
	f.Result("Runtime.awaitPromise", map[string]any{"result": map[string]any{"type": "string", "value": "settled"}})
	f.HoldUntil("Runtime.awaitPromise", "Runtime.getProperties")
	f.Result("Runtime.getProperties", map[string]any{"properties": []any{}})
	awaited := make(chan json.RawMessage, 1)
	go func() {
		res, err := c.Call(testCtx(t), sid, "Runtime.callFunctionOn", map[string]any{
			"objectId": "poller", "functionDeclaration": "function() { return this.result(); }", "awaitPromise": true, "returnByValue": true,
		})
		if err != nil {
			t.Errorf("the awaited callFunctionOn: %v", err)
		}
		awaited <- res
	}()
	eventually(t, 5*time.Second, "the awaitPromise", func() bool { return len(f.ReceivedMethod("Runtime.awaitPromise")) > 0 })
	call(t, c, sid, "Runtime.callFunctionOn", map[string]any{"objectId": "args", "functionDeclaration": "function() { return 1; }"})
	call(t, c, sid, "Runtime.getProperties", map[string]any{"objectId": "args", "ownProperties": true})
	res := decode[struct{ Result map[string]any }](t, <-awaited)
	if res.Result["value"] != "settled" {
		t.Fatalf("the awaited callFunctionOn = %v", res.Result)
	}
}

// FIX-M F1: awaited calls beyond the page's 8 calls in flight, as one
// chrome-devtools-mcp wait_for makes for 2 locators x texts x frames, do
// not hold the page's other calls while their promises are pending: a
// pending await gives its send slot back once it was written. Before, the
// ninth call waited until an await ended at its call budget.
func TestPendingAwaitsDoNotHoldOtherCalls(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	f.Result("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"type": "object", "className": "Promise", "objectId": "p-1"}})
	f.Stall("Runtime.awaitPromise")
	f.Result("Runtime.getProperties", map[string]any{"properties": []any{}})
	const awaits = 9
	ended := make(chan struct{}, awaits)
	for i := 0; i < awaits; i++ {
		go func() {
			_, _ = c.Call(testCtx(t), sid, "Runtime.callFunctionOn", map[string]any{
				"objectId": "poller", "functionDeclaration": "function() { return this.result(); }", "awaitPromise": true, "returnByValue": true,
			})
			ended <- struct{}{}
		}()
	}
	waitUntil(t, "every awaitPromise", func() bool { return len(f.ReceivedMethod("Runtime.awaitPromise")) == awaits })
	call(t, c, sid, "Runtime.getProperties", map[string]any{"objectId": "args", "ownProperties": true})
	select {
	case <-ended:
		t.Fatal("getProperties was answered only after an awaited call ended")
	default:
	}
}

// Runtime.getProperties maps WIP property descriptors; releaseObject and
// releaseObjectGroup reach WIP.
func TestGetPropertiesAndRelease(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	f.Result("Runtime.getProperties", map[string]any{
		"properties": []any{
			map[string]any{"name": "a", "value": map[string]any{"type": "number", "value": 1, "description": "1"}, "writable": true, "configurable": true, "enumerable": true, "isOwn": true, "nativeGetter": false},
			map[string]any{"name": "b", "get": map[string]any{"type": "function", "objectId": "g"}, "configurable": false, "enumerable": false},
		},
		"internalProperties": []any{map[string]any{"name": "[[Prototype]]", "value": map[string]any{"type": "object", "objectId": "proto"}}},
	})
	res := decode[struct {
		Result             []map[string]any `json:"result"`
		InternalProperties []map[string]any `json:"internalProperties"`
	}](t, call(t, c, sid, "Runtime.getProperties", map[string]any{"objectId": "o-1", "ownProperties": true}))
	if len(res.Result) != 2 || res.Result[0]["value"].(map[string]any)["value"] != float64(1) || res.Result[0]["nativeGetter"] != nil || res.Result[1]["get"].(map[string]any)["objectId"] != "g" || res.InternalProperties[0]["name"] != "[[Prototype]]" {
		t.Fatalf("getProperties = %+v", res)
	}
	acc := decode[struct{ Result []map[string]any }](t, call(t, c, sid, "Runtime.getProperties", map[string]any{"objectId": "o-1", "accessorPropertiesOnly": true}))
	if len(acc.Result) != 1 || acc.Result[0]["name"] != "b" {
		t.Fatalf("accessorPropertiesOnly = %v", acc.Result)
	}
	call(t, c, sid, "Runtime.releaseObject", map[string]any{"objectId": "o-1"})
	call(t, c, sid, "Runtime.releaseObjectGroup", map[string]any{"objectGroup": "g1"})
	if len(f.ReceivedMethod("Runtime.releaseObject")) != 1 || !strings.Contains(string(f.ReceivedMethod("Runtime.releaseObjectGroup")[0].Params), "g1") {
		t.Fatal("releaseObject or releaseObjectGroup did not reach WIP")
	}
}

// Console messages: console API calls become Runtime.consoleAPICalled,
// JavaScript errors Runtime.exceptionThrown (0-based positions), and the
// page's internal markers never reach the client as console output.
func TestConsoleEvents(t *testing.T) {
	_, fp, c, sid, _ := attachPage(t)
	call(t, c, sid, "Runtime.enable", nil)
	seq := c.Seq()
	fp.f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{
		"source": "console-api", "level": "warning", "type": "log", "text": "careful 1",
		"parameters": []any{map[string]any{"type": "string", "value": "careful"}, map[string]any{"type": "number", "value": 1, "description": "1"}},
		"stackTrace": map[string]any{"callFrames": []any{map[string]any{"functionName": "f", "url": "https://one.example/app.js", "scriptId": "3", "lineNumber": 10, "columnNumber": 5}}},
	}})
	fp.consoleDebug("[agent-ios:error] {}")
	fp.consoleDebug(bindMarker + `{"n":"x","k":"nope","p":"1"}`)
	fp.f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{
		"source": "javascript", "level": "error", "text": "TypeError: x is not a function", "url": "https://one.example/app.js", "line": 4, "column": 2,
	}})
	ex := waitEvent(t, c, seq, sid, "Runtime.exceptionThrown", nil)
	d := decode[struct {
		ExceptionDetails struct {
			Text                     string
			LineNumber, ColumnNumber int
			URL                      string
		}
	}](t, ex.Params).ExceptionDetails
	if d.Text != "TypeError: x is not a function" || d.LineNumber != 3 || d.ColumnNumber != 1 {
		t.Fatalf("exceptionThrown %+v", d)
	}
	calls := sessionEvents(c, seq, sid, "Runtime.consoleAPICalled")
	if len(calls) != 1 {
		t.Fatalf("consoleAPICalled %v, want only the page's own message", calls)
	}
	ev := decode[struct {
		Type       string
		Args       []map[string]any
		StackTrace struct {
			CallFrames []struct{ LineNumber, ColumnNumber int }
		}
	}](t, calls[0].Params)
	if ev.Type != "warning" || len(ev.Args) != 2 || ev.Args[1]["value"] != float64(1) || ev.StackTrace.CallFrames[0].LineNumber != 9 {
		t.Fatalf("consoleAPICalled %+v", ev)
	}
	if b := sessionEvents(c, seq, sid, "Runtime.bindingCalled"); len(b) != 0 {
		t.Fatalf("a binding message with a foreign token was delivered: %v", b)
	}
}

// addBinding installs the page function in the current document and in
// the attachment's bootstrap part; its marked message with the binding's
// token becomes Runtime.bindingCalled; removeBinding removes it.
func TestBindings(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Runtime.addBinding", map[string]any{"name": "hello"})
	var token string
	for _, rc := range fp.runtimeCalls() {
		if strings.HasPrefix(rc, "cdpBind ") {
			var a struct{ Add []struct{ N, K string } }
			_ = json.Unmarshal([]byte(strings.TrimPrefix(rc, "cdpBind ")), &a)
			if len(a.Add) == 1 && a.Add[0].N == "hello" {
				token = a.Add[0].K
			}
		}
	}
	if token == "" {
		t.Fatalf("runtime calls %v, want cdpBind adding hello", fp.runtimeCalls())
	}
	boots := f.ReceivedMethod("Page.setBootstrapScript")
	if !strings.Contains(string(boots[len(boots)-1].Params), `cdpBind`) || !strings.Contains(string(boots[len(boots)-1].Params), token) {
		t.Fatal("the bootstrap script has no binding for new documents")
	}
	if len(f.ReceivedMethod("Console.enable")) == 0 {
		t.Fatal("addBinding did not enable Console, which carries the binding's messages")
	}
	seq := c.Seq()
	fp.consoleDebug(bindMarker + `{"n":"hello","k":"` + token + `","p":"{\"a\":1}"}`)
	b := decode[struct{ Name, Payload string }](t, waitEvent(t, c, seq, sid, "Runtime.bindingCalled", nil).Params)
	if b.Name != "hello" || b.Payload != `{"a":1}` {
		t.Fatalf("bindingCalled %+v", b)
	}
	call(t, c, sid, "Runtime.enable", nil) // for the consoleAPICalled that marks the end of the events below
	call(t, c, sid, "Runtime.removeBinding", map[string]any{"name": "hello"})
	if calls := fp.runtimeCalls(); !strings.Contains(calls[len(calls)-1], `"del":["hello"]`) {
		t.Fatalf("removeBinding sent %v", calls)
	}
	seq = c.Seq()
	fp.consoleDebug(bindMarker + `{"n":"hello","k":"` + token + `","p":"late"}`)
	fp.consoleDebug("sync")
	waitEvent(t, c, seq, sid, "Runtime.consoleAPICalled", nil)
	if b := sessionEvents(c, seq, sid, "Runtime.bindingCalled"); len(b) != 0 {
		t.Fatalf("bindingCalled after removeBinding: %v", b)
	}
}

// bindingToken returns the token of the last cdpBind call that added name.
func bindingToken(t *testing.T, fp *fakePage, name string) string {
	t.Helper()
	token := ""
	for _, rc := range fp.runtimeCalls() {
		var a struct{ Add []struct{ N, K string } }
		if strings.HasPrefix(rc, "cdpBind ") && json.Unmarshal([]byte(strings.TrimPrefix(rc, "cdpBind ")), &a) == nil {
			for _, b := range a.Add {
				if b.N == name {
					token = b.K
				}
			}
		}
	}
	if token == "" {
		t.Fatalf("runtime calls %v, want cdpBind adding %s", fp.runtimeCalls(), name)
	}
	return token
}

// Runtime.bindingCalled names the context that the binding was added for
// (executionContextName or executionContextId), as Chrome does: Puppeteer
// handles the call only in the ExecutionContext whose id the event names,
// so its utility world's __ariaQuerySelector (aria/ selectors,
// chrome-devtools-mcp's wait_for) never answered when the event named the
// main world. A binding without either names the main world.
func TestBindingCalledNamesTheBindingsContext(t *testing.T) {
	_, fp, c, sid, target := attachPage(t)
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	mainCtx := 0
	for _, x := range contextsOf(t, c, 0, sid) {
		if x.AuxData.IsDefault && x.AuxData.FrameID == target {
			mainCtx = x.ID
		}
	}
	alias := decode[struct{ ExecutionContextID int }](t, call(t, c, sid, "Page.createIsolatedWorld", map[string]any{"frameId": target, "worldName": "util"})).ExecutionContextID
	if mainCtx == 0 || alias == 0 || alias == mainCtx {
		t.Fatalf("main context %d, alias %d", mainCtx, alias)
	}
	for _, tc := range []struct {
		name   string
		params map[string]any
		want   int
	}{
		{"byName", map[string]any{"executionContextName": "util"}, alias},
		{"byId", map[string]any{"executionContextId": alias}, alias},
		{"global", map[string]any{}, mainCtx},
	} {
		params := map[string]any{"name": tc.name}
		for k, v := range tc.params {
			params[k] = v
		}
		call(t, c, sid, "Runtime.addBinding", params)
		seq := c.Seq()
		fp.consoleDebug(bindMarker + `{"n":"` + tc.name + `","k":"` + bindingToken(t, fp, tc.name) + `","p":"x"}`)
		b := decode[struct {
			Name               string
			ExecutionContextID int
		}](t, waitEvent(t, c, seq, sid, "Runtime.bindingCalled", nil).Params)
		if b.Name != tc.name || b.ExecutionContextID != tc.want {
			t.Errorf("%s: bindingCalled %+v, want context %d (main %d, alias %d)", tc.name, b, tc.want, mainCtx, alias)
		}
	}
}

// Child frames: their default context maps to the frame's WIP main world,
// so a call on it carries the WIP contextId; a detached frame's contexts
// are destroyed.
func TestChildFrameContexts(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	fp.children = []wipTree{{Frame: wipFrame{ID: "child", ParentID: "main", LoaderID: "0.9", URL: "https://one.example/frame.html", SecurityOrigin: "https://one.example"}}}
	call(t, c, sid, "Page.enable", nil)
	f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 7, "type": "normal", "frameId": "child"}})
	f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 8, "type": "user", "frameId": "child", "name": "extension"}})
	call(t, c, sid, "Runtime.enable", nil)
	var child cdpContext
	for _, x := range contextsOf(t, c, 0, sid) {
		if x.AuxData.FrameID == "child" {
			child = x
		}
		if x.Name == "extension" {
			t.Fatalf("a WebKit user world was exposed: %+v", x)
		}
	}
	if child.ID == 0 || !child.AuxData.IsDefault {
		t.Fatalf("contexts %+v, want the child frame's default context", contextsOf(t, c, 0, sid))
	}
	call(t, c, sid, "Runtime.evaluate", map[string]any{"expression": "document.title", "contextId": child.ID})
	evs := f.ReceivedMethod("Runtime.evaluate")
	if !strings.Contains(string(evs[len(evs)-1].Params), `"contextId":7`) {
		t.Fatalf("evaluate in the child frame sent %s", evs[len(evs)-1].Params)
	}
	seq := c.Seq()
	f.Emit("Page.frameDetached", map[string]any{"frameId": "child"})
	waitEvent(t, c, seq, sid, "Page.frameDetached", nil)
	d := sessionEvents(c, seq, sid, "Runtime.executionContextDestroyed")
	if len(d) != 1 || decode[struct{ ExecutionContextID int }](t, d[0].Params).ExecutionContextID != child.ID {
		t.Fatalf("executionContextDestroyed %v, want the child's context %d", d, child.ID)
	}
}

// Puppeteer adds its utility world's bindings (__ariaQuerySelector) once
// per frame context, by executionContextName, and calls them in each
// frame (chrome-devtools-mcp wait_for races aria/ locators over the
// frames): addBinding installs the page function in the child frame's
// document too, and a call from the child frame names the child's context
// of the binding's world. Before, the child had no function (page error
// "undefined is not an object (evaluating 'o.args')") and a call would
// have named the main frame's context.
func TestBindingCalledFromChildFrame(t *testing.T) {
	r, fp, c, sid, target := attachPage(t)
	f := r.fake["One"]
	fp.children = []wipTree{{Frame: wipFrame{ID: "child", ParentID: "main", LoaderID: "0.9", URL: "https://one.example/frame.html", SecurityOrigin: "https://one.example"}}}
	call(t, c, sid, "Page.enable", nil)
	f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 7, "type": "normal", "frameId": "child"}})
	call(t, c, sid, "Runtime.enable", nil)
	childCtx := contextOf(t, c, sid, "child")
	world := func(frame string) int {
		return decode[struct{ ExecutionContextID int }](t, call(t, c, sid, "Page.createIsolatedWorld", map[string]any{"frameId": frame, "worldName": "util"})).ExecutionContextID
	}
	mainUtil, childUtil := world(target), world("child")
	const name = "puppeteer___ariaQuerySelector"
	call(t, c, sid, "Runtime.addBinding", map[string]any{"name": name, "executionContextName": "util"})
	token := bindingToken(t, fp, name)
	installed := func() int {
		n := 0
		for _, ev := range f.ReceivedMethod("Runtime.evaluate") {
			var p struct {
				Expression string
				ContextID  int
			}
			_ = json.Unmarshal(ev.Params, &p)
			if p.ContextID == 7 && strings.Contains(p.Expression, name) && strings.Contains(p.Expression, token) {
				n++
			}
		}
		return n
	}
	if installed() != 1 {
		t.Fatalf("addBinding did not install %s in the child frame's document (WIP context 7): %v", name, f.ReceivedMethod("Runtime.evaluate"))
	}
	// Puppeteer adds the binding again for the child's own context: the
	// token stays, so the functions installed before still match.
	call(t, c, sid, "Runtime.addBinding", map[string]any{"name": name, "executionContextName": "util"})
	if got := bindingToken(t, fp, name); got != token {
		t.Fatalf("a second addBinding changed the token from %s to %s", token, got)
	}
	for _, tc := range []struct {
		from string
		want int
	}{
		{fmt.Sprintf(`,"f":%d`, childCtx), childUtil},
		{"", mainUtil},
	} {
		seq := c.Seq()
		fp.consoleDebug(bindMarker + `{"n":"` + name + `","k":"` + token + `"` + tc.from + `,"p":"{\"seq\":1}"}`)
		b := decode[struct {
			Name, Payload      string
			ExecutionContextID int
		}](t, waitEvent(t, c, seq, sid, "Runtime.bindingCalled", nil).Params)
		if b.Name != name || b.Payload != `{"seq":1}` || b.ExecutionContextID != tc.want {
			t.Errorf("call with tag %q: bindingCalled %+v, want context %d (main util %d, child util %d)", tc.from, b, tc.want, mainUtil, childUtil)
		}
	}
}

// FIX-O: Runtime.callFunctionOn with an executionContextId costs one WIP
// round trip (chrome-devtools-mcp wait_for on the iOS Simulator, where a
// round trip takes about 40 ms, timed out at 3 per call): with only value
// arguments it is one Runtime.evaluate in the context that applies the
// declaration to them; with an argument object of the context it is one
// WIP callFunctionOn on that object. An argument object of another context
// keeps the evaluate and trampoline path, where WebKit refuses it as
// before; a thrown exception and an awaited promise keep their results.
func TestCallFunctionOnInContextRoundTrips(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Page.enable", nil)
	fp.mu.Lock()
	fp.replay = true
	fp.mu.Unlock()
	call(t, c, sid, "Runtime.enable", nil)
	main := contextsOf(t, c, 0, sid)[0].ID
	methods := []string{"Runtime.evaluate", "Runtime.callFunctionOn", "Runtime.releaseObject", "Runtime.awaitPromise", "Runtime.getProperties"}
	counts := func() map[string]int {
		m := map[string]int{}
		for _, n := range methods {
			m[n] = len(f.ReceivedMethod(n))
		}
		return m
	}
	sent := func(before map[string]int) map[string]int {
		d := map[string]int{}
		for n, v := range counts() {
			if v != before[n] {
				d[n] = v - before[n]
			}
		}
		return d
	}
	fp.eval = func(expr string) (any, bool) {
		switch {
		case strings.Contains(expr, "throwIt"):
			return map[string]any{"result": map[string]any{"type": "object", "className": "Error", "description": "Error: boom", "objectId": "err-1"}, "wasThrown": true}, true
		case strings.Contains(expr, "((42\n)"):
			// WebKit runs the guard: 42 is not a function.
			return map[string]any{"result": map[string]any{"type": "string", "value": notFunctionValue}}, true
		case strings.Contains(expr, "f.apply(undefined, a)"):
			return map[string]any{"result": map[string]any{"type": "number", "value": 3, "description": "3"}}, true
		case strings.HasPrefix(expr, "(function"):
			return map[string]any{"result": map[string]any{"type": "function", "objectId": "fn-1"}}, true
		}
		return nil, false
	}

	// Value arguments: one evaluate in the context with the arguments in it.
	before := counts()
	res := decode[struct{ Result map[string]any }](t, call(t, c, sid, "Runtime.callFunctionOn", map[string]any{
		"executionContextId": main, "functionDeclaration": "(a, b) => a + b.length\n//# sourceURL=pptr:x\n",
		"arguments": []any{map[string]any{"value": 1}, map[string]any{"value": "xy"}}, "returnByValue": true,
	}))
	if res.Result["value"] != float64(3) {
		t.Fatalf("callFunctionOn with values = %v", res.Result)
	}
	if d := sent(before); len(d) != 1 || d["Runtime.evaluate"] != 1 {
		t.Fatalf("callFunctionOn with values sent %v, want one Runtime.evaluate", d)
	}
	ev := f.ReceivedMethod("Runtime.evaluate")
	var ep struct {
		Expression    string `json:"expression"`
		ContextID     int    `json:"contextId"`
		ReturnByValue bool   `json:"returnByValue"`
	}
	_ = json.Unmarshal(ev[len(ev)-1].Params, &ep)
	if ep.Expression != "(function (f, a) { return typeof f === \"function\" ? f.apply(undefined, a) : \""+notFunctionValue+"\"; })(((a, b) => a + b.length\n//# sourceURL=pptr:x\n\n), [1, \"xy\"])" || ep.ContextID != 1 || !ep.ReturnByValue {
		t.Fatalf("callFunctionOn with values evaluated %s", ev[len(ev)-1].Params)
	}

	// FIX-P F1: a declaration that is not a function is Chrome's error,
	// not an exception of the bridge's expression.
	before = counts()
	if e := callErr(t, c, sid, "Runtime.callFunctionOn", map[string]any{"executionContextId": main, "functionDeclaration": "42", "arguments": []any{map[string]any{"value": 1}}}); e.Code != -32000 || e.Message != "Given expression does not evaluate to a function" {
		t.Fatalf("callFunctionOn with a number declaration: %+v", e)
	}
	if d := sent(before); len(d) != 1 || d["Runtime.evaluate"] != 1 {
		t.Fatalf("callFunctionOn with a number declaration sent %v, want one Runtime.evaluate", d)
	}

	// An argument object of the context: one callFunctionOn on it.
	own := `{"injectedScriptId":1,"id":5}`
	f.Result("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"type": "object", "objectId": `{"injectedScriptId":1,"id":6}`}})
	before = counts()
	res = decode[struct{ Result map[string]any }](t, call(t, c, sid, "Runtime.callFunctionOn", map[string]any{
		"executionContextId": main, "functionDeclaration": "r => r.result()",
		"arguments": []any{map[string]any{"value": "v"}, map[string]any{"objectId": own}},
	}))
	if res.Result["objectId"] != `{"injectedScriptId":1,"id":6}` {
		t.Fatalf("callFunctionOn with an object = %v", res.Result)
	}
	if d := sent(before); len(d) != 1 || d["Runtime.callFunctionOn"] != 1 {
		t.Fatalf("callFunctionOn with an object of the context sent %v, want one Runtime.callFunctionOn", d)
	}
	cf := f.ReceivedMethod("Runtime.callFunctionOn")
	var p struct {
		ObjectID            string           `json:"objectId"`
		FunctionDeclaration string           `json:"functionDeclaration"`
		Arguments           []map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(cf[len(cf)-1].Params, &p)
	if p.ObjectID != own || p.FunctionDeclaration != "(f => typeof f === \"function\" ? f.bind(undefined) : f)((r => r.result()\n))" || p.Arguments[0]["value"] != "v" || p.Arguments[1]["objectId"] != own {
		t.Fatalf("callFunctionOn with an object of the context sent %s", cf[len(cf)-1].Params)
	}

	// An argument object of another context: the trampoline path, so
	// WebKit's same-world check refuses it as before.
	other := `{"injectedScriptId":9,"id":5}`
	f.Result("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"type": "number", "value": 1, "description": "1"}})
	before = counts()
	call(t, c, sid, "Runtime.callFunctionOn", map[string]any{
		"executionContextId": main, "functionDeclaration": "function(x) { return 1; }", "arguments": []any{map[string]any{"objectId": other}},
	})
	cf = f.ReceivedMethod("Runtime.callFunctionOn")
	_ = json.Unmarshal(cf[len(cf)-1].Params, &p)
	if d := sent(before); d["Runtime.evaluate"] != 1 || d["Runtime.callFunctionOn"] != 1 || d["Runtime.releaseObject"] != 1 || p.ObjectID != "fn-1" || p.Arguments[0]["objectId"] != other {
		t.Fatalf("callFunctionOn with another context's object sent %v, last %s; want the trampoline path", d, cf[len(cf)-1].Params)
	}

	// A thrown exception is the call's exceptionDetails.
	ex := decode[struct {
		ExceptionDetails map[string]any `json:"exceptionDetails"`
	}](t, call(t, c, sid, "Runtime.callFunctionOn", map[string]any{"executionContextId": main, "functionDeclaration": "function throwIt() { throw new Error('boom'); }"}))
	if ex.ExceptionDetails == nil {
		t.Fatal("a thrown callFunctionOn has no exceptionDetails")
	}

	// An awaited promise: the call, then Runtime.awaitPromise.
	f.Result("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"type": "object", "className": "Promise", "objectId": "p-2"}})
	f.Result("Runtime.awaitPromise", map[string]any{"result": map[string]any{"type": "string", "value": "done"}})
	before = counts()
	res = decode[struct{ Result map[string]any }](t, call(t, c, sid, "Runtime.callFunctionOn", map[string]any{
		"executionContextId": main, "functionDeclaration": "async r => r", "arguments": []any{map[string]any{"objectId": own}}, "awaitPromise": true, "returnByValue": true,
	}))
	if d := sent(before); res.Result["value"] != "done" || len(d) != 2 || d["Runtime.callFunctionOn"] != 1 || d["Runtime.awaitPromise"] != 1 {
		t.Fatalf("awaited callFunctionOn = %v, sent %v", res.Result, d)
	}
}

// FIX-P F5: a child frame that never answers does not fail
// Runtime.addBinding once the binding is recorded and installed in the
// main frame (Chrome's addBinding does not fail per frame): the stalled
// frame is skipped, the other child frame still gets the binding, and the
// command succeeds.
func TestAddBindingSkipsStalledChildFrame(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	fp.children = []wipTree{
		{Frame: wipFrame{ID: "stuck", ParentID: "main", LoaderID: "0.8", URL: "https://one.example/a.html", SecurityOrigin: "https://one.example"}},
		{Frame: wipFrame{ID: "child", ParentID: "main", LoaderID: "0.9", URL: "https://one.example/b.html", SecurityOrigin: "https://one.example"}},
	}
	call(t, c, sid, "Page.enable", nil)
	f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 7, "type": "normal", "frameId": "stuck"}})
	f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 8, "type": "normal", "frameId": "child"}})
	call(t, c, sid, "Runtime.enable", nil)
	contextOf(t, c, sid, "stuck")
	contextOf(t, c, sid, "child")
	const name = "fixpBinding"
	f.DropRepliesIf("Runtime.evaluate", func(params json.RawMessage) bool {
		var p struct {
			Expression string
			ContextID  int
		}
		_ = json.Unmarshal(params, &p)
		return p.ContextID == 7 && strings.Contains(p.Expression, name)
	})
	call(t, c, sid, "Runtime.addBinding", map[string]any{"name": name})
	token := bindingToken(t, fp, name)
	got := map[int]bool{}
	for _, ev := range f.ReceivedMethod("Runtime.evaluate") {
		var p struct {
			Expression string
			ContextID  int
		}
		_ = json.Unmarshal(ev.Params, &p)
		if strings.Contains(p.Expression, name) && strings.Contains(p.Expression, token) {
			got[p.ContextID] = true
		}
	}
	if !got[7] || !got[8] {
		t.Fatalf("addBinding sent the frame install to contexts %v, want 7 (stalled) and 8", got)
	}
}
