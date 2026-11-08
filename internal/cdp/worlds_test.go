package cdp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Design 7.8 item 4 (5.2 "Execution contexts and utility worlds"):
// createIsolatedWorld{worldName} produces a named executionContextCreated
// with isDefault:false and an id distinct from the main context, before
// its reply; calls on the alias run in the main world; a navigation
// clears the aliases with executionContextsCleared and new ids appear for
// the new document, also for names registered by
// addScriptToEvaluateOnNewDocument{worldName}; no id is emitted under two
// names.
func TestUtilityWorlds(t *testing.T) {
	r, fp, c, sid, target := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Page.enable", nil)
	seq := c.Seq()
	call(t, c, sid, "Runtime.enable", nil)
	ctxs := contextsOf(t, c, seq, sid)
	if len(ctxs) != 1 || !ctxs[0].AuxData.IsDefault || ctxs[0].AuxData.Type != "default" || ctxs[0].AuxData.FrameID != target || ctxs[0].Name != "" {
		t.Fatalf("contexts after Runtime.enable %+v, want the main frame's default context", ctxs)
	}
	main := ctxs[0].ID
	// WebKit reports the main world again (its Runtime.enable replay): it
	// is the same document's default context, so nothing new appears.
	f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 1, "type": "normal", "frameId": "main"}})

	_, replySeq, err := c.CallSeq(ctxFor(t, 5*time.Second), sid, "Page.createIsolatedWorld", map[string]any{"frameId": target, "worldName": "util"})
	if err != nil {
		t.Fatal(err)
	}
	ctxs = contextsOf(t, c, seq, sid)
	if len(ctxs) != 2 {
		t.Fatalf("contexts after createIsolatedWorld %+v, want the default and one alias", ctxs)
	}
	alias := ctxs[1]
	if alias.Name != "util" || alias.AuxData.IsDefault || alias.AuxData.Type != "isolated" || alias.AuxData.FrameID != target || alias.ID == main {
		t.Fatalf("alias context %+v (main %d)", alias, main)
	}
	if ev := sessionEvents(c, seq, sid, "Runtime.executionContextCreated"); ev[1].Seq > replySeq {
		t.Fatalf("the alias was announced after the createIsolatedWorld reply (event %d, reply %d)", ev[1].Seq, replySeq)
	}
	again := decode[struct{ ExecutionContextID int }](t, call(t, c, sid, "Page.createIsolatedWorld", map[string]any{"frameId": target, "worldName": "util"}))
	if again.ExecutionContextID != alias.ID || len(contextsOf(t, c, seq, sid)) != 2 {
		t.Fatalf("a second createIsolatedWorld gave context %d and %d contexts, want the alias %d again and no new one", again.ExecutionContextID, len(contextsOf(t, c, seq, sid)), alias.ID)
	}
	if _, err := c.Call(ctxFor(t, 5*time.Second), sid, "Page.createIsolatedWorld", map[string]any{"frameId": "nope", "worldName": "util"}); err == nil || !strings.Contains(err.Error(), "No frame for given id found") {
		t.Fatalf("createIsolatedWorld of an unknown frame: %v", err)
	}

	// Calls on the alias run in the WIP main world, bound to this
	// document: the contextId is the main world that WebKit reported (1).
	call(t, c, sid, "Runtime.evaluate", map[string]any{"expression": "21*2", "contextId": alias.ID})
	evs := f.ReceivedMethod("Runtime.evaluate")
	if last := evs[len(evs)-1]; !strings.Contains(string(last.Params), `"contextId":1,`) || !strings.Contains(string(last.Params), "21*2") {
		t.Fatalf("evaluate on the alias sent %s, want the document's WIP main world 1", last.Params)
	}

	call(t, c, sid, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "", "worldName": "later"})
	if got := contextsOf(t, c, seq, sid); len(got) != 2 {
		t.Fatalf("addScriptToEvaluateOnNewDocument{worldName} created a context in the current document: %+v", got)
	}

	seq = c.Seq()
	fp.navigate("https://one.example/next")
	for _, name := range []string{"util", "later"} {
		waitEvent(t, c, seq, sid, "Runtime.executionContextCreated", func(p json.RawMessage) bool {
			return strings.Contains(string(p), `"name":"`+name+`"`)
		})
	}
	cleared := sessionEvents(c, seq, sid, "Runtime.executionContextsCleared")
	next := contextsOf(t, c, seq, sid)
	created := sessionEvents(c, seq, sid, "Runtime.executionContextCreated")
	if len(cleared) != 1 || cleared[0].Seq > created[0].Seq {
		t.Fatalf("executionContextsCleared %v, want one before the new document's contexts", cleared)
	}
	names := map[string]bool{}
	for _, n := range next[1:] {
		names[n.Name] = !n.AuxData.IsDefault
	}
	if len(next) != 3 || next[0].Name != "" || !next[0].AuxData.IsDefault || !names["util"] || !names["later"] {
		t.Fatalf("contexts of the new document %+v, want default, util and later", next)
	}
	for _, n := range next {
		if n.ID == main || n.ID == alias.ID {
			t.Fatalf("the new document reused the context id %d", n.ID)
		}
	}
	if _, err := c.Call(ctxFor(t, 5*time.Second), sid, "Runtime.evaluate", map[string]any{"expression": "1", "contextId": alias.ID}); err == nil || !strings.Contains(err.Error(), "Cannot find context with specified id") {
		t.Fatalf("evaluate on the old document's alias: %v, want Chrome's unknown-context error", err)
	}
	assertOneNamePerID(t, contextsOf(t, c, 0, sid))
}

// assertOneNamePerID fails when a context id appears with two names or
// two frames (Playwright keys contexts by id).
func assertOneNamePerID(t *testing.T, ctxs []cdpContext) {
	t.Helper()
	seen := map[int]cdpContext{}
	for _, x := range ctxs {
		if p, ok := seen[x.ID]; ok && (p.Name != x.Name || p.AuxData.FrameID != x.AuxData.FrameID) {
			t.Fatalf("context id %d emitted as %q in %s and as %q in %s", x.ID, p.Name, p.AuxData.FrameID, x.Name, x.AuxData.FrameID)
		}
		seen[x.ID] = x
	}
}
