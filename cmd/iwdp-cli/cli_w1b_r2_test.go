package main

import (
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
	wantExit(t, runCLI(t, "", env, "eval", "1+1", "--page", f.URL), 6, "device reserve --owner default")
	if n := f.ConnectionCount(); n != 0 {
		t.Fatalf("the refused explicit-URL attach dialed the page %d times", n)
	}
	if res := runCLI(t, "", env, "device", "reserve", "--owner", "default"); res.code != 0 {
		t.Fatalf("device reserve: exit %d, stderr %q", res.code, res.stderr)
	}
	res := runCLI(t, "", env, "eval", "1+1", "--page", f.URL)
	if res.code != 0 || strings.TrimSpace(res.stdout) != "2" {
		t.Fatalf("eval by URL with the reservation: exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
	}
}

// R2-6, design 2.4 and INV A10: with IWDP_NO_DAEMON=1 a command that sets
// owned page state (the enabled Console and Debugger domains of
// setRegistryCommand) resets it before its process exits, as a daemon does
// at release and shutdown. The fake page records the reset after the
// process ended.
func TestCLINoDaemonCommandResetsItsStateBeforeExit(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	res := runCLI(t, "", []string{"IWDP_NO_DAEMON=1"}, append(strings.Fields(setRegistryCommand), "--page", f.URL)...)
	if res.code != 0 || strings.TrimSpace(res.stdout) != "set" {
		t.Fatalf("%s: exit %d, stdout %q, stderr %q", setRegistryCommand, res.code, res.stdout, res.stderr)
	}
	for _, m := range []string{"Console.disable", "Debugger.disable"} {
		if n := len(f.ReceivedMethod(m)); n != 1 {
			t.Errorf("%s sent %d times before the exit, want 1 (the reset of the command's state)", m, n)
		}
	}
}
