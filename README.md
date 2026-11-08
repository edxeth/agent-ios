# iOS WebKit Debug Proxy MCP Server + CLI

[![CI](https://github.com/nnemirovsky/iwdp-mcp/actions/workflows/test.yml/badge.svg)](https://github.com/nnemirovsky/iwdp-mcp/actions/workflows/test.yml)
[![Lint](https://github.com/nnemirovsky/iwdp-mcp/actions/workflows/lint.yml/badge.svg)](https://github.com/nnemirovsky/iwdp-mcp/actions/workflows/lint.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/nnemirovsky/iwdp-mcp)](https://goreportcard.com/report/github.com/nnemirovsky/iwdp-mcp)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

[![Install in Claude Code](https://img.shields.io/badge/Claude_Code-Install_Plugin-F97316?style=flat-square&logo=claude&logoColor=white)](#claude-code)
[![Install in VS Code](https://img.shields.io/badge/VS_Code-Install_Server-0098FF?style=flat-square&logo=visualstudiocode&logoColor=white)](https://vscode.dev/redirect/mcp/install?name=iwdp-mcp&config=%7B%22command%22%3A%22iwdp-mcp%22%7D)
[![Install in VS Code Insiders](https://img.shields.io/badge/VS_Code_Insiders-Install_Server-24bfa5?style=flat-square&logo=visualstudiocode&logoColor=white)](https://insiders.vscode.dev/redirect/mcp/install?name=iwdp-mcp&config=%7B%22command%22%3A%22iwdp-mcp%22%7D&quality=insiders)
[![Install in Cursor](https://img.shields.io/badge/Cursor-Install_Server-7B3FE4?style=flat-square&logo=cursor&logoColor=white)](https://cursor.com/en/install-mcp?name=iwdp-mcp&config=eyJjb21tYW5kIjoiaXdkcC1tY3AifQ==)
[![Install in Windsurf](https://img.shields.io/badge/Windsurf-Install_Server-5C2D91?style=flat-square&logo=codeium&logoColor=white)](#windsurf)
[![Install in Codex](https://img.shields.io/badge/Codex_CLI-Install_Server-74AA9C?style=flat-square&logo=openai&logoColor=white)](#codex-cli)
[![Install in Antigravity](https://img.shields.io/badge/Antigravity-Install_Server-4285F4?style=flat-square&logo=google&logoColor=white)](#antigravity)
[![Install in JetBrains](https://img.shields.io/badge/JetBrains-Install_Server-000000?style=flat-square&logo=jetbrains&logoColor=white)](#jetbrains-ides)

**Debug iOS Safari from your AI agent.** No existing tool lets AI assistants interact with Safari on a real iPhone. Chrome DevTools MCP speaks CDP, which is incompatible with WebKit. This project bridges that gap.

<video src="https://github.com/user-attachments/assets/dcc0dc05-3a1f-4454-88a8-51ee28da9fd2" poster="https://github.com/user-attachments/assets/3901f7ff-6531-4866-b49e-cf5d17304327" autoplay loop muted playsinline></video>

An agentic loop in the style of agent-browser (snapshots with `@eN` refs, actions, waits, dialogs, tabs, frames), DevTools extras (network log, routes, HAR, console, overrides, storage state, traces, profiles, Web Vitals), an opt-in CDP endpoint for agent-browser, Puppeteer, Playwright and chrome-devtools-mcp, and the 100+ legacy tools across the WebKit Inspector Protocol domains. A background daemon owns every page connection, so the CLI, the MCP server and CDP clients share sessions, leases and refs.

Documentation: [docs/README.md](docs/README.md) (user guide, sessions and leases, synthetic input, CDP, device runbook, migration, known limits) and the generated [command reference](docs/commands.md).

## Why?

- **Chrome DevTools MCP** exists for desktop Chrome. **Nothing existed for iOS Safari** — until now.
- **httpOnly cookies** are invisible to `document.cookie`. This tool reads them via the protocol.
- **Real device testing** catches iOS-specific bugs that simulators miss.
- Works with **Claude Code, VS Code, Cursor, Windsurf, Codex CLI, Antigravity, JetBrains**, and any MCP-compatible client.

## Installation

### Prerequisites

```bash
# Install ios-webkit-debug-proxy (macOS)
brew install ios-webkit-debug-proxy

# Connect an iOS device via USB and enable Web Inspector:
# Settings → Safari → Advanced → Web Inspector → ON
```

For Linux and other platforms, see the [ios-webkit-debug-proxy installation guide](https://github.com/google/ios-webkit-debug-proxy#installation).

### Claude Code Plugin (recommended)

Inside Claude Code, run:

```
/plugin marketplace add nnemirovsky/iwdp-mcp
/plugin install iwdp-mcp
```

### Go Install

```bash
go install github.com/nnemirovsky/iwdp-mcp/cmd/...@latest
```

### Pre-built Binaries

Download from [GitHub Releases](https://github.com/nnemirovsky/iwdp-mcp/releases).

### Build from Source

```bash
git clone https://github.com/nnemirovsky/iwdp-mcp.git
cd iwdp-mcp
make build
```

## Quick Start

### CLI

```bash
# Start the proxy
ios_webkit_debug_proxy --no-frontend &

# List connected devices (port 9221)
iwdp-cli devices

# List Safari tabs on the first device (port 9224)
iwdp-cli pages

# Evaluate JavaScript (exceptions print message and stack and exit 1)
iwdp-cli eval "document.title"
iwdp-cli eval --await "fetch('/api/me').then(r => r.status)"

# Take a screenshot: viewport by default, or --full-page, or --selector <css>.
# Without -o the PNG goes to $TMPDIR/iwdp-screenshot-<timestamp>.png.
iwdp-cli screenshot
iwdp-cli screenshot --selector '#checkout' -o checkout.png

# Show all cookies (including httpOnly); values are redacted unless --show-values
iwdp-cli cookies

# Set a cookie (SameSite defaults to Lax) and verify it by reading it back
iwdp-cli set-cookie session abc123 example.com --same-site Lax --secure --http-only --expires 24h

# Print console messages or network requests for 10s (-d 30s, or --follow to stream)
iwdp-cli console
iwdp-cli network -d 30s

# Help for one command (never connects to a page)
iwdp-cli screenshot --help
```

### The agentic loop

```bash
export IWDP_SESSION=my-agent                    # one stable session per agent
iwdp-cli device reserve --owner my-agent        # physical devices need a reservation
iwdp-cli open https://example.com/login         # navigate and wait for load
iwdp-cli snapshot -i                            # accessibility tree with @eN refs
iwdp-cli fill @e3 user@example.com
iwdp-cli dialog accept                          # arm the answer before the action
iwdp-cli click @e5                              # synthetic events after actionability checks
iwdp-cli wait --url '**/home'
iwdp-cli close && iwdp-cli device release --owner my-agent
```

All input is synthetic (`isTrusted=false`); see [docs/synthetic-input.md](docs/synthetic-input.md). Sessions, leases and reservations: [docs/sessions.md](docs/sessions.md). The MCP server registers the `core` profile (47 tools) by default; `IWDP_MCP_TOOLS=all` or `iwdp-mcp --tools all` registers every tool ([MCP profiles](docs/guide.md#mcp-profiles)). A CDP URL for other clients: `iwdp-cli get cdp-url` ([docs/cdp.md](docs/cdp.md)).

Every command runs under a deadline: 30 s by default, set with `--timeout <duration>` (any position) or `IWDP_TIMEOUT`. Recording commands add their `-d` duration; `heap-snapshot` defaults to 5 min. On expiry the command exits 124 and prints the page, the elapsed time and the likely causes (another inspector session holds the page, the page is stalled or shows a dialog, the device is locked).

### MCP Server

<details id="claude-code">
<summary><strong>Claude Code</strong></summary>

Install as a plugin (recommended):

```
/plugin marketplace add nnemirovsky/iwdp-mcp
/plugin install iwdp-mcp
```

Or add to your project's `.mcp.json`:

```json
{
  "mcpServers": {
    "iwdp-mcp": {
      "command": "iwdp-mcp"
    }
  }
}
```

Or via CLI:

```sh
claude mcp add iwdp-mcp -- iwdp-mcp
```
</details>

<details>
<summary><strong>Claude Desktop</strong></summary>

Add to `~/Library/Application Support/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "iwdp-mcp": {
      "command": "iwdp-mcp"
    }
  }
}
```
</details>

<details>
<summary><strong>VS Code / VS Code Insiders</strong></summary>

Click the install badges at the top of this README, or add to `.vscode/mcp.json`:

```json
{
  "servers": {
    "iwdp-mcp": {
      "command": "iwdp-mcp"
    }
  }
}
```

Or via CLI:

```sh
code --add-mcp '{"name":"iwdp-mcp","command":"iwdp-mcp"}'
```
</details>

<details>
<summary><strong>Cursor</strong></summary>

Click the Cursor install badge at the top of this README, or add to `.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "iwdp-mcp": {
      "command": "iwdp-mcp"
    }
  }
}
```
</details>

<details id="windsurf">
<summary><strong>Windsurf</strong></summary>

Add to `~/.codeium/windsurf/mcp_config.json`:

```json
{
  "mcpServers": {
    "iwdp-mcp": {
      "command": "iwdp-mcp"
    }
  }
}
```
</details>

<details id="codex-cli">
<summary><strong>Codex CLI</strong></summary>

```sh
codex mcp add iwdp-mcp -- iwdp-mcp
```

Or add to `~/.codex/config.toml`:

```toml
[mcp_servers.iwdp-mcp]
command = "iwdp-mcp"
```
</details>

<details id="antigravity">
<summary><strong>Antigravity</strong></summary>

Add to `~/.gemini/antigravity/mcp_config.json`:

```json
{
  "mcpServers": {
    "iwdp-mcp": {
      "command": "iwdp-mcp"
    }
  }
}
```
</details>

<details id="jetbrains-ides">
<summary><strong>JetBrains IDEs</strong></summary>

Go to **Settings → Tools → AI Assistant → Model Context Protocol (MCP) → Add**, or add the following JSON config:

```json
{
  "mcpServers": {
    "iwdp-mcp": {
      "command": "iwdp-mcp"
    }
  }
}
```
</details>

### Example Prompts

Once the MCP server is configured, you can ask your AI assistant things like:

**Getting started**
```
Connect to my iPhone's Safari and show me what tabs are open
Take a screenshot of the current page
What's the page title and URL?
```

**JavaScript & DOM**
```
Run document.querySelectorAll('a') and list all links on the page
Find all elements with class "error" and show their text content
Get the full DOM tree of the page
What event listeners are attached to the submit button?
Highlight the navigation bar so I can see its bounds
```

**CSS & Styles**
```
What CSS rules are applied to the header element?
Show me the computed styles for the main content area
Force the :hover state on the dropdown menu
Change the background color of .hero to #f0f0f0
List all stylesheets loaded on this page
```

**Network**
```
Monitor network requests while I interact with the page
Show me the response body of that failed API call
Intercept all requests to api.example.com and log them
Block all requests to analytics.google.com
Set a custom Authorization header for all requests
Throttle the network to simulate a slow 3G connection
Disable the browser cache and reload the page
```

**Storage & Cookies**
```
Show me all cookies including httpOnly and secure ones
Set a session cookie named "debug" with value "true"
Delete the authentication cookie
What's in localStorage for this site?
Set a localStorage item "theme" to "dark"
List all IndexedDB databases and their object stores
Show me the data in the "users" object store
```

**Debugging**
```
Set a breakpoint in main.js at line 42
Pause execution and show me the call stack
Step through the code and show variable values
What's the source of the script at bundle.js?
Search for "addEventListener" across all loaded scripts
Break on any uncaught exceptions
Set a DOM breakpoint — break when the #content div is modified
```

**Performance & Profiling**
```
Start a timeline recording while I navigate through the app
Take a heap snapshot and find potential memory leaks
Profile the CPU usage while this animation runs
Track memory usage while I scroll through the feed
Force a garbage collection
```

**Advanced**
```
List all compositing layers and why they were composited
Show me all active CSS animations on the page
Capture the canvas content as an image
Get the TLS certificate details for this page
Check if there are any web workers running
```

## How It Works

```
┌────────────┐    USB     ┌──────────┐   HTTP/WS    ┌──────────┐
│ iOS Device │◄──────────►│   iwdp   │◄────────────►│ iwdp-mcp │
│  (Safari)  │            │ :9221-N  │              │  or CLI  │
└────────────┘            └──────────┘              └──────────┘
```

`ios-webkit-debug-proxy` exposes:
- **Port 9221** — lists all connected devices
- **Port 9222+** — each device gets an incremented port listing its Safari tabs
- Each tab provides a **WebSocket URL** for the WebKit Inspector Protocol

`iwdp-mcp` and `iwdp-cli` start a per-user daemon (`<binary> daemon run`) that connects to those WebSocket endpoints, keeps the page connections, and serves the CLI, the MCP server and the optional CDP endpoint over a private Unix socket. `--transport direct` reaches the device through usbmuxd without iwdp (opt-in), and `--transport webkitgtk` drives a local WebKitGTK MiniBrowser for tests. See [docs/guide.md](docs/guide.md).

## Tools

### Core
| Tool | Description |
|------|-------------|
| `iwdp_status` | Check/auto-start ios-webkit-debug-proxy |
| `restart_iwdp` | Restart iwdp after a crash, only when iwdp-mcp started it; refuses for any other iwdp ([runbook](docs/device-runbook.md)) |
| `list_devices` | List connected iOS devices (HTTP GET :9221) |
| `list_pages` | List Safari tabs (HTTP GET :9222+) |
| `select_page` | Connect to a specific tab |
| `navigate` | Go to URL |
| `take_screenshot` | Capture the viewport (default), the full page (`full_page`) or one element (`selector`) as PNG |
| `evaluate_script` | Run JavaScript (`await_promise` waits for a returned Promise; exceptions return message and stack) |
| `get_document` | Get DOM tree |
| `query_selector` | Find elements by CSS selector |

### DOM & CSS
`get_outer_html`, `get_attributes`, `get_event_listeners`, `highlight_node`, `get_matched_styles`, `get_computed_style`, `set_style_text`, `force_pseudo_state`, and more.

### Network
`network_enable`, `list_network_requests`, `get_response_body`, `set_request_interception`, `intercept_continue`, `intercept_with_response`, `set_emulated_conditions`, `set_resource_caching_disabled`.

### Storage
`get_cookies` (httpOnly + secure; values redacted unless `show_values`), `set_cookie` (SameSite defaults to Lax, `expires` in Unix seconds, verified by reading back), `delete_cookie`, `get_local_storage`, `get_session_storage`, `list_indexed_databases`, `get_indexed_db_data`.

### Debugging
`debugger_enable`, `set_breakpoint`, `pause`, `resume`, `step_over`, `step_into`, `step_out`, `get_script_source`, `evaluate_on_call_frame`, `set_pause_on_exceptions`.

### Performance
`timeline_start/stop`, `memory_start/stop_tracking`, `heap_snapshot`, `cpu_start/stop_profiling`, `script_start/stop_profiling`.

### More
Animation, Canvas, LayerTree, Workers, Audit, Security (TLS certificates), and element interaction (`click`, `fill`, `type_text`).

## Development

```bash
make build              # Build both binaries
make test               # Run unit + smoke tests
make test-e2e           # E2E tests (boots iOS Simulator, tests all tools)
make test-coverage      # Tests with coverage report
make lint               # golangci-lint
make fmt                # gofumpt formatting
make docs               # regenerate docs/commands.md from the command registry
```

### Testing

Unit tests use a mock WebSocket server (`internal/webkit/testutil/`) that simulates the WebKit Inspector Protocol. E2E tests boot an iOS Simulator and run every tool against real Safari.

```bash
# Unit + smoke tests (no device needed)
make test

# E2E tests — boots iOS Simulator + iwdp, tests ALL tools against real Safari
make test-e2e

# Or manually for debugging:
make sim-setup          # Prints IWDP_SIM_WS_URL
export IWDP_SIM_WS_URL=ws://localhost:9222/devtools/page/1
go test -tags=simulator ./e2e/ -v -run TestSim_Navigate
make sim-teardown
```

> **Note:** E2E tests require macOS with Xcode and `ios-webkit-debug-proxy` installed. GitHub Actions `macos-latest` runners have Xcode and iOS Simulator runtimes pre-installed.

### Project Structure

```
iwdp-mcp/
├── cmd/
│   ├── iwdp-mcp/          # MCP server (stdio transport)
│   └── iwdp-cli/          # CLI tool
├── internal/
│   ├── webkit/            # WebKit Inspector Protocol client
│   │   ├── client.go      # WebSocket connection + message routing
│   │   ├── types.go       # Protocol type definitions
│   │   ├── domains.go     # Domain enable/disable helpers
│   │   └── testutil/      # Mock WebSocket server
│   ├── tools/             # Tool implementations (shared by both binaries)
│   └── proxy/             # iwdp process detection + management
├── skills/                # Claude Code skill definition
├── .claude-plugin/        # Claude Code plugin manifest
└── .mcp.json              # MCP server configuration
```

## License

MIT — see [LICENSE](LICENSE).

`ios-webkit-debug-proxy` is a separate project licensed under BSD-3-Clause. This tool connects to it over HTTP/WebSocket at runtime without bundling its code.
