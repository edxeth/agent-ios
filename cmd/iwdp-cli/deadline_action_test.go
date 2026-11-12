package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/daemon"
)

// Device check of 2026-11-11: a click on a selector that matched nothing
// ended at its deadline with "Last wait: ... no element matches", but the
// likely causes named another inspector, a stalled page and a locked
// device, none of which applies when the page answered every attempt.
// An action whose attempts the deadline ended (core.ErrActionTimeout, also
// through the daemon's reply) names the action's checks instead.
func TestDeadlineMessageOfAnActionNamesItsChecks(t *testing.T) {
	lastWait := fmt.Errorf("click stopped before step 1 of 1 (0 acknowledged): no element matches \"#missing\": %w", core.ErrActionTimeout)
	// The daemon's reply of that error (TestErrorReplyMarksAnActionTimeout).
	remote := &daemon.RemoteError{E: daemon.ErrorReply{Code: exitTimeout, Message: lastWait.Error(), Timeout: true, ActionTimeout: true}}
	for name, cause := range map[string]error{"in process": lastWait, "from the daemon": remote} {
		t.Run(name, func(t *testing.T) {
			r := &commandRun{name: "click", page: `"Fixture" (key)`, start: time.Now(), budget: 5 * time.Second}
			msg := r.timeoutMessage(cause)
			for _, want := range []string{"Error: iwdp-cli click timed out", "Last wait: click stopped", "no element matches", "Likely causes:", "did not pass its checks", "take a new snapshot", "--timeout <duration>"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message lacks %q:\n%s", want, msg)
				}
			}
			for _, wrong := range []string{"another inspector session", "device is locked"} {
				if strings.Contains(msg, wrong) {
					t.Errorf("message names %q, which an answered action rules out:\n%s", wrong, msg)
				}
			}
		})
	}
	// Any other deadline keeps the general causes.
	r := &commandRun{name: "eval", page: "ws://page", start: time.Now(), budget: time.Second}
	msg := r.timeoutMessage(fmt.Errorf("Runtime.evaluate: %w", context.DeadlineExceeded))
	for _, want := range []string{"another inspector session", "device is locked"} {
		if !strings.Contains(msg, want) {
			t.Errorf("general deadline message lacks %q:\n%s", want, msg)
		}
	}
}
