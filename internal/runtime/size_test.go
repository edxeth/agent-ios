package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/runtime"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/direct"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// Size rules of design 3.1 and acceptance 7.5 item 7.

// The joined bundle stays at or below its 64 KiB source budget.
func TestBundleWithinSourceBudget(t *testing.T) {
	if n := len(runtime.Source()); n > runtime.MaxSourceBytes || runtime.MaxSourceBytes != 64*1024 {
		t.Fatalf("runtime bundle is %d bytes; the budget is %d (64 KiB, design 3.1)", n, runtime.MaxSourceBytes)
	}
}

// escapeHeavy returns n bytes of init-script text made of the characters
// that JSON escaping grows: quotes, backslashes and control characters
// (one byte becomes up to 7 on the wire after the second escaping of
// Target routing).
func escapeHeavy(n int) string {
	const unit = "\"\\'\n\t\x00\x01\x1f</script>"
	return strings.Repeat(unit, n/len(unit)+1)[:n]
}

// wireBytes is the size of the routed outer message that carries an inner
// command: the inner command JSON-encoded as a string inside
// Target.sendMessageToTarget (webkit.Session's form).
func wireBytes(t *testing.T, method string, params any) int {
	inner, err := json.Marshal(map[string]any{"id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(map[string]any{"id": 1, "method": "Target.sendMessageToTarget",
		"params": map[string]string{"targetId": "page-1", "message": string(inner)}})
	if err != nil {
		t.Fatal(err)
	}
	return len(outer)
}

func dialLimit(t *testing.T, f *fakewebkit.Server, limit int) *webkit.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true, MaxOutgoing: limit})
	}, webkit.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// The install message and the bootstrap message with all parts (the
// runtime plus an escape-heavy user init script) fit the outgoing limit of
// every transport, measured on their wire bytes; an escape-heavy script
// whose raw text fits but whose wire form does not fails before anything
// is written, with the size error of design 3.1.
func TestInstallAndBootstrapAgainstEachTransportLimit(t *testing.T) {
	boot := func(initScript string) string {
		parts := core.NewBootstrapParts()
		if err := parts.Set("runtime", core.RankRuntime, runtime.Source()); err != nil {
			t.Fatal(err)
		}
		if err := parts.Set("init-script", core.RankInit, initScript); err != nil {
			t.Fatal(err)
		}
		return parts.Script()
	}
	fits := boot(escapeHeavy(8 << 10))
	for _, c := range []struct {
		name  string
		limit int
	}{
		{"webkitgtk", transport.WebKitGTKMaxOutgoing},
		{"iwdp", transport.IWDPMaxOutgoing},
		{"direct", direct.MaxOutgoing},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := fakewebkit.New(fakewebkit.Options{})
			defer f.Close()
			s := dialLimit(t, f, c.limit)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			install := map[string]any{"expression": runtime.Source(), "returnByValue": true}
			bootstrap := map[string]any{"source": fits}
			for _, m := range []struct {
				method string
				params any
			}{{"Runtime.evaluate", install}, {"Page.setBootstrapScript", bootstrap}} {
				n := wireBytes(t, m.method, m.params)
				t.Logf("%s: %d bytes on the wire, limit %d", m.method, n, c.limit)
				if n > c.limit {
					t.Fatalf("%s is %d bytes on the wire, over the %s limit %d", m.method, n, c.name, c.limit)
				}
				if _, err := s.Send(ctx, m.method, m.params); err != nil {
					t.Fatalf("%s: %v", m.method, err)
				}
			}
		})
	}

	// The second escaping more than doubles control characters: 40 KiB of
	// them keep the raw script under WebKitGTK's 127 KiB but not the wire form.
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	s := dialLimit(t, f, transport.WebKitGTKMaxOutgoing)
	big := boot(strings.Repeat("\x01", 40<<10))
	if len(big) >= transport.WebKitGTKMaxOutgoing {
		t.Fatalf("test setup: the raw script is %d bytes, not under the limit", len(big))
	}
	_, err := s.Send(context.Background(), "Page.setBootstrapScript", map[string]any{"source": big})
	var wl *webkit.WireLimitError
	if !errors.As(err, &wl) || wl.Limit != transport.WebKitGTKMaxOutgoing ||
		!strings.Contains(err.Error(), "bytes on the wire; the limit for this transport is") {
		t.Fatalf("oversized bootstrap gave %v", err)
	}
	if n := len(f.ReceivedMethod("Page.setBootstrapScript")); n != 0 {
		t.Fatalf("the oversized bootstrap reached the page %d times", n)
	}
}
