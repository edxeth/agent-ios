package main

import (
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
)

// L01 (w1c backlog), the MCP caller: get_document on a DOM.getDocument
// reply without "root" is a tool error that names the method, not a
// successful null result.
func TestGetDocumentToolWithoutRootIsAnError(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("DOM.getDocument", map[string]any{})
	cs := connectMCP(t, mock)
	res, text := callTool(t, cs, "get_document", map[string]any{})
	if !res.IsError || !strings.Contains(text, "DOM.getDocument") {
		t.Fatalf("get_document: error %v, text %q; want a tool error naming DOM.getDocument", res.IsError, text)
	}
}
