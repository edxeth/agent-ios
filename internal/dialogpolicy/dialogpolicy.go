// Package dialogpolicy is the session's dialog override (design 3.7,
// decision D6) as one desired-state change. The dialog command
// (internal/commands/dialog.go) and the CDP bridge (internal/cdp, design
// 5.2 "Dialogs") both install it through Install, so both put the same
// bootstrap part under the same owner.
//
// WIP has no dialog events, and a native dialog blocks Runtime.evaluate,
// so the agent runtime replaces alert, confirm and prompt (80-dialogs.js)
// in the current document and, through the bootstrap part, in new ones.
package dialogpolicy

import (
	"context"
	"fmt"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// Policies (decision D6): with no answer armed, Dismiss makes confirm
// return false and prompt null; Accept makes confirm return true and
// prompt its default value. alert is always accepted. Default is the
// policy of a session that set none.
const (
	Dismiss = "dismiss"
	Accept  = "accept"
	Default = Dismiss
)

// PartName is the name of the override's bootstrap part.
const PartName = "dialog"

// Owner is the desired-state owner of session's override: it belongs to
// the logical session, so the session's release removes it.
func Owner(session string) core.Owner { return core.SessionOwner("dialog", session) }

// Part is the bootstrap part that installs the override with policy in a
// new document before its scripts run (with pause-on-start, design 2.7).
func Part(policy string) string {
	return fmt.Sprintf("window.__iwdp && window.__iwdp.call(\"dialogs\", {policy: %q});", policy)
}

// Install makes session's override with policy part of the desired state
// st: the bootstrap part and the Console ownership that keeps the
// override's log messages flowing (design 3.7, 2.4). Both are re-applied
// on reconnect and target swaps and removed when the session releases the
// page. They are one change (DesiredState.Change), so a provisional
// target whose setup already ran gets them too before it commits (design
// 2.7). c is the operation's caller. The current document is the
// caller's to update (the runtime function "dialogs").
func Install(ctx context.Context, st *core.DesiredState, c webkit.Caller, session, policy string) error {
	owner := Owner(session)
	return st.Change(ctx, c, func(c webkit.Caller) error {
		if err := st.SetPart(owner, PartName, core.RankDialog, Part(policy)); err != nil {
			return err
		}
		if _, err := c.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": st.Boot.Script()}); err != nil {
			return fmt.Errorf("installing the dialog override for new documents: %w", err)
		}
		if st.Acquire(owner, "Console") {
			if _, err := c.Send(ctx, "Console.enable", nil); err != nil {
				return fmt.Errorf("enabling Console for the dialog log: %w", err)
			}
		}
		return nil
	})
}
