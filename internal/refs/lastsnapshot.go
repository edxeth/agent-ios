package refs

import (
	goruntime "runtime"
	"sync"
	"weak"
)

// SnapshotOptions are the options a snapshot was taken with (snapshot -s,
// -i, -c, -d, -u). "diff snapshot" without --baseline takes its fresh
// snapshot with the options of the last one and refuses options that
// differ (agent-browser 0.45 implicit baseline).
type SnapshotOptions struct {
	Selector    string // "" for the whole page
	Interactive bool
	Compact     bool
	URLs        bool
	Depth       int
	DepthSet    bool // Depth was given
}

// LastSnapshot is the last snapshot that a logical session took of a page:
// the implicit baseline of "diff snapshot".
type LastSnapshot struct {
	Text    string // the rendered snapshot, with its [ref=eN] annotations
	Options SnapshotOptions
	FrameID string // the frame scope it was taken in; "" for the top document
}

// lastSnapshots holds the last snapshot of each logical session of one
// table. It is kept beside the table, keyed weakly by it, so that it goes
// away with the table (the page record: a closed tab, a daemon restart).
type lastSnapshots struct {
	mu   sync.Mutex
	last map[string]LastSnapshot // by logical session
}

var (
	lastMu  sync.Mutex
	lastFor = map[weak.Pointer[Table]]*lastSnapshots{}
)

func (t *Table) lastSnapshots(create bool) *lastSnapshots {
	lastMu.Lock()
	defer lastMu.Unlock()
	k := weak.Make(t)
	l := lastFor[k]
	if l == nil && create {
		l = &lastSnapshots{last: map[string]LastSnapshot{}}
		lastFor[k] = l
		goruntime.AddCleanup(t, dropLastSnapshots, k)
	}
	return l
}

func dropLastSnapshots(k weak.Pointer[Table]) {
	lastMu.Lock()
	defer lastMu.Unlock()
	delete(lastFor, k)
}

// RememberSnapshot keeps s as the last snapshot of session on this page.
func (t *Table) RememberSnapshot(session string, s LastSnapshot) {
	l := t.lastSnapshots(true)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last[session] = s
}

// LastSnapshot returns the last snapshot of session on this page; other
// sessions' snapshots are never returned.
func (t *Table) LastSnapshot(session string) (LastSnapshot, bool) {
	l := t.lastSnapshots(false)
	if l == nil {
		return LastSnapshot{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.last[session]
	return s, ok
}
