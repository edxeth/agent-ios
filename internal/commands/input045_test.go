package commands

import (
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"unicode"
)

// parity-045 contract item 1: click and dblclick --button and --modifiers
// values, and --new-tab with either, as agent-browser 0.45 parses them
// (commands.rs parse_click; pointer.rs masks Alt 1, Control 2, Meta 4,
// Shift 8).
func TestClickOptsParity045(t *testing.T) {
	for _, c := range []struct {
		line      []string
		btn, mods int
		err       string
	}{
		{[]string{"click", "#b"}, 0, 0, ""},
		{[]string{"click", "#b", "--button", "right"}, 2, 0, ""},
		{[]string{"click", "--button", "middle", "#b"}, 1, 0, ""},
		{[]string{"click", "#b", "--button", "back"}, 3, 0, ""},
		{[]string{"dblclick", "#b", "--button", "forward"}, 4, 0, ""},
		{[]string{"click", "#b", "--modifiers", "shift"}, 0, 8, ""},
		{[]string{"click", "#b", "--modifiers", " Ctrl + CMD , alt"}, 0, 7, ""},
		{[]string{"dblclick", "#b", "--modifiers", "control,command,meta,shift,alt"}, 0, 15, ""},
		{[]string{"click", "#b", "--button", "Right"}, 0, 0, "Unknown mouse button 'Right'. Use left, right, middle, back, or forward"},
		{[]string{"click", "#b", "--button", ""}, 0, 0, "Unknown mouse button ''."},
		{[]string{"click", "#b", "--modifiers", "super"}, 0, 0, "Unknown modifier 'super'. Use alt, control, meta, or shift"},
		{[]string{"click", "#b", "--modifiers", "shift++alt"}, 0, 0, "Unknown modifier ''."},
		{[]string{"click", "#b", "--modifiers", ""}, 0, 0, "Unknown modifier ''."},
		{[]string{"click", "#b", "--new-tab", "--button", "left"}, 0, 0, "--new-tab opens the link URL directly and cannot be combined with --button or --modifiers"},
		{[]string{"click", "#b", "--modifiers", "shift", "--new-tab"}, 0, 0, "--new-tab opens the link URL directly and cannot be combined with --button or --modifiers"},
	} {
		inv, err := Parse(c.line, noEnv)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.line, err)
		}
		btn, mods, err := clickOpts(inv.Input)
		if c.err != "" {
			if _, ok := err.(*UsageError); !ok || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("%q: error %v, want a usage error with %q", c.line, err, c.err)
			}
			continue
		}
		if err != nil || btn != c.btn || mods != c.mods {
			t.Fatalf("%q: button %d, modifiers %d, %v; want %d, %d", c.line, btn, mods, err, c.btn, c.mods)
		}
	}
	if _, err := Parse([]string{"dblclick", "#b", "--new-tab"}, noEnv); err == nil || !strings.Contains(err.Error(), "unknown flag --new-tab") {
		t.Fatalf("dblclick --new-tab: %v, want unknown flag --new-tab", err)
	}
}

// parity-045 contract item 2: the key, code, keyCode, location, modifier
// bit and US shifted key of the keys that press, keydown and keyup take
// (agent-browser 0.45 named_key_info, char_to_key_info, key_location,
// key_modifier_bit, shifted_char).
func TestParseKeyParity045(t *testing.T) {
	for in, want := range map[string]keyDesc{
		"Shift":        {Key: "Shift", Code: "ShiftLeft", KC: 16, Loc: 1, MB: 8},
		"shiftright":   {Key: "Shift", Code: "ShiftRight", KC: 16, Loc: 2, MB: 8},
		"ctrl":         {Key: "Control", Code: "ControlLeft", KC: 17, Loc: 1, MB: 2},
		"ControlRight": {Key: "Control", Code: "ControlRight", KC: 17, Loc: 2, MB: 2},
		"AltLeft":      {Key: "Alt", Code: "AltLeft", KC: 18, Loc: 1, MB: 1},
		"AltRight":     {Key: "Alt", Code: "AltRight", KC: 18, Loc: 2, MB: 1},
		"cmd":          {Key: "Meta", Code: "MetaLeft", KC: 91, Loc: 1, MB: 4},
		"command":      {Key: "Meta", Code: "MetaLeft", KC: 91, Loc: 1, MB: 4},
		"MetaRight":    {Key: "Meta", Code: "MetaRight", KC: 92, Loc: 2, MB: 4},
		"a":            {Key: "a", Code: "KeyA", KC: 65, Text: "a", SK: "A"},
		"KeyW":         {Key: "w", Code: "KeyW", KC: 87, Text: "w", SK: "W"},
		"digit1":       {Key: "1", Code: "Digit1", KC: 49, Text: "1", SK: "!"},
		"0":            {Key: "0", Code: "Digit0", KC: 48, Text: "0", SK: ")"},
		";":            {Key: ";", Code: "Semicolon", KC: 186, Text: ";", SK: ":"},
		":":            {Key: ":", Code: "Semicolon", KC: 186, Text: ":"},
		"/":            {Key: "/", Code: "Slash", KC: 191, Text: "/", SK: "?"},
		"`":            {Key: "`", Code: "Backquote", KC: 192, Text: "`", SK: "~"},
		"\\":           {Key: "\\", Code: "Backslash", KC: 220, Text: "\\", SK: "|"},
		"'":            {Key: "'", Code: "Quote", KC: 222, Text: "'", SK: "\""},
		"}":            {Key: "}", Code: "BracketRight", KC: 221, Text: "}"},
		"!":            {Key: "!", Text: "!"},
		"Space":        {Key: " ", Code: "Space", KC: 32, Text: " "},
	} {
		k, _, err := parseKey(in)
		if err != nil {
			t.Fatalf("parseKey(%q): %v", in, err)
		}
		got := keyDesc{Key: k.Key, Code: k.Code, KC: k.KC, Text: k.Text, Loc: k.Loc, MB: k.MB, SK: k.SK}
		if got != want {
			t.Fatalf("parseKey(%q) = %+v, want %+v", in, got, want)
		}
	}
	for in, want := range map[string]int{"Control+a": 2, "Shift+1": 8, "Alt+Shift+l": 9, "Meta+Control+Shift": 6, "Ctrl+Cmd+Alt+Shift+x": 15} {
		if _, m, err := parseKey(in); err != nil || m.mask() != want {
			t.Fatalf("parseKey(%q) modifiers %+v (mask %d), %v; want mask %d", in, m, m.mask(), err, want)
		}
	}
}

// parity-045 review F4 (agent-browser 0.45 parse_key_chord and
// key_modifier_bit, and its test_modifier_helpers): a chord part that
// names a modifier, the Left and Right modifier keys too, is a modifier
// wherever it is; the other parts are the key; a "+" key stays a key; a
// key that is not known is refused.
func TestParseKeyChordParity045(t *testing.T) {
	for in, want := range map[string]struct {
		key  string
		mask int
	}{
		"ShiftRight+a":             {"a", 8},
		"a+Shift":                  {"a", 8},
		"ShiftRight+KeyA":          {"a", 8},
		"ControlLeft+l":            {"l", 2},
		"shiftleft+b":              {"b", 8},
		"AltRight+x":               {"x", 1},
		"MetaLeft+x":               {"x", 4},
		"x+ControlRight+metaright": {"x", 6},
		"Tab+Shift":                {"Tab", 8},
		"Control++":                {"+", 2},
		"+":                        {"+", 0},
		"++Shift":                  {"+", 8},
		"Meta+Control+Shift":       {"Shift", 6},
		"ShiftLeft":                {"Shift", 0},
	} {
		k, m, err := parseKey(in)
		if err != nil || k.Key != want.key || m.mask() != want.mask {
			t.Errorf("parseKey(%q) = key %q, mask %d, %v; want key %q, mask %d", in, k.Key, m.mask(), err, want.key, want.mask)
		}
	}
	for _, in := range []string{"Hyper+a", "Shift+a+b", "Shift+", "Option+a", "ctrlleft+a"} {
		if _, _, err := parseKey(in); err == nil {
			t.Errorf("parseKey(%q) accepted; want a usage error", in)
		} else if _, ok := err.(*UsageError); !ok {
			t.Errorf("parseKey(%q): %v, want a usage error", in, err)
		}
	}
}

// chordModifierNames are the modifier parts of a chord with their bits
// (agent-browser 0.45 key_modifier_bit).
var chordModifierNames = map[string]int{
	"Alt": 1, "AltLeft": 1, "AltRight": 1, "Control": 2, "Ctrl": 2, "ControlLeft": 2, "ControlRight": 2,
	"Meta": 4, "Cmd": 4, "Command": 4, "MetaLeft": 4, "MetaRight": 4, "Shift": 8, "ShiftLeft": 8, "ShiftRight": 8,
}

// chordKeyNames are keys that name no modifier: printable characters, "+"
// among them, named keys, aliases and code names.
var chordKeyNames = []string{"a", "Z", "7", "+", "!", ";", " ", "Enter", "tab", "Escape", "ArrowLeft", "esc", "space", "KeyQ", "digit3", "PageDown"}

// randomCase changes the case of each letter of s at random (both parsers
// are case-insensitive).
func randomCase(r *rand.Rand, s string) string {
	b := []byte(s)
	for i := range b {
		if r.Intn(2) == 0 {
			b[i] = byte(unicode.ToUpper(rune(b[i])))
		} else {
			b[i] = byte(unicode.ToLower(rune(b[i])))
		}
	}
	return string(b)
}

// parity-045 review F7: the chord grammar against a model. A chord built
// from a key and modifier parts in any order and case presses that key
// with the union of the parts' bits; one part that is neither (a word
// that names no modifier) makes it a usage error.
func TestParseKeyChordProperty(t *testing.T) {
	names := make([]string, 0, len(chordModifierNames))
	for n := range chordModifierNames {
		names = append(names, n)
	}
	slices.Sort(names)
	type chord struct {
		parts []string
		key   string
		mask  int
		bad   bool
	}
	cfg := &quick.Config{MaxCount: 3000, Values: func(v []reflect.Value, r *rand.Rand) {
		var c chord
		for range r.Intn(5) {
			n := names[r.Intn(len(names))]
			c.parts = append(c.parts, randomCase(r, n))
			c.mask |= chordModifierNames[n]
		}
		c.key = chordKeyNames[r.Intn(len(chordKeyNames))]
		at := r.Intn(len(c.parts) + 1)
		c.parts = slices.Insert(c.parts, at, c.key)
		if c.mask != 0 && r.Intn(4) == 0 {
			c.bad = true
			c.parts = slices.Insert(c.parts, r.Intn(len(c.parts)+1), []string{"Hyper", "Super", "Fn", "Option", "Shft"}[r.Intn(5)])
		}
		v[0] = reflect.ValueOf(c)
	}}
	f := func(c chord) bool {
		in := strings.Join(c.parts, "+")
		k, m, err := parseKey(in)
		if c.bad {
			_, usage := err.(*UsageError)
			if !usage {
				t.Logf("parseKey(%q) = %+v, %v; want a usage error", in, k, err)
			}
			return usage
		}
		want, _, werr := parseKey(c.key)
		if werr != nil || err != nil || k != want || m.mask() != c.mask {
			t.Logf("parseKey(%q) = %+v mask %d, %v; want %+v mask %d (%v)", in, k, m.mask(), err, want, c.mask, werr)
			return false
		}
		return true
	}
	if err := quick.Check(f, cfg); err != nil {
		t.Fatal(err)
	}
}

// parity-045 review F7: click --modifiers against a model (agent-browser
// 0.45 pointer.rs parse_list). Names of alt, control, meta and shift and
// their aliases, in any case, padded, joined by "," or "+", give the union
// of their bits; one empty or unknown part is refused with agent-browser's
// message naming that part, trimmed.
func TestParseModifiersProperty(t *testing.T) {
	bits := map[string]int{"alt": 1, "control": 2, "ctrl": 2, "meta": 4, "cmd": 4, "command": 4, "shift": 8}
	names := slices.Sorted(maps.Keys(bits))
	type list struct {
		parts []string
		mask  int
		bad   string
		isBad bool
	}
	pad := func(r *rand.Rand) string { return []string{"", " ", "  ", "\t"}[r.Intn(4)] }
	cfg := &quick.Config{MaxCount: 3000, Values: func(v []reflect.Value, r *rand.Rand) {
		var l list
		for range 1 + r.Intn(5) {
			n := names[r.Intn(len(names))]
			l.parts = append(l.parts, pad(r)+randomCase(r, n)+pad(r))
			l.mask |= bits[n]
		}
		if r.Intn(3) == 0 {
			l.isBad = true
			l.bad = []string{"", "super", "ShiftLeft", "option", "hyper"}[r.Intn(5)]
			l.parts = slices.Insert(l.parts, r.Intn(len(l.parts)+1), pad(r)+l.bad+pad(r))
		}
		v[0] = reflect.ValueOf(l)
	}}
	f := func(l list) bool {
		var b strings.Builder
		for i, p := range l.parts {
			if i > 0 {
				b.WriteString([]string{",", "+"}[len(p)%2])
			}
			b.WriteString(p)
		}
		mask, err := parseModifiers(b.String())
		if l.isBad {
			want := "Unknown modifier '" + l.bad + "'. Use alt, control, meta, or shift"
			if _, usage := err.(*UsageError); !usage || err.Error() != want {
				t.Logf("parseModifiers(%q) = %d, %v; want %q", b.String(), mask, err, want)
				return false
			}
			return true
		}
		if err != nil || mask != l.mask {
			t.Logf("parseModifiers(%q) = %d, %v; want %d", b.String(), mask, err, l.mask)
			return false
		}
		return true
	}
	if err := quick.Check(f, cfg); err != nil {
		t.Fatal(err)
	}
}
