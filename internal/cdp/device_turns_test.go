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

// The CDP endpoint keeps the physical devices' reservations (design 2.5
// rules 1 and 4) as it keeps the simulator's turns (sim_turns_test.go): an
// owner without the device's reservation gets no endpoint for its page, and
// an endpoint issued before the reservation moved to another owner no
// longer lists the page, the selected one included; the owner's own
// endpoint does. A reservation that cannot be read fails the listing.
func TestCDPKeepsThePhonesReservations(t *testing.T) {
	t.Run("issue", func(t *testing.T) {
		r := newRig(t) // device A is o1's; device B, with Three, nobody's
		req := r.req
		req.Page = "Three"
		ep, err := r.srv.Issue(testCtx(t), IssueRequest{Session: "s1", Reservation: "o1", Page: req})
		if !errors.Is(err, core.ErrNoReservation) {
			t.Fatalf("o1 got an endpoint of an unreserved phone's page: %+v, %v", ep, err)
		}
		req.Page = "One"
		ep, err = r.srv.Issue(testCtx(t), IssueRequest{Session: "s2", Reservation: "o2", Page: req})
		if !errors.Is(err, core.ErrReserved) {
			t.Fatalf("o2 got an endpoint of o1's phone page: %+v, %v", ep, err)
		}
		ep, err = r.srv.Issue(testCtx(t), IssueRequest{Session: "s1", Reservation: "o1", Page: req})
		if err != nil {
			t.Fatal(err)
		}
		if code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil); code != http.StatusOK || !strings.Contains(body, "one.example") {
			t.Fatalf("o1's own endpoint: %d %s", code, body)
		}
	})
	t.Run("earlier endpoint", func(t *testing.T) {
		r := newRig(t)
		ep := r.issue(t, "s1", "One", "")
		if code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil); code != http.StatusOK || !strings.Contains(body, "one.example") {
			t.Fatalf("o1's endpoint in its reservation: %d %s", code, body)
		}
		if err := r.res.Release(r.devA.ID, "o1"); err != nil {
			t.Fatal(err)
		}
		if code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil); code != http.StatusOK || strings.Contains(body, "one.example") {
			t.Fatalf("o1's earlier endpoint without its reservation: %d %s", code, body)
		}
		if _, err := r.res.Reserve(r.devA.ID, "o2", time.Hour); err != nil {
			t.Fatal(err)
		}
		if code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil); code != http.StatusOK || strings.Contains(body, "one.example") {
			t.Fatalf("o1's earlier endpoint in o2's reservation: %d %s", code, body)
		}
		if err := os.WriteFile(filepath.Join(r.res.Dir, "device-broken.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, body := httpDo(t, "GET", ep.HTTP+"/json/list", nil); code == http.StatusOK {
			t.Fatalf("a listing with an unreadable reservation: %d %s", code, body)
		}
	})
}
