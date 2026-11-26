package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/refs"
)

// Tests of diff snapshot (agent-browser 0.45 contract, section 6 of
// agent-browser-045-contract.md). Expected texts come from that contract
// and from agent-browser's diff.rs and actions.rs at v0.45.0.

// treeReply is a snapshot value with one node per (role, name, ref).
func treeReply(next int, nodes ...[3]string) map[string]any {
	var tree, rs []any
	for _, n := range nodes {
		tree = append(tree, map[string]any{"role": n[0], "name": n[1], "ref": n[2]})
		rs = append(rs, map[string]any{"ref": n[2], "role": n[0], "name": n[1], "unique": true})
	}
	return okEnv(map[string]any{"tree": tree, "refs": rs, "nextRef": next, "doc": "d", "url": "http://x.test/", "title": "X"})
}

func (p *fakePage) count(fn string) int {
	n := 0
	for _, f := range p.fns() {
		if f == fn {
			n++
		}
	}
	return n
}

// Without --baseline, diff snapshot compares a fresh snapshot with the last
// snapshot of this session on the tab, and becomes the next baseline;
// generated refs are not compared.
func TestDiffSnapshotImplicitBaseline(t *testing.T) {
	h := newHarness(t)
	code, _, errOut := h.run("diff", "snapshot")
	if want := "no_baseline: no earlier snapshot on this tab; run 'snapshot' first or pass --baseline <file>"; code != ExitError || !strings.Contains(errOut, want) {
		t.Fatalf("diff without a snapshot: exit %d, %q; want %q", code, errOut, want)
	}
	if n := h.page.count("snapshot"); n != 0 {
		t.Fatalf("diff without a baseline took %d snapshots, want none", n)
	}
	h.page.set("snapshot", treeReply(3, [3]string{"button", "Save", "e1"}, [3]string{"link", "Home", "e2"}))
	h.mustRun(t, "snapshot")
	// The same content with other refs (a new document) is no change.
	h.page.set("snapshot", treeReply(9, [3]string{"button", "Save", "e7"}, [3]string{"link", "Home", "e8"}))
	if got := h.mustRun(t, "diff", "snapshot"); got != "No changes detected" {
		t.Fatalf("diff of the same content with new refs = %q", got)
	}
	h.page.set("snapshot", treeReply(11, [3]string{"button", "Save", "e7"}, [3]string{"button", "Undo", "e10"}))
	want := "--- before\n+++ after\n@@ -1,2 +1,2 @@\n - button \"Save\"\n-- link \"Home\"\n+- button \"Undo\"\n\n1 additions, 1 removals, 1 unchanged"
	if got := h.mustRun(t, "diff", "snapshot"); got != want {
		t.Fatalf("diff snapshot =\n%s\nwant\n%s", got, want)
	}
	// That diff is the next baseline.
	code, out, errOut := h.run("--json", "diff", "snapshot")
	var data map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &data) != nil {
		t.Fatalf("diff --json: exit %d, %q, %q", code, out, errOut)
	}
	if data["changed"] != false || data["additions"] != 0.0 || data["removals"] != 0.0 || data["unchanged"] != 2.0 || data["diff"] != "" {
		t.Fatalf("diff after a diff = %v; want no change against the last diff's snapshot", data)
	}
}

// A bare diff snapshot takes its snapshot with the last snapshot's options;
// an option that differs is baseline_mismatch, with no snapshot taken.
func TestDiffSnapshotInheritsAndChecksOptions(t *testing.T) {
	h := newHarness(t)
	h.page.set("snapshot", treeReply(3, [3]string{"heading", "T", "e1"}))
	h.mustRun(t, "snapshot", "-s", "h1", "-i")
	h.mustRun(t, "diff", "snapshot")
	if got := h.page.last("snapshot"); got["scope"] == nil || got["scope"].(map[string]any)["selector"] != "h1" {
		t.Fatalf("bare diff snapshot scope = %v; want the last snapshot's selector h1", got)
	}
	last, ok := h.ps.Refs.LastSnapshot("")
	if !ok || last.Options != (refs.SnapshotOptions{Selector: "h1", Interactive: true}) {
		t.Fatalf("last snapshot after the diff = %+v, %v; want the inherited options", last, ok)
	}
	// Naming the same options is fine.
	h.mustRun(t, "diff", "snapshot", "-s", "h1", "--interactive")
	before := h.page.count("snapshot")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-s", "p", "-c"}, `baseline_mismatch: the last snapshot used different options: --selector "p" (last snapshot: "h1"), --compact (last snapshot: without); run 'snapshot' with the same options or pass --baseline <file>`},
		{[]string{"-d", "2"}, `baseline_mismatch: the last snapshot used different options: --depth 2 (last snapshot: no depth); run 'snapshot' with the same options or pass --baseline <file>`},
		{[]string{"-u"}, `baseline_mismatch: the last snapshot used different options: --urls (last snapshot: without); run`},
	} {
		code, _, errOut := h.run(append([]string{"diff", "snapshot"}, c.args...)...)
		if code != ExitError || !strings.Contains(errOut, c.want) {
			t.Errorf("diff snapshot %v: exit %d, %q; want %q", c.args, code, errOut, c.want)
		}
	}
	if n := h.page.count("snapshot"); n != before {
		t.Fatalf("refused diffs took %d snapshots", n-before)
	}
	// A depth that differs from the last snapshot's depth.
	h.mustRun(t, "snapshot", "-d", "1")
	if code, _, errOut := h.run("diff", "snapshot", "-d", "3"); code != ExitError || !strings.Contains(errOut, "--depth 3 (last snapshot: 1)") {
		t.Fatalf("diff -d 3 after snapshot -d 1: exit %d, %q", code, errOut)
	}
}

// A last snapshot of another frame is baseline_mismatch.
func TestDiffSnapshotFrameMismatch(t *testing.T) {
	h := newHarness(t)
	h.ps.Refs.RememberSnapshot("", refs.LastSnapshot{Text: "- button \"A\"", FrameID: "frame-1"})
	code, _, errOut := h.run("diff", "snapshot")
	want := "baseline_mismatch: the last snapshot was taken in a different frame; select that frame or run 'snapshot' again, or pass --baseline <file>"
	if code != ExitError || !strings.Contains(errOut, want) {
		t.Fatalf("diff after a snapshot of another frame: exit %d, %q; want %q", code, errOut, want)
	}
}

// With --baseline the baseline is a file of snapshot text (one trailing
// newline stripped, refs not compared) and the snapshot uses exactly the
// given flags; a missing file is file_not_found.
func TestDiffSnapshotBaselineFile(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "none.txt")
	code, _, errOut := h.run("diff", "snapshot", "--baseline", missing)
	if want := "file_not_found: baseline " + missing + " does not exist"; code != ExitError || !strings.Contains(errOut, want) {
		t.Fatalf("missing baseline: exit %d, %q; want %q", code, errOut, want)
	}
	h.page.set("snapshot", treeReply(3, [3]string{"button", "Save", "e1"}))
	h.mustRun(t, "snapshot", "-c")
	f := filepath.Join(dir, "before.txt")
	if err := os.WriteFile(f, []byte("- button \"Save\" [ref=e40]\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := h.mustRun(t, "diff", "snapshot", "-b", f); got != "No changes detected" {
		t.Fatalf("diff against the same content in a file = %q", got)
	}
	if last, _ := h.ps.Refs.LastSnapshot(""); last.Options.Compact {
		t.Fatalf("diff --baseline inherited --compact from the last snapshot: %+v", last.Options)
	}
	if err := os.WriteFile(f, []byte("- button \"Save\" [ref=e40]\n- link \"Gone\" [ref=e41]\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Only one trailing newline is stripped: the empty last line counts.
	want := "--- before\n+++ after\n@@ -1,3 +1 @@\n - button \"Save\"\n-- link \"Gone\"\n-\n\n0 additions, 2 removals, 1 unchanged"
	if got := h.mustRun(t, "diff", "snapshot", "--baseline", f); got != want {
		t.Fatalf("diff against a file =\n%s\nwant\n%s", got, want)
	}
}

// snapshotComparisonText drops only the generated ref annotations
// (agent-browser diff.rs content_comparison_ignores_only_generated_ref_annotations
// and snapshot_comparison_text).
func TestSnapshotComparisonText(t *testing.T) {
	for _, c := range [][2]string{
		{`- button "Save [ref=e3]" [ref=e1]`, `- button "Save [ref=e3]"`},
		{`- checkbox "Agree" [checked=false, ref=e2]`, `- checkbox "Agree" [checked=false]`},
		{`- heading "T" [ref=e3, level=1]`, `- heading "T" [level=1]`},
		{`- button "a\"b [ref=e1]" [ref=e2]`, `- button "a\"b [ref=e1]"`},
		{`- button "x" [ref=ex]`, `- button "x" [ref=ex]`},
		{`- button "x" [ref=e]`, `- button "x" [ref=e]`},
		{`- text: a[ref=e1]`, `- text: a[ref=e1]`},
		{"- button \"x\" [ref=e1\n- link [ref=e2]", "- button \"x\" [ref=e1\n- link"},
		{`- list: [ref=e5]`, `- list:`},
	} {
		if got := snapshotComparisonText(c[0]); got != c[1] {
			t.Errorf("snapshotComparisonText(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

// network har stop without an active recording fails with
// no_har_recording and writes no file (agent-browser 0.45, contract
// section 6): before any capture, and after a stop.
func TestHARStopWithoutRecording(t *testing.T) {
	h := newDTHarness(t)
	const want = "no_har_recording: no HAR recording is active; run 'network har start' first. A daemon restart (close or idle shutdown) discards an active recording"
	stop := func(what string) {
		t.Helper()
		code, _, errOut := h.run("network", "har", "stop", "x.har")
		if code != ExitError || !strings.Contains(errOut, want) {
			t.Fatalf("har stop %s: exit %d, %q; want %q", what, code, errOut, want)
		}
		if _, err := os.Stat(filepath.Join(h.dir, "x.har")); !os.IsNotExist(err) {
			t.Fatalf("har stop %s wrote a file: %v", what, err)
		}
	}
	stop("before any capture")
	h.mustRun(t, "network", "har", "start", "--content", "none")
	h.mustRun(t, "network", "har", "stop", "first.har")
	stop("after the recording stopped")
}

// tab keep exists and fails with the reason (no WIP focus emulation or
// lifecycle freeze), touching no page (pageEnv{nil}); a bad mode is a
// usage error.
func TestTabKeepUnsupported(t *testing.T) {
	newEnv := func(context.Context, *Invocation) (Env, error) { return pageEnv{nil}, nil }
	for _, mode := range []string{"live", "throttled", "frozen"} {
		var out, errb bytes.Buffer
		code := RunCLIStarted(context.Background(), []string{"tab", "keep", mode, "t2"}, &out, &errb, nil, newEnv)
		if code != ExitError || !strings.Contains(errb.String(), "unsupported: tab keep "+mode+": WebKit's inspector protocol has no focus emulation") {
			t.Errorf("tab keep %s: exit %d, %q", mode, code, errb.String())
		}
	}
	for _, args := range [][]string{{"tab", "keep", "bogus"}, {"tab", "keep"}} {
		var out, errb bytes.Buffer
		if code := RunCLIStarted(context.Background(), args, &out, &errb, nil, newEnv); code != ExitUsage || strings.Contains(errb.String(), "unsupported") {
			t.Errorf("%v: exit %d, %q; want a usage error", args, code, errb.String())
		}
	}
}

// diff url --full without --screenshot is a usage error before any page
// work: pageEnv{nil} has no page (agent-browser
// test_diff_url_full_requires_screenshot).
func TestDiffURLFullNeedsScreenshot(t *testing.T) {
	var out, errb bytes.Buffer
	code := RunCLIStarted(context.Background(), []string{"diff", "url", "http://a.test/", "http://b.test/", "--full"}, &out, &errb, nil,
		func(context.Context, *Invocation) (Env, error) { return pageEnv{nil}, nil })
	if code != ExitUsage || !strings.Contains(errb.String(), "--full only applies to the screenshot diff; add --screenshot") {
		t.Fatalf("diff url --full: exit %d, %q", code, errb.String())
	}
}

func testPNG(t *testing.T, w, h int, fill func(x, y int) color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, fill(x, y))
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The screenshot part of diff url: agent-browser's JSON members and text;
// images of different sizes count every pixel as different.
func TestDiffURLScreenshotCompare(t *testing.T) {
	white := func(int, int) color.NRGBA { return color.NRGBA{255, 255, 255, 255} }
	a := testPNG(t, 4, 2, white)
	half := testPNG(t, 4, 2, func(x, y int) color.NRGBA {
		if x < 2 {
			return color.NRGBA{0, 0, 0, 255}
		}
		return white(x, y)
	})
	for _, c := range []struct {
		name     string
		b        []byte
		text     string
		match    bool
		pct      float64
		diff     int
		total    int
		dimsNull bool
	}{
		{"same", a, "Images match (0% difference)\n  0 different / 8 total pixels", true, 0, 0, 8, true},
		{"half", half, "50.00% pixels differ\n  4 different / 8 total pixels", false, 50, 4, 8, true},
		{"size", testPNG(t, 2, 2, white), "Images have different dimensions (4x2 and 2x2)\n  8 different / 8 total pixels", false, 100, 8, 8, false},
	} {
		got, err := comparePNGs(a, c.b)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if txt := got.text(); txt != c.text {
			t.Errorf("%s: text %q, want %q", c.name, txt, c.text)
		}
		d := got.data()
		if d["match"] != c.match || d["mismatchPercentage"] != c.pct || d["differentPixels"] != c.diff || d["totalPixels"] != c.total ||
			(d["dimensionMismatch"] == nil) != c.dimsNull || d["diffPath"] != nil {
			t.Errorf("%s: data %v", c.name, d)
		}
	}
}
