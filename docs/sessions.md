# Sessions, leases and device reservations

Safari allows one inspector connection per tab, and a new connection takes the tab from the old one. agent-ios therefore has three layers of ownership. This page states the rules; agents must follow them.

## Sessions

- A session is a name that the client chooses: `--session <name>` or `AGENT_IOS_SESSION`. The default is `default`.
- The default is for compatibility only. It is not isolation: two agents that both use `default` share leases, refs and dialog answers. **Every agent sets its own stable session name.**
- Sharing a session is correct only when a CDP client connects through a URL that `get cdp-url` issued for that session ([cdp.md](cdp.md)).
- `agent-ios session` prints the current name; `session info` shows the session's leases; `session list` lists the sessions that hold leases.

## Leases

A lease binds one page to one session. It carries an **epoch**, a number that increases with each grant. Every operation carries the epoch it was admitted under, and an operation with an old epoch is refused before it sends anything.

- A session works on the page that it chose: `--page`, `tab <id>` or the tab that `tab new` opened, else the page that its last command without `--page` used while the daemon holds it open. A session that has chosen nothing gets a page only when the listing has exactly one tab and no hidden page; with several tabs its page commands fail with exit 5 and touch no page, because the first tab can be another agent's or the user's ([guide.md](guide.md#navigation-tabs-and-frames)). Start with `agent-ios tab new <url>` or `agent-ios tab <id>`.
- The first page command of a session takes the lease on its page.
- A command of another session on a leased page fails at once with: `page "<title>" is leased by session "<name>" since <time> (last command <age> ago); use --session <name>, wait, or run 'agent-ios lease break <page>'` (exit 4).
- `--wait-lease <duration>` waits for the lease inside the command deadline.
- A lease ends with `close` or `session release` (this session only), after 5 minutes without commands and captures (idle expiry), or with `lease break <page>`.
- The end of a lease runs the cleanup of the session's state on the page: interception and paused requests are continued, breakpoints removed and the debugger resumed, overrides and init scripts removed, dialog overrides restored, overlays removed. The release does **not** undo what the page did: it does not restore authentication state, and it does not undo submitted forms or transactions.
- `lease break` ends another session's lease. Use it only for a session that you own or that has clearly ended.

### Lost pages and takeover

When a page connection closes without a request from the daemon (another inspector took the tab, the device went away), the lease becomes **lost**. The daemon cannot know who holds the page now, so:

- it never reconnects by itself;
- the next command fails with "connection to page was closed by someone else or by the device; ownership is unknown" (exit 4);
- only `--takeover` on a command, or `agent-ios lease takeover [page]`, reattaches the page. It needs the device reservation and **the approval of the device owner**: a takeover takes the page back from whoever holds it now.

A page that stops answering is different: it is **stalled**. The daemon then tries one background reconnect (15 s at most), only while the lease is valid and the page is not lost. A command that arrives during that reconnect waits for it within its own deadline.

## Device reservations

The daemon cannot see every inspector client: another agent's script, an older agent-ios, or Safari Web Inspector on a Mac can hold a page, and iwdp gives a page to the newest client. Reservations are the cooperative rule that prevents this between users of agent-ios.

1. **No page connection to a physical device without a reservation.** Reserve the device before the first page command:

   ```bash
   agent-ios device reserve --owner <owner> [--device <udid>] [--ttl 2h]
   ```

   Without `--device` the reservation covers every device. It fails when another owner holds a live reservation. The reservation is renewed while the owner holds leases. A page command without a matching reservation fails with exit 6, also on a page that the daemon is already attached to and under a lease that the session took before; so does a CDP operation under such a lease. When the owner's reservation is released, expires or passes to another owner, the daemon's sweep (every 5 s) ends the owner's leases on the device's pages, so that no event stream or CDP attachment goes on without it. A reservation that cannot be read admits nothing on a physical device.
2. The reservation owner of a command is `--reservation <owner>` or `AGENT_IOS_DEVICE_RESERVATION`, else the session name. An orchestrator can reserve once and give its workers its owner name in `AGENT_IOS_DEVICE_RESERVATION` (for example `export AGENT_IOS_DEVICE_RESERVATION="$OWNER"`).
3. Release at the end of the run: `agent-ios device release --owner <owner>`. `device list` shows the live reservations.
4. WebKitGTK needs no reservation. An iOS Simulator over SSH is shared by taking turns with `agent-ios sim reserve` and `sim release` instead ([simulator.md](simulator.md)): its own reservation, which hides its tabs from other owners and ends 2 minutes after the owner's last use. A simulator without such a reservation needs none.
5. Before the first attach to a page, the daemon checks the reservation and then a busy hint: iwdp lists an empty `devtoolsFrontendUrl` for a page that already has a client, but only when iwdp runs with its frontend. With `--no-frontend` (how agent-ios starts it) ownership is unknown, and the attach is accepted only inside a reservation.
6. **Hints and reservations are not locks.** They do not stop a client that ignores reservations (another tool, a raw script, Safari Web Inspector).
7. Tab discovery (`tab list`) and the CDP endpoint list only pages of devices that the owner reserved: `get cdp-url` for another device's page fails with exit 6, and an endpoint issued before its owner lost the reservation no longer lists the device's pages, its selected page included. An owner that is refused learns nothing of the page: the daemon names the page to the client only after the reservation check.

## Operations on one page

All entry points (CLI commands and CDP messages) are operations in one scheduler per page. Commands that change the page (actions, snapshots, which allocate refs) run one at a time in arrival order; reads run in parallel with at most 8 protocol calls in flight. A pending `Runtime.awaitPromise` (the armed reply of an action, a CDP awaited evaluation) gives its place back once it is written, so waits never block the calls that settle them. A cancelled command (Ctrl+C) is cancelled in the daemon at once; an action step that was already sent may still have run, and the error says so ("the page may have run step 2 of 3 of type before the reply was lost").

## Rules for agents

- Set a distinct, stable `--session` (or `AGENT_IOS_SESSION`) for each agent.
- On a physical device: `device reserve` at the start of the run, `close` and `device release` at the end.
- Never `lease break` or `--takeover` a page that another agent may use, unless the device owner approved it.
- On "lost": stop and ask; do not retry in a loop.
- Never stop or restart an iwdp that you did not start; see [device-runbook.md](device-runbook.md).
