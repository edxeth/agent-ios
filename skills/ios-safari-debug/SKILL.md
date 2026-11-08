---
name: ios-safari-debug
description: Debug and drive iOS Safari pages through the iwdp-mcp MCP tools (snapshot with @eN refs, synthetic click, fill, tap and press, waits, dialogs, tabs, console, network, cookies, screenshots). Use when the iwdp-mcp server is connected and the user asks to inspect, test or debug a page in Safari on a USB-connected iPhone or iPad.
---

# iOS Safari debugging with the iwdp-mcp tools

The iwdp-mcp server gives MCP tools for Safari tabs on a USB-connected iPhone or iPad. It talks to the iwdp daemon, one background process per user that holds the only inspector connection to each page. The CLI `iwdp-cli` uses the same daemon; its skill is `iwdp-cli skills get iwdp`. Use the MCP tools by default, and the CLI only when the user asks for it or when a step has no tool (for example `iwdp-cli device reserve`).

## Safety rules

These rules bind every run that uses this skill.

1. **No infrastructure writes.** Do not start, stop, restart or reconfigure servers, containers, compose stacks, proxies, routers, databases, runtime or devenv helpers, or the checkout. Use only the sign-in helpers that the task names. When a guard or wrapper refuses a command, stop and report the refusal; never change the call so that the guard passes.
2. **Preflight, then one final verdict.** Before the first check, confirm with one bounded call each that the origin answers, that the build under test is live (a fingerprint, or the changed rule or text is present), and that the sign-in state is the one the task states. When one differs, the check is BLOCKED; do not work around it. End the run with exactly one verdict line, `VERDICT: PASS`, `VERDICT: FAIL` or `VERDICT: BLOCKED`, and one PASS, FAIL or NOT RUN row per check.
3. **One bounded diagnostic budget, then BLOCKED.** When the same step fails twice (a deadline, a wait that timed out, a lease error, a refused sign-in), stop retrying. Call `session_info` once as the health probe, record the result, and mark the check NOT RUN. When no check can run, report BLOCKED.
4. **Masking covers a few outputs only; filter the rest.** By default `get_cookies` redacts cookie values. The network and storage masking needs the `devtools` profile (`devtools`, `all`, or a list such as `core,devtools`; `get_network_request` is in the tool list only then): there `list_network_requests`, `get_network_request` and `har_stop` print the values of secret headers and of query, form and JSON fields whose names mark password and token fields as `[masked]`, and `storage` masks the values of password and token keys. Without `devtools`, the `core` (default) and `legacy` profiles give the older `list_network_requests`, which is not masked: it returns full URLs with their query tokens and request headers such as `Authorization`. `get_local_storage` and `get_session_storage` are not masked in any profile. Without `devtools`, do not list the requests of a signed-in page or of a page that sends tokens: mark that check NOT RUN and name the profile as the reason. Ask a tool for values only when the task needs a value. Page reads are not masked: `get` (text, html, value), `read_page`, `evaluate_script`, `get_console_messages`, `get_errors`, `get_response_body` and snapshot text return what the page holds. Read only the fields you need, with an allowlisted expression, and never read the value of a password or token field. Do not read the text, HTML, snapshot or screenshot of account, security or settings pages: read named fields only. HAR and state files have mode 0600 and hold live credentials: write them only into the evidence folder and delete them before the run ends.
5. **Preserve and restore cookie owners.** Before you set or clear a cookie, save the current state (`state_save`), and restore it (`state_load`) at the end. Revoking a server-side session belongs to the sign-in helper that created it; report every session you created and whether it was revoked.
6. **Bounded shell calls and known evidence folders.** Give every shell call a timeout. Write every file by absolute path into the evidence folder that the task names. Never search the filesystem (`find /`, `find ~`, `grep -r` over home or `~/.pi`).
7. **The device is shared.** Never stop, restart or kill an iwdp that another agent may use. `iwdp_status` starts iwdp only when none runs and never restarts a running one; `restart_iwdp` refuses an iwdp that iwdp-mcp did not start. Reserve the device at the start of a run and release it at the end. Give the MCP server a session name of its own (`IWDP_SESSION` in its environment).

## Setup

- The iPhone needs Web Inspector on (Settings, Safari, Advanced, Web Inspector), an open Safari tab, and the device unlocked and trusted.
- `iwdp_status` checks ios_webkit_debug_proxy. Port 9221 lists the devices; the first device's pages are on port 9224.
- The server reads `IWDP_SESSION` (session name, default `default`) and `IWDP_DEVICE_RESERVATION` (reservation owner) from its environment. Pages of a physical device need a reservation of that owner. Make it with the CLI, which shares the daemon:

```bash
iwdp-cli device reserve --owner "$IWDP_SESSION" --ttl 1h
iwdp-cli device list
```

- `IWDP_MCP_TOOLS` (or `--tools`) selects the tool profiles: `core` (default, about 45 agentic and key debugging tools), `devtools` (the masked network log, network routes, HAR, emulation, traces, the CDP URL), `legacy` (the older one-domain tools, such as the debugger, heap and canvas tools), or `all`.

## The agentic loop

```text
1. tab_list                            the tabs t1, t2, ...; * marks this session's tab
2. tab_select {id: "t1"}               make t1 the session's page (probes that it answers)
3. navigate {url: "https://example.com/login"}
4. snapshot {interactive: true}        elements with refs: textbox "Email" [ref=e1] ...
5. fill {target: "@e1", text: "qa@example.com"}
6. click {target: "@e3"}
7. wait {text: "Signed in"}
8. snapshot {interactive: true}        new refs after every change of the page
```

- A ref (`@e3`) names one element of the current document and stays valid across calls of the session. After a navigation, a dialog or any change of the page, take a new snapshot and use only refs from it. Targets can also be CSS selectors or locators (`text=Sign in`, `role=button`, `label=Email`).
- Before an action on an element, the runtime repeats the actionability checks of that action until the action deadline (5 s). The checks differ per action (the table in docs/guide.md, step 4 of the agentic loop). Every action on an element checks that the element is attached and visible (`scroll` and `swipe` act on an element only with `selector`; `press` acts on the focused element and checks nothing). `click`, `tap`, `hover` and `check` also wait until it is stable and receives events at its center (not covered). `click`, `tap`, `check`, `select_option`, `fill`, `type_text` and `upload_file` check that it is enabled, and `fill` and `type_text` that it is editable. `fill` and `type_text` do not check stability or coverage, so they can change a field that another element covers: a successful fill is no evidence that a user can reach the field. `hover` does not check that the element is enabled. A click on a covered element fails and names the covering element: report it as a finding.
- `find` locates by role, text, label, placeholder or test id and can act on the match. `get` reads url, title, text, html, value, attr, count, box or styles; `is` answers visible, enabled, checked, editable or focused; `hit_test` names the element at a viewport point; `read_page` prints the visible text; `diff_snapshot` shows what changed.
- `wait` takes a ref, `text`, `url` (a glob), `load` (`load`, `domcontentloaded`, `networkidle`) or `fn` (a JavaScript condition), with a timeout of 25 s. Never wait with a bare sleep.
- `go_back`, `go_forward` and `reload` wait for the load event. `select_frame` moves later calls into a same-origin iframe.

## Synthetic input

WebKit has no input domain: every click, tap, key and scroll is a synthetic DOM event (`isTrusted` false) in the order a browser sends them, and every result says so. Code that requires trusted input or a user gesture (popups, clipboard, fullscreen, media, some payment and auth widgets) can behave differently from a real finger. State the input method in every report, and report a check that needs a real touch as NOT RUN.

## Dialogs

WebKit cannot answer a dialog after it opens, so arm the answer first with `dialog`: `{action: "accept"}` (with `value` for a prompt), `{action: "dismiss"}`, `{action: "status"}` for the log, `{action: "default", value: "accept"}` for unarmed dialogs. Without an armed answer, `confirm()` returns false and `prompt()` returns null. A `beforeunload` dialog cannot be answered.

## Evidence and DevTools

- `take_screenshot` writes a PNG (viewport, full page, or one element with `selector`) and returns its path; give an absolute path in the evidence folder. Read the image file to view it.
- With `devtools` in the profile, `get_console_messages`, `clear_console` and `get_errors` read what the daemon captured since the session took the page. Without `devtools` (`core`, the default, or `legacy`), `get_console_messages` and `clear_console` are the older tools: `get_console_messages` lists only the messages that `console_enable` (`legacy` profile) captured, so in `core` alone it is always empty, and `get_errors` is not registered.
- With `devtools` in the profile, `list_network_requests` and `get_network_request` show the requests since the session took the page, and the values of secret headers and of password and token fields print as `[masked]`. Without `devtools` (`core`, the default, or `legacy`), `list_network_requests` is the older tool and is not masked: it lists only the requests that `network_enable` (`legacy` profile) captured, with full URLs and request headers, so in `core` alone it is always empty, and `get_network_request` is not registered. An empty list from an older tool is no evidence that the page sent no request or logged nothing. `get_response_body` returns a response body as the server sent it, unmasked.
- devtools profile: `network_route` and `network_unroute` (abort, fulfill or delay requests), `har_start` and `har_stop`, `set_media`, `set_user_agent`, `set_geolocation`, `set_offline`, `set_emulated_conditions`, `trace_start` and `trace_stop`, `profiler_start` and `profiler_stop`, `vitals`, `storage`, `state_save` and `state_load`, `clear_cookies`, `get_cdp_url`.
- `get_cookies` lists cookies with redacted values, httpOnly ones included. `set_cookie` takes name, value, domain and sameSite (default Lax) and reads the cookie back.
- `evaluate_script` runs JavaScript in the page and can await a promise; a thrown exception returns its message and stack.

## CDP endpoint

`get_cdp_url` (devtools profile) returns a `ws://127.0.0.1:9230/...` URL for the session's page, for agent-browser (`--cdp`), Puppeteer (`browserWSEndpoint`, with `defaultViewport: null`), Playwright (`connectOverCDP`) and chrome-devtools-mcp (`--wsEndpoint`). The URL holds a secret token: never print it or write it to a file. A CDP client acts for the session of the MCP server. Today the endpoint answers the Browser and Target domains and `Runtime.evaluate`; the Page, DOM, Accessibility, Input, Network, Fetch, Storage and Tracing domains: W5b/W5c, see docs/notes/w5.md.

## End a run

Call `close_session`: it releases the session's pages and removes its routes, headers, overrides and dialog answers, and revokes its CDP URLs. It does not close the Safari tab and does not undo forms you submitted. Then release the device:

```bash
iwdp-cli device release --owner "$IWDP_SESSION"
```

## Errors

- "page ... is leased by session ...": another session holds the page. Wait or ask; never take it over without the device owner's approval.
- "tab is not answering; it is probably not the visible Safari tab": Safari answers only for the visible tab. Ask the user to bring it to the front.
- "connection to page was closed by someone else or by the device; ownership is unknown": another inspector took the page. Stop and ask.
- A deadline error names the command, the page and the likely causes. Follow safety rule 3.
- Not possible on iOS WebKit: trusted input, viewport and device emulation, PDF, downloads, video recording, browser contexts (every session shares one cookie jar), native pickers, cross-origin iframes.
