package main

import (
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// W3b final round: a word that is a legacy command and a registry command
// alone (screenshot, click, reload, navigate) goes to the registry when the
// registry parses the line, except when the line names its page by URL (a
// ws:// argument or IWDP_WS_URL), which only legacy commands support
// (design 7.3 test 13: the legacy CLI tests run unchanged).
func TestRoutingKeepsURLLinesLegacy(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	for _, tc := range []struct {
		env      []string
		args     []string
		registry bool
	}{
		{nil, []string{"screenshot", "--help"}, true},
		{nil, []string{"click", "#a", "--help"}, true},
		{nil, []string{"screenshot", mock.URL, "--help"}, false},
		{[]string{"IWDP_WS_URL=" + mock.URL}, []string{"screenshot", "--help"}, false},
		{[]string{"IWDP_WS_URL=" + mock.URL}, []string{"click", "#a", "--help"}, false},
	} {
		r := runCLI(t, "", tc.env, tc.args...)
		// Only the registry commands have --force (click) and --annotate
		// (screenshot).
		isRegistry := strings.Contains(r.stdout, "--annotate") || strings.Contains(r.stdout, "--force")
		if r.code != 0 || !strings.HasPrefix(r.stdout, "Usage: iwdp-cli "+tc.args[0]) || isRegistry != tc.registry {
			t.Errorf("%v with %v: exit %d, registry help %v, stdout %q; want registry %v", tc.args, tc.env, r.code, isRegistry, r.stdout, tc.registry)
		}
	}
	if n := mock.ConnectionCount(); n != 0 {
		t.Fatalf("help opened %d page connections, want 0", n)
	}
}
