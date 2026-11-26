package refs

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

// RememberSnapshot keeps s as the last snapshot of session on this page.
func (t *Table) RememberSnapshot(session string, s LastSnapshot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last[session] = s
}

// LastSnapshot returns the last snapshot of session on this page; other
// sessions' snapshots are never returned.
func (t *Table) LastSnapshot(session string) (LastSnapshot, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.last[session]
	return s, ok
}
