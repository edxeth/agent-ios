package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// modulePage is a fake page for the module protocol of parity-045
// decision D5: it answers a call expression with MissingSentinel while
// the core is absent, with ModuleMissingSentinel while a module that the
// expression checks is absent, and else with an ok envelope; an install
// script marks the core and the modules it carries as installed.
type modulePage struct {
	mu        sync.Mutex
	core      bool
	mods      map[string]bool
	installs  []string // what each install carried: "core", "core+forms", "forms", ...
	calls     int
	neverMods bool // installs never take (a module that does not install)
}

func (p *modulePage) handle(c fakewebkit.Call) (any, *fakewebkit.Error) {
	var params struct{ Expression string }
	_ = json.Unmarshal(c.Params, &params)
	e := params.Expression
	p.mu.Lock()
	defer p.mu.Unlock()
	str := func(s string) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "string", "value": s}}, nil
	}
	if res, ok := fakewebkit.ArmedPromise(e); ok {
		return res, nil
	}
	if m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(e); m != nil {
		p.calls++
		if !p.core {
			return str(MissingSentinel)
		}
		if h := hasCall.FindStringSubmatch(e); h != nil {
			for _, mod := range Modules() {
				if strings.Contains(h[1], fmt.Sprintf("%q", mod)) && !p.mods[string(mod)] {
					return str(ModuleMissingSentinel)
				}
			}
		}
		return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"ok": true, "value": m[1]}}}, nil
	}
	var parts []string
	if strings.Contains(e, "agent-ios agent runtime bundle") {
		p.core = true
		parts = append(parts, "core")
	}
	for _, mod := range Modules() {
		if strings.Contains(e, "agent-ios agent runtime module "+string(mod)+",") {
			if !p.neverMods {
				p.mods[string(mod)] = true
			}
			parts = append(parts, string(mod))
		}
	}
	p.installs = append(p.installs, strings.Join(parts, "+"))
	return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
}

var hasCall = regexp.MustCompile(`window\.__iwdp\.has\(([^)]*)\)`)

func newModulePage(t *testing.T, core bool) (*modulePage, *Runtime) {
	t.Helper()
	p := &modulePage{core: core, mods: map[string]bool{}}
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Handle("Runtime.evaluate", p.handle)
	return p, New(session(t, f), nil)
}

// A call declared with With checks its modules in the page before it runs
// (decision D5), and the expression keeps the call form of design 3.1 that
// the fake pages parse, with the same function name and arguments.
func TestWithExpressionGuardsTheCallOnItsModules(t *testing.T) {
	expr, err := Expression(With("act", ModForms, ModGestures), map[string]int{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	m := fakewebkit.RuntimeCallPattern.FindStringSubmatch(expr)
	if m == nil || m[1] != "act" || m[2] != `{"n":1}` {
		t.Fatalf("expression %s does not keep the call form", expr)
	}
	if !strings.Contains(expr, `window.__iwdp.has("forms", "gestures") ? 0 : "`+ModuleMissingSentinel+`"`) {
		t.Fatalf("expression %s does not check the modules before the call", expr)
	}
	if plain, _ := Expression("act", map[string]int{"n": 1}); strings.Contains(plain, ".has(") {
		t.Fatalf("a call without modules checks modules: %s", plain)
	}
	if _, err := Expression(With("act", Module("nosuch")), nil); err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Fatalf("a call that needs an unknown module gave %v", err)
	}
}

// First use: the core is there, the module is not; the call installs only
// the module and retries once (two extra round trips). Later uses send
// the call alone (none).
func TestModuleInstalledOnFirstUseOnly(t *testing.T) {
	p, r := newModulePage(t, true)
	fn := With("overlay", ModOverlay)
	if err := r.Call(context.Background(), fn, nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.installs, ",") != "overlay" || p.calls != 2 {
		t.Fatalf("first use: installs %v, calls %d; want the overlay module alone and one retry", p.installs, p.calls)
	}
	if err := r.Call(context.Background(), fn, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Call(context.Background(), "ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.installs) != 1 || p.calls != 4 {
		t.Fatalf("later uses: installs %v, calls %d; want no install and one evaluate per call", p.installs, p.calls)
	}
}

// A call without modules never installs one, also when it installs the
// core.
func TestCoreInstallCarriesOnlyTheNeededModules(t *testing.T) {
	p, r := newModulePage(t, false)
	if err := r.Call(context.Background(), "ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Call(context.Background(), With("act", ModForms), nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.installs, ","); got != "core,forms" {
		t.Fatalf("installs %q, want the core alone for ping, then the forms module alone", got)
	}
}

// A new document (navigation, reload): the core and the modules are both
// missing; one install message carries both (two extra round trips, as
// for the core alone).
func TestNewDocumentInstallsCoreAndModulesTogether(t *testing.T) {
	p, r := newModulePage(t, false)
	if err := r.CallAsync(context.Background(), With("act", ModForms, ModGestures), nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.installs, ","); got != "core+forms+gestures" || p.calls != 2 {
		t.Fatalf("installs %q, calls %d; want one install of the core with both modules and one retry", got, p.calls)
	}
}

// A module that does not install fails the call with ErrNotInstalled
// after one install, instead of looping or running without the module.
func TestModuleThatDoesNotInstallFailsTheCall(t *testing.T) {
	p, r := newModulePage(t, true)
	p.neverMods = true
	err := r.Call(context.Background(), With("overlay", ModOverlay), nil, nil)
	if !errors.Is(err, ErrNotInstalled) || !errors.Is(err, ErrNothingRan) || !strings.Contains(err.Error(), "overlay") {
		t.Fatalf("err = %v, want ErrNotInstalled and ErrNothingRan naming the module", err)
	}
	if len(p.installs) != 1 || p.calls != 2 {
		t.Fatalf("installs %v, calls %d; want one install and one retry", p.installs, p.calls)
	}
}

// One version covers the core and every module (decision D5): each
// module's install script carries Version and installs only on the core
// of that version.
func TestModulesCarryTheRuntimeVersion(t *testing.T) {
	if len(Modules()) == 0 {
		t.Fatal("no modules")
	}
	for _, m := range Modules() {
		s, err := ModuleSource(m)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(s, `"`+Version+`"`) || strings.Contains(s, versionPlaceholder) {
			t.Fatalf("module %s does not carry the runtime Version", m)
		}
	}
	if _, err := ModuleSource("nosuch"); err == nil {
		t.Fatal("ModuleSource accepted an unknown module")
	}
}
