# Changelog

## [Unreleased]

Parity with agent-browser 0.45.0 and chrome-devtools-mcp 1.14.0 wherever iOS WebKit allows; see [known limits](docs/known-limits.md#agent-browser-045-and-chrome-devtools-mcp-114-features) for the rest.

### Features

- Added `heap snapshot [path]` (a JavaScriptCore heap snapshot written as is, with a summary by class), `heap gc` and `heap diff <a> <b>` (class deltas computed on the host). See [DevTools extras](docs/guide.md#devtools-extras).
- Added `css matched <target>` and `css computed <target>`: the matched CSS rules with the cascade, in chrome-devtools-mcp's `get_css_styles` format, and the computed style from the CSS domain, without `CSS.enable`.
- Added `logpoint add|list|remove|clear`: logpoints that never pause, on script URLs or on original sources through source maps (inline and file maps), kept across reloads, navigations and reconnects.
- Added `diff url <url1> <url2> [--screenshot [--full]]`, and `diff snapshot --baseline <file>`.
- Added `state list`, `state show`, `state rename` and `state clear`, and named states under `$XDG_STATE_HOME/agent-ios/states` (directory 0700, files 0600).
- Added `click` and `dblclick` `--button left|right|middle|back|forward` and `--modifiers alt,control,meta,shift`; every event carries them, and `--new-tab` refuses both. `mouse down|up` take back and forward. `keydown` of a modifier key holds it for later `press`, `keydown`, `keyup` and `mouse` commands; key events carry `code`, `keyCode` and `location`.
- Added `select --by value|label`, and `select` on ARIA comboboxes and listboxes. `fill` sets date, time, color and range inputs. `upload` expands `~`.
- Added snapshot hints for controls with an empty or shared name; `snapshot -s` covers every match; the cursor scan covers `<summary>`, native controls with non-control roles and shadow-root clickables; iframes are labelled by title, else name, and nested same-origin iframes are walked four levels deep.
- `eval` runs in the frame that `frame` selected, also a cross-origin one, and `eval --json` reports the document's URL as `origin`. `frame` resolves unnamed, cross-origin and nested iframes and refs of iframes inside same-origin frames. `console` and `errors` capture iframe messages.
- `screenshot --annotate --json` gives each label a `box`; refs inside iframes are boxed through scrolled, offset and scaled frames.
- Added `tab keep live|throttled|frozen`, which always fails on iOS WebKit with the reason.

### Behavior changes

- **BREAKING:** `state save`, `state load` and `state show` take a state name (letters, digits, `_` and `-`) or a path: `state save login` now writes the named state `login`, not the file `./login`. Write `./login` or `login.json` for a file. See [migration.md](docs/migration.md#changes-after-010).
- `keydown` follows agent-browser 0.45 (#2734): keydown, then keypress and the insertion or default action unless the page cancels it. `press <key> --phase down` keeps the bare keydown. `press` chords send the modifier key events, apply the US Shift mapping and insert no text with Alt, Control or Meta.
- `diff snapshot` compares with the session's last snapshot of the page (with its options) or with `--baseline`, ignores refs, and fails with `no_baseline`, `baseline_mismatch` or `file_not_found`; it used to compare the last two snapshots.
- fill, type, check, uncheck, select and upload refuse an impossible target at once, change nothing and start the error with agent-browser 0.45's stable code (`read_only`, `disabled`, `not_editable`, `option_not_found`, ...). fill and select warn when the page changes the value.
- A ref from a replaced document fails with agent-browser 0.45's message "Ref @eN is from a document that is no longer loaded ..." (code `stale_ref` unchanged).
- `click --count 2` and `dblclick` end with dblclick only for the left button.
- `network har stop` without a recording fails with `no_har_recording` and writes no file.

### Fixes

- click, dblclick, tap, hover, check and drag scroll a clipped or edge-straddling target fully into view and wait for the layout to settle before the receives-events check, so a cover that appears on scroll fails the action as `covered by`, and the events carry the settled point; a press that lands on a label that activates the target is accepted (agent-browser 0.45 #2587, #2611).
- The WebKitGTK test suite builds the CLI outside the daemon's socket directory, so its daemon-based tests run again.

### CDP endpoint

- chrome-devtools-mcp 1.14's `emulate` works: `Emulation.setCPUThrottlingRate` (rate 1), `Emulation.clearGeolocationOverride`, `Network.overrideNetworkState` and `Network.emulateNetworkConditionsByRule`; offline through interception; iOS 27.3 throttling with bandwidth and latency.
- A Debugger subset for logpoints (chrome-devtools-mcp `set_logpoints`): `enable`, `disable`, logpoint-only `setBreakpointByUrl`, `removeBreakpoint`, `getScriptSource`, `getPossibleBreakpoints`, `scriptParsed` and `breakpointResolved`; anything that could pause is refused.
- Nodes of same-origin iframes work in the DOM methods with main-frame coordinates, and mouse and touch input reaches elements inside same-origin iframes.
- `DOM.requestNode` works before `DOM.getDocument`, also on WebKit's empty refusal.
- Console stack traces carry async parents (`stackTrace.parent`).
- HeapProfiler and CSS are still not translated. See [cdp.md](docs/cdp.md#limits-per-domain).

### Docs

- The guide, known limits, CDP limits, design and migration pages describe the new commands, their deadlines and exit codes, and the limits of iOS WebKit against agent-browser 0.45 and chrome-devtools-mcp 1.14. The skill guides name the new commands.
- Internal: the injected runtime is split into a core and on-demand modules (forms, gestures, overlay); the first form, gesture or annotate command in a document costs one extra install message.

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
