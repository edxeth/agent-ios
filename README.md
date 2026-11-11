# iOS WebKit Debug Proxy CLI

[![CI](https://github.com/nnemirovsky/iwdp-cli/actions/workflows/test.yml/badge.svg)](https://github.com/nnemirovsky/iwdp-cli/actions/workflows/test.yml)
[![Lint](https://github.com/nnemirovsky/iwdp-cli/actions/workflows/lint.yml/badge.svg)](https://github.com/nnemirovsky/iwdp-cli/actions/workflows/lint.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/nnemirovsky/iwdp-cli)](https://goreportcard.com/report/github.com/nnemirovsky/iwdp-cli)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**Debug iOS Safari from your AI agent or your shell.** `iwdp-cli` drives Safari on a real iPhone or iPad. Chrome tooling speaks the Chrome DevTools Protocol (CDP), which WebKit does not; this project bridges that gap.

<video src="https://github.com/user-attachments/assets/dcc0dc05-3a1f-4454-88a8-51ee28da9fd2" poster="https://github.com/user-attachments/assets/3901f7ff-6531-4866-b49e-cf5d17304327" autoplay loop muted playsinline></video>

An agentic loop in the style of agent-browser (snapshots with `@eN` refs, actions, waits, dialogs, tabs, frames), DevTools extras (network log, routes, HAR, console, overrides, storage state, traces, profiles, Web Vitals), and an opt-in CDP endpoint for agent-browser, Puppeteer, Playwright and chrome-devtools-mcp. A background daemon owns every page connection, so CLI commands and CDP clients share sessions, leases and refs.

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
go install github.com/nnemirovsky/iwdp-cli/cmd/iwdp-cli@latest
```

### Pre-built Binaries

Download from [GitHub Releases](https://github.com/nnemirovsky/iwdp-cli/releases).

### Build from Source

```bash
git clone https://github.com/nnemirovsky/iwdp-cli.git
cd iwdp-cli
make build
```

### Agent Skill

The agent skill has two parts, as in agent-browser:

- `skills/iwdp/SKILL.md` is a short discovery stub. It has the skill's name and description, so an agent finds it, and it tells the agent to load the guide with `iwdp-cli skills get iwdp`.
- `skill-data/iwdp/SKILL.md` is the guide: safety rules, the agentic loop, errors and limits. The binary embeds it, and `iwdp-cli skills get iwdp` prints it, so the guide always matches the installed binary.

To install the stub, symlink `skills/iwdp` of a checkout into an Agent Skills directory, for example `ln -s "$PWD/skills/iwdp" ~/.agents/skills/iwdp`, or install the skill with a skills manager from this repository. Install `iwdp-cli` too: the stub holds no guide text of its own.

## Quick Start

### CLI

```bash
# Start the proxy (on Linux add -s 127.0.0.1:27753, see docs/known-limits.md)
ios_webkit_debug_proxy --no-frontend &

# List connected devices (port 9221)
iwdp-cli devices

# Page commands on a physical device need a session and a device reservation
# (docs/sessions.md); the reservation owner defaults to the session name
export IWDP_SESSION=my-run
iwdp-cli device reserve --owner my-run

# List the Safari tabs of the reserved devices (t1, t2, ...)
iwdp-cli tab list

# Evaluate JavaScript in the session's page, or in a tab named with --page
# (a returned Promise is awaited; an exception prints message and stack and exits 1)
iwdp-cli eval "document.title"
iwdp-cli --page t2 eval "fetch('/api/me').then(r => r.status)"

# Take a screenshot: viewport by default, or --full-page, or --selector <css>.
# Without -o the PNG goes to $TMPDIR/iwdp-screenshot-<timestamp>.png.
iwdp-cli screenshot
iwdp-cli screenshot --selector '#checkout' -o checkout.png

# Show all cookies (including httpOnly); values are hidden unless --show-values
iwdp-cli cookies

# Set a cookie (SameSite defaults to Lax) and verify it by reading it back
iwdp-cli cookies set session abc123 --domain example.com --same-site Lax --secure --http-only --expires 2030-01-01T00:00:00Z

# Print the console messages and the network requests since the session leased the page
iwdp-cli console
iwdp-cli network requests

# Help for one command, or the list of a command group (never connects to a page)
iwdp-cli screenshot --help
iwdp-cli network --help

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

Every command runs under a deadline: 30 s by default, set with `--timeout <duration>` (any position) or `IWDP_TIMEOUT`, a duration such as `10s` or whole seconds such as `10`. On expiry the command exits 124 and prints the page, the elapsed time and the likely causes (another inspector session holds the page, the page is stalled or shows a dialog, the device is locked).

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

[docs/commands.md](docs/commands.md) lists every registry command with its arguments and flags. `iwdp-cli --help` lists every command, [docs/migration.md](docs/migration.md) maps the removed commands of earlier versions, and `iwdp-cli <command> --help` prints the usage of one command without connecting to a page. `iwdp-cli <group> --help` lists the commands of a group, for example `network` or `cdp`.

## Development

```bash
make build              # Build iwdp-cli
make test               # Run unit + smoke tests
make test-e2e           # Simulator tests (macOS: boots iOS Simulator, tests the shared helpers against Safari)
make test-coverage      # Tests with coverage report
make lint               # golangci-lint
make fmt                # gofumpt formatting
make docs               # regenerate docs/commands.md from the command registry
```

### Testing

Unit tests use a mock WebSocket server (`internal/webkit/testutil/`) that simulates the WebKit Inspector Protocol. The e2e tests in `e2e/` run the CLI binary against fake listings and pages. The WebKitGTK suite (`e2e/webkitgtk`, build tag `webkitgtk`) runs the commands against a real WebKit browser, and `make test-cdpcompat` runs the pinned CDP clients against the CDP endpoint (e2e/cdpcompat/README.md).

```bash
# Unit + smoke tests (no device needed)
make test

# Simulator tests (macOS) — boots iOS Simulator + iwdp, tests the shared
# WebKit Inspector Protocol helpers and the proxy listing against real Safari
make test-e2e
```

The supported model is one Simulator run per user account at a time: setup takes a lease, and every other run is refused while the lease is held. When setup refuses because an earlier run holds the lease, wait until that run has finished; that run tears itself down. Run `make sim-teardown` only after a crashed run, as the manual recovery: it tears down whatever run holds the lease.

To debug one simulator test by hand, run `make sim-setup`. Only when it succeeds, export the variables that it prints (`IWDP_SIM_WS_URL`, `IWDP_SIM_PAGE_URL` and `IWDP_SIM_RUN_TOKEN`) and run the test, for example `go test -tags=simulator ./e2e/ -v -run TestSim_EvaluateBasic`. When you are done, tear down this run only: `./scripts/sim-setup.sh --teardown-run "$IWDP_SIM_RUN_TOKEN"`. A failed setup prints no variables; do not run the tests with values from an earlier setup.

> **Note:** the simulator tests require macOS with Xcode and `ios-webkit-debug-proxy` installed. GitHub Actions `macos-latest` runners have Xcode and iOS Simulator runtimes pre-installed.
>
> Setup opens a test page of its own (`IWDP_SIM_PAGE_URL`, unique by a random query) in the Simulator, and the tests run only against that page. Teardown (at the end of `make test-e2e`, in CI, or `make sim-teardown` after a crashed run) shuts down the Simulator only when setup's own boot started it; a Simulator that was already running stays booted. Teardown stops the iwdp that setup started only through `scripts/iwdp-owner-check.sh --terminate`, which verifies that process (the recorded pid, start time and start token, and the program ios_webkit_debug_proxy) and sends SIGTERM only to it. It removes the record only with `scripts/iwdp-owner-check.sh --forget`, which reads it again and removes it only when the process that this read names is gone and the file did not change after that read; a changed record that `--forget` keeps under a private name is not checked by the next teardown (the warning names it; see docs/device-runbook.md). Otherwise no signal is sent, or teardown reports an iwdp that still runs after SIGTERM; it gives the check's reason and keeps the record for the next teardown, and the next setup refuses meanwhile. Never stop that iwdp with a command of your own; if it stays running, report it. Stop any other iwdp by hand only if you started it yourself and nobody else uses it; otherwise send no signal and ask whoever started it, or the user. The stop is `scripts/iwdp-owner-check.sh --terminate <record>`, then `scripts/iwdp-owner-check.sh <record>` until it reports `gone`, as [Stopping an iwdp by hand](docs/device-runbook.md#stopping-an-iwdp-by-hand) in docs/device-runbook.md describes.

### Project Structure

```
iwdp-cli/
├── cmd/
│   └── iwdp-cli/          # CLI and daemon binary
├── internal/
│   ├── webkit/            # WebKit Inspector Protocol client
│   │   ├── client.go      # WebSocket connection + message routing
│   │   ├── types.go       # Protocol type definitions
│   │   ├── domains.go     # Domain enable/disable helpers
│   │   └── testutil/      # Mock WebSocket server
│   ├── commands/          # Command registry (one file per command)
│   ├── daemon/            # Per-user daemon that owns the page connections
│   ├── core/              # Sessions, leases, scheduler, desired state
│   ├── cdp/               # Opt-in CDP endpoint
│   ├── tools/             # Shared helpers: evaluation, screenshots, DOM lookup, cookies
│   └── proxy/             # iwdp process detection + management
├── skill-data/            # Agent skill guides, embedded (iwdp-cli skills get iwdp)
└── skills/                # Agent skill discovery stubs (skills/iwdp/SKILL.md)
```

## License

MIT — see [LICENSE](LICENSE).

`ios-webkit-debug-proxy` is a separate project licensed under BSD-3-Clause. This tool connects to it over HTTP/WebSocket at runtime without bundling its code.
