package cdp

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nnemirovsky/iwdp-mcp/internal/logsafe"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Client sources above the transport's message limit (proof P2 item 2;
// design 5.3, Runtime). Playwright's injected script is about 330 KB, and
// WebKitGTK closes the inspector socket for a message above about 127 KiB
// (transport.WebKitGTKMaxOutgoing). webkit.Session refuses such a message
// before it writes anything (WireLimitError). The bridge then stages the
// source in the page, in chunks below the limit, and evaluates it there
// with indirect eval:
//
//   - Runtime.evaluate: (0, eval)(<staged source>) with the call's other
//     params (context, awaitPromise, returnByValue, objectGroup, ...).
//   - Runtime.callFunctionOn with an execution context: the staged
//     declaration becomes the function object, which applyFunction calls.
//   - Runtime.callFunctionOn with an objectId: the chunks are stored by
//     calls on that object (so they land in its realm), and a small
//     declaration turns the staged source into the function and applies it
//     with the call's this and arguments.
//
// The staging runs in the call's realm: each staging message is sent by
// the call's boundOp, so it goes to the same execution context (an alias
// runs in its frame's main world), target and connection as the call.
//
// Page side: one non-enumerable global __iwdpSrc (the __iwdp namespace of
// the agent runtime's globals) maps a random id per call to its chunks.
// The final expression deletes the entry (and the global when it is
// empty) first, before anything that can throw, and then runs the joined
// chunks. A failed or thrown final call also sends the removal, for a
// call that did not run. A new document starts without the global. An
// entry that a lost connection left behind is dropped when the next call
// of that realm starts to stage (at most stageKeep entries stay).
//
// Eval and the page's Content Security Policy: WebKit runs every
// injected-script call (Runtime.evaluate, callFunctionOn, awaitPromise)
// with eval enabled (InjectedScriptBase::callFunctionWithEvalEnabled,
// JSC::DebuggerEvalEnabler; in WebKit main and in safari-7615-branch, the
// WebKit of iOS 16), so the staged eval works on a page whose CSP forbids
// eval, and the page's own eval stays refused (e2e/webkitgtk
// TestCDPLargeSourcesOnWebKitGTK). Part 0 still checks that indirect eval
// works before anything is stored: when it throws (a page that replaced
// eval, or a WebKit without the exemption), the call fails with
// errStagedEval, which names the page's error.
//
// The page can replace globalThis.eval, so the final expression reads it
// once, checks that one reference with two random probes (a nonce string
// literal and a sum), and evaluates the source with that same reference.
// A replacement that throws, or that does not give both probe values, is
// refused with errStagedEval instead of giving a wrong value. This is a
// check, not a guarantee: a page that deliberately emulates eval (one
// that passes the probes and then changes the source or its value) can
// still interfere with a staged call.
//
// Page.addScriptToEvaluateOnNewDocument cannot use this: its source must
// be in the page before any page script of a new document runs, and a new
// document starts without the staged chunks. A new-document script that
// makes the page's bootstrap script too large fails (page_domain.go).

const (
	// refusalPrefix starts the refusal marker of each staged call. The
	// marker adds 16 random hex digits and ": ", so it is new for each
	// call; the final expression throws the string marker + cause when the
	// page's eval fails its check, and the bridge turns only that exact
	// form into errStagedEval with the cause. The marker is stored with
	// the staged parts and is in no final call's source text, so a staged
	// source cannot read it (its entry is removed before it runs), and an
	// error of the source cannot have this form.
	refusalPrefix = "__iwdp_noeval_"
	// notBuiltinEval is the refusal for an eval that gives a wrong probe
	// value.
	notBuiltinEval = "the page's eval is not the built-in eval"
	// stageReserve is the part of the transport limit that a staging
	// message needs besides its chunk: the staging function, the id, the
	// refusal marker, the call's ids and method, an objectId or context id, and the
	// Target.sendMessageToTarget envelope with its target id.
	stageReserve = 4096
	// stageEscape bounds the wire bytes of one source byte in a staging
	// message. The chunk is a JSON string literal in the JavaScript
	// (\u00XX, 6 bytes, for a control character and for <, > and &), that
	// JavaScript is a JSON string in the WIP message (7 bytes), and with
	// Target routing that message is a JSON string in the envelope (9).
	stageEscape = 9
	// maxStagedSource bounds a source that the bridge stages, in bytes. It
	// is the direct transport's message limit; with chunks of about 14 KB
	// (WebKitGTK) it is about 1200 staging messages.
	maxStagedSource = 16 << 20
	// stageKeep bounds the entries of the page's staging store: a new call
	// drops the oldest entries beyond it. Calls of one page run one at a
	// time (core.ClassMutate), so only entries that a failed cleanup left
	// behind are dropped.
	stageKeep = 4
)

// Page-side staging functions. Each is a function expression that the
// bridge calls with JSON arguments; the fake page of the tests matches
// these texts.
var (
	// stageChunkJS stores chunk c as part i of the entry k and returns
	// "ok". Part 0 first checks that indirect eval works (else it returns
	// "noeval:" and the error) and starts the entry with the call's
	// refusal marker r (as its property r). A missing entry or a part out
	// of order gives "lost".
	stageChunkJS = `(function (k, i, c, r) { var g = globalThis, d = g.__iwdpSrc, e; ` +
		`if (i === 0) { try { (0, eval)("0"); } catch (x) { return "noeval:" + x; } ` +
		`if (!d) { d = { __proto__: null }; Object.defineProperty(g, "__iwdpSrc", { value: d, configurable: true }); } ` +
		`var ks = Object.keys(d); for (var j = 0; j <= ks.length - ` + strconv.Itoa(stageKeep) + `; j++) { delete d[ks[j]]; } d[k] = []; d[k].r = r; } ` +
		`e = d && d[k]; if (!e || e.length !== i) { return "lost"; } e[i] = c; return "ok"; })`
	// stageRunJS runs the n parts of the entry k: with f false as a
	// program, with f true as the function that the declaration defines,
	// made as WIP Runtime.callFunctionOn makes it from "(" + declaration +
	// ")" (the line break keeps a closing line comment of the declaration
	// from hiding the parenthesis). It first removes the entry (and the
	// store when it is empty) with steps that cannot throw: a for-in
	// emptiness check, not the page's Object.keys. Then it reads
	// globalThis.eval once, checks that reference with the bridge's two
	// random probes (p must give v and q must give w) and evaluates the
	// source with it (an indirect eval: global scope). An evaluator that
	// fails a probe or throws is refused: the expression throws the string
	// r + cause, where r is the call's refusal marker, read from the entry
	// into a local (a string, so no constructor of the page is needed). An
	// error of the source itself propagates as it is. It throws when the
	// entry is incomplete.
	stageRunJS = `(function (k, n, p, v, q, w, f) { var g = globalThis, d = g.__iwdpSrc, e = d && d[k], s = "", i, x, ev, r; ` +
		`if (d) { delete d[k]; x = true; for (i in d) { x = false; break; } if (x) { delete g.__iwdpSrc; } } ` +
		`if (!e || e.length !== n) { throw new Error("iwdp: the staged source " + k + " is incomplete"); } ` +
		`r = e.r; for (i = 0; i < n; i++) { s += e[i]; } ` +
		`try { ev = g.eval; x = typeof ev === "function" && ev(p) === v && ev(q) === w; } catch (y) { throw r + y; } ` +
		`if (!x) { throw r + "` + notBuiltinEval + `"; } ` +
		`return f ? ev("(" + s + "\n)") : ev(s); })`
	// stageDropJS removes the entry k (and the store when it is empty).
	stageDropJS = `(function (k) { var g = globalThis, d = g.__iwdpSrc, x = true, i; ` +
		`if (d) { delete d[k]; for (i in d) { x = false; break; } if (x) { delete g.__iwdpSrc; } } return "ok"; })`
)

// errStagedEval is the error of a staged call on a page that refuses
// indirect eval; cause is the page's error (for a Content-Security-Policy
// without 'unsafe-eval', WebKit's EvalError names the directive).
func errStagedEval(method, what string, size, limit int, cause string) error {
	return serverError("%s: the %s is %d bytes, so its message is above this page connection's limit of %d bytes; "+
		"the bridge runs such a source by staging it in the page and evaluating it with indirect eval, and the page refuses eval: %s. "+
		"Use a source below the limit",
		method, what, size, limit, cause)
}

// wireLimit returns the WireLimitError in err, if any.
func wireLimit(err error) *webkit.WireLimitError {
	var wl *webkit.WireLimitError
	if errors.As(err, &wl) {
		return wl
	}
	return nil
}

// splitSource splits src into parts of at most size bytes that end on
// rune boundaries, so no part splits a UTF-8 sequence (a part as a JSON
// string would turn a split rune into U+FFFD). size is at least
// utf8.UTFMax.
func splitSource(src string, size int) []string {
	var parts []string
	for len(src) > size {
		end := size
		for end > 0 && !utf8.RuneStart(src[end]) {
			end--
		}
		parts = append(parts, src[:end])
		src = src[end:]
	}
	return append(parts, src)
}

// probeMax bounds the two random terms of the sum probe; twice their sum
// stays an exact JavaScript number.
const probeMax = 1 << 30

// staged is a source stored in the page: its entry id and part count, the
// eval probes of its final expression (p is the JavaScript string literal
// of the nonce v, and q is a sum whose value is w) and its refusal marker.
type staged struct {
	id      string
	n       int
	p, v    string
	q       string
	w       int
	refusal string
}

// newStaged returns an entry with a new random id, new random probes and
// a new refusal marker.
func newStaged() staged {
	st := staged{id: "k" + randomHex(8), v: "n" + randomHex(8), refusal: refusalPrefix + randomHex(8) + ": "}
	p, _ := json.Marshal(st.v) // a string
	st.p = string(p)
	x, y := rand.IntN(probeMax), rand.IntN(probeMax)
	st.q, st.w = fmt.Sprintf("(%d+%d)*2", x, y), 2*(x+y)
	return st
}

// run is the expression stageRunJS(...) for the staged entry: its value is
// the evaluated source (f false) or the function that the staged
// declaration defines (f true); it removes the entry.
func (st staged) run(f bool) string {
	a, _ := json.Marshal([]any{st.id, st.n, st.p, st.v, st.q, st.w, f}) // strings, numbers and a bool
	return stageRunJS + "(" + string(a[1:len(a)-1]) + ")"
}

// evalExpr is the expression that evaluates the staged source, as
// Runtime.evaluate would evaluate it (indirect eval: global scope).
func (st staged) evalExpr() string { return st.run(false) }

// fnExpr is the expression whose value is the function that the staged
// declaration defines, as WIP Runtime.callFunctionOn makes it.
func (st staged) fnExpr() string { return st.run(true) }

// callDecl is the declaration that callFunctionOn sends instead of the
// staged one: it makes the function and applies it with the call's this
// and arguments. It is strict code (so it has a simple parameter list and
// uses arguments): the staged function runs with this trampoline as its
// caller, and a non-strict function's .caller is null for a strict
// caller, so the source cannot read the trampoline or its arguments.
func (st staged) callDecl() string {
	return `function () { "use strict"; return ` + st.fnExpr() + ".apply(this, arguments); }"
}

// stageSink sends one staging expression in the call's realm: by
// Runtime.evaluate in the operation's context, or, with objectID, by
// Runtime.callFunctionOn on that object.
type stageSink struct {
	b        *boundOp
	objectID string
}

// run evaluates expr in the sink's realm and returns its value.
func (k stageSink) run(expr string) (any, error) {
	var (
		r   wipResult
		err error
	)
	if k.objectID == "" {
		r, err = k.b.call("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "doNotPauseOnExceptionsAndMuteConsole": true})
	} else {
		r, err = k.b.call("Runtime.callFunctionOn", map[string]any{
			"objectId": k.objectID, "functionDeclaration": "function () { return " + expr + "; }",
			"returnByValue": true, "doNotPauseOnExceptionsAndMuteConsole": true,
		})
	}
	if err != nil {
		return nil, err
	}
	if r.WasThrown {
		return nil, fmt.Errorf("the staging expression threw %v", r.Result["description"])
	}
	return r.Result["value"], nil
}

// stage stores src in the sink's realm in parts below the limit that wl
// reported. method and what name the call and the source for errors.
func (k stageSink) stage(method, what, src string, wl *webkit.WireLimitError) (staged, error) {
	if len(src) > maxStagedSource {
		return staged{}, &Error{Code: codeInvalidParams, Message: fmt.Sprintf("%s: the %s is %d bytes; the bridge sends at most %d bytes of source to the page", method, what, len(src), maxStagedSource)}
	}
	size := (wl.Limit - stageReserve) / stageEscape
	if size < utf8.UTFMax {
		return staged{}, fmt.Errorf("%s: the %s is %d bytes, above this page connection's message limit of %d bytes, which is too small to stage it", method, what, len(src), wl.Limit)
	}
	st := newStaged()
	id, _ := json.Marshal(st.id)        // a string
	mark, _ := json.Marshal(st.refusal) // a string
	for i, part := range splitSource(src, size) {
		lit, _ := json.Marshal(part) // a string
		v, err := k.run(fmt.Sprintf("%s(%s, %d, %s, %s)", stageChunkJS, id, i, lit, mark))
		if err == nil && v != "ok" {
			if s, ok := v.(string); ok && strings.HasPrefix(s, "noeval:") {
				return staged{}, errStagedEval(method, what, len(src), wl.Limit, strings.TrimPrefix(s, "noeval:"))
			}
			err = fmt.Errorf("the page answered %v", v)
		}
		if err != nil {
			k.drop(st)
			return staged{}, fmt.Errorf("%s: staging part %d of the %s (%d bytes; this page connection's message limit is %d bytes): %w", method, i, what, len(src), wl.Limit, err)
		}
		st.n++
	}
	return st, nil
}

// evaluate stages src and sends Runtime.evaluate with params, whose
// expression becomes expr of the staged source (evalExpr or fnExpr).
func (k stageSink) evaluate(method, what, src string, wl *webkit.WireLimitError, params map[string]any, expr func(staged) string) (wipResult, error) {
	st, err := k.stage(method, what, src, wl)
	if err != nil {
		return wipResult{}, err
	}
	params["expression"] = expr(st)
	r, err := k.b.call("Runtime.evaluate", params)
	return k.settle(st, method, what, len(src), wl, r, err)
}

// stageCall stages the declaration decl of a callFunctionOn on an object
// (in that object's realm) and sends params with the declaration that
// applies the staged one.
func stageCall(b *boundOp, method, decl string, wl *webkit.WireLimitError, params map[string]any) (wipResult, error) {
	k := stageSink{b: b, objectID: params["objectId"].(string)}
	st, err := k.stage(method, "function declaration", decl, wl)
	if err != nil {
		return wipResult{}, err
	}
	params["functionDeclaration"] = st.callDecl()
	r, err := b.call("Runtime.callFunctionOn", params)
	return k.settle(st, method, "function declaration", len(decl), wl, r, err)
}

// settle ends the final call of st, which gave r and err. After a failed
// or thrown call it sends the removal of the entry: the final expression
// removes the entry itself before anything that can throw, but a call
// that failed may not have run it. A thrown refusal of the page's eval
// (a string that starts with the call's refusal marker) becomes
// errStagedEval; size and wl describe the source for that error. Every
// other thrown value stays the call's result.
func (k stageSink) settle(st staged, method, what string, size int, wl *webkit.WireLimitError, r wipResult, err error) (wipResult, error) {
	if err != nil || r.WasThrown {
		k.drop(st)
	}
	if err == nil && r.WasThrown {
		thrown, _ := r.Result["value"].(string)
		if cause, ok := strings.CutPrefix(thrown, st.refusal); ok && r.Result["type"] == "string" {
			return wipResult{}, errStagedEval(method, what, size, wl.Limit, cause)
		}
	}
	return r, err
}

// drop removes the entry of st from the page after a call that may not
// have taken it. A failure is only logged: the entry goes with its
// document, or the next staging of the realm drops it (stageKeep).
func (k stageSink) drop(st staged) {
	id, _ := json.Marshal(st.id) // a string
	if _, err := k.run(fmt.Sprintf("%s(%s)", stageDropJS, id)); err != nil {
		s := k.b.s
		s.c.s.o.Logf("cdp: %s: removing the staged source %s of %s session %s: %v", logsafe.ID(s.c.name), logsafe.ID(st.id), logsafe.ID(s.kind), logsafe.ID(shortID(s.id)), logsafe.Error(err))
	}
}
