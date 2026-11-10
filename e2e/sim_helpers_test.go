//go:build simulator

package e2e_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/proxy"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// shared simulator connection — WebKit only allows one debugger connection per page.
var (
	simOnce sync.Once
	sim     *simTarget
)

func getSimClient(t *testing.T) *webkit.Client {
	t.Helper()
	wsURL := os.Getenv("IWDP_SIM_WS_URL")
	if wsURL == "" {
		t.Skip("IWDP_SIM_WS_URL not set — run scripts/sim-setup.sh first")
	}

	simOnce.Do(func() {
		// CI runners are slower — give iwdp more time to send Target.targetCreated.
		webkit.TargetWaitTimeout = 2 * time.Second
		sim = &simTarget{listingURL: "http://" + proxy.ListingAddr(), wsURL: wsURL, pageURL: os.Getenv("IWDP_SIM_PAGE_URL"), runToken: os.Getenv("IWDP_SIM_RUN_TOKEN")}
	})
	// Checked on every call (simTarget.get): a test that navigates the
	// page must restore IWDP_SIM_PAGE_URL before it ends.
	ctx, cancel := simCtx()
	defer cancel()
	client, err := sim.get(ctx)
	if err != nil {
		t.Fatalf("simulator test page: %v", err)
	}
	return client
}

// simMutate runs a destructive step of a simulator test (data deletion,
// cookie changes, DOM replacement, any other change of the page or its
// origin) right after simTarget.mutate checks the device and the page's
// location again. A failed check or action fails the test. Every such
// step goes through simMutate, never through the client directly.
func simMutate(t *testing.T, what string, action func(ctx context.Context, client *webkit.Client) error) {
	t.Helper()
	if sim == nil {
		t.Fatal("simMutate before getSimClient")
	}
	sim.mustMutate(t, what, action)
}

func simCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
