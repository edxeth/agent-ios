package refs

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"
)

func TestParseRefAcceptsAgentBrowserForms(t *testing.T) {
	for in, want := range map[string]int{"@e5": 5, "e12": 12, "ref=e7": 7, "@e1": 1} {
		if n, ok := ParseRef(in); !ok || n != want {
			t.Errorf("ParseRef(%q) = %d, %v; want %d", in, n, ok, want)
		}
	}
	for _, in := range []string{"#e5", "@e", "@e0", "e-1", "button", "@e5x", "text=e5"} {
		if n, ok := ParseRef(in); ok {
			t.Errorf("ParseRef(%q) = %d, accepted a non-ref", in, n)
		}
	}
}

func alloc(t *testing.T, tb *Table, gen uint64, doc string, n int) []int {
	t.Helper()
	var got []int
	err := tb.Allocate(context.Background(), func(next int) (Allocation, error) {
		a := Allocation{Generation: gen, Doc: doc, Next: next + n}
		for i := 0; i < n; i++ {
			a.Refs = append(a.Refs, Entry{Ref: next + i, Role: "button", Name: "b", Unique: true})
			got = append(got, next+i)
		}
		return a, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Design 3.3: resolve order (1) unknown, (2) generation older than the
// current one.
func TestLookupOrder(t *testing.T) {
	tb := NewTable()
	refs := alloc(t, tb, 1, "d1", 2)
	e, err := tb.Lookup(refs[1], 1)
	if err != nil || e.Doc != "d1" || e.Role != "button" || !e.Unique {
		t.Fatalf("Lookup(e2) = %+v, %v", e, err)
	}
	var re *Error
	if _, err := tb.Lookup(9, 1); !errors.As(err, &re) || re.Code != "unknown_ref" || !strings.Contains(err.Error(), "no snapshot produced @e9") {
		t.Fatalf("unknown ref gave %v", err)
	}
	if _, err := tb.Lookup(refs[0], 2); !errors.As(err, &re) || re.Code != "stale_ref" ||
		!strings.Contains(err.Error(), "Ref @e1 is from a document that is no longer loaded (the page navigated or the frame was removed). Take a new snapshot and use a fresh ref.") {
		t.Fatalf("old generation gave %v", err)
	}
}

// Design 3.1 and 7.5 test 5: a reset by another daemon makes old refs fail
// with "daemon restarted; run snapshot", and the counter moves past every
// ref the page handed out, so no id is reused.
func TestResetByAnotherDaemon(t *testing.T) {
	tb := NewTable() // the restarted daemon's table
	tb.NoteReset("daemon", 7)
	var re *Error
	if _, err := tb.Lookup(5, 1); !errors.As(err, &re) || re.Code != "unknown_ref" || !strings.Contains(err.Error(), "@e5: daemon restarted; run snapshot") {
		t.Fatalf("old ref after daemon restart gave %v", err)
	}
	if got := alloc(t, tb, 1, "d1", 1); got[0] != 8 {
		t.Fatalf("first ref after the reset = e%d, want e8", got[0])
	}
	if _, err := tb.Lookup(12, 1); err == nil || !strings.Contains(err.Error(), "no snapshot produced @e12") {
		t.Fatalf("a never-produced ref gave %v", err)
	}
}

// Design 3.3: a session or epoch change resets the page maps; the table
// forgets its entries and keeps counting forward.
func TestResetByAnotherOwnerForgetsEntries(t *testing.T) {
	tb := NewTable()
	alloc(t, tb, 1, "d1", 3)
	tb.NoteReset("session", 3)
	var re *Error
	if _, err := tb.Lookup(2, 1); !errors.As(err, &re) || re.Code != "unknown_ref" || !strings.Contains(err.Error(), "no snapshot produced @e2") {
		t.Fatalf("ref of the previous owner gave %v", err)
	}
	tb.NoteReset("epoch", 3)
	if _, err := tb.Lookup(3, 1); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("ref after an epoch change gave %v", err)
	}
	if got := alloc(t, tb, 1, "d1", 1); got[0] != 4 {
		t.Fatalf("next ref = e%d, want e4", got[0])
	}
}

// 7.5 test 6: concurrent allocations are serialized and get disjoint ranges.
func TestAllocateSerializesConcurrentSnapshots(t *testing.T) {
	tb := NewTable()
	var mu sync.Mutex
	var all []int
	var inside, maxInside int
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := tb.Allocate(context.Background(), func(next int) (Allocation, error) {
				mu.Lock()
				inside++
				maxInside = max(maxInside, inside)
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				a := Allocation{Generation: 1, Doc: "d", Next: next + 3}
				for i := 0; i < 3; i++ {
					a.Refs = append(a.Refs, Entry{Ref: next + i})
				}
				mu.Lock()
				inside--
				for i := 0; i < 3; i++ {
					all = append(all, next+i)
				}
				mu.Unlock()
				return a, nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("%d allocations ran at once, want 1", maxInside)
	}
	sort.Ints(all)
	for i, n := range all {
		if n != i+1 {
			t.Fatalf("refs %v are not one disjoint range from e1", all)
		}
	}
}

func TestAllocateHonorsContextWhileWaiting(t *testing.T) {
	tb := NewTable()
	hold := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = tb.Allocate(context.Background(), func(next int) (Allocation, error) {
			close(started)
			<-hold
			return Allocation{Next: next}, nil
		})
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	ran := false
	err := tb.Allocate(ctx, func(next int) (Allocation, error) { ran = true; return Allocation{Next: next}, nil })
	close(hold)
	if !errors.Is(err, context.DeadlineExceeded) || ran {
		t.Fatalf("waiting allocation: err %v, ran %v", err, ran)
	}
}

// A failed page call that still reports a counter moves the table forward:
// the page may have handed out those refs.
func TestAllocateKeepsCounterOfFailedCall(t *testing.T) {
	tb := NewTable()
	boom := errors.New("boom")
	err := tb.Allocate(context.Background(), func(next int) (Allocation, error) { return Allocation{Next: next + 4}, boom })
	if !errors.Is(err, boom) || tb.Next() != 5 {
		t.Fatalf("err %v, next %d; want boom and 5", err, tb.Next())
	}
}

// Property (design 3.3, "Invalidated IDs are never recycled"): over any
// sequence of allocations, page-side counter jumps and resets, every ref the
// table hands out is new, and Next never moves back.
func TestRefIDsNeverRepeatProperty(t *testing.T) {
	f := func(ops []uint8) bool {
		tb := NewTable()
		seen := map[int]bool{}
		maxPage := 0
		for _, op := range ops {
			before := tb.Next()
			switch op % 4 {
			case 0, 1: // snapshot: the page allocates from max(next, its own max+1)
				n := int(op%5) + 1
				_ = tb.Allocate(context.Background(), func(next int) (Allocation, error) {
					start := max(next, maxPage+1)
					a := Allocation{Next: start + n}
					for i := 0; i < n; i++ {
						if seen[start+i] {
							return a, errors.New("reused")
						}
						seen[start+i] = true
						a.Refs = append(a.Refs, Entry{Ref: start + i})
					}
					maxPage = start + n - 1
					return a, nil
				})
			case 2:
				tb.NoteReset([]string{"daemon", "session", "epoch"}[int(op)%3], maxPage)
			case 3:
				_ = tb.Advance(tb.Next() + int(op%3))
			}
			if tb.Next() < before || tb.Next() <= maxPage {
				return false
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

// diff snapshot (4.2) compares the last two snapshots of the session.
func TestSnapshotHistoryKeepsLastTwo(t *testing.T) {
	tb := NewTable()
	if _, _, n := tb.Snapshots("s1"); n != 0 {
		t.Fatalf("history of a new table has %d entries", n)
	}
	tb.RecordSnapshot("s1", "a")
	tb.RecordSnapshot("s1", "b")
	tb.RecordSnapshot("s1", "c")
	if prev, cur, n := tb.Snapshots("s1"); prev != "b" || cur != "c" || n != 2 {
		t.Fatalf("Snapshots = %q, %q, %d", prev, cur, n)
	}
}

// The daemon keeps one table per page for every lease owner in turn;
// "diff snapshot" compares the last two snapshots of this session (4.2),
// so each session sees only its own history.
func TestSnapshotHistoryIsPerSession(t *testing.T) {
	tb := NewTable()
	tb.RecordSnapshot("a", "a1")
	tb.RecordSnapshot("a", "a2")
	if prev, cur, n := tb.Snapshots("b"); n != 0 || prev != "" || cur != "" {
		t.Fatalf("Snapshots(b) after two snapshots of a = %q, %q, %d; want none", prev, cur, n)
	}
	tb.RecordSnapshot("b", "b1")
	if prev, cur, n := tb.Snapshots("b"); prev != "" || cur != "b1" || n != 1 {
		t.Fatalf("Snapshots(b) = %q, %q, %d; want only b1", prev, cur, n)
	}
	if prev, cur, n := tb.Snapshots("a"); prev != "a1" || cur != "a2" || n != 2 {
		t.Fatalf("Snapshots(a) after b's snapshot = %q, %q, %d; want a1, a2", prev, cur, n)
	}
}
