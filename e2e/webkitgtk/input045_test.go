//go:build webkitgtk

package webkitgtk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// parity-045 (agent-browser 0.45 #2594 and #2734): the event properties
// that a page listener of input045.html records for click and dblclick
// with --button and --modifiers, and for keydown, keyup, press and mouse
// with held modifiers. Expected values come from the contract
// (agent-browser-045-contract.md items 1 and 2) and its e2e tests.

func loadInput045(t *testing.T) {
	t.Helper()
	navigate(t, "input045.html", "!!window.L")
}

// takeLog returns the page's log name (L or M) and empties it.
func takeLog(t *testing.T, name string) []string {
	t.Helper()
	var l []string
	pageJSON(t, fmt.Sprintf("(function () { var l = %s; window.%s = []; return l; })()", name, name), &l)
	return l
}

func wantLog(t *testing.T, what, name string, want ...string) {
	t.Helper()
	got := takeLog(t, name)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("%s: the page logged %s\nwant %s", what, g, w)
	}
}

// Contract item 1: click and dblclick --button and --modifiers set
// button, buttons, detail and the modifier flags of every event; right
// ends with contextmenu, the other non-left buttons with auxclick;
// dblclick is two clicks (detail 1, 2) and dblclick.
func TestInput045ClickButtonsAndModifiers(t *testing.T) {
	loadInput045(t)
	ps, _ := actionSession(t, hooks{})
	mustAct(t, ps, "click", "#b", "--modifiers", "shift")
	wantLog(t, "click --modifiers shift", "L", "mousedown:b0:bs1:d1:c0s1a0m0", "mouseup:b0:bs0:d1:c0s1a0m0", "click:b0:bs0:d1:c0s1a0m0")
	mustAct(t, ps, "click", "--modifiers", "Control+meta,alt", "#b")
	wantLog(t, "click --modifiers Control+meta,alt", "L", "mousedown:b0:bs1:d1:c1s0a1m1", "mouseup:b0:bs0:d1:c1s0a1m1", "click:b0:bs0:d1:c1s0a1m1")
	mustAct(t, ps, "click", "#b", "--button", "right")
	wantLog(t, "click --button right", "L", "mousedown:b2:bs2:d1:c0s0a0m0", "mouseup:b2:bs0:d1:c0s0a0m0", "contextmenu:b2:bs0:d1:c0s0a0m0")
	mustAct(t, ps, "click", "#b", "--button", "middle")
	wantLog(t, "click --button middle", "L", "mousedown:b1:bs4:d1:c0s0a0m0", "mouseup:b1:bs0:d1:c0s0a0m0", "auxclick:b1:bs0:d1:c0s0a0m0")
	mustAct(t, ps, "click", "#b", "--button", "back")
	wantLog(t, "click --button back", "L", "mousedown:b3:bs8:d1:c0s0a0m0", "mouseup:b3:bs0:d1:c0s0a0m0", "auxclick:b3:bs0:d1:c0s0a0m0")
	mustAct(t, ps, "click", "#b", "--button", "forward")
	wantLog(t, "click --button forward", "L", "mousedown:b4:bs16:d1:c0s0a0m0", "mouseup:b4:bs0:d1:c0s0a0m0", "auxclick:b4:bs0:d1:c0s0a0m0")
	mustAct(t, ps, "dblclick", "#b")
	wantLog(t, "dblclick", "L",
		"mousedown:b0:bs1:d1:c0s0a0m0", "mouseup:b0:bs0:d1:c0s0a0m0", "click:b0:bs0:d1:c0s0a0m0",
		"mousedown:b0:bs1:d2:c0s0a0m0", "mouseup:b0:bs0:d2:c0s0a0m0", "click:b0:bs0:d2:c0s0a0m0", "dblclick:b0:bs0:d2:c0s0a0m0")
	mustAct(t, ps, "dblclick", "#b", "--modifiers", "meta+shift")
	wantLog(t, "dblclick --modifiers meta+shift", "L",
		"mousedown:b0:bs1:d1:c0s1a0m1", "mouseup:b0:bs0:d1:c0s1a0m1", "click:b0:bs0:d1:c0s1a0m1",
		"mousedown:b0:bs1:d2:c0s1a0m1", "mouseup:b0:bs0:d2:c0s1a0m1", "click:b0:bs0:d2:c0s1a0m1", "dblclick:b0:bs0:d2:c0s1a0m1")
	// Chrome fires dblclick for the left button only.
	mustAct(t, ps, "dblclick", "#b", "--button", "right")
	wantLog(t, "dblclick --button right", "L",
		"mousedown:b2:bs2:d1:c0s0a0m0", "mouseup:b2:bs0:d1:c0s0a0m0", "contextmenu:b2:bs0:d1:c0s0a0m0",
		"mousedown:b2:bs2:d2:c0s0a0m0", "mouseup:b2:bs0:d2:c0s0a0m0", "contextmenu:b2:bs0:d2:c0s0a0m0")
}

// Contract item 1: bad --button and --modifiers values and --new-tab with
// either fail as usage errors with agent-browser's messages, and send no
// event; dblclick has no --new-tab.
func TestInput045ClickUsageErrors(t *testing.T) {
	loadInput045(t)
	ps, _ := actionSession(t, hooks{})
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"click", "#b", "--button", "bogus"}, "Unknown mouse button 'bogus'. Use left, right, middle, back, or forward"},
		{[]string{"click", "#b", "--button", "Left"}, "Unknown mouse button 'Left'"},
		{[]string{"dblclick", "#b", "--modifiers", "super"}, "Unknown modifier 'super'. Use alt, control, meta, or shift"},
		{[]string{"click", "#b", "--modifiers", "shift,"}, "Unknown modifier ''"},
		{[]string{"click", "#b", "--new-tab", "--button", "left"}, "--new-tab opens the link URL directly and cannot be combined with --button or --modifiers"},
		{[]string{"click", "#b", "--modifiers", "shift", "--new-tab"}, "--new-tab opens the link URL directly and cannot be combined with --button or --modifiers"},
		{[]string{"dblclick", "#b", "--new-tab"}, "unknown flag --new-tab"},
		{[]string{"click", "#b", "--button"}, "--button"},
	} {
		code, _, errOut := act(context.Background(), ps, c.args...)
		if code != 2 || !strings.Contains(errOut, c.want) {
			t.Fatalf("%s: exit %d, stderr %q; want exit 2 with %q", strings.Join(c.args, " "), code, errOut, c.want)
		}
	}
	wantLog(t, "the refused clicks", "L")
}

// Contract item 2: keydown and keyup send key, code, keyCode and location;
// a held Shift maps a later press to the US shifted key and text; press
// with a chord sends the modifier key events (Control+a inserts nothing,
// Shift+1 types "!"); keydown of a printable key also sends keypress and
// inserts its text (agent-browser e2e_keydown_keyup_send_code_and_key_code).
func TestInput045KeyboardModifiers(t *testing.T) {
	loadInput045(t)
	ps, _ := actionSession(t, hooks{})
	mustAct(t, ps, "focus", "#k")
	mustAct(t, ps, "keydown", "Shift")
	mustAct(t, ps, "press", "a")
	mustAct(t, ps, "keyup", "Shift")
	wantLog(t, "keydown Shift, press a, keyup Shift", "L",
		"keydown Shift ShiftLeft 16 1 c0s1a0m0",
		"keydown A KeyA 65 0 c0s1a0m0", "keypress A KeyA 65 0 c0s1a0m0", "input A", "keyup A KeyA 65 0 c0s1a0m0",
		"keyup Shift ShiftLeft 16 1 c0s0a0m0")
	mustAct(t, ps, "press", "Control+a")
	wantLog(t, "press Control+a", "L",
		"keydown Control ControlLeft 17 1 c1s0a0m0", "keydown a KeyA 65 0 c1s0a0m0", "keyup a KeyA 65 0 c1s0a0m0",
		"keyup Control ControlLeft 17 1 c0s0a0m0")
	mustAct(t, ps, "press", "Shift+1")
	wantLog(t, "press Shift+1", "L",
		"keydown Shift ShiftLeft 16 1 c0s1a0m0",
		"keydown ! Digit1 49 0 c0s1a0m0", "keypress ! Digit1 33 0 c0s1a0m0", "input !", "keyup ! Digit1 49 0 c0s1a0m0",
		"keyup Shift ShiftLeft 16 1 c0s0a0m0")
	mustAct(t, ps, "press", "Alt+l")
	wantLog(t, "press Alt+l", "L",
		"keydown Alt AltLeft 18 1 c0s0a1m0", "keydown l KeyL 76 0 c0s0a1m0", "keyup l KeyL 76 0 c0s0a1m0",
		"keyup Alt AltLeft 18 1 c0s0a0m0")
	// A held modifier is not pressed again by a chord that names it.
	mustAct(t, ps, "keydown", "ShiftRight")
	mustAct(t, ps, "press", "Shift+b")
	mustAct(t, ps, "keyup", "ShiftRight")
	wantLog(t, "keydown ShiftRight, press Shift+b, keyup ShiftRight", "L",
		"keydown Shift ShiftRight 16 2 c0s1a0m0",
		"keydown B KeyB 66 0 c0s1a0m0", "keypress B KeyB 66 0 c0s1a0m0", "input B", "keyup B KeyB 66 0 c0s1a0m0",
		"keyup Shift ShiftRight 16 2 c0s0a0m0")
	mustAct(t, ps, "keydown", "w")
	mustAct(t, ps, "keyup", "w")
	wantLog(t, "keydown w, keyup w", "L", "keydown w KeyW 87 0 c0s0a0m0", "keypress w KeyW 119 0 c0s0a0m0", "input w", "keyup w KeyW 87 0 c0s0a0m0")
	if v := pageString(t, "document.getElementById('k').value"); v != "A!Bw" {
		t.Fatalf("the input holds %q, want A!Bw", v)
	}
	// A new owner (lease epoch) resets the held modifiers.
	mustAct(t, ps, "keydown", "Shift")
	ps2, _ := actionSession(t, hooks{})
	mustAct(t, ps2, "press", "c")
	if l := takeLog(t, "L"); len(l) < 2 || l[1] != "keydown c KeyC 67 0 c0s0a0m0" {
		t.Fatalf("press c after a new owner logged %q; want an unshifted keydown c", l)
	}
}

// Contract item 2: a held modifier reaches mouse down and up, not a
// selector click.
func TestInput045HeldModifierReachesMouse(t *testing.T) {
	loadInput045(t)
	ps, _ := actionSession(t, hooks{})
	mustAct(t, ps, "focus", "#k")
	mustAct(t, ps, "keydown", "Control")
	mustAct(t, ps, "mouse", "move", "100", "60")
	mustAct(t, ps, "mouse", "down")
	mustAct(t, ps, "mouse", "up")
	wantLog(t, "mouse down and up with Control held", "M", "mousedown:b0:bs1:d0:c1s0a0m0", "mouseup:b0:bs0:d0:c1s0a0m0")
	takeLog(t, "L")
	mustAct(t, ps, "click", "#b")
	wantLog(t, "click with Control held", "L", "mousedown:b0:bs1:d1:c0s0a0m0", "mouseup:b0:bs0:d1:c0s0a0m0", "click:b0:bs0:d1:c0s0a0m0")
	takeLog(t, "M")
	mustAct(t, ps, "keyup", "Control")
	mustAct(t, ps, "mouse", "down", "right")
	mustAct(t, ps, "mouse", "up", "right")
	wantLog(t, "mouse down and up right after keyup Control", "M", "mousedown:b2:bs2:d0:c0s0a0m0", "mouseup:b2:bs0:d0:c0s0a0m0")
}
