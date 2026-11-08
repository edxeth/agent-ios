# CDP compatibility suite

Design: docs/design/agentic-parity.md, sections 5.1, 5.2 ("Pins"), 5.4, 5.5, 7.8 (tests 8, 10 and 12) and 8.4.

The suite runs the pinned CDP clients against the daemon's CDP endpoint and checks what they do. A Go driver (the `_test.go` files, build tag `cdpcompat`) starts the endpoint, runs the real `iwdp-cli` to get the URLs, runs one client script per client and form, and checks the JSON result of each script and the requests that the endpoint saw.

Built so far (W5c-2a): the Node project, the pins, the driver, the result and expected-missing formats, and test 10 (endpoint resolution). The client scenarios (tests 8 and 12) come in the second part of W5c-2, after W5c-1a.

## How to run

```sh
make test-cdpcompat
```

- The target runs `npm ci` in this directory when `package.json` or `package-lock.json` is newer than `node_modules` (with `PUPPETEER_SKIP_DOWNLOAD=1`: no browser download is needed), then runs `IWDP_CDP_COMPAT=1 go test -tags=cdpcompat ./e2e/cdpcompat/ -v -count=1`.
- Requirements: Node.js 24 or later with npm. The lockfile pins every client, agent-browser included. A missing requirement fails the suite with a message that says what to install; the suite never skips.
- npm ci runs with --ignore-scripts: agent-browser's postinstall would download a missing binary and patches global bin links. The package ships its binaries; the Makefile sets their execute bit.
- `CDPCOMPAT_LOG_DIR=<dir>` keeps the logs of a run: the daemon log, the output of every client run and every result file. Tokens and endpoint ids in them are replaced with `<secretN>`.

### The gate

The design gates the suite with `IWDP_CDP_COMPAT=1` (8.4). Here the gate has two parts:
- The build tag `cdpcompat` keeps the suite out of `go test ./...`, which needs no Node.
- `IWDP_CDP_COMPAT=1` must be set when the tag is given; without it, the test binary stops at once with exit 2 and says how to run the suite. `make test-cdpcompat` sets it.

### Isolation

- The driver runs the daemon inside the test process (`daemon.Run` with `RunOptions.CDPObserve`), on a free port, in a private `XDG_RUNTIME_DIR`. The `iwdp-cli` binary is built from this checkout and reaches that daemon through the shared socket directory.
- No process of the suite can reach the user's daemon, iwdp or devices (the e2e guard of W1c): private `XDG_RUNTIME_DIR` and `XDG_STATE_HOME`, fake `ios_webkit_debug_proxy` and `pkill` first on `PATH`, and a dead iwdp listing for the in-process daemon (`iwdp.DefaultListingAddr`) and for `iwdp-cli` (the link-time seam `proxy.listingAddr`).
- Clients run without proxy variables (`HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, `NODE_USE_ENV_PROXY`), with `NO_PROXY=127.0.0.1,localhost`, and without the user's `AGENT_BROWSER_*` settings, so nothing sits between a client and the endpoint.
- agent-browser gets a session name unique to the run (`cdpc-<pid>-<n>`); the driver closes each session at the end. Its sockets live in a short directory below `/tmp` (`AGENT_BROWSER_SOCKET_DIR`), because agent-browser refuses a socket path longer than 103 bytes.

## Pins

| Client | Version | Pinned by |
|---|---|---|
| playwright-core | 1.65.0 | package.json, package-lock.json |
| puppeteer-core | 25.16.0 | package.json, package-lock.json |
| chrome-devtools-mcp | 1.12.0 | package.json, package-lock.json |
| agent-browser | 0.41.2 | package.json, package-lock.json, and the SHA-256 of the binary (pins_test.go) |

- Every locked package resolves from `https://registry.npmjs.org/` with an integrity hash (`TestNodeProjectPins`).
- chrome-devtools-mcp 1.12.0 bundles its Puppeteer: its package.json has `puppeteer 25.16.0` as a development dependency, and `build/src/third_party/bundled-packages.json` names `puppeteer-core 25.16.0`. The package has no runtime dependencies, so the lockfile shows no Puppeteer below it; the driver checks the bundled version on every run.
- agent-browser: the pinned run uses the release binary that npm ci installs, `node_modules/agent-browser/bin/agent-browser-linux-x64`. It must have this SHA-256 and must print `agent-browser 0.41.2`:
  - linux/amd64: `c4351f3802ffa5254d8c69f20a52e2fcf56925c6016fcf4203db25a07d811379`.
  - A build of another commit fails the check even when it reports 0.41.2.
  - The agent-browser on PATH runs too, without a checksum pin (below, "agent-browser: pinned run and PATH run").

## Result format

Each client script (`clients/<client>.mjs <scenario> <url> <result-file>`) writes one JSON object to the result file:

```json
{
  "client": "puppeteer-core",
  "version": "25.16.0",
  "scenario": "resolve",
  "form": "http",
  "url": "http://127.0.0.1:<port>/t/<token>/e/<endpoint>",
  "ok": true,
  "error": "only when the script itself failed",
  "steps": [{"name": "connect", "ok": true, "ms": 41, "error": "only when the step failed"}],
  "sent": [{"method": "Target.getBrowserContexts", "sessionId": ""}]
}
```

- `client` and `version`: the package name and the installed version; the driver checks them against the pins.
- `scenario`: the scenario that ran. `form`: `ws` or `http`, from the URL's scheme.
- `url`: the URL exactly as the script received it on its command line. The driver checks that it equals the output of `get cdp-url` byte for byte.
- `ok`: the scenario's own verdict (every assertion of the scenario passed). `error`: the exception that ended the script, or the script's 60 s limit.
- `steps`: each named step in order, with its outcome, duration and error.
- `sent`: every CDP command the client sent, in send order, with its CDP session id (`""` for the browser session). The scripts record them with client-side protocol hooks:
  - puppeteer-core: a wrapper around `Connection.prototype._rawSend`, which every browser and session command goes through;
  - playwright-core: the `pw:protocol` debug logger (`DEBUG=pw:protocol`), whose `SEND ►` lines the script reads from its own stderr.
  - agent-browser has no hook: the driver uses the endpoint's unknown-method counts (below) for it.
- The parser (`parseResult`) refuses unknown fields and a result without `client`, `version`, `scenario`, `form` or `url`.

For every client run, the driver also reads `iwdp-cli cdp log --json` before and after the run; the counts that grew are the run's unknown methods (design 5.4).

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
- The lists are empty until the scenarios of tests 8 and 12 run (the second part of W5c-2). Test 10 checks endpoint resolution only: it records the unknown methods and the sent commands, and it checks that each unknown method appears in a Node client's own record, but it does not apply the rule.

## Test 10: endpoint resolution

`TestEndpointResolution` runs against a fake WebKit page (no browser). One `get cdp-url` record serves every run; each client gets the printed ws line or the `http` member of `--json` unchanged. The recorded requests of each run must be exactly:

| Run | Requests (raw request target, status) |
|---|---|
| agent-browser, ws form | upgrade `/t/<token>/e/<endpoint>/devtools/browser/<endpoint>` 101 |
| agent-browser, http form | `/json/version` 404, `/json/list` 404, upgrade `/devtools/browser` 404 |
| playwright-core, ws form | upgrade `/t/<token>/e/<endpoint>/devtools/browser/<endpoint>` 101 |
| playwright-core, http form | `/t/<token>/e/<endpoint>/json/version/` 200, then the advertised upgrade 101 |
| puppeteer-core, ws form | upgrade `/t/<token>/e/<endpoint>/devtools/browser/<endpoint>` 101 |
| puppeteer-core, http form | `/t/<token>/e/<endpoint>/json/version` 200, then the advertised upgrade 101 |

Every request has the Host `127.0.0.1:<port>` of the printed URL. `/json/version` advertises exactly the ws form. Discovery paths and upgrades without the token, without the endpoint id, or with wrong ones get 404 without discovery data, and no recorded request outside `/t/<token>/e/<endpoint>/` got anything but 404.

## agent-browser: pinned run and PATH run

The user chose: upstream pinned, and the build on PATH also run.
- **Pinned:** node_modules/agent-browser/bin/agent-browser-linux-x64 from agent-browser@0.41.2 (lockfile), SHA-256 c4351f3802ffa5254d8c69f20a52e2fcf56925c6016fcf4203db25a07d811379, version 0.41.2.
- **PATH:** the agent-browser that PATH names, which the user's agents run. The build is recorded, not pinned. See "agent-browser on PATH" for how the driver chooses the binary.
- Both builds run the same scenarios (test 10: ws and http forms), and both must pass. agent-browser-builds.json in the log directory records each build: version, SHA-256 and mtime, and for the PATH build the source checkout's HEAD and git describe (a build can be older than its checkout). For a script on PATH it also records the script and the mapping.
- The PATH runs are skipped only when the binary that would run has the pinned SHA-256 (the record says so), or when PATH has no agent-browser (recorded; the pinned build runs once).

### agent-browser on PATH

- **A native binary** (an ELF file, or a symlink to one): the driver runs it directly.
- **A script** (a wrapper such as `~/.pi/agent/bin/agent-browser`, or the npm launcher `bin/agent-browser.js`): the driver never runs it and does not guess which build it runs. Set `IWDP_CDPCOMPAT_PATH_AGENT_BROWSER` to the absolute path of the native binary that the script runs:

  ```sh
  IWDP_CDPCOMPAT_PATH_AGENT_BROWSER=$HOME/projects/agent-browser/cli/target/release/agent-browser make test-cdpcompat
  ```

  Without it, the suite fails at once with the script's path and this setting. With it, the scenarios run the named binary with the driver's home, working directory and `--config`, like the pinned binary.
- **Why the mapping is explicit:** a script can run one build for `--help` and another for real commands, and a wrapper can find its build through the user's `HOME`. Watching what the script runs for one command does not show what it runs for the scenarios, and running it would expose the user's configuration. The user knows which binary the script runs; the record shows it.
- The setting is an error when PATH names a native binary or no agent-browser, so a stale value is never ignored silently.
- Every external command of the build description (`--version`, `git rev-parse`, `git describe`) has a 10 s deadline. A timeout kills only that command's process group; its output is bounded, and its process is reaped. After the command exits, the driver also kills what is left of its group.

### Deadlines of client runs

Every client run (a Node script, an agent-browser command and its session close), every `iwdp-cli` run and the build of `iwdp-cli` run in their own process group under a deadline (90 s for a client run, 30 s for the close and for `iwdp-cli`, 5 min for the build). At the deadline the driver kills the whole group, so a child that holds the output open (for example a session daemon) cannot block the run; the pipes close at most 2 s later, and the process is reaped. The run fails with the timeout, and its log keeps the output so far (at most 8 MiB per run, with a note of what was dropped). A client run that ends by itself keeps its group: agent-browser's session must live until its close, which the driver runs after each agent-browser run, also after a timeout (`TestScenarioTimeoutEndsItsChild`, `TestScenarioTimeoutStillClosesSession`).

## Client isolation

Every client runs with a HOME, XDG base and working directory that the driver owns, and without proxy, AGENT_BROWSER_*, DEBUG and DEBUG_* variables. agent-browser gets an explicit --config with the driver's {} file, which replaces its config discovery. The driver sets Playwright's DEBUG and DEBUG_COLORS. Clients also get no variable that changes how Node or npm load code: no NODE_* variable (NODE_OPTIONS among them), no OPENSSL_CONF, OPENSSL_CONF_INCLUDE, OPENSSL_MODULES or OPENSSL_ENGINES, and no npm_config_* variable. A test injects a fault only through the driver's explicit preload option (execNode). TestClientsIgnoreUserSettings checks the isolation with a hostile user environment, a NODE_OPTIONS preload among it. TestNodeRunFailureIsReported checks that a failed script fails the run. TestPathAgentBrowserResolution, TestPathScriptRunsMappedBinary and TestMetadataCommandsAreBounded check the PATH build choice and the deadlines. TestFormatProperties checks the format contract with testing/quick.
