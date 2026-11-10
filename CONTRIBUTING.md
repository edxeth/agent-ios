# Contributing to iwdp-mcp

Thanks for your interest in contributing!

## Development Setup

```bash
# Clone and build
git clone https://github.com/nnemirovsky/iwdp-mcp.git
cd iwdp-mcp
make build

# Install ios-webkit-debug-proxy (macOS)
brew install ios-webkit-debug-proxy
```

## Before Submitting a PR

```bash
make fmt    # gofumpt formatting
make lint   # golangci-lint v2.15 or later, built with Go 1.27
make docs   # after a change to a registry command: regenerate docs/commands.md
make test   # all unit tests
```

An older golangci-lint binary cannot read Go 1.27 export data and reports type errors that are not in the code. `TestReferenceIsCurrent` fails when docs/commands.md is stale.

## Testing

- **Unit tests** (`make test`) — no device needed, uses mock WebSocket server
- **E2E tests** (`go test ./e2e/`, part of `make test`) — builds the CLI binary and tests it against fake listings and pages
- **WebKitGTK tests** (`go test -tags=webkitgtk ./e2e/webkitgtk/`) — local MiniBrowser under broadwayd, no phone
- **Simulator tests** (`make test-e2e`, macOS) — boots iOS Simulator, tests the shared helpers of internal/tools and the proxy listing against real Safari
- **CDP compatibility** (`make test-cdpcompat`) — the pinned CDP clients against the CDP endpoint (e2e/cdpcompat/README.md)
- **Coverage** (`make coverage-all`) — unit tests plus the WebKitGTK suites
- Tests never start, stop or read the real iwdp; never run device tests without the device owner's approval ([docs/device-runbook.md](docs/device-runbook.md)).

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

- `internal/webkit/` — WebKit Inspector Protocol client (WebSocket + Target routing)
- `internal/tools/` — shared WebKit Inspector Protocol helpers (evaluation, screenshots, DOM lookup, cookies)
- `internal/proxy/` — iwdp process detection and management
- `cmd/iwdp-cli/` — CLI binary

- `internal/daemon/`, `internal/core/` — the daemon, sessions, leases, reservations, desired state, scheduler
- `internal/commands/` — the command registry (one file per command; `make docs` writes docs/commands.md)
- `internal/runtime/`, `internal/refs/` — the injected agent runtime and refs
- `internal/devtools/`, `internal/cdp/`, `internal/transport/` — DevTools extras, CDP endpoint, transports

A new command is a new file in `internal/commands` whose `init()` calls `Register`. Helpers return structured results; formatting is done by the caller. User docs live in docs/ (see docs/README.md); docs/notes/ is history.

## License

By contributing, you agree that your contributions will be licensed under the MIT License.
