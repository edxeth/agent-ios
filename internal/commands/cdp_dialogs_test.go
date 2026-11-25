package commands

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp"
)

// Design 5.2 "Dialogs" and 3.7: CDPDialogs is the logical session's own
// dialog bridge. Before activates the session's override ("dialog status"
// then says it is on) and returns the session's policy: the default
// dismiss (decision D6), then accept after "dialog default accept" in the
// CLI. After records the answered dialogs in the session's log, which
// "dialog status" lists; another session's log does not get them.
func TestCDPDialogsIsTheSessionBridge(t *testing.T) {
	h := newActionHarness(t)
	session, epoch := h.ps.Sched.Lease()
	p := cdp.DialogPage{PS: h.ps, Session: session, Epoch: epoch, Caller: h.ps.Caller, Runtime: h.ps.Runtime}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	policy, err := CDPDialogs.Before(ctx, p)
	if err != nil || policy != "dismiss" {
		t.Fatalf("Before = %q, %v; want the default dismiss", policy, err)
	}
	if out := h.mustRun(t, "dialog", "status"); !strings.Contains(out, "dialog override: on") {
		t.Fatalf("dialog status after a CDP event:\n%s\nwant the override on", out)
	}
	h.mustRun(t, "dialog", "default", "accept")
	if policy, err = CDPDialogs.Before(ctx, p); err != nil || policy != "accept" {
		t.Fatalf("Before after dialog default accept = %q, %v", policy, err)
	}
	CDPDialogs.After(p, []cdp.DialogUse{{Type: "confirm", Message: "Sure?", Result: true}})
	if out := h.mustRun(t, "dialog", "status"); !strings.Contains(out, `dialog confirm "Sure?" answered true (no answer armed: session default)`) {
		t.Fatalf("dialog status after a CDP-caused dialog:\n%s", out)
	}
	other := p
	other.Session = "someone-else"
	CDPDialogs.After(other, []cdp.DialogUse{{Type: "alert", Message: "other"}})
	if out := h.mustRun(t, "dialog", "status"); strings.Contains(out, "other") {
		t.Fatalf("another session's dialog reached this session's log:\n%s", out)
	}
}
