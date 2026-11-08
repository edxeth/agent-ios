package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/nnemirovsky/iwdp-mcp/internal/commands"
)

// coreLegacyTools are the legacy tools of the default MCP profile "core"
// (decision D2): page selection, navigation and screenshots, evaluation,
// DOM reads, computed styles, interaction, network, cookies and storage,
// and console. With the core registry tools (session_info, close_session
// and the W3a tools snapshot, diff_snapshot, get, is, find, hit_test and
// read_page) the profile has 47 tools. IWDP_MCP_TOOLS=all registers every tool. This
// list is the single definition of the profile; docs/notes/w1b.md lists it
// for the README.
var coreLegacyTools = []string{
	// iwdp, devices and pages
	"iwdp_status", "list_devices", "list_pages", "select_page",
	// page
	"navigate", "reload", "take_screenshot", "snapshot_node",
	// runtime
	"evaluate_script", "call_function", "get_properties",
	// DOM
	"get_document", "query_selector", "query_selector_all", "get_outer_html",
	"get_attributes", "highlight_node", "hide_highlight",
	// CSS (computed and matched styles work through Target routing)
	"get_computed_style", "get_matched_styles",
	// interaction
	"click", "fill", "type_text",
	// network
	"network_enable", "network_disable", "list_network_requests", "get_response_body", "set_extra_headers",
	// cookies and storage
	"get_cookies", "set_cookie", "delete_cookie", "get_local_storage", "set_local_storage_item", "get_session_storage",
	// console
	"console_enable", "console_disable", "get_console_messages", "clear_console",
}

// legacyToolSelected reports whether the selected profile registers the
// legacy tool name: profile legacy (or all) registers every legacy tool,
// profile core the coreLegacyTools.
func legacyToolSelected(name string) bool {
	p := mcpProfile()
	return commands.ProfileIncludes(p, commands.ProfileLegacy) ||
		(commands.ProfileIncludes(p, commands.ProfileCore) && slices.Contains(coreLegacyTools, name))
}

// mcpProfile is the selected MCP profile (IWDP_MCP_TOOLS). main checks
// the value with selectProfile before it registers any tool, so an error
// here means a tool was registered without that check.
func mcpProfile() string {
	p, err := commands.SelectedProfile(os.Getenv)
	if err != nil {
		panic(fmt.Sprintf("MCP profile not checked before registration: %v", err))
	}
	return p
}

// selectProfile applies the --tools flag of the server's arguments (design
// 4.1: "IWDP_MCP_TOOLS=core,devtools,legacy or --tools"); the flag
// overrides IWDP_MCP_TOOLS. It returns an error for a missing or unknown
// value. Other arguments are left alone, as before.
func selectProfile(args []string) error {
	for i := 0; i < len(args); i++ {
		v, ok := strings.CutPrefix(args[i], "--tools=")
		if !ok && args[i] == "--tools" {
			if i+1 >= len(args) {
				return fmt.Errorf("--tools needs a value: all, or a comma-separated list of core, devtools and legacy")
			}
			i++
			v, ok = args[i], true
		}
		if ok {
			if err := os.Setenv("IWDP_MCP_TOOLS", v); err != nil {
				return fmt.Errorf("--tools %q: %w", v, err)
			}
		}
	}
	_, err := commands.SelectedProfile(os.Getenv)
	return err
}
