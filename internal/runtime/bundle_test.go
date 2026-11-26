package runtime

import (
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/edxeth/agent-ios/internal/runtime/bundlegen"
)

// The installed bundle is bundle.js, generated from the readable sources
// js/*.js by go generate (package bundlegen; W3b bundle budget, design
// 3.1). It must be the output of the current sources: an edit of js/
// without go generate fails here.
func TestBundleMatchesSources(t *testing.T) {
	want, err := bundlegen.Build(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	if bundle != want {
		t.Fatalf("bundle.js (%d bytes) is not the output of js/*.js (%d bytes); run go generate ./internal/runtime", len(bundle), len(want))
	}
}

// Each module file mod-<name>.js is the output of its sources
// js/mod-<name>/*.js, and the generated module files are exactly the
// module directories (parity-045 decision D5): an edit of a module without
// go generate, and the file of a removed module, fail here.
func TestModulesMatchSources(t *testing.T) {
	fsys := os.DirFS(".")
	names, err := bundlegen.Modules(fsys)
	if err != nil || len(names) == 0 {
		t.Fatalf("module directories %v: %v", names, err)
	}
	want := map[string]bool{}
	for _, n := range names {
		out, err := bundlegen.BuildModule(fsys, n)
		if err != nil {
			t.Fatal(err)
		}
		f := "mod-" + n + ".js"
		want[f] = true
		got, err := os.ReadFile(f)
		if err != nil || string(got) != out {
			t.Errorf("%s is not the output of js/mod-%s/*.js (%v); run go generate ./internal/runtime", f, n, err)
		}
	}
	files, err := fs.Glob(fsys, "mod-*.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !want[f] {
			t.Errorf("%s has no sources in js/; run go generate ./internal/runtime", f)
		}
	}
}

// The Module constants name exactly the generated modules, so a call site
// cannot declare a module that does not exist (With fails it) and no
// module lacks its constant.
func TestModuleConstantsMatchGenerated(t *testing.T) {
	consts := []Module{ModForms, ModGestures, ModOverlay}
	got := Modules()
	if len(got) != len(consts) {
		t.Fatalf("generated modules %v, constants %v", got, consts)
	}
	for _, m := range consts {
		if _, err := ModuleSource(m); err != nil {
			t.Errorf("constant %q: %v", m, err)
		}
	}
}

// The readable sources are ES2017 themselves (decision D4). The bundle is
// built for an ES2017 target, which would lower newer syntax silently, so
// the check of the installed bundle (TestRuntimeBundleIsES2017Syntax)
// alone would not see it.
func TestSourcesAreES2017Syntax(t *testing.T) {
	names, err := fs.Glob(os.DirFS("."), "js/*.js")
	if err != nil || len(names) == 0 {
		t.Fatalf("listing js/*.js: %v %v", names, err)
	}
	mods, err := fs.Glob(os.DirFS("."), "js/mod-*/*.js")
	if err != nil || len(mods) == 0 {
		t.Fatalf("listing js/mod-*/*.js: %v %v", mods, err)
	}
	names = append(names, mods...)
	sort.Strings(names)
	for _, n := range names {
		data, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkES2017(string(data)); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
}

// Budget with headroom: the installed core stays at or below 80% of the
// 64 KiB budget of design 3.1, so later runtime changes still fit. W3b
// decision 4 reserved 20% for the runtime files of W3b and W5; those landed,
// and the agent-browser 0.45 parity update first moved the reserve to 10%
// (parity-045 decision D4), then split the runtime into the core and
// feature modules installed on demand (parity-045 decision D5), which
// restored the 20% reserve for the core. The hard limits stay:
// TestBundleWithinSourceBudget, TestModulesWithinSourceBudget and the
// per-transport install and bootstrap test.
func TestBundleLeavesHeadroom(t *testing.T) {
	const headroomLimit = MaxSourceBytes * 80 / 100
	if n := len(Source()); n > headroomLimit {
		t.Fatalf("installed core is %d bytes; keep it at or below %d (80%% of %d) for later runtime changes", n, headroomLimit, MaxSourceBytes)
	}
	t.Logf("installed bundle: %d bytes of %d (%d%%)", len(Source()), MaxSourceBytes, len(Source())*100/MaxSourceBytes)
}

// Build fails on JavaScript that does not parse and keeps strings, the
// version placeholder and property names (hand-written case).
func TestBuildKeepsNamesAndRejectsBadSource(t *testing.T) {
	ok := fstest.MapFS{"js/10-a.js": {Data: []byte("(function (I) { var longName = \"__IWDP_VERSION__\"; window.__iwdpBuilding = { v: longName }; })();\n")}}
	out, err := bundlegen.Build(ok)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"__IWDP_VERSION__", "__iwdpBuilding", bundlegen.Header} {
		if !strings.Contains(out, s) {
			t.Errorf("bundle %q lacks %q", out, s)
		}
	}
	if strings.Contains(out, "longName") {
		t.Errorf("bundle %q kept a local identifier", out)
	}
	if _, err := bundlegen.Build(fstest.MapFS{"js/10-a.js": {Data: []byte("const =")}}); err == nil {
		t.Fatal("Build accepted JavaScript that does not parse")
	}
}
