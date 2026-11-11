package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/transport"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// WithReplyPosition gives the reply of a call through the page's
// connection the position that the page's ordered events get
// (SubscribeOrdered): the same connection value, the active target, and a
// Seq after the events the page sent before the reply and before those it
// sent after it (W5b-1 review r4, F10).
func TestReplyPositionAmongOrderedEvents(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wire, err := webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := webkit.NewSession(ctx, wire, webkit.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p := newPageConn(s, nil, trackedBackground(t), t.Logf)
	var rec orderedRec
	_, unsub := p.subscribeOrdered("Console.messageAdded", rec.handle)
	defer unsub()
	f.Handle("Page.getResourceTree", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		f.Emit("Console.messageAdded", "before")
		return map[string]any{"frameTree": map[string]any{}}, nil
	})
	var (
		reply  EventPosition
		target string
		n      int
	)
	rctx := WithReplyPosition(ctx, func(pos EventPosition, tg string, _ json.RawMessage) {
		reply, target = pos, tg
		n++
	})
	if _, err := p.Send(rctx, "Page.getResourceTree", nil); err != nil {
		t.Fatal(err)
	}
	f.Emit("Console.messageAdded", "after")
	deadline := time.Now().Add(3 * time.Second)
	for len(rec.events()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("ordered events %+v, want before and after", rec.events())
		}
		time.Sleep(5 * time.Millisecond)
	}
	ev := rec.events()
	if n != 1 || reply.Conn != webkit.Caller(s) || ev[0].pos.Conn != reply.Conn || ev[1].pos.Conn != reply.Conn {
		t.Fatalf("reply position %+v (hook ran %d times), events %+v: want one reply on the events' connection", reply, n, ev)
	}
	if ev[0].text != "before" || ev[0].pos.Seq >= reply.Seq || reply.Seq >= ev[1].pos.Seq {
		t.Fatalf("reply Seq %d, events %+v: want it after the event sent before the reply and before the one sent after", reply.Seq, ev)
	}
	if target != s.ActiveTarget() || reply.ActiveTarget != target {
		t.Fatalf("reply of %q at active target %q, want the session's active target %q", target, reply.ActiveTarget, s.ActiveTarget())
	}
}

// fakeSession is a Session on a fake WebKit page through Target routing,
// as every page transport delivers it (core/manager.go webkit.Dial).
func fakeSession(t *testing.T) (*fakewebkit.Server, *webkit.Session) {
	t.Helper()
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wire, err := webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := webkit.NewSession(ctx, wire, webkit.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return f, s
}

// streamLog records, in the order of their handlers, the ordered events
// (their text) and the reply handlers ("reply") of a page connection,
// with their positions.
type streamLog struct {
	mu   sync.Mutex
	got  []string
	seqs []uint64
	// block, when set, is called by the handler of the event with this
	// text before it records it.
	blockOn string
	block   func()
}

func (l *streamLog) event(pos EventPosition, _, _ string, params json.RawMessage) {
	var text string
	_ = json.Unmarshal(params, &text)
	if l.block != nil && text == l.blockOn {
		l.block()
	}
	l.add(text, pos.Seq)
}

func (l *streamLog) reply(pos EventPosition, _ string, _ json.RawMessage) { l.add("reply", pos.Seq) }

func (l *streamLog) add(what string, seq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, what)
	l.seqs = append(l.seqs, seq)
}

func (l *streamLog) items() ([]string, []uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.got...), append([]uint64(nil), l.seqs...)
}

// heldItems counts h's held read-loop items, and the reply handlers
// among them.
func heldItems(h *handoff) (all, replies int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.syncHeld {
		if e.reply != nil {
			replies++
		}
	}
	return len(h.syncHeld), replies
}

func ascending(seqs []uint64) bool {
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			return false
		}
	}
	return true
}

// The reply of a call on a prepared connection is held with that
// connection's events until the swap, in read order: its handler runs
// after the events the page sent before the reply and before the later
// ones (W5b-1 review r5, F11: the brief's "a reply hook that fires on a
// prepared connection before the swap must be queued in the same held
// queue as that connection's events, in read order").
func TestReplyOfPreparedConnectionHeldInReadOrder(t *testing.T) {
	first := newStandingCaller(t, "A")
	p := newPageConn(first, nil, trackedBackground(t), t.Logf)
	var log streamLog
	_, unsub := p.subscribeOrdered("Console.messageAdded", log.event)
	defer unsub()
	f, next := fakeSession(t)
	p.prepare(next)
	f.Handle("Page.getResourceTree", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		f.Emit("Console.messageAdded", "before")
		return map[string]any{"frameTree": map[string]any{}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rctx, _ := p.replyOrder(WithReplyPosition(ctx, log.reply))
	if _, err := next.Send(rctx, "Page.getResourceTree", nil); err != nil {
		t.Fatal(err)
	}
	f.Emit("Console.messageAdded", "after")
	p.mu.Lock()
	h := p.next
	p.mu.Unlock()
	eventually(t, 3*time.Second, "three held items", func() bool { n, _ := heldItems(h); return n == 3 })
	if got, _ := log.items(); len(got) != 0 {
		t.Fatalf("handled before the swap: %v, want nothing", got)
	}
	mustSwap(t, p, next)
	got, seqs := log.items()
	if !equalStrings(got, []string{"before", "reply", "after"}) || !ascending(seqs) {
		t.Fatalf("handled %v at %v when swap returned, want before, reply, after in read order", got, seqs)
	}
}

// A reply that the installed connection reads while swap still delivers
// its held events is held behind them: the call returns only after its
// handler ran, and the handler runs after every event read before the
// reply. The read loop does not wait for that delivery: it holds the
// reply and answers a later call (W1c-1: the read loop never blocks).
func TestReplyHeldWhileSwapDeliversHeldEvents(t *testing.T) {
	first := newStandingCaller(t, "A")
	p := newPageConn(first, nil, trackedBackground(t), t.Logf)
	blocked, release := make(chan struct{}), make(chan struct{})
	log := streamLog{blockOn: "e1", block: func() {
		close(blocked)
		<-release
	}}
	_, unsub := p.subscribeOrdered("Console.messageAdded", log.event)
	defer unsub()
	f, next := fakeSession(t)
	p.prepare(next)
	p.mu.Lock()
	h := p.next
	p.mu.Unlock()
	f.Emit("Console.messageAdded", "e1")
	f.Emit("Console.messageAdded", "e2")
	eventually(t, 3*time.Second, "two held events", func() bool { n, _ := heldItems(h); return n == 2 })
	swapped := make(chan error, 1)
	go func() { swapped <- p.swap(next) }()
	<-blocked // swap delivers e1; next is installed
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		err     error
		handled []string
	}
	sent := make(chan result, 1)
	go func() {
		_, err := p.Send(WithReplyPosition(ctx, log.reply), "Page.getResourceTree", nil)
		got, _ := log.items()
		sent <- result{err, got}
	}()
	eventually(t, 3*time.Second, "the reply held behind e2", func() bool { _, r := heldItems(h); return r == 1 })
	if _, err := next.Send(ctx, "Page.reload", nil); err != nil {
		t.Fatalf("a later call while the held events are delivered: %v", err)
	}
	close(release)
	if err := <-swapped; err != nil {
		t.Fatalf("swap: %v", err)
	}
	res := <-sent
	if res.err != nil {
		t.Fatal(res.err)
	}
	if !equalStrings(res.handled, []string{"e1", "e2", "reply"}) {
		t.Fatalf("handled %v when the call returned, want e1, e2, reply", res.handled)
	}
	got, seqs := log.items()
	if !equalStrings(got, []string{"e1", "e2", "reply"}) || !ascending(seqs) {
		t.Fatalf("handled %v at %v, want e1, e2, reply in read order", got, seqs)
	}
}

// Held reply handlers count like a read-loop subscription's events
// (W1c-1 handoff limit): a prepared connection holds at most handoffLimit
// of them; one more is dropped and counted, never run, and fails the
// install, as a lost read-loop event does. The read loop does not wait
// for the room: the extra one returns at once.
func TestHeldRepliesWithinHandoffLimit(t *testing.T) {
	var drops atomic.Int64
	for _, tc := range []struct {
		name       string
		n          int
		installErr bool
	}{
		{"at the limit", handoffLimit, false},
		{"one more", handoffLimit + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drops.Store(0)
			h := &handoff{c: newEmitCallerFor(t), drops: &drops, replies: &connSub{method: "reply", sync: true}}
			ran := 0
			for range tc.n {
				h.deliverReply(func() { ran++ })
			}
			if ran != 0 {
				t.Fatalf("%d reply handlers ran before the install, want them held", ran)
			}
			err := h.install()
			if (err != nil) != tc.installErr {
				t.Fatalf("install after %d held replies: %v, want an error %v", tc.n, err, tc.installErr)
			}
			if tc.installErr {
				if d := drops.Load(); d != 1 {
					t.Fatalf("dropped %d, want 1 counted", d)
				}
				return
			}
			h.openReadLoop()
			if ran != handoffLimit || drops.Load() != 0 {
				t.Fatalf("ran %d, dropped %d at the swap, want %d and 0", ran, drops.Load(), handoffLimit)
			}
			h.deliverReply(func() { ran++ })
			if ran != handoffLimit+1 {
				t.Fatalf("a reply after the delivery ran %d handlers in all, want it at once", ran)
			}
		})
	}
}

// A call whose reply handler is still held when the call's context ends
// fails with the context's cause instead of returning a reply whose
// handler has not run (WithReplyPosition: "a call whose context ends
// before it fails"); the handler still runs at the delivery.
func TestHeldReplyCallEndsWithItsContext(t *testing.T) {
	first := newStandingCaller(t, "A")
	p := newPageConn(first, nil, trackedBackground(t), t.Logf)
	blocked, release := make(chan struct{}), make(chan struct{})
	log := streamLog{blockOn: "e1", block: func() {
		close(blocked)
		<-release
	}}
	_, unsub := p.subscribeOrdered("Console.messageAdded", log.event)
	defer unsub()
	f, next := fakeSession(t)
	p.prepare(next)
	f.Emit("Console.messageAdded", "e1")
	p.mu.Lock()
	h := p.next
	p.mu.Unlock()
	eventually(t, 3*time.Second, "the held event", func() bool { n, _ := heldItems(h); return n == 1 })
	swapped := make(chan error, 1)
	go func() { swapped <- p.swap(next) }()
	<-blocked
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := p.Send(WithReplyPosition(ctx, log.reply), "Page.getResourceTree", nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "still held") {
		t.Fatalf("call whose handler is held past its deadline: %v, want the deadline", err)
	}
	close(release)
	if err := <-swapped; err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got, _ := log.items(); !equalStrings(got, []string{"e1", "reply"}) {
		t.Fatalf("handled %v, want e1 and then the held reply", got)
	}
}

// hooklessCaller answers every call without running reply hooks, as a
// caller that is not a webkit.Session does.
type hooklessCaller struct{ *emitCaller }

func (hooklessCaller) Send(context.Context, string, any) (json.RawMessage, error) {
	return json.RawMessage("{}"), nil
}

// On a connection that runs no reply hooks the call returns its reply at
// once, and the handler never runs (WithReplyPosition: h runs never for "a
// call that does not go through the page's connection" of a Session;
// replyOrder: "It returns at once when the hook did not run").
func TestReplyPositionWithoutHooks(t *testing.T) {
	c := hooklessCaller{newEmitCallerFor(t)}
	p := newPageConn(c, nil, trackedBackground(t), t.Logf)
	ran := false
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := p.Send(WithReplyPosition(ctx, func(EventPosition, string, json.RawMessage) { ran = true }), "Page.getResourceTree", nil)
	if err != nil || string(res) != "{}" || ran {
		t.Fatalf("reply %s, %v, handler ran %v; want the reply at once and no handler", res, err, ran)
	}
}
