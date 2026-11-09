# iwdp-mcp documentation

User documentation:

| Page | Content |
|---|---|
| [guide.md](guide.md) | How the product works: the daemon, the agentic loop (snapshot, refs, actions, waits, dialogs), tabs and frames, DevTools extras, MCP profiles, transports, output and exit codes |
| [sessions.md](sessions.md) | Sessions, page leases and device reservations: the rules for agents |
| [synthetic-input.md](synthetic-input.md) | The synthetic-input statement and the features that iOS WebKit cannot support |
| [cdp.md](cdp.md) | The CDP endpoint: get a tokenized URL, connect agent-browser, Puppeteer, Playwright or chrome-devtools-mcp, the method table, the limits per domain and the compatibility suite |
| [device-runbook.md](device-runbook.md) | Work on a physical iPhone: reserve and release, iwdp ownership, restart_iwdp, a lost page, pause-on-start |
| [migration.md](migration.md) | From the legacy commands and MCP tools to the registry commands |
| [known-limits.md](known-limits.md) | Every accepted limit with its reason |
| [commands.md](commands.md) | The command reference, generated from the command registry (`make docs`) |

Other material:

- [design/agentic-parity.md](design/agentic-parity.md) is the design of the daemon, the agent runtime, the command registry, the CDP endpoint and the direct transport. It explains why; the user pages above say what the product does now.
- [notes/](notes/) holds the workstream notes (W1a to W5, P2, W1c). They are history: they record how each part was built, reviewed and repaired. When a note and a user page disagree, the user page is correct.
