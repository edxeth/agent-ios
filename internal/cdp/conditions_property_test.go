package cdp

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/edxeth/agent-ios/internal/devtools"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// F10 (parity-045 review-bridge.md): a generated-history property of the
// attachment's network conditions and geolocation state (throttle, the
// offline lifecycle offlinePhase, the geolocation override) against a
// reference model of what WebKit holds. A history applies and clears
// throttles, turns offline on and off, sends Network.disable, sets and
// clears the geolocation, with WebKit refusing some of these calls, pauses
// requests (some of which a second attachment's Fetch pattern claims), and
// ends with the detach. The oracle:
//   - a call reaches WebKit exactly when it changes what WebKit holds by
//     the model, and fails exactly when WebKit refused it; a refused call
//     changes nothing (throttle, offline setup) or keeps its obligation
//     (an offline rule or a geolocation restore is retried by the next
//     call that ends it, and by the detach);
//   - a paused request is failed while offline is on and the Fetch client
//     never sees it; otherwise the claiming Fetch client reports it, or the
//     bridge continues it; every paused request gets exactly one answer,
//     and none is left paused, also after the detach.

type condOpKind int

const (
	condThrottle condOpKind = iota // V: 0 clears
	condOffline                    // On
	condDisable
	condPause // JS: the second attachment's pattern claims it
	condGeoSet
	condGeoClear
	condOpKinds
)

type condOp struct {
	Kind   condOpKind
	V      int
	On     bool
	JS     bool
	Refuse bool // WebKit refuses the op's first state call
}

type condHistory struct{ Ops []condOp }

func (condHistory) Generate(r *rand.Rand, _ int) reflect.Value {
	var h condHistory
	for range 3 + r.Intn(18) {
		op := condOp{Kind: condOpKind(r.Intn(int(condOpKinds))), On: r.Intn(2) == 0, JS: r.Intn(2) == 0, Refuse: r.Intn(3) == 0}
		op.V = []int{0, 0, 50000, 80000}[r.Intn(4)]
		h.Ops = append(h.Ops, op)
	}
	return reflect.ValueOf(h)
}

// condModel is what WebKit holds by the reference model.
type condModel struct {
	throttle int  // 0: none
	offline  bool // offline on: paused requests fail
	rule     bool // the offline rule may be installed
	geo      bool // the shim is (or may still be) in the current document
}

func runCondHistory(t *testing.T, h condHistory) []string {
	r, _, c, a, target := attachPage(t)
	f := r.fake["One"]
	var bad []string
	failf := func(format string, args ...any) { bad = append(bad, fmt.Sprintf(format, args...)) }
	call(t, c, a, "Network.enable", nil)
	b := decode[struct{ SessionID string }](t, call(t, c, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true})).SessionID
	call(t, c, b, "Network.enable", nil)
	call(t, c, b, "Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "*.js"}}})
	count := func(method string) int { return len(f.ReceivedMethod(method)) }
	evals := func(substr string) int {
		n := 0
		for _, m := range f.ReceivedMethod("Runtime.evaluate") {
			var p struct{ Expression string }
			_ = json.Unmarshal(m.Params, &p)
			if strings.Contains(p.Expression, substr) {
				n++
			}
		}
		return n
	}
	refuse := func(method string, pred func(fakewebkit.Call) bool) {
		f.Handle(method, func(k fakewebkit.Call) (any, *fakewebkit.Error) {
			if pred == nil || pred(k) {
				return nil, &fakewebkit.Error{Code: -32000, Message: "injected refusal"}
			}
			return value(0), nil
		})
	}
	conditions := func(offline bool, v int) error {
		dl := float64(v)
		if v == 0 {
			dl = -1
		}
		_, err := c.Call(testCtx(t), a, "Network.emulateNetworkConditionsByRule", map[string]any{"matchedNetworkConditions": []any{
			map[string]any{"urlPattern": "", "offline": offline, "latency": 0, "downloadThroughput": dl, "uploadThroughput": -1},
		}})
		return err
	}
	var m condModel
	for i, op := range h.Ops {
		switch op.Kind {
		case condThrottle:
			before, removes := count("Network.setEmulatedConditions"), count("Network.removeInterception")
			retry := m.rule && !m.offline // offline stays off: a rule left by a refused removal goes first
			send := op.V != m.throttle
			if op.Refuse {
				refuse("Network.setEmulatedConditions", nil)
			}
			err := conditions(m.offline, op.V)
			f.Handle("Network.setEmulatedConditions", nil)
			refused := op.Refuse && send
			if n := count("Network.setEmulatedConditions") - before; n != map[bool]int{true: 1}[send] {
				failf("op %d %+v: %d setEmulatedConditions, want %v (model %+v)", i, op, n, send, m)
			}
			if (err != nil) != refused {
				failf("op %d %+v: error %v, want refused=%v", i, op, err, refused)
			}
			if n := count("Network.removeInterception") - removes; n != map[bool]int{true: 1}[retry] {
				failf("op %d %+v: %d removeInterception, want %v (model %+v)", i, op, n, retry, m)
			}
			if retry {
				m.rule = false
			}
			if !refused {
				m.throttle = op.V
			}
		case condOffline:
			adds, removes := count("Network.addInterception"), count("Network.removeInterception")
			wantAdd := op.On && !m.offline
			wantRemove := m.rule && (!op.On || !m.offline)
			if op.Refuse {
				if op.On {
					refuse("Network.addInterception", nil)
				} else {
					refuse("Network.removeInterception", nil)
				}
			}
			err := conditions(op.On, m.throttle)
			f.Handle("Network.addInterception", nil)
			f.Handle("Network.removeInterception", nil)
			refused := op.Refuse && (op.On && wantAdd || !op.On && wantRemove)
			if n := count("Network.addInterception") - adds; n != map[bool]int{true: 1}[wantAdd] {
				failf("op %d %+v: %d addInterception, want %v (model %+v)", i, op, n, wantAdd, m)
			}
			if n := count("Network.removeInterception") - removes; n != map[bool]int{true: 1}[wantRemove] {
				failf("op %d %+v: %d removeInterception, want %v (model %+v)", i, op, n, wantRemove, m)
			}
			if (err != nil) != refused {
				failf("op %d %+v: error %v, want refused=%v", i, op, err, refused)
			}
			switch {
			case op.On && !refused:
				m.offline, m.rule = true, true
			case op.On: // refused setup: nothing installed, an earlier rule removed
				m.offline, m.rule = false, false
			case !refused:
				m.offline, m.rule = false, false
			default: // refused removal: offline ends, the rule may stay
				m.offline = false
			}
		case condDisable:
			call(t, c, a, "Network.disable", nil)
			call(t, c, a, "Network.enable", nil)
			m.throttle, m.offline, m.rule = 0, false, false
		case condPause:
			id := fmt.Sprintf("p%d", i)
			url := fmt.Sprintf("https://one.example/r%d.png", i)
			if op.JS {
				url = fmt.Sprintf("https://one.example/r%d.js", i)
			}
			seq := c.Seq()
			f.InterceptEvent("wk-One", id, "request", map[string]any{"request": map[string]any{"url": url, "method": "GET"}})
			want := "Network.interceptContinue"
			switch {
			case m.offline:
				want = "Network.interceptRequestWithError"
			case op.JS:
				p := decode[pausedEvent](t, waitEvent(t, c, seq, b, "Fetch.requestPaused", func(raw json.RawMessage) bool { return strings.Contains(string(raw), url) }).Params)
				call(t, c, b, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID})
			}
			answeredWith(t, f, id, want)
			for _, s := range []string{a, b} {
				_, ses := r.pageSession(t, s)
				if err := ses.page.caughtUp(testCtx(t)); err != nil {
					t.Fatal(err)
				}
			}
			if m.offline && len(sessionEvents(c, seq, b, "Fetch.requestPaused")) > 0 {
				failf("op %d %+v: the Fetch client saw a request that offline failed", i, op)
			}
			n := 0
			for _, ans := range f.Answers() {
				if ans.RequestID == id {
					n++
				}
			}
			if n != 1 {
				failf("op %d %+v: %d answers, want one %s", i, op, n, want)
			}
		case condGeoSet:
			call(t, c, a, "Emulation.setGeolocationOverride", map[string]any{"latitude": op.V % 90, "longitude": 0, "accuracy": 1})
			m.geo = true
		case condGeoClear:
			before := evals(devtools.GeoRestore)
			if op.Refuse {
				refuse("Runtime.evaluate", func(k fakewebkit.Call) bool { return strings.Contains(string(k.Params), "__iwdpGeo") })
			}
			_, err := c.Call(testCtx(t), a, "Emulation.clearGeolocationOverride", nil)
			f.Handle("Runtime.evaluate", nil)
			refused := op.Refuse && m.geo
			if n := evals(devtools.GeoRestore) - before; n != map[bool]int{true: 1}[m.geo] {
				failf("op %d %+v: %d restores, want %v (model %+v)", i, op, n, m.geo, m)
			}
			if (err != nil) != refused {
				failf("op %d %+v: error %v, want refused=%v", i, op, err, refused)
			}
			if !refused {
				m.geo = false
			}
		}
	}
	conds, removes, restores := slices.Clone(f.ReceivedMethod("Network.setEmulatedConditions")), count("Network.removeInterception"), evals(devtools.GeoRestore)
	call(t, c, "", "Target.detachFromTarget", map[string]any{"sessionId": a})
	if got := f.ReceivedMethod("Network.setEmulatedConditions")[len(conds):]; m.throttle != 0 && (len(got) == 0 || string(got[len(got)-1].Params) != "{}") {
		failf("detach: setEmulatedConditions %v, want the throttle %d cleared", got, m.throttle)
	}
	if m.rule && count("Network.removeInterception") == removes {
		failf("detach: the offline rule was not removed")
	}
	if m.geo && evals(devtools.GeoRestore) == restores {
		failf("detach: the current document's geolocation was not restored")
	}
	f.InterceptEvent("wk-One", "after", "request", map[string]any{"request": map[string]any{"url": "https://one.example/after.png", "method": "GET"}})
	answeredWith(t, f, "after", "Network.interceptContinue")
	if held := f.HeldRequests("wk-One"); len(held) != 0 {
		failf("requests left paused: %v", held)
	}
	return bad
}

func TestConditionsHistoryProperty(t *testing.T) {
	run := 0
	prop := func(h condHistory) bool {
		run++
		ok := true
		t.Run(fmt.Sprintf("history-%d", run), func(t *testing.T) {
			if bad := runCondHistory(t, h); len(bad) > 0 {
				ok = false
				t.Errorf("history %+v:\n%s", h, strings.Join(bad, "\n"))
			}
		})
		return ok
	}
	// A fixed configuration, so that a failure repeats.
	if err := quick.Check(prop, &quick.Config{MaxCount: 200, Rand: rand.New(rand.NewSource(20261126))}); err != nil {
		t.Error(err)
	}
}
