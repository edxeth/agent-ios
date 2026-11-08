package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/proxy"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// closeCounter is a page client that counts its Close calls.
type closeCounter struct {
	webkit.Caller
	closed int
}

func (c *closeCounter) Close() error {
	c.closed++
	return nil
}

// L06: iwdp_status makes one status call (proxy.Status: one listing check
// for both steps) and reports each outcome. proxy.Status itself is tested
// in internal/proxy (TestStatusSpendsOneProbeBudget).
func TestIWDPStatusToolMakesOneStatusCall(t *testing.T) {
	old := iwdpStatus
	t.Cleanup(func() { iwdpStatus = old })
	stuck := errors.New("ios_webkit_debug_proxy on 127.0.0.1:9221 does not answer GET /json within 5s: it is busy or stuck")
	notListening := fmt.Errorf("%w (127.0.0.1:9221)", proxy.ErrNotListening)
	cs := mcpServerSession(t)
	for _, c := range []struct {
		name      string
		args      map[string]any
		started   bool
		err       error
		wantStart bool
		want      []string
	}{
		{"running", nil, false, nil, true, []string{`"running":true`, "is running on port 9221"}},
		{"started", nil, true, nil, true, []string{`"running":true`, `"started":true`, "started it automatically"}},
		{"not running, no auto-start", map[string]any{"auto_start": false}, false, notListening, false, []string{`"running":false`, "ios_webkit_debug_proxy --no-frontend"}},
		{"stuck, auto-start", nil, false, stuck, true, []string{`"running":false`, "failed to start iwdp: ", "busy or stuck"}},
		{"stuck, no auto-start", map[string]any{"auto_start": false}, false, stuck, false, []string{`"running":false`, "does not answer: ", "busy or stuck"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls []bool
			iwdpStatus = func(_ context.Context, autoStart bool) (bool, error) {
				calls = append(calls, autoStart)
				return c.started, c.err
			}
			_, text := callTool(t, cs, "iwdp_status", c.args)
			if len(calls) != 1 || calls[0] != c.wantStart {
				t.Fatalf("iwdp_status made the status calls %v, want one with auto-start %v", calls, c.wantStart)
			}
			for _, want := range c.want {
				if !strings.Contains(text, want) {
					t.Errorf("iwdp_status = %s, want %q", text, want)
				}
			}
		})
	}
}

// L02: restart_iwdp passes on Restart's refusal, keeps the page client
// then (a refused restart changes nothing), and says that it restarts only
// an iwdp that iwdp-mcp started. A restart closes the client of the old
// iwdp.
func TestRestartIWDPToolReportsTheRefusal(t *testing.T) {
	old := restartIWDP
	t.Cleanup(func() { restartIWDP = old })
	cs := mcpServerSession(t)
	client := &closeCounter{}
	sess.mu.Lock()
	sess.client = client
	sess.mu.Unlock()
	refusal := errors.New("restart refused: 127.0.0.1:9221 is held by pid 4242, which is not an ios_webkit_debug_proxy that iwdp-mcp started")
	restartIWDP = func(context.Context) error { return refusal }
	_, text := callTool(t, cs, "restart_iwdp", nil)
	if !strings.Contains(text, `"restarted":false`) || !strings.Contains(text, refusal.Error()) {
		t.Errorf("restart_iwdp with a refusal = %s, want restarted false and the refusal", text)
	}
	sess.mu.Lock()
	kept := sess.client == pageClient(client)
	sess.mu.Unlock()
	if !kept || client.closed != 0 {
		t.Errorf("a refused restart_iwdp changed the page client (kept %v, closed %d times)", kept, client.closed)
	}
	restartIWDP = func(context.Context) error { return nil }
	if _, text := callTool(t, cs, "restart_iwdp", nil); !strings.Contains(text, `"restarted":true`) {
		t.Errorf("restart_iwdp = %s, want restarted true", text)
	}
	sess.mu.Lock()
	cleared := sess.client == nil
	sess.mu.Unlock()
	if !cleared || client.closed != 1 {
		t.Errorf("restart_iwdp left the old page client (cleared %v, closed %d times)", cleared, client.closed)
	}
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name == "restart_iwdp" {
			for _, want := range []string{"only when iwdp-mcp started it", "never another iwdp", "refuses"} {
				if !strings.Contains(tool.Description, want) {
					t.Errorf("restart_iwdp description %q lacks %q", tool.Description, want)
				}
			}
			return
		}
	}
	t.Fatal("restart_iwdp is not registered")
}
