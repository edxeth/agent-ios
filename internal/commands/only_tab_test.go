package commands

import (
	"maps"
	"strings"
	"testing"
)

// --only-tab is a boolean global flag, as --takeover: alone or with a
// strconv.ParseBool value, in any position before "--". --only-tab=false
// is the same as no flag.
func TestOnlyTabIsABooleanGlobalFlag(t *testing.T) {
	noEnv := func(string) string { return "" }
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"--only-tab", "open", "https://a.test/"}, true},
		{[]string{"open", "https://a.test/", "--only-tab"}, true},
		{[]string{"--only-tab=true", "eval", "1"}, true},
		{[]string{"--only-tab=false", "eval", "1"}, false},
		{[]string{"eval", "1"}, false},
	} {
		inv, err := Parse(c.argv, noEnv)
		if err != nil || inv.Input.Global.OnlyTab != c.want || inv.Input.Global.Page != "" {
			t.Errorf("Parse(%q) = %+v, %v; want OnlyTab %v and no --page", c.argv, inv, err, c.want)
		}
	}
	inv, err := Parse([]string{"--only-tab=false", "--page", "t1", "eval", "1"}, noEnv)
	if err != nil || inv.Input.Global.OnlyTab || inv.Input.Global.Page != "t1" {
		t.Errorf("--only-tab=false --page t1: %+v, %v; want --page t1 alone", inv, err)
	}
	if _, err := Parse([]string{"--only-tab=maybe", "eval", "1"}, noEnv); ExitCodeOf(err) != ExitUsage || !strings.Contains(err.Error(), "--only-tab \"maybe\"") {
		t.Errorf("--only-tab=maybe: %v (exit %d); want a usage error that names the flag and the value", err, ExitCodeOf(err))
	}
}

// --only-tab together with --page is a usage error (exit 2) before any
// connection, in any order and also with --help.
func TestOnlyTabWithPageIsAUsageErrorBeforeAnyConnection(t *testing.T) {
	for _, argv := range [][]string{
		{"--only-tab", "--page", "t1", "eval", "1"},
		{"eval", "1", "--page=Fixture", "--only-tab=true"},
		{"--page", "only", "--only-tab", "open", "https://a.test/", "--help"},
		{"--only-tab", "--page", "t1", "tab", "list"},
	} {
		code, stdout, stderr, envCalls := runCLI(argv...)
		if code != ExitUsage || envCalls != 0 || stdout != "" || !strings.Contains(stderr, "--only-tab and --page") {
			t.Errorf("%q: exit %d, env calls %d, stdout %q, stderr %q; want exit 2 before any connection", argv, code, envCalls, stdout, stderr)
		}
	}
}

// --only-tab names the page of a page command, so AGENT_IOS_WS_URL does not
// refuse it (refusePageURLEnv), as it does not refuse --page.
func TestOnlyTabNamesThePageDespiteIWDPWSURL(t *testing.T) {
	t.Setenv("AGENT_IOS_WS_URL", "ws://127.0.0.1:9/devtools/page/1")
	if _, _, stderr, envCalls := runCLI("--only-tab", "eval", "1"); envCalls != 1 || strings.Contains(stderr, "AGENT_IOS_WS_URL") {
		t.Errorf("--only-tab eval 1 with AGENT_IOS_WS_URL: env calls %d, stderr %q; want the run", envCalls, stderr)
	}
}

// Every command's help lists --only-tab among the global flags (the
// reference docs/commands.md takes the list from this line).
func TestOnlyTabIsInTheGlobalFlagsLine(t *testing.T) {
	_, list, ok := strings.Cut(Help(&Command{Name: "x", Summary: "x"}), "Global flags:")
	if !ok || !strings.Contains(list, " --only-tab,") {
		t.Fatalf("the Global flags line %q does not list --only-tab", list)
	}
}

// F42: --only-tab names the page of the command, so an argument that
// names a page (Arg.Page) beside it is a usage error (exit 2) before any
// connection, as --page is. Without the argument the command runs. From
// the reviewer's repro TestReviewOnlyTabTakeoverArgument.
func TestOnlyTabWithAPageArgumentIsAUsageErrorBeforeAnyConnection(t *testing.T) {
	for _, argv := range [][]string{
		{"--only-tab", "lease", "takeover", "ws://127.0.0.1:9/devtools/page/2"},
		{"lease", "takeover", "t2", "--only-tab"},
		{"--only-tab", "lease", "break", "Fixture"},
		{"--only-tab", "tab", "t2"},
		{"--only-tab=true", "tab", "close", "t2"},
	} {
		code, stdout, stderr, envCalls := runCLI(argv...)
		if code != ExitUsage || envCalls != 0 || stdout != "" || !strings.Contains(stderr, "--only-tab and the <") {
			t.Errorf("%q: exit %d, env calls %d, stdout %q, stderr %q; want exit 2 before any connection", argv, code, envCalls, stdout, stderr)
		}
	}
	for _, argv := range [][]string{
		{"--only-tab", "lease", "takeover"},
		{"--only-tab", "tab"},
		{"--only-tab", "tab", "close"},
		{"--only-tab=false", "lease", "takeover", "t2"},
	} {
		if code, _, stderr, envCalls := runCLI(argv...); envCalls != 1 || strings.Contains(stderr, "--only-tab and") {
			t.Errorf("%q: exit %d, env calls %d, stderr %q; want the run", argv, code, envCalls, stderr)
		}
	}
	// The help of the command needs none of its arguments.
	if code, _, stderr, envCalls := runCLI("--only-tab", "lease", "takeover", "t2", "--help"); code != ExitOK || envCalls != 0 {
		t.Errorf("--only-tab lease takeover t2 --help: exit %d, env calls %d, stderr %q; want the help", code, envCalls, stderr)
	}
}

// The registry declares each argument that names a page (Arg.Page). This
// is the table of the review of F42: lease takeover [page] and lease
// break <page> take a page key, ws:// URL or part of a title or URL; tab
// <id> and tab close [id] take a tab id or label. Every other argument
// names something else (a URL to open or find, an element, a file, a
// value). A new page argument changes this table on purpose.
func TestPageArgumentsAreDeclared(t *testing.T) {
	want := map[string]bool{"lease takeover page": true, "lease break page": true, "tab id": true, "tab close id": true}
	got := map[string]bool{}
	for _, c := range All() {
		for _, a := range c.Args {
			if a.Page {
				got[c.Name+" "+a.Name] = true
			}
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("arguments declared as a page: %v; want %v", got, want)
	}
}
