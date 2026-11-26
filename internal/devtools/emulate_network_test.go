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

// parity-045 simulator check: iwdp lists no iOS version for the iOS
// Simulator, so the command list cannot refuse first; WebKit's own "was not
// found" answer (iOS 18.0 to 27.2 have no Network.setEmulatedConditions) is
// reported as unsupported with the reason, not as a raw protocol error, and
// leaves no throttle recorded.
func TestEmulateNetworkUnknownVersionWithoutTheMethodIsUnsupported(t *testing.T) {
	r := newRigOS(t, webkit.SessionOptions{TargetWait: time.Second}, "")
	r.f.Unsupported("Network.setEmulatedConditions")
	for i := 0; i < 2; i++ {
		err := r.do(t, func(ctx context.Context, p *Page, wc webkit.Caller) error {
			return p.EmulateNetworkRate(ctx, wc, "s1", 1000)
		})
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "18.0 to 27.2") {
			t.Fatalf("attempt %d: %v; want ErrUnsupported naming the iOS versions without the method", i+1, err)
		}
	}
	if n := len(r.f.ReceivedMethod("Network.setEmulatedConditions")); n != 2 {
		t.Fatalf("Network.setEmulatedConditions sent %d times, want 2: a refused throttle must not stay recorded as applied", n)
	}
}

// FIX-K (sim recheck claim 1): on the iOS Simulator, which iwdp lists as
// 0.0.0, the refusal names no iOS 0.0.0 and carries no raw protocol text
// ("'Network.setEmulatedConditions' was not found" and its JSON), while the
// error chain keeps ErrUnsupported and WebKit's error for errors.Is/As and
// the daemon log.
func TestEmulateNetworkUnsupportedMessageIsClean(t *testing.T) {
	r := newRigOS(t, webkit.SessionOptions{TargetWait: time.Second}, "0.0.0")
	r.f.Unsupported("Network.setEmulatedConditions")
	err := r.do(t, func(ctx context.Context, p *Page, wc webkit.Caller) error {
		return p.EmulateNetworkRate(ctx, wc, "s1", 1000)
	})
	want := "emulate network: unsupported: this WebKit (iOS version unknown) has no Network.setEmulatedConditions; iOS 18.0 to 27.2 lack it"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	var wd *webkit.ErrorData
	if !errors.Is(err, ErrUnsupported) || !errors.As(err, &wd) || wd.Code != -32601 {
		t.Fatalf("err = %v: the chain lost ErrUnsupported or WebKit's -32601 error", err)
	}
	if got := string(r.f.ReceivedMethod("Network.setEmulatedConditions")[0].Params); got != `{"bandwidth":1000}` {
		t.Fatalf("sent %s, want the bandwidth form of an unknown (current) WebKit", got)
	}
}
