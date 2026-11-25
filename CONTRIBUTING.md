# Contributing to agent-ios

Thanks for your interest in contributing!

## Development Setup

```bash
# Clone and build
git clone https://github.com/edxeth/agent-ios.git
cd agent-ios
make build

# Install ios-webkit-debug-proxy (macOS)
brew install ios-webkit-debug-proxy
```

## Before Submitting a PR

```bash
make fmt    # gofumpt formatting
make lint   # golangci-lint v2.15 or later, built with Go 1.27
make docs   # after a change to a registry command: regenerate docs/commands.md
make test   # unit and e2e tests (go test ./...)
```

An older golangci-lint binary cannot read Go 1.27 export data and reports type errors that are not in the code. `TestReferenceIsCurrent` fails when docs/commands.md is stale.

## Testing

- **Unit tests** (`make test`) — no device needed, uses mock WebSocket server
- **E2E tests** (`go test ./e2e/`, part of `make test`) — builds the CLI binary and tests it against fake listings and pages
- **WebKitGTK tests** (`go test -tags=webkitgtk ./e2e/webkitgtk/`) — local MiniBrowser under broadwayd, no phone
- **Simulator tests** (`make test-e2e`, macOS) — boots iOS Simulator, tests the shared helpers of internal/tools and the proxy listing against real Safari; the lease, debugging one test by hand and the teardown are in [README.md, Simulator tests](README.md#simulator-tests)
- **CDP compatibility** (`make test-cdpcompat`) — the pinned CDP clients against the CDP endpoint (e2e/cdpcompat/README.md)
- **Coverage** (`make coverage-all`) — unit tests plus the WebKitGTK suites
- No test starts, stops or reads an iwdp that it did not start: the other suites use fake listings or a local browser, and the simulator setup refuses when port 9221 is in use, then starts and stops its own iwdp. Never run device tests without the device owner's approval ([docs/device-runbook.md](docs/device-runbook.md)).

## Commit Messages

Use [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add heap snapshot collection
fix: prevent concurrent WebSocket write panic
refactor: extract URL parsing into DevicePort helper
test: add table-driven tests for proxy port parsing
```

## Branch Names

Prefix branches with the change type:

```
feat/heap-snapshots
fix/concurrent-write
refactor/proxy-port-parsing
```

## Architecture

[docs/design/agentic-parity.md](docs/design/agentic-parity.md) is the design; [docs/webkit-protocol.md](docs/webkit-protocol.md) explains the WebKit Inspector Protocol through ios-webkit-debug-proxy.

- `cmd/agent-ios/` — the CLI and daemon binary
- `internal/daemon/` — the per-user daemon, which owns every page connection, and its client
- `internal/core/` — sessions, leases, reservations, the per-page scheduler and desired state
- `internal/commands/` — the command registry (one file per command; `make docs` writes docs/commands.md)
- `internal/webkit/`, `internal/wip/` — the WebKit Inspector Protocol client, and the commands each iOS version accepts
- `internal/tools/` — protocol helpers that the commands, the CDP endpoint and the DevTools extras share
- `internal/runtime/`, `internal/refs/` — the injected agent runtime and element refs
- `internal/devtools/`, `internal/cdp/` — the DevTools extras and the CDP endpoint
- `internal/transport/`, `internal/proxy/`, `internal/sim/` — the iwdp, direct and WebKitGTK transports; iwdp detection, start and restart; the iOS Simulator on a Mac over SSH
- `internal/dialogpolicy/`, `internal/upload/`, `internal/browserurl/` — the dialog override, the host files of an upload, and the URL match of `tab find`
- `internal/logsafe/`, `internal/logcause/` — errors written to the daemon log without page text
- `internal/skills/`, `skill-data/`, `skills/` — the agent skills embedded in the binary, and their discovery stubs
- `e2e/` — end-to-end suites; `scripts/` — setup and helper scripts

A new command is a new file in `internal/commands` whose `init()` calls `Register`. Helpers return structured results; formatting is done by the caller. User docs live in docs/ (see docs/README.md); docs/notes/ is history.

## License

By contributing, you agree that your contributions will be licensed under the MIT License.
