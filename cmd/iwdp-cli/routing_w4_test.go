package main

import (
	"context"
	"strings"
	"testing"

	reg "github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// routeProbe is a registry command under the legacy word "dom" that only
// this test binary has. It shows that the routing rule of main reaches
// registry commands under a legacy word without naming them (W4-2b's
// "cookies" subcommands are such commands).
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
// subcommand under the legacy word "cookies", while the registry has no
// bare "cookies". The name avoids a clash with the real command when
// W4-2b's files are merged.
func init() {
	reg.Register(&reg.Command{
		Name:    "cookies zz-clear-probe",
		Summary: "test fixture: a registry subcommand of the legacy cookies command",
		Flags:   []reg.Flag{{Name: "domain", Type: reg.FlagString, Help: "printed back"}},
		Local:   true,
		Run: func(_ context.Context, _ reg.Env, in reg.Input) (reg.Result, error) {
			d, _ := in.Flags["domain"].(string)
			return reg.Result{Text: "cookies-probe:" + d}, nil
		},
	})
}

// Orchestrator note on W4-2b: the routing rule checks that the line's
// words name a registry command, not only that the registry has some
// command under the first word. With a registry "cookies <sub>" next to
// the legacy "cookies", bare "cookies" and its legacy flags stay with
// the legacy listing; "cookies <sub>" goes to the registry.
func TestRoutingKeepsBareCookiesLegacy(t *testing.T) {
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
		if r.code != 0 || !strings.Contains(r.stdout, "sid=") || strings.Contains(r.stdout, "cookies-probe") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the legacy cookie listing", args, r.code, r.stdout, r.stderr)
		}
	}
}

// W4-2a decision 2 (design 2.10, 7.3 test 13): a first word that names a
// legacy command and registry commands goes to the registry when the
// registry owns the line, and to the legacy command otherwise.
func TestRoutingBetweenLegacyAndRegistryCommands(t *testing.T) {
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
	if n := mock.ConnectionCount(); n != 0 {
		t.Fatalf("usage errors and help opened %d page connections, want 0", n)
	}
	// Legacy lines stay legacy: the legacy "console" rejects its unknown
	// option with exit 1 (pinned by TestStreamRejectsUnknownOptionBeforeConnecting),
	// and "dom <selector>" is the legacy DOM command, which connects.
	if r := runCLI(t, "", env, "console", "--forever"); r.code != 1 || !strings.Contains(r.stderr, "Usage: iwdp-cli console [-d 10s] [--follow] [ws-url]") {
		t.Errorf("console --forever: exit %d, stderr %q; want the legacy usage error", r.code, r.stderr)
	}
	mock.HandleFunc("DOM.getDocument", map[string]any{"root": map[string]any{"nodeId": 1, "nodeName": "#document", "nodeType": 9}})
	if r := runCLI(t, "", env, "--timeout", "5s", "dom", "body"); strings.Contains(r.stdout+r.stderr, "probe") || mock.ConnectionCount() == 0 {
		t.Errorf("dom body: exit %d, stdout %q, stderr %q, connections %d; want the legacy DOM command", r.code, r.stdout, r.stderr, mock.ConnectionCount())
	}
}

// Design 2.11: the registry's global flags may stand anywhere before
// "--", also before or between the command words. "network --json
// requests" is the registry command "network requests", not the legacy
// stream "network" (W4 command review, finding main.go:203). Lines
// without registry command words stay legacy.
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
	// The legacy stream keeps its help and its own flags.
	for _, args := range [][]string{{"network", "--help"}, {"network", "-d", "1", "--help"}} {
		if r := runCLI(t, "", env, args...); r.code != 0 || !strings.HasPrefix(r.stdout, "Usage: iwdp-cli network [-d 10s]") {
			t.Errorf("%v: exit %d, stdout %q; want the legacy network help", args, r.code, r.stdout)
		}
	}
	if n := mock.ConnectionCount(); n != 0 {
		t.Fatalf("help and usage errors opened %d page connections, want 0", n)
	}
}
