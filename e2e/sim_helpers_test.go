//go:build simulator

package e2e_test

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// shared simulator connection — WebKit only allows one debugger connection per page.
var (
	simOnce   sync.Once
	simClient *webkit.Client
	simErr    error
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
		simClient, simErr = webkit.NewClient(context.Background(), wsURL)
	})
	if simErr != nil {
		t.Fatalf("connecting to simulator: %v", simErr)
	}
	return simClient
}

func simCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func simOrigin(t *testing.T, client *webkit.Client) string {
	t.Helper()
	ctx, cancel := simCtx()
	defer cancel()
	result, err := tools.EvaluateScript(ctx, client, "window.location.origin", true)
	if err != nil {
		t.Fatalf("getting origin: %v", err)
	}
	var origin string
	_ = json.Unmarshal(result.Result.Value, &origin)
	return origin
}

// simRequest is a request that simNetwork saw: its id and URL from
// Network.requestWillBeSent, and whether Network.loadingFinished or
// Network.loadingFailed ended it.
type simRequest struct {
	ID   string
	URL  string
	Done bool
}

// simNetwork records the requests of the shared simulator page, so that the
// tests of the network functions can find request ids.
type simNetwork struct {
	mu    sync.Mutex
	byID  map[string]*simRequest
	order []string
}

// startSimNetwork enables the Network domain on client and records every
// request the page sends. At t's cleanup it removes its event handlers and
// disables the domain again.
func startSimNetwork(t *testing.T, client *webkit.Client) *simNetwork {
	t.Helper()
	n := &simNetwork{byID: map[string]*simRequest{}}
	unsubscribe := []func(){
		client.Subscribe("Network.requestWillBeSent", func(_ string, params json.RawMessage) {
			var evt struct {
				RequestID string `json:"requestId"`
				Request   struct {
					URL string `json:"url"`
				} `json:"request"`
			}
			if json.Unmarshal(params, &evt) != nil || evt.RequestID == "" {
				return
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			if _, seen := n.byID[evt.RequestID]; !seen {
				n.order = append(n.order, evt.RequestID)
			}
			n.byID[evt.RequestID] = &simRequest{ID: evt.RequestID, URL: evt.Request.URL}
		}),
		client.Subscribe("Network.loadingFinished", n.markDone),
		client.Subscribe("Network.loadingFailed", n.markDone),
	}
	t.Cleanup(func() {
		for _, u := range unsubscribe {
			u()
		}
		ctx, cancel := simCtx()
		defer cancel()
		if _, err := client.Send(ctx, "Network.disable", nil); err != nil {
			t.Errorf("Network.disable: %v", err)
		}
	})
	ctx, cancel := simCtx()
	defer cancel()
	if _, err := client.Send(ctx, "Network.enable", nil); err != nil {
		t.Fatalf("Network.enable: %v", err)
	}
	return n
}

// markDone marks the request of a Network.loadingFinished or
// Network.loadingFailed event as ended.
func (n *simNetwork) markDone(_ string, params json.RawMessage) {
	var evt struct {
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(params, &evt) != nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if r, ok := n.byID[evt.RequestID]; ok {
		r.Done = true
	}
}

// requests returns a copy of the recorded requests in the order the page
// sent them.
func (n *simNetwork) requests() []simRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]simRequest, 0, len(n.order))
	for _, id := range n.order {
		out = append(out, *n.byID[id])
	}
	return out
}
