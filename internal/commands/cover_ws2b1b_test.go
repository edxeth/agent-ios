package commands

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/devtools/devtoolstest"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// A line with global options and no command word is a usage error that
// names the line (exit 2), also with --help or -h (WS2 round-1 review S1
// F1: only "--help" or "-h" alone is the overview). Both end before any
// connection.
func TestOnlyGlobalOptionsAreAUsageErrorOrTheOverview(t *testing.T) {
	_, err := Parse([]string{"--json", "--session", "s1"}, noEnv)
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), `no command in "--json --session s1"`) {
		t.Fatalf("--json --session s1: %v; want the no-command usage error", err)
	}
	_, err = Parse([]string{"--json", "-h"}, noEnv)
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), `no command in "--json -h"`) {
		t.Fatalf("--json -h: %v; want the no-command usage error", err)
	}
	code, out, _, env := runCLI("--json", "--help")
	if code != ExitUsage || out != "" || env != 0 {
		t.Errorf("--json --help: exit %d, stdout %q, env calls %d; want exit 2 without a connection", code, out, env)
	}
	inv, err := Parse([]string{"-h"}, noEnv)
	if err != nil || !inv.Help || inv.Cmd != nil || inv.Group != "" {
		t.Fatalf("-h: %+v, %v; want the overview's help invocation", inv, err)
	}
}

// "cookies" fails with the page's error when Page.getCookies fails (WS2a:
// the legacy listing's error path).
func TestCookiesListFailsWithThePageError(t *testing.T) {
	p := devtoolstest.New(t, "https://fixture.example")
	p.Server.Handle("Page.getCookies", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "deliberate getCookies failure"}
	})
	_, err := runCmd(t, spEnv{ps: &core.PageSession{Caller: p.Conn}}, "cookies")
	if err == nil || !strings.Contains(err.Error(), "deliberate getCookies failure") {
		t.Fatalf("cookies with a failing Page.getCookies: %v; want the page's error", err)
	}
}

// evalText reports a value that is not JSON as a decode error that names
// the value's type, and prints an empty string, or one that ends with a
// newline, with one more newline (the legacy fmt.Println output: "" is one
// empty line).
func TestEvalTextDecodeErrorAndNewlines(t *testing.T) {
	if _, err := evalText(webkit.RemoteObject{Type: "object", Value: json.RawMessage(`{`)}); err == nil || !strings.Contains(err.Error(), "decoding the object result") {
		t.Fatalf("evalText of a malformed value: %v; want the decode error naming the type", err)
	}
	for in, want := range map[string]string{`""`: "\n", `"a\n"`: "a\n\n", `"a"`: "a"} {
		got, err := evalText(webkit.RemoteObject{Type: "string", Value: json.RawMessage(in)})
		if err != nil || got != want {
			t.Errorf("evalText(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
}
