package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Tabs (design docs/design/agentic-parity.md, sections 3.8 and 4.2;
// agent-browser "tab"). The daemon keeps tab ids t1, t2, ... per page and
// each session's current page (core/pages_w3b.go). WIP cannot raise a tab,
// and Safari answers only for the visible tab (MAP fact 12), so "tab <id>"
// probes the tab before it moves the session there.
func init() {
	Register(&Command{
		Name: "tab list", MCPName: "tab_list", Profile: ProfileCore,
		Summary:   "List the pages of the device as tabs t1, t2, ... with labels; * marks this session's current tab.",
		RetrySafe: true, Output: OutputText,
		Run: runTabList,
	})
	Register(&Command{
		Name: "tab", MCPName: "tab_select", Profile: ProfileCore,
		Summary: "Make a tab (tN or label) this session's current page after a liveness probe; without an id, list the tabs.",
		Args:    []Arg{{Name: "id", Help: "tab id (t1, t2, ...) or label from tab list; none: list the tabs"}},
		Output:  OutputText,
		Run:     runTab,
	})
	Register(&Command{
		Name: "tab new", Aliases: []string{"window new"}, MCPName: "tab_new", Profile: ProfileDevtools,
		Summary: "Open a new tab with window.open (emulated user gesture) from the current page and make it the current tab.",
		Args:    []Arg{{Name: "url", Help: "URL to open (default about:blank)"}},
		Flags:   []Flag{{Name: "label", Type: FlagString, Help: "label for the new tab, usable instead of its tN id"}},
		Page:    LeasedPage, Mutates: true, Output: OutputText,
		Run: runTabNew,
	})
	Register(&Command{
		Name: "tab close", MCPName: "tab_close", Profile: ProfileDevtools,
		Summary: "Close a tab (default: the current one) with window.close(); works only for tabs that a script opened.",
		Args:    []Arg{{Name: "id", Help: "tab id (tN) or label; default: the current tab"}},
		Output:  OutputText,
		Run:     runTabClose,
	})
}

const (
	// tabProbeBudget bounds the liveness probe of "tab <id>" (design 2.6).
	tabProbeBudget = 2 * time.Second
	// tabPollEvery and tabNewWait: "tab new" polls the page list every
	// 250 ms for up to 10 s (design 3.8); tabCloseWait bounds the wait
	// for a closed tab to leave the list.
	tabPollEvery = 250 * time.Millisecond
	tabNewWait   = 10 * time.Second
	tabCloseWait = 3 * time.Second
)

// callerOf is the caller of a command's page: its operation's fenced
// caller in the daemon (design 2.10), else the page's.
func callerOf(env Env, ps *core.PageSession) webkit.Caller {
	if oe, ok := env.(interface{ Op() *core.Op }); ok && oe.Op() != nil {
		return oe.Op().Caller()
	}
	return ps.Caller
}

func tabRequest(in Input, page string) core.PageRequest {
	return core.PageRequest{Transport: in.Global.Transport, Page: page}
}

func acquireFor(in Input) core.AcquireOptions {
	g := in.Global
	return core.AcquireOptions{Session: g.Session, Reservation: g.Reservation, WaitLease: g.WaitLease, Takeover: g.Takeover}
}

func runTabList(ctx context.Context, env Env, in Input) (Result, error) {
	m, err := managerOf(env)
	if err != nil {
		return Result{}, err
	}
	tabs, hidden, err := m.Tabs(ctx, tabRequest(in, ""), acquireFor(in))
	if err != nil {
		return Result{}, err
	}
	var b strings.Builder
	for _, t := range tabs {
		mark := " "
		if t.Current {
			mark = "*"
		}
		fmt.Fprintf(&b, "%s %s", mark, t.ID)
		if t.Label != "" {
			fmt.Fprintf(&b, " [%s]", t.Label)
		}
		fmt.Fprintf(&b, "  %q  %s", t.Title, t.URL)
		if t.LeasedBy != "" {
			fmt.Fprintf(&b, "  (leased by %s)", t.LeasedBy)
		}
		if !t.Debuggable {
			b.WriteString("  (not debuggable)")
		}
		b.WriteString("\n")
	}
	if len(tabs) == 0 {
		b.WriteString("No tabs.\n")
	}
	if hidden > 0 {
		// Design 2.5 rule 4: no title or URL of a device without the
		// caller's reservation.
		owner := acquireFor(in).Reservation
		if owner == "" {
			owner = in.Global.Session
		}
		fmt.Fprintf(&b, "%d page(s) of physical devices without a reservation of owner %q are not listed; run 'iwdp-cli device reserve --owner %s' or pass --reservation <owner>", hidden, owner, owner)
	}
	return Result{Text: strings.TrimRight(b.String(), "\n"), Data: map[string]any{"tabs": tabs, "hidden": hidden}}, nil
}

// tabKey returns the page key of a tab id or label, listing the tabs
// first when the daemon has not handed out that id yet.
func tabKey(ctx context.Context, m *core.Manager, in Input, id string) (string, error) {
	if k, ok := m.TabKey(id); ok {
		return k, nil
	}
	if _, _, err := m.Tabs(ctx, tabRequest(in, ""), acquireFor(in)); err != nil {
		return "", err
	}
	if k, ok := m.TabKey(id); ok {
		return k, nil
	}
	return "", &core.CodedError{Code: core.CodeNoPage, Err: fmt.Errorf("%w: no tab %q; run 'tab list' for the tab ids and labels", core.ErrNoPage, id)}
}

func runTab(ctx context.Context, env Env, in Input) (Result, error) {
	id := arg(in, 0)
	if id == "" || id == "list" {
		return runTabList(ctx, env, in)
	}
	m, err := managerOf(env)
	if err != nil {
		return Result{}, err
	}
	key, err := tabKey(ctx, m, in, id)
	if err != nil {
		return Result{}, err
	}
	req := tabRequest(in, key)
	probed := false
	var state core.ConnState // after a failed probe
	err = m.Do(ctx, req, acquireFor(in), "tab select", core.ClassRead, false, func(ctx context.Context, mo *core.ManagedOp) error {
		pctx, cancel := context.WithTimeout(ctx, tabProbeBudget)
		defer cancel()
		probed = true
		_, err := mo.Op.Caller().Send(pctx, "Runtime.evaluate", map[string]any{"expression": "1", "returnByValue": true})
		if err != nil {
			state = mo.Page.ConnState()
		}
		return err
	})
	if err != nil {
		return Result{}, tabSelectError(id, probed, state, err)
	}
	m.SelectPage(in.Global.Session, req)
	lines := []string{fmt.Sprintf("tab %s is now the current page of session %q", id, in.Global.Session)}
	// The session moves its lease with it (design 3.8): its leases on other
	// pages end, with their cleanup.
	for _, p := range sessionPages(m, in.Global.Session) {
		if p.Key == key {
			continue
		}
		if err := m.Release(ctx, in.Global.Session, p.Key); err != nil {
			lines = append(lines, fmt.Sprintf("warning: releasing the lease of %q: %v", p.Title, err))
		}
	}
	return Result{Text: strings.Join(lines, "\n"), Data: map[string]any{"tab": id, "key": key}}, nil
}

// tabSelectError is the error of "tab <id>" when its page operation failed
// with err. probed is whether the probe ran; state is the page's
// connection state read right after the probe failed (meaningful only when
// probed). The causes are checked from the most specific to the most
// general, because one failure can carry several identities: a closed
// direct-transport page reports ErrPageClosed inside ErrSessionClosed, and a
// write timeout reports ErrWriteTimeout inside ErrSessionClosed.
//
//  1. The daemon ended or refused the operation: core.ErrOpEnded in any form
//     (refused before admission as "not sent", or a call in flight
//     canceled), the lease ended or changed (core.ErrLeaseEnded,
//     core.ErrStaleEpoch), another cancellation that is no deadline
//     (context.Canceled), or state ConnClosed (the daemon closed the
//     connection itself, which only its shutdown does while an operation
//     runs): run the command again.
//  2. ErrPageClosed: the tab closed (iwdp: its target was destroyed; direct:
//     its application or the device connection ended); see 'tab list'.
//  3. ErrWriteTimeout: the page did not take data, which the daemon
//     recovers from by itself, so it is the hidden-tab advice of 5.
//  4. ErrLost, ErrSessionClosed (a peer end: a read error or ErrWriteBroken)
//     or state ConnLost: another inspector took the page or the device
//     ended the connection; only --takeover reattaches it.
//  5. Any other probe failure (its deadline, ErrSessionStalled, a reply
//     error): the tab does not answer, as a hidden Safari tab does.
//
// Exit 4 in every case. An error before the probe keeps its own text,
// except a deadline or a stall, which get the advice of 5.
func tabSelectError(id string, probed bool, state core.ConnState, err error) error {
	busy := func(format string) error {
		return &core.CodedError{Code: core.CodeBusy, Err: fmt.Errorf(format, id, err)}
	}
	const hidden = "tab %s is not the visible Safari tab; bring it to the front on the device (%w)"
	if !probed {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, webkit.ErrSessionStalled) {
			return busy(hidden)
		}
		return err
	}
	switch {
	case state == core.ConnClosed || errors.Is(err, core.ErrOpEnded) || errors.Is(err, core.ErrLeaseEnded) || errors.Is(err, core.ErrStaleEpoch) ||
		(errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)):
		return busy("tab %s was not selected: the daemon ended the probe (it is shutting down, the page's lease ended or changed, or the operation was canceled); run the command again (%w)")
	case errors.Is(err, webkit.ErrPageClosed):
		return busy("tab %s closed during the probe (or its device connection ended), so it was not selected; run 'tab list' for the open tabs (%w)")
	case errors.Is(err, webkit.ErrWriteTimeout):
		return busy(hidden)
	case state == core.ConnLost || errors.Is(err, core.ErrLost) || errors.Is(err, webkit.ErrSessionClosed):
		return busy("tab %s lost its page connection during the probe (another inspector took the page, or the device ended the connection), so it was not selected; run the command again with --takeover once the device owner agrees (%w)")
	}
	return busy(hidden)
}

func runTabNew(ctx context.Context, env Env, in Input) (Result, error) {
	m, err := managerOf(env)
	if err != nil {
		return Result{}, err
	}
	label := flagString(in, "label")
	if label != "" {
		if _, ok := m.TabKey(label); ok {
			return Result{}, usagef("tab new --label %q: a tab has this label already", label)
		}
	}
	before, _, err := m.Tabs(ctx, tabRequest(in, ""), acquireFor(in))
	if err != nil {
		return Result{}, err
	}
	known := map[string]bool{}
	for _, t := range before {
		known[t.Key] = true
	}
	ps, err := env.Page(ctx)
	if err != nil {
		return Result{}, err
	}
	caller := callerOf(env, ps)
	u := arg(in, 0)
	if u == "" {
		u = "about:blank"
	}
	expr, err := json.Marshal(u)
	if err != nil {
		return Result{}, err
	}
	raw, err := caller.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "!!window.open(" + string(expr) + ")", "returnByValue": true, "emulateUserGesture": true})
	if err != nil {
		return Result{}, fmt.Errorf("tab new: window.open: %w", err)
	}
	if opened, err := evalValue[bool](raw); err != nil || !opened {
		return Result{}, fmt.Errorf("tab new: window.open(%s) returned no window; the browser blocked the popup (%v)", expr, err)
	}
	end := time.Now().Add(tabNewWait)
	for {
		tabs, _, err := m.Tabs(ctx, tabRequest(in, ""), acquireFor(in))
		if err != nil {
			return Result{}, err
		}
		for _, t := range tabs {
			if known[t.Key] {
				continue
			}
			if label != "" {
				if err := m.LabelTab(t.Key, label); err != nil {
					return Result{}, usagef("tab new --label: %v", err)
				}
			}
			m.SelectPage(in.Global.Session, tabRequest(in, t.Key))
			return Result{
				Text: fmt.Sprintf("opened tab %s (%s); it is now the current page of session %q", t.ID, u, in.Global.Session),
				Data: map[string]any{"tab": t.ID, "key": t.Key, "label": label},
			}, nil
		}
		if time.Now().After(end) {
			return Result{}, &core.CodedError{Code: core.CodeNoPage, Err: fmt.Errorf("tab new: no new tab appeared in the page list within %s; the browser may have blocked the popup or may not list script-opened windows", tabNewWait)}
		}
		sleepUntil(ctx, time.Now().Add(tabPollEvery))
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
	}
}

func runTabClose(ctx context.Context, env Env, in Input) (Result, error) {
	m, err := managerOf(env)
	if err != nil {
		return Result{}, err
	}
	id, page := arg(in, 0), ""
	if id != "" {
		if page, err = tabKey(ctx, m, in, id); err != nil {
			return Result{}, err
		}
	}
	var key string
	err = m.Do(ctx, tabRequest(in, page), acquireFor(in), "tab close", core.ClassMutate, false, func(ctx context.Context, mo *core.ManagedOp) error {
		key = mo.Page.Key
		cctx, cancel := context.WithTimeout(ctx, navStartBudget)
		defer cancel()
		// A closing page can go away before it answers; a lost reply is
		// accepted, the page list decides.
		_, err := mo.Op.Caller().Send(cctx, "Runtime.evaluate", map[string]any{"expression": "window.close()"})
		if errors.Is(err, webkit.ErrNotSent) {
			return err
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	end := time.Now().Add(tabCloseWait)
	for {
		tabs, _, err := m.Tabs(ctx, tabRequest(in, ""), acquireFor(in))
		if err != nil {
			return Result{}, err
		}
		open, name := false, id
		for _, t := range tabs {
			if t.Key == key {
				open, name = true, t.ID
			}
		}
		if !open {
			break
		}
		if time.Now().After(end) {
			return Result{}, fmt.Errorf("tab close: tab %s is still open after window.close(); only tabs that a script opened can be closed", name)
		}
		sleepUntil(ctx, time.Now().Add(tabPollEvery))
	}
	m.ForgetTab(key)
	lines := []string{"closed the tab"}
	if id != "" {
		lines[0] = "closed tab " + id
	}
	if err := m.Release(ctx, in.Global.Session, key); err != nil {
		lines = append(lines, "warning: releasing the lease of the closed tab: "+err.Error())
	}
	return Result{Text: strings.Join(lines, "\n"), Data: map[string]any{"closed": key}}, nil
}
