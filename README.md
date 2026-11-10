# iOS WebKit Debug Proxy CLI

[![CI](https://github.com/nnemirovsky/iwdp-mcp/actions/workflows/test.yml/badge.svg)](https://github.com/nnemirovsky/iwdp-mcp/actions/workflows/test.yml)
[![Lint](https://github.com/nnemirovsky/iwdp-mcp/actions/workflows/lint.yml/badge.svg)](https://github.com/nnemirovsky/iwdp-mcp/actions/workflows/lint.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/nnemirovsky/iwdp-mcp)](https://goreportcard.com/report/github.com/nnemirovsky/iwdp-mcp)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**Debug iOS Safari from your AI agent or your shell.** `iwdp-cli` drives Safari on a real iPhone or iPad. Chrome tooling speaks the Chrome DevTools Protocol (CDP), which WebKit does not; this project bridges that gap.

<video src="https://github.com/user-attachments/assets/dcc0dc05-3a1f-4454-88a8-51ee28da9fd2" poster="https://github.com/user-attachments/assets/3901f7ff-6531-4866-b49e-cf5d17304327" autoplay loop muted playsinline></video>

An agentic loop in the style of agent-browser (snapshots with `@eN` refs, actions, waits, dialogs, tabs, frames), DevTools extras (network log, routes, HAR, console, overrides, storage state, traces, profiles, Web Vitals), an opt-in CDP endpoint for agent-browser, Puppeteer, Playwright and chrome-devtools-mcp, and the legacy commands across the WebKit Inspector Protocol domains. A background daemon owns every page connection, so CLI commands and CDP clients share sessions, leases and refs.

Documentation: [docs/README.md](docs/README.md) (user guide, sessions and leases, synthetic input, CDP, device runbook, migration, known limits) and the generated [command reference](docs/commands.md).

## Why?

- Agent tooling exists for desktop Chrome. **Nothing existed for iOS Safari** — until now.
- **httpOnly cookies** are invisible to `document.cookie`. This tool reads them via the protocol.
- **Real device testing** catches iOS-specific bugs that simulators miss.
- Works from any agent or script that can run a command; an agent reads the built-in skill with `iwdp-cli skills get iwdp`.

## Installation

### Prerequisites

```bash
# Install ios-webkit-debug-proxy (macOS)
brew install ios-webkit-debug-proxy

# Connect an iOS device via USB and enable Web Inspector:
# Settings → Safari → Advanced → Web Inspector → ON
```

For Linux and other platforms, see the [ios-webkit-debug-proxy installation guide](https://github.com/google/ios-webkit-debug-proxy#installation).

### Go Install

```bash
go install github.com/nnemirovsky/iwdp-mcp/cmd/iwdp-cli@latest
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
# Start the proxy (on Linux add -s 127.0.0.1:27753, see docs/known-limits.md)
ios_webkit_debug_proxy --no-frontend &

# List connected devices (port 9221)
iwdp-cli devices

# List Safari tabs on the first device (port 9224)
iwdp-cli pages

# Page commands on a physical device need a session and a device reservation
# (docs/sessions.md); the reservation owner defaults to the session name
export IWDP_SESSION=my-run
iwdp-cli device reserve --owner my-run

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

# End the session and release the device
iwdp-cli close && iwdp-cli device release --owner my-run
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

All input is synthetic (`isTrusted=false`); see [docs/synthetic-input.md](docs/synthetic-input.md). Sessions, leases and reservations: [docs/sessions.md](docs/sessions.md). A CDP URL for other clients: `iwdp-cli get cdp-url` ([docs/cdp.md](docs/cdp.md)).

Every command runs under a deadline: 30 s by default, set with `--timeout <duration>` (any position) or `IWDP_TIMEOUT`. Recording commands add their `-d` duration; `heap-snapshot` defaults to 5 min. On expiry the command exits 124 and prints the page, the elapsed time and the likely causes (another inspector session holds the page, the page is stalled or shows a dialog, the device is locked).

## How It Works

```
┌────────────┐    USB     ┌──────────┐   HTTP/WS    ┌──────────┐
│ iOS Device │◄──────────►│   iwdp   │◄────────────►│ iwdp-cli │
│  (Safari)  │            │ :9221-N  │              │          │
└────────────┘            └──────────┘              └──────────┘
```

`ios-webkit-debug-proxy` exposes:
- **Port 9221** — lists all connected devices
- **Port 9222+** — each device gets an incremented port listing its Safari tabs
- Each tab provides a **WebSocket URL** for the WebKit Inspector Protocol

`iwdp-cli` starts a per-user daemon (`iwdp-cli daemon run`) that connects to those WebSocket endpoints, keeps the page connections, and serves the CLI and the optional CDP endpoint over a private Unix socket. `--transport direct` reaches the device through usbmuxd without iwdp (opt-in), and `--transport webkitgtk` drives a local WebKitGTK MiniBrowser for tests. See [docs/guide.md](docs/guide.md).

## Commands

[docs/commands.md](docs/commands.md) lists every registry command with its arguments and flags. `iwdp-cli --help` lists every command, including the legacy commands of the WebKit Inspector Protocol domains (DOM, CSS, network, storage, debugger, timeline, heap and CPU profiling), and `iwdp-cli <command> --help` prints the usage of one command without connecting to a page.

## Development

```bash
make build              # Build iwdp-cli
make test               # Run unit + smoke tests
make test-e2e           # E2E tests (boots iOS Simulator, tests all commands)
make test-coverage      # Tests with coverage report
make lint               # golangci-lint
make fmt                # gofumpt formatting
make docs               # regenerate docs/commands.md from the command registry
```

### Testing

Unit tests use a mock WebSocket server (`internal/webkit/testutil/`) that simulates the WebKit Inspector Protocol. E2E tests boot an iOS Simulator and run the commands against real Safari.

```bash
# Unit + smoke tests (no device needed)
make test

# E2E tests — boots iOS Simulator + iwdp, tests the commands against real Safari
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
│   └── iwdp-cli/          # CLI and daemon binary
├── internal/
│   ├── webkit/            # WebKit Inspector Protocol client
│   │   ├── client.go      # WebSocket connection + message routing
│   │   ├── types.go       # Protocol type definitions
│   │   ├── domains.go     # Domain enable/disable helpers
│   │   └── testutil/      # Mock WebSocket server
│   ├── tools/             # WebKit Inspector Protocol helpers of the legacy commands
│   └── proxy/             # iwdp process detection + management
└── skills/                # Agent skill (iwdp-cli skills get iwdp)
```

## License

MIT — see [LICENSE](LICENSE).

`ios-webkit-debug-proxy` is a separate project licensed under BSD-3-Clause. This tool connects to it over HTTP/WebSocket at runtime without bundling its code.
