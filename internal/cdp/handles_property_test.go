package cdp

import (
	"fmt"
	"testing"
	"testing/quick"
)

// handleObs is one observation of a runtime handle: the page answered
// handle H for a node of document Doc (design 5.2 "Node ids").
type handleObs struct {
	Doc, H uint8
}

// Design 5.2 "Node ids" and the W5b-2 brief: a CDP backendNodeId is a
// runtime handle number, and the same node keeps its number for its
// document. Over random sequences of observations from up to 12
// documents (more than the table keeps), handleTable.id
//   - gives the same number for the same (document, handle) while the
//     table keeps that document,
//   - never gives one number to two different (document, handle) pairs,
//     also after a document was dropped and came back,
//   - resolves each number to its own pair while the document is kept,
//     and to nothing after the table dropped the document.
//
// The table keeps the maxHandleDocs documents that it saw last for the
// first time (the contract in handles.go); the model below follows that
// contract, not the implementation.
func TestHandleNumbersProperty(t *testing.T) {
	prop := func(obs []handleObs) bool {
		tab := newHandleTable()
		var kept []string                // model: kept documents, oldest first
		owner := map[int64]handleKey{}   // every number ever given, and its pair
		current := map[handleKey]int64{} // model: the numbers of kept documents
		for i, o := range obs {
			doc := fmt.Sprintf("doc-%d", o.Doc%12)
			k := handleKey{doc: doc, h: int64(o.H%20) + 1}
			isKept := false
			for _, d := range kept {
				isKept = isKept || d == doc
			}
			if !isKept {
				if len(kept) == maxHandleDocs {
					gone := kept[0]
					kept = kept[1:]
					for ck := range current {
						if ck.doc == gone {
							delete(current, ck)
						}
					}
				}
				kept = append(kept, doc)
			}
			id := tab.id(k.doc, k.h)
			if id <= 0 {
				t.Logf("step %d: %v got number %d, want a positive number", i, k, id)
				return false
			}
			if prev, ok := current[k]; ok && prev != id {
				t.Logf("step %d: %v got number %d, earlier %d in the same kept document", i, k, id, prev)
				return false
			}
			if other, ok := owner[id]; ok && other != k {
				t.Logf("step %d: number %d given to %v and earlier to %v", i, id, k, other)
				return false
			}
			owner[id] = k
			current[k] = id
			for n, pair := range owner {
				got, found := tab.lookup(n)
				live := current[pair] == n
				if live && (!found || got != pair) {
					t.Logf("step %d: number %d resolves to %v (%v), want %v", i, n, got, found, pair)
					return false
				}
				if !live && found {
					t.Logf("step %d: number %d of a dropped document resolves to %v", i, n, got)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}
