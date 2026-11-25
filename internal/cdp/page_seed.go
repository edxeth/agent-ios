package cdp

import (
	"context"
	"encoding/json"

	"github.com/edxeth/agent-ios/internal/core"
)

// The first page-session methods. W5a needs them for its acceptance
// (design 7.8 item 11: a client evaluates in the page; detaching removes
// the extra header the attachment set). W5b-1 added execution contexts
// (contextId and uniqueContextId, worlds.go) to Runtime.evaluate; the rest
// of the Runtime domain is in runtime_domain.go. W5c-2b stages expressions
// above the transport's message limit (P2 item 2, large_source.go); the
// Network domain is W5c's.

func init() {
	register(
		Method{Name: "Runtime.evaluate", Levels: OnPage, Handle: runtimeEvaluate},
		Method{Name: "Network.setExtraHTTPHeaders", Levels: OnPage, Handle: ownedCall},
	)
}

// ownedCall sends the call with the attachment as its desired-state owner
// (design 2.4, 5.2): the daemon maps it like a legacy call (for example a
// header map is merged over the owners, with the Network reinitialization
// barrier when a name loses its last owner), and detaching the attachment
// releases exactly this state.
func ownedCall(ctx context.Context, call *Call) (any, Later, error) {
	s := call.session
	var res json.RawMessage
	err := s.do(ctx, call.Method, core.ClassExclusive, func(ctx context.Context, mo *core.ManagedOp) error {
		var err error
		res, err = s.c.s.o.Manager.RawAs(ctx, mo, s.owner, call.Method, call.Params)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return res, nil, nil
}

// evalParams are the CDP Runtime.evaluate params that WIP can serve.
type evalParams struct {
	Expression            string `json:"expression"`
	ObjectGroup           string `json:"objectGroup"`
	IncludeCommandLineAPI bool   `json:"includeCommandLineAPI"`
	Silent                bool   `json:"silent"`
	ContextID             int    `json:"contextId"`
	UniqueContextID       string `json:"uniqueContextId"`
	ReturnByValue         bool   `json:"returnByValue"`
	UserGesture           bool   `json:"userGesture"`
	AwaitPromise          bool   `json:"awaitPromise"`
}

// wipResult is a WIP evaluate, callFunctionOn or awaitPromise result.
type wipResult struct {
	Result    map[string]any `json:"result"`
	WasThrown bool           `json:"wasThrown"`
}

// thisFunction returns its receiver; Runtime.callFunctionOn with it reads
// an object by value (as tools.EvaluateScriptWithOptions does).
const thisFunction = "function() { return this; }"

// runtimeEvaluate runs the expression in the main world of the context's
// frame (an alias runs in its frame's main world, worlds.go). WIP
// evaluate has no awaitPromise (docs/webkit-protocol.md), so with awaitPromise it
// evaluates by reference: a Promise result is awaited with
// Runtime.awaitPromise in a Later, and the session's next messages do not
// wait for it; any other object is read by value when returnByValue asks
// for it (boundOp.finish). Every send of the evaluation keeps the
// binding of its first one (boundOp).
func runtimeEvaluate(ctx context.Context, call *Call) (any, Later, error) {
	var p evalParams
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	s := call.session
	mo, end, err := s.begin(ctx, "Runtime.evaluate", core.ClassMutate)
	if err != nil {
		return nil, nil, err
	}
	b, err := s.bind(mo, p.ContextID, p.UniqueContextID)
	if err != nil {
		end()
		return nil, nil, err
	}
	wip := map[string]any{
		"expression": p.Expression, "returnByValue": p.ReturnByValue && !p.AwaitPromise,
		"emulateUserGesture": p.UserGesture, "doNotPauseOnExceptionsAndMuteConsole": p.Silent,
		"includeCommandLineAPI": p.IncludeCommandLineAPI,
	}
	if p.ObjectGroup != "" {
		wip["objectGroup"] = p.ObjectGroup
	}
	r, err := b.call("Runtime.evaluate", wip)
	if wl := wireLimit(err); wl != nil {
		// Above the transport's message limit: staged (large_source.go).
		r, err = stageSink{b: b}.evaluate(call.Method, "expression", p.Expression, wl, wip, staged.evalExpr)
	}
	return b.finish(end, r, boundErr(err), p.ReturnByValue, p.AwaitPromise)
}

// valueOf reads the object objectID by value and releases the reference,
// which the client never saw.
func (b *boundOp) valueOf(objectID any) (wipResult, error) {
	r, err := b.call("Runtime.callFunctionOn", map[string]any{
		"objectId": objectID, "functionDeclaration": thisFunction, "returnByValue": true,
	})
	b.release(objectID, "the evaluated object")
	return r, err
}

func isPromise(o map[string]any) bool {
	return o["type"] == "object" && o["className"] == "Promise" && o["objectId"] != nil
}

// toCDP converts a WIP RemoteObject: WIP has no unserializableValue, its
// preview has another shape (dropped), and "class" is no CDP subtype.
func toCDP(o map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"type", "subtype", "className", "value", "description", "objectId"} {
		if v, ok := o[k]; ok {
			out[k] = v
		}
	}
	if out["subtype"] == "class" {
		delete(out, "subtype")
	}
	if _, has := o["value"]; !has && (o["type"] == "number" || o["type"] == "bigint") {
		out["unserializableValue"] = o["description"]
	}
	return out
}

// cdpResult builds the CDP evaluate result; a thrown value becomes
// exceptionDetails (WIP reports wasThrown only).
func cdpResult(r wipResult) map[string]any {
	res := toCDP(r.Result)
	out := map[string]any{"result": res}
	if r.WasThrown {
		text := "Uncaught"
		if d, ok := res["description"].(string); ok && d != "" {
			text = "Uncaught " + d
		}
		out["exceptionDetails"] = map[string]any{"exceptionId": 1, "text": text, "lineNumber": 0, "columnNumber": 0, "exception": res}
	}
	return out
}
