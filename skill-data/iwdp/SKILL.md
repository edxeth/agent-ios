---
name: iwdp
description: Drive and debug real iPhone or iPad Safari pages (and local WebKitGTK pages) from Linux with iwdp-cli. Snapshot with @eN refs, synthetic actions, waits, dialogs, tabs, console, network, cookies, storage, screenshots, traces, and a CDP endpoint for agent-browser, Puppeteer and Playwright. Use when the user mentions iOS Safari, iPhone Safari, Web Inspector, iwdp, ios-webkit-debug-proxy, WebKit Inspector Protocol, mobile Safari bugs, real-device Safari screenshots, or asks to drive a connected iPhone browser.
---

# iOS Safari with iwdp-cli

`iwdp-cli` drives Safari pages on a USB-connected iPhone or iPad, and pages of a local WebKitGTK browser. It speaks the WebKit Inspector Protocol, not the Chrome DevTools Protocol (CDP). The command names and flags follow agent-browser where WebKit can support them.

```text
iPhone Safari -> ios_webkit_debug_proxy (iwdp) -> iwdp daemon -> iwdp-cli, CDP clients
```

The **daemon** is one background process per user. The first command starts it. It holds the only inspector connection to each page, so refs, waits and captures survive from one command to the next. Every `iwdp-cli` command is a short client of the daemon.

Run `iwdp-cli skills get iwdp` to print this skill as it matches the installed binary. `iwdp-cli <command> --help` prints the flags of a command and never connects to a page; `iwdp-cli network --help` (any group word) lists the commands of that group.

## Safety rules

These rules bind every run that uses this skill.

1. **No infrastructure writes.** Do not start, stop, restart or reconfigure servers, containers, compose stacks, proxies, routers, databases, runtime or devenv helpers, or the checkout. Use only the sign-in helpers that the task names. When a guard or wrapper refuses a command, stop and report the refusal; never change the call so that the guard passes.
2. **Preflight, then one final verdict.** Before the first check, confirm three things with one bounded call each: the origin answers, the build under test is live (a fingerprint, or the changed rule or text is present), and the sign-in state is the one the task states. When one differs, the check is BLOCKED; do not work around it. End the run with exactly one verdict line, `VERDICT: PASS`, `VERDICT: FAIL` or `VERDICT: BLOCKED`, and one PASS, FAIL or NOT RUN row per check. A check that never reached the behavior under test (for example an HTTP 5xx before the logic ran) is NOT RUN, not FAIL.
3. **One bounded diagnostic budget, then BLOCKED.** When the same step fails twice (exit 124, exit 3 from a wait, exit 4, a refused sign-in), stop retrying. Run one health probe, the filtered `session info` below, record its output, and mark the check NOT RUN. When no check can run, report BLOCKED. Sleeping and retrying does not bring a stalled page back.
4. **Masking covers a few outputs only; filter the rest.** By default `cookies` hides cookie values in every form; `network requests`, `network request` and HAR files (`network har stop`) print the values of secret headers and of query, form and JSON fields whose names mark password and token fields as `[masked]`; `storage` masks the values of password and token keys. Use `--show-values` or `--show-secrets` only when the task asks for a value, and never on a shared transcript. Page reads are not masked: `get text`, `get value`, `get html`, `read`, `eval`, `console`, `errors` and snapshot text print what the page holds, and a token in a field with an ordinary name prints in full. Read only the fields you need, with an allowlisted read or a filter inside the command, and never run `get value` on a password or token field. Pass passwords with `fill --stdin`, never on the command line. HAR files (`network har stop`) and state files (`state save`) are written with mode 0600 and hold live credentials (a state file keeps every value): write them only into the evidence folder and delete them before the run ends. Do not dump the text, HTML, snapshot or screenshot of account, security or settings pages (two-factor setup keys, recovery codes, API tokens): read named fields only.
5. **Preserve and restore cookie owners.** Before you set or clear a cookie, save the current state (`state save`), and restore it (`state load`) at the end, so the cookies of the user or of another agent come back. Revoking a server-side session belongs to the sign-in helper that created it; report every session you created and whether it was revoked.
6. **Bounded shell calls and known evidence folders.** Give every shell call a timeout. `iwdp-cli` has its own deadline (30 s, `--timeout` or `IWDP_TIMEOUT`, as `10s` or `10`; exit 124), but other helpers need their own bound, such as `curl --max-time`; `timeout` is part of GNU coreutils, which macOS does not have by default. Write every file by absolute path into the evidence folder that the task names. Never search the filesystem (`find /`, `find ~`, `grep -r` over home or `~/.pi`): search only the checkout, the evidence folder and paths the task names.
7. **The device is shared.** Never stop, restart or kill an iwdp that another agent may use (`pkill`, or a script that stops or restarts it). The daemon never stops a running iwdp; `iwdp-cli restart-iwdp` stops only an iwdp that iwdp-cli started and recorded, and refuses any other. Stop an iwdp by hand only if you started it yourself and nobody else uses it; otherwise send no signal and ask whoever started it, or the user. The stop is `scripts/iwdp-owner-check.sh --terminate <record>` from a checkout of the iwdp-cli repository, which sends SIGTERM only when the identity check reports `verified`, then `scripts/iwdp-owner-check.sh <record>` until it reports `gone`, as docs/device-runbook.md (Stopping an iwdp by hand) in that repository describes. Reserve the device at the start of a run (`device reserve`) and release it at the end (`device release`). Use a distinct `--session` name per agent.

**Private mode is the default.** Page titles and URLs are private unless the task marks a page as non-sensitive. Discovery (`tab list`), reservation (`device reserve`, `device release`), diagnostics (`session info`, device checks), tab selection (`tab <id>`) and page actions (`open`, `navigate`, `back`, `forward`, `reload`, `click`, `fill`, `type`, `press`, `wait`, `dialog`) print page titles, URLs, page keys with the device identifier, other sessions' names, dialog messages and warnings that name pages. In private mode none of their own output reaches the transcript: each example prints the exact status and only allowlisted facts, named fields of `--json` output or counts. A failure of the command and a failure of the filter that reads its output are reported separately, each under its own name with its exact status, and either one ends the shell with that status.

Every `iwdp-cli` command that this guide runs discards stderr, because an error can name the page (Errors and exit codes), and prints the exact status of the command. Tab selection and page actions run through `iw`, the other page commands through `iwr`. Every block that uses one of them defines it on its first lines, because each shell call starts a new shell; to run a command that the text names outside a block, copy the definition into the same shell call.

- `iw` runs `iwdp-cli` with `--json` and its arguments, discards stderr and keeps the JSON result in a variable, never printed. It prints `<command> exit 0`. When the result is that of an element or key action (`click`, `fill`, `type`, `press`, `find ... click` and the other actions with the members `trusted`, `steps`, `warnings` and `dialogs`), python3 adds `warnings <n> dialogs <n>`: the lengths of the `warnings` and `dialogs` arrays, never their text. A warning can name another page, and a dialog holds the page's message. Counts come only from these arrays, never from text lines, which page text (an element id, a dialog message) can forge. The results of `open`, `navigate`, `back`, `forward`, `reload`, `wait`, `tab` and `dialog` have no such arrays (their warnings are text only), so `iw` claims no count for them. A failure of the filter prints `<command> exit 0`, then `filter exit <status>`, and ends the shell with that status.
- `iwr` runs `iwdp-cli` with its arguments, discards stderr, prints stdout (page content, which safety rule 4 governs) and then `<command> exit 0`.
- `iw` refuses every word that is `--json` or `--content-boundaries`, or starts with `--json=` or `--content-boundaries=`, at any position, also after a `--`: it prints a usage line, runs nothing and ends the shell with status 2, because such a word would run the action in text mode or change the shape of its JSON. `iw` parses none of your words to find them, so a session name, page value or argument that equals such a word cannot pass through `iw`.
- Both helpers refuse every word that is `-h` or `--help`, at any position, also after a `--`: they print a usage line, run nothing and end the shell with status 2, because the CLI would print help instead of running the command; read a command's help with `iwdp-cli <command> --help`, without a helper.
- `<command>` is the first word after the global options (`--page`, `--session`, `--timeout`, `--json` and the others of `iwdp-cli --help`) and their values, which neither helper prints.
- A failure of either prints `<command> exit <status>` and ends the shell with that status, also with `set -e`.

Page-content reads (`snapshot`, `get`, `is`, `eval`, `console`, `errors`) follow safety rule 4; snapshot text holds no page title or URL. The non-sensitive exception covers only `get title`, `get url` and `open` on the page that the task marks.

The health probe of rule 3 reads the JSON form of `session info` and prints only the lease, epoch and connection state of each page. It relies on the deadline of `iwdp-cli` (30 s, exit 124), so it needs no `timeout` command.

```bash
probe="$(iwdp-cli session info --json 2>/dev/null)" || { s=$?; echo "session info exit $s"; exit "$s"; }
printf '%s\n' "$probe" | python3 -c 'import json,sys; [print("page", i, "lease", p.get("lease"), "epoch", p.get("epoch"), "connection", p.get("state")) for i, p in enumerate(json.load(sys.stdin).get("pages") or [], 1)]' || { s=$?; echo "filter exit $s"; exit "$s"; }
echo "session info exit 0"
```

<!-- doc-test: expect -->
```text
session info exit 0
```

## Start a run

Set a session name that is unique to your agent, and the absolute evidence folder of the task. Reserve the device: on a physical device, page commands fail with exit 6 until the session's owner has a reservation. Local WebKitGTK pages need no reservation. List the tabs only with the filtered listing below, never with plain `tab list`: that prints the title and URL of every tab, also of tabs that other sessions hold, and their session names.

```bash
export IWDP_SESSION=qa-login            # one stable name per agent; --session works too
mkdir -p -m 700 "$EVIDENCE"             # EVIDENCE: the absolute folder the task names
s=0; iwdp-cli device reserve --owner "$IWDP_SESSION" --ttl 1h >/dev/null 2>&1 || s=$?; echo "device reserve exit $s"; [ "$s" = 0 ] || exit "$s"
tabs="$(iwdp-cli tab list --json 2>/dev/null)" || { s=$?; echo "tab list exit $s"; exit "$s"; }
printf '%s\n' "$tabs" | python3 -c '
import json, sys
d = json.load(sys.stdin)
for t in d.get("tabs") or []:
    print("*" if t.get("current") else " ", t.get("id"), "debuggable" if t.get("debuggable") else "not-debuggable")
hidden = d.get("hidden") or 0
print("hidden", hidden)
if hidden:
    print("Pages of physical devices without a reservation of your owner are not listed: reserve the device as in Start a run, or ask the device owner.")
' || { s=$?; echo "filter exit $s"; exit "$s"; }
echo "tab list exit 0"
```

<!-- doc-test: expect -->
```text
device reserve exit 0
hidden 0
tab list exit 0
```

The listing prints one line per tab: `*` before the current tab of your session, the tab id (`t1`, `t2`, ...) and whether the tab is debuggable. Then it prints how many pages are hidden, and when some are, why. It never prints titles, URLs, labels, page keys, devices or the sessions that hold tabs, and it cannot tell which tab shows the task's target: `tab list` drops the query and fragment of every URL. Read the title or URL of a page that the task marks as non-sensitive with `get title` or `get url` on that page. A page that another session holds fails at once with exit 4 and names the session; wait for it (`--wait-lease 30s`) or ask, and never run `lease break` on another agent's page.

**The target check.** Before the first action on a tab, run this check on it; every later example of this guide acts only on the tab that it selected. Its first command on the chosen tab is a read-only `eval` with `--page` that compares the full URL in the page with the task's target, which the same shell call reads from its file. The check prints the status of `eval` and `target true`, or `target false` and ends the shell with status 1: then act on no tab, and report it. Only after `target true` does it select the tab; then use the selected tab, or pass `--page` with the same tab to one command. It never opens or navigates the chosen tab: a tab that shows another page can hold the state of another task or of the user. Check another tab the same way before you act on it.

```bash
iw() { local a=("$@") c out n s=0 x; for x in "$@"; do case "$x" in --json|--json=*|--content-boundaries|--content-boundaries=*) echo "iw: usage: iw sets the output format itself; remove --json and --content-boundaries"; exit 2;; -h|--help) echo "iw: usage: iw runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; out="$(iwdp-cli --json "$@" 2>/dev/null)" || { s=$?; echo "$c exit $s"; exit "$s"; }; n="$(printf '%s\n' "$out" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" warnings %d dialogs %d" % (len(d.get("warnings") or []), len(d.get("dialogs") or [])) if isinstance(d, dict) and "trusted" in d and "steps" in d else "")')" || { s=$?; echo "$c exit 0"; echo "filter exit $s"; exit "$s"; }; echo "$c exit 0$n"; }
TARGET="$(cat -- "$TARGET_FILE")" && [ -n "$TARGET" ] || { echo 'stopped: the target file is missing or empty'; exit 1; }
export TARGET   # never print it
lit="$(python3 -c 'import json,os; print(json.dumps(os.environ["TARGET"]))')" || { s=$?; echo "python3 exit $s"; exit "$s"; }
tab=t1                                  # the tab you chose from the listing
s=0; match="$(iwdp-cli --page "$tab" eval "location.href === new URL($lit).href" 2>/dev/null)" || s=$?; echo "eval exit $s"; [ "$s" = 0 ] || exit "$s"
[ "$match" = true ] || { echo "target false"; exit 1; }
echo "target true"
iw tab "$tab"                           # select it only after the check
```

<!-- doc-test: expect -->
```text
eval exit 0
target true
tab exit 0
```

## The agentic loop

Snapshot, act on refs, wait for the result, snapshot again. The examples act on the tab that the target check selected.

```bash
iwr() { local a=("$@") c s=0 x; for x in "$@"; do case "$x" in -h|--help) echo "iwr: usage: iwr runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; iwdp-cli "$@" 2>/dev/null || s=$?; echo "$c exit $s"; [ "$s" = 0 ] || exit "$s"; }
iwr snapshot -i
```

`snapshot -i` prints the interactive elements of the accessibility tree, each with a ref:

<!-- doc-test: expect -->
```text
- textbox "Email" [ref=e1]
- textbox "Password" [ref=e2]
- button "Sign in" [ref=e3]
```

```bash
iw() { local a=("$@") c out n s=0 x; for x in "$@"; do case "$x" in --json|--json=*|--content-boundaries|--content-boundaries=*) echo "iw: usage: iw sets the output format itself; remove --json and --content-boundaries"; exit 2;; -h|--help) echo "iw: usage: iw runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; out="$(iwdp-cli --json "$@" 2>/dev/null)" || { s=$?; echo "$c exit $s"; exit "$s"; }; n="$(printf '%s\n' "$out" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" warnings %d dialogs %d" % (len(d.get("warnings") or []), len(d.get("dialogs") or [])) if isinstance(d, dict) and "trusted" in d and "steps" in d else "")')" || { s=$?; echo "$c exit 0"; echo "filter exit $s"; exit "$s"; }; echo "$c exit 0$n"; }
iwr() { local a=("$@") c s=0 x; for x in "$@"; do case "$x" in -h|--help) echo "iwr: usage: iwr runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; iwdp-cli "$@" 2>/dev/null || s=$?; echo "$c exit $s"; [ "$s" = 0 ] || exit "$s"; }
iw fill @e1 'qa@example.com'
iw fill @e2 --stdin <<<"$QA_PASSWORD"
iw click @e3
iw wait --text 'Signed in'
iwr snapshot -i
```

The form is gone, so the new snapshot lists new elements with new refs:

<!-- doc-test: expect -->
```text
- heading "Account" [level=2, ref=e4]
- button "Delete draft" [ref=e5]
- link "Next page" [ref=e6]
```

- A ref (`@e3`) names one element of the current document. Refs stay valid across commands of your session. After a navigation, a dialog, or any change of the page, take a new snapshot and use only refs from it. Never guess a ref number.
- Targets can also be CSS selectors (`'#email'`) or locators (`text=Sign in`, `role=button`, `label=Email`, `placeholder=...`, `testid=...`). `find role button --name 'Sign in' click` finds and acts in one command; run it through `iw`.
- Before an action on an element, the runtime repeats the actionability checks of that action until the action deadline (5 s by default). The checks differ per action (the table in docs/guide.md, step 4 of the agentic loop). Every action on an element checks that the element is attached and visible. `click`, `dblclick`, `tap`, `hover`, `check`, `uncheck` and `drag` also wait until it is stable and receives events at its center (not covered). `click`, `dblclick`, `tap`, `check`, `uncheck`, `select`, `fill`, `type` and `upload` check that it is enabled, and `fill` and `type` that it is editable. `fill` and `type` do not check stability or coverage, so they can change a field that another element covers: a successful fill is no evidence that a user can reach the field. `hover` and `drag` do not check that the element is enabled. A click on a covered element fails and names the covering element: report it as a finding. `--force` skips the checks; use it only when the task asks for it.
- `snapshot -c` keeps text lines, `-s '#form'` limits the snapshot to one element, `-u` adds link URLs, and `diff snapshot` shows what changed between your last two snapshots.

### Synthetic input

WebKit has no input domain, so every click, tap, key and scroll is a synthetic DOM event (`isTrusted` is false) that the page runtime dispatches in the order a browser would. `tap` and `swipe` send touch events, `press` emulates default actions (Enter submits, Tab moves focus). Code that requires trusted input or a user gesture (popups, clipboard, fullscreen, media, some payment and auth widgets) can behave differently from a real finger. State the input method in every report ("synthetic click", "synthetic tap"), and report a check that needs a real touch as NOT RUN.

### Waits

Every wait has a timeout (25 s by default, `--timeout` to change) and exits 3 when it expires. Wait for something you saw in a snapshot or for a URL, never with a bare `sleep`.

```bash
iw() { local a=("$@") c out n s=0 x; for x in "$@"; do case "$x" in --json|--json=*|--content-boundaries|--content-boundaries=*) echo "iw: usage: iw sets the output format itself; remove --json and --content-boundaries"; exit 2;; -h|--help) echo "iw: usage: iw runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; out="$(iwdp-cli --json "$@" 2>/dev/null)" || { s=$?; echo "$c exit $s"; exit "$s"; }; n="$(printf '%s\n' "$out" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" warnings %d dialogs %d" % (len(d.get("warnings") or []), len(d.get("dialogs") or [])) if isinstance(d, dict) and "trusted" in d and "steps" in d else "")')" || { s=$?; echo "$c exit 0"; echo "filter exit $s"; exit "$s"; }; echo "$c exit 0$n"; }
iw wait @e5 --state visible
iw wait --url '**/login'
iw wait --load networkidle
iw wait --fn 'document.querySelectorAll("button").length > 1'
```

### Reading state

```bash
iwr() { local a=("$@") c s=0 x; for x in "$@"; do case "$x" in -h|--help) echo "iwr: usage: iwr runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; iwdp-cli "$@" 2>/dev/null || s=$?; echo "$c exit $s"; [ "$s" = 0 ] || exit "$s"; }
iwr get text @e4
iwr get value @e1
iwr is visible @e5
iwr get box @e5
iwr get styles @e5 display,visibility
iwr hit-test 40 40
iwr read --outline
iwr eval 'document.querySelectorAll("form").length'
```

- Judge visibility with `is visible`, `get box` and `get styles`, not with text content: hidden elements have text too.
- `hit-test <x> <y>` prints the element at a viewport point, so you can show what a tap at that point reaches.
- `eval` waits for a returned promise. A string, number or boolean prints raw on one line, other values as JSON. A thrown exception prints its message and stack and exits 1. `--page <tab>` evaluates in another tab. Only known flags are flags: an expression that starts with one `-` (`-1`, `-window.scrollY`) is the expression, and one that starts with `--` needs `--` before it (`eval -- '--x'`).

### Dialogs

WebKit cannot answer a dialog after it opens, so you arm the answer first. An unanswered `confirm()` returns false (dismiss), `alert()` returns at once, and `prompt()` returns null.

```bash
iw() { local a=("$@") c out n s=0 x; for x in "$@"; do case "$x" in --json|--json=*|--content-boundaries|--content-boundaries=*) echo "iw: usage: iw sets the output format itself; remove --json and --content-boundaries"; exit 2;; -h|--help) echo "iw: usage: iw runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; out="$(iwdp-cli --json "$@" 2>/dev/null)" || { s=$?; echo "$c exit $s"; exit "$s"; }; n="$(printf '%s\n' "$out" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" warnings %d dialogs %d" % (len(d.get("warnings") or []), len(d.get("dialogs") or [])) if isinstance(d, dict) and "trusted" in d and "steps" in d else "")')" || { s=$?; echo "$c exit 0"; echo "filter exit $s"; exit "$s"; }; echo "$c exit 0$n"; }
iw dialog accept                        # the next dialog is accepted
iw click @e5                            # the page calls confirm(): dialogs 1
```

<!-- doc-test: expect -->
```text
dialog exit 0
click exit 0 warnings 0 dialogs 1
```

`dialog accept 'text'` answers a `prompt()`, `dialog dismiss` cancels, and `dialog default accept` changes the answer for unarmed dialogs. Run `dialog status` through `iw` like the other dialog commands: it prints only `dialog exit <status>`, never the session's answered dialogs or their messages, on any page. The counts of the action that met a dialog (`dialogs 1` above) are the record of it. A `beforeunload` dialog cannot be answered.

### Evidence

```bash
iwr() { local a=("$@") c s=0 x; for x in "$@"; do case "$x" in -h|--help) echo "iwr: usage: iwr runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; iwdp-cli "$@" 2>/dev/null || s=$?; echo "$c exit $s"; [ "$s" = 0 ] || exit "$s"; }
iwr screenshot "$EVIDENCE/after-sign-in.png"
iwr screenshot --selector @e5 "$EVIDENCE/delete-button.png"
iwr screenshot --annotate "$EVIDENCE/annotated.png"
iwr console --level warning
iwr errors
iwr network requests --filter /api/
iwr vitals
```

- A screenshot without a path goes to `$TMPDIR`; always give an absolute path in the evidence folder. `--full-page` captures the whole document, `--selector` one element, `--annotate` draws the ref labels. Do not capture pages that show secrets unless the task allows it: cropping later is too late.
- `console` and `errors` print what the daemon captured since your session took the page (and up to 100 messages that WebKit replays). There is no live stream: for new messages only, run `console --since "$last_id"` again, with `last_id` set to the last id that `console` printed.
- `network requests` lists the requests since your session took the page; `network request <id> --body` shows one request. The values of secret headers and of query, form and JSON fields with password or token names print as `[masked]`; other values print in full.

### Navigation, tabs and frames

```bash
iw() { local a=("$@") c out n s=0 x; for x in "$@"; do case "$x" in --json|--json=*|--content-boundaries|--content-boundaries=*) echo "iw: usage: iw sets the output format itself; remove --json and --content-boundaries"; exit 2;; -h|--help) echo "iw: usage: iw runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; out="$(iwdp-cli --json "$@" 2>/dev/null)" || { s=$?; echo "$c exit $s"; exit "$s"; }; n="$(printf '%s\n' "$out" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" warnings %d dialogs %d" % (len(d.get("warnings") or []), len(d.get("dialogs") or [])) if isinstance(d, dict) and "trusted" in d and "steps" in d else "")')" || { s=$?; echo "$c exit 0"; echo "filter exit $s"; exit "$s"; }; echo "$c exit 0$n"; }
iw click @e6                            # a link to another page
iw wait --url '**/second'
iw back
iw forward
iw reload
```

`open`, `back`, `forward` and `reload` wait for the load event (`--wait domcontentloaded` or `none` to change). `tab new <url>` opens a tab with `window.open`; open no tab that the task did not ask for. `frame @eN` moves your later commands into a same-origin iframe, and `frame main` moves them back.

`get url`, `get title` and `open` print the URL or title of one page. A URL or title can carry a token: print them only for a page that the task marks as non-sensitive. That never makes the other tabs non-sensitive: list tabs only with the filtered listing of "Start a run". Before the first action on another tab, run the target check of "Start a run" on it.

## DevTools extras

```bash
iw() { local a=("$@") c out n s=0 x; for x in "$@"; do case "$x" in --json|--json=*|--content-boundaries|--content-boundaries=*) echo "iw: usage: iw sets the output format itself; remove --json and --content-boundaries"; exit 2;; -h|--help) echo "iw: usage: iw runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; out="$(iwdp-cli --json "$@" 2>/dev/null)" || { s=$?; echo "$c exit $s"; exit "$s"; }; n="$(printf '%s\n' "$out" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" warnings %d dialogs %d" % (len(d.get("warnings") or []), len(d.get("dialogs") or [])) if isinstance(d, dict) and "trusted" in d and "steps" in d else "")')" || { s=$?; echo "$c exit 0"; echo "filter exit $s"; exit "$s"; }; echo "$c exit 0$n"; }
iwr() { local a=("$@") c s=0 x; for x in "$@"; do case "$x" in -h|--help) echo "iwr: usage: iwr runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; iwdp-cli "$@" 2>/dev/null || s=$?; echo "$c exit $s"; [ "$s" = 0 ] || exit "$s"; }
iwr network har start
iw reload
iwr network har stop "$EVIDENCE/reload.har"
rm -f -- "$EVIDENCE/reload.har"          # after you read what you need
iwr network route '**/api/me' --status 503 --body '{"error":"down"}'
iwr network unroute
iwr set media dark
iwr set media light
iwr trace start
iwr trace stop "$EVIDENCE/page.trace.json"
```

- `network route` can abort, fulfill or delay matching requests (`--abort`, `--status`, `--body`, `--delay`); `network unroute` removes the routes. `set headers`, `set useragent`, `set geo`, `set offline` and `emulate network` change the page for your session only, and `close` resets them.
- `profiler start` and `profiler stop <path>` record a JavaScript CPU profile; `addinitscript` runs a script in every new document of the page.
- Not possible on iOS WebKit: trusted input, `set viewport` and `set device` (the viewport is the device's), PDF, downloads, video recording, browser contexts (every session shares one cookie jar), native pickers, `beforeunload` dialogs, cross-origin iframes.

## Cookies and storage

```bash
iwr() { local a=("$@") c s=0 x; for x in "$@"; do case "$x" in -h|--help) echo "iwr: usage: iwr runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; iwdp-cli "$@" 2>/dev/null || s=$?; echo "$c exit $s"; [ "$s" = 0 ] || exit "$s"; }
iwr state save "$EVIDENCE/state-before.json"
iwr cookies
iwr cookies set session_hint abc123 --http-only --same-site Lax --expires 0
iwr storage local
iwr state load "$EVIDENCE/state-before.json"
rm -f -- "$EVIDENCE/state-before.json"
```

- `cookies` prints names and attributes with hidden values (`name=(hidden)`); `--show-values` prints values. Name another page with `--page`. `cookies set` reads the cookie back and exits 1 when WebKit stored something else; `--expires 0` makes a session cookie. `cookies set --curl <file>` imports cookies from a file.
- `state save` writes the cookies, localStorage and sessionStorage of the current origin (mode 0600); `state load` replaces them with the file's, on a page of the same origin. This is the restore step of safety rule 5.

## CDP endpoint

Prefer the `iwdp-cli` commands of this skill. Use the CDP endpoint only when the task needs a CDP client: an existing Puppeteer or Playwright script, or a client feature that has no command here. The commands have refs that survive reconnects, actionability checks, masking and deadlines; a CDP client has none of these from iwdp-cli.

`get cdp-url` starts the CDP endpoint on 127.0.0.1 (port 9230) and prints a WebSocket URL (the ws form). The URL holds a secret token: keep it in a variable, never print it or write it to a file. Use it exactly as printed. A CDP client acts for your session: it shares your lease, refs and dialog answers, and `close` revokes the URL.

<!-- doc-test: skip w5b: runs agent-browser, which the skill doc test does not install; e2e/cdpcompat TestClientScenarios runs this form -->
```bash
CDP="$(iwdp-cli get cdp-url 2>/dev/null)" || { s=$?; echo "get cdp-url exit $s"; exit "$s"; }
echo "get cdp-url exit 0"
agent-browser --cdp "$CDP" snapshot -i
```

- The URL is for the page your other commands use: the tab you chose with `tab`, or `--page t2` for another tab.
- agent-browser: `agent-browser --cdp "$CDP" <command>`, ws form only. Its refs are its own, not iwdp-cli's.
- Puppeteer: `puppeteer.connect({browserWSEndpoint: CDP, defaultViewport: null})`; without `defaultViewport: null` it fails, because the viewport is fixed.
- Playwright: `chromium.connectOverCDP(CDP)`, then `browser.contexts()[0]`; new contexts fail. For `page.route` use a full-URL glob such as `'**/api/*'`. On iOS below 17.4 (Safari 16), locator actions, `getByRole` and `ariaSnapshot` work through the endpoint's `checkVisibility` shim (passed on an iPhone with iOS 16.7), but `locator.highlight()` and the `mask` option of screenshots fail, because Safari 16 has no popover API.
- chrome-devtools-mcp: `--wsEndpoint "$CDP"`; call `list_pages` first and pass the page id to the page tools. `get_css_styles` and source-mapped console stacks do not work.
- `get cdp-url --form http` prints the discovery base for Playwright and Puppeteer `browserURL`. `cdp status` shows the endpoint without the token; `cdp stop` revokes every URL.
- The endpoint serves only the methods of the method table in docs/cdp.md, with the limits there. Any other method gets "wasn't found". Input through CDP is synthetic too, and CDP output is not masked.

## End a run

```bash
iwr() { local a=("$@") c s=0 x; for x in "$@"; do case "$x" in -h|--help) echo "iwr: usage: iwr runs no help; read it with iwdp-cli <command> --help"; exit 2;; esac; done; while [ "${#a[@]}" -gt 0 ]; do case "${a[0]}" in --page|--session|--timeout|--transport|--max-output|--reservation|--wait-lease) a=("${a[@]:2}");; --*) a=("${a[@]:1}");; *) break;; esac; done; c="${a[0]:-}"; iwdp-cli "$@" 2>/dev/null || s=$?; echo "$c exit $s"; [ "$s" = 0 ] || exit "$s"; }
iwr close
s=0; iwdp-cli device release --owner "$IWDP_SESSION" >/dev/null 2>&1 || s=$?; echo "device release exit $s"; [ "$s" = 0 ] || exit "$s"
```

`close` ends your session: it releases your pages, removes your routes, headers, overrides and dialog answers, and revokes your CDP URLs. It does not close the Safari tab, does not touch other sessions, and does not undo forms you submitted or sign-ins you made.

## Errors and exit codes

| Exit | Meaning | What to do |
|---|---|---|
| 0 | done | |
| 1 | the command failed (message on stderr) | read the message; it names the cause |
| 2 | usage error, before any connection | fix the command line |
| 3 | a wait timed out | the expected state did not come; record it |
| 4 | the page is leased by another session, or its connection was lost | wait (`--wait-lease`) or ask; never take over without the device owner's approval |
| 5 | no device or page | check the device and the open tab |
| 6 | device reservation missing | reserve the device as in "Start a run" |
| 124 | the command deadline passed | one health probe, then follow safety rule 3 |

- "tab is not answering; it is probably not the visible Safari tab": hidden Safari tabs accept commands and never answer. Ask the user to bring the tab to the front.
- "connection to page was closed by someone else or by the device; ownership is unknown": another inspector (another agent, Safari Web Inspector on a Mac) took the page. Stop and ask. Only the device owner can approve `--takeover`.
- Error messages of page commands can name the page: a page whose connection was lost fails with its title, and other errors can repeat its URL. When titles and URLs are private, run page commands through `iw` or `iwr`, defined in the same shell call (Private mode, after the safety rules), as every block of this guide does: they discard stderr and print the exact status after success and failure, also in a shell with `set -e`, which ends at a failed command before a later `echo` runs; `|| true` or a pipe after the command would replace the status.
- The conflict error of `device reserve` and `device release` names the device by the last four characters of its identifier (a short identifier in full). Treat that like any other device identifier: keep it out of reports and shared transcripts. The reservation steps of this guide discard stderr and print only the exit status.

## Device setup

The iPhone needs Web Inspector on (Settings, Safari, Advanced, Web Inspector), at least one open Safari tab, and the device unlocked and trusted. ios_webkit_debug_proxy serves the device: port 9221 lists devices, and the first device's pages are on port 9224. This read-only check prints the number of listed devices only (no device identifiers); then run the filtered tab listing of "Start a run". The curl call is bounded by `--max-time` and needs no `timeout` command.

<!-- doc-test: skip device: needs ios_webkit_debug_proxy and a USB-connected iPhone -->
```bash
listing="$(curl -fsS --max-time 3 http://127.0.0.1:9221/json 2>/dev/null)" || { s=$?; echo "curl exit $s"; exit "$s"; }
printf '%s\n' "$listing" | python3 -c 'import json,sys; print("devices", sum(1 for d in json.load(sys.stdin) if d.get("deviceId") != "SIMULATOR"))' || { s=$?; echo "filter exit $s"; exit "$s"; }
echo "curl exit 0"
```

- The device listing is empty or 9221 does not answer: iwdp is down or stale. Do not restart it yourself (safety rule 7): report it, with the failed check. While the daemon holds a page connection, an empty reply (curl: "Empty reply from server") can also be the listing fault of the next item.
- "iwdp did not answer the page list while the daemon has ... open" (exit 5): iwdp 1.9.2 can get one broken file-descriptor slot. Then it closes every page-list request without an answer while a page connection is open. Commands on a page that the daemon already holds still work: keep working in your session without `--page`, or name that page by its tab id or `ws://` URL. These fail on such an iwdp: `tab list`, `tab new`, `tab close`, `tab <id>` or `--page` for a page that is not attached, `--page` with a title, another session's first command, and CDP clients (their target list and attach). A restart of iwdp clears the fault (only `iwdp-cli restart-iwdp`, which refuses an iwdp that iwdp-cli did not start; never restart another agent's iwdp, rule 7), and the message names it and `iwdp-cli daemon stop` as remedies; both belong to the device owner. Do not restart iwdp yourself (safety rule 7) and do not run `iwdp-cli daemon stop`: it ends every session's page connections, also other agents'. Report it, and mark the checks that need another tab NOT RUN.
- The device is listed, and the tab listing shows no tab and `hidden 0`: ask the user to open the page in Safari. With `hidden` above 0 the pages exist, but your owner has no reservation of their device: reserve it as in "Start a run", or ask the device owner.
- A passcode or Trust prompt needs the user; it does not mean iwdp is down.

## Known limits

- Only one inspector connection per page. The daemon holds it; another tool that attaches (Safari Web Inspector, an older iwdp-cli, a hand-written WebSocket client) takes the page from the daemon. Never write your own WebSocket client.
- A command names another page with `--page` (a tab id or the page's `ws://` URL), after the target check of that page: `navigate` with `--page` is `open` on that page and waits for the load, and `cookies` with `--page t2` reads the cookies of tab t2. It opens no second inspector connection. A trailing `ws://` argument, or `IWDP_WS_URL` set without `--page`, is a usage error (exit 2) for every page command.
- After a release whose cleanup could not run (the page connection was already gone), page-side overrides (the geolocation shim, JavaScript routes) can stay in the current document until it navigates.
- `CSS.enable`, `CSS.getAllStyleSheets` and `CSS.getStyleSheetText` hang through ios_webkit_debug_proxy, so the commands that need them refuse to run there; `hover --css` works only on WebKitGTK.
