package fakewebkit

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// NoReply, returned by a RuntimeHandler, makes the fake drop the reply.
var NoReply = &struct{ noReply bool }{true}

// PageRuntime is a fake agent runtime that enforces the page side of design
// 2.6 and 3.1 on a fake page clock. An attempt dispatches only for the
// page's current document, only for the current owner epoch or a newer one
// (which becomes the owner, as the reset of 3.1 does), only while the page
// clock is strictly below both its attemptEnd and its deadline, only for
// the next step ordinal of its operation, and never for a canceled
// operation. A synchronous attempt settles with its reply; later events of
// its step run only while the attempt is open (KeepOpen). It never reads a
// wall clock.
type PageRuntime struct {
	mu sync.Mutex
	// Now is the page's performance.now(). Default: time since NewPageRuntime.
	Now func() time.Duration
	// Ready reports whether the target of an attempt is ready (default:
	// always). A false answer gives status "retry" with the reason.
	Ready func(opID string, step int) (bool, string)
	// IgnoreCancel drops every cancel call, as if the message were lost.
	IgnoreCancel bool
	// DropReplyOfStep drops the reply of the attempt that dispatches this
	// step, after the page ran it (0: none).
	DropReplyOfStep int
	// KeepOpen models an attempt whose step has events in separate tasks:
	// the call stays unsettled after its first event, TryEvent runs later
	// events while the attempt end, the deadline, the owner epoch and the
	// document allow it, and Settle ends it. Without KeepOpen the call
	// settles with its reply and TryEvent runs nothing.
	KeepOpen bool
	// WallClock is the page's Date.now(); the protocol never reads it.
	WallClock time.Time

	doc        string // the current document's id, as the runtime picks it at install
	docs       int
	owner      uint64 // owner epoch recorded at install; 0: no owner yet
	lastStep   map[string]int
	canceled   map[string]bool
	open       map[string]attemptArgs // unsettled attempts by operation (KeepOpen)
	calls      []RuntimeRecord
	dispatched []Dispatch
}

// RuntimeRecord is one runtime call the page received.
type RuntimeRecord struct {
	Fn      string
	Args    json.RawMessage
	At      time.Time     // host time of arrival
	PageNow time.Duration // page clock at arrival
}

// Dispatch is a step side effect that the page ran.
type Dispatch struct {
	OpID    string
	Step    int
	Event   int // 0: the attempt's first event; later events count up
	PageNow time.Duration
	At      time.Time
}

type attemptArgs struct {
	OpID       string  `json:"opId"`
	Epoch      uint64  `json:"epoch"`
	Doc        string  `json:"doc"`
	Step       int     `json:"step"`
	Steps      int     `json:"steps"`
	AttemptEnd float64 `json:"attemptEnd"`
	Deadline   float64 `json:"deadline"`
}

// NewPageRuntime returns a page runtime whose clock starts at zero, with
// document "doc-1".
func NewPageRuntime() *PageRuntime {
	start := time.Now()
	r := &PageRuntime{
		Now:      func() time.Duration { return time.Since(start) },
		lastStep: make(map[string]int),
		canceled: make(map[string]bool),
		open:     make(map[string]attemptArgs),
	}
	r.newDoc()
	return r
}

func (r *PageRuntime) newDoc() {
	r.docs++
	r.doc = fmt.Sprintf("doc-%d", r.docs)
	r.owner = 0
	r.lastStep = make(map[string]int)
	r.canceled = make(map[string]bool)
	r.open = make(map[string]attemptArgs)
}

// Navigate replaces the document: a new document id and a fresh runtime
// with no owner, steps, cancels or open attempts. The page clock is kept;
// tests that need a new time origin set Now.
func (r *PageRuntime) Navigate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.newDoc()
}

// Handler adapts the runtime to Server.SetRuntime.
func (r *PageRuntime) Handler() RuntimeHandler {
	return func(_ Call, fn string, args json.RawMessage) any {
		r.mu.Lock()
		now := r.Now()
		r.mu.Unlock()
		return r.HandleAt(now, fn, args)
	}
}

func ok(v any) map[string]any { return map[string]any{"ok": true, "value": v} }

func ms(d time.Duration) float64 { return float64(d) / 1e6 }

// HandleAt answers runtime call fn as the page would at page time now.
// Functions: "clock" returns {now, doc}; "cancel" marks {opId} canceled;
// every other function is an attempt with the fence of design 3.1.
func (r *PageRuntime) HandleAt(now time.Duration, fn string, raw json.RawMessage) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, RuntimeRecord{Fn: fn, Args: append(json.RawMessage(nil), raw...), At: time.Now(), PageNow: now})
	switch fn {
	case "clock":
		return ok(map[string]any{"now": ms(now), "doc": r.doc})
	case "cancel":
		var a struct {
			OpID string `json:"opId"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return map[string]any{"ok": false, "error": map[string]string{"code": "bad_args", "message": err.Error()}}
		}
		if !r.IgnoreCancel {
			r.canceled[a.OpID] = true
			delete(r.open, a.OpID)
		}
		return ok(nil)
	}
	var a attemptArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return map[string]any{"ok": false, "error": map[string]string{"code": "bad_args", "message": err.Error()}}
	}
	reply := func(status, reason string) any {
		return ok(map[string]any{"status": status, "reason": reason, "now": ms(now), "doc": r.doc})
	}
	switch {
	case r.canceled[a.OpID]:
		return reply("refused", "canceled")
	case a.Doc != r.doc:
		return reply("refused", "other document")
	case a.Epoch < r.owner:
		return reply("refused", "other epoch")
	}
	r.owner = a.Epoch // a newer owner resets the page state (3.1); op ids stay unique
	switch {
	case ms(now) >= a.AttemptEnd || ms(now) >= a.Deadline:
		return reply("expired", "")
	case a.Step != r.lastStep[a.OpID]+1:
		return reply("refused", "not the next step")
	}
	if r.Ready != nil {
		if ready, why := r.Ready(a.OpID, a.Step); !ready {
			return reply("retry", why)
		}
	}
	r.lastStep[a.OpID] = a.Step
	if r.KeepOpen {
		r.open[a.OpID] = a
	}
	r.dispatched = append(r.dispatched, Dispatch{OpID: a.OpID, Step: a.Step, PageNow: now, At: time.Now()})
	if a.Step == r.DropReplyOfStep {
		return NoReply
	}
	return reply("dispatched", "")
}

// TryEvent models a later event of the step that op's open attempt
// dispatched (events of one step run in separate tasks). It runs, and is
// recorded, only while the attempt is open (KeepOpen, not settled), the
// operation is not canceled, its epoch is still the owner, and the page
// clock is below the attempt's end and deadline.
func (r *PageRuntime) TryEvent(opID string, now time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, isOpen := r.open[opID]
	if !isOpen || r.canceled[opID] || a.Epoch < r.owner || ms(now) >= a.AttemptEnd || ms(now) >= a.Deadline {
		return false
	}
	n := 0
	for _, d := range r.dispatched {
		if d.OpID == opID && d.Step == a.Step {
			n++
		}
	}
	r.dispatched = append(r.dispatched, Dispatch{OpID: opID, Step: a.Step, Event: n, PageNow: now, At: time.Now()})
	return true
}

// Settle ends op's open attempt: its call has settled, so no later event
// of its step runs.
func (r *PageRuntime) Settle(opID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.open, opID)
}

// Calls returns the runtime calls received so far.
func (r *PageRuntime) Calls() []RuntimeRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RuntimeRecord(nil), r.calls...)
}

// CallsOf returns the received calls of fn.
func (r *PageRuntime) CallsOf(fn string) []RuntimeRecord {
	var out []RuntimeRecord
	for _, c := range r.Calls() {
		if c.Fn == fn {
			out = append(out, c)
		}
	}
	return out
}

// Dispatched returns the side effects the page ran.
func (r *PageRuntime) Dispatched() []Dispatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Dispatch(nil), r.dispatched...)
}
