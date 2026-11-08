package main

import (
	"fmt"
	"io"
	"strings"

	reg "github.com/nnemirovsky/iwdp-mcp/internal/commands"
)

// commandHelp documents one command. synopsis follows "iwdp-cli ".
type commandHelp struct {
	name     string
	synopsis string
	summary  string
	details  string // optional option list for the command's own help
}

type helpSection struct {
	title    string
	commands []commandHelp
}

const streamDetails = `Options:
  -d, --duration <d>   Capture time (Go duration such as 30s, or whole seconds). Default 10s.
  -f, --follow         Stream until Ctrl+C instead of stopping after the duration.
                       Only the connect and enable steps then have a deadline.
At the end the command prints a one-line summary with the count to stderr.`

const recordingDetails = `Options:
  -d, --duration <d>   Recording time (Go duration such as 10s, or whole seconds). Default 3s.
The command deadline is --timeout plus the recording time.`

var helpSections = []helpSection{
	{"Core", []commandHelp{
		{"devices", "devices", "List connected iOS devices", ""},
		{"pages", "pages [port]", "List open Safari tabs (default port: 9224)", ""},
		{"status", "status", "Check if ios_webkit_debug_proxy is running", ""},
		{"restart-iwdp", "restart-iwdp", "Restart the ios_webkit_debug_proxy that iwdp-mcp started", "Stops only an iwdp that iwdp-mcp or iwdp-cli started and recorded, then starts\na new one. Another program's iwdp (the user's or another agent's) is never\nstopped: the command fails and names the owner pid and the manual restart\ncommand. When no iwdp runs, it starts one."},
		{"eval", "eval [--await] <expr> [ws-url]", "Evaluate JavaScript", `Options:
  --await   If the expression returns a Promise, wait for it and print the settled value.
Strings print as plain text; other values print as JSON. A thrown exception or a
rejected Promise prints its message and stack to stderr and exits 1.`},
		{"navigate", "navigate <url> [ws-url]", "Navigate to URL", ""},
		{"reload", "reload [--ignore-cache] [ws-url]", "Reload page", ""},
		{"screenshot", "screenshot [--full-page | --selector <css>] [-o file] [ws-url]", "Take screenshot (viewport by default)", `Options:
  --full-page         Capture the whole scrollable document instead of the viewport.
  --selector <css>    Capture only the first element that matches the CSS selector.
  -o, --output <file> Write the PNG here. Default: $TMPDIR/iwdp-screenshot-<timestamp>.png
                      (never the current directory).
Prints the file path, the pixel size and the byte size.`},
		{"snapshot-node", "snapshot-node <nodeId> [-o file] [ws-url]", "Snapshot a DOM node", `Options:
  -o, --output <file> Write the PNG here. Default: $TMPDIR/iwdp-snapshot-<timestamp>.png
                      (never the current directory).`},
		{"cookies", "cookies [--show-values] [ws-url]", "Show all cookies (incl. httpOnly), values redacted", `Options:
  --show-values   Print cookie values in full. By default each value is redacted to
                  its length and a short prefix, so session tokens do not leak into logs.`},
		{"dom", "dom [selector] [ws-url]", "Inspect DOM tree or element", ""},
		{"console", "console [-d 10s] [--follow] [ws-url]", "Print console messages for 10s (or --follow)", streamDetails},
		{"network", "network [-d 10s] [--follow] [ws-url]", "Print network requests for 10s (or --follow)", streamDetails},
	}},
	{"Runtime", []commandHelp{
		{"call-function", "call-function <objectId> <func> [ws-url]", "Call function on object", ""},
		{"get-properties", "get-properties <objectId> [ws-url]", "Get object properties", ""},
	}},
	{"DOM", []commandHelp{
		{"query-selector", "query-selector <selector> [ws-url]", "Find first matching element", ""},
		{"query-selector-all", "query-selector-all <selector> [ws-url]", "Find all matching elements", ""},
		{"get-outer-html", "get-outer-html <nodeId> [ws-url]", "Get element outer HTML", ""},
		{"get-attributes", "get-attributes <nodeId> [ws-url]", "Get element attributes", ""},
		{"get-event-listeners", "get-event-listeners <nodeId> [ws-url]", "Get event listeners", ""},
		{"highlight-node", "highlight-node <nodeId> [ws-url]", "Highlight element in browser", ""},
		{"hide-highlight", "hide-highlight [ws-url]", "Hide highlight overlay", ""},
	}},
	{"CSS", []commandHelp{
		{"get-matched-styles", "get-matched-styles <nodeId> [ws-url]", "Get matching CSS rules", ""},
		{"get-computed-style", "get-computed-style <nodeId> [ws-url]", "Get computed style", ""},
		{"get-inline-styles", "get-inline-styles <nodeId> [ws-url]", "Get inline styles", ""},
		{"set-style-text", "set-style-text <styleId> <text> [ws-url]", "Modify style declaration", ""},
		{"get-all-stylesheets", "get-all-stylesheets [ws-url]", "List all stylesheets (not through iwdp)", ""},
		{"get-stylesheet-text", "get-stylesheet-text <id> [ws-url]", "Get stylesheet source (not through iwdp)", ""},
		{"force-pseudo-state", "force-pseudo-state <nodeId> <states>", "Force :hover,:focus etc.", ""},
	}},
	{"Interaction", []commandHelp{
		{"click", "click <selector> [ws-url]", "Click element", ""},
		{"fill", "fill <selector> <value> [ws-url]", "Fill input field", ""},
		{"type-text", "type-text <text> [ws-url]", "Type text into focused element", ""},
	}},
	{"Storage", []commandHelp{
		{"set-cookie", "set-cookie <name> <val> <domain> [opts] [ws-url]", "Set a cookie and verify it by reading it back", `Options:
  --same-site <Lax|Strict|None>  SameSite policy (any case). Default Lax.
  --path <path>                  Cookie path. Default /.
  --expires <time>               Expiry: Unix seconds, RFC 3339 (2026-12-31T00:00:00Z),
                                 or a duration from now (24h). Default: session cookie.
  --secure                       Send only over HTTPS (required by SameSite=None).
  --http-only                    Hide from JavaScript (alias: --httpOnly).
After setting, the command reads the cookie back with Page.getCookies and exits 1
when it is missing or stored with other attributes. The value is never printed.`},
		{"delete-cookie", "delete-cookie <name> <url> [ws-url]", "Delete a cookie", ""},
		{"get-local-storage", "get-local-storage <origin> [ws-url]", "Get localStorage items", ""},
		{"set-local-storage-item", "set-local-storage-item <origin> <k> <v>", "Set localStorage item", ""},
		{"remove-local-storage-item", "remove-local-storage-item <origin> <key>", "Remove localStorage item", ""},
		{"clear-local-storage", "clear-local-storage <origin> [ws-url]", "Clear all localStorage", ""},
		{"get-session-storage", "get-session-storage <origin> [ws-url]", "Get sessionStorage items", ""},
		{"set-session-storage-item", "set-session-storage-item <origin> <k> <v>", "Set sessionStorage item", ""},
		{"remove-session-storage-item", "remove-session-storage-item <origin> <key>", "Remove sessionStorage item", ""},
		{"clear-session-storage", "clear-session-storage <origin> [ws-url]", "Clear all sessionStorage", ""},
		{"list-indexed-databases", "list-indexed-databases <origin> [ws-url]", "List IndexedDB databases", ""},
		{"get-indexed-db-data", "get-indexed-db-data <origin> <db> <store>", "Query IndexedDB store", ""},
		{"clear-indexed-db-store", "clear-indexed-db-store <origin> <db> <store>", "Clear IndexedDB store", ""},
	}},
	{"Network", []commandHelp{
		{"get-response-body", "get-response-body <requestId> [ws-url]", "Get response body", ""},
		{"set-extra-headers", "set-extra-headers <json> [ws-url]", "Set custom HTTP headers", ""},
		{"set-request-interception", "set-request-interception <true/false>", "Enable/disable interception", ""},
		{"intercept-continue", "intercept-continue <requestId> [opts]", "Continue intercepted request", ""},
		{"intercept-respond", "intercept-respond <id> <status> <body>", "Respond to intercepted request", ""},
		{"set-network-conditions", "set-network-conditions <bytes/sec>", "Throttle network", ""},
		{"disable-cache", "disable-cache <true/false> [ws-url]", "Disable resource caching", ""},
	}},
	{"Console", []commandHelp{
		{"console-messages", "console-messages [-d secs] [ws-url]", "Collect console messages as JSON", recordingDetails},
		{"clear-console", "clear-console [ws-url]", "Clear console", ""},
		{"set-log-level", "set-log-level <source> <level> [ws-url]", "Set logging level", ""},
	}},
	{"Debugger", []commandHelp{
		{"debugger-enable", "debugger-enable [ws-url]", "Enable debugger and switch breakpoints on", "WebKit starts with breakpoints inactive; this command also sends\nDebugger.setBreakpointsActive(true), as Web Inspector does, so breakpoints pause."},
		{"debugger-disable", "debugger-disable [ws-url]", "Disable debugger", ""},
		{"set-breakpoint", "set-breakpoint <url> <line> [col] [opts]", "Set breakpoint by URL", ""},
		{"remove-breakpoint", "remove-breakpoint <id> [ws-url]", "Remove breakpoint", ""},
		{"pause", "pause [ws-url]", "Pause execution", ""},
		{"resume", "resume [ws-url]", "Resume execution", ""},
		{"step-over", "step-over [ws-url]", "Step over", ""},
		{"step-into", "step-into [ws-url]", "Step into", ""},
		{"step-out", "step-out [ws-url]", "Step out", ""},
		{"get-script-source", "get-script-source <scriptId> [ws-url]", "Get script source", ""},
		{"search-in-content", "search-in-content <scriptId> <query>", "Search in script", ""},
		{"eval-on-frame", "eval-on-frame <frameId> <expr> [ws-url]", "Evaluate on call frame", ""},
		{"set-pause-on-exceptions", "set-pause-on-exceptions <none|uncaught|all>", "Configure exception pausing", ""},
	}},
	{"DOMDebugger", []commandHelp{
		{"set-dom-breakpoint", "set-dom-breakpoint <nodeId> <type>", "Break on DOM change", ""},
		{"remove-dom-breakpoint", "remove-dom-breakpoint <nodeId> <type>", "Remove DOM breakpoint", ""},
		{"set-event-breakpoint", "set-event-breakpoint <event> [--type t]", "Break on event", ""},
		{"remove-event-breakpoint", "remove-event-breakpoint <event> [--type t]", "Remove event breakpoint", ""},
		{"set-url-breakpoint", "set-url-breakpoint <url> [--regex]", "Break on URL request", ""},
		{"remove-url-breakpoint", "remove-url-breakpoint <url> [ws-url]", "Remove URL breakpoint", ""},
	}},
	{"Performance", []commandHelp{
		{"timeline-record", "timeline-record [-d secs] [ws-url]", "Record timeline events", recordingDetails},
		{"memory-track", "memory-track [-d secs] [ws-url]", "Track memory usage", recordingDetails},
		{"heap-snapshot", "heap-snapshot [ws-url]", "Take heap snapshot", "Deadline: 5m unless --timeout or IWDP_TIMEOUT is set."},
		{"heap-track", "heap-track [-d secs] [ws-url]", "Track heap allocations", recordingDetails},
		{"heap-gc", "heap-gc [ws-url]", "Force garbage collection", ""},
		{"cpu-profile", "cpu-profile [-d secs] [ws-url]", "CPU profiling", recordingDetails},
		{"script-profile", "script-profile [-d secs] [ws-url]", "Script execution profiling", recordingDetails},
	}},
	{"Animation", []commandHelp{
		{"animation-enable", "animation-enable [ws-url]", "Enable animation tracking", ""},
		{"animation-disable", "animation-disable [ws-url]", "Disable animation tracking", ""},
		{"animation-track", "animation-track [-d secs] [ws-url]", "Track animations", recordingDetails},
		{"get-animation-effect", "get-animation-effect <id> [ws-url]", "Get animation effect", ""},
		{"resolve-animation", "resolve-animation <id> [ws-url]", "Resolve animation object", ""},
	}},
	{"Canvas", []commandHelp{
		{"canvas-enable", "canvas-enable [ws-url]", "Enable canvas tracking", ""},
		{"canvas-disable", "canvas-disable [ws-url]", "Disable canvas tracking", ""},
		{"get-canvas-content", "get-canvas-content <canvasId> [ws-url]", "Get canvas image", ""},
		{"start-canvas-recording", "start-canvas-recording <id> [frames]", "Start canvas recording", ""},
		{"stop-canvas-recording", "stop-canvas-recording <canvasId> [ws-url]", "Stop canvas recording", ""},
		{"get-shader-source", "get-shader-source <progId> <type>", "Get WebGL shader source", ""},
	}},
	{"LayerTree", []commandHelp{
		{"get-layer-tree", "get-layer-tree <nodeId> [ws-url]", "Get compositing layers", ""},
		{"get-compositing-reasons", "get-compositing-reasons <layerId>", "Get compositing reasons", ""},
	}},
	{"Workers", []commandHelp{
		{"worker-enable", "worker-enable [ws-url]", "Enable worker tracking", ""},
		{"worker-disable", "worker-disable [ws-url]", "Disable worker tracking", ""},
		{"send-to-worker", "send-to-worker <workerId> <msg> [ws-url]", "Send message to worker", ""},
		{"get-service-worker-info", "get-service-worker-info [ws-url]", "Get service worker info", ""},
	}},
	{"Audit & Security", []commandHelp{
		{"run-audit", "run-audit <testJSON> [ws-url]", "Run audit", ""},
		{"get-certificate-info", "get-certificate-info <requestId> [ws-url]", "Get TLS certificate info", ""},
	}},
	{"Browser", []commandHelp{
		{"browser-extensions-enable", "browser-extensions-enable [ws-url]", "Enable browser extensions", ""},
		{"browser-extensions-disable", "browser-extensions-disable [ws-url]", "Disable browser extensions", ""},
	}},
}

// synopsisWidth aligns command summaries in the overview.
const synopsisWidth = 44

const globalOptionsHelp = `Global options (any position):
  --timeout <d>   Deadline for the whole command: a Go duration (10s, 2m) or whole
                  seconds. Default 30s, or IWDP_TIMEOUT. On expiry the command exits
                  124 and names the page, the elapsed time and the likely causes.
  -h, --help      Show help and exit 0. Never connects to a page or writes files.
  --session <s>   Session that holds the page lease (IWDP_SESSION; default "default").
  --reservation <owner>  Device reservation owner (IWDP_DEVICE_RESERVATION; default
                  the session). Pages of a physical device need one: 'device reserve'.
  --wait-lease <d>  Wait this long for another session's lease (inside the deadline).
  --takeover      Reattach a page whose connection someone else took (needs the
                  reservation and the device owner's approval).
  --transport <t> auto (default), iwdp, direct or webkitgtk.
Commands run in the iwdp daemon, started on demand ('iwdp-cli daemon --help').
Exit codes: 0 ok, 1 error, 2 usage, 4 page busy or lost, 5 no device or page,
6 device reservation missing, 124 deadline.`

const footerHelp = `Port assignment:
  ios_webkit_debug_proxy listens on port 9221 for device listing.
  Each connected device gets an incremented port starting at 9224:
    Device 1 -> localhost:9224
    Extra devices and the SIMULATOR ghost entry -> localhost:9225+

The ws-url argument is the WebSocket URL from 'pages' output.
If omitted, connects to the first available page across all devices.

Environment:
  IWDP_WS_URL    Default WebSocket URL to connect to
  IWDP_PORT      Default device port (default: 9224)
  IWDP_TIMEOUT   Default command deadline (default: 30s)`

// findCommandHelp returns the help entry of a command.
func findCommandHelp(name string) (commandHelp, bool) {
	for _, s := range helpSections {
		for _, c := range s.commands {
			if c.name == name {
				return c, true
			}
		}
	}
	return commandHelp{}, false
}

// printUsage writes the overview of all commands.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, "iwdp-cli — iOS Safari debugging CLI")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: iwdp-cli <command> [options]")
	fmt.Fprintln(w, "       iwdp-cli <command> --help")
	for _, s := range helpSections {
		fmt.Fprintf(w, "\n%s:\n", s.title)
		for _, c := range s.commands {
			fmt.Fprintf(w, "  %-*s %s\n", synopsisWidth, c.synopsis, c.summary)
		}
	}
	_, _ = fmt.Fprintf(w, "\nDaemon, session, lease and device commands:\n")
	for _, c := range reg.All() {
		_, _ = fmt.Fprintf(w, "  %-*s %s\n", synopsisWidth, c.Name, c.Summary)
	}
	fmt.Fprintf(w, "\n%s\n\n%s\n", globalOptionsHelp, footerHelp)
}

// printCommandHelp writes the help of one command. It reports false for an
// unknown command.
func printCommandHelp(w io.Writer, name string) bool {
	c, ok := findCommandHelp(name)
	if !ok {
		if rc, n := reg.Lookup(strings.Fields(name)); rc != nil && n == len(strings.Fields(name)) {
			_, _ = fmt.Fprint(w, reg.Help(rc))
			return true
		}
		return false
	}
	fmt.Fprintf(w, "Usage: iwdp-cli %s\n\n%s.\n", c.synopsis, c.summary)
	if c.details != "" {
		fmt.Fprintf(w, "\n%s\n", c.details)
	}
	if strings.Contains(c.synopsis, "ws-url") {
		fmt.Fprintln(w, "\nws-url: WebSocket URL from 'iwdp-cli pages'. Default: IWDP_WS_URL, then the first debuggable page.")
	}
	fmt.Fprintf(w, "\n%s\n", globalOptionsHelp)
	return true
}

// wantsHelp reports whether command arguments ask for help.
func wantsHelp(args []string) bool {
	return hasFlag(args, "--help", "-h")
}
