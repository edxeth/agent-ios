package core

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// standingCaller is an emitCaller with an active target that the test
// moves, as a webkit.Session moves it in its read loop before the
// subscribers of an event run.
type standingCaller struct {
	*emitCaller
	amu    sync.Mutex
	active string
}

func newStandingCaller(t *testing.T, active string) *standingCaller {
	return &standingCaller{emitCaller: newEmitCallerFor(t), active: active}
}

func (c *standingCaller) setActive(t string) {
	c.amu.Lock()
	c.active = t
	c.amu.Unlock()
}

func (c *standingCaller) ActiveTarget() string {
	c.amu.Lock()
	defer c.amu.Unlock()
	return c.active
}

func (c *standingCaller) ExecutionContexts(string) []webkit.ExecutionContext { return nil }
func (c *standingCaller) AgentStates(string) map[string]webkit.AgentState    { return nil }

// emitFrom delivers one event of target to the read-loop subscribers.
func (c *standingCaller) emitFrom(target, text string) {
	c.mu.Lock()
	hs := make([]webkit.TargetEventHandler, 0, len(c.subs))
	for _, h := range c.subs {
		hs = append(hs, h)
	}
	c.mu.Unlock()
	for _, h := range hs {
		h(target, "Console.messageAdded", json.RawMessage(`"`+text+`"`))
	}
}

type orderedGot struct {
	pos          EventPosition
	target, text string
}

type orderedRec struct {
	mu  sync.Mutex
	got []orderedGot
}

func (r *orderedRec) handle(pos EventPosition, target, _ string, params json.RawMessage) {
	var text string
	_ = json.Unmarshal(params, &text)
	r.mu.Lock()
	r.got = append(r.got, orderedGot{pos, target, text})
	r.mu.Unlock()
}

func (r *orderedRec) events() []orderedGot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]orderedGot(nil), r.got...)
}

// An ordered subscription (Manager.SubscribeOrdered) runs in the read loop
// and gets each event with the delivering connection and its active
// target at that event; it returns the connection installed when it
// started (design 2.7; W5b-1 review F1).
func TestSubscribeOrderedPositionAtEvent(t *testing.T) {
	first := newStandingCaller(t, "A")
	p := newPageConn(first, nil, trackedBackground(t), t.Logf)
	var rec orderedRec
	initial, unsub := p.subscribeOrdered("Console.messageAdded", rec.handle)
	if initial != first {
		t.Fatalf("initial connection %v, want the installed one", initial)
	}
	first.emitFrom("B", "provisional")
	if got := rec.events(); len(got) != 1 {
		t.Fatalf("events after a read-loop delivery %v, want it handled in the read loop", got)
	}
	first.setActive("B")
	first.emitFrom("B", "committed")
	want := []orderedGot{
		{EventPosition{Conn: first, ActiveTarget: "A"}, "B", "provisional"},
		{EventPosition{Conn: first, ActiveTarget: "B"}, "B", "committed"},
	}
	if got := rec.events(); !equalOrdered(got, want) {
		t.Fatalf("ordered events %+v, want %+v", got, want)
	}
	unsub()
	first.emitFrom("B", "after")
	if got := rec.events(); len(got) != 2 {
		t.Fatalf("events after unsubscribe %+v", got)
	}
}

// The events of a prepared connection that wait for the swap keep the
// position at which that connection read them, not the one at the swap,
// and come before its later events (W1c-1 handoff; W5b-1 review F1).
func TestSubscribeOrderedKeepsPositionAcrossHandoff(t *testing.T) {
	first := newStandingCaller(t, "A")
	p := newPageConn(first, nil, trackedBackground(t), t.Logf)
	var rec orderedRec
	initial, unsub := p.subscribeOrdered("Console.messageAdded", rec.handle)
	defer unsub()
	next := newStandingCaller(t, "X")
	p.prepare(next)
	next.emitFrom("Y", "held-provisional")
	next.setActive("Y")
	next.emitFrom("Y", "held-committed")
	if got := rec.events(); len(got) != 0 {
		t.Fatalf("events before the swap %+v, want none", got)
	}
	mustSwap(t, p, next)
	next.emitFrom("Y", "live")
	want := []orderedGot{
		{EventPosition{Conn: next, ActiveTarget: "X"}, "Y", "held-provisional"},
		{EventPosition{Conn: next, ActiveTarget: "Y"}, "Y", "held-committed"},
		{EventPosition{Conn: next, ActiveTarget: "Y"}, "Y", "live"},
	}
	if got := rec.events(); !equalOrdered(got, want) {
		t.Fatalf("ordered events %+v, want %+v", got, want)
	}
	if initial != first {
		t.Fatalf("initial connection %v, want the first", initial)
	}
}

func equalOrdered(a, b []orderedGot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
