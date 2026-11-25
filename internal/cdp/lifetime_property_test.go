package cdp

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/edxeth/agent-ios/internal/cdp/cdptest"
	"github.com/edxeth/agent-ios/internal/core"
)

// lifeStep is one step of a random interleaving on one browser connection.
// Op selects the step, Arg its target, session or filter.
type lifeStep struct {
	Op, Arg uint8
}

// Steps of the lifetime property.
const (
	stepAutoOn      = iota // browser setAutoAttach with a filter (synchronous)
	stepAutoOff            // browser setAutoAttach autoAttach:false (synchronous)
	stepTabAutoOn          // setAutoAttach on a tab session
	stepTabAutoOff         // setAutoAttach autoAttach:false on a tab session
	stepDetach             // detachFromTarget of any session (a parent detaches its children)
	stepHeader             // Network.setExtraHTTPHeaders on a page session (attachment state)
	stepBlock              // hold or free an exclusive operation, so that commands queue
	stepLeaseEnd           // break the lease of a page (synchronous)
	stepAttach             // attachToTarget of a tab or page target
	stepDiscoverOn         // setDiscoverTargets with a filter (synchronous)
	stepDiscoverOff        // setDiscoverTargets discover:false (synchronous)
	stepEvaluate           // Runtime.evaluate on a page session
	stepPause              // let the page-list poll run
	lifeSteps
)

// maxLifeSteps bounds one interleaving. A blocker ends after blockSteps
// steps or after blockHold, whichever comes first: a synchronous step can
// wait for the blocker (a detach needs an exclusive operation), so only a
// timer independent of the step loop keeps every blocker below
// cleanupTimeout, and every detach can release its state.
const (
	maxLifeSteps = 16
	blockSteps   = 4
	blockHold    = cleanupTimeout / 5
)

// lifeFilters are the filters the steps use: nil is "no filter" (Chrome's
// default), then the empty filter, Puppeteer's browser filter, all
// targets, and pages only.
var lifeFilters = []struct {
	param any
	f     filter
}{
	{nil, chromeDefault},
	{[]any{}, filter{}},
	{[]any{map[string]any{"type": "page", "exclude": true}, map[string]any{}}, filter{{Type: "page", Exclude: true}, {}}},
	{[]any{map[string]any{}}, filter{{}}},
	{[]any{map[string]any{"type": "page"}}, filter{{Type: "page"}}},
}

func lifeParams(enable bool, filterArg uint8) map[string]any {
	p := map[string]any{"autoAttach": enable, "flatten": true}
	if lf := lifeFilters[int(filterArg)%len(lifeFilters)]; enable && lf.param != nil {
		p["filter"] = lf.param
	}
	return p
}

// Review finding 7: random interleavings of attach, detach, parent
// detach, queued commands, auto-attach and discovery reconfiguration and
// lease end keep the invariants of design 5.2 and Chrome's Target domain:
//   - no child attachment outlives its parent;
//   - no attachment outlives its lease;
//   - no desired state outlives the attachment that set it;
//   - after a final setAutoAttach every target kind its filter allows has
//     an auto-attached session (none after autoAttach:false; a narrower
//     filter detaches nothing, as in Chrome);
//   - after a final setDiscoverTargets every target kind its filter allows
//     was announced, and no target was announced twice without
//     targetDestroyed in between.
func TestAttachmentLifetimeProperty(t *testing.T) {
	n := 0
	prop := func(steps []lifeStep, finalAuto, finalDiscover uint8) bool {
		if len(steps) > maxLifeSteps {
			steps = steps[:maxLifeSteps]
		}
		n++
		return t.Run(fmt.Sprintf("case%d", n), func(t *testing.T) {
			runLifetime(t, steps, finalAuto, finalDiscover)
		})
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 25}); err != nil {
		t.Fatal(err)
	}
}

func runLifetime(t *testing.T, steps []lifeStep, finalAuto, finalDiscover uint8) {
	r := newRig(t)
	c := dial(t, r.issue(t, "s1", "One", ScopeDevice).WS)
	co := r.onlyConn(t)
	waits := testCtx(t)
	var wg sync.WaitGroup
	async := func(session, method string, params any) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Call(waits, session, method, params) // refusals are expected; the invariants decide
		}()
	}
	pick := func(arg uint8, ok func(*session) bool) string {
		co.mu.Lock()
		defer co.mu.Unlock()
		var ids []string
		for id, s := range co.sessions {
			if ok(s) {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return ""
		}
		slices.Sort(ids)
		return ids[int(arg)%len(ids)]
	}
	var blocker *core.ManagedOp
	var endBlocker func() // ends blocker once, from the loop or its timer
	blockedFor := 0
	free := func() {
		if blocker != nil {
			endBlocker()
			blocker, endBlocker, blockedFor = nil, nil, 0
		}
	}
	defer free()
	discoverFrom := 0 // client sequence number of the last discover:false
	var log []string
	for _, st := range steps {
		op, arg := int(st.Op)%lifeSteps, st.Arg
		log = append(log, fmt.Sprintf("%d/%d", op, arg))
		switch op {
		case stepAutoOn:
			call(t, c, "", "Target.setAutoAttach", lifeParams(true, arg))
		case stepAutoOff:
			call(t, c, "", "Target.setAutoAttach", lifeParams(false, 0))
		case stepTabAutoOn, stepTabAutoOff:
			if id := pick(arg, func(s *session) bool { return s.kind == kindTab }); id != "" {
				async(id, "Target.setAutoAttach", lifeParams(op == stepTabAutoOn, 0))
			}
		case stepDetach:
			if id := pick(arg, func(*session) bool { return true }); id != "" {
				async("", "Target.detachFromTarget", map[string]any{"sessionId": id})
			}
		case stepHeader:
			if id := pick(arg, func(s *session) bool { return s.kind == kindPage }); id != "" {
				async(id, "Network.setExtraHTTPHeaders", map[string]any{"headers": map[string]string{fmt.Sprintf("X-Step-%d", len(log)): "1"}})
			}
		case stepBlock:
			if blocker != nil {
				free()
				break
			}
			title := []string{"One", "Two"}[arg%2]
			for _, ps := range r.m.Status() {
				if ps.Title == title && ps.Session == "s1" {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					mo, err := r.m.BeginLeased(ctx, r.m.LookupPage(ps.Key), "s1", ps.Epoch, "test blocker", core.ClassExclusive)
					cancel()
					if err == nil {
						var once sync.Once
						end := func() { once.Do(func() { r.m.End(mo.ID) }) }
						timer := time.AfterFunc(blockHold, end)
						blocker, endBlocker = mo, func() { timer.Stop(); end() }
					}
				}
			}
		case stepLeaseEnd:
			free()
			title := []string{"One", "Two"}[arg%2]
			for _, ps := range r.m.Status() {
				if ps.Title == title {
					if err := r.m.Break(waits, ps.Key); err != nil {
						t.Fatalf("breaking the lease of %s: %v", title, err)
					}
				}
			}
		case stepAttach:
			tis := decode[struct{ TargetInfos []targetInfo }](t, call(t, c, "", "Target.getTargets", map[string]any{"filter": []any{map[string]any{}}})).TargetInfos
			async("", "Target.attachToTarget", map[string]any{"targetId": tis[int(arg)%len(tis)].TargetID, "flatten": true})
		case stepDiscoverOn:
			p := map[string]any{"discover": true}
			if lf := lifeFilters[int(arg)%len(lifeFilters)]; lf.param != nil {
				p["filter"] = lf.param
			}
			call(t, c, "", "Target.setDiscoverTargets", p)
		case stepDiscoverOff:
			call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": false})
			discoverFrom = c.Seq()
		case stepEvaluate:
			if id := pick(arg, func(s *session) bool { return s.kind == kindPage }); id != "" {
				async(id, "Runtime.evaluate", map[string]any{"expression": "1"})
			}
		case stepPause:
			time.Sleep(150 * time.Millisecond)
		}
		if blocker != nil {
			if blockedFor++; blockedFor > blockSteps {
				free()
			}
		}
	}
	free()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-waits.Done():
		t.Fatalf("steps %v: commands did not finish", log)
	}
	autoOn := finalAuto%(uint8(len(lifeFilters))+1) != uint8(len(lifeFilters))
	call(t, c, "", "Target.setAutoAttach", lifeParams(autoOn, finalAuto))
	discoverOn := finalDiscover%(uint8(len(lifeFilters))+1) != uint8(len(lifeFilters))
	if discoverOn {
		p := map[string]any{"discover": true}
		if lf := lifeFilters[int(finalDiscover)%len(lifeFilters)]; lf.param != nil {
			p["filter"] = lf.param
		}
		call(t, c, "", "Target.setDiscoverTargets", p)
	} else {
		call(t, c, "", "Target.setDiscoverTargets", map[string]any{"discover": false})
		discoverFrom = c.Seq()
	}
	checkLifetime(t, r, co, c, log, autoOn, lifeFilters[int(finalAuto)%len(lifeFilters)].f, discoverOn, lifeFilters[int(finalDiscover)%len(lifeFilters)].f, discoverFrom)
}

func checkLifetime(t *testing.T, r *rig, co *conn, c *cdptest.Client, log []string, autoOn bool, af filter, discoverOn bool, df filter, discoverFrom int) {
	t.Helper()
	epochs := map[string]uint64{} // page key -> epoch of s1's lease
	stale := func() []string {
		for k := range epochs {
			delete(epochs, k)
		}
		for _, ps := range r.m.Status() {
			if ps.Session == "s1" {
				epochs[ps.Key] = ps.Epoch
			}
		}
		co.mu.Lock()
		defer co.mu.Unlock()
		var out []string
		for id, s := range co.sessions {
			if e, ok := epochs[s.t.key]; !ok || e != s.epoch {
				out = append(out, id)
			}
		}
		return out
	}
	// Lease watchers detach asynchronously (design 5.2: nothing of a
	// session outlives its lease).
	waitUntil(t, fmt.Sprintf("steps %v: attachments of ended leases detach", log), func() bool { return len(stale()) == 0 })
	ts, err := co.rec.targets(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	co.mu.Lock()
	sessions := map[string]*session{}
	for id, s := range co.sessions {
		sessions[id] = s
	}
	children := map[string][]string{}
	for id, s := range sessions {
		children[id] = append([]string(nil), s.children...)
	}
	co.mu.Unlock()
	for id, s := range sessions {
		if s.parent == "" {
			continue
		}
		if _, ok := sessions[s.parent]; !ok || !slices.Contains(children[s.parent], id) {
			t.Fatalf("steps %v: %s session %s outlives its parent %s", log, s.kind, id, s.parent)
		}
	}
	for _, ps := range r.m.Status() {
		pg := r.m.LookupPage(ps.Key)
		for _, o := range pg.PS.State.AllOwners() {
			name, _, _ := strings.Cut(string(o), ":")
			if id, ok := strings.CutPrefix(name, "cdp-"); ok && sessions[id] == nil {
				t.Fatalf("steps %v: owner %s of a detached attachment still has state on %s (headers %v)", log, o, ps.Title, pg.PS.State.EffectiveHeaders())
			}
		}
	}
	for _, tg := range ts {
		for _, kind := range kinds {
			has := false
			for _, s := range sessions {
				has = has || (s.auto && s.parent == "" && s.t.key == tg.key && s.kind == kind)
			}
			// Narrowing the filter detaches nothing (Chrome adds only);
			// disabling detaches every auto-attached session.
			if (autoOn && af.allows(kind) && !has) || (!autoOn && has) {
				t.Fatalf("steps %v: auto-attach on=%v filter %v: %s %s auto-attached=%v", log, autoOn, af, kind, tg.title, has)
			}
		}
	}
	announced := map[string]bool{}
	for _, e := range c.Events() {
		if e.Seq <= discoverFrom {
			continue
		}
		p := decode[struct {
			TargetID   string `json:"targetId"`
			TargetInfo targetInfo
		}](t, e.Params)
		switch e.Method {
		case "Target.targetCreated":
			if announced[p.TargetInfo.TargetID] {
				t.Fatalf("steps %v: %s announced twice", log, p.TargetInfo.TargetID)
			}
			announced[p.TargetInfo.TargetID] = true
		case "Target.targetDestroyed":
			delete(announced, p.TargetID)
		}
	}
	for _, tg := range ts {
		for _, kind := range kinds {
			if discoverOn && df.allows(kind) && !announced[tg.id(kind)] {
				t.Fatalf("steps %v: discovery filter %v: %s %s was not announced", log, df, kind, tg.title)
			}
		}
	}
	if !discoverOn && len(announced) != 0 {
		t.Fatalf("steps %v: targets announced after discover:false: %v", log, announced)
	}
}

// A blocker followed by a synchronous autoAttach:false: the detach waits
// for the blocker, so the blocker must end without the step loop's
// progress, before cleanupTimeout. (Review reproduction
// TestReviewLifetimeBlockThenDisable.)
func TestLifetimeBlockThenDisable(t *testing.T) {
	runLifetime(t, []lifeStep{
		{Op: stepAutoOn, Arg: 4},
		{Op: stepHeader, Arg: 0},
		{Op: stepPause},
		{Op: stepBlock, Arg: 0},
		{Op: stepAutoOff},
	}, 5, 5)
}

// onlyConn returns the only connection of the rig's server.
func (r *rig) onlyConn(t *testing.T) *conn {
	t.Helper()
	var co *conn
	waitUntil(t, "the connection registers", func() bool {
		r.srv.mu.Lock()
		defer r.srv.mu.Unlock()
		for cc := range r.srv.conns {
			co = cc
		}
		return co != nil
	})
	return co
}
