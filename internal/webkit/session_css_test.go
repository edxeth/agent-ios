package webkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/transport"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// The CSS methods that hang through iwdp (CLAUDE.md, Known Limitations)
// are refused without a write unless the connection's limits say the
// endpoint answers them (transport.Limits.CSSDomain, set by WebKitGTK);
// then they are sent like any other method.
func TestSessionSendsCSSMethodsOnlyWhereTheEndpointAnswersThem(t *testing.T) {
	methods := []string{"CSS.enable", "CSS.getAllStyleSheets", "CSS.getStyleSheetText"}
	for _, tc := range []struct {
		name string
		lim  transport.Limits
		sent bool
	}{
		{"iwdp-like", transport.Limits{Routed: true}, false},
		{"webkitgtk", transport.Limits{Routed: true, CSSDomain: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakewebkit.New(fakewebkit.Options{})
			t.Cleanup(f.Close)
			for _, m := range methods {
				f.Result(m, map[string]any{})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s, err := Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
				return DialWebSocket(ctx, f.URL, tc.lim)
			}, SessionOptions{})
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			for _, m := range methods {
				_, err := s.Send(ctx, m, nil)
				if tc.sent && err != nil {
					t.Fatalf("%s: %v, want the page's answer", m, err)
				}
				if !tc.sent && (!errors.Is(err, ErrHangsThroughTarget) || !errors.Is(err, ErrNotSent)) {
					t.Fatalf("%s: %v, want ErrHangsThroughTarget and ErrNotSent", m, err)
				}
				if n := len(f.ReceivedMethod(m)); (n == 1) != tc.sent || n > 1 {
					t.Fatalf("%s reached the page %d times, want sent=%v", m, n, tc.sent)
				}
			}
		})
	}
}
