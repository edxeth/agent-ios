# Migration notes

Earlier releases had one connection per command and one tool per WebKit method. The current release runs every command through the daemon and adds the registry commands of the agentic loop. The legacy commands and tools still work; this page says what changed and which registry command replaces which legacy one.

## What changed for every user

- **The daemon.** The first command starts a background daemon that keeps the page connection. Commands are faster and share state. `iwdp-cli daemon stop` stops it; see [guide.md](guide.md#the-daemon).
- **Sessions and leases.** A page belongs to one session at a time. Give each agent its own `--session` or `IWDP_SESSION`, and end it with `iwdp-cli close`. See [sessions.md](sessions.md).
- **Device reservations.** Pages of a physical device need a reservation: `iwdp-cli device reserve --owner <owner>` first. Without one, page commands exit 6.
- **Lost pages.** When someone else takes the page, commands fail with exit 4 and "ownership is unknown"; nothing reconnects by itself. `--takeover` reattaches, with the device owner's approval.
- **The "Connecting to: <title>" line is gone.** Errors still name the page.
- **restart_iwdp and restart-iwdp** restart only an iwdp that iwdp-mcp started, and refuse otherwise ([device-runbook.md](device-runbook.md)).
- **Exit codes of registry commands**: 0 ok, 1 error, 2 usage, 3 wait timeout, 4 page busy or lost, 5 no device or page, 6 reservation missing, 124 deadline, 130 signal. Legacy commands differ; see [Legacy commands: output and exit codes](#legacy-commands-output-and-exit-codes).

## MCP server

- The server registers the `core` profile by default (47 tools). Earlier releases registered every tool. To get them all back, set `IWDP_MCP_TOOLS=all` or start `iwdp-mcp --tools all`; `--tools core,legacy` keeps the earlier tool names beside the new ones (the table below says which handler serves a shared name). See [guide.md](guide.md#mcp-profiles).
- Set `IWDP_SESSION` in the server's environment to a name of its own, and `IWDP_DEVICE_RESERVATION` to the owner of the device reservation.
- Thirteen registry commands reuse the name and the legacy parameters of a legacy tool. The selected profiles decide which handler serves such a name. When they include the profile of the registry command, the registry command serves the name and the legacy handler is not registered. Otherwise the legacy handler serves the name when the selected profiles register it (`legacy` registers every legacy tool, `core` a fixed list):

  | Tool names | Registry command (profile) serves them in | Legacy handler serves them in | Not registered in |
  |---|---|---|---|
  | `navigate`, `reload`, `take_screenshot`, `click`, `fill`, `type_text`, `highlight_node`, `set_cookie` (`core`) | `core` (default), `core,devtools`, `core,legacy`, `all` | `legacy`, `devtools,legacy` | `devtools` |
  | `set_extra_headers`, `list_network_requests`, `get_console_messages`, `clear_console` (`devtools`) | `devtools`, `core,devtools`, `devtools,legacy`, `all` | `core` (default), `core,legacy`, `legacy` | none |
  | `set_emulated_conditions` (`devtools`) | `devtools`, `core,devtools`, `devtools,legacy`, `all` | `core,legacy`, `legacy` | `core` (default) |

  In the default `core` profile the network and console tools are therefore the legacy handlers: `list_network_requests` is not masked, and it and `get_console_messages` stay empty there ([known-limits.md](known-limits.md#mcp-profiles)). For the daemon's masked network log and its console buffer, select a profile with `devtools`, for example `IWDP_MCP_TOOLS=core,devtools`.
- Where the registry command serves the name, a legacy parameter and its new name are aliases (for example click's `selector` and `target`); give one of them, not both. `click`, `fill` and `type_text` then accept `@eN` refs and locators and run the actionability checks of their action ([guide.md](guide.md#the-agentic-loop), step 4); the checks differ per action.
- New tools: see the index of [commands.md](commands.md). The MCP name of a registry command can differ from its CLI name (`open` is `navigate`, `tab` is `tab_select`, `frame` is `select_frame`, `close` is `close_session`, `read` is `read_page`).

## CLI: legacy command to registry command

A command line whose first word is both a legacy command and a registry command goes to the registry when the registry parses the whole line or has a longer command for its words (`network requests`, `cookies set`). A line that names its page by a `ws://` argument or `IWDP_WS_URL` stays with the legacy command.

| Legacy command | Registry command | Notes |
|---|---|---|
| `navigate <url> [ws-url]` | `open <url>` (aliases `goto`, `navigate`) | waits for the load state of the new document; `--wait none` returns at once |
| `reload [--ignore-cache]` | `reload [--ignore-cache]` | waits for the load state; withdraws unused dialog answers first |
| `screenshot [--full-page] [--selector css] [-o file]` | `screenshot [path] [--full] [--selector @eN or css] [--annotate] [--if-changed]` | the same flags work |
| `dom [selector]`, `query-selector`, `get-outer-html`, `get-attributes` | `snapshot`, `find`, `get html|attr|text|count|box|styles <target>`, `is <state> <target>` | refs instead of node ids |
| `click <selector>` | `click <target>` | target: `@eN`, CSS selector or locator; actionability checks |
| `fill <selector> <value>` | `fill <target> <text>`, or `fill <target> --stdin` for secrets | the value never appears in argv with `--stdin` |
| `type-text <text>` | `type <target> <text>`, `keyboard type <text>`, `press <key>` | `type` has no `type-text` alias, because the legacy form takes other arguments |
| `highlight-node <nodeId>`, `hide-highlight` | `highlight <target>` | |
| `force-pseudo-state <nodeId> hover` | `hover <target> --css` | through iwdp `CSS.enable` hangs, so `--css` fails there (exit 2) |
| `console [-d 10s] [--follow]`, `console-messages` | `console` (buffer since the lease), `errors` | `console -d` and `console --follow` stay the legacy streams |
| `network [-d 10s] [--follow]`, `get-response-body <id>` | `network requests`, `network request <id> --body` | secrets masked unless `--show-secrets` |
| `set-request-interception`, `intercept-continue`, `intercept-respond` | `network route <url> [--abort|--body|--status|--delay]`, `network unroute` | requests are always answered; nothing stays paused |
| `set-extra-headers <json>` | `set headers <json>` | `set headers '{}'` removes them |
| `set-network-conditions <bytes/sec>` | `emulate network <preset>` | only where the iOS version has the method |
| `set-cookie ...` | `cookies set <name> <value> [flags]`, `cookies set --curl <file>` | bare `cookies` stays the legacy listing |
| `delete-cookie` | `cookies clear [--domain d]` | |
| `get-local-storage`, `set-local-storage-item`, ... | `storage local|session [get|set|clear] ...` | values of secret keys are masked |
| `timeline-record` | `trace start`, `trace stop [path]` | Chrome trace JSON |
| `cpu-profile`, `script-profile` | `profiler start`, `profiler stop [path]` | `.cpuprofile` |
| none | `wait`, `dialog`, `back`, `forward`, `tab`, `frame`, `diff snapshot`, `diff screenshot`, `vitals`, `state save|load`, `network har start|stop`, `set media|useragent|geo|offline|credentials`, `addinitscript`, `get cdp-url` | new |

Legacy commands with no registry replacement stay as they are: `eval` (with `--await`), `devices`, `pages`, `status`, `restart-iwdp`, the debugger and DOM breakpoint commands, CSS by node id, heap and memory, animation, canvas, layer tree, workers, audit, certificate info and browser extensions. They run through the daemon too, in one operation each, and the daemon removes the state they set (breakpoints, interception, overrides) when the session ends.

## Legacy commands: output and exit codes

The legacy commands keep their earlier output and argument parsing. Only the registry commands (the commands in [commands.md](commands.md)) have the registry's output flags and its usage exit code.

- **Output flags.** Legacy commands do not apply `--json`, `--max-output` or `--content-boundaries`. A legacy command ignores such a flag or reads it as one of its arguments, depending on the command and the position. For example, `iwdp-cli eval document.title --content-boundaries` prints the page title without the content markers. When an agent must see untrusted page text between markers, use a registry command (`get text`, `read`, `snapshot`) with `--content-boundaries`.
- **Usage errors exit 1.** A legacy command with a missing argument or a bad flag exits 1, not 2: for example `iwdp-cli eval` without an expression, or `iwdp-cli screenshot -o` without a file name.
- **Other exit codes.** A legacy command exits 124 when the command deadline expires and 130 on SIGINT or SIGTERM. A daemon error that has its own exit code (4 page busy or lost, 5 no device or page, 6 reservation missing) keeps it; every other error exits 1.

## For skills and prompts written for agent-browser

The registry commands keep agent-browser's names, flags and output wherever WIP allows (`open`, `snapshot -i`, `click @e3`, `fill`, `get text`, `is visible`, `find role button --name Save click`, `wait --load networkidle`, `--session`, `--json`, `--content-boundaries`). The differences: all input is synthetic ([synthetic-input.md](synthetic-input.md)); `set viewport` and `set device` fail; there are no browser contexts, PDF or downloads; physical devices need `device reserve`.
