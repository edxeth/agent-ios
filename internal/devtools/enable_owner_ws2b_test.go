package devtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/edxeth/agent-ios/internal/core"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// WS2b-1b-b item 3 (found by WS2b-1a while it ported the signal test
// R6-6): the page applies the lease start's Console.enable but never
// answers it, and the lease start is canceled (as a signal or a deadline
// cancels a registry command). The enable may have reached the page, so
// the console owner keeps Console and the release sends Console.disable,
// as core's releaseFailedAcquire does for an enable of the same unknown
// outcome.
func TestCanceledLeaseStartKeepsTheConsoleOwner(t *testing.T) {
	r := newRig(t)
	r.f.Stall("Console.enable")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		// Cancel the command once the page has the enable; the 5 s
		// bound of ctx (the lease start's own bound too) ends the loop.
		for ctx.Err() == nil {
			if len(received(r.f, "Console.enable")) > 0 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	// The lease hook logs its error and the acquisition goes on, so the
	// result of Do is not the subject here.
	_ = r.m.Do(core.WithLeaseHooks(ctx), r.req, r.ao, "test", core.ClassMutate, false, func(context.Context, *core.ManagedOp) error { return nil })
	if ctx.Err() == nil {
		t.Fatal("the lease start ended before the test canceled it")
	}
	if got := len(received(r.f, "Console.enable")); got != 1 {
		t.Fatalf("Console.enable sent %d times at the lease start, want 1", got)
	}
	mark := lastSeq(r.f)
	r.release(t)
	if got := len(receivedAfter(r.f, "Console.disable", mark)); got != 1 {
		t.Fatalf("Console.disable sent %d times at the release, want 1: the enable may have reached the page", got)
	}
}

// failingCaller is the operation's caller whose sends of method fail with
// err and send nothing.
type failingCaller struct {
	webkit.Caller
	method string
	err    error
}

func (c failingCaller) Send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == c.method {
		return nil, c.err
	}
	return c.Caller.Send(ctx, method, params)
}

// An enable that provably changed nothing (never written, or refused by
// the page with a protocol error) gives the ownership up, so the release
// sends no disable. Any other failure leaves the outcome unknown: the
// owner keeps the domain and the release disables it.
func TestFailedEnableKeepsItsOwnerUnlessItHadNoEffect(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		disables int
	}{
		{"not sent", fmt.Errorf("Console.enable: %w: %w", webkit.ErrNotSent, context.Canceled), 0},
		{"refused by the page", &webkit.ErrorData{Code: -32000, Message: "refused"}, 0},
		{"reply lost", fmt.Errorf("Console.enable: %w", context.DeadlineExceeded), 1},
		{"connection lost", errors.New("connection closed"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			err := r.do(t, func(ctx context.Context, p *Page, c webkit.Caller) error {
				return acquireDomain(ctx, p.ps, failingCaller{c, "Console.enable", tc.err}, owner("console", "s1"), "Console")
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("acquireDomain: %v; want the error of the enable", err)
			}
			mark := lastSeq(r.f)
			r.release(t)
			if got := len(receivedAfter(r.f, "Console.disable", mark)); got != tc.disables {
				t.Fatalf("Console.disable sent %d times at the release, want %d", got, tc.disables)
			}
		})
	}
}
