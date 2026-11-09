package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	reg "github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/daemon"
	"github.com/nnemirovsky/iwdp-mcp/internal/proxy"
	"github.com/nnemirovsky/iwdp-mcp/internal/tools"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// commands maps each command name to its handler. Every name has a help
// entry in helpSections (pinned by TestEveryCommandHasHelp).
var commands = map[string]func(context.Context, []string){
	// Core
	"devices":       func(ctx context.Context, _ []string) { cmdDevices(ctx) },
	"pages":         cmdPages,
	"status":        func(ctx context.Context, _ []string) { cmdStatus(ctx) },
	"restart-iwdp":  func(ctx context.Context, _ []string) { cmdRestartIWDP(ctx) },
	"eval":          cmdEval,
	"navigate":      cmdNavigate,
	"reload":        cmdReload,
	"screenshot":    cmdScreenshot,
	"snapshot-node": cmdSnapshotNode,
	"cookies":       cmdCookies,
	"dom":           cmdDOM,
	"console":       cmdConsole,
	"network":       cmdNetwork,

	// Runtime
	"call-function":  cmdCallFunction,
	"get-properties": cmdGetProperties,

	// DOM
	"query-selector":      cmdQuerySelector,
	"query-selector-all":  cmdQuerySelectorAll,
	"get-outer-html":      cmdGetOuterHTML,
	"get-attributes":      cmdGetAttributes,
	"get-event-listeners": cmdGetEventListeners,
	"highlight-node":      cmdHighlightNode,
	"hide-highlight":      cmdHideHighlight,

	// CSS
	"get-matched-styles":  cmdGetMatchedStyles,
	"get-computed-style":  cmdGetComputedStyle,
	"get-inline-styles":   cmdGetInlineStyles,
	"set-style-text":      cmdSetStyleText,
	"get-all-stylesheets": cmdGetAllStylesheets,
	"get-stylesheet-text": cmdGetStylesheetText,
	"force-pseudo-state":  cmdForcePseudoState,

	// Interaction
	"click":     cmdClick,
	"fill":      cmdFill,
	"type-text": cmdTypeText,

	// Storage
	"set-cookie":                  cmdSetCookie,
	"delete-cookie":               cmdDeleteCookie,
	"get-local-storage":           cmdGetLocalStorage,
	"set-local-storage-item":      cmdSetLocalStorageItem,
	"remove-local-storage-item":   cmdRemoveLocalStorageItem,
	"clear-local-storage":         cmdClearLocalStorage,
	"get-session-storage":         cmdGetSessionStorage,
	"set-session-storage-item":    cmdSetSessionStorageItem,
	"remove-session-storage-item": cmdRemoveSessionStorageItem,
	"clear-session-storage":       cmdClearSessionStorage,
	"list-indexed-databases":      cmdListIndexedDatabases,
	"get-indexed-db-data":         cmdGetIndexedDBData,
	"clear-indexed-db-store":      cmdClearIndexedDBStore,

	// Network (extended)
	"get-response-body":        cmdGetResponseBody,
	"set-extra-headers":        cmdSetExtraHeaders,
	"set-request-interception": cmdSetRequestInterception,
	"intercept-continue":       cmdInterceptContinue,
	"intercept-respond":        cmdInterceptWithResponse,
	"set-network-conditions":   cmdSetEmulatedConditions,
	"disable-cache":            cmdSetResourceCachingDisabled,

	// Console (extended)
	"console-messages": cmdConsoleMessages,
	"clear-console":    cmdClearConsole,
	"set-log-level":    cmdSetLogLevel,

	// Debugger
	"debugger-enable":         cmdDebuggerEnable,
	"debugger-disable":        cmdDebuggerDisable,
	"set-breakpoint":          cmdSetBreakpoint,
	"remove-breakpoint":       cmdRemoveBreakpoint,
	"pause":                   cmdPause,
	"resume":                  cmdResume,
	"step-over":               cmdStepOver,
	"step-into":               cmdStepInto,
	"step-out":                cmdStepOut,
	"get-script-source":       cmdGetScriptSource,
	"search-in-content":       cmdSearchInContent,
	"eval-on-frame":           cmdEvalOnFrame,
	"set-pause-on-exceptions": cmdSetPauseOnExceptions,

	// DOMDebugger
	"set-dom-breakpoint":      cmdSetDOMBreakpoint,
	"remove-dom-breakpoint":   cmdRemoveDOMBreakpoint,
	"set-event-breakpoint":    cmdSetEventBreakpoint,
	"remove-event-breakpoint": cmdRemoveEventBreakpoint,
	"set-url-breakpoint":      cmdSetURLBreakpoint,
	"remove-url-breakpoint":   cmdRemoveURLBreakpoint,

	// Performance & Profiling
	"timeline-record": cmdTimelineRecord,
	"memory-track":    cmdMemoryTrack,
	"heap-snapshot":   cmdHeapSnapshot,
	"heap-track":      cmdHeapTrack,
	"heap-gc":         cmdHeapGC,
	"cpu-profile":     cmdCPUProfile,
	"script-profile":  cmdScriptProfile,

	// Animation
	"animation-enable":     cmdAnimationEnable,
	"animation-disable":    cmdAnimationDisable,
	"animation-track":      cmdAnimationTrack,
	"get-animation-effect": cmdGetAnimationEffect,
	"resolve-animation":    cmdResolveAnimation,

	// Canvas
	"canvas-enable":          cmdCanvasEnable,
	"canvas-disable":         cmdCanvasDisable,
	"get-canvas-content":     cmdGetCanvasContent,
	"start-canvas-recording": cmdStartCanvasRecording,
	"stop-canvas-recording":  cmdStopCanvasRecording,
	"get-shader-source":      cmdGetShaderSource,

	// LayerTree
	"get-layer-tree":          cmdGetLayerTree,
	"get-compositing-reasons": cmdGetCompositingReasons,

	// Workers
	"worker-enable":           cmdWorkerEnable,
	"worker-disable":          cmdWorkerDisable,
	"send-to-worker":          cmdSendToWorker,
	"get-service-worker-info": cmdGetServiceWorkerInfo,

	// Audit & Security
	"run-audit":            cmdRunAudit,
	"get-certificate-info": cmdGetCertificateInfo,

	// Browser
	"browser-extensions-enable":  cmdBrowserExtensionsEnable,
	"browser-extensions-disable": cmdBrowserExtensionsDisable,
}

func main() {
	args, opts, err := parseGlobalOptions(os.Args[1:], os.Getenv(timeoutEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	globals = opts
	if len(args) == 0 {
		printUsage(os.Stderr)
		os.Exit(1)
	}

	name, cmdArgs := args[0], args[1:]
	switch name {
	case "help", "--help", "-h":
		if len(cmdArgs) == 0 {
			printUsage(os.Stdout)
			return
		}
		if !printCommandHelp(os.Stdout, strings.Join(cmdArgs, " ")) {
			fmt.Fprintf(os.Stderr, "Unknown command: %s\n", strings.Join(cmdArgs, " "))
			os.Exit(1)
		}
		return
	}

	run, ok := commands[name]
	if ok {
		// A first word that names a legacy command and registry commands
		// (console, network, cookies) goes to the registry when the
		// registry owns the line: a registry command longer than that word
		// (for example "network requests"), or the registry command of
		// that word alone when the registry parses the whole line. Every
		// other line stays with the legacy command, so its forms and flags
		// work as before (design 7.3 test 13). The words skip the
		// registry's global flags, which may stand between them (design
		// 2.11: "network --json requests" is "network requests").
		// A line that names its page by URL (a ws:// argument or
		// IWDP_WS_URL) keeps the legacy command of a word that is also a
		// registry command alone (screenshot, click, reload): only legacy
		// commands connect by URL (design 7.3 test 13).
		if c, n := reg.Lookup(reg.CommandWords(args)); c != nil {
			if _, err := reg.Parse(os.Args[1:], os.Getenv); n > 1 || (err == nil && getWSURL(cmdArgs) == "") {
				ok = false
			}
		}
	}
	if !ok && isRegistryCommand(args) {
		// Registry commands parse their own global flags (design 2.11).
		code := runRegistry(os.Args[1:])
		exits.finish(func() int { return code })
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", name)
		printUsage(os.Stderr)
		os.Exit(1)
	}
	// Help must never connect to a page or write files.
	if wantsHelp(cmdArgs) {
		printCommandHelp(os.Stdout, name)
		return
	}

	budget, unlimited, err := commandBudget(name, cmdArgs, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := current.begin(name, opts.timeout, budget, unlimited)
	defer cancel()
	cleanupOnSignal()
	run(ctx, cmdArgs)
	exits.finish(func() int { return 0 })
}

func getPort(args []string) int {
	if len(args) > 0 {
		if p, err := strconv.Atoi(args[0]); err == nil {
			return p
		}
	}
	if p := os.Getenv("IWDP_PORT"); p != "" {
		if port, err := strconv.Atoi(p); err == nil {
			return port
		}
	}
	return proxy.DefaultFirstDevicePort
}

func getWSURL(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "ws://") || strings.HasPrefix(a, "wss://") {
			return a
		}
	}
	if u := os.Getenv("IWDP_WS_URL"); u != "" {
		return u
	}
	return ""
}

// pageClient is a page connection of a legacy command: the daemon-backed
// caller (design 2.10, legacy bridge).
type pageClient interface {
	webkit.Caller
	Close() error
}

// controlCommands send only control-lane messages (design 2.10). They
// lease the page without an operation, so they never wait behind the
// operation they let finish (an evaluation paused in the debugger, an
// awaited fetch held by interception).
var controlCommands = map[string]bool{"resume": true, "intercept-continue": true, "intercept-respond": true}

// connectToPage opens the page in the daemon (started on demand) and
// begins the command's operation on it: the ws:// URL among args or
// IWDP_WS_URL, else the first debuggable page of the --transport.
func connectToPage(ctx context.Context, args []string) (pageClient, error) {
	pr := daemon.PageReq{Transport: globals.transport}
	if wsURL := getWSURL(args); wsURL != "" {
		current.setPage(wsURL)
		pr.Page = wsURL
	}
	c, err := daemon.Connect(ctx, daemonOptions())
	if err != nil {
		return nil, err
	}
	c.Logf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, "warning: "+f+"\n", a...) }
	open := c.OpenPage
	if controlCommands[current.commandName()] {
		open = c.AttachPage
	}
	pc, err := open(ctx, daemon.PageOptions{Page: pr, Owner: globals.owner(), Name: current.commandName()})
	if err != nil {
		c.Close()
		return nil, err
	}
	if pr.Page == "" {
		// The page is named only in error messages, never on every
		// command: page titles must not land in agent transcripts and logs.
		info := pc.Info()
		current.setPage(fmt.Sprintf("%q (%s)", info.Title, info.Key))
	}
	return pc, nil
}

// endOperation ends the command's page operation early, so that a long
// stream does not hold the page for the session's other commands; later
// calls run one at a time.
func endOperation(c pageClient) {
	if pc, ok := c.(*daemon.PageCaller); ok {
		pc.EndOp()
	}
}

// requireIWDP ends the command unless iwdp's listing answers. Only a
// listing port that nothing owns means that iwdp is not running (exit 1);
// an owner that does not answer gets the check's error, which says how it
// failed (an iwdp can close listing requests while a page is connected,
// docs/known-limits.md), with the exit code of a listing failure (5).
func requireIWDP(ctx context.Context) {
	err := proxy.CheckListing(ctx, proxy.ListingAddr())
	if err == nil {
		return
	}
	if errors.Is(err, proxy.ErrNotListening) {
		fmt.Fprintf(os.Stderr, "ios_webkit_debug_proxy is not running.\nStart it with: %s\n", proxy.ManualCommand())
		os.Exit(1)
	}
	fatal(&core.CodedError{Code: core.CodeNoPage, Err: err})
}

// listingReadFailed ends a legacy listing command whose read of addr (the
// device list, or a device's page list) failed after requireIWDP passed,
// classified as requireIWDP does (proxy.ReadFailure): err itself with
// exit 1 when nothing owns addr, otherwise how the owner failed, with the
// exit code of a listing failure (5).
func listingReadFailed(ctx context.Context, addr string, err error) {
	if cerr := proxy.ReadFailure(ctx, addr, err); !errors.Is(cerr, proxy.ErrNotListening) {
		err = &core.CodedError{Code: core.CodeNoPage, Err: cerr}
	}
	fatal(err)
}

func cmdDevices(ctx context.Context) {
	requireIWDP(ctx)
	devices, err := proxy.ListDevices()
	if err != nil {
		listingReadFailed(ctx, proxy.ListingAddr(), err)
	}
	if len(devices) == 0 {
		fmt.Println("No devices connected.")
		return
	}
	for _, d := range devices {
		fmt.Printf("%-40s  %s  (pages on %s)\n", d.DeviceName, d.DeviceID, d.URL)
	}
}

func cmdPages(ctx context.Context, args []string) {
	port := getPort(args)
	requireIWDP(ctx)
	pages, err := proxy.ListPages(port)
	if err != nil {
		listingReadFailed(ctx, proxy.DeviceAddr(port), err)
	}
	if len(pages) == 0 {
		fmt.Println("No Safari tabs found.")
		return
	}
	for _, p := range pages {
		ws := p.WebSocketDebuggerURL
		if ws == "" {
			ws = "(not debuggable)"
		}
		fmt.Printf("[%d] %s\n    %s\n    %s\n", p.PageID, p.Title, p.URL, ws)
	}
}

func cmdEval(ctx context.Context, args []string) {
	awaitPromise := hasFlag(args, "--await")
	var rest []string
	for _, a := range args {
		if a != "--await" {
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli eval [--await] <expression> [ws-url]")
		os.Exit(1)
	}

	expr := rest[0]
	client, err := connectToPage(ctx, rest[1:])
	if err != nil {
		fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: closing connection: %v\n", err)
		}
	}()

	result, err := tools.EvaluateScriptWithOptions(ctx, client, expr, tools.EvalOptions{ReturnByValue: true, AwaitPromise: awaitPromise})
	if err != nil {
		fatal(err) // a *tools.JSException prints its message and stack
	}

	if result.Result.Value != nil {
		var val interface{}
		_ = json.Unmarshal(result.Result.Value, &val)
		switch v := val.(type) {
		case string:
			fmt.Println(v)
		default:
			out, _ := json.MarshalIndent(val, "", "  ")
			fmt.Println(string(out))
		}
	} else if result.Result.Description != "" {
		fmt.Println(result.Result.Description)
	} else {
		fmt.Printf("(%s)\n", result.Result.Type)
	}
}

func cmdNavigate(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: iwdp-cli navigate <url> [ws-url]")
		os.Exit(1)
	}

	url := args[0]
	client, err := connectToPage(ctx, args[1:])
	if err != nil {
		fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: closing connection: %v\n", err)
		}
	}()

	if err := tools.Navigate(ctx, client, url); err != nil {
		fatal(err)
	}
	fmt.Println("Navigated to", url)
}

func cmdScreenshot(ctx context.Context, args []string) {
	const usage = "Usage: iwdp-cli screenshot [--full-page | --selector <css>] [-o file] [ws-url]"
	var opts tools.ScreenshotOptions
	outputFile := ""
	var wsArgs []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-o", "--output", "--selector":
			if i+1 >= len(args) {
				usageError(usage, fmt.Errorf("%s needs a value", a))
			}
			if a == "--selector" {
				opts.Selector = args[i+1]
			} else {
				outputFile = args[i+1]
			}
			i++
		case "--full-page":
			opts.FullPage = true
		default:
			if strings.HasPrefix(a, "-") {
				usageError(usage, fmt.Errorf("unknown option %s", a))
			}
			wsArgs = append(wsArgs, a)
		}
	}
	if opts.FullPage && opts.Selector != "" {
		usageError(usage, errors.New("use either --full-page or --selector, not both"))
	}

	client, err := connectToPage(ctx, wsArgs)
	if err != nil {
		fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: closing connection: %v\n", err)
		}
	}()

	dataURL, err := tools.CaptureScreenshot(ctx, client, opts)
	if err != nil {
		fatal(err)
	}
	path, info, err := saveCapture(dataURL, outputFile, "iwdp-screenshot")
	if err != nil {
		fatal(err)
	}
	fmt.Printf("Screenshot saved to %s (%s)\n", path, info)
}

// captureTimestamp names default capture files; milliseconds keep quick
// successive captures apart.
const captureTimestamp = "20060102-150405.000"

// saveCapture decodes a PNG data URL and writes it to outputFile, or, when
// outputFile is empty, to a new file $TMPDIR/<prefix>-<timestamp>.png. It
// never writes into the current directory unless asked to. It returns the
// path and a description of the pixel and byte size.
func saveCapture(dataURL, outputFile, prefix string) (string, string, error) {
	b64Data := dataURL
	if idx := strings.Index(dataURL, ","); idx >= 0 {
		b64Data = dataURL[idx+1:]
	}
	data, err := base64.StdEncoding.DecodeString(b64Data)
	if err != nil {
		return "", "", fmt.Errorf("decoding capture: %w", err)
	}

	path := outputFile
	if path != "" {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", "", err
		}
	} else {
		path, err = writeNewTempFile(prefix, data)
		if err != nil {
			return "", "", err
		}
	}

	size := fmt.Sprintf("%d bytes", len(data))
	if cfg, err := png.DecodeConfig(bytes.NewReader(data)); err == nil {
		size = fmt.Sprintf("%dx%d px, %s", cfg.Width, cfg.Height, size)
	} else {
		size = fmt.Sprintf("pixel size unknown: %v; %s", err, size)
	}
	return path, size, nil
}

// writeNewTempFile writes data to $TMPDIR/<prefix>-<timestamp>.png without
// replacing an existing file. Captures can show secrets, so the file is
// private to the user.
func writeNewTempFile(prefix string, data []byte) (string, error) {
	name := prefix + "-" + time.Now().Format(captureTimestamp)
	path := filepath.Join(os.TempDir(), name+".png")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		f, err = os.CreateTemp(os.TempDir(), name+"-*.png")
	}
	if err != nil {
		return "", fmt.Errorf("creating capture file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("writing %s: %w", f.Name(), err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("writing %s: %w", f.Name(), err)
	}
	return f.Name(), nil
}

func cmdCookies(ctx context.Context, args []string) {
	showValues := false
	var wsArgs []string
	for _, a := range args {
		switch {
		case a == "--show-values":
			showValues = true
		case strings.HasPrefix(a, "-"):
			usageError("Usage: iwdp-cli cookies [--show-values] [ws-url]", fmt.Errorf("unknown option %s", a))
		default:
			wsArgs = append(wsArgs, a)
		}
	}

	client, err := connectToPage(ctx, wsArgs)
	if err != nil {
		fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: closing connection: %v\n", err)
		}
	}()

	cookies, err := tools.GetCookies(ctx, client)
	if err != nil {
		fatal(err)
	}

	if len(cookies) == 0 {
		fmt.Println("No cookies.")
		return
	}
	if !showValues {
		cookies = tools.RedactCookies(cookies)
		fmt.Fprintln(os.Stderr, "Cookie values are redacted; pass --show-values to print them.")
	}

	for _, c := range cookies {
		fmt.Println(formatCookie(c))
	}
}

// formatCookie renders a cookie as one line: name=value and its attributes.
func formatCookie(c webkit.Cookie) string {
	flags := ""
	if c.HTTPOnly {
		flags += " httpOnly"
	}
	if c.Secure {
		flags += " secure"
	}
	if c.Session {
		flags += " session"
	}
	line := fmt.Sprintf("%s=%s  domain=%s path=%s", c.Name, c.Value, c.Domain, c.Path)
	if c.SameSite != "" {
		line += " sameSite=" + c.SameSite
	}
	if !c.Session && c.Expires > 0 {
		line += " expires=" + time.UnixMilli(int64(c.Expires)).UTC().Format(time.RFC3339)
	}
	return line + flags
}

func cmdDOM(ctx context.Context, args []string) {
	client, err := connectToPage(ctx, args)
	if err != nil {
		fatal(err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: closing connection: %v\n", err)
		}
	}()

	selector := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "ws://") && !strings.HasPrefix(args[0], "wss://") {
		selector = args[0]
	}

	if selector != "" {
		// Get document root first
		doc, err := tools.GetDocument(ctx, client, 1)
		if err != nil {
			fatal(err)
		}
		nodeID, err := tools.QuerySelector(ctx, client, doc.NodeID, selector)
		if err != nil {
			fatal(err)
		}
		html, err := tools.GetOuterHTML(ctx, client, nodeID)
		if err != nil {
			fatal(err)
		}
		fmt.Println(html)
	} else {
		doc, err := tools.GetDocument(ctx, client, 3)
		if err != nil {
			fatal(err)
		}
		out, _ := json.MarshalIndent(doc, "", "  ")
		fmt.Println(string(out))
	}
}

// streamDisableTimeout bounds the Console.disable or Network.disable call at
// the end of a stream; the page may be gone by then.
const streamDisableTimeout = 5 * time.Second

// streamCleanupAfterDeadline bounds the disable call when the command
// deadline has already passed. It is shorter than watchdogGrace, so the
// deadline error comes from fatal and not from the watchdog.
const streamCleanupAfterDeadline = time.Second

// streamOptions parses the flags of console and network.
func streamOptions(command string, args []string) (duration time.Duration, follow bool, wsArgs []string) {
	usage := fmt.Sprintf("Usage: iwdp-cli %s [-d 10s] [--follow] [ws-url]", command)
	duration, rest, err := recordingDuration(command, args)
	if err != nil {
		usageError(usage, err)
	}
	for _, a := range rest {
		switch {
		case a == "--follow" || a == "-f":
			follow = true
		case strings.HasPrefix(a, "-"):
			usageError(usage, fmt.Errorf("unknown option %s", a))
		default:
			wsArgs = append(wsArgs, a)
		}
	}
	return duration, follow, wsArgs
}

// streamSetupContext bounds the connect and enable steps. A bounded stream
// already runs under the command deadline; a --follow stream has none, so its
// setup gets the --timeout value.
func streamSetupContext(ctx context.Context, follow bool) (context.Context, context.CancelFunc) {
	if !follow {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, current.setupTimeout())
}

// waitForStream blocks until the duration ends (unless follow), the user
// interrupts, the connection closes or ctx ends. It returns the reason, and
// an error when ctx ended the stream before the duration.
func waitForStream(ctx context.Context, client pageClient, duration time.Duration, follow bool) (string, error) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	streamWaiting.Store(true) // the stream ends on a signal, then the command

	var timeout <-chan time.Time
	if !follow {
		timer := time.NewTimer(duration)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-timeout:
		return "duration reached", nil
	case <-sig:
		return "interrupted", nil
	case <-client.Done():
		return "connection closed by the page or iwdp", nil
	case <-ctx.Done():
		return "deadline reached", fmt.Errorf("streaming ended before -d %s: %w", duration, ctx.Err())
	}
}

// endStream disables the domain and closes the connection, so the page is
// free for the next inspector session. The disable call ends at the command
// deadline, or after streamCleanupAfterDeadline when that has passed. It
// returns the disable error after printing it as a warning.
func endStream(ctx context.Context, client pageClient, disableMethod string) error {
	disableCtx, cancel := context.WithTimeout(ctx, streamDisableTimeout)
	if ctx.Err() != nil {
		cancel()
		disableCtx, cancel = context.WithTimeout(context.Background(), streamCleanupAfterDeadline)
	}
	defer cancel()
	_, disableErr := client.Send(disableCtx, disableMethod, nil)
	if disableErr != nil {
		fmt.Fprintf(os.Stderr, "warning: %s: %v\n", disableMethod, disableErr)
	}
	if err := client.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: closing connection: %v\n", err)
	}
	return disableErr
}

// finishStream exits with the deadline error when the command deadline ended
// the stream or its disable call. Other disable failures stay warnings: the
// capture already happened.
func finishStream(ctx context.Context, waitErr, disableErr error) {
	if waitErr != nil {
		fatal(waitErr)
	}
	if disableErr != nil && ctx.Err() != nil {
		fatal(disableErr)
	}
}

func cmdConsole(ctx context.Context, args []string) {
	duration, follow, wsArgs := streamOptions("console", args)
	setupCtx, cancelSetup := streamSetupContext(ctx, follow)
	defer cancelSetup()

	client, err := connectToPage(setupCtx, wsArgs)
	if err != nil {
		fatal(err)
	}

	// Register a handler that prints console messages immediately as they arrive.
	var count atomic.Int64
	client.OnEvent("Console.messageAdded", func(method string, params json.RawMessage) {
		var envelope struct {
			Message struct {
				Level  string `json:"level"`
				Text   string `json:"text"`
				Source string `json:"source"`
				URL    string `json:"url"`
				Line   int    `json:"line"`
			} `json:"message"`
		}
		if err := json.Unmarshal(params, &envelope); err != nil {
			return
		}
		if reg.IsRuntimeMessage(envelope.Message.Text) {
			return // the injected runtime's own message, not the page's
		}
		count.Add(1)
		m := envelope.Message
		loc := ""
		if m.URL != "" {
			loc = fmt.Sprintf(" (%s:%d)", m.URL, m.Line)
		}
		fmt.Printf("[%s] %s%s\n", m.Level, m.Text, loc)
	})

	if _, err := client.Send(setupCtx, "Console.enable", nil); err != nil {
		fatal(err)
	}
	endOperation(client)

	start := time.Now()
	if follow {
		fmt.Fprintln(os.Stderr, "Listening for console messages (Ctrl+C to stop)...")
	} else {
		fmt.Fprintf(os.Stderr, "Listening for console messages for %s (--follow streams until Ctrl+C)...\n", duration)
	}
	reason, waitErr := waitForStream(ctx, client, duration, follow)
	disableErr := endStream(ctx, client, "Console.disable")
	fmt.Fprintf(os.Stderr, "Captured %d console messages in %s (%s).\n", count.Load(), time.Since(start).Round(100*time.Millisecond), reason)
	finishStream(ctx, waitErr, disableErr)
}

func cmdNetwork(ctx context.Context, args []string) {
	duration, follow, wsArgs := streamOptions("network", args)
	setupCtx, cancelSetup := streamSetupContext(ctx, follow)
	defer cancelSetup()

	client, err := connectToPage(setupCtx, wsArgs)
	if err != nil {
		fatal(err)
	}

	// Register handlers that print network events immediately as they arrive.
	var requests, responses, failures atomic.Int64
	client.OnEvent("Network.requestWillBeSent", func(method string, params json.RawMessage) {
		var evt struct {
			Request struct {
				Method string `json:"method"`
				URL    string `json:"url"`
			} `json:"request"`
		}
		if err := json.Unmarshal(params, &evt); err != nil {
			return
		}
		requests.Add(1)
		fmt.Printf("→ %s %s\n", evt.Request.Method, evt.Request.URL)
	})

	client.OnEvent("Network.responseReceived", func(method string, params json.RawMessage) {
		var evt struct {
			Response struct {
				URL        string `json:"url"`
				Status     int    `json:"status"`
				StatusText string `json:"statusText"`
				MIMEType   string `json:"mimeType"`
			} `json:"response"`
		}
		if err := json.Unmarshal(params, &evt); err != nil {
			return
		}
		responses.Add(1)
		fmt.Printf("← %d %s %s (%s)\n", evt.Response.Status, evt.Response.StatusText, evt.Response.URL, evt.Response.MIMEType)
	})

	client.OnEvent("Network.loadingFailed", func(method string, params json.RawMessage) {
		var evt struct {
			RequestID string `json:"requestId"`
			ErrorText string `json:"errorText"`
			Canceled  bool   `json:"canceled"`
		}
		if err := json.Unmarshal(params, &evt); err != nil {
			return
		}
		failures.Add(1)
		if evt.Canceled {
			fmt.Printf("✕ %s (canceled)\n", evt.RequestID)
		} else {
			fmt.Printf("✕ %s: %s\n", evt.RequestID, evt.ErrorText)
		}
	})

	if _, err := client.Send(setupCtx, "Network.enable", nil); err != nil {
		fatal(err)
	}
	endOperation(client)

	start := time.Now()
	if follow {
		fmt.Fprintln(os.Stderr, "Monitoring network requests (Ctrl+C to stop)...")
	} else {
		fmt.Fprintf(os.Stderr, "Monitoring network requests for %s (--follow streams until Ctrl+C)...\n", duration)
	}
	reason, waitErr := waitForStream(ctx, client, duration, follow)
	disableErr := endStream(ctx, client, "Network.disable")
	fmt.Fprintf(os.Stderr, "Captured %d requests, %d responses, %d failures in %s (%s).\n",
		requests.Load(), responses.Load(), failures.Load(), time.Since(start).Round(100*time.Millisecond), reason)
	finishStream(ctx, waitErr, disableErr)
}

// usageError reports a bad command line and exits 1 before any connection.
func usageError(usage string, err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n%s\n", err, usage)
	os.Exit(1)
}

// fatal reports err and exits. A deadline error exits exitTimeout with the
// command, page, elapsed time and likely causes; an error with an exit code
// (page busy or lost 4, no page 5, reservation 6) exits with it. It exits
// through exits.finish, after the in-process cleanup; a signal or the
// watchdog decides the exit code instead when either came first.
func fatal(err error) {
	exits.finish(func() int {
		if current.deadlineHit(err) {
			fmt.Fprintln(os.Stderr, current.timeoutMessage(err))
			return exitTimeout
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		var ec reg.ExitCoder
		if errors.As(err, &ec) && ec.ExitCode() > 1 {
			return ec.ExitCode()
		}
		return 1
	})
}
