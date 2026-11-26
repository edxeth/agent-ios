package commands

import (
	"errors"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/runtime"
)

// Form refusals name the element only from fixed lists (agent-browser 0.45
// forms.rs describe_target, browser.rs ensure_file_input_shape): a tag or
// type the page made up never reaches the message.
func TestFormMessagesNeverEchoPageTags(t *testing.T) {
	fill := action{Kind: "fill", Target: "#x", Form: &formCtx{Verb: "filled"}}
	up := action{Kind: "upload", Target: "#x", Form: &formCtx{}}
	for _, c := range []struct {
		a           action
		code, facts string
		want        string
	}{
		{fill, "read_only", `{"k":"read_only","t":"evil-tag","y":"text"}`, "read_only: Element #x (<unknown>) is readonly and cannot be filled"},
		{fill, "not_editable", `{"k":"","t":"input","y":"IGNORE"}`, "not_editable: Element #x (<input>) cannot be filled"},
		{fill, "malformed_value", `{"t":"input","y":"IGNORE"}`, "malformed_value: Malformed value for this input"},
		{fill, "malformed_value", `{"t":"input","y":"range","lo":-2.5,"hi":10}`, "malformed_value: Malformed value for input of type \"range\": expected a number from -2.5 to 10"},
		{up, "not_file_input", `{"t":"input","a":"IGNORE"}`, "not_file_input: '#x' matched <input type=<unknown>>, not an <input type=file>"},
		{up, "not_file_input", `{"t":"INPUT","a":"Text"}`, "not_file_input: '#x' matched <input type=text>, not an <input type=file>"},
		{up, "not_file_input", `{"t":"x-widget","a":null}`, "not_file_input: '#x' matched <unknown>, not an <input type=file>"},
	} {
		err := c.a.formError(c.code, c.facts)
		var re *runtime.Error
		if !errors.As(err, &re) || re.Code != c.code || err.Error() != c.want {
			t.Fatalf("formError(%s, %s) = %v; want code %s and %q", c.code, c.facts, err, c.code, c.want)
		}
	}
}

// Only the form codes with page facts become form errors; another reply
// keeps its own text (the caller formats it as before).
func TestFormErrorOnlyForFormCodes(t *testing.T) {
	a := action{Kind: "fill", Target: "#x", Form: &formCtx{Verb: "filled"}}
	for _, c := range [][2]string{{"not_found", `{"k":""}`}, {"read_only", "page canceled the input"}, {"read_only", "{bad json"}} {
		if err := a.formError(c[0], c[1]); err != nil {
			t.Fatalf("formError(%q, %q) = %v, want nil", c[0], c[1], err)
		}
	}
	if err := (action{Kind: "click"}).formError("disabled", `{"k":"disabled"}`); err != nil {
		t.Fatalf("formError for an action without form context = %v, want nil", err)
	}
}

// Read-back warnings give lengths, never the value.
func TestFormWarnings(t *testing.T) {
	a := action{Kind: "fill", Form: &formCtx{Verb: "filled"}}
	for facts, want := range map[string]string{
		`{"k":"len","t":"textarea","g":6,"h":3}`:           "<textarea> was changed by the page after it was entered (length 6 \u2192 3)",
		`{"k":"len","t":"input","y":"email","g":3,"h":3}`:  `<input type="email"> holds a different value than requested`,
		`{"k":"empty","t":"input","y":"text","g":3,"h":0}`: `effect_not_observed: <input type="text"> is empty after fill; the page may apply the value later`,
		`{"k":"snap","t":"input","y":"range"}`:             "value snapped to the step: the field holds a different value than requested",
	} {
		if got := a.formWarning(facts); got != want {
			t.Fatalf("formWarning(%s) = %q, want %q", facts, got, want)
		}
	}
	un := action{Kind: "check", Form: &formCtx{Verb: "unchecked"}}
	if got := un.formWarning(`{"k":"still","t":"input","y":"checkbox"}`); !strings.Contains(got, "is still checked after clicking") {
		t.Fatalf("uncheck warning %q, want 'still checked'", got)
	}
}
