// Package refs holds element refs such as @e7 (design
// docs/design/agentic-parity.md, section 3.3): the serialized ref
// allocator of one logical session and page, the table that resolves a ref
// to the snapshot that produced it, and the last snapshot of each logical
// session, the baseline of "diff snapshot".
package refs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Entry is what the table knows about one ref (design 3.3 "Daemon table").
type Entry struct {
	Ref        int    // the number of @eN
	Generation uint64 // document generation of the snapshot that produced it
	Doc        string // the runtime's document id of that snapshot
	Role, Name string
	Unique     bool // the role and name were unique in that snapshot
}

// Allocation is the outcome of one page call that hands out refs.
type Allocation struct {
	Next       int // the page's counter after the call; 0: unknown
	Generation uint64
	Doc        string
	Refs       []Entry // Generation and Doc are filled from the allocation
}

// Error is a ref that cannot be resolved; Code is "unknown_ref" or
// "stale_ref" (design 3.1 error codes).
type Error struct {
	Code    string
	Ref     int
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Reset reasons reported by the page runtime (design 3.1 owner epoch).
const (
	ResetDaemon  = "daemon"  // another daemon owned the page: the daemon restarted
	ResetSession = "session" // another logical session owned the page
	ResetEpoch   = "epoch"   // the same session under an older lease epoch
)

// Table is the ref allocator and table of one logical session and page.
// Ref ids are never reused inside a session: the counter only moves
// forward, also across navigations and resets of the page maps.
// Allocation is serialized, so two snapshots never get the same range,
// even when they run at the same time.
type Table struct {
	alloc chan struct{} // one token: the allocation in progress

	mu      sync.Mutex
	next    int
	entries map[int]Entry
	// Refs up to these numbers were handed out by an earlier owner of the
	// page and were dropped by a reset (design 3.1).
	daemonBelow, epochBelow int
	// last holds the last snapshot of each logical session
	// (lastsnapshot.go). The daemon keeps one table per page record, used
	// by every lease owner of the page in turn, and "diff snapshot"
	// compares only with the snapshot of its own session. It grows with the
	// sessions that took a snapshot on the page and goes away with the
	// table (a closed tab, a daemon restart).
	last map[string]LastSnapshot
}

// NewTable returns a table whose first ref is e1.
func NewTable() *Table {
	return &Table{alloc: make(chan struct{}, 1), next: 1, entries: map[int]Entry{}, last: map[string]LastSnapshot{}}
}

// ParseRef parses "@e5", "e5" or "ref=e5" (agent-browser forms) and returns 5.
func ParseRef(s string) (int, bool) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "ref="), "@")
	if len(s) < 2 || s[0] != 'e' {
		return 0, false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s[1:])
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// Format returns the page form of ref n, "e5".
func Format(n int) string { return "e" + strconv.Itoa(n) }

// Next returns the number of the next unallocated ref.
func (t *Table) Next() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.next
}

// Advance records that the page allocated refs up to (not including) next.
func (t *Table) Advance(next int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if next < t.next {
		return fmt.Errorf("ref counter cannot move back from %d to %d", t.next, next)
	}
	t.next = next
	return nil
}

// Allocate runs fn, the page call that hands out refs from next on, while
// no other allocation of this table runs. It records the refs fn reports
// and moves the counter to the page's counter. When fn fails after the page
// may have handed out refs, a reported counter still moves the table
// forward, so those ids are never handed out again.
func (t *Table) Allocate(ctx context.Context, fn func(next int) (Allocation, error)) error {
	select {
	case t.alloc <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("waiting for another snapshot of this session: %w", ctx.Err())
	}
	defer func() { <-t.alloc }()
	a, err := fn(t.Next())
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next = max(t.next, a.Next)
	if err != nil {
		return err
	}
	for _, e := range a.Refs {
		e.Generation, e.Doc = a.Generation, a.Doc
		t.entries[e.Ref] = e
		t.next = max(t.next, e.Ref+1)
	}
	return nil
}

// Lookup returns the entry of ref n for a page at document generation gen,
// in the resolve order of design 3.3: (1) a ref the table does not know is
// unknown_ref; (2) a ref from an older generation is stale_ref. The page
// does steps 3 to 5.
func (t *Table) Lookup(n int, gen uint64) (Entry, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[n]
	ref := "@" + Format(n)
	switch {
	case !ok && n <= t.daemonBelow:
		return Entry{}, &Error{Code: "unknown_ref", Ref: n, Message: ref + ": daemon restarted; run snapshot"}
	case !ok && n <= t.epochBelow:
		return Entry{}, &Error{Code: "unknown_ref", Ref: n, Message: ref + ": the lease of this page changed since the snapshot that produced it; run snapshot"}
	case !ok:
		return Entry{}, &Error{Code: "unknown_ref", Ref: n, Message: "no snapshot produced " + ref}
	case e.Generation < gen:
		return Entry{}, &Error{Code: "stale_ref", Ref: n, Message: "Ref " + ref + " is from a document that is no longer loaded (the page navigated or the frame was removed). Take a new snapshot and use a fresh ref."}
	}
	return e, nil
}

// NoteReset records that the page runtime reset its ref maps for a new
// owner (design 3.1). reason is ResetDaemon, ResetSession or ResetEpoch;
// maxRef is the highest ref the page had handed out. The table forgets its
// entries and keeps counting after maxRef, so no id is reused.
func (t *Table) NoteReset(reason string, maxRef int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = map[int]Entry{}
	t.next = max(t.next, maxRef+1)
	switch reason {
	case ResetDaemon:
		t.daemonBelow = max(t.daemonBelow, maxRef)
	case ResetEpoch:
		t.epochBelow = max(t.epochBelow, maxRef)
	}
}
