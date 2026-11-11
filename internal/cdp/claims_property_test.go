package cdp

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"
)

// F12 (W5c-1a review round 3): a generated-history property of the live
// Fetch arbitration between the CDP attachments of one page (fetch_domain.go
// fetchClaims: pageClaims, decide, take, through onWire, fetchIntercepted
// and continueUnreported). A history attaches up to three attachments, some
// after requests started, with patterns whose resource types agree or
// disagree; it starts requests (requestWillBeSent), pauses them (request
// stage), sometimes while some attachments hold the event queued and
// disable Fetch before they handle it (withdrawals), and at the end the
// clients answer what they were told. The oracle (the domain comment of
// fetch_domain.go and docs/notes/w5.md, "No request stays paused" and
// "Between CDP attachments"):
//   - a paused request is reported exactly to the attachments whose
//     pattern claimed it (URL and resource type, from the page's record of
//     its requestWillBeSent, else Other) and that did not withdraw, or, when
//     there is none, it is continued by the bridge;
//   - a reported request is not continued silently: until a client answers
//     or a reporter disables Fetch, nobody completes it;
//   - every paused request gets exactly one completion, and none is left
//     paused at the end.

// claimPat is a Fetch pattern of an attachment (request stage).
type claimPat struct{ URL, Type string }

// claimOp is one step of a history.
type claimOp struct {
	Kind int   // claimAttach, claimSend, claimPause
	Req  int   // claimSend, claimPause
	Held []int // claimPause: attachments that hold the event and disable Fetch before they handle it
}

const (
	claimAttach = iota
	claimSend
	claimPause
)

type claimHistory struct {
	Pats [][]claimPat // per attachment; attachment 0 is attached at the start
	Ops  []claimOp
}

var (
	claimURLs     = []string{"*", "*.js"}
	claimPatTypes = []string{"", "Script", "Image", "Other"}
	claimReqTypes = []string{"Script", "Image", "Other"}
)

// claimReq is the URL and WIP type of request j.
func claimReq(j int) (url, typ string) {
	ext := ".js"
	if j%2 == 1 {
		ext = ".png"
	}
	return fmt.Sprintf("https://one.example/r%d%s", j, ext), claimReqTypes[(j/2)%len(claimReqTypes)]
}

// Generate draws a history: attachments, their patterns, and a valid order
// of attachments, request starts and pauses.
func (claimHistory) Generate(r *rand.Rand, _ int) reflect.Value {
	var h claimHistory
	n := 1 + r.Intn(3)
	for range n {
		var pats []claimPat
		for range 1 + r.Intn(2) {
			pats = append(pats, claimPat{URL: claimURLs[r.Intn(len(claimURLs))], Type: claimPatTypes[r.Intn(len(claimPatTypes))]})
		}
		h.Pats = append(h.Pats, pats)
	}
	attached, sent, paused, disabled := 1, map[int]bool{}, map[int]bool{}, map[int]bool{}
	reqs := 1 + r.Intn(4)
	for len(paused) < reqs {
		switch k := r.Intn(4); {
		case k == 0 && attached < n:
			h.Ops = append(h.Ops, claimOp{Kind: claimAttach})
			attached++
		case k == 1:
			if j := r.Intn(reqs); !sent[j] && !paused[j] {
				sent[j] = true
				h.Ops = append(h.Ops, claimOp{Kind: claimSend, Req: j})
			}
		default:
			j := r.Intn(reqs)
			if paused[j] {
				continue
			}
			paused[j] = true
			op := claimOp{Kind: claimPause, Req: j}
			if r.Intn(2) == 0 {
				for i := range attached {
					if !disabled[i] && r.Intn(2) == 0 {
						op.Held = append(op.Held, i)
						disabled[i] = true
					}
				}
			}
			h.Ops = append(h.Ops, op)
		}
	}
	return reflect.ValueOf(h)
}

// claims reports whether pats claim request j of type typ.
func claimMatch(pats []claimPat, url, typ string) bool {
	for _, p := range pats {
		if (p.URL == "*" || strings.HasSuffix(url, ".js")) && (p.Type == "" || p.Type == typ) {
			return true
		}
	}
	return false
}

// holdInterceptions makes the attachment's goroutine stop before the next
// interception event until release is closed; reached is closed then.
func holdInterceptions(ps *pageState) (reached, release chan struct{}) {
	reached, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	ps.mu.Lock()
	ps.testApply = func(ev wipEvent) {
		if interceptionEvent(ev.method) {
			once.Do(func() { close(reached); <-release })
		}
	}
	ps.mu.Unlock()
	return reached, release
}

// runClaimHistory runs h on a fresh page and returns the violations.
func runClaimHistory(t *testing.T, h claimHistory) []string {
	r, _, c, sid0, target := attachPage(t)
	f := r.fake["One"]
	var bad []string
	failf := func(format string, a ...any) { bad = append(bad, fmt.Sprintf(format, a...)) }
	type att struct {
		sid string
		ps  *pageState
		on  bool
	}
	var atts []*att
	attach := func(sid string) {
		i := len(atts)
		call(t, c, sid, "Page.enable", nil)
		call(t, c, sid, "Network.enable", nil)
		var pats []any
		for _, p := range h.Pats[i] {
			m := map[string]any{"urlPattern": p.URL}
			if p.Type != "" {
				m["resourceType"] = p.Type
			}
			pats = append(pats, m)
		}
		call(t, c, sid, "Fetch.enable", map[string]any{"patterns": pats})
		_, s := r.pageSession(t, sid)
		atts = append(atts, &att{sid: sid, ps: s.page, on: true})
	}
	attach(sid0)
	start := c.Seq()
	sent := map[int]bool{}
	expect := map[int][]string{} // request -> reporting sessions
	answered := map[int]bool{}   // completed by the bridge or by a reporter's disable
	var pausedOrder []int
	// emit sends one WIP event and waits until every attachment queued it.
	emit := func(fn func()) {
		var before []uint64
		for _, a := range atts {
			before = append(before, a.ps.q.pushed())
		}
		fn()
		for i, a := range atts {
			pushedAtLeast(t, a.ps, before[i]+1)
		}
	}
	settle := func() {
		for _, a := range atts {
			if err := a.ps.caughtUp(testCtx(t)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, op := range h.Ops {
		switch op.Kind {
		case claimAttach:
			sid := decode[struct{ SessionID string }](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true})).SessionID
			attach(sid)
		case claimSend:
			url, typ := claimReq(op.Req)
			emit(func() {
				f.Emit("Network.requestWillBeSent", wipRequest(fmt.Sprint("rq", op.Req), "main", "0.1", typ, url, nil))
			})
			sent[op.Req] = true
			settle()
		case claimPause:
			url, typ := claimReq(op.Req)
			if !sent[op.Req] {
				typ = "Other"
			}
			var claimants []string
			held := map[string]bool{}
			for _, i := range op.Held {
				held[atts[i].sid] = true
			}
			for i, a := range atts {
				if a.on && claimMatch(h.Pats[i], url, typ) && !held[a.sid] {
					claimants = append(claimants, a.sid)
				}
			}
			expect[op.Req] = claimants
			pausedOrder = append(pausedOrder, op.Req)
			var releases []chan struct{}
			for _, i := range op.Held {
				_, release := holdInterceptions(atts[i].ps)
				releases = append(releases, release)
			}
			emit(func() {
				f.InterceptEvent("wk-One", fmt.Sprint("rq", op.Req), "request", map[string]any{"request": map[string]any{"url": url, "method": "GET", "headers": map[string]string{}}})
			})
			for _, i := range op.Held {
				call(t, c, atts[i].sid, "Fetch.disable", nil) // the withdrawal: before it handles the event
				atts[i].on = false
				for j, rs := range expect {
					if j != op.Req && slices.Contains(rs, atts[i].sid) {
						answered[j] = true // its disable continues what it reported
					}
				}
			}
			for k, i := range op.Held {
				close(releases[k])
				atts[i].ps.mu.Lock()
				atts[i].ps.testApply = nil
				atts[i].ps.mu.Unlock()
			}
			if len(claimants) == 0 {
				answered[op.Req] = true
			}
			settle()
		}
	}
	sends := func(j int) int {
		n := 0
		for _, rec := range f.ReceivedMethod("Network.interceptContinue") {
			if strings.Contains(string(rec.Params), fmt.Sprintf(`"requestId":"rq%d"`, j)) {
				n++
			}
		}
		return n
	}
	reports := func(j int) map[string]string { // session -> Fetch requestId
		url, _ := claimReq(j)
		out := map[string]string{}
		for _, a := range atts {
			for _, e := range sessionEvents(c, start, a.sid, "Fetch.requestPaused") {
				if p := decode[pausedEvent](t, e.Params); p.Request.URL == url {
					out[a.sid] = p.RequestID
				}
			}
		}
		return out
	}
	// The bridge's continues run on their own goroutines.
	waits := testCtx(t)
	for _, j := range pausedOrder {
		want := 0
		if answered[j] {
			want = 1
		}
		for sends(j) < want && waits.Err() == nil {
			time.Sleep(2 * time.Millisecond)
		}
	}
	time.Sleep(20 * time.Millisecond) // a wrong extra continue has the time to arrive
	for _, j := range pausedOrder {
		got := reports(j)
		var who []string
		for sid := range got {
			who = append(who, sid)
		}
		slices.Sort(who)
		want := slices.Clone(expect[j])
		slices.Sort(want)
		if !slices.Equal(who, want) {
			failf("request %d: reported to %v, want the claimants %v", j, who, want)
		}
		wantSends := 0
		if answered[j] {
			wantSends = 1
		}
		if n := sends(j); n != wantSends {
			failf("request %d (reported to %d attachments): %d completions before the clients answered, want %d", j, len(who), n, wantSends)
		}
	}
	// The clients answer what they were told; the first answer completes
	// the request, a later one is refused.
	for _, j := range pausedOrder {
		got := reports(j)
		for _, a := range atts {
			id, ok := got[a.sid]
			if !ok {
				continue
			}
			if _, err := c.Call(testCtx(t), a.sid, "Fetch.continueRequest", map[string]any{"requestId": id}); err != nil {
				if !strings.Contains(err.Error(), "Invalid InterceptionId.") {
					failf("request %d: answer of %s: %v", j, a.sid, err)
				}
			}
		}
	}
	for _, j := range pausedOrder {
		if n := sends(j); n != 1 {
			failf("request %d: %d completions in all, want exactly one", j, n)
		}
	}
	if held := f.HeldRequests("wk-One"); len(held) != 0 {
		failf("requests left paused: %v", held)
	}
	return bad
}

func TestFetchClaimsHistoryProperty(t *testing.T) {
	run := 0
	prop := func(h claimHistory) bool {
		run++
		ok := true
		t.Run(fmt.Sprintf("history-%d", run), func(t *testing.T) {
			if bad := runClaimHistory(t, h); len(bad) > 0 {
				ok = false
				t.Errorf("history %+v:\n%s", h, strings.Join(bad, "\n"))
			}
		})
		return ok
	}
	// A fixed configuration, so that a failure repeats.
	if err := quick.Check(prop, &quick.Config{MaxCount: 80, Rand: rand.New(rand.NewSource(20261108))}); err != nil {
		t.Error(err)
	}
}
