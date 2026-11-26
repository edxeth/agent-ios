package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/logcause"
	"github.com/edxeth/agent-ios/internal/logsafe"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// Tabs (design docs/design/agentic-parity.md, sections 3.8 and 4.2;
// agent-browser "tab"). The daemon keeps tab ids t1, t2, ... per page and
// each session's current page (core/pages_w3b.go). WIP cannot raise a tab,
// and Safari answers only for the visible tab (MAP fact 12), so "tab <id>"
// probes the tab before it moves the session there.
func init() {
	Register(&Command{
		Name:      "tab list",
		Summary:   "List the pages of the device as tabs t1, t2, ... with labels; * marks this session's current tab.",
		Flags:     []Flag{tabDeviceFlag},
		RetrySafe: true, Output: OutputText,
		Run: runTabList,
	})
	Register(&Command{
		Name:    "tab",
		Summary: "Make a tab (tN or label) this session's current page after a liveness probe; without an id, list the tabs.",
		Args:    []Arg{{Name: "id", Help: "tab id (t1, t2, ...) or label from tab list; none: list the tabs", Page: true}},
		Output:  OutputText,
		Run:     runTab,
	})
	Register(&Command{
		Name: "tab new", Aliases: []string{"window new"},
		Summary: "Open a new tab with window.open (emulated user gesture) from the current page and make it the current tab; the session's leases on other pages end.",
		Args:    []Arg{{Name: "url", Help: "URL to open (default about:blank)"}},
		Flags:   []Flag{{Name: "label", Type: FlagString, Help: "label for the new tab, usable instead of its tN id"}},
		// No page of the command: runTabNew leases the current page for
		// window.open in its own operation, so that it can end that lease
		// after the operation. It runs window.open in the --page page.
		ReadsPage: true, Mutates: true, Output: OutputText,
		Run: runTabNew,
	})
	Register(&Command{
		Name:    "tab close",
		Summary: "Close a tab (default: the current one) with window.close(); works only for tabs that a script opened. In your turn on the simulator, a session that selected no tab must name the tab (exit 5).",
		Args:    []Arg{{Name: "id", Help: "tab id (tN) or label; default: the current tab", Page: true}},
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
	// tabReleaseBudget bounds the release of the lease that a failed
	// "tab new" took, which runs also after the command's own deadline
	// or cancel: the 5 s in which the daemon's stop cleans up every page.
	tabReleaseBudget = 5 * time.Second
)

// tabDeviceFlag limits tab list and tab find to the pages of one device.
var tabDeviceFlag = Flag{Name: "device", Type: FlagString, Help: "only the pages of this device: SIMULATOR (the iOS Simulator) or a UDID; other devices' pages are neither listed nor counted as hidden"}

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
	tabs, hidden, err := m.DeviceTabs(ctx, tabRequest(in, ""), acquireFor(in), flagString(in, "device"))
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
		b.WriteString(tabDevice(t.DeviceID))
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
		fmt.Fprintf(&b, "%d page(s) are not listed: those of physical devices without a reservation of owner %q (run 'agent-ios device reserve --owner %s' or pass --reservation <owner>), and the simulator's while another owner reserves it (agent-ios sim reserve waits for it)", hidden, owner, owner)
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
	lines = append(lines, releaseOtherLeases(ctx, m, in.Global.Session, key)...)
	return Result{Text: strings.Join(lines, "\n"), Data: map[string]any{"tab": id, "key": key}}, nil
}

// releaseOtherLeases ends session's leases on every page but keep, with
// their cleanup: the session moves its lease with its current page
// (design 3.8). It returns a warning line for each release that failed
// (releaseLeaseOf).
func releaseOtherLeases(ctx context.Context, m *core.Manager, session, keep string) []string {
	var warnings []string
	for _, p := range sessionPages(m, session) {
		if p.Key == keep {
			continue
		}
		if w := releaseLeaseOf(ctx, m, session, p.Key, strconv.Quote(p.Title)); w != "" {
			warnings = append(warnings, w)
		}
	}
	return warnings
}

// releaseLeaseOf ends session's lease on the page of key with its cleanup
// and returns "" or a warning line that names the page as name. A release
// whose page connection ended (closedPageGone, as for a tab that closed
// after a "tab close" whose page listing failed) cannot send its
// page-side cleanup, and nothing of the session stays on a page that is
// gone: that is a daemon log line, not a warning.
func releaseLeaseOf(ctx context.Context, m *core.Manager, session, key, name string) string {
	return releaseWarning(m, session, name, m.Release(ctx, session, key))
}

// releaseWarning is the warning line of err, the result of a release of
// session's lease on the page named name (releaseLeaseOf).
func releaseWarning(m *core.Manager, session, name string, err error) string {
	switch {
	case err == nil:
		return ""
	case closedPageGone(err):
		m.Logf("tab: the lease of session %s on another page was released, but its page-side cleanup was not sent because the page connection ended: %v", logsafe.ID(session), logsafe.Error(err))
		return ""
	}
	return fmt.Sprintf("warning: releasing the lease of %s: %v", name, err)
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
	session := in.Global.Session
	u := arg(in, 0)
	if u == "" {
		u = "about:blank"
	}
	expr, err := json.Marshal(u)
	if err != nil {
		return Result{}, err
	}
	// window.open runs in the session's current page (the opener), which
	// in a new session is the device's first tab, often the user's own.
	// The new tab is found as click --new-tab finds it (listBeforeOpen).
	// borrowed records the lease that this call's acquisition granted on
	// the opener (none when the session held it before), also when the
	// operation's admission fails after the grant (core.LeaseClaim).
	var borrowed core.LeaseClaim
	ao := acquireFor(in)
	ao.Claim = &borrowed
	var device string
	var known map[string]bool
	var listErr error
	err = m.Do(ctx, tabRequest(in, in.Global.Page), ao, "tab new", core.ClassMutate, false, func(ctx context.Context, mo *core.ManagedOp) error {
		device = mo.Page.PS.Device.ID
		known, listErr = listBeforeOpen(ctx, m, in, device)
		raw, err := mo.Op.Caller().Send(ctx, "Runtime.evaluate", map[string]any{"expression": "!!window.open(" + string(expr) + ")", "returnByValue": true, "emulateUserGesture": true})
		if err != nil {
			return fmt.Errorf("tab new: window.open: %w", err)
		}
		if opened, err := evalValue[bool](raw); err != nil || !opened {
			return logcause.Errorf("tab new: window.open(%s) returned no window; the browser blocked the popup (%v)", expr, err)
		}
		return nil
	})
	// A failed tab new leaves the session on the opener; it ends the lease
	// that it borrowed there, only while that lease is still this call's
	// alone (ReleaseClaimed): a lease held before, a lease on another page,
	// a lease granted again later and a lease that another command of the
	// session used meanwhile stay. The release gets its own budget,
	// because a canceled or expired command must still end the lease and
	// send its cleanup.
	release := func() (warnings []string) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tabReleaseBudget)
		defer cancel()
		for _, key := range borrowed.Keys() {
			if w := releaseWarning(m, session, "the page that ran window.open", m.ReleaseClaimed(rctx, &borrowed, key)); w != "" {
				warnings = append(warnings, w)
			}
		}
		return warnings
	}
	failed := func(err error) error {
		for _, w := range release() {
			err = fmt.Errorf("%w; %s", err, w)
		}
		return err
	}
	if err != nil {
		return Result{}, failed(err)
	}
	if listErr != nil {
		// window.open returned a window, but nothing tells the new tab
		// from the tabs that were there (review 2 F9): the session stays
		// on the opener, without the lease that tab new borrowed there.
		warning := notFollowedWarning("window.open", listErr)
		if label != "" {
			warning += fmt.Sprintf("; the label %q was given to no tab", label)
		}
		lines := append([]string{"opened " + u + " with window.open", warning}, release()...)
		return Result{Text: strings.Join(lines, "\n"), Data: map[string]any{"tab": "", "key": "", "label": ""}}, nil
	}
	t, found, err := waitNewTab(ctx, m, in, device, newKeyIn(known), time.Now().Add(tabNewWait))
	switch {
	case err != nil:
		return Result{}, failed(err)
	case !found:
		return Result{}, failed(&core.CodedError{Code: core.CodeNoPage, Err: fmt.Errorf("tab new: no new tab appeared in the page list within %s; the browser may have blocked the popup or may not list script-opened windows", tabNewWait)})
	}
	if label != "" {
		if err := m.LabelTab(t.Key, label); err != nil {
			return Result{}, failed(usagef("tab new --label: %v", err))
		}
	}
	lines := followTab(ctx, m, in, t, fmt.Sprintf("opened tab %s (%s)", t.ID, u))
	return Result{
		Text: strings.Join(lines, "\n"),
		Data: map[string]any{"tab": t.ID, "key": t.Key, "label": label},
	}, nil
}

// listBeforeOpen is the listing that tab new and click --new-tab take of
// the opener's device right before their window.open call, within
// newTabListBudget: the new tab is a tab of that device that it does not
// have (newKeyIn, waitNewTab on device). When it fails, the caller still
// calls window.open, but takes no tab for the new one and keeps the
// session on the opener with notFollowedWarning (review 2 F9).
func listBeforeOpen(ctx context.Context, m *core.Manager, in Input, device string) (map[string]bool, error) {
	lctx, cancel := context.WithTimeout(ctx, newTabListBudget)
	defer cancel()
	return deviceKnownTabs(lctx, m, in, device)
}

// notFollowedWarning is the result line of tab new and click --new-tab
// when listBeforeOpen failed before call (what ran: "the click",
// "window.open").
func notFollowedWarning(call string, err error) string {
	return fmt.Sprintf("warning: the session stays on this page: the tab list right before %s could not be read, so no tab is taken for the new one (%v); run 'tab list', then 'tab <id>'", call, err)
}

// deviceKnownTabs are the page keys of the tabs of the device id listed
// now, before a command opens a tab, for waitNewTab on that device.
// Only a complete listing of the device counts: its page list failing is
// an error (Manager.DeviceTabs), and so is a listing that hides pages of
// it (a reservation that ended), or a device without an id.
func deviceKnownTabs(ctx context.Context, m *core.Manager, in Input, device string) (map[string]bool, error) {
	if device == "" {
		return nil, errors.New("the opener's device has no id")
	}
	tabs, hidden, err := m.DeviceTabs(ctx, tabRequest(in, ""), acquireFor(in), device)
	if err != nil {
		return nil, err
	}
	if hidden != 0 {
		return nil, fmt.Errorf("the listing hides %d page(s) of device %s", hidden, device)
	}
	known := map[string]bool{}
	for _, t := range tabs {
		known[t.Key] = true
	}
	return known, nil
}

// newKeyIn picks the first listed tab whose key known has not.
func newKeyIn(known map[string]bool) func([]core.Tab) (core.Tab, bool) {
	return func(tabs []core.Tab) (core.Tab, bool) {
		for _, t := range tabs {
			if !known[t.Key] {
				return t, true
			}
		}
		return core.Tab{}, false
	}
}

// waitNewTab polls the page list of device ("" for every device) every
// tabPollEvery until pick finds the new tab in it, and returns it; found
// is false when none appeared by end.
func waitNewTab(ctx context.Context, m *core.Manager, in Input, device string, pick func([]core.Tab) (core.Tab, bool), end time.Time) (t core.Tab, found bool, err error) {
	for {
		tabs, _, err := m.DeviceTabs(ctx, tabRequest(in, ""), acquireFor(in), device)
		if err != nil {
			return core.Tab{}, false, err
		}
		if t, ok := pick(tabs); ok {
			return t, true, nil
		}
		if time.Now().After(end) {
			return core.Tab{}, false, nil
		}
		sleepUntil(ctx, time.Now().Add(tabPollEvery))
		if ctx.Err() != nil {
			return core.Tab{}, false, ctx.Err()
		}
	}
}

// followTab makes the new tab t the session's current page, as "tab <id>"
// does: the session moves its lease with its current page, so its leases
// on other pages (the opener's too) end with their cleanup. It returns
// the result lines: opened, which names the tab, with the session, and a
// warning for each release that failed.
func followTab(ctx context.Context, m *core.Manager, in Input, t core.Tab, opened string) []string {
	session := in.Global.Session
	m.SelectPage(session, tabRequest(in, t.Key))
	lines := []string{fmt.Sprintf("%s; it is now the current page of session %q", opened, session)}
	return append(lines, releaseOtherLeases(ctx, m, session, t.Key)...)
}

// closedPageGone reports whether err, a failed release of a tab that is
// no longer listed, comes from the end of the tab's page connection: the
// page is gone with the tab. Any other release failure is a warning.
func closedPageGone(err error) bool {
	return errors.Is(err, webkit.ErrSessionClosed) || errors.Is(err, webkit.ErrPageClosed)
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
	} else if err := refuseUnselectedSimulatorTab(ctx, m, in); err != nil {
		return Result{}, err
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
		if !closedPageGone(err) {
			lines = append(lines, "warning: releasing the lease of the closed tab: "+err.Error())
		} else {
			// The tab is no longer listed, and on iwdp its page connection
			// ends with it (close 1001, "Page 9224/0 not found"), so the
			// page-side cleanup of the release cannot be sent. Nothing of
			// the session is left on a page that is gone: a log line only.
			label := id
			if label == "" {
				label = "(the current page)"
			}
			m.Logf("tab close: closed tab %s: the lease was released, but its page-side cleanup was not sent because the page is gone: %v", logsafe.ID(label), logsafe.Error(err))
		}
	}
	return Result{Text: strings.Join(lines, "\n"), Data: map[string]any{"closed": key}}, nil
}

// refuseUnselectedSimulatorTab refuses "tab close" without a tab id in a
// session that selected no tab, when the page it would close is the
// simulator's and the caller holds the simulator's turn: that page is the
// session's default page or the listing's first, which can be an earlier
// agent's tab (an agent ends its turn with "tab close" also after a failed
// sim up or selection). Elsewhere, and on a free simulator, the current
// page stays the default.
func refuseUnselectedSimulatorTab(ctx context.Context, m *core.Manager, in Input) error {
	if _, selected := m.CurrentPage(in.Global.Session); selected {
		return nil
	}
	ao := acquireFor(in)
	lp, _, err := m.ResolvePage(ctx, ao.Session, ao.Reservation, tabRequest(in, ""))
	if err != nil {
		return err
	}
	if lp.Device.ID != core.SimulatorDevice || lp.Physical {
		return nil
	}
	if mine, err := m.HoldsSimulator(ao); err != nil || !mine {
		return err
	}
	return &core.CodedError{Code: core.CodeNoPage, Err: fmt.Errorf("%w: tab close: this session has selected no tab, and the simulator's tab that it would close may be another agent's: name your tab ('agent-ios tab close <id>'); when you opened none, run only 'agent-ios sim release'", core.ErrNoPage)}
}

// tabDevice marks a tab of the simulator, so that an agent with
// reservations of several devices tells the simulator's tabs from a
// phone's (live run 18: an agent on a physical iPhone counted the
// simulator's tabs as the phone's). Other tabs stay unmarked.
func tabDevice(device string) string {
	if device == core.SimulatorDevice {
		return "  (simulator)"
	}
	return ""
}
