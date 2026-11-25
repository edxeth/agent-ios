package cdp

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
)

// The CDP endpoint keeps the simulator's turns (review of 2026-11-24, F3):
// another owner gets no endpoint for a reserved simulator's page, and an
// endpoint issued before another owner's turn no longer lists the page; the
// owner's own endpoint does. A reservation that cannot be read fails the
// listing.
func TestCDPKeepsTheSimulatorsTurns(t *testing.T) {
	t.Run("issue", func(t *testing.T) {
		r := newRig(t)
		r.addPage(t, r.l.AddDevice(core.SimulatorDevice, "Simulator", ""), "SimSecret")
		if _, err := r.res.Reserve(core.SimulatorDevice, "alice", time.Hour); err != nil {
			t.Fatal(err)
		}
		req := r.req
		req.Page = "SimSecret"
		ep, err := r.srv.Issue(testCtx(t), IssueRequest{Session: "bob", Reservation: "bob", Page: req})
		if !errors.Is(err, core.ErrReserved) {
			t.Fatalf("bob got an endpoint of alice's simulator page: %+v, %v", ep, err)
		}
		ep, err = r.srv.Issue(testCtx(t), IssueRequest{Session: "alice", Reservation: "alice", Page: req})
		if err != nil {
			t.Fatal(err)
		}
		if code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil); code != http.StatusOK || !strings.Contains(body, "SimSecret") {
			t.Fatalf("alice's own endpoint: %d %s", code, body)
		}
	})
	t.Run("earlier endpoint", func(t *testing.T) {
		r := newRig(t)
		r.addPage(t, r.l.AddDevice(core.SimulatorDevice, "Simulator", ""), "SimSecret")
		req := r.req
		req.Page = "SimSecret"
		ep, err := r.srv.Issue(testCtx(t), IssueRequest{Session: "bob", Reservation: "bob", Page: req})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.res.Reserve(core.SimulatorDevice, "alice", time.Hour); err != nil {
			t.Fatal(err)
		}
		if code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil); code != http.StatusOK || strings.Contains(body, "SimSecret") {
			t.Fatalf("bob's earlier endpoint in alice's turn: %d %s", code, body)
		}
		if err := os.WriteFile(filepath.Join(r.res.Dir, "device-broken.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil); code == http.StatusOK {
			t.Fatalf("a listing with an unreadable reservation: %d %s", code, body)
		}
	})
}
