package cdp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
)

// The first page-session methods. W5a needs them for its acceptance
// (design 7.8 item 11: a client evaluates in the page; detaching removes
// the extra header the attachment set). The Runtime and Network domains
// belong to W5b, which extends these handlers (execution contexts and
// utility-world aliases, callFunctionOn, oversized sources) and adds the
// rest of each domain in its own files.

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

// runtimeEvaluate runs the expression in the page's main world. WIP
// evaluate has no awaitPromise (CLAUDE.md), so with awaitPromise it
// evaluates by reference: a Promise result is awaited with
// Runtime.awaitPromise in a Later, and the session's next messages do not
// wait for it; any other object is read by value when returnByValue asks
// for it.
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
	wip := map[string]any{
		"expression": p.Expression, "returnByValue": p.ReturnByValue && !p.AwaitPromise,
		"emulateUserGesture": p.UserGesture, "doNotPauseOnExceptionsAndMuteConsole": p.Silent,
		"includeCommandLineAPI": p.IncludeCommandLineAPI,
	}
	if p.ObjectGroup != "" {
		wip["objectGroup"] = p.ObjectGroup
	}
	r, err := wipCall(mo.Op.Context(), mo, "Runtime.evaluate", wip)
	if err != nil || r.WasThrown || !p.AwaitPromise || !isPromise(r.Result) {
		defer end()
		if err == nil && !r.WasThrown && p.AwaitPromise && p.ReturnByValue && r.Result["objectId"] != nil {
			r, err = s.valueOf(mo, r.Result["objectId"])
		}
		if err != nil {
			return nil, nil, err
		}
		return cdpResult(r), nil, nil
	}
	return nil, func(context.Context) (any, error) {
		defer end()
		ar, err := wipCall(mo.Op.Context(), mo, "Runtime.awaitPromise", map[string]any{
			"promiseObjectId": r.Result["objectId"], "returnByValue": p.ReturnByValue,
		})
		if err != nil {
			return nil, err
		}
		return cdpResult(ar), nil
	}, nil
}

// valueOf reads the object objectID by value and releases the reference,
// which the client never saw.
func (s *session) valueOf(mo *core.ManagedOp, objectID any) (wipResult, error) {
	ctx := mo.Op.Context()
	r, err := wipCall(ctx, mo, "Runtime.callFunctionOn", map[string]any{
		"objectId": objectID, "functionDeclaration": thisFunction, "returnByValue": true,
	})
	if _, rerr := mo.Op.Caller().Send(ctx, "Runtime.releaseObject", map[string]any{"objectId": objectID}); rerr != nil {
		s.c.s.o.Logf("cdp: %s: releasing the evaluated object of %s session %s: %v", s.c.name, s.kind, shortID(s.id), rerr)
	}
	return r, err
}

func wipCall(ctx context.Context, mo *core.ManagedOp, method string, params any) (wipResult, error) {
	raw, err := mo.Op.Caller().Send(ctx, method, params)
	if err != nil {
		return wipResult{}, err
	}
	var r wipResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return wipResult{}, fmt.Errorf("decoding the WIP %s reply: %w", method, err)
	}
	return r, nil
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
