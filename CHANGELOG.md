# Changelog

## [Unreleased]

Parity with agent-browser 0.45.0 and chrome-devtools-mcp 1.14.0 wherever iOS WebKit allows; see [known limits](docs/known-limits.md#agent-browser-045-and-chrome-devtools-mcp-114-features) for the rest.

### Features

- Added `heap snapshot [path]` (a JavaScriptCore heap snapshot written as is, with a summary by class; the path is checked before the snapshot, and the snapshot is held once in memory), `heap gc` and `heap diff <a> <b>` (class deltas computed on the host; regular files up to 512 MiB). See [DevTools extras](docs/guide.md#devtools-extras).
- Added `css matched <target>` and `css computed <target>`: the matched CSS rules with the cascade, in chrome-devtools-mcp's `get_css_styles` format, and the computed style from the CSS domain, without `CSS.enable`. Node ids are pinned to their target and document, logical and physical properties conflict in the cascade, and var() is read as CSS tokens; `--page-size 0` is refused.
- Added `logpoint add|list|remove|clear`: logpoints that never pause, on script URLs or on original sources through source maps (inline and file maps), kept across reloads, navigations and reconnects. Debugger is enabled only after the pauses on `debugger` statements and exceptions are off, on every path; source maps have budgets (16 MiB and 2 million segments per map, 32 maps or 4 million segments per page); a location that two loaded scripts map equally fails as ambiguous.
- Added `diff url <url1> <url2> [--screenshot [--full]]`, and `diff snapshot --baseline <file>`.
- Added `state list`, `state show`, `state rename` and `state clear`, and named states under `$XDG_STATE_HOME/agent-ios/states` (directory 0700, files 0600). `state rename` and `state clear` take names only (`state clear login`, not `login.json`), `state list` prints names, `state show` prints "Origins with localStorage: M" (agent-browser's label and count), and `state save <name>` writes atomically and refuses a symlink at the state file.
- Added `click` and `dblclick` `--button left|right|middle|back|forward` and `--modifiers alt,control,meta,shift`; every event carries them, and `--new-tab` refuses both. `mouse down|up` take back and forward. `keydown` of a modifier key holds it for later `press`, `keydown`, `keyup` and `mouse` commands; key events carry `code`, `keyCode` and `location`.
- Added `select --by value|label`, and `select` on ARIA comboboxes and listboxes; a refused combobox select closes the list it opened. A native `<select>` is selected and read back in one step, so a change handler that navigates or removes it does not fail the command, and options that share a value count once. `fill` sets date, time, color and range inputs. `upload` expands `~`.
- Added snapshot hints for controls with an empty or shared name, escaped exactly as Rust's `{:?}` (as agent-browser prints them); `snapshot -s` covers every match; the cursor scan covers `<summary>`, native controls with non-control roles and shadow-root clickables; iframes are labelled by title, else name, and nested same-origin iframes are walked four levels deep.
- `eval` runs in the frame that `frame` selected, also a cross-origin one, and `eval --json` reports the document's URL as `origin` (read only when printed, within 1 s). `frame` resolves unnamed, cross-origin and nested iframes and refs of iframes inside same-origin frames. `console` and `errors` capture iframe messages.
- `screenshot --annotate --json` gives each label a `box` in the coordinates of the captured image (viewport, full page or `--selector` element); refs inside iframes are boxed through scrolled, offset and scaled frames, and `--selector` with a ref inside a frame captures the right area.
- Added `tab keep live|throttled|frozen`, which always fails on iOS WebKit with the reason.

### Behavior changes

- **BREAKING:** `state save`, `state load` and `state show` take a state name (letters, digits, `_` and `-`) or a path: `state save login` now writes the named state `login`, not the file `./login`. Write `./login` or `login.json` for a file. See [migration.md](docs/migration.md#changes-after-010).
- `keydown` follows agent-browser 0.45 (#2734): keydown, then keypress and the insertion or default action unless the page cancels it. `press <key> --phase down` keeps the bare keydown. `press` chords send the modifier key events, apply the US Shift mapping and insert no text with Alt, Control or Meta; modifier parts may stand anywhere in a chord and take the Left/Right forms (`press a+Shift`, `press ShiftRight+a`), and every keydown and keyup goes to the element that has focus when it is sent (after `Shift+Tab`, the newly focused element gets the keyups). 0.1.0's undocumented `Option+` alias is gone: `press Option+a` is a usage error (unknown key); use `Alt+a`.
- `diff snapshot` compares with the session's last snapshot of the page (with its options) or with `--baseline`, ignores refs, and fails with `no_baseline`, `baseline_mismatch` or `file_not_found`; it used to compare the last two snapshots.
- fill, type, check, uncheck, select and upload refuse an impossible target at once and start the error with agent-browser 0.45's stable code (`read_only`, `disabled`, `not_editable`, `option_not_found`, ...). fill and select warn when the page changes the value. A refusal changes nothing, except that a refused combobox `select` closes the list it opened first, and `type` refused mid-way (the page disabled the field) says how many characters were typed before the refusal. The option list of `option_not_found` is bounded, then "... and N more".
- A ref from a replaced document fails with agent-browser 0.45's message "Ref @eN is from a document that is no longer loaded ..." (code `stale_ref` unchanged).
- `click --count 2` and `dblclick` end with dblclick only for the left button.
- `network har stop` without a recording fails with `no_har_recording` and writes no file.
- `set geo` follows "last set wins" also when it replaces the session's earlier override: repeated after a CDP client's geolocation override, it runs after that override in new documents, and the release of another owner brings it back.

### Fixes

- An iwdp that agent-ios starts (`restart-iwdp`, `devices`, the daemon) is no longer stopped after 5 s while it connects a USB device: ios_webkit_debug_proxy listens at once and answers only after the device connection, which took about 4.5 s with an iPhone X and longer right after a restart. A started iwdp that listens gets up to 20 s from its start to answer; when it has not answered by then, or the command ends first, it is kept running and recorded ("kept running and recorded"), with no start back-off. A start that did not answer no longer blames the device for the "Unable to connect to ..." lines that iwdp prints when the start's SIGTERM ends it.
- `click --new-tab` makes the new tab the session's current page, as `tab new` does and as agent-browser 0.45's `click --new-tab` makes it the active tab; the session's lease on the opener ends. On iOS Safari the next command used to run on the opener, which Safari hides behind the new tab, and timed out (exit 124, "waiting for the element to be stable"). The result names the new tab (`--json`: `tab`), and says so when `window.open` returned no window.
- The stable check of click, dblclick, tap, hover, check and drag no longer waits for animation frames that never come: on a page that runs none (a background Safari tab) it samples the element's box 50 ms apart.
- `snapshot`, actions, `press`, `screenshot --annotate` and the CDP endpoint's DOM tree no longer log "Blocked a frame with origin ... from accessing a frame with origin ..." to the page's console on pages with cross-origin iframes: the runtime checks whether it may reach a frame's window before it reads `contentDocument` or `frameElement`.
- click, dblclick, tap, hover, check and drag scroll a clipped or edge-straddling target fully into view and wait for the layout to settle before the receives-events check, so a cover that appears on scroll fails the action as `covered by`, and the events carry the settled point; a press that lands on a label that activates the target is accepted unless it lands on a control inside that label (agent-browser 0.45's interactive list, #2587, #2611). Targets inside nested same-origin frames are hit-tested through every frame, and a ref whose element was replaced is found again only in the document that held it.
- drag between two refs of one same-origin frame delivers dragover and drop to the target; a drag across documents is refused with `unsupported`.
- press, keydown, keyup, keyboard type/inserttext and type without a target reach the element focused inside same-origin frames; focus in a cross-origin frame is refused with `unsupported`. Tab moves within the focused element's document, and inserttext into a contenteditable inside a frame edits it. `is focused` follows the focus into same-origin frames.
- click refuses a label control covered by a `contenteditable="plaintext-only"` editing host.
- screenshot --annotate cuts frame refs at their frame's viewport and refuses `--selector` targets that a frame cuts partly.
- heap diff: the class comparison, sort and result now run under the command deadline; an expired deadline exits 124 instead of printing a late result.
- select: option_not_found lists option values and labels cut on Unicode code points (100 and "…"), so an option with emoji is no longer listed with U+FFFD and without the ellipsis.
- On the iOS Simulator (about 40 ms per round trip), `select` on a native select whose change handler navigates and `click` on a link to a fast page no longer fail with "the page may have run step 1 ... before the reply was lost ... Missing injected script" although the action ran: the reply is armed in the page before the call is sent (20 of 20 runs each on iOS 26.5). The arm's round trip takes nothing from the attempt window, and `click --new-tab` checks its window right before the gesture call.
- A pending `Runtime.awaitPromise` (an armed action reply, a CDP awaited evaluation) no longer holds one of the page's 8 send slots, so a `wait` next to other waits, and chrome-devtools-mcp `wait_for` with several texts or frames, no longer block the page's other commands.
- `emulate network` reports WebKit's "was not found" answer as `unsupported` (`this WebKit (iOS version unknown) has no Network.setEmulatedConditions; iOS 18.0 to 27.2 lack it`), without WebKit's raw error text. iwdp lists the iOS Simulator as iOS 0.0.0, which now counts as an unknown version everywhere (command lists, throttle parameters). A refused throttle is not kept, and a refusal after a throttle with an unknown outcome (also through the CDP endpoint) keeps the outcome unknown, so an identical retry is sent.
- `emulate network` on iOS 27.3 and later sends `bandwidth` (the form that version takes) instead of `bytesPerSecondLimit`.
- The WebKitGTK test suite builds the CLI outside the daemon's socket directory, so its daemon-based tests run again.

### CDP endpoint

- chrome-devtools-mcp 1.14's `emulate` works: `Emulation.setCPUThrottlingRate` (rate 1), `Emulation.clearGeolocationOverride`, `Network.overrideNetworkState` and `Network.emulateNetworkConditionsByRule` (URL rules only when every URL gets the same conditions); offline through interception; iOS 27.3 throttling with bandwidth and latency. Its network presets all carry an upload limit, which WIP cannot emulate, so they are refused as a whole; `Offline` works. Between geolocation overrides the last set wins, also a replaced one; a `clearGeolocationOverride` that WebKit refuses keeps the override in its place in that order, so it does not win over a later one.
- A Debugger subset for logpoints (chrome-devtools-mcp `set_logpoints`): `enable`, `disable`, logpoint-only `setBreakpointByUrl`, `removeBreakpoint`, `getScriptSource`, `getPossibleBreakpoints`, `scriptParsed` and `breakpointResolved`; anything that could pause is refused. Breakpoints belong to their attachment: `breakpointResolved` goes only to it with the id it got, also after a replay, and removing another attachment's breakpoint is refused.
- Nodes of same-origin iframes work in the DOM methods with main-frame coordinates, and mouse and touch input reaches elements inside same-origin iframes.
- `DOM.requestNode` works before `DOM.getDocument`, also on WebKit's empty refusal.
- Console stack traces carry async parents (`stackTrace.parent`).
- CSS for chrome-devtools-mcp's `get_css_styles`: `CSS.enable` and `CSS.disable` (answered by the endpoint), `CSS.getMatchedStylesForNode`, `getInlineStylesForNode`, `getComputedStyleForNode` and `DOM.pushNodesByBackendIdsToFrontend` for sessions that send `CSS.enable`. A shorthand's `longhandProperties` are given only when its declaration block has no other shorthand.
- chrome-devtools-mcp's `wait_for` works: it timed out on every text, also on text that was on the page. An awaited `Runtime.evaluate` or `callFunctionOn` no longer holds the page's later calls while its promise is pending (Puppeteer's locator pollers need them to settle), and `Runtime.bindingCalled` names the context that the binding was added for (`executionContextName` or `executionContextId`), so Puppeteer's `aria/` selectors answer. Checked on WebKitGTK and the iOS 26.5 Simulator.
- Bindings work in same-origin child frames: `Runtime.addBinding` installs the page function in each child frame's document, `Runtime.bindingCalled` names the calling frame's context (before, a frame call failed in the page with "undefined is not an object (evaluating 'o.args')"), and `DOM.resolveNode{backendNodeId, executionContextId}` of a child frame's context returns the node in that frame's world. chrome-devtools-mcp's `wait_for` finds text in the main document and in the frames of a page with same-origin iframes, on WebKitGTK and on the iOS 26.5 Simulator.
- `Runtime.callFunctionOn` in an execution context takes one WIP round trip instead of three (about 43 ms instead of 127 ms per call through iwdp on the iOS 26.5 Simulator), so chrome-devtools-mcp's `wait_for` on a page with iframes finishes within its 5 s locator passes.
- `Runtime.callFunctionOn` with a declaration that is not a function answers Chrome's error -32000 "Given expression does not evaluate to a function" on every path, also in an execution context; `Runtime.addBinding` no longer fails when one child frame does not answer.
- HeapProfiler and the other CSS methods are still not translated. See [cdp.md](docs/cdp.md#limits-per-domain).

### Docs

- The guide, known limits, CDP limits, design and migration pages describe the new commands, their deadlines and exit codes, and the limits of iOS WebKit against agent-browser 0.45 and chrome-devtools-mcp 1.14. The skill guides name the new commands.
- Internal: the injected runtime is split into a core and on-demand modules (forms, gestures, overlay); the first form, gesture or annotate command in a document costs one extra install message. An install of the core with modules that does not fit one 64 KiB message is split (the core first, then the modules joined while they fit).

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
