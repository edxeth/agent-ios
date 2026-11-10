package main

import (
	"context"
	"strings"
	"testing"

	reg "github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// routeProbe is a registry command under the word "dom", which names no
// command alone (it named a removed command), that only this test binary
// has. It shows that the registry finds a command by all its words.
func init() {
	reg.Register(&reg.Command{
		Name:    "dom zz-route-probe",
		Summary: "test fixture: prints its argument",
		Args:    []reg.Arg{{Name: "word", Help: "printed back"}},
		Flags:   []reg.Flag{{Name: "upper", Type: reg.FlagBool, Help: "print in upper case"}},
		Local:   true,
		Run: func(_ context.Context, _ reg.Env, in reg.Input) (reg.Result, error) {
			w := "probe:" + strings.Join(in.Args, ",")
			if b, _ := in.Flags["upper"].(bool); b {
				w = strings.ToUpper(w)
			}
			return reg.Result{Text: w}, nil
		},
	})
}

// cookiesProbe stands in for W4-2b's "cookies clear": a registry
// subcommand under the word "cookies", which is a command alone too. The
// name avoids a clash with the real command.
func init() {
	reg.Register(&reg.Command{
		Name:    "cookies zz-clear-probe",
		Summary: "test fixture: a registry subcommand of the cookies command",
		Flags:   []reg.Flag{{Name: "domain", Type: reg.FlagString, Help: "printed back"}},
		Local:   true,
		Run: func(_ context.Context, _ reg.Env, in reg.Input) (reg.Result, error) {
			d, _ := in.Flags["domain"].(string)
			return reg.Result{Text: "cookies-probe:" + d}, nil
		},
	})
}

// Orchestrator note on W4-2b: the line's words name the command, not only
// its first word: "cookies <sub>" is the subcommand. The bare "cookies" is
// the listing (TestBareCookiesRunsTheRegistryListing): a line that names
// its page by URL (IWDP_WS_URL here, or a ws:// argument) is a usage error
// (exit 2) that names --page, before any connection and without a value.
func TestRoutingSendsCookiesByURLToTheRegistry(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Page.getCookies", map[string]any{"cookies": []webkit.Cookie{
		{Name: "sid", Value: secretValue, Domain: ".example.com", Path: "/", Session: true, SameSite: "Lax"},
	}})
	env := []string{"IWDP_WS_URL=" + mock.URL}
	if r := runCLI(t, "", env, "cookies", "zz-clear-probe", "--domain", "a.test"); r.code != 0 || strings.TrimSpace(r.stdout) != "cookies-probe:a.test" {
		t.Fatalf("cookies zz-clear-probe: exit %d, stdout %q, stderr %q; want the registry fixture", r.code, r.stdout, r.stderr)
	}
	for _, args := range [][]string{{"cookies"}, {"cookies", "--show-values"}, {"cookies", mock.URL}} {
		r := runCLI(t, "", env, args...)
		if r.code != 2 || !strings.Contains(r.stderr, "--page") || strings.Contains(r.stdout+r.stderr, "s3cr") || strings.Contains(r.stdout, "cookies-probe") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the registry usage error that names --page (exit 2)", args, r.code, r.stdout, r.stderr)
		}
	}
	if n := mock.ConnectionCount(); n != 0 {
		t.Errorf("the refused cookies lines connected to the page %d times, want 0", n)
	}
}

// W4-2a decision 2 (design 2.10) and WS2b: iwdp-cli runs registry
// commands only. A registry command under a word that named a removed
// command runs; a malformed line or a removed command is the registry's
// usage error (exit 2) before any connection.
func TestRoutingRunsOnlyRegistryCommands(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	env := []string{"IWDP_WS_URL=" + mock.URL}

	// A registry command under a legacy word runs, with its flags.
	if r := runCLI(t, "", env, "dom", "zz-route-probe", "--upper", "x"); r.code != 0 || strings.TrimSpace(r.stdout) != "PROBE:X" {
		t.Fatalf("dom zz-route-probe --upper x: exit %d, stdout %q, stderr %q; want the registry fixture", r.code, r.stdout, r.stderr)
	}
	// A registry subcommand with an unknown flag is a registry usage
	// error: exit 2 before any connection.
	for _, args := range [][]string{{"dom", "zz-route-probe", "--bogus"}, {"network", "requests", "--bogus"}, {"network", "route"}} {
		if r := runCLI(t, "", env, args...); r.code != 2 || !strings.Contains(r.stderr, "iwdp-cli "+args[0]+" "+args[1]+" --help") {
			t.Errorf("%v: exit %d, stderr %q; want the registry usage error (exit 2)", args, r.code, r.stderr)
		}
	}
	// Help of the registry forms comes from the registry, without a connection.
	for _, args := range [][]string{{"network", "requests", "--help"}, {"console", "--help"}, {"console", "clear", "-h"}, {"network", "har", "stop", "--help"}} {
		name := strings.Join(args[:len(args)-1], " ")
		if r := runCLI(t, "", env, args...); r.code != 0 || !strings.HasPrefix(r.stdout, "Usage: iwdp-cli "+name) {
			t.Errorf("%v: exit %d, stdout %q; want the registry help of %q", args, r.code, r.stdout, name)
		}
	}
	// The removed forms: the stream flags of "console" are unknown flags,
	// and "dom <selector>" and the other removed commands are unknown
	// commands.
	if r := runCLI(t, "", env, "console", "--forever"); r.code != 2 || !strings.Contains(r.stderr, "unknown flag --forever") {
		t.Errorf("console --forever: exit %d, stderr %q; want the registry usage error (exit 2)", r.code, r.stderr)
	}
	for _, args := range [][]string{{"--timeout", "5s", "dom", "body"}, {"pages"}, {"set-breakpoint", "https://fixture.example/app.js", "3"}, {"console-messages", "-d", "1"}} {
		if r := runCLI(t, "", env, args...); r.code != 2 || !strings.Contains(r.stderr, "unknown command") || strings.Contains(r.stdout, "probe") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the unknown-command usage error (exit 2)", args, r.code, r.stdout, r.stderr)
		}
	}
	if n := mock.ConnectionCount(); n != 0 {
		t.Fatalf("usage errors and help opened %d page connections, want 0", n)
	}
}

// Design 2.11: the registry's global flags may stand anywhere before
// "--", also before or between the command words. "network --json
// requests" is the registry command "network requests", not the legacy
// stream "network" (W4 command review, finding main.go:203). Since WS2b
// "network" alone names no command but a group of commands, so its help
// lists the group (TestCommandGroupWordListsItsCommands).
func TestRoutingWithGlobalFlagsBetweenCommandWords(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	env := []string{"IWDP_WS_URL=" + mock.URL}
	for _, args := range [][]string{
		{"network", "--json", "requests", "--help"},
		{"--json", "network", "requests", "--help"},
		{"network", "--max-output", "100", "requests", "--help"},
		{"network", "--page=t1", "requests", "--help"},
		{"network", "--content-boundaries", "--json", "requests", "-h"},
		{"network", "--timeout", "5s", "requests", "--help"},
	} {
		if r := runCLI(t, "", env, args...); r.code != 0 || !strings.HasPrefix(r.stdout, "Usage: iwdp-cli network requests") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the registry help of network requests", args, r.code, r.stdout, r.stderr)
		}
	}
	if r := runCLI(t, "", env, "network", "--json", "requests", "--bogus"); r.code != 2 || !strings.Contains(r.stderr, "iwdp-cli network requests --help") {
		t.Errorf("network --json requests --bogus: exit %d, stderr %q; want the registry usage error (exit 2)", r.code, r.stderr)
	}
	for _, args := range [][]string{{"network", "--help"}, {"network", "-d", "1", "--help"}} {
		if r := runCLI(t, "", env, args...); r.code != 0 || !strings.Contains(r.stdout, `Commands of "network"`) || !strings.Contains(r.stdout, "network requests") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the list of the group network (exit 0)", args, r.code, r.stdout, r.stderr)
		}
	}
	if n := mock.ConnectionCount(); n != 0 {
		t.Fatalf("help and usage errors opened %d page connections, want 0", n)
	}
}
