package cdp

import (
	"context"
	"errors"
	"time"

	"github.com/edxeth/agent-ios/internal/logsafe"
)

// The Target domain (design 5.2 and 5.3): each page in the record's scope
// is a tab target with one child page target. Filters apply exactly as the
// client gives them, with Chrome's default filter when none is given.

func init() {
	all := OnBrowser | OnTab | OnPage
	register(
		Method{Name: "Target.setDiscoverTargets", Levels: OnBrowser, Handle: setDiscoverTargets},
		Method{Name: "Target.getTargets", Levels: all, Handle: getTargets},
		Method{Name: "Target.getTargetInfo", Levels: all, Handle: getTargetInfo},
		Method{Name: "Target.setAutoAttach", Levels: all, Handle: setAutoAttach},
		Method{Name: "Target.attachToTarget", Levels: all, Handle: attachToTarget},
		Method{Name: "Target.detachFromTarget", Levels: all, Handle: detachFromTarget},
		Method{Name: "Target.createTarget", Levels: OnBrowser, Handle: createTarget},
		Method{Name: "Target.closeTarget", Levels: all, Handle: closeTarget},
		Method{Name: "Target.activateTarget", Levels: all, Handle: activateTarget},
		Method{Name: "Target.getBrowserContexts", Levels: OnBrowser, Handle: getBrowserContexts},
		// Clients send it on every attached session (Puppeteer and
		// Playwright fail attach without it, proof P2 item 3); no target
		// ever waits for a debugger here.
		Method{Name: "Runtime.runIfWaitingForDebugger", Levels: OnTab | OnPage},
	)
	registerEvents("Target", "targetCreated", "targetDestroyed", "targetInfoChanged", "attachedToTarget", "detachedFromTarget")
}

// filterEntry is a CDP TargetFilter entry.
type filterEntry struct {
	Type    string `json:"type,omitempty"`
	Exclude bool   `json:"exclude,omitempty"`
}

// filter is a CDP TargetFilter: the first entry whose type matches (or
// that has no type) decides; no match excludes.
type filter []filterEntry

// chromeDefault applies when a client gives no filter: everything except
// browser and tab targets.
var chromeDefault = filter{{Type: "browser", Exclude: true}, {Type: "tab", Exclude: true}, {}}

func (f filter) allows(typ string) bool {
	for _, e := range f {
		if e.Type == "" || e.Type == typ {
			return !e.Exclude
		}
	}
	return false
}

func filterOf(p *[]filterEntry) filter {
	if p == nil {
		return chromeDefault
	}
	return *p
}

// kinds are the target kinds of one page, in announcement order.
var kinds = []string{kindTab, kindPage}

// browserInfo is the TargetInfo of the browser target.
func (c *conn) browserInfo() map[string]any {
	return map[string]any{
		"targetId": stableID("browser", c.rec.id), "type": "browser", "title": "", "url": "",
		"attached": true, "canAccessOpener": false,
	}
}

func setDiscoverTargets(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Discover bool           `json:"discover"`
		Filter   *[]filterEntry `json:"filter"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	c := call.conn
	f := filterOf(p.Filter)
	c.discMu.Lock()
	defer c.discMu.Unlock()
	if !p.Discover {
		c.mu.Lock()
		c.discover, c.discovered, c.reported = false, nil, nil
		c.mu.Unlock()
		return nil, nil, nil
	}
	ts, err := c.rec.targets(ctx)
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	if !c.discover {
		c.discovered, c.reported = map[string]target{}, map[string]bool{}
	}
	c.discover, c.discoverFilter = true, f
	for _, t := range ts {
		if _, ok := c.discovered[t.pageID]; !ok {
			c.discovered[t.pageID] = t
		}
	}
	c.mu.Unlock()
	// A repeated call announces the targets that its filter newly allows
	// (Chrome, target_handler.cc SetDiscoverTargets: "Report the newly
	// matching targets that were not yet reported").
	c.announce(ts)
	c.startPolling()
	return nil, nil, nil
}

// targetKind is one target id: a page's tab or page target.
type targetKind struct {
	t    target
	kind string
}

// announce sends targetCreated for each kind of ts that the discovery
// filter allows and that was not announced yet. The caller holds discMu.
func (c *conn) announce(ts []target) {
	var out []targetKind
	c.mu.Lock()
	for _, t := range ts {
		for _, kind := range kinds {
			if id := t.id(kind); c.discoverFilter.allows(kind) && !c.reported[id] {
				c.reported[id] = true
				out = append(out, targetKind{t, kind})
			}
		}
	}
	c.mu.Unlock()
	for _, x := range out {
		c.event("", "Target.targetCreated", map[string]any{"targetInfo": c.targetInfo(x.t, x.kind)})
	}
}

func getTargets(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Filter *[]filterEntry `json:"filter"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	c := call.conn
	f := chromeDefault
	c.mu.Lock()
	if c.discover {
		f = c.discoverFilter // Chrome uses the discovery filter when none is given
	}
	c.mu.Unlock()
	if p.Filter != nil {
		f = *p.Filter
	}
	ts, err := c.rec.targets(ctx)
	if err != nil {
		return nil, nil, err
	}
	infos := []map[string]any{}
	for _, t := range ts {
		for _, kind := range kinds {
			if f.allows(kind) {
				infos = append(infos, c.targetInfo(t, kind))
			}
		}
	}
	return map[string]any{"targetInfos": infos}, nil, nil
}

func getTargetInfo(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		TargetID string `json:"targetId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	c := call.conn
	switch {
	case p.TargetID == "" && call.session == nil, p.TargetID == stableID("browser", c.rec.id):
		// Without an id on the browser session: the browser (design 5.2).
		return map[string]any{"targetInfo": c.browserInfo()}, nil, nil
	case p.TargetID == "":
		s := call.session
		return map[string]any{"targetInfo": c.targetInfo(s.t, s.kind)}, nil, nil
	}
	t, kind, err := c.rec.find(ctx, p.TargetID)
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{"targetInfo": c.targetInfo(t, kind)}, nil, nil
}

// setAutoAttach on the browser session attaches every existing target the
// filter allows, before its reply (Puppeteer counts the initial targets
// only then, proof P2 item 6), and later targets as the page list shows
// them. On a tab session it attaches the tab's page. A page leased by
// another session is never attached. Each call reconsiders the existing
// targets with its filter, and autoAttach:false detaches the sessions that
// auto-attach created, as Chrome does (Target.pdl setAutoAttach;
// target_handler.cc SetAutoAttachInternal); attachToTarget sessions stay.
func setAutoAttach(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		AutoAttach             bool           `json:"autoAttach"`
		WaitForDebuggerOnStart bool           `json:"waitForDebuggerOnStart"`
		Flatten                bool           `json:"flatten"`
		Filter                 *[]filterEntry `json:"filter"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if p.AutoAttach && !p.Flatten {
		return nil, nil, serverError("only flat sessions are supported: send Target.setAutoAttach with flatten: true")
	}
	c := call.conn
	f := filterOf(p.Filter)
	switch s := call.session; {
	case s == nil:
		if !p.AutoAttach {
			c.attachMu.Lock()
			c.mu.Lock()
			c.autoAttach, c.autoFilter, c.tried = false, nil, map[string]bool{}
			var auto []string
			for id, as := range c.sessions {
				if as.auto && as.parent == "" {
					auto = append(auto, id)
				}
			}
			c.mu.Unlock()
			c.attachMu.Unlock()
			for _, id := range auto {
				c.detach(id, true)
			}
			return nil, nil, nil
		}
		ts, err := c.rec.targets(ctx)
		if err != nil {
			return nil, nil, err
		}
		c.attachMu.Lock()
		c.mu.Lock()
		c.autoAttach, c.autoFilter, c.tried = true, f, map[string]bool{}
		c.mu.Unlock()
		c.autoAttachLocked(ctx, ts)
		c.attachMu.Unlock()
		c.startPolling()
	case s.kind == kindTab:
		c.attachMu.Lock()
		c.mu.Lock()
		live := c.sessions[s.id] == s
		autoPage := ""
		for _, ch := range s.children {
			if cs := c.sessions[ch]; cs != nil && cs.auto && cs.kind == kindPage {
				autoPage = ch
			}
		}
		c.mu.Unlock()
		if !live {
			c.attachMu.Unlock()
			return nil, nil, errDetached
		}
		if !p.AutoAttach {
			c.attachMu.Unlock()
			if autoPage != "" {
				c.detach(autoPage, true)
			}
			return nil, nil, nil
		}
		if autoPage != "" || !f.allows(kindPage) {
			c.attachMu.Unlock()
			return nil, nil, nil
		}
		ps, err := c.attach(ctx, s, kindPage, s.t, true)
		c.attachMu.Unlock()
		if err != nil {
			return nil, nil, err
		}
		c.announceAttached(ps)
	}
	// A page session has no child targets in v1 (no workers, no
	// cross-origin frame targets, design 5.2).
	return nil, nil, nil
}

// autoAttachTargets attaches the targets of ts that browser auto-attach
// has not handled since its last setAutoAttach, when it is on.
func (c *conn) autoAttachTargets(ctx context.Context, ts []target) {
	c.attachMu.Lock()
	defer c.attachMu.Unlock()
	c.autoAttachLocked(ctx, ts)
}

// autoAttachLocked attaches each kind of each target of ts that the
// auto-attach filter allows and that has no auto-attached browser-level
// session yet. A target is handled once per setAutoAttach: a page that
// another session leases is listed but never attached (design 5.2), and
// a refused attach is logged, not retried; a session that the client
// detaches is not attached again by the page-list poll. The caller holds
// attachMu, so the configuration cannot change meanwhile.
func (c *conn) autoAttachLocked(ctx context.Context, ts []target) {
	m := c.s.o.Manager
	for _, t := range ts {
		holder, epoch, leased := m.LeaseOf(t.key)
		c.mu.Lock()
		on, f, done := c.autoAttach, c.autoFilter, c.tried[t.key]
		if on {
			c.tried[t.key] = true
		}
		// A session of an ended lease does not count: its lease watcher
		// is detaching it.
		has := map[string]bool{}
		for _, s := range c.sessions {
			if s.auto && s.parent == "" && s.t.key == t.key && leased && holder == c.rec.session && s.epoch == epoch {
				has[s.kind] = true
			}
		}
		c.mu.Unlock()
		if !on || done {
			continue
		}
		if leased && holder != c.rec.session {
			c.s.o.Logf("cdp: %s: not attaching page %s: session %q holds its lease", logsafe.ID(c.name), logsafe.ID(t.pageID), logsafe.ID(holder))
			continue
		}
		for _, kind := range kinds {
			if !f.allows(kind) || has[kind] {
				continue
			}
			s, err := c.attach(ctx, nil, kind, t, true)
			if err != nil {
				c.s.o.Logf("cdp: %s: auto-attach of page %s: %s", logsafe.ID(c.name), logsafe.ID(t.pageID), logsafe.Error(err))
				break
			}
			c.announceAttached(s)
		}
	}
}

func attachToTarget(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		TargetID string `json:"targetId"`
		Flatten  bool   `json:"flatten"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if !p.Flatten {
		return nil, nil, serverError("only flat sessions are supported: send Target.attachToTarget with flatten: true")
	}
	c := call.conn
	t, kind, err := c.rec.find(ctx, p.TargetID)
	if err != nil {
		return nil, nil, err
	}
	var parent *session // the root of a page connection has no session id
	if call.SessionID != "" {
		parent = call.session
	}
	c.attachMu.Lock()
	s, err := c.attach(ctx, parent, kind, t, false)
	c.attachMu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	c.announceAttached(s)
	return map[string]any{"sessionId": s.id}, nil, nil
}

func detachFromTarget(_ context.Context, call *Call) (any, Later, error) {
	var p struct {
		SessionID string `json:"sessionId"`
		TargetID  string `json:"targetId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	c := call.conn
	var ids []string
	c.mu.Lock()
	for id, s := range c.sessions {
		if (p.SessionID != "" && id == p.SessionID) || (p.SessionID == "" && p.TargetID != "" && s.targetID() == p.TargetID) {
			ids = append(ids, id)
		}
	}
	c.mu.Unlock()
	if len(ids) == 0 {
		return nil, nil, serverError("No session with given id")
	}
	for _, id := range ids {
		c.detach(id, true)
	}
	return nil, nil, nil
}

func createTarget(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		URL string `json:"url"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	t, err := call.conn.rec.createTarget(ctx, p.URL)
	if err != nil {
		return nil, nil, err
	}
	call.conn.refresh(ctx)
	return map[string]any{"targetId": t.pageID}, nil, nil
}

func closeTarget(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		TargetID string `json:"targetId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	if err := call.conn.rec.closeTarget(ctx, p.TargetID); err != nil {
		return nil, nil, err
	}
	call.conn.refresh(ctx)
	return map[string]any{"success": true}, nil, nil
}

func activateTarget(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		TargetID string `json:"targetId"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	return nil, nil, call.conn.rec.activateTarget(ctx, p.TargetID)
}

// getBrowserContexts lists the non-default contexts, as Chrome does:
// there are none (design 4.3).
func getBrowserContexts(context.Context, *Call) (any, Later, error) {
	return map[string]any{"browserContextIds": []string{}}, nil, nil
}

// ---- page list polling ----

// startPolling reads the page list every PollEvery while the connection
// lives, once discovery or browser auto-attach is on (design 5.3).
func (c *conn) startPolling() {
	c.mu.Lock()
	if c.polling || c.closing {
		c.mu.Unlock()
		return
	}
	c.polling = true
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		t := time.NewTicker(c.s.o.PollEvery)
		defer t.Stop()
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(c.ctx, callTimeout)
				c.refresh(ctx)
				cancel()
			}
		}
	}()
}

// refresh compares the page list with what the connection knows: new
// targets are announced (discovery) and attached (auto-attach), changed
// ones get targetInfoChanged, and gone ones detach their sessions and get
// targetDestroyed. Info changes and destruction are sent for the target
// ids that were announced.
func (c *conn) refresh(ctx context.Context) {
	ts, err := c.rec.targets(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			c.s.o.Logf("cdp: %s: listing the pages: %v", logsafe.ID(c.name), logsafe.Error(err))
		}
		return
	}
	cur := map[string]target{}
	keys := map[string]bool{}
	for _, t := range ts {
		cur[t.pageID] = t
		keys[t.key] = true
	}
	c.discMu.Lock()
	var changed []targetKind
	var destroyed, dead []string // target ids, CDP session ids
	c.mu.Lock()
	discover := c.discover
	if discover {
		for _, t := range ts {
			old, ok := c.discovered[t.pageID]
			if ok && (old.title != t.title || old.url != t.url) {
				for _, kind := range kinds {
					if c.reported[t.id(kind)] {
						changed = append(changed, targetKind{t, kind})
					}
				}
			}
			c.discovered[t.pageID] = t
		}
		for id, t := range c.discovered {
			if _, ok := cur[id]; ok {
				continue
			}
			delete(c.discovered, id)
			for _, kind := range []string{kindPage, kindTab} {
				if tid := t.id(kind); c.reported[tid] {
					destroyed = append(destroyed, tid)
					delete(c.reported, tid)
				}
			}
		}
	}
	for id, s := range c.sessions {
		if _, ok := cur[s.t.pageID]; !ok && c.sessions[s.parent] == nil {
			dead = append(dead, id)
		}
	}
	for k := range c.tried {
		if !keys[k] {
			delete(c.tried, k)
		}
	}
	c.mu.Unlock()
	if discover {
		c.announce(ts)
	}
	for _, x := range changed {
		c.event("", "Target.targetInfoChanged", map[string]any{"targetInfo": c.targetInfo(x.t, x.kind)})
	}
	for _, id := range dead {
		c.detach(id, true)
	}
	for _, id := range destroyed {
		c.event("", "Target.targetDestroyed", map[string]any{"targetId": id})
	}
	c.discMu.Unlock()
	c.autoAttachTargets(ctx, ts)
}
