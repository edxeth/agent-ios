//go:build webkitgtk

package webkitgtk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// diff snapshot and diff url against real WebKitGTK pages (agent-browser
// 0.45 contract, section 6). The expected diffs are hand-written from the
// snapshot rules (e2e/fixtures/runtime/README.md) for
// diff045-a.html and diff045-b.html.

// Without --baseline, diff snapshot compares a fresh snapshot with the
// session's last one, without the refs, and becomes the next baseline.
func TestDiffSnapshotImplicitBaselineGTK(t *testing.T) {
	navigate(t, "diff045-a.html", "")
	ps := pageSession("d1", "diff-implicit", 1)
	if code, _, errOut := run(t, ps, "diff", "snapshot"); code != 1 || !strings.Contains(errOut, "no_baseline: ") {
		t.Fatalf("diff before any snapshot: exit %d, %q", code, errOut)
	}
	mustRun(t, ps, "snapshot")
	if got := mustRun(t, ps, "diff", "snapshot"); got != "No changes detected" {
		t.Fatalf("diff of an unchanged page = %q", got)
	}
	eval(t, "addButton('Added')")
	want := "--- before\n+++ after\n@@ -2,3 +2,4 @@\n   - heading \"Orders\" [level=1]\n   - button \"Save\"\n   - link \"Home\"\n+  - button \"Added\"\n\n1 additions, 0 removals, 4 unchanged"
	if got := mustRun(t, ps, "diff", "snapshot"); got != want {
		t.Fatalf("diff after adding a button =\n%s\nwant\n%s", got, want)
	}
	if got := mustRun(t, ps, "diff", "snapshot"); got != "No changes detected" {
		t.Fatalf("diff after a diff = %q; want the last diff as the baseline", got)
	}
	if code, _, errOut := run(t, ps, "diff", "snapshot", "-c"); code != 1 || !strings.Contains(errOut, "baseline_mismatch: the last snapshot used different options: --compact (last snapshot: without)") {
		t.Fatalf("diff -c after a snapshot without -c: exit %d, %q", code, errOut)
	}
}

// With --baseline, diff snapshot compares with a file of snapshot text
// (as "snapshot > file" writes it, refs ignored).
func TestDiffSnapshotBaselineFileGTK(t *testing.T) {
	navigate(t, "diff045-a.html", "")
	ps := pageSession("d1", "diff-file", 1)
	f := filepath.Join(t.TempDir(), "before.txt")
	if code, _, errOut := run(t, ps, "diff", "snapshot", "--baseline", f); code != 1 || !strings.Contains(errOut, "file_not_found: baseline "+f+" does not exist") {
		t.Fatalf("missing baseline: exit %d, %q", code, errOut)
	}
	if err := os.WriteFile(f, []byte(mustRun(t, ps, "snapshot")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A new snapshot hands out new refs; the content is the same.
	mustRun(t, ps, "snapshot")
	if got := mustRun(t, ps, "diff", "snapshot", "-b", f); got != "No changes detected" {
		t.Fatalf("diff against the saved snapshot = %q", got)
	}
	eval(t, "document.querySelector('a').remove()")
	want := "--- before\n+++ after\n@@ -1,4 +1,3 @@\n - main\n   - heading \"Orders\" [level=1]\n   - button \"Save\"\n-  - link \"Home\"\n\n0 additions, 1 removals, 3 unchanged"
	if got := mustRun(t, ps, "diff", "snapshot", "--baseline", f); got != want {
		t.Fatalf("diff against the file =\n%s\nwant\n%s", got, want)
	}
}

// diff url opens url1 and url2, compares their snapshots (and screenshots)
// and leaves the page on url2.
func TestDiffURLGTK(t *testing.T) {
	navigate(t, "diff045-a.html", "")
	ps := pageSession("d1", "diff-url", 1)
	a, b := origin+"/diff045-a.html", origin+"/diff045-b.html"
	code, out, errOut := run(t, ps, "--json", "diff", "url", a, b, "--screenshot")
	if code != 0 {
		t.Fatalf("diff url: exit %d, %q", code, errOut)
	}
	var data struct {
		Diff       map[string]any `json:"diff"`
		Snapshot   map[string]any `json:"snapshot"`
		URL1, URL2 string
		Snapshot1  string         `json:"snapshot1"`
		Snapshot2  string         `json:"snapshot2"`
		Screenshot map[string]any `json:"screenshot"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("diff url --json: %v: %s", err, out)
	}
	wantDiff := "--- before\n+++ after\n@@ -1,4 +1,4 @@\n - main\n   - heading \"Orders\" [level=1]\n   - button \"Save\"\n-  - link \"Home\"\n+  - button \"Undo\"\n"
	if data.URL1 != a || data.URL2 != b || data.Snapshot["diff"] != wantDiff || data.Snapshot["additions"] != 1.0 || data.Snapshot["removals"] != 1.0 ||
		data.Diff["identical"] != false || data.Diff["deletions"] != 1.0 || !strings.Contains(data.Snapshot1, "link \"Home\" [ref=") ||
		!strings.Contains(data.Snapshot2, "button \"Undo\" [ref=") {
		t.Fatalf("diff url data = %+v", data)
	}
	if data.Screenshot["match"] != false || data.Screenshot["differentPixels"] == 0.0 || data.Screenshot["dimensionMismatch"] != nil {
		t.Fatalf("diff url screenshot = %v; want differing pixels of same-size viewports", data.Screenshot)
	}
	if href := string(eval(t, "location.href")); href != `"`+b+`"` {
		t.Fatalf("the page is at %s after diff url, want %s", href, b)
	}
	// The same URL twice: no change, and the text form.
	got := mustRun(t, ps, "diff", "url", b, b, "--screenshot", "--wait-until", "domcontentloaded")
	want := "Snapshot diff:\nNo changes detected\n\nScreenshot diff:\nImages match (0% difference)\n  0 different / "
	if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, " total pixels") {
		t.Fatalf("diff url b b =\n%s\nwant the prefix\n%s", got, want)
	}
}
