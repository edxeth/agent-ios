# AGENTS.md

agent-ios is a Go CLI and per-user daemon that drives Safari on iPhone, iPad and the iOS Simulator, and local WebKitGTK pages, through ios-webkit-debug-proxy (iwdp) and the WebKit Inspector Protocol.

- The daemon (`internal/daemon`, started on demand as `<binary> daemon run`) owns every page connection; the CLI and the opt-in CDP endpoint (`internal/cdp`) are its clients. A page accepts only one WebSocket debugger connection.
- All WebKit protocol communication goes through a `webkit.Caller` (`webkit.Session` for daemon pages). docs/webkit-protocol.md explains iwdp's ports and Target routing and lists the methods that hang or differ from CDP; read it before you change code that sends protocol commands.
- agent-ios runs registry commands only: a new command is a new file in `internal/commands` whose init() calls `Register`; the CLI parser, help, overview (`cmd/agent-ios/usage.go`) and docs/commands.md come from it. `TestEveryCommandHasHelp` checks the overview and the help of every registry command.
- In registry arguments (`commands.parseCommand`) only exact, known flags are flags, and `--` ends the flags. Never add a heuristic for dash-leading arguments.
- A client reuses a running daemon only when it runs the same executable (internal/daemon/executable.go): after a rebuild, an idle daemon of the old binary is replaced, but a busy one stays until `agent-ios daemon stop`.
- docs/README.md indexes the user docs, which are canonical; docs/guide.md states the deadlines and exit codes of every command. docs/design/agentic-parity.md is the design, docs/known-limits.md lists the accepted limits, and docs/notes/ is history. CONTRIBUTING.md gives the commit message and branch name conventions.
- ios-webkit-debug-proxy (BSD-3-Clause) is a runtime dependency only: connect to it over HTTP and WebSocket, and never bundle its code.

## Commands

```bash
go build ./...                    # Compile (quick check)
make build                        # Compile to bin/agent-ios
make install                      # Install agent-ios via go install
make test                         # Run unit + smoke tests (go test ./... -v -count=1)
make test-e2e                     # macOS: boot iOS Sim + iwdp, run the simulator tests (shared helpers, proxy listing)
make test-coverage                # Run tests with coverage → coverage.html
make sim-setup                    # Boot iOS Simulator + iwdp, open a test page (prints AGENT_IOS_SIM_WS_URL, AGENT_IOS_SIM_PAGE_URL)
make sim-teardown                 # Manual recovery after a crashed run only (one Simulator run per user; never while another run is active): shut down the simulator if setup booted it; stop setup's iwdp only through scripts/iwdp-owner-check.sh --terminate, which verifies it (pid, start time, start token, program) and signals only it, and keep its record until it is gone; otherwise it leaves that iwdp running, says why and sends no signal (docs/device-runbook.md, Stopping an iwdp by hand)
make fmt                          # gofumpt -w . (format all Go files)
make lint                         # golangci-lint run ./... (needs golangci-lint v2.15+ built with Go 1.27)
make docs                         # regenerate docs/commands.md from the command registry
make tidy                         # go mod tidy
make clean                        # Remove bin/ and coverage files
```

## Before committing

- Run `make fmt` and `make lint` and fix any issues. An older golangci-lint binary cannot read Go 1.27 export data and reports false type errors.
- After changing a registry command, run `make docs`; `TestReferenceIsCurrent` in `internal/commands/refgen` fails on a stale docs/commands.md.
- After a change in `internal/cdp` or `e2e/cdpcompat`, also run `make test-cdpcompat` (requirements and variables: e2e/cdpcompat/README.md). For their JavaScript files, see `scripts/js-coverage.sh`.

## Tests

- Unit tests use the mock WebSocket server of `internal/webkit/testutil/`: `NeverRespond(method)` simulates a stalled page; `ReceivedMethods()` and `ConnectionCount()` show what a command sent.
- CLI behavior tests in `cmd/agent-ios/cli_w0_test.go` re-run the test binary as `agent-ios` (`runCLI`) to check exit codes, output and written files.
- Use `t.Fatal`/`t.Fatalf`, almost never `t.Skip`/`t.Skipf`.
- Bound a wait on an in-process test fixture (a fake page or session, a test hook) by an event or by the test's own deadline (`t.Deadline()`, as `untilDeadline` in internal/devtools/devtoolstest/page.go does): the suites run under `-race` on shared, loaded hosts. Fixed timeouts stay where the timeout is the behavior under test, and in the external-process start guard `browserStart` that e2e/webkitgtk/harness_w1c6_test.go enforces.
- A new daemon log line follows the safe-argument policy and the stated limits of `TestLogCallsWriteOnlySafeKinds` in internal/logsafe/sinks_test.go. docs/guide.md states what the daemon log keeps.

## Work on a device

- Never stop, restart or pattern-kill an iwdp you did not start. `restart-iwdp` refuses every iwdp whose record agent-ios did not write, also one that the removed iwdp-mcp started (record field `writer`, internal/proxy/restart.go; decision L02, docs/device-runbook.md).
- Stop an iwdp by hand only if you started it yourself and nobody else uses it; otherwise send no signal and ask whoever started it, or the user. The stop is `scripts/iwdp-owner-check.sh --terminate <record>`, then `scripts/iwdp-owner-check.sh <record>` until it reports `gone` ([docs/device-runbook.md, Stopping an iwdp by hand](docs/device-runbook.md#stopping-an-iwdp-by-hand)).
- Use a distinct `--session`; reserve physical devices (`device reserve`) and release them; on "lost" stop and ask before `--takeover`.
- The iOS Simulator over SSH is shared by agents in turns: `sim reserve` (refuses the `default` session and the guides' `qa-login`, waits, exit 3 to retry), work in your own tab (`sim up --url` prints its id for `tab <id>`; else `tab find --device SIMULATOR`), `tab close <id>` (when it fails, still release; without an id and a selected tab it refuses in your turn), `sim release`; an unused reservation ends after 2 minutes (internal/commands/sim_reserve.go, docs/simulator.md).
- All input is synthetic (`isTrusted=false`); `set viewport`, PDF, downloads and browser contexts are impossible (docs/synthetic-input.md).

## Releases

- The git tag is the only version source; no file holds a version number. goreleaser names the release and its archives after the tag, and the Go toolchain stamps the tag into the binary, which `agent-ios daemon status` reports (`daemon.BuildVersion`).
- Pushing a `v*` tag (`git tag vX.Y.Z`, `git push origin vX.Y.Z`) publishes a release through .github/workflows/release.yml.
