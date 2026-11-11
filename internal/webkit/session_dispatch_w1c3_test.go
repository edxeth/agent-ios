package webkit

import (
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// dispatchLog records, per event number, the handlers that saw the event,
// in the order they ran.
type dispatchLog struct {
	mu   sync.Mutex
	runs map[int][]string
	seen map[string][]int // queued subscriber -> event numbers in handling order
}

func (l *dispatchLog) add(n int, who string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs[n] = append(l.runs[n], who)
}

func eventNumber(t *testing.T, params json.RawMessage) int {
	var p struct{ N int }
	if err := json.Unmarshal(params, &p); err != nil {
		t.Errorf("event params %s: %v", params, err)
	}
	return p.N
}

// C09 (W1c-3): the dispatch order that Subscribe documents. Read-loop
// handlers of the event's method run before read-loop handlers of
// AllEvents, whatever the order of registration; every read-loop handler of
// an event returns before any queued subscriber handles it; each queued
// subscriber handles events in arrival order. The interceptor of
// internal/devtools depends on the first rule.
func TestSessionDispatchOrderIsTheDocumentedContract(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := newFakeSession(t, f, SessionOptions{})
	l := &dispatchLog{runs: map[int][]string{}, seen: map[string][]int{}}
	const method, events = "Test.ordered", 30
	syncHandler := func(who string) TargetEventHandler {
		return func(_, _ string, p json.RawMessage) {
			// A slow handler widens the window in which a queued
			// subscriber or another handler could run out of order.
			time.Sleep(time.Millisecond)
			l.add(eventNumber(t, p), who)
		}
	}
	queued := func(who string) TargetEventHandler {
		return func(_, m string, p json.RawMessage) {
			if m != method {
				return
			}
			n := eventNumber(t, p)
			l.add(n, who)
			l.mu.Lock()
			l.seen[who] = append(l.seen[who], n)
			l.mu.Unlock()
		}
	}
	// AllEvents handlers are registered first: registration order must
	// not decide the dispatch order.
	s.SubscribeTarget(AllEvents, queued("queued-all"))
	s.SubscribeTargetSync(AllEvents, syncHandler("sync-all-1"))
	s.SubscribeTargetSync(AllEvents, syncHandler("sync-all-2"))
	s.SubscribeTarget(method, queued("queued-method"))
	s.SubscribeTargetSync(method, syncHandler("sync-method-1"))
	s.SubscribeTargetSync(method, syncHandler("sync-method-2"))

	for n := 1; n <= events; n++ {
		f.Emit(method, map[string]int{"n": n})
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.mu.Lock()
		done := len(l.seen["queued-all"]) == events && len(l.seen["queued-method"]) == events
		l.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the queued subscribers did not handle %d events within 5 s", events)
		}
		time.Sleep(5 * time.Millisecond)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	want := make([]int, events)
	for i := range want {
		want[i] = i + 1
	}
	for _, who := range []string{"queued-all", "queued-method"} {
		if !slices.Equal(l.seen[who], want) {
			t.Errorf("%s handled events %v, want arrival order %v", who, l.seen[who], want)
		}
	}
	rank := map[string]int{
		"sync-method-1": 0, "sync-method-2": 0,
		"sync-all-1": 1, "sync-all-2": 1,
		"queued-method": 2, "queued-all": 2,
	}
	for n := 1; n <= events; n++ {
		runs := l.runs[n]
		if len(runs) != len(rank) {
			t.Errorf("event %d reached %v, want each of the %d handlers once", n, runs, len(rank))
			continue
		}
		for i := 1; i < len(runs); i++ {
			if rank[runs[i]] < rank[runs[i-1]] {
				t.Errorf("event %d: handlers ran in the order %v; want the read-loop handlers of %s, then those of AllEvents, then the queued subscribers", n, runs, method)
				break
			}
		}
	}
}

// C09 (W1c-3), rule 1 of the dispatch order: a read-loop handler of a
// commit and of a main-frame navigation already sees the session's new
// active target and document generation.
func TestSessionUpdatesItsRecordBeforeDispatch(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := newFakeSession(t, f, SessionOptions{})
	type seen struct {
		active string
		gen    uint64
	}
	got := make(chan seen, 4)
	for _, key := range []string{"Target.didCommitProvisionalTarget", AllEvents} {
		s.SubscribeTargetSync(key, func(_, m string, _ json.RawMessage) {
			if m == "Target.didCommitProvisionalTarget" {
				got <- seen{s.ActiveTarget(), s.Generation()}
			}
		})
	}
	navigated := make(chan uint64, 1)
	s.SubscribeTargetSync("Page.frameNavigated", func(string, string, json.RawMessage) { navigated <- s.Generation() })
	gen := s.Generation()
	f.Swap("page-2", 0)
	for i := range 2 {
		select {
		case v := <-got:
			if v.active != "page-2" || v.gen != gen+1 {
				t.Fatalf("read-loop handler %d of the commit saw active target %q, generation %d; want page-2, %d", i, v.active, v.gen, gen+1)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no read-loop handler saw the commit within 5 s")
		}
	}
	// A main-frame navigation to a new loader is a new document.
	f.Emit("Page.frameNavigated", map[string]any{"frame": map[string]string{"id": "main", "loaderId": "L-first"}})
	f.Emit("Page.frameNavigated", map[string]any{"frame": map[string]string{"id": "main", "loaderId": "L-second"}})
	var gens []uint64
	for range 2 {
		select {
		case g := <-navigated:
			gens = append(gens, g)
		case <-time.After(5 * time.Second):
			t.Fatal("the read-loop handler did not see Page.frameNavigated within 5 s")
		}
	}
	if want := []uint64{gen + 1, gen + 2}; !slices.Equal(gens, want) {
		t.Fatalf("generations seen by the Page.frameNavigated handler = %v, want %v (the commit's document, then a new one)", gens, want)
	}
}
