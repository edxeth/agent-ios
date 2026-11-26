# CDP compatibility suite

Design: docs/design/agentic-parity.md, sections 5.1, 5.2 ("Pins"), 5.4, 5.5, 7.8 (tests 8, 10 and 12) and 8.4.

The suite runs the pinned CDP clients against the daemon's CDP endpoint and checks what they do. A Go driver (the `_test.go` files, build tag `cdpcompat`) starts the endpoint, runs the real `agent-ios` to get the URLs, runs one client script per client and form, and checks the JSON result of each script and the requests that the endpoint saw.

Built: the Node project, the pins, the driver, the result and expected-missing formats, test 10 (endpoint resolution; W5c-2a), and the client scenarios on WebKitGTK: test 8 for agent-browser (both builds), Puppeteer, Playwright and chrome-devtools-mcp, test 11 (a) and (d) with Puppeteer, and test 12 with Puppeteer and Playwright (W5c-2b part 2; "Scenarios" below); the features scenarios of agent-browser 0.45.0 (both builds) and chrome-devtools-mcp 1.14.0 (wp-compat, 2026-11-26).

## How to run

```sh
make test-cdpcompat
```

- The target runs `npm ci` in this directory when `package.json` or `package-lock.json` is newer than `node_modules` (with `PUPPETEER_SKIP_DOWNLOAD=1`: no browser download is needed), then runs `AGENT_IOS_CDP_COMPAT=1 go test -tags=cdpcompat ./e2e/cdpcompat/ -v -count=1`.
- Requirements: Node.js 24 or later with npm. The lockfile pins every client, agent-browser included. The scenario tests also need the WebKitGTK MiniBrowser (/usr/lib/webkit2gtk-4.1/MiniBrowser), broadwayd, gcc and pkg-config gdk-3.0 (for the DPI shim of e2e/webkitgtk), as the WebKitGTK suite does. A missing requirement fails the suite with a message that says what to install; the suite never skips.
- npm ci runs with --ignore-scripts: agent-browser's postinstall would download a missing binary and patches global bin links. Since 0.43 the binary comes in a platform package, an optional dependency of agent-browser (`@agent-browser/cli-linux-x64-gnu` on this host), which npm ci installs for the host's platform only; the Makefile sets its execute bit.
- `CDPCOMPAT_LOG_DIR=<dir>` keeps the logs of a run: the daemon log, the output of every client run and every result file. Tokens and endpoint ids in them are replaced with `<secretN>`.
- `CDPCOMPAT_V8_COVERAGE=<absolute dir>` turns on V8 coverage of the client scripts: the driver gives every Node client `NODE_V8_COVERAGE=<dir>`. An inherited `NODE_V8_COVERAGE` never reaches a client (see "Client isolation"). scripts/js-coverage.sh uses it and reports the coverage of clients/playwright.mjs with c8 (see "Pins").

### The gate

The design gates the suite with `AGENT_IOS_CDP_COMPAT=1` (8.4). Here the gate has two parts:
- The build tag `cdpcompat` keeps the suite out of `go test ./...`, which needs no Node.
- `AGENT_IOS_CDP_COMPAT=1` must be set when the tag is given; without it, the test binary stops at once with exit 2 and says how to run the suite. `make test-cdpcompat` sets it.

### Isolation

- The driver runs the daemon inside the test process (`daemon.Run` with `RunOptions.CDPObserve`), on a free port, in a private `XDG_RUNTIME_DIR`. The `agent-ios` binary is built from this checkout and reaches that daemon through the shared socket directory.
- No process of the suite can reach the user's daemon, iwdp or devices (the e2e guard of W1c): private `XDG_RUNTIME_DIR` and `XDG_STATE_HOME`, fake `ios_webkit_debug_proxy` and `pkill` first on `PATH`, and a dead iwdp listing for the in-process daemon (`iwdp.DefaultListingAddr`) and for `agent-ios` (the link-time seam `proxy.listingAddr`).
- Clients run without proxy variables (`HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, `NODE_USE_ENV_PROXY`), with `NO_PROXY=127.0.0.1,localhost`, and without the user's `AGENT_BROWSER_*` settings, so nothing sits between a client and the endpoint.
- agent-browser gets a session name unique to the run (`cdpc-<pid>-<n>`); the driver closes each session at the end. Its sockets live in a short directory below `/tmp` (`AGENT_BROWSER_SOCKET_DIR`), because agent-browser refuses a socket path longer than 103 bytes.

## Pins

| Client | Version | Pinned by |
|---|---|---|
| playwright-core | 1.65.0 | package.json, package-lock.json |
| puppeteer-core | 25.18.1 | package.json, package-lock.json |
| chrome-devtools-mcp | 1.14.0 | package.json, package-lock.json |
| agent-browser | 0.45.0 | package.json, package-lock.json (with its platform packages), and the SHA-256 of the binary (pins_test.go) |

- c8 12.0.0, the coverage reporter of the client scripts, is the only development dependency (package.json `devDependencies`, package-lock.json). No client loads it.
- Every locked package resolves from `https://registry.npmjs.org/` with an integrity hash (`TestNodeProjectPins`).
- chrome-devtools-mcp 1.14.0 bundles its Puppeteer: its package.json has `puppeteer 25.18.1` as a development dependency, and `out/Default/gen/src/third_party/bundled-packages.json` names `puppeteer-core 25.18.1` (1.14.0 moved the build from `build/src` to `out/Default/gen/src`). The package has no runtime dependencies, so the lockfile shows no Puppeteer below it; the driver checks the bundled version on every run.
- agent-browser: the pinned run uses the release binary that npm ci installs from the platform package, `node_modules/@agent-browser/cli-linux-x64-gnu/bin/agent-browser`. It must have this SHA-256 and must print `agent-browser 0.45.0`:
  - linux/amd64: `5eff2baaabb337ec728e4571ccfb228ad1fb6babce529fcaf7a6062fcdec6087`.
  - A build of another commit fails the check even when it reports 0.45.0.
  - The npm launcher `node_modules/agent-browser/bin/agent-browser.js` is never run: it would repoint package-manager bin links.
  - The agent-browser on PATH runs too, without a checksum pin (below, "agent-browser: pinned run and PATH run").

## Result format

Each client script (`clients/<client>.mjs <scenario> <url> <result-file>`) writes one JSON object to the result file:

```json
{
  "client": "puppeteer-core",
  "version": "25.18.1",
  "scenario": "resolve",
  "form": "http",
  "url": "http://127.0.0.1:<port>/t/<token>/e/<endpoint>",
  "ok": true,
  "error": "only when the script itself failed",
  "steps": [{"name": "connect", "ok": true, "ms": 41, "error": "only when the step failed", "value": "only for an observed step"}],
  "sent": [{"method": "Target.getBrowserContexts", "sessionId": ""}]
}
```

- `client` and `version`: the package name and the installed version; the driver checks them against the pins.
- `scenario`: the scenario that ran. `form`: `ws` or `http`, from the URL's scheme.
- `url`: the URL exactly as the script received it on its command line. The driver checks that it equals the output of `get cdp-url` byte for byte.
- `ok`: the scenario's own verdict (every assertion of the scenario passed). `error`: the exception that ended the script, or the script's 60 s limit.
- `steps`: each named step in order, with its outcome, duration and error. An observed step (`observe` in clients/result.mjs) also records the value it saw (any JSON), so the driver can check it.
- `sent`: every CDP command the client sent, in send order, with its CDP session id (`""` for the browser session). The scripts record them with client-side protocol hooks:
  - puppeteer-core: a wrapper around `Connection.prototype._rawSend`, which every browser and session command goes through;
  - playwright-core: the `pw:protocol` debug logger (`DEBUG=pw:protocol`), whose `SEND ►` lines the script reads from its own stderr;
  - chrome-devtools-mcp: its bundled Puppeteer's `puppeteer:protocol:SEND ►` debug log, which the script turns on for the server process only (`NODE_DEBUG=puppeteer:protocol:SEND*`) and reads from the server's `--logFile` after the server ended.
  - agent-browser has no hook: the driver uses the endpoint's unknown-method counts (below) for it.
- The parser (`parseResult`) refuses unknown fields and a result without `client`, `version`, `scenario`, `form` or `url`.

For every client run, the driver also reads `agent-ios cdp log --json` before and after the run; the counts that grew are the run's unknown methods (design 5.4).

## Expected-missing lists

`expected-missing.json` holds one list per client (`agent-browser`, `chrome-devtools-mcp`, `playwright-core`, `puppeteer-core`; an empty list is `[]`):

```json
{
  "playwright-core": [
    {
      "method": "Inspector.enable",
      "feature": "the optional client feature that sends it",
      "review": "who reviewed it, when, and the evidence that the client continues without it"
    }
  ]
}
```

- Each entry names a CDP method (`Domain.method`) once per client, the optional client feature it belongs to, and a review note. The parser refuses an unknown client, a missing list, a malformed method, a duplicate, an empty feature or review, and unknown fields (`TestExpectedMissingFormat`).
- **Rule:** in a scenario test, every unknown method that the endpoint counted during a client's run must be on that client's list; any other unknown method fails the test, and the failure names each method with its count (`expectedMissing.unexpectedUnknown`, `TestUnknownMethodRule`). This keeps a required client operation from failing while the suite stays green.
- The scenario tests (8, 11 and 12) apply the rule to every client run. Review rule for a new entry: name the client code that sends the method (file and lines of the pinned version), show that the client continues without it (the code catches the error, or a scenario passes with the endpoint's "wasn't found" answer), name the feature that is lost, and date and sign the note. A method that a scenario needs gets a translation instead (design 5.4). Test 10 checks endpoint resolution only: it records the unknown methods and the sent commands, and it checks that each unknown method appears in a Node client's own record, but it does not apply the rule.

## Scenarios on WebKitGTK (tests 8, 11 and 12)

Each test starts broadwayd, MiniBrowser (with the DPI shim of e2e/webkitgtk) on fixtures/second.html, a fixture server and the in-process daemon on the webkitgtk transport (`startGTK`, gtk_test.go). Every client gets a fresh browser and its own logical session; the driver issues its URLs with `agent-ios --session <s> get cdp-url` and closes the session afterwards, which releases the lease. Every step is checked by its effect on the page, by the script and, after the run, by `agent-ios --session <s> eval` in the same logical session (an oracle that does not depend on the client).

Fixtures (fixtures/): form.html (a heading, the fields Name and Email, a button "Press me" that becomes "Clicked N" and adds #late 300 ms later, #status that shows every input of Name), second.html (the start page), form-no-checkvisibility.html (form.html with a head script that removes WebKitGTK's native `Element.prototype.checkVisibility`, as Safari 16.6 lacks it; it never removes the endpoint's shim), features.html (the agent-browser features scenario: a button "Modifier probe" whose click writes `click:b<button>:d<detail>:c<ctrl>s<shift>a<alt>m<meta>` into #modlog, a select Size whose option values s, m, x have the labels Small, Medium, m, two rows "Blue mug" and "Red mug" with a button Delete each (names del-blue, del-red) that removes its row, and the same-origin iframe #child titled "Child frame" on frame.html, which has a button #inner that writes "inner clicked"), frames.html (a heading "Frames heading" and three same-origin iframes on frame.html), css.html (a button "Styled" with id target and class box under a `<style>` whose lines 1 to 4 are `:root { --brand: rgb(255, 0, 0); }`, `.box { color: blue; padding: 1px; text-decoration: underline !important; }`, `#target { color: var(--brand); text-decoration: none; }` and `@media (min-width: 1px) { #target { padding: 3px; } }`), console.html with console.js (logFromFixture logs "fixture log 1" at console.js line 3 on load; doubled(n) returns n * 2 at line 6). The fixture server also answers /resource with "from the server" and counts the requests.

| Test | Client | Steps and assertions |
|---|---|---|
| 8 | agent-browser --cdp (pinned and PATH build) | open form.html (the page shows it), snapshot -i (refs of the button and the Name textbox), click @ref (button says Clicked 1), fill @ref Linus (value and the page's input handler), get title (CDP form), screenshot (a PNG of the viewport size); the endpoint saw only upgrades of the printed ws URL |
| features | agent-browser --cdp (pinned and PATH build) | open features.html; click #mod --modifiers meta,shift (the page saw click:b0:d1:c0s1a0m1); select #size m --by label (value x) and --by value (value m), each with the page's change event; snapshot -i (the two Delete buttons have hint="name:del-blue; in:Blue mug" and hint="name:del-red; in:Red mug", the unique "Modifier probe" none, the iframe line is labelled "Child frame"); click on the red Delete's ref (one row left); --json diff snapshot -i (changed, with removed lines of the red row); frame #child, click #inner (the iframe says "inner clicked"), eval document.title (CDP child frame), frame main, eval document.title (CDP features). Every effect is read through the eval oracle |
| 8 | puppeteer-core, ws and http forms | connect, one page, goto (status 200, title), $eval (h1), click (Clicked 1), waitForSelector('#late') with a main-world read of the handle, type (value and #status), screenshot (PNG of innerWidth x innerHeight), accessibility.snapshot (root RootWebArea named by the title, the button "Clicked 1", the textbox Name with Ada) |
| 8 | chrome-devtools-mcp, ws form | the script starts the server with `--wsEndpoint <printed ws URL>` and drives it through MCP stdio (newline-delimited JSON-RPC: initialize, tools/list, tools/call); list_pages (one page, the start page; its id goes with every page tool, because the server routes by page id by default), navigate_page to form.html ("Successfully navigated to"), take_snapshot (RootWebArea "CDP form", heading, the uids of the button and the Name textbox), click by the button's uid with includeSnapshot (the new snapshot has the button "Clicked 1"), list_network_requests (GET form.html [200]), take_screenshot (one image/png of the viewport size), and the server exits 0 when its stdin closes; the eval oracle sees the form with the button "Clicked 1"; every unknown method that the endpoint counted is in the script's record |
| features | chrome-devtools-mcp, ws form | the server runs with `--categoryNetworkMocks`; list_pages; navigate_page to css.html, take_snapshot and get_css_styles on the button "Styled" (the cascade of the fixture's CSS: `#target (<style>:4)` with `color: var(--brand); /* rgb(255, 0, 0) */` and `[overloaded] text-decoration: none;`, `.box (<style>:3)` with `[overloaded] color: blue;`, `[overloaded] padding: 1px;` and `text-decoration: underline !important;`, and in `@media (min-width: 1px)` `#target (<style>:5)` with `padding: 3px;`; lines count inside the `<style>` element, 1-based); navigate_page to frames.html, wait_for "Frames heading" (main document) and wait_for "Inner button" (the frames), both present at the call (Puppeteer runs its aria/ locator in each frame's utility world, so the binding call of a frame must name that frame's context); navigate_page to console.html; list_console_messages with includeStackTraces (the message "fixture log 1" has the frame `at logFromFixture (…/console.js:3:…)`); get_console_message of that msgid (ID, message and the frame); emulate colorScheme dark, geolocation 52.52,13.405 and cpuThrottlingRate 1 ("Emulation configured successfully"; evaluate_script sees prefers-color-scheme: dark and the position); emulate networkConditions Slow 3G (the endpoint's refusal "Network.emulateNetworkConditionsByRule: upload throttling cannot be emulated on iOS WebKit": every preset of the tool has an upload limit, refused before any change on every target; WebKitGTK also lacks WIP Network.setEmulatedConditions, which answers "was not found" there, so no throttle can be emulated on it; a success text alone is never accepted, because Puppeteer answers success when the endpoint says "Not supported"); emulate networkConditions Offline (a fetch of /resource rejects with a TypeError), then emulate without settings (offline ends); mock_network on <origin>/resource with a body file (a fetch gets the file's text), remove_network_mock ("Removed 1 network mock."; a fetch gets "from the server"; the fixture server counted exactly 1 request, so the offline fetch never reached it); set_logpoints at console.js:6 (the answer has [bound], doubled(21) returns 42 and logs "logpoint 21"; any error fails the step); wait_for "Console" (the heading, present from the start), wait_for a text and wait_for an accessible name (a button's aria-label only, which only Puppeteer's aria/ locator finds) that a timer adds 500 ms after the call (each answers "Element matching one of [...] found."); the eval oracle sees console.html |
| 8 | playwright-core, ws and http forms | connectOverCDP (one context, one page), goto, evaluate, getByRole('button').click(), getByLabel('Name').fill (value and #status), locator('#late').textContent() in the utility world (the endpoint announced an isolated context named `__playwright_utility_world_*`), screenshot, ariaSnapshot (heading, button, textbox lines) |
| shims | playwright-core, ws form | MiniBrowser starts on form-no-checkvisibility.html (the CLI eval sees no checkVisibility); connectOverCDP, no navigation: the page's checkVisibility is the endpoint's shim (docs/cdp.md "Page compatibility shims"), getByRole('button').click(), getByLabel('Name').fill, ariaSnapshot (heading, button, textbox, #late lines); the eval oracle sees the form state and, after the client's detach, the shim still in the document (`TestPlaywrightLocatorsWithoutNativeCheckVisibility`). Without the shim the click and fill fail with the device's `TypeError: element.checkVisibility is not a function` |
| 12 | puppeteer-core | setRequestInterception(true) with a handler that answers /resource with respond({body: 'ok'}); page.evaluate(() => fetch('/resource').then(r => r.text())) returns ok within 2 s of a 10 s deadline; the fixture server got no /resource request |
| 12 | playwright-core | the same with page.route('**/resource', r => r.fulfill({body: 'ok'})). The design writes '/resource'; Playwright resolves a path glob only against a baseURL, which connectOverCDP's default context does not have, so '/resource' never matches. |
| 11 (a), (d) | puppeteer-core | s1 opens the form and takes a snapshot through the CLI; Puppeteer connects with s1's URL and evaluates; at the checkpoint "attached" the driver checks s1's lease epoch, clicks with the snapshot's ref through the CLI (the script then sees "Clicked 1") and checks that "s2 get title" exits 4 with the lease message; Puppeteer's connection through s2's URL lists the page and Target.attachToTarget fails with the lease message; s1's connection still evaluates and its epoch is unchanged (checkpoint "refused") |

Checkpoints: a script that needs the driver in the middle of a run writes `@checkpoint <name>` on stdout and waits for "ok" on stdin (clients/result.mjs `checkpoint`; the driver's `runInteractive`).

## Test 10: endpoint resolution

`TestEndpointResolution` runs against a fake WebKit page (no browser). One `get cdp-url` record serves every run; each client gets the printed ws line or the `http` member of `--json` unchanged. The recorded requests of each run must be exactly:

| Run | Requests (raw request target, status) |
|---|---|
| agent-browser, ws form | upgrade `/t/<token>/e/<endpoint>/devtools/browser/<endpoint>` 101, twice: 0.45.0 probes an attach target (a WebSocket with Browser.getVersion) before it attaches (cli/src/native/actions.rs lines 6870-6880, browser.rs lines 1068-1099) |
| agent-browser, http form | `/json/version` 404, `/json/list` 404, upgrade `/devtools/browser` 404, upgrade `/session` 404 (0.45.0's Firefox WebDriver BiDi probe, cli/src/native/bidi/attach.rs lines 129-134) |
| playwright-core, ws form | upgrade `/t/<token>/e/<endpoint>/devtools/browser/<endpoint>` 101 |
| playwright-core, http form | `/t/<token>/e/<endpoint>/json/version/` 200, then the advertised upgrade 101 |
| puppeteer-core, ws form | upgrade `/t/<token>/e/<endpoint>/devtools/browser/<endpoint>` 101 |
| puppeteer-core, http form | `/t/<token>/e/<endpoint>/json/version` 200, then the advertised upgrade 101 |

Every request has the Host `127.0.0.1:<port>` of the printed URL. `/json/version` advertises exactly the ws form. Discovery paths and upgrades without the token, without the endpoint id, or with wrong ones get 404 without discovery data, and no recorded request outside `/t/<token>/e/<endpoint>/` got anything but 404.

## agent-browser: pinned run and PATH run

The user chose: upstream pinned, and the build on PATH also run.
- **Pinned:** node_modules/@agent-browser/cli-linux-x64-gnu/bin/agent-browser from agent-browser@0.45.0 and its platform package @agent-browser/cli-linux-x64-gnu@0.45.0 (lockfile), SHA-256 5eff2baaabb337ec728e4571ccfb228ad1fb6babce529fcaf7a6062fcdec6087, version 0.45.0.
- **PATH:** the agent-browser that PATH names, which the user's agents run. The build is recorded, not pinned. See "agent-browser on PATH" for how the driver chooses the binary.
- Both builds run the same scenarios (test 10: ws and http forms), and both must pass. agent-browser-builds.json in the log directory records each build: version, SHA-256 and mtime, and for the PATH build the source checkout's HEAD and git describe (a build can be older than its checkout). For a script on PATH it also records the script and the mapping.
- The PATH runs are skipped only when the binary that would run has the pinned SHA-256 (the record says so), or when PATH has no agent-browser (recorded; the pinned build runs once).

### agent-browser on PATH

- **A native binary** (an ELF file, or a symlink to one): the driver runs it directly.
- **A script** (a wrapper such as `~/.pi/agent/bin/agent-browser`, or the npm launcher `bin/agent-browser.js`): the driver never runs it and does not guess which build it runs. Set `AGENT_IOS_CDPCOMPAT_PATH_AGENT_BROWSER` to the absolute path of the native binary that the script runs:

  ```sh
  AGENT_IOS_CDPCOMPAT_PATH_AGENT_BROWSER=$HOME/projects/agent-browser/cli/target/release/agent-browser make test-cdpcompat
  ```

  Without it, the suite fails at once with the script's path and this setting. With it, the scenarios run the named binary with the driver's home, working directory and `--config`, like the pinned binary.
- **Why the mapping is explicit:** a script can run one build for `--help` and another for real commands, and a wrapper can find its build through the user's `HOME`. Watching what the script runs for one command does not show what it runs for the scenarios, and running it would expose the user's configuration. The user knows which binary the script runs; the record shows it.
- The setting is an error when PATH names a native binary or no agent-browser, so a stale value is never ignored silently.
- Every external command of the build description (`--version`, `git rev-parse`, `git describe`) has a 10 s deadline. A timeout kills only that command's process group; its output is bounded, and its process is reaped. After the command exits, the driver also kills what is left of its group.

### Deadlines of client runs

Every client run (a Node script, an agent-browser command and its session close), every `agent-ios` run and the build of `agent-ios` run in their own process group under a deadline (90 s for a client run, 30 s for the close and for `agent-ios`, 5 min for the build). At the deadline the driver kills the whole group, so a child that holds the output open (for example a session daemon) cannot block the run; the pipes close at most 2 s later, and the process is reaped. The run fails with the timeout, and its log keeps the output so far (at most 8 MiB per run, with a note of what was dropped). A client run that ends by itself keeps its group: agent-browser's session must live until its close, which the driver runs after each agent-browser run, also after a timeout (`TestScenarioTimeoutEndsItsChild`, `TestScenarioTimeoutStillClosesSession`).

## Client isolation

Every client runs with a HOME, XDG base and working directory that the driver owns, and without proxy, AGENT_BROWSER_*, CHROME_DEVTOOLS_MCP_*, DEBUG and DEBUG_* variables. The driver sets chrome-devtools-mcp's CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS, CHROME_DEVTOOLS_MCP_NO_UPDATE_CHECKS and CHROME_DEVTOOLS_MCP_NO_CONFIG_DISCOVERY (no statistics, no update check, no configuration file). agent-browser gets an explicit --config with the driver's {} file, which replaces its config discovery. The driver sets Playwright's DEBUG and DEBUG_COLORS. Clients also get no variable that changes how Node or npm load code: no NODE_* variable (NODE_OPTIONS among them), no OPENSSL_CONF, OPENSSL_CONF_INCLUDE, OPENSSL_MODULES or OPENSSL_ENGINES, and no npm_config_* variable. A test injects a fault only through the driver's explicit preload option (execNode). TestClientsIgnoreUserSettings checks the isolation with a hostile user environment, a NODE_OPTIONS preload among it. TestNodeRunFailureIsReported checks that a failed script fails the run. TestPathAgentBrowserResolution, TestPathScriptRunsMappedBinary and TestMetadataCommandsAreBounded check the PATH build choice and the deadlines. TestFormatProperties checks the format contract with testing/quick.
