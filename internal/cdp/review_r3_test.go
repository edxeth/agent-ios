package cdp

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W5c-1a review round 3 (/home/devkit/.pi/artifacts/reviewer/w5c1a-r2-review-20261108-222403.md):
// the reviewer's reproductions of F13 and F14, kept as regression tests.

// F13 control: a protocol refusal changes nothing, so a refused add of a
// rule that the owner holds already keeps the owner's rules as they were.
func TestR2DuplicateAddPreservesPriorOwner(t *testing.T) {
	r, _, _, sid, _, f := enableFetch(t)
	_, s := r.pageSession(t, sid)
	owner := s.fetchOwner()
	prior := s.pg.PS.State.InterceptRules()[owner]
	if len(prior) != 1 {
		t.Fatalf("prior rules=%v", prior)
	}
	f.Handle("Network.addInterception", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "Intercept already exists for given url, given isRegex, and given stage"}
	})
	mo, err := r.m.BeginLeased(ctxFor(t, 5*time.Second), s.pg, "s1", s.epoch, "duplicate legacy interception add", core.ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(prior[0])
	_, err = r.m.RawAs(mo.Op.Context(), mo, owner, "Network.addInterception", raw)
	r.m.End(mo.ID)
	if err == nil {
		t.Fatal("duplicate add should fail")
	}
	after := s.pg.PS.State.InterceptRules()[owner]
	if !slices.Equal(prior, after) {
		t.Fatalf("no-effect duplicate add changed prior owner rules: before=%v after=%v", prior, after)
	}
}

// F13: a failed removal of a rule that the owner does not hold must not
// invent a rule of that owner.
func TestR2RefusedUnknownRemovalPreservesPriorOwner(t *testing.T) {
	r, _, _, sid, _, f := enableFetch(t)
	_, s := r.pageSession(t, sid)
	owner := core.SessionOwner("legacy-unknown", "s1")
	rule := s.pg.PS.State.InterceptRules()[s.fetchOwner()][0]
	f.Handle("Network.removeInterception", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32602, Message: "refused before effect"}
	})
	mo, err := r.m.BeginLeased(ctxFor(t, 5*time.Second), s.pg, "s1", s.epoch, "unowned legacy interception removal", core.ClassExclusive)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rule)
	_, err = r.m.RawAs(mo.Op.Context(), mo, owner, "Network.removeInterception", raw)
	r.m.End(mo.ID)
	if err == nil {
		t.Fatal("removal should fail")
	}
	if after := s.pg.PS.State.InterceptRules()[owner]; len(after) != 0 {
		t.Fatalf("no-effect removal invented owner's rules: %v", after)
	}
}

// F14: a new response-stage hold of the same WIP request must not make an
// already continued, still queued request-stage event live again at a
// reset.
func TestR2NewStageDoesNotResurrectOldPause(t *testing.T) {
	r, _, c, sid, _, f := enableFetch(t)
	_, s := r.pageSession(t, sid)
	ps := s.page
	call(t, c, sid, "Fetch.enable", map[string]any{"patterns": []any{
		map[string]any{"urlPattern": "*", "requestStage": "Request"},
		map[string]any{"urlPattern": "*", "requestStage": "Response"},
	}})
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
	f.Emit("Network.requestWillBeSent", wipRequest("same", "main", "0.1", "Fetch", "https://one.example/same", nil))
	waitClosed(t, reached, "attachment held before request")
	n := ps.q.pushed()
	f.InterceptEvent("wk-One", "same", "request", map[string]any{"request": map[string]any{"url": "https://one.example/same", "method": "GET", "headers": map[string]string{}}})
	pushedAtLeast(t, ps, n+1)
	fired := false
	f.Handle("Network.addInterception", func(call fakewebkit.Call) (any, *fakewebkit.Error) {
		if !fired && strings.Contains(string(call.Params), `"stage":"response"`) {
			fired = true
			f.InterceptEvent(call.Target, "same", "response", map[string]any{"response": map[string]any{"url": "https://one.example/same", "status": 200, "statusText": "OK", "headers": map[string]string{}}})
		}
		return nil, nil
	})
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{}})
	if !fired {
		t.Fatal("no response-stage hold was emitted")
	}
	answeredWith(t, f, "same", "Network.interceptContinue")
	close(release)
	waitEvent(t, c, seq, sid, "Log.entryAdded", nil)
	ps.mu.Lock()
	var stale int
	for _, p := range ps.fetch.paused {
		if p.stage == stageRequest {
			stale++
		}
	}
	ps.mu.Unlock()
	pauses := sessionEvents(c, seq, sid, "Fetch.requestPaused")
	if stale != 0 || len(pauses) != 1 {
		t.Fatalf("request-stage records=%d and pause events=%d, want only the new response-stage hold; events=%v", stale, len(pauses), pauses)
	}
}

// F14, the generation: WebKit pauses the same request again at the same
// stage (a redirect keeps the request id) after the reset re-installed the
// rule. The queued event of the hold that the barrier continued stays
// stale; only the new hold is reported.
func TestR3NewGenerationDoesNotResurrectOldPause(t *testing.T) {
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
	f.Emit("Network.requestWillBeSent", wipRequest("again", "main", "0.1", "Fetch", "https://one.example/again", nil))
	waitClosed(t, reached, "attachment held before request")
	n := ps.q.pushed()
	req := map[string]any{"request": map[string]any{"url": "https://one.example/again", "method": "GET", "headers": map[string]string{}}}
	f.InterceptEvent("wk-One", "again", "request", req)
	pushedAtLeast(t, ps, n+1)
	fired := false
	f.Handle("Network.addInterception", func(call fakewebkit.Call) (any, *fakewebkit.Error) {
		if !fired {
			fired = true
			f.InterceptEvent(call.Target, "again", "request", req)
		}
		return nil, nil
	})
	call(t, c, sid, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{}})
	if !fired {
		t.Fatal("the reset installed no rule")
	}
	close(release)
	waitEvent(t, c, seq, sid, "Log.entryAdded", nil)
	pauses := sessionEvents(c, seq, sid, "Fetch.requestPaused")
	ps.mu.Lock()
	held := len(ps.fetch.paused)
	ps.mu.Unlock()
	if len(pauses) != 1 || held != 1 {
		t.Fatalf("pause events=%d, records=%d, want the new hold only", len(pauses), held)
	}
	p := decode[pausedEvent](t, pauses[0].Params)
	call(t, c, sid, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID})
	if h := f.HeldRequests("wk-One"); len(h) != 0 {
		t.Fatalf("held after the client's answer: %v", h)
	}
}
