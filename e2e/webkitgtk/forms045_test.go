//go:build webkitgtk

package webkitgtk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/commands"
	"github.com/edxeth/agent-ios/internal/core"
)

// Form preconditions of agent-browser 0.45 (forms.rs #2687, custom ARIA
// select #2592, upload preconditions #2689) on forms045.html. Expected
// texts are written from the agent-browser 0.45 contract
// (cli/src/native/forms.rs and browser.rs messages), not from a run.

// formState is everything a refusal must leave as it was: values,
// checked states, selections, ARIA states and the recorded events.
const formState = `JSON.stringify({
  v: Array.prototype.map.call(document.querySelectorAll("input, select"), function (e) { return [e.id, e.value, e.checked]; }),
  ed: document.getElementById("ed").textContent,
  aria: document.getElementById("aria").getAttribute("aria-checked"),
  ev: T.ev, combo: T.combo, active: document.activeElement && document.activeElement.id
})`

func loadForms(t *testing.T) *core.PageSession {
	t.Helper()
	navigate(t, "forms045.html", "!!window.T")
	ps, _ := actionSession(t, hooks{})
	return ps
}

// refuse runs args, wants exit 1 with stderr "Error: <command>: <want>"
// and the form state unchanged.
func refuse(t *testing.T, ps *core.PageSession, want string, args ...string) {
	t.Helper()
	before := pageString(t, formState)
	code, _, errOut := act(context.Background(), ps, append([]string{"--timeout", "10s"}, args...)...)
	line := "Error: " + args[0] + ": " + want
	if code != 1 || strings.TrimRight(errOut, "\n") != line {
		t.Fatalf("%s: exit %d, stderr %q; want exit 1 and %q", strings.Join(args, " "), code, errOut, line)
	}
	if after := pageString(t, formState); after != before {
		t.Fatalf("%s changed the page:\nbefore %s\nafter  %s", strings.Join(args, " "), before, after)
	}
}

// Contract 3: fill and type refuse a target that cannot take the text,
// with the stable code first, and change nothing.
func TestForms045FillAndTypeRefusals(t *testing.T) {
	ps := loadForms(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"fill", "#ro", "x"}, `read_only: Element #ro (<input type="text">) is readonly and cannot be filled`},
		{[]string{"fill", "#dis", "x"}, `disabled: Element #dis (<input type="text">) is disabled and cannot be filled`},
		{[]string{"fill", "#hid", "x"}, `not_editable: Element #hid (<input type="hidden">) cannot be filled`},
		{[]string{"fill", "#btn", "x"}, `not_editable: Element #btn (<button>) cannot be filled`},
		{[]string{"fill", "#plain", "x"}, `not_editable: Element #plain (<div>) cannot be filled`},
		{[]string{"fill", "#s1", "x"}, `not_editable: Element #s1 (<select>) cannot be filled; use select`},
		{[]string{"fill", "#cb", "x"}, `not_editable: Element #cb (<input type="checkbox">) cannot be filled; use check or uncheck`},
		{[]string{"fill", "#inert", "x"}, `not_editable: Element #inert (<input type="text">) cannot be filled: focusing it does not move focus, so the text would go to whatever was focused before`},
		{[]string{"type", "#color", "x"}, `not_editable: Element #color (<input type="color">) cannot be typed into; use fill to set the value of this input`},
		{[]string{"type", "#dis", "x"}, `disabled: Element #dis (<input type="text">) is disabled and cannot be typed into`},
		{[]string{"type", "#cb", "x"}, `not_editable: Element #cb (<input type="checkbox">) cannot be typed into; use check or uncheck`},
		{[]string{"type", "#s1", "x"}, `not_editable: Element #s1 (<select>) cannot be typed into; use select`},
		{[]string{"fill", "#date", "2024-13-45"}, `malformed_value: Malformed value for input of type "date": expected YYYY-MM-DD`},
		{[]string{"fill", "#time", "25:99"}, `malformed_value: Malformed value for input of type "time": expected HH:MM or HH:MM:SS`},
		{[]string{"fill", "#dtl", "2024-02-29"}, `malformed_value: Malformed value for input of type "datetime-local": expected YYYY-MM-DDTHH:MM`},
		{[]string{"fill", "#month", "2024-13"}, `malformed_value: Malformed value for input of type "month": expected YYYY-MM`},
		{[]string{"fill", "#week", "2024-W60"}, `malformed_value: Malformed value for input of type "week": expected YYYY-Www`},
		{[]string{"fill", "#color", "notacolor"}, `malformed_value: Malformed value for input of type "color": expected a CSS color such as #rrggbb, #rgb or red`},
		{[]string{"fill", "#color", "rgba(255, 0, 0, 0.5)"}, `malformed_value: Malformed value for input of type "color": expected a CSS color such as #rrggbb, #rgb or red`},
		{[]string{"fill", "#range", "11"}, `malformed_value: Malformed value for input of type "range": expected a number from 0 to 10`},
		{[]string{"fill", "#range", "abc"}, `malformed_value: Malformed value for input of type "range": expected a number from 0 to 10`},
	} {
		refuse(t, ps, c.want, c.args...)
	}
	// A field that turns readonly when it gets focus is refused after the
	// focus, with its value kept.
	code, _, errOut := act(context.Background(), ps, "--timeout", "10s", "fill", "#rofocus", "new")
	if want := "Error: fill: read_only: Element #rofocus (<input type=\"text\">) is readonly and cannot be filled\n"; code != 1 || errOut != want {
		t.Fatalf("fill #rofocus: exit %d, stderr %q; want %q", code, errOut, want)
	}
	if v := pageString(t, "document.getElementById('rofocus').value"); v != "keep" {
		t.Fatalf("fill #rofocus left %q, want keep", v)
	}
}

// Contract 3: fill sets date, time, datetime-local, month, week, color
// and range inputs directly, with input and change; a range value off the
// step is snapped by the browser and reported.
func TestForms045FillValueInputs(t *testing.T) {
	ps := loadForms(t)
	for _, c := range []struct{ id, in, want string }{
		{"date", "2024-02-29", "2024-02-29"},
		{"time", "13:45", "13:45"},
		{"dtl", "2024-02-29T13:45", "2024-02-29T13:45"},
		{"month", "2024-02", "2024-02"},
		{"week", "2024-W09", "2024-W09"},
		{"color", " red ", "#ff0000"},
		{"color", "#00FF00", "#00ff00"},
		{"range", "6", "6"},
	} {
		eval(t, "T.ev = []; true")
		if out := mustAct(t, ps, "fill", "#"+c.id, c.in); strings.Contains(out, "warning:") {
			t.Fatalf("fill #%s %q warned: %q", c.id, c.in, out)
		}
		if v := pageString(t, "document.getElementById('"+c.id+"').value"); v != c.want {
			t.Fatalf("fill #%s %q: value %q, want %q", c.id, c.in, v, c.want)
		}
		if ev := pageString(t, "T.ev.join(',')"); ev != c.id+":input,"+c.id+":change" {
			t.Fatalf("fill #%s: events %q, want input then change", c.id, ev)
		}
	}
	out := mustAct(t, ps, "fill", "#range", "5")
	if v := pageString(t, "document.getElementById('range').value"); v != "6" || !strings.Contains(out, "warning: value snapped to the step: the field holds a different value than requested") {
		t.Fatalf("fill #range 5 (step 2): value %q, output %q; want 6 and the snap warning", v, out)
	}
}

// Contract 3 read-back: a value that the page changes after fill is a
// warning, never echoed; a password is not judged; a label fills its
// control.
func TestForms045FillWarnings(t *testing.T) {
	ps := loadForms(t)
	for _, c := range []struct{ id, in, value, warning string }{
		{"upper", "abc", "ABC", `warning: <input type="text"> holds a different value than requested`},
		{"cut", "abcdef", "abc", "warning: <input type=\"text\"> was changed by the page after it was entered (length 6 \u2192 3)"},
		{"wipe", "abc", "", `warning: effect_not_observed: <input type="text"> is empty after fill; the page may apply the value later`},
	} {
		out := mustAct(t, ps, "fill", "#"+c.id, c.in)
		if v := pageString(t, "document.getElementById('"+c.id+"').value"); v != c.value || !strings.Contains(out, c.warning) {
			t.Fatalf("fill #%s %q: value %q, output %q; want %q and %q", c.id, c.in, v, out, c.value, c.warning)
		}
	}
	if out := mustAct(t, ps, "fill", "#pw", "secret"); strings.Contains(out, "warning:") {
		t.Fatalf("fill #pw warned: %q", out)
	}
	if out := mustAct(t, ps, "fill", "#lbl", "via label"); strings.Contains(out, "warning:") || pageString(t, "document.getElementById('name').value") != "via label" {
		t.Fatalf("fill #lbl: output %q, #name %s; want the label's control filled", out, eval(t, "document.getElementById('name').value"))
	}
}

// Contract 3 check and uncheck: refusals, the box behind a label or
// wrapper, a hidden box by DOM click, ARIA boxes, an unchanged state as
// a no-op, and a box the page keeps unchecked as a warning.
func TestForms045Check(t *testing.T) {
	ps := loadForms(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"check", "#cbdis"}, `disabled: Element <input type="checkbox"> is disabled and cannot be checked`},
		{[]string{"uncheck", "#r1"}, "not_checkable: A checked radio cannot be unchecked by clicking; select another radio in its group instead"},
		{[]string{"check", "#link"}, "not_checkable: Element <a> is not a checkbox, radio or element with a checkbox-like role, so it cannot be checked"},
		{[]string{"uncheck", "#plain"}, "not_checkable: Element <div> is not a checkbox, radio or element with a checkbox-like role, so it cannot be unchecked"},
		{[]string{"check", "#btn"}, "not_checkable: Element <button> is not a checkbox, radio or element with a checkbox-like role, so it cannot be checked"},
	} {
		refuse(t, ps, c.want, c.args...)
	}
	for _, c := range []struct{ target, state string }{
		{"#opa", "document.getElementById('opa').checked"},
		{"#wrap", "document.getElementById('inwrap').checked"},
		{"#aria", "document.getElementById('aria').getAttribute('aria-checked') === 'true'"},
		{"#cb", "document.getElementById('cb').checked"},
	} {
		if out := mustAct(t, ps, "check", c.target); string(eval(t, c.state)) != "true" || strings.Contains(out, "warning:") {
			t.Fatalf("check %s: state %s, output %q; want checked without a warning", c.target, eval(t, c.state), out)
		}
	}
	// Already checked: no click, no event.
	before := pageString(t, formState)
	mustAct(t, ps, "check", "#cb")
	mustAct(t, ps, "check", "#r1")
	if after := pageString(t, formState); after != before {
		t.Fatalf("check of checked boxes changed the page:\nbefore %s\nafter  %s", before, after)
	}
	out := mustAct(t, ps, "check", "#stuck")
	if want := `warning: effect_not_observed: <input type="checkbox"> is still unchecked after clicking; the page may apply the change later`; !strings.Contains(out, want) {
		t.Fatalf("check #stuck: output %q, want %q", out, want)
	}
}

// Contract 3 select: refusals with their codes, --by resolving an option
// whose value equals another option's label, and a selection the page
// resets reported as a warning.
func TestForms045Select(t *testing.T) {
	ps := loadForms(t)
	opts := "\nAvailable options:\n" + `{"value":"x","label":"Apple"}` + "\n" + `{"value":"Apple","label":"Avocado"}` + "\n" + `{"value":"y","label":"Cherry"}`
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"select", "#sdis", "a"}, "disabled: select target <select> is disabled"},
		{[]string{"select", "#s1", "x", "y"}, "not_multiple: 2 values requested but <select> does not allow multiple selection"},
		{[]string{"select", "#s1", "Apple"}, `ambiguous: Multiple options matched "Apple" (2 options with different values). Pass --by value to match option values or --by label to match visible labels.`},
		{[]string{"select", "#s1", "Durian"}, `option_not_found: No option matched ["Durian"]. The select has 3 options` + opts},
		{[]string{"select", "#s1", "--by", "label", "x"}, `option_not_found: No option matched ["x"] by label. The select has 3 options` + opts},
		{[]string{"select", "#btn", "a"}, "not_editable: select target is not a <select> or a role=combobox/listbox element"},
	} {
		refuse(t, ps, c.want, c.args...)
	}
	for _, c := range []struct{ by, want string }{{"label", "x"}, {"value", "Apple"}} {
		eval(t, "T.ev = []; true")
		out := mustAct(t, ps, "select", "#s1", "Apple", "--by", c.by)
		if v := pageString(t, "document.getElementById('s1').value"); v != c.want || pageString(t, "T.ev.join(',')") != "s1:input,s1:change" || strings.Contains(out, "warning:") {
			t.Fatalf("select #s1 Apple --by %s: value %q, events %s, output %q; want %q, input then change", c.by, v, eval(t, "T.ev"), out, c.want)
		}
	}
	out := mustAct(t, ps, "select", "#sreset", "b")
	if !strings.Contains(out, "warning: <select> holds a different value than requested") {
		t.Fatalf("select #sreset b (the page resets it): output %q, want the warning", out)
	}
}

// Contract 3 (#2592): select on a Radix-like combobox opens it, waits for
// the options of its aria-controls listbox and clicks the one that
// matches by name or value; refusals name the list.
func TestForms045Combobox(t *testing.T) {
	ps := loadForms(t)
	out := mustAct(t, ps, "select", "#combo", "Banana")
	if got := pageString(t, "T.combo.join(',')"); got != "banana" || pageString(t, "document.getElementById('combo').textContent") != "Banana" {
		t.Fatalf("select #combo Banana: picked %q, output %q; want banana", got, out)
	}
	mustAct(t, ps, "select", "#combo", "cherry")
	if got := pageString(t, "T.combo.join(',')"); got != "banana,cherry" {
		t.Fatalf("select #combo cherry (by data-value): picked %q", got)
	}
	code, _, errOut := act(context.Background(), ps, "--timeout", "10s", "select", "#combo", "Durian")
	want := "Error: select: option_not_found: No option matched [\"Durian\"]. The list has 4 options\nAvailable options:\n" +
		`{"value":"apple","label":"Apple"}` + "\n" + `{"value":"banana","label":"Banana"}` + "\n" + `{"value":"kiwi","label":"Kiwi"}` + "\n" + `{"value":"cherry","label":"Banana split"}` + "\n"
	if code != 1 || errOut != want {
		t.Fatalf("select #combo Durian: exit %d, stderr %q; want %q", code, errOut, want)
	}
	code, _, errOut = act(context.Background(), ps, "--timeout", "10s", "select", "#combo", "Kiwi")
	if want := "Error: select: disabled: The matched option is disabled\n"; code != 1 || errOut != want {
		t.Fatalf("select #combo Kiwi: exit %d, stderr %q; want %q", code, errOut, want)
	}
	if got := pageString(t, "T.combo.join(',')"); got != "banana,cherry" {
		t.Fatalf("the refused selects picked %q", got)
	}
}

// cwdEnv is the daemon's view of a CLI run from dir: relative paths
// resolve against it (daemon runEnv.ResolvePath).
type cwdEnv struct {
	env
	dir string
}

func (e cwdEnv) ResolvePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(e.dir, p)
}

// Contract 6 upload: a target that is not a file input is not_file_input
// and a missing file file_not_found, before anything reaches the page;
// relative paths resolve against the CLI's directory and "~/" expands.
func TestForms045Upload(t *testing.T) {
	ps := loadForms(t)
	dir, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(dir, "rel.txt"), []byte("relative"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "home.txt"), []byte("home"), 0o600); err != nil {
		t.Fatal(err)
	}
	upload := func(args ...string) (int, string, string) {
		var out, errb strings.Builder
		code := commands.RunCLIStarted(context.Background(), append([]string{"--timeout", "10s", "upload"}, args...), &out, &errb, nil,
			func(context.Context, *commands.Invocation) (commands.Env, error) { return cwdEnv{env{ps}, dir}, nil })
		return code, out.String(), errb.String()
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"#btn", "rel.txt"}, "not_file_input: '#btn' matched <button>, not an <input type=file>"},
		{[]string{"#name", "rel.txt"}, "not_file_input: '#name' matched <input>, not an <input type=file>"},
		{[]string{"#cb", "rel.txt"}, "not_file_input: '#cb' matched <input type=checkbox>, not an <input type=file>"},
		{[]string{"#file", "missing.txt"}, "file_not_found: " + filepath.Join(dir, "missing.txt") + " does not exist"},
		{[]string{"#file", "rel.txt", "~/nope.txt"}, "file_not_found: " + filepath.Join(home, "nope.txt") + " does not exist"},
	} {
		before := pageString(t, formState)
		code, _, errOut := upload(c.args...)
		if want := "Error: upload: " + c.want + "\n"; code != 1 || errOut != want {
			t.Fatalf("upload %v: exit %d, stderr %q; want %q", c.args, code, errOut, want)
		}
		if after := pageString(t, formState); after != before {
			t.Fatalf("upload %v changed the page:\nbefore %s\nafter  %s", c.args, before, after)
		}
	}
	eval(t, "document.getElementById('file').multiple = true; true")
	if code, _, errOut := upload("#file", "rel.txt", "~/home.txt"); code != 0 {
		t.Fatalf("upload rel.txt ~/home.txt: exit %d: %s", code, errOut)
	}
	if got := pageString(t, "Array.prototype.map.call(document.getElementById('file').files, function (f) { return f.name + ':' + f.size; }).join(',')"); got != "rel.txt:8,home.txt:4" {
		t.Fatalf("the input's files are %q, want rel.txt:8,home.txt:4", got)
	}
}
