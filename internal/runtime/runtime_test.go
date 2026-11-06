package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
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
	if len(evals) != 3 || !strings.Contains(string(evals[1].Params), "iwdp-mcp agent runtime bundle") {
		t.Fatalf("evaluates = %d, want call, install of the bundle, one retry", len(evals))
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
