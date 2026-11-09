package main

import (
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// Review devfix-R1 F2 (device check 3, P3-H): get_console_messages of the
// legacy profile leaves out the runtime's "__iwdp_" messages and keeps the
// page's own.
func TestReviewLegacyMCPConsoleMarker(t *testing.T) {
	t.Setenv("IWDP_MCP_TOOLS", "legacy")
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Console.enable", map[string]interface{}{})
	cs := connectMCP(t, mock)
	r, text := callTool(t, cs, "console_enable", nil)
	if r.IsError {
		t.Fatalf("console_enable: %s", text)
	}
	defer func() { sess.mu.Lock(); sess.consoleCollector = nil; sess.mu.Unlock() }()
	for _, msg := range []string{"page message", "__iwdp_dialog__ fixture"} {
		if err := mock.SendEvent("Console.messageAdded", map[string]interface{}{"message": map[string]interface{}{"level": "debug", "text": msg}}); err != nil {
			t.Fatal(err)
		}
	}
	end := time.Now().Add(3 * time.Second)
	for {
		r, text = callTool(t, cs, "get_console_messages", nil)
		if r.IsError {
			t.Fatalf("get_console_messages: %s", text)
		}
		if strings.Contains(text, "page message") {
			break
		}
		if time.Now().After(end) {
			t.Fatal("page fixture not delivered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(text, "__iwdp_dialog__") {
		t.Fatalf("runtime marker in legacy MCP console output: %s", text)
	}
}
