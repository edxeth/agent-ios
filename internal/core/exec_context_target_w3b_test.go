package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// targetPage is a fake page whose targets each report the frame kid's main
// world on Runtime.enable: context 17 on page-1, 27 on page-2. Its
// session enables Runtime in every provisional target's setup, as the
// desired state does while an owner holds Runtime.
func targetPage(t *testing.T) (*fakewebkit.Server, *webkit.Session) {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Handle("Runtime.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		id := 17
		if c.Target == "page-2" {
			id = 27
		}
		f.EmitFrom(c.Target, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": id, "frameId": "kid", "type": "normal"}})
		return map[string]any{}, nil
	})
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "boolean", "value": true}})
	ctx := ctxFor(t, 5*time.Second)
	sess, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	}, webkit.SessionOptions{PauseOnStart: true, Setup: func(ctx context.Context, c webkit.TargetCaller) error {
		_, err := c.Send(ctx, "Runtime.enable", nil)
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if _, err := sess.Send(ctx, "Runtime.enable", nil); err != nil {
		t.Fatal(err)
	}
	return f, sess
}

// commitPage2 commits the provisional target page-2, which replaces page-1
// (a cross-site navigation), and waits for its setup.
func commitPage2(t *testing.T, ctx context.Context, f *fakewebkit.Server, sess *webkit.Session) error {
	f.Swap("page-2", 0)
	eventually(t, time.Second, "page-2 to commit", func() bool { return sess.ActiveTarget() == "page-2" })
	return sess.ActiveSetup(ctx)
}

// evaluatesWith returns the targets that received a Runtime.evaluate with
// contextId id.
func evaluatesWith(f *fakewebkit.Server, id int64) []string {
	var out []string
	for _, r := range f.ReceivedMethod("Runtime.evaluate") {
		var p struct {
			ContextID int64 `json:"contextId"`
		}
		_ = json.Unmarshal(r.Params, &p)
		if p.ContextID == id {
			out = append(out, r.Target)
		}
	}
	return out
}

// W3b final repair round 1, finding 1 (the reviewer's reproduction
// TestReviewFrameReleasePinsTheReportingTarget; design 3.8, step 3c of the
// W3b final brief: no old context id is ever sent). A frame context id
// goes only to the target that reported it. When an ordinary commit
// replaces that target between the lookup of the id and the release's
// send, the send is refused (webkit.ErrTargetSwapped), the release fails,
// and the frame stays recorded; the next release finds the frame's
// context on the new target. Both records are covered: a frame recorded
// on an earlier connection, looked up by the release, and a context that
// an operation selected on this connection.
func TestFrameReleasePinsTheReportingTarget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		moved bool // the frame was recorded on an earlier connection (contextsMoved)
	}{{"frame of an earlier connection", true}, {"context selected on this connection", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f, sess := targetPage(t)
			ctx := ctxFor(t, 5*time.Second)
			s := NewScheduler(sess, SchedulerOptions{})
			t.Cleanup(s.Close)
			s.SetLease("s1", 1)
			ec := s.FrameContext("kid")
			if ec.ID != 17 {
				t.Fatalf("frame context on page-1 = %d, want 17", ec.ID)
			}
			s.installContext(1, ec)
			if tc.moved {
				s.contextsMoved()
			}
			op, err := s.Begin(ctx, OpSpec{Session: "s1", Epoch: 1, Name: "get text", Class: ClassRead})
			if err != nil {
				t.Fatal(err)
			}
			defer op.End()
			var once sync.Once
			err = s.ReleaseExecutionContexts(ctx, 1, op.Caller(), func(ctx context.Context, c webkit.Caller) error {
				var cerr error
				once.Do(func() { cerr = commitPage2(t, ctx, f, sess) }) // between the lookup and the send
				if cerr != nil {
					return cerr
				}
				_, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "release", "returnByValue": true})
				return err
			})
			if got := evaluatesWith(f, 17); len(got) != 0 {
				t.Fatalf("context 17 of page-1 was sent to %q after page-2 replaced page-1", got)
			}
			if !errors.Is(err, webkit.ErrTargetSwapped) {
				t.Fatalf("release error = %v, want ErrTargetSwapped (the cleanup did not reach its target)", err)
			}
			if e := s.ContextEpochsBefore(2); !reflect.DeepEqual(e, []uint64{1}) {
				t.Fatalf("epochs with recorded contexts after the refused release = %v, want [1]", e)
			}
			var sent []int64
			err = s.ReleaseExecutionContexts(ctx, 1, op.Caller(), func(ctx context.Context, c webkit.Caller) error {
				sent = append(sent, c.(contextCaller).ec.ID)
				_, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "release", "returnByValue": true})
				return err
			})
			if err != nil || !reflect.DeepEqual(sent, []int64{27}) || !reflect.DeepEqual(evaluatesWith(f, 27), []string{"page-2"}) {
				t.Fatalf("second release: contexts %v (to %q), err %v; want 27 on page-2", sent, evaluatesWith(f, 27), err)
			}
			if e := s.ContextEpochsBefore(2); len(e) != 0 {
				t.Fatalf("epochs with recorded contexts at the end = %v", e)
			}
		})
	}
}

// Finding 1, the other senders of a context id: an operation that works
// in a frame (frame selection, eval, actions and waits in the frame) sends
// to the frame's reporting target. After a commit replaced that target,
// its sends are refused, and none of them reaches the new target; a call
// that names its own target keeps it and gets no context id.
func TestFrameOperationPinsTheReportingTarget(t *testing.T) {
	f, sess := targetPage(t)
	ctx := ctxFor(t, 5*time.Second)
	s := NewScheduler(sess, SchedulerOptions{})
	t.Cleanup(s.Close)
	s.SetLease("s1", 1)
	op, err := s.Begin(ctx, OpSpec{Session: "s1", Epoch: 1, Name: "get text", Class: ClassRead})
	if err != nil {
		t.Fatal(err)
	}
	defer op.End()
	op.SetExecutionContext(s.FrameContext("kid"))
	if _, err := op.Caller().Send(ctx, "Runtime.evaluate", map[string]any{"expression": "1"}); err != nil {
		t.Fatal(err)
	}
	if got := evaluatesWith(f, 17); !reflect.DeepEqual(got, []string{"page-1"}) {
		t.Fatalf("context 17 sent to %q, want page-1", got)
	}
	if err := commitPage2(t, ctx, f, sess); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"Runtime.evaluate", "Runtime.callFunctionOn"} {
		if _, err := op.Caller().Send(ctx, m, map[string]any{"expression": "1", "objectId": `{"injectedScriptId":17,"id":1}`}); !errors.Is(err, webkit.ErrTargetSwapped) || !errors.Is(err, webkit.ErrNotSent) {
			t.Fatalf("%s after the commit: %v, want not sent with ErrTargetSwapped", m, err)
		}
	}
	if got := evaluatesWith(f, 17); !reflect.DeepEqual(got, []string{"page-1"}) {
		t.Fatalf("context 17 sent to %q after the commit, want only the earlier page-1 call", got)
	}
	if n := len(f.ReceivedMethod("Runtime.callFunctionOn")); n != 0 {
		t.Fatalf("Runtime.callFunctionOn reached the page %d times", n)
	}
	// A desired-state change addresses its own target (webkit.WithTarget).
	if _, err := op.Caller().Send(webkit.WithTarget(ctx, "page-2"), "Runtime.evaluate", map[string]any{"expression": "2"}); err != nil {
		t.Fatal(err)
	}
	if got := evaluatesWith(f, 0); len(got) == 0 || got[len(got)-1] != "page-2" {
		t.Fatalf("a call addressed to page-2 went to %q without a context id, want page-2", got)
	}
}

// Finding 1 across a reconnect: a send addressed to an execution context
// of the page's earlier connection is refused by the page connection
// (errConnReplaced), also when the new connection names its target with
// the same id.
func TestPinnedSendNeverReachesAReplacementConnection(t *testing.T) {
	a, b := newInventoryCaller(t), newInventoryCaller(t)
	a.set("page-1", webkit.AgentEnabled, normalCtx(7, "kid"))
	b.set("page-1", webkit.AgentEnabled, normalCtx(7, "kid"))
	pc := newPageConn(a, func() {}, func(func(context.Context)) bool { return false }, t.Logf)
	// Closed before the callers' cleanup ends them, so the connection
	// watch never logs after the test (a race with t.Logf).
	t.Cleanup(pc.close)
	s := NewScheduler(pc, SchedulerOptions{})
	t.Cleanup(s.Close)
	ec := s.FrameContext("kid")
	if ec.ID != 7 || ec.conn != a || ec.target != "page-1" {
		t.Fatalf("frame context = %+v, want 7 on page-1 of the first connection", ec)
	}
	pc.swap(b)
	_, err := contextCaller{Caller: pc, ec: ec}.Send(ctxFor(t, time.Second), "Runtime.evaluate", map[string]any{"expression": "release"})
	if !errors.Is(err, errConnReplaced) || !errors.Is(err, webkit.ErrNotSent) || !targetReplaced(err) {
		t.Fatalf("send to the replaced connection: %v, want not sent with errConnReplaced", err)
	}
	if got := s.FrameContext("kid"); got.conn != b {
		t.Fatalf("frame context after the swap = %+v, want the new connection's", got)
	}
}

// W3b final repair round 1, finding 2 (the reviewer's reproduction
// TestReviewExplicitURLClientRetainsFrameInventory): core opens a page
// given by an explicit ws:// URL with webkit.Client (ManagerOptions
// OpenURL). Its inventory keeps the frame contexts that its target
// reported from the connection's start, also before the scheduler
// existed, and binds them to that target: after the client follows a new
// target, a send of the old id is refused.
func TestExplicitURLClientRetainsFrameInventory(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Handle("Runtime.enable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		f.EmitFrom(c.Target, "Runtime.executionContextCreated", map[string]any{"context": map[string]any{"id": 17, "type": "normal", "frameId": "kid"}})
		return map[string]any{}, nil
	})
	ctx := ctxFor(t, 5*time.Second)
	c, err := webkit.NewClient(ctx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Send(ctx, "Runtime.enable", nil); err != nil {
		t.Fatal(err)
	}
	s := NewScheduler(c, SchedulerOptions{}) // after the report
	t.Cleanup(s.Close)
	ec := s.FrameContext("kid")
	if ec.ID != 17 || ec.target != "page-1" {
		t.Fatalf("frame context via explicit-URL Client = %+v; want reported context 17 of page-1", ec)
	}
	f.Swap("page-2", 0) // the client follows the new target
	eventually(t, time.Second, "the client to follow page-2", func() bool {
		target, _ := c.ContextInventory()
		return target == "page-2"
	})
	if got := s.FrameContext("kid"); got.ID != 0 {
		t.Fatalf("frame context of page-2 before it reported one = %+v, want none", got)
	}
	_, err = contextCaller{Caller: c, ec: ec}.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "release"})
	if !errors.Is(err, webkit.ErrTargetSwapped) || !errors.Is(err, webkit.ErrNotSent) {
		t.Fatalf("send of page-1's context after the switch: %v, want not sent with ErrTargetSwapped", err)
	}
	if got := evaluatesWith(f, 17); len(got) != 0 {
		t.Fatalf("context 17 sent to %q", got)
	}
}
