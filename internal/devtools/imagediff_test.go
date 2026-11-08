package devtools

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func diffFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "diff", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Design 4.2 "diff screenshot": a host pixel diff. The fixtures are
// hand-made 4x3 PNGs (testdata/diff/generate.go lists every pixel); the
// expected counts come from those lists:
//   - two_px: (0,0) black and (3,2) red on white; both differ by 255 in a
//     channel, so they count at every threshold below 1.
//   - faint: (1,1) is 240,240,240 (15/255 = 0.059 from white) and (2,1)
//     is 200,255,255 (55/255 = 0.216).
//   - transparent: (0,2) is white with alpha 0 (alpha differs by 255).
func TestDiffPNGHandMadeFixtures(t *testing.T) {
	base := diffFixture(t, "base.png")
	cases := []struct {
		other     string
		threshold float64
		want      int
	}{
		{"same.png", 0, 0},
		{"two_px.png", 0.1, 2},
		{"two_px.png", 0.99, 2},
		{"faint.png", 0, 2},
		{"faint.png", 0.05, 2},
		{"faint.png", 0.1, 1},
		{"faint.png", 0.25, 0},
		{"transparent.png", 0.1, 1},
	}
	for _, c := range cases {
		res, err := DiffPNG(base, diffFixture(t, c.other), c.threshold)
		if err != nil {
			t.Fatalf("%s at %v: %v", c.other, c.threshold, err)
		}
		if res.Different != c.want || res.Total != 12 || res.Width != 4 || res.Height != 3 {
			t.Errorf("%s at threshold %v: %d of %d (%dx%d) differ, want %d of 12 (4x3)", c.other, c.threshold, res.Different, res.Total, res.Width, res.Height, c.want)
		}
		if want := float64(c.want) / 12; res.Ratio != want {
			t.Errorf("%s at threshold %v: ratio %v, want %v", c.other, c.threshold, res.Ratio, want)
		}
	}
	// The diff is symmetric: the baseline and the screenshot can swap.
	if res, err := DiffPNG(diffFixture(t, "faint.png"), base, 0.1); err != nil || res.Different != 1 {
		t.Errorf("faint against base: %+v, %v; want 1 different pixel", res, err)
	}
}

func TestDiffPNGErrors(t *testing.T) {
	base := diffFixture(t, "base.png")
	if _, err := DiffPNG(base, diffFixture(t, "small.png"), 0.1); err == nil || !strings.Contains(err.Error(), "4x3") || !strings.Contains(err.Error(), "3x3") {
		t.Errorf("size mismatch: %v; want an error naming 4x3 and 3x3", err)
	}
	if _, err := DiffPNG([]byte("not a png"), base, 0.1); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Errorf("bad baseline: %v; want an error naming the baseline", err)
	}
	if _, err := DiffPNG(base, []byte("not a png"), 0.1); err == nil || !strings.Contains(err.Error(), "screenshot") {
		t.Errorf("bad screenshot: %v; want an error naming the screenshot", err)
	}
	for _, th := range []float64{-0.1, 1, 1.5} {
		if _, err := DiffPNG(base, base, th); err == nil {
			t.Errorf("threshold %v was accepted; want 0 <= threshold < 1", th)
		}
	}
}

// A threshold that is not a finite number is refused: NaN fails every
// comparison, so it used to count no pixel and report a match (W4 storage
// review, finding imagediff.go:29).
func TestDiffPNGRejectsANonFiniteThreshold(t *testing.T) {
	base, other := diffFixture(t, "base.png"), diffFixture(t, "two_px.png")
	for _, th := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if d, err := DiffPNG(base, other, th); err == nil {
			t.Errorf("threshold %v was accepted: %d of %d pixels differ; want an error", th, d.Different, d.Total)
		}
	}
}
