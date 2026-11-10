package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/upload"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Action commands of design 3.5 and 4.2 (W3b): every event is synthetic,
// every result says "trusted": false, and the checks of 50-actionability.js
// run in attempts of the daemon-owned loop of design 2.6.

// actionTimeout is the default deadline of an action command: design 3.5
// repeats the actionability attempts until the action timeout, default
// 5 s; --timeout overrides it.
const actionTimeout = 5 * time.Second

// dragSteps is the number of pointer increments of drag (design 3.5).
const dragSteps = 10

var forceFlag = Flag{Name: "force", Type: FlagBool, Help: "skip the actionability checks (attached, visible, stable, enabled, receives events)"}

// registerAction registers an action command with the common settings.
func registerAction(c *Command) {
	c.Page, c.Mutates, c.Output = LeasedPage, true, OutputText
	if c.Timeout == 0 {
		c.Timeout = actionTimeout
	}
	Register(c)
}

func targetArg(help string) Arg {
	return Arg{Name: "target", Help: help + ": @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator", Required: true}
}

func init() {
	registerAction(&Command{
		Name:    "click",
		Summary: "Click an element with synthetic pointer and mouse events after the actionability checks.",
		Args:    []Arg{targetArg("element to click")},
		Flags: []Flag{
			{Name: "button", Type: FlagEnum, Enum: []string{"left", "right", "middle"}, Default: "left", Help: "mouse button; right ends with contextmenu, middle with auxclick"},
			{Name: "count", Type: FlagInt, Help: "number of clicks (detail 1..n)"},
			{Name: "new-tab", Type: FlagBool, Help: "open the link's href with window.open (emulated user gesture) instead of clicking; a gesture call delivered after its window opens nothing, but the page may then hold a transient user activation for a few seconds"},
			forceFlag,
		},
		Run: runClick,
	})
	registerAction(&Command{
		Name:    "dblclick",
		Summary: "Double-click an element: two click sequences (detail 1 and 2), then dblclick.",
		Args:    []Arg{targetArg("element to double-click")},
		Flags:   []Flag{forceFlag},
		Run:     simpleAction("dblclick", "dblclick", "double-clicked"),
	})
	registerAction(&Command{
		Name:    "tap",
		Summary: "Tap an element, or the element at viewport point <x> <y>: touchstart and touchend, then the click sequence with pointerType touch.",
		Args: []Arg{
			targetArg("element to tap, or the x coordinate of a point (with y)"),
			{Name: "y", Help: "y coordinate of the point to tap; target is then its x coordinate (CSS pixels of the viewport)"},
		},
		Flags: []Flag{forceFlag},
		Run:   runTap,
	})
	registerAction(&Command{
		Name:    "hover",
		Summary: "Move the pointer over an element (synthetic events do not set CSS :hover; --css forces it).",
		Args:    []Arg{targetArg("element to hover")},
		Flags: []Flag{
			{Name: "css", Type: FlagBool, Help: "also force the CSS :hover state with CSS.forcePseudoState until the next action or release; needs the CSS domain, which only WebKitGTK answers (through iwdp CSS.enable hangs, so --css fails with exit 2 before any event)"},
			forceFlag,
		},
		Run: runHover,
	})
	registerAction(&Command{
		Name:    "focus",
		Summary: "Focus an element (el.focus()).",
		Args:    []Arg{targetArg("element to focus")},
		Run:     simpleAction("focus", "focus", "focused"),
	})
	registerAction(&Command{
		Name:    "blur",
		Summary: "Blur the focused element.",
		Run:     simpleAction("blur", "blur", "blurred the focused element"),
	})
	registerAction(&Command{
		Name:    "check",
		Summary: "Check a checkbox or radio (a hidden input through its label) and read the state back.",
		Args:    []Arg{targetArg("checkbox or radio")},
		Flags: []Flag{
			{Name: "checked", Type: FlagEnum, Enum: []string{"true", "false"}, Default: "true", Help: "the wanted state; false unchecks"},
			forceFlag,
		},
		Run: runCheck(true),
	})
	registerAction(&Command{
		Name:    "uncheck",
		Summary: "Uncheck a checkbox (a hidden input through its label) and read the state back.",
		Args:    []Arg{targetArg("checkbox")},
		Flags:   []Flag{forceFlag},
		Run:     runCheck(false),
	})
	registerAction(&Command{
		Name:    "select",
		Summary: "Select options of a <select> by value, then by visible label; dispatches input and change.",
		Args:    []Arg{targetArg("select element"), {Name: "values", Help: "option values or labels (several for a multiple select)", Required: true, Variadic: true}},
		Flags:   []Flag{forceFlag},
		Run:     runSelect,
	})
	registerAction(&Command{
		Name:    "scroll",
		Summary: "Scroll the window or an element up, down, left or right (default 300 px).",
		Args:    []Arg{{Name: "direction", Help: "up, down, left or right", Required: true}, {Name: "px", Help: "pixels (default 300)"}},
		Flags:   []Flag{{Name: "selector", Type: FlagString, Help: "scroll this element instead of the window"}},
		Run:     runScroll("scroll", "scroll", "scrolled"),
	})
	registerAction(&Command{
		Name:    "swipe",
		Summary: "Swipe with synthetic touch events; scrolls as a fallback when the page cancels none (swipe up scrolls down).",
		Args:    []Arg{{Name: "direction", Help: "up, down, left or right (the finger's direction)", Required: true}, {Name: "px", Help: "pixels (default 300)"}},
		Flags:   []Flag{{Name: "selector", Type: FlagString, Help: "swipe on this element instead of the page"}},
		Run:     runScroll("swipe", "swipe", "swiped"),
	})
	registerAction(&Command{
		Name:    "scrollintoview",
		Summary: "Scroll an element into the center of the viewport.",
		Args:    []Arg{targetArg("element")},
		Run:     simpleAction("scrollintoview", "scrollintoview", "scrolled into view"),
	})
	registerAction(&Command{
		Name:    "drag",
		Summary: "Drag from one element to another in 10 pointer steps with synthetic drag events (best effort).",
		Args:    []Arg{targetArg("source element"), {Name: "destination", Help: "drop target: @eN ref, CSS selector or locator", Required: true}},
		Flags:   []Flag{forceFlag},
		Run:     runDrag,
	})
	registerAction(&Command{
		Name:    "upload",
		Summary: "Set the files of a file input (read on the host, at most 8 MiB in total) and dispatch input and change.",
		Args:    []Arg{targetArg("file input"), {Name: "files", Help: "file paths, relative to the current directory", Required: true, Variadic: true}},
		Run:     runUpload,
	})
	registerAction(&Command{
		Name:    "mouse",
		Summary: "Low-level mouse: move <x> <y> [--steps n], down [button], up [button], wheel <dy> [dx].",
		Args: []Arg{
			{Name: "action", Help: "move, down, up or wheel", Required: true},
			{Name: "a", Help: "move: x; down and up: left, right or middle; wheel: dy"},
			{Name: "b", Help: "move: y; wheel: dx"},
		},
		Flags: []Flag{{Name: "steps", Type: FlagInt, Help: "move: number of increments, each one step (default 1)"}},
		Run:   runMouse,
	})
	registerAction(&Command{
		Name:    "highlight",
		Summary: "Highlight an element on the device screen (DOM.highlightNode) until release.",
		Args:    []Arg{{Name: "target", Help: "element to highlight: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator"}},
		Flags:   []Flag{{Name: "node-id", Type: FlagInt, Help: "DOM node id instead of a target (as DOM.getDocument and query-selector return it)"}},
		Run:     runHighlight,
	})
}

func flagInt(in Input, name string, def int) int {
	if v, ok := in.Flags[name].(int); ok && v != 0 {
		return v
	}
	return def
}

// simpleAction is an action with only a target (or none) and no fields.
func simpleAction(name, kind, done string) func(context.Context, Env, Input) (Result, error) {
	return func(ctx context.Context, env Env, in Input) (Result, error) {
		w, err := openPage(ctx, env, name)
		if err != nil {
			return Result{}, err
		}
		defer w.close()
		t := arg(in, 0)
		r, err := w.run(ctx, action{Name: name, Kind: kind, Target: t, Force: flagBool(in, "force")})
		if err != nil {
			return Result{}, err
		}
		return w.result(strings.TrimSpace(done+" "+t), r), nil
	}
}

var buttons = map[string]int{"left": 0, "middle": 1, "right": 2}

// runTap taps a target, or with two arguments the element at a viewport
// point (design 4.2: tap <target|x y>).
func runTap(ctx context.Context, env Env, in Input) (Result, error) {
	if len(in.Args) < 2 {
		return simpleAction("tap", "tap", "tapped")(ctx, env, in)
	}
	x, errX := strconv.ParseFloat(arg(in, 0), 64)
	y, errY := strconv.ParseFloat(arg(in, 1), 64)
	if errX != nil || errY != nil {
		return Result{}, usagef("tap %q %q: a point is two numbers <x> <y> (viewport CSS pixels)", arg(in, 0), arg(in, 1))
	}
	w, err := openPage(ctx, env, "tap")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	r, err := w.run(ctx, action{Name: "tap", Kind: "tapAt", Args: func(int) map[string]any { return map[string]any{"x": x, "y": y} }})
	if err != nil {
		return Result{}, err
	}
	var v struct {
		At string `json:"at"`
	}
	_ = json.Unmarshal(r.Value, &v)
	return w.result(fmt.Sprintf("tapped %s at (%g, %g)", v.At, x, y), r), nil
}

func runClick(ctx context.Context, env Env, in Input) (Result, error) {
	btn, ok := buttons[flagString(in, "button")]
	if !ok && flagString(in, "button") != "" {
		return Result{}, usagef("click --button %q: want left, right or middle", flagString(in, "button"))
	}
	count := flagInt(in, "count", 1)
	if count < 1 {
		return Result{}, usagef("click --count %d: want 1 or more", count)
	}
	w, err := openPage(ctx, env, "click")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	t := arg(in, 0)
	if flagBool(in, "new-tab") {
		return w.openInNewTab(ctx, t, flagBool(in, "force"))
	}
	r, err := w.run(ctx, action{
		Name: "click", Kind: "click", Target: t, Force: flagBool(in, "force"),
		Args: func(int) map[string]any { return map[string]any{"b": btn, "cnt": count} },
	})
	if err != nil {
		return Result{}, err
	}
	return w.result("clicked "+t, r), nil
}

// openInNewTab opens a link's href with window.open (design 3.5 click
// --new-tab): one fenced attempt step with click's checks, whose runtime
// call carries emulateUserGesture (runtime function actGesture, kind
// newtab), so popup blocking sees a user gesture.
func (w *w3bPage) openInNewTab(ctx context.Context, t string, force bool) (Result, error) {
	r, err := w.run(ctx, action{Name: "click --new-tab", Kind: "newtab", Target: t, Force: force, Gesture: true})
	if err != nil {
		return Result{}, err
	}
	var v struct {
		Href string `json:"href"`
	}
	if err := json.Unmarshal(r.Value, &v); err != nil {
		return Result{}, fmt.Errorf("click --new-tab: decoding the page's result %s: %w", r.Value, err)
	}
	return w.result("opened "+v.Href+" with window.open (popup blocking can stop it; run tab list)", r), nil
}

// evalValue decodes the value of a Runtime.evaluate reply with returnByValue.
func evalValue[T any](raw json.RawMessage) (T, error) {
	var v T
	var ev struct {
		Result struct {
			Value       json.RawMessage `json:"value"`
			Description string          `json:"description"`
		} `json:"result"`
		WasThrown bool `json:"wasThrown"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return v, err
	}
	if ev.WasThrown {
		return v, fmt.Errorf("%s", strings.TrimPrefix(ev.Result.Description, "Error: "))
	}
	if len(ev.Result.Value) == 0 {
		return v, nil
	}
	err := json.Unmarshal(ev.Result.Value, &v)
	return v, err
}

func runHover(ctx context.Context, env Env, in Input) (Result, error) {
	w, err := openPage(ctx, env, "hover")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	t := arg(in, 0)
	css := flagBool(in, "css")
	if css {
		// End an earlier forced state first: it holds the CSS domain,
		// which the forced state of this command needs as well.
		if err := w.clearHover(ctx); err != nil {
			return Result{}, err
		}
		if err := w.enableCSS(ctx); err != nil {
			return Result{}, err
		}
	}
	r, err := w.run(ctx, action{Name: "hover", Kind: "hover", Target: t, Force: flagBool(in, "force")})
	if css {
		// From here the next action or the release disables the CSS
		// domain and resets the forced state, also when this one failed.
		w.st.mu.Lock()
		w.st.hoverCSS = true
		w.st.mu.Unlock()
		if err == nil {
			err = w.forceHover(ctx, t)
		}
	}
	if err != nil {
		return Result{}, err
	}
	if css {
		r.Emulated = append(r.Emulated, "CSS :hover forced with CSS.forcePseudoState until the next action")
	}
	return w.result("hovered "+t, r), nil
}

// errNoCSSDomain: hover --css needs the CSS agent, and the transport
// refuses CSS.enable (design 4.3, impossible or partial features).
var errNoCSSDomain = errors.New("--css: CSS :hover cannot be forced on this transport: WebKit applies CSS.forcePseudoState only while the CSS domain is enabled, and CSS.enable hangs through ios-webkit-debug-proxy, so it is not sent (only the WebKitGTK transport answers it). Run hover without --css; synthetic events do not set :hover")

// errNoDesiredState: without the page's desired state (a page session
// made outside the daemon) nothing would reset a forced state.
var errNoDesiredState = errors.New("--css: this page session has no desired state (core.DesiredState), which holds the CSS domain and resets the forced state; run it through the daemon")

// enableCSS gives the session's hover owner the CSS domain, which WebKit
// needs to apply a forced pseudo state (InspectorCSSAgent: a forced state
// counts only while the agent is enabled). The first owner's acquisition
// sends CSS.enable. A refused CSS.enable fails with exit 2 and changes
// nothing; when its outcome is unknown, the owner keeps the domain so that
// the next action or the release disables it. It is one change of the
// desired state (core.DesiredState.Change), so it never interleaves with
// another owner's enable or disable, and an operation that a release ended
// acquires nothing. A CSS cleanup that an earlier release left pending is
// reset first, in the same change (core.ResetUnknownAgent).
func (w *w3bPage) enableCSS(ctx context.Context) error {
	if w.ps.State == nil {
		return errNoDesiredState
	}
	owner := hoverOwner(w.session)
	return w.ps.State.Change(ctx, w.caller, func(c webkit.Caller) error {
		if err := core.ResetUnknownAgent(ctx, w.ps, c, "CSS"); err != nil {
			return fmt.Errorf("--css: %w", err)
		}
		if !w.ps.State.Acquire(owner, "CSS") {
			return nil
		}
		_, err := c.Send(ctx, "CSS.enable", nil)
		if err == nil {
			return nil
		}
		var pe *webkit.ErrorData
		switch {
		case errors.Is(err, webkit.ErrHangsThroughTarget):
			w.ps.State.ReleaseDomain(owner, "CSS")
			return &core.CodedError{Code: ExitUsage, Err: errNoCSSDomain}
		case errors.Is(err, webkit.ErrNotSent) || errors.As(err, &pe):
			w.ps.State.ReleaseDomain(owner, "CSS")
		default:
			w.st.mu.Lock()
			w.st.hoverCSS = true
			w.st.mu.Unlock()
		}
		return fmt.Errorf("--css: CSS.enable: %w", err)
	})
}

// forceHover sets CSS.forcePseudoState{hover} on the target's node as a
// desired-state entry of the session; the next action or the release
// resets it (design 2.4, 3.5). The session holds the CSS domain
// (enableCSS).
func (w *w3bPage) forceHover(ctx context.Context, t string) error {
	spec, err := w.spec(ctx, t)
	if err != nil {
		return err
	}
	n, err := w.nodeOf(ctx, spec)
	if err != nil {
		return err
	}
	// One change: a release that ended the operation meanwhile gets no
	// entry recorded after its removal of the session's owners. The entry
	// is recorded before the send (DesiredState.Change), so a commit that
	// replaces the node's target after a successful send drops it with the
	// document (Entry.Scoped) and its release never sends n to the new
	// target. Only a failure that proves the call had no effect removes the
	// entry (core.NoEffect); after an unknown outcome (a reply lost after
	// the write) the hover may be active, so its reset stays recorded.
	key := "w3b:hover|" + w.session
	return w.ps.State.Change(ctx, w.caller, func(webkit.Caller) error {
		w.ps.State.Put(core.Entry{
			Key: key, Owner: hoverOwner(w.session), Domain: "CSS", Scoped: true,
			Release: func(ctx context.Context, s core.Sender) error {
				_, err := s.Send(ctx, "CSS.forcePseudoState", map[string]any{"nodeId": n, "forcedPseudoClasses": []string{}})
				return err
			},
		})
		// n is valid only on the target that reported it: the operation's
		// own caller keeps the execution context's target fence (frame
		// scope). The change's caller addresses the change's target, which
		// a commit since nodeOf may have replaced.
		if _, err := w.caller.Send(ctx, "CSS.forcePseudoState", map[string]any{"nodeId": n, "forcedPseudoClasses": []string{"hover"}}); err != nil {
			if core.NoEffect(err) {
				w.ps.State.Remove(key)
			}
			return fmt.Errorf("hover --css: %w", err)
		}
		return nil
	})
}

func runCheck(want bool) func(context.Context, Env, Input) (Result, error) {
	return func(ctx context.Context, env Env, in Input) (Result, error) {
		v := want
		if s := flagString(in, "checked"); s != "" {
			v = s == "true"
		}
		w, err := openPage(ctx, env, "check")
		if err != nil {
			return Result{}, err
		}
		defer w.close()
		t := arg(in, 0)
		r, err := w.run(ctx, action{
			Name: "check", Kind: "check", Target: t, Force: flagBool(in, "force"),
			Args: func(int) map[string]any { return map[string]any{"v": v} },
		})
		if err != nil {
			return Result{}, err
		}
		done := "checked "
		if !v {
			done = "unchecked "
		}
		return w.result(done+t, r), nil
	}
}

func runSelect(ctx context.Context, env Env, in Input) (Result, error) {
	vals := in.Args[1:]
	if len(vals) == 0 {
		return Result{}, usagef("select needs at least one value")
	}
	w, err := openPage(ctx, env, "select")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	r, err := w.run(ctx, action{
		Name: "select", Kind: "select", Target: arg(in, 0), Force: flagBool(in, "force"),
		Args: func(int) map[string]any { return map[string]any{"vals": vals} },
	})
	if err != nil {
		return Result{}, err
	}
	return w.result(fmt.Sprintf("selected %s in %s", r.Value, arg(in, 0)), r), nil
}

// direction returns the scroll delta of a direction word and an optional
// pixel count (default 300 px, design 3.5).
func direction(dir, px string) (dx, dy int, err error) {
	n := 300
	if px != "" {
		if n, err = strconv.Atoi(px); err != nil || n <= 0 {
			return 0, 0, usagef("%q: the pixel count must be a positive integer", px)
		}
	}
	switch dir {
	case "up":
		return 0, -n, nil
	case "down":
		return 0, n, nil
	case "left":
		return -n, 0, nil
	case "right":
		return n, 0, nil
	}
	return 0, 0, usagef("direction %q: want up, down, left or right", dir)
}

func runScroll(name, kind, done string) func(context.Context, Env, Input) (Result, error) {
	return func(ctx context.Context, env Env, in Input) (Result, error) {
		dx, dy, err := direction(arg(in, 0), arg(in, 1))
		if err != nil {
			return Result{}, err
		}
		if kind == "swipe" {
			// The finger moves up, so the content scrolls down.
			dx, dy = -dx, -dy
		}
		w, err := openPage(ctx, env, name)
		if err != nil {
			return Result{}, err
		}
		defer w.close()
		r, err := w.run(ctx, action{
			Name: name, Kind: kind, Target: flagString(in, "selector"),
			Args: func(int) map[string]any { return map[string]any{"dx": dx, "dy": dy} },
		})
		if err != nil {
			return Result{}, err
		}
		return w.result(fmt.Sprintf("%s %s", done, arg(in, 0)), r), nil
	}
}

func runDrag(ctx context.Context, env Env, in Input) (Result, error) {
	w, err := openPage(ctx, env, "drag")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	r, err := w.run(ctx, action{Name: "drag", Kind: "drag", Target: arg(in, 0), Target2: arg(in, 1), Steps: dragSteps, Force: flagBool(in, "force")})
	if err != nil {
		return Result{}, err
	}
	var v struct {
		Dropped bool `json:"dropped"`
	}
	_ = json.Unmarshal(r.Value, &v)
	done := fmt.Sprintf("dragged %s to %s in %d steps (best effort: native HTML drag and drop is not driven by synthetic events)", arg(in, 0), arg(in, 1), dragSteps)
	if !v.Dropped {
		done += "; no drop: the page canceled no dragover"
	}
	return w.result(done, r), nil
}

type uploadFile struct {
	Name string `json:"name"`
	Type string `json:"type"`
	B64  string `json:"b64"`
}

// runUpload reads the files with the shared bounded reader (upload.Read:
// regular files only, at most the 8 MiB limit of design 3.5 in total,
// stopped by ctx); on WebKitGTK the wire-byte limit of the transport also
// applies.
func runUpload(ctx context.Context, env Env, in Input) (Result, error) {
	var files []uploadFile
	total := 0
	for _, p := range in.Args[1:] {
		data, err := upload.Read(ctx, env.ResolvePath(p), upload.MaxBytes-total)
		if err != nil {
			return Result{}, fmt.Errorf("upload: reading %s: %w", p, err)
		}
		total += len(data)
		if total > upload.MaxBytes {
			return Result{}, fmt.Errorf("upload: the files are %d bytes or more; the limit is %d bytes (8 MiB)", total, upload.MaxBytes)
		}
		name := p[strings.LastIndex(p, "/")+1:]
		files = append(files, uploadFile{Name: name, Type: mimeOf(name), B64: base64.StdEncoding.EncodeToString(data)})
	}
	w, err := openPage(ctx, env, "upload")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	r, err := w.run(ctx, action{
		Name: "upload", Kind: "upload", Target: arg(in, 0),
		Args: func(int) map[string]any { return map[string]any{"files": files} },
	})
	if err != nil {
		return Result{}, err
	}
	return w.result(fmt.Sprintf("set %d file(s), %d bytes, on %s", len(files), total, arg(in, 0)), r), nil
}

// mimeOf guesses a file's MIME type from its extension for File.type.
func mimeOf(name string) string {
	ext := strings.ToLower(name[strings.LastIndex(name, ".")+1:])
	switch ext {
	case "png", "gif", "webp":
		return "image/" + ext
	case "jpg", "jpeg":
		return "image/jpeg"
	case "txt":
		return "text/plain"
	case "json", "pdf":
		return "application/" + ext
	case "csv", "html":
		return "text/" + ext
	}
	return "application/octet-stream"
}

func runMouse(ctx context.Context, env Env, in Input) (Result, error) {
	op, a, b := arg(in, 0), arg(in, 1), arg(in, 2)
	fields := map[string]any{"op": op}
	steps := 1
	num := func(s, what string) (float64, error) {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, usagef("mouse %s: %s %q is not a number", op, what, s)
		}
		return f, nil
	}
	switch op {
	case "move":
		x, err := num(a, "x")
		if err != nil {
			return Result{}, err
		}
		y, err := num(b, "y")
		if err != nil {
			return Result{}, err
		}
		fields["x"], fields["y"] = x, y
		if steps = flagInt(in, "steps", 1); steps < 1 {
			return Result{}, usagef("mouse move --steps %d: want 1 or more", steps)
		}
	case "down", "up":
		btn, ok := buttons[a]
		if !ok && a != "" {
			return Result{}, usagef("mouse %s %q: want left, right or middle", op, a)
		}
		fields["b"] = btn
	case "wheel":
		dy, err := num(a, "dy")
		if err != nil {
			return Result{}, err
		}
		dx := 0.0
		if b != "" {
			if dx, err = num(b, "dx"); err != nil {
				return Result{}, err
			}
		}
		fields["dx"], fields["dy"] = dx, dy
	default:
		return Result{}, usagef("mouse %q: want move, down, up or wheel", op)
	}
	w, err := openPage(ctx, env, "mouse")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	r, err := w.run(ctx, action{Name: "mouse " + op, Kind: "mouse", Steps: steps, Args: func(int) map[string]any { return fields }})
	if err != nil {
		return Result{}, err
	}
	return w.result(fmt.Sprintf("mouse %s: pointer at %s", op, r.Value), r), nil
}

func runHighlight(ctx context.Context, env Env, in Input) (Result, error) {
	t, n := arg(in, 0), flagInt(in, "node-id", 0)
	if (t == "") == (n == 0) {
		return Result{}, usagef("highlight: give a target or --node-id, exactly one")
	}
	w, err := openPage(ctx, env, "highlight")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	if n == 0 {
		spec, err := w.spec(ctx, t)
		if err != nil {
			return Result{}, err
		}
		if n, err = w.nodeOf(ctx, spec); err != nil {
			return Result{}, err
		}
	} else {
		t = fmt.Sprintf("node %d", n)
	}
	cfg := map[string]any{
		"showInfo": true, "contentColor": map[string]any{"r": 111, "g": 168, "b": 220, "a": 0.66},
		"borderColor": map[string]any{"r": 229, "g": 25, "b": 126, "a": 1},
	}
	// n is valid only on the target that reported it, so the send goes
	// through the operation's own caller with its execution context's
	// target fence, also inside the change (as forceHover). The hide entry
	// is recorded before the send, so a highlight whose reply is lost after
	// the write is still hidden at the release. Only a failure that proves
	// the call had no effect (core.NoEffect) removes it, and only when no
	// earlier highlight of the session recorded it: that one still shows,
	// and the entry (the same key and the same DOM.hideHighlight) hides it.
	key := "w3b:highlight|" + w.session
	highlight := func(webkit.Caller) error {
		earlier := false
		if w.ps.State != nil {
			earlier = w.ps.State.HasEntry(key)
			w.ps.State.Put(core.Entry{
				Key: key, Owner: core.SessionOwner("w3b-highlight", w.session),
				Release: func(ctx context.Context, s core.Sender) error {
					_, err := s.Send(ctx, "DOM.hideHighlight", nil)
					return err
				},
			})
		}
		if _, err := w.caller.Send(ctx, "DOM.highlightNode", map[string]any{"nodeId": n, "highlightConfig": cfg}); err != nil {
			if w.ps.State != nil && !earlier && core.NoEffect(err) {
				w.ps.State.Remove(key)
			}
			return fmt.Errorf("highlight: %w", err)
		}
		return nil
	}
	if w.ps.State != nil {
		// One change, as forceHover records its entry.
		err = w.ps.State.Change(ctx, w.caller, highlight)
	} else {
		err = highlight(w.caller)
	}
	if err != nil {
		return Result{}, err
	}
	return Result{Text: "highlighted " + t + " on the device (DOM.highlightNode)", Data: map[string]any{"nodeId": n}}, nil
}
