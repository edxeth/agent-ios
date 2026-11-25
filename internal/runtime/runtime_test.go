package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

func session(t *testing.T, f *fakewebkit.Server) *webkit.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	}, webkit.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestExpressionFollowsTheCallContract(t *testing.T) {
	expr, err := Expression("snapshot", map[string]any{"q": "</script>\u2028"})
	if err != nil {
		t.Fatal(err)
	}
	m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(expr)
	if m == nil || m[1] != "snapshot" || !strings.HasPrefix(expr, `(window.__iwdp && window.__iwdp.v === "`+Version+`")`) {
		t.Fatalf("expression %s does not follow design 3.1", expr)
	}
	if strings.Contains(m[2], "</script>") || strings.Contains(m[2], "\u2028") {
		t.Fatalf("args %s are not escaped for JavaScript source", m[2])
	}
	if _, err := Expression("x); alert(1", nil); err == nil {
		t.Fatal("a function name that is not an identifier was accepted")
	}
}

func TestCallDecodesEnvelopes(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.SetRuntime(func(_ fakewebkit.Call, fn string, args json.RawMessage) any {
		if fn == "fail" {
			return map[string]any{"ok": false, "error": map[string]string{"code": "stale_ref", "message": "ref e3 is stale"}}
		}
		return map[string]any{"ok": true, "value": map[string]json.RawMessage{"echo": args}}
	})
	r := New(session(t, f), nil)
	var out struct{ Echo map[string]int }
	if err := r.Call(context.Background(), "echo", map[string]int{"n": 7}, &out); err != nil || out.Echo["n"] != 7 {
		t.Fatalf("Call = %+v, %v", out, err)
	}
	var re *Error
	if err := r.Call(context.Background(), "fail", nil, nil); !errors.As(err, &re) || re.Code != "stale_ref" {
		t.Fatalf("error envelope gave %v", err)
	}
}

func TestCallInstallsOnceWhenMissing(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "string", "value": MissingSentinel}}, nil
	})
	err := New(session(t, f), nil).Call(context.Background(), "echo", nil, nil)
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("err = %v, want ErrNotInstalled", err)
	}
	evals := f.ReceivedMethod("Runtime.evaluate")
	if len(evals) != 3 || !strings.Contains(string(evals[1].Params), "agent-ios agent runtime bundle") {
		t.Fatalf("evaluates = %d, want call, install of the bundle, one retry", len(evals))
	}
}

// Design 3.1 owner epoch: with an owner, every call first hands the owner
// to the page (__iwdp.own), which resets the page maps when it differs; the
// call stays in the form the fake page parses.
func TestOwnedCallCarriesOwner(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	var gotFn string
	f.SetRuntime(func(_ fakewebkit.Call, fn string, _ json.RawMessage) any {
		gotFn = fn
		return map[string]any{"ok": true, "value": 1}
	})
	r := New(session(t, f), nil)
	r.SetOwner(Owner{Daemon: "d1", Session: "s\"1", Epoch: 4})
	if err := r.Call(context.Background(), "ping", nil, nil); err != nil || gotFn != "ping" {
		t.Fatalf("Call = %v, page saw %q", err, gotFn)
	}
	expr := string(f.ReceivedMethod("Runtime.evaluate")[0].Params)
	if !strings.Contains(expr, `window.__iwdp.own({\"daemon\":\"d1\",\"session\":\"s\\\"1\",\"epoch\":4})`) {
		t.Fatalf("call does not hand the owner to the page: %s", expr)
	}
	if o, ok := r.Owner(); !ok || o.Epoch != 4 {
		t.Fatalf("Owner() = %+v, %v", o, ok)
	}
}

// Design 3.1: the page accepts an owner with the first call that carries
// it. A refusal of an older epoch (stale_owner) does not confirm it; any
// other envelope does, also one with another error.
func TestOwnerConfirmedByTheFirstAcceptedCall(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.SetRuntime(func(_ fakewebkit.Call, fn string, _ json.RawMessage) any {
		switch fn {
		case "refused":
			return map[string]any{"ok": false, "error": map[string]string{"code": CodeStaleOwner, "message": "lease epoch 1 is older"}}
		case "fail":
			return map[string]any{"ok": false, "error": map[string]string{"code": "not_found", "message": "no element"}}
		}
		return map[string]any{"ok": true, "value": 1}
	})
	r := New(session(t, f), nil)
	if !r.OwnerConfirmed() {
		t.Fatal("a runtime without an owner is not confirmed")
	}
	r.SetOwner(Owner{Daemon: "d1", Session: "s", Epoch: 1})
	if r.OwnerConfirmed() {
		t.Fatal("an owner is confirmed before any call carried it")
	}
	if err := r.Call(context.Background(), "refused", nil, nil); err == nil || r.OwnerConfirmed() {
		t.Fatalf("after a stale_owner refusal: err %v, confirmed %v", err, r.OwnerConfirmed())
	}
	if err := r.Call(context.Background(), "fail", nil, nil); err == nil || !r.OwnerConfirmed() {
		t.Fatalf("after a not_found envelope: err %v, confirmed %v", err, r.OwnerConfirmed())
	}
	r.SetOwner(Owner{Daemon: "d1", Session: "s", Epoch: 2})
	if r.OwnerConfirmed() {
		t.Fatal("a new owner is confirmed before any call carried it")
	}
	if err := r.Call(context.Background(), "ping", nil, nil); err != nil || !r.OwnerConfirmed() {
		t.Fatalf("after an accepted call: err %v, confirmed %v", err, r.OwnerConfirmed())
	}
}

// The page reports a reset of its maps in the envelope; OnReset receives it
// once, also when the call itself fails.
func TestResetReportReachesOnReset(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.SetRuntime(func(_ fakewebkit.Call, fn string, _ json.RawMessage) any {
		reset := map[string]any{"reason": "daemon", "maxRef": 7}
		if fn == "fail" {
			return map[string]any{"ok": false, "error": map[string]string{"code": "unknown_ref", "message": "no element"}, "reset": reset}
		}
		return map[string]any{"ok": true, "value": 1, "reset": reset}
	})
	r := New(session(t, f), nil)
	var got []ResetInfo
	r.OnReset(func(i ResetInfo) { got = append(got, i) })
	if err := r.Call(context.Background(), "ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Call(context.Background(), "fail", nil, nil); err == nil {
		t.Fatal("failing call succeeded")
	}
	if len(got) != 2 || got[0] != (ResetInfo{Reason: "daemon", MaxRef: 7}) {
		t.Fatalf("OnReset got %+v", got)
	}
}

// RuntimeCaller (core.SchedulerOptions.Runtime) runs a call through the
// operation's caller with the runtime's owner.
func TestRuntimeCallerUsesGivenCaller(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.SetRuntime(func(_ fakewebkit.Call, fn string, args json.RawMessage) any {
		return map[string]any{"ok": true, "value": map[string]any{"fn": fn, "args": args}}
	})
	s := session(t, f)
	r := New(nil, nil)
	r.SetOwner(Owner{Daemon: "d", Session: "s", Epoch: 1})
	raw, err := r.RuntimeCaller()(context.Background(), s, "clock", map[string]int{"a": 1})
	if err != nil || !strings.Contains(string(raw), `"fn":"clock"`) {
		t.Fatalf("RuntimeCaller = %s, %v", raw, err)
	}
	if !strings.Contains(string(f.ReceivedMethod("Runtime.evaluate")[0].Params), "window.__iwdp.own(") {
		t.Fatal("RuntimeCaller call carries no owner")
	}
}

// Design 3.1: the installed script defines this Version; the bundle text
// carries a placeholder that the install replaces.
func TestSourceCarriesVersion(t *testing.T) {
	if !strings.Contains(Source(), Version) || strings.Contains(Source(), "__IWDP_VERSION__") {
		t.Fatal("Source() does not carry the runtime Version")
	}
}

// Design 3.1: the install message is checked against the transport's
// MaxOutgoing on its wire bytes, and an oversized install fails before
// anything is written.
func TestInstallOverWireLimitFailsBeforeWriting(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.SetRuntime(func(fakewebkit.Call, string, json.RawMessage) any { return MissingSentinel })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	limit := len(Source()) / 2
	s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true, MaxOutgoing: limit})
	}, webkit.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	err = New(s, nil).Call(ctx, "ping", nil, nil)
	var wl *webkit.WireLimitError
	if !errors.As(err, &wl) || wl.Limit != limit || !strings.Contains(err.Error(), "installing the agent runtime") ||
		!strings.Contains(err.Error(), "bytes on the wire; the limit for this transport is") {
		t.Fatalf("oversized install gave %v", err)
	}
	if n := len(f.ReceivedMethod("Runtime.evaluate")); n != 1 {
		t.Fatalf("the fake received %d evaluates, want only the first call (no install written)", n)
	}
}

func TestCallAsyncAwaitsPromise(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "object", "subtype": "promise", "objectId": "p1"}})
	f.Handle("Runtime.awaitPromise", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if !strings.Contains(string(c.Params), `"promiseObjectId":"p1"`) {
			return nil, &fakewebkit.Error{Code: -1, Message: "wrong promise"}
		}
		return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"ok": true, "value": 42}}}, nil
	})
	var v int
	if err := New(session(t, f), nil).CallAsync(context.Background(), "wait", nil, &v); err != nil || v != 42 {
		t.Fatalf("CallAsync = %d, %v", v, err)
	}
}

// Design 2.4 (row "Page-side runtime state"): the daemon releases a lease
// with __iwdp.release(epoch). The call carries no owner, so it never makes
// the page reset for a new owner; a document without the runtime has
// nothing to release, so it is not installed; an error envelope is
// returned to the caller.
func TestReleaseCallsThePageWithoutOwnerOrInstall(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	reply := any(map[string]any{"ok": true, "value": true})
	var args []string
	f.SetRuntime(func(_ fakewebkit.Call, fn string, a json.RawMessage) any {
		args = append(args, fn+" "+string(a))
		return reply
	})
	s := session(t, f)
	r := New(s, nil)
	r.SetOwner(Owner{Daemon: "d", Session: "s1", Epoch: 4})
	if err := r.Release(context.Background(), s, 4); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if len(args) != 1 || args[0] != `release {"epoch":4}` {
		t.Fatalf("page calls = %q, want one release with epoch 4", args)
	}
	evals := f.ReceivedMethod("Runtime.evaluate")
	if strings.Contains(string(evals[0].Params), "__iwdp.own(") {
		t.Fatalf("the release call carries an owner: %s", evals[0].Params)
	}

	reply = map[string]any{"ok": false, "error": map[string]string{"code": "internal", "message": "boom"}}
	var re *Error
	if err := r.Release(context.Background(), s, 4); !errors.As(err, &re) || re.Code != "internal" {
		t.Fatalf("Release with an error envelope = %v, want the page error", err)
	}

	reply = MissingSentinel
	if err := r.Release(context.Background(), s, 4); err != nil {
		t.Fatalf("Release on a page without the runtime = %v, want nil", err)
	}
	for _, e := range f.ReceivedMethod("Runtime.evaluate") {
		if strings.Contains(string(e.Params), "agent-ios agent runtime bundle") {
			t.Fatal("Release installed the runtime")
		}
	}
}

// Via (design 2.5, 2.10): a runtime bound to another caller sends through
// that caller and shares the owner and its confirmation with the page
// runtime.
func TestViaSendsThroughItsCallerAndSharesTheOwner(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.SetRuntime(func(fakewebkit.Call, string, json.RawMessage) any { return map[string]any{"ok": true, "value": true} })
	s := session(t, f)
	r := New(s, nil)
	cc := &countingCaller{Caller: s}
	v := r.Via(cc)
	r.SetOwner(Owner{Daemon: "d", Session: "s1", Epoch: 2})
	if r.OwnerConfirmed() {
		t.Fatal("owner confirmed before any call")
	}
	if err := v.Call(context.Background(), "ping", nil, nil); err != nil {
		t.Fatalf("Call through Via: %v", err)
	}
	if cc.n != 1 {
		t.Fatalf("the bound caller sent %d messages, want 1", cc.n)
	}
	if !r.OwnerConfirmed() {
		t.Fatal("a call through Via did not confirm the owner of the page runtime")
	}
}

type countingCaller struct {
	webkit.Caller
	n int
}

func (c *countingCaller) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.n++
	return c.Caller.Send(ctx, method, params)
}
