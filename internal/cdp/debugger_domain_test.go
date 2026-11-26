package cdp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// wipScriptParsed is a WIP Debugger.scriptParsed of the fake page.
func wipScriptParsed(id, url, sourceMapURL string) map[string]any {
	return map[string]any{"scriptId": id, "url": url, "startLine": 0, "startColumn": 0, "endLine": 9, "endColumn": 0, "isContentScript": false, "module": false, "sourceMapURL": sourceMapURL}
}

// wipParams returns the params of the fake's received calls of method.
func wipParams(t *testing.T, f *fakewebkit.Server, method string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range f.ReceivedMethod(method) {
		out = append(out, decode[map[string]any](t, r.Params))
	}
	return out
}

// chrome-devtools-mcp 1.14.0 set_logpoints through DevTools: the bridge's
// Debugger.enable activates WIP breakpoints and turns off the pause on
// debugger statements, translates scriptParsed into CDP's shape, sets a
// DevTools logpoint as a WIP breakpoint that continues by itself and
// evaluates the condition, and passes removeBreakpoint, getScriptSource,
// getPossibleBreakpoints (WIP getBreakpointLocations) and
// breakpointResolved through.
func TestDebuggerLogpointTranslation(t *testing.T) {
	r, fp, c, sid, target := attachPage(t)
	f := fp.f
	f.Handle("Debugger.enable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		f.Emit("Debugger.scriptParsed", wipScriptParsed("7", "https://one.example/app.js", "app.js.map"))
		return map[string]any{}, nil
	})
	f.Handle("Debugger.setBreakpointByUrl", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"breakpointId": "https://one.example/app.js:4:0", "locations": []any{map[string]any{"scriptId": "7", "lineNumber": 5, "columnNumber": 2}}}, nil
	})
	f.Handle("Debugger.getScriptSource", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"scriptSource": "var a = 1;"}, nil
	})
	f.Handle("Debugger.getBreakpointLocations", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"locations": []any{map[string]any{"scriptId": "7", "lineNumber": 5, "columnNumber": 2}}}, nil
	})
	_ = r

	seq := c.Seq()
	res := decode[struct{ DebuggerID string }](t, call(t, c, sid, "Debugger.enable", map[string]any{"maxScriptsCacheSize": 1e7}))
	if res.DebuggerID == "" {
		t.Fatal("Debugger.enable returned no debuggerId")
	}
	ev := decode[struct {
		ScriptID           string
		URL                string
		ExecutionContextID int
		Hash               string
		SourceMapURL       string
		AuxData            struct{ FrameID string } `json:"executionContextAuxData"`
	}](t, waitEvent(t, c, seq, sid, "Debugger.scriptParsed", nil).Params)
	if ev.ScriptID != "7" || ev.URL != "https://one.example/app.js" || ev.ExecutionContextID == 0 || ev.Hash == "" || ev.SourceMapURL != "app.js.map" || ev.AuxData.FrameID != target {
		t.Fatalf("scriptParsed %+v", ev)
	}
	// Core's agent reset sends WebKit's baselines first; the last values
	// are the enable's.
	if p := wipParams(t, f, "Debugger.setBreakpointsActive"); len(p) == 0 || p[len(p)-1]["active"] != true {
		t.Fatalf("WIP setBreakpointsActive %v, want active true last", p)
	}
	if p := wipParams(t, f, "Debugger.setPauseOnDebuggerStatements"); len(p) == 0 || p[len(p)-1]["enabled"] != false {
		t.Fatalf("WIP setPauseOnDebuggerStatements %v, want enabled false last", p)
	}

	cond := "/** DEVTOOLS_LOGPOINT */ console.log('total', total)\n\n//# sourceURL=debugger://logpoint"
	bp := decode[struct {
		BreakpointID string
		Locations    []cdpLocation
	}](t, call(t, c, sid, "Debugger.setBreakpointByUrl", map[string]any{"url": "https://one.example/app.js", "lineNumber": 4, "columnNumber": 0, "condition": cond}))
	if bp.BreakpointID != "https://one.example/app.js:4:0" || len(bp.Locations) != 1 || bp.Locations[0].LineNumber != 5 {
		t.Fatalf("setBreakpointByUrl reply %+v", bp)
	}
	sent := wipParams(t, f, "Debugger.setBreakpointByUrl")
	if len(sent) != 1 {
		t.Fatalf("WIP setBreakpointByUrl sent %d times", len(sent))
	}
	opts, _ := sent[0]["options"].(map[string]any)
	acts, _ := opts["actions"].([]any)
	if sent[0]["url"] != "https://one.example/app.js" || sent[0]["lineNumber"] != 4.0 || opts["autoContinue"] != true || opts["condition"] != nil || len(acts) != 1 {
		t.Fatalf("WIP setBreakpointByUrl %v: want the url and line, autoContinue and one action, no condition", sent[0])
	}
	if a := acts[0].(map[string]any); a["type"] != "evaluate" || a["data"] != cond {
		t.Fatalf("WIP breakpoint action %v, want evaluate of the condition", a)
	}

	seq = c.Seq()
	f.Emit("Debugger.breakpointResolved", map[string]any{"breakpointId": bp.BreakpointID, "location": map[string]any{"scriptId": "8", "lineNumber": 5, "columnNumber": 2}})
	waitEvent(t, c, seq, sid, "Debugger.breakpointResolved", func(p json.RawMessage) bool { return strings.Contains(string(p), `"scriptId":"8"`) })

	if src := decode[struct{ ScriptSource string }](t, call(t, c, sid, "Debugger.getScriptSource", map[string]any{"scriptId": "7"})); src.ScriptSource != "var a = 1;" {
		t.Fatalf("getScriptSource %q", src.ScriptSource)
	}
	locs := decode[struct{ Locations []cdpLocation }](t, call(t, c, sid, "Debugger.getPossibleBreakpoints", map[string]any{"start": map[string]any{"scriptId": "7", "lineNumber": 4, "columnNumber": 0}}))
	if len(locs.Locations) != 1 {
		t.Fatalf("getPossibleBreakpoints %+v", locs)
	}
	gl := wipParams(t, f, "Debugger.getBreakpointLocations")
	if end, _ := gl[0]["end"].(map[string]any); len(gl) != 1 || end["lineNumber"] != 9.0 || end["scriptId"] != "7" {
		t.Fatalf("WIP getBreakpointLocations %v, want the script's end as end", gl)
	}

	call(t, c, sid, "Debugger.removeBreakpoint", map[string]any{"breakpointId": bp.BreakpointID})
	if p := wipParams(t, f, "Debugger.removeBreakpoint"); len(p) != 1 || p[0]["breakpointId"] != bp.BreakpointID {
		t.Fatalf("WIP removeBreakpoint %v", p)
	}
	for _, m := range []string{"Debugger.setAsyncCallStackDepth", "Debugger.setBlackboxPatterns", "Debugger.setSkipAllPauses"} {
		call(t, c, sid, m, map[string]any{"maxDepth": 32, "patterns": []string{}, "skip": true})
		if n := len(f.ReceivedMethod(m)); n != 0 {
			t.Fatalf("%s reached WebKit %d times; it has no effect", m, n)
		}
	}
}

// The page never pauses through the bridge: a breakpoint without a
// DevTools logpoint or never-pause condition is refused and never sent, a
// pause on exceptions other than none is refused, and pause, resume and
// step are unknown methods.
func TestDebuggerRefusesWhatCouldPause(t *testing.T) {
	_, fp, c, sid, _ := attachPage(t)
	f := fp.f
	call(t, c, sid, "Debugger.enable", nil)
	for _, p := range []map[string]any{
		{"url": "https://one.example/app.js", "lineNumber": 1},
		{"url": "https://one.example/app.js", "lineNumber": 1, "condition": "x > 1"},
		{"url": "https://one.example/app.js", "lineNumber": 1, "condition": "console.log(x)"},
		{"scriptHash": "abc", "lineNumber": 1, "condition": "/** DEVTOOLS_LOGPOINT */ console.log(x)"},
	} {
		if e := callErr(t, c, sid, "Debugger.setBreakpointByUrl", p); e.Code != codeServer {
			t.Fatalf("setBreakpointByUrl %v: error %+v, want a refusal", p, e)
		}
	}
	if n := len(f.ReceivedMethod("Debugger.setBreakpointByUrl")); n != 0 {
		t.Fatalf("refused breakpoints reached WebKit %d times", n)
	}
	if e := callErr(t, c, sid, "Debugger.setPauseOnExceptions", map[string]any{"state": "uncaught"}); e.Code != codeServer {
		t.Fatalf("setPauseOnExceptions uncaught: %+v", e)
	}
	before := len(f.ReceivedMethod("Debugger.setPauseOnExceptions")) // core's agent reset sends the baseline
	call(t, c, sid, "Debugger.setPauseOnExceptions", map[string]any{"state": "none"})
	for _, p := range wipParams(t, f, "Debugger.setPauseOnExceptions") {
		if p["state"] != "none" {
			t.Fatalf("WIP setPauseOnExceptions %v", p)
		}
	}
	if n := len(f.ReceivedMethod("Debugger.setPauseOnExceptions")); n != before {
		t.Fatalf("the client's setPauseOnExceptions none reached WebKit")
	}
	for _, m := range []string{"Debugger.pause", "Debugger.resume", "Debugger.stepOver", "Debugger.stepInto", "Debugger.stepOut", "Debugger.setBreakpoint", "Debugger.evaluateOnCallFrame", "Debugger.setBreakpointsActive"} {
		before := len(f.ReceivedMethod(m))
		if e := callErr(t, c, sid, m, map[string]any{}); e.Code != codeNotFound {
			t.Fatalf("%s: %+v, want wasn't found", m, e)
		}
		if n := len(f.ReceivedMethod(m)); n != before {
			t.Fatalf("%s reached WebKit", m)
		}
	}
	// The never-pause condition of DevTools is safe: no action, continue.
	call(t, c, sid, "Debugger.setBreakpointByUrl", map[string]any{"urlRegex": "app\\.js$", "lineNumber": 2, "condition": "false\n\n//# sourceURL=debugger://breakpoint"})
	sent := wipParams(t, f, "Debugger.setBreakpointByUrl")
	if opts, _ := sent[0]["options"].(map[string]any); len(sent) != 1 || opts["autoContinue"] != true || opts["actions"] != nil || sent[0]["urlRegex"] != "app\\.js$" {
		t.Fatalf("WIP setBreakpointByUrl for a never-pause condition: %v", sent)
	}
}

// A script that WebKit reported before the attachment enabled Debugger
// (another owner had enabled WIP Debugger, so this enable gets no
// scriptParsed) is announced at the enable, once.
func TestDebuggerEnableAnnouncesKnownScripts(t *testing.T) {
	_, fp, c, sid, _ := attachPage(t)
	fp.f.Emit("Debugger.scriptParsed", wipScriptParsed("3", "https://one.example/early.js", ""))
	fp.f.Emit("Debugger.scriptParsed", wipScriptParsed("4", "", "")) // eval code: not kept
	call(t, c, sid, "Page.enable", nil)                              // an event after them: they were read
	seq := c.Seq()
	call(t, c, sid, "Debugger.enable", nil)
	waitEvent(t, c, seq, sid, "Debugger.scriptParsed", func(p json.RawMessage) bool { return strings.Contains(string(p), "early.js") })
	call(t, c, sid, "Debugger.setSkipAllPauses", map[string]any{"skip": true}) // a later reply: no event pending
	if n := len(sessionEvents(c, seq, sid, "Debugger.scriptParsed")); n != 1 {
		t.Fatalf("%d scriptParsed events at the enable, want the kept script once", n)
	}
}
