package commands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/devtools"
)

// W4 round 2 (watchout of /home/devkit/.pi/artifacts/worker/w4-r1-cmd-20261107-061500.md,
// design 7.9 and R17): WebKit's error text of a failed request can quote
// the URL; "network requests" and "network request" mask it as they mask
// the URL, in the text line and in the JSON data.
func TestNetworkListingsMaskURLsInTheErrorText(t *testing.T) {
	req := devtools.Request{
		ID: "r1", Seq: 1, Method: "GET", State: devtools.StateFailed, URL: "https://a.test/cb",
		Error: "Failed to load https://u:pw-secret@a.test/cb?access_token=tok-secret&page=2",
	}
	m := maskRequest(req)
	data, _ := json.Marshal(m)
	for _, out := range []string{requestLine(m), string(data)} {
		if strings.Contains(out, "pw-secret") || strings.Contains(out, "tok-secret") {
			t.Fatalf("masked listing has a secret of the error text: %s", out)
		}
		if !strings.Contains(out, "page=2") {
			t.Fatalf("masked listing lost the rest of the error text: %s", out)
		}
	}
	if !strings.Contains(req.Error, "tok-secret") {
		t.Fatal("masking changed the logged request")
	}
}
