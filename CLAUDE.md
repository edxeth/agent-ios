# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Dev Commands

```bash
go build ./...                    # Compile (quick check)
make build                        # Compile to bin/iwdp-mcp + bin/iwdp-cli
make install                      # Install both binaries via go install
make test                         # Run unit + smoke tests (go test ./... -v -count=1)
make test-e2e                     # Run e2e tests (boots iOS Sim + iwdp, tests ALL tools)
make test-coverage                # Run tests with coverage → coverage.html
make sim-setup                    # Boot iOS Simulator + iwdp (prints IWDP_SIM_WS_URL)
make sim-teardown                 # Shut down simulator + iwdp
make fmt                          # gofumpt -w . (format all Go files)
make lint                         # golangci-lint run ./...
make tidy                         # go mod tidy
make clean                        # Remove bin/ and coverage files
```

## Pre-commit

Before committing, always run `make fmt` and `make lint` and fix any issues.

## How ios-webkit-debug-proxy Works

- **Port 9221** — listing port: `http://localhost:9221/json` returns connected devices
- **Port 9224+** — each device gets an incremented port starting at 9224
  - `http://localhost:9224/json` → pages for first device
  - `http://localhost:9225/json` → pages for second device, etc.
  - iwdp enumerates a SIMULATOR pseudo-device before real devices, so an unpinned config gives that ghost entry the first range port. Auto-start (`proxy.Start`) pins the first UDID to 9224 to keep the default port on a real device.
- Each page has a `webSocketDebuggerUrl` for WebSocket connection
- The WebSocket speaks **WebKit Inspector Protocol**

### Target-Based Message Routing

iwdp uses **Target routing** — you cannot send domain commands directly on the WebSocket. On connect, iwdp sends a `Target.targetCreated` event with a `targetId`. All subsequent commands must be wrapped in `Target.sendMessageToTarget`:

```
Client → Target.sendMessageToTarget { targetId, message: JSON-stringified inner command }
iwdp → WebKit
WebKit → Target.dispatchMessageFromTarget { targetId, message: JSON-stringified response }
iwdp → Client
```

- The outer `sendMessageToTarget` gets an empty ack `{"result":{},"id":N}` — this is ignored
- The inner command has its own ID used for response matching (dual-ID routing)
- `webkit.Client` handles this transparently: it waits up to 100ms for `Target.targetCreated` on connect; if received, all `Send()` calls auto-wrap in Target routing
- Mock test servers don't send `Target.targetCreated` by default, so the client falls back to direct mode (no wrapping). `MockServer.EnableTargetRouting(id)` makes the mock behave like iwdp for tests of Target routing.

### Domain Enable/Disable Through Target Routing

Some `<Domain>.enable` methods work through iwdp Target routing (e.g., `Debugger.enable`, `Canvas.enable`, `Worker.enable`, `Animation.enable`). Others like `CSS.enable` hang without a response. Most actual domain methods (DOM.getDocument, CSS.getMatchedStylesForNode, CSS.getComputedStyleForNode, CSS.setStyleText, Runtime.evaluate) work without explicit enabling.

### Known Limitations

- `CSS.enable`, `CSS.getAllStyleSheets`, `CSS.getStyleSheetText` hang through iwdp Target routing. The command is sent but no response comes back, and the hang corrupts the connection pipeline (all subsequent commands on the same connection will also hang). The tool implementations detect Target routing and return an error immediately instead, and `webkit.Client.Send` refuses these three methods when Target-routed (`ErrHangsThroughTarget`).
- `Page.snapshotRect` requires explicit pixel dimensions — compute them first via `Runtime.evaluate` (see `CaptureScreenshot` in page.go). `coordinateSystem` is `Viewport` (default screenshot) or `Page` (full page); element screenshots use `Page.snapshotNode`.
- WebKit's `Runtime.evaluate` has no `awaitPromise` parameter (iOS 16.7 and current WebKit). `EvaluateScriptWithOptions` evaluates by reference and calls `Runtime.awaitPromise` on a returned Promise. A thrown value comes back as `result` with `wasThrown: true` (no `exceptionDetails`); the stack is read from the thrown object.
- `Page.Cookie.expires` is in milliseconds since the Unix epoch. `sameSite` must be `None`, `Lax` or `Strict`; an empty value is rejected, so `tools.SetCookie` defaults it to `Lax`. A session cookie is sent with `session: true` and no `expires` key: iOS 16.7 WebKit keeps any `expires`, and 0 becomes an expiry at the Unix epoch. `session` is always sent, because iOS 16.7 reads it without a check.

### Deadlines

- `webkit.Client.Send` applies `DefaultSendTimeout` (30 s) when the context has no deadline, and `NewClient` uses a 5 s WebSocket handshake timeout (`DialTimeout`).
- Every `iwdp-cli` command runs under a deadline: 30 s, `--timeout <duration>` or `IWDP_TIMEOUT`. Recording commands add their `-d` duration; `heap-snapshot` defaults to 5 min; `console`/`network --follow` bound only the connect and enable steps. A deadline error exits 124 and names the command, page, elapsed time and likely causes; a watchdog ends commands that ignore the context.
- `iwdp-cli <command> --help` (or `-h`) prints usage and exits before any connection. Add every new command to both `commands` (main.go) and `helpSections` (usage.go); `TestEveryCommandHasHelp` checks this.
- Only **one WebSocket debugger connection per page** — simulator tests use a `sync.Once` shared connection pattern.
- iwdp sends error `data` as a JSON array `[{"code":...,"message":...}]` — `ErrorData.Data` is `json.RawMessage` to handle this.

## Architecture

Two binaries, one shared `internal/` package tree:

- `cmd/iwdp-mcp/` — MCP server binary (stdio transport, 100+ tools registered via `mcp.AddTool`)
- `cmd/iwdp-cli/` — CLI binary (devices, pages, eval, navigate, screenshot, cookies, dom, console, network)

### Package Layout

- `internal/webkit/` — WebKit Inspector Protocol client
  - `client.go` — WebSocket connection with Target-based message routing for iwdp, concurrent-safe writes (`writeSem`), dual-ID request/response routing, event dispatch
  - `types.go` — Protocol type definitions for all domains (DOM, CSS, Network, Runtime, Debugger, etc.)
  - `domains.go` — Domain enable/disable helpers
  - `testutil/mock.go` — Mock WebSocket server for unit tests
- `internal/tools/` — Tool implementations shared by MCP server and CLI
  - One file per domain: page.go, runtime.go, dom.go, css.go, network.go, storage.go, console.go, debugger.go, timeline.go, memory.go, profiler.go, animation.go, canvas.go, layertree.go, worker.go, audit.go, security.go, interaction.go
- `internal/proxy/` — iwdp process detection and management
  - `IsRunning()` — checks port 9221 (listing port). Never use a device port for detection.
  - `ListDevices()` — HTTP GET on port 9221
  - `ListPages(port)` — HTTP GET on a device-specific port
  - `ListAllPages()` — discovers all devices, queries each for pages
  - `DevicePort(entry)` — extracts port from device entry URL (e.g., `localhost:9224` → 9224)

### Key Patterns

- All WebKit protocol communication goes through `webkit.Client`
- Tools return structured results; formatting is done by the caller (MCP or CLI)
- Unit tests use mock WebSocket server from `internal/webkit/testutil/` (`NeverRespond(method)` simulates a stalled page; `ReceivedMethods()` and `ConnectionCount()` show what a command sent)
- CLI behavior tests in `cmd/iwdp-cli/cli_w0_test.go` re-run the test binary as `iwdp-cli` (`runCLI`) to check exit codes, output and written files
- E2E tests in `e2e/` cover CLI binary smoke tests, MCP server JSON-RPC, and simulator-based tool tests (`-tags=simulator`) that test ALL tools against real Safari via iOS Simulator + iwdp
- Error messages should be actionable (tell the user what to do)
- Network/Console/Timeline use collector patterns: `Start()` registers event handlers, `Get*()` returns collected data
- gorilla/websocket is not concurrent-write-safe — the one-token channel `Client.writeSem` protects `conn.WriteMessage`; `Send` stops waiting for it when its context ends
- E2E simulator tests share a single WebSocket connection via `sync.Once` (`getSimClient` in `e2e/sim_helpers_test.go`) — never create multiple connections to the same page
- Use `simOrigin()` helper to get the page's actual origin for storage tests — never hardcode origins
- Almost never use `t.Skipf`/`t.Skip`. Use `t.Fatalf`/`t.Fatal` instead. The env-var check in `getSimClient` is the only legitimate `t.Skip` in e2e tests.

## Git Conventions

### Commit Messages

Use [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add heap snapshot collection
fix: prevent concurrent WebSocket write panic
refactor: extract URL parsing into DevicePort helper
test: add table-driven tests for proxy port parsing
```

### Branch Names

Prefix branches with the change type:

```
feat/heap-snapshots
fix/concurrent-write
refactor/proxy-port-parsing
```

## Version Bumps

When bumping the version, update all three files:

1. `cmd/iwdp-mcp/main.go` — `Version: "X.Y.Z"` in `mcp.Implementation`
2. `.claude-plugin/plugin.json` — `"version": "X.Y.Z"`
3. `.claude-plugin/marketplace.json` — `"version": "X.Y.Z"`

Then commit, push, and create a git tag:

```bash
git add cmd/iwdp-mcp/main.go .claude-plugin/plugin.json .claude-plugin/marketplace.json
git commit -m "chore: bump version to X.Y.Z"
git push
git tag vX.Y.Z
git push origin vX.Y.Z
```

## Key Dependencies

- `github.com/modelcontextprotocol/go-sdk` v1.4.0 — official MCP Go SDK
- `github.com/gorilla/websocket` — WebSocket client

## License

MIT — see LICENSE file. ios-webkit-debug-proxy (BSD-3-Clause) is a runtime dependency only; we connect to it over HTTP/WebSocket without bundling its code.
