package commands

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// Orchestrator note on WS2 round 2 (worker B's open point): an invalid
// option value exits 2 before any connection. "cookies set" checked
// --same-site (and --expires) only in its Run, which the daemon calls
// after it leased the page; "trace start" did the same with --categories.
// Parse now checks them (Command.Check), so neither the CLI nor the daemon
// (which parses /v1/run with Parse) reaches the page.
func TestInvalidOptionValuesExitTwoBeforeAnyConnection(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string // in stderr
	}{
		{[]string{"cookies", "set", "a", "b", "--same-site=bogus"}, `invalid sameSite "bogus"`},
		{[]string{"cookies", "set", "a", "b", "--expires", "tomorrow"}, `--expires "tomorrow"`},
		{[]string{"trace", "start", "--categories", "timeline,bogus"}, `"bogus" is not timeline or script`},
	} {
		var out, errb bytes.Buffer
		built := false
		newEnv := func(context.Context, *Invocation) (Env, error) { built = true; return nil, fmt.Errorf("no env") }
		code := RunCLIStarted(context.Background(), tc.args, &out, &errb, nil, newEnv)
		if code != ExitUsage || built || !strings.Contains(errb.String(), tc.want) {
			t.Errorf("%q: exit %d, environment built %v, stderr %q; want exit 2 naming %q before any connection", tc.args, code, built, errb.String(), tc.want)
		}
		if _, err := Parse(tc.args, noEnv); ExitCodeOf(err) != ExitUsage {
			t.Errorf("Parse(%q) (as the daemon parses /v1/run): %v; want a usage error", tc.args, err)
		}
	}
	// Valid values still parse.
	for _, args := range [][]string{
		{"cookies", "set", "a", "b", "--same-site=strict", "--expires", "0"},
		{"cookies", "set", "--curl", "f.json"},
		{"trace", "start", "--categories", "timeline"},
		{"trace", "start"},
	} {
		if _, err := Parse(args, noEnv); err != nil {
			t.Errorf("Parse(%q): %v; want it accepted", args, err)
		}
	}
}

// WS2 round-2 review F6: help checks the command's option values too
// (criterion 3b: fixed-set values are checked before any connection), but
// needs none of its arguments. Before, the help branch of Parse returned
// before Command.Check, so these lines printed help and exited 0.
func TestOptionValuesAreCheckedAlsoWithHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string // in stderr
	}{
		{[]string{"cookies", "set", "--same-site=bogus", "--help"}, `invalid sameSite "bogus"`},
		{[]string{"cookies", "set", "--expires=tomorrow", "--help"}, `--expires "tomorrow"`},
		{[]string{"cookies", "set", "a", "b", "-h", "--expires", "tomorrow"}, `--expires "tomorrow"`},
		{[]string{"trace", "start", "--categories=bogus", "--help"}, `"bogus" is not timeline or script`},
	} {
		var out, errb bytes.Buffer
		built := false
		newEnv := func(context.Context, *Invocation) (Env, error) { built = true; return nil, fmt.Errorf("no env") }
		code := RunCLIStarted(context.Background(), tc.args, &out, &errb, nil, newEnv)
		if code != ExitUsage || built || out.Len() != 0 || !strings.Contains(errb.String(), tc.want) {
			t.Errorf("%q: exit %d, environment built %v, stdout %q, stderr %q; want exit 2 naming %q and no help", tc.args, code, built, out.String(), errb.String(), tc.want)
		}
	}
	// Valid values, and missing arguments, still give the help.
	for _, args := range [][]string{
		{"cookies", "set", "--help"},
		{"cookies", "set", "--same-site=strict", "--expires=0", "--help"},
		{"trace", "start", "--categories=timeline", "-h"},
	} {
		inv, err := Parse(args, noEnv)
		if err != nil || !inv.Help || inv.Cmd == nil {
			t.Errorf("Parse(%q) = %+v, %v; want the command's help", args, inv, err)
		}
	}
}

// WS2 round-2 review F7: --expires in Unix seconds whose milliseconds are
// outside the range of a JavaScript Date (ECMA-262 "Time Values and Time
// Range": ±8.64e15 ms, so ±8.64e12 s) is a usage error before any
// connection; 1 followed by 306 zeros is finite in seconds but +Inf in
// milliseconds. A tiny nonzero value that rounds to 0 is a time in the
// past, not a session cookie (round-2 review repro TestReviewR2TinyNonzeroExpiry).
func TestCookieExpiryRange(t *testing.T) {
	huge := "1" + strings.Repeat("0", 306)
	for _, v := range []string{huge, "-" + huge, "8640000000000.001", "-8640000000001"} {
		if _, _, err := cookieExpiry(v); ExitCodeOf(err) != ExitUsage || !strings.Contains(err.Error(), "out of range") {
			t.Errorf("cookieExpiry(%.20s...): %v; want an out-of-range usage error", v, err)
		}
		args := []string{"cookies", "set", "a", "b", "--domain=example.com", "--expires=" + v}
		if _, err := Parse(args, noEnv); ExitCodeOf(err) != ExitUsage {
			t.Errorf("Parse(cookies set --expires=%.20s...) (as the daemon parses /v1/run): %v; want a usage error", v, err)
		}
	}
	for v, want := range map[string]float64{"8640000000000": 8.64e15, "-8640000000000": -8.64e15} {
		if session, ms, err := cookieExpiry(v); err != nil || session || ms != want {
			t.Errorf("cookieExpiry(%q) = %v, %v, %v; want %v ms", v, session, ms, err, want)
		}
	}
	tiny := "0." + strings.Repeat("0", 330) + "1"
	if session, ms, err := cookieExpiry(tiny); err != nil || session || ms != 0 {
		t.Errorf("cookieExpiry(0.<330 zeros>1) = %v, %v, %v; want a past expiry of 0 ms, not a session cookie", session, ms, err)
	}
	for _, v := range []string{"0", "-0", "0.000", "00"} {
		if session, _, err := cookieExpiry(v); err != nil || !session {
			t.Errorf("cookieExpiry(%q) = session %v, %v; want a session cookie", v, session, err)
		}
	}
}
