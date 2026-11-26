//go:build webkitgtk

package webkitgtk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/edxeth/agent-ios/internal/heapsnap"
)

// heapProbe allocates n instances of the class HeapProbe045 in the page and
// keeps them in window.__heap045.
const heapProbe = `(() => { class HeapProbe045 { constructor(i) { this.i = i; } }
window.__heap045 = []; for (let i = 0; i < 60000; i++) window.__heap045.push(new HeapProbe045(i));
return window.__heap045.length; })()`

// heap snapshot on a real page (WP-HEAP): a page that holds 60000
// instances of a named class gets that class at the top of the summary by
// count, with at least 60000 nodes; the written file is the raw
// JavaScriptCore JSON with mode 0600. After the page drops them, heap gc
// runs and heap diff of the two files reports the class's 60000 fewer
// nodes. AGENT_IOS_HEAP_FIXTURE_OUT, when set, receives a copy of the
// first snapshot (the source of internal/heapsnap/testdata).
func TestHeapSnapshotSummaryAndDiff(t *testing.T) {
	navigate(t, "forms.html?heap045", "")
	if got := string(eval(t, heapProbe)); got != "60000" {
		t.Fatalf("setup: allocated %s instances, want 60000", got)
	}
	ps := pageSession("heap-daemon", "heap", 1)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")

	var sum struct {
		Path       string           `json:"path"`
		Bytes      int              `json:"bytes"`
		Version    int              `json:"version"`
		Nodes      int64            `json:"nodes"`
		TopByCount []heapsnap.Class `json:"topByCount"`
		TopBySize  []heapsnap.Class `json:"topBySize"`
	}
	out := mustRun(t, ps, "--json", "heap", "snapshot", a, "--top", "5")
	if err := json.Unmarshal([]byte(out), &sum); err != nil {
		t.Fatalf("heap snapshot --json: %v: %s", err, out)
	}
	t.Logf("snapshot: version %d, %d bytes, %d nodes, top by count %+v, top by size %+v", sum.Version, sum.Bytes, sum.Nodes, sum.TopByCount, sum.TopBySize)
	if len(sum.TopByCount) != 5 || len(sum.TopBySize) != 5 {
		t.Fatalf("--top 5 gave %d and %d classes", len(sum.TopByCount), len(sum.TopBySize))
	}
	if top := sum.TopByCount[0]; top.Name != "HeapProbe045" || top.Count < 60000 {
		t.Fatalf("top class by count is %+v, want HeapProbe045 with at least 60000 nodes", top)
	}
	if sum.Path != a {
		t.Fatalf("path %q, want %q", sum.Path, a)
	}
	st, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 || st.Size() != int64(sum.Bytes) {
		t.Fatalf("snapshot file: mode %v, size %d; want 0600 and %d bytes", st.Mode().Perm(), st.Size(), sum.Bytes)
	}
	data, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := heapsnap.Parse(data); err != nil || s.Nodes != sum.Nodes {
		t.Fatalf("the written file: %v, %d nodes; want the summary's %d", err, s.Nodes, sum.Nodes)
	}
	if out := os.Getenv("AGENT_IOS_HEAP_FIXTURE_OUT"); out != "" {
		if err := os.WriteFile(out, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	eval(t, "window.__heap045 = null; 0")
	if out := mustRun(t, ps, "heap", "gc"); out != "Garbage collection done." {
		t.Fatalf("heap gc: %q", out)
	}
	mustRun(t, ps, "heap", "snapshot", b)
	var diff struct {
		Classes []heapsnap.ClassDelta `json:"classes"`
	}
	out = mustRun(t, ps, "--json", "heap", "diff", a, b)
	if err := json.Unmarshal([]byte(out), &diff); err != nil {
		t.Fatalf("heap diff --json: %v: %s", err, out)
	}
	for _, c := range diff.Classes {
		if c.Name == "HeapProbe045" {
			if c.CountDelta != -60000 || c.CountB != 0 {
				t.Fatalf("HeapProbe045 after the page dropped its instances: %+v; want 60000 fewer, none left", c)
			}
			return
		}
	}
	t.Fatalf("heap diff lists no change of HeapProbe045: %s", out)
}
