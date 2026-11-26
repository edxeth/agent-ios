package commands

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// docs/sessions.md, "Lost pages and takeover": "tab close" on a tab that
// another inspector claimed (iwdp: close 1001 "Page ... claimed by
// remote"; the tab stays listed) fails with the lost-page error (exit 4,
// --takeover), as other commands do, and not with "still open after
// window.close()" (exit 1). A tab whose connection ends because it closed
// (iwdp: close 1001 "Page ... not found") still closes normally.
func TestTabCloseOfAClaimedTabReportsTheLostPage(t *testing.T) {
	for _, tc := range []struct {
		name string
		// closeOn is the fake's answer to window.close(): it ends the
		// connection and, for a closed tab, leaves the list.
		closeOn func(f *tabFake, s *fakewebkit.Server)
		before  bool // the claim came before tab close
		lost    bool
	}{
		{name: "claim during tab close", lost: true, closeOn: func(_ *tabFake, s *fakewebkit.Server) {
			s.CloseConnectionsWith(1001, "Page 9223/2 claimed by remote")
		}},
		{name: "claim before tab close", before: true, lost: true},
		{name: "the tab closes and its connection ends", closeOn: func(f *tabFake, s *fakewebkit.Server) {
			f.mu.Lock()
			f.pages = f.pages[:1]
			f.mu.Unlock()
			s.CloseConnectionsWith(1001, "Page 9223/2 not found")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newTabManager(t)
			f.add("One", "http://one.test/")
			mustIn(t, m, "a", "tab", "new", "--label", "docs", "http://docs.test/")
			mustIn(t, m, "a", "eval", "1") // opens the new tab's page connection
			f.mu.Lock()
			s := f.servers["page-2"]
			f.mu.Unlock()
			if tc.before {
				s.CloseConnectionsWith(1001, "Page 9223/2 claimed by remote")
			} else {
				s.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
					var p struct{ Expression string }
					_ = json.Unmarshal(c.Params, &p)
					if p.Expression == "window.close()" {
						tc.closeOn(f, s)
					}
					return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
				})
			}
			r, err := runIn(t, m, "a", "tab", "close", "docs")
			if !tc.lost {
				if err != nil || r.Text != "closed tab docs" {
					t.Fatalf("tab close docs = %q, %v; want the tab closed", r.Text, err)
				}
				return
			}
			if ExitCodeOf(err) != ExitBusy || !errors.Is(err, core.ErrLost) || !strings.Contains(err.Error(), "--takeover") || strings.Contains(err.Error(), "only tabs that a script opened") {
				t.Fatalf("tab close of a claimed tab = %v (exit %d), want the lost-page error (exit %d)", err, ExitCodeOf(err), ExitBusy)
			}
		})
	}
}
