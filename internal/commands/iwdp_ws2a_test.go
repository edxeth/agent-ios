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

// The three iwdp commands need no page, arguments or flags (WS2a brief,
// section 3); the help of restart-iwdp states which iwdp it may stop.
// status and restart-iwdp run in the client; devices runs in the daemon,
// which is then, without IWDP_NO_DAEMON=1, the only process that starts
// iwdp automatically, so its start back-off covers every automatic start
// (review of 2026-11-12; docs/known-limits.md).
func TestIWDPCommandsAreLocalRegistryCommands(t *testing.T) {
	for name, local := range map[string]bool{"status": true, "devices": false, "restart-iwdp": true} {
		c, n := Lookup([]string{name})
		if c == nil || n != 1 || c.Name != name {
			t.Errorf("%s is not a registry command", name)
			continue
		}
		if c.Local != local || c.Page != NoPage || len(c.Args) != 0 || len(c.Flags) != 0 {
			t.Errorf("%s: local %v, page %v, args %v, flags %v; want local %v without page, arguments or flags", name, c.Local, c.Page, c.Args, c.Flags, local)
		}
	}
}
