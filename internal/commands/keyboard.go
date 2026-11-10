package commands

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Keyboard and text commands of design 3.5 (W3b): fill, type, keyboard,
// press, keydown and keyup. Go turns keys into descriptors; the runtime
// (60-keyboard.js) sends the events and emulates default actions.

// typeChunk is how many characters one step of type without --delay
// carries; each step must fit its attempt window (design 2.6), and every
// key takes two task gaps on the page (js/60-keyboard.js).
const typeChunk = 8

// typeTimeout: type and keyboard can span many steps (--delay), so they
// keep the default command deadline instead of the 5 s action timeout.
const typeTimeout = 30 * time.Second

func init() {
	registerAction(&Command{
		Name:    "fill",
		Summary: "Replace an input's value: beforeinput (insertReplacementText), the native value setter, input and change.",
		Args:    []Arg{targetArg("input, textarea or contenteditable"), {Name: "text", Help: "the new value (or --stdin)"}},
		Flags: []Flag{
			{Name: "stdin", Type: FlagBool, Help: "read the value from the standard input (iwdp-cli only), so a secret is not in the command line; one trailing newline is removed"},
			forceFlag,
		},
		ClientStep: fillStdin,
		Run:        runFill,
	})
	// type has no "type-text" alias: the removed command of that name took
	// <text> [ws-url], which this command would read as <target> <text>.
	registerAction(&Command{
		Name:    "type",
		Summary: "Type text key by key (keydown, keypress, beforeinput, insertion, input, keyup) into an element, or with only <text> into the focused element.",
		Args: []Arg{
			{Name: "target", Help: "input, textarea or contenteditable: @eN ref, CSS selector, or text=, role=, label=, placeholder=, testid= locator; omit it to type into the focused element"},
			{Name: "text", Help: "the text; a newline is the Enter key", Required: true},
		},
		Flags:   []Flag{{Name: "delay", Type: FlagDuration, Help: "pause between characters; each character is then one step"}, forceFlag},
		Timeout: typeTimeout,
		Run:     runType,
	})
	registerAction(&Command{
		Name:    "keyboard",
		Summary: "Type into the focused element (keyboard type <text>) or insert text without key events (keyboard inserttext <text>).",
		Args:    []Arg{{Name: "mode", Help: "type or inserttext", Required: true}, {Name: "text", Help: "the text", Required: true}},
		Flags:   []Flag{{Name: "delay", Type: FlagDuration, Help: "type: pause between characters"}},
		Timeout: typeTimeout,
		Run:     runKeyboard,
	})
	registerAction(&Command{
		Name: "press", Aliases: []string{"key"},
		Summary: "Press a key or combination (Enter, Tab, Shift+Tab, Control+a) on the focused element, with its emulated default action.",
		Args:    []Arg{{Name: "key", Help: "key name or character, with optional Control+, Shift+, Alt+, Meta+ modifiers", Required: true}},
		Flags:   []Flag{{Name: "phase", Type: FlagEnum, Enum: []string{"both", "down", "up"}, Default: "both", Help: "down or up sends only keydown or keyup"}},
		Run:     runPress(""),
	})
	registerAction(&Command{
		Name: "keydown", Summary: "Send keydown for a key on the focused element (no default action).",
		Args: []Arg{{Name: "key", Help: "key name or character", Required: true}},
		Run:  runPress("down"),
	})
	registerAction(&Command{
		Name: "keyup", Summary: "Send keyup for a key on the focused element.",
		Args: []Arg{{Name: "key", Help: "key name or character", Required: true}},
		Run:  runPress("up"),
	})
}

// keyDesc is one key as 60-keyboard.js takes it: KeyboardEvent key and
// code, the legacy keyCode (kc), charCode of keypress (cc) and
// keyIdentifier (ki), and the text a printable key inserts.
type keyDesc struct {
	Key  string `json:"key"`
	Code string `json:"code,omitempty"`
	KC   int    `json:"kc"`
	CC   int    `json:"cc,omitempty"`
	KI   string `json:"ki,omitempty"`
	Text string `json:"text,omitempty"`
}

// namedKeys are the non-printable keys with their legacy codes (UI Events
// KeyboardEvent key and code values; keyCode and keyIdentifier as WebKit
// reports them).
var namedKeys = map[string]keyDesc{
	"Enter":      {Key: "Enter", Code: "Enter", KC: 13, CC: 13, KI: "Enter"},
	"Tab":        {Key: "Tab", Code: "Tab", KC: 9, KI: "U+0009"},
	"Backspace":  {Key: "Backspace", Code: "Backspace", KC: 8, KI: "U+0008"},
	"Delete":     {Key: "Delete", Code: "Delete", KC: 46, KI: "U+007F"},
	"Escape":     {Key: "Escape", Code: "Escape", KC: 27, KI: "U+001B"},
	"ArrowLeft":  {Key: "ArrowLeft", Code: "ArrowLeft", KC: 37, KI: "Left"},
	"ArrowUp":    {Key: "ArrowUp", Code: "ArrowUp", KC: 38, KI: "Up"},
	"ArrowRight": {Key: "ArrowRight", Code: "ArrowRight", KC: 39, KI: "Right"},
	"ArrowDown":  {Key: "ArrowDown", Code: "ArrowDown", KC: 40, KI: "Down"},
	"Home":       {Key: "Home", Code: "Home", KC: 36, KI: "Home"},
	"End":        {Key: "End", Code: "End", KC: 35, KI: "End"},
	"PageUp":     {Key: "PageUp", Code: "PageUp", KC: 33, KI: "PageUp"},
	"PageDown":   {Key: "PageDown", Code: "PageDown", KC: 34, KI: "PageDown"},
}

var keyAliases = map[string]string{
	"return": "Enter", "esc": "Escape", "left": "ArrowLeft", "right": "ArrowRight", "up": "ArrowUp", "down": "ArrowDown",
	"del": "Delete", "space": " ",
}

// charKey is the descriptor of a printable character; a newline is Enter.
func charKey(r rune) keyDesc {
	if r == '\n' || r == '\r' {
		return namedKeys["Enter"]
	}
	s := string(r)
	k := keyDesc{Key: s, Text: s, CC: int(r), KI: fmt.Sprintf("U+%04X", unicode.ToUpper(r))}
	switch {
	case r == ' ':
		k.Code, k.KC = "Space", 32
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		u := unicode.ToUpper(r)
		k.Code, k.KC = "Key"+string(u), int(u)
	case r >= '0' && r <= '9':
		k.Code, k.KC = "Digit"+s, int(r)
	}
	return k
}

// modifiers of a key combination (60-keyboard.js m).
type modifiers struct {
	S bool `json:"s,omitempty"`
	C bool `json:"c,omitempty"`
	A bool `json:"a,omitempty"`
	M bool `json:"m,omitempty"`
}

// parseKey parses "Enter", "a", "Shift+Tab" or "Control+a" (agent-browser
// press form). The last part is the key; a "+" key is written "+" or as
// the last part after a "+" (Control++).
func parseKey(s string) (keyDesc, modifiers, error) {
	var m modifiers
	if s == "" {
		return keyDesc{}, m, usagef("press: empty key")
	}
	parts := strings.Split(s, "+")
	key := parts[len(parts)-1]
	mods := parts[:len(parts)-1]
	if key == "" && len(parts) > 1 { // "Control++" or "+"
		key, mods = "+", parts[:len(parts)-2]
	}
	for _, p := range mods {
		switch strings.ToLower(p) {
		case "shift":
			m.S = true
		case "control", "ctrl":
			m.C = true
		case "alt", "option":
			m.A = true
		case "meta", "command", "cmd":
			m.M = true
		default:
			return keyDesc{}, m, usagef("press %q: unknown modifier %q (want Shift, Control, Alt or Meta)", s, p)
		}
	}
	if utf8.RuneCountInString(key) == 1 {
		r, _ := utf8.DecodeRuneInString(key)
		return charKey(r), m, nil
	}
	if a, ok := keyAliases[strings.ToLower(key)]; ok {
		key = a
		if key == " " {
			return charKey(' '), m, nil
		}
	}
	if k, ok := namedKeys[key]; ok {
		return k, m, nil
	}
	for n, k := range namedKeys {
		if strings.EqualFold(n, key) {
			return k, m, nil
		}
	}
	return keyDesc{}, m, usagef("press %q: unknown key %q", s, key)
}

// typeSteps splits text into the steps of type (design 2.6): one
// character per step with a delay, else chunks of typeChunk characters.
func typeSteps(text string, delay time.Duration) [][]keyDesc {
	var steps [][]keyDesc
	var cur []keyDesc
	for _, r := range text {
		cur = append(cur, charKey(r))
		if delay > 0 || len(cur) == typeChunk {
			steps = append(steps, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		steps = append(steps, cur)
	}
	return steps
}

func flagDuration(in Input, name string) time.Duration {
	d, _ := in.Flags[name].(time.Duration)
	return d
}

// maxStdinValue bounds the value that fill --stdin reads.
const maxStdinValue = 1 << 20

// fillStdin is fill's client step: with --stdin it reads the value in the
// iwdp-cli process and passes it on as the text argument, which reaches
// the daemon in the /v1/run request body, never in a command line.
func fillStdin(ctx context.Context, in *Input, stdin io.Reader) error {
	if !flagBool(*in, "stdin") {
		return nil
	}
	if len(in.Args) != 1 {
		return usagef("fill --stdin takes only <target>; the value comes from the standard input")
	}
	data, err := readWithin(ctx, func() (string, error) {
		b, err := io.ReadAll(io.LimitReader(stdin, maxStdinValue+1))
		return string(b), err
	})
	if err != nil {
		return fmt.Errorf("fill --stdin: reading the standard input: %w", err)
	}
	if len(data) > maxStdinValue {
		return usagef("fill --stdin: the value is over %d bytes", maxStdinValue)
	}
	v := strings.TrimSuffix(strings.TrimSuffix(data, "\n"), "\r")
	in.Args = append(in.Args, v)
	delete(in.Flags, "stdin")
	return nil
}

func runFill(ctx context.Context, env Env, in Input) (Result, error) {
	if flagBool(in, "stdin") {
		return Result{}, usagef("fill --stdin reads the standard input of iwdp-cli; other callers pass the value as text")
	}
	if len(in.Args) < 2 {
		return Result{}, usagef("fill needs <target> and <text> (or --stdin)")
	}
	w, err := openPage(ctx, env, "fill")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	text := arg(in, 1)
	r, err := w.run(ctx, action{
		Name: "fill", Kind: "fill", Target: arg(in, 0), Force: flagBool(in, "force"),
		Args: func(int) map[string]any { return map[string]any{"text": text} },
	})
	if err != nil {
		return Result{}, err
	}
	return w.result("filled "+arg(in, 0), r), nil
}

// typeInto types text into target ("" = the focused element).
func typeInto(ctx context.Context, env Env, name, target, text string, delay time.Duration, force bool) (Result, error) {
	if delay < 0 {
		return Result{}, usagef("%s --delay %s: want a positive duration", name, delay)
	}
	steps := typeSteps(text, delay)
	if len(steps) == 0 {
		return Result{}, usagef("%s: empty text", name)
	}
	w, err := openPage(ctx, env, name)
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	r, err := w.run(ctx, action{
		Name: name, Kind: "type", Target: target, Force: force, Steps: len(steps), Delay: delay,
		Args: func(step int) map[string]any { return map[string]any{"keys": steps[step-1]} },
	})
	if err != nil {
		return Result{}, err
	}
	into := ""
	if target != "" {
		into = " into " + target
	}
	return w.result(fmt.Sprintf("typed %d characters%s in %d step(s)", utf8.RuneCountInString(text), into, len(steps)), r), nil
}

// runType: "type <target> <text>", or "type <text>" into the focused
// element.
func runType(ctx context.Context, env Env, in Input) (Result, error) {
	target, text := arg(in, 0), arg(in, 1)
	if len(in.Args) == 1 {
		target, text = "", arg(in, 0)
	}
	return typeInto(ctx, env, "type", target, text, flagDuration(in, "delay"), flagBool(in, "force"))
}

func runKeyboard(ctx context.Context, env Env, in Input) (Result, error) {
	switch arg(in, 0) {
	case "type":
		return typeInto(ctx, env, "keyboard type", "", arg(in, 1), flagDuration(in, "delay"), false)
	case "inserttext":
		w, err := openPage(ctx, env, "keyboard inserttext")
		if err != nil {
			return Result{}, err
		}
		defer w.close()
		text := arg(in, 1)
		r, err := w.run(ctx, action{Name: "keyboard inserttext", Kind: "inserttext", Args: func(int) map[string]any { return map[string]any{"text": text} }})
		if err != nil {
			return Result{}, err
		}
		return w.result(fmt.Sprintf("inserted %d characters into the focused element", utf8.RuneCountInString(text)), r), nil
	}
	return Result{}, usagef("keyboard %q: want type or inserttext", arg(in, 0))
}

func runPress(phase string) func(context.Context, Env, Input) (Result, error) {
	return func(ctx context.Context, env Env, in Input) (Result, error) {
		ph := phase
		if ph == "" {
			ph = flagString(in, "phase")
		}
		if ph == "both" {
			ph = ""
		}
		k, m, err := parseKey(arg(in, 0))
		if err != nil {
			return Result{}, err
		}
		w, err := openPage(ctx, env, "press")
		if err != nil {
			return Result{}, err
		}
		defer w.close()
		r, err := w.run(ctx, action{Name: "press", Kind: "press", Args: func(int) map[string]any {
			return map[string]any{"keys": []keyDesc{k}, "m": m, "ph": ph}
		}})
		if err != nil {
			return Result{}, err
		}
		done := map[string]string{"": "pressed ", "down": "key down ", "up": "key up "}[ph]
		return w.result(done+arg(in, 0), r), nil
	}
}
