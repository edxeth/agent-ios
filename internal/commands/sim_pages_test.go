package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Review F8: a page list that iwdp answers with a status other than 200
// is a failed read, also when its body is a valid empty list, so that sim
// up reports the failed listing and not "no tab of the --url" (exit 7).
func TestSimulatorPagesRefusesAnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	if pages, err := simulatorPagesAt(context.Background(), srv.URL+"/json"); err == nil {
		t.Fatalf("a 503 answer with body [] was read as the page list %v", pages)
	}
}

// A 200 answer is the page list: each page's id is its
// webSocketDebuggerUrl.
func TestSimulatorPagesReadsTheList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"url":"http://10.0.2.2:8791/","webSocketDebuggerUrl":"ws://127.0.0.1:9224/devtools/page/3"}]`))
	}))
	defer srv.Close()
	pages, err := simulatorPagesAt(context.Background(), srv.URL+"/json")
	if err != nil || len(pages) != 1 || pages[0].ID != "ws://127.0.0.1:9224/devtools/page/3" || pages[0].URL != "http://10.0.2.2:8791/" {
		t.Fatalf("pages %+v, %v", pages, err)
	}
}
