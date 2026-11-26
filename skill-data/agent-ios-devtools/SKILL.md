---
name: agent-ios-devtools
description: DevTools extras of agent-ios for iOS Safari and WebKitGTK pages. A cookie or storage change and its restore, named states, HAR files, request routes, media and network emulation, extra headers, traces, CPU profiles, heap snapshots, matched CSS rules, logpoints and init scripts. Load it from the agent-ios guide when a task needs one of them.
---

# agent-ios: DevTools extras

The safety rules and private mode of `agent-ios skills get agent-ios` bind every command of this guide.

```bash
export AGENT_IOS_SESSION=qa-login AGENT_IOS_PRIVATE=1
mkdir -p -m 700 "$EVIDENCE" || exit "$?"
agent-ios network har start || exit "$?"
agent-ios reload || exit "$?"
agent-ios network har stop "$EVIDENCE/reload.har" || exit "$?"
rm -f -- "$EVIDENCE/reload.har" || exit "$?"   # after you read what you need: it holds live credentials
agent-ios network route '**/api/me' --status 503 --body '{"error":"down"}' || exit "$?"
agent-ios network unroute || exit "$?"
agent-ios set media dark || exit "$?"
agent-ios set media light || exit "$?"
agent-ios trace start || exit "$?"
agent-ios trace stop "$EVIDENCE/page.trace.json" || exit "$?"
agent-ios close
```

<!-- doc-test: expect -->
```text
network har start exit 0
reload exit 0
network route exit 0
set media exit 0
close exit 0
```

- `network route` can abort, fulfill or delay matching requests (`--abort`, `--status`, `--body`, `--delay`): a glob with `*` and `**` matches the whole URL. `network unroute` removes the routes.
- `set headers`, `set useragent`, `set geo`, `set offline` and `emulate network` change the page for your session only, and `close` resets them.
- `profiler start` and `profiler stop <path>` record a JavaScript CPU profile; `trace start` and `trace stop <path>` a Chrome trace file.
- `addinitscript` runs a script in every new document of the page; `removeinitscript <id>` removes it.
- Heap: `heap snapshot "$EVIDENCE/h1.json"`, the action under test, `heap snapshot "$EVIDENCE/h2.json"`, then `heap diff "$EVIDENCE/h1.json" "$EVIDENCE/h2.json"`: classes whose instance count grew are leak candidates. The files are JavaScriptCore JSON, not V8 `.heapsnapshot`, and hold page content: delete them before the run ends. `heap gc` collects garbage.
- CSS: `css matched <target>` lists the rules that match an element, overridden declarations marked `[overloaded]`; `css computed <target> [props]` prints computed values from the CSS domain.
- Logpoints: `logpoint add app.js:42 '"total", cart.total'`, then `console`; the location can be an original source of a source map (`src/cart.ts:12`); the page never pauses. `logpoint list`, `logpoint remove <id>`, `logpoint clear`.
- In private mode, `heap`, `css`, `logpoint`, `diff url` and the `state` reads print only their status line; pass `--private=false` to the one command whose result the task needs.
- After a release whose cleanup could not run (the page connection was already gone), page-side overrides (the geolocation shim, JavaScript routes) can stay in the current document until it navigates.

## Cookies and storage

Safety rule 4 binds a change: save the state first, and restore it at the end, also after a failed change.

```bash
export AGENT_IOS_SESSION=qa-login AGENT_IOS_PRIVATE=1
mkdir -p -m 700 "$EVIDENCE" || exit "$?"
agent-ios cookies || exit "$?"
agent-ios storage local || exit "$?"
agent-ios state save "$EVIDENCE/state-before.json" || exit "$?"   # change nothing without a saved state
changed=0
agent-ios cookies set session_hint abc123 --http-only --same-site Lax --expires 0 || changed=$?   # restore also after a failed change
agent-ios state load "$EVIDENCE/state-before.json" || exit "$?"   # keep the file when the restore fails
rm -f -- "$EVIDENCE/state-before.json" || exit "$?"
[ "$changed" = 0 ]
```

- `cookies set` reads the cookie back and exits 1 when WebKit stored something else.
- `state save` and `state load` cover the cookies, localStorage and sessionStorage of the current origin; load on a page of the same origin. When `state load` fails, the block keeps the file: run `state load` again, and delete the file before the run ends.
- A bare name is a named state, not a file: `state save before` writes `$XDG_STATE_HOME/agent-ios/states/before.json`; `state list` (prints names), `state show before` (counts only), `state clear before`. Anything with a dot or slash (`./before`, `before.json`) is a path; `state rename` and `state clear` take names only (`state clear before`, never `before.json`).
