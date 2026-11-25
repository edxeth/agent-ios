package devtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// W4 repair round 4 (brief W4-R4): no intercepted request is ever left
// paused without an answer, and a removal that a provisional target refused
// is finished by a tombstone.

// wkRules models WebKit's interception rules per target on the fake, as
// InspectorNetworkAgent keeps them (WebKit 7615.3.12.11.2, WebKitGTK 2.54):
// addInterception appends a rule unless an equal one exists, and
// removeInterception removes every equal rule or answers its error for an
// unknown rule; setInterceptionEnabled turns pausing on and off;
// Network.disable clears both and continues every request that the target
// holds (InspectorNetworkAgent::disable and continuePendingRequests,
// webkitgtk-2.54.2 InspectorNetworkAgent.cpp:848-867, 898-910), as the
// fake's own Network.disable does. A test can
// make a target refuse removals (a provisional target), or fail the next
// removal once with another error.
type wkRules struct {
	f        *fakewebkit.Server
	mu       sync.Mutex
	rules    map[string][]core.InterceptRule // by target
	enabled  map[string]bool
	refuse   map[string]bool // target -> refuse every removal
	failOnce map[string]bool // target -> fail the next removal
	removals map[string]int  // target -> removal attempts
	// disabled holds the requests that a Network.disable of their target
	// continued.
	disabled map[reqKey]bool
}

func newWKRules(f *fakewebkit.Server) *wkRules {
	m := &wkRules{
		f: f, rules: map[string][]core.InterceptRule{}, enabled: map[string]bool{},
		refuse: map[string]bool{}, failOnce: map[string]bool{}, removals: map[string]int{},
		disabled: map[reqKey]bool{},
	}
	decode := func(c fakewebkit.Call) core.InterceptRule {
		// WebKit's defaults: caseSensitive true, isRegex false.
		v := struct {
			URL           string `json:"url"`
			Stage         string `json:"stage"`
			CaseSensitive *bool  `json:"caseSensitive"`
			IsRegex       bool   `json:"isRegex"`
		}{}
		_ = json.Unmarshal(c.Params, &v)
		r := core.InterceptRule{URL: v.URL, Stage: v.Stage, CaseSensitive: true, IsRegex: v.IsRegex}
		if v.CaseSensitive != nil {
			r.CaseSensitive = *v.CaseSensitive
		}
		return r
	}
	f.Handle("Network.addInterception", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		r := decode(c)
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, x := range m.rules[c.Target] {
			if x == r {
				return nil, &fakewebkit.Error{Code: -32000, Message: "Intercept for given url, given isRegex, and given stage already exists"}
			}
		}
		m.rules[c.Target] = append(m.rules[c.Target], r)
		return nil, nil
	})
	f.Handle("Network.removeInterception", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		r := decode(c)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.removals[c.Target]++
		if m.refuse[c.Target] {
			return nil, &fakewebkit.Error{Code: -32000, Message: "provisional target refused Network.removeInterception"}
		}
		if m.failOnce[c.Target] {
			delete(m.failOnce, c.Target)
			return nil, &fakewebkit.Error{Code: -32000, Message: "removal failed once"}
		}
		var keep []core.InterceptRule
		for _, x := range m.rules[c.Target] {
			if x != r {
				keep = append(keep, x)
			}
		}
		if len(keep) == len(m.rules[c.Target]) {
			return nil, &fakewebkit.Error{Code: -32000, Message: missingRuleError}
		}
		m.rules[c.Target] = keep
		return nil, nil
	})
	f.Handle("Network.setInterceptionEnabled", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var v struct {
			Enabled bool `json:"enabled"`
		}
		_ = json.Unmarshal(c.Params, &v)
		m.mu.Lock()
		m.enabled[c.Target] = v.Enabled
		m.mu.Unlock()
		return nil, nil
	})
	f.Handle("Network.disable", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		m.mu.Lock()
		delete(m.rules, c.Target)
		delete(m.enabled, c.Target)
		for _, id := range f.ContinueHolds(c.Target) {
			m.disabled[reqKey{c.Target, id}] = true
		}
		m.mu.Unlock()
		return nil, nil
	})
	return m
}

// holds reports whether target holds a rule with url u.
func (m *wkRules) holds(target, u string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rules[target] {
		if r.URL == u {
			return true
		}
	}
	return false
}

// pauses reports whether target pauses a request for rawURL at the request
// stage. The rules of these tests are regular expressions (isRegex), so a
// search with Go's regexp is WebKit's match for them.
func (m *wkRules) pauses(target, rawURL string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled[target] {
		return false
	}
	for _, r := range m.rules[target] {
		if r.Stage == "request" && r.IsRegex && regexp.MustCompile(r.URL).MatchString(rawURL) {
			return true
		}
	}
	return false
}

func (m *wkRules) set(field map[string]bool, target string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if on {
		field[target] = true
	} else {
		delete(field, target)
	}
}

// continuedByDisable reports whether a Network.disable of target continued
// request id.
func (m *wkRules) continuedByDisable(target, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.disabled[reqKey{target, id}]
}

func (m *wkRules) removalCount(target string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.removals[target]
}

// ruleKind is one kind of interception rule of internal/devtools with its
// add and remove calls, its WebKit rule url and a URL it pauses.
type ruleKind struct {
	name, rule, url string
	add, remove     func(ctx context.Context, p *Page, c webkit.Caller) error
}

func ruleKinds() []ruleKind {
	return []ruleKind{
		{
			name: "unroute", rule: "^.*/blocked/[^/]*$", url: "https://fixture.example/blocked/x",
			add: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/blocked/*", Abort: true})
				return err
			},
			remove: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.RemoveRoutes(ctx, c, "s1", "**/blocked/*")
				return err
			},
		},
		{
			name: "credentials clear", rule: "^https://a\\.example/", url: "https://a.example/private",
			add: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.SetCredentials(ctx, c, "s1", "https://a.example", "u", "pw")
				return err
			},
			remove: func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, err := p.ClearCredentials(ctx, c, "s1")
				return err
			},
		},
		{
			name: "offline off", rule: ".*", url: "https://fixture.example/any",
			add: func(ctx context.Context, p *Page, c webkit.Caller) error { return p.SetOffline(ctx, c, "s1", true) },
			remove: func(ctx context.Context, p *Page, c webkit.Caller) error {
				return p.SetOffline(ctx, c, "s1", false)
			},
		},
	}
}

// answers returns the completions that the fake received for request id.
func answers(f *fakewebkit.Server, id string) []fakewebkit.Record {
	var out []fakewebkit.Record
	for _, m := range []string{"Network.interceptContinue", "Network.interceptWithRequest", "Network.interceptWithResponse", "Network.interceptRequestWithResponse", "Network.interceptRequestWithError"} {
		for _, rec := range received(f, m) {
			var v struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(rec.Params, &v) == nil && v.RequestID == id {
				out = append(out, rec)
			}
		}
	}
	return out
}

// oneUnchangedContinue waits for the answer of request id and checks that
// it is exactly one Network.interceptContinue without changes at stage.
func oneUnchangedContinue(t *testing.T, f *fakewebkit.Server, id, stage string) {
	t.Helper()
	eventually(t, "an answer of "+id, func() bool { return len(answers(f, id)) > 0 })
	time.Sleep(100 * time.Millisecond) // a second answer would arrive by now
	got := answers(f, id)
	if len(got) != 1 {
		t.Fatalf("request %s got %d answers, want exactly 1: %v", id, len(got), got)
	}
	var p map[string]any
	_ = json.Unmarshal(got[0].Params, &p)
	if got[0].Method != "Network.interceptContinue" || len(p) != 2 || p["stage"] != stage {
		t.Fatalf("request %s was answered with %s %s, want Network.interceptContinue {requestId, stage: %s}", id, got[0].Method, got[0].Params, stage)
	}
}

// keepRoute is a WIP route that keeps Network and interception enabled
// while a test removes another rule: otherwise the removal's
// Network.disable, which the provisional target accepts, clears its rules
// (InspectorNetworkAgent::disable) and no rule is left behind.
func keepRoute(ctx context.Context, p *Page, c webkit.Caller) error {
	_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/keep/*", Abort: true})
	return err
}

// refusedRemoval adds a keeper route and kind's rule, starts the
// provisional target id, which gets both in its setup, and removes kind's
// rule while that target refuses the forwarded removal. r must pause
// targets on start (newPausingRig), so the removal comes after id's setup
// ended: a removal queued while the setup runs is sent by the setup, and
// its refusal fails the setup (W4 round 7: seen under host load, where the
// committed target then refused the tombstone's removal as setup-failed).
func refusedRemoval(t *testing.T, r *rig, m *wkRules, k ruleKind, id string) {
	t.Helper()
	r.must(t, keepRoute)
	r.must(t, k.add)
	<-r.f.Provisional(id)
	eventually(t, "the provisional setup with the rule", func() bool { return m.holds(id, k.rule) })
	m.set(m.refuse, id, true)
	err := r.do(t, k.remove)
	if err == nil || !strings.Contains(err.Error(), "provisional target refused") {
		t.Fatalf("want the provisional target's refusal, got %v", err)
	}
	if m.holds(r.f.ActiveTarget(), k.rule) || !m.holds(id, k.rule) {
		m.mu.Lock()
		defer m.mu.Unlock()
		t.Fatalf("setup: the active target %s must have removed the rule and the provisional target kept it: %v", r.f.ActiveTarget(), m.rules)
	}
}

func tombstones(p *Page) int {
	p.mu.Lock()
	ic := p.icpt
	p.mu.Unlock()
	ic.mu.Lock()
	defer ic.mu.Unlock()
	return len(ic.tombs)
}

// The removal gap of round 3: a provisional target refuses the forwarded
// removal and commits. The tombstone removes the rule from the committed
// target and goes away.
func TestRemovalRefusedByAProvisionalTargetIsFinishedAtCommit(t *testing.T) {
	for _, k := range ruleKinds() {
		t.Run(k.name, func(t *testing.T) {
			r := newPausingRig(t)
			credentialsVerified[transport.KindIWDP] = true
			t.Cleanup(func() { delete(credentialsVerified, transport.KindIWDP) })
			m := newWKRules(r.f)
			id := "r4-commit"
			refusedRemoval(t, r, m, k, id)
			p := r.page(t)
			if n := tombstones(p); n != 1 {
				t.Fatalf("%d tombstones after the refused removal, want 1", n)
			}
			m.set(m.refuse, id, false) // a committed target accepts removals
			r.f.Commit(id)
			eventually(t, "the tombstone removes the rule from the committed target", func() bool { return !m.holds(id, k.rule) })
			eventually(t, "the tombstone goes", func() bool { return tombstones(p) == 0 })
			// A later removal attempt would mean a retry loop.
			n := m.removalCount(id)
			time.Sleep(100 * time.Millisecond)
			if m.removalCount(id) != n {
				t.Fatal("the tombstone was applied again after it succeeded")
			}
		})
	}
}

// A tombstone never removes a rule that an owner holds again: WebKit keeps
// one copy of equal rules, so the removal would take the new owner's rule.
// Here the route is added again after the refused removal; at commit the
// tombstone sends nothing and goes.
func TestTombstoneSkipsARuleThatIsHeldAgain(t *testing.T) {
	r := newPausingRig(t)
	m := newWKRules(r.f)
	k := ruleKinds()[0]
	id := "r4-again"
	refusedRemoval(t, r, m, k, id)
	// The provisional target still holds the rule, so it refuses the
	// forwarded add as a duplicate; the active target and the model get it.
	if err := r.do(t, k.add); err == nil || !strings.Contains(err.Error(), "stays active") {
		t.Fatalf("want the duplicate refusal of the provisional target and the route kept, got %v", err)
	}
	before := m.removalCount(id)
	m.set(m.refuse, id, false)
	r.f.Commit(id)
	p := r.page(t)
	eventually(t, "the tombstone goes", func() bool { return tombstones(p) == 0 })
	if n := m.removalCount(id) - before; n != 0 || !m.holds(id, k.rule) {
		t.Fatalf("the committed target got %d removals and holds the rule: %v; want no removal of the held rule", n, m.holds(id, k.rule))
	}
}

// The safety net and the release: the committed target keeps the rule
// because the tombstone's attempt at commit fails. A matching request gets
// exactly one answer, an unchanged continue. The release runs the
// tombstone, which removes the rule; a matching request paused after the
// release still gets its one answer, from the safety net.
func TestOrphanRuleRequestsAreContinuedAndReleaseRemovesTheRule(t *testing.T) {
	for _, k := range ruleKinds() {
		t.Run(k.name, func(t *testing.T) {
			r := newPausingRig(t)
			credentialsVerified[transport.KindIWDP] = true
			t.Cleanup(func() { delete(credentialsVerified, transport.KindIWDP) })
			m := newWKRules(r.f)
			id := "r4-orphan"
			refusedRemoval(t, r, m, k, id)
			m.set(m.refuse, id, false)
			m.set(m.failOnce, id, true)
			before := m.removalCount(id)
			r.f.Commit(id)
			eventually(t, "the tombstone's attempt at commit", func() bool { return m.removalCount(id) > before })
			if !m.holds(id, k.rule) || !m.pauses(id, k.url) {
				t.Fatal("setup: the committed target must still pause matching requests")
			}
			intercept(r.f, "r4-before", k.url)
			oneUnchangedContinue(t, r.f, "r4-before", "request")
			attempts := m.removalCount(id)
			time.Sleep(100 * time.Millisecond)
			if m.removalCount(id) != attempts {
				t.Fatal("the failed tombstone was retried before the release")
			}

			// A legacy owner keeps Network and interception, so the release
			// sends no Network.disable and only the tombstone can remove
			// the rule.
			p := r.page(t)
			p.ps.State.Acquire("legacy:x", "Network")
			p.ps.State.SetInterception("legacy:x", true)
			mark := lastSeq(r.f)
			r.release(t)
			if n := len(receivedAfter(r.f, "Network.disable", mark)); n != 0 {
				t.Fatalf("setup: the release sent Network.disable %d times", n)
			}
			if m.holds(id, k.rule) {
				t.Fatal("the release did not remove the rule that the committed target kept")
			}
			if n := tombstones(r.page(t)); n != 0 {
				t.Fatalf("%d tombstones after the release, want 0", n)
			}
			intercept(r.f, "r4-after", k.url)
			oneUnchangedContinue(t, r.f, "r4-after", "request")
		})
	}
}

// W4 rebase onto core-events (the case that W4 round 7 had to avoid): the
// removal reaches the provisional target while its setup runs, so the setup
// sends it and the refusal fails the setup (webkit.SetupError). After the
// commit the session refuses every gated send to the committed target, the
// tombstone's attempt among them, until core applies the page state again
// (Manager.recoverSetup at the next command). Then the target no longer
// holds the rule (the recovery's Network reset clears its rules), and the
// rule can be added and removed again. The netlog owns Network, so the
// removal ends with a Network.setInterceptionEnabled to the active target;
// the provisional setup's Network.addInterception is answered only when that
// call arrives, after the change queued its removal for the provisional
// target, so the setup is still running when the removal is queued.
func TestRemovalRefusedDuringAProvisionalSetupWorksAfterRecovery(t *testing.T) {
	for _, k := range ruleKinds() {
		t.Run(k.name, func(t *testing.T) {
			r := newRig(t)
			credentialsVerified[transport.KindIWDP] = true
			t.Cleanup(func() { delete(credentialsVerified, transport.KindIWDP) })
			m := newWKRules(r.f)
			id := "rebase-recover"
			r.must(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
				_, _, err := p.StartNetlog(ctx, c, "s1")
				return err
			})
			r.must(t, k.add)
			r.f.HoldUntil("Network.addInterception", "Network.setInterceptionEnabled")
			<-r.f.Provisional(id)
			eventually(t, "the provisional setup sends the rule", func() bool { return m.holds(id, k.rule) })
			m.set(m.refuse, id, true)
			if err := r.do(t, k.remove); err == nil || !strings.Contains(err.Error(), "provisional target refused") {
				t.Fatalf("want the provisional target's refusal, got %v", err)
			}
			if !m.holds(id, k.rule) {
				t.Fatal("setup: the provisional target must keep the rule")
			}
			p := r.page(t)
			if n := tombstones(p); n != 1 {
				t.Fatalf("%d tombstones after the refused removal, want 1", n)
			}
			m.set(m.refuse, id, false)
			r.f.Commit(id)
			eventually(t, "the page connection commits "+id, func() bool { return activeTargetOf(p.ps.Caller) == id })
			setup, ok := p.ps.Caller.(interface{ ActiveSetup(context.Context) error })
			if !ok {
				t.Fatalf("the page connection %T does not report its setup", p.ps.Caller)
			}
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := setup.ActiveSetup(sctx)
			cancel()
			if !errors.As(err, new(*webkit.SetupError)) {
				t.Fatalf("setup: the committed target's setup must have failed on the refused removal, got %v", err)
			}

			// The next command applies the page state again before it runs.
			r.must(t, func(context.Context, *Page, webkit.Caller) error { return nil })
			if m.holds(id, k.rule) || m.pauses(id, k.url) {
				t.Fatal("the committed target still holds the rule after core applied the page state again")
			}
			if err := r.do(t, k.add); err != nil {
				t.Fatalf("adding the rule again after the recovery: %v", err)
			}
			if !m.pauses(id, k.url) {
				t.Fatal("the committed target does not pause requests of the rule added again")
			}
			interceptOn(r.f, id, "rebase-req", k.url)
			eventually(t, "the handler answers on "+id, func() bool { return len(answersFor(r.f, "rebase-req")) > 0 })
			if got := answersFor(r.f, "rebase-req"); len(got) != 1 || got[0].Target != id {
				t.Fatalf("answers of the request: %v, want one on %s", got, id)
			}
			if err := r.do(t, k.remove); err != nil {
				t.Fatalf("removing the rule after the recovery: %v", err)
			}
			if m.holds(id, k.rule) {
				t.Fatal("the committed target still holds the rule after its removal")
			}
			r.release(t)
			if n := tombstones(p); n != 0 {
				t.Fatalf("%d tombstones after the release, want 0", n)
			}
		})
	}
}

// A request paused at the response stage that no handler claims is
// continued at the response stage.
func TestSafetyNetContinuesAResponseStagePause(t *testing.T) {
	r := newRig(t)
	_ = r.page(t)
	r.f.Emit("Network.responseIntercepted", map[string]any{
		"requestId": "r4-resp",
		"response":  map[string]any{"url": "https://fixture.example/r", "status": 200, "statusText": "OK", "headers": map[string]string{}, "mimeType": "text/plain", "source": "network"},
	})
	oneUnchangedContinue(t, r.f, "r4-resp", "response")
}

// An owner outside internal/devtools answers what its rule pauses; the
// safety net leaves that request alone at either stage, also after the
// lease ended, and continues a request that its rule does not match.
func TestSafetyNetLeavesOtherOwnersRequestsAlone(t *testing.T) {
	r := newRig(t)
	p := r.page(t)
	p.ps.State.AddInterception("legacy:x", core.InterceptRule{URL: "/legacy/", Stage: "response", CaseSensitive: true, IsRegex: true})
	r.release(t)
	r.f.Emit("Network.responseIntercepted", map[string]any{"requestId": "r4-legacy", "response": map[string]any{"url": "https://fixture.example/legacy/1"}})
	r.f.Emit("Network.requestIntercepted", map[string]any{"requestId": "r4-req", "request": map[string]any{"url": "https://fixture.example/legacy/1"}})
	oneUnchangedContinue(t, r.f, "r4-req", "request")
	if got := answers(r.f, "r4-legacy"); len(got) != 0 {
		t.Fatalf("the legacy owner's response was answered by the daemon: %v", got)
	}
}

// ruleMatches follows InspectorNetworkAgent::shouldIntercept and
// ContentSearchUtilities::createRegularExpressionForSearchString (WebKit
// 7615.3.12.11.2): see the comment of ruleMatches for the rules.
func TestRuleMatchesFollowsWebKit(t *testing.T) {
	const u = "https://Fixture.example/a/b?q=1"
	for _, tc := range []struct {
		name         string
		r            core.InterceptRule
		stage, url   string
		match, known bool
	}{
		{"empty rule url matches every URL", core.InterceptRule{Stage: "request"}, "request", u, true, true},
		{"other stage", core.InterceptRule{Stage: "response"}, "request", u, false, true},
		{"empty URL is never paused", core.InterceptRule{Stage: "request"}, "request", "", false, true},
		{"exact string is the whole URL", core.InterceptRule{URL: u, Stage: "request", CaseSensitive: true}, "request", u, true, true},
		{"exact string is not a substring", core.InterceptRule{URL: "Fixture", Stage: "request", CaseSensitive: true}, "request", u, false, true},
		{"exact string, case-sensitive", core.InterceptRule{URL: strings.ToLower(u), Stage: "request", CaseSensitive: true}, "request", u, false, true},
		{"exact string, case-insensitive", core.InterceptRule{URL: strings.ToLower(u), Stage: "request"}, "request", u, true, true},
		{"the fragment is removed", core.InterceptRule{URL: u, Stage: "request", CaseSensitive: true}, "request", u + "#top", true, true},
		{"regex searches", core.InterceptRule{URL: "/a/", Stage: "request", CaseSensitive: true, IsRegex: true}, "request", u, true, true},
		{"regex, case-sensitive", core.InterceptRule{URL: "fixture", Stage: "request", CaseSensitive: true, IsRegex: true}, "request", u, false, true},
		{"regex, case-insensitive", core.InterceptRule{URL: "fixture", Stage: "request", IsRegex: true}, "request", u, true, true},
		{"JavaScript-only syntax is unknown", core.InterceptRule{URL: "a(?=b)", Stage: "request", IsRegex: true}, "request", u, false, false},
		{"a stage WebKit does not have is unknown", core.InterceptRule{URL: "x"}, "request", u, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, k := ruleMatches(tc.r, tc.stage, tc.url)
			if m != tc.match || k != tc.known {
				t.Fatalf("ruleMatches = (%v, %v), want (%v, %v)", m, k, tc.match, tc.known)
			}
		})
	}
}

// syncBuffer is a log output that tests read while the daemon writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The safety net logs one line per rule and then stays quiet for that
// rule for unclaimedLogEvery; another rule gets its own line. The line
// names the rule by its number (ruleName), never by its URL pattern or the
// request's URL.
func TestSafetyNetLogIsRateLimitedPerRule(t *testing.T) {
	var buf syncBuffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	r := newRig(t)
	p := r.page(t)
	ic := p.icpt
	ic.mu.Lock()
	ic.tombs["t1"] = core.InterceptRule{URL: "/one/", Stage: "request", CaseSensitive: true, IsRegex: true}
	ic.mu.Unlock()
	for i := 0; i < 3; i++ {
		intercept(r.f, fmt.Sprintf("r4-log-%d", i), fmt.Sprintf("https://fixture.example/one/secret-%d", i))
	}
	intercept(r.f, "r4-log-other", "https://fixture.example/two")
	eventually(t, "every continue", func() bool { return len(received(r.f, "Network.interceptContinue")) == 4 })
	lines := strings.Count(buf.String(), "no handler claims it")
	if lines != 2 || strings.Count(buf.String(), "paused by interception rule #1 (request stage)") != 1 ||
		strings.Contains(buf.String(), "secret") || strings.Contains(buf.String(), "/one/") {
		t.Fatalf("log lines %d, want 2 (one per rule, without URLs):\n%s", lines, buf.String())
	}
}

// Review round 3, P1 (intercept.go:438): a JS route whose install outcome
// is unknown (a late Runtime.evaluate reply) stays, so the release clears
// the JS rules of the current document.
func TestJSRouteWithAnUnknownInstallIsClearedOnRelease(t *testing.T) {
	r := newRig(t)
	p := r.page(t)
	// The install's Runtime.evaluate is written; when the page gets it, the
	// call's context ends, and the page reads the call, and answers it, only
	// after AddRoute returned: a late install reply whatever the scheduling.
	// (A 100 ms deadline ended the call before Page.setBootstrapScript was
	// written under -race and a host load of 70, devfix R3.)
	const body = "js-route-marker"
	cancels := make(chan context.CancelFunc, 1)
	returned := make(chan struct{})
	var installed sync.Once
	r.f.BeforeRecord("Target.sendMessageToTarget", func(params json.RawMessage) {
		var outer struct{ Message string }
		_ = json.Unmarshal(params, &outer)
		var inner struct {
			Method string
			Params struct{ Expression string }
		}
		_ = json.Unmarshal([]byte(outer.Message), &inner)
		if inner.Method != "Runtime.evaluate" || !strings.Contains(inner.Params.Expression, body) {
			return
		}
		installed.Do(func() {
			(<-cancels)()
			<-returned
		})
	})
	t.Cleanup(func() { r.f.BeforeRecord("Target.sendMessageToTarget", nil) })
	err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		short, cancel := context.WithCancel(ctx)
		defer cancel()
		cancels <- cancel
		defer close(returned)
		b := body
		_, err := p.AddRoute(short, c, "s1", Route{Pattern: "**/mocked", Body: &b, Mode: ModeJS})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "stays active") {
		t.Fatalf("want the late install reply and the route kept, got %v", err)
	}
	if n := len(p.Routes("s1")); n != 1 {
		t.Fatalf("%d JS routes after an unknown install, want 1", n)
	}
	before := len(received(r.f, "Runtime.evaluate"))
	r.release(t)
	for _, rec := range received(r.f, "Runtime.evaluate")[before:] {
		var v struct{ Expression string }
		_ = json.Unmarshal(rec.Params, &v)
		if v.Expression == jsRoutesClear {
			return
		}
	}
	t.Fatal("the release did not clear the JS route whose install may have run")
}

// A JS route whose bootstrap script the page refused is not installed and
// goes again.
func TestJSRouteRefusedByThePageIsRolledBack(t *testing.T) {
	r := newRig(t)
	_ = r.page(t)
	r.f.Handle("Page.setBootstrapScript", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "refused"}
	})
	body := "x"
	err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
		_, err := p.AddRoute(ctx, c, "s1", Route{Pattern: "**/mocked", Body: &body, Mode: ModeJS})
		return err
	})
	if err == nil || strings.Contains(err.Error(), "stays active") {
		t.Fatalf("want the plain refusal, got %v", err)
	}
	if n := len(r.page(t).Routes("s1")); n != 0 {
		t.Fatalf("%d JS routes after a refused install, want 0", n)
	}
}

// safetyStep is one step of TestEveryPausedRequestGetsExactlyOneAnswerProperty:
// an op, the rule kind (an index of ruleKinds) it uses, and for a request
// whether a live provisional target sends it.
type safetyStep struct {
	op          string
	kind        int
	provisional bool
}

// Property (briefs W4-R4 and W4-R6): under random interleavings of rule
// adds and removals, provisional targets, refused removals, commits,
// aborted provisional targets and releases, every request that a live
// target pauses (the active target or a provisional one) gets exactly one
// successful completion, and it is the completion of the target that
// holds it: the fake takes the hold of the target that a completion is
// sent to (fakewebkit Answers), as WebKit's InspectorNetworkAgent does,
// and a completion sent to another target finds no hold. The fake pauses
// a request only when its target's rules match (wkRules), as WebKit does.
// One exception: a request whose target was destroyed (aborted, or
// replaced by a commit) may get no answer, because WebKit drops its paused
// requests with it and webkit.Session sends nothing to a destroyed
// target; it never gets an answer on another target.
// A request that a Network.disable of its target continued (a reset of
// the target's Network agent) got its one answer from WebKit
// (InspectorNetworkAgent::disable), and no successful completion; the
// scheduler records its hold as reset (core DesiredState.networkDisabled).
func TestEveryPausedRequestGetsExactlyOneAnswerProperty(t *testing.T) {
	credentialsVerified[transport.KindIWDP] = true
	t.Cleanup(func() { delete(credentialsVerified, transport.KindIWDP) })
	var n safetyCounts
	check := func(seed int64) bool { return runSafetyPlan(t, seed, safetyPlan(seed), &n, safetyHooks{}) }
	if s := os.Getenv("AGENT_IOS_SAFETYNET_SEED"); s != "" {
		seed, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("AGENT_IOS_SAFETYNET_SEED=%q: want an integer", s)
		}
		if !check(seed) {
			t.Fatalf("seed %d fails", seed)
		}
		return
	}
	// The fixed plan reaches the case under test on every run (W4 round 9;
	// random draws reach it in most runs only): a provisional target
	// refuses the removal of a route and keeps it, then pauses a request
	// that the route matches. The safety net answers it on that target
	// with one unchanged continue.
	fixed := []safetyStep{{op: "keep"}, {op: "add"}, {op: "provisional"}, {op: "refuse"}, {op: "remove"}, {op: "request", provisional: true}}
	if !runSafetyPlan(t, 0, fixed, &n, safetyHooks{}) || n.continued.Load() != 1 || n.onProvisional.Load() != 1 {
		t.Fatalf("fixed plan: %d unchanged continues and %d answered requests of the live provisional target, want 1 and 1", n.continued.Load(), n.onProvisional.Load())
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 50}); err != nil {
		t.Fatalf("%v (the input is the case's seed; replay it with AGENT_IOS_SAFETYNET_SEED)", err)
	}
	t.Logf("%d paused requests were answered with an unchanged continue; %d requests of a live provisional target were answered on it; a Network.disable continued %d", n.continued.Load(), n.onProvisional.Load(), n.byDisable.Load())
	if n.onProvisional.Load() == 0 {
		t.Fatal("no case answered a request of a live provisional target; the generator does not reach the case under test")
	}
}

// safetyCounts counts the requests of TestEveryPausedRequestGetsExactlyOneAnswerProperty
// that got one unchanged continue, those of a live provisional target that
// were answered on it, and those of a live target that a Network.disable
// continued.
type safetyCounts struct{ continued, onProvisional, byDisable atomic.Int64 }

// safetyPlan draws the plan of one case of the property from seed: 8 to
// 24 steps. The ops are weighted so that refused removals before a commit,
// with a keeper route that leaves Network enabled, are frequent.
func safetyPlan(seed int64) []safetyStep {
	ops := []string{"add", "add", "keep", "remove", "remove", "provisional", "provisional", "refuse", "refuse", "commit", "abort", "release", "request", "request", "request", "request"}
	kinds := len(ruleKinds())
	rnd := rand.New(rand.NewSource(seed))
	var plan []safetyStep
	for range 8 + rnd.Intn(17) {
		plan = append(plan, safetyStep{ops[rnd.Intn(len(ops))], rnd.Intn(kinds), rnd.Intn(2) == 0})
	}
	return plan
}

// safetyHooks let a test order the steps of a plan against the daemon's
// sends; the property uses none.
type safetyHooks struct {
	prepare func(f *fakewebkit.Server) // runs before the first step
	// pause makes target pause request id for rawURL (interceptOn when nil).
	pause func(f *fakewebkit.Server, target, id, rawURL string)
}

// runSafetyPlan runs plan, a random draw or a fixed plan, on a new rig
// and reports whether the property holds; seed names the plan in the log.
func runSafetyPlan(t *testing.T, seed int64, plan []safetyStep, n *safetyCounts, hooks safetyHooks) bool {
	kinds := ruleKinds()
	r := newPausingRig(t)
	m := newWKRules(r.f)
	if hooks.prepare != nil {
		hooks.prepare(r.f)
	}
	ps := r.page(t).ps
	provisional, nTargets, nReqs := "", 0, 0
	var reqs []string
	reqTarget := map[string]string{}
	var trace []string
	for _, st := range plan {
		op, k := st.op, kinds[st.kind]
		switch op {
		case "add":
			trace = append(trace, "add "+k.name)
			_ = r.do(t, k.add)
		case "keep":
			trace = append(trace, "keeper route")
			_ = r.do(t, keepRoute)
		case "remove":
			trace = append(trace, "remove "+k.name)
			_ = r.do(t, k.remove)
		case "provisional":
			if provisional == "" {
				nTargets++
				provisional = fmt.Sprintf("p%d", nTargets)
				trace = append(trace, "provisional "+provisional)
				<-r.f.Provisional(provisional)
			}
		case "refuse":
			if provisional != "" {
				trace = append(trace, "refuse "+provisional)
				m.set(m.refuse, provisional, true)
			}
		case "commit":
			if provisional != "" {
				trace = append(trace, "commit "+provisional)
				m.set(m.refuse, provisional, false)
				r.f.Commit(provisional)
				provisional = ""
			}
		case "abort":
			if provisional != "" {
				trace = append(trace, "abort "+provisional)
				r.f.DestroyTarget(provisional)
				provisional = ""
			}
		case "release":
			trace = append(trace, "release")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := r.m.ReleaseSession(ctx, "s1")
			cancel()
			if err != nil {
				t.Logf("seed %d: release: %v", seed, err)
			}
		case "request":
			target := r.f.ActiveTarget()
			if provisional != "" && st.provisional {
				target = provisional
			}
			if m.pauses(target, k.url) {
				nReqs++
				id := fmt.Sprintf("q%d", nReqs)
				trace = append(trace, "request "+id+" "+k.name+" on "+target)
				reqTarget[id] = target
				if hooks.pause != nil {
					hooks.pause(r.f, target, id, k.url)
				} else {
					interceptOn(r.f, target, id, k.url)
				}
				reqs = append(reqs, id)
				// The next step (a release, a commit) comes after the
				// scheduler recorded the hold, or after its answer.
				for end := time.Now().Add(3 * time.Second); !heldBy(ps, target, id) && len(answers(r.f, id)) == 0 && !m.continuedByDisable(target, id) && time.Now().Before(end); {
					time.Sleep(time.Millisecond)
				}
			}
		}
	}
	live := map[string]bool{r.f.ActiveTarget(): true, provisional: provisional != ""}
	deadline := time.Now().Add(3 * time.Second)
	for _, id := range reqs {
		for live[reqTarget[id]] && len(answersFor(r.f, id)) == 0 && !m.continuedByDisable(reqTarget[id], id) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	time.Sleep(100 * time.Millisecond)
	for _, id := range reqs {
		got := answersFor(r.f, id)
		for _, a := range got {
			if a.Target != reqTarget[id] {
				t.Logf("seed %d: request %s of %s was answered on %s; ops: %s", seed, id, reqTarget[id], a.Target, strings.Join(trace, ", "))
				return false
			}
		}
		if len(got) == 1 && got[0].Method == "Network.interceptContinue" {
			n.continued.Add(1)
		}
		if !live[reqTarget[id]] {
			if len(got) > 1 {
				t.Logf("seed %d: request %s of the destroyed target %s got %d answers; ops: %s", seed, id, reqTarget[id], len(got), strings.Join(trace, ", "))
				return false
			}
			continue // its target was destroyed with its paused requests
		}
		if m.continuedByDisable(reqTarget[id], id) {
			// WebKit answered it: the fake ended its hold, so no completion
			// took it. A completion that the daemon wrote before it read the
			// Network.disable's reply reaches WebKit after the disable and is
			// refused ("Missing pending intercept ..."); only the reply makes
			// the scheduler refuse later ones (networkDisabled). Such a
			// completion must still go to the request's own target.
			for _, rec := range answers(r.f, id) {
				if rec.Target != reqTarget[id] {
					t.Logf("seed %d: request %s of %s, which a Network.disable continued, got a completion on %s; ops: %s", seed, id, reqTarget[id], rec.Target, strings.Join(trace, ", "))
					return false
				}
			}
			n.byDisable.Add(1)
			continue
		}
		if len(got) != 1 || len(answers(r.f, id)) != 1 {
			t.Logf("seed %d: request %s of %s got %d successful completions and %d completion commands, want 1 and 1; ops: %s", seed, id, reqTarget[id], len(got), len(answers(r.f, id)), strings.Join(trace, ", "))
			return false
		}
		if reqTarget[id] == provisional {
			n.onProvisional.Add(1)
		}
	}
	return true
}

// The counterexample of the property that worker 1b-a logged (seed
// 7917047387415046709, about 2 runs in 10). After the commit of the
// provisional target p1, core applies the page state to p1 again with a
// reset of its Network agent (DesiredState.applyResetting): it continues
// the requests that p1 holds through the control lane, then sends
// Network.disable. In the failing runs p1
// paused the plan's request q1 between those two steps, so WebKit
// continued q1 with the Network.disable (InspectorNetworkAgent::disable),
// the scheduler recorded its hold as reset (DesiredState.networkDisabled)
// and the daemon sent q1 no completion. Here the fake pauses q1 when that
// Network.disable arrives, just before it handles it, which makes that
// order the order of every run.
func TestEveryPausedRequestGetsExactlyOneAnswerSeed7917047387415046709(t *testing.T) {
	credentialsVerified[transport.KindIWDP] = true
	t.Cleanup(func() { delete(credentialsVerified, transport.KindIWDP) })
	const seed = 7917047387415046709
	q1 := make(chan func()) // pauses q1
	paused, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(done) }) // runs before the fake closes
	var once sync.Once
	hooks := safetyHooks{
		prepare: func(f *fakewebkit.Server) {
			f.BeforeRecord("Target.sendMessageToTarget", func(params json.RawMessage) {
				var outer struct{ TargetID, Message string }
				_ = json.Unmarshal(params, &outer)
				var inner struct{ Method string }
				_ = json.Unmarshal([]byte(outer.Message), &inner)
				if inner.Method != "Network.disable" || outer.TargetID != "p1" || f.ActiveTarget() != "p1" {
					return
				}
				once.Do(func() {
					select {
					case pause := <-q1:
						pause()
						close(paused)
					case <-done:
					}
				})
			})
			t.Cleanup(func() { f.BeforeRecord("Target.sendMessageToTarget", nil) })
		},
		pause: func(f *fakewebkit.Server, target, id, rawURL string) {
			if id != "q1" {
				interceptOn(f, target, id, rawURL)
				return
			}
			var atDeadline <-chan time.Time // nil without a deadline: wait for the event
			if d, ok := t.Deadline(); ok {
				atDeadline = time.After(time.Until(d))
			}
			select {
			case q1 <- func() { interceptOn(f, target, id, rawURL) }:
				<-paused
			case <-atDeadline:
				t.Fatal("no Network.disable reached p1 after its commit: the plan no longer reaches the case")
			}
		},
	}
	var n safetyCounts
	if !runSafetyPlan(t, seed, safetyPlan(seed), &n, hooks) {
		t.Fatalf("seed %d fails", seed)
	}
	if n.byDisable.Load() != 1 {
		t.Fatalf("%d requests were continued by a Network.disable, want 1 (q1): the run did not reach the case", n.byDisable.Load())
	}
}
