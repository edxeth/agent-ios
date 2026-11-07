package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// R2-7, design 2.6: when the daemon shuts down, the MCP server's selected
// page ends with it: the next tool call asks for select_page again instead
// of using a caller whose heartbeat is gone, and the selection and its
// collectors are dropped.
func TestSelectedPageEndsWithTheDaemon(t *testing.T) {
	sock := privateDaemonDir(t)
	t.Setenv("IWDP_SESSION", "mcp-r2")
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 2}})
	cs := mcpServerSession(t)
	if res, text := callTool(t, cs, "select_page", map[string]any{"websocket_url": mock.URL}); res.IsError {
		t.Fatalf("select_page: %s", text)
	}
	sess.mu.Lock()
	pc, _ := sess.client.(*daemon.PageCaller)
	sess.mu.Unlock()
	if pc == nil {
		t.Fatal("select_page did not attach through the daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := daemon.Connect(ctx, daemon.Options{Dir: sock, NoStart: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Shutdown(ctx, true); err != nil {
		t.Fatalf("daemon shutdown: %v", err)
	}
	_ = c.WaitGone(ctx, 7*time.Second)
	c.Close()
	select {
	case <-pc.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the selected page's Done stayed open after the daemon shut down")
	}
	res, text := callTool(t, cs, "evaluate_script", map[string]any{"expression": "1+1", "return_by_value": true})
	if !res.IsError || !strings.Contains(text, "select_page") {
		t.Fatalf("evaluate_script after the daemon ended: error %v, %q; want a request to select the page again", res.IsError, text)
	}
	sess.mu.Lock()
	cur := sess.client
	sess.mu.Unlock()
	if cur != nil {
		t.Fatal("the ended page is still selected")
	}
}
