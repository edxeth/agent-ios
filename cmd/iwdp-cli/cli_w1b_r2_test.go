package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// R2-2 at the CLI (D11, design 2.5), the production defaults: the CLI
// passes no transport or address, and the daemon was started with
// "--transport iwdp --addr <listing>" on a custom port. An explicit page
// URL of a listed physical page needs the device reservation (exit 6,
// before any connection), as the same page selected by discovery does.
func TestCLIExplicitURLOnTheDaemonListingNeedsReservation(t *testing.T) {
	env, f, _, _ := physicalPage(t, false, fakewebkit.Options{})
	wantExit(t, runCLI(t, "", env, "eval", "1+1", f.URL), 6, "device reserve --owner default")
	if n := f.ConnectionCount(); n != 0 {
		t.Fatalf("the refused explicit-URL attach dialed the page %d times", n)
	}
	if res := runCLI(t, "", env, "device", "reserve", "--owner", "default"); res.code != 0 {
		t.Fatalf("device reserve: exit %d, stderr %q", res.code, res.stderr)
	}
	res := runCLI(t, "", env, "eval", "1+1", f.URL)
	if res.code != 0 || strings.TrimSpace(res.stdout) != "2" {
		t.Fatalf("eval by URL with the reservation: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
}

// R2-6, design 2.4 and INV A10: with IWDP_NO_DAEMON=1 a command that sets
// owned page state (a forced pseudo state) resets it before its process
// exits, as a daemon does at release and shutdown. The fake page records
// the reset after the process ended.
func TestCLINoDaemonCommandResetsItsStateBeforeExit(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Result("CSS.forcePseudoState", map[string]any{})
	res := runCLI(t, "", []string{"IWDP_NO_DAEMON=1"}, "force-pseudo-state", "7", "hover", f.URL)
	if res.code != 0 {
		t.Fatalf("force-pseudo-state: exit %d, stderr %q", res.code, res.stderr)
	}
	var classes [][]string
	for _, r := range f.ReceivedMethod("CSS.forcePseudoState") {
		var p struct {
			NodeID  int      `json:"nodeId"`
			Classes []string `json:"forcedPseudoClasses"`
		}
		if err := json.Unmarshal(r.Params, &p); err != nil || p.NodeID != 7 {
			t.Fatalf("CSS.forcePseudoState params %s (%v), want node 7", r.Params, err)
		}
		classes = append(classes, p.Classes)
	}
	if len(classes) != 2 || len(classes[0]) != 1 || classes[0][0] != "hover" || len(classes[1]) != 0 {
		t.Fatalf("CSS.forcePseudoState calls %v, want [hover] then the reset []", classes)
	}
}
