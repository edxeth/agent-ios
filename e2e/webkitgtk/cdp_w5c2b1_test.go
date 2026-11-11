//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp/cdptest"
)

// largeText is a text of at least n bytes of mixed runes: ASCII, the
// characters that JSON escapes, 2- and 3-byte runes, the line separators
// and astral characters (surrogate pairs in JavaScript). Its 23-byte
// pattern puts the staging part boundaries at different places of it.
func largeText(n int) string {
	const pattern = "a\"\\\n<é€\u2028😀z&𝄞\x01"
	return strings.Repeat(pattern, n/len(pattern)+1)
}

// jsLength is the JavaScript length of s (UTF-16 code units).
func jsLength(s string) float64 { return float64(len(utf16.Encode([]rune(s)))) }

// W5c-2b part 1 on WebKitGTK (P2 item 2, design 5.3 Runtime): sources
// above WebKitGTK's message limit (127 KiB; Playwright's injected script
// is about 330 KB) reach the page through the staging of
// internal/cdp/large_source.go, and
//   - a Runtime.evaluate of a 330 KB expression returns its value, also in
//     a named context with awaitPromise, and a thrown error keeps its
//     exceptionDetails;
//   - a Runtime.callFunctionOn with a large declaration works on an object
//     (with its this and arguments) and on an execution context;
//   - nothing stays staged in the page afterwards;
//   - on a page whose CSP forbids eval, such a call still works, because
//     WebKit runs inspector calls with eval enabled
//     (JSC::DebuggerEvalEnabler), and the page's own eval stays refused;
//   - on a page that replaced eval with a function that throws, such a
//     call fails with an error that names the page's error, and a small
//     call still works.
func TestCDPLargeSourcesOnWebKitGTK(t *testing.T) {
	s := newSite(t, "large.localhost")
	s.html("/open.html", "<!doctype html><title>W5c-2b open</title><body><p>open</p>")
	s.handle("/csp.html", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", "script-src 'self'")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>W5c-2b csp</title><body><p>csp</p>"))
	})
	devtoolsBrowser(t, s.url+"/open.html")
	port := strings.TrimPrefix(freeAddr(t), "127.0.0.1:")
	if r := cli(t, "cdp", "start", "--port", port); r.code != 0 {
		t.Fatalf("cdp start --port %s: exit %d, stderr %q", port, r.code, r.stderr)
	}
	mustCLI(t, "--session", "s1", "snapshot")
	ws := strings.TrimSpace(mustCLI(t, "--session", "s1", "get", "cdp-url"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := cdptest.Dial(ctx, ws, nil)
	if err != nil {
		t.Fatalf("dialing the printed URL: %v", err)
	}
	defer c.Close()
	gtkCall(t, c, "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}},
	})
	tab, _ := gtkLastAttach(t, c)
	p := &cdpPage{c: c, tab: tab}
	p.attachPage(t)
	p.call(t, "Page.enable", nil)
	seq := c.Seq()
	p.call(t, "Runtime.enable", nil)
	defaultContext := func(after int) int {
		var ec struct {
			Context struct {
				ID      int `json:"id"`
				AuxData struct {
					IsDefault bool `json:"isDefault"`
				} `json:"auxData"`
			} `json:"context"`
		}
		raw := gtkEvent(t, c, after, p.page, "Runtime.executionContextCreated", func(raw json.RawMessage) bool {
			return json.Unmarshal(raw, &ec) == nil && ec.Context.AuxData.IsDefault
		})
		_ = json.Unmarshal(raw, &ec)
		return ec.Context.ID
	}
	ctxID := defaultContext(seq)

	big := largeText(330 << 10)
	lit, _ := json.Marshal(big) // a JavaScript string literal (JSON is a subset)
	type result struct {
		Result struct {
			Type     string `json:"type"`
			Value    any    `json:"value"`
			ObjectID string `json:"objectId"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	run := func(t *testing.T, method string, params map[string]any) result {
		t.Helper()
		var r result
		if err := json.Unmarshal(p.call(t, method, params), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	noStaging := func(t *testing.T) {
		t.Helper()
		if got := p.evalString(t, `typeof globalThis.__iwdpSrc + ":" + Object.getOwnPropertyNames(globalThis).filter(function (n) { return n.indexOf("__iwdpSrc") === 0; }).length`); got != "undefined:0" {
			t.Fatalf("the page keeps staged sources: %s", got)
		}
	}

	t.Run("evaluate of a 330 KB expression", func(t *testing.T) {
		expr := "(function () { var s = " + string(lit) + "; return s; })()"
		r := run(t, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
		if r.ExceptionDetails != nil || r.Result.Value != big {
			t.Fatalf("evaluate of %d bytes: exception %+v, value of %d bytes (want %d)", len(expr), r.ExceptionDetails, len(fmt.Sprint(r.Result.Value)), len(big))
		}
		// A global declaration of the expression is global (indirect eval).
		run(t, "Runtime.evaluate", map[string]any{"expression": "var w5c2bBig = " + string(lit) + ".length; w5c2bBig"})
		if got := p.evalString(t, "String(w5c2bBig)"); got != fmt.Sprint(jsLength(big)) {
			t.Fatalf("the staged var declaration gave %q, want %v", got, jsLength(big))
		}
		noStaging(t)
	})
	t.Run("evaluate in a context with awaitPromise", func(t *testing.T) {
		r := run(t, "Runtime.evaluate", map[string]any{
			"expression": "new Promise(function (ok) { setTimeout(function () { ok(" + string(lit) + ".length); }, 10); })",
			"contextId":  ctxID, "awaitPromise": true, "returnByValue": true,
		})
		if r.ExceptionDetails != nil || r.Result.Value != jsLength(big) {
			t.Fatalf("awaited staged evaluate = %+v", r)
		}
		noStaging(t)
	})
	t.Run("thrown error keeps exceptionDetails", func(t *testing.T) {
		r := run(t, "Runtime.evaluate", map[string]any{"expression": "(function () { var s = " + string(lit) + "; throw new Error('big ' + s.length); })()"})
		if want := fmt.Sprintf("big %v", jsLength(big)); r.ExceptionDetails == nil || !strings.Contains(r.ExceptionDetails.Text, want) {
			t.Fatalf("thrown staged evaluate: %+v, want exceptionDetails with %q", r.ExceptionDetails, want)
		}
		noStaging(t)
	})
	t.Run("callFunctionOn with a large declaration", func(t *testing.T) {
		body := run(t, "Runtime.evaluate", map[string]any{"expression": "document.body"})
		if body.Result.ObjectID == "" {
			t.Fatalf("document.body: %+v", body)
		}
		decl := "function (a, b) { var pad = " + string(lit) + "; return this.tagName + ':' + a + ':' + b + ':' + pad.length; }"
		r := run(t, "Runtime.callFunctionOn", map[string]any{
			"objectId": body.Result.ObjectID, "functionDeclaration": decl, "returnByValue": true,
			"arguments": []any{map[string]any{"value": "x"}, map[string]any{"unserializableValue": "NaN"}},
		})
		if want := fmt.Sprintf("BODY:x:NaN:%v", jsLength(big)); r.ExceptionDetails != nil || r.Result.Value != want {
			t.Fatalf("staged callFunctionOn on an object = %+v, want %q", r, want)
		}
		r = run(t, "Runtime.callFunctionOn", map[string]any{
			"executionContextId": ctxID, "returnByValue": true,
			"functionDeclaration": "async function (a) { var pad = " + string(lit) + "; return (this === globalThis) + ':' + a + ':' + pad.length; }",
			"arguments":           []any{map[string]any{"value": 7}}, "awaitPromise": true,
		})
		if want := fmt.Sprintf("true:7:%v", jsLength(big)); r.ExceptionDetails != nil || r.Result.Value != want {
			t.Fatalf("staged callFunctionOn on a context = %+v, want %q", r, want)
		}
		noStaging(t)
	})
	t.Run("CSP without unsafe-eval", func(t *testing.T) {
		seq := c.Seq()
		p.call(t, "Page.navigate", map[string]any{"url": s.url + "/csp.html"})
		ctxID = defaultContext(seq) // the CSP page's, also for the next subtest
		// The page's own eval (a timer task, outside an inspector call)
		// is refused before and after the staged calls.
		pageEval := func() string {
			p.evalString(t, `setTimeout(function () { try { (0, eval)("1"); window.w5c2bEval = "eval works"; } catch (e) { window.w5c2bEval = String(e); } }, 0), ""`)
			for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
				if v := p.evalString(t, "String(window.w5c2bEval || '')"); v != "" {
					p.evalString(t, "delete window.w5c2bEval, ''")
					return v
				}
			}
			t.Fatal("the page's eval check did not run")
			return ""
		}
		if got := pageEval(); !strings.Contains(got, "Content Security Policy") {
			t.Fatalf("the page's eval on the CSP page: %q; the test needs a page whose CSP forbids eval", got)
		}
		r := run(t, "Runtime.evaluate", map[string]any{"expression": "(function () { var s = " + string(lit) + "; return s.length; })()", "contextId": ctxID, "returnByValue": true})
		if r.ExceptionDetails != nil || r.Result.Value != jsLength(big) {
			t.Fatalf("staged evaluate on the CSP page = %+v", r)
		}
		body := run(t, "Runtime.evaluate", map[string]any{"expression": "document.body", "contextId": ctxID})
		r = run(t, "Runtime.callFunctionOn", map[string]any{"objectId": body.Result.ObjectID, "functionDeclaration": "function () { return this.tagName + " + string(lit) + ".length; }", "returnByValue": true})
		if want := fmt.Sprintf("BODY%v", jsLength(big)); r.ExceptionDetails != nil || r.Result.Value != want {
			t.Fatalf("staged callFunctionOn on the CSP page = %+v, want %q", r, want)
		}
		if got := pageEval(); !strings.Contains(got, "Content Security Policy") {
			t.Fatalf("after the staged calls the page's eval gives %q, want the CSP refusal", got)
		}
		noStaging(t)
	})
	// The reviewer's reproductions (review w5c2b1-20261108-234450, F1 and
	// F2), kept as regressions.
	refused := func(t *testing.T, method string, params map[string]any, cause string) {
		t.Helper()
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := c.Call(cctx, p.page, method, params)
		var ce *cdptest.Error
		if !errors.As(err, &ce) || !strings.Contains(ce.Message, "the page refuses eval: "+cause) {
			t.Fatalf("%s: %v; want the refusal with %q", method, err, cause)
		}
	}
	t.Run("replaced eval that does not throw is refused", func(t *testing.T) {
		p.evalString(t, "window.__reviewEval = window.eval; window.eval = function(s) { return s === '0' ? 0 : (s[0] === '(' ? function(){return 23;} : 23); }, ''")
		defer p.evalString(t, "Object.defineProperty(window, 'eval', {value: window.__reviewEval, writable: true, configurable: true}); delete window.__reviewEval; ''")
		const cause = "the page's eval is not the built-in eval"
		refused(t, "Runtime.evaluate", map[string]any{"expression": string(lit) + ".length", "returnByValue": true}, cause)
		body := run(t, "Runtime.evaluate", map[string]any{"expression": "document.body"})
		refused(t, "Runtime.callFunctionOn", map[string]any{"objectId": body.Result.ObjectID, "functionDeclaration": "function(){ return " + string(lit) + ".length; }", "returnByValue": true}, cause)
		refused(t, "Runtime.callFunctionOn", map[string]any{"executionContextId": ctxID, "functionDeclaration": "function(){ return " + string(lit) + ".length; }", "returnByValue": true}, cause)
		noStaging(t)
	})
	t.Run("late throwing eval getter", func(t *testing.T) {
		body := run(t, "Runtime.evaluate", map[string]any{"expression": "document.body"})
		for _, tc := range []struct {
			name, method string
			params       map[string]any
		}{
			{"evaluate", "Runtime.evaluate", map[string]any{"expression": string(lit) + ".length", "returnByValue": true}},
			{"object", "Runtime.callFunctionOn", map[string]any{"objectId": body.Result.ObjectID, "functionDeclaration": "function(){ return " + string(lit) + ".length; }", "returnByValue": true}},
			{"context", "Runtime.callFunctionOn", map[string]any{"executionContextId": ctxID, "functionDeclaration": "function(){ return " + string(lit) + ".length; }", "returnByValue": true}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				// The getter allows the first read (part 0's check) and
				// throws on the next, after it noted whether the entry was
				// still in the page at that read.
				p.evalString(t, "window.__reviewEval = window.eval; window.__reviewReads = 0; Object.defineProperty(window, 'eval', {configurable: true, get: function(){if (++window.__reviewReads === 1) return window.__reviewEval; window.__reviewStore = Object.keys(globalThis.__iwdpSrc || {}).length; throw new Error('late eval getter'); }}); ''")
				defer p.evalString(t, "Object.defineProperty(window, 'eval', {value: window.__reviewEval, writable: true, configurable: true}); delete window.__reviewEval; delete window.__reviewReads; delete window.__reviewStore; ''")
				refused(t, tc.method, tc.params, "Error: late eval getter")
				// The page removed the entry itself before it read eval.
				if got := p.evalString(t, "String(window.__reviewStore)"); got != "0" {
					t.Fatalf("staged entries when the final call read eval: %s, want 0", got)
				}
				noStaging(t)
			})
		}
	})
	// The reviewer's reproduction of F3 (review w5c2b1-r1-20261109-013046):
	// a staged source whose own error names the old static refusal marker
	// keeps its exceptionDetails, with native eval untouched. The context
	// body case is a control: its function runs after the staged step.
	t.Run("user error containing marker keeps exceptionDetails", func(t *testing.T) {
		const message = "application __iwdp_noeval__ actual failure"
		small := run(t, "Runtime.evaluate", map[string]any{"expression": "throw new Error('" + message + "')"})
		if small.ExceptionDetails == nil || !strings.Contains(small.ExceptionDetails.Text, message) {
			t.Fatalf("small-source baseline did not retain user error: %+v", small)
		}
		body := run(t, "Runtime.evaluate", map[string]any{"expression": "document.body"})
		for _, tc := range []struct {
			name, method string
			params       map[string]any
		}{
			{"evaluate", "Runtime.evaluate", map[string]any{"expression": "void " + string(lit) + "; throw new Error('" + message + "')"}},
			{"object", "Runtime.callFunctionOn", map[string]any{"objectId": body.Result.ObjectID, "functionDeclaration": "function(){ var pad = " + string(lit) + "; throw new Error('" + message + "'); }"}},
			{"context body control", "Runtime.callFunctionOn", map[string]any{"executionContextId": ctxID, "functionDeclaration": "function(){ var pad = " + string(lit) + "; throw new Error('" + message + "'); }"}},
			{"context declaration initializer", "Runtime.callFunctionOn", map[string]any{"executionContextId": ctxID, "functionDeclaration": "(function(){ var pad = " + string(lit) + "; throw new Error('" + message + "'); return function(){}; })()"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				raw, err := c.Call(cctx, p.page, tc.method, tc.params)
				if err != nil {
					t.Errorf("user exception misclassified as CDP method error: %v", err)
				} else {
					var r result
					if err := json.Unmarshal(raw, &r); err != nil {
						t.Fatal(err)
					}
					if r.ExceptionDetails == nil || !strings.Contains(r.ExceptionDetails.Text, message) {
						t.Errorf("lost original user exceptionDetails: %+v", r)
					}
				}
				noStaging(t)
			})
		}
	})

	// The reviewer's reproduction of F4 (review w5c2b1-r2-20261109-023612):
	// a staged source must not recover its call's refusal marker through
	// legacy caller introspection (the caller's source text or its
	// arguments) and throw it as a fake eval refusal. No page global or
	// built-in is changed, and the source holds no literal marker.
	t.Run("source cannot recover current refusal marker", func(t *testing.T) {
		body := run(t, "Runtime.evaluate", map[string]any{"expression": "document.body"})
		const probe = "var c = leak.caller, marker = null, m, i; " +
			"try { if (c) { m = String(c).match(/__iwdp_noeval_[0-9a-f]{16}: /); if (m) { marker = m[0]; } " +
			"for (i = 0; !marker && c.arguments && i < c.arguments.length; i++) { " +
			"if (typeof c.arguments[i] === 'string' && /^__iwdp_noeval_[0-9a-f]{16}: $/.test(c.arguments[i])) { marker = c.arguments[i]; } } } } " +
			"catch (x) { marker = null; } " +
			"if (typeof marker !== 'string') { throw new Error('marker inaccessible'); } throw marker + 'application failure';"
		for _, tc := range []struct {
			name, method string
			params       map[string]any
		}{
			{"evaluate caller", "Runtime.evaluate", map[string]any{"expression": "(function leak(){ var pad = " + string(lit) + "; " + probe + " })()", "returnByValue": true}},
			{"object caller", "Runtime.callFunctionOn", map[string]any{"objectId": body.Result.ObjectID, "functionDeclaration": "function leak(){ var pad = " + string(lit) + "; " + probe + " }", "returnByValue": true}},
			{"context initializer caller", "Runtime.callFunctionOn", map[string]any{"executionContextId": ctxID, "functionDeclaration": "(function leak(){ var pad = " + string(lit) + "; " + probe + " })()", "returnByValue": true}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				raw, err := c.Call(cctx, p.page, tc.method, tc.params)
				if err != nil {
					t.Fatalf("the source recovered its call's marker and its exception became the CDP error: %v", err)
				}
				var r result
				if err := json.Unmarshal(raw, &r); err != nil {
					t.Fatal(err)
				}
				if r.ExceptionDetails == nil || r.ExceptionDetails.Text != "Uncaught Error: marker inaccessible" {
					t.Fatalf("exceptionDetails %+v, want the source's own \"marker inaccessible\" error", r.ExceptionDetails)
				}
				noStaging(t)
			})
		}
	})

	t.Run("page without eval", func(t *testing.T) {
		const refusal = "EvalError: eval is off on this page"
		p.evalString(t, "window.eval = function () { throw new EvalError('eval is off on this page'); }, ''")
		body := run(t, "Runtime.evaluate", map[string]any{"expression": "document.body", "contextId": ctxID})
		for _, m := range []struct {
			method string
			params map[string]any
		}{
			{"Runtime.evaluate", map[string]any{"expression": "(function () { var s = " + string(lit) + "; return s.length; })()", "returnByValue": true}},
			{"Runtime.evaluate", map[string]any{"expression": string(lit) + ".length", "contextId": ctxID, "awaitPromise": true}},
			{"Runtime.callFunctionOn", map[string]any{"objectId": body.Result.ObjectID, "functionDeclaration": "function () { return " + string(lit) + ".length; }"}},
			{"Runtime.callFunctionOn", map[string]any{"executionContextId": ctxID, "functionDeclaration": "function () { return " + string(lit) + ".length; }"}},
		} {
			cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, err := c.Call(cctx, p.page, m.method, m.params)
			cancel()
			var ce *cdptest.Error
			if !errors.As(err, &ce) || !strings.Contains(ce.Message, "the page refuses eval: "+refusal) {
				t.Fatalf("%s on a page without eval: %v; want the error with %q", m.method, err, refusal)
			}
		}
		if got := p.evalString(t, "document.title"); got != "W5c-2b csp" {
			t.Fatalf("a small evaluate on the page = %q", got)
		}
		noStaging(t)
	})
}
