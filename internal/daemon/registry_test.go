package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

func run(t *testing.T, c *Client, session string, argv ...string) (RunResult, error) {
	t.Helper()
	return c.Run(ctxFor(t, 5*time.Second), RunRequest{Argv: argv, Owner: Owner{Session: session}}, nil)
}

func leaseFake(t *testing.T, c *Client, session string, sends ...string) *fakewebkit.Server {
	t.Helper()
	f := newFake(t, fakewebkit.Options{})
	if err := leasePage(t, c, f.URL, Owner{Session: session}, 5*time.Second, sends...); err != nil {
		t.Fatal(err)
	}
	return f
}

// 7.3 item 8 (registry part): close ends only the caller's session and runs
// its cleanup; close --all is a usage error (exit 2) that changes nothing.
func TestCloseEndsOnlyTheCallersSession(t *testing.T) {
	c := connect(t, startInProcess(t, RunOptions{}))
	f1 := leaseFake(t, c, "s1", "Network.enable")
	f2 := leaseFake(t, c, "s2", "Network.enable")
	if _, err := run(t, c, "s1", "close", "--all"); commands.ExitCodeOf(err) != 2 {
		t.Fatalf("close --all: %v (exit %d), want 2", err, commands.ExitCodeOf(err))
	}
	if st, _ := c.Status(ctxFor(t, 2*time.Second)); len(st.Pages) != 2 {
		t.Fatalf("close --all changed the leases: %+v", st.Pages)
	}
	res, err := run(t, c, "s1", "close")
	if err != nil || !strings.Contains(res.Text, "released 1 page lease") {
		t.Fatalf("close: %+v, %v", res, err)
	}
	st, err := c.Status(ctxFor(t, 2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range st.Pages {
		if p.Session == "s1" {
			t.Fatalf("s1 still holds %s after close", p.Key)
		}
	}
	if len(f1.ReceivedMethod("Network.disable")) != 1 || len(f2.ReceivedMethod("Network.disable")) != 0 {
		t.Fatalf("cleanup: s1's page got %d Network.disable, s2's page %d; want 1 and 0",
			len(f1.ReceivedMethod("Network.disable")), len(f2.ReceivedMethod("Network.disable")))
	}
	info, err := run(t, c, "s2", "session", "info")
	if err != nil || !strings.Contains(info.Text, "1 leased page") || !strings.Contains(info.Text, "lease active") {
		t.Fatalf("session info of s2: %+v, %v", info, err)
	}
	list, err := run(t, c, "s2", "session", "list")
	if err != nil || strings.TrimSpace(list.Text) != "s2\t1 page(s)" {
		t.Fatalf("session list: %q, %v", list.Text, err)
	}
	if name, err := run(t, c, "s2", "session"); err != nil || name.Text != "s2" {
		t.Fatalf("session: %+v, %v", name, err)
	}
}

// lease break ends another session's lease; the page is then free.
func TestLeaseBreak(t *testing.T) {
	c := connect(t, startInProcess(t, RunOptions{}))
	f := leaseFake(t, c, "s1")
	if err := leasePage(t, c, f.URL, Owner{Session: "s2"}, time.Second); commands.ExitCodeOf(err) != 4 {
		t.Fatalf("s2 before the break: %v", err)
	}
	if _, err := run(t, c, "s2", "lease", "break", "no-such-page"); commands.ExitCodeOf(err) != 5 {
		t.Fatalf("break of an unknown page: %v (exit %d), want 5", err, commands.ExitCodeOf(err))
	}
	if _, err := run(t, c, "s2", "lease", "break", f.URL); err != nil {
		t.Fatalf("lease break: %v", err)
	}
	if err := leasePage(t, c, f.URL, Owner{Session: "s2"}, 2*time.Second); err != nil {
		t.Fatalf("s2 after the break: %v", err)
	}
}

// device reserve, list and release (design 2.5 rule 1): a second owner is
// refused with exit 6.
func TestDeviceReservationCommands(t *testing.T) {
	c := connect(t, startInProcess(t, RunOptions{}))
	res, err := run(t, c, "w1", "device", "reserve", "--owner", "orchestrator", "--ttl", "1h")
	if err != nil || !strings.Contains(res.Text, "reserved by \"orchestrator\"") {
		t.Fatalf("reserve: %+v, %v", res, err)
	}
	if _, err := run(t, c, "w2", "device", "reserve"); commands.ExitCodeOf(err) != 6 {
		t.Fatalf("second owner: %v (exit %d), want 6", err, commands.ExitCodeOf(err))
	}
	list, err := run(t, c, "w1", "device", "list")
	if err != nil || !strings.Contains(list.Text, "orchestrator") {
		t.Fatalf("list: %+v, %v", list, err)
	}
	if _, err := c.Run(ctxFor(t, 5*time.Second), RunRequest{Argv: []string{"device", "release"}, Owner: Owner{Session: "w1", Reservation: "orchestrator"}}, nil); err != nil {
		t.Fatalf("release with the reservation owner: %v", err)
	}
	list, err = run(t, c, "w1", "device", "list")
	if err != nil || !strings.Contains(list.Text, "No device is reserved") {
		t.Fatalf("list after release: %+v, %v", list, err)
	}
}
