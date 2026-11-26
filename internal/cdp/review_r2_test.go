package cdp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W5c-1a review round 2 (/home/devkit/.pi/artifacts/reviewer/w5c1a-r1-review-20261108-190228.md):
// the reviewer's reproductions of F9, F10 and F11, kept as regression tests.

// F9: Fetch.enable success must mean its backend interception flag and
// rules have been installed, also after a transient error in an earlier
// enable.
func TestClosureRetryFetchEnable(t *testing.T) {
	for _, method := range []string{"Network.setInterceptionEnabled", "Network.addInterception"} {
		t.Run(method, func(t *testing.T) {
			r, _, c, sid, _ := attachPage(t)
			f := r.fake["One"]
			call(t, c, sid, "Network.enable", nil)
			failed := false
			f.Handle(method, func(fakewebkit.Call) (any, *fakewebkit.Error) {
				if !failed {
					failed = true
					return nil, &fakewebkit.Error{Code: -32000, Message: "transient failure"}
				}
				return nil, nil
			})
			if e := callErr(t, c, sid, "Fetch.enable", nil); !strings.Contains(e.Message, "transient failure") {
				t.Fatal(e)
			}
			before := len(f.ReceivedMethod(method))
			call(t, c, sid, "Fetch.enable", nil)
			got := len(f.ReceivedMethod(method))
			if got != before+1 {
				t.Errorf("retry returned success but %s sends stayed at %d (before %d)", method, got, before)
			}
			if got := len(f.ReceivedMethod("Network.addInterception")); got == 0 {
				t.Error("no interception rule installed")
			}
		})
	}
}

// F10: each attachment must agree on the request's type before deferring
// to another claimant. A later attachment has no record of this request.
func TestClosureLateAttachmentTypeArbitration(t *testing.T) {
	r, _, c, sid, target := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Network.enable", nil)
	call(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*", "resourceType": "Other", "requestStage": "Response"}}})
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("late", "main", "0.1", "Script", "https://one.example/late.js", nil))
	waitEvent(t, c, seq, sid, "Network.requestWillBeSent", nil)
	second := decode[struct{ SessionID string }](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true})).SessionID
	call(t, c, second, "Network.enable", nil)
	call(t, c, second, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*", "resourceType": "Script", "requestStage": "Response"}}})
	f.InterceptEvent("wk-One", "late", "response", map[string]any{"response": map[string]any{"url": "https://one.example/late.js", "status": 200, "statusText": "OK", "headers": map[string]string{}}})
	call(t, c, sid, "Page.getFrameTree", nil)
	call(t, c, second, "Page.getFrameTree", nil)
	eventually(t, time.Second, "some attachment to report or continue the paused response", func() bool {
		return len(sessionEvents(c, seq, sid, "Fetch.requestPaused"))+len(sessionEvents(c, seq, second, "Fetch.requestPaused"))+len(f.Answers()) > 0
	})
	// The request is a Script, which only the second attachment's pattern
	// claims: it alone reports it, with that type, and nobody continues it.
	if n := len(sessionEvents(c, seq, sid, "Fetch.requestPaused")); n != 0 {
		t.Errorf("first attachment reported %d pauses of a Script request that its Other pattern does not claim", n)
	}
	paused := sessionEvents(c, seq, second, "Fetch.requestPaused")
	if len(paused) != 1 || !strings.Contains(string(paused[0].Params), `"resourceType":"Script"`) {
		t.Errorf("second attachment's pauses %+v, want one Script pause", paused)
	}
	if a := f.Answers(); len(a) != 0 {
		t.Errorf("answers %v for a request that the second attachment reported", a)
	}
}

// F11: a reset must invalidate a pause delivered before the reset even if
// the attachment has not yet translated its requestIntercepted event.
func TestClosureQueuedPauseAtNetworkReset(t *testing.T) {
	r, _, c, sid, _, f := enableFetch(t)
	_, s := r.pageSession(t, sid)
	ps := s.page
	call(t, c, sid, "Log.enable", nil)
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"X-Review": "1"}})
	reached, release := holdApply(t, ps, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("pre-reset", "main", "0.1", "Fetch", "https://one.example/resource", nil))
	waitClosed(t, reached, "attachment held before request")
	n := ps.q.pushed()
	f.InterceptEvent("wk-One", "pre-reset", "request", map[string]any{"request": map[string]any{"url": "https://one.example/resource", "method": "GET", "headers": map[string]string{}}})
	pushedAtLeast(t, ps, n+1)
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{}})
	answeredWith(t, f, "pre-reset", "Network.interceptContinue")
	close(release)
	waitEvent(t, c, seq, sid, "Log.entryAdded", nil) // netGapLocked applied at gapAt
	ps.mu.Lock()
	var held []string
	for id, p := range ps.fetch.paused {
		held = append(held, fmt.Sprintf("%s=%s", id, p.key.wip))
	}
	ps.mu.Unlock()
	if len(held) != 0 {
		t.Errorf("reset resurrected local pauses %v after core continued the request", held)
	}
	failed := sessionEvents(c, seq, sid, "Network.loadingFailed")
	if len(failed) != 1 {
		t.Errorf("terminal events=%d, want loadingFailed for request the barrier continued", len(failed))
	}
	if p := sessionEvents(c, seq, sid, "Fetch.requestPaused"); len(p) != 0 {
		t.Errorf("reported %d pauses of a request that the reset continued", len(p))
	}
}

// F11 guard: a request that WebKit pauses after the reset re-applied the
// interception rules, but before core's report, is a new hold: it is
// reported, also when the attachment handles its event after the report.
func TestQueuedNewHoldAtNetworkResetIsReported(t *testing.T) {
	r, _, c, sid, _, f := enableFetch(t)
	_, s := r.pageSession(t, sid)
	ps := s.page
	call(t, c, sid, "Log.enable", nil)
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"X-Review": "1"}})
	once := false
	f.Handle("Network.addInterception", func(call fakewebkit.Call) (any, *fakewebkit.Error) {
		if !once { // the reset's setup installs the Fetch rule again
			once = true
			f.InterceptEvent(call.Target, "new", "request", map[string]any{"request": map[string]any{"url": "https://one.example/new", "method": "GET", "headers": map[string]string{}}})
		}
		return nil, nil
	})
	reached, release := holdApply(t, ps, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("new", "main", "0.1", "Fetch", "https://one.example/new", nil))
	waitClosed(t, reached, "attachment held before request")
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{}})
	if !once {
		t.Fatal("the reset installed no interception rule")
	}
	close(release)
	waitEvent(t, c, seq, sid, "Log.entryAdded", nil)
	paused := sessionEvents(c, seq, sid, "Fetch.requestPaused")
	if len(paused) != 1 {
		t.Fatalf("pauses %+v, want the new hold reported", paused)
	}
	ps.mu.Lock()
	held := len(ps.fetch.paused)
	ps.mu.Unlock()
	if held != 1 {
		t.Fatalf("%d paused requests remembered, want the new hold", held)
	}
	if n := len(sessionEvents(c, seq, sid, "Network.loadingFailed")); n != 0 {
		t.Errorf("%d loadingFailed for a request that is paused", n)
	}
}

// F11, the reset's Network.disable: WebKit continues a request whose pause
// it reported after the barrier continued the holds it knew. The
// attachment reported that pause already; the reset ends it.
func TestPauseContinuedByResetDisableEnds(t *testing.T) {
	r, _, c, sid, _, f := enableFetch(t)
	_, s := r.pageSession(t, sid)
	ps := s.page
	call(t, c, sid, "Log.enable", nil)
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{"X-Review": "1"}})
	seq := c.Seq()
	f.Emit("Network.requestWillBeSent", wipRequest("late", "main", "0.1", "Fetch", "https://one.example/late", nil))
	waitEvent(t, c, seq, sid, "Network.requestWillBeSent", nil)
	once := false
	f.Handle("Network.disable", func(call fakewebkit.Call) (any, *fakewebkit.Error) {
		if !once {
			once = true
			f.InterceptEvent(call.Target, "late", "request", map[string]any{"request": map[string]any{"url": "https://one.example/late", "method": "GET", "headers": map[string]string{}}})
			// The attachment reports it before the reset ends (the test
			// goroutine checks the report below).
			for end := time.Now().Add(5 * time.Second); len(sessionEvents(c, seq, sid, "Fetch.requestPaused")) == 0 && time.Now().Before(end); {
				time.Sleep(time.Millisecond)
			}
			f.ContinueHolds(call.Target)
		}
		return nil, nil
	})
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{}})
	waitEvent(t, c, seq, sid, "Log.entryAdded", nil)
	ps.mu.Lock()
	held := len(ps.fetch.paused)
	ps.mu.Unlock()
	if held != 0 {
		t.Errorf("%d paused requests remembered after the reset continued them", held)
	}
	if n := len(sessionEvents(c, seq, sid, "Network.loadingFailed")); n != 1 {
		t.Errorf("%d loadingFailed, want the continued request ended", n)
	}
	p := decode[pausedEvent](t, sessionEvents(c, seq, sid, "Fetch.requestPaused")[0].Params)
	if e := callErr(t, c, sid, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID}); e.Message != "Invalid InterceptionId." {
		t.Errorf("answer to a pause that the reset continued: %v", e)
	}
}

// F10: the claimants of an event share its decision. Two attachments claim
// a request and both stop reporting before they handle its event: the
// second one to withdraw continues it, so it does not stay paused.
func TestSharedClaimLastWithdrawalContinues(t *testing.T) {
	r, _, c, sid, target := attachPage(t)
	f := r.fake["One"]
	second := decode[struct{ SessionID string }](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true})).SessionID
	var pss []*pageState
	for _, id := range []string{sid, second} {
		call(t, c, id, "Network.enable", nil)
		call(t, c, id, "Fetch.enable", nil)
		_, s := r.pageSession(t, id)
		pss = append(pss, s.page)
	}
	var releases []chan struct{}
	for _, ps := range pss {
		reached, release := holdApply(t, ps, 1)
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()
		releases = append(releases, release)
		f.Emit("Network.requestWillBeSent", wipRequest("both", "main", "0.1", "Fetch", "https://one.example/both", nil))
		waitClosed(t, reached, "attachment held before request")
	}
	n := []uint64{pss[0].q.pushed(), pss[1].q.pushed()}
	f.InterceptEvent("wk-One", "both", "request", map[string]any{"request": map[string]any{"url": "https://one.example/both", "method": "GET", "headers": map[string]string{}}})
	pushedAtLeast(t, pss[0], n[0]+1)
	pushedAtLeast(t, pss[1], n[1]+1)
	for _, id := range []string{sid, second} {
		call(t, c, id, "Fetch.disable", nil) // before the attachment handles the event
	}
	for _, rel := range releases {
		close(rel)
	}
	answeredWith(t, f, "both", "Network.interceptContinue")
}

// F9: a rule that WebKit refused to add is not WebKit's: Fetch.disable
// does not remove it, and it still turns interception off, which the
// enable turned on.
func TestFetchDisableAfterRefusedAdd(t *testing.T) {
	r, _, c, sid, _ := attachPage(t)
	f := r.fake["One"]
	call(t, c, sid, "Network.enable", nil)
	f.Unsupported("Network.addInterception")
	if e := callErr(t, c, sid, "Fetch.enable", nil); e == nil {
		t.Fatal("Fetch.enable succeeded although WebKit refused the rule")
	}
	call(t, c, sid, "Fetch.disable", nil)
	if n := len(f.ReceivedMethod("Network.removeInterception")); n != 0 {
		t.Errorf("%d removals of a rule that WebKit refused to add", n)
	}
	var off int
	for _, rec := range f.ReceivedMethod("Network.setInterceptionEnabled") {
		if strings.Contains(string(rec.Params), `"enabled":false`) {
			off++
		}
	}
	if off != 1 {
		t.Errorf("interception turned off %d times at Fetch.disable, want once", off)
	}
}

// F9: after a Fetch.disable whose cleanup failed, core no longer records
// the attachment's rules, so a new Fetch.enable installs them again.
func TestFetchEnableAfterFailedDisable(t *testing.T) {
	_, _, c, sid, _, f := enableFetch(t)
	f.Unsupported("Network.removeInterception")
	if e := callErr(t, c, sid, "Fetch.disable", nil); e == nil {
		t.Fatal("Fetch.disable succeeded although WebKit refused the removal")
	}
	f.Handle("Network.removeInterception", nil)
	adds := len(f.ReceivedMethod("Network.addInterception"))
	call(t, c, sid, "Fetch.enable", nil)
	if n := len(f.ReceivedMethod("Network.addInterception")); n != adds+1 {
		t.Errorf("rule installations %d after the new Fetch.enable, want %d", n, adds+1)
	}
}

// F9: WebKit refuses the removal of a rule that the target does not hold.
// The rule is gone, as Fetch.enable wants: the change succeeds, and
// neither the attachment nor core keeps the rule for a later removal.
func TestFetchEnableRemovalOfMissingRule(t *testing.T) {
	_, _, c, sid, _, f := enableFetch(t)
	f.Handle("Network.removeInterception", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "Missing intercept for given url, given isRegex, and given stage"}
	})
	call(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*/b"}}})
	call(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*/b"}}})
	f.Handle("Network.removeInterception", nil)
	call(t, c, sid, "Fetch.disable", nil)
	var old int
	for _, rec := range f.ReceivedMethod("Network.removeInterception") {
		if strings.Contains(string(rec.Params), `"url":"^.*(?:`) {
			old++
		}
	}
	if old != 1 {
		t.Errorf("%d removals of the rule that the target did not hold, want the one refused", old)
	}
}

// F9: a refused Network.setInterceptionEnabled{false} (Fetch.enable with
// no patterns) leaves interception on, so the retry sends it again.
func TestFetchEnableRetryAfterRefusedInterceptionOff(t *testing.T) {
	_, _, c, sid, _, f := enableFetch(t)
	failed := false
	f.Handle("Network.setInterceptionEnabled", func(call fakewebkit.Call) (any, *fakewebkit.Error) {
		if !failed && strings.Contains(string(call.Params), `"enabled":false`) {
			failed = true
			return nil, &fakewebkit.Error{Code: -32000, Message: "transient failure"}
		}
		return nil, nil
	})
	off := func() int {
		n := 0
		for _, rec := range f.ReceivedMethod("Network.setInterceptionEnabled") {
			if strings.Contains(string(rec.Params), `"enabled":false`) {
				n++
			}
		}
		return n
	}
	if e := callErr(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{}}); !strings.Contains(e.Message, "transient failure") {
		t.Fatal(e)
	}
	call(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{}})
	if n := off(); n != 2 {
		t.Errorf("interception turned off %d times, want the refused call and its retry", n)
	}
}

// fetchClaims bounds (fetch_domain.go maxRequestTypes, maxClaimDecisions;
// docs/notes/w5.md, CLI interception and CDP Fetch): the oldest request
// type is forgotten (that request is then Other), and only the newest
// decisions are kept.
func TestFetchClaimsBounds(t *testing.T) {
	var c fetchClaims
	pg := &core.Page{}
	c.join(pg)
	for i := range maxRequestTypes + 1 {
		c.noteType(pg, "t", json.RawMessage(fmt.Sprintf(`{"requestId":"r%d","type":"Script"}`, i)))
	}
	if n := len(c.m[pg].types); n != maxRequestTypes {
		t.Fatalf("%d request types kept, want %d", n, maxRequestTypes)
	}
	ev := func(id string) json.RawMessage {
		return json.RawMessage(`{"requestId":"` + id + `","request":{"url":"https://one.example/"}}`)
	}
	if d := decided(&c, pg, core.EventPosition{}, "t", evRequestIntercepted, ev("r0")); d.typ != "Other" {
		t.Errorf("type of the forgotten oldest request %q, want Other", d.typ)
	}
	if d := decided(&c, pg, core.EventPosition{}, "t", evRequestIntercepted, ev(fmt.Sprint("r", maxRequestTypes))); d.typ != "Script" {
		t.Errorf("type of the newest request %q, want Script", d.typ)
	}
	first := decided(&c, pg, core.EventPosition{Seq: 1}, "t", evRequestIntercepted, ev("x"))
	var last *claimDecision
	for seq := uint64(2); seq <= maxClaimDecisions+1; seq++ {
		last = decided(&c, pg, core.EventPosition{Seq: seq}, "t", evRequestIntercepted, ev("x"))
	}
	if n := len(c.m[pg].decisions); n != maxClaimDecisions {
		t.Fatalf("%d decisions kept, want %d", n, maxClaimDecisions)
	}
	if decided(&c, pg, core.EventPosition{Seq: maxClaimDecisions + 1}, "t", evRequestIntercepted, ev("x")) != last {
		t.Error("the newest event got a second decision")
	}
	if decided(&c, pg, core.EventPosition{Seq: 1}, "t", evRequestIntercepted, ev("x")) == first {
		t.Error("the oldest decision was kept beyond the bound")
	}
	c.leave(pg)
	if c.m[pg] != nil {
		t.Error("the page's arbitration outlived its last attachment")
	}
}

// decided is fetchClaims.decide without its fresh flag.
func decided(c *fetchClaims, pg *core.Page, pos core.EventPosition, target, method string, params json.RawMessage) *claimDecision {
	d, _ := c.decide(pg, pos, target, method, params)
	return d
}
