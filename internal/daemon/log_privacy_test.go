package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Device check 3, P2-C: the daemon log held the title of the user's tab
// ("cdp: issued an endpoint for session ..., page \"<title>\"" and
// "page <title>: page connection closed unexpectedly"). The log is a file
// on disk, so it names a page by its id (the iwdp WebSocket URL, a CDP
// target id), never by its title or URL. Both reported paths run here: a
// CDP endpoint is issued for the page, and the page connection is lost.
func TestDaemonLogNamesNoPageTitleOrURL(t *testing.T) {
	const title = "Private Title 7f3a"
	const host = "private-host-7f3a.example"
	f := newFake(t, fakewebkit.Options{})
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("SIMULATOR", "Simulator", "16.7")
	dev.AddPage(title, "https://"+host+"/account?id=42", f.URL)
	var log lockedBuffer
	c := connect(t, startInProcess(t, RunOptions{Transport: "iwdp", Addr: l.Addr(), Log: &log}))
	mustRunOn(t, c, "s1", "", "cdp", "start", "--port", "0")
	mustRunOn(t, c, "s1", "", "get", "cdp-url")
	pc, err := c.OpenPage(ctxFor(t, 10*time.Second), PageOptions{Page: PageReq{Page: title}, Owner: Owner{Session: "s1"}, Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	if _, err := pc.Send(ctxFor(t, 5*time.Second), "Runtime.evaluate", map[string]any{"expression": "1"}); err != nil {
		t.Fatal(err)
	}
	f.CloseConnections()
	select {
	case <-pc.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the page connection loss was not reported")
	}
	for end := time.Now().Add(5 * time.Second); !strings.Contains(log.String(), "page connection closed unexpectedly"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("no connection-loss line in the daemon log:\n%s", log.String())
		}
	}
	out := log.String()
	if !strings.Contains(out, "issued an endpoint") {
		t.Fatalf("no issue line in the daemon log:\n%s", out)
	}
	for _, private := range []string{title, host, "/account"} {
		if strings.Contains(out, private) {
			t.Errorf("the daemon log names the page by %q:\n%s", private, out)
		}
	}
	// The page is still named: by its id, the WebSocket URL of the listing.
	if !strings.Contains(out, "page "+f.URL+": page connection closed unexpectedly") {
		t.Errorf("the connection-loss line does not name the page by its id %s:\n%s", f.URL, out)
	}
}
