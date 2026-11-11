package webkit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil"
)

// errOpDeadline stands for core.ErrDeadline: a cancellation cause that is a
// deadline error.
type errOpDeadline struct{}

func (errOpDeadline) Error() string { return "the operation deadline passed" }
func (errOpDeadline) Unwrap() error { return context.DeadlineExceeded }

// Decision D9: a call that its caller's deadline ends fails as a deadline,
// also when that deadline arrives as the cause of a cancellation (core
// cancels an operation at its deadline with such a cause). Before, the
// client reported only context.Canceled. A cancellation without a deadline
// cause stays a cancellation.
func TestCallEndedByACancelCauseKeepsTheCause(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.NeverRespond("Runtime.evaluate")
	c, err := webkit.NewClient(context.Background(), mock.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for _, tc := range []struct {
		name     string
		cause    error
		deadline bool
	}{
		{"deadline cause", errOpDeadline{}, true},
		{"plain cancel", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			time.AfterFunc(100*time.Millisecond, func() { cancel(tc.cause) })
			_, err := c.Send(ctx, "Runtime.evaluate", map[string]string{"expression": "1"})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Send: %v, want context.Canceled in the chain", err)
			}
			if got := errors.Is(err, context.DeadlineExceeded); got != tc.deadline {
				t.Fatalf("Send: %v; errors.Is DeadlineExceeded = %v, want %v", err, got, tc.deadline)
			}
		})
	}
}
