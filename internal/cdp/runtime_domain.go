package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/logsafe"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// The Runtime domain (design docs/design/agentic-parity.md, section 5.3).
// Runtime.evaluate is in page_seed.go; contexts and aliases in worlds.go.

func init() {
	register(
		Method{Name: "Runtime.enable", Levels: OnPage, Handle: runtimeEnable},
		Method{Name: "Runtime.disable", Levels: OnPage, Handle: runtimeDisable},
		Method{Name: "Runtime.callFunctionOn", Levels: OnPage, Handle: callFunctionOn},
		Method{Name: "Runtime.getProperties", Levels: OnPage, Handle: getProperties},
		Method{Name: "Runtime.releaseObject", Levels: OnPage, Handle: releaseObject},
		Method{Name: "Runtime.releaseObjectGroup", Levels: OnPage, Handle: releaseObject},
		Method{Name: "Runtime.addBinding", Levels: OnPage, Handle: addBinding},
		Method{Name: "Runtime.removeBinding", Levels: OnPage, Handle: removeBinding},
	)
	registerEvents("Runtime", "executionContextCreated", "executionContextsCleared", "executionContextDestroyed",
		"consoleAPICalled", "exceptionThrown", "bindingCalled")
	onPageEvent("Console.messageAdded", consoleMessage)
	onPageEvent("Console.messageRepeatCountUpdated", consoleRepeated)
	onPageEvent("Console.messagesCleared", func(ps *pageState, target string, _ json.RawMessage) { delete(ps.lastConsole, target) })
}

// errNoContext is Chrome's error for an unknown execution context.
var errNoContext = serverError("Cannot find context with specified id")

// runtimeEnable turns on the attachment's Runtime events: WIP Runtime and
// Console are owned by the attachment, and every context of the current
// document is announced before the reply (P2 item 6), the default
// contexts first, then the aliases.
func runtimeEnable(ctx context.Context, call *Call) (any, Later, error) {
	s := call.session
	ps := s.page
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		if err := s.owned(ctx, mo, "Runtime.enable", nil); err != nil {
			return err
		}
		if err := s.owned(ctx, mo, "Console.enable", nil); err != nil {
			return err
		}
		_, err := ps.readTree(ctx, mo)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.enabled[domRuntime] = true
	ps.announceAllLocked()
	for _, f := range ps.framesLocked() {
		ec := s.pg.PS.Sched.FrameContext(f)
		if f == ps.main.wip.ID || ec.ID != 0 {
			ps.frameContextsLocked(f)
		}
	}
	return nil, nil, nil
}

func runtimeDisable(ctx context.Context, call *Call) (any, Later, error) {
	ps := call.session.page
	ps.setEnabled(domRuntime, false)
	err := call.session.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		return call.session.owned(ctx, mo, "Runtime.disable", nil)
	})
	return nil, nil, err
}

// boundOp is an admitted Runtime operation with the binding of all its
// sends (W5b-1 review F2): the first call, the function call of
// callFunctionOn, awaitPromise, the read by value and every release go to
// the same target and connection, because remote object ids are valid
// only there. With an execution context (bind), the operation works in
// that context (core Op.SetExecutionContext): each send goes to the
// context's reporting target and connection and fails when either was
// replaced, and a Runtime.evaluate gets the context's WIP id. Without
// one, the sends go to the target that was active at the first send
// (webkit.WithTarget), so a commit since fails them.
type boundOp struct {
	s      *session
	mo     *core.ManagedOp
	ctx    context.Context
	follow bool // no context: the first send goes to the target active then
	sent   bool
	wipID  int64 // the bound context's WIP id; 0 without one
}

// bind binds the admitted operation mo to the CDP execution context id or
// unique (none: the main world of the main frame at the first send).
func (s *session) bind(mo *core.ManagedOp, id int, unique string) (*boundOp, error) {
	b := &boundOp{s: s, mo: mo, ctx: mo.Op.Context()}
	if id == 0 && unique == "" {
		b.follow = true
		b.ctx = pinActive(b.ctx, s.pg)
		return b, nil
	}
	ec, err := s.wipContext(mo.Op.Context(), id, unique)
	if err != nil {
		return nil, err
	}
	mo.Op.SetExecutionContext(ec)
	b.wipID = ec.ID
	return b, nil
}

// pinActive addresses ctx's sends to the page's active target.
func pinActive(ctx context.Context, pg *core.Page) context.Context {
	if t := activeTarget(pg); t != "" {
		return webkit.WithTarget(ctx, t)
	}
	return ctx
}

// send sends one call of the operation with its binding. When no context
// was named and a commit came between the read of the active target and
// the first send, which then reached no page, it goes once more to the
// new active target.
func (b *boundOp) send(method string, params any) (json.RawMessage, error) {
	raw, err := b.mo.Op.Caller().Send(b.ctx, method, params)
	if b.follow && !b.sent && errors.Is(err, webkit.ErrNotSent) && errors.Is(err, webkit.ErrTargetSwapped) {
		b.ctx = pinActive(b.mo.Op.Context(), b.s.pg)
		raw, err = b.mo.Op.Caller().Send(b.ctx, method, params)
	}
	// A message above the transport's limit was refused before it was
	// written, so the staged sends that follow it count as the first.
	b.sent = b.sent || wireLimit(err) == nil
	return raw, err
}

// call is send with the reply decoded as a WIP evaluation result.
func (b *boundOp) call(method string, params any) (wipResult, error) {
	raw, err := b.send(method, params)
	if err != nil {
		return wipResult{}, err
	}
	var r wipResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return wipResult{}, fmt.Errorf("decoding the WIP %s reply: %w", method, err)
	}
	return r, nil
}

// wipContext resolves the CDP execution context id or unique for a call
// of an admitted operation (ctx is the operation's context) to the WIP
// main world of its frame, and proves that this world belongs to the
// document in which the attachment announced the context; otherwise it
// fails closed with Chrome's unknown-context error and nothing runs. The
// candidate is the frame's newest main world that the page's connection
// recorded (core Scheduler.FrameContext: the session records every
// context, also those reported before this attachment enabled Runtime).
// It is proven when, after the attachment has handled every event that
// its connections delivered before that read (caughtUp), so its document
// is at least as new as the candidate:
//   - the context still exists in the attachment's document, so no
//     navigation of its frame came before the candidate's read;
//   - the candidate's reporting target is the target that reported the
//     frame, and its connection is the attachment's connection, whose
//     events the attachment has seen without a gap (staging.go);
//   - when the attachment's own events reported the frame's main world,
//     the candidate is that world.
//
// A navigation after the proof makes WebKit refuse the WIP id, which no
// newer document reuses in its target (boundErr).
func (s *session) wipContext(ctx context.Context, id int, unique string) (core.ExecContext, error) {
	ps := s.page
	ps.mu.Lock()
	c, ok := ps.lookupContextLocked(id, unique)
	ps.mu.Unlock()
	if !ok {
		return core.ExecContext{}, errNoContext
	}
	frame := c.wipFrame
	ec := s.pg.PS.Sched.FrameContext(frame)
	if err := ps.caughtUp(ctx); err != nil {
		return core.ExecContext{}, err
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	c, ok = ps.lookupContextLocked(id, unique)
	if !ok || c.wipFrame != frame {
		return core.ExecContext{}, errNoContext
	}
	f := ps.frameInfoLocked(frame)
	target, conn := ec.Source()
	wipID := int64(ps.mainWorldLocked(frame))
	// A frame without a recorded world has the zero candidate, whose
	// connection is nil: never the attachment's.
	if f == nil || conn != ps.conn || target != f.target || (wipID != 0 && wipID != ec.ID) {
		return core.ExecContext{}, errNoContext
	}
	return ec, nil
}

// boundErr is Chrome's unknown-context error for a failed first call of
// an operation bound to a document that is gone (boundOp): WebKit no
// longer knows the WIP context id (a newer document replaced it in the
// same target), or the target or the connection was replaced before the
// call was sent. Other errors stay.
func boundErr(err error) error {
	var e *webkit.ErrorData
	switch {
	case err == nil:
		return nil
	case errors.Is(err, webkit.ErrNotSent) && (errors.Is(err, webkit.ErrTargetSwapped) || errors.Is(err, webkit.ErrTargetAbandoned)):
		return errNoContext
	case errors.As(err, &e) && strings.Contains(e.Message, wipNoContext):
		return errNoContext
	}
	return err
}

// wipNoContext is the WebKit error text for a Runtime.evaluate whose
// contextId names no live context (InspectorRuntimeAgent
// injectedScriptForEval; measured on WebKitGTK 2.54, docs/notes/w1b.md,
// and core's errNoContext).
const wipNoContext = "Missing injected script for given executionContextId"

// callArg is a CDP Runtime.CallArgument.
type callArg struct {
	Value               json.RawMessage `json:"value"`
	UnserializableValue string          `json:"unserializableValue"`
	ObjectID            string          `json:"objectId"`
}

// unserializable lists the CDP unserializable values; WIP CallArgument
// has only value and objectId, so they are set in a wrapper (P2).
var unserializable = regexp.MustCompile(`^(undefined|NaN|Infinity|-Infinity|-0|-?[0-9]+n)$`)

// applyFunction is called on a function object made from a declaration
// (callFunctionOn with an executionContextId): it calls the function with
// the arguments and an undefined receiver, as V8 does for a call without
// an objectId.
const applyFunction = "function(...a) { return this.apply(undefined, a); }"

// callFunctionOn: with an objectId the call goes to WIP
// Runtime.callFunctionOn; with an execution context it is one WIP call
// (directCall) when it can be, else the declaration is first turned into
// a function object by Runtime.evaluate in that context and then called
// through applyFunction. awaitPromise uses
// Runtime.awaitPromise (WIP callFunctionOn has no awaitPromise on 16.6).
func callFunctionOn(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		FunctionDeclaration string    `json:"functionDeclaration"`
		ObjectID            string    `json:"objectId"`
		Arguments           []callArg `json:"arguments"`
		Silent              bool      `json:"silent"`
		ReturnByValue       bool      `json:"returnByValue"`
		UserGesture         bool      `json:"userGesture"`
		AwaitPromise        bool      `json:"awaitPromise"`
		ExecutionContextID  int       `json:"executionContextId"`
		ObjectGroup         string    `json:"objectGroup"`
		UniqueContextID     string    `json:"uniqueContextId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	s := call.session
	if p.ObjectID == "" && p.ExecutionContextID == 0 && p.UniqueContextID == "" {
		return nil, nil, &Error{Code: codeInvalidParams, Message: "Either ObjectId or executionContextId or uniqueContextId must be specified"}
	}
	decl := p.FunctionDeclaration
	args := make([]map[string]any, len(p.Arguments))
	var fixes []string
	for i, a := range p.Arguments {
		switch {
		case a.ObjectID != "":
			args[i] = map[string]any{"objectId": a.ObjectID}
		case a.UnserializableValue != "":
			if !unserializable.MatchString(a.UnserializableValue) {
				return nil, nil, &Error{Code: codeInvalidParams, Message: fmt.Sprintf("Invalid unserializableValue %q", a.UnserializableValue)}
			}
			args[i] = map[string]any{}
			fixes = append(fixes, fmt.Sprintf("a[%d] = %s;", i, a.UnserializableValue))
		case a.Value != nil:
			args[i] = map[string]any{"value": a.Value}
		default:
			args[i] = map[string]any{}
		}
	}
	if len(fixes) > 0 {
		decl = fmt.Sprintf("(function() { const f = (%s); return function(...a) { %s return f.apply(this, a); }; })()", decl, strings.Join(fixes, " "))
	}
	mo, end, err := s.begin(ctx, call.Method, core.ClassMutate)
	if err != nil {
		return nil, nil, err
	}
	b, err := s.bind(mo, p.ExecutionContextID, p.UniqueContextID)
	if err != nil {
		end()
		return nil, nil, err
	}
	this := p.ObjectID
	if this == "" && p.ObjectGroup == "" {
		opts := map[string]any{
			"returnByValue": p.ReturnByValue && !p.AwaitPromise, "emulateUserGesture": p.UserGesture,
			"doNotPauseOnExceptionsAndMuteConsole": p.Silent,
		}
		if r, ok, err := b.directCall(decl, p.Arguments, args, opts); ok {
			return b.finish(end, r, err, p.ReturnByValue, p.AwaitPromise)
		}
	}
	if this == "" {
		ev := map[string]any{"expression": "(" + decl + ")", "objectGroup": cmpOr(p.ObjectGroup, "iwdp-cdp-call")}
		fr, err := b.call("Runtime.evaluate", ev)
		if wl := wireLimit(err); wl != nil {
			// Above the transport's message limit: staged (large_source.go).
			fr, err = stageSink{b: b}.evaluate(call.Method, "function declaration", decl, wl, ev, staged.fnExpr)
		}
		if err != nil || fr.WasThrown {
			defer end()
			if err != nil {
				return nil, nil, boundErr(err)
			}
			return cdpResult(fr), nil, nil
		}
		this, _ = fr.Result["objectId"].(string)
		if this == "" {
			end()
			return nil, nil, serverError("Given expression does not evaluate to a function")
		}
		decl = applyFunction
	}
	cf := map[string]any{
		"objectId": this, "functionDeclaration": decl, "arguments": args,
		"returnByValue": p.ReturnByValue && !p.AwaitPromise, "emulateUserGesture": p.UserGesture,
		"doNotPauseOnExceptionsAndMuteConsole": p.Silent,
	}
	r, err := b.call("Runtime.callFunctionOn", cf)
	if wl := wireLimit(err); wl != nil && p.ObjectID != "" {
		r, err = stageCall(b, call.Method, decl, wl, cf)
	}
	if p.ObjectID == "" {
		b.release(this, "a function object") // the one made from the declaration
	}
	return b.finish(end, r, err, p.ReturnByValue, p.AwaitPromise)
}

// directCall makes a callFunctionOn with an execution context one WIP
// round trip (FIX-O; through iwdp each takes about 40 ms on the iOS
// Simulator): with an argument object of the bound context, a WIP
// callFunctionOn on that object with the declaration bound to an
// undefined receiver; with only value arguments, a Runtime.evaluate in the
// context that applies the declaration to them as literals. Both evaluate
// the declaration at the context's global scope with an undefined
// receiver, as the evaluate and trampoline path does. ok is false, and
// no message was written, when that path must run instead: an argument object
// of another context (WebKit refuses it there, as Chrome does), a value
// that a literal does not keep (__proto__), or a message above the
// transport's limit. The result object of the call on an argument object
// belongs to that object's group, so a call with an objectGroup never
// comes here.
func (b *boundOp) directCall(decl string, in []callArg, args []map[string]any, opts map[string]any) (wipResult, bool, error) {
	if b.wipID == 0 {
		return wipResult{}, false, nil
	}
	target := ""
	lits := make([]string, len(in))
	for i, a := range in {
		switch {
		case a.ObjectID != "":
			id, ok := objectContext(a.ObjectID)
			if !ok || id != b.wipID {
				return wipResult{}, false, nil
			}
			if target == "" {
				target = a.ObjectID
			}
		case a.UnserializableValue == "" && a.Value != nil:
			if bytes.Contains(a.Value, []byte("__proto__")) {
				return wipResult{}, false, nil
			}
			lits[i] = string(a.Value)
		default:
			lits[i] = "undefined" // unserializable values are set in decl
		}
	}
	method, params := "Runtime.evaluate", map[string]any{}
	if target != "" {
		method = "Runtime.callFunctionOn"
		params["objectId"] = target
		params["functionDeclaration"] = "(" + decl + "\n).bind(undefined)"
		params["arguments"] = args
	} else {
		params["expression"] = "(" + decl + "\n).apply(undefined, [" + strings.Join(lits, ", ") + "])"
		params["objectGroup"] = "iwdp-cdp-call"
	}
	maps.Copy(params, opts)
	r, err := b.call(method, params)
	if wireLimit(err) != nil {
		return wipResult{}, false, nil
	}
	var e *webkit.ErrorData
	if errors.As(err, &e) && strings.Contains(e.Message, wipNoObjectContext) {
		err = errNoContext // the argument's context, the bound one, is gone
	}
	return r, true, boundErr(err)
}

// objectContext returns the context of a WIP remote object id: WebKit's
// ids are JSON objects that name their injected script, whose id is the
// context's (InjectedScriptManager).
func objectContext(objectID string) (int64, bool) {
	var o struct {
		InjectedScriptID *int64 `json:"injectedScriptId"`
	}
	if json.Unmarshal([]byte(objectID), &o) != nil || o.InjectedScriptID == nil {
		return 0, false
	}
	return *o.InjectedScriptID, true
}

// wipNoObjectContext is the WebKit error text for a call on an object
// whose context is gone (InspectorRuntimeAgent::callFunctionOn).
const wipNoObjectContext = "Missing injected script for given objectId"

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// release releases a remote object the client never saw; what names it
// for the log.
func (b *boundOp) release(objectID any, what logsafe.Label) {
	if _, err := b.send("Runtime.releaseObject", map[string]any{"objectId": objectID}); err != nil {
		s := b.s
		s.c.s.o.Logf("cdp: %s: releasing %s of %s session %s: %v", logsafe.ID(s.c.name), what, logsafe.ID(s.kind), logsafe.ID(shortID(s.id)), logsafe.Error(err))
	}
}

// finish turns the WIP result r of an evaluate or callFunctionOn sent by
// reference into the CDP result. A Promise is awaited with
// Runtime.awaitPromise in a Later, so the session's next messages do not
// wait for it, and the operation is detached from the page's admission
// first (core Op.Detach), so the page's next operations do not wait for
// it either: in Chrome other commands run while a promise is awaited,
// and a promise can need them to settle (a Puppeteer binding's answer).
// Another object is read by value when returnByValue asks for it. end is
// called when the operation's work is done.
func (b *boundOp) finish(end func(), r wipResult, err error, byValue, await bool) (any, Later, error) {
	if err != nil || r.WasThrown || !await || !isPromise(r.Result) {
		defer end()
		if err == nil && !r.WasThrown && await && byValue && r.Result["objectId"] != nil {
			r, err = b.valueOf(r.Result["objectId"])
		}
		if err != nil {
			return nil, nil, err
		}
		return cdpResult(r), nil, nil
	}
	return nil, func(context.Context) (any, error) {
		defer end()
		b.mo.Op.Detach()
		ar, err := b.call("Runtime.awaitPromise", map[string]any{
			"promiseObjectId": r.Result["objectId"], "returnByValue": byValue,
		})
		if err != nil {
			return nil, err
		}
		return cdpResult(ar), nil
	}, nil
}

// getProperties maps WIP property descriptors to CDP.
func getProperties(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		ObjectID               string `json:"objectId"`
		OwnProperties          bool   `json:"ownProperties"`
		AccessorPropertiesOnly bool   `json:"accessorPropertiesOnly"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	raw, err := call.session.send(ctx, core.ClassRead, "Runtime.getProperties", map[string]any{"objectId": p.ObjectID, "ownProperties": p.OwnProperties})
	if err != nil {
		return nil, nil, err
	}
	var r struct {
		Properties         []map[string]any `json:"properties"`
		InternalProperties []map[string]any `json:"internalProperties"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, nil, fmt.Errorf("decoding the WIP Runtime.getProperties reply: %w", err)
	}
	props := []map[string]any{}
	for _, d := range r.Properties {
		if p.AccessorPropertiesOnly && d["get"] == nil && d["set"] == nil {
			continue
		}
		out := map[string]any{"name": d["name"], "configurable": d["configurable"] == true, "enumerable": d["enumerable"] == true}
		for _, k := range []string{"writable", "wasThrown", "isOwn"} {
			if v, ok := d[k]; ok {
				out[k] = v
			}
		}
		for _, k := range []string{"value", "get", "set", "symbol"} {
			if o, ok := d[k].(map[string]any); ok {
				out[k] = toCDP(o)
			}
		}
		props = append(props, out)
	}
	internal := []map[string]any{}
	for _, d := range r.InternalProperties {
		out := map[string]any{"name": d["name"]}
		if o, ok := d["value"].(map[string]any); ok {
			out["value"] = toCDP(o)
		}
		internal = append(internal, out)
	}
	return map[string]any{"result": props, "internalProperties": internal}, nil, nil
}

// releaseObject serves Runtime.releaseObject and releaseObjectGroup, whose
// params WIP shares.
func releaseObject(ctx context.Context, call *Call) (any, Later, error) {
	_, err := call.session.send(ctx, core.ClassRead, call.Method, call.Params)
	return nil, nil, err
}

// ---- bindings ----

// binding is a Runtime.addBinding of the attachment. All worlds of a
// frame share its WIP main world (worlds.go), so the page function cannot
// tell which world called it; Runtime.bindingCalled names the calling
// frame's context of the world the binding was added for, as Chrome does:
// Puppeteer handles the call only in the ExecutionContext whose id the
// event names (puppeteer-core 25.18.1 cdp/ExecutionContext.js
// #onBindingCalled), and the utility world of every frame adds
// __ariaQuerySelector, which aria/ selectors call, by
// executionContextName. The page function of a child frame tags its
// messages with the frame's default context (frameBindSource); the main
// frame's function has no tag.
type binding struct {
	token string // marks the page function's messages (addBinding)
	world string // executionContextName; "" for none
	ctx   int    // executionContextId; 0 for none
}

// bindingContextLocked returns the context id that a call of b reports
// when the page function of the frame whose default context is from (0,
// or a context that is gone: the main frame) called it: b's context when
// it is in that frame, else that frame's context of b's world (of the
// world of b's context when b names one that still exists), else the
// frame's default context.
func (ps *pageState) bindingContextLocked(b binding, from int) int {
	if ps.main == nil {
		return 0
	}
	frame := ps.main.wip.ID
	if c, ok := ps.lookupContextLocked(from, ""); from != 0 && ok {
		frame = c.wipFrame
	}
	world := b.world
	if b.ctx != 0 {
		c, ok := ps.lookupContextLocked(b.ctx, "")
		switch {
		case ok && c.wipFrame == frame:
			return c.id
		case ok:
			world = c.world
		default:
			world = ""
		}
	}
	for _, c := range ps.ctxs {
		if c.wipFrame == frame && c.world == world {
			return c.id
		}
	}
	return ps.contextLocked(frame, "").id
}

// frameBind is a binding installed in the document of a child frame,
// named by the frame's default context (frameBound).
type frameBind struct {
	ctx         int
	name, token string
}

// frameBindSource installs bindings (%s: a JSON list of {n, k}) in a child
// frame's document, whose default context is %d: each page function
// writes the marked message of 66-cdp-page.js cdpBind with that context
// as f, so that bindingCalled can name the frame. The agent runtime is not
// needed in the frame.
const frameBindSource = `(function (l, f) {
  var d = console.debug.bind(console);
  l.forEach(function (b) {
    window[b.n] = function (p) {
      try { d("` + bindMarker + `" + JSON.stringify({ n: b.n, k: b.k, f: f, p: String(p) })); } catch (e) { /* console replaced by the page */ }
    };
  });
  return true;
})(%s, %d)`

// bindChildFrames installs the binding name in the document of every child
// frame of the current document that has none with token yet (Chrome adds
// a binding to the contexts of every frame; Puppeteer adds its utility
// world's bindings once per frame context and calls them in each frame).
// New documents get it from the bootstrap part, untagged, until the next
// addBinding. A frame that navigated or detached meanwhile is skipped.
func (s *session) bindChildFrames(mo *core.ManagedOp, name, token string) error {
	ps := s.page
	ps.mu.Lock()
	var ids []int
	live := map[int]bool{}
	if ps.main != nil {
		for _, c := range ps.ctxs {
			live[c.id] = true
			if c.world == "" && c.wipFrame != ps.main.wip.ID && !ps.frameBound[frameBind{c.id, name, token}] {
				ids = append(ids, c.id)
			}
		}
	}
	for k := range ps.frameBound {
		if !live[k.ctx] {
			delete(ps.frameBound, k)
		}
	}
	ps.mu.Unlock()
	list, _ := json.Marshal([]map[string]string{{"n": name, "k": token}}) // strings only
	for _, id := range ids {
		b, err := s.bind(mo, id, "")
		if err == nil {
			_, err = b.send("Runtime.evaluate", map[string]any{"expression": fmt.Sprintf(frameBindSource, list, id), "returnByValue": true})
			err = boundErr(err)
		}
		if errors.Is(err, errNoContext) {
			continue
		}
		if err != nil {
			return err
		}
		ps.mu.Lock()
		ps.frameBound[frameBind{id, name, token}] = true
		ps.mu.Unlock()
	}
	return nil
}

// addBinding installs window[name] in the current document and, through
// the attachment's bootstrap part, in new ones. The page function writes
// a marked console.debug message with the binding's token; the bridge
// turns it into Runtime.bindingCalled and hides it from console output.
// The token keeps messages of an earlier attachment's binding of the same
// name (which Console.enable can replay) from reaching this client.
func addBinding(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Name                 string `json:"name"`
		ExecutionContextID   int    `json:"executionContextId"`
		ExecutionContextName string `json:"executionContextName"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.Name == "" {
		return nil, nil, &Error{Code: codeInvalidParams, Message: "Invalid parameters: name is empty"}
	}
	s := call.session
	ps := s.page
	// A name that is added again keeps its token: the functions that an
	// earlier addBinding installed, also in child frames, stay valid.
	ps.mu.Lock()
	token := randomHex(8)
	if old, ok := ps.bindings[p.Name]; ok {
		token = old.token
	}
	ps.bindings[p.Name] = binding{token: token, world: p.ExecutionContextName, ctx: p.ExecutionContextID}
	ps.mu.Unlock()
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		if err := s.owned(ctx, mo, "Console.enable", nil); err != nil {
			return err
		}
		if err := ps.installBoot(ctx, mo); err != nil {
			return err
		}
		if err := runtimeCall(ctx, mo, "cdpBind", map[string]any{"add": []map[string]string{{"n": p.Name, "k": token}}}); err != nil {
			return err
		}
		return s.bindChildFrames(mo, p.Name, token)
	})
	return nil, nil, err
}

func removeBinding(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	ps := call.session.page
	ps.mu.Lock()
	_, had := ps.bindings[p.Name]
	delete(ps.bindings, p.Name)
	ps.mu.Unlock()
	if !had {
		return nil, nil, nil
	}
	err := call.session.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		if err := ps.installBoot(ctx, mo); err != nil {
			return err
		}
		return runtimeCall(ctx, mo, "cdpBind", map[string]any{"del": []string{p.Name}})
	})
	return nil, nil, err
}

// ---- console messages ----

// Markers of the page's internal console.debug messages: the hooks of
// 66-cdp-page.js, the dialog override (80-dialogs.js) and the W4 error hooks
// (internal/devtools/console.go). They never reach the client as console
// output.
const (
	navMarker      = "__iwdp_nav__ "
	bindMarker     = "__iwdp_bind__ "
	dialogMarker   = "__iwdp_dialog__ "
	internalPrefix = "__iwdp_"
	devtoolsPrefix = "[agent-ios:"
)

// wipConsoleMessage is a WIP Console.ConsoleMessage.
type wipConsoleMessage struct {
	Source     string           `json:"source"`
	Level      string           `json:"level"`
	Text       string           `json:"text"`
	Type       string           `json:"type"`
	URL        string           `json:"url"`
	Line       int              `json:"line"`
	Column     int              `json:"column"`
	Parameters []map[string]any `json:"parameters"`
	StackTrace json.RawMessage  `json:"stackTrace"`
	RequestID  string           `json:"networkRequestId"`
}

// consoleMessage routes a WIP console message: the internal markers to
// navigatedWithinDocument, bindingCalled or a Log warning (dialogs);
// console API calls to Runtime.consoleAPICalled; JavaScript errors to
// Runtime.exceptionThrown; the rest to Log.entryAdded.
func consoleMessage(ps *pageState, target string, params json.RawMessage) {
	var p struct {
		Message wipConsoleMessage `json:"message"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	if _, ok := ps.lastConsole[target]; !ok && len(ps.lastConsole) >= maxConsoleTargets {
		clear(ps.lastConsole)
	}
	ps.lastConsole[target] = params
	m := p.Message
	switch {
	case strings.HasPrefix(m.Text, navMarker):
		var v struct{ U, T string }
		if json.Unmarshal([]byte(m.Text[len(navMarker):]), &v) == nil {
			ps.sameDocumentLocked(target, v.U, v.T)
		}
	case strings.HasPrefix(m.Text, bindMarker):
		var v struct {
			N, K, P string
			F       int // the calling child frame's default context; 0 for the main frame
		}
		if json.Unmarshal([]byte(m.Text[len(bindMarker):]), &v) != nil {
			break
		}
		if b, ok := ps.bindings[v.N]; ok && v.K != "" && b.token == v.K {
			ps.emitLocked("", "Runtime.bindingCalled", map[string]any{"name": v.N, "payload": v.P, "executionContextId": ps.bindingContextLocked(b, v.F)})
		}
	case strings.HasPrefix(m.Text, dialogMarker):
		ps.dialogEntryLocked(m.Text[len(dialogMarker):])
	case strings.HasPrefix(m.Text, internalPrefix) || strings.HasPrefix(m.Text, devtoolsPrefix):
	case m.Source == "console-api":
		args := []map[string]any{}
		for _, a := range m.Parameters {
			args = append(args, toCDP(a))
		}
		if len(args) == 0 {
			args = append(args, map[string]any{"type": "string", "value": m.Text})
		}
		ev := map[string]any{"type": consoleType(m), "args": args, "executionContextId": ps.mainContextLocked(), "timestamp": nowMillis()}
		if st := cdpStack(m.StackTrace); st != nil {
			ev["stackTrace"] = st
		}
		ps.emitLocked(domRuntime, "Runtime.consoleAPICalled", ev)
	case m.Source == "javascript" && m.Level == "error":
		ps.exceptions++
		d := map[string]any{
			"exceptionId": ps.exceptions, "text": m.Text, "lineNumber": max(m.Line-1, 0), "columnNumber": max(m.Column-1, 0),
			"url": m.URL, "executionContextId": ps.mainContextLocked(),
			"exception": map[string]any{"type": "object", "subtype": "error", "className": "Error", "description": m.Text},
		}
		if st := cdpStack(m.StackTrace); st != nil {
			d["stackTrace"] = st
		}
		ps.emitLocked(domRuntime, "Runtime.exceptionThrown", map[string]any{"timestamp": nowMillis(), "exceptionDetails": d})
	default:
		ps.logEntryLocked(m)
	}
}

// maxConsoleTargets bounds the targets whose last console message an
// attachment keeps (pageState.lastConsole); beyond it all are forgotten.
const maxConsoleTargets = 8

// consoleRepeated handles Console.messageRepeatCountUpdated. WebKit
// reports a console message that equals the target's previous one (text,
// arguments, source location and call stack; InspectorConsoleAgent
// addMessageToConsole) with this event instead of messageAdded, once per
// repetition. A second equal dialog answer, binding call or same-document
// navigation is a new occurrence, so the target's last message is routed
// again. A target whose last message the attachment did not see (a reset
// or a clear since) gets nothing.
func consoleRepeated(ps *pageState, target string, _ json.RawMessage) {
	if last, ok := ps.lastConsole[target]; ok {
		consoleMessage(ps, target, last)
	}
}

// consoleType is the CDP consoleAPICalled type of a WIP message.
func consoleType(m wipConsoleMessage) string {
	switch m.Type {
	case "", "log", "image":
		switch m.Level {
		case "warning", "error", "info", "debug":
			return m.Level
		}
		return "log"
	case "timing":
		return "timeEnd"
	}
	return m.Type
}

// cdpStack converts a WIP stack trace (an object with callFrames, or the
// older bare array) to a CDP StackTrace with 0-based positions. WIP's
// parentStackTrace (async frames, Console.json in WebKit 7615.3.12.11.2
// and main) becomes CDP's parent; when its topCallFrameIsBoundary is true,
// its first frame is the native function that scheduled the work (such as
// setTimeout), which becomes the parent's description, as Chrome names
// async boundaries. At most maxParentStacks parents are kept.
func cdpStack(raw json.RawMessage) map[string]any {
	if len(raw) > 0 && raw[0] == '[' {
		var frames []wipCallFrame
		_ = json.Unmarshal(raw, &frames)
		if len(frames) == 0 {
			return nil
		}
		return map[string]any{"callFrames": cdpFrames(frames)}
	}
	var st wipStackTrace
	if json.Unmarshal(raw, &st) != nil || len(st.CallFrames) == 0 {
		return nil
	}
	out := map[string]any{"callFrames": cdpFrames(st.CallFrames)}
	cur := out
	for p, n := st.Parent, 0; p != nil && n < maxParentStacks; p, n = p.Parent, n+1 {
		frames, desc := p.CallFrames, ""
		if p.TopCallFrameIsBoundary && len(frames) > 0 {
			desc, frames = frames[0].FunctionName, frames[1:]
		}
		parent := map[string]any{"callFrames": cdpFrames(frames)}
		if desc != "" {
			parent["description"] = desc
		}
		cur["parent"] = parent
		cur = parent
	}
	return out
}

// maxParentStacks bounds the async parents of one CDP stack trace.
const maxParentStacks = 32

// wipCallFrame and wipStackTrace are WIP's Console.CallFrame and
// Console.StackTrace.
type wipCallFrame struct {
	FunctionName string `json:"functionName"`
	URL          string `json:"url"`
	ScriptID     string `json:"scriptId"`
	LineNumber   int    `json:"lineNumber"`
	ColumnNumber int    `json:"columnNumber"`
}

type wipStackTrace struct {
	CallFrames             []wipCallFrame `json:"callFrames"`
	TopCallFrameIsBoundary bool           `json:"topCallFrameIsBoundary"`
	Parent                 *wipStackTrace `json:"parentStackTrace"`
}

// cdpFrames converts WIP call frames (1-based positions) to CDP's.
func cdpFrames(frames []wipCallFrame) []map[string]any {
	out := make([]map[string]any, len(frames))
	for i, f := range frames {
		out[i] = map[string]any{
			"functionName": f.FunctionName, "url": f.URL, "scriptId": f.ScriptID,
			"lineNumber": max(f.LineNumber-1, 0), "columnNumber": max(f.ColumnNumber-1, 0),
		}
	}
	return out
}

// nowMillis is a CDP Runtime.Timestamp: milliseconds since the epoch.
func nowMillis() float64 { return float64(time.Now().UnixMicro()) / 1000 }
