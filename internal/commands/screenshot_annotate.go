package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/logcause"
	"github.com/edxeth/agent-ios/internal/logsafe"
	"github.com/edxeth/agent-ios/internal/refs"
	"github.com/edxeth/agent-ios/internal/runtime"
	"github.com/edxeth/agent-ios/internal/tools"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// screenshot with --annotate and --if-changed (design
// docs/design/agentic-parity.md, section 3.9; agent-browser "screenshot").
// The capture itself is the W0 code (tools.CaptureScreenshot). The
// command also takes the legacy agent-ios forms (-o file, --full-page,
// --selector css).
func init() {
	Register(&Command{
		Name:    "screenshot",
		Summary: "Capture the viewport, the full page or one element as PNG; --annotate labels each ref [N] (@eN), --if-changed skips an identical capture.",
		Args:    []Arg{{Name: "path", Help: "PNG file to write (default $TMPDIR/agent-ios-screenshot-<time>.png)"}},
		Flags: []Flag{
			{Name: "output", Short: "o", Type: FlagPath, Help: "PNG file to write (same as path)"},
			{Name: "full-page", Type: FlagBool, Help: "capture the whole scrollable document"},
			{Name: "full", Type: FlagBool, Help: "same as --full-page"},
			{Name: "selector", Type: FlagString, Help: "capture one element: CSS selector or @eN ref"},
			{Name: "annotate", Type: FlagBool, Help: "take a snapshot and draw a box and an [N] label for each @eN ref in the capture; the overlay is removed after the capture"},
			{Name: "if-changed", Type: FlagBool, Help: "compare with this session's previous capture and write nothing when the PNG is identical"},
		},
		Page:    LeasedPage,
		Mutates: true, // --annotate draws into the page, and refs are allocated
		Output:  OutputFile,
		// The usage errors of the options end the command before the
		// daemon attaches the page.
		ClientStep: func(_ context.Context, in *Input, _ io.Reader) error {
			_, _, _, err := screenshotOptions(*in)
			return err
		},
		Run: runScreenshot,
	})
}

// screenshotOptions returns the file (path or -o), --full-page (or
// --full) and --selector of a screenshot, or the usage error of a
// conflicting combination. A page URL as the file is a usage error that
// names --page: earlier agent-ios versions took "screenshot <ws:// URL>"
// as the page, and that line must not capture the session's page instead.
func screenshotOptions(in Input) (out string, full bool, sel string, err error) {
	out = arg(in, 0)
	if o := flagString(in, "output"); o != "" {
		if out != "" {
			return "", false, "", usagef("screenshot: give the file once, as path or with -o")
		}
		out = o
	}
	if isPageURL(out) {
		return "", false, "", usagef("screenshot: %q is a page URL, not a file; pass the page with --page <ws:// URL>", out)
	}
	full = flagBool(in, "full-page") || flagBool(in, "full")
	sel = flagString(in, "selector")
	if full && sel != "" {
		return "", false, "", usagef("screenshot: use either --full-page or --selector, not both")
	}
	return out, full, sel, nil
}

// overlayCleanupBudget bounds the cleanup that removes the overlay after
// the command's operation ended (removeOverlay). The operation's slot is
// held until the cleanup ends, so a queued successor waits at most this
// long; it equals the budget of the best-effort page cancel (design 2.6).
const overlayCleanupBudget = time.Second

// captureArea is a capture rectangle in document (Page) coordinates.
type captureArea struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// shotLabel is one label of an annotated capture. Box is the ref's box
// in whole CSS pixels in the coordinates of the captured image, as
// agent-browser 0.45's annotation box: relative to the viewport, to the
// document with --full, or to the element with --selector. A ref inside a
// same-origin frame is placed through its frames the same way
// (mod-overlay/90-overlay.js).
type shotLabel struct {
	N    int       `json:"n"`
	Ref  string    `json:"ref"`
	Role string    `json:"role"`
	Name string    `json:"name"`
	Box  *labelBox `json:"box,omitempty"`
}

// labelBox is the box of an annotation label.
type labelBox struct {
	X      int64 `json:"x"`
	Y      int64 `json:"y"`
	Width  int64 `json:"width"`
	Height int64 `json:"height"`
}

// line is the label in the text of screenshot; private mode makes it from
// the members that it prints (privateReads).
func (l shotLabel) line() string { return fmt.Sprintf("[%d] @%s %s %q", l.N, l.Ref, l.Role, l.Name) }

func runScreenshot(ctx context.Context, env Env, in Input) (Result, error) {
	out, full, sel, err := screenshotOptions(in)
	if err != nil {
		return Result{}, err
	}
	w, err := openPage(ctx, env, "screenshot")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	// A capture shows the top document, so it works there (frame.go).
	leaveFrame(w.ps, w.op, false)
	annotate := flagBool(in, "annotate")
	var labels []shotLabel
	var dataURL string
	if annotate {
		var area *captureArea
		labels, area, err = w.drawOverlay(ctx, full, sel)
		if err != nil {
			return Result{}, err
		}
		if area != nil {
			// The element's box, in the coordinates the overlay used:
			// Page.snapshotNode would paint the element alone, without
			// the overlay.
			dataURL, err = snapshotArea(ctx, w.caller, *area)
		} else {
			dataURL, err = w.capture(ctx, full, sel)
		}
		// The overlay goes in every case (design 3.9: a finally step), also
		// when the capture failed or the operation ended.
		if cerr := w.removeOverlay(ctx, env); err == nil && cerr != nil {
			return Result{}, fmt.Errorf("removing the screenshot overlay: %w", cerr)
		}
	} else {
		dataURL, err = w.capture(ctx, full, sel)
	}
	if err != nil {
		if cause := w.ended(ctx); cause != nil {
			// The webkit error carries only ctx.Err(); the cause says
			// whether the deadline passed (exit 124) or the client left.
			if errors.Is(cause, core.ErrDeadline) {
				cause = fmt.Errorf("%w: %w", cause, context.DeadlineExceeded)
			}
			return Result{}, fmt.Errorf("%w: %w", err, cause)
		}
		return Result{}, err
	}
	data, err := base64.StdEncoding.DecodeString(dataURL[strings.IndexByte(dataURL, ',')+1:])
	if err != nil {
		return Result{}, fmt.Errorf("decoding the capture: %w", err)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	w.st.mu.Lock()
	prev := w.st.lastShot
	w.st.lastShot = hash
	w.st.mu.Unlock()
	if flagBool(in, "if-changed") && prev == hash {
		return Result{Text: "unchanged", Data: map[string]any{"unchanged": true, "sha256": hash}}, nil
	}
	path, err := writeCapture(env.ResolvePath(out), data)
	if err != nil {
		return Result{}, err
	}
	size := fmt.Sprintf("%d bytes", len(data))
	if cfg, err := png.DecodeConfig(bytes.NewReader(data)); err == nil {
		size = fmt.Sprintf("%dx%d px, %s", cfg.Width, cfg.Height, size)
	}
	lines := []string{fmt.Sprintf("Screenshot saved to %s (%s)", path, size)}
	for _, l := range labels {
		lines = append(lines, l.line())
	}
	return Result{Text: strings.Join(lines, "\n"), Data: map[string]any{"path": path, "sha256": hash, "labels": labels}}, nil
}

// overlayFn and overlayClearFn are the runtime functions of the overlay,
// which lives in the overlay module (parity-045 decision D5): each call
// installs the module first where the document lacks it.
var (
	overlayFn      = runtime.With("overlay", runtime.ModOverlay)
	overlayClearFn = runtime.With("overlayClear", runtime.ModOverlay)
)

// drawOverlay takes a snapshot (new refs for the current document) and
// draws the overlay for the refs whose boxes intersect the capture area:
// the viewport, the full page, or the box of the element sel (a CSS
// selector or a ref), which it returns as area.
func (w *w3bPage) drawOverlay(ctx context.Context, full bool, sel string) ([]shotLabel, *captureArea, error) {
	var a allocated
	if _, _, err := w.allocate(ctx, "snapshot", map[string]any{}, &a); err != nil {
		return nil, nil, fmt.Errorf("screenshot --annotate: snapshot: %w", err)
	}
	args := map[string]any{"full": full}
	if sel != "" {
		spec, err := w.spec(ctx, sel)
		if err != nil {
			return nil, nil, err
		}
		args["t"] = spec
	}
	byRef := map[string]shotLabel{}
	ids := make([]string, 0, len(a.Refs))
	for _, r := range a.Refs {
		n, _ := refs.ParseRef(r.Ref)
		byRef[r.Ref] = shotLabel{N: n, Ref: r.Ref, Role: r.Role, Name: r.Name}
		ids = append(ids, r.Ref)
	}
	args["refs"] = ids
	var drawn struct {
		Refs  []string      `json:"refs"`
		Boxes []captureArea `json:"boxes"`
		Area  captureArea   `json:"area"`
	}
	if err := w.rt.Call(ctx, overlayFn, args, &drawn); err != nil {
		// A failed draw can leave a partial overlay.
		return nil, nil, errors.Join(fmt.Errorf("screenshot --annotate: drawing the overlay: %w", err), w.removeOverlay(ctx, nil))
	}
	out := make([]shotLabel, 0, len(drawn.Refs))
	for i, r := range drawn.Refs {
		l := byRef[r]
		if i < len(drawn.Boxes) {
			b := drawn.Boxes[i]
			b.X, b.Y = b.X-drawn.Area.X, b.Y-drawn.Area.Y // image coordinates; the area is (0, 0) for --full
			l.Box = &labelBox{X: int64(math.Round(b.X)), Y: int64(math.Round(b.Y)), Width: int64(math.Round(b.Width)), Height: int64(math.Round(b.Height))}
		}
		out = append(out, l)
	}
	if sel == "" {
		return out, nil, nil
	}
	return out, &drawn.Area, nil
}

// snapshotArea captures a document area with Page.snapshotRect in Page
// coordinates, which also reaches an area outside the viewport. The area
// is widened to whole pixels.
func snapshotArea(ctx context.Context, c webkit.Caller, a captureArea) (string, error) {
	x, y := math.Floor(a.X), math.Floor(a.Y)
	width, height := int(math.Ceil(a.X+a.Width)-x), int(math.Ceil(a.Y+a.Height)-y)
	if width <= 0 || height <= 0 {
		return "", fmt.Errorf("screenshot --selector: the element has no area to capture (%gx%g)", a.Width, a.Height)
	}
	raw, err := c.Send(ctx, "Page.snapshotRect", map[string]any{"x": int(x), "y": int(y), "width": width, "height": height, "coordinateSystem": "Page"})
	if err != nil {
		return "", err
	}
	var res struct {
		DataURL string `json:"dataURL"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || !strings.HasPrefix(res.DataURL, "data:image/png;base64,") {
		return "", logcause.Errorf("Page.snapshotRect: the reply has no PNG data URL (%v)", err)
	}
	return res.DataURL, nil
}

// removeOverlay removes the annotation overlay. While the command's
// operation is active the call runs in it. After the operation ended
// (deadline or cancel) its fenced caller sends nothing more, so the
// removal becomes a cleanup of the operation (core.Op.Finally): it runs
// when the operation ends and before the operation's slot is released, so
// no queued successor sees the overlay (design 2.10). It is fenced to the
// same lease epoch: a later epoch refuses it, and that owner's first
// runtime call resets the page, which also removes the overlay
// (mod-overlay/90-overlay.js). env, when set, gives the daemon log for the cleanup's
// outcome.
func (w *w3bPage) removeOverlay(ctx context.Context, env Env) error {
	if w.ended(ctx) == nil || w.ps.Sched == nil {
		return w.rt.Call(ctx, overlayClearFn, nil, nil)
	}
	logf := func(string, ...any) {}
	if m, err := managerOf(env); err == nil {
		logf = m.Logf
	}
	if err := w.op.Finally(overlayCleanupBudget, func(ctx context.Context, c webkit.Caller) {
		clearOverlay(ctx, w.ps.Runtime.Via(c), logf)
	}); err != nil {
		logf("screenshot --annotate: the overlay stays until the next owner's first runtime call: %v", logsafe.Error(err))
	}
	return nil
}

// ended returns why the command's context or its operation ended (the
// operation's deadline can pass a moment before ctx's), or nil.
func (w *w3bPage) ended(ctx context.Context) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if w.op != nil && w.op.Context().Err() != nil {
		return context.Cause(w.op.Context())
	}
	return nil
}

// clearOverlay is the cleanup of removeOverlay after the command's
// operation ended; rt sends through the operation's lease-fenced caller.
func clearOverlay(ctx context.Context, rt *runtime.Runtime, logf func(string, ...any)) {
	if err := rt.Call(ctx, overlayClearFn, nil, nil); err != nil {
		logf("screenshot --annotate: removing the overlay after the command ended: %v", logsafe.Error(err))
	}
}

// capture returns the PNG data URL of the viewport, the full page or one
// element (a CSS selector, or a ref through the runtime resolver).
func (w *w3bPage) capture(ctx context.Context, full bool, sel string) (string, error) {
	if _, ok := refs.ParseRef(sel); ok {
		spec, err := w.spec(ctx, sel)
		if err != nil {
			return "", err
		}
		node, err := w.nodeOf(ctx, spec)
		if err != nil {
			return "", err
		}
		return tools.SnapshotNode(ctx, w.caller, node)
	}
	return tools.CaptureScreenshot(ctx, w.caller, tools.ScreenshotOptions{FullPage: full, Selector: sel})
}

// writeCapture writes a PNG to path, or to a new private file in the
// temporary directory when path is "" (captures can show secrets).
func writeCapture(path string, data []byte) (string, error) {
	if path != "" {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return "", fmt.Errorf("writing the screenshot: %w", err)
		}
		return path, nil
	}
	name := "agent-ios-screenshot-" + time.Now().Format("20060102-150405.000")
	path = filepath.Join(os.TempDir(), name+".png")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		f, err = os.CreateTemp(os.TempDir(), name+"-*.png")
	}
	if err != nil {
		return "", fmt.Errorf("creating the screenshot file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("writing the screenshot: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("writing the screenshot: %w", err)
	}
	return f.Name(), nil
}
