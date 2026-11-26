package commands

import (
	"strings"
	"testing"
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
