package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// mcpEndpoint is the result of the get_cdp_url tool (cdp.Endpoint).
type mcpEndpoint struct {
	WS   string `json:"ws"`
	Page string `json:"page"`
}

// Design docs/design/agentic-parity.md 5.2 through MCP: the get_cdp_url
// tool selects the page that the MCP session's other tools act on, as the
// CLI form does for the same session: the tab chosen with tab_select, and
// the page chosen with select_page. Both forms give the same record.
func TestGetCDPURLToolSelectsTheSessionsPage(t *testing.T) {
	sock := privateDaemonDir(t)
	t.Setenv("IWDP_SESSION", "mcp-a")
	page := func() *fakewebkit.Server {
		f := fakewebkit.New(fakewebkit.Options{})
		t.Cleanup(f.Close)
		f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 2}})
		return f
	}
	fixture, other := page(), page()
	l := fakewebkit.NewListing(false)
	t.Cleanup(l.Close)
	dev := l.AddDevice("SIMULATOR", "Simulator", "16.7")
	dev.AddPage("Fixture", "https://fixture.example/", fixture.URL)
	dev.AddPage("Other", "https://other.example/", other.URL)
	if code := runDaemonCommand([]string{"daemon", "start", "--transport", "iwdp", "--addr", l.Addr()}); code != 0 {
		t.Fatalf("iwdp-mcp daemon start: exit %d", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := daemon.Connect(ctx, daemon.Options{Dir: sock, NoStart: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Port 0: the endpoint binds a free port itself (get_cdp_url keeps a
	// running endpoint).
	if _, err := c.Run(ctx, daemon.RunRequest{Argv: []string{"cdp", "start", "--port", "0"}}, nil); err != nil {
		t.Fatalf("cdp start --port 0: %v", err)
	}
	cs := mcpServerSession(t)
	issue := func(what string) mcpEndpoint {
		t.Helper()
		res, text := callTool(t, cs, "get_cdp_url", map[string]any{})
		var e mcpEndpoint
		if res.IsError || json.Unmarshal([]byte(text), &e) != nil {
			t.Fatalf("get_cdp_url %s: error %v, %q", what, res.IsError, text)
		}
		return e
	}

	for _, call := range []struct {
		name string
		args map[string]any
	}{{"tab_list", map[string]any{}}, {"tab_select", map[string]any{"id": "t2"}}} {
		if res, text := callTool(t, cs, call.name, call.args); res.IsError {
			t.Fatalf("%s: %s", call.name, text)
		}
	}
	e := issue("after tab_select t2")
	if e.Page != "Other" {
		t.Fatalf("get_cdp_url after tab_select t2 selected %q, want the tab's page Other", e.Page)
	}
	// The CLI form of the same session (iwdp-cli --session mcp-a get
	// cdp-url) gets the same record.
	cli, err := c.Run(ctx, daemon.RunRequest{Argv: []string{"get", "cdp-url"}, Owner: daemon.Owner{Session: "mcp-a"}}, nil)
	if err != nil || cli.Text != e.WS {
		t.Fatalf("the CLI form for session mcp-a = %q, %v; want the MCP tool's URL %q", cli.Text, err, e.WS)
	}

	if res, text := callTool(t, cs, "select_page", map[string]any{"websocket_url": fixture.URL}); res.IsError {
		t.Fatalf("select_page: %s", text)
	}
	if e := issue("after select_page"); e.Page != "Fixture" {
		t.Fatalf("get_cdp_url after select_page of Fixture selected %q, want Fixture", e.Page)
	}
}
