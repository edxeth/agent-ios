package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	"github.com/nnemirovsky/agent-ios/internal/cdp/cdptest"
	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Client sources above the transport's message limit (P2 item 2,
// large_source.go): the rig's transport gets a small outgoing limit, and
// the fake page models the page side of the staging (stageModel).

// testLimit is the outgoing limit of the limited rig once the page is
// attached: staging parts are (testLimit - stageReserve) / stageEscape =
// 455 bytes. The attach itself runs with WebKitGTK's limit, because the
// page's setup sends the agent runtime's bootstrap part (about 60 KB).
const testLimit = 8 << 10

// limitedTransport gives every page connection the outgoing limit and
// records the largest message written while the limit is testLimit or
// lower.
type limitedTransport struct {
	transport.Transport
	limit   *atomic.Int64
	largest *atomic.Int64
}

func (l limitedTransport) Open(ctx context.Context, p transport.Page) (transport.MessageConn, error) {
	c, err := l.Transport.Open(ctx, p)
	if err != nil {
		return nil, err
	}
	return &limitedConn{MessageConn: c, limit: l.limit, largest: l.largest}, nil
}

type limitedConn struct {
	transport.MessageConn
	limit   *atomic.Int64
	largest *atomic.Int64
}

func (c *limitedConn) Limits() transport.Limits {
	l := c.MessageConn.Limits()
	l.MaxOutgoing = int(c.limit.Load())
	return l
}

func (c *limitedConn) Write(ctx context.Context, msg []byte) error {
	for c.limit.Load() <= testLimit {
		cur := c.largest.Load()
		if int64(len(msg)) <= cur || c.largest.CompareAndSwap(cur, int64(len(msg))) {
			break
		}
	}
	return c.MessageConn.Write(ctx, msg)
}

// stageModel is the page side of the staging on the fake page: one store
// per realm (an evaluate's context id, or the object of a callFunctionOn),
// the eval checks (part 0's, and the final expression's probes), and the
// evaluation of a staged source (run) or a call of a staged declaration
// (calls are recorded).
type stageModel struct {
	mu      sync.Mutex
	stores  map[string]map[string][]string // realm -> id -> parts
	ids     []string                       // every started entry
	parts   int                            // staged parts
	noEval  string                         // the eval check's error; "" when eval works
	lost    bool                           // part 1 of each entry answers "lost"
	throws  bool                           // part 1 of each entry throws
	noDrop  bool                           // the removal of an entry throws
	errPart bool                           // a part sent by callFunctionOn gets a WIP error
	failFn  bool                           // the call of a staged declaration fails
	fns     map[string]string              // function objects made from staged declarations
	fnCount int
	run     func(src string) map[string]any // the WIP result of an evaluated source

	// evalFault is the refusal of the final expression's eval check ("" when
	// eval passes the probes): the page removes the entry, then throws
	// the call's refusal marker + evalFault.
	evalFault string
	// keepOnThrow makes the final call throw before it removes the entry,
	// as a call that did not run the final expression.
	keepOnThrow bool
	finals      []staged // the entries of the final calls
	// srcThrow is the thrown result of the staged source itself (after
	// the eval check passed), for every call form; nil when it does not
	// throw.
	srcThrow map[string]any
	marks    map[string]string // entry id -> the refusal marker that part 0 stored
}

// runArgs matches the arguments of a stageRunJS call as the bridge writes
// them (a JSON array without its brackets): the id, the part count, the
// nonce probe and its value, the sum probe and its value, and f.
var runArgs = regexp.MustCompile(regexp.QuoteMeta(stageRunJS) +
	`\(("k[0-9a-f]{16}"),(\d+),("\\"n[0-9a-f]{16}\\""),("n[0-9a-f]{16}"),("\(\d+\+\d+\)\*2"),(\d+),(?:true|false)\)`)

// stagedIn parses the staged entry that expr runs, if any.
func stagedIn(expr string) (staged, bool) {
	m := runArgs.FindStringSubmatch(expr)
	if m == nil {
		return staged{}, false
	}
	var st staged
	_ = json.Unmarshal([]byte(m[1]), &st.id)
	st.n, _ = strconv.Atoi(m[2])
	_ = json.Unmarshal([]byte(m[3]), &st.p)
	_ = json.Unmarshal([]byte(m[4]), &st.v)
	_ = json.Unmarshal([]byte(m[5]), &st.q)
	st.w, _ = strconv.Atoi(m[6])
	return st, true
}

// probesHold reports whether the built-in eval gives the probe values of
// st: p is the string literal of v, and q is a sum whose value is w.
func probesHold(st staged) bool {
	var v string
	var x, y int
	if json.Unmarshal([]byte(st.p), &v) != nil || v != st.v {
		return false
	}
	if n, err := fmt.Sscanf(st.q, "(%d+%d)*2", &x, &y); err != nil || n != 2 {
		return false
	}
	return 2*(x+y) == st.w
}

// final runs the start of the final expression of st in realm: the
// removal of the entry and the eval check. It returns the joined source,
// or the thrown result that ends the call. The refusal marker comes from
// the entry (stored by part 0), as in the page.
func (m *stageModel) final(realm string, st staged) (string, map[string]any) {
	m.mu.Lock()
	st.refusal = m.marks[st.id]
	m.finals = append(m.finals, st)
	keep, fault, srcThrow := m.keepOnThrow, m.evalFault, m.srcThrow
	m.mu.Unlock()
	if keep {
		return "", thrown("Error: the final call threw before it removed the entry")
	}
	src, ok := m.take(realm, st)
	switch {
	case !ok:
		return "", thrown("Error: agent-ios: the staged source is incomplete")
	case fault == "" && !probesHold(st):
		fault = notBuiltinEval
	}
	if fault != "" {
		return "", thrownString(st.refusal + fault)
	}
	if srcThrow != nil {
		return "", srcThrow
	}
	return src, nil
}

// args decodes the JSON arguments of a call of fn in expr.
func args(expr, fn string) ([]json.RawMessage, bool) {
	if !strings.HasPrefix(expr, fn+"(") || !strings.HasSuffix(expr, ")") {
		return nil, false
	}
	var a []json.RawMessage
	if json.Unmarshal([]byte("["+expr[len(fn)+1:len(expr)-1]+"]"), &a) != nil {
		return nil, false
	}
	return a, true
}

// staging runs a staging expression (part or drop) in realm.
func (m *stageModel) staging(realm, expr string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := args(expr, stageChunkJS); ok && len(a) == 4 {
		var k, c, r string
		var i int
		_ = json.Unmarshal(a[0], &k)
		_ = json.Unmarshal(a[1], &i)
		_ = json.Unmarshal(a[2], &c)
		_ = json.Unmarshal(a[3], &r)
		d := m.stores[realm]
		if i == 0 {
			if m.noEval != "" {
				return "noeval:" + m.noEval, true
			}
			if d == nil {
				d = map[string][]string{}
				m.stores[realm] = d
			}
			d[k] = nil
			m.ids = append(m.ids, k)
			m.marks[k] = r
		}
		e, ok := d[k]
		if !ok || len(e) != i || (m.lost && i == 1) {
			return "lost", true
		}
		if m.throws && i == 1 {
			return "", false // the caller answers a thrown error
		}
		d[k] = append(e, c)
		m.parts++
		return "ok", true
	}
	if a, ok := args(expr, stageDropJS); ok && len(a) == 1 && !m.noDrop {
		var k string
		_ = json.Unmarshal(a[0], &k)
		m.removeLocked(realm, k)
		return "ok", true
	}
	return "", false
}

func (m *stageModel) removeLocked(realm, k string) {
	delete(m.stores[realm], k)
	if len(m.stores[realm]) == 0 {
		delete(m.stores, realm)
	}
}

// take removes the entry st of realm and joins its parts.
func (m *stageModel) take(realm string, st staged) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.stores[realm][st.id]
	m.removeLocked(realm, st.id)
	return strings.Join(e, ""), ok && len(e) == st.n
}

// left lists the entries still stored.
func (m *stageModel) left() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for realm, d := range m.stores {
		for k := range d {
			out = append(out, realm+"/"+k)
		}
	}
	return out
}

// thrownString is the WIP result of a thrown string.
func thrownString(s string) map[string]any {
	return map[string]any{"result": map[string]any{"type": "string", "value": s, "description": s}, "wasThrown": true}
}

func thrown(desc string) map[string]any {
	return map[string]any{"result": map[string]any{"type": "object", "subtype": "error", "className": "Error", "description": desc}, "wasThrown": true}
}

// install makes the fake page of fp serve the model: evaluates through
// fp.eval (realm "ctx:<contextId>", 0 for none) and every
// Runtime.callFunctionOn (realm "obj:<objectId>"). A call of a staged
// declaration answers {src, this, args}; a plain call answers "plain".
func (m *stageModel) install(fp *fakePage) {
	fp.mu.Lock()
	fp.eval = func(expr string) (any, bool) {
		ev := fp.f.ReceivedMethod("Runtime.evaluate")
		var p struct {
			ContextID int `json:"contextId"`
		}
		_ = json.Unmarshal(ev[len(ev)-1].Params, &p)
		realm := fmt.Sprintf("ctx:%d", p.ContextID)
		if v, ok := m.staging(realm, expr); ok {
			return value(v), true
		}
		st, ok := stagedIn(expr)
		if !ok {
			if strings.HasPrefix(expr, stageChunkJS) || strings.HasPrefix(expr, stageDropJS) {
				return thrown("Error: the staging failed"), true
			}
			return nil, false
		}
		if expr != st.evalExpr() && expr != st.fnExpr() {
			return thrown("Error: unexpected expression"), true
		}
		src, res := m.final(realm, st)
		switch {
		case res != nil:
			return res, true
		case expr == st.evalExpr():
			return m.run(src), true
		}
		m.mu.Lock()
		m.fnCount++
		id := fmt.Sprintf("staged-fn-%d", m.fnCount)
		m.fns[id] = src
		m.mu.Unlock()
		return map[string]any{"result": map[string]any{"type": "function", "objectId": id}}, true
	}
	fp.mu.Unlock()
	fp.f.Handle("Runtime.callFunctionOn", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			ObjectID            string            `json:"objectId"`
			FunctionDeclaration string            `json:"functionDeclaration"`
			Arguments           []json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(c.Params, &p)
		realm := "obj:" + p.ObjectID
		if body, ok := strings.CutPrefix(p.FunctionDeclaration, "function () { return "); ok {
			m.mu.Lock()
			errPart := m.errPart
			m.mu.Unlock()
			if errPart && strings.HasPrefix(body, stageChunkJS) {
				return nil, &fakewebkit.Error{Code: -32000, Message: "the part was not delivered"}
			}
			if v, ok := m.staging(realm, strings.TrimSuffix(body, "; }")); ok {
				return value(v), nil
			}
		}
		answer := func(src, this string) (any, *fakewebkit.Error) {
			m.mu.Lock()
			fail := m.failFn
			m.mu.Unlock()
			if fail {
				return nil, &fakewebkit.Error{Code: -32000, Message: "the call failed"}
			}
			return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"src": src, "this": this, "args": p.Arguments}}}, nil
		}
		if st, ok := stagedIn(p.FunctionDeclaration); ok && p.FunctionDeclaration == st.callDecl() {
			src, res := m.final(realm, st)
			if res != nil {
				return res, nil
			}
			return answer(src, p.ObjectID)
		}
		m.mu.Lock()
		src, ok := m.fns[p.ObjectID]
		m.mu.Unlock()
		if ok && p.FunctionDeclaration == applyFunction {
			return answer(src, "")
		}
		return value("plain"), nil
	})
}

// limitedPage attaches page One (as attachPage does) behind a transport
// with WebKitGTK's limit, with the staging model on its fake page, then
// sets the limit to limit (before that, before runs when given). It
// returns the largest message written to the page's connections under
// testLimit.
func limitedPage(t *testing.T, limit int, before ...func(*rig)) (*rig, *fakePage, *stageModel, *cdptest.Client, string, *atomic.Int64) {
	t.Helper()
	largest, lim := &atomic.Int64{}, &atomic.Int64{}
	lim.Store(transport.WebKitGTKMaxOutgoing)
	r := newRigWrapped(t, func(tr transport.Transport) transport.Transport {
		return limitedTransport{Transport: tr, limit: lim, largest: largest}
	})
	fp := newFakePage(r.fake["One"])
	m := &stageModel{stores: map[string]map[string][]string{}, fns: map[string]string{}, marks: map[string]string{}, run: func(src string) map[string]any { return value(src) }}
	m.install(fp)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, sid := puppeteerAttach(t, c)
	for _, f := range before {
		f(r)
	}
	lim.Store(int64(limit))
	return r, fp, m, c, sid, largest
}

// bigSource is a string literal of n+len(tag)+2 bytes (the page model
// echoes a source, so its value does not matter).
func bigSource(tag string, n int) string {
	return "'" + tag + strings.Repeat("x", n) + "'"
}

func checkStagingDone(t *testing.T, m *stageModel, largest *atomic.Int64) {
	t.Helper()
	if left := m.left(); len(left) != 0 {
		t.Fatalf("staged entries left in the page: %v", left)
	}
	if l := largest.Load(); l > testLimit {
		t.Fatalf("a message of %d bytes went to the page; the limit is %d", l, testLimit)
	}
}

// Runtime.evaluate of an expression above the limit returns the value of
// the whole expression, keeps the call's params on the final evaluate,
// keeps a thrown value's exceptionDetails, and leaves nothing staged.
func TestLargeEvaluateIsStaged(t *testing.T) {
	_, fp, m, c, sid, largest := limitedPage(t, testLimit)
	src := bigSource("a", 20000)
	res := decode[struct{ Result map[string]any }](t, call(t, c, sid, "Runtime.evaluate", map[string]any{
		"expression": src, "returnByValue": true, "objectGroup": "g7", "userGesture": true,
	}))
	if res.Result["value"] != src {
		t.Fatalf("evaluate returned %d bytes, want the %d-byte source", len(fmt.Sprint(res.Result["value"])), len(src))
	}
	if m.parts < 2 || len(m.ids) != 1 {
		t.Fatalf("staged %d parts in %d entries, want several parts in one entry", m.parts, len(m.ids))
	}
	ev := fp.f.ReceivedMethod("Runtime.evaluate")
	var last struct {
		Expression         string `json:"expression"`
		ReturnByValue      bool   `json:"returnByValue"`
		ObjectGroup        string `json:"objectGroup"`
		EmulateUserGesture bool   `json:"emulateUserGesture"`
	}
	_ = json.Unmarshal(ev[len(ev)-1].Params, &last)
	if st, ok := stagedIn(last.Expression); !ok || last.Expression != st.evalExpr() || !last.ReturnByValue || last.ObjectGroup != "g7" || !last.EmulateUserGesture {
		t.Fatalf("final evaluate %s", ev[len(ev)-1].Params)
	}
	checkStagingDone(t, m, largest)

	m.run = func(string) map[string]any { return thrown("Error: boom") }
	res2 := decode[struct {
		Result           map[string]any
		ExceptionDetails map[string]any
	}](t, call(t, c, sid, "Runtime.evaluate", map[string]any{"expression": bigSource("b", 20000)}))
	if res2.ExceptionDetails["text"] != "Uncaught Error: boom" || res2.Result["subtype"] != "error" {
		t.Fatalf("thrown staged evaluate = %+v", res2)
	}
	checkStagingDone(t, m, largest)

	// A small expression is sent as it is.
	n := len(m.ids)
	m.run = func(src string) map[string]any { return value(src) }
	if v := decode[struct{ Result map[string]any }](t, call(t, c, sid, "Runtime.evaluate", map[string]any{"expression": "'small'"})); v.Result["value"] != float64(0) {
		t.Fatalf("small evaluate = %v", v.Result)
	}
	if len(m.ids) != n {
		t.Fatal("a small expression was staged")
	}
}

// Runtime.callFunctionOn with a declaration above the limit: on an
// objectId the parts are stored by calls on that object (its realm) and
// the staged declaration is applied with the call's this and arguments;
// on an execution context the parts go to that context, the staged
// declaration becomes the function object, which is called through the
// trampoline and released. Nothing stays staged.
func TestLargeCallFunctionOnIsStaged(t *testing.T) {
	_, fp, m, c, sid, largest := limitedPage(t, testLimit)
	decl := "function(a) { return " + bigSource("c", 20000) + "; }"
	type callValue struct {
		Src  string            `json:"src"`
		This string            `json:"this"`
		Args []json.RawMessage `json:"args"`
	}
	res := decode[struct {
		Result struct {
			Value callValue `json:"value"`
		}
	}](t, call(t, c, sid, "Runtime.callFunctionOn", map[string]any{
		"objectId": "obj-9", "functionDeclaration": decl, "returnByValue": false,
		"arguments": []any{map[string]any{"value": "x"}, map[string]any{"objectId": "obj-2"}},
	}))
	v := res.Result.Value
	if v.Src != decl || v.This != "obj-9" || len(v.Args) != 2 || !strings.Contains(string(v.Args[0]), `"x"`) || !strings.Contains(string(v.Args[1]), "obj-2") {
		t.Fatalf("staged call on an object: this %q, args %s, declaration of %d bytes (want %d)", v.This, v.Args, len(v.Src), len(decl))
	}
	checkStagingDone(t, m, largest)

	fp.mu.Lock()
	fp.replay = true
	fp.mu.Unlock()
	call(t, c, sid, "Runtime.enable", nil)
	main := contextsOf(t, c, 0, sid)[0].ID
	res = decode[struct {
		Result struct {
			Value callValue `json:"value"`
		}
	}](t, call(t, c, sid, "Runtime.callFunctionOn", map[string]any{
		"executionContextId": main, "functionDeclaration": decl, "arguments": []any{map[string]any{"value": 5}},
	}))
	if v := res.Result.Value; v.Src != decl || v.This != "" || len(v.Args) != 1 {
		t.Fatalf("staged call on a context: this %q, args %s, declaration of %d bytes (want %d)", v.This, v.Args, len(v.Src), len(decl))
	}
	// Every staging evaluate went to the call's context (realm ctx:1).
	for _, e := range fp.f.ReceivedMethod("Runtime.evaluate") {
		if strings.Contains(string(e.Params), "__iwdpSrc") && !strings.Contains(string(e.Params), `"contextId":1`) {
			t.Fatalf("a staging evaluate outside the call's context: %.200s", e.Params)
		}
	}
	rel := fp.f.ReceivedMethod("Runtime.releaseObject")
	if len(rel) == 0 || !strings.Contains(string(rel[len(rel)-1].Params), "staged-fn-1") {
		t.Fatalf("the function object was not released: %v", rel)
	}
	checkStagingDone(t, m, largest)
}

// A page that refuses indirect eval (its eval check throws; a CSP refusal
// as WebKit words it) fails a staged call with an error that names the
// cause; nothing is evaluated and nothing stays staged.
func TestLargeSourceWithoutEvalFails(t *testing.T) {
	_, fp, m, c, sid, largest := limitedPage(t, testLimit)
	m.noEval = `EvalError: Refused to evaluate a string as JavaScript because 'unsafe-eval' is not an allowed source of script in the following Content Security Policy directive: "script-src 'self'".`
	for _, p := range []map[string]any{
		{"method": "Runtime.evaluate", "expression": bigSource("d", 20000)},
		{"method": "Runtime.callFunctionOn", "objectId": "obj-9", "functionDeclaration": "function() { return " + bigSource("e", 20000) + "; }"},
	} {
		method := p["method"].(string)
		delete(p, "method")
		_, err := c.Call(ctxFor(t, 10*time.Second), sid, method, p)
		var ce *cdptest.Error
		if !errors.As(err, &ce) || ce.Code != codeServer || !strings.Contains(ce.Message, "the page refuses eval: "+m.noEval) || !strings.Contains(ce.Message, method) {
			t.Fatalf("%s on a page that refuses eval: %v", method, err)
		}
	}
	for _, e := range fp.f.ReceivedMethod("Runtime.evaluate") {
		if _, ok := stagedIn(expressionOf(e)); ok {
			t.Fatalf("a staged source was evaluated: %.200s", e.Params)
		}
	}
	checkStagingDone(t, m, largest)
}

// stagedCallForms returns the three staged call forms with sources above
// testLimit: Runtime.evaluate, and Runtime.callFunctionOn on an object and
// on an execution context (it enables Runtime on the replaying fake page
// for the context).
func stagedCallForms(t *testing.T, fp *fakePage, c *cdptest.Client, sid string) []struct {
	method string
	params map[string]any
} {
	t.Helper()
	fp.mu.Lock()
	fp.replay = true
	fp.mu.Unlock()
	call(t, c, sid, "Runtime.enable", nil)
	main := contextsOf(t, c, 0, sid)[0].ID
	decl := "function() { return " + bigSource("w", 20000) + "; }"
	return []struct {
		method string
		params map[string]any
	}{
		{"Runtime.evaluate", map[string]any{"expression": bigSource("r", 20000), "returnByValue": true}},
		{"Runtime.callFunctionOn", map[string]any{"objectId": "obj-9", "functionDeclaration": decl, "returnByValue": true}},
		{"Runtime.callFunctionOn", map[string]any{"executionContextId": main, "functionDeclaration": decl, "returnByValue": true}},
	}
}

// F1 of review w5c2b1-20261108-234450: a page whose eval does not give the
// final expression's probe values (a replacement that returns constants),
// or whose eval getter throws at the final read, gets the CDP error that
// names the refusal for every call form, instead of a value; the source
// does not run and nothing stays staged. Each final call sends its own
// random probes and refusal marker.
func TestStagedCallRefusesAFakeEval(t *testing.T) {
	_, fp, m, c, sid, largest := limitedPage(t, testLimit)
	var ran atomic.Int64
	m.run = func(src string) map[string]any { ran.Add(1); return value(src) }
	forms := stagedCallForms(t, fp, c, sid)
	for _, fault := range []string{notBuiltinEval, "Error: late eval getter"} {
		m.mu.Lock()
		m.evalFault = fault
		m.mu.Unlock()
		for _, f := range forms {
			_, err := c.Call(ctxFor(t, 10*time.Second), sid, f.method, f.params)
			var ce *cdptest.Error
			if !errors.As(err, &ce) || ce.Code != codeServer || !strings.Contains(ce.Message, f.method) || !strings.Contains(ce.Message, "the page refuses eval: "+fault) || strings.Contains(ce.Message, refusalPrefix) {
				t.Fatalf("%s %v with the eval fault %q: %v", f.method, f.params["objectId"], fault, err)
			}
			checkStagingDone(t, m, largest)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ran.Load() != 0 || len(m.fns) != 0 {
		t.Fatalf("a refused source ran (%d evaluations, %d functions)", ran.Load(), len(m.fns))
	}
	nonces, sums, markers := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, st := range m.finals {
		nonces[st.v], sums[st.q], markers[st.refusal] = true, true, true
	}
	if len(m.finals) != 6 || len(nonces) != 6 || len(sums) != 6 || len(markers) != 6 {
		t.Fatalf("%d final calls with %d distinct nonces, %d distinct sums and %d distinct refusal markers, want 6 of each", len(m.finals), len(nonces), len(sums), len(markers))
	}
}

// F2 of review w5c2b1-20261108-234450: a final call that throws without
// removing its entry (here before the final expression ran) keeps the
// thrown result for the client, and the bridge removes the entry, for
// every call form.
func TestThrownFinalCallIsRemoved(t *testing.T) {
	_, fp, m, c, sid, largest := limitedPage(t, testLimit)
	forms := stagedCallForms(t, fp, c, sid)
	m.mu.Lock()
	m.keepOnThrow = true
	m.mu.Unlock()
	for _, f := range forms {
		res := decode[struct {
			ExceptionDetails map[string]any
		}](t, call(t, c, sid, f.method, f.params))
		if res.ExceptionDetails["text"] != "Uncaught Error: the final call threw before it removed the entry" {
			t.Fatalf("%s %v with a thrown final call = %+v", f.method, f.params["objectId"], res)
		}
		checkStagingDone(t, m, largest)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.finals) != 3 {
		t.Fatalf("%d final calls, want 3", len(m.finals))
	}
}

// F3 of review w5c2b1-r1-20261109-013046: an error that the staged source
// itself throws keeps its result and exceptionDetails for every call form,
// also when its text names the refusal marker: the old static marker
// inside an Error's message, and a thrown string in the form of a refusal
// with another call's marker.
func TestStagedUserErrorKeepsExceptionDetails(t *testing.T) {
	_, fp, m, c, sid, largest := limitedPage(t, testLimit)
	forms := stagedCallForms(t, fp, c, sid)
	const foreign = "__iwdp_noeval_0123456789abcdef: the page's eval is not the built-in eval"
	for _, tc := range []struct {
		thrown map[string]any
		text   string
	}{
		{thrown("Error: application __iwdp_noeval__ actual failure"), "Uncaught Error: application __iwdp_noeval__ actual failure"},
		{thrownString(foreign), "Uncaught " + foreign},
	} {
		m.mu.Lock()
		m.srcThrow = tc.thrown
		m.mu.Unlock()
		for _, f := range forms {
			raw, err := c.Call(ctxFor(t, 10*time.Second), sid, f.method, f.params)
			if err != nil {
				t.Fatalf("%s %v: a user error became the CDP error %v", f.method, f.params["objectId"], err)
			}
			var res struct{ ExceptionDetails map[string]any }
			_ = json.Unmarshal(raw, &res)
			if res.ExceptionDetails["text"] != tc.text {
				t.Fatalf("%s %v: exceptionDetails %v, want the text %q", f.method, f.params["objectId"], res.ExceptionDetails, tc.text)
			}
			checkStagingDone(t, m, largest)
		}
	}
}

// F4 of review w5c2b1-r2-20261109-023612: no final call's source text
// holds the call's refusal marker (a staged source could read a caller's
// text), and the object-call trampoline is strict code, so a non-strict
// callee's .caller cannot reach it; it still applies the staged function
// with the call's this and arguments.
func TestStagedFinalTextsHoldNoMarker(t *testing.T) {
	_, fp, m, c, sid, largest := limitedPage(t, testLimit)
	for _, f := range stagedCallForms(t, fp, c, sid) {
		call(t, c, sid, f.method, f.params)
	}
	var finals []string
	for _, e := range fp.f.ReceivedMethod("Runtime.evaluate") {
		if expr := expressionOf(e); strings.HasPrefix(expr, stageRunJS) {
			finals = append(finals, expr)
		}
	}
	var trampolines []string
	for _, e := range fp.f.ReceivedMethod("Runtime.callFunctionOn") {
		var p struct {
			FunctionDeclaration string `json:"functionDeclaration"`
		}
		_ = json.Unmarshal(e.Params, &p)
		if strings.Contains(p.FunctionDeclaration, stageRunJS) {
			trampolines = append(trampolines, p.FunctionDeclaration)
		}
	}
	if len(finals) != 2 || len(trampolines) != 1 {
		t.Fatalf("%d final evaluates and %d trampolines, want 2 and 1", len(finals), len(trampolines))
	}
	for _, text := range append(finals, trampolines...) {
		if strings.Contains(text, refusalPrefix) {
			t.Fatalf("a final call's source text holds the refusal marker: %.300s", text)
		}
	}
	if !strings.HasPrefix(trampolines[0], `function () { "use strict"; `) {
		t.Fatalf("the object-call trampoline is not strict code with a simple parameter list: %.120s", trampolines[0])
	}
	checkStagingDone(t, m, largest)
}

// expressionOf is the expression of a received Runtime.evaluate.
func expressionOf(c fakewebkit.Record) string {
	var p struct {
		Expression string `json:"expression"`
	}
	_ = json.Unmarshal(c.Params, &p)
	return p.Expression
}

// isDrop reports whether expr is the removal of a staged entry.
func isDrop(expr string) bool {
	a, ok := args(expr, stageDropJS)
	return ok && len(a) == 1
}

// A staging that fails part way, and a staged call that fails, remove the
// entry from the page.
func TestFailedStagingIsRemoved(t *testing.T) {
	r, fp, m, c, sid, largest := limitedPage(t, testLimit)
	m.lost = true
	if _, err := c.Call(ctxFor(t, 10*time.Second), sid, "Runtime.evaluate", map[string]any{"expression": bigSource("f", 20000)}); err == nil || !strings.Contains(err.Error(), "staging part 1") {
		t.Fatalf("evaluate with a lost part: %v", err)
	}
	checkStagingDone(t, m, largest)
	m.lost, m.failFn = false, true
	if _, err := c.Call(ctxFor(t, 10*time.Second), sid, "Runtime.callFunctionOn", map[string]any{"objectId": "obj-9", "functionDeclaration": "function() { return " + bigSource("g", 20000) + "; }"}); err == nil {
		t.Fatal("a failing staged call succeeded")
	}
	m.mu.Lock()
	ids := len(m.ids)
	m.mu.Unlock()
	if ids != 2 {
		t.Fatalf("%d entries staged, want 2", ids)
	}
	checkStagingDone(t, m, largest)

	// A part whose send fails fails the call and removes the entry.
	m.failFn, m.errPart = false, true
	if _, err := c.Call(ctxFor(t, 10*time.Second), sid, "Runtime.callFunctionOn", map[string]any{"objectId": "obj-9", "functionDeclaration": "function() { return " + bigSource("p", 20000) + "; }"}); err == nil || !strings.Contains(err.Error(), "staging part 0") || !strings.Contains(err.Error(), "the part was not delivered") {
		t.Fatalf("callFunctionOn whose part is not delivered: %v", err)
	}
	checkStagingDone(t, m, largest)
	m.errPart = false

	// A final evaluate whose send fails sends the removal of its entry.
	fp.mu.Lock()
	fp.before = func(expr string) {
		if _, ok := stagedIn(expr); ok {
			fp.mu.Lock()
			fp.stale[0] = true // the final evaluate gets WebKit's error
			fp.mu.Unlock()
		}
	}
	fp.mu.Unlock()
	if _, err := c.Call(ctxFor(t, 10*time.Second), sid, "Runtime.evaluate", map[string]any{"expression": bigSource("q", 20000)}); err == nil || !strings.Contains(err.Error(), errNoContext.Message) {
		t.Fatalf("evaluate whose final send fails: %v", err)
	}
	ev := fp.f.ReceivedMethod("Runtime.evaluate")
	if last := expressionOf(ev[len(ev)-1]); !isDrop(last) {
		t.Fatalf("the last evaluate after the failed final send is %.200s, want the removal", last)
	}
	fp.mu.Lock()
	fp.before, fp.stale[0] = nil, false
	fp.mu.Unlock()
	m.mu.Lock()
	clear(m.stores) // the removal got the same error, as a navigation would give
	m.mu.Unlock()

	// A part that throws fails the call; a removal that fails is logged
	// (the entry goes with its document or the realm's next staging).
	m.failFn, m.throws, m.noDrop = false, true, true
	if _, err := c.Call(ctxFor(t, 10*time.Second), sid, "Runtime.evaluate", map[string]any{"expression": bigSource("t", 20000)}); err == nil || !strings.Contains(err.Error(), "staging part 1") || !strings.Contains(err.Error(), "threw Error: the staging failed") {
		t.Fatalf("evaluate with a throwing part: %v", err)
	}
	if !strings.Contains(r.log.text(), "removing the staged source k") {
		t.Fatalf("the failed removal was not logged: %s", r.log.text())
	}
}

// A source above maxStagedSource is refused before anything is staged,
// and a limit too small for a part fails the call with a message that
// says so.
func TestStagingBounds(t *testing.T) {
	_, _, m, c, sid, _ := limitedPage(t, testLimit)
	_, err := c.Call(ctxFor(t, 30*time.Second), sid, "Runtime.evaluate", map[string]any{"expression": bigSource("u", maxStagedSource)})
	var ce *cdptest.Error
	if !errors.As(err, &ce) || ce.Code != codeInvalidParams || !strings.Contains(ce.Message, "the bridge sends at most 16777216 bytes of source") {
		t.Fatalf("evaluate above maxStagedSource: %v", err)
	}
	m.mu.Lock()
	n := len(m.ids)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d entries staged for a refused source", n)
	}

	_, _, _, c, sid, _ = limitedPage(t, stageReserve+stageEscape*utf8.UTFMax-1)
	if _, err := c.Call(ctxFor(t, 10*time.Second), sid, "Runtime.evaluate", map[string]any{"expression": bigSource("v", 5000)}); err == nil || !strings.Contains(err.Error(), "which is too small to stage it") {
		t.Fatalf("evaluate under a tiny limit: %v", err)
	}
}

// Concurrent staged calls of two attachments of the page each get their
// own source back, under distinct staging ids.
func TestConcurrentLargeSourcesDoNotMix(t *testing.T) {
	var c2 *cdptest.Client
	var sid2 string
	_, _, m, c, sid, largest := limitedPage(t, testLimit, func(r *rig) {
		c2 = dial(t, r.issue(t, "s1", "One", ScopePage).WS)
		_, sid2 = puppeteerAttach(t, c2)
	})
	const each = 4
	var wg sync.WaitGroup
	errs := make(chan error, 2*each)
	for i := range 2 * each {
		cl, s := c, sid
		if i%2 == 1 {
			cl, s = c2, sid2
		}
		src := bigSource(fmt.Sprintf("call%d-", i), 9000+i*700)
		wg.Go(func() {
			raw, err := cl.Call(ctxFor(t, 20*time.Second), s, "Runtime.evaluate", map[string]any{"expression": src})
			if err != nil {
				errs <- err
				return
			}
			var res struct{ Result map[string]any }
			_ = json.Unmarshal(raw, &res)
			if res.Result["value"] != src {
				errs <- fmt.Errorf("call %d got another value (%.40v)", i, res.Result["value"])
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	m.mu.Lock()
	seen := map[string]bool{}
	for _, id := range m.ids {
		seen[id] = true
	}
	m.mu.Unlock()
	if len(seen) != 2*each {
		t.Fatalf("%d distinct staging ids for %d calls", len(seen), 2*each)
	}
	checkStagingDone(t, m, largest)
}

// A new-document script that makes the bootstrap script too large fails
// with an error that names the cause, and the attachment's scripts stay as
// they were: a later small script is installed without it.
func TestLargeNewDocumentScriptFails(t *testing.T) {
	_, fp, _, c, sid, _ := limitedPage(t, transport.WebKitGTKMaxOutgoing)
	big := "window.big = " + bigSource("h", transport.WebKitGTKMaxOutgoing) + ";"
	_, err := c.Call(ctxFor(t, 10*time.Second), sid, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": big})
	if err == nil || !strings.Contains(err.Error(), "new document") || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized new-document script: %v", err)
	}
	call(t, c, sid, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.small = 1;"})
	boot := fp.f.ReceivedMethod("Page.setBootstrapScript")
	if len(boot) == 0 || !strings.Contains(string(boot[len(boot)-1].Params), "window.small") || strings.Contains(string(boot[len(boot)-1].Params), "window.big") {
		t.Fatalf("bootstrap script after the refusal: %v", boot)
	}

	// When the bootstrap script cannot be installed again, the error says
	// so too.
	fp.f.Handle("Page.setBootstrapScript", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "the page refused the script"}
	})
	_, err = c.Call(ctxFor(t, 10*time.Second), sid, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": big})
	if err == nil || !strings.Contains(err.Error(), "new document") || !strings.Contains(err.Error(), "Installing the bootstrap script again without it failed") || !strings.Contains(err.Error(), "the page refused the script") {
		t.Fatalf("oversized new-document script with a failing reinstall: %v", err)
	}
}

// Audits.enable succeeds without reaching WebKit (Puppeteer 25.16.0 sends
// it on every attach and fails when it is unknown; P2 item 3).
func TestAuditsEnableIsANoEffectSuccess(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	call(t, c, sid, "Audits.enable", nil)
	for _, m := range received(r.fake["One"]) {
		if strings.HasPrefix(m, "Audits.") {
			t.Fatalf("%s reached WebKit", m)
		}
	}
}

// ---- properties ----

// sourceAlphabet has runes of every UTF-8 length and the characters that
// JSON escapes (quotes, backslash, control characters, <, >, & and the
// line separators), so part boundaries fall inside multi-byte runes and
// escapes.
var sourceAlphabet = []rune{
	'a', 'Z', '0', ' ', '"', '\\', '\'', '\n', '\t', 0x01, 0x1f, '<', '>', '&',
	'é', 'ß', 0x7ff, '€', '中', 0x2028, 0x2029, 0xfeff, 0xffff, '😀', '𝄞', 0x10ffff,
}

// genSource is a random source for the properties: n runes of the
// alphabet, where one in four sources is runes of one length after 0 to 3
// ASCII bytes, to put that length at every part boundary.
type genSource string

func (genSource) Generate(r *rand.Rand, size int) reflect.Value {
	n := 200 + r.Intn(40*size+1)
	var b strings.Builder
	if r.Intn(4) == 0 {
		b.WriteString(strings.Repeat("p", r.Intn(4)))
		c := sourceAlphabet[r.Intn(len(sourceAlphabet))]
		for range n {
			b.WriteRune(c)
		}
	} else {
		for range n {
			b.WriteRune(sourceAlphabet[r.Intn(len(sourceAlphabet))])
		}
	}
	return reflect.ValueOf(genSource(b.String()))
}

// propertyConfig is the fixed configuration of the properties.
func propertyConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(1))}
}

// splitSource: the parts join to the source, each is valid UTF-8 and at
// most the part size, and three JSON encodings of a part (the chunk
// literal, the WIP message, the Target-routing envelope) stay within
// stageEscape bytes per source byte plus the quotes and their escapes
// (14 bytes).
func TestSplitSourceProperty(t *testing.T) {
	f := func(s genSource, sz uint16) bool {
		size := utf8.UTFMax + int(sz)%600
		parts := splitSource(string(s), size)
		if strings.Join(parts, "") != string(s) {
			t.Logf("parts do not join to the source (size %d)", size)
			return false
		}
		for _, p := range parts {
			if !utf8.ValidString(p) || len(p) > size || (len(p) == 0 && len(s) > 0) {
				t.Logf("part of %d bytes (size %d, valid %v)", len(p), size, utf8.ValidString(p))
				return false
			}
			enc := p
			for range 3 {
				b, _ := json.Marshal(enc)
				enc = string(b)
			}
			if len(enc) > stageEscape*len(p)+14 {
				t.Logf("a %d-byte part is %d bytes after three encodings", len(p), len(enc))
				return false
			}
		}
		return true
	}
	if err := quick.Check(f, propertyConfig(2000)); err != nil {
		t.Fatal(err)
	}
}

// Any source above the limit, through the bridge and the page model, is
// evaluated exactly: the page gets the same source back, nothing stays
// staged, and no message exceeds the limit.
func TestStagedEvaluateExactProperty(t *testing.T) {
	_, _, m, c, sid, largest := limitedPage(t, testLimit)
	f := func(s genSource) bool {
		src := string(s)
		for len(src) < testLimit {
			src += string(s)
		}
		raw, err := c.Call(ctxFor(t, 20*time.Second), sid, "Runtime.evaluate", map[string]any{"expression": src})
		if err != nil {
			t.Logf("evaluate: %v", err)
			return false
		}
		var res struct{ Result map[string]any }
		_ = json.Unmarshal(raw, &res)
		if res.Result["value"] != src {
			t.Logf("a %d-byte source came back changed", len(src))
			return false
		}
		return len(m.left()) == 0 && largest.Load() <= testLimit
	}
	if err := quick.Check(f, propertyConfig(40)); err != nil {
		t.Fatal(err)
	}
}
