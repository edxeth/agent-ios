---
name: iwdp
description: Drive and debug Safari on a real iPhone or iPad or in an iOS Simulator on macOS over SSH, and local WebKitGTK pages, from Linux with iwdp-cli. Use whenever a task involves iOS, iPhone or iPad (for example checking that web changes look right on iOS), iOS Safari, Web Inspector, iwdp or mobile Safari bugs. Prefer it to desktop browser emulation for iOS.
---

# iOS Safari with iwdp-cli

`iwdp-cli` drives Safari pages in an iOS Simulator or on a USB-connected iPhone or iPad, and pages of a local WebKitGTK browser, from the shell. The command names and flags follow agent-browser where WebKit can support them.

`iwdp-cli <command> --help` prints the flags of a command and never connects to a page; `iwdp-cli network --help` (any group word) lists the commands of that group. Load another guide when the task needs it:

| Load | When you need |
|---|---|
| `iwdp-cli skills get iwdp-device` | A physical device or local WebKitGTK pages; on the simulator a delegate, a lost turn, a refused `sim up`, real touches and the keyboard; a device or page list that does not answer; the rules for a restart of iwdp |
| `iwdp-cli skills get iwdp-devtools` | A cookie or storage change, HAR files, request routes, media and network emulation, traces, CPU profiles, init scripts |
| `iwdp-cli skills get iwdp-cdp` | A CDP client: agent-browser, Puppeteer, Playwright, chrome-devtools-mcp |

## Private mode

A transcript that other parties read must not hold a page's title, URL or origin, a device, a session name or the text of an error: each can carry a token or name a person. With `IWDP_PRIVATE=1` (or `--private`), iwdp-cli prints none of that:

- **Stdout holds only the result that you asked for**, also with `--json`: what a read reads (`console` and `errors` without the URL and stack of their source), the tab ids of `tab list` and `tab find`, and short summaries of `session info`, `status` and the `sim` commands, among them the id of the tab that `sim up` opened. A command that only acts prints nothing there.
- **Stderr holds exactly one status line for every run**, help included: `<command> exit 0`, with `warnings <n> dialogs <n>` after an element or key action; or for a failure `<command> exit <code>: <meaning>`. The message of the error is hidden.
- `get url` and `get title` are refused (exit 2).
- A read prints what it reads: `eval 'location.href'`, `snapshot -u`, `network requests` and `get cdp-url` print URLs. Read only what the task needs (safety rule 3).

For a page that the task marks as non-sensitive, pass `--private=false` to the one command whose title, URL or error message you need, for example when it fails the same way twice and you need the cause. That mark never covers another tab.

Most harnesses start a new shell for every call, so start every call with the first line of the examples. Its `qa-login` is a placeholder: replace it with a session name of your own and keep that name for the whole run, so that no command runs outside private mode or in another agent's session.

Keep the `|| exit "$?"` of the examples: a failed command stops its call with its exit code. Look at the page or the session before you go on. Only a cleanup step (a restore, a release) runs after a failed step, and the call still fails.

## Safety rules

These rules bind every run that uses this skill.

1. **No infrastructure writes.** Do not start, stop, restart or reconfigure servers, containers, compose stacks, proxies, routers, databases, runtime or devenv helpers, or the checkout. Use only the sign-in helpers that the task names. When a guard or wrapper refuses a command, stop and report the refusal; never change the call so that the guard passes. `iwdp-cli sim up` is no such write: it is how a run gets its simulator, also when that starts a VM; nor is serving your repository's page for the check.
2. **One bounded diagnostic budget, then stop.** When the same step fails twice (exit 124, exit 3 from a wait, exit 4, a refused sign-in), stop retrying. Run one health probe, `iwdp-cli session info`, record its output, and report the step as not run. Sleeping and retrying does not bring a stalled page back. `sim reserve` is no such step: repeat it until it succeeds.
3. **Private mode hides titles and URLs; masking covers a few outputs only.** `cookies` hides cookie values; `network requests`, `network request` and HAR files print the values of secret headers and of password and token fields (query, form and JSON fields with such names) as `[masked]`; `storage` masks the values of password and token keys. Use `--show-values` or `--show-secrets` only when the task asks for a value. Page reads are not masked: `get text`, `get value`, `get html`, `read`, `eval`, `console`, `errors` and snapshot text print what the page holds, in private mode too, and a token in a field with an ordinary name prints in full. Read only the fields you need, and never run `get value` on a password or token field. Pass passwords with `fill --stdin`, never on the command line. Pass a signed or one-time link with `--url-file`, never on the command line. HAR files and state files are written with mode 0600 and hold live credentials: write them only into the evidence folder and delete them before the run ends. Do not dump the text, HTML, snapshot or screenshot of account, security or settings pages: read named fields only.
4. **Preserve and restore cookie owners.** Before you set or clear a cookie, save the current state (`state save`), and restore it (`state load`) at the end, so the cookies of the user or of another agent come back; `iwdp-cli skills get iwdp-devtools` has the block. Revoking a server-side session belongs to the sign-in helper that created it.
5. **Bounded shell calls and a known evidence folder.** Give every shell call a timeout. `iwdp-cli` has its own deadline (30 s, `--timeout` or `IWDP_TIMEOUT`; exit 124); other helpers need their own, such as `curl --max-time`. Write every file by absolute path into the evidence folder that the task names. Never search the filesystem (`find /`, `find ~`, `grep -r` over home): search only the checkout, the evidence folder and paths the task names.
6. **The device is shared.** Never stop, restart or kill an iwdp that another agent may use; `iwdp-cli skills get iwdp-device` has the rules for a restart. Reserve the device before your first page command and release it at the end, also after a failure: the simulator with `sim reserve` and `sim release`, a physical device with `device reserve` and `device release`. Use a distinct session name per agent, and never run `lease break` or `--takeover` on a page of another session without the approval of the device owner.

## Start and end a run

Use the iOS Simulator unless the user names a physical device, also when an iPhone is plugged in; for a physical device or local WebKitGTK pages, `iwdp-cli skills get iwdp-device` has the start and end of a run. Agents share the simulator in turns, so every run does these steps:

1. **Serve your page first, before you reserve.** Serve it on a random free port of this host (not a common one such as 8000, 8080 or 8765: other agents and programs use them) in a background job that outlives your tool call, and check that `http://127.0.0.1:<port>/` answers with your page. If you cannot serve it, report that without reserving.
2. **Take your turn and open your own tab.** `sim reserve` waits while another agent holds the simulator. Exit 3 or a tool timeout means it is still theirs: run the call again until it succeeds, and run no other `iwdp-cli` command on the simulator meanwhile. Waiting is normal, never a reason to stop, skip the check or ask the user. The agent that drives the simulator reserves it, not an agent that waits for a delegate. `sim up --url` then starts the Mac or VM if needed, which can take 15 minutes, and opens your URL in a new tab; 10.0.2.2 is this host as the simulator sees it.

<!-- doc-test: skip simulator: needs a Mac over SSH and its configuration -->
```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli sim reserve || exit "$?"                # exit 3: another agent's turn; run it again
iwdp-cli sim up --url "http://10.0.2.2:41873/"   # 41873: the port of your server
```

3. **Select the tab that `sim up` printed** (`new tab t1`) before any page command: a command without it may land on another agent's tab. When it prints no tab id, do what its line says, and stop when that fails. Never select, navigate, close or `lease break` a tab that you did not open.

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli tab t1                                  # the id that sim up printed
```

<!-- doc-test: expect -->
```text
tab exit 0
```

4. **Work in your tab**, as the sections below show.
5. **End your turn, also after a failure.** Close your tab, then release the simulator, which also closes your session. When `tab close` fails, leave the tab and still release. Neither undoes forms you submitted or sign-ins you made.

<!-- doc-test: skip simulator: needs a Mac over SSH and its configuration -->
```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli tab close
closed=$?                                        # release also when Safari keeps the tab
iwdp-cli sim release || exit "$?"
[ "$closed" = 0 ]
```

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
- An action waits up to 5 s until its element is attached, visible, stable, enabled and not covered at its center, as far as the action needs each. `fill` and `type` do not check coverage: a successful fill is no evidence that a user can reach the field. A click on a covered element fails (exit 1): find the covering element with `hit-test <x> <y>` at the element's center and report it as a finding. `--force` skips the checks; use it only when the task asks for it.
- `snapshot -c` keeps text lines, `-s '#form'` limits the snapshot to one element, `-u` adds link URLs, and `diff snapshot` shows what changed between your last two snapshots.

### Synthetic input

Every click, tap, key and scroll is a synthetic DOM event (`isTrusted` is false). Code that requires trusted input or a user gesture (popups, clipboard, fullscreen, media, some payment and auth widgets) can behave differently from a real finger. State the input method in every report ("synthetic click", "synthetic tap"). For a check that needs a real touch, use `sim tap`, `sim swipe` and `sim type` on the simulator (device guide); on a physical device report it as not run.

### Waits

A wait exits 3 when its timeout (25 s, `--timeout`) expires. Wait for something you saw in a snapshot or for a URL, never with a bare `sleep`.

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli wait @e5 --state visible || exit "$?"
iwdp-cli wait --load networkidle || exit "$?"
iwdp-cli wait --fn 'document.querySelectorAll("button").length > 1'
```

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
- `eval` waits for a returned promise, and a thrown exception exits 1. An expression that starts with `--` needs `--` before it (`eval -- '--x'`).

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

`dialog accept 'text'` answers a `prompt()`, `dialog dismiss` cancels, and `dialog default accept` changes the answer for unarmed dialogs. The counts of the action that met a dialog are the record of it; the dialog log (`dialog status`) holds the messages, which private mode does not print.

### Evidence

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
mkdir -p -m 700 "$EVIDENCE" || exit "$?"   # EVIDENCE: the absolute folder the task names
iwdp-cli screenshot "$EVIDENCE/after-sign-in.png" || exit "$?"
iwdp-cli screenshot --selector @e5 "$EVIDENCE/delete-button.png" || exit "$?"
iwdp-cli screenshot --annotate "$EVIDENCE/annotated.png" || exit "$?"
iwdp-cli console --level warning || exit "$?"
iwdp-cli errors || exit "$?"
iwdp-cli network requests --filter /api/ || exit "$?"
iwdp-cli vitals
```

- `screenshot --full-page` captures the whole document. Do not capture pages that show secrets unless the task allows it.
- `console`, `errors` and `network requests` print what the daemon captured since your session took the page. For new messages only, run `console --since <id>` with the last id that `console` printed; `network request <id> --body` shows one request.

### Navigation, tabs and frames

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
iwdp-cli click @e6 || exit "$?"         # a link to another page
iwdp-cli wait --url '**/second' || exit "$?"
iwdp-cli back || exit "$?"
iwdp-cli forward || exit "$?"
iwdp-cli reload
```

`open`, `back`, `forward` and `reload` wait for the load event. Open no tab (`tab new`) that the task did not ask for. `frame @eN` moves your later commands into a same-origin iframe, and `frame main` moves them back. Before the first action on another tab, find it with `tab find`, then select it (`tab <id>`, or `--page <id>` for one command).

## Errors and exit codes

| Exit | Meaning | What to do |
|---|---|---|
| 0 | done | |
| 1 | the command failed | for `tab find`: several tabs show the target, or a tab could not be read |
| 2 | usage error, before any connection | `iwdp-cli <command> --help` |
| 3 | a wait timed out | record it; `sim reserve`: run it again |
| 4 | the page is leased by another session, or its connection was lost | wait (`--wait-lease 30s`) or ask; never take over without the device owner's approval |
| 5 | no device or page | select your tab; else check the device (`iwdp-cli skills get iwdp-device`) |
| 6 | device reservation missing or another's | reserve it; simulator: `sim reserve` |
| 7 | no tab shows the target (`tab find`, `sim up`) | `tab find`: act on no tab; report it. `sim up`: check that your server answers; serve your page on a URL of your own |
| 124 | the command deadline passed | one health probe, then safety rule 2 |

## Known limits

- Only one inspector connection per page. The daemon holds it; another tool that attaches (Safari Web Inspector on a Mac, a hand-written WebSocket client) takes the page from the daemon. Never write your own WebSocket client.
- Not possible on iOS WebKit: trusted input, `set viewport` and `set device` (the viewport is the device's), PDF, downloads, video recording, browser contexts (every session shares one cookie jar), native pickers, `beforeunload` dialogs, cross-origin iframes.
