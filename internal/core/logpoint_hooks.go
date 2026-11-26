package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// Hooks for the logpoint command (internal/commands/logpoint.go) and the
// CDP bridge's Debugger domain (internal/cdp/debugger_domain.go). A
// logpoint is a URL breakpoint of the owner "logpoint:<session>" (or of a
// CDP attachment) that never pauses (autoContinue with an evaluate
// action). It goes through the same desired-state path as a CDP
// attachment's Debugger calls (legacyCall): Debugger is acquired for the
// owner without any pause (AcquireNoPauseDebugger), the breakpoint is
// replayed on new and provisional targets (breakpointEntry), and the lease
// release of the session removes it with the owner's breakpoint activation
// and pause settings.

// LegacyCall sends method as a call of owner through the page's desired
// state, as Manager.RawAs does for a CDP attachment; c is the caller of
// the running operation.
func LegacyCall(ctx context.Context, ps *PageSession, owner Owner, c webkit.Caller, method string, params json.RawMessage) (json.RawMessage, error) {
	return legacyCall(ctx, ps, owner, c, method, params)
}

// noPauseSettings are the JSC::Debugger settings that let a page with
// Debugger enabled run without pausing: no pause on debugger statements
// and none on exceptions. The other pauses of WebKit (assertions,
// microtasks, DOM, event and URL breakpoints, Debugger.pause, stepping)
// are agent state that Debugger.disable and the agent reset clear, and
// that only an owner's own call sets.
var noPauseSettings = []struct {
	method string
	params map[string]any
}{
	{"Debugger.setPauseOnDebuggerStatements", map[string]any{"enabled": false}},
	{"Debugger.setPauseOnExceptions", map[string]any{"state": "none"}},
}

// AcquireNoPauseDebugger gives owner the Debugger domain with active
// breakpoints in a state where the page cannot pause, in one change and in
// the only order that leaves no window: an unknown agent is reset first,
// then the pause settings go out (owned shared values of owner, which a
// target gets again before its Debugger.enable: stateRule.beforeEnable),
// then Debugger.enable (when owner is the first owner), then the
// breakpoint activation. A failed send fails the acquisition before any
// later step, so the caller sets no breakpoint. c is the caller of the
// running operation.
func AcquireNoPauseDebugger(ctx context.Context, ps *PageSession, owner Owner, c webkit.Caller) error {
	return ps.State.Change(ctx, c, func(c webkit.Caller) error {
		return acquireNoPauseDebugger(ctx, ps, owner, c)
	})
}

// AcquireNoPauseDebugger is AcquireNoPauseDebugger for the operation mo of
// a CDP attachment, with the command deadline of Manager.RawAs.
func (m *Manager) AcquireNoPauseDebugger(ctx context.Context, mo *ManagedOp, owner Owner) error {
	if dl, ok := ctx.Deadline(); ok {
		ctx = webkit.WithCallBudget(ctx, time.Until(dl))
	}
	err := AcquireNoPauseDebugger(ctx, mo.Page.PS, owner, mo.Op.Caller())
	if err != nil && (errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(context.Cause(mo.Op.Context()), ErrDeadline)) {
		return &CodedError{Code: CodeTimeout, Err: fmt.Errorf("%w: %w", err, context.DeadlineExceeded)}
	}
	return err
}

func acquireNoPauseDebugger(ctx context.Context, ps *PageSession, owner Owner, c webkit.Caller) error {
	if err := ResetUnknownAgent(ctx, ps, c, "Debugger"); err != nil {
		return err
	}
	for _, x := range noPauseSettings {
		rule := stateRules[x.method]
		if _, err := c.Send(withoutForward(ctx), x.method, x.params); err != nil {
			return fmt.Errorf("%s: turning off the page's pauses before Debugger is enabled, so nothing was enabled: %w", x.method, err)
		}
		ps.State.setShared(c, owner, entryKey(x.method, rule.key(x.params)), x.method, x.method, x.params, rule.clear(x.params))
	}
	if err := acquireFor(ctx, ps.State, owner, c, "Debugger"); err != nil {
		return fmt.Errorf("%s: %w", "Debugger.enable", err)
	}
	const act = "Debugger.setBreakpointsActive"
	p := map[string]any{"active": true}
	if _, _, err := setSharedValue(ctx, ps, owner, c, entryKey(act, stateRules[act].key(p)), act, act, p, false); err != nil {
		return fmt.Errorf("%s: %w", act, err)
	}
	return nil
}

// HasBreakpoint reports whether the URL breakpoint that a client knows as
// id is still owned state: a lease release or the owner's
// Debugger.disable removes it.
func (d *DesiredState) HasBreakpoint(id string) bool {
	return d.HasEntry(breakpointKey(id))
}

// OwnsBreakpoint reports whether owner holds the breakpoint that a client
// knows as id.
func (d *DesiredState) OwnsBreakpoint(owner Owner, id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[breakpointKey(id)]
	return ok && e.Owner == owner
}

// breakpointHolderLocked returns the owner of the breakpoint that id names:
// the id a client got, or the id a target gave a replayed copy. The
// caller holds d.mu.
func (d *DesiredState) breakpointHolderLocked(id string) (Owner, bool) {
	if e, ok := d.entries[breakpointKey(id)]; ok {
		return e.Owner, true
	}
	holder := func(client string) (Owner, bool) {
		e, ok := d.entries[breakpointKey(client)]
		return e.Owner, ok
	}
	for client, cur := range d.bpIDs {
		if cur == id {
			return holder(client)
		}
	}
	for _, ids := range d.staged {
		for client, cur := range ids {
			if cur == id {
				return holder(client)
			}
		}
	}
	return "", false
}

// BreakpointClientID returns the id under which owner knows the breakpoint
// that target's backend reports as backendID (Debugger.breakpointResolved):
// the id the client got when it set the breakpoint, also after a replay on
// a new or provisional target gave it another id. ok is false for a
// breakpoint that owner does not hold, or a stale id of a replaced target.
func (d *DesiredState) BreakpointClientID(owner Owner, target, backendID string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	owns := func(id string) bool {
		e, ok := d.entries[breakpointKey(id)]
		return ok && e.Owner == owner
	}
	if target != "" && target != d.active {
		for id, cur := range d.staged[target] {
			if cur == backendID && owns(id) {
				return id, true
			}
		}
	}
	for id, cur := range d.bpIDs {
		if cur == backendID && owns(id) {
			return id, true
		}
	}
	if cur, replaced := d.bpIDs[backendID]; owns(backendID) && (!replaced || cur == backendID) {
		return backendID, true
	}
	return "", false
}
