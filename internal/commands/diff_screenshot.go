package commands

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/nnemirovsky/iwdp-mcp/internal/devtools"
	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
)

// diff screenshot (design docs/design/agentic-parity.md, section 4.2): a
// viewport screenshot compared pixel by pixel with a baseline PNG on the
// host (devtools.DiffPNG).
func init() {
	Register(&Command{
		Name:    "diff screenshot",
		MCPName: "diff_screenshot",
		Profile: ProfileDevtools,
		Summary: "Compare a viewport screenshot of the page with a baseline PNG, pixel by pixel on the host, and report how many pixels differ.",
		Flags: []Flag{
			{Name: "baseline", Type: FlagPath, Help: "the baseline PNG (for example a viewport screenshot taken earlier)"},
			{
				Name: "threshold", Type: FlagString, Default: strconv.FormatFloat(devtools.DefaultDiffThreshold, 'g', -1, 64),
				Help: "a pixel differs when a colour or alpha value changes by more than this fraction of 255 (0 to below 1)",
			},
		},
		Page:      LeasedPage,
		RetrySafe: true,
		Output:    OutputText,
		Run:       runDiffScreenshot,
	})
}

// pngDataURL is the prefix of the data URL of Page.snapshotRect.
const pngDataURL = "data:image/png;base64,"

func runDiffScreenshot(ctx context.Context, env Env, in Input) (Result, error) {
	baseline := flagString(in, "baseline")
	if baseline == "" {
		return Result{}, usagef("diff screenshot needs --baseline <png>")
	}
	th, err := strconv.ParseFloat(flagString(in, "threshold"), 64)
	if err != nil || math.IsNaN(th) || th < 0 || th >= 1 {
		return Result{}, usagef("--threshold %q: want a number from 0 up to (not including) 1", flagString(in, "threshold"))
	}
	base, err := os.ReadFile(env.ResolvePath(baseline))
	if err != nil {
		return Result{}, fmt.Errorf("reading the baseline: %w", err)
	}
	_, c, err := pageAndCaller(ctx, env)
	if err != nil {
		return Result{}, err
	}
	url, err := tools.CaptureScreenshot(ctx, c, tools.ScreenshotOptions{})
	if err != nil {
		return Result{}, fmt.Errorf("taking the screenshot: %w", err)
	}
	if !strings.HasPrefix(url, pngDataURL) {
		return Result{}, fmt.Errorf("the screenshot is not a PNG data URL (%.30q...)", url)
	}
	shot, err := base64.StdEncoding.DecodeString(url[len(pngDataURL):])
	if err != nil {
		return Result{}, fmt.Errorf("decoding the screenshot: %w", err)
	}
	d, err := devtools.DiffPNG(base, shot, th)
	if err != nil {
		return Result{}, err
	}
	ths := strconv.FormatFloat(th, 'g', -1, 64)
	data := map[string]any{
		"width": d.Width, "height": d.Height, "total": d.Total, "different": d.Different, "ratio": d.Ratio,
		"threshold": th, "match": d.Different == 0,
	}
	if d.Different == 0 {
		return Result{Text: fmt.Sprintf("No pixel differs (threshold %s).", ths), Data: data}, nil
	}
	return Result{Text: fmt.Sprintf("%d of %d pixels differ (%.2f%%) at threshold %s.", d.Different, d.Total, 100*d.Ratio, ths), Data: data}, nil
}
