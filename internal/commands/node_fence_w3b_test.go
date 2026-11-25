package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W3b rebase repair round 1, finding F1 (docs/notes/w3b.md; design 2.4,
// 2.7, 3.8): a node id is valid only on the target that reported it. hover
// --css and highlight look the node up in the session's frame scope on
// page-1, then send CSS.forcePseudoState or DOM.highlightNode inside a
// desired-state change. When a commit puts page-2 in page-1's place
// between the lookup and the send, and the desired state follows it, the
// send is refused (webkit.ErrTargetSwapped, as the frame scope's fenced
// caller refuses it): page-2 gets no call with page-1's node id, and no
// entry is recorded for the node.
func TestNodeSendAfterACommitIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, method, entry string
		args                []string
	}{
		{"hover", "CSS.forcePseudoState", "w3b:hover|agent", []string{"hover", "#hov", "--css"}},
		{"highlight", "DOM.highlightNode", "w3b:highlight|agent", []string{"highlight", "#hov"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fh := frameNodeHarness(t)
			h := fh.h
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// The desired state addresses page-1, as the daemon's state of
			// the page's connection does.
			if err := h.ps.State.Apply(ctx, sequential{h.sess}); err != nil {
				t.Fatal(err)
			}
			fh.setInfo(map[string]any{"tag": "iframe", "src": "child.html", "index": 1, "frames": 2})
			if code, _, msg := fh.run("frame", "#kid"); code != 0 {
				t.Fatalf("frame #kid: %s", msg)
			}
			// The node lookup takes the change lock before it answers, so
			// the command's change waits until page-2 committed.
			held := make(chan func(), 1)
			var once sync.Once
			h.f.Handle("DOM.requestNode", func(fakewebkit.Call) (any, *fakewebkit.Error) {
				once.Do(func() { held <- startChange(h.ps) })
				return map[string]any{"nodeId": 42}, nil
			})
			op := fh.begin(t)
			defer op.End()
			res := make(chan cliRun, 1)
			go func() {
				var out, errb bytes.Buffer
				code := RunCLIStarted(context.Background(), append([]string{"--timeout", "5s"}, tc.args...), &out, &errb, nil,
					func(context.Context, *Invocation) (Env, error) { return opEnv{pageEnv{h.ps}, op}, nil })

				res <- cliRun{code, errb.String()}
			}()
			var release func()
			select {
			case release = <-held:
			case r := <-res:
				t.Fatalf("%s ended (exit %d, %q) before its node lookup", tc.name, r.code, r.errText)
			case <-time.After(5 * time.Second):
				t.Fatal("no node lookup")
			}
			h.f.Swap("page-2", 0)
			for end := time.Now().Add(time.Second); h.sess.ActiveTarget() != "page-2"; time.Sleep(time.Millisecond) {
				if time.Now().After(end) {
					release()
					t.Fatal("page-2 did not commit")
				}
			}
			if err := h.sess.ActiveSetup(ctx); err != nil {
				release()
				t.Fatal(err)
			}
			// The desired state follows the commit (the daemon's state
			// follows the connection's Target events).
			if err := h.ps.State.Apply(ctx, sequential{h.sess}); err != nil {
				release()
				t.Fatal(err)
			}
			release()
			var r cliRun
			select {
			case r = <-res:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s did not end after the change ended", tc.name)
			}
			for _, rec := range h.f.ReceivedMethod(tc.method) {
				if rec.Target == "page-2" {
					t.Fatalf("%s sent page-1's node id to page-2: %s (exit %d, %q)", tc.name, rec.Params, r.code, r.errText)
				}
			}
			if r.code == ExitOK || !strings.Contains(r.errText, webkit.ErrTargetSwapped.Error()) {
				t.Fatalf("%s after the commit: exit %d, stderr %q; want the refusal (target swapped)", tc.name, r.code, r.errText)
			}
			if h.ps.State.HasEntry(tc.entry) {
				t.Fatalf("%s recorded the entry %s although its send was refused", tc.name, tc.entry)
			}
		})
	}
}

// sequential is the page's connection without pipelining: its batches
// would refuse CSS.enable, which this connection answers one at a time
// (transport.Limits.CSSDomain).
type sequential struct{ s *webkit.Session }

func (q sequential) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return q.s.Send(ctx, method, params)
}
func (q sequential) ActiveTarget() string { return q.s.ActiveTarget() }

// startChange starts a page-state change that runs until the returned
// function is called; it returns once the change runs.
func startChange(ps *core.PageSession) func() {
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		_ = ps.State.Change(context.Background(), ps.Caller, func(webkit.Caller) error {
			close(started)
			<-done
			return nil
		})
	}()
	<-started
	return func() { close(done) }
}

// frameNodeHarness is newFrameHarness on a connection that answers
// CSS.enable, whose page also answers what hover --css and highlight send:
// the runtime's actions, nodeOf's pick by reference (DOM.requestNode, node
// 42) and the CSS and DOM methods.
func frameNodeHarness(t *testing.T) *frameHarness {
	t.Helper()
	fh := &frameHarness{t: t, h: newActionHarnessWith(t, transport.Limits{Routed: true, CSSDomain: true})}
	h := fh.h
	h.ps.State = core.NewDesiredState()
	h.page.set("act", attempt("dispatched", "", map[string]any{}))
	h.f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(c.Params, &p)
		if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(p.Expression); m != nil {
			return map[string]any{"result": map[string]any{"type": "object", "value": h.page.handler(c, m[1], json.RawMessage(m[2]))}}, nil
		}
		if strings.Contains(p.Expression, "contentWindow") { // frameExpr
			fh.mu.Lock()
			defer fh.mu.Unlock()
			return map[string]any{"result": map[string]any{"type": "object", "value": fh.info}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "object", "objectId": "obj-1"}}, nil // nodeOf's pick by reference
	})
	h.f.Handle("Runtime.enable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		h.f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 1, "type": "normal", "frameId": "main"}})
		h.f.Emit("Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 7, "type": "normal", "frameId": "f-kid"}})
		return map[string]any{}, nil
	})
	h.f.Result("Page.getResourceTree", map[string]any{"frameTree": map[string]any{
		"frame":       map[string]any{"id": "main"},
		"childFrames": []any{map[string]any{"frame": map[string]any{"id": "f-same"}}, map[string]any{"frame": map[string]any{"id": "f-kid"}}},
	}})
	h.f.Result("DOM.requestNode", map[string]any{"nodeId": 42})
	for _, m := range []string{"CSS.enable", "CSS.disable", "CSS.forcePseudoState", "DOM.highlightNode"} {
		h.f.Result(m, map[string]any{})
	}
	return fh
}
