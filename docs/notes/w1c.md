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

## Listing independence (second device check, F1 and F2)
- Finding: the user's iwdp closed every HTTP listing request without a reply while any page WebSocket was open (device-smoke-2 report, F1; cause unknown). Every command read the listing (daemon ensureIWDP, core findListed), so after the first attach plain `eval 1+1` failed 60 of 60 times (exit 5).
- Rule (internal/core/listing_attached.go, resolve in manager.go): a request resolves to an open page record (attached or stalled) without the listing when it names the record's key (the `tab` choice, a tab id, `--page <key>`) or names no page and the record is the session's default page: the page its last request without a page and without a current page resolved to, for the same transport and address (pages_w3b.go setDefaultPage). A lost or closed connection falls back to the listing, as before. Explicit ws:// URLs already used the attached record (resolveURL).
- A listing read that fails while the daemon holds an iwdp page connection (core findListed and ListPages; the daemon marks its listing check as core.ListingError) gets advice: what happened, that commands on the attached page work, and `iwdp-cli daemon stop` or an iwdp restart. Exit 5 as before. The daemon starts iwdp only when the port can be bound, so it never starts one for this.
- A device's page list that fails while the device list answers (repair round 1, review F2) is the error only when the daemon holds an open connection to a page of that device (core pageListFailed): a title selection that matches no page on another device, `tab list` and the CDP ListPages return the coded listing error with the advice. A page that matches on another device is still used. Any other device whose page list fails is left out and logged, as before, so that a device whose port does not answer hides no other device of a healthy iwdp. Tests: internal/core listing_pages_test.go.
- F2: proxy.CheckListing names the failure: an empty reply (EOF or reset, with the open-WebSocket hint), a timeout (with ProbeBudget, the limit), or another error; a refusal and a status other than 200 keep their messages. The legacy `pages`, `devices` and `status` say "not running" only when nothing owns the port (exit 1); `pages` and `devices` exit 5 for an owner that does not answer (requireIWDP). The check is no evidence that the read after it succeeds (repair round 2, review F5): a failed ListDevices or ListPages read is classified the same way (proxy.ReadFailure, cmd/iwdp-cli listingReadFailed), so an empty reply of the device list or of a device's page list gives exit 5 and the advice, and an address that nothing owns keeps the read's error and exit 1. Only a failed request gets that classification (repair round 3, review F6): an answer that arrived but is unusable (a status other than 200, or a body that does not decode) is named as such, still with exit 5 (docs/migration.md, legacy exit codes).
- e2e coverage: under a coverage run (go test -cover, or the quality gate's -test.gocoverdir), the e2e iwdp-cli binaries are built with -cover and write to the same directory (e2e/cover_test.go), so the legacy listing commands count in the changed-line report. e2e listing_attached_test.go runs them on an empty reply, a healthy fake listing and an unowned port.
- Fake: fakewebkit.Listing.EmptyReplyWhileConnected. Tests: cmd/iwdp-cli listing_attached_test.go (red on the old code: eval after the attach exit 5), internal/core listing_attached_test.go, internal/daemon listing_attached_test.go, internal/proxy listing_reply_test.go, e2e listing_attached_test.go.
- Two existing core tests depended on the listing of every command: stallSecondCommand (repair_r2_test.go) now selects the page by title so that its listing still stores the self-busy hint, and TestSweepDoesNotEndARenewedCommand counts the agent reset's Network.disable before it waits for the expiry's (it used to pass only because the listing delayed the next Begin past the sweep's claim).
- Limits: the CDP endpoint lists pages through the listing, so its target list fails with the advice while a page is connected; `get cdp-url` without `--page` needs the listing too. `tab new` cannot find the new tab on such an iwdp. MCP iwdp_status still reports "failed to start iwdp" with the check's error when it starts nothing.

## W1c-6: test isolation (T01 to T04)
- e2e tests use private fake listings through a link-time seam (proxy.listingAddr), never the real ports 9221 and 9224. cmd/iwdp-mcp tests cannot reach a shared daemon. TestBackCommandAcrossTwoSites covers back across two sites. Browser helpers use the shared broadway(t) and the 60 s start guard (go/ast check).
