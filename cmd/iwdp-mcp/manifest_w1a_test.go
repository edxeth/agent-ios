package main

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nnemirovsky/iwdp-mcp/internal/commands"
)

// 7.3 test 12: the MCP registration manifest has no duplicate tool name.
func TestRegistrationManifestHasNoDuplicateToolName(t *testing.T) {
	legacyToolNames = nil
	registerTools(mcp.NewServer(&mcp.Implementation{Name: "iwdp-mcp", Version: "test"}, nil))
	if len(legacyToolNames) < 100 {
		t.Fatalf("recorded %d legacy tools, want the full legacy set", len(legacyToolNames))
	}
	m, err := commands.Manifest(mcpProfile(), legacyToolNames)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range m {
		if seen[e.Name] {
			t.Fatalf("tool %q is in the manifest twice", e.Name)
		}
		seen[e.Name] = true
	}
}

// Design 2.10: when a registry command reuses an existing tool name, the
// legacy handler is not registered and the registry command serves the
// name. registerTools must register the replacement, not only skip the
// legacy tool.
func TestRegistryCommandReplacesLegacyTool(t *testing.T) {
	c := &commands.Command{
		Name: "fixture list devices", MCPName: "list_devices", Profile: commands.ProfileCore,
		Summary: "Registry replacement of list_devices.",
		Run: func(context.Context, commands.Env, commands.Input) (commands.Result, error) {
			return commands.Result{Text: "registry handler ran"}, nil
		},
	}
	commands.Register(c)
	t.Cleanup(func() { commands.Unregister(c) })
	server := mcp.NewServer(&mcp.Implementation{Name: "iwdp-mcp", Version: "test"}, nil)
	registerTools(server)
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	n := 0
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name == "list_devices" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("server lists list_devices %d times, want exactly 1 (the registry command)", n)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_devices"})
	if err != nil || res.IsError {
		t.Fatalf("call list_devices: %v %+v", err, res)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; text != "registry handler ran" {
		t.Fatalf("list_devices answered %q, want the registry handler", text)
	}
}
