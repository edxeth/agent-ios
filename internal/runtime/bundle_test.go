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

// The readable sources are ES2017 themselves (decision D4). The bundle is
// built for an ES2017 target, which would lower newer syntax silently, so
// the check of the installed bundle (TestRuntimeBundleIsES2017Syntax)
// alone would not see it.
func TestSourcesAreES2017Syntax(t *testing.T) {
	names, err := fs.Glob(os.DirFS("."), "js/*.js")
	if err != nil || len(names) == 0 {
		t.Fatalf("listing js/*.js: %v %v", names, err)
	}
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

// Budget with headroom (W3b decision 4): the installed bundle stays at or
// below 80% of the 64 KiB budget of design 3.1, so the later runtime files
// of W3b and W5 fit.
func TestBundleLeavesHeadroom(t *testing.T) {
	const headroomLimit = MaxSourceBytes * 80 / 100
	if n := len(Source()); n > headroomLimit {
		t.Fatalf("installed bundle is %d bytes; keep it at or below %d (80%% of %d) for the later runtime files", n, headroomLimit, MaxSourceBytes)
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
