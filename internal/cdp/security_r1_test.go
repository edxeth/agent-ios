package cdp

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/cdp/cdptest"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// secretsOf returns the token and the endpoint id of an issued endpoint.
func secretsOf(t *testing.T, ep Endpoint) (token, id string) {
	t.Helper()
	_, rest, ok := strings.Cut(ep.HTTP, "/t/")
	if !ok {
		t.Fatalf("endpoint %q has no /t/ segment", ep.HTTP)
	}
	token, rest, _ = strings.Cut(rest, "/e/")
	if token == "" || rest == "" || strings.Contains(rest, "/") {
		t.Fatalf("endpoint %q is not .../t/<token>/e/<endpoint>", ep.HTTP)
	}
	return token, rest
}

// setEnter installs the daemon activity hook under the server lock, so the
// handlers that read it later are ordered after the write.
func setEnter(s *Server, enter func() func()) {
	s.mu.Lock()
	s.o.Enter = enter
	s.mu.Unlock()
}

// noDiscoveryData reports that a response body has no discovery data.
func noDiscoveryData(body string) bool {
	return !strings.Contains(body, "webSocketDebuggerUrl") && !strings.Contains(body, "domains")
}

func decodeString[T any](t *testing.T, s string) T {
	t.Helper()
	return decode[T](t, json.RawMessage(s))
}

// Design 5.2 ("after that its URLs get 404") and 7.8 item 11(e): an
// upgrade that passed the token and record checks, but had not registered
// its connection yet when the record was revoked, never becomes a live
// connection. RevokeSession returns only after that upgrade has finished,
// so a session release after it is final: no connection of the revoked
// URL takes the lease again. (Review reproduction TestReviewUpgradeAfterRevoke.)
func TestRevokedUpgradeNeverGoesLive(t *testing.T) {
	r := newRig(t)
	ep := r.issue(t, "s1", "One", ScopePage)
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	setEnter(r.srv, func() func() {
		once.Do(func() { close(entered) })
		<-resume
		return func() {}
	})
	type result struct {
		c    *cdptest.Client
		resp *http.Response
		err  error
	}
	dialed := make(chan result, 1)
	go func() {
		c, resp, err := cdptest.Dial(ctxFor(t, 10*time.Second), ep.WS, nil)
		dialed <- result{c, resp, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the upgrade did not reach the activity hook")
	}
	revoked := make(chan int, 1)
	go func() { revoked <- r.srv.RevokeSession("s1") }()
	eventually(t, 5*time.Second, "the record leaves the endpoint table", func() bool { return len(r.srv.Status().Endpoints) == 0 })
	n, returned := 0, false
	select {
	case n = <-revoked:
		returned = true
		t.Error("RevokeSession returned while an admitted upgrade of the revoked record was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(resume)
	if !returned {
		select {
		case n = <-revoked:
		case <-time.After(10 * time.Second):
			t.Fatal("RevokeSession did not return after the upgrade went on")
		}
	}
	if n != 1 {
		t.Fatalf("RevokeSession = %d records, want 1", n)
	}
	if _, err := r.m.ReleaseSession(ctxFor(t, 5*time.Second), "s1"); err != nil {
		t.Fatal(err)
	}
	var got result
	select {
	case got = <-dialed:
	case <-time.After(10 * time.Second):
		t.Fatal("the dial did not finish")
	}
	if got.err == nil {
		defer got.c.Close()
		_, callErr := got.c.Call(ctxFor(t, 5*time.Second), "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
		holder, epoch := r.leaseOf(t, "One")
		t.Fatalf("the revoked URL upgraded (setAutoAttach error: %v); %d connection(s) live; lease of One: %q at epoch %d", callErr, r.srv.Status().Connections, holder, epoch)
	}
	if got.resp == nil || got.resp.StatusCode != http.StatusNotFound {
		t.Errorf("refused upgrade: %v (response %v), want 404 like any revoked URL", got.err, got.resp)
	}
	if holder, _ := r.leaseOf(t, "One"); holder != "" {
		t.Errorf("lease of One after revoke and release: %q, want none", holder)
	}
	if c := r.srv.Status().Connections; c != 0 {
		t.Errorf("%d connection(s) after the revocation, want 0", c)
	}
}

// Design 5.2: revocation ends the authenticated HTTP actions of the
// record too. A PUT action in flight is cancelled at once (its probe of a
// silent page would wait probeWait), and a following release leaves no
// lease. That RevokeSession waits for admitted requests is pinned by
// TestRevokedUpgradeNeverGoesLive, which can hold one server-side.
func TestRevokeCancelsAdmittedHTTPActions(t *testing.T) {
	r := newRig(t)
	ep := r.issue(t, "s1", "One", ScopePage)
	code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil)
	if code != 200 {
		t.Fatalf("/json/list: %d %s", code, body)
	}
	entries := decodeString[[]map[string]any](t, body)
	if len(entries) == 0 {
		t.Fatalf("/json/list is empty: %s", body)
	}
	id, _ := entries[0]["id"].(string)
	f := r.fake["One"]
	f.Stall("Runtime.evaluate")
	type answer struct {
		code int
		at   time.Time
	}
	answered := make(chan answer, 1)
	go func() {
		c, _ := httpDo(t, "PUT", ep.HTTP+"/json/activate/"+id, nil)
		answered <- answer{c, time.Now()}
	}()
	eventually(t, 5*time.Second, "the activation probe reaches the page", func() bool { return len(f.ReceivedMethod("Runtime.evaluate")) > 0 })
	r.srv.RevokeSession("s1")
	returned := time.Now()
	var a answer
	select {
	case a = <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("the PUT action did not end")
	}
	// The client sees the answer after the handler returned; allow for
	// that, but far less than probeWait.
	if late := a.at.Sub(returned); late > probeWait/4 {
		t.Errorf("the PUT action of the revoked record ended %v after RevokeSession returned; want it cancelled at once", late)
	}
	if a.code == http.StatusOK {
		t.Errorf("the PUT action of a revoked record succeeded")
	}
	if _, err := r.m.ReleaseSession(ctxFor(t, 5*time.Second), "s1"); err != nil {
		t.Fatal(err)
	}
	if holder, _ := r.leaseOf(t, "One"); holder != "" {
		t.Errorf("lease of One after revoke and release: %q, want none", holder)
	}
}

// Design 5.2 (scope device uses the record's reservation owner; issuing
// again for the same session, page and scope returns the same record):
// repeated issuing, with the same or another owner, runs beside discovery
// on the same record without a data race (go test -race), and discovery
// uses the owner of the latest issue. (Review reproductions
// TestReviewConcurrentIssue and TestReviewIssueWhileListing.)
func TestReissueBesideDiscovery(t *testing.T) {
	r := newRig(t)
	ep := r.issue(t, "s1", "One", ScopeDevice)
	issueAs := func(owner string) Endpoint {
		req := r.req
		req.Page = "One"
		e, err := r.srv.Issue(ctxFor(t, 5*time.Second), IssueRequest{Session: "s1", Reservation: owner, Page: req, Scope: ScopeDevice})
		if err != nil {
			t.Errorf("Issue as %s: %v", owner, err)
		}
		return e
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 30 {
			issueAs([]string{"o1", "o2"}[i%2])
		}
	}()
	for range 30 {
		if code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil); code != 200 {
			t.Errorf("/json/list beside Issue: %d %s", code, body)
		}
	}
	wg.Wait()
	titles := func() string {
		code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil)
		if code != 200 {
			t.Fatalf("/json/list: %d %s", code, body)
		}
		var out []string
		for _, e := range decodeString[[]map[string]any](t, body) {
			out = append(out, e["title"].(string))
		}
		return strings.Join(out, ",")
	}
	if e := issueAs("o2"); e.WS != ep.WS {
		t.Fatalf("issuing again with owner o2 gave another record")
	}
	if got := titles(); got != "One" {
		t.Errorf("device scope for owner o2 (no reservation) lists %s, want One", got)
	}
	issueAs("o1")
	if got := titles(); got != "One,Two" {
		t.Errorf("device scope for owner o1 (device A reserved) lists %s, want One,Two", got)
	}
}

// Design 5.1 ("it is never logged, and URLs in logs and status output are
// redacted"): the token and endpoint ids stay out of the log for the life
// of the process, also for late requests to a revoked record, to a
// previous listener's token, or with a secret outside the path. (Review
// reproduction TestReviewRetiredPathLeaksSecrets.)
func TestRedactionOutlivesRevocationAndStop(t *testing.T) {
	r := newRig(t)
	ep := r.issue(t, "s1", "One", ScopePage)
	token, id := secretsOf(t, ep)
	nullOrigin := func(q *http.Request) { q.Header.Set("Origin", "null") }
	// A live record, with the URL itself as the Origin header (a client
	// that sends its endpoint as origin).
	if code, _ := httpDo(t, "GET", ep.HTTP+"/json/version", func(q *http.Request) { q.Header.Set("Origin", ep.HTTP) }); code != 403 {
		t.Fatalf("Origin with the URL: %d, want 403", code)
	}
	r.srv.RevokeSession("s1")
	if code, _ := httpDo(t, "GET", ep.HTTP+"/json/version", nullOrigin); code != 403 {
		t.Fatalf("revoked URL with Origin null: %d, want 403", code)
	}
	r.srv.Stop()
	if _, err := r.srv.Start(0, nil); err != nil {
		t.Fatal(err)
	}
	addr := r.srv.Status().Addr
	old := "/t/" + token + "/e/" + id
	for _, p := range []string{old + "/json/version", "/x" + old + "/devtools/browser/" + id, old + "/devtools/browser/" + id} {
		if code, _ := httpDo(t, "GET", "http://"+addr+p, nullOrigin); code != 403 {
			t.Errorf("previous token with Origin null: %d, want 403", code)
		}
		if code, _ := httpDo(t, "GET", "http://"+addr+p, func(q *http.Request) { q.Host = "evil.example" }); code != 403 {
			t.Errorf("previous token with a foreign Host: %d, want 403", code)
		}
	}
	log := r.log.text()
	if strings.Contains(log, token) || strings.Contains(log, id) {
		t.Fatalf("the log has a token or endpoint id of a revoked record or a previous listener:\n%s", log)
	}
	if !strings.Contains(log, "/t/<token>/e/<endpoint>") {
		t.Fatalf("the refused requests were not logged with the redacted path:\n%s", log)
	}
}

// Design 5.1: redaction replaces exactly the secret path segments (the
// token after /t/, the endpoint id after /e/ and /devtools/browser/),
// known or not, and keeps the rest of the log line.
func TestRedactKeepsTheRestOfTheLine(t *testing.T) {
	s := New(Options{})
	for in, want := range map[string]string{
		"cdp: refused GET /t/abc/e/def/json/version: Origin \"null\" is not allowed":  "cdp: refused GET /t/<token>/e/<endpoint>/json/version: Origin \"null\" is not allowed",
		"refused PUT /t/abc/e/def/devtools/browser/def?x: Host \"evil\"":              "refused PUT /t/<token>/e/<endpoint>/devtools/browser/<endpoint>?x: Host \"evil\"",
		"Origin \"http://127.0.0.1:1/x/t/abc#f\" and /t/ alone":                       "Origin \"http://127.0.0.1:1/x/t/<token>#f\" and /t/ alone",
		"cdp3 connected (session \"s1\") to /devtools/page/0A1B for https://a.test/s": "cdp3 connected (session \"s1\") to /devtools/page/0A1B for https://a.test/s",
	} {
		if got := s.redact(in); got != want {
			t.Errorf("redact(%q)\n = %q\nwant %q", in, got, want)
		}
	}
}

// Design 5.1: PUT /json/new?<url> opens a tab with window.open and answers
// its /json/list entry (the URL query-decoded, Chrome's form); PUT
// /json/activate/<id> probes the tab; PUT /json/close/<id> runs
// window.close() and waits until the list no longer shows it. A bad URL
// encoding is 400; an unknown target or a page that another session leases
// is an error with the reason.
func TestAuthenticatedPutActions(t *testing.T) {
	r := newRig(t)
	const opened = "https://opened.example/a b?q=1&r=%2F"
	var mu sync.Mutex
	var exprs []string
	r.fake["One"].Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var p struct{ Expression string }
		_ = json.Unmarshal(c.Params, &p)
		mu.Lock()
		exprs = append(exprs, p.Expression)
		mu.Unlock()
		if strings.Contains(p.Expression, "window.open") {
			r.addPage(t, r.devA, "Opened")
			return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "number", "value": 1}}, nil
	})
	ep := r.issue(t, "s1", "One", ScopePage)
	code, body := httpDo(t, "PUT", ep.HTTP+"/json/new?"+url.QueryEscape(opened), nil)
	if code != 200 {
		t.Fatalf("PUT /json/new: %d %s", code, body)
	}
	entry := decodeString[map[string]any](t, body)
	newID, _ := entry["id"].(string)
	_, endpointID := secretsOf(t, ep)
	wsBase := strings.TrimSuffix(ep.WS, "/devtools/browser/"+endpointID)
	if newID == "" || entry["type"] != "page" || entry["title"] != "Opened" || entry["webSocketDebuggerUrl"] != wsBase+"/devtools/page/"+newID {
		t.Fatalf("PUT /json/new entry = %v", entry)
	}
	lit, _ := json.Marshal(opened)
	mu.Lock()
	sawOpen := false
	for _, e := range exprs {
		sawOpen = sawOpen || strings.Contains(e, "window.open("+string(lit))
	}
	mu.Unlock()
	if !sawOpen {
		t.Fatalf("no window.open(%s) in the page; expressions: %v", lit, exprs)
	}
	if _, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil); !strings.Contains(body, newID) {
		t.Fatalf("the new tab is not in the record's scope: %s", body)
	}
	if code, body := httpDo(t, "PUT", ep.HTTP+"/json/activate/"+newID, nil); code != 200 || body != "Target activated" {
		t.Fatalf("PUT /json/activate: %d %q", code, body)
	}
	r.fake["Opened"].Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		r.devA.RemovePage(r.listed["Opened"])
		return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
	})
	if code, body := httpDo(t, "PUT", ep.HTTP+"/json/close/"+newID, nil); code != 200 || body != "Target is closing" {
		t.Fatalf("PUT /json/close: %d %q", code, body)
	}
	if _, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil); strings.Contains(body, newID) {
		t.Fatalf("the closed tab is still listed: %s", body)
	}
	// Errors.
	if code, body := httpDo(t, "PUT", ep.HTTP+"/json/new?%zz", nil); code != http.StatusBadRequest || !strings.Contains(body, "bad URL") {
		t.Errorf("PUT /json/new with a bad escape: %d %q, want 400 bad URL", code, body)
	}
	for _, p := range []string{"/json/activate/NOPE", "/json/close/NOPE"} {
		if code, body := httpDo(t, "PUT", ep.HTTP+p, nil); code != http.StatusInternalServerError || !strings.Contains(body, "No target with given id found") {
			t.Errorf("PUT %s: %d %q, want 500 with the CDP message", p, code, body)
		}
	}
	req := r.req
	req.Page = "Two"
	if _, _, err := r.m.Touch(ctxFor(t, 5*time.Second), req, core.AcquireOptions{Session: "s2", Reservation: "o1"}); err != nil {
		t.Fatal(err)
	}
	_, e2 := r.leaseOf(t, "Two")
	two := r.issue(t, "s1", "Two", ScopePage)
	_, body = httpDo(t, "GET", two.HTTP+"/json/list", nil)
	twoID, _ := decodeString[[]map[string]any](t, body)[0]["id"].(string)
	if code, body := httpDo(t, "PUT", two.HTTP+"/json/activate/"+twoID, nil); code != http.StatusInternalServerError || !strings.Contains(body, "leased by session \"s2\"") {
		t.Errorf("PUT /json/activate on a page leased by s2: %d %q, want 500 with the lease message", code, body)
	}
	if code, body := httpDo(t, "PUT", two.HTTP+"/json/new?about:blank", nil); code != http.StatusInternalServerError || !strings.Contains(body, "leased by session \"s2\"") {
		t.Errorf("PUT /json/new from a page leased by s2: %d %q, want 500 with the lease message", code, body)
	}
	if s, e := r.leaseOf(t, "Two"); s != "s2" || e != e2 {
		t.Errorf("Two's lease after the refused actions: %s/%d, want s2/%d", s, e, e2)
	}
}

// Design 5.1: GET /json/protocol (with or without the trailing slash) is
// the supported subset in CDP JSON form, generated from the handler table:
// every table method is listed under its domain and nothing else is.
// The discovery paths accept only GET.
func TestProtocolDiscovery(t *testing.T) {
	r := newRig(t)
	ep := r.issue(t, "s1", "One", ScopePage)
	type proto struct {
		Version struct{ Major, Minor string }
		Domains []struct {
			Domain   string
			Commands []struct{ Name string }
			Events   []struct{ Name string }
		}
	}
	for _, p := range []string{"/json/protocol", "/json/protocol/"} {
		req, err := http.NewRequestWithContext(ctxFor(t, 5*time.Second), "GET", ep.HTTP+p, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var got proto
		err = json.NewDecoder(resp.Body).Decode(&got)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Fatalf("GET %s: %d %s, %v", p, resp.StatusCode, resp.Header.Get("Content-Type"), err)
		}
		if got.Version.Major != "1" || got.Version.Minor != "3" {
			t.Errorf("protocol version %+v, want 1.3", got.Version)
		}
		listed := map[string]bool{}
		events := map[string]bool{}
		for _, d := range got.Domains {
			for _, c := range d.Commands {
				listed[d.Domain+"."+c.Name] = true
			}
			for _, e := range d.Events {
				events[d.Domain+"."+e.Name] = true
			}
		}
		tableMu.RLock()
		for name := range table {
			if !listed[name] {
				t.Errorf("%s is in the handler table but not in /json/protocol", name)
			}
		}
		for name := range listed {
			if _, ok := table[name]; !ok {
				t.Errorf("/json/protocol lists %s, which has no handler", name)
			}
		}
		tableMu.RUnlock()
		for _, want := range []string{"Target.setAutoAttach", "Target.attachToTarget", "Browser.getVersion", "Runtime.evaluate"} {
			if !listed[want] {
				t.Errorf("/json/protocol lacks %s", want)
			}
		}
		if !events["Target.attachedToTarget"] {
			t.Errorf("/json/protocol lacks the event Target.attachedToTarget")
		}
	}
	for _, p := range []string{"/json/protocol", "/json/version", "/json/list"} {
		if code, body := httpDo(t, "PUT", ep.HTTP+p, nil); code != http.StatusMethodNotAllowed || !noDiscoveryData(body) {
			t.Errorf("PUT %s: %d %q, want 405 without data", p, code, body)
		}
	}
}

// Design 5.1 and 5.2, property over random sequences of issue (and
// reissue), revoke, release, listener restart, WebSocket upgrades, HTTP
// requests and CDP attach calls, on live and on revoked or stale URLs:
//   - a revoked or stale URL gets 404 (403 with an Origin header) and no
//     discovery data, never upgrades, and never reaches a page lease: a
//     session holds a lease only when one of its live URLs acted since its
//     last release;
//   - revocation and restart close the connections of the revoked records;
//   - the endpoint table holds exactly the live records, and issuing again
//     for a live session, page and scope returns the same record;
//   - no token or endpoint id ever issued appears in any log line, also
//     when a client sends one, current or retired, as a header value
//     (Origin, Host, Referer).
//
// Every other run starts with reachPlan, a sequence that reaches each case
// of the reach check by construction, then runs its random operations; the
// other runs are random only. So the check that the property is not
// vacuous holds on every run of the test, without a fixed seed (a random
// sequence of at most 20 operations reaches an attach only when it issues,
// upgrades and calls setAutoAttach in that order).
func TestEndpointLifecycleProperty(t *testing.T) {
	var reach struct {
		reissues, staleRequests, staleUpgrades, staleCalls, liveUpgrades, liveRequests, attaches, revokes, releases, restarts, headerRefusals, headerSecrets int
	}
	sessions := []string{"s1", "s2"}
	pages := []string{"One", "Two"}
	op := func(kind, arg int) uint8 { return uint8(kind + 7*arg) } // the decoding of the switch below
	reachPlan := []uint8{
		op(0, 0), // issue s1, page One, page scope
		op(3, 0), // live upgrade of its browser endpoint
		op(5, 0), // setAutoAttach on it: an attach
		op(4, 0), // live GET /json/version
		op(4, 6), // GET /json/version with a secret as Origin: a header refusal
		op(1, 0), // revoke s1
		op(4, 0), // stale request
		op(3, 0), // stale upgrade
		op(5, 0), // stale call on the closed connection
		op(2, 0), // release s1
		op(6, 0), // listener restart
	}
	runs := 0
	prop := func(ops []uint8) (ok bool) {
		if len(ops) > 20 {
			ops = ops[:20]
		}
		if runs++; runs%2 == 1 {
			ops = append(slices.Clone(reachPlan), ops...)
			defer func() {
				if !ok {
					t.Logf("ops %v start with the %d operations of the reach plan", ops, len(reachPlan))
				}
			}()
		}
		r := newRig(t)
		type issued struct {
			session, page, scope, id string
			ep                       Endpoint
			pageID                   string
		}
		type open struct {
			c    *cdptest.Client
			u    *issued
			page bool
		}
		var urls []*issued
		var conns []*open
		live := map[string]bool{}
		secrets := map[string]bool{}
		mayHold := map[string]bool{} // a live URL of the session acted since its last release
		autoOn := map[string]bool{}  // a live browser connection of the session auto-attaches
		retarget := func(u string) string {
			scheme, rest, _ := strings.Cut(u, "://")
			_, path, _ := strings.Cut(rest, "/")
			return scheme + "://" + r.srv.Status().Addr + "/" + path
		}
		closedWithin := func(o *open) bool {
			select {
			case <-o.c.Done():
				return true
			case <-time.After(5 * time.Second):
				return false
			}
		}
		defer func() {
			for _, o := range conns {
				o.c.Close()
			}
		}()
		for step, b := range ops {
			kind, arg := int(b%7), int(b/7)
			switch kind {
			case 0: // issue or reissue
				s, p, scope := sessions[arg%2], pages[arg/2%2], ScopePage
				if arg/4%2 == 1 {
					scope = ScopeDevice
				}
				ep := r.issue(t, s, p, scope)
				token, id := secretsOf(t, ep)
				secrets[token], secrets[id] = true, true
				known := false
				for _, u := range urls {
					if live[u.id] && u.session == s && u.page == p && u.scope == scope {
						if u.id != id {
							t.Logf("step %d: issuing again for %s/%s/%s gave a new record while one is live", step, s, p, scope)
							return false
						}
						known = true
						reach.reissues++
					}
				}
				if known {
					break
				}
				code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil)
				if code != 200 {
					t.Logf("step %d: /json/list of a new record: %d %s", step, code, body)
					return false
				}
				pageID, _ := decodeString[[]map[string]any](t, body)[0]["id"].(string)
				urls = append(urls, &issued{session: s, page: p, scope: scope, id: id, ep: ep, pageID: pageID})
				live[id] = true
			case 1: // revoke
				s := sessions[arg%2]
				r.srv.RevokeSession(s)
				reach.revokes++
				for _, u := range urls {
					if u.session == s {
						live[u.id] = false
					}
				}
				autoOn[s] = false
				for _, o := range conns {
					if o.u.session == s && !closedWithin(o) {
						t.Logf("step %d: a connection of a revoked record of %s is still open", step, s)
						return false
					}
				}
			case 2: // release
				s := sessions[arg%2]
				if _, err := r.m.ReleaseSession(ctxFor(t, 5*time.Second), s); err != nil {
					t.Logf("step %d: ReleaseSession(%s): %v", step, s, err)
					return false
				}
				reach.releases++
				mayHold[s] = autoOn[s]
			case 3: // WebSocket upgrade, browser or page endpoint
				if len(urls) == 0 {
					break
				}
				u := urls[arg%len(urls)]
				page := arg/len(urls)%2 == 1
				target := u.ep.WS
				if page {
					target = strings.TrimSuffix(u.ep.WS, "/browser/"+u.id) + "/page/" + u.pageID
				}
				c, resp, err := cdptest.Dial(ctxFor(t, 10*time.Second), retarget(target), nil)
				switch {
				case !live[u.id]:
					reach.staleUpgrades++
					if err == nil {
						c.Close()
						t.Logf("step %d: a revoked URL upgraded", step)
						return false
					}
					if resp == nil || resp.StatusCode != http.StatusNotFound {
						t.Logf("step %d: revoked URL upgrade: %v, want 404", step, err)
						return false
					}
				case err != nil:
					// A page endpoint attaches first; another session's lease refuses it.
					if !page || resp == nil || resp.StatusCode != http.StatusConflict {
						t.Logf("step %d: live URL upgrade (page %v): %v", step, page, err)
						return false
					}
				default:
					reach.liveUpgrades++
					conns = append(conns, &open{c: c, u: u, page: page})
					if page {
						mayHold[u.session] = true
					}
				}
			case 4: // HTTP request, without a refused header, with Origin null, or with a secret as Origin or Host
				if len(urls) == 0 {
					break
				}
				u := urls[arg%len(urls)]
				variant := arg / len(urls)
				method, path := "GET", []string{"/json/version", "/json/list", "/json/activate/" + u.pageID}[variant%3]
				if variant%3 == 2 {
					method = "PUT"
				}
				header := variant / 3 % 4
				ever := slices.Sorted(maps.Keys(secrets))
				secret := ever[arg%len(ever)] // a current or a retired token or endpoint id
				code, body := httpDo(t, method, retarget(u.ep.HTTP)+path, func(q *http.Request) {
					switch header {
					case 1:
						q.Header.Set("Origin", "null")
					case 2:
						q.Header.Set("Origin", secret)
						q.Header.Set("Referer", "http://a.test/"+secret)
					case 3:
						q.Host = secret
						q.Header.Set("Referer", secret)
					}
				})
				if header >= 2 {
					reach.headerSecrets++
				}
				switch {
				case header != 0:
					reach.headerRefusals++
					if code != http.StatusForbidden || !noDiscoveryData(body) {
						t.Logf("step %d: request with refused header variant %d: %d %q, want 403", step, header, code, body)
						return false
					}
				case !live[u.id]:
					reach.staleRequests++
					if code != http.StatusNotFound || !noDiscoveryData(body) {
						t.Logf("step %d: %s on a revoked URL: %d %q, want 404", step, method, code, body)
						return false
					}
				default:
					reach.liveRequests++
					if method == "GET" && code != 200 {
						t.Logf("step %d: GET %s on a live URL: %d %q", step, path, code, body)
						return false
					}
					if method == "PUT" {
						mayHold[u.session] = true
					}
				}
			case 5: // a CDP call that attaches, on an open browser connection
				var browser []*open
				for _, o := range conns {
					if !o.page {
						browser = append(browser, o)
					}
				}
				if len(browser) == 0 {
					break
				}
				o := browser[arg%len(browser)]
				_, err := o.c.Call(ctxFor(t, 10*time.Second), "", "Target.setAutoAttach", map[string]any{"autoAttach": true, "flatten": true})
				if !live[o.u.id] {
					reach.staleCalls++
					if err == nil {
						t.Logf("step %d: a connection of a revoked record answered setAutoAttach", step)
						return false
					}
					break
				}
				if err != nil {
					t.Logf("step %d: setAutoAttach on a live connection: %v", step, err)
					return false
				}
				reach.attaches++
				mayHold[o.u.session], autoOn[o.u.session] = true, true
			case 6: // listener restart: every record is revoked, the token changes
				r.srv.Stop()
				if _, err := r.srv.Start(0, nil); err != nil {
					t.Logf("step %d: Start: %v", step, err)
					return false
				}
				reach.restarts++
				for id := range live {
					live[id] = false
				}
				autoOn = map[string]bool{}
				for _, o := range conns {
					if !closedWithin(o) {
						t.Logf("step %d: a connection is still open after the restart", step)
						return false
					}
				}
			}
			// Invariants after every step.
			log := r.log.text()
			for sec := range secrets {
				if strings.Contains(log, sec) {
					t.Logf("step %d: a token or endpoint id is in the log:\n%s", step, log)
					return false
				}
			}
			n := 0
			for _, ok := range live {
				if ok {
					n++
				}
			}
			if got := len(r.srv.Status().Endpoints); got != n {
				t.Logf("step %d: %d endpoint records, model %d live", step, got, n)
				return false
			}
			for _, ps := range r.m.Status() {
				if ps.Session != "" && !mayHold[ps.Session] {
					t.Logf("step %d: %s holds the lease of %q although none of its live URLs acted since its release", step, ps.Session, ps.Title)
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 60}); err != nil {
		t.Fatal(err)
	}
	if reach.staleRequests == 0 || reach.staleUpgrades == 0 || reach.liveUpgrades == 0 || reach.attaches == 0 ||
		reach.revokes == 0 || reach.releases == 0 || reach.restarts == 0 || reach.headerRefusals == 0 || reach.liveRequests == 0 || reach.headerSecrets == 0 {
		t.Fatalf("generator reach %+v: want each case", reach)
	}
	t.Logf("reach %+v", reach)
}
