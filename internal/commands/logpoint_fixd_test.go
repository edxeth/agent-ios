package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/sourcemap"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// lpHarness is a fake page with a desired state and a lease, whose
// Debugger.enable reports app.js (script 1) and whose breakpoints bind on
// line 2 of it.
func lpHarness(t *testing.T) *rtHarness {
	t.Helper()
	h := newActionHarness(t)
	h.ps.State = core.NewDesiredState()
	h.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil {
			return map[string]any{"result": map[string]any{"type": "object", "value": h.page.handler(c, m[1], json.RawMessage(m[2]))}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "string", "value": ""}}, nil // the syntax check: it compiles
	})
	h.f.Handle("Debugger.enable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		h.f.Emit("Debugger.scriptParsed", map[string]any{"scriptId": "1", "url": "https://x.test/app.js"})
		return map[string]any{}, nil
	})
	h.f.Handle("Debugger.setBreakpointByUrl", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"breakpointId": "https://x.test/app.js:1:0", "locations": []any{map[string]any{"scriptId": "1", "lineNumber": 1, "columnNumber": 0}}}, nil
	})
	return h
}

// lpPauseWindows replays the fake's Debugger calls per target and reports
// every point where WebKit could pause the page (FIX-D F1): Debugger
// enabled, or breakpoints activated or set, while the pause on debugger
// statements or on exceptions may be on (unknown counts as on: JSC keeps
// both across connections).
func lpPauseWindows(recs []fakewebkit.Record) []string {
	type st struct{ stmtsOff, excNone, enabled bool }
	state := map[string]*st{}
	var out []string
	for i, r := range recs {
		if !strings.HasPrefix(r.Method, "Debugger.") {
			continue
		}
		k := fmt.Sprint(r.Conn, r.Target)
		s := state[k]
		if s == nil {
			s = &st{}
			state[k] = s
		}
		var p map[string]any
		_ = json.Unmarshal(r.Params, &p)
		switch r.Method {
		case "Debugger.setPauseOnDebuggerStatements":
			s.stmtsOff = p["enabled"] == false
		case "Debugger.setPauseOnExceptions":
			s.excNone = p["state"] == "none"
		case "Debugger.enable":
			s.enabled = true
		case "Debugger.disable":
			s.enabled = false
		}
		safe := s.stmtsOff && s.excNone
		if !safe && (s.enabled || r.Method == "Debugger.setBreakpointsActive" || r.Method == "Debugger.setBreakpointByUrl") {
			out = append(out, fmt.Sprintf("#%d %s%s on %s", i, r.Method, r.Params, r.Target))
		}
	}
	return out
}

// FIX-D F1: logpoint add acquires Debugger with the pauses off before the
// enable, and neither it, remove nor the last owner's release turns a
// pause on.
func TestLogpointAddNeverLeavesAPauseWindow(t *testing.T) {
	h := lpHarness(t)
	if out := h.mustRun(t, "logpoint", "add", "app.js:2", "x"); !strings.Contains(out, "bound at https://x.test/app.js:2:1") {
		t.Fatalf("logpoint add: %q", out)
	}
	h.mustRun(t, "logpoint", "remove", "1")
	if len(h.f.ReceivedMethod("Debugger.enable")) == 0 || len(h.f.ReceivedMethod("Debugger.disable")) == 0 {
		t.Fatal("Debugger was not enabled for the logpoint and disabled after the last one")
	}
	if w := lpPauseWindows(h.f.Received()); len(w) > 0 {
		t.Fatalf("the page could pause:\n%s", strings.Join(w, "\n"))
	}
	for _, r := range h.f.ReceivedMethod("Debugger.setPauseOnDebuggerStatements") {
		if !strings.Contains(string(r.Params), "false") {
			t.Fatalf("setPauseOnDebuggerStatements %s was sent; agent-ios never turns a pause on", r.Params)
		}
	}
}

// FIX-D F1: when the page refuses the pause settings, logpoint add fails
// and enables nothing.
func TestLogpointAddRefusedWithoutNoPause(t *testing.T) {
	h := lpHarness(t)
	h.f.Handle("Debugger.setPauseOnExceptions", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "refused"}
	})
	if code, _, errOut := h.run("logpoint", "add", "app.js:2", "x"); code == 0 || !strings.Contains(errOut, "without pausing") {
		t.Fatalf("logpoint add with the pause settings refused: exit %d, %q", code, errOut)
	}
	if n := len(h.f.ReceivedMethod("Debugger.enable")) + len(h.f.ReceivedMethod("Debugger.setBreakpointByUrl")); n != 0 {
		t.Fatalf("Debugger.enable or a breakpoint was sent (%d) though the page could pause", n)
	}
}

// testMap is a parsed source map with the given mappings of source src.
func testMap(t *testing.T, src string, mappings string) *lpMap {
	t.Helper()
	m, err := sourcemap.Parse([]byte(`{"version":3,"sources":["` + src + `"],"mappings":"` + mappings + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	return &lpMap{m: m, sources: []string{src}}
}

// FIX-D F5: two loaded bundles that both contain an original source are
// both candidates: the one that maps the asked line wins, and two that map
// it equally are reported as ambiguous.
func TestLogpointResolveKeepsEveryBundle(t *testing.T) {
	const source = "http://site/src/shared.ts"
	setup := func(aMappings, bMappings string) *lpPage {
		p := newLpPage()
		for _, s := range []struct{ id, url, m string }{{"a", "http://site/a.js", aMappings}, {"b", "http://site/b.js", bMappings}} {
			sc := lpScript{id: s.id, url: s.url, sourceMapURL: s.id + ".map", seq: uint64(len(p.scripts) + 1)}
			p.scripts[sc.key()] = sc
			p.putMapLocked(sc.key(), testMap(t, source, s.m))
		}
		return p
	}
	// a: gen 1:0 <- line 0; b: gen 1:0 <- line 20 (";AAAA" and ";AAoBA").
	got, err := (lpCmd{p: setup(";AAAA", ";AAoBA")}).resolve(context.Background(), source, 1, 0)
	if err != nil || got.url != "http://site/a.js" || got.line != 1 || got.original == nil {
		t.Fatalf("resolve = %+v, %v; want bundle a, which maps line 1", got, err)
	}
	got, err = (lpCmd{p: setup(";AAoBA", ";AAAA")}).resolve(context.Background(), source, 1, 0)
	if err != nil || got.url != "http://site/b.js" {
		t.Fatalf("resolve = %+v, %v; want bundle b, which maps line 1", got, err)
	}
	if _, err = (lpCmd{p: setup(";AAAA", ";;AAAA")}).resolve(context.Background(), source, 1, 0); err == nil || !strings.Contains(err.Error(), "several loaded scripts") || !strings.Contains(err.Error(), "a.js") || !strings.Contains(err.Error(), "b.js") {
		t.Fatalf("resolve of a line both bundles map: %v, want an ambiguity naming both", err)
	}
}

// FIX-D F4: Debugger.globalObjectCleared ends the old document's scripts
// and maps; the logpoints stay. A path that only the old document had then
// waits for a script, and a new document's script with the same map URL
// reads its map again; a script of another target than the active one is
// no candidate.
func TestLogpointDocumentChangeForgetsScripts(t *testing.T) {
	h := newHarness(t)
	p := lpFor(h.ps)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sync := func() {
		if _, err := h.sess.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "1"}); err != nil {
			t.Fatal(err)
		}
	}
	h.f.Result("Page.getResourceTree", map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "main"}}})
	h.f.Result("Network.loadResource", map[string]any{"status": 200, "content": `{"version":3,"sources":["src/a.ts"],"mappings":"AAAA"}`})
	l := lpCmd{ps: h.ps, c: h.sess, p: p}

	h.f.Emit("Debugger.scriptParsed", map[string]any{"scriptId": "1", "url": "https://one.example/old.js", "sourceMapURL": "a.map"})
	sync()
	if r, err := l.resolve(ctx, "src/a.ts", 1, 0); err != nil || r.url != "https://one.example/old.js" {
		t.Fatalf("resolve before the clear: %+v, %v", r, err)
	}
	h.f.Emit("Debugger.globalObjectCleared", map[string]any{})
	sync()
	r, err := l.resolve(ctx, "old.js", 1, 0)
	if err != nil || r.url != "" || r.regex == "" {
		t.Fatalf("old document's script still a candidate after the clear: %+v, %v", r, err)
	}
	h.f.Emit("Debugger.scriptParsed", map[string]any{"scriptId": "2", "url": "https://one.example/old.js", "sourceMapURL": "a.map"})
	sync()
	if r, err := l.resolve(ctx, "src/a.ts", 1, 0); err != nil || r.url != "https://one.example/old.js" {
		t.Fatalf("resolve in the new document: %+v, %v", r, err)
	}
	if n := len(h.f.ReceivedMethod("Network.loadResource")); n != 2 {
		t.Fatalf("the map was read %d times; the new document's script must read it again", n)
	}
	h.f.EmitFrom("other-target", "Debugger.scriptParsed", map[string]any{"scriptId": "9", "url": "https://one.example/stale.js"})
	sync()
	if r, err := l.resolve(ctx, "stale.js", 1, 0); err != nil || r.url != "" {
		t.Fatalf("a script of a target that is not the active one was a candidate: %+v, %v", r, err)
	}
}

// FIX-D F7: list and add output are snapshots: a breakpointResolved that
// changes a logpoint's bound location while they format is no data race
// (run with -race).
func TestLogpointOutputIsASnapshot(t *testing.T) {
	h := lpHarness(t)
	h.mustRun(t, "logpoint", "add", "app.js:2", "x")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.f.Emit("Debugger.breakpointResolved", map[string]any{"breakpointId": "https://x.test/app.js:1:0", "location": map[string]any{"scriptId": "1", "lineNumber": 1 + i%3, "columnNumber": 0}})
			time.Sleep(100 * time.Microsecond)
		}
	})
	for range 30 {
		h.mustRun(t, "--json", "logpoint", "list")
		h.mustRun(t, "logpoint", "list")
	}
	close(stop)
	wg.Wait()
}

// FIX-D F13: a page keeps at most maxLpMaps source maps, dropping the least
// recently used; a map beyond the budget of sourcemap.DefaultLimits is
// refused with an error that says what to do instead.
func TestLogpointSourceMapBudgets(t *testing.T) {
	p := newLpPage()
	for i := range maxLpMaps + 1 {
		sc := lpScript{id: fmt.Sprint(i), url: fmt.Sprintf("http://site/%d.js", i), sourceMapURL: "m"}
		p.scripts[sc.key()] = sc
		p.putMapLocked(sc.key(), testMap(t, "s.ts", "AAAA"))
		if i == 1 {
			p.useMapLocked(lpScript{id: "0"}.key()) // 0 was used again: 1 is older
		}
	}
	if len(p.maps) != maxLpMaps || len(p.mapUse) != maxLpMaps || p.mapSegs != maxLpMaps {
		t.Fatalf("%d maps (%d in use order, %d segments) kept; the budget is %d", len(p.maps), len(p.mapUse), p.mapSegs, maxLpMaps)
	}
	if p.maps[lpScript{id: "0"}.key()] == nil || p.maps[lpScript{id: "1"}.key()] != nil {
		t.Fatal("eviction did not drop the least recently used map first")
	}

	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	big := "data:application/json;base64," + strings.Repeat("A", sourcemap.DefaultLimits.Bytes/3*4+2048)
	l := lpCmd{ps: h.ps, c: h.sess, p: newLpPage()}
	_, err := l.loadMap(ctx, lpScript{id: "1", url: "http://site/app.js", sourceMapURL: big})
	if err == nil || !strings.Contains(err.Error(), "set the logpoint on the generated script (http://site/app.js:<line>)") {
		t.Fatalf("an inline map beyond the byte budget: %v; want an error that names the generated script", err)
	}
}
