package commands

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/core"
)

// netidleHarness is an action harness whose page answers Network.enable,
// for the network capture that "wait --load networkidle" starts.
func netidleHarness(t *testing.T) *rtHarness {
	t.Helper()
	h := newActionHarness(t)
	h.ps.State = core.NewDesiredState()
	h.f.Result("Network.enable", map[string]any{})
	return h
}

// netRequest emits the start of request id; done emits its end.
func netRequest(h *rtHarness, id string) {
	h.f.Emit("Network.requestWillBeSent", map[string]any{
		"requestId": id, "frameId": "f1", "loaderId": "l1", "documentURL": "https://a.test/", "timestamp": 1, "walltime": 1.7e9,
		"initiator": map[string]any{"type": "other"}, "type": "Fetch",
		"request": map[string]any{"url": "https://a.test/" + id, "method": "GET", "headers": map[string]string{}},
	})
}

func netDone(h *rtHarness, id string) {
	h.f.Emit("Network.loadingFinished", map[string]any{"requestId": id, "timestamp": 2})
}

// waitForMethod waits until the page received method; it reports false
// when that took more than 3 s.
func waitForMethod(h *rtHarness, method string) bool {
	for end := time.Now().Add(3 * time.Second); len(h.f.ReceivedMethod(method)) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			return false
		}
	}
	return true
}

// Design 3.6: "wait --load networkidle" ends when no request has been in
// flight for 500 ms (W4's tracker, devtools.WaitNetworkIdle). The first
// wait starts the session's network capture and says that it may have
// missed earlier requests; a request in flight keeps the wait going until
// it ends; a wait that ends first is exit 3 with the requests in flight.
func TestWaitNetworkIdle(t *testing.T) {
	t.Run("idle after the last request", func(t *testing.T) {
		h := netidleHarness(t)
		var wg sync.WaitGroup
		var finished time.Time
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !waitForMethod(h, "Network.enable") {
				t.Error("Network.enable was not sent")
			}
			netRequest(h, "r1")
			time.Sleep(300 * time.Millisecond)
			finished = time.Now()
			netDone(h, "r1")
		}()
		code, out, errOut := h.run("wait", "--load", "networkidle")
		ended := time.Now()
		wg.Wait()
		if code != ExitOK || !strings.HasPrefix(out, "done: --load networkidle") || !strings.Contains(out, "started the network capture") {
			t.Fatalf("wait --load networkidle: exit %d, %q, %q", code, out, errOut)
		}
		if quiet := ended.Sub(finished); quiet < 500*time.Millisecond {
			t.Fatalf("the wait ended %v after the last request, want at least the 500 ms quiet time", quiet)
		}
		// The capture runs now: a second wait does not start it.
		if code, out, _ := h.run("wait", "--load", "networkidle"); code != ExitOK || strings.Contains(out, "started the network capture") {
			t.Fatalf("second wait: exit %d, %q", code, out)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		h := netidleHarness(t)
		go func() {
			if !waitForMethod(h, "Network.enable") {
				t.Error("Network.enable was not sent")
			}
			netRequest(h, "slow")
		}()
		start := time.Now()
		code, _, errOut := h.run("--timeout", "1500ms", "wait", "--load", "networkidle")
		if code != ExitWaitTimeout || !strings.Contains(errOut, "1 request(s) still in flight") || time.Since(start) > 3*time.Second {
			t.Fatalf("wait --load networkidle with a request in flight: exit %d after %v, %q; want exit 3 naming it", code, time.Since(start), errOut)
		}
	})
}
