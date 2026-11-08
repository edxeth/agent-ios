package webkit

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// A call made with WithReplyHook runs its hook in the read loop at the
// place of its reply in the stream (ReplyHook): after the events that the
// page sent before the reply, before the later ones, with the reply's own
// read position, the session, the reply's target and its result. An error
// reply runs no hook (W5b-1 review r4, F10: a CDP attachment applies a
// frame tree at that place).
func TestReplyHookAtReplyPosition(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := newFakeSession(t, f, SessionOptions{})
	type mark struct {
		what string
		pos  uint64
	}
	var (
		mu   sync.Mutex
		seen []mark
	)
	note := func(what string) {
		mu.Lock()
		seen = append(seen, mark{what, s.ReadPosition()})
		mu.Unlock()
	}
	marks := func() []mark {
		mu.Lock()
		defer mu.Unlock()
		return append([]mark(nil), seen...)
	}
	s.SubscribeTargetSync(AllEvents, func(_, method string, _ json.RawMessage) { note(method) })
	f.Handle("Test.read", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		f.Emit("Test.before", nil) // sent before the reply
		return map[string]int{"v": 1}, nil
	})
	f.Handle("Test.fail", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "refused"}
	})
	var (
		conn   Caller
		target string
		result json.RawMessage
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = WithReplyHook(ctx, func(c Caller, tg string, r json.RawMessage) {
		conn, target, result = c, tg, r
		note("reply")
	})
	if _, err := s.Send(ctx, "Test.read", nil); err != nil {
		t.Fatal(err)
	}
	f.Emit("Test.after", nil)
	if _, err := s.Send(ctx, "Test.fail", nil); err == nil {
		t.Fatal("Test.fail succeeded")
	}
	f.Emit("Test.end", nil)
	waitFor(t, func() bool { m := marks(); return len(m) > 0 && m[len(m)-1].what == "Test.end" })
	m := marks()
	if len(m) != 4 || m[0].what != "Test.before" || m[1].what != "reply" || m[2].what != "Test.after" {
		t.Fatalf("seen %v, want Test.before, the reply's hook once, Test.after, Test.end", m)
	}
	for i := 1; i < len(m); i++ {
		if m[i].pos <= m[i-1].pos {
			t.Fatalf("read positions %v, want them growing in stream order", m)
		}
	}
	if conn != Caller(s) || target != s.ActiveTarget() || string(result) != `{"v":1}` {
		t.Fatalf("hook got connection %v, target %q, result %s; want the session, its active target and the result", conn, target, result)
	}
}
