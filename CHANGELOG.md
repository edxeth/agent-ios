# Changelog

## [0.1.0] - 2026-11-26

First alpha release of agent-ios. All input is synthetic (`isTrusted=false`); see [known limits](docs/known-limits.md) for what iOS WebKit cannot do.

### New Features

- **Drive iOS Safari from your shell or agent**: control Safari on a real iPhone or iPad through ios-webkit-debug-proxy, with accessibility snapshots and `@eN` refs, clicks, fills, waits, dialogs, tabs and frames. See [the agentic loop](docs/guide.md#the-agentic-loop).
- **iOS Simulator over SSH**: `sim up`, `sim status` and `sim down` drive Safari in an iOS Simulator on a Mac or macOS VM, and `sim reserve` lets several agents take turns on it. See [simulator.md](docs/simulator.md).
- **CDP endpoint**: an opt-in endpoint lets agent-browser, Puppeteer, Playwright and chrome-devtools-mcp drive iOS Safari; `agent-ios get cdp-url` prints its URL. See [cdp.md](docs/cdp.md).
- **DevTools extras**: network log, routes, HAR, console, overrides, storage state, traces, profiles and Web Vitals. See [DevTools extras](docs/guide.md#devtools-extras).

### Added

- Added a per-user background daemon that keeps the page connections, so commands share sessions, leases and refs; `agent-ios daemon stop` ends it.
- Added sessions, page leases and device reservations, so several agents can share a device without taking each other's page. See [sessions.md](docs/sessions.md).
- Added a deadline to every command: 30 s by default, set with `--timeout` or `AGENT_IOS_TIMEOUT`. A command that runs out exits 124 and names the likely cause. See [deadlines and exit codes](docs/guide.md#deadlines-and-exit-codes).
- Added cookie reads that include httpOnly cookies. Cookie values and secret headers stay masked unless you pass `--show-values` or `--show-secrets`.
- Added private output (`--private` or `AGENT_IOS_PRIVATE=1`) for transcripts that others read, plus `tab find` and `--only-tab` for picking a page safely.
- Added built-in agent skills that always match the installed binary: `agent-ios skills get agent-ios` prints the guide, and `skills/agent-ios` is the discovery stub.
- Added an automatic iwdp start and `restart-iwdp`, which restarts only an iwdp that agent-ios started. See [device-runbook.md](docs/device-runbook.md).
- Added `--transport webkitgtk` for local WebKitGTK pages, and an opt-in `--transport direct` that reaches the device through usbmuxd without iwdp. The direct transport has not been checked on a device yet.
- Added prebuilt binaries for Linux and macOS (amd64 and arm64), and `go install github.com/edxeth/agent-ios/cmd/agent-ios@latest`.
