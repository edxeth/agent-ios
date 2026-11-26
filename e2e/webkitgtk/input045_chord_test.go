//go:build webkitgtk

package webkitgtk

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

// parity-045 review F4: modifier parts anywhere in a chord, the Left and
// Right forms too (agent-browser 0.45 parse_key_chord), press the key
// with Shift; the chord presses the modifier key named by its bit, as
// agent-browser's modifier_key_names does (Shift, code ShiftLeft).
func TestInput045ChordModifierAnywhere(t *testing.T) {
	loadInput045(t)
	ps, _ := actionSession(t, hooks{})
	mustAct(t, ps, "focus", "#k")
	for _, chord := range []string{"ShiftRight+a", "a+Shift"} {
		mustAct(t, ps, "press", chord)
		wantLog(t, "press "+chord, "L",
			"keydown Shift ShiftLeft 16 1 c0s1a0m0",
			"keydown A KeyA 65 0 c0s1a0m0", "keypress A KeyA 65 0 c0s1a0m0", "input A", "keyup A KeyA 65 0 c0s1a0m0",
			"keyup Shift ShiftLeft 16 1 c0s0a0m0")
	}
	if v := pageString(t, "document.getElementById('k').value"); v != "AA" {
		t.Fatalf("the input holds %q, want AA", v)
	}
}

// parity-045 review F5: each key event of a chord goes to the element
// focused when it is sent, as WebKit sends keyup to the focused element:
// after Shift+Tab moved focus from #k2 to #k, #k gets the keyup of Tab and
// of Shift. The keypress, the insertion and the default action stay with
// the keydown's target, and Space activates a button only when its keyup
// reaches the same button: a keydown listener of #b that focuses #k
// leaves #b unclicked and #k unchanged.
func TestInput045ChordFollowsFocus(t *testing.T) {
	loadInput045(t)
	eval(t, `(function () {
		document.body.insertAdjacentHTML("beforeend", '<input id="k2">');
		window.K2 = [];
		var k2 = document.getElementById("k2");
		["keydown", "keypress", "keyup"].forEach(function (type) {
			k2.addEventListener(type, function (e) { K2.push([type, e.key, e.code, e.keyCode, e.location, mods(e)].join(" ")); });
		});
		return true;
	})()`)
	ps, _ := actionSession(t, hooks{})
	mustAct(t, ps, "focus", "#k2")
	mustAct(t, ps, "press", "Shift+Tab")
	if id := pageString(t, "document.activeElement.id"); id != "k" {
		t.Fatalf("Shift+Tab from #k2 focused %q, want k", id)
	}
	wantLog(t, "press Shift+Tab on #k2", "K2", "keydown Shift ShiftLeft 16 1 c0s1a0m0", "keydown Tab Tab 9 0 c0s1a0m0")
	wantLog(t, "press Shift+Tab, then on #k", "L", "keyup Tab Tab 9 0 c0s1a0m0", "keyup Shift ShiftLeft 16 1 c0s0a0m0")

	eval(t, `document.getElementById("b").addEventListener("keydown", function () { document.getElementById("k").focus(); }); true`)
	mustAct(t, ps, "focus", "#b")
	mustAct(t, ps, "press", "Space")
	wantLog(t, "press Space on #b whose keydown focuses #k", "L", "keyup   Space 32 0 c0s0a0m0")
	if v := pageString(t, "document.getElementById('k').value"); v != "" {
		t.Fatalf("#k holds %q after the Space of #b, want nothing", v)
	}
}

// heldOp is one step of the held-modifier model test.
type heldOp struct {
	kind  string // keydown, keyup, press or owner (a new owner)
	key   string // keydown, keyup: a modifier key or q; press: q
	chord int    // press: the chord's modifier bits
}

func (o heldOp) String() string {
	if o.kind == "press" {
		return fmt.Sprintf("press %s", chordName(o.chord, o.key))
	}
	return o.kind + " " + o.key
}

// modBits are the bits of the modifier keys (Alt 1, Control 2, Meta 4,
// Shift 8) and modOrder the order in which press sends chord modifiers
// (agent-browser 0.45 press_key_with_chord_and_held).
var (
	modBits  = map[string]int{"Shift": 8, "ShiftLeft": 8, "ShiftRight": 8, "Control": 2, "ControlRight": 2, "Alt": 1, "AltLeft": 1, "Meta": 4, "MetaRight": 4}
	modNames = map[int]string{1: "Alt", 2: "Control", 4: "Meta", 8: "Shift"}
	modOrder = []int{2, 1, 8, 4}
)

func chordName(chord int, key string) string {
	s := ""
	for _, b := range modOrder {
		if chord&b != 0 {
			s += modNames[b] + "+"
		}
	}
	return s + key
}

func modFlags(m int) string {
	f := func(b int) int {
		if m&b != 0 {
			return 1
		}
		return 0
	}
	return fmt.Sprintf("c%ds%da%dm%d", f(2), f(8), f(1), f(4))
}

// heldModel is the reference model of the held modifiers (contract item 2):
// keydown of a modifier key holds its bit and its event carries the held
// bits with its own; keyup releases it and carries the rest; press sends
// the chord modifiers not held, in modOrder, the key with every bit, and
// releases them in reverse; Shift makes q a Q; a new owner holds nothing.
// step returns the keydown and keyup events "type key flags" of #k.
type heldModel struct{ held int }

func (h *heldModel) step(o heldOp) []string {
	name := func(k string, m int) string {
		if b, ok := modBits[k]; ok {
			return modNames[b]
		}
		if m&8 != 0 {
			return strings.ToUpper(k)
		}
		return k
	}
	switch o.kind {
	case "owner":
		h.held = 0
		return nil
	case "keydown":
		m := h.held | modBits[o.key]
		h.held = m
		return []string{"keydown " + name(o.key, m) + " " + modFlags(m)}
	case "keyup":
		h.held &^= modBits[o.key]
		return []string{"keyup " + name(o.key, h.held) + " " + modFlags(h.held)}
	}
	var out []string
	cur, pressed := h.held, []int{}
	for _, b := range modOrder {
		if o.chord&b != 0 && h.held&b == 0 {
			cur |= b
			pressed = append(pressed, b)
			out = append(out, "keydown "+modNames[b]+" "+modFlags(cur))
		}
	}
	out = append(out, "keydown "+name(o.key, cur)+" "+modFlags(cur), "keyup "+name(o.key, cur)+" "+modFlags(cur))
	for i := len(pressed) - 1; i >= 0; i-- {
		cur &^= pressed[i]
		out = append(out, "keyup "+modNames[pressed[i]]+" "+modFlags(cur))
	}
	return out
}

// parity-045 review F7: the page's held-modifier state (60-keyboard.js H)
// against heldModel over generated sequences of keydown, keyup and press
// commands and owner changes (which reset it): after every command, the
// keydown and keyup events of #k carry the model's modifier flags and key.
func TestInput045HeldModifierModel(t *testing.T) {
	loadInput045(t)
	keys := []string{"Shift", "ShiftLeft", "ShiftRight", "Control", "ControlRight", "Alt", "AltLeft", "Meta", "MetaRight", "q"}
	cfg := &quick.Config{MaxCount: 15, Values: func(v []reflect.Value, r *rand.Rand) {
		ops := make([]heldOp, 3+r.Intn(8))
		for i := range ops {
			switch n := r.Intn(10); {
			case n < 4:
				ops[i] = heldOp{kind: "keydown", key: keys[r.Intn(len(keys))]}
			case n < 7:
				ops[i] = heldOp{kind: "keyup", key: keys[r.Intn(len(keys))]}
			case n < 9:
				ops[i] = heldOp{kind: "press", key: "q", chord: r.Intn(16)}
			default:
				ops[i] = heldOp{kind: "owner"}
			}
		}
		v[0] = reflect.ValueOf(ops)
	}}
	f := func(ops []heldOp) bool {
		ps, _ := actionSession(t, hooks{}) // a new owner: nothing held
		mustAct(t, ps, "focus", "#k")
		takeLog(t, "L")
		var model heldModel
		for i, o := range ops {
			want := model.step(o)
			switch o.kind {
			case "owner":
				ps, _ = actionSession(t, hooks{})
				continue
			case "press":
				mustAct(t, ps, "press", chordName(o.chord, o.key))
			default:
				mustAct(t, ps, o.kind, o.key)
			}
			var got []string
			for _, l := range takeLog(t, "L") {
				if p := strings.Fields(l); len(p) == 6 && (p[0] == "keydown" || p[0] == "keyup") {
					got = append(got, p[0]+" "+p[1]+" "+p[5])
				}
			}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Logf("step %d of %v (%v): #k got %q, want %q", i, ops, o, got, want)
				return false
			}
		}
		return true
	}
	if err := quick.Check(f, cfg); err != nil {
		t.Fatal(err)
	}
}
