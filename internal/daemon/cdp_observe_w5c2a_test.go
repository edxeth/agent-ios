package daemon

import (
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/nnemirovsky/iwdp-cli/internal/webkit/testutil/fakewebkit"
)

// observed is one request that RunOptions.CDPObserve saw.
type observed struct {
	method, path string
	status       int
}

// W5c-2a (design 7.8 test 10): RunOptions.CDPObserve reaches the daemon's
// CDP endpoint, so a test that runs the daemon in-process records every
// HTTP request of the clients with the status it got. A discovery request
// with the token gets 200, one without it 404.
func TestCDPObserveSeesEndpointRequests(t *testing.T) {
	var mu sync.Mutex
	var seen []observed
	port := freePort(t)
	o := startInProcess(t, RunOptions{CDPPort: port, CDPObserve: func(r *http.Request, status int) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, observed{r.Method, r.URL.Path, status})
	}})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	urls := issueURLs(t, c, "s1", f.URL)
	token, endpoint := secretsOf(t, urls.HTTP)
	if code, _ := httpGet(t, urls.HTTP+"/json/version"); code != http.StatusOK {
		t.Fatalf("GET <http form>/json/version: %d, want 200", code)
	}
	if code, _ := httpGet(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/json/version"); code != http.StatusNotFound {
		t.Fatalf("GET /json/version without the token: %d, want 404", code)
	}
	want := []observed{
		{http.MethodGet, "/t/" + token + "/e/" + endpoint + "/json/version", http.StatusOK},
		{http.MethodGet, "/json/version", http.StatusNotFound},
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(want) {
		t.Fatalf("CDPObserve saw %d requests %+v, want %d %+v", len(seen), seen, len(want), want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("CDPObserve request %d = %+v, want %+v", i, seen[i], want[i])
		}
	}
}
