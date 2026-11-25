package cdp

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
	"github.com/edxeth/agent-ios/internal/core"
)

// Reproductions of the W5b-1 review, round 1
// (/home/devkit/.pi/artifacts/reviewer/w5b1-20261108-080619.md), kept as
// regression tests with their assertions unchanged.

// Review F1: a provisional target's main-frame event must not replace the
// active document before its commit; abandoned, it leaves no trace.
// (The review synchronized on the provisional frameNavigated, which the
// fix no longer emits; this version synchronizes on a console message of
// the active target that follows the abort in the event stream.)
func TestReviewAbortedProvisionalDoesNotReplaceDocument(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	call(t, c, sid, "Page.enable", nil)
	call(t, c, sid, "Runtime.enable", nil)
	f := r.fake["One"]
	original := decode[struct{ FrameTree navFrame }](t, call(t, c, sid, "Page.getFrameTree", nil)).FrameTree.Frame
	resumed := f.Provisional("abandoned")
	select {
	case <-resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("provisional setup did not finish")
	}
	seq := c.Seq()
	f.EmitFrom("abandoned", "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": "main-new", "loaderId": "0.1", "url": "https://abandoned.example/", "securityOrigin": "https://abandoned.example"}})
	f.DestroyTarget("abandoned")
	syncConsole(t, r, c, seq, sid)
	got := decode[struct{ FrameTree navFrame }](t, call(t, c, sid, "Page.getFrameTree", nil)).FrameTree.Frame
	if got.URL != original.URL || got.LoaderID != original.LoaderID {
		t.Fatalf("aborted provisional replaced active document: active target=%s, got URL=%s loader=%s; want URL=%s loader=%s", f.ActiveTarget(), got.URL, got.LoaderID, original.URL, original.LoaderID)
	}
}

// syncConsole emits a console message of the active target and waits for
// its Runtime.consoleAPICalled: the attachment has then translated every
// earlier event of the page.
func syncConsole(t *testing.T, r *rig, c *cdptest.Client, after int, sid string) {
	t.Helper()
	r.fake["One"].Emit("Console.messageAdded", map[string]any{"message": map[string]any{
		"source": "console-api", "level": "log", "type": "log", "text": "sync-marker",
		"parameters": []any{map[string]any{"type": "string", "value": "sync-marker"}},
	}})
	waitEvent(t, c, after, sid, "Runtime.consoleAPICalled", func(p json.RawMessage) bool { return strings.Contains(string(p), "sync-marker") })
}

// Review F2: a context that a navigation ended while the call waited for
// admission is unknown; the call never runs in the new document.
func TestReviewOldContextMustNotRunInNewDocument(t *testing.T) {
	for _, method := range []string{"Runtime.evaluate", "Runtime.callFunctionOn"} {
		t.Run(method, func(t *testing.T) {
			r, fp, c, sid, target := attachPage(t)
			call(t, c, sid, "Page.enable", nil)
			call(t, c, sid, "Runtime.enable", nil)
			alias := decode[struct{ ExecutionContextID int }](t, call(t, c, sid, "Page.createIsolatedWorld", map[string]any{"frameId": target, "worldName": "util"})).ExecutionContextID
			if method == "Runtime.callFunctionOn" {
				fp.mu.Lock()
				fp.eval = func(expr string) (any, bool) {
					if strings.HasPrefix(expr, "(function") {
						return map[string]any{"result": map[string]any{"type": "function", "objectId": "review-fn"}}, true
					}
					return nil, false
				}
				fp.mu.Unlock()
				r.fake["One"].Result("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"type": "undefined"}})
			}
			_, s := r.pageSession(t, sid)
			mo, err := r.m.BeginLeased(ctxFor(t, 5*time.Second), s.pg, "s1", s.epoch, "review blocker", core.ClassExclusive)
			if err != nil {
				t.Fatal(err)
			}
			defer r.m.End(mo.ID)
			done := make(chan error, 1)
			go func() {
				params := map[string]any{"expression": "window.__wrongDocument = true", "contextId": alias}
				if method == "Runtime.callFunctionOn" {
					params = map[string]any{"functionDeclaration": "function(){window.__wrongDocument = true}", "executionContextId": alias}
				}
				_, e := c.Call(ctxFor(t, 5*time.Second), sid, method, params)
				done <- e
			}()
			eventually(t, time.Second, "Runtime.evaluate waits for scheduler admission", func() bool {
				b := make([]byte, 1<<20)
				n := runtime.Stack(b, true)
				stacks := string(b[:n])
				for _, stack := range strings.Split(stacks, "\n\n") {
					if (strings.Contains(stack, "cdp.runtimeEvaluate(") || strings.Contains(stack, "cdp.callFunctionOn(")) && strings.Contains(stack, "core.(*Scheduler).Begin(") {
						return true
					}
				}
				return false
			})
			seq := c.Seq()
			fp.navigate("https://one.example/newdoc")
			waitEvent(t, c, seq, sid, "Runtime.executionContextsCleared", nil)
			r.m.End(mo.ID)
			err = <-done
			if err == nil || !strings.Contains(err.Error(), "Cannot find context with specified id") {
				t.Fatalf("%s used old alias %d after navigation: error=%v; sent expressions=%v", method, alias, err, fp.expressions())
			}
		})
	}
}

// Review (passing check): a child frame's context reported before
// Runtime.enable is called with its WIP id.
func TestReviewChildContextUsesCorrectWIPIDOnRuntimeReplay(t *testing.T) {
	r, fp, c, sid, _ := attachPage(t)
	fp.mu.Lock()
	fp.children = []wipTree{{Frame: wipFrame{ID: "child", ParentID: "main", LoaderID: "0.9", URL: "https://one.example/child"}}}
	fp.mu.Unlock()
	call(t, c, sid, "Page.enable", nil)
	// Context inventory is populated synchronously by the transport before the queued bridge subscriber runs.
	r.fake["One"].Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 7, "type": "normal", "frameId": "child"}})
	call(t, c, sid, "Runtime.enable", nil)
	var child int
	for _, ec := range contextsOf(t, c, 0, sid) {
		if ec.AuxData.FrameID == "child" {
			child = ec.ID
		}
	}
	if child == 0 {
		t.Fatal("child context missing")
	}
	call(t, c, sid, "Runtime.evaluate", map[string]any{"expression": "document.title", "contextId": child})
	es := r.fake["One"].ReceivedMethod("Runtime.evaluate")
	var p map[string]any
	if err := json.Unmarshal(es[len(es)-1].Params, &p); err != nil {
		t.Fatal(err)
	}
	if p["contextId"] != float64(7) {
		t.Fatalf("got params %v", p)
	}
}
