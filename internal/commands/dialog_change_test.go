package commands

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
)

// W3b final round, leftover (b) (design 2.4, 2.7, 3.7): the dialog bridge
// sets its bootstrap part and takes Console as one desired-state change
// (DesiredState.Change). A provisional target whose setup already ran
// (it got the runtime's part, so its snapshot predates the dialog part)
// gets the dialog part and Console.enable before it commits.
func TestDialogBridgeReachesAProvisionalTarget(t *testing.T) {
	h := newDTHarness(t)
	const provisional = "page-2"
	commitAfter := 1500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sentTo := func(method string) []string {
		var out []string
		for _, r := range h.f.Received() {
			if r.Target == provisional && r.Method == method {
				out = append(out, string(r.Params))
			}
		}
		return out
	}
	err := h.m.Do(ctx, h.req, core.AcquireOptions{Session: "s1", Reservation: "default"}, "dialog test", core.ClassMutate, false, func(ctx context.Context, mo *core.ManagedOp) error {
		swapped := make(chan struct{})
		go func() { h.f.Swap(provisional, commitAfter); close(swapped) }()
		for end := time.Now().Add(3 * time.Second); len(sentTo("Page.setBootstrapScript")) == 0; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatal("the provisional target's setup sent no bootstrap script")
			}
		}
		if got := sentTo("Page.setBootstrapScript"); strings.Contains(got[0], "dialogs") {
			t.Fatalf("the setup already had the dialog part: %s", got[0])
		}
		ps := mo.Page.PS
		session, epoch := ps.Sched.Lease()
		w := &w3bPage{ps: ps, caller: mo.Op.Caller(), session: session, epoch: epoch, st: stateFor(ps, session, epoch), end: func() {}}
		if err := w.activateDialogs(ctx, "dismiss"); err != nil {
			return err
		}
		select {
		case <-swapped:
			t.Fatal("the provisional target committed before the change ended; raise commitAfter")
		default:
		}
		<-swapped
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	scripts := sentTo("Page.setBootstrapScript")
	var last struct{ Source string }
	_ = json.Unmarshal([]byte(scripts[len(scripts)-1]), &last)
	if !strings.Contains(last.Source, `__iwdp.call("dialogs", {policy: "dismiss"})`) {
		t.Fatalf("the provisional target's last bootstrap script (%d bytes) lacks the dialog part", len(last.Source))
	}
	if n := len(sentTo("Console.enable")); n == 0 {
		t.Fatal("the provisional target got no Console.enable for the dialog log")
	}
}
