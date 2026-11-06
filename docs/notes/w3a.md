# W3a notes for later workstreams

Design: docs/design/agentic-parity.md, sections 3.1 to 3.4, 4.2 rows of W3a, 7.5. W6 merges these notes.

## What W3a provides
- internal/runtime: the injected bundle (js/10-core.js to js/45-locate.js, ES2017, about 46 KiB of the 64 KiB budget), Source() and Version, the owner epoch (SetOwner, OnReset, Envelope.Reset), CallWarn, RuntimeCaller() and the snapshot renderer (Render, golden tests in render_test.go).
- internal/refs: Table (serialized Allocate, Lookup with the unknown_ref and stale_ref messages of design 3.3, NoteReset, snapshot history for diff snapshot), ParseRef and Format.
- Page functions for W3b: `__iwdp.def(name, fn)`, `__iwdp.attempt(args, check, dispatch)` (the operation fence), `__iwdp.onReset`, `__iwdp.onRelease`, and the shared internals on `__iwdp._`: `resolve(spec, call)` (one element; ambiguous and not_found errors), `all(spec, call)`, `locate(kind, value, opts)`, `visible(el)`, `role`, `name`, `states`, `refFor`.
- Registry commands (internal/commands): snapshot, diff snapshot, get, is, find (locate part), hit-test, read; MCP tools snapshot, diff_snapshot, get, is, find, hit_test, read_page (profile core). The shared helper is page_runtime.go: target specs from refs, ref allocation inside Refs.Allocate, owner from the scheduler's lease, reset reports into the ref table.

## What W1b must wire
- A stable `runtime.DaemonID`. The default is random per process. In no-daemon mode every CLI process is then a new owner, so every call resets the page's ref maps and refs never survive from one CLI call to the next. Set it before the first call (for example from the daemon state directory) or keep the daemon.
- `core.PageSession.Runtime = runtime.New(caller, session.Generation)` and `Refs = refs.NewTable()`, one table per logical session and page (design 3.3: refs live for the session).
- `core.SchedulerOptions.Runtime = ps.Runtime.RuntimeCaller()`, so attempt calls carry the owner.
- `runtime.Source()` as the bootstrap part "runtime" with `core.RankRuntime`.
- Owner: the W3a commands call `SetOwner(Owner{runtime.DaemonID, holder, epoch})` from `ps.Sched.Lease()` when `ps.Sched` is set; without a scheduler the daemon must call SetOwner itself when it grants the lease.
- Lease epochs of a page only grow within one DaemonID, across sessions and releases: the page refuses an owned call whose epoch is older than its latest owner's (error code `stale_owner`, `runtime.CodeStaleOwner`) and changes nothing for it.
- Dispatch: snapshot, find and hit-test set `Command.Mutates` (ref allocation is a mutation, design 2.11); get, is, read and diff snapshot are `RetrySafe`. The commands do not call `Sched.Begin` themselves. `read <url>` never calls `Env.Page`, so a dispatcher must not lease a page before Run for it (its Page field is LeasedPage for the no-URL form).
- `--content-boundaries` is still not applied (W1a open item).

## Decisions and deviations
- Ref storage in the page: a WeakMap from element to ref info instead of the expando `el.__iwdpRef` of design 3.3. Same lifetime and behavior, and the page cannot see or change it.
- Visibility: the runtime always uses its own function (20-visibility.js); it does not patch `Element.prototype.checkVisibility`.
- `find nth` is 0-based and a negative index counts from the end, as agent-browser: `agent-browser find --help` (0.41.2) says "nth <index> <selector>  Nth matching element (0-based)", and its `handle_nth` (cli/src/native/actions.rs) maps a negative index to `length + index`. The earlier WebKitGTK test used "nth 2" for the second input; it now uses "nth 1". On the command line a negative index needs `--` (`find nth -- -1 input`), because the registry parser reads "-1" as a flag.
- `find` with an action after the locator (`find text Save click`) is a usage error until W3b adds actions.
- The hit-test command file is `internal/commands/hittest.go`: Go treats `hit_test.go` (the name in design 7.5) as a test file.
- `diff snapshot` compares the last two snapshots of the session (design 4.2). agent-browser instead takes a new snapshot and compares it with a baseline, and removes `ref=` annotations before comparing. The text format follows agent-browser's: unified diff, three lines of context, headers "before" and "after", then "N additions, M removals, K unchanged", or "No changes detected".
- `get attr` of a missing attribute prints nothing and returns `{"value": null}` with --json.
- `read <url>` fetches on the host with `golang.org/x/net/html`. golang.org/x/net v0.55.0 was already in the module graph (indirect); it is now a direct dependency (go.mod only moved the line). The reader skips head, script, style, noscript, template, svg, iframe, object, select and elements with `hidden` or `aria-hidden="true"`; it reads at most 10 MiB.
- Two W1a tests in internal/commands/commands_test.go counted all registered MCP tools and assumed that only the test fixtures existed. They now count the production tools first and still require exactly the fixture entries on top.

- Visibility adds one rule to design 3.4: content of a closed `<details>` other than its first `<summary>` is hidden. WebKit hides it inside the details element's user-agent shadow root, which page scripts cannot walk, and getBoundingClientRect still reports a box there. Playwright's fallback for engines without checkVisibility checks `<details>` the same way.

## WebKitGTK suite (e2e/webkitgtk, build tag webkitgtk)
- Run: `go test -tags=webkitgtk ./e2e/webkitgtk/ -v -count=1`. Needs MiniBrowser, broadwayd, gcc and pkg-config with the gdk-3.0 headers. The suite kills every process it starts.
- Under broadwayd, WebKitGTK 2.54 lays pages out with page zoom -1/96 (innerWidth -98304, every box empty, elementFromPoint null). Cause: WebKit sets the page zoom to the font DPI / 96 and reads the DPI from `gdk_screen_get_resolution`, which broadway leaves at -1 (WebKit SystemSettingsManagerProxyGtk.cpp and WebKitWebViewBase.cpp refreshInternalScaling; GTK applies gtk-xft-dpi to the resolution only when the application sets it, so GDK_DPI_SCALE and settings.ini do not help). The harness compiles a one-function shim that returns 96 and starts MiniBrowser with LD_PRELOAD. `screen.width` and `screen.height` stay 0; no test reads them. The proofs harness (e2e/proofs) has the same layout problem if a later proof needs geometry.
- `navigate` also waits for the document generation to change: on the same URL the readiness check was true in the old document, and a late frameNavigated made the refs of the next snapshot stale (seen once in TestConcurrentSnapshots).

## Size measurements (internal/runtime/size_test.go)
- Bundle source: within 64 KiB (TestBundleWithinSourceBudget).
- Install message (`Runtime.evaluate` of the bundle, Target-routed): about 51 KB on the wire. Bootstrap message with the runtime part and an 8 KiB escape-heavy init script: about 79 KB on the wire. Both fit WebKitGTK (127 KiB), iwdp (64 MiB) and direct (16 MiB).
- 40 KiB of control characters keep the raw bootstrap under 127 KiB, but the wire form is far above it: the send fails before writing with webkit.WireLimitError ("<message> is N bytes on the wire; the limit for this transport is M").
