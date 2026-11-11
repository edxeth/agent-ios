package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// W3b (design 3.5, one task per event): the attempt loop's caller sends
// "act" by reference and awaits its promise, and keeps the one-message
// path (returnByValue) for the clock of the attempt fence.
func TestRuntimeCallerAwaitsTheActPromise(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "object", "objectId": "p7"}})
	f.Handle("Runtime.awaitPromise", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"ok": true, "value": map[string]any{"status": "dispatched"}}}}, nil
	})
	s := session(t, f)
	call := New(s, nil).RuntimeCaller()
	v, err := call(context.Background(), s, "act", map[string]any{"opId": "o1"})
	if err != nil || !strings.Contains(string(v), "dispatched") {
		t.Fatalf("act = %s, %v", v, err)
	}
	evals := f.ReceivedMethod("Runtime.evaluate")
	if len(evals) != 1 || !strings.Contains(string(evals[0].Params), `"returnByValue":false`) || len(f.ReceivedMethod("Runtime.awaitPromise")) != 1 {
		t.Fatalf("act was not an async call: evaluate %s, %d awaitPromise", evals[0].Params, len(f.ReceivedMethod("Runtime.awaitPromise")))
	}
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"ok": true, "value": map[string]any{"now": 1, "doc": "d"}}}})
	if _, err := call(context.Background(), s, "clock", nil); err != nil {
		t.Fatal(err)
	}
	evals = f.ReceivedMethod("Runtime.evaluate")
	if !strings.Contains(string(evals[len(evals)-1].Params), `"returnByValue":true`) || len(f.ReceivedMethod("Runtime.awaitPromise")) != 1 {
		t.Fatalf("clock was not a one-message call: %s", evals[len(evals)-1].Params)
	}
}

// An async call that the page answers at once with an object (the call
// contract answers a refused owner without calling the function) is read
// by value, not awaited: WebKit refuses Runtime.awaitPromise for an object
// that is not a promise ("Object with given id is not a Promise", seen on
// WebKitGTK).
func TestCallAsyncReadsAnImmediateObject(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "object", "className": "Object", "objectId": "o1"}})
	f.Handle("Runtime.callFunctionOn", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if !strings.Contains(string(c.Params), `"objectId":"o1"`) || !strings.Contains(string(c.Params), `"returnByValue":true`) {
			return nil, &fakewebkit.Error{Code: -1, Message: "wrong object"}
		}
		return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{
			"ok":    false,
			"error": map[string]any{"code": "stale_owner", "message": "lease epoch 1 is older"},
		}}}, nil
	})
	err := New(session(t, f), nil).CallAsync(context.Background(), "act", nil, nil)
	var re *Error
	if !errors.As(err, &re) || re.Code != CodeStaleOwner {
		t.Fatalf("CallAsync = %v, want the page's stale_owner envelope", err)
	}
	if n := len(f.ReceivedMethod("Runtime.awaitPromise")); n != 0 {
		t.Fatalf("awaited a non-promise %d times", n)
	}
}
