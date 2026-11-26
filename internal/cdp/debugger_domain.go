package cdp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/edxeth/agent-ios/internal/core"
)

// The Debugger domain, only as far as logpoints need it
// (chrome-devtools-mcp 1.14.0 set_logpoints through its DevTools universe:
// DevTools' DebuggerModel and BreakpointManager). The page must never
// pause: a paused page blocks Runtime.evaluate and every other command of
// its single connection, and the bridge never forwards pause, resume or
// step (those methods are not in the table, so they are refused as
// unknown). So:
//
//   - setBreakpointByUrl takes only a DevTools logpoint (its condition
//     starts with "/** DEVTOOLS_LOGPOINT */ console.log(") or a
//     never-pause condition ("false"), and becomes a WIP breakpoint with
//     options {autoContinue: true, actions: [{type: "evaluate", data:
//     condition}]}: WebKit runs the condition's console.log in the
//     paused frame and continues, whatever the expression returns. Any
//     other condition, or none, is refused, as is a breakpoint by
//     scriptHash (an anonymous script; WIP has no hash).
//   - Debugger.enable, and setBreakpointByUrl without it, acquire WIP
//     Debugger for the attachment without any pause
//     (core.AcquireNoPauseDebugger): the pause on debugger statements and
//     on exceptions go off before WIP Debugger.enable, and breakpoints are
//     activated after it (a new WebKit backend starts with them inactive;
//     CDP's are active), as owned state of the attachment, which a new or
//     provisional target gets in the same order.
//   - setPauseOnExceptions takes only "none"; setSkipAllPauses,
//     setAsyncCallStackDepth and setBlackboxPatterns have no effect (they
//     change only pausing, stepping and async stacks of a debugger that
//     never pauses).
//
// The breakpoints are owned by the attachment in the page's desired state
// (core legacyCall through Manager.RawAs): replayed on new and provisional
// targets and removed at detach. A client knows a breakpoint by the id
// its setBreakpointByUrl returned, also after a replay gave it another id
// (breakpointResolved is translated back, core BreakpointClientID); it
// sees only its own breakpoints' events and removes only its own. Scripts: WIP Debugger.scriptParsed
// becomes CDP Debugger.scriptParsed with the main frame's default context
// and a hash made from the script id; scripts with a URL are kept per
// document (until Debugger.globalObjectCleared), so that an enable reports
// them also when another owner had enabled WIP Debugger before and WebKit
// sends no scriptParsed for this enable.

func init() {
	register(
		Method{Name: "Debugger.enable", Levels: OnPage, Handle: debuggerEnable},
		Method{Name: "Debugger.disable", Levels: OnPage, Handle: debuggerDisable},
		Method{Name: "Debugger.setBreakpointByUrl", Levels: OnPage, Handle: setBreakpointByURL},
		Method{Name: "Debugger.removeBreakpoint", Levels: OnPage, Handle: removeBreakpoint},
		Method{Name: "Debugger.getScriptSource", Levels: OnPage, Handle: getScriptSource},
		Method{Name: "Debugger.getPossibleBreakpoints", Levels: OnPage, Handle: getPossibleBreakpoints},
		Method{Name: "Debugger.setPauseOnExceptions", Levels: OnPage, Handle: setPauseOnExceptions},
		Method{Name: "Debugger.setAsyncCallStackDepth", Levels: OnPage},
		Method{Name: "Debugger.setBlackboxPatterns", Levels: OnPage},
		Method{Name: "Debugger.setSkipAllPauses", Levels: OnPage},
	)
	registerEvents("Debugger", "scriptParsed", "breakpointResolved")
	onPageEvent("Debugger.scriptParsed", scriptParsed)
	onPageEvent("Debugger.globalObjectCleared", func(ps *pageState, _ string, _ json.RawMessage) { ps.dbg.clear() })
	onPageEvent("Debugger.breakpointResolved", breakpointResolved)
}

const domDebugger = "Debugger"

// DevTools' logpoint and never-pause conditions (BreakpointManager.ts
// LOGPOINT_PREFIX and NEVER_PAUSE_HERE_CONDITION; backendCondition adds
// a "//# sourceURL=" line to both).
const (
	logpointPrefix = "/** DEVTOOLS_LOGPOINT */ console.log("
	neverPause     = "false"
)

// maxKeptScripts bounds the scripts with a URL that an attachment keeps
// for a later enable; older ones are dropped first.
const maxKeptScripts = 5000

// dbgState is the Debugger side of a page attachment (under ps.mu).
type dbgState struct {
	kept  []json.RawMessage // CDP scriptParsed params of the document's scripts with a URL, in order
	byID  map[string]wipScript
	shown map[string]bool // script ids announced on the attachment's session
}

// wipScript is what the bridge keeps of a WIP script.
type wipScript struct {
	endLine, endColumn int
}

func (d *dbgState) clear() {
	d.kept, d.byID, d.shown = nil, nil, nil
}

// scriptParsed translates a WIP Debugger.scriptParsed (scriptId, url,
// startLine..endColumn, isContentScript, sourceURL, sourceMapURL,
// hasSourceURL, module) into CDP's shape.
func scriptParsed(ps *pageState, target string, params json.RawMessage) {
	var w struct {
		ScriptID     string `json:"scriptId"`
		URL          string `json:"url"`
		StartLine    int    `json:"startLine"`
		StartColumn  int    `json:"startColumn"`
		EndLine      int    `json:"endLine"`
		EndColumn    int    `json:"endColumn"`
		SourceURL    string `json:"sourceURL"`
		SourceMapURL string `json:"sourceMapURL"`
		HasSourceURL bool   `json:"hasSourceURL"`
		Module       bool   `json:"module"`
	}
	if json.Unmarshal(params, &w) != nil || w.ScriptID == "" {
		return
	}
	u := w.URL
	if w.SourceURL != "" {
		u = w.SourceURL
	}
	sum := sha256.Sum256([]byte(target + "\x00" + w.ScriptID))
	ev := map[string]any{
		"scriptId": w.ScriptID, "url": u,
		"startLine": w.StartLine, "startColumn": w.StartColumn, "endLine": w.EndLine, "endColumn": w.EndColumn,
		"executionContextId": ps.mainContextLocked(), "hash": hex.EncodeToString(sum[:20]),
		"executionContextAuxData": map[string]any{"isDefault": true, "type": "default", "frameId": ps.target},
		"isLiveEdit":              false, "sourceMapURL": w.SourceMapURL, "hasSourceURL": w.HasSourceURL || w.SourceURL != "",
		"isModule": w.Module, "scriptLanguage": "JavaScript", "embedderName": u,
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	d := &ps.dbg
	if d.byID == nil {
		d.byID, d.shown = map[string]wipScript{}, map[string]bool{}
	}
	d.byID[w.ScriptID] = wipScript{endLine: w.EndLine, endColumn: w.EndColumn}
	if u != "" {
		d.kept = append(d.kept, raw)
		if len(d.kept) > maxKeptScripts {
			d.kept = d.kept[1:]
		}
	}
	if ps.enabled[domDebugger] && !d.shown[w.ScriptID] {
		d.shown[w.ScriptID] = true
		ps.emitLocked(domDebugger, "Debugger.scriptParsed", json.RawMessage(raw))
	}
}

// announceScriptsLocked sends the kept scripts that the session has not
// seen (Debugger.enable).
func (ps *pageState) announceScriptsLocked() {
	d := &ps.dbg
	for _, raw := range d.kept {
		var v struct {
			ScriptID string `json:"scriptId"`
		}
		if json.Unmarshal(raw, &v) != nil || d.shown[v.ScriptID] {
			continue
		}
		d.shown[v.ScriptID] = true
		ps.emitLocked(domDebugger, "Debugger.scriptParsed", raw)
	}
}

// breakpointResolved forwards WIP's event (its shape is CDP's:
// breakpointId, location{scriptId, lineNumber, columnNumber}) to the
// attachment that owns the breakpoint, with the id that attachment got.
func breakpointResolved(ps *pageState, target string, params json.RawMessage) {
	if ps.s == nil {
		return
	}
	var v struct {
		BreakpointID string          `json:"breakpointId"`
		Location     json.RawMessage `json:"location"`
	}
	if json.Unmarshal(params, &v) != nil {
		return
	}
	id, ok := ps.s.pg.PS.State.BreakpointClientID(ps.s.owner, target, v.BreakpointID)
	if !ok {
		return // another owner's breakpoint, or a stale id of a replaced target
	}
	ps.emitLocked(domDebugger, "Debugger.breakpointResolved", map[string]any{"breakpointId": id, "location": v.Location})
}

// rawAs sends a WIP call as the attachment's owned call and decodes its
// reply into out (when not nil).
func (s *session) rawAs(ctx context.Context, mo *core.ManagedOp, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	res, err := s.c.s.o.Manager.RawAs(ctx, mo, s.owner, method, raw)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(res, out)
	}
	return nil
}

func debuggerEnable(ctx context.Context, call *Call) (any, Later, error) {
	s := call.session
	ps := s.page
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		// The document first: scriptParsed names its main frame's
		// default context (executionContextId).
		if _, err := ps.readTree(ctx, mo); err != nil {
			return err
		}
		return s.c.s.o.Manager.AcquireNoPauseDebugger(ctx, mo, s.owner)
	})
	if err != nil {
		return nil, nil, err
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.enabled[domDebugger] = true
	if ps.dbg.shown == nil {
		ps.dbg.byID, ps.dbg.shown = map[string]wipScript{}, map[string]bool{}
	}
	ps.announceScriptsLocked()
	return map[string]any{"debuggerId": ps.target + ".debugger"}, nil, nil
}

// debuggerDisable ends the attachment's Debugger events and its WIP
// Debugger state: the owner's disable removes its breakpoints and
// settings (core disableDomain), and WIP Debugger itself when no other
// owner needs it.
func debuggerDisable(ctx context.Context, call *Call) (any, Later, error) {
	s := call.session
	ps := s.page
	ps.mu.Lock()
	ps.enabled[domDebugger] = false
	ps.dbg.shown = map[string]bool{}
	ps.mu.Unlock()
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		return s.rawAs(ctx, mo, "Debugger.disable", nil, nil)
	})
	return nil, nil, err
}

// logpointCondition reports whether a CDP breakpoint condition is one
// that the bridge may set: a DevTools logpoint or never-pause condition,
// with or without DevTools' trailing sourceURL comment.
func logpointCondition(cond string) bool {
	body, _, _ := strings.Cut(cond, "\n\n//# sourceURL=")
	return strings.HasPrefix(body, logpointPrefix) && strings.HasSuffix(strings.TrimSpace(body), ")") ||
		strings.TrimSpace(body) == neverPause
}

// errMayPause refuses a breakpoint that could pause the page.
var errMayPause = serverError("Debugger.setBreakpointByUrl: only logpoints (a DevTools logpoint condition) can be set on iOS WebKit through agent-ios; a breakpoint that pauses would block the page's only debugger connection")

func setBreakpointByURL(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		LineNumber   *int   `json:"lineNumber"`
		URL          string `json:"url"`
		URLRegex     string `json:"urlRegex"`
		ScriptHash   string `json:"scriptHash"`
		ColumnNumber *int   `json:"columnNumber"`
		Condition    string `json:"condition"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	switch {
	case p.LineNumber == nil:
		return nil, nil, invalidParams("lineNumber is required")
	case p.URL == "" && p.URLRegex == "":
		if p.ScriptHash != "" {
			return nil, nil, serverError("Debugger.setBreakpointByUrl: a breakpoint by scriptHash (a script without a URL) is not supported on iOS WebKit")
		}
		return nil, nil, invalidParams("either url or urlRegex must be specified")
	case p.URL != "" && p.URLRegex != "":
		return nil, nil, invalidParams("url and urlRegex cannot be both specified")
	case !logpointCondition(p.Condition):
		return nil, nil, errMayPause
	}
	wp := map[string]any{"lineNumber": *p.LineNumber}
	if p.URL != "" {
		wp["url"] = p.URL
	} else {
		wp["urlRegex"] = p.URLRegex
	}
	if p.ColumnNumber != nil {
		wp["columnNumber"] = *p.ColumnNumber
	}
	opts := map[string]any{"autoContinue": true}
	if body, _, _ := strings.Cut(p.Condition, "\n\n//# sourceURL="); strings.TrimSpace(body) != neverPause {
		opts["actions"] = []any{map[string]any{"type": "evaluate", "data": p.Condition}}
	}
	wp["options"] = opts
	var res struct {
		BreakpointID string            `json:"breakpointId"`
		Locations    []json.RawMessage `json:"locations"`
	}
	s := call.session
	err := s.do(ctx, call.Method, core.ClassMutate, func(ctx context.Context, mo *core.ManagedOp) error {
		// Also without Debugger.enable: no breakpoint before the page
		// is safe from pausing (implicit acquisition).
		if err := s.c.s.o.Manager.AcquireNoPauseDebugger(ctx, mo, s.owner); err != nil {
			return err
		}
		return s.rawAs(ctx, mo, "Debugger.setBreakpointByUrl", wp, &res)
	})
	if err != nil {
		return nil, nil, err
	}
	if res.Locations == nil {
		res.Locations = []json.RawMessage{}
	}
	return map[string]any{"breakpointId": res.BreakpointID, "locations": res.Locations}, nil, nil
}

func removeBreakpoint(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		BreakpointID string `json:"breakpointId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.BreakpointID == "" {
		return nil, nil, invalidParams("breakpointId is required")
	}
	s := call.session
	if !s.pg.PS.State.OwnsBreakpoint(s.owner, p.BreakpointID) {
		return nil, nil, serverError("Debugger.removeBreakpoint: breakpoint %s was not set by this session", p.BreakpointID)
	}
	err := s.do(ctx, call.Method, core.ClassMutate, func(ctx context.Context, mo *core.ManagedOp) error {
		return s.rawAs(ctx, mo, "Debugger.removeBreakpoint", map[string]string{"breakpointId": p.BreakpointID}, nil)
	})
	return nil, nil, err
}

func getScriptSource(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		ScriptID string `json:"scriptId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	var res struct {
		ScriptSource string `json:"scriptSource"`
	}
	s := call.session
	err := s.do(ctx, call.Method, core.ClassRead, func(ctx context.Context, mo *core.ManagedOp) error {
		return s.rawAs(ctx, mo, "Debugger.getScriptSource", map[string]string{"scriptId": p.ScriptID}, &res)
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"scriptSource": res.ScriptSource}, nil, nil
}

// cdpLocation is a CDP (and WIP) Debugger.Location.
type cdpLocation struct {
	ScriptID     string `json:"scriptId"`
	LineNumber   int    `json:"lineNumber"`
	ColumnNumber *int   `json:"columnNumber,omitempty"`
}

// getPossibleBreakpoints is WIP Debugger.getBreakpointLocations (iOS 16.4
// and later; core refuses it on older lists). WIP needs an end: without
// one, the end of the script as WebKit reported it.
func getPossibleBreakpoints(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Start *cdpLocation `json:"start"`
		End   *cdpLocation `json:"end"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.Start == nil {
		return nil, nil, invalidParams("start is required")
	}
	s := call.session
	ps := s.page
	end := p.End
	if end == nil {
		ps.mu.Lock()
		sc, ok := ps.dbg.byID[p.Start.ScriptID]
		ps.mu.Unlock()
		if !ok {
			return nil, nil, serverError("Debugger.getPossibleBreakpoints: unknown script %s", p.Start.ScriptID)
		}
		col := sc.endColumn
		end = &cdpLocation{ScriptID: p.Start.ScriptID, LineNumber: sc.endLine, ColumnNumber: &col}
	}
	var res struct {
		Locations []json.RawMessage `json:"locations"`
	}
	err := s.do(ctx, call.Method, core.ClassRead, func(ctx context.Context, mo *core.ManagedOp) error {
		return s.rawAs(ctx, mo, "Debugger.getBreakpointLocations", map[string]any{"start": p.Start, "end": end}, &res)
	})
	if err != nil {
		return nil, nil, err
	}
	if res.Locations == nil {
		res.Locations = []json.RawMessage{}
	}
	return map[string]any{"locations": res.Locations}, nil, nil
}

// setPauseOnExceptions accepts only "none": the bridge never lets the page
// pause. Nothing is sent: the attachment never set another state.
func setPauseOnExceptions(_ context.Context, call *Call) (any, Later, error) {
	var p struct {
		State string `json:"state"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.State != "none" {
		return nil, nil, serverError("Debugger.setPauseOnExceptions %q: the page cannot pause through agent-ios on iOS WebKit; only \"none\" is accepted", p.State)
	}
	return nil, nil, nil
}
