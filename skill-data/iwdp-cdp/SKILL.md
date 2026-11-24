---
name: iwdp-cdp
description: Attach a CDP client to an iOS Safari or WebKitGTK page through iwdp-cli. The tokenized CDP URL, agent-browser, Puppeteer, Playwright and chrome-devtools-mcp, and the limits of the endpoint. Load it from the iwdp guide when a task needs a CDP client.
---

# iwdp-cli: the CDP endpoint

The safety rules and private mode of `iwdp-cli skills get iwdp` bind every command of this guide.

Prefer the `iwdp-cli` commands of the iwdp guide. Use the CDP endpoint only when the task needs a CDP client: an existing Puppeteer or Playwright script, or a client feature that has no command. The commands have refs that survive reconnects, actionability checks, masking, deadlines and private mode; a CDP client has none of these from iwdp-cli, and its output is not private.

`get cdp-url` starts the CDP endpoint on 127.0.0.1 (port 9230) and prints a WebSocket URL. The URL holds a secret token: keep it in a variable, never print it or write it to a file. A CDP client acts for your session: it shares your lease, refs and dialog answers, and `close` revokes the URL.

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
CDP="$(iwdp-cli get cdp-url)" || exit "$?"
[ -n "$CDP" ] || exit "$?"
iwdp-cli close                               # revokes the URL
```

<!-- doc-test: expect -->
```text
close exit 0
```

<!-- doc-test: skip w5b: runs agent-browser, which the skill doc test does not install; e2e/cdpcompat TestClientScenarios runs this form -->
```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
mkdir -p -m 700 "$EVIDENCE" || exit "$?"
CDP="$(iwdp-cli get cdp-url)" || exit "$?"
agent-browser --cdp "$CDP" snapshot -i >"$EVIDENCE/cdp-snapshot.txt"
```

- The URL is for the page your other commands use: the tab you chose with `tab`, or `--page t2` for another tab.
- agent-browser: `agent-browser --cdp "$CDP" <command>`, ws form only. Its refs are its own, not iwdp-cli's, and it prints page titles and URLs: send its output to a file in the evidence folder.
- Puppeteer: `puppeteer.connect({browserWSEndpoint: CDP, defaultViewport: null})`; without `defaultViewport: null` it fails, because the viewport is fixed.
- Playwright: `chromium.connectOverCDP(CDP)`, then `browser.contexts()[0]`; new contexts fail. For `page.route` use a full-URL glob such as `'**/api/*'`. On iOS below 17.4 (Safari 16), locator actions, `getByRole` and `ariaSnapshot` work through the endpoint's `checkVisibility` shim, but `locator.highlight()` and the `mask` option of screenshots fail, because Safari 16 has no popover API.
- chrome-devtools-mcp: `--wsEndpoint "$CDP"`; call `list_pages` first and pass the page id to the page tools. `get_css_styles` and source-mapped console stacks do not work.
- `get cdp-url --form http` prints the discovery base for Playwright and Puppeteer `browserURL`. `cdp stop` revokes every URL.
- The endpoint serves only the methods of the method table in docs/cdp.md of the iwdp-cli repository, with the limits there. Any other method gets "wasn't found". Input through CDP is synthetic too.
