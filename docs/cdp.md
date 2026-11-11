# The CDP endpoint

The daemon can serve the Chrome DevTools Protocol (CDP) for the pages it owns, so that CDP clients drive Safari on the iPhone: agent-browser (`--cdp`), Puppeteer, Playwright (`connectOverCDP`) and chrome-devtools-mcp. The endpoint translates CDP to WIP and runtime calls. It is off by default.

## Status

This release serves every domain and method in the table below. The method table in the code (internal/cdp, one `register` call per domain file) is the complete list: `/json/protocol` is generated from it, and nothing outside it reaches WebKit.

- The four pinned clients pass their scenarios on WebKitGTK (see "Compatibility suite").
- Device check 3 (iPhone X, iOS 16.7, Safari 16.6, iwdp 1.9.2) passed agent-browser (pinned and PATH builds), Puppeteer, chrome-devtools-mcp and the awaited intercepted fetch of Puppeteer and Playwright. Playwright's goto, evaluate, screenshot and `page.route` passed; its locator actions failed with `TypeError: element.checkVisibility is not a function`, which the endpoint now fixes with a page shim (see "Page compatibility shims"). Device check 4 (the same iPhone, code a8aec70) ran Playwright 1.65.0 with the shim: all 15 steps of its scenario passed (connectOverCDP, goto, evaluate, a `getByRole` click, a `getByLabel` fill, a locator read, a screenshot, `ariaSnapshot`, and an awaited fetch that `page.route` fulfilled), with no `checkVisibility` error. The remaining open device questions are in [known-limits.md](known-limits.md#open-device-questions).

A method that the endpoint does not translate is never forwarded. It gets `{"code": -32601, "message": "'<Domain.method>' wasn't found"}`, the same answer as Chrome, so clients that probe optional features continue. `iwdp-cli cdp log` counts the unknown methods per client session.

## Method table

Status words:

- **done**: translated to WIP or to runtime calls.
- **no effect**: answers success and changes nothing. Clients send these methods on every attach.
- **error**: always fails with the reason in the message.
- **partial**: works for some parameters and fails for the others; see "Limits per domain".

Sessions: "browser" is the browser session of a `/devtools/browser/...` connection, "tab" a tab target's session, "page" a page target's session or a `/devtools/page/<targetId>` connection. A method sent on another kind of session gets the "wasn't found" answer.

| Domain | Methods | Status | Sessions | Events |
|---|---|---|---|---|
| HTTP discovery | `/json/version`, `/json`, `/json/list`, `/json/protocol`; PUT `/json/new`, `/json/close/<id>`, `/json/activate/<id>` | done | | |
| Target | `getTargets`, `getTargetInfo`, `setAutoAttach` (flat sessions, Chrome's default filter), `attachToTarget`, `detachFromTarget`, `activateTarget` (a liveness probe), `closeTarget` (`window.close()`) | done | browser, tab, page | `targetCreated`, `targetDestroyed`, `targetInfoChanged`, `attachedToTarget`, `detachedFromTarget` |
| Target | `setDiscoverTargets`, `createTarget` (`window.open` with an emulated user gesture), `getBrowserContexts` (the default context only) | done | browser | |
| Browser | `getVersion` (product `Safari/<iOS version>`), `close` (detaches only; Safari stays open) | done | browser | |
| Browser | `getWindowForTarget` (a synthetic window) | done | browser, tab, page | |
| Browser | `setDownloadBehavior` | no effect | browser | |
| Browser | `grantPermissions` | error | browser | |
| Runtime | `enable`, `disable`, `evaluate`, `callFunctionOn`, `getProperties`, `releaseObject`, `releaseObjectGroup`, `addBinding`, `removeBinding` | done | page | `executionContextCreated`, `executionContextsCleared`, `executionContextDestroyed`, `consoleAPICalled`, `exceptionThrown`, `bindingCalled` |
| Runtime | `runIfWaitingForDebugger` | no effect | tab, page | |
| Page | `enable`, `disable`, `getFrameTree`, `getResourceTree`, `navigate`, `reload`, `stopLoading`, `getNavigationHistory`, `navigateToHistoryEntry`, `captureScreenshot`, `getLayoutMetrics`, `addScriptToEvaluateOnNewDocument`, `removeScriptToEvaluateOnNewDocument`, `createIsolatedWorld`, `setLifecycleEventsEnabled`, `bringToFront` (a liveness probe) | done | page | `frameNavigated`, `navigatedWithinDocument`, `domContentEventFired`, `loadEventFired`, `lifecycleEvent`, `frameAttached`, `frameDetached`, `frameStartedLoading`, `frameStoppedLoading` |
| Page | `handleJavaScriptDialog`, `setBypassCSP` | error | page | |
| DOM | `getDocument`, `querySelector`, `querySelectorAll`, `describeNode`, `resolveNode`, `requestNode`, `getBoxModel`, `getContentQuads`, `scrollIntoViewIfNeeded`, `getOuterHTML`, `focus`, `setAttributeValue`, `removeNode`, `getFrameOwner`, `getNodeForLocation`, `setFileInputFiles` | done | page | none |
| DOM | `enable`, `disable` | no effect | page | |
| Accessibility | `getFullAXTree`, `getPartialAXTree`, `queryAXTree` | done | page | |
| Accessibility | `enable`, `disable` | no effect | page | |
| Input | `dispatchMouseEvent`, `dispatchKeyEvent`, `dispatchTouchEvent`, `emulateTouchFromMouseEvent`, `insertText`, `synthesizeTapGesture` (all synthetic, `isTrusted=false`) | done | page | |
| Emulation | `setEmulatedMedia`, `setUserAgentOverride`, `setGeolocationOverride` | done | page | |
| Emulation | `setDeviceMetricsOverride` | partial | page | |
| Emulation | `setFocusEmulationEnabled`, `setTouchEmulationEnabled` | no effect | page | |
| Emulation | `clearDeviceMetricsOverride` | error | page | |
| Network | `enable`, `disable`, `getResponseBody`, `getRequestPostData`, `setExtraHTTPHeaders`, `setCacheDisabled`, `setUserAgentOverride`, `getCookies`, `setCookie`, `setCookies`, `deleteCookies`, `clearBrowserCookies` | done | page | `requestWillBeSent`, `responseReceived`, `dataReceived`, `loadingFinished`, `loadingFailed`, `requestServedFromCache` |
| Network | `emulateNetworkConditions` | partial | page | |
| Fetch | `enable`, `disable`, `continueRequest`, `fulfillRequest`, `failRequest` | done (see the limits) | page | `requestPaused` |
| Fetch | `continueWithAuth` | error | page | |
| Storage | `getCookies`, `setCookies`, `clearCookies` | done | browser, page | |
| Storage | `clearDataForOrigin` | partial | browser, page | |
| Log | `enable`, `disable` | done | page | `entryAdded` |
| Performance | `getMetrics` | done | page | |
| Performance | `enable`, `disable` | no effect | page | |
| Tracing | `start`, `end` | done | page | `dataCollected`, `tracingComplete` |
| IO | `read`, `close` (trace streams) | done | page | |
| Profiler | `enable`, `disable`, `start`, `stop` | done | page | |
| Audits | `enable` | no effect | page | none (`Audits.issueAdded` is never sent) |

Not translated: every other method of these domains (for example `Network.getAllCookies`, `Tracing.getCategories`, `Profiler.startPreciseCoverage`, `Page.printToPDF`, `DOM.pushNodesByBackendIdsToFrontend`), and the domains Debugger, HeapProfiler, Overlay, CSS, Security, Inspector, WebMCP and Autofill. `CSS.enable`, `CSS.getAllStyleSheets` and `CSS.getStyleSheetText` are always refused, because they hang through iwdp.

## How to connect

1. Pick the session and page as for any command, and get the URL:

   ```bash
   export IWDP_SESSION=pw-run
   iwdp-cli device reserve --owner pw-run        # physical devices only
   iwdp-cli open https://example.com             # the session's page
   iwdp-cli get cdp-url
   # ws://127.0.0.1:9230/t/<token>/e/<endpoint>/devtools/browser/<endpoint>
   ```

   `get cdp-url` starts the endpoint when it is not running.
2. Give the client **the exact URL as printed**. Never build a URL by hand: the token and the endpoint id are part of the path.
3. When the work is done, run `iwdp-cli close` for the session. It releases the lease and revokes the session's CDP URLs; later requests to them get 404.

Which page the URL selects:

`get cdp-url` resolves the page as every page command of the session does, in this order:

1. `--page`, when given: a tab id or label (`t2`, as `tab list` printed it), a page key (the first column of a page line in `iwdp-cli daemon status`, or `key` in `iwdp-cli tab list --json`), a part of a page's title or URL (the first listed page that matches), or a `ws://` page URL.
2. Else the page chosen with `tab <id>`.
3. Else the session's default page (the page that its last command without `--page` used), while the daemon holds that page's connection open. This step reads no listing.
4. Else the listing's first debuggable page.
5. When none of these gives a page (for example the chosen tab is gone, or the listing has no page), the command exits 5.

The transport of the request (`--transport` or `IWDP_TRANSPORT`) limits steps 2 and 3: step 2 skips a tab that was chosen with another explicit transport, and step 3 uses only a default page that a command with the same transport value set (no value counts as `auto`).

Issuing a URL takes no lease and does not change the session's default page.

The endpoint record lists and attaches pages through the transport and listing address where the selected page was found, not through the request's. For a page that the daemon holds open, these are the transport and address where the daemon found the page when it attached it. For example, a tab chosen on the direct transport, or the page key of an attached direct-transport page, gives a URL whose `/json/list` lists the direct transport's pages, also when the request named no transport. The record of an unlisted local endpoint (below) reads no listing.

A `ws://` page URL (in `--page`) gives one of two record types. The type follows how the daemon identifies the page, not whether the daemon holds it open:

- **A listed page**: the listing of the request's transport shows the URL, or the daemon holds open the page of that URL and found it through a listing (for example an iwdp page that a command attached by its tab id, title or URL). The URL resolves to that page's ordinary record, as a tab id or title would. Its scope rules hold: `--scope page` lists that page and the pages that the client creates, `--scope device` also the other pages of the reserved devices, and `Target.createTarget` works. Its target list reads the listing.
- **An unlisted local endpoint** (a WebKitGTK page or a mock that no listing shows, and only when the URL is not on an iwdp device port): the record has that page as its only target, whatever the scope. Its target list reads no listing, and `Target.createTarget` fails ("a new tab needs a page from the transport's page list"). The page stays an unlisted local endpoint while the daemon holds it open: a URL issued again for it, also with `--scope device` and also when a listing shows it by then, gives this record type again. An iwdp device-port URL that the listing does not show fails with exit 5 instead.

Limit: issuing a URL can avoid the listing for a page that the daemon holds open (a tab id, label, page key or `ws://` URL in step 1, and steps 2 and 3), but a record of a listed page still reads the listing for its `/json/list` and for every target lookup of a client (see "When iwdp does not list pages"). Only the record of an unlisted local endpoint reads none.

Other commands:

- `iwdp-cli cdp start [--port 9230] [--allow-origin a,b]` starts the endpoint without issuing a URL. `--port 0` picks a free port. The daemon flag `daemon start --cdp-port <port>` starts it with the daemon.
- `iwdp-cli cdp status` shows the address, the allowed origins and, per endpoint record, its session, page, scope, connections and attachments. It never prints the token.
- `iwdp-cli cdp log` shows the unknown-method counts per CDP session.
- `iwdp-cli cdp stop` closes every client connection (each releases its state) and revokes every URL.

### URL forms

- `--form ws` (default): the browser WebSocket URL `ws://127.0.0.1:9230/t/<token>/e/<endpoint>/devtools/browser/<endpoint>`. Every client accepts it. agent-browser needs it: with an `http://` URL it requests the discovery paths without the token and fails.
- `--form http`: the discovery base `http://127.0.0.1:9230/t/<token>/e/<endpoint>`, for clients that append `/json/version` to the base path (Playwright `connectOverCDP` and Puppeteer `browserURL`).
- `--json` prints both forms, the scope and the page.

### Scope, sessions and leases

- A URL belongs to an **endpoint record**: the session that issued it, the selected page (see "Which page the URL selects"), the reservation owner and the scope.
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

`<cdp-ws-url>` is the default output of `get cdp-url`; `<cdp-http-url>` is the output of `get cdp-url --form http`. Get them into a variable, for example `CDP="$(iwdp-cli get cdp-url)"`, and never print or store them.

The pinned versions are agent-browser 0.41.2, playwright-core 1.65.0, puppeteer-core 25.16.0 and chrome-devtools-mcp 1.12.0. "Loses" lists the optional features whose methods the endpoint answers with "wasn't found" (e2e/cdpcompat/expected-missing.json has each method with the client code that sends it). The client continues without them.

- **agent-browser**: `agent-browser --cdp <cdp-ws-url> open <url>`, then `snapshot -i`, `click @e3`, `fill @e4 text`, `get title`, `screenshot`. Use the ws form only. It attaches the page directly and makes no discovery request. Its refs are its own and are separate from iwdp-cli's refs. It prints "launched browser" also with `--cdp`; it launches nothing.
  - Loses: the experimental `webmcp` commands (`WebMCP.enable`; WebKit has no WebMCP).
- **Puppeteer**: `puppeteer.connect({browserWSEndpoint: '<cdp-ws-url>', defaultViewport: null})`, or `browserURL: '<cdp-http-url>'`. Always pass `defaultViewport: null`; otherwise Puppeteer sends `Emulation.setDeviceMetricsOverride`, which fails because the viewport is fixed. `page.setRequestInterception(true)` works through Fetch (suite test 12). `page.tracing` uses Tracing with a stream and IO, which the endpoint serves; the suite does not run it through Puppeteer.
  - Loses: `page.webmcp` (`WebMCP.enable`).
- **Playwright**: `chromium.connectOverCDP('<cdp-ws-url>')` (or `<cdp-http-url>`), then use `browser.contexts()[0]` and its pages. `newContext()` and pages in a new context fail, because iOS has no browser contexts. Locators run through Runtime and DOM; `page.mouse`, `page.keyboard` and `page.touchscreen` go through Input and are synthetic.
  - `page.route` works through Fetch. Use a glob that matches the full URL, such as `'**/resource'`: the default context of `connectOverCDP` has no `baseURL`, so a path glob such as `'/resource'` never matches.
  - `browser.startTracing()` without a page sends Tracing on the browser session and fails with "wasn't found"; pass a page.
  - Loses: `page.on('crash')` for a page that existed when `connectOverCDP` connected (`Inspector.enable`).
  - On Safari below 17.4 (iOS 16.7), locator actions, `getByRole` and `ariaSnapshot` need the endpoint's `checkVisibility` shim, and the features that use the popover API or `getHTML` fail (see "Page compatibility shims").
- **chrome-devtools-mcp**: `chrome-devtools-mcp --wsEndpoint <cdp-ws-url>`. It uses its bundled Puppeteer; the viewport rule above holds. Its page tools need the page id from `list_pages` (the server routes by page id by default); the list has the page of the URL. Checked tools: `list_pages`, `navigate_page`, `take_snapshot`, `click` by uid, `list_network_requests` and `take_screenshot`.
  - Loses: `get_css_styles` (`CSS.enable`; the endpoint never serves CSS), source-mapped stack traces of console messages and errors (`Debugger.*`), the element or request selected in an open DevTools window (`Target.getDevToolsTarget`), `list_webmcp_tools` and `execute_webmcp_tool` (`WebMCP.enable`), and DevTools-SDK extras that no tool reads (`Autofill.*`, `Log.startViolationsReport`, `Network.setAttachDebugStack`, `Runtime.getIsolateId`, `Storage.getStorageKey`). Most of these come from the second, DevTools-SDK CDP session that the server opens per page.
  - Not checked: whether its trace tools accept a trace converted from WebKit's Timeline (they expect Chrome's trace metadata).
- **Worlds**: Playwright's and Puppeteer's utility worlds are emulated as named contexts in the page's main world. They are not isolated from page scripts.
- **Dialogs**: `Page.handleJavaScriptDialog` fails with "dynamic dialog handling is not supported on iOS WebKit; pre-arm answers with iwdp-cli dialog accept|dismiss". The session's dialog policy still answers dialogs (`Page.enable` installs it when the session has none), and each automatic answer is reported as a `Log.entryAdded` warning. No `Page.javascriptDialog*` events are sent.

## Page compatibility shims

CDP clients believe that they drive Chromium, so their page scripts call newer Web APIs without a check. Safari 16.6 (iOS 16.7) lacks some of them. The endpoint adds the missing APIs that the pinned clients need on their main paths.

- **What is shimmed:** `Element.prototype.checkVisibility` ([CSSOM View](https://drafts.csswg.org/cssom-view/#dom-element-checkvisibility); Safari has it from 17.4). Playwright 1.65's injected script calls it, without a check, on every visibility check: the actionability checks of locator actions, `getByRole` (which leaves hidden elements out) and `ariaSnapshot`. Without it these fail at once with `TypeError: element.checkVisibility is not a function` (device check 3, iOS 16.7). An inventory of the pinned clients' page scripts against the browser compatibility data found no other API that these paths need: Puppeteer and chrome-devtools-mcp use their own visibility check, and agent-browser uses none of the missing APIs.
- **When:** each page attachment installs the shims when it attaches, in two places: in the current document of the main frame, and as a bootstrap part of the attachment, which WebKit runs in each new document before its scripts. The part runs before the client's `Page.addScriptToEvaluateOnNewDocument` scripts. A failed install refuses the attachment with the reason.
- **Only when missing:** a shim is installed only when its API is missing, so a native method is never replaced. On iOS 17.4 and later and on WebKitGTK the shims change nothing.
- **Only for CDP attachments:** the CLI installs no shims; their runtime has its own visibility rule.
- **Main world:** the shims are in the page's main world, where the utility worlds of Playwright and Puppeteer also run.
- **Detach:** the detach removes the attachment's bootstrap part, so later documents have no shim. The current document keeps the shim: a page script may already hold or call the method, and removing it would break that script. The shim answers as the native method does, except in the closed-shadow-root case under "Limits".
- **The checkVisibility rules:**
  - No associated box gives false. The shim finds no box when the element has no client rects (`getClientRects()`: `display: none` on the element or an ancestor, `display: contents`, a detached element), and for the elements that WebKit lays out without a box of their own: the content of a closed `<details>` element other than its first `<summary>` (Safari 16 puts it in a `display: none` slot of the element's user-agent shadow root), and the options and option groups of a `<select>` (WebKit draws the select's content itself).
  - `checkOpacity` or `opacityProperty`: computed opacity 0 on the element or a flat-tree ancestor gives false. The shim walks the flat tree from the element to the document: from a slotted element to its slot (`assignedSlot`), from a shadow root's child to the host.
  - `checkVisibilityCSS` or `visibilityProperty`: a computed `visibility` other than `visible` gives false.
  - `content-visibility` (step 2 and `contentVisibilityAuto`): Safari 16 has no `content-visibility`, so no ancestor skips its contents; the shim reads the option and checks nothing.
  - Arguments are checked and converted as WebIDL does, before the steps above: first a `this` that is not an element is a `TypeError`; then the options: `undefined` and `null` are no options, another non-object is a `TypeError`, and each option is read once, in alphabetical order (`checkOpacity`, `checkVisibilityCSS`, `contentVisibilityAuto`, `opacityProperty`, `visibilityProperty`), and converted to a boolean. The box test runs after the conversion, so an option getter that changes the layout changes the answer as it does for the native method.
  - Proof: a differential test on WebKitGTK compares the shim with the native method on 65 cases (display, visibility, opacity, `<details>`, `display: contents`, zero size, SVG, shadow DOM, slots, `position: fixed`, inline elements, selects, detached elements) with every combination of the options, and finds no difference (e2e/webkitgtk/page_shims_test.go). Its edge cases add option getters that change the layout, the order and number of option reads, boolean conversion, and elements slotted into closed shadow roots; the only differences are the documented closed-shadow-root limit. The compatibility suite runs Playwright's `getByRole().click()`, `fill` and `ariaSnapshot` on a page without the native method.
- **What the page sees:** on a page without the native method, `Element.prototype.checkVisibility` exists while a CDP client is attached, and in that document after the detach. It is writable, enumerable and configurable, like a native method, and its function has the property `__iwdpShim` (`true`, not enumerable). A page script that tests for the method finds it. The shim keeps the `getClientRects` and `getComputedStyle` that it found at the install, so a later replacement by a page script does not change its answers.
- **Limits:**
  - Not shimmed: Playwright's highlight overlay calls `showPopover` and `hidePopover` (Safari 17) without a check, so `locator.highlight()`, the `mask` option of screenshots and the inspector and screencast annotations fail on iOS 16.7 (from the client code; not run). A script cannot reproduce the popover's top layer. Its document content with shadow roots calls `Element.getHTML` (Safari 18). The restore of a storage state with origin-private files calls `FileSystemFileHandle.createWritable` (Safari 26), and the WebSocket mock of `routeWebSocket` calls `URL.parse` (Safari 18), both without a check (from the client code; not run).
  - **Closed shadow roots:** a page script cannot see a slot inside a closed shadow root: `assignedSlot` of an element assigned to such a slot is `null`. The shim's flat-tree walk then goes from the element straight to the shadow host and misses the slot and its ancestors inside the closed root. Effect:
    - With `checkOpacity` or `opacityProperty`, an `opacity: 0` ancestor of that slot inside the closed root is missed, and the shim answers true where the native method answers false. Playwright's call has no options and is not affected.
    - A closed `<details>` ancestor of that slot inside the closed root is missed. On Safari 16 this changes no answer: Safari 16 lays the content of a closed `<details>` out in a `display: none` slot, so the element has no client rects and the box test gives false (inferred from the WebKit layout described above; not run on a device). WebKitGTK lays that content out with `content-visibility: hidden`, which keeps client rects, so on WebKitGTK with the native method removed every call answers true where the native method answers false (observed). Safari has the native method from Safari 17.4, and the endpoint installs the shim only where the method is missing. The Safari release whose layout of closed `<details>` content keeps client rects is not known, so this difference can also occur on a Safari earlier than 17.4 that has that layout.
    - Visibility is not affected: the shim reads the element's computed `visibility`, which the browser inherits along the flat tree.
    - The shim does not intercept `attachShadow` to find closed roots: a root that existed before the install would still be missed, and the interception would change a page API. The differential test keeps these cases as expected differences.
  - A child frame whose document existed at the attach has no shim until it loads a new document.
  - A new document gets the shim from the bootstrap script, which is best effort after a cross-site navigation, as for every bootstrap part (pause-on-start, [known-limits.md](known-limits.md#features-and-platforms)). A document that missed it has no shim until the next attach or navigation.
  - The shim was proved on WebKitGTK with the native method removed, and on iOS 16.7 by device check 4: Playwright's `getByRole` click, `getByLabel` fill, locator read and `ariaSnapshot` passed (see "Status").

## Limits per domain

General:

- An endpoint started under `IWDP_NO_DAEMON=1` ends with that CLI process.
- The endpoint reads iwdp's page listing for its target list; see "When iwdp does not list pages".
- Workers and service workers are not exposed as targets, and their Network events are not reported.

Target and Browser:

- `createTarget` uses `window.open` in the selected page with an emulated user gesture; a popup blocker can stop it. `closeTarget` uses `window.close()`, which works only for tabs that a script opened.
- `activateTarget` and `Page.bringToFront` only probe the page: WIP cannot bring a tab to the front.
- `Browser.grantPermissions` fails: WIP has no permission method; allow permissions on the device.

Runtime:

- The utility worlds are not isolated (see "Client notes").
- **Oversized sources.** A `Runtime.evaluate` expression or `Runtime.callFunctionOn` declaration whose message is above the page connection's limit is staged in the page in parts and run with indirect eval. Limits as configured: WebKitGTK 127 KiB (measured), iwdp 64 MiB and the direct transport 16 MiB (not measured on a device). A source that must be staged is refused (invalid params) when it is above 16 MiB; a source of any size that the connection's limit accepts is sent as one message, as before. A staged call sends one message per part plus one (about 25 messages for a 330 KB source on WebKitGTK).
  - WebKit allows this eval on a page whose Content-Security-Policy forbids eval (checked on WebKitGTK).
  - The page can replace `eval`, so the bridge reads it once, checks it with two random probes and runs the source with that same function. A replacement that throws, or that does not give both probe values, makes the call fail with an error that names the refusal; nothing is evaluated.
  - An error that the staged source itself throws keeps its result and `exceptionDetails`, whatever its text: the refusal signal uses a random marker per call, which the page holds only while the source is staged and which is in no source text or caller that the staged code can read.
  - This is a check, not a guarantee: a page that deliberately interferes (one that emulates `eval` well enough to pass the probes, or a page script that reads the staging store while a call stages) can still change the result.
  - A staged source runs without the console's command line API and keeps its top-level `let`, `const` and `class` declarations local (`var` and `function` declarations are global).
  - A `Page.addScriptToEvaluateOnNewDocument` script that makes the page's bootstrap script larger than the limit is refused.

Page:

- `captureScreenshot`: png and jpeg (jpeg is encoded on the host); webp is refused, and `clip.scale` is ignored (the image has the device's pixel ratio).
- `getNavigationHistory` lists only the current entry (id 0); `navigateToHistoryEntry` runs `history.go(entryId)`, so entry ids are offsets.
- `navigate` of a child frame is refused.
- `networkAlmostIdle` comes together with `networkIdle` (500 ms without a request in flight).
- `setBypassCSP` fails: bypassing the Content Security Policy is not supported on iOS WebKit.
- A console replay by WebKit can repeat old navigation markers or dialog warnings of the current document. CDP navigations do not withdraw dialog answers armed with `iwdp-cli dialog`; they expire with their document.

DOM:

- No DOM events (`DOM.documentUpdated`, `DOM.setChildNodes`) are sent.
- `DOM.getDocument` nodes have `backendNodeId` 0, and `DOM.describeNode` gives `nodeId` 0 unless the client named the node by `nodeId`. The pinned clients do not read these values.
- `setFileInputFiles` reads at most 8 MiB of regular files from the host and skips the actionability checks.

Accessibility:

- **Whitespace-only text nodes.** `getFullAXTree` has a StaticText node named " " for each whitespace-only text node between elements, which Chrome leaves out. chrome-devtools-mcp's `take_snapshot` shows them as lines such as `uid=1_3 " "`, and Puppeteer's `accessibility.snapshot()` as extra StaticText leaves. Elements, their names and their uids or refs are not affected. See [known-limits.md](known-limits.md).

Input and Emulation:

- All input is synthetic (`isTrusted=false`); see [synthetic-input.md](synthetic-input.md).
- `setDeviceMetricsOverride` succeeds only for the real viewport (or 0/0/0); `clearDeviceMetricsOverride` always fails with "viewport is fixed by the device".

Network:

- **emulateNetworkConditions: download limit only.** `downloadThroughput` is applied through WIP `Network.setEmulatedConditions` when the device's command list has it, else the call fails. `offline: true` fails and points to `iwdp-cli set offline on`; a positive `latency` or `uploadThroughput` fails before any change. The unthrottled values that clients send (latency 0, throughput -1 or 0) are accepted.
- Post data comes only from WIP's `requestWillBeSent`: post data above 64 KiB is left out of the event, and `getRequestPostData` keeps at most 1 MiB per request and 16 MiB in total.
- A request in flight ends with `loadingFailed` (`net::ERR_ABORTED`, canceled) when its target is replaced or destroyed, when the record bound (2000 requests) pushes it out, or at a reset of the attachment's events.
  - **Reset on the same connection** (for example after lost or overflowed events): a request that the attachment holds paused through Fetch (`Fetch.requestPaused` without an answer) stays paused, because WebKit still holds it. It still needs an answer (`continueRequest`, `fulfillRequest` or `failRequest`); every other request in flight ends.
  - **Reset on a new page connection** (a reconnect): every request in flight ends, also the paused ones, because WebKit continued them when the old connection went away. An answer to such a request then gets "Invalid InterceptionId.".
  - **The daemon's Network reset:** after the daemon resets the page's network agent (a removed extra header, a new page connection, a setup that disables Network), the requests in flight end the same way, except a request that the attachment still holds paused and that the daemon's reset did not continue; such a request stays paused and needs an answer. A paused request that the reset continued is no longer paused (an answer gets "Invalid InterceptionId."). Response bodies of the ended requests are gone, and the client gets one `Log.entryAdded` warning.

Cookies (Network and Storage):

- **Exact delete.** `Network.deleteCookies` matches as Chrome does: name and domain exactly (a leading dot makes a domain cookie), path only when given. Each matched cookie is deleted by a `Page.setCookie` of the same identity with an expiry in the past, then read back; the call fails, naming the cookies without values, when one is still there. Namesakes of another path, of the parent domain and of the other kind stay (checked on WebKitGTK). Open on iOS: whether an expired `Page.setCookie` (NSHTTPCookieStorage) deletes exactly that identity and keeps those namesakes, and whether the `Page.getCookies` right after it already shows the change.
- iOS cannot give `priority` (always "Medium"), `sourceScheme` ("Unset"), `sourcePort` (-1) or partition keys. A `partitionKey` on set fails; on delete it matches nothing.
- WIP lists only the cookies of the page's frames and resources, so `Storage.getCookies` gives the selected page's cookies, and a cookie of another site cannot be verified (`setCookie` returns `success: false`).
- On the browser session, cookie methods act on the record's selected page under the session's lease; a page that another session leases fails with the lease message. `browserContextId` may only be "default".

Storage:

- **clearDataForOrigin: localStorage only.** `local_storage` and `all` clear localStorage of every page in the endpoint's scope that shows the origin. sessionStorage is never cleared. A request with only `cookies`, `indexeddb`, `cache_storage`, `file_systems`, `shader_cache` or `service_workers` fails and clears nothing; in a mixed list these values are ignored. A request fails when no page in scope shows the origin.

Fetch:

- WIP has no resource-type filter: the bridge checks `resourceType` against the request's type from its latest `requestWillBeSent`, and continues a request that no pattern claims. A request whose `requestWillBeSent` no attachment saw counts as type Other.
- **continueWithAuth** always fails with Chrome's "authChallengeResponse not expected.": the bridge never pauses for an authentication challenge (`handleAuthRequests` has no effect, `Fetch.authRequired` is never sent).
- Not supported (server error): `continueRequest` with `interceptResponse`, a change of a request at the response stage, `failRequest` at the response stage, and `fulfillRequest` without a body at the response stage.
- **A lost reply.** When the reply to a rule that WebKit applied is lost, a retried `Fetch.enable` fails because WebKit refuses the duplicate rule. Recover with `Fetch.disable`, then `Fetch.enable`.
- Paused requests that the client did not answer are continued unchanged at `Fetch.disable`, at the detach, at a connection loss and at the lease end. A request matched by the session's CLI interception (`network route`, `set offline`, `set credentials`) is answered by the CLI handler; the CDP client sees only its Network events.

Tracing, IO and Profiler:

- **Browser-level Tracing is not served.** `Tracing.start` and `Tracing.end` exist on page sessions only.
- A trace is a WIP Timeline recording (plus ScriptProfiler events when the categories ask for V8), converted to Chrome's trace format; it is capped at 32 MiB (`dataLossOccurred: true` beyond). One Timeline recording per page: a second start, or another Timeline user (also `iwdp-cli trace start`), fails with "Tracing has already been started (possibly in another tab)."
- `IO` serves only trace streams; at most 2 unread streams per attachment.
- `Profiler`: no coverage methods. A start while another recording holds the page's ScriptProfiler fails and names the holder.
- A detach or the end of the connection stops the attachment's recordings.

Other:

- Passthrough of untranslated methods to WIP is not available. `CSS.enable`, `CSS.getAllStyleSheets` and `CSS.getStyleSheetText` are always refused, because they hang through iwdp.
- See also [known-limits.md](known-limits.md).

## When iwdp does not list pages

On the second device check, the user's iwdp closed every listing request without a reply while any WebSocket to a page was open (see [known-limits.md](known-limits.md)). Device check 3 found the cause: one broken file-descriptor slot inside iwdp 1.9.2; restarting iwdp recovers. The CLI commands on a page that the daemon holds open do not need the listing. The CDP endpoint does:

- The target list and every target lookup of a record of a listed page (an iwdp page of the listing) read the listing: `/json` and `/json/list` (HTTP 503 "listing the pages: ..."), a `/devtools/page/<targetId>` connection, `Target.setDiscoverTargets`, `getTargets`, `getTargetInfo`, `setAutoAttach`, `attachToTarget`, `createTarget`, `closeTarget`, `activateTarget`, the cookie methods on the browser session and `Storage.clearDataForOrigin`. On such an iwdp they fail with the listing error and its advice while the daemon holds a page connection, also the connection of an earlier CLI command.
- The target discovery that runs every 2 s while a client discovers or auto-attaches logs the failure and announces nothing.
- `get cdp-url` reads no listing when it selects a page that the daemon holds open: the session's `tab` choice, its default page, or `--page` with its tab id, label, key or `ws://` URL (steps 1 to 3 of "Which page the URL selects"). It reads the listing for a title or URL text, for the `ws://` URL of a page that the daemon does not hold open, and for the listing's first page.

Advice: run `iwdp-cli daemon stop` to close the page connections (this also stops the endpoint and revokes its URLs), or restart that iwdp (`iwdp-cli restart-iwdp` for an iwdp that iwdp-cli started; any other iwdp only whoever started it, and only when nobody else uses it: `scripts/iwdp-owner-check.sh --terminate <record>`, then `scripts/iwdp-owner-check.sh <record>` until it reports `gone`, as [Stopping an iwdp by hand](device-runbook.md#stopping-an-iwdp-by-hand) in device-runbook.md describes; otherwise ask whoever started it, or the user); then get a new URL. Not checked on a device: whether a CDP client can work on such an iwdp at all, because its own attachment opens a page connection.

## Compatibility suite

`make test-cdpcompat` runs the pinned clients against the endpoint (e2e/cdpcompat; its README has the details).

- It runs `npm ci --ignore-scripts` when the lockfile is newer than `node_modules`, then `IWDP_CDP_COMPAT=1 go test -tags=cdpcompat ./e2e/cdpcompat/`. It needs Node.js 24 or later with npm, the WebKitGTK MiniBrowser, broadwayd, gcc and `pkg-config gdk-3.0`. A missing requirement fails the suite; it never skips.
- agent-browser: the pinned release binary always runs. The agent-browser on `PATH` runs the same scenarios too, but only when `PATH` has one and the binary that it runs differs from the pinned binary (another SHA-256); otherwise the record says why it did not run. So a passing suite has not always run a second build. When `PATH` names a script (a wrapper), set `IWDP_CDPCOMPAT_PATH_AGENT_BROWSER` to the absolute path of the native binary that the script runs; without it the suite fails and names the script. Example: `IWDP_CDPCOMPAT_PATH_AGENT_BROWSER=$HOME/projects/agent-browser/cli/target/release/agent-browser make test-cdpcompat`.
- `CDPCOMPAT_LOG_DIR` keeps the logs of a run in that directory, with tokens and endpoint ids replaced. Example: `CDPCOMPAT_LOG_DIR="$PWD/cdpcompat-logs" make test-cdpcompat`.

What it proves, on WebKitGTK:

- Endpoint resolution (test 10, fake page): each client opens exactly the printed URL; agent-browser's ws form makes no discovery request; Playwright and Puppeteer with the http form read `/json/version` below the token prefix and open the advertised URL; no request without the token gets data.
- Client scenarios (test 8): agent-browser (both builds), Puppeteer and Playwright (ws and http forms) and chrome-devtools-mcp navigate, snapshot, click, type or fill, read, wait and take screenshots, and each step is checked on the page through an `iwdp-cli eval` of the same session.
- Interception (test 12): an awaited `fetch` that Puppeteer's request interception or Playwright's `page.route` fulfills returns within 2 s.
- Session binding (test 11 a and d): a Puppeteer connection joins the CLI session without a lease change, and a URL of another session lists the page but cannot attach it.
- Page compatibility shims: on a page that removed its native `checkVisibility` before the client attached, Playwright's `getByRole().click()`, `getByLabel().fill()` and `ariaSnapshot` pass through the shim; without the shim they fail with the device's TypeError.
- Every unknown method that a client sends must be on its expected-missing list, with the client code, the reason the client continues and the feature it loses.

Not proved: the suite runs only against WebKitGTK. Device check 3 ran the client scenarios on iOS 16.7 by hand, and device check 4 ran the Playwright scenario with the shim there (see "Status").
