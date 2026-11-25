package commands

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// WS2b (brief ws2b-1a, "AGENT_IOS_WS_URL"): the WS2a rule of eval and cookies
// holds for every page command, now that no command takes its page from
// AGENT_IOS_WS_URL: with the variable set and no --page, a command whose Page
// is not NoPage exits 2 with an error that names the command, the
// variable and --page, before any connection (newEnv is not called).
func TestIWDPWSURLNeedsThePageFlagForEveryPageCommand(t *testing.T) {
	t.Setenv("AGENT_IOS_WS_URL", "ws://127.0.0.1:9/devtools/page/1")
	pageCommands := 0
	for _, c := range All() {
		if c.Page == NoPage {
			continue
		}
		pageCommands++
		argv := strings.Fields(c.Name)
		for _, a := range c.Args {
			if a.Required {
				argv = append(argv, "x")
			}
		}
		code, stdout, stderr, envCalls := runCLI(argv...)
		if code != ExitUsage || envCalls != 0 || stdout != "" || !strings.Contains(stderr, c.Name+": AGENT_IOS_WS_URL") || !strings.Contains(stderr, "--page") {
			t.Errorf("%q with AGENT_IOS_WS_URL: exit %d, env calls %d, stdout %q, stderr %q; want exit 2 naming the command, AGENT_IOS_WS_URL and --page before any connection", argv, code, envCalls, stdout, stderr)
		}
	}
	if pageCommands < 50 {
		t.Fatalf("only %d page commands are registered; the registry did not load", pageCommands)
	}
	// A command without a page ignores the variable.
	if code, _, stderr, envCalls := runCLI("skills", "list"); code != ExitOK || envCalls != 1 || strings.Contains(stderr, "AGENT_IOS_WS_URL") {
		t.Errorf("skills list with AGENT_IOS_WS_URL: exit %d, env calls %d, stderr %q; want the run", code, envCalls, stderr)
	}
	// Help comes first.
	if code, stdout, _, envCalls := runCLI("click", "x", "--help"); code != ExitOK || envCalls != 0 || !strings.Contains(stdout, "Usage: agent-ios click") {
		t.Errorf("click --help with AGENT_IOS_WS_URL: exit %d, env calls %d, stdout %q; want the help", code, envCalls, stdout)
	}
}

// WS2b (brief ws2b-1a, the removed trailing-URL convention): a page URL
// as a command's last argument is a usage error that names --page, also
// in the place of the screenshot's optional file, so that a line of an
// earlier version ("screenshot <ws:// URL>") never captures the session's
// page instead. The check of the file runs in the CLI (ClientStep) and in
// the daemon (Run).
func TestPageURLArgumentNeedsThePageFlag(t *testing.T) {
	for _, argv := range [][]string{
		{"screenshot", "ws://127.0.0.1:9224/devtools/page/1"},
		{"screenshot", "-o", "wss://h/devtools/page/1"},
		{"reload", "ws://127.0.0.1:9224/devtools/page/1"},
		{"click", "#a", "ws://127.0.0.1:9224/devtools/page/1"},
		{"fill", "#a", "v", "ws://127.0.0.1:9224/devtools/page/1"},
	} {
		code, _, stderr, envCalls := runCLI(argv...)
		if code != ExitUsage || envCalls != 0 || !strings.Contains(stderr, "--page <ws:// URL>") || !strings.Contains(stderr, argv[len(argv)-1]) {
			t.Errorf("%q: exit %d, env calls %d, stderr %q; want a usage error that names the URL and --page before any connection", argv, code, envCalls, stderr)
		}
	}
	if _, err := runScreenshot(context.Background(), fakeEnv{}, Input{Args: []string{"ws://127.0.0.1:9224/devtools/page/1"}, Flags: map[string]any{}}); ExitCodeOf(err) != ExitUsage || !strings.Contains(fmt.Sprint(err), "--page") {
		t.Errorf("screenshot <ws:// URL> in the daemon: %v; want the usage error that names --page", err)
	}
}
