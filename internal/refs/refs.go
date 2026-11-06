// Package refs holds element refs such as @e7 (design
// docs/design/agentic-parity.md, section 3.3). W1a provides the counter
// contract; W3a adds resolution and staleness.
package refs

import (
	"fmt"
	"sync"
)

// Table is the ref counter of one logical session and page. Ref ids are
// never reused inside a session: the counter only moves forward, also
// across navigations and resets of the page maps. Allocation happens only
// inside a mutate operation, so two snapshots never get the same range.
type Table struct {
	mu   sync.Mutex
	next int
}

// NewTable returns a table whose first ref is e1.
func NewTable() *Table { return &Table{next: 1} }

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
