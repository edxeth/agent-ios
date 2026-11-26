package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

var settledID = regexp.MustCompile(`^\(function \(r\) \{ .*?var q = window\.__iwdpQ, f = q && q\["([^"]+)"\]`)

// wkPage answers the messages of runtime calls as WebKit does: answer
// gives the runtime function's result and whether it is a promise. An
// armed promise (armExpression) is a Promise object; Runtime.awaitPromise
// on it answers the value that the call settles it with (awaitErr: the
// error of a destroyed document instead); the settled call expression
// (settleExpression) answers PendingSentinel for a promise, else its
// value, by value with returnByValue and as an object id without.
type wkPage struct {
	mu       sync.Mutex
	answer   func(fn string) (value any, promise bool)
	awaitErr string
	armed    []string // the ids of the armed promises
	awaited  []string // the object ids that Runtime.awaitPromise awaited
	settled  []string // the ids that the call expressions settle
	installs int
}

func newWKPage(f *fakewebkit.Server, answer func(fn string) (any, bool)) *wkPage {
	p := &wkPage{answer: answer}
	f.Handle("Runtime.evaluate", p.evaluate)
	f.Handle("Runtime.awaitPromise", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var a struct {
			ID string `json:"promiseObjectId"`
		}
		_ = json.Unmarshal(c.Params, &a)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.awaited = append(p.awaited, a.ID)
		if p.awaitErr != "" {
			return nil, &fakewebkit.Error{Code: -32000, Message: p.awaitErr}
		}
		v, _ := p.answer("")
		return map[string]any{"result": map[string]any{"type": "object", "value": v}}, nil
	})
	f.Handle("Runtime.callFunctionOn", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"ok": true, "value": "by callFunctionOn"}}}, nil
	})
	return p
}

func (p *wkPage) evaluate(c fakewebkit.Call) (any, *fakewebkit.Error) {
	var a struct {
		Expression    string `json:"expression"`
		ReturnByValue bool   `json:"returnByValue"`
	}
	_ = json.Unmarshal(c.Params, &a)
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := fakewebkit.ArmPattern.FindStringSubmatch(a.Expression); m != nil {
		p.armed = append(p.armed, m[1])
		return map[string]any{"result": map[string]any{"type": "object", "subtype": "promise", "className": "Promise", "objectId": "armed-" + m[1]}}, nil
	}
	m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(a.Expression)
	if m == nil { // an install
		p.installs++
		return map[string]any{"result": map[string]any{"type": "undefined"}}, nil
	}
	if s := settledID.FindStringSubmatch(a.Expression); s != nil {
		p.settled = append(p.settled, s[1])
	}
	v, promise := p.answer(m[1])
	switch {
	case promise && strings.HasPrefix(a.Expression, "(function (r)"):
		return map[string]any{"result": map[string]any{"type": "string", "value": PendingSentinel}}, nil
	case promise:
		return map[string]any{"result": map[string]any{"type": "object", "subtype": "promise", "className": "Promise", "objectId": "p0"}}, nil
	case a.ReturnByValue:
		return map[string]any{"result": map[string]any{"type": "object", "value": v}}, nil
	}
	return map[string]any{"result": map[string]any{"type": "object", "className": "Object", "objectId": "o1"}}, nil
}

// methods lists the Runtime commands that f received, in order.
func methods(f *fakewebkit.Server) string {
	var out []string
	for _, c := range f.Received() {
		if strings.HasPrefix(c.Method, "Runtime.") {
			out = append(out, c.Method)
		}
	}
	return strings.Join(out, " ")
}

// FIX-K (sim recheck claim 5): the reply of an act attempt that the page
// answers in the task of the call (a native select whose change handler
// navigates, a retry, a refusal, the stale_owner envelope of the call
// contract) comes by value with the call's own Runtime.evaluate, so a
// navigation that the step started cannot lose it: the call does not
// wait for the armed await, nor read the result with a second message.
// The await is written before the call, so a step that answers a promise
// is awaited from before it runs.
func TestActAnsweredAtOnceComesWithTheCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]any
	}{
		{"dispatched", map[string]any{"ok": true, "value": map[string]any{"status": "dispatched"}}},
		{"stale owner", map[string]any{"ok": false, "error": map[string]any{"code": CodeStaleOwner, "message": "lease epoch 1 is older"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakewebkit.New(fakewebkit.Options{})
			defer f.Close()
			p := newWKPage(f, func(string) (any, bool) { return tc.env, false })
			f.Stall("Runtime.awaitPromise") // the page settles it too, but the call must not wait for it
			s := session(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			v, err := New(s, nil).RuntimeCaller()(ctx, s, "act", map[string]any{"opId": "o1"})
			var re *Error
			if tc.env["ok"] == true && (err != nil || !strings.Contains(string(v), "dispatched")) ||
				tc.env["ok"] == false && (!errors.As(err, &re) || re.Code != CodeStaleOwner) {
				t.Fatalf("act = %s, %v; want the page's envelope", v, err)
			}
			if got := methods(f); got != "Runtime.evaluate Runtime.awaitPromise Runtime.evaluate" {
				t.Fatalf("page received %q, want the arm, its await, then the call", got)
			}
			evals := f.ReceivedMethod("Runtime.evaluate")
			if !strings.Contains(string(evals[1].Params), `"returnByValue":true`) {
				t.Fatalf("act was not sent by value: %s", evals[1].Params)
			}
			if len(p.armed) != 1 || len(p.settled) != 1 || p.armed[0] != p.settled[0] {
				t.Fatalf("armed %v, settled %v: the call must settle the promise armed for it", p.armed, p.settled)
			}
		})
	}
}

// A probe dispatches nothing and always answers at once: one
// Runtime.evaluate by value, nothing armed.
func TestActProbeIsOneEvaluate(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	newWKPage(f, func(string) (any, bool) {
		return map[string]any{"ok": true, "value": map[string]any{"status": "retry", "reason": "ready to dispatch"}}, false
	})
	s := session(t, f)
	if _, err := New(s, nil).RuntimeCaller()(context.Background(), s, "actProbe", nil); err != nil {
		t.Fatal(err)
	}
	if got := methods(f); got != "Runtime.evaluate" {
		t.Fatalf("page received %q, want one Runtime.evaluate", got)
	}
}

// An async call whose function answers a promise (an act step that waits
// a task, the wait function) gets the value of the promise armed for it,
// each call its own.
func TestAsyncCallAwaitsItsArmedPromise(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	p := newWKPage(f, func(string) (any, bool) { return map[string]any{"ok": true, "value": 42}, true })
	r := New(session(t, f), nil)
	for i := range 2 {
		var v int
		if err := r.CallAsync(context.Background(), "wait", nil, &v); err != nil || v != 42 {
			t.Fatalf("call %d: CallAsync = %d, %v", i, v, err)
		}
	}
	want := "Runtime.evaluate Runtime.awaitPromise Runtime.evaluate Runtime.evaluate Runtime.awaitPromise Runtime.evaluate"
	if got := methods(f); got != want {
		t.Fatalf("page received %q, want %q", got, want)
	}
	if len(p.armed) != 2 || p.armed[0] == p.armed[1] || strings.Join(p.settled, " ") != strings.Join(p.armed, " ") ||
		p.awaited[0] != "armed-"+p.armed[0] || p.awaited[1] != "armed-"+p.armed[1] {
		t.Fatalf("armed %v, awaited %v, settled %v: each call must await and settle the promise armed for it", p.armed, p.awaited, p.settled)
	}
}

// A document destroyed before the promise of a call that ran settled
// loses its result: the error says so (ErrResultLost) and never counts as
// nothing ran, so the attempt loop reports the step as possibly run.
func TestLostPendingResultIsNotANothingRan(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	p := newWKPage(f, func(string) (any, bool) { return map[string]any{"ok": true}, true })
	p.awaitErr = "Missing injected script for given promiseObjectId"
	s := session(t, f)
	_, err := New(s, nil).RuntimeCaller()(context.Background(), s, "act", map[string]any{"opId": "o1"})
	if !errors.Is(err, ErrResultLost) || errors.Is(err, ErrNothingRan) || errors.Is(err, webkit.ErrNotSent) || !strings.Contains(err.Error(), p.awaitErr) {
		t.Fatalf("err = %v, want ErrResultLost with WebKit's error", err)
	}
}

// A page that gives no promise to arm (an error, a page that broke the
// expression) runs nothing of the call: the call expression is not sent,
// and the error wraps ErrNothingRan.
func TestArmFailureRunsNothing(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "object", "description": "TypeError: x"}, "wasThrown": true}, nil
	})
	s := session(t, f)
	_, err := New(s, nil).RuntimeCaller()(context.Background(), s, "act", map[string]any{"opId": "o1"})
	if !errors.Is(err, ErrNothingRan) || errors.Is(err, ErrResultLost) {
		t.Fatalf("err = %v, want ErrNothingRan", err)
	}
	if got := methods(f); got != "Runtime.evaluate" {
		t.Fatalf("page received %q, want only the arm", got)
	}
}

// The page answers a missing sentinel before it runs anything and leaves
// the armed promise alone, so the retry after the install settles the
// same armed promise: a first call in a new document costs no second arm.
func TestInstallRetryKeepsTheArmedReply(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	calls := 0
	p := newWKPage(f, func(fn string) (any, bool) {
		if fn == "" { // the await's answer
			return map[string]any{"ok": true, "value": 7}, false
		}
		calls++
		if calls == 1 {
			return MissingSentinel, false
		}
		return map[string]any{"ok": true, "value": 7}, true
	})
	var v int
	if err := New(session(t, f), nil).CallAsync(context.Background(), "wait", nil, &v); err != nil || v != 7 {
		t.Fatalf("CallAsync = %d, %v", v, err)
	}
	if len(p.armed) != 1 || p.installs != 1 || len(p.settled) != 2 || p.settled[0] != p.armed[0] || p.settled[1] != p.armed[0] {
		t.Fatalf("armed %v, installs %d, settled %v: want one arm that the call and its retry settle", p.armed, p.installs, p.settled)
	}
}

// A call that reached a newer document than its arm (a navigation between
// the two messages) and answered a promise there cannot be awaited: the
// page says so (UnarmedSentinel), and the call fails at once with
// ErrResultLost instead of waiting for an await that nothing settles.
func TestUnarmedPromiseIsAResultLost(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	newWKPage(f, func(string) (any, bool) { return UnarmedSentinel, false })
	f.Stall("Runtime.awaitPromise")
	s := session(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := New(s, nil).RuntimeCaller()(ctx, s, "act", map[string]any{"opId": "o1"})
	if !errors.Is(err, ErrResultLost) || errors.Is(err, ErrNothingRan) || ctx.Err() != nil {
		t.Fatalf("err = %v, want ErrResultLost at once", err)
	}
}
