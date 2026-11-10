package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// runTokenRE matches a run token of scripts/sim-setup.sh (new_token).
var runTokenRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// simTarget is the shared connection of the simulator tests to the test
// page of one scripts/sim-setup.sh run (WebKit allows one debugger
// connection per page). getSimClient hands it out through get.
type simTarget struct {
	// runToken is IWDP_SIM_RUN_TOKEN, the lease token of the setup run:
	// the names of the run's test data derive from it (testName).
	listingURL, wsURL, pageURL, runToken string

	once   sync.Once
	client *webkit.Client
	err    error
}

// get returns the shared connection only after it checks the target, on
// every call: the iwdp listing at listingURL must show the page pageURL on
// the SIMULATOR entry at wsURL (checkSimTarget), and the connected page
// must report pageURL as its location. The tests delete data and replace
// the DOM of the page they get, so a page of another device or another
// tab, or the test page after it navigated away, fails here before the
// next test runs.
func (s *simTarget) get(ctx context.Context) (*webkit.Client, error) {
	if s.pageURL == "" {
		return nil, errors.New("IWDP_SIM_PAGE_URL is not set; run make sim-setup and export the variables that it prints")
	}
	if !runTokenRE.MatchString(s.runToken) {
		return nil, fmt.Errorf("IWDP_SIM_RUN_TOKEN %q is not a run token (32 hex digits); run make sim-setup and export the variables that it prints", s.runToken)
	}
	if err := checkSimTarget(ctx, s.listingURL, s.wsURL, s.pageURL); err != nil {
		return nil, err
	}
	s.once.Do(func() {
		s.client, s.err = webkit.NewClient(context.Background(), s.wsURL)
		if s.err != nil {
			s.err = fmt.Errorf("connecting to %s: %w", s.wsURL, s.err)
		}
	})
	if s.err != nil {
		return nil, s.err
	}
	result, err := tools.EvaluateScript(ctx, s.client, "location.href", true)
	var href string
	if err == nil {
		err = json.Unmarshal(result.Result.Value, &href)
	}
	if err != nil || href != s.pageURL {
		return nil, fmt.Errorf("the page at %s reports location %q (error %v); want IWDP_SIM_PAGE_URL %s", s.wsURL, href, err, s.pageURL)
	}
	return s.client, nil
}

// mutate runs action, a destructive step (data deletion, cookie changes,
// DOM replacement, any other change of the page or its origin), on the
// shared connection right after get checks the target again. When the
// check fails, action does not run: a page that navigated after an
// earlier check is never changed.
func (s *simTarget) mutate(ctx context.Context, what string, action func(*webkit.Client) error) error {
	client, err := s.get(ctx)
	if err != nil {
		return fmt.Errorf("refusing to %s: %w", what, err)
	}
	if err := action(client); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// checkSimTarget checks, before any simulator test changes a page, that
// the iwdp listing at listingURL ("http://host:port") has exactly one
// SIMULATOR entry and that this Simulator lists exactly one page with the
// URL pageURL (IWDP_SIM_PAGE_URL, unique to one scripts/sim-setup.sh run),
// whose WebSocket URL is wsURL (IWDP_SIM_WS_URL). A page of another
// device or another tab fails the check.
func checkSimTarget(ctx context.Context, listingURL, wsURL, pageURL string) error {
	var devices []webkit.DeviceEntry
	if err := getJSONList(ctx, strings.TrimSuffix(listingURL, "/")+"/json", &devices); err != nil {
		return err
	}
	var sims []webkit.DeviceEntry
	for _, d := range devices {
		if d.DeviceID == "SIMULATOR" {
			sims = append(sims, d)
		}
	}
	if len(sims) != 1 {
		return fmt.Errorf("the iwdp listing %s lists %d SIMULATOR entries; want 1", listingURL, len(sims))
	}
	devURL := sims[0].URL
	if !strings.HasPrefix(devURL, "http") {
		devURL = "http://" + devURL
	}
	var pages []webkit.PageEntry
	if err := getJSONList(ctx, strings.TrimSuffix(devURL, "/")+"/json", &pages); err != nil {
		return err
	}
	var matches []webkit.PageEntry
	for _, p := range pages {
		if p.URL == pageURL {
			matches = append(matches, p)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("the SIMULATOR (%s) lists %d pages with the URL %s; want 1", devURL, len(matches), pageURL)
	}
	if got := matches[0].WebSocketDebuggerURL; got != wsURL {
		return fmt.Errorf("the SIMULATOR's page %s has the WebSocket URL %q, not IWDP_SIM_WS_URL %q", pageURL, got, wsURL)
	}
	return nil
}

func getJSONList(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("listing %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("listing %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("listing %s: HTTP %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("listing %s: %w", url, err)
	}
	return nil
}

// TestSimTargetChecksEveryCall: get checks the listing and the page's
// location on every call, not only on the first, so the shared connection
// is never handed out after its page navigated away or left the listing
// (review round 3, F9: a mismatch fails the call).
func TestSimTargetChecksEveryCall(t *testing.T) {
	page := testutil.NewMockServer()
	t.Cleanup(page.Close)
	var href atomic.Value
	href.Store(simTestPageURL)
	page.Handle("Runtime.evaluate", func(string, json.RawMessage) (interface{}, error) {
		return map[string]any{"result": map[string]any{"type": "string", "value": href.Load().(string)}}, nil
	})
	var listed atomic.Bool
	listed.Store(true)
	devicePages := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages := []map[string]string{fakePage("https://mail.example/", "ws://127.0.0.1:1/devtools/page/9")}
		if listed.Load() {
			pages = append(pages, fakePage(simTestPageURL, page.URL))
		}
		_ = json.NewEncoder(w).Encode(pages)
	}))
	t.Cleanup(devicePages.Close)
	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{"deviceId": "SIMULATOR", "deviceName": "SIMULATOR", "url": strings.TrimPrefix(devicePages.URL, "http://")}})
	}))
	t.Cleanup(listing.Close)
	target := &simTarget{listingURL: listing.URL, wsURL: page.URL, pageURL: simTestPageURL, runToken: simTestRunToken}

	first, err := target.get(t.Context())
	if err != nil {
		t.Fatalf("first get on the test page: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	href.Store("https://unrelated.example/")
	if c, err := target.get(t.Context()); err == nil || !strings.Contains(err.Error(), "https://unrelated.example/") {
		t.Fatalf("get after the page navigated away = %v, %v; want an error naming its location", c, err)
	}

	href.Store(simTestPageURL)
	listed.Store(false)
	if c, err := target.get(t.Context()); err == nil || !strings.Contains(err.Error(), "lists 0 pages") {
		t.Fatalf("get after the listing dropped the test page = %v, %v; want a listing error", c, err)
	}

	listed.Store(true)
	again, err := target.get(t.Context())
	if err != nil || again != first {
		t.Fatalf("get on the restored test page = %p, %v; want the shared client %p", again, err, first)
	}
	if n := page.ConnectionCount(); n != 1 {
		t.Fatalf("the page got %d connections; want the one shared connection", n)
	}
}

// TestSimTargetChecksBeforeEachMutation: a destructive step checks the
// target right before it acts. After get passed, the page navigates away;
// the next mutate refuses and does not run its action. On the restored
// test page it runs (review round 3).
func TestSimTargetChecksBeforeEachMutation(t *testing.T) {
	page := testutil.NewMockServer()
	t.Cleanup(page.Close)
	var href atomic.Value
	href.Store(simTestPageURL)
	page.Handle("Runtime.evaluate", func(string, json.RawMessage) (interface{}, error) {
		return map[string]any{"result": map[string]any{"type": "string", "value": href.Load().(string)}}, nil
	})
	listing := fakeIWDPListing(t, fakeIWDPDevice{id: "SIMULATOR", pages: []map[string]string{fakePage(simTestPageURL, page.URL)}})
	target := &simTarget{listingURL: listing, wsURL: page.URL, pageURL: simTestPageURL, runToken: simTestRunToken}
	client, err := target.get(t.Context())
	if err != nil {
		t.Fatalf("get on the test page: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	href.Store("https://unrelated.example/") // navigates after the check
	ran := false
	err = target.mutate(t.Context(), "replace the body", func(*webkit.Client) error { ran = true; return nil })
	if err == nil || ran || !strings.Contains(err.Error(), "https://unrelated.example/") {
		t.Fatalf("mutate after the page navigated away: error %v, action ran %v; want a refusal naming the location and no action", err, ran)
	}

	href.Store(simTestPageURL)
	if err := target.mutate(t.Context(), "replace the body", func(c *webkit.Client) error {
		ran = c == client
		return nil
	}); err != nil || !ran {
		t.Fatalf("mutate on the test page: error %v, action ran on the shared client %v; want it to run", err, ran)
	}
}

// TestCheckSimTarget runs getSimClient's target check against fake iwdp
// listings (review F5): it accepts only the SIMULATOR's test page.
func TestCheckSimTarget(t *testing.T) {
	const ws = "ws://localhost:9223/devtools/page/2"
	sim := func(pages ...map[string]string) fakeIWDPDevice { return fakeIWDPDevice{id: "SIMULATOR", pages: pages} }
	phone := func(pages ...map[string]string) fakeIWDPDevice {
		return fakeIWDPDevice{id: "00008030-PHONE", pages: pages}
	}
	for _, tc := range []struct {
		name    string
		devices []fakeIWDPDevice
		wantErr string // "" when the check passes
	}{
		{"the Simulator's test page", []fakeIWDPDevice{phone(fakePage(simTestPageURL, ws)), sim(fakePage("https://mail.example/", "ws://localhost:9223/devtools/page/1"), fakePage(simTestPageURL, ws))}, ""},
		{"the test page only on a phone", []fakeIWDPDevice{phone(fakePage(simTestPageURL, ws)), sim()}, "lists 0 pages"},
		{"no Simulator", []fakeIWDPDevice{phone(fakePage(simTestPageURL, ws))}, "0 SIMULATOR entries"},
		{"another tab at the WebSocket URL", []fakeIWDPDevice{sim(fakePage("https://mail.example/", ws), fakePage(simTestPageURL, "ws://localhost:9223/devtools/page/3"))}, "not IWDP_SIM_WS_URL"},
		{"two test pages", []fakeIWDPDevice{sim(fakePage(simTestPageURL, ws), fakePage(simTestPageURL, "ws://localhost:9223/devtools/page/3"))}, "lists 2 pages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listing := fakeIWDPListing(t, tc.devices...)
			err := checkSimTarget(t.Context(), listing, ws, simTestPageURL)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("checkSimTarget: %v; want no error", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("checkSimTarget error %v; want one with %q", err, tc.wantErr)
			}
		})
	}
}
