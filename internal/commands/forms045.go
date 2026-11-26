package commands

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/edxeth/agent-ios/internal/runtime"
)

// Form preconditions and read-back warnings of agent-browser 0.45
// (cli/src/native/forms.rs, browser.rs ensure_file_input_shape). The page
// (55-actions.js, 60-keyboard.js) refuses before it changes anything and
// answers with a stable code and the JSON of fixed facts (formFacts); the
// text is built here, so nothing the page controls reaches a message
// except the element's tag and type, which only name it when they are in
// the fixed lists below, and the option list of option_not_found, which
// agent-browser prints as page content too.

// formCodes are the codes of the form refusals (forms.rs FORM_ERROR_CODES
// and the upload precondition not_file_input).
var formCodes = []string{"not_editable", "read_only", "disabled", "not_checkable", "option_not_found", "ambiguous", "not_multiple", "malformed_value", "not_file_input"}

// formKinds is the fill and type refusal table that the page receives as
// "fk": a tag or "input:<type>" that never takes text, mapped to the
// command to use instead ("" for none), and "v:<type>", the value inputs
// that fill sets directly and type refuses.
var formKinds = map[string]string{
	"select": "select", "optgroup": "select", "option": "select", "button": "", "label": "",
	"input:checkbox": "check", "input:radio": "check", "input:file": "", "input:hidden": "", "input:image": "",
	"input:button": "", "input:submit": "", "input:reset": "",
	"v:color": "", "v:date": "", "v:time": "", "v:datetime-local": "", "v:month": "", "v:week": "", "v:range": "",
}

// checkBox is the table of the element that check toggles ("cb"): box
// roles r, roles n and tags t that are never a box, the interactive
// elements i between a target and its label, and the other elements o
// that keep a wrapper from standing for its one native box.
var checkBox = map[string]string{
	"r": "checkbox radio switch menuitemcheckbox menuitemradio option treeitem",
	"n": "button link textbox combobox listbox menuitem searchbox slider spinbutton tab",
	"t": "button a area select textarea input summary details iframe embed object",
	"i": `button,a[href],area[href],input,select,textarea,summary,details,iframe,embed,object,[role],[onclick],[tabindex]:not([tabindex="-1"]),[contenteditable=""],[contenteditable="true"]`,
	"o": `button,a[href],area[href],input:not([type="checkbox"]):not([type="radio"]):not([type="hidden"]),select,textarea,summary,details,iframe,embed,object,[role],[onclick],[tabindex]:not([tabindex="-1"]),[contenteditable=""],[contenteditable="true"]`,
}

var (
	knownTags  = strings.Fields("a article aside button canvas details dialog div fieldset footer form h1 h2 h3 h4 h5 h6 header iframe img input label legend li main nav ol optgroup option output p pre section select span summary table td textarea th tr ul")
	knownTypes = strings.Fields("button checkbox color date datetime-local email file hidden image month number password radio range reset search submit tel text time url week")
	// htmlTags is browser.rs HTML_TAGS, the tags that not_file_input names.
	htmlTags      = strings.Fields("a abbr address area article aside audio b base bdi bdo blockquote body br button canvas caption cite code col colgroup data datalist dd del details dfn dialog div dl dt em embed fieldset figcaption figure footer form h1 h2 h3 h4 h5 h6 head header hgroup hr html i iframe img input ins kbd label legend li link main map mark menu meta meter nav noscript object ol optgroup option output p param picture pre progress q rp rt ruby s samp script search section select slot small source span strong style sub summary sup svg table tbody td template textarea tfoot th thead time title tr track u ul var video wbr")
	malformedWant = map[string]string{
		"color": "a CSS color such as #rrggbb, #rgb or red", "date": "YYYY-MM-DD", "time": "HH:MM or HH:MM:SS",
		"datetime-local": "YYYY-MM-DDTHH:MM", "month": "YYYY-MM", "week": "YYYY-Www",
	}
)

// formFacts is what the page reports of a refusal or a read-back.
type formFacts struct {
	K  string      `json:"k"` // the case: kind of refusal or warning
	T  string      `json:"t"` // tag (localName)
	Y  string      `json:"y"` // input type
	A  *string     `json:"a"` // the type attribute (upload)
	N  int         `json:"n"` // number of options
	I  int         `json:"i"` // index of the value that matched several options
	D  int         `json:"d"` // number of options it matched
	O  [][2]string `json:"o"` // the options: value, label
	Lo float64     `json:"lo"`
	Hi float64     `json:"hi"`
	G  int         `json:"g"` // length given
	H  int         `json:"h"` // length held
}

// formCtx is what the form messages need of the command line.
type formCtx struct {
	Verb   string   // filled, typed into, checked, unchecked
	Values []string // select values
	By     string   // select --by
}

// describe names an element from fixed vocabulary only: <input
// type="t">, <tag> or <unknown> (forms.rs describe_target).
func (f formFacts) describe() string {
	switch {
	case f.T == "input" && slices.Contains(knownTypes, f.Y):
		return fmt.Sprintf("<input type=%q>", f.Y)
	case slices.Contains(knownTags, f.T):
		return "<" + f.T + ">"
	}
	return "<unknown>"
}

// describeUpload names the target of a refused upload (browser.rs
// ensure_file_input_shape; tags and types compared without case).
func (f formFacts) describeUpload() string {
	tag := strings.ToLower(f.T)
	switch {
	case !slices.Contains(htmlTags, tag):
		return "<unknown>"
	case tag == "input" && f.A != nil:
		t := strings.ToLower(*f.A)
		if !slices.Contains(knownTypes, t) {
			t = "<unknown>"
		}
		return "<input type=" + t + ">"
	}
	return "<" + tag + ">"
}

func numText(n float64) string { return strings.TrimSuffix(fmt.Sprintf("%g", n), ".0") }

// formError turns a page refusal "code: <facts JSON>" of the action a into
// a runtime error with agent-browser's text; anything else is nil.
func (a action) formError(code, facts string) error {
	if a.Form == nil || !slices.Contains(formCodes, code) || !strings.HasPrefix(facts, "{") {
		return nil
	}
	var f formFacts
	if json.Unmarshal([]byte(facts), &f) != nil {
		return nil
	}
	return &runtime.Error{Code: code, Message: a.formMessage(code, f)}
}

func (a action) formMessage(code string, f formFacts) string {
	el := fmt.Sprintf("Element %s (%s) ", a.Target, f.describe())
	v := a.Form.Verb
	switch a.Kind + " " + code {
	case "upload not_file_input":
		return fmt.Sprintf("'%s' matched %s, not an <input type=file>", a.Target, f.describeUpload())
	case "check not_checkable":
		if f.K == "radio" {
			return "A checked radio cannot be unchecked by clicking; select another radio in its group instead"
		}
		return fmt.Sprintf("Element %s is not a checkbox, radio or element with a checkbox-like role, so it cannot be %s", f.describe(), v)
	case "check disabled":
		return fmt.Sprintf("Element %s is disabled and cannot be %s", f.describe(), v)
	case "select not_editable":
		return "select target is not a <select> or a role=combobox/listbox element"
	case "select disabled":
		if f.K == "opt" {
			return "The matched option is disabled"
		}
		return "select target <select> is disabled"
	case "select not_multiple":
		return fmt.Sprintf("%d values requested but <select> does not allow multiple selection", len(a.Form.Values))
	case "select option_not_found":
		return a.optionNotFound(f)
	case "select ambiguous":
		return a.ambiguousOption(f)
	case "fill malformed_value":
		want, ok := malformedWant[f.Y]
		switch {
		case f.Y == "range":
			want = fmt.Sprintf("a number from %s to %s", numText(f.Lo), numText(f.Hi))
		case !ok:
			return "Malformed value for this input"
		}
		return fmt.Sprintf("Malformed value for input of type %q: expected %s", f.Y, want)
	}
	switch {
	case code == "disabled":
		return el + "is disabled and cannot be " + v
	case code == "read_only":
		return el + "is readonly and cannot be " + v
	case f.K == "focus":
		return el + "cannot be " + v + ": focusing it does not move focus, so the text would go to whatever was focused before"
	case f.K == "select":
		return el + "cannot be " + v + "; use select"
	case f.K == "check":
		return el + "cannot be " + v + "; use check or uncheck"
	case f.K == "fill":
		return el + "cannot be " + v + "; use fill to set the value of this input"
	}
	return el + "cannot be " + v
}

// optionNotFound is the option_not_found text: the values asked as a JSON
// array, the option count, then "Available options:" and one JSON object
// per option (agent-browser prints them after the error line).
func (a action) optionNotFound(f formFacts) string {
	asked, _ := json.Marshal(a.Form.Values)
	var b strings.Builder
	if f.K == "list" {
		fmt.Fprintf(&b, "No option matched %s. The list has %d options", asked, f.N)
	} else {
		by := ""
		if a.Form.By != "" {
			by = " by " + a.Form.By
		}
		fmt.Fprintf(&b, "No option matched %s%s. The select has %d options", asked, by, f.N)
	}
	b.WriteString("\nAvailable options:")
	for _, o := range f.O {
		line, _ := json.Marshal(struct {
			Value string `json:"value"`
			Label string `json:"label"`
		}{o[0], o[1]})
		b.WriteString("\n" + string(line))
	}
	return b.String()
}

func (a action) ambiguousOption(f formFacts) string {
	named, _ := json.Marshal(a.Form.Values)
	if f.I >= 0 && f.I < len(a.Form.Values) {
		named, _ = json.Marshal(a.Form.Values[f.I])
	}
	advice := " Pass --by value to match option values or --by label to match visible labels."
	if f.K == "list" {
		return fmt.Sprintf("Multiple options matched %s (%d options).%s", named, f.D, advice)
	}
	if a.Form.By == "label" {
		advice = " Several options share that label; pass --by value with the option value."
	}
	return fmt.Sprintf("Multiple options matched %s (%d options with different values).%s", named, f.D, advice)
}

// formWarning words a read-back warning (the page's facts in "fw"):
// values are never echoed, only lengths.
func (a action) formWarning(facts string) string {
	var f formFacts
	if json.Unmarshal([]byte(facts), &f) != nil {
		return "could not verify the result: the page could not be inspected"
	}
	d := f.describe()
	switch f.K {
	case "snap":
		return "value snapped to the step: the field holds a different value than requested"
	case "empty":
		return "effect_not_observed: " + d + " is empty after fill; the page may apply the value later"
	case "len":
		if f.G != f.H {
			return fmt.Sprintf("%s was changed by the page after it was entered (length %d \u2192 %d)", d, f.G, f.H)
		}
	case "still":
		state := "unchecked"
		if a.Form != nil && a.Form.Verb == "unchecked" {
			state = "checked"
		}
		return "effect_not_observed: " + d + " is still " + state + " after clicking; the page may apply the change later"
	}
	return d + " holds a different value than requested"
}
