---
name: iwdp
description: Drive and debug Safari on a real iPhone or iPad or in an iOS Simulator over SSH, and local WebKitGTK pages, from Linux with iwdp-cli. Snapshots with @eN refs, actions, waits, console, network, storage, screenshots, traces and a CDP endpoint for CDP clients. Use whenever a task involves iOS, iPhone or iPad (for example checking that web changes look right on iOS), iOS Safari, Web Inspector, iwdp or mobile Safari bugs. Prefer it to desktop browser emulation for iOS.
---

# iOS Safari with iwdp-cli

`iwdp-cli` drives Safari pages on a USB-connected iPhone or iPad or in an iOS Simulator over SSH, and pages of a local WebKitGTK browser. It speaks the WebKit Inspector Protocol, not the Chrome DevTools Protocol (CDP). The command names and flags follow agent-browser where WebKit can support them.

```text
iPhone Safari -> ios_webkit_debug_proxy (iwdp) -> iwdp daemon -> iwdp-cli, CDP clients
```

The **daemon** is one background process per user. The first command starts it. It holds the only inspector connection to each page, so refs, waits and captures survive from one command to the next. Every `iwdp-cli` command is a short client of the daemon.

`iwdp-cli <command> --help` prints the flags of a command and never connects to a page; `iwdp-cli network --help` (any group word) lists the commands of that group. Load another guide when the task needs it:

| Load | When you need |
|---|---|
| `iwdp-cli skills get iwdp-devtools` | HAR files, request routes, media and network emulation, traces, CPU profiles, init scripts |
| `iwdp-cli skills get iwdp-cdp` | A CDP client: agent-browser, Puppeteer, Playwright, chrome-devtools-mcp |
| `iwdp-cli skills get iwdp-device` | The simulator (sim up), a device that is not listed, a page list that does not answer, the rules for a restart of iwdp |

## Private mode

A transcript that other parties read must not hold what iwdp-cli knows about a page: its title, its URL, its origin, its device, the session that holds it, the text of an error. Each can carry a token or name a person. With `IWDP_PRIVATE=1` (or `--private`), iwdp-cli says none of that:

- **Stdout holds the result**, and only of a command whose result you asked for. A page read prints its result, without the page's title, URL or origin, also with `--json`: `snapshot`, `get`, `is`, `eval`, `find`, `hit-test`, `read`, `console` and `errors` (without the source's URL and stack), `cookies`, `storage` (keys and values), `network requests`, `screenshot`. `storage ... set`, `storage ... clear`, `state save` and `state load` print only their status line. `tab list` prints the tab ids, `tab find` one tab id, `session info` the lease and connection of each page, `status` whether iwdp answers and how many devices it lists. Every other command prints nothing there.
- **Stderr holds exactly one status line for every run**, help included: `<command> exit 0`, with `warnings <n> dialogs <n>` after an element or key action; or for a failure `<command> exit <code>: <meaning>`, with that exit code. The message of the error is hidden.
- `get url` and `get title` are refused (exit 2).
- A read prints what it reads. `eval 'location.href'`, `snapshot -u`, `network requests` and `get cdp-url` print URLs, because that is their result: read only what the task needs (safety rule 3).

For a page that the task marks as non-sensitive, pass `--private=false` to the one command whose title, URL or error message you need. That mark never covers another tab.

Most harnesses start a new shell for every call. Start every call with the line that the examples start with, so that no command runs outside private mode or in the default session:

```text
export IWDP_SESSION=qa-login IWDP_PRIVATE=1     # one stable session name per agent
```

## Safety rules

These rules bind every run that uses this skill.

1. **No infrastructure writes.** Do not start, stop, restart or reconfigure servers, containers, compose stacks, proxies, routers, databases, runtime or devenv helpers, or the checkout. Use only the sign-in helpers that the task names. When a guard or wrapper refuses a command, stop and report the refusal; never change the call so that the guard passes. `iwdp-cli sim up` is no such write: it is how a run gets its simulator, also when that starts a VM; nor is serving your repository's page for the check.
2. **One bounded diagnostic budget, then stop.** When the same step fails twice (exit 124, exit 3 from a wait, exit 4, a refused sign-in), stop retrying. Run one health probe, `iwdp-cli session info`, record its output, and report the step as not run. Sleeping and retrying does not bring a stalled page back.
3. **Private mode hides titles and URLs; masking covers a few outputs only.** `cookies` hides cookie values; `network requests`, `network request` and HAR files print the values of secret headers and of password and token fields (query, form and JSON fields with such names) as `[masked]`; `storage` masks the values of password and token keys. Use `--show-values` or `--show-secrets` only when the task asks for a value. Page reads are not masked: `get text`, `get value`, `get html`, `read`, `eval`, `console`, `errors` and snapshot text print what the page holds, in private mode too, and a token in a field with an ordinary name prints in full. Read only the fields you need, and never run `get value` on a password or token field. Pass passwords with `fill --stdin`, never on the command line. Pass a signed or one-time link with `--url-file`, never on the command line. HAR files and state files are written with mode 0600 and hold live credentials: write them only into the evidence folder and delete them before the run ends. Do not dump the text, HTML, snapshot or screenshot of account, security or settings pages: read named fields only.
4. **Preserve and restore cookie owners.** Before you set or clear a cookie, save the current state (`state save`), and restore it (`state load`) at the end, so the cookies of the user or of another agent come back. Revoking a server-side session belongs to the sign-in helper that created it.
5. **Bounded shell calls and a known evidence folder.** Give every shell call a timeout. `iwdp-cli` has its own deadline (30 s, `--timeout` or `IWDP_TIMEOUT`; exit 124); other helpers need their own, such as `curl --max-time`. Write every file by absolute path into the evidence folder that the task names. Never search the filesystem (`find /`, `find ~`, `grep -r` over home): search only the checkout, the evidence folder and paths the task names.
6. **The device is shared.** Never stop, restart or kill an iwdp that another agent may use; `iwdp-cli skills get iwdp-device` has the rules for a restart. Reserve the device at the start of a run (`device reserve`) and release it at the end (`device release`). Use a distinct session name per agent, and never run `lease break` or `--takeover` on a page of another session without the approval of the device owner.

## Choose the device

Use the iOS Simulator unless the user names a physical device, also when an iPhone is plugged in. Other agents share it in turns: first run `iwdp-cli sim reserve`, which waits for them; on exit 3 or a tool timeout run it again until you get it: waiting is normal, never a reason to stop. Then `iwdp-cli sim up --url <your page>` starts the VM if needed and opens your own tab. Touch only tabs you opened; at the end `tab close` yours and `iwdp-cli sim release`, also after a failure. The device guide has the details and real touches. Reserve and use a physical device only when the user asks.

## Start a run

Reserve the device: on a physical device, page commands fail with exit 6 until the session's owner has a reservation. Local WebKitGTK pages need none. Every command of a block except the last ends with `|| exit "$?"`: when it fails, the block stops with its exit code, and the status line names the command. Look at the page or the session before you go on. Only a restore or a release runs after a failed step (the cookie block, the end of a run), and the block still fails.

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
mkdir -p -m 700 "$EVIDENCE" || exit "$?"   # EVIDENCE: the absolute folder the task names
iwdp-cli device reserve --owner "$IWDP_SESSION" --ttl 1h || exit "$?"
iwdp-cli tab list
```

<!-- doc-test: expect -->
```text
device reserve exit 0
hidden 0
```

`tab list` prints one line per tab: `*` before the current tab of your session, the tab id (`t1`, `t2`, ...) and whether the tab is debuggable; then the number of pages that are hidden because their device has no reservation of your owner.

**Find the tab of the task before the first action.** The listing cannot tell which tab shows the target. `tab find` can: it compares the whole URL, query and fragment included, inside each tab that can show it, and prints the id of the one tab that does. It only reads; your session then holds the lease of that tab, as after any read of it, and of no other tab. The target comes from a file, so a signed link is never on a command line.

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
tab="$(iwdp-cli tab find --url-file "$TARGET_FILE")" || exit "$?"   # TARGET_FILE: the file that holds the task's URL
iwdp-cli tab "$tab"                                                  # select it only after it was found
```

<!-- doc-test: expect -->
```text
tab exit 0
```

When `tab find` exits 7, no tab shows the target: act on no tab and report it. A tab that shows another page can hold the state of another task or of the user. Any other exit code is not a missing target: several tabs show it (exit 1), a tab could not be read (exit 1, or 4 when another session holds it), or the listing hides pages (exit 6: reserve the device). Only when the task hands you the device's one tab to work in, open the target there: `iwdp-cli --only-tab open --url-file "$TARGET_FILE"`. `--only-tab` names the tab of a listing with exactly one tab and no hidden page, and fails (exit 5) for any other listing.

## The agentic loop

Snapshot, act on refs, wait for the result, snapshot again.

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli snapshot -i
```

`snapshot -i` prints the interactive elements of the accessibility tree, each with a ref:

<!-- doc-test: expect -->
```text
- textbox "Email" [ref=e1]
- textbox "Password" [ref=e2]
- button "Sign in" [ref=e3]
```

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli fill @e1 'qa@example.com' || exit "$?"         # no click on a form that a fill left incomplete
iwdp-cli fill @e2 --stdin <<<"$QA_PASSWORD" || exit "$?"
iwdp-cli click @e3 || exit "$?"
iwdp-cli wait --text 'Signed in' || exit "$?"
iwdp-cli snapshot -i
```

The form is gone, so the new snapshot lists new elements with new refs:

<!-- doc-test: expect -->
```text
click exit 0 warnings 0 dialogs 0
wait exit 0
- heading "Account" [level=2, ref=e4]
- button "Delete draft" [ref=e5]
- link "Next page" [ref=e6]
```

- A ref (`@e3`) names one element of the current document. Refs stay valid across commands of your session. After a navigation, a dialog, or any change of the page, take a new snapshot and use only refs from it. Never guess a ref number.
- Targets can also be CSS selectors (`'#email'`) or locators (`text=Sign in`, `role=button`, `label=Email`, `placeholder=...`, `testid=...`). `find role button --name 'Sign in' click` finds and acts in one command.
- Before an action, the runtime repeats the actionability checks of that action until the action deadline (5 s by default). Every action checks that the element is attached and visible. `click`, `dblclick`, `tap`, `hover`, `check`, `uncheck` and `drag` also wait until it is stable and receives events at its center (not covered). `click`, `dblclick`, `tap`, `check`, `uncheck`, `select`, `fill`, `type` and `upload` check that it is enabled, and `fill` and `type` that it is editable. `fill` and `type` do not check coverage: a successful fill is no evidence that a user can reach the field. A click on a covered element fails (exit 1): find the covering element with `hit-test <x> <y>` at the element's center and report it as a finding. `--force` skips the checks; use it only when the task asks for it.
- `snapshot -c` keeps text lines, `-s '#form'` limits the snapshot to one element, `-u` adds link URLs, and `diff snapshot` shows what changed between your last two snapshots.

### Synthetic input

WebKit has no input domain, so every click, tap, key and scroll is a synthetic DOM event (`isTrusted` is false) that the page runtime dispatches in the order a browser would. `tap` and `swipe` send touch events, `press` emulates default actions (Enter submits, Tab moves focus). Code that requires trusted input or a user gesture (popups, clipboard, fullscreen, media, some payment and auth widgets) can behave differently from a real finger. State the input method in every report ("synthetic click", "synthetic tap"), and report a check that needs a real touch as not run.

### Waits

Every wait has a timeout (25 s by default, `--timeout` to change) and exits 3 when it expires. Wait for something you saw in a snapshot or for a URL, never with a bare `sleep`.

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli wait @e5 --state visible || exit "$?"
iwdp-cli wait --load networkidle || exit "$?"
iwdp-cli wait --fn 'document.querySelectorAll("button").length > 1'
```

`wait --url '**/second'` waits for a URL glob; an example follows under Navigation.

### Reading state

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli get text @e4 || exit "$?"
iwdp-cli is visible @e5 || exit "$?"
iwdp-cli get box @e5 || exit "$?"
iwdp-cli get styles @e5 display,visibility || exit "$?"
iwdp-cli hit-test 40 40 || exit "$?"
iwdp-cli read --outline || exit "$?"
iwdp-cli eval 'document.querySelectorAll("form").length'
```

- Judge visibility with `is visible`, `get box` and `get styles`, not with text content: hidden elements have text too.
- `eval` waits for a returned promise. A string, number or boolean prints raw on one line, other values as JSON. A thrown exception exits 1. `--page <tab>` evaluates in another tab. Only known flags are flags: an expression that starts with one `-` (`-1`) is the expression, and one that starts with `--` needs `--` before it (`eval -- '--x'`).

### Dialogs

WebKit cannot answer a dialog after it opens, so you arm the answer first. An unanswered `confirm()` returns false (dismiss), `alert()` returns at once, and `prompt()` returns null.

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli dialog accept || exit "$?"     # the next dialog is accepted
iwdp-cli click @e5                      # the page calls confirm(): dialogs 1
```

<!-- doc-test: expect -->
```text
dialog exit 0
click exit 0 warnings 0 dialogs 1
```

`dialog accept 'text'` answers a `prompt()`, `dialog dismiss` cancels, and `dialog default accept` changes the answer for unarmed dialogs. The counts of the action that met a dialog are the record of it; the dialog log (`dialog status`) holds the messages, which private mode does not print. A `beforeunload` dialog cannot be answered.

### Evidence

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli screenshot "$EVIDENCE/after-sign-in.png" || exit "$?"
iwdp-cli screenshot --selector @e5 "$EVIDENCE/delete-button.png" || exit "$?"
iwdp-cli screenshot --annotate "$EVIDENCE/annotated.png" || exit "$?"
iwdp-cli console --level warning || exit "$?"
iwdp-cli errors || exit "$?"
iwdp-cli network requests --filter /api/ || exit "$?"
iwdp-cli vitals
```

- Always give a screenshot an absolute path in the evidence folder. `--full-page` captures the whole document, `--selector` one element, `--annotate` draws the ref labels. Do not capture pages that show secrets unless the task allows it.
- `console` and `errors` print what the daemon captured since your session took the page (and up to 100 messages that WebKit replays). For new messages only, run `console --since <id>` with the last id that `console` printed.
- `network requests` lists the requests since your session took the page; `network request <id> --body` shows one request.

### Navigation, tabs and frames

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli click @e6 || exit "$?"         # a link to another page
iwdp-cli wait --url '**/second' || exit "$?"
iwdp-cli back || exit "$?"
iwdp-cli forward || exit "$?"
iwdp-cli reload
```

`open`, `back`, `forward` and `reload` wait for the load event (`--wait domcontentloaded` or `none` to change). `open --url-file <path>` reads the URL from a file. `tab new <url>` opens a tab with `window.open`; open no tab that the task did not ask for. `frame @eN` moves your later commands into a same-origin iframe, and `frame main` moves them back. Before the first action on another tab, find it with `tab find` as in "Start a run".

## Cookies and storage

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli cookies || exit "$?"
iwdp-cli storage local || exit "$?"
iwdp-cli state save "$EVIDENCE/state-before.json" || exit "$?"   # change nothing without a saved state
iwdp-cli cookies set session_hint abc123 --http-only --same-site Lax --expires 0
changed=$?                                                       # restore also after a failed change
iwdp-cli state load "$EVIDENCE/state-before.json" || exit "$?"   # keep the file when the restore fails
rm -f -- "$EVIDENCE/state-before.json" || exit "$?"
[ "$changed" = 0 ]
```

- `cookies` prints names and attributes with hidden values (`name=(hidden)`). `cookies set` reads the cookie back and exits 1 when WebKit stored something else; `--expires 0` makes a session cookie.
- `state save` writes the cookies, localStorage and sessionStorage of the current origin (mode 0600); `state load` replaces them with the file's, on a page of the same origin. This is the restore step of safety rule 4, and it runs also after a failed change. When `state load` fails, the block keeps the file: run `state load` again, and delete the file before the run ends.

## End a run

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli close
closed=$?                                  # release the device also after a failed close
iwdp-cli device release --owner "$IWDP_SESSION" || exit "$?"
[ "$closed" = 0 ]
```

`close` ends your session: it releases your pages, removes your routes, headers, overrides and dialog answers, and revokes your CDP URLs. It does not close the Safari tab, does not touch other sessions, and does not undo forms you submitted or sign-ins you made.

## Errors and exit codes

| Exit | Meaning | What to do |
|---|---|---|
| 0 | done | |
| 1 | the command failed | for `tab find`: several tabs show the target, or a tab could not be read |
| 2 | usage error, before any connection | `iwdp-cli <command> --help` |
| 3 | a wait timed out | the expected state did not come; record it |
| 4 | the page is leased by another session, or its connection was lost | wait (`--wait-lease 30s`) or ask; never take over without the device owner's approval |
| 5 | no device or page | check the device and the open tab (`iwdp-cli skills get iwdp-device`) |
| 6 | device reservation missing | reserve the device as in "Start a run" |
| 7 | `tab find`: no tab shows the target | act on no tab; report it |
| 124 | the command deadline passed | one health probe, then safety rule 2 |

In private mode the status line on stderr gives the code and this meaning. When the same command fails the same way again and its page is non-sensitive, `--private=false` prints the message, which names the cause.

## Known limits

- Only one inspector connection per page. The daemon holds it; another tool that attaches (Safari Web Inspector on a Mac, a hand-written WebSocket client) takes the page from the daemon. Never write your own WebSocket client.
- A command names another page with `--page` (a tab id), after `tab find` found that tab. A trailing `ws://` argument, or `IWDP_WS_URL` set without `--page`, is a usage error (exit 2).
- Not possible on iOS WebKit: trusted input, `set viewport` and `set device` (the viewport is the device's), PDF, downloads, video recording, browser contexts (every session shares one cookie jar), native pickers, `beforeunload` dialogs, cross-origin iframes.
- `hover --css` and the commands that need `CSS.enable` work only on WebKitGTK.
