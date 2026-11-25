package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edxeth/agent-ios/internal/webkit/testutil/fakewebkit"
)

// Reviews ws2a-r3-a F8 and ws2a-r3-b F7 (brief ws2b-1a, "Regression
// tests"): an unknown long flag on a command that also had a removed
// command of the same name (click, console, fill, navigate, reload,
// screenshot) fell back to that command, which ran or reached page
// discovery. Now it is the registry's usage error (exit 2), and the page
// receives nothing.
func TestUnknownLongFlagsExit2WithoutContactingAPage(t *testing.T) {
	env, f := reservedPage(t)
	before := len(f.Received())
	for _, args := range [][]string{
		{"click", "body", "--not-a-flag"},
		{"console", "--not-a-flag"},
		{"fill", "#a", "x", "--not-a-flag"},
		{"navigate", "https://example.test/", "--not-a-flag"},
		{"reload", "--not-a-flag"},
		{"screenshot", "--not-a-flag"},
	} {
		r := runCLI(t, "", env, args...)
		if r.code != 2 || !strings.Contains(r.stderr, "unknown flag --not-a-flag") || r.stdout != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the usage error naming the flag (exit 2)", args, r.code, r.stdout, r.stderr)
		}
	}
	if n := len(f.Received()) - before; n != 0 {
		t.Errorf("the rejected lines sent %d messages to the page, want 0", n)
	}
}

// Review ws2a-r3-b F7: "click body --bogus" exited 0 and clicked through
// the removed command. It exits 2 and evaluates nothing in the page.
func TestUnknownFlagDoesNotClick(t *testing.T) {
	env, f := reservedPage(t)
	f.Handle("Runtime.evaluate", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
	})
	r := runCLI(t, "", env, "click", "body", "--bogus")
	if n := len(f.ReceivedMethod("Runtime.evaluate")); r.code != 2 || n != 0 {
		t.Fatalf("click body --bogus: exit %d, stdout %q, stderr %q, evaluations %d; want exit 2 and none", r.code, r.stdout, r.stderr, n)
	}
}

// Review ws2a-r3-b F5: a ws:// value of a global option was taken for the
// page of a removed command (exit 1). It is the usage error of that
// option's value (exit 2), and no daemon starts.
func TestURLValuedGlobalOptionsAreUsageErrors(t *testing.T) {
	env := privateDaemon(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"screenshot", "--timeout", "ws://example.test"}, `--timeout "ws://example.test"`},
		{[]string{"screenshot", "--transport", "ws://example.test"}, `--transport "ws://example.test"`},
		{[]string{"screenshot", "--wait-lease", "ws://example.test"}, `--wait-lease "ws://example.test"`},
		{[]string{"screenshot", "--session", "ws://example.test", "--timeout", "bogus"}, `--timeout "bogus"`},
	} {
		r := runCLI(t, "", env, tc.args...)
		if r.code != 2 || !strings.Contains(r.stderr, tc.want) || strings.Contains(r.stderr, "page") || r.stdout != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the usage error %s (exit 2), not a page", tc.args, r.code, r.stdout, r.stderr, tc.want)
		}
	}
	dir := strings.TrimPrefix(env[0], "XDG_RUNTIME_DIR=")
	if _, err := os.Stat(filepath.Join(dir, "agent-ios", "daemon.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a usage error started the daemon: stat of its socket: %v", err)
	}
}

// WS2b: the page URL forms of the removed commands (a trailing ws:// URL,
// AGENT_IOS_WS_URL) and a line with only flags are usage errors (exit 2) that
// contact no page.
func TestRemovedPageURLFormsAreUsageErrors(t *testing.T) {
	env, f := reservedPage(t)
	before := len(f.Received())
	for _, tc := range []struct {
		env  []string
		args []string
		want string
	}{
		{env, []string{"screenshot", f.URL}, "--page"},
		{env, []string{"reload", f.URL}, "--page"},
		{with(env, "AGENT_IOS_WS_URL="+f.URL), []string{"screenshot"}, "AGENT_IOS_WS_URL"},
		{with(env, "AGENT_IOS_WS_URL="+f.URL), []string{"click", "body"}, "AGENT_IOS_WS_URL"},
		{env, []string{"--bogus"}, "no command"},
	} {
		r := runCLI(t, "", tc.env, tc.args...)
		if r.code != 2 || !strings.Contains(r.stderr, tc.want) {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the usage error naming %s (exit 2)", tc.args, r.code, r.stdout, r.stderr, tc.want)
		}
	}
	if n := len(f.Received()) - before; n != 0 {
		t.Errorf("the refused lines sent %d messages to the page, want 0", n)
	}
}
