package cdp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// puppeteerAttach attaches the tab, then its page, as Puppeteer does, and
// returns the tab and page session ids.
func puppeteerAttach(t *testing.T, c *cdptest.Client) (string, string) {
	t.Helper()
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
	att := eventsOf(c, "Target.attachedToTarget")
	tab := decode[attached](t, att[len(att)-1].Params).SessionID
	call(t, c, tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	att = eventsOf(c, "Target.attachedToTarget")
	return tab, decode[attached](t, att[len(att)-1].Params).SessionID
}

// Design 7.8 item 11 (b) and (c): the tab and page attachments have
// distinct session ids; detaching the page attachment removes only the
// state it set (its extra header goes, the session's legacy header stays)
// and leaves the lease with s1; a second connection through the same URL
// joins s1 without a lease change.
func TestAttachmentStateAndSharedSession(t *testing.T) {
	r := newRig(t)
	req := r.req
	req.Page = "One"
	ao := core.AcquireOptions{Session: "s1", Reservation: "o1"}
	mo, err := r.m.Begin(ctxFor(t, 5*time.Second), req, ao, "legacy", core.ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.RawAs(ctxFor(t, 5*time.Second), mo, core.SessionOwner("legacy", mo.Session), "Network.setExtraHTTPHeaders", []byte(`{"headers":{"X-Legacy":"1"}}`)); err != nil {
		t.Fatal(err)
	}
	r.m.End(mo.ID)
	_, epoch := r.leaseOf(t, "One")
	ep := r.issue(t, "s1", "One", ScopePage)
	c := dial(t, ep.WS)
	tab, page := puppeteerAttach(t, c)
	if tab == page || tab == "" {
		t.Fatalf("tab session %q and page session %q must differ", tab, page)
	}
	call(t, c, page, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"X-CDP": "1"}})
	if h := r.m.LookupPage(mo.Page.Key).PS.State.EffectiveHeaders(); h["X-CDP"] != "1" || h["X-Legacy"] != "1" {
		t.Fatalf("effective headers %v, want both", h)
	}
	f := r.fake["One"]
	mark := len(f.Received())
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": page})
	if _, err := c.WaitEvent(ctxFor(t, 5*time.Second), 0, "Target.detachedFromTarget", nil); err != nil {
		t.Fatal(err)
	}
	h := r.m.LookupPage(mo.Page.Key).PS.State.EffectiveHeaders()
	if _, ok := h["X-CDP"]; ok || h["X-Legacy"] != "1" {
		t.Fatalf("effective headers after the detach %v, want X-Legacy only", h)
	}
	var after []string
	for _, rec := range f.Received()[mark:] {
		after = append(after, rec.Method+" "+string(rec.Params))
	}
	joined := strings.Join(after, "\n")
	if !strings.Contains(joined, "Network.disable") || strings.Contains(joined, "X-CDP") || !strings.Contains(joined, "X-Legacy") {
		t.Fatalf("after the detach the page received:\n%s\nwant the Network reinitialization with X-Legacy only", joined)
	}
	if s, e := r.leaseOf(t, "One"); s != "s1" || e != epoch {
		t.Fatalf("lease after the detach %s/%d, want s1/%d", s, e, epoch)
	}
	// (c) A second connection joins s1 without a lease change.
	c2 := dial(t, ep.WS)
	_, page2 := puppeteerAttach(t, c2)
	if s, e := r.leaseOf(t, "One"); s != "s1" || e != epoch || page2 == page {
		t.Fatalf("lease after a second connection %s/%d, want s1/%d", s, e, epoch)
	}
	// The tab of the first connection still evaluates through a new page attach.
	call(t, c, tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	att := eventsOf(c, "Target.attachedToTarget")
	call(t, c, decode[attached](t, att[len(att)-1].Params).SessionID, "Runtime.evaluate", map[string]any{"expression": "1"})
}

// A lease break ends the attachments of the session's connections: they
// get detachedFromTarget, and later page messages are refused.
func TestLeaseEndDetaches(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	key := ""
	for _, ps := range r.m.Status() {
		key = ps.Key
	}
	if err := r.m.Break(ctxFor(t, 5*time.Second), key); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitEvent(ctxFor(t, 5*time.Second), 0, "Target.detachedFromTarget", func(e cdptest.Event) bool {
		return strings.Contains(string(e.Params), page)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctxFor(t, 5*time.Second), page, "Runtime.evaluate", map[string]any{"expression": "1"}); err == nil {
		t.Fatal("a detached page session still evaluates")
	}
	if s, _ := r.leaseOf(t, "One"); s != "" {
		t.Fatalf("the detach leased the page again for %q", s)
	}
}

// Browser domain: getVersion names Safari and the iOS version; the user
// agent is read only under the session's own lease; Browser.close ends
// the connection after its reply and keeps the lease.
func TestBrowserDomain(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	v := decode[struct{ Product, UserAgent, ProtocolVersion string }](t, call(t, c, "", "Browser.getVersion", nil))
	if v.Product != "Safari/16.7.10" || v.UserAgent != "" || v.ProtocolVersion != "1.3" {
		t.Fatalf("getVersion before attach %+v", v)
	}
	if n := len(r.fake["One"].Received()); n != 0 {
		t.Fatalf("getVersion before attach sent %d messages to the page", n)
	}
	puppeteerAttach(t, c)
	r.fake["One"].Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "string", "value": "Mozilla/5.0 (iPhone)"}})
	v = decode[struct{ Product, UserAgent, ProtocolVersion string }](t, call(t, c, "", "Browser.getVersion", nil))
	if v.UserAgent != "Mozilla/5.0 (iPhone)" {
		t.Fatalf("getVersion after attach %+v", v)
	}
	call(t, c, "", "Browser.setDownloadBehavior", map[string]any{"behavior": "deny"})
	if _, err := c.Call(ctxFor(t, 5*time.Second), "", "Browser.grantPermissions", map[string]any{}); err == nil {
		t.Fatal("grantPermissions succeeded")
	}
	call(t, c, "", "Browser.close", nil)
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Browser.close did not end the connection")
	}
	if s, _ := r.leaseOf(t, "One"); s != "s1" {
		t.Fatalf("lease after Browser.close %q, want s1", s)
	}
}

// A page connection (/devtools/page/<id>) serves the page at its root; a
// page that another session leases is refused before the upgrade.
func TestPageConnection(t *testing.T) {
	r := newRig(t)
	ep := r.issue(t, "s1", "One", ScopeDevice)
	code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil)
	if code != 200 || !strings.Contains(body, "/devtools/page/") || strings.Contains(body, "Three") {
		t.Fatalf("/json/list: %d %s", code, body)
	}
	list := decode[[]struct{ ID, Title, WebSocketDebuggerURL string }](t, []byte(body))
	urls := map[string]string{}
	for _, e := range list {
		urls[e.Title] = e.WebSocketDebuggerURL
	}
	c := dial(t, urls["One"])
	r.fake["One"].Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 3}})
	if v := decode[struct{ Result map[string]any }](t, call(t, c, "", "Runtime.evaluate", map[string]any{"expression": "3"})); v.Result["value"] != float64(3) {
		t.Fatalf("evaluate on the page connection %v", v)
	}
	req := r.req
	req.Page = "Two"
	if _, _, err := r.m.Touch(ctxFor(t, 5*time.Second), req, core.AcquireOptions{Session: "s2", Reservation: "o1"}); err != nil {
		t.Fatal(err)
	}
	_, resp, err := cdptest.Dial(ctxFor(t, 5*time.Second), urls["Two"], nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("page connection to a page leased by s2: %v, want 409", err)
	}
}

// Tab actions: createTarget opens a tab with window.open in the selected
// page, and it joins the scope; closeTarget runs window.close() and waits
// until the list drops the tab; activateTarget is a probe.
func TestTabActions(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	opened := false
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if strings.Contains(string(c.Params), "window.open") {
			opened = true
			r.addPage(t, r.devA, "Opened")
			return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
	})
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": true})
	res := decode[struct{ TargetID string }](t, call(t, c, "", "Target.createTarget", map[string]any{"url": "https://opened.example/"}))
	if !opened || res.TargetID == "" {
		t.Fatalf("createTarget = %+v", res)
	}
	if _, err := c.WaitEvent(ctxFor(t, 5*time.Second), 0, "Target.targetCreated", func(e cdptest.Event) bool {
		return strings.Contains(string(e.Params), res.TargetID)
	}); err != nil {
		t.Fatal(err)
	}
	r.fake["Opened"].Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		r.devA.RemovePage(r.listed["Opened"])
		return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
	})
	call(t, c, "", "Target.closeTarget", map[string]any{"targetId": res.TargetID})
	if _, err := c.WaitEvent(ctxFor(t, 5*time.Second), 0, "Target.targetDestroyed", nil); err != nil {
		t.Fatal(err)
	}
	tis := decode[struct{ TargetInfos []targetInfo }](t, call(t, c, "", "Target.getTargets", nil)).TargetInfos
	call(t, c, "", "Target.activateTarget", map[string]any{"targetId": tis[0].TargetID})
	r.fake["One"].Stall("Runtime.evaluate")
	start := time.Now()
	_, err := c.Call(ctxFor(t, 10*time.Second), "", "Target.activateTarget", map[string]any{"targetId": tis[0].TargetID})
	if err == nil || !strings.Contains(err.Error(), "not answering") || time.Since(start) > 4*time.Second {
		t.Fatalf("activateTarget on a silent tab: %v after %v", err, time.Since(start))
	}
}

// pageSession returns the server side of the CDP session sid of the only
// connection of the rig.
func (r *rig) pageSession(t *testing.T, sid string) (*conn, *session) {
	t.Helper()
	var co *conn
	r.srv.mu.Lock()
	for cc := range r.srv.conns {
		co = cc
	}
	r.srv.mu.Unlock()
	if co == nil || co.session(sid) == nil {
		t.Fatalf("no connection with CDP session %s", sid)
	}
	return co, co.session(sid)
}

// Review finding 1 (W5a targets review, TestReviewDetachQueuedHeader):
// detaching fences the attachment. A command of the attachment that waits
// in the page's queue while the attachment detaches is never run, so it
// cannot add state that outlives the attachment (design 5.2: detaching
// releases the attachment's entries).
func TestDetachFencesQueuedCommands(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	sid := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	_, s := r.pageSession(t, sid)
	// An exclusive operation of the session holds the page, so the header
	// call waits in the queue.
	mo, err := r.m.BeginLeased(ctxFor(t, 5*time.Second), s.pg, "s1", s.epoch, "test blocker", core.ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	ended := false
	defer func() {
		if !ended {
			r.m.End(mo.ID)
		}
	}()
	before := r.m.Status()[0].LastUsed
	result := make(chan error, 1)
	go func() {
		_, err := c.Call(ctxFor(t, 10*time.Second), sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"X-After-Detach": "leaked"}})
		result <- err
	}()
	eventually(t, 5*time.Second, "the header call queues behind the blocker", func() bool { return r.m.Status()[0].LastUsed.After(before) })
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
	r.m.End(mo.ID)
	ended = true
	err = <-result
	if h := s.pg.PS.State.EffectiveHeaders(); h["X-After-Detach"] != "" {
		t.Fatalf("the detached attachment left effective headers %v", h)
	}
	for _, rec := range r.fake["One"].ReceivedMethod("Network.setExtraHTTPHeaders") {
		if strings.Contains(string(rec.Params), "X-After-Detach") {
			t.Fatalf("the page received the header of a detached attachment: %s", rec.Params)
		}
	}
	if err == nil {
		t.Fatal("the queued command of a detached attachment succeeded")
	}
	for _, o := range s.pg.PS.State.AllOwners() {
		if o == s.owner {
			t.Fatalf("the owner %v of the detached attachment still has state", o)
		}
	}
}

// Review finding 2 (TestReviewAutoAttachChildAfterParentDetach): a child
// attach that is in flight when its parent detaches is refused; no child
// outlives its parent.
func TestChildAttachAfterParentDetach(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
	tab := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	co, _ := r.pageSession(t, tab)
	// Holding the attach mutex stops the tab's setAutoAttach after its
	// dispatch resolved the tab session and before the child attach.
	co.attachMu.Lock()
	result := make(chan error, 1)
	go func() {
		_, err := c.Call(ctxFor(t, 10*time.Second), tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
		result <- err
	}()
	time.Sleep(100 * time.Millisecond) // lets the call reach the attach mutex
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": tab})
	co.attachMu.Unlock()
	err := <-result
	if n := co.attachments(); n != 0 {
		t.Fatalf("%d attachments survive the detach of their parent", n)
	}
	if n := len(eventsOf(c, "Target.attachedToTarget")); n != 1 {
		t.Fatalf("%d attachedToTarget events, want only the tab's", n)
	}
	if err == nil {
		t.Fatal("the child attach of a detached tab succeeded")
	}
}

// Review finding 5 (TestReviewCloseBeforeRun): a connection that the
// server closes before its WebSocket loop runs is finalized once: Enter's
// leave runs exactly once (the daemon counts active API work with it).
func TestCloseBeforeRunFinalizesOnce(t *testing.T) {
	r := newRig(t)
	r.issue(t, "s1", "One", ScopePage)
	var rec *record
	r.srv.mu.Lock()
	for _, rr := range r.srv.records {
		rec = rr
	}
	r.srv.mu.Unlock()
	var leaves atomic.Int32
	r.srv.o.Enter = func() func() { return func() { leaves.Add(1) } }
	co := r.srv.newConn(rec)
	// Stop or RevokeSession can close a connection before serveWS runs it.
	co.close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		up := websocket.Upgrader{}
		ws, err := up.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		co.run(ws)
	}))
	defer srv.Close()
	ctx := ctxFor(t, 5*time.Second)
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.Close() }()
	select {
	case <-co.done:
	case <-ctx.Done():
		t.Fatal("run did not finish")
	}
	if n := leaves.Load(); n != 1 {
		t.Fatalf("Enter's leave ran %d times for one connection, want once", n)
	}
}

// The fence (review finding 1): after a detach, the attachment's begin is
// refused before it reaches the page's lease, whatever context its
// caller gives (the lease stays with s1, so only the fence refuses it).
func TestBeginRefusedAfterDetach(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	sid := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	_, s := r.pageSession(t, sid)
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
	before := r.m.Status()[0].LastUsed
	mo, end, err := s.begin(ctxFor(t, 5*time.Second), "late", core.ClassRead)
	if err != errDetached || mo != nil || end != nil {
		t.Fatalf("begin after the detach = %v, %v; want errDetached", mo, err)
	}
	if after := r.m.Status()[0].LastUsed; !after.Equal(before) {
		t.Fatal("begin after the detach reached the page's lease")
	}
}

// The publication check (review finding 2): detach removes a parent from
// the connection before it cancels the parent's context, so an attach
// that passed its early check must still be refused when its parent is
// gone at publication. The parent here is in exactly that state.
func TestAttachRefusedWhenParentGone(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
	tab := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	co, s := r.pageSession(t, tab)
	co.mu.Lock()
	delete(co.sessions, tab)
	co.mu.Unlock()
	defer func() { // put the tab back so the connection's cleanup detaches it
		co.mu.Lock()
		co.sessions[tab] = s
		co.mu.Unlock()
	}()
	ps, err := co.attach(ctxFor(t, 5*time.Second), s, kindPage, s.t, true)
	if err != errDetached || ps != nil {
		t.Fatalf("attach below a removed parent = %v, %v; want errDetached", ps, err)
	}
	if n := co.attachments(); n != 0 || len(s.children) != 0 {
		t.Fatalf("the refused attach left %d attachments and children %v", n, s.children)
	}
}

// The early check (review finding 2): an attach below a parent that has
// detached is refused before it touches the page.
func TestAttachRefusedBelowDetachedParent(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
	tab := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	co, s := r.pageSession(t, tab)
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": tab})
	before := r.m.Status()[0].LastUsed
	ps, err := co.attach(ctxFor(t, 5*time.Second), s, kindPage, s.t, true)
	if err != errDetached || ps != nil {
		t.Fatalf("attach below a detached parent = %v, %v; want errDetached", ps, err)
	}
	if after := r.m.Status()[0].LastUsed; !after.Equal(before) || co.attachments() != 0 {
		t.Fatal("the attach below a detached parent touched the page or attached")
	}
}

// attachToTarget sent on a tab session attaches below that session: the
// attachedToTarget event goes to the tab, and detaching the tab detaches
// the child too.
func TestAttachToTargetBelowTab(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}})
	tab := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params)
	tis := decode[struct{ TargetInfos []targetInfo }](t, call(t, c, "", "Target.getTargets", map[string]any{"filter": []any{map[string]any{"type": "page"}}})).TargetInfos
	pageID := tis[0].TargetID
	child := decode[struct{ SessionID string }](t, call(t, c, tab.SessionID, "Target.attachToTarget", map[string]any{"targetId": pageID, "flatten": true})).SessionID
	att := eventsOf(c, "Target.attachedToTarget")
	if len(att) != 2 || att[1].SessionID != tab.SessionID || !strings.Contains(string(att[1].Params), child) {
		t.Fatalf("attachedToTarget events %+v, want the child's on the tab session", att)
	}
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": tab.SessionID})
	co := r.onlyConn(t)
	if n := co.attachments(); n != 0 {
		t.Fatalf("%d attachments after detaching the tab, want none", n)
	}
	det := eventsOf(c, "Target.detachedFromTarget")
	if len(det) != 2 || det[0].SessionID != tab.SessionID || !strings.Contains(string(det[0].Params), child) {
		t.Fatalf("detachedFromTarget events %+v, want the child's on the tab first, then the tab's", det)
	}
}

// Review finding 2 (announcement on a dead session): an attachment that
// detaches between its attach and its announcement is never announced,
// and its detach sends no detachedFromTarget.
func TestDetachBeforeAnnouncement(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.getTargets", nil)
	co := r.onlyConn(t)
	ts, err := co.rec.targets(ctxFor(t, 5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	s, err := co.attach(ctxFor(t, 5*time.Second), nil, kindPage, ts[0], true)
	if err != nil {
		t.Fatal(err)
	}
	co.detach(s.id, true)
	co.announceAttached(s)
	call(t, c, "", "Target.getTargets", nil) // the events before this reply have arrived
	if n := len(eventsOf(c, "Target.attachedToTarget")) + len(eventsOf(c, "Target.detachedFromTarget")); n != 0 {
		t.Fatalf("%d attach or detach events for an attachment that detached before its announcement", n)
	}
}
