package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Device check 3, P3-D: "iwdp-cli status" names the log of an iwdp that
// iwdp-mcp starts (proxy.LogPath), or why it is unknown.
func TestStatusNamesTheIWDPLog(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if got, want := iwdpLogLine(), "iwdp log of an iwdp that iwdp-mcp starts: "+filepath.Join(state, "iwdp-mcp", "iwdp.log"); got != want {
		t.Fatalf("iwdpLogLine() = %q, want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	if got := iwdpLogLine(); !strings.Contains(got, "unknown") || !strings.Contains(got, "no XDG_STATE_HOME and no home directory") {
		t.Fatalf("iwdpLogLine() without a state or home directory = %q, want the reason", got)
	}
}
