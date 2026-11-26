package devtools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/webkit"
)

// parity-045 review D11: "emulate network" sends the throttle in the form
// of the page's iOS version, as the CDP bridge does (WebKit commit
// c4a7ebcc295f; command lists of internal/wip): bytesPerSecondLimit on
// iOS 16.4 to 17.x, bandwidth on 27.3 and later; no limit sends {}; the
// versions whose list lacks Network.setEmulatedConditions are refused.
func TestEmulateNetworkRateFormByIOSVersion(t *testing.T) {
	for _, c := range []struct {
		os, want string
	}{
		{"16.7.10", `{"bytesPerSecondLimit":1000} {}`},
		{"17.0", `{"bytesPerSecondLimit":1000} {}`},
		{"27.3", `{"bandwidth":1000} {}`},
		{"27.4.1", `{"bandwidth":1000} {}`},
	} {
		r := newRigOS(t, webkit.SessionOptions{TargetWait: time.Second}, c.os)
		r.must(t, func(ctx context.Context, p *Page, wc webkit.Caller) error {
			return p.EmulateNetworkRate(ctx, wc, "s1", 1000)
		})
		r.must(t, func(ctx context.Context, p *Page, wc webkit.Caller) error {
			return p.EmulateNetworkRate(ctx, wc, "s1", 0)
		})
		var sent []string
		for _, rec := range r.f.ReceivedMethod("Network.setEmulatedConditions") {
			sent = append(sent, string(rec.Params))
		}
		if got := strings.Join(sent, " "); got != c.want {
			t.Errorf("iOS %s: Network.setEmulatedConditions received %s, want %s", c.os, got, c.want)
		}
	}
	r := newRigOS(t, webkit.SessionOptions{TargetWait: time.Second}, "18.0")
	err := r.do(t, func(ctx context.Context, p *Page, wc webkit.Caller) error {
		return p.EmulateNetworkRate(ctx, wc, "s1", 1000)
	})
	if !errors.Is(err, ErrUnsupported) || len(r.f.ReceivedMethod("Network.setEmulatedConditions")) != 0 {
		t.Fatalf("iOS 18.0: %v, want ErrUnsupported and nothing sent", err)
	}
}
