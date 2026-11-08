package cdp

import (
	"encoding/json"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// pageFake scripts the fake page "One" for the W5b-2 domains: runtime
// calls go to fn (act through a fenced PageRuntime, so the attempt loop
// runs), and the page functions of dom_domain.go (jsCall) answer from
// page, with this = the objectId, "<doc>"#<handle>, or null.
type pageFake struct {
	mu   sync.Mutex
	pr   *fakewebkit.PageRuntime
	fn   map[string]func(args json.RawMessage) any // runtime function -> value
	acts []map[string]any                          // the args of every act step
	// actValue is the value of a dispatched act step (nil: none).
	actValue func(args map[string]any) any
	page     func(src, this string, args json.RawMessage) any
	ran      []pageFn // the page functions that ran
}

// rtError, returned by a pageFake runtime function, is the runtime's
// refusal with code and message.
type rtError struct{ code, msg string }

// pageFn is one page function call: its source, this and argument.
type pageFn struct {
	src, this string
	args      json.RawMessage
}

var byHandleCall = regexp.MustCompile(`I\._\.byHandle\(("[^"]*"),(\d+)\),`)

// evalArgs is the argument of a page function that jsCall evaluates.
var evalArgs = regexp.MustCompile(`(?s)\.call\((?:null|I\._\.byHandle\("[^"]*",\d+\)),(.*),I\._\)\}\}catch\(e\)`)

func newPageFake(f *fakewebkit.Server) *pageFake {
	pf := &pageFake{pr: fakewebkit.NewPageRuntime(), fn: map[string]func(json.RawMessage) any{}}
	rt := func(fn string, args json.RawMessage) any {
		pf.mu.Lock()
		h, av := pf.fn[fn], pf.actValue
		var a struct {
			Args map[string]any `json:"args"`
		}
		if fn == "act" {
			_ = json.Unmarshal(args, &a)
			pf.acts = append(pf.acts, a.Args)
		}
		pf.mu.Unlock()
		if h != nil {
			v := h(args)
			if e, ok := v.(rtError); ok {
				return map[string]any{"ok": false, "error": map[string]string{"code": e.code, "message": e.msg}}
			}
			return map[string]any{"ok": true, "value": v}
		}
		if fn == "ping" {
			return map[string]any{"ok": true, "value": true}
		}
		rep := pf.pr.Handler()(fakewebkit.Call{}, fn, args)
		if m, ok := rep.(map[string]any); ok && fn == "act" && av != nil {
			if v, ok := m["value"].(map[string]any); ok && v["status"] == "dispatched" {
				v["value"] = av(a.Args)
			}
		}
		return rep
	}
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil {
			return map[string]any{"result": map[string]any{"type": "object", "value": rt(m[1], json.RawMessage(m[2]))}}, nil
		}
		this := "null"
		if m := byHandleCall.FindStringSubmatch(p.Expression); m != nil {
			this = m[1] + "#" + m[2]
		}
		var a json.RawMessage
		if m := evalArgs.FindStringSubmatch(p.Expression); m != nil {
			a = json.RawMessage(m[1])
		}
		return pf.answer(p.Expression, this, a), nil
	})
	f.Handle("Runtime.callFunctionOn", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			ObjectID            string `json:"objectId"`
			FunctionDeclaration string `json:"functionDeclaration"`
			Arguments           []struct {
				Value    json.RawMessage `json:"value"`
				ObjectID string          `json:"objectId"`
			} `json:"arguments"`
		}
		_ = json.Unmarshal(c.Params, &p)
		var a json.RawMessage
		var objs []string
		for _, arg := range p.Arguments {
			if arg.ObjectID != "" {
				objs = append(objs, arg.ObjectID)
			} else {
				a = arg.Value
			}
		}
		if objs != nil {
			a, _ = json.Marshal(objs)
		}
		return pf.answer(p.FunctionDeclaration, p.ObjectID, a), nil
	})
	return pf
}

func (pf *pageFake) answer(src, this string, args json.RawMessage) any {
	pf.mu.Lock()
	h := pf.page
	pf.ran = append(pf.ran, pageFn{src: src, this: this, args: args})
	pf.mu.Unlock()
	var v any = map[string]any{"e": "no page function scripted"}
	if h != nil {
		v = h(src, this, args)
	}
	return map[string]any{"result": map[string]any{"type": "object", "value": v}}
}

// setPage scripts the page functions.
func (pf *pageFake) setPage(h func(src, this string, args json.RawMessage) any) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	pf.page = h
}

// setFn scripts runtime function name.
func (pf *pageFake) setFn(name string, f func(args json.RawMessage) any) {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	pf.fn[name] = f
}

// steps returns the args of the act steps so far.
func (pf *pageFake) steps() []map[string]any {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return append([]map[string]any(nil), pf.acts...)
}

// pageFns returns the page functions that ran so far.
func (pf *pageFake) pageFns() []pageFn {
	pf.mu.Lock()
	defer pf.mu.Unlock()
	return append([]pageFn(nil), pf.ran...)
}

// w5b2Page is the rig with the page fake on page "One" and a Puppeteer
// page attachment of session s1 on it.
type w5b2Page struct {
	r    *rig
	pf   *pageFake
	f    *fakewebkit.Server
	c    *cdptest.Client
	page string
}

func newW5b2Page(t *testing.T, opts ...func(*Options)) *w5b2Page {
	t.Helper()
	r := newRig(t, opts...)
	pf := newPageFake(r.fake["One"])
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	return &w5b2Page{r: r, pf: pf, f: r.fake["One"], c: c, page: page}
}

// call runs method on the page attachment and fails the test on an error.
func (w *w5b2Page) call(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	return call(t, w.c, w.page, method, params)
}

// fail runs method on the page attachment and returns its CDP error.
func (w *w5b2Page) fail(t *testing.T, method string, params any) *cdptest.Error {
	t.Helper()
	res, err := w.c.Call(ctxFor(t, 10*time.Second), w.page, method, params)
	if err == nil {
		t.Fatalf("%s %v succeeded with %s, want an error", method, params, res)
	}
	ce, ok := err.(*cdptest.Error)
	if !ok {
		t.Fatalf("%s: %v is not a CDP error", method, err)
	}
	return ce
}

// mark is the number of messages the fake page received so far.
func (w *w5b2Page) mark() int { return len(w.f.Received()) }

// sentSince lists the methods with params the fake page received after
// mark, without the runtime's own calls.
func (w *w5b2Page) sentSince(mark int) []string {
	var out []string
	for _, rec := range w.f.Received()[mark:] {
		out = append(out, rec.Method+" "+string(rec.Params))
	}
	return out
}

// methodsSince lists the methods the fake page received after mark.
func (w *w5b2Page) methodsSince(mark int) []string {
	var out []string
	for _, rec := range w.f.Received()[mark:] {
		out = append(out, rec.Method)
	}
	return out
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
