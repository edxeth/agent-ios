# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Dev Commands

```bash
go build ./...                    # Compile (quick check)
make build                        # Compile to bin/iwdp-cli
make install                      # Install iwdp-cli via go install
make test                         # Run unit + smoke tests (go test ./... -v -count=1)
make test-e2e                     # macOS: boot iOS Sim + iwdp, run the simulator tests (shared helpers, proxy listing)
make test-coverage                # Run tests with coverage → coverage.html
make sim-setup                    # Boot iOS Simulator + iwdp, open a test page (prints IWDP_SIM_WS_URL, IWDP_SIM_PAGE_URL)
make sim-teardown                 # Manual recovery after a crashed run only (one Simulator run per user; never while another run is active): shut down the simulator if setup booted it; stop setup's iwdp only through scripts/iwdp-owner-check.sh --terminate, which verifies it (pid, start time, start token, program) and signals only it, and keep its record until it is gone; otherwise it leaves that iwdp running, says why and sends no signal (docs/device-runbook.md, Stopping an iwdp by hand)
make fmt                          # gofumpt -w . (format all Go files)
make lint                         # golangci-lint run ./... (needs golangci-lint v2.15+ built with Go 1.27)
make docs                         # regenerate docs/commands.md from the command registry
make tidy                         # go mod tidy
make clean                        # Remove bin/ and coverage files
```

## Pre-commit

Before committing, always run `make fmt` and `make lint` and fix any issues. Use golangci-lint v2.15 or later, built with Go 1.27: an older binary cannot read Go 1.27 export data and reports false type errors. After changing a registry command, run `make docs`; `internal/commands/refgen` tests fail on a stale docs/commands.md.

- When a change touches `internal/cdp` or `e2e/cdpcompat`: also run `make test-cdpcompat` (requirements and variables: e2e/cdpcompat/README.md). For their JavaScript files, see `scripts/js-coverage.sh`.

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

- `CSS.enable`, `CSS.getAllStyleSheets`, `CSS.getStyleSheetText` hang through iwdp Target routing. The command is sent but no response comes back, and the hang corrupts the connection pipeline (all subsequent commands on the same connection will also hang). `webkit.Client.Send` refuses these three methods when Target-routed (`ErrHangsThroughTarget`).
- `Page.snapshotRect` requires explicit pixel dimensions — compute them first via `Runtime.evaluate` (see `CaptureScreenshot` in page.go). `coordinateSystem` is `Viewport` (default screenshot) or `Page` (full page); element screenshots use `Page.snapshotNode`.
- WebKit's `Runtime.evaluate` has no `awaitPromise` parameter (iOS 16.7 and current WebKit). `EvaluateScriptWithOptions` evaluates by reference and calls `Runtime.awaitPromise` on a returned Promise. A thrown value comes back as `result` with `wasThrown: true` (no `exceptionDetails`); the stack is read from the thrown object.
- `Page.Cookie.expires` is in milliseconds since the Unix epoch. `sameSite` must be `None`, `Lax` or `Strict`; an empty value is rejected, so `tools.SetCookie` defaults it to `Lax`. A session cookie is sent with `session: true` and no `expires` key: iOS 16.7 WebKit keeps any `expires`, and 0 becomes an expiry at the Unix epoch. `session` is always sent, because iOS 16.7 reads it without a check.

### Deadlines

- `webkit.Client.Send` applies `DefaultSendTimeout` (30 s) when the context has no deadline, and `NewClient` uses a 5 s WebSocket handshake timeout (`DialTimeout`).
- Every `iwdp-cli` command runs under a deadline: 30 s, `--timeout <duration>` or `IWDP_TIMEOUT`. A command's own default (for example 5 s of click and fill) applies without them. A deadline error exits 124 and names the command, page, elapsed time and likely causes; a watchdog ends commands that ignore the context.
- `iwdp-cli <command> --help` (or `-h`) prints usage and exits before any connection. iwdp-cli runs registry commands only: a new command is a new file in `internal/commands` whose init() calls `Register`; the CLI parser, help, overview (usage.go) and docs/commands.md come from it. `TestEveryCommandHasHelp` checks the overview and the help of every registry command.
- Every usage error exits 2 before any connection: an unknown command or `--flag`, a bad global option, a trailing `ws://` argument, and a page command with `IWDP_WS_URL` set and no `--page` (`refusePageURLEnv` in internal/commands/cli.go). Name the page with `--page`.
- Registry arguments (`commands.parseCommand`): only exact, known flags are flags. Another `--name` is a usage error; every other token, also `-1` or `-Math.abs(2)`, is an argument; `--` ends the flags. Never add a heuristic for dash-leading arguments.
- Registry deadlines: `startRegistry` (daemon_cli.go) records the command for `RunCLIStarted` before its ClientStep, so every stage (stdin, daemon start, run) prints the CLI's deadline message. The daemon gets the full remaining deadline; `RunRemote` waits up to `deadlineGrace` (200 ms) past it for the daemon's error. The daemon's `/v1/run` stream sends a `page` item as soon as an operation resolved its page (`core.WithPageObserver`), before attach, lease and queue.
- Daemon reuse: a client reuses a running daemon only with the same `Protocol` and the same executable content (`Hello.Executable`: the file's identity, else its SHA-256; internal/daemon/executable.go); an idle daemon of another binary is replaced, a busy one stays (run `iwdp-cli daemon stop`). A harness that embeds the daemon for a separately built iwdp-cli sets `daemon.RunOptions.Executable` to that binary (e2e/cdpcompat).
- Only **one WebSocket debugger connection per page** — simulator tests use a `sync.Once` shared connection pattern.

### Rules for agents working on a device

- Never stop, restart or pattern-kill an iwdp you did not start. `restart-iwdp` refuses every iwdp whose record iwdp-cli did not write, also one that the removed iwdp-mcp started (record field `writer`, internal/proxy/restart.go; decision L02, docs/device-runbook.md).
- Stop an iwdp by hand only if you started it yourself and nobody else uses it; otherwise send no signal and ask whoever started it, or the user. The stop is `scripts/iwdp-owner-check.sh --terminate <record>`, then `scripts/iwdp-owner-check.sh <record>` until it reports `gone` ([docs/device-runbook.md, Stopping an iwdp by hand](docs/device-runbook.md#stopping-an-iwdp-by-hand)).
- Use a distinct `--session`; reserve physical devices (`device reserve`) and release them; on "lost" stop and ask before `--takeover`.
- The iOS Simulator over SSH is shared by agents in turns: `sim reserve` (refuses the `default` session, waits, exit 3 to retry), work in your own tab, `tab close`, `sim release`; an unused reservation ends after 2 minutes (internal/commands/sim_reserve.go, docs/simulator.md).
- All input is synthetic (`isTrusted=false`); `set viewport`, PDF, downloads and browser contexts are impossible (docs/synthetic-input.md).
- Pause-on-start is on in the daemon but best effort; "runs before page scripts" is partial without it.
- ios_webkit_debug_proxy 1.9.2 subscribes to usbmuxd once; after usbmuxd restarts (Linux: an unplug and replug) it lists no device for good. On Linux the daemon restarts only the iwdp that iwdp-cli recorded, and only when it holds no Unix socket (no usbmuxd connection) while usbmuxd's socket answers (`proxy.Recover`, internal/proxy/recover.go); another iwdp needs its owner.
- iwdp sends error `data` as a JSON array `[{"code":...,"message":...}]` — `ErrorData.Data` is `json.RawMessage` to handle this.

## Architecture

User docs: docs/README.md (canonical). Design: docs/design/agentic-parity.md. docs/notes/ is workstream history. Accepted limits: docs/known-limits.md.

A per-user daemon (`internal/daemon`, started on demand as `<binary> daemon run`) owns every page connection; the CLI and the opt-in CDP endpoint (`internal/cdp`, `get cdp-url`, tokenized URLs) are its clients. `internal/core` holds sessions, leases with epochs, device reservations, the per-page scheduler and the desired state that release cleans up. `internal/commands` is the command registry (one file per command; CLI parser, help and dispatch come from it). `internal/runtime` is the injected agent runtime (snapshots, refs, synthetic actions, waits, dialogs); `internal/devtools` the DevTools extras; `internal/transport` the iwdp, direct (go-ios) and webkitgtk transports.

One binary over the `internal/` package tree:

- `cmd/iwdp-cli/` — CLI and daemon binary: main (registry dispatch), usage (overview), deadline (deadline message, watchdog), daemon_cli (daemon client, process exit and cleanup)

### Package Layout

- `internal/webkit/` — WebKit Inspector Protocol client
  - `client.go` — WebSocket connection with Target-based message routing for iwdp, concurrent-safe writes (`writeSem`), dual-ID request/response routing, event dispatch
  - `types.go` — Protocol type definitions for all domains (DOM, CSS, Network, Runtime, Debugger, etc.)
  - `domains.go` — Domain enable/disable helpers
  - `testutil/mock.go` — Mock WebSocket server for unit tests
- `internal/tools/` — WebKit Inspector Protocol helpers that commands, the CDP endpoint and devtools share: evaluation, screenshots, DOM lookup, cookies
- `internal/sim/` — an iOS Simulator on a Mac over SSH for `sim up`, `sim status`, `sim down` (internal/commands/sim.go, docs/simulator.md): the Mac's simctl calls, the ssh forward of 127.0.0.1:27753 to its webinspectord_sim socket, and the order of the steps
- `internal/proxy/` — iwdp process detection, start and restart
  - `CheckListing(ctx, addr)` — checks the listing port 9221. Never use a device port for detection.
  - `ListDevicesContext(ctx)` — HTTP GET on port 9221
  - `DevicePort(entry)` — extracts port from device entry URL (e.g., `localhost:9224` → 9224)

### Key Patterns

- All WebKit protocol communication goes through a `webkit.Caller` (`webkit.Session` for daemon pages)
- Helpers return structured results; formatting is done by the caller
- Unit tests use mock WebSocket server from `internal/webkit/testutil/` (`NeverRespond(method)` simulates a stalled page; `ReceivedMethods()` and `ConnectionCount()` show what a command sent)
- CLI behavior tests in `cmd/iwdp-cli/cli_w0_test.go` re-run the test binary as `iwdp-cli` (`runCLI`) to check exit codes, output and written files
- E2E tests in `e2e/` run the CLI binary against fake listings and pages (`listingLDFlags` links it with a private listing and no usbmuxd, so it never recovers the fake iwdp for a USB device of the host); `e2e/webkitgtk` (`-tags=webkitgtk`) runs commands against a real WebKitGTK MiniBrowser; the simulator tests (`-tags=simulator`, e2e/simulator_test.go) test the shared helpers of internal/tools, the proxy listing helpers and the iwdp transport against real Safari via iOS Simulator + iwdp, only on the test page that scripts/sim-setup.sh opened (`getSimClient` checks the device and the page's location on every call); e2e/simsetup_script_test.go runs that script's page selection, lease and Simulator teardown against fakes on any host, and e2e/sim_entry_test.go the Makefile targets and the CI setup step
- E2E simulator tests share a single WebSocket connection via `sync.Once` (`getSimClient` in `e2e/sim_helpers_test.go`) — never create multiple connections to the same page
- Every destructive step of a simulator test (data deletion, cookie changes, DOM replacement, any other change of the page or its origin) goes through `simMutate`, which checks the device and the page's location right before it acts; a test that navigates the page restores `IWDP_SIM_PAGE_URL` before it ends
- Destructive targets of the simulator tests (cookie URL and domain, origin) come from the checked `IWDP_SIM_PAGE_URL`, never from page state read earlier; every cookie, database and storage key that they create or delete has a name derived from `IWDP_SIM_RUN_TOKEN` (`simTarget.testName`)
- Error messages should be actionable (tell the user what to do)
- gorilla/websocket is not concurrent-write-safe — the one-token channel `Client.writeSem` protects `conn.WriteMessage`; `Send` stops waiting for it when its context ends
- Almost never use `t.Skipf`/`t.Skip`. Use `t.Fatalf`/`t.Fatal` instead. The env-var check in `getSimClient` is the only legitimate `t.Skip` in the simulator tests.
- Bound a wait on an in-process test fixture (a fake page or session, a test hook) by an event or by the test's own deadline (`t.Deadline()`, as `untilDeadline` in internal/devtools/devtoolstest/page.go does): the suites run under `-race` on shared, loaded hosts. Fixed timeouts stay where the timeout is the behavior under test, and in the external-process start guard `browserStart` that e2e/webkitgtk/harness_w1c6_test.go enforces.
- When many e2e/webkitgtk tests fail with "MiniBrowser did not list the page": check the host's free memory and swap before the code; a memory-starved host starts MiniBrowser too slowly.
- When adding a daemon log line: follow the safe-argument policy and the stated limits of `TestLogCallsWriteOnlySafeKinds` in internal/logsafe/sinks_test.go. docs/guide.md states what the daemon log keeps.

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

The git tag is the only version source. goreleaser names the release and its archives after the tag, and the Go toolchain stamps the tag into the binary's build information (`go version -m`), which `iwdp-cli daemon status` reports (`daemon.BuildVersion`). No file holds a version number. To release:

```bash
git tag vX.Y.Z
git push origin vX.Y.Z
```

## License

MIT — see LICENSE file. ios-webkit-debug-proxy (BSD-3-Clause) is a runtime dependency only; we connect to it over HTTP/WebSocket without bundling its code.
