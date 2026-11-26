package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parity-045 decision D5: every action whose dispatcher or checks live in
// a runtime module declares it, so each attempt checks the module in the
// page (and installs it there when missing); the actions of the core do
// not, so they never install a module.
func TestActionsDeclareTheirRuntimeModules(t *testing.T) {
	h := newActionHarness(t)
	h.page.set("act", attempt("dispatched", "", map[string]any{"trusted": false, "v": "x"}))
	file := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(file, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want string // the module check of the act call; "" for none
	}{
		{[]string{"fill", "#a", "x"}, `has("forms")`},
		{[]string{"type", "#a", "x"}, `has("forms")`},
		{[]string{"check", "#a"}, `has("forms")`},
		{[]string{"uncheck", "#a"}, `has("forms")`},
		{[]string{"select", "#a", "v"}, `has("forms")`},
		{[]string{"upload", "#a", file}, `has("forms")`},
		{[]string{"drag", "#a", "#b"}, `has("gestures")`},
		{[]string{"swipe", "up"}, `has("gestures")`},
		{[]string{"mouse", "move", "1", "2"}, `has("gestures")`},
		{[]string{"keyboard", "type", "x"}, ""},
		{[]string{"type", "#a", "\n"}, ""},
		{[]string{"click", "#a"}, ""},
		{[]string{"press", "Enter"}, ""},
		{[]string{"scroll", "down"}, ""},
	} {
		before := len(h.f.ReceivedMethod("Runtime.evaluate"))
		if code, out, errOut := h.run(c.args...); code != 0 {
			t.Fatalf("%v: exit %d: %s %s", c.args, code, out, errOut)
		}
		acts := 0
		for _, call := range h.f.ReceivedMethod("Runtime.evaluate")[before:] {
			var p struct{ Expression string }
			_ = json.Unmarshal(call.Params, &p)
			if !strings.Contains(p.Expression, `window.__iwdp.call("act"`) {
				continue
			}
			acts++
			has := strings.Contains(p.Expression, ".has(")
			if c.want == "" && has || c.want != "" && !strings.Contains(p.Expression, c.want) {
				t.Fatalf("%v: act call %.200s, want module check %q", c.args, p.Expression, c.want)
			}
		}
		if acts == 0 {
			t.Fatalf("%v sent no act call", c.args)
		}
	}
}
