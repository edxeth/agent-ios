# The CDP endpoint

The daemon can serve the Chrome DevTools Protocol (CDP) for the pages it owns, so that CDP clients drive Safari on the iPhone: agent-browser (`--cdp`), Puppeteer, Playwright (`connectOverCDP`) and chrome-devtools-mcp. The endpoint translates CDP to WIP and runtime calls. It is off by default.

## Status of the domains

The endpoint is built in steps. This page describes the endpoint as designed; methods marked "W5b/W5c" are not in this release yet (see [notes/w5.md](notes/w5.md), "Remaining"). Until they land, the clients' page features are limited to what the table marks as available. A later documentation pass updates the table.

| Domain | Status |
|---|---|
| HTTP discovery (`/json/version`, `/json`, `/json/list`, `/json/protocol`, PUT `/json/new`, `/json/close`, `/json/activate`) | available |
| Target (tab and page targets, `setDiscoverTargets`, `setAutoAttach` with filters, `attachToTarget`, `createTarget`, `closeTarget`, ...) | available |
| Browser (`getVersion`, `close` as detach, `setDownloadBehavior` without effect, ...) | available |
| Runtime: `evaluate` (with `awaitPromise` and `returnByValue`), `runIfWaitingForDebugger` | available |
| Network: `setExtraHTTPHeaders` | available |
| Page, Runtime (the other methods and events), DOM, Accessibility, Input, Emulation, Performance, Log | W5b/W5c: see [notes/w5.md](notes/w5.md) |
| Network (events, bodies, cookies), Fetch, Storage, Tracing, Profiler | W5b/W5c: see [notes/w5.md](notes/w5.md) |
| Debugger, HeapProfiler, Overlay, CSS, Security | not planned for this release |

The endpoint never forwards a method that it does not translate. Such a method gets `{"code": -32601, "message": "'<Domain.method>' wasn't found"}`, the same answer as Chrome, so clients that probe optional features continue. Methods that clients call on every attach but that have no effect on iOS answer success. `iwdp-cli cdp log` counts the unknown methods per client session.

## How to connect

1. Pick the session and page as for any command, and get the URL:

   ```bash
   export IWDP_SESSION=pw-run
   iwdp-cli device reserve --owner pw-run        # physical devices only
   iwdp-cli open https://example.com             # the session's page
   iwdp-cli get cdp-url
   # ws://127.0.0.1:9230/t/<token>/e/<endpoint>/devtools/browser/<endpoint>
   ```

   `get cdp-url` starts the endpoint when it is not running. MCP clients use the tool `get_cdp_url` (profile `devtools`).
2. Give the client **the exact URL as printed**. Never build a URL by hand: the token and the endpoint id are part of the path.
3. When the work is done, run `iwdp-cli close` for the session. It releases the lease and revokes the session's CDP URLs; later requests to them get 404.

Other commands:

- `iwdp-cli cdp start [--port 9230] [--allow-origin a,b]` starts the endpoint without issuing a URL. `--port 0` picks a free port. The daemon flag `daemon start --cdp-port <port>` starts it with the daemon.
- `iwdp-cli cdp status` shows the address, the allowed origins and, per endpoint record, its session, page, scope, connections and attachments. It never prints the token.
- `iwdp-cli cdp stop` closes every client connection (each releases its state) and revokes every URL.

### URL forms

- `--form ws` (default): the browser WebSocket URL `ws://127.0.0.1:9230/t/<token>/e/<endpoint>/devtools/browser/<endpoint>`. Every client accepts it. agent-browser needs it: with an `http://` URL it keeps only the host and port and loses the token path.
- `--form http`: the discovery base `http://127.0.0.1:9230/t/<token>/e/<endpoint>`, for clients that append `/json/version` to the base path (Playwright `connectOverCDP` and Puppeteer `browserURL`).
- `--json` prints both forms, the scope and the page.

### Scope, sessions and leases

- A URL belongs to an **endpoint record**: the session that issued it, the selected page (the session's page at that time), the reservation owner and the scope.
- `--scope page` (default): the client sees only that page and the pages it opens itself. `--scope device`: also the other pages of the devices reserved by the reservation owner.
- Every connection through the URL acts for the issuing session: the lease, refs, dialog policy and page state belong to that session, exactly as for a CLI command with the same `--session`. A second connection through the same URL joins the same session. Attaching takes the session's lease or reuses it, so the session's refs stay valid.
- Issuing a URL takes no lease. A page that another session holds is listed but never attached; the attach fails with the lease message.
- A later `tab` switch does not move an issued URL; get a new URL for the new page.
- Detaching a client removes only the state that this client set; the lease stays with the session.

## Security: tokens and other rules

The endpoint gives full control of Safari on the device, so it has these rules:

- It listens on `127.0.0.1` only. The default port is 9230; 9221 to 9229 belong to iwdp, and 9222 is often used by a local Chrome.
- **Token.** The daemon makes a new random 256-bit token at each start of the endpoint. Every HTTP path and WebSocket upgrade must start with `/t/<token>/e/<endpoint>/`; any other request gets 404 and no data. There is no discovery without the token.
- The token reaches clients only through the private Unix socket of the daemon (`get cdp-url`), whose directory must be private (mode 0700). It is never logged; log lines and status output redact the token and endpoint ids, and values that clients choose appear only as short hashes.
- The `Host` header must be `127.0.0.1:<port>` or `localhost:<port>` (a guard against DNS rebinding).
- Requests with an `Origin` header (any web page, also `null`) are refused unless the origin is in `--allow-origin` (empty by default). The supported clients send no `Origin` header.
- `/json/new`, `/json/close` and `/json/activate` accept only PUT.
- Treat a printed URL as a secret: anyone who has it can drive the page until the session ends.

## Client notes

`<cdp-ws-url>` is the default output of `get cdp-url`; `<cdp-http-url>` is the output of `get cdp-url --form http`.

- **agent-browser**: `agent-browser --cdp <cdp-ws-url> open <url>`, `snapshot -i`, `click @e3`. It attaches the page directly and makes no discovery request. Its snapshot uses `Accessibility.getFullAXTree` and `DOM.resolveNode` (W5b), so its refs are its own and are separate from iwdp-cli's refs. It prints "launched browser" also with `--cdp`; it launches nothing.
- **Puppeteer**: `puppeteer.connect({browserWSEndpoint: '<cdp-ws-url>', defaultViewport: null})`, or `browserURL: '<cdp-http-url>'`. Always pass `defaultViewport: null`; otherwise Puppeteer sends `Emulation.setDeviceMetricsOverride`, which fails because the viewport is fixed.
- **chrome-devtools-mcp**: `--wsEndpoint <cdp-ws-url>`. It uses Puppeteer; the same viewport rule applies.
- **Playwright**: `chromium.connectOverCDP('<cdp-ws-url>')` (or `<cdp-http-url>`), then use `browser.contexts()[0]` and its pages. `newContext()` and pages in a new context fail, because iOS has no browser contexts. Locators run through Runtime and DOM; `page.mouse`, `page.keyboard` and `page.touchscreen` go through Input and are synthetic.
- **Worlds**: Playwright's and Puppeteer's utility worlds are emulated as named contexts in the page's main world. They are not isolated from page scripts.
- **Dialogs**: `Page.handleJavaScriptDialog` fails with "dynamic dialog handling is not supported on iOS WebKit; pre-arm answers with iwdp-cli dialog accept|dismiss". The session's dialog policy still answers dialogs, and each automatic answer is reported as a `Log.entryAdded` warning.
- Versions checked against the endpoint design: agent-browser 0.41.2, playwright-core 1.65.0, puppeteer-core 25.16.0, chrome-devtools-mcp 1.12.0. The compatibility suite that pins them (W5c) is not in this release yet.

## Limits

- An endpoint started under `IWDP_NO_DAEMON=1` ends with that CLI process.
- Passthrough of untranslated methods to WIP is not available. `CSS.enable`, `CSS.getAllStyleSheets` and `CSS.getStyleSheetText` are always refused, because they hang through iwdp.
- See also [known-limits.md](known-limits.md).
