package webkit

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// SessionOptions.NetworkDisabled (core-events round 4): it runs for a
// target's successful Network.disable, before the caller gets the reply,
// after every interception event of the target that came before the
// reply; a refused Network.disable does not call it.
func TestSessionNetworkDisabledHook(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	var mu sync.Mutex
	var seen []string // request ids, in event order
	var calls []string
	var s *Session
	s = newFakeSession(t, f, SessionOptions{NetworkDisabled: func(got *Session, target string) {
		mu.Lock()
		defer mu.Unlock()
		if got != s {
			t.Errorf("NetworkDisabled got session %p, want %p", got, s)
		}
		calls = append(calls, target+" after "+fmtIDs(seen))
	}})
	s.SubscribeTargetSync("Network.requestIntercepted", func(_, _ string, p json.RawMessage) {
		var v struct {
			RequestID string `json:"requestId"`
		}
		_ = json.Unmarshal(p, &v)
		mu.Lock()
		seen = append(seen, v.RequestID)
		mu.Unlock()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.Intercept("page-1", "r1", "request")
	if _, err := s.Send(ctx, "Network.disable", nil); err != nil {
		t.Fatalf("Network.disable: %v", err)
	}
	mu.Lock()
	got := slices.Clone(calls)
	mu.Unlock()
	if want := []string{"page-1 after [r1]"}; !slices.Equal(got, want) {
		t.Fatalf("NetworkDisabled calls at the reply = %v, want %v", got, want)
	}
	f.Handle("Network.disable", func(fakewebkit.Call) (any, *fakewebkit.Error) {
		return nil, &fakewebkit.Error{Code: -32000, Message: "refused (test)"}
	})
	if _, err := s.Send(ctx, "Network.disable", nil); err == nil {
		t.Fatal("refused Network.disable succeeded")
	}
	if _, err := s.Send(ctx, "Network.enable", nil); err != nil {
		t.Fatalf("Network.enable: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("NetworkDisabled calls = %v, want only the successful disable", calls)
	}
}

// Design 2.7 (core-events round 4): a target that commits without a
// targetCreated on this connection (it was created before the connection)
// had no setup, so the session reports it as a failed setup until
// ReapplySetup succeeds.
func TestSessionCommitOfAnUnannouncedTargetNeedsSetup(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	t.Cleanup(f.Close)
	f.Provisional("page-2") // before the connection: not announced to it
	setups := 0
	s := newFakeSession(t, f, SessionOptions{Setup: func(ctx context.Context, tc TargetCaller) error {
		setups++
		_, err := tc.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": "x"})
		return err
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.Commit("page-2")
	waitFor(t, func() bool { return s.ActiveTarget() == "page-2" })
	if err := s.ActiveSetup(ctx); !errors.As(err, new(*SetupError)) {
		t.Fatalf("ActiveSetup after the commit = %v, want a SetupError", err)
	}
	if _, err := s.Send(ctx, "Test.x", nil); !errors.Is(err, ErrNotSent) {
		t.Fatalf("gated send = %v, want not sent", err)
	}
	if err := s.ReapplySetup(ctx); err != nil || setups != 1 {
		t.Fatalf("ReapplySetup = %v after %d setups, want nil after one", err, setups)
	}
	if err := s.ActiveSetup(ctx); err != nil {
		t.Fatalf("ActiveSetup after ReapplySetup = %v, want nil", err)
	}
}

func fmtIDs(ids []string) string {
	b, _ := json.Marshal(ids)
	return string(slices.DeleteFunc(b, func(c byte) bool { return c == '"' }))
}
