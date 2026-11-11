package devtools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
)

// geoSender records the expressions of the Runtime.evaluate calls it gets.
type geoSender struct{ evals []string }

func (g *geoSender) Send(_ context.Context, method string, params any) (json.RawMessage, error) {
	if method == "Runtime.evaluate" {
		g.evals = append(g.evals, params.(map[string]any)["expression"].(string))
	}
	return json.RawMessage("{}"), nil
}

// Review finding F2 (W5b-2 review; design 2.4: a release removes only its
// owner's state): the release of a geolocation owner restores the current
// document's geolocation and, when another owner's geolocation part
// remains, installs the effective one, the last in script order, again.
// Parts that are no geolocation shim are left alone; without a surviving
// geolocation part only the restore is sent.
func TestGeoReleaseKeepsTheSurvivingOverride(t *testing.T) {
	boot := core.NewBootstrapParts()
	release := GeoRelease(boot)
	for _, p := range []struct{ name, src string }{
		{GeoPartPrefix + "s1", GeoShim(12, 34, 1)},
		{GeoPartPrefix + "cdp-A:s1", GeoShim(56, 78, 1)},
		{"init:s1:1", "window.initScript = 1"},
	} {
		if err := boot.Set(p.name, core.RankShim, p.src); err != nil {
			t.Fatal(err)
		}
	}
	var g geoSender
	if err := release(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	if len(g.evals) != 2 || g.evals[0] != GeoRestore || g.evals[1] != GeoShim(56, 78, 1) {
		t.Fatalf("release with two geolocation parts left evaluated:\n%s\nwant the restore, then the last part's shim", strings.Join(g.evals, "\n---\n"))
	}
	boot.Remove(GeoPartPrefix + "cdp-A:s1")
	g = geoSender{}
	if err := release(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	if len(g.evals) != 2 || g.evals[1] != GeoShim(12, 34, 1) {
		t.Fatalf("release with the session's part left evaluated:\n%s\nwant the restore, then the session's shim", strings.Join(g.evals, "\n---\n"))
	}
	boot.Remove(GeoPartPrefix + "s1")
	g = geoSender{}
	if err := release(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	if len(g.evals) != 1 || g.evals[0] != GeoRestore {
		t.Fatalf("release without a geolocation part left evaluated:\n%s\nwant only the restore", strings.Join(g.evals, "\n---\n"))
	}
}
