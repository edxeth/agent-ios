package core

import (
	"context"
	"encoding/json"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// Hooks for the logpoint command (internal/commands/logpoint.go). A
// logpoint is a URL breakpoint of the owner "logpoint:<session>" that
// never pauses (autoContinue with an evaluate action). It goes through
// the same desired-state path as a CDP attachment's Debugger calls
// (legacyCall): Debugger is acquired for the owner, the breakpoint is
// replayed on new and provisional targets (breakpointEntry), and the
// lease release of the session removes it with the owner's breakpoint
// activation and pause settings.

// LegacyCall sends method as a call of owner through the page's desired
// state, as Manager.RawAs does for a CDP attachment; c is the caller of
// the running operation.
func LegacyCall(ctx context.Context, ps *PageSession, owner Owner, c webkit.Caller, method string, params json.RawMessage) (json.RawMessage, error) {
	return legacyCall(ctx, ps, owner, c, method, params)
}

// HasBreakpoint reports whether the URL breakpoint that a client knows as
// id is still owned state: a lease release or the owner's
// Debugger.disable removes it.
func (d *DesiredState) HasBreakpoint(id string) bool {
	return d.HasEntry(breakpointKey(id))
}

// CurrentBreakpointID returns the id that the active target gives the
// breakpoint a client knows as id: another one after a replay on a new
// target, else id.
func (d *DesiredState) CurrentBreakpointID(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cur, ok := d.bpIDs[id]; ok {
		return cur
	}
	return id
}
