package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/devtools/devtoolstest"
	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// evalPage is a fake page whose Runtime.evaluate answers each expression
// with the result in results (a Runtime.evaluate reply); an expression
// that results does not name fails the test.
func evalPage(t *testing.T, results map[string]any) *devtoolstest.Page {
	t.Helper()
	p := devtoolstest.New(t, "https://eval.test")
	p.Server.Handle("Runtime.evaluate", func(c fakewebkit.Call) (any, *fakewebkit.Error) {
		var params struct {
			Expression string `json:"expression"`
		}
		if err := json.Unmarshal(c.Params, &params); err != nil {
			t.Errorf("Runtime.evaluate params %s: %v", c.Params, err)
		}
		r, ok := results[params.Expression]
		if !ok {
			t.Errorf("unexpected Runtime.evaluate of %q", params.Expression)
			return nil, &fakewebkit.Error{Code: -32000, Message: "unexpected expression"}
		}
		return r, nil
	})
	return p
}

func evalResult(obj map[string]any) map[string]any { return map[string]any{"result": obj} }

// evalCmd runs "eval" with argv on page p.
func evalCmd(t *testing.T, p *devtoolstest.Page, argv ...string) (Result, error) {
	t.Helper()
	return runCmd(t, spEnv{ps: &core.PageSession{Caller: p.Conn}}, append([]string{"eval"}, argv...)...)
}

// WS2a brief, section 1, output contract (user-stated): a string, number
// or boolean result prints raw on one line, other values print as JSON.
// The expected texts are those of the legacy eval (cmd/agent-ios cmdEval at
// f731071): a string as is, any other value as json.MarshalIndent with two
// spaces, a value-less result as its description, else "(<type>)".
func TestEvalPrintsTheLegacyOutput(t *testing.T) {
	p := evalPage(t, map[string]any{
		"s":    evalResult(map[string]any{"type": "string", "value": "hello world"}),
		"n":    evalResult(map[string]any{"type": "number", "value": 42, "description": "42"}),
		"f":    evalResult(map[string]any{"type": "number", "value": 1.5, "description": "1.5"}),
		"b":    evalResult(map[string]any{"type": "boolean", "value": true}),
		"no":   evalResult(map[string]any{"type": "boolean", "value": false}),
		"null": evalResult(map[string]any{"type": "object", "subtype": "null", "value": nil}),
		"o":    evalResult(map[string]any{"type": "object", "value": map[string]any{"a": 1, "b": []int{1, 2}}}),
		"u":    evalResult(map[string]any{"type": "undefined"}),
		"sym":  evalResult(map[string]any{"type": "symbol", "description": "Symbol(x)"}),
	})
	for _, c := range []struct{ expr, want string }{
		{"s", "hello world"},
		{"n", "42"},
		{"f", "1.5"},
		{"b", "true"},
		{"no", "false"},
		{"null", "null"},
		{"o", "{\n  \"a\": 1,\n  \"b\": [\n    1,\n    2\n  ]\n}"},
		{"u", "(undefined)"},
		{"sym", "Symbol(x)"},
	} {
		res, err := evalCmd(t, p, c.expr)
		if err != nil || res.Text != c.want {
			t.Errorf("eval %s: text %q, error %v; want %q", c.expr, res.Text, err, c.want)
		}
	}
}

// --json prints the result's type and value (or its description when the
// value has no JSON form), so that agents read it without guessing.
func TestEvalJSONData(t *testing.T) {
	p := evalPage(t, map[string]any{
		"n":   evalResult(map[string]any{"type": "number", "value": 42, "description": "42"}),
		"s":   evalResult(map[string]any{"type": "string", "value": "x"}),
		"u":   evalResult(map[string]any{"type": "undefined"}),
		"sym": evalResult(map[string]any{"type": "symbol", "description": "Symbol(x)"}),
	})
	for expr, want := range map[string]string{
		"n":   `{"type":"number","value":42}`,
		"s":   `{"type":"string","value":"x"}`,
		"u":   `{"type":"undefined"}`,
		"sym": `{"type":"symbol","description":"Symbol(x)"}`,
	} {
		res, err := evalCmd(t, p, expr)
		if err != nil {
			t.Fatalf("eval %s: %v", expr, err)
		}
		data, err := json.Marshal(res.Data)
		if err != nil || string(data) != want {
			t.Errorf("eval %s: data %s (%v), want %s", expr, data, err, want)
		}
	}
}

// WS2a brief, section 1 (user-stated): eval awaits a returned Promise
// without a flag (WebKit's Runtime.evaluate has no awaitPromise, CLAUDE.md
// Known Limitations: evaluate by reference, then Runtime.awaitPromise). The
// legacy --await is still accepted and changes nothing.
func TestEvalAwaitsAPromise(t *testing.T) {
	p := evalPage(t, map[string]any{
		"fetchIt()": evalResult(map[string]any{"type": "object", "className": "Promise", "objectId": "p1"}),
	})
	p.Server.Result("Runtime.awaitPromise", evalResult(map[string]any{"type": "object", "value": map[string]any{"ok": true}}))
	for _, argv := range [][]string{{"fetchIt()"}, {"--await", "fetchIt()"}} {
		res, err := evalCmd(t, p, argv...)
		if err != nil || res.Text != "{\n  \"ok\": true\n}" {
			t.Errorf("eval %v: text %q, error %v; want the settled object as JSON", argv, res.Text, err)
		}
	}
	if n := len(p.Server.ReceivedMethod("Runtime.awaitPromise")); n != 2 {
		t.Errorf("Runtime.awaitPromise was called %d times, want 2 (once per eval)", n)
	}
}

// An object result comes back by reference (eval evaluates by reference to
// see a Promise) and is then read by value.
func TestEvalReadsAnObjectByValue(t *testing.T) {
	p := evalPage(t, map[string]any{
		"({a: 1})": evalResult(map[string]any{"type": "object", "className": "Object", "objectId": "o1"}),
	})
	p.Server.Result("Runtime.callFunctionOn", evalResult(map[string]any{"type": "object", "value": map[string]any{"a": 1}}))
	res, err := evalCmd(t, p, "({a: 1})")
	if err != nil || res.Text != "{\n  \"a\": 1\n}" {
		t.Fatalf("eval ({a: 1}): text %q, error %v; want the object as JSON", res.Text, err)
	}
}

// WS2a brief, section 1 (user-stated): a thrown value exits 1 with its
// message and the stack where WebKit gives one. WebKit returns the thrown
// value as the result with wasThrown and no exceptionDetails; the stack is
// read from the thrown object.
func TestEvalThrownValueExitsOneWithMessageAndStack(t *testing.T) {
	p := evalPage(t, map[string]any{
		"nope": map[string]any{
			"result":    map[string]any{"type": "object", "subtype": "error", "description": "ReferenceError: Can't find variable: nope", "objectId": "e1"},
			"wasThrown": true,
		},
		"throw 'plain'": map[string]any{
			"result":    map[string]any{"type": "string", "value": "plain"},
			"wasThrown": true,
		},
	})
	p.Server.Result("Runtime.callFunctionOn", evalResult(map[string]any{"type": "string", "value": "global code@\nevaluateWithScopeExtension@[native code]"}))
	res, err := evalCmd(t, p, "nope")
	if err == nil || ExitCodeOf(err) != ExitError {
		t.Fatalf("eval nope: result %+v, error %v (exit %d); want exit %d", res, err, ExitCodeOf(err), ExitError)
	}
	if msg := err.Error(); !strings.Contains(msg, "ReferenceError: Can't find variable: nope") || !strings.Contains(msg, "global code@") {
		t.Errorf("eval nope: error %q, want the message and the stack", msg)
	}
	_, err = evalCmd(t, p, "throw 'plain'")
	if err == nil || ExitCodeOf(err) != ExitError || err.Error() != "plain" {
		t.Errorf("eval throw 'plain': error %v, want exit %d and the thrown string", err, ExitError)
	}
}

// The help says how the page is chosen: the registry eval takes no
// trailing ws:// URL (WS2a brief, section 1).
func TestEvalHelpNamesThePageFlag(t *testing.T) {
	c, n := Lookup([]string{"eval"})
	if c == nil || n != 1 || c.Name != "eval" {
		t.Fatal("eval is not a registry command")
	}
	if c.Page != LeasedPage || !c.Mutates || c.RetrySafe {
		t.Errorf("eval: page %v, mutates %v, retry-safe %v; want a leased page, a mutation and no retry", c.Page, c.Mutates, c.RetrySafe)
	}
	if h := Help(c); !strings.Contains(h, "--page") {
		t.Errorf("eval help does not say that --page chooses the page:\n%s", h)
	}
	if _, err := Parse([]string{"eval", "1", "ws://127.0.0.1:9224/devtools/page/1"}, func(string) string { return "" }); ExitCodeOf(err) != ExitUsage {
		t.Errorf("eval with a trailing ws:// URL: %v, want a usage error", err)
	}
}

// WS2a round 2 decision: a token such as -1 or -.5 is an argument (the
// expression of eval), not a flag, before or after the global flags.
// WS2a round 4 rule: only exact, known flags are flags, so every other
// token with a single leading dash is an argument too (-Math.abs(2),
// -window.scrollY, review ws2a-r2-a F5).
func TestParseTakesNegativeNumbersAsArguments(t *testing.T) {
	noEnv := func(string) string { return "" }
	for _, argv := range [][]string{
		{"eval", "-1"},
		{"eval", "--json", "-1"},
		{"eval", "-1", "--json"},
		{"eval", "-.5"},
		{"eval", "-2.5"},
		{"eval", "-Math.abs(2)"},
		{"eval", "-window.scrollY"},
		{"eval", "--json", "-window.scrollY"},
		{"eval", "-x"},
	} {
		inv, err := Parse(argv, noEnv)
		want := argv[1]
		if argv[1] == "--json" {
			want = argv[2]
		}
		if err != nil || inv.Cmd == nil || inv.Cmd.Name != "eval" || len(inv.Input.Args) != 1 || inv.Input.Args[0] != want {
			t.Errorf("Parse(%q) = %+v, %v; want eval with the expression %q", argv, inv, err, want)
		}
	}
}

// WS2a round 4 rule, for every registry command: only exact, known flags
// are flags. A token that starts with "--" and is not a known flag is a
// usage error (exit 2); any other token, also one with a single leading
// dash that names no short flag of the command, is an argument; "--" ends
// the flags. Known short flags (-i, -d of snapshot) stay flags.
func TestParseKnowsOnlyExactFlags(t *testing.T) {
	noEnv := func(string) string { return "" }
	for _, tc := range []struct {
		argv []string
		args []string
	}{
		{[]string{"eval", "--", "--json"}, []string{"--json"}},
		{[]string{"eval", "--", "-1"}, []string{"-1"}},
		{[]string{"fill", "#amount", "-5"}, []string{"#amount", "-5"}},
		{[]string{"fill", "-x", "-Infinity"}, []string{"-x", "-Infinity"}},
		{[]string{"snapshot", "-i", "-d", "-1"}, nil},
	} {
		inv, err := Parse(tc.argv, noEnv)
		if err != nil || inv.Cmd == nil || strings.Join(inv.Input.Args, "\x00") != strings.Join(tc.args, "\x00") {
			t.Errorf("Parse(%q) = %+v, %v; want the arguments %q", tc.argv, inv, err, tc.args)
		}
	}
	inv, err := Parse([]string{"eval", "--", "--json"}, noEnv)
	if err == nil && inv.Input.Global.JSON {
		t.Error("Parse(eval -- --json): a token after -- set --json")
	}
	inv, err = Parse([]string{"snapshot", "-i", "-d", "-1"}, noEnv)
	if err != nil || inv.Input.Flags["interactive"] != true || inv.Input.Flags["depth"] != -1 {
		t.Errorf("Parse(snapshot -i -d -1) = %+v, %v; want the short flags -i and -d", inv, err)
	}
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"eval", "--x", "1"}, "unknown flag --x"},
		{[]string{"eval", "1", "--x=2"}, "unknown flag --x"},
		{[]string{"cookies", "--bogus"}, "unknown flag --bogus"},
		{[]string{"fill", "#a", "b", "--no-such-flag"}, "unknown flag --no-such-flag"},
		{[]string{"snapshot", "-q"}, `unexpected argument "-q"`},
		{[]string{"eval", "-x", "1"}, `unexpected argument "1"`},
	} {
		_, err := Parse(tc.argv, noEnv)
		if ExitCodeOf(err) != ExitUsage || !strings.Contains(fmt.Sprint(err), tc.want) {
			t.Errorf("Parse(%q): %v (exit %d); want the usage error %q (exit 2)", tc.argv, err, ExitCodeOf(err), tc.want)
		}
	}
}

// WS2a round 2 decision: a trailing ws:// URL is not a page selector; the
// usage error names --page.
func TestParseNamesThePageFlagForATrailingURL(t *testing.T) {
	for _, argv := range [][]string{{"cookies", "ws://127.0.0.1:9/devtools/page/1"}, {"eval", "1+1", "wss://h/devtools/page/1"}} {
		_, err := Parse(argv, func(string) string { return "" })
		var ue *UsageError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), "--page <ws:// URL>") || !strings.Contains(err.Error(), argv[len(argv)-1]) {
			t.Errorf("Parse(%q): %v; want a usage error that names the URL and --page", argv, err)
		}
	}
}

// WS2a round 2 decision: AGENT_IOS_WS_URL no longer selects the page of eval
// and cookies: without --page they exit 2 before any connection (newEnv
// is not called); an explicit --page wins over the variable.
func TestIWDPWSURLNeedsThePageFlag(t *testing.T) {
	t.Setenv("AGENT_IOS_WS_URL", "ws://127.0.0.1:9/devtools/page/1")
	for _, argv := range [][]string{{"eval", "1"}, {"cookies"}, {"--json", "cookies", "--show-values"}} {
		code, stdout, stderr, envCalls := runCLI(argv...)
		if code != ExitUsage || envCalls != 0 || stdout != "" || !strings.Contains(stderr, "AGENT_IOS_WS_URL") || !strings.Contains(stderr, "--page") {
			t.Errorf("%q with AGENT_IOS_WS_URL: exit %d, env calls %d, stdout %q, stderr %q; want exit 2 naming --page before any connection", argv, code, envCalls, stdout, stderr)
		}
	}
	for _, argv := range [][]string{{"eval", "1", "--page", "t1"}, {"--page", "t1", "cookies"}} {
		if _, _, stderr, envCalls := runCLI(argv...); envCalls != 1 || strings.Contains(stderr, "AGENT_IOS_WS_URL") {
			t.Errorf("%q with AGENT_IOS_WS_URL: env calls %d, stderr %q; want the run with --page", argv, envCalls, stderr)
		}
	}
}
