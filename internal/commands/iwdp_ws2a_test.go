package commands

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/proxy"
)

// Device check 3, P3-D: "iwdp-cli status" names the log of an iwdp that
// iwdp-cli starts (proxy.LogPath), or why it is unknown. (Moved with
// status from cmd/iwdp-cli, WS2a.)
func TestStatusNamesTheIWDPLog(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if got, want := iwdpLogLine(proxy.LogPath()), "iwdp log of an iwdp that iwdp-cli starts: "+filepath.Join(state, "iwdp-cli", "iwdp.log"); got != want {
		t.Fatalf("iwdpLogLine = %q, want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	if got := iwdpLogLine(proxy.LogPath()); !strings.Contains(got, "unknown") || !strings.Contains(got, "no XDG_STATE_HOME and no home directory") {
		t.Fatalf("iwdpLogLine without a state or home directory = %q, want the reason", got)
	}
}

// The three iwdp commands need no page and run in the client (WS2a brief,
// section 3); the help of restart-iwdp states which iwdp it may stop.
func TestIWDPCommandsAreLocalRegistryCommands(t *testing.T) {
	for _, name := range []string{"status", "devices", "restart-iwdp"} {
		c, n := Lookup([]string{name})
		if c == nil || n != 1 || c.Name != name {
			t.Errorf("%s is not a registry command", name)
			continue
		}
		if !c.Local || c.Page != NoPage || len(c.Args) != 0 || len(c.Flags) != 0 {
			t.Errorf("%s: local %v, page %v, args %v, flags %v; want a Local command without page, arguments or flags", name, c.Local, c.Page, c.Args, c.Flags)
		}
	}
}
