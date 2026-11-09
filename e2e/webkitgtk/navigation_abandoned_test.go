//go:build webkitgtk

package webkitgtk

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// tapWait bounds the wait for one protocol event of the tap.
const tapWait = 15 * time.Second

// connCount returns how many page connections the daemon opened.
func (h *tapHooks) connCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// waitTap waits until the tap logs a record after the first from records
// that match accepts, and returns its index and the record.
func (e *navEnv) waitTap(from int, what string, match func(tapRecord) bool) (int, tapRecord) {
	e.t.Helper()
	for end := time.Now().Add(tapWait); ; time.Sleep(20 * time.Millisecond) {
		recs := e.tap.records()
		for i := from; i < len(recs); i++ {
			if match(recs[i]) {
				return i, recs[i]
			}
		}
		if time.Now().After(end) {
			e.t.Fatalf("the daemon's page connection saw no %s within %v", what, tapWait)
		}
	}
}

// lastTarget returns the target of the last command that the daemon sent
// in recs.
func lastTarget(recs []tapRecord) string {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Out && recs[i].Target != "" {
			return recs[i].Target
		}
	}
	return ""
}

// Proof P1, its second part (docs/notes/w1a.md; the proof was
// e2e/proofs/p1_test.go until W5c-2b part 2): a cross-site navigation that
// the server answers with 204 No Content. WebKit announces a provisional
// target in a new process for it and destroys that target without a
// commit, because a 204 leaves the current document in place. The daemon's
// page session drops the abandoned target (webkit.Session, design 2.7): no
// process swap, the same page connection, the next command goes to the
// target that was active before, and the document and its state stay. In
// both modes of pause on start; with it, the provisional target is paused
// at its creation and gets its setup first.
func TestAbandonedProvisionalTarget(t *testing.T) {
	for _, pause := range []bool{false, true} {
		t.Run(fmt.Sprintf("pauseOnStart=%v", pause), func(t *testing.T) {
			e := newNavEnv(t, "nav.html", pause)
			// open returns after load: WebKit swaps the process only for a
			// navigation from a process that committed a load (P1).
			start := e.sites.a + "/second.html?abandon"
			e.must(agentA, "open", start)
			e.js(agentA, "window.__stay = 'kept'; 1")
			from := len(e.tap.records())
			before, conns, swaps := lastTarget(e.tap.records()), e.tap.connCount(), e.swaps()
			if before == "" {
				t.Fatal("the daemon sent no command to a target before the navigation")
			}

			e.js(agentA, fmt.Sprintf("location.href = %q; 1", e.sites.b+"/nocontent"))
			var provisional string
			_, created := e.waitTap(from, "Target.targetCreated of a provisional target", func(r tapRecord) bool {
				var p struct {
					TargetInfo struct {
						TargetID      string `json:"targetId"`
						IsProvisional bool   `json:"isProvisional"`
					} `json:"targetInfo"`
				}
				if r.Method != "Target.targetCreated" || json.Unmarshal([]byte(r.Params), &p) != nil || !p.TargetInfo.IsProvisional {
					return false
				}
				provisional = p.TargetInfo.TargetID
				return true
			})
			destroyed, _ := e.waitTap(from, "Target.targetDestroyed of the provisional target "+provisional, func(r tapRecord) bool {
				return r.Method == "Target.targetDestroyed" && strings.Contains(r.Params, fmt.Sprintf("%q", provisional))
			})
			t.Logf("provisional target: %s; destroyed at record %d", created.Params, destroyed)

			if got, want := e.js(agentA, "location.href + ' ' + window.__stay"), fmt.Sprintf("%q", start+" kept"); got != want {
				t.Errorf("after the abandoned navigation the page is %s, want %s (the same document)", got, want)
			}
			recs := e.tap.records()
			if got := lastTarget(recs[destroyed:]); got != before {
				t.Errorf("the next command went to the target %q, want %s, the target that was active before the navigation", got, before)
			}
			if got := e.swaps(); got != swaps {
				t.Errorf("%d process swaps after the abandoned navigation, want %d (no commit)", got, swaps)
			}
			if got := e.tap.connCount(); got != conns {
				t.Errorf("the daemon opened %d page connections, want %d: it reconnected after the abandoned target", got, conns)
			}
		})
	}
}
