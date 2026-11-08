package cdp

import (
	goruntime "runtime"
	"sync"
	"weak"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
)

// Node identity (design docs/design/agentic-parity.md 5.2 "Node ids"):
//   - A CDP nodeId is the WIP nodeId of the page session's connection; the
//     DOM domain passes it through unchanged.
//   - A CDP backendNodeId is a number of this file. The agent runtime
//     gives each node that it hands out a handle that stays the same for
//     the life of its document (internal/runtime/js/65-cdp.js handle), and
//     the page record's handle table maps each pair (document id, handle)
//     to one number. So a node keeps its number in its document, two nodes
//     never share a number (also across documents, whose handles restart
//     at 1), and the number of a dropped document resolves to nothing. An
//     AX node's backendDOMNodeId is the same number, so a client resolves
//     it with DOM.resolveNode{backendNodeId} in one runtime call, without a
//     DOM.requestNode round trip.

// maxHandleDocs is how many documents a page record's table keeps: the
// ones it saw last for the first time. A number of an older document no
// longer resolves (its page navigated away long ago), and the table of a
// page that sees many documents stays bounded.
const maxHandleDocs = 8

// handleKey is a runtime handle in its document.
type handleKey struct {
	doc string // the runtime's document id (10-core.js DOC)
	h   int64  // the handle, 1 and up within the document
}

// handleTable maps the runtime handles of one page record to
// backendNodeIds. Numbers start at 1 and are never given twice.
type handleTable struct {
	mu    sync.Mutex
	next  int64
	ids   map[handleKey]int64
	keys  map[int64]handleKey
	byDoc map[string][]int64
	docs  []string // kept documents, oldest first
}

func newHandleTable() *handleTable {
	return &handleTable{ids: map[handleKey]int64{}, keys: map[int64]handleKey{}, byDoc: map[string][]int64{}}
}

// id returns the backendNodeId of handle h of document doc, and gives the
// pair a new number when it has none. A document that the table does not
// keep becomes the newest kept one; the oldest is dropped when the table
// keeps maxHandleDocs already.
func (t *handleTable) id(doc string, h int64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := handleKey{doc: doc, h: h}
	if n, ok := t.ids[k]; ok {
		return n
	}
	if _, kept := t.byDoc[doc]; !kept {
		if len(t.docs) == maxHandleDocs {
			gone := t.docs[0]
			t.docs = t.docs[1:]
			for _, n := range t.byDoc[gone] {
				delete(t.ids, t.keys[n])
				delete(t.keys, n)
			}
			delete(t.byDoc, gone)
		}
		t.docs = append(t.docs, doc)
		t.byDoc[doc] = nil
	}
	t.next++
	t.ids[k] = t.next
	t.keys[t.next] = k
	t.byDoc[doc] = append(t.byDoc[doc], t.next)
	return t.next
}

// lookup returns the runtime handle of a backendNodeId; ok is false for a
// number that the table never gave or whose document it dropped.
func (t *handleTable) lookup(id int64) (k handleKey, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k, ok = t.keys[id]
	return k, ok
}

// The handle tables by page record. A record is held weakly: when the
// daemon drops it, a cleanup drops its table (the pattern of
// internal/commands/w3b_page.go).
var handleTables = struct {
	sync.Mutex
	m map[weak.Pointer[core.Page]]*handleTable
}{m: map[weak.Pointer[core.Page]]*handleTable{}}

// handlesOf returns the handle table of page record pg. Every CDP
// connection and attachment on pg shares it, so a backendNodeId means the
// same node to all of them.
func handlesOf(pg *core.Page) *handleTable {
	handleTables.Lock()
	defer handleTables.Unlock()
	k := weak.Make(pg)
	t := handleTables.m[k]
	if t == nil {
		t = newHandleTable()
		handleTables.m[k] = t
		goruntime.AddCleanup(pg, dropHandles, k)
	}
	return t
}

func dropHandles(k weak.Pointer[core.Page]) {
	handleTables.Lock()
	defer handleTables.Unlock()
	delete(handleTables.m, k)
}
