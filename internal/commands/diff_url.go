package commands

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"io"
	"strings"
	"time"

	"github.com/edxeth/agent-ios/internal/devtools"
)

// diff url (agent-browser 0.45 "diff url"): open url1 in the page, take a
// snapshot (and with --screenshot a screenshot), then the same for url2,
// and compare them. It reuses open, snapshot and the pixel diff of diff
// screenshot. The page stays on url2, whose snapshot is the session's last
// snapshot (the implicit baseline of diff snapshot) and holds the current
// refs.
func init() {
	Register(&Command{
		Name: "diff url",
		Summary: "Open url1, snapshot it (and screenshot it with --screenshot), then url2 the same way, and print the snapshot diff " +
			"(and the pixel diff); the page stays on url2.",
		Args: []Arg{
			{Name: "url1", Help: "the first URL (the before side)", Required: true},
			{Name: "url2", Help: "the second URL (the after side)", Required: true},
		},
		Flags: []Flag{
			{Name: "screenshot", Type: FlagBool, Help: "also compare viewport screenshots of the two pages, pixel by pixel (threshold 0.1)"},
			{Name: "full", Type: FlagBool, Help: "with --screenshot: capture the whole scrollable documents"},
			{Name: "wait-until", Type: FlagEnum, Enum: []string{"load", "domcontentloaded", "networkidle"}, Default: "load", Help: "after each navigation, wait for this load state (networkidle: also no request in flight for 500 ms)"},
			{Name: "selector", Short: "s", Type: FlagString, Help: "snapshot only this element: CSS selector or locator"},
			{Name: "compact", Short: "c", Type: FlagBool, Help: "keep lines with refs, values or text and their ancestors"},
			{Name: "depth", Short: "d", Type: FlagInt, Help: "deepest level shown (0 = top level only)"},
		},
		Page:    LeasedPage,
		Mutates: true,
		Timeout: diffURLTimeout,
		Output:  OutputText,
		// The client refuses --full without --screenshot before it starts
		// the daemon or resolves a page; Run checks again for a caller
		// without the client step. Not in Check: a lone boolean flag
		// must parse.
		ClientStep: func(_ context.Context, in *Input, _ io.Reader) error { return checkDiffURLFlags(*in) },
		Run:        runDiffURL,
	})
}

// diffURLTimeout is the default deadline of diff url: two navigations,
// each with its load wait, two snapshots and maybe two screenshots.
const diffURLTimeout = 60 * time.Second

// diffURLThreshold is the pixel threshold of diff url (agent-browser: the
// default tolerance of diff screenshot).
const diffURLThreshold = devtools.DefaultDiffThreshold

// checkDiffURLFlags refuses --full without --screenshot (agent-browser's
// usage error).
func checkDiffURLFlags(in Input) error {
	if flagBool(in, "full") && !flagBool(in, "screenshot") {
		return usagef("--full only applies to the screenshot diff; add --screenshot (diff url <url1> <url2> --screenshot --full)")
	}
	return nil
}

// urlCapture is what diff url takes of one URL.
type urlCapture struct {
	snapshot string
	png      []byte
}

func runDiffURL(ctx context.Context, env Env, in Input) (Result, error) {
	if err := checkDiffURLFlags(in); err != nil {
		return Result{}, err
	}
	url1, url2 := arg(in, 0), arg(in, 1)
	shot := flagBool(in, "screenshot")
	var caps [2]urlCapture
	for i, u := range []string{url1, url2} {
		c, err := captureURL(ctx, env, in, u, shot)
		if err != nil {
			return Result{}, err
		}
		caps[i] = c
	}
	d := diffSnapshots(snapshotComparisonText(caps[0].snapshot), snapshotComparisonText(caps[1].snapshot))
	data := map[string]any{
		"diff": map[string]any{
			"identical": !d.changed(), "additions": d.add, "removals": d.del, "deletions": d.del,
			"unchanged": d.same, "changed": d.changed(),
		},
		"snapshot":  map[string]any{"diff": d.diff, "additions": d.add, "removals": d.del, "unchanged": d.same, "changed": d.changed()},
		"url1":      url1,
		"url2":      url2,
		"snapshot1": caps[0].snapshot,
		"snapshot2": caps[1].snapshot,
	}
	text := "Snapshot diff:\n" + diffSnapshotText(d.diff, d.add, d.del, d.same)
	if shot {
		s, err := comparePNGs(caps[0].png, caps[1].png)
		if err != nil {
			return Result{}, err
		}
		data["screenshot"] = s.data()
		text += "\n\nScreenshot diff:\n" + s.text()
	}
	return Result{Text: text, Data: data}, nil
}

// captureURL opens u in the page with the --wait-until state, then takes
// the snapshot of the command's snapshot flags and, with shot, a
// screenshot.
func captureURL(ctx context.Context, env Env, in Input, u string, shot bool) (urlCapture, error) {
	mode := flagString(in, "wait-until")
	wait := mode
	if mode == "networkidle" {
		wait = "load"
	}
	if _, err := runOpen(ctx, env, Input{Args: []string{u}, Flags: map[string]any{"wait": wait}, Global: in.Global}); err != nil {
		return urlCapture{}, err
	}
	if mode == "networkidle" {
		w, err := openPage(ctx, env, "diff url")
		if err != nil {
			return urlCapture{}, err
		}
		_, err = w.waitNetworkIdle(ctx)
		w.close()
		if err != nil {
			return urlCapture{}, fmt.Errorf("diff url %s: %w", u, err)
		}
	}
	snap := Input{Flags: map[string]any{"compact": flagBool(in, "compact")}, Global: in.Global}
	if sel := flagString(in, "selector"); sel != "" {
		snap.Flags["selector"] = sel
	}
	if depth, ok := in.Flags["depth"].(int); ok {
		snap.Flags["depth"] = depth
	}
	text, _, err := takeSnapshot(ctx, env, snap)
	if err != nil {
		return urlCapture{}, fmt.Errorf("diff url: the snapshot of %s: %w", u, err)
	}
	c := urlCapture{snapshot: text}
	if shot {
		if c.png, err = screenshotPNG(ctx, env, flagBool(in, "full")); err != nil {
			return urlCapture{}, fmt.Errorf("diff url: the screenshot of %s: %w", u, err)
		}
	}
	return c, nil
}

// pngCompare is the screenshot part of diff url.
type pngCompare struct {
	total, different int
	percent          float64
	// dims is set when the images differ in size: [expected w, h, actual w, h].
	dims *[4]int
}

// comparePNGs compares the screenshots of url1 (a) and url2 (b). Images of
// different sizes are not compared: every pixel counts as different
// (agent-browser diff_screenshot).
func comparePNGs(a, b []byte) (pngCompare, error) {
	ca, err := png.DecodeConfig(bytes.NewReader(a))
	if err != nil {
		return pngCompare{}, fmt.Errorf("decoding the screenshot of url1: %w", err)
	}
	cb, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return pngCompare{}, fmt.Errorf("decoding the screenshot of url2: %w", err)
	}
	if ca.Width != cb.Width || ca.Height != cb.Height {
		n := ca.Width * ca.Height
		return pngCompare{total: n, different: n, percent: 100, dims: &[4]int{ca.Width, ca.Height, cb.Width, cb.Height}}, nil
	}
	d, err := devtools.DiffPNG(a, b, diffURLThreshold)
	if err != nil {
		return pngCompare{}, err
	}
	return pngCompare{total: d.Total, different: d.Different, percent: 100 * d.Ratio}, nil
}

func (c pngCompare) data() map[string]any {
	var dims any
	if c.dims != nil {
		dims = map[string]any{
			"expected": map[string]int{"width": c.dims[0], "height": c.dims[1]},
			"actual":   map[string]int{"width": c.dims[2], "height": c.dims[3]},
		}
	}
	return map[string]any{
		"match": c.dims == nil && c.different == 0, "mismatchPercentage": c.percent, "totalPixels": c.total,
		"differentPixels": c.different, "diffPath": nil, "dimensionMismatch": dims,
	}
}

func (c pngCompare) text() string {
	var b strings.Builder
	switch {
	case c.dims != nil:
		fmt.Fprintf(&b, "Images have different dimensions (%dx%d and %dx%d)", c.dims[0], c.dims[1], c.dims[2], c.dims[3])
	case c.different == 0:
		b.WriteString("Images match (0% difference)")
	default:
		fmt.Fprintf(&b, "%.2f%% pixels differ", c.percent)
	}
	fmt.Fprintf(&b, "\n  %d different / %d total pixels", c.different, c.total)
	return b.String()
}
