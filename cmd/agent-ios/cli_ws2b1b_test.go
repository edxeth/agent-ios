package main

import (
	"bytes"
	"strings"
	"testing"

	reg "github.com/nnemirovsky/agent-ios/internal/commands"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil"
)

// WS2b-1b-b item 1: agent-ios before the registry (f731071,
// parsePositiveDuration) took a bare whole number as seconds for
// --timeout, AGENT_IOS_TIMEOUT and --wait-lease. Help parses the global
// options first, so a refused value fails the help with exit 2.
func TestBareSecondsWorkForTheDurationOptions(t *testing.T) {
	for _, tc := range []struct {
		env  []string
		args []string
	}{
		{nil, []string{"status", "--timeout", "5", "--help"}},
		{nil, []string{"eval", "--timeout=5", "--help"}},
		{nil, []string{"cookies", "--wait-lease", "5", "--help"}},
		{[]string{"AGENT_IOS_TIMEOUT=5"}, []string{"devices", "--help"}},
	} {
		r := runCLI(t, "", tc.env, tc.args...)
		if r.code != 0 || !strings.HasPrefix(r.stdout, "Usage: agent-ios "+tc.args[0]) {
			t.Errorf("%v %v: exit %d, stdout %q, stderr %q; want the help (exit 0)", tc.env, tc.args, r.code, r.stdout, r.stderr)
		}
	}
	r := runCLI(t, "", nil, "status", "--timeout", "0", "--help")
	if r.code != 2 || !strings.Contains(r.stderr, "30s") || !strings.Contains(r.stderr, "such as 30") {
		t.Errorf("status --timeout 0 --help: exit %d, stderr %q; want exit 2 naming both forms", r.code, r.stderr)
	}
}

// groupNames lists the registry command names (and aliases) that start
// with the word group and have more words.
func groupNames(t *testing.T, group string) []string {
	t.Helper()
	var names []string
	for _, c := range reg.All() {
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			if strings.HasPrefix(n, group+" ") {
				names = append(names, n)
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("the registry has no command of the group %q", group)
	}
	return names
}

// assertLists checks that out names each command of the group with its
// one-line summary.
func assertLists(t *testing.T, what, out, group string) {
	t.Helper()
	for _, n := range groupNames(t, group) {
		c, _ := reg.Lookup(strings.Fields(n))
		if c == nil {
			t.Fatalf("lookup of %q failed", n)
		}
		if !strings.Contains(out, n+" ") || !strings.Contains(out, c.Summary) {
			t.Errorf("%s: output does not list %q with its summary %q:\n%s", what, n, c.Summary, out)
		}
	}
}

// WS2b-1b-b item 2: a word that starts registry commands but names none
// (a group such as "network", the name of a removed command) lists the
// group's commands: with --help on stdout and exit 0, else on stderr as a
// usage error (exit 2). A word that starts no command keeps the plain
// unknown-command error, and a word that is a command and a group ("tab")
// keeps its own help. None of these connects to a page.
func TestCommandGroupWordListsItsCommands(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	env := []string{"AGENT_IOS_WS_URL=" + mock.URL}
	for _, args := range [][]string{
		{"network", "--help"},
		{"network", "-h"},
		{"--json", "network", "--help"},
		{"network", "-d", "1", "--help"},
		{"help", "network"},
	} {
		r := runCLI(t, "", env, args...)
		if r.code != 0 || r.stderr != "" {
			t.Errorf("%v: exit %d, stderr %q; want the group list (exit 0)", args, r.code, r.stderr)
		}
		assertLists(t, strings.Join(args, " "), r.stdout, "network")
	}
	for _, group := range []string{"cdp", "daemon", "device", "window"} {
		r := runCLI(t, "", env, group, "--help")
		if r.code != 0 {
			t.Errorf("%s --help: exit %d, stderr %q; want the group list (exit 0)", group, r.code, r.stderr)
		}
		assertLists(t, group+" --help", r.stdout, group)
	}
	for _, args := range [][]string{{"network"}, {"network", "--follow"}, {"network", "-d", "1"}} {
		r := runCLI(t, "", env, args...)
		if r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, `"network" is a group of commands`) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the group list as a usage error (exit 2)", args, r.code, r.stdout, r.stderr)
		}
		assertLists(t, strings.Join(args, " "), r.stderr, "network")
	}
	for _, args := range [][]string{{"zz-no-such-group"}, {"zz-no-such-group", "--help"}, {"network", "zz-no-such-command"}, {"network", "zz-no-such-command", "--help"}} {
		r := runCLI(t, "", env, args...)
		if r.code != 2 || !strings.Contains(r.stderr, "unknown command") || strings.Contains(r.stderr, "network requests") {
			t.Errorf("%v: exit %d, stderr %q; want the plain unknown-command error (exit 2)", args, r.code, r.stderr)
		}
	}
	if r := runCLI(t, "", env, "help", "zz-no-such-group"); r.code == 0 {
		t.Errorf("help zz-no-such-group: exit 0, stdout %q; want a failure", r.stdout)
	}
	// "tab" is a command and a group: its own help, not the list.
	if r := runCLI(t, "", env, "tab", "--help"); r.code != 0 || !strings.HasPrefix(r.stdout, "Usage: agent-ios tab") || strings.Contains(r.stdout, "Commands of") {
		t.Errorf("tab --help: exit %d, stdout %q; want the help of the command tab", r.code, r.stdout)
	}
	if n := mock.ConnectionCount(); n != 0 {
		t.Fatalf("group lists opened %d page connections, want 0", n)
	}
}

// "help <group>" prints the group's list in process too (printCommandHelp,
// which main runs for "help", "--help" and "-h" with words); "help tab"
// stays the help of the command tab, and a word that starts no command
// reports false.
func TestPrintCommandHelpListsAGroup(t *testing.T) {
	var b bytes.Buffer
	if !knownHelp(&b, "network") {
		t.Fatal("printCommandHelp(network) reports an unknown command")
	}
	assertLists(t, "printCommandHelp(network)", b.String(), "network")
	b.Reset()
	if !knownHelp(&b, "tab") || !strings.HasPrefix(b.String(), "Usage: agent-ios tab") || strings.Contains(b.String(), "Commands of") {
		t.Errorf("printCommandHelp(tab): %q; want the help of the command tab", b.String())
	}
	b.Reset()
	if knownHelp(&b, "zz-no-such-group") || b.Len() != 0 {
		t.Errorf("printCommandHelp(zz-no-such-group) printed %q; want false and nothing", b.String())
	}
	if knownHelp(&b, "tab zz-none") {
		t.Error("printCommandHelp(tab zz-none) reports a command")
	}
	b.Reset()
	if err := printUsage(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "agent-ios <group> --help") {
		t.Errorf("the overview does not name the group help:\n%s", b.String())
	}
}
