---
name: iwdp-devtools
description: DevTools extras of iwdp-cli for iOS Safari and WebKitGTK pages. A cookie or storage change and its restore, HAR files, request routes, media and network emulation, extra headers, traces, CPU profiles and init scripts. Load it from the iwdp guide when a task needs one of them.
---

# iwdp-cli: DevTools extras

The safety rules and private mode of `iwdp-cli skills get iwdp` bind every command of this guide.

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
mkdir -p -m 700 "$EVIDENCE" || exit "$?"
iwdp-cli network har start || exit "$?"
iwdp-cli reload || exit "$?"
iwdp-cli network har stop "$EVIDENCE/reload.har" || exit "$?"
rm -f -- "$EVIDENCE/reload.har" || exit "$?"   # after you read what you need: it holds live credentials
iwdp-cli network route '**/api/me' --status 503 --body '{"error":"down"}' || exit "$?"
iwdp-cli network unroute || exit "$?"
iwdp-cli set media dark || exit "$?"
iwdp-cli set media light || exit "$?"
iwdp-cli trace start || exit "$?"
iwdp-cli trace stop "$EVIDENCE/page.trace.json" || exit "$?"
iwdp-cli close
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
- After a release whose cleanup could not run (the page connection was already gone), page-side overrides (the geolocation shim, JavaScript routes) can stay in the current document until it navigates.

## Cookies and storage

Safety rule 4 binds a change: save the state first, and restore it at the end, also after a failed change.

```bash
export IWDP_SESSION=qa-login IWDP_PRIVATE=1
mkdir -p -m 700 "$EVIDENCE" || exit "$?"
iwdp-cli cookies || exit "$?"
iwdp-cli storage local || exit "$?"
iwdp-cli state save "$EVIDENCE/state-before.json" || exit "$?"   # change nothing without a saved state
changed=0
iwdp-cli cookies set session_hint abc123 --http-only --same-site Lax --expires 0 || changed=$?   # restore also after a failed change
iwdp-cli state load "$EVIDENCE/state-before.json" || exit "$?"   # keep the file when the restore fails
rm -f -- "$EVIDENCE/state-before.json" || exit "$?"
[ "$changed" = 0 ]
```

- `cookies set` reads the cookie back and exits 1 when WebKit stored something else.
- `state save` and `state load` cover the cookies, localStorage and sessionStorage of the current origin; load on a page of the same origin. When `state load` fails, the block keeps the file: run `state load` again, and delete the file before the run ends.
