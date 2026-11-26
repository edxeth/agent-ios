package core

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/edxeth/agent-ios/internal/runtime"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// awaitReturns is a Caller that reports when a Runtime.awaitPromise sent
// through it returned: the page connection below has then decided
// whether to probe.
type awaitReturns struct {
	webkit.Caller
	returned chan error
}

func (c awaitReturns) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := c.Caller.Send(ctx, method, params)
	if method == "Runtime.awaitPromise" {
		c.returned <- err
	}
	return raw, err
}

// FIX-M F2: an armed runtime call that ends without its await (the page
// threw, the call answered UNARMED, the install failed, the by-value reply
// came first) cancels the written await; the page connection starts no
// liveness probe for that cancel. Before, each such cancel started a
// background probe, which can mark a loading page stalled.
func TestCanceledArmedAwaitStartsNoProbe(t *testing.T) {
	value := func(v any) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "object", "value": v}}, nil
	}
	for _, tc := range []struct {
		name string
		call func() (any, *fakewebkit.Error)
	}{
		{"the page threw", func() (any, *fakewebkit.Error) {
			return map[string]any{"result": map[string]any{"type": "object", "description": "TypeError"}, "wasThrown": true}, nil
		}},
		{"unarmed", func() (any, *fakewebkit.Error) { return value(runtime.UnarmedSentinel) }},
		{"the install failed", func() (any, *fakewebkit.Error) { return value(runtime.MissingSentinel) }},
		{"the by-value reply came first", func() (any, *fakewebkit.Error) {
			return value(map[string]any{"ok": true, "value": map[string]any{"met": true}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakewebkit.New(fakewebkit.Options{})
			t.Cleanup(f.Close)
			f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
				var p struct {
					Expression string `json:"expression"`
				}
				_ = json.Unmarshal(c.Params, &p)
				if res, ok := fakewebkit.ArmedPromise(p.Expression); ok {
					return res, nil
				}
				if fakewebkit.RuntimeCallPattern.MatchString(p.Expression) {
					return tc.call()
				}
				// an install script
				return map[string]any{"result": map[string]any{"type": "object", "description": "SyntaxError"}, "wasThrown": true}, nil
			})
			f.Stall("Runtime.awaitPromise")
			ctx := testDeadlineCtx(t)
			sess, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
				return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
			}, webkit.SessionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sess.Close() })
			var backgrounds atomic.Int32
			pc := newPageConn(sess, func() {}, func(func(context.Context)) bool { backgrounds.Add(1); return true }, t.Logf)
			watches := backgrounds.Load() // the connection's watch
			s := NewScheduler(pc, SchedulerOptions{})
			t.Cleanup(s.Close)
			s.SetLease(testSession, 1)
			op, err := s.Begin(ctx, OpSpec{Name: "test", Class: ClassRead, Session: testSession, Epoch: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer op.End()
			c := awaitReturns{op.Caller(), make(chan error, 1)}
			var v any
			err = runtime.New(nil, nil).Via(c).CallAsync(ctx, "wait", map[string]any{"k": "text", "text": "x", "slice": 1000}, &v)
			t.Logf("CallAsync: %v", err)
			awaitErr := <-c.returned
			if awaitErr == nil {
				t.Fatal("the stalled await was answered")
			}
			if n := backgrounds.Load() - watches; n != 0 {
				t.Fatalf("the canceled armed await (%v) started %d background probes, want none", awaitErr, n)
			}
		})
	}
}
