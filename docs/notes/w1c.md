# W1c notes (daemon core leftovers)

Backlog: /home/devkit/.pi/artifacts/retro/browser-checker-20261106/w1c-backlog.md; decisions: w1c-decisions.md. Six workers, committed as ac71afd, d9f35c0, a0256b0, 52f8e98, 8af5aa6 and 05aa57b. These notes are history; the user pages (docs/known-limits.md, docs/device-runbook.md) are canonical.

## W1c-1: attach and recovery (C03, C05, C06, C07, C10, C11, L07 core part)
- C05: a command that arrives during a background reconnect waits for it within its own deadline; a failed recovery gives exit 4, a deadline 124, an owner's cancel the cancellation. A lost page is never waited for.
- C06 and L07: a probe that finds a peer end keeps ErrLost and the --takeover guidance (Begin idle probe, recovery probe, fresh-connection probe).
- C07: a reconnect prepares the subscriptions on the new connection before the re-apply, so its events (the Console.enable replay) reach subscribers.
- C03: a commit that leaves cleanup owed starts a bounded background reset.
- C10: a client cancel during setup returns the cancellation, not exit 4. C11: watch goroutines run through the tracked background.
- Repair round 1b (F4 to F8):
  - A reconnect's prepared connection holds at most 1024 events per subscription. Queued drops are counted in pageConn.DroppedEvents. A read-loop overflow refuses the install (exit 4).
  - Queued replays run on one goroutine per subscriber, and Close does not wait for them.
  - No connection is installed after Manager.Close begins (errShuttingDown).
  - When a peer ends a reconnect's new connection (probe or re-apply), the page is lost, and only --takeover reconnects it.
- Known limit (F13): a first attach whose connection a peer ends has no page state to mark lost; the next first attach is decided by the busy hint.

## W1c-2: scheduler (C02, C04, C12)
- C02: Debugger pauses are kept per source target and resumed on that target (WithTarget), also on a provisional target; dropped at targetDestroyed and for the replaced target at commit. Debugger.resume accepts params.targetId.
- Release drains every live pause over the whole release window (open pauses, client resumes in flight, pauses during the cleanup); an unsent resume is retried at most 3 times, an unknown outcome never; the error names every target still paused and joins all failures.
- C04: the reset reason is recorded in the critical section that deletes the hold. C12: an empty Page.overrideUserAgent value clears the override.
- Limits: a pause after the lease ended is not drained; pause records live until their target is destroyed.

## W1c-3: pause-on-start, reconnect gap, dispatch order (C01, C08, C09)
- C01: the daemon (and the IWDP_NO_DAEMON core) turns on pause-on-start for every connection (daemon.PauseOnStart). Best effort: a refusal or no reply within 1 s keeps the connection, logs one line and records it off; a wire failure fails the connect.
- Close waits for resumes in flight; a session that ended clears its paused records (WebKit resumes paused targets when the last frontend disconnects).
- C08: Dial closes a silent attempt and waits 500 ms before the next; each attempt waits at most 3 s, and a longer TargetWait is spread over more attempts. This amends design 2.6 ("one more attempt", "12 s after a close"). The WebKitGTK tests no longer sleep before a takeover.
- C09: the Session dispatch order (read-loop subscribers of the method, then of AllEvents, then queued subscribers) is a documented contract with a test.
- Unverified on a device: whether iwdp relays the reply of Target.setPauseOnStart; iOS 26 may hold it.

## W1c-4: CLI output (L01, L03, L04, L05, L07 tab part)
- L01: DOM.getDocument without a root is an actionable error, no panic. L03: the "Connecting to: <title>" line is removed.
- L04: --content-boundaries wraps output in agent-browser 0.41.2's markers with a per-run nonce; --json adds "_boundary".
- L05: a registry command ended by a signal prints no error line (exit 130 unchanged).
- L07: tab <id> classifies a failed probe: daemon-ended operation, closed page, write timeout (hidden tab), lost (takeover), other (hidden tab). Exit 4 stays.

## W1c-5: iwdp process control (L02, L06)
- restart_iwdp, iwdp-cli restart-iwdp and proxy.Restart stop only an iwdp that iwdp-mcp started: a private record (pid, group, address, start time, token) checked against the process identity and the listening socket before every signal; otherwise they refuse and name the owner and the manual command. No pattern kill. macOS reads the token through KERN_PROCARGS2 and refuses when it cannot.
- scripts/sim-setup.sh records "pid token start" in a private directory and tears down only that process; on macOS teardown always refuses and prints kill <pid>.
- L06: iwdp_status uses one listing check.

## W1c-6: test isolation (T01 to T04)
- e2e tests use private fake listings through a link-time seam (proxy.listingAddr), never the real ports 9221 and 9224. cmd/iwdp-mcp tests cannot reach a shared daemon. TestBackCommandAcrossTwoSites covers back across two sites. Browser helpers use the shared broadway(t) and the 60 s start guard (go/ast check).
