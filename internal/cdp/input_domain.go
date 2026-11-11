package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/runtime"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// The Input domain (design docs/design/agentic-parity.md 5.3, tier 1, and
// 3.5). Every event is synthetic (isTrusted false). Each CDP event is one
// step of the daemon's attempt loop (core.Op.RunSteps, design 2.6) with a
// W3b dispatcher of the runtime: press, inserttext and tapAt
// (55-actions.js, 60-keyboard.js) and the CDP mouse and touch dispatchers
// built from W3b's event primitives (65-cdp.js). The attachment keeps the
// mouse buttons it holds (inputState), so a release clicks only after a
// press of the same CDP session; the page keeps the press targets and the
// touch of each attachment apart (the step's a), and the attachment's
// release at detach drops them (ownInput).

func init() {
	register(
		Method{Name: "Input.dispatchMouseEvent", Levels: OnPage, Handle: inputMouse},
		Method{Name: "Input.dispatchKeyEvent", Levels: OnPage, Handle: inputKey},
		Method{Name: "Input.insertText", Levels: OnPage, Handle: inputInsertText},
		Method{Name: "Input.dispatchTouchEvent", Levels: OnPage, Handle: inputTouch},
		Method{Name: "Input.emulateTouchFromMouseEvent", Levels: OnPage, Handle: inputTouchFromMouse},
		Method{Name: "Input.synthesizeTapGesture", Levels: OnPage, Handle: inputTap},
	)
}

// inputState is the Input state of one CDP attachment: the mouse buttons
// it holds down (CDP's buttons bits) and whether a touch emulated from the
// mouse is active.
type inputState struct {
	mu       sync.Mutex
	buttons  int
	touching bool
}

// mouseButtons maps CDP's button names to the DOM button number and the
// buttons bit (UI Events: left 0/1, middle 1/4, right 2/2, back 3/8,
// forward 4/16).
var mouseButtons = map[string]struct{ n, bit int }{
	"": {0, 0}, "none": {0, 0}, "left": {0, 1}, "middle": {1, 4}, "right": {2, 2}, "back": {3, 8}, "forward": {4, 16},
}

var mouseOps = map[string]string{"mousePressed": "down", "mouseReleased": "up", "mouseMoved": "move", "mouseWheel": "wheel"}

type mouseParams struct {
	Type       string  `json:"type"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Modifiers  int     `json:"modifiers"`
	Button     string  `json:"button"`
	Buttons    *int    `json:"buttons"`
	ClickCount int     `json:"clickCount"`
	DeltaX     float64 `json:"deltaX"`
	DeltaY     float64 `json:"deltaY"`
}

func invalidParam(format string, a ...any) *Error {
	return &Error{Code: codeInvalidParams, Message: "Invalid parameters: " + fmt.Sprintf(format, a...)}
}

// mouseArgs returns the cdpMouse step of a mouse event and updates the
// session's buttons: a press adds its button, a release removes it and
// clicks (cl) only when the session held that button. buttons, when the
// client gives it, is the event's buttons value.
func (s *session) mouseArgs(p mouseParams) (map[string]any, error) {
	op, ok := mouseOps[p.Type]
	if !ok {
		return nil, invalidParam("unknown mouse event type %q", p.Type)
	}
	b, ok := mouseButtons[p.Button]
	if !ok {
		return nil, invalidParam("unknown mouse button %q", p.Button)
	}
	st := &s.input
	st.mu.Lock()
	clicks := false
	switch op {
	case "down":
		st.buttons |= b.bit
	case "up":
		clicks = b.bit != 0 && st.buttons&b.bit != 0
		st.buttons &^= b.bit
	}
	bs := st.buttons
	st.mu.Unlock()
	if p.Buttons != nil {
		bs = *p.Buttons
	}
	return map[string]any{
		"k": "cdpMouse", "a": s.id, "op": op, "x": p.X, "y": p.Y, "b": b.n, "bs": bs, "n": p.ClickCount,
		"m": p.Modifiers, "cl": clicks, "dx": p.DeltaX, "dy": p.DeltaY,
	}, nil
}

func inputMouse(ctx context.Context, call *Call) (any, Later, error) {
	var p mouseParams
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	args, err := call.session.mouseArgs(p)
	if err != nil {
		return nil, nil, err
	}
	return call.session.dispatch(ctx, call.Method, args["op"] == "down", 1, func(int) map[string]any { return args })
}

// keyParams are CDP's Input.dispatchKeyEvent params that the page uses.
type keyParams struct {
	Type                  string `json:"type"`
	Modifiers             int    `json:"modifiers"`
	Text                  string `json:"text"`
	KeyIdentifier         string `json:"keyIdentifier"`
	Code                  string `json:"code"`
	Key                   string `json:"key"`
	WindowsVirtualKeyCode int    `json:"windowsVirtualKeyCode"`
	NativeVirtualKeyCode  int    `json:"nativeVirtualKeyCode"`
}

// keyPhases maps CDP key event types to the phases of 60-keyboard.js key:
// keyDown and rawKeyDown run everything before keyup (rawKeyDown inserts
// no text), keyUp the keyup, char the keypress and the insertion.
var keyPhases = map[string]string{"keyDown": "kd", "rawKeyDown": "kd", "keyUp": "up", "char": "ch"}

// keyArgs returns the press step of a key event: the key as 60-keyboard.js
// takes it (key, code, kc keyCode, cc charCode, ki keyIdentifier, text)
// and the modifiers (CDP bits: Alt 1, Ctrl 2, Meta 4, Shift 8). A control
// character in text (Enter's "\r") inserts nothing: the key's emulated
// default action handles it (design 3.5).
func keyArgs(p keyParams) (map[string]any, error) {
	ph, ok := keyPhases[p.Type]
	if !ok {
		return nil, invalidParam("unknown key event type %q", p.Type)
	}
	key := p.Key
	if key == "" {
		key = p.Text
	}
	text := p.Text
	if p.Type == "rawKeyDown" || strings.IndexFunc(text, unicode.IsControl) >= 0 {
		text = ""
	}
	cc := 0
	if r, _ := utf8.DecodeRuneInString(p.Text); r != utf8.RuneError {
		cc = int(r)
	}
	kc := p.WindowsVirtualKeyCode
	if kc == 0 {
		kc = p.NativeVirtualKeyCode
	}
	ki := p.KeyIdentifier
	if ki == "" {
		if r, n := utf8.DecodeRuneInString(key); n == len(key) && r != utf8.RuneError {
			ki = fmt.Sprintf("U+%04X", unicode.ToUpper(r))
		} else {
			ki = key
		}
	}
	m := p.Modifiers
	return map[string]any{
		"k": "press", "ph": ph,
		"keys": []map[string]any{{"key": key, "code": p.Code, "kc": kc, "cc": cc, "ki": ki, "text": text}},
		"m":    map[string]bool{"a": m&1 != 0, "c": m&2 != 0, "m": m&4 != 0, "s": m&8 != 0},
	}, nil
}

func inputKey(ctx context.Context, call *Call) (any, Later, error) {
	var p keyParams
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	args, err := keyArgs(p)
	if err != nil {
		return nil, nil, err
	}
	return call.session.dispatch(ctx, call.Method, false, 1, func(int) map[string]any { return args })
}

// inputInsertText inserts text at the focused element's selection
// (keyboard inserttext: beforeinput, insertion, input; no key events).
func inputInsertText(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Text string `json:"text"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	return call.session.dispatch(ctx, call.Method, false, 1, func(int) map[string]any {
		return map[string]any{"k": "inserttext", "text": p.Text}
	})
}

var touchOps = map[string]string{"touchStart": "start", "touchMove": "move", "touchEnd": "end", "touchCancel": "cancel"}

type touchPoint struct {
	X  float64  `json:"x"`
	Y  float64  `json:"y"`
	ID *float64 `json:"id,omitempty"`
}

// inputTouch dispatches one touch event (65-cdp.js cdpTouch).
func inputTouch(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		Type        string       `json:"type"`
		TouchPoints []touchPoint `json:"touchPoints"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	op, ok := touchOps[p.Type]
	if !ok {
		return nil, nil, invalidParam("unknown touch event type %q", p.Type)
	}
	if (op == "start" || op == "move") && len(p.TouchPoints) == 0 {
		return nil, nil, invalidParam("%s needs at least one touch point", p.Type)
	}
	if p.TouchPoints == nil {
		p.TouchPoints = []touchPoint{}
	}
	return call.session.dispatch(ctx, call.Method, op == "start", 1, func(int) map[string]any {
		return map[string]any{"k": "cdpTouch", "a": call.session.id, "op": op, "p": p.TouchPoints}
	})
}

// inputTouchFromMouse turns a mouse event into touch: a press starts a
// touch at the point, a move with the touch active moves it, a release
// ends it (a tap when it did not move), and a wheel scrolls as the mouse
// wheel does. A move without an active touch does nothing.
func inputTouchFromMouse(ctx context.Context, call *Call) (any, Later, error) {
	var p mouseParams
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	s := call.session
	if p.Type == "mouseWheel" {
		args, err := s.mouseArgs(p)
		if err != nil {
			return nil, nil, err
		}
		return s.dispatch(ctx, call.Method, false, 1, func(int) map[string]any { return args })
	}
	if _, ok := mouseOps[p.Type]; !ok {
		return nil, nil, invalidParam("unknown mouse event type %q", p.Type)
	}
	if _, ok := mouseButtons[p.Button]; !ok {
		return nil, nil, invalidParam("unknown mouse button %q", p.Button)
	}
	st := &s.input
	st.mu.Lock()
	op := ""
	switch {
	case p.Type == "mousePressed":
		op, st.touching = "start", true
	case p.Type == "mouseMoved" && st.touching:
		op = "move"
	case p.Type == "mouseReleased" && st.touching:
		op, st.touching = "end", false
	}
	st.mu.Unlock()
	if op == "" {
		return map[string]any{}, nil, nil
	}
	pts := []touchPoint{{X: p.X, Y: p.Y}}
	if op == "end" {
		pts = []touchPoint{}
	}
	return s.dispatch(ctx, call.Method, op == "start", 1, func(int) map[string]any {
		return map[string]any{"k": "cdpTouch", "a": s.id, "op": op, "p": pts}
	})
}

// inputTap taps tapCount times at a client point (55-actions.js tapAt:
// touch events, then the click sequence with pointerType touch).
// duration and gestureSourceType are ignored.
func inputTap(ctx context.Context, call *Call) (any, Later, error) {
	var p struct {
		X        float64 `json:"x"`
		Y        float64 `json:"y"`
		TapCount int     `json:"tapCount"`
	}
	if err := call.decode(&p); err != nil {
		return nil, nil, err
	}
	return call.session.dispatch(ctx, call.Method, false, max(1, p.TapCount), func(step int) map[string]any {
		return map[string]any{"k": "tapAt", "x": p.X, "y": p.Y, "n": step}
	})
}

// dispatch runs an Input event as steps of the attempt loop in one
// mutating operation; the result is empty. presses marks an event that
// leaves page-side state of the attachment (a mouse press, a touchstart):
// ownInput runs first.
func (s *session) dispatch(ctx context.Context, name string, presses bool, steps int, args func(step int) map[string]any) (any, Later, error) {
	err := s.withPage(ctx, name, core.ClassMutate, func(p *pageCall) error {
		if presses {
			if err := p.ownInput(); err != nil {
				return err
			}
		}
		_, err := p.act(name, steps, args)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return map[string]any{}, nil, nil
}

// jsInputDrop drops the press targets and the touch of the CDP session
// %s (a JSON string) in the current document (65-cdp.js cdpDrop); a
// document without the runtime has none.
const jsInputDrop = `(function(I){if(I&&I._&&I._.cdpDrop){I._.cdpDrop(%s)}return true})(window.__iwdp)`

// ownInput records once, as state of the attachment's desired-state owner,
// the entry whose release drops the attachment's page-side press targets
// and touch (jsInputDrop), so a detach removes them (design 5.2: a detach
// removes the attachment's state). The entry lives as long as the owner,
// across reconnections: it is not Scoped, because a same-document
// recovery keeps the page runtime and its press and touch maps (a Scoped
// entry would be dropped on the new connection, review finding F7). Its
// Apply does nothing, so a re-apply to a new connection or target sends
// nothing; a release in a new document finds empty maps, and cdpDrop
// there is harmless.
func (p *pageCall) ownInput() error {
	ps, s := p.s.pg.PS, p.s
	key := "cdp:input:" + string(s.owner)
	if ps.State.HasEntry(key) {
		return nil
	}
	id, _ := json.Marshal(s.id) // a string always encodes
	drop := fmt.Sprintf(jsInputDrop, id)
	return ps.State.Change(p.ctx, p.mo.Op.Caller(), func(webkit.Caller) error {
		ps.State.Put(core.Entry{
			Key: key, Owner: s.owner, Apply: func(context.Context, core.Sender) error { return nil },
			Release: func(ctx context.Context, snd core.Sender) error {
				if _, err := snd.Send(ctx, "Runtime.evaluate", map[string]any{"expression": drop, "returnByValue": true}); err != nil {
					return fmt.Errorf("dropping the press state of CDP session %s in the current document: %w", shortID(s.id), err)
				}
				return nil
			},
		})
		return nil
	})
}

// Dialogs is the dialog bridge of design 3.7 for CDP Input (5.2
// "Dialogs"). An Input event runs through it as a W3b action of the
// logical session does (internal/commands w3bPage.run), so a dialog that
// the event opens gets the session's armed answer or its policy instead of
// blocking the page.
type Dialogs interface {
	// Before prepares the bridge for page work that can open a dialog (it
	// activates the session's override and issues its queued answers) and
	// returns the policy that each step carries (act's dp).
	Before(ctx context.Context, p DialogPage) (policy string, err error)
	// After records the dialog uses that the steps reported (act's dlg).
	After(p DialogPage, uses []DialogUse)
}

// DialogPage is the page work that Dialogs serves: the page session, the
// logical session and its lease epoch, and the operation's fenced caller
// and runtime.
type DialogPage struct {
	PS      *core.PageSession
	Session string
	Epoch   uint64
	Caller  webkit.Caller
	Runtime *runtime.Runtime
}

// DialogUse is one dialog that the page answered (80-dialogs.js handle):
// ID is the armed answer it used, "" when the session policy answered.
type DialogUse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message"`
	Result  any    `json:"result"`
}

// actStep is the value of one dispatched step (50-actionability.js act):
// emulated default actions, a failure after dispatch, a warning, a value.
type actStep struct {
	D   []string        `json:"d"`
	Err string          `json:"err"`
	Wrn string          `json:"warn"`
	V   json.RawMessage `json:"v"`
	Dlg []DialogUse     `json:"dlg"`
}

// actReplyReserve: the attempts end this long before the command deadline
// (core.Action.Reserve), so the last attempt's reply arrives inside it;
// internal/commands uses the same reserve (actionReplyReserve).
const actReplyReserve = 250 * time.Millisecond

// failedPrefix is how core.RunSteps reports a page reply with status
// "failed": the act function refused its target before the fence, so
// nothing ran (internal/commands actionError reads it the same way).
const failedPrefix = "page answered \"failed\" "

// act runs steps of a runtime dispatcher in the attempt loop without
// actionability checks (f): CDP input goes where the client sends it. The
// session's dialog bridge (Options.Dialogs) is prepared first, each step
// carries its policy, and the dialogs that the steps answered go back to
// it, as for a W3b action (internal/commands w3bPage.run).
func (p *pageCall) act(name string, steps int, args func(step int) map[string]any) ([]actStep, error) {
	reserve := actReplyReserve
	if d, ok := p.ctx.Deadline(); ok {
		reserve = min(reserve, time.Until(d)/4)
	}
	dl := p.s.c.s.o.Dialogs
	dp := DialogPage{PS: p.s.pg.PS, Session: p.s.c.rec.session, Epoch: p.s.epoch, Caller: p.mo.Op.Caller(), Runtime: p.rt}
	policy := ""
	if dl != nil {
		var err error
		if policy, err = dl.Before(p.ctx, dp); err != nil {
			return nil, err
		}
	}
	res, err := p.mo.Op.RunSteps(core.Action{Name: name, Fn: "act", Steps: steps, Reserve: reserve, Args: func(step int) any {
		m := args(step)
		m["f"] = true
		if policy != "" {
			m["dp"] = policy
		}
		return m
	}})
	var out []actStep
	var uses []DialogUse
	defer func() {
		if dl != nil {
			dl.After(dp, uses)
		}
	}()
	for _, raw := range res.Values {
		if len(raw) == 0 || string(raw) == "null" {
			continue // a step without a value (no default action, no failure)
		}
		var v actStep
		if derr := json.Unmarshal(raw, &v); derr != nil {
			return out, fmt.Errorf("%s: decoding the page's step result: %w", name, derr)
		}
		out = append(out, v)
		uses = append(uses, v.Dlg...)
		if v.Err != "" && err == nil {
			err = serverError("%s: %s", name, v.Err)
		}
	}
	var se *core.StepError
	if errors.As(err, &se) && strings.HasPrefix(se.Reason, failedPrefix) && !se.MayHaveRun {
		code, msg, _ := strings.Cut(strings.TrimPrefix(se.Reason, failedPrefix), ": ")
		if code == "no_node" {
			return out, errNoNode
		}
		return out, &runtime.Error{Code: code, Message: msg}
	}
	return out, err
}
