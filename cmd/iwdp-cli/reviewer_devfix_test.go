package main

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/proxy"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// Review devfix-R1 F2 (device check 3, P3-H): the CLI's legacy JSON
// console command leaves out the runtime's "__iwdp_" messages and keeps the
// page's own.
func TestReviewLegacyConsoleMessagesMarker(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.Handle("Console.enable", func(string, json.RawMessage) (interface{}, error) {
		go func() {
			time.Sleep(100 * time.Millisecond)
			for _, text := range []string{"page message", "__iwdp_dialog__ fixture"} {
				_ = mock.SendEvent("Console.messageAdded", map[string]interface{}{"message": map[string]interface{}{"level": "debug", "text": text}})
			}
		}()
		return map[string]interface{}{}, nil
	})
	res := runCLI(t, "", []string{"IWDP_WS_URL=" + mock.URL}, "console-messages", "-d", "1")
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	if !strings.Contains(res.stdout, "page message") {
		t.Fatal("page fixture not delivered")
	}
	if strings.Contains(res.stdout, "__iwdp_dialog__") {
		t.Fatalf("runtime marker in CLI console output: %s", res.stdout)
	}
}

// Review devfix-R1 F3 (device check 3, P3-D): status names the iwdp log
// when an owned listing is broken, too.
func TestReviewBrokenListingStatusLogPath(t *testing.T) {
	listener, err := net.Listen("tcp", proxy.ListingAddr()) // private network namespace only
	if err != nil {
		t.Fatal(err)
	}
	listing := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fixture broken", http.StatusServiceUnavailable)
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = listing.Serve(listener) }()
	defer func() { _ = listing.Close() }()
	res := runCLI(t, "", nil, "status")
	if res.code == 0 {
		t.Fatal("broken listing unexpectedly healthy")
	}
	if !strings.Contains(res.stdout+res.stderr, "iwdp log of an iwdp that iwdp-mcp starts:") {
		t.Fatalf("status omits log path on the listing failure: stdout %q, stderr %q", res.stdout, res.stderr)
	}
}
