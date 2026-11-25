package commands

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/webkit"
)

// Navigation commands of design docs/design/agentic-parity.md, sections
// 3.6 and 4.2 (agent-browser open, back, forward, reload, pushstate).
// Page.navigate is never used: iOS 26.4 and WebKitGTK 2.54 removed it.
// A command that navigates on purpose first withdraws the dialog answers
// that the current document holds and has not used (design 3.7): open,
// back and forward in the same runtime call that starts the navigation,
// reload in a first phase whose reply must arrive before Page.reload.
func init() {
	waitFlag := Flag{
		Name: "wait", Type: FlagEnum, Enum: []string{"load", "domcontentloaded", "none"}, Default: "load",
		Help: "after the navigation, wait for this load state of the new document (none: return at once)",
	}
	nav := func(c *Command) {
		c.Page, c.Mutates, c.Output = LeasedPage, true, OutputText
		c.Flags = append([]Flag{waitFlag}, c.Flags...)
		Register(c)
	}
	nav(&Command{
		Name: "open", Aliases: []string{"goto", "navigate"},
		Summary:    "Open a URL in the page (location.href), then wait for the load state; a URL that does not start with http:, https:, about:, data: or file: gets https:// (agent-browser open).",
		Args:       []Arg{{Name: "url", Help: "the URL to open; or --url-file"}},
		Flags:      []Flag{urlFileFlag},
		ClientStep: urlFromFile,
		Run:        runOpen,
	})
	nav(&Command{
		Name:    "back",
		Summary: "Go back in the page's history (history.back), then wait for the load state.",
		Run:     historyRun("back", -1),
	})
	nav(&Command{
		Name:    "forward",
		Summary: "Go forward in the page's history (history.forward), then wait for the load state.",
		Run:     historyRun("forward", 1),
	})
	nav(&Command{
		Name:    "reload",
		Summary: "Reload the page (Page.reload after withdrawing unused dialog answers), then wait for the load state.",
		Flags:   []Flag{{Name: "ignore-cache", Type: FlagBool, Help: "reload without the cache (Page.reload ignoreCache)"}},
		Run:     runReload,
	})
	Register(&Command{
		Name:    "pushstate",
		Summary: "Change the URL without loading: the Next.js router when the page has one, else history.pushState and a popstate event.",
		Args:    []Arg{{Name: "url", Help: "the new URL (absolute or relative to the page)", Required: true}},
		Page:    LeasedPage, Mutates: true, Output: OutputText,
		Run: runPushState,
	})
}

// navStartBudget bounds the runtime call that starts a navigation: a
// committed navigation can destroy the target before it answers (design
// 3.6), so its reply may be lost.
const navStartBudget = time.Second

// notNavigated is the end of a navigation wait in which the page kept its
// document and its URL href. For open --url-file (urlFile: the file's
// path) it names no URL: the page can still show the URL of the file (the
// same link opened again), and the error would print it.
type notNavigated struct{ href, urlFile string }

func (n notNavigated) Error() string {
	if n.urlFile != "" {
		return "the page did not navigate; it still shows the URL it showed before (not printed: the target is " + fileURLName(n.urlFile) + ")"
	}
	return "the page did not navigate; URL is still " + n.href
}

// navPoll is how often a navigation wait checks for the new document.
const navPoll = 50 * time.Millisecond

// openSchemes are the URL prefixes that open passes unchanged, as
// agent-browser's open does (cli/src/commands.rs normalize_navigation_url,
// commit 6682da3); its chrome:// and chrome-extension:// are Chrome's.
// Every other URL gets https:// in front, so open never runs a
// javascript: URL in the page.
var openSchemes = []string{"http://", "https://", "about:", "data:", "file:"}

// absoluteURL is the URL that open navigates to: raw when it starts with
// one of openSchemes (any case), else https:// and raw (agent-browser
// open). It normalizes before it parses, as agent-browser does: a
// schemeless "127.0.0.1:8080/path" does not parse alone (its first path
// segment holds a colon), and with https:// in front it is a host and a
// port. A result that does not parse is a usage error.
func absoluteURL(raw string) (string, error) {
	u := "https://" + raw
	lower := strings.ToLower(raw)
	for _, p := range openSchemes {
		if strings.HasPrefix(lower, p) {
			u = raw
			break
		}
	}
	if _, err := url.Parse(u); err != nil {
		return "", usagef("open %q: %v", raw, err)
	}
	return u, nil
}

// navMark is what a navigation wait compares against: the document
// generation and URL before the navigation started. urlFile is the path
// of open --url-file, whose wait names no URL (notNavigated).
type navMark struct {
	gen     uint64
	href    string
	urlFile string
}

func (w *w3bPage) mark(ctx context.Context) navMark {
	return navMark{gen: w.rt.Generation(), href: w.href(ctx)}
}

// href reads location.href; "" when the page does not answer within 1 s
// (a navigation in progress).
func (w *w3bPage) href(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	raw, err := w.caller.Send(cctx, "Runtime.evaluate", map[string]any{"expression": "location.href", "returnByValue": true})
	if err != nil {
		return ""
	}
	s, _ := evalValue[string](raw)
	return s
}

// startError reports whether the error of a navigation's first call ends
// the command: the call was never sent, the page refused it, or the
// operation ended. A lost reply does not: the navigation may have started.
func startError(ctx context.Context, err error) bool {
	return err != nil && (errors.Is(err, webkit.ErrNotSent) || stopWaiting(ctx, err))
}

// navigated waits for the result of a navigation (design 3.6): first a new
// document generation or a changed URL, then the load state mode. It then
// issues the session's queued dialog answers into the new document
// (design 3.7) and returns the final URL and title.
func (w *w3bPage) navigated(ctx context.Context, mode string, m navMark) (Result, error) {
	what := "--load " + mode
	if mode != "none" {
		start, end := time.Now(), waitEnd(ctx)
		for w.rt.Generation() == m.gen {
			if h := w.href(ctx); h != "" && h != m.href {
				break
			}
			if time.Now().After(end) || ctx.Err() != nil {
				return Result{}, &core.CodedError{Code: ExitWaitTimeout, Err: fmt.Errorf("wait %s timed out after %s: %w", what, time.Since(start).Round(time.Millisecond), notNavigated{m.href, m.urlFile})}
			}
			sleepUntil(ctx, earliest(time.Now().Add(navPoll), end))
		}
		if _, err := w.waitSlices(ctx, what, map[string]any{"k": "load", "load": mode}); err != nil {
			return Result{}, err
		}
	}
	w.st.mu.Lock()
	issue := w.st.dialogs.active && w.st.dialogs.pendingWork()
	w.st.mu.Unlock()
	var lines []string
	if issue && mode != "none" {
		if _, err := w.dialogSync(ctx, map[string]any{}); err != nil {
			lines = append(lines, "warning: issuing the armed dialog answers into the new document: "+err.Error())
		}
	}
	href := w.href(ctx)
	var title string
	if raw, err := w.caller.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "document.title", "returnByValue": true}); err == nil {
		title, _ = evalValue[string](raw)
	}
	lines = append([]string{"navigated to " + href}, lines...)
	if mode == "none" {
		lines[0] = "navigation started"
	}
	lines = append(lines, w.notes()...)
	return Result{Text: strings.Join(lines, "\n"), Data: map[string]any{"url": href, "title": title}}, nil
}

// startNav runs the runtime call that withdraws the unused dialog answers
// and starts the navigation (extra: nav or hist), within navStartBudget.
func (w *w3bPage) startNav(ctx context.Context, extra map[string]any) error {
	extra["withdraw"] = true
	cctx, cancel := context.WithTimeout(ctx, navStartBudget)
	defer cancel()
	_, err := w.dialogSync(cctx, extra)
	if startError(ctx, err) {
		return err
	}
	return nil
}

// runOpen names the URL in its errors as the caller gave it, or, for a
// URL from --url-file, by the file's path (hideFileURL); the result of a
// navigation ("navigated to <url>") is not an error and prints the URL.
func runOpen(ctx context.Context, env Env, in Input) (_ Result, err error) {
	if err := needURL(in, "open"); err != nil {
		return Result{}, err
	}
	raw, file := arg(in, 0), urlFile(in)
	var u string
	defer func() { err = hideFileURL(err, file, u, raw) }()
	u, err = absoluteURL(raw)
	if err != nil {
		if file != "" {
			// The parser's error repeats the URL, and parts of it.
			return Result{}, usagef("open: %s is not a valid URL", fileURLName(file))
		}
		return Result{}, err
	}
	w, err := openPage(ctx, env, "open")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	// A navigation works on the top document and ends the frame scope.
	leaveFrame(w.ps, w.op, true)
	m := w.mark(ctx)
	m.urlFile = file
	if err := w.startNav(ctx, map[string]any{"nav": u}); err != nil {
		return Result{}, fmt.Errorf("open %s: %w", u, err)
	}
	return w.navigated(ctx, flagString(in, "wait"), m)
}

func historyRun(name string, delta int) func(context.Context, Env, Input) (Result, error) {
	return func(ctx context.Context, env Env, in Input) (Result, error) {
		w, err := openPage(ctx, env, name)
		if err != nil {
			return Result{}, err
		}
		defer w.close()
		leaveFrame(w.ps, w.op, true)
		m := w.mark(ctx)
		if err := w.startNav(ctx, map[string]any{"hist": delta}); err != nil {
			return Result{}, fmt.Errorf("%s: %w", name, err)
		}
		res, err := w.navigated(ctx, flagString(in, "wait"), m)
		var nn notNavigated
		if errors.As(err, &nn) {
			// Device check of 2026-11-11 (iOS 16.7): a forward sent at
			// once after a back across a cross-site navigation started no
			// navigation in 2 of 5 runs, without an error. The move cannot
			// be asked for again safely: a move that WebKit accepted and
			// has not started yet would then move twice.
			return Result{}, &core.CodedError{Code: ExitWaitTimeout, Err: fmt.Errorf("%w. WebKit can ignore a history move that arrives while the page still settles after another navigation; check the URL ('get url') and run '%s' again", err, name)}
		}
		return res, err
	}
}

// runReload is one exclusive operation in two phases (design 3.6): (1) a
// runtime call withdraws the unused dialog answers of the current
// document; (2) only after its reply, Page.reload. When the reply of phase
// 1 is lost, the withdrawn answers expire (dialogSync) and phase 2 still
// runs while time remains.
func runReload(ctx context.Context, env Env, in Input) (Result, error) {
	w, err := openPage(ctx, env, "reload")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	// A navigation works on the top document and ends the frame scope.
	leaveFrame(w.ps, w.op, true)
	m := w.mark(ctx)
	cctx, cancel := context.WithTimeout(ctx, navStartBudget)
	_, err = w.dialogSync(cctx, map[string]any{"withdraw": true})
	cancel()
	if err != nil && ctx.Err() != nil {
		return Result{}, err
	}
	if _, err := w.caller.Send(ctx, "Page.reload", map[string]any{"ignoreCache": flagBool(in, "ignore-cache")}); startError(ctx, err) {
		return Result{}, fmt.Errorf("reload: %w", err)
	}
	return w.navigated(ctx, flagString(in, "wait"), m)
}

func runPushState(ctx context.Context, env Env, in Input) (Result, error) {
	w, err := openPage(ctx, env, "pushstate")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	var how string
	if err := w.rt.Call(ctx, "pushstate", map[string]any{"url": arg(in, 0)}, &how); err != nil {
		return Result{}, err
	}
	href := w.href(ctx)
	return Result{Text: fmt.Sprintf("URL is now %s (%s)", href, how), Data: map[string]any{"url": href, "method": how}}, nil
}
