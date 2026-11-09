package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/logsafe"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
)

// Waits of the tab actions (window.open and window.close take effect
// asynchronously; the page list shows the result).
const (
	createWait = 5 * time.Second
	closeWait  = 3 * time.Second
	listPoll   = 100 * time.Millisecond
	probeWait  = 2 * time.Second
)

// record is an endpoint record (design 5.2): the logical session that
// every connection through it acts for, the page selected when it was
// issued, the reservation owner and the scope.
type record struct {
	s        *Server
	id       string
	session  string
	scope    string
	listing  core.PageRequest // transport and listing address that resolved the selected page (Page unused)
	selected core.ListedPage

	// ctx ends when the record is revoked (RevokeSession, Stop) and
	// cancels the requests admitted on it; work counts those requests
	// (Server.admit), so a revocation can wait for them (drain).
	ctx    context.Context
	cancel context.CancelFunc
	work   sync.WaitGroup
	// inflight holds the admitted requests, so that a revocation can close
	// their connections (Server.drain). Guarded by s.mu.
	inflight map[*request]bool

	mu          sync.Mutex
	reservation string          // reservation owner; a later Issue may change it (owner, setOwner)
	created     map[string]bool // keys of pages created through this record; they join its scope
}

// owner returns the reservation owner. Each decision reads it once.
func (rec *record) owner() string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.reservation
}

// setOwner sets the reservation owner (Issue for the same session, page
// and scope with another --reservation).
func (rec *record) setOwner(owner string) {
	rec.mu.Lock()
	rec.reservation = owner
	rec.mu.Unlock()
}

func (rec *record) ao() core.AcquireOptions {
	return core.AcquireOptions{Session: rec.session, Reservation: rec.owner()}
}

// target is one page in a record's scope. It is exposed as a tab target
// with one child page target (design 5.2), both with stable ids.
type target struct {
	key       string // page record key in the manager
	pageID    string // CDP page targetId
	tabID     string // CDP tab targetId
	title     string
	url       string
	osVersion string
	kind      transport.Kind
	req       core.PageRequest // selects exactly this page for Touch
}

func isURLKey(key string) bool { return strings.HasPrefix(key, "url|") }

func (rec *record) targetOf(lp core.ListedPage) target {
	req := rec.listing
	req.Page = lp.Key
	if isURLKey(lp.Key) {
		req.Page = lp.Page.ID // an explicit ws:// URL resolves by itself
	}
	return target{
		key: lp.Key, title: lp.Title, url: lp.URL, osVersion: lp.Device.OSVersion, kind: lp.Device.Transport, req: req,
		pageID: stableID("page", lp.Device.ID, lp.Page.ID), tabID: stableID("tab", lp.Device.ID, lp.Page.ID),
	}
}

// targets lists the pages in the record's scope, the selected page first:
// the selected page, the pages created through the record, and with scope
// device every debuggable page of a device that the record's reservation
// owner reserved. A page of an explicit ws:// URL has no listing; it is
// its own scope.
func (rec *record) targets(ctx context.Context) ([]target, error) {
	m := rec.s.o.Manager
	if isURLKey(rec.selected.Key) {
		t := rec.targetOf(rec.selected)
		for _, ps := range m.Status() {
			if ps.Key == t.key && ps.Title != "" {
				t.title = ps.Title
			}
		}
		return []target{t}, nil
	}
	pages, err := m.ListPages(ctx, rec.listing)
	if err != nil {
		return nil, err
	}
	reserved := rec.reservedDevices()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var first, rest []target
	for _, lp := range pages {
		switch {
		case lp.Key == rec.selected.Key:
			first = append(first, rec.targetOf(lp))
		case !lp.Page.Debuggable:
		case rec.created[lp.Key], rec.scope == ScopeDevice && reserved(lp.Device.ID):
			rest = append(rest, rec.targetOf(lp))
		}
	}
	return append(first, rest...), nil
}

// reservedDevices returns whether the record's reservation owner holds a
// live reservation that covers a device (scope device, design 5.2).
func (rec *record) reservedDevices() func(device string) bool {
	if rec.scope != ScopeDevice || rec.s.o.Reservations == nil {
		return func(string) bool { return false }
	}
	rs, err := rec.s.o.Reservations.List()
	if err != nil {
		rec.s.o.Logf("cdp: reading the device reservations: %v", logsafe.Error(err))
		return func(string) bool { return false }
	}
	now, owner := time.Now(), rec.owner()
	return func(device string) bool {
		for _, r := range rs {
			if r.Owner == owner && r.Live(now) && r.Covers(device) {
				return true
			}
		}
		return false
	}
}

// find returns the target with the CDP id (a page or a tab id) and its
// kind ("page" or "tab").
func (rec *record) find(ctx context.Context, id string) (target, string, error) {
	ts, err := rec.targets(ctx)
	if err != nil {
		return target{}, "", err
	}
	for _, t := range ts {
		switch id {
		case t.pageID:
			return t, kindPage, nil
		case t.tabID:
			return t, kindTab, nil
		}
	}
	return target{}, "", serverError("No target with given id found")
}

// product names the browser of the record for Browser.getVersion and
// /json/version (design 5.1).
func (rec *record) product() string {
	switch {
	case rec.selected.Device.Transport == transport.KindWebKitGTK:
		return "Safari/WebKitGTK"
	case rec.selected.Device.OSVersion != "":
		return "Safari/" + rec.selected.Device.OSVersion
	}
	return "Safari"
}

// evalIn runs one Runtime.evaluate in the page of t for the record's
// logical session (it takes or renews the session's lease, as any command
// of the session does) and returns the WIP result.
func (rec *record) evalIn(ctx context.Context, t target, name, expr string, gesture bool) (json.RawMessage, error) {
	m := rec.s.o.Manager
	ao := rec.ao()
	pg, epoch, err := m.Touch(ctx, t.req, ao)
	if err != nil {
		return nil, err
	}
	mo, err := m.BeginLeased(ctx, pg, rec.session, epoch, "cdp "+name, core.ClassMutate)
	if err != nil {
		return nil, err
	}
	defer m.End(mo.ID)
	return mo.Op.Caller().Send(mo.Op.Context(), "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "emulateUserGesture": gesture,
	})
}

// createTarget opens a tab with window.open in the selected page (the
// only way on WIP, design 3.8) and waits until the page list shows it.
// The new page joins the record's scope.
func (rec *record) createTarget(ctx context.Context, url string) (target, error) {
	if url == "" {
		url = "about:blank"
	}
	if isURLKey(rec.selected.Key) {
		return target{}, errors.New("a new tab needs a page from the transport's page list; issue the URL for a listed page, not a ws:// page URL")
	}
	m := rec.s.o.Manager
	before := map[string]bool{}
	pages, err := m.ListPages(ctx, rec.listing)
	if err != nil {
		return target{}, err
	}
	for _, p := range pages {
		before[p.Key] = true
	}
	ts, err := rec.targets(ctx)
	if err != nil {
		return target{}, err
	}
	if len(ts) == 0 || ts[0].key != rec.selected.Key {
		return target{}, fmt.Errorf("the selected page %q is gone; issue a new URL with 'iwdp-cli get cdp-url'", rec.selected.Title)
	}
	lit, _ := json.Marshal(url) // a string always marshals
	raw, err := rec.evalIn(ctx, ts[0], "Target.createTarget", fmt.Sprintf("!!window.open(%s, '_blank')", lit), true)
	if err != nil {
		return target{}, fmt.Errorf("window.open in %q: %w", rec.selected.Title, err)
	}
	if v := evalValue(raw); v != true {
		return target{}, fmt.Errorf("window.open(%s) in %q returned no window (a popup blocker or a missing user gesture); open the tab on the device", lit, rec.selected.Title)
	}
	end := time.Now().Add(createWait)
	for {
		pages, err := m.ListPages(ctx, rec.listing)
		if err != nil {
			return target{}, err
		}
		for _, p := range pages {
			if !before[p.Key] && p.Device.ID == rec.selected.Device.ID {
				rec.mu.Lock()
				if rec.created == nil {
					rec.created = map[string]bool{}
				}
				rec.created[p.Key] = true
				rec.mu.Unlock()
				return rec.targetOf(p), nil
			}
		}
		if time.Now().After(end) {
			return target{}, fmt.Errorf("window.open(%s) ran, but no new tab was listed within %s", lit, createWait)
		}
		select {
		case <-ctx.Done():
			return target{}, ctx.Err()
		case <-time.After(listPoll):
		}
	}
}

// evalValue is the value of a returnByValue WIP evaluate result, or nil.
func evalValue(raw json.RawMessage) any {
	var r struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		WasThrown bool `json:"wasThrown"`
	}
	if json.Unmarshal(raw, &r) != nil || r.WasThrown {
		return nil
	}
	return r.Result.Value
}

// closeTarget runs window.close() in the page and waits until the page
// list no longer shows it. Safari closes only windows that a script opened.
func (rec *record) closeTarget(ctx context.Context, id string) error {
	t, _, err := rec.find(ctx, id)
	if err != nil {
		return err
	}
	if _, err := rec.evalIn(ctx, t, "Target.closeTarget", "window.close(), true", false); err != nil {
		return fmt.Errorf("window.close in %q: %w", t.title, err)
	}
	if isURLKey(t.key) {
		return nil // no page list shows whether it closed
	}
	end := time.Now().Add(closeWait)
	for {
		pages, err := rec.s.o.Manager.ListPages(ctx, rec.listing)
		if err != nil {
			return err
		}
		listed := false
		for _, p := range pages {
			listed = listed || p.Key == t.key
		}
		if !listed {
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("tab %q is still open %s after window.close(); Safari closes only tabs that a script opened, so close it on the device", t.title, closeWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(listPoll):
		}
	}
}

// activateTarget is a probe (design 5.3: WIP cannot raise a tab): it
// fails when the page does not answer, which is how a hidden Safari tab
// behaves.
func (rec *record) activateTarget(ctx context.Context, id string) error {
	t, _, err := rec.find(ctx, id)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, probeWait)
	defer cancel()
	if _, err := rec.evalIn(pctx, t, "Target.activateTarget", "1", false); err != nil {
		if errors.Is(err, core.ErrLeased) || errors.Is(err, core.ErrNoReservation) {
			return err
		}
		return fmt.Errorf("tab %q is not answering; it is probably not the visible Safari tab. Bring it to the front on the device; WIP cannot raise a tab (%w)", t.title, err)
	}
	return nil
}
