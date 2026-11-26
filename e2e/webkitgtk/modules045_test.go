//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	iwdpruntime "github.com/edxeth/agent-ios/internal/runtime"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// Runtime modules (parity-045 decision D5): the core is installed as
// before; a feature module is installed by Runtime.evaluate only when a
// command that needs it runs, in the document that command works in, and
// only on a core of the same version.

// hasModule reports whether the top document's runtime has module m.
func hasModule(t *testing.T, m iwdpruntime.Module) bool {
	t.Helper()
	return string(eval(t, fmt.Sprintf("!!(window.__iwdp && window.__iwdp.has(%q))", m))) == "true"
}

// D5: no module before a command needs it; fill installs the forms module
// and no other; a reload starts without modules, and the next fill installs
// the core and the module again.
func TestModules045InstalledOnFirstUseAndAfterReload(t *testing.T) {
	ps := loadForms(t)
	mustAct(t, ps, "click", "#name")
	if string(eval(t, "typeof window.__iwdp")) != `"object"` {
		t.Fatal("click did not install the core")
	}
	for _, m := range iwdpruntime.Modules() {
		if hasModule(t, m) {
			t.Fatalf("module %s is installed before a command needed it", m)
		}
	}
	mustAct(t, ps, "fill", "#name", "abc")
	if got := pageString(t, "document.getElementById('name').value"); got != "abc" {
		t.Fatalf("fill #name: value %q", got)
	}
	if !hasModule(t, iwdpruntime.ModForms) || hasModule(t, iwdpruntime.ModGestures) || hasModule(t, iwdpruntime.ModOverlay) {
		t.Fatal("fill did not install exactly the forms module")
	}
	loadForms(t) // a reload: a new document without the runtime
	if string(eval(t, "typeof window.__iwdp")) != `"undefined"` {
		t.Fatal("the reloaded document still has the runtime")
	}
	ps, _ = actionSession(t, hooks{})
	mustAct(t, ps, "fill", "#name", "def")
	if got := pageString(t, "document.getElementById('name').value"); got != "def" || !hasModule(t, iwdpruntime.ModForms) {
		t.Fatalf("fill after the reload: value %q, forms module %v", got, hasModule(t, iwdpruntime.ModForms))
	}
}

// D5: a module's state follows the core's reset hooks: after a new lease
// epoch the gestures module is still there (no reinstall) and its mouse
// state starts over, as the core's own state does.
func TestModules045StateFollowsEpochReset(t *testing.T) {
	ps := loadForms(t)
	mustAct(t, ps, "mouse", "move", "30", "40")
	if out := mustAct(t, ps, "mouse", "down"); !strings.Contains(out, `"x":30`) || !strings.Contains(out, `"y":40`) {
		t.Fatalf("mouse down after the move: %q", out)
	}
	mustAct(t, ps, "mouse", "up")
	session, epoch := ps.Sched.Lease()
	ps.Sched.SetLease(session, epoch+1)
	if !hasModule(t, iwdpruntime.ModGestures) {
		t.Fatal("the gestures module is gone after the epoch change")
	}
	out := mustAct(t, ps, "mouse", "down")
	if !strings.Contains(out, `"x":0`) || !strings.Contains(out, `"y":0`) {
		t.Fatalf("mouse down in the new epoch: %q; want the pointer reset to 0, 0", out)
	}
	mustAct(t, ps, "mouse", "up")
}

// D5: a module installs only on the core of its own version: on another
// version's core, and without a core, it changes nothing and answers the
// missing sentinel; on its own core it installs once.
func TestModules045RefusedOnAnotherVersion(t *testing.T) {
	ps := loadForms(t)
	mustAct(t, ps, "click", "#name")
	src, err := iwdpruntime.ModuleSource(iwdpruntime.ModGestures)
	if err != nil {
		t.Fatal(err)
	}
	eval(t, `window.__realIwdp = window.__iwdp; Object.defineProperty(window, "__iwdp", { configurable: true, writable: true, value:
		{ v: "000000000000", has: function () { return false; }, _: { mod: function () { window.__marked = true; }, A: {} } } }); true`)
	got := string(eval(t, src))
	marked := string(eval(t, "!!window.__marked"))
	eval(t, `Object.defineProperty(window, "__iwdp", { configurable: true, writable: true, value: window.__realIwdp }); delete window.__realIwdp; true`)
	if got != `"`+iwdpruntime.MissingSentinel+`"` || marked != "false" {
		t.Fatalf("module on another version's core answered %s, marked %s; want the missing sentinel and no change", got, marked)
	}
	if hasModule(t, iwdpruntime.ModGestures) {
		t.Fatal("the refused install marked the module on the real core")
	}
	if got := string(eval(t, src)); got != "true" || !hasModule(t, iwdpruntime.ModGestures) {
		t.Fatalf("module on its own core answered %s", got)
	}
}

// countCaller counts the module installs sent through it.
type countCaller struct {
	webkit.Caller
	installs atomic.Int32
}

func (c *countCaller) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if p, ok := params.(map[string]any); ok {
		if e, _ := p["expression"].(string); strings.Contains(e, "agent-ios agent runtime module gestures") {
			c.installs.Add(1)
		}
	}
	return c.Caller.Send(ctx, method, params)
}

// D5: concurrent first uses of a module all succeed, and a second install
// of an installed module changes nothing (its dispatchers stay the same
// functions); later calls send no install.
func TestModules045ConcurrentFirstUse(t *testing.T) {
	loadForms(t)
	cc := &countCaller{Caller: sess}
	rt := iwdpruntime.New(cc, sess.Generation)
	fn := iwdpruntime.With("ping", iwdpruntime.ModGestures)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() { errs <- rt.Call(ctx, fn, nil, nil) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := cc.installs.Load(); n < 1 || n > 4 {
		t.Fatalf("%d installs for 4 concurrent first uses", n)
	}
	eval(t, "window.__drag = window.__iwdp._.A.drag; true")
	src, err := iwdpruntime.ModuleSource(iwdpruntime.ModGestures)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(eval(t, src)); got != "true" || string(eval(t, "window.__drag === window.__iwdp._.A.drag")) != "true" {
		t.Fatalf("a second install answered %s or replaced the dispatchers", got)
	}
	before := cc.installs.Load()
	if err := rt.Call(ctx, fn, nil, nil); err != nil || cc.installs.Load() != before {
		t.Fatalf("a later call: %v, %d more installs", err, cc.installs.Load()-before)
	}
}

// D5: a command in the selected frame installs its module in the frame's
// context, not in the top document.
func TestFrames045ModuleInstalledInTheSelectedFrame(t *testing.T) {
	e := newFrames045(t)
	e.must(agentA, "frame", ".xo")
	if got := e.must(agentA, "eval", "!!(window.__iwdp && window.__iwdp.has('forms'))"); got != "false" {
		t.Fatalf("the frame has the forms module before a form command: %q", got)
	}
	e.must(agentA, "check", "#agree")
	if got := e.must(agentA, "eval", "window.__iwdp.has('forms')"); got != "true" {
		t.Fatalf("the frame lacks the forms module after check: %q", got)
	}
	if got := e.js(agentA, "!!(window.__iwdp && window.__iwdp.has('forms'))"); got != "false" {
		t.Fatalf("check in the frame installed the forms module in the top document: %s", got)
	}
}
