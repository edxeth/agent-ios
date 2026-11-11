package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// Review devfix-R1 F1 (criterion 1 of device check 3, P2-C): page content
// must not reach the daemon log, including error paths.
func TestReviewPrivacyCleanupError(t *testing.T) {
	const title = "Private Review Title"
	const pageURL = "https://private-review.example/account?secret=fixture"
	f := newFake(t, fakewebkit.Options{})
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("SIMULATOR", "Simulator", "16.7")
	dev.AddPage(title, pageURL, f.URL)
	var log lockedBuffer
	c := connect(t, startInProcess(t, RunOptions{Transport: "iwdp", Addr: l.Addr(), Log: &log}))
	mustRunOn(t, c, "review", "", "console")
	f.Handle("Console.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "cleanup failed on " + pageURL + " (" + title + ")"}
	})
	_, err := c.Run(ctxFor(t, 5*time.Second), RunRequest{Argv: []string{"session", "release"}, Owner: Owner{Session: "review"}}, nil)
	if err == nil {
		t.Fatal("fixture cleanup must fail")
	}
	out := log.String()
	if strings.Contains(out, title) || strings.Contains(out, pageURL) || strings.Contains(out, "private-review.example") {
		t.Fatalf("daemon persisted page content from cleanup failure:\n%s", out)
	}
	// The log still classifies the failure: the release line names the
	// operation in its format, and logsafe.Error adds the method of the
	// request and the WebKit protocol error code (review devfix-R2 F5: no
	// operation text is taken from an error).
	if !strings.Contains(out, `lease of "review" released; its page-side cleanup was not sent or failed (Console.disable: WebKit protocol error -32000)`) {
		t.Fatalf("the release line lost the failure's classification:\n%s", out)
	}
}
