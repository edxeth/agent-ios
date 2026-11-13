---
name: iwdp-devtools
description: DevTools extras of iwdp-cli for iOS Safari and WebKitGTK pages. HAR files, request routes, media and network emulation, extra headers, traces, CPU profiles and init scripts. Load it from the iwdp guide when a task needs one of them.
---

# iwdp-cli: DevTools extras

The safety rules and private mode of `iwdp-cli skills get iwdp` bind every command of this guide. Start every call with the line that the examples start with.

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
- `profiler start` and `profiler stop <path>` record a JavaScript CPU profile; `trace start` and `trace stop <path>` a Chrome trace file. Give every path as an absolute path in the evidence folder.
- `addinitscript` runs a script in every new document of the page; `removeinitscript <id>` removes it.
- HAR files and state files are written with mode 0600. The values of secret headers and of password and token fields print as `[masked]`; other values are in the file in full.
- After a release whose cleanup could not run (the page connection was already gone), page-side overrides (the geolocation shim, JavaScript routes) can stay in the current document until it navigates.
