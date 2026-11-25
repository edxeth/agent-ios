package cdp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Behavior tests of the W5c-1b Profiler, Storage and cookie failure paths
// (W5c-1b review F2). Expected values: V8's Profiler rules and Chrome's
// texts (/home/devkit/.pi/artifacts/worker/w5c1b/chrome-research.md), the
// CDP protocol, and the contracts in docs/notes/w5.md "W5c-1b".

// A Profiler.stop or Profiler.disable whose ScriptProfiler.stopTracking
// fails keeps the profile: the call fails and says to send it again, the
// run stays, and the retry stops it (stop returns the profile).
func TestCDPProfilerStopFailureKeepsTheProfile(t *testing.T) {
	for _, method := range []string{"Profiler.stop", "Profiler.disable"} {
		t.Run(method, func(t *testing.T) {
			r := newRig(t)
			f := r.fake["One"]
			c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
			_, page := puppeteerAttach(t, c)
			call(t, c, page, "Profiler.enable", nil)
			call(t, c, page, "Profiler.start", nil)
			f.ProfilerSample("work")
			restore := failWIP(f, "ScriptProfiler.stopTracking")
			e := cdpFail(t, c, page, method, nil)
			restore()
			if !strings.Contains(e.Message, "send it again") || !strings.Contains(e.Message, "deliberate") {
				t.Fatalf("%s with a failing stopTracking: %+v", method, e)
			}
			if !f.Tracking(f.ActiveTarget()) || !r.pageStateOf(t, "One").HasEntry("cdp:profiler:"+page) {
				t.Fatalf("the failed %s dropped the run or its entry", method)
			}
			res := decode[struct {
				Profile *struct{ Samples []int }
			}](t, call(t, c, page, method, nil))
			if f.Tracking(f.ActiveTarget()) {
				t.Fatalf("the retried %s left the run", method)
			}
			if method == "Profiler.stop" && (res.Profile == nil || len(res.Profile.Samples) != 1) {
				t.Fatalf("the retried Profiler.stop: %+v, want the profile with its sample", res)
			}
		})
	}
}

// A Profiler.stop for which WebKit sends no ScriptProfiler.trackingComplete
// returns no profile: it waits for the completion and fails when the call
// ends (here, the detach of the attachment cancels it).
func TestCDPProfilerStopWithoutCompletion(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	tab, page := puppeteerAttach(t, c)
	call(t, c, page, "Profiler.enable", nil)
	call(t, c, page, "Profiler.start", nil)
	f.Handle("ScriptProfiler.stopTracking", func(fakewebkit.Call) (any, *fakewebkit.Error) { return map[string]any{}, nil })
	type result struct {
		raw json.RawMessage
		err error
	}
	got := make(chan result, 1)
	go func() {
		raw, err := c.Call(ctxFor(t, 20*time.Second), page, "Profiler.stop", nil)
		got <- result{raw, err}
	}()
	eventually(t, 5*time.Second, "the stopTracking of Profiler.stop", func() bool {
		return len(f.ReceivedMethod("ScriptProfiler.stopTracking")) > 0
	})
	select {
	case res := <-got:
		t.Fatalf("Profiler.stop without trackingComplete returned before the call ended: %s, %v", res.raw, res.err)
	case <-time.After(300 * time.Millisecond):
	}
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": page})
	waitSessionEvent(t, c, 0, tab, "Target.detachedFromTarget")
	select {
	case res := <-got:
		ce, ok := res.err.(*cdptest.Error)
		if !ok || !strings.Contains(ce.Message, "no ScriptProfiler.trackingComplete") || strings.Contains(string(res.raw), "profile") {
			t.Fatalf("Profiler.stop without trackingComplete: %s, %v; want an error that names the missing completion, and no profile", res.raw, res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Profiler.stop did not end with the detach")
	}
}

// A malformed ScriptProfiler.trackingComplete is ignored, and of two
// completions of one run the first is the profile: a second cannot replace
// it or end the run twice.
func TestCDPProfilerCompletionCountsOnce(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	call(t, c, page, "Profiler.enable", nil)
	call(t, c, page, "Profiler.start", nil)
	f.Emit("ScriptProfiler.trackingComplete", map[string]any{"timestamp": "not a number"})
	frame := map[string]any{"sourceID": "1", "name": "first", "line": 1, "column": 1, "url": "https://fixture.example/app.js"}
	f.Emit("ScriptProfiler.trackingComplete", map[string]any{"timestamp": 99.0, "samples": map[string]any{
		"stackTraces": []any{map[string]any{"timestamp": 98.5, "stackFrames": []any{frame}}},
	}})
	f.ProfilerSample("second") // in the fake's own trackingComplete at the stop
	p := decode[struct {
		Profile struct {
			Nodes []struct{ CallFrame struct{ FunctionName string } }
		}
	}](t, call(t, c, page, "Profiler.stop", nil))
	var names []string
	for _, n := range p.Profile.Nodes {
		names = append(names, n.CallFrame.FunctionName)
	}
	if !slices.Contains(names, "first") || slices.Contains(names, "second") {
		t.Fatalf("profile functions %v, want the first completion's (first) only", names)
	}
}

// Storage.clearDataForOrigin fails, and reports no success, when the
// params are malformed, when the page's evaluation fails, when the page
// returns something other than the clear function's result, and when the
// page listing fails.
func TestCDPClearDataForOriginFailures(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	puppeteerAttach(t, c)
	req := map[string]any{"origin": "https://one.example", "storageTypes": "local_storage"}
	if e := cdpFail(t, c, "", "Storage.clearDataForOrigin", map[string]any{"origin": 5}); e.Code != -32602 || !strings.Contains(e.Message, "Invalid parameters") {
		t.Errorf("malformed params: %+v", e)
	}
	reply := func(result map[string]any, werr *fakewebkit.Error) {
		f.Handle("Runtime.evaluate", func(call fakewebkit.Call) (any, *fakewebkit.Error) {
			var p struct{ Expression string }
			_ = json.Unmarshal(call.Params, &p)
			if !strings.Contains(p.Expression, "localStorage.clear()") {
				return map[string]any{"result": map[string]any{"type": "number", "value": 1, "description": "1"}}, nil
			}
			return result, werr
		})
	}
	reply(nil, &fakewebkit.Error{Code: -32000, Message: "deliberate evaluation failure"})
	if e := cdpFail(t, c, "", "Storage.clearDataForOrigin", req); !strings.Contains(e.Message, "clearing localStorage of https://one.example") || !strings.Contains(e.Message, "deliberate evaluation failure") {
		t.Errorf("a failing evaluation: %+v", e)
	}
	for _, res := range []map[string]any{
		{"result": map[string]any{"type": "number", "value": 3, "description": "3"}},
		{"result": map[string]any{"type": "string", "value": "not json"}},
	} {
		reply(res, nil)
		if e := cdpFail(t, c, "", "Storage.clearDataForOrigin", req); !strings.Contains(e.Message, "the page returned") {
			t.Errorf("result %v: %+v, want the page's result named", res, e)
		}
	}
	r.l.Close()
	if e := cdpFail(t, c, "", "Storage.clearDataForOrigin", req); e.Message == "" {
		t.Errorf("a failing page listing: %+v", e)
	}
}

// With two pages of the origin in the endpoint's scope, one of them leased
// by another session, Storage.clearDataForOrigin fails with the lease
// message before it clears any page (the check runs over every page
// first).
func TestCDPClearDataForOriginLeaseCheckBeforeAnyClear(t *testing.T) {
	r := newRig(t)
	twin := fakewebkit.New(fakewebkit.Options{TargetID: "wk-Twin"})
	t.Cleanup(twin.Close)
	r.fake["Twin"] = twin
	r.listed["Twin"] = r.devA.AddPage("Twin", "https://one.example/twin", twin.URL)
	one := &storagePage{origin: "https://one.example", items: 3}
	one.install(r.fake["One"])
	tw := &storagePage{origin: "https://one.example", items: 2}
	tw.install(twin)
	c2 := dial(t, r.issue(t, "s2", "Twin", ScopePage).WS)
	puppeteerAttach(t, c2)
	c := dial(t, r.issue(t, "s1", "One", ScopeDevice).WS)
	e := cdpFail(t, c, "", "Storage.clearDataForOrigin", map[string]any{"origin": "https://one.example", "storageTypes": "local_storage"})
	if !strings.Contains(e.Message, "s2") {
		t.Fatalf("clear with the twin page leased by s2: %+v, want the lease message", e)
	}
	if ev, _, items := one.counts(); ev != 0 || items != 3 {
		t.Fatalf("the selected page was cleared before the refusal: %d evaluations, %d items", ev, items)
	}
	if ev, _, _ := tw.counts(); ev != 0 {
		t.Fatalf("the leased page evaluated the clear: %d", ev)
	}
}

// Cookie requests with malformed or refused params fail before any
// cookie command; an empty setCookies sends nothing; getCookies{urls}
// filters with RFC 6265's rules; a failing Page.getCookies fails every
// call that reads the jar and changes nothing; an expired cookie that
// WebKit refuses to set answers success false.
func TestCDPCookieRequestErrors(t *testing.T) {
	r, jar := jarRig(t)
	c, page := r.jarPage(t, "s1")
	all := []webkit.Cookie{sessionCookie("k", "host", "app.example", "/"), sessionCookie("d", "dom", ".app.example", "/"), sessionCookie("o", "other", ".other.example", "/")}
	setJar(jar, all...)
	mark := len(jar.Server.Received())
	cookie := map[string]any{"name": "a", "value": "b", "url": "https://app.example/"}
	for _, tc := range []struct {
		session, method string
		params          map[string]any
		code            int
		msg             string
	}{
		{page, "Network.getCookies", map[string]any{"urls": "x"}, -32602, "Invalid parameters"},
		{"", "Storage.getCookies", map[string]any{"browserContextId": 5}, -32602, "Invalid parameters"},
		{page, "Network.setCookie", map[string]any{"name": 5}, -32602, "Invalid parameters"},
		{page, "Network.setCookie", map[string]any{"name": "a", "value": "b", "url": "https://app.example/", "sameSite": "Sometimes"}, -32602, "invalid sameSite"},
		{page, "Network.setCookies", map[string]any{"cookies": "x"}, -32602, "Invalid parameters"},
		{page, "Network.setCookies", map[string]any{"cookies": []any{cookie}, "browserContextId": "default"}, -32602, "browserContextId is only allowed for Browser target"},
		{page, "Network.setCookies", map[string]any{"cookies": []any{map[string]any{"name": "a", "value": "b", "url": "https://app.example/", "partitionKey": map[string]any{"topLevelSite": "https://app.example"}}}}, -32000, "partitioned cookies"},
		{page, "Network.deleteCookies", map[string]any{"name": 5}, -32602, "Invalid parameters"},
		{page, "Network.clearBrowserCookies", map[string]any{"browserContextId": 5}, -32602, "Invalid parameters"},
	} {
		e := cdpFail(t, c, tc.session, tc.method, tc.params)
		if e.Code != tc.code || !strings.Contains(e.Message, tc.msg) {
			t.Errorf("%s %v: %+v, want %d %q", tc.method, tc.params, e, tc.code, tc.msg)
		}
	}
	call(t, c, page, "Network.setCookies", map[string]any{"cookies": []any{}})
	for _, m := range sinceMark(jar.Server, mark) {
		if strings.HasPrefix(m, "Page.") {
			t.Fatalf("a refused or empty request sent %s", m)
		}
	}
	// The browser session accepts the default context (Chrome's only one).
	if got := decode[struct{ Cookies []any }](t, call(t, c, "", "Storage.getCookies", map[string]any{"browserContextId": "default"})); len(got.Cookies) != len(all) {
		t.Fatalf("Storage.getCookies{default}: %d cookies, want %d", len(got.Cookies), len(all))
	}
	for _, tc := range []struct {
		url  string
		want []string
	}{
		{"ftp://app.example/", nil},
		{"https://app.example", []string{"d", "k"}}, // no path is "/"
		{"https://notother.example/", nil},          // not a subdomain of other.example
		{"https://other.example/x", []string{"o"}},
	} {
		got := decode[struct{ Cookies []struct{ Name string } }](t, call(t, c, page, "Network.getCookies", map[string]any{"urls": []string{tc.url}}))
		var names []string
		for _, ck := range got.Cookies {
			names = append(names, ck.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, tc.want) {
			t.Errorf("Network.getCookies{%s} = %v, want %v", tc.url, names, tc.want)
		}
	}

	jar.Server.Handle("Page.getCookies", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "deliberate read failure"}
	})
	for _, tc := range []struct {
		session, method string
		params          map[string]any
	}{
		{page, "Network.getCookies", nil},
		{"", "Storage.getCookies", nil},
		{page, "Network.deleteCookies", map[string]any{"name": "k", "domain": "app.example"}},
		{page, "Network.clearBrowserCookies", nil},
	} {
		e := cdpFail(t, c, tc.session, tc.method, tc.params)
		if !strings.Contains(e.Message, "Page.getCookies") || !strings.Contains(e.Message, "deliberate read failure") {
			t.Errorf("%s with a failing Page.getCookies: %+v", tc.method, e)
		}
	}
	if got := jarCookies(jar); len(got) != len(all) {
		t.Fatalf("jar after the failed reads: %v", got)
	}
	jar.Server.Handle("Page.getCookies", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		jar.Mu.Lock()
		defer jar.Mu.Unlock()
		return map[string]any{"cookies": slices.Clone(jar.Cookies)}, nil
	})
	jar.Server.Handle("Page.setCookie", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "deliberate set refusal"}
	})
	res := decode[struct{ Success bool }](t, call(t, c, page, "Network.setCookie", map[string]any{"name": "k", "value": "", "domain": "app.example", "path": "/", "expires": 1}))
	if res.Success {
		t.Fatal("an expired Network.setCookie that WebKit refuses: success true")
	}
	if got := jarCookies(jar); len(got) != len(all) {
		t.Fatalf("jar after the refused expired cookie: %v", got)
	}
}

// A cookie call on the browser session reaches the record's selected
// page; when the page listing no longer lists that page, or the listing
// fails, the call fails and names the cause.
func TestCDPBrowserCookieCallsNeedTheSelectedPage(t *testing.T) {
	r, _ := jarRig(t)
	c, _ := r.jarPage(t, "s1")
	r.devA.RemovePage(r.listed["Jar"])
	if e := cdpFail(t, c, "", "Storage.getCookies", nil); !strings.Contains(e.Message, `the selected page "Jar" is gone`) || !strings.Contains(e.Message, "get cdp-url") {
		t.Fatalf("Storage.getCookies without the selected page: %+v", e)
	}
	r.l.Close()
	if e := cdpFail(t, c, "", "Storage.clearCookies", nil); e.Message == "" {
		t.Fatalf("Storage.clearCookies with a failing listing: %+v", e)
	}
}

// pageStatus is the manager's status of the page title.
func (r *rig) pageStatus(t *testing.T, title string) core.PageStatus {
	t.Helper()
	for _, ps := range r.m.Status() {
		if ps.Title == title {
			return ps
		}
	}
	t.Fatalf("no page %q", title)
	return core.PageStatus{}
}

// releaseWhileQueued runs send while an exclusive operation of session
// s1's lease holds page title in its scheduler, so the command's operation
// waits in the page's queue under the lease that the command took or
// renewed, and then releases that lease (core.Manager.Release, as
// "agent-ios release" does). The command has taken or renewed the lease
// when the lease's last use moves. The release closes admission, cancels
// the holder, which then ends, and fails the queued operations
// (core.Scheduler.Release). It returns send's error.
func (r *rig) releaseWhileQueued(t *testing.T, title string, send func() error) error {
	t.Helper()
	session, epoch := r.leaseOf(t, title)
	if session != "s1" {
		t.Fatalf("page %q is leased by %q, want s1", title, session)
	}
	waits := testCtx(t)
	hold, err := r.pageSessionOf(t, title).Sched.Begin(waits, core.OpSpec{Name: "hold", Class: core.ClassExclusive, Session: session, Epoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	defer hold.End()
	go func() {
		<-hold.Context().Done()
		hold.End()
	}()
	key := r.pageStatus(t, title).Key
	held := r.pageStatus(t, title).LastUsed
	errc := make(chan error, 1)
	go func() { errc <- send() }()
	for ; r.pageStatus(t, title).LastUsed.Equal(held); time.Sleep(time.Millisecond) {
		if waits.Err() != nil {
			t.Fatal("the command never reached the page's lease")
		}
	}
	if err := r.m.Release(waits, "s1", key); err != nil {
		t.Fatalf("releasing the lease: %v", err)
	}
	select {
	case err := <-errc:
		return err
	case <-waits.Done():
		t.Fatal("the command did not end after the release of its lease")
		return nil
	}
}

// A cookie command whose operation is refused after it took the lease
// (the lease ends while the command waits for its turn on the page) fails
// with the lease message and writes no cookie: on the browser session
// (record.inPage: Touch, then core.Manager.BeginLeased refuses) and on a
// page session (session.do, through Network.setCookie). Release fails the
// queued operations with core.ErrLeaseEnded; the page session's watcher
// may detach the attachment first, and then the command fails with the
// detach message.
func TestCDPCookieWriteRefusedWhenTheLeaseEndsBeforeItRuns(t *testing.T) {
	cookie := map[string]any{"name": "late", "value": "v", "domain": "app.example", "path": "/"}
	for _, tc := range []struct {
		name, method string
		page         bool
		params       any
	}{
		{"browser session", "Storage.setCookies", false, map[string]any{"cookies": []any{cookie}}},
		{"page session", "Network.setCookie", true, cookie},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, jar := jarRig(t)
			c, page := r.jarPage(t, "s1")
			setJar(jar, sessionCookie("k", "v", "app.example", "/"))
			mark := len(jar.Server.Received())
			session := ""
			if tc.page {
				session = page
			}
			err := r.releaseWhileQueued(t, "Jar", func() error {
				_, err := c.Call(ctxFor(t, 10*time.Second), session, tc.method, tc.params)
				return err
			})
			ce, ok := err.(*cdptest.Error)
			lease := ok && strings.Contains(ce.Message, "lease") && strings.Contains(ce.Message, "ended")
			detached := ok && tc.page && strings.Contains(ce.Message, "detached")
			if !lease && !detached {
				t.Fatalf("%s while its lease ended: %v, want the lease message", tc.method, err)
			}
			if n := countSince(jar.Server, mark, "Page.setCookie"); n != 0 {
				t.Fatalf("%s sent Page.setCookie %d times after its lease ended", tc.method, n)
			}
			if got := jarCookies(jar); !slices.Equal(got, []string{"k=v app.example /"}) {
				t.Fatalf("jar %v after the refused %s", got, tc.method)
			}
		})
	}
}

// A Profiler.stop whose operation was admitted under the lease, when the
// lease is released before the operation looks for the run: the release
// ends the running operations and stops the session's run with its
// entries (core.Manager.Release, design 2.4), so the call fails and says
// that the profile ended with the lease, and it sends no
// ScriptProfiler.stopTracking of its own.
func TestCDPProfilerStopAfterTheLeaseReleasedTheRun(t *testing.T) {
	r := newRig(t)
	f := r.fake["One"]
	c := dial(t, r.issue(t, "s1", "One", ScopePage).WS)
	_, page := puppeteerAttach(t, c)
	call(t, c, page, "Profiler.enable", nil)
	call(t, c, page, "Profiler.start", nil)
	admitted, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	stopProfileAdmitted = func() {
		once.Do(func() { close(admitted) })
		<-resume
	}
	t.Cleanup(func() { stopProfileAdmitted = nil })
	errc := make(chan error, 1)
	go func() {
		_, err := c.Call(ctxFor(t, 10*time.Second), page, "Profiler.stop", nil)
		errc <- err
	}()
	select {
	case <-admitted:
	case <-time.After(10 * time.Second):
		close(resume)
		t.Fatal("Profiler.stop was never admitted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.m.Release(ctx, "s1", r.pageStatus(t, "One").Key)
	state := r.pageStateOf(t, "One")
	mark := len(f.Received())
	close(resume)
	if err != nil {
		t.Fatalf("releasing the lease: %v", err)
	}
	if state.HasEntry("cdp:profiler:" + page) {
		t.Fatal("the release kept the run's entry")
	}
	err = <-errc
	if ce, ok := err.(*cdptest.Error); !ok || !strings.Contains(ce.Message, "the profile ended with the lease of the session") {
		t.Fatalf("Profiler.stop after the release stopped its run: %v, want the lease message", err)
	}
	if n := countSince(f, mark, "ScriptProfiler.stopTracking"); n != 0 {
		t.Fatalf("Profiler.stop sent ScriptProfiler.stopTracking %d times after the release stopped the run", n)
	}
}
