package cdp

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

func eventsOf(c *cdptest.Client, method string) []cdptest.Event {
	var out []cdptest.Event
	for _, e := range c.Events() {
		if e.Method == method {
			out = append(out, e)
		}
	}
	return out
}

func createdTitles(t *testing.T, c *cdptest.Client) []string {
	t.Helper()
	var out []string
	for _, e := range eventsOf(c, "Target.targetCreated") {
		ti := decode[struct{ TargetInfo targetInfo }](t, e.Params).TargetInfo
		out = append(out, ti.Type+":"+ti.Title)
	}
	return out
}

// Design 7.8 item 3: a Puppeteer-style setAutoAttach whose filter excludes
// page at browser level attaches the tab, then (on the tab session) its
// page; existing targets are announced as created, then attached; each
// attachedToTarget comes before the reply of its setAutoAttach (proof P2
// item 6); by default only the record's selected page is announced.
func TestPuppeteerStyleAutoAttach(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": true, "filter": []any{map[string]any{}}})
	if got := strings.Join(createdTitles(t, c), ","); got != "tab:One,page:One" {
		t.Fatalf("targetCreated = %s, want the selected page only (tab:One,page:One)", got)
	}
	_, seq, err := c.CallSeq(ctxFor(t, 10*time.Second), "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []any{map[string]any{"type": "page", "exclude": true}, map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	att := eventsOf(c, "Target.attachedToTarget")
	if len(att) != 1 || att[0].Seq > seq || att[0].SessionID != "" {
		t.Fatalf("attachedToTarget events %+v (reply seq %d), want one tab attach on the browser session before the reply", att, seq)
	}
	tab := decode[attached](t, att[0].Params)
	if tab.TargetInfo.Type != "tab" || !tab.TargetInfo.Attached {
		t.Fatalf("browser auto-attach attached %+v, want the tab", tab.TargetInfo)
	}
	created := eventsOf(c, "Target.targetCreated")
	if created[0].Seq > att[0].Seq {
		t.Fatal("the tab was attached before it was announced as created")
	}
	_, seq, err = c.CallSeq(ctxFor(t, 10*time.Second), tab.SessionID, "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true, "filter": []any{map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	att = eventsOf(c, "Target.attachedToTarget")
	if len(att) != 2 || att[1].Seq > seq || att[1].SessionID != tab.SessionID {
		t.Fatalf("after the tab's setAutoAttach: %+v, want the page attached on the tab session before the reply", att)
	}
	page := decode[attached](t, att[1].Params)
	if page.TargetInfo.Type != "page" || page.TargetInfo.Title != "One" || page.SessionID == tab.SessionID {
		t.Fatalf("page attach %+v", page)
	}
	if s, e := r.leaseOf(t, "One"); s != "s1" || e == 0 {
		t.Fatalf("lease of One = %q/%d, want s1", s, e)
	}
	call(t, c, page.SessionID, "Runtime.runIfWaitingForDebugger", nil)
	r.fake["One"].Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 2, "description": "2"}})
	res := decode[struct{ Result map[string]any }](t, call(t, c, page.SessionID, "Runtime.evaluate", map[string]any{"expression": "1+1", "returnByValue": true}))
	if res.Result["value"] != float64(2) {
		t.Fatalf("Runtime.evaluate = %v", res.Result)
	}
	bi := decode[struct{ TargetInfo targetInfo }](t, call(t, c, "", "Target.getTargetInfo", nil)).TargetInfo
	pi := decode[struct{ TargetInfo targetInfo }](t, call(t, c, page.SessionID, "Target.getTargetInfo", nil)).TargetInfo
	if bi.Type != "browser" || pi.TargetID != page.TargetInfo.TargetID {
		t.Fatalf("getTargetInfo: browser %+v, page session %+v", bi, pi)
	}
}

// With no filter, Chrome's default filter applies: browser auto-attach
// attaches pages, not tabs; getTargets lists pages only.
func TestDefaultFilter(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	att := eventsOf(c, "Target.attachedToTarget")
	if len(att) != 1 || decode[attached](t, att[0].Params).TargetInfo.Type != "page" {
		t.Fatalf("default-filter auto-attach: %+v, want one page", att)
	}
	tis := decode[struct{ TargetInfos []targetInfo }](t, call(t, c, "", "Target.getTargets", nil)).TargetInfos
	if len(tis) != 1 || tis[0].Type != "page" || !tis[0].Attached {
		t.Fatalf("getTargets = %+v, want the attached page only", tis)
	}
	if _, err := c.Call(ctxFor(t, 5*time.Second), "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": false}); err == nil {
		t.Fatal("non-flat auto-attach succeeded")
	}
}

// Scope device adds the other pages of devices the reservation owner
// reserved (One and Two on device A), never device B's Three. A page that
// another session leases is listed and never attached; attachToTarget on
// it fails with the lease message and leaves that lease unchanged.
func TestDeviceScopeAndLeasedPage(t *testing.T) {
	r := newRig(t)
	req := r.req
	req.Page = "Two"
	if _, _, err := r.m.Touch(ctxFor(t, 5*time.Second), req, core.AcquireOptions{Session: "s2", Reservation: "o1"}); err != nil {
		t.Fatal(err)
	}
	_, e2 := r.leaseOf(t, "Two")
	c := dial(t, r.issue(t, "s1", "One", ScopeDevice).WS)
	call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": true})
	if got := strings.Join(createdTitles(t, c), ","); got != "page:One,page:Two" {
		t.Fatalf("device scope targetCreated = %s, want page:One,page:Two", got)
	}
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	var titles []string
	for _, e := range eventsOf(c, "Target.attachedToTarget") {
		titles = append(titles, decode[attached](t, e.Params).TargetInfo.Title)
	}
	if strings.Join(titles, ",") != "One" {
		t.Fatalf("auto-attached %v, want One only (Two is leased by s2)", titles)
	}
	var two targetInfo
	for _, ti := range decode[struct{ TargetInfos []targetInfo }](t, call(t, c, "", "Target.getTargets", nil)).TargetInfos {
		if ti.Title == "Two" {
			two = ti
		}
	}
	_, err := c.Call(ctxFor(t, 5*time.Second), "", "Target.attachToTarget", map[string]any{"targetId": two.TargetID, "flatten": true})
	if err == nil || !strings.Contains(err.Error(), "leased by session \"s2\"") {
		t.Fatalf("attachToTarget(Two) = %v, want the lease message", err)
	}
	if s, e := r.leaseOf(t, "Two"); s != "s2" || e != e2 {
		t.Fatalf("Two's lease after the refused attach: %s/%d, want s2/%d", s, e, e2)
	}
	c2 := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c2, "", "Target.setDiscoverTargets", map[string]any{"discover": true})
	if got := strings.Join(createdTitles(t, c2), ","); got != "page:One" {
		t.Fatalf("page scope targetCreated = %s", got)
	}
}

// Polling: a page that appears in a reserved device's list is announced
// and auto-attached; a page that leaves it is detached and destroyed.
func TestPollingAnnouncesAndDestroys(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopeDevice).WS)
	call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": true})
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	r.addPage(t, r.devA, "Four")
	ev, err := c.WaitEvent(ctxFor(t, 5*time.Second), 0, "Target.attachedToTarget", func(e cdptest.Event) bool {
		return decode[attached](t, e.Params).TargetInfo.Title == "Four"
	})
	if err != nil {
		t.Fatal(err)
	}
	four := decode[attached](t, ev.Params)
	r.devA.RemovePage(r.listed["Four"])
	if _, err := c.WaitEvent(ctxFor(t, 5*time.Second), ev.Seq, "Target.detachedFromTarget", func(e cdptest.Event) bool {
		return strings.Contains(string(e.Params), four.SessionID)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitEvent(ctxFor(t, 5*time.Second), ev.Seq, "Target.targetDestroyed", func(e cdptest.Event) bool {
		return strings.Contains(string(e.Params), four.TargetInfo.TargetID)
	}); err != nil {
		t.Fatal(err)
	}
}

// Design 7.8 item 1: unknown and unsupported methods are answered by the
// bridge with Chrome's text and never forwarded (the fake records none of
// them); each is logged once per CDP session and counted for cdp log.
func TestUnknownMethodsAreNeverForwarded(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	sid := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	unknown := []string{"CSS.enable", "CSS.getAllStyleSheets", "Foo.bar", "Page.printToPDF", "DOM.getAttributes", "Debugger.enable"}
	for i := 0; i < 2; i++ {
		for _, m := range unknown {
			_, err := c.Call(ctxFor(t, 5*time.Second), sid, m, map[string]any{})
			ce, ok := err.(*cdptest.Error)
			if !ok || ce.Code != -32601 || ce.Message != "'"+m+"' wasn't found" {
				t.Fatalf("%s on the page session: %v, want -32601 '%s' wasn't found", m, err, m)
			}
		}
	}
	if _, err := c.Call(ctxFor(t, 5*time.Second), "", "Runtime.evaluate", map[string]any{"expression": "1"}); err == nil || !strings.Contains(err.Error(), "wasn't found") {
		t.Fatalf("Runtime.evaluate on the browser session: %v", err)
	}
	for _, got := range received(r.fake["One"]) {
		for _, m := range unknown {
			if got == m {
				t.Fatalf("the page received the unknown method %s", m)
			}
		}
	}
	counts := map[string]int{}
	for _, u := range r.srv.Unknown() {
		counts[u.Method] = u.Count
	}
	if counts["Foo.bar"] != 2 || counts["Runtime.evaluate"] != 1 {
		t.Fatalf("unknown counts %v", counts)
	}
	if n := strings.Count(r.log.text(), "unknown method Foo.bar"); n != 1 {
		t.Fatalf("Foo.bar logged %d times, want once per CDP session", n)
	}
}

// Design 7.8 item 1: the CDP target id stays the same across a WebKit
// provisional-target swap, and the page session keeps working.
func TestTargetIDStableAcrossSwap(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": true})
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	page := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params)
	f := r.fake["One"]
	f.Swap("wk-One-2", 50*time.Millisecond)
	eventually(t, 5*time.Second, "the swap commits", func() bool { return f.ActiveTarget() == "wk-One-2" })
	time.Sleep(300 * time.Millisecond) // three polls
	tis := decode[struct{ TargetInfos []targetInfo }](t, call(t, c, "", "Target.getTargets", nil)).TargetInfos
	if len(tis) != 1 || tis[0].TargetID != page.TargetInfo.TargetID {
		t.Fatalf("targets after the swap %+v, want the same id %s", tis, page.TargetInfo.TargetID)
	}
	if n := len(eventsOf(c, "Target.targetDestroyed")) + len(eventsOf(c, "Target.detachedFromTarget")); n != 0 {
		t.Fatalf("%d destroy or detach events for a swap", n)
	}
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "string", "value": "after"}})
	res := decode[struct{ Result map[string]any }](t, call(t, c, page.SessionID, "Runtime.evaluate", map[string]any{"expression": "x"}))
	if res.Result["value"] != "after" {
		t.Fatalf("evaluate after the swap: %v", res.Result)
	}
	if recs := f.ReceivedMethod("Runtime.evaluate"); recs[len(recs)-1].Target != "wk-One-2" {
		t.Fatalf("evaluate went to %s, want the new WebKit target", recs[len(recs)-1].Target)
	}
}

// Runtime.evaluate mapping: a thrown value becomes exceptionDetails; with
// awaitPromise a Promise is awaited with Runtime.awaitPromise.
func TestRuntimeEvaluateMapping(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	sid := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	f := r.fake["One"]
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "object", "className": "Promise", "objectId": "p1"}})
	f.Result("Runtime.awaitPromise", map[string]any{"result": map[string]any{"type": "string", "value": "done"}})
	res := decode[struct{ Result map[string]any }](t, call(t, c, sid, "Runtime.evaluate", map[string]any{"expression": "p()", "awaitPromise": true, "returnByValue": true}))
	if res.Result["value"] != "done" {
		t.Fatalf("awaited evaluate = %v", res.Result)
	}
	ap := f.ReceivedMethod("Runtime.awaitPromise")
	if len(ap) != 1 || !strings.Contains(string(ap[0].Params), "\"promiseObjectId\":\"p1\"") || !strings.Contains(string(ap[0].Params), "\"returnByValue\":true") {
		t.Fatalf("awaitPromise calls %+v", ap)
	}
	ev := f.ReceivedMethod("Runtime.evaluate")
	if strings.Contains(string(ev[len(ev)-1].Params), "\"returnByValue\":true") {
		t.Fatalf("the evaluate of an awaited promise asked for a value: %s", ev[len(ev)-1].Params)
	}
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "object", "description": "Error: boom"}, "wasThrown": true})
	raw := call(t, c, sid, "Runtime.evaluate", map[string]any{"expression": "throw 1"})
	var thrown struct {
		ExceptionDetails struct{ Text string } `json:"exceptionDetails"`
	}
	_ = json.Unmarshal(raw, &thrown)
	if thrown.ExceptionDetails.Text != "Uncaught Error: boom" {
		t.Fatalf("thrown evaluate = %s", raw)
	}
}

// Review finding 3 (TestReviewDisableAutoAttach): autoAttach:false detaches
// the auto-attached sessions with detachedFromTarget and keeps the sessions
// that attachToTarget created (Chrome, Target.pdl setAutoAttach: "When
// turned off, automatically detaches from all currently attached targets";
// target_handler.cc SetAutoAttachInternal detaches auto_attached_sessions_
// only).
func TestDisableAutoAttach(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	auto := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params)
	explicit := decode[struct{ SessionID string }](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": auto.TargetInfo.TargetID, "flatten": true})).SessionID
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": false, "flatten": true})
	if _, err := c.Call(ctxFor(t, 5*time.Second), auto.SessionID, "Runtime.evaluate", map[string]any{"expression": "1"}); err == nil {
		t.Fatal("disabling auto-attach left the auto-attached page session usable")
	}
	det := eventsOf(c, "Target.detachedFromTarget")
	if len(det) != 1 || !strings.Contains(string(det[0].Params), auto.SessionID) {
		t.Fatalf("detachedFromTarget events %+v, want one for the auto-attached session", det)
	}
	call(t, c, explicit, "Runtime.evaluate", map[string]any{"expression": "1"})
}

// Review finding 3, tab level: autoAttach:false on a tab session detaches
// the page that the tab's auto-attach attached.
func TestDisableTabAutoAttach(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	tab, page := puppeteerAttach(t, c)
	// A repeated enable attaches no second page.
	call(t, c, tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	if n := len(eventsOf(c, "Target.attachedToTarget")); n != 2 {
		t.Fatalf("%d attachedToTarget events after a repeated tab auto-attach, want tab and page", n)
	}
	call(t, c, tab, "Target.setAutoAttach", map[string]any{"autoAttach": false, "flatten": true})
	if _, err := c.Call(ctxFor(t, 5*time.Second), page, "Runtime.evaluate", map[string]any{"expression": "1"}); err == nil {
		t.Fatal("disabling the tab's auto-attach left its page session usable")
	}
	det := eventsOf(c, "Target.detachedFromTarget")
	if len(det) != 1 || det[0].SessionID != tab || !strings.Contains(string(det[0].Params), page) {
		t.Fatalf("detachedFromTarget events %+v, want the page's on the tab session", det)
	}
	// Enabling again attaches the page again.
	call(t, c, tab, "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	if n := len(eventsOf(c, "Target.attachedToTarget")); n != 3 {
		t.Fatalf("%d attachedToTarget events, want tab, page and the page again", n)
	}
}

// Review finding 3 (TestReviewAutoAttachFilterChange): a new setAutoAttach
// reconsiders the existing targets with its filter (Chrome replays existing
// targets, target_handler.cc SetAutoAttachInternal and AutoAttach), and
// it does not attach a target twice.
func TestAutoAttachFilterChange(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{}})
	if n := len(eventsOf(c, "Target.attachedToTarget")); n != 0 {
		t.Fatalf("the empty filter attached %d targets", n)
	}
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	att := eventsOf(c, "Target.attachedToTarget")
	if len(att) != 1 || decode[attached](t, att[0].Params).TargetInfo.Type != "page" {
		t.Fatalf("after expanding the filter to the default: %+v, want the existing page attached", att)
	}
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true, "filter": []any{map[string]any{}}})
	var kinds []string
	for _, e := range eventsOf(c, "Target.attachedToTarget") {
		kinds = append(kinds, decode[attached](t, e.Params).TargetInfo.Type)
	}
	if strings.Join(kinds, ",") != "page,tab" {
		t.Fatalf("attached kinds %v, want the page once and then the tab", kinds)
	}
}

// Review finding 4 (TestReviewDiscoverFilterChange): a repeated
// setDiscoverTargets announces the existing targets that its filter newly
// allows, never one that was announced already (Chrome,
// target_handler.cc SetDiscoverTargets: "Report the newly matching targets
// that were not yet reported"); narrowing destroys nothing.
func TestDiscoverFilterChange(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": true, "filter": []any{}})
	if got := createdTitles(t, c); len(got) != 0 {
		t.Fatalf("the empty filter announced %v", got)
	}
	call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": true})
	if got := strings.Join(createdTitles(t, c), ","); got != "page:One" {
		t.Fatalf("after expanding the filter to the default: %s, want page:One", got)
	}
	call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": true, "filter": []any{map[string]any{}}})
	if got := strings.Join(createdTitles(t, c), ","); got != "page:One,tab:One" {
		t.Fatalf("after allowing every type: %s, want the tab added once", got)
	}
	call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": true, "filter": []any{}})
	time.Sleep(250 * time.Millisecond) // two polls
	if n := len(eventsOf(c, "Target.targetDestroyed")); n != 0 || len(createdTitles(t, c)) != 2 {
		t.Fatalf("narrowing the filter: %d targetDestroyed, %v created", n, createdTitles(t, c))
	}
	// Turning discovery off forgets what was announced.
	call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": false})
	call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": true})
	if got := strings.Join(createdTitles(t, c), ","); got != "page:One,tab:One,page:One" {
		t.Fatalf("discovery after off and on: %s", got)
	}
}

// Review finding 6 (TestReviewAwaitNonPromiseByValue): with awaitPromise
// and returnByValue, an ordinary object comes back by value. WIP evaluate
// ran by reference (it has no awaitPromise), so the bridge reads the
// object's value with Runtime.callFunctionOn, as
// tools.EvaluateScriptWithOptions does, and releases the reference.
func TestAwaitPromiseNonPromiseByValue(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	sid := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	f := r.fake["One"]
	byValue := func(raw json.RawMessage) bool {
		var p struct{ ReturnByValue bool }
		if err := json.Unmarshal(raw, &p); err != nil {
			panic(err)
		}
		return p.ReturnByValue
	}
	f.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if byValue(c.Params) {
			return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"a": 1}}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "object", "className": "Object", "objectId": "ordinary-object"}}, nil
	})
	f.Handle("Runtime.callFunctionOn", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		if !byValue(c.Params) || !strings.Contains(string(c.Params), "ordinary-object") {
			return nil, &fakewebkit.Error{Code: -32602, Message: "unexpected callFunctionOn " + string(c.Params)}
		}
		return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"a": 1}}}, nil
	})
	res := decode[struct{ Result map[string]any }](t, call(t, c, sid, "Runtime.evaluate", map[string]any{"expression": "({a:1})", "awaitPromise": true, "returnByValue": true}))
	if v, ok := res.Result["value"].(map[string]any); !ok || v["a"] != float64(1) {
		t.Fatalf("awaitPromise and returnByValue for an ordinary object returned %v, want its value {a:1}", res.Result)
	}
	if _, ok := res.Result["objectId"]; ok {
		t.Fatalf("the by-value result has an objectId: %v", res.Result)
	}
	runs := 0
	for _, rec := range f.ReceivedMethod("Runtime.evaluate") {
		if strings.Contains(string(rec.Params), "({a:1})") {
			runs++
		}
	}
	if runs != 1 {
		t.Fatalf("the expression ran %d times, want once", runs)
	}
	rel := f.ReceivedMethod("Runtime.releaseObject")
	if len(rel) != 1 || !strings.Contains(string(rel[0].Params), "ordinary-object") {
		t.Fatalf("releaseObject calls %+v, want the evaluated object released", rel)
	}
	// Without returnByValue the reference stays.
	res = decode[struct{ Result map[string]any }](t, call(t, c, sid, "Runtime.evaluate", map[string]any{"expression": "({a:1})", "awaitPromise": true}))
	if res.Result["objectId"] != "ordinary-object" {
		t.Fatalf("awaitPromise without returnByValue returned %v, want the reference", res.Result)
	}
}

// Design 5.3 (Browser and Target rows): getWindowForTarget is synthetic
// and getBrowserContexts lists no context besides the default one.
func TestSyntheticBrowserResults(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	w := decode[struct {
		WindowID int `json:"windowId"`
		Bounds   map[string]any
	}](t, call(t, c, "", "Browser.getWindowForTarget", nil))
	if w.WindowID != 1 || w.Bounds["windowState"] != "normal" {
		t.Fatalf("getWindowForTarget = %+v", w)
	}
	ids := decode[struct {
		BrowserContextIDs []string `json:"browserContextIds"`
	}](t, call(t, c, "", "Target.getBrowserContexts", nil)).BrowserContextIDs
	if ids == nil || len(ids) != 0 {
		t.Fatalf("getBrowserContexts = %#v, want an empty list", ids)
	}
}

// Design 5.1: /json/protocol is the supported subset in the form of
// Chrome's, generated from the method table: domains sorted, each with its
// sorted commands and events, protocol version 1.3.
func TestProtocolFromTheMethodTable(t *testing.T) {
	p := protocol()
	if v := p["version"].(map[string]string); v["major"] != "1" || v["minor"] != "3" {
		t.Fatalf("version %v", v)
	}
	doms := p["domains"].([]map[string]any)
	var names []string
	var target map[string]any
	for _, d := range doms {
		names = append(names, d["domain"].(string))
		if d["domain"] == "Target" {
			target = d
		}
	}
	if !sort.StringsAreSorted(names) || target == nil {
		t.Fatalf("domains %v, want sorted with Target", names)
	}
	var cmds, evs []string
	for _, c := range target["commands"].([]map[string]string) {
		cmds = append(cmds, c["name"])
	}
	for _, e := range target["events"].([]map[string]string) {
		evs = append(evs, e["name"])
	}
	if !sort.StringsAreSorted(cmds) || !slices.Contains(cmds, "setAutoAttach") || !slices.Contains(cmds, "getBrowserContexts") {
		t.Fatalf("Target commands %v", cmds)
	}
	if strings.Join(evs, ",") != "attachedToTarget,detachedFromTarget,targetCreated,targetDestroyed,targetInfoChanged" {
		t.Fatalf("Target events %v", evs)
	}
	for _, d := range doms {
		for _, c := range d["commands"].([]map[string]string) {
			if _, ok := lookup(d["domain"].(string)+"."+c["name"], OnBrowser|OnTab|OnPage); !ok {
				t.Fatalf("/json/protocol lists %s.%s, which the table does not have", d["domain"], c["name"])
			}
		}
	}
}

// Polling sends targetInfoChanged for a changed target only for the target
// ids that discovery announced: with Chrome's default filter the page, not
// the tab. The connection's last known title is changed here, so the next
// poll sees the listed title as a change.
func TestPollingInfoChangedForAnnouncedTargets(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": true})
	co := r.onlyConn(t)
	var tg target
	co.mu.Lock()
	for id, known := range co.discovered {
		tg = known
		known.title = "Old title"
		co.discovered[id] = known
	}
	co.mu.Unlock()
	ev, err := c.WaitEvent(ctxFor(t, 5*time.Second), 0, "Target.targetInfoChanged", nil)
	if err != nil {
		t.Fatal(err)
	}
	ti := decode[struct{ TargetInfo targetInfo }](t, ev.Params).TargetInfo
	if ti.TargetID != tg.pageID || ti.Title != "One" {
		t.Fatalf("targetInfoChanged %+v, want the page with its listed title One", ti)
	}
	time.Sleep(250 * time.Millisecond) // two more polls
	for _, e := range eventsOf(c, "Target.targetInfoChanged") {
		if strings.Contains(string(e.Params), tg.tabID) {
			t.Fatalf("targetInfoChanged for the tab, which discovery did not announce: %s", e.Params)
		}
	}
}

// A failed release of the evaluated object is logged; the value is still
// returned.
func TestAwaitPromiseByValueReleaseFailure(t *testing.T) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	call(t, c, "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
	sid := decode[attached](t, eventsOf(c, "Target.attachedToTarget")[0].Params).SessionID
	f := r.fake["One"]
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "object", "className": "Object", "objectId": "obj-1"}})
	f.Result("Runtime.callFunctionOn", map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"b": 2}}})
	f.Unsupported("Runtime.releaseObject")
	res := decode[struct{ Result map[string]any }](t, call(t, c, sid, "Runtime.evaluate", map[string]any{"expression": "o", "awaitPromise": true, "returnByValue": true}))
	if v, ok := res.Result["value"].(map[string]any); !ok || v["b"] != float64(2) {
		t.Fatalf("result %v, want the value {b:2}", res.Result)
	}
	if !strings.Contains(r.log.text(), "releasing the evaluated object") {
		t.Fatalf("the failed release was not logged:\n%s", r.log.text())
	}
}
