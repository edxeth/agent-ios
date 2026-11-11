package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// WS2 round-2 review F6, F7 and F8 (part 1), ported from the reviewer's
// TestReviewR2DaemonRejectsHelpBeforeLease, TestReviewR2DaemonDoesNotRunHelp
// and TestReviewR2OverflowingExpiryDoesNotLease: a /v1/run request is a
// usage error (exit 2) with no page work, no attach and no lease, when it
// asks for help (a help invocation runs nothing; before, the daemon ran
// "console clear --help" and cleared the console), also for the overview
// or a group's help, or when an option value
// is invalid (Command.Check, also with --help), including an --expires
// whose milliseconds overflow to +Inf.
func TestDaemonRefusesHelpAndBadValuesBeforePageWork(t *testing.T) {
	huge := "1" + strings.Repeat("0", 306)
	for _, tc := range []struct {
		argv []string
		want string // in the error
	}{
		{[]string{"console", "clear", "--help"}, "runs no help request"},
		{[]string{"eval", "1", "-h"}, "runs no help request"},
		// The overview and group help (round-3 reviews: they returned
		// "no command" with exit 1).
		{[]string{"--help"}, "runs no help request"},
		{[]string{"-h"}, "runs no help request"},
		{[]string{"-h", "--help"}, "runs no help request"},
		{[]string{"network", "--help"}, "runs no help request"},
		{[]string{"daemon", "--help"}, "runs no help request"},
		{[]string{"cookies", "set", "--same-site=bogus", "--help"}, `invalid sameSite "bogus"`},
		{[]string{"cookies", "set", "a", "b", "--domain=example.com", "--expires=" + huge}, "out of range"},
		{[]string{"cookies", "set", "a", "b", "--domain=example.com", "--expires=" + huge, "--help"}, "out of range"},
	} {
		o := startInProcess(t, RunOptions{})
		c := connect(t, o)
		f := newFake(t, fakewebkit.Options{})
		req := RunRequest{Argv: tc.argv, PageReq: PageReq{Page: f.URL}, Owner: Owner{Session: "help"}}
		_, err := c.Run(ctxFor(t, 5*time.Second), req, nil)
		if commands.ExitCodeOf(err) != commands.ExitUsage || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%.60q: %v (exit %d); want a usage error naming %q", tc.argv, err, commands.ExitCodeOf(err), tc.want)
		}
		if n := f.ConnectionCount(); n != 0 {
			t.Errorf("%.60q: the daemon dialed the page %d time(s); want no page work", tc.argv, n)
		}
		if n := len(f.ReceivedMethod("Console.clearMessages")); n != 0 {
			t.Errorf("%.60q: Console.clearMessages reached the page %d time(s); want none", tc.argv, n)
		}
		st, err := c.Status(ctxFor(t, 5*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Pages) != 0 || st.Daemon.Leases != 0 {
			t.Errorf("%.60q: the daemon has pages %+v and %d lease(s); want none", tc.argv, st.Pages, st.Daemon.Leases)
		}
	}
}
