package commands

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/devtools"
	"github.com/nnemirovsky/agent-ios/internal/runtime"
)

// wait: the waits of design 3.6 (agent-browser "wait"). Element, text, URL
// and load waits run as in-page slices of at most waitSlice (70-waits.js);
// Go repeats slices until the wait deadline, so a navigation or a target
// swap ends one slice, not the wait. "wait --load networkidle" waits in
// the daemon on W4's network tracker (devtools.WaitNetworkIdle).
func init() {
	Register(&Command{
		Name:    "wait",
		Summary: "Wait for milliseconds, an element state, text, a URL glob, a load state or a JavaScript condition (default timeout 25 s; exit 3 on timeout).",
		Args:    []Arg{{Name: "target", Help: "milliseconds, or an element: @eN ref, CSS selector or locator"}},
		Flags: []Flag{
			{Name: "state", Type: FlagEnum, Enum: []string{"visible", "hidden", "attached", "detached"}, Default: "visible", Help: "element state to wait for"},
			{Name: "text", Type: FlagString, Help: "wait until this text is visible on the page"},
			{Name: "url", Type: FlagString, Help: "wait until the URL matches this glob (* within a path segment, ** across segments)"},
			{Name: "load", Type: FlagEnum, Enum: []string{"domcontentloaded", "load", "networkidle"}, Help: "wait until document.readyState reaches this load state, or (networkidle) until no request has been in flight for 500 ms"},
			{Name: "fn", Type: FlagString, Help: "wait until this JavaScript expression is truthy (polled every 100 ms)"},
		},
		Page:    LeasedPage,
		Timeout: waitTimeout + waitReserve,
		Output:  OutputText,
		Run:     runWait,
	})
}

const (
	// waitTimeout is the default wait (agent-browser value, design 3.6).
	waitTimeout = 25 * time.Second
	// waitReserve ends a wait this long before the command deadline, so
	// that a timed-out wait reports exit 3 with its last state instead of
	// the deadline error (124).
	waitReserve = 150 * time.Millisecond
	waitSlice   = time.Second
	waitPoll    = 100 * time.Millisecond
	// ExitWaitTimeout is the exit code of a wait that timed out (design 3.6).
	ExitWaitTimeout = 3
)

// waitTimedOut is the error of an expired wait: exit 3 and the last state
// observed (for example "URL is still https://a/b").
func waitTimedOut(what, last string, d time.Duration) error {
	if last == "" {
		last = "no answer from the page"
	}
	return &core.CodedError{Code: ExitWaitTimeout, Err: fmt.Errorf("wait %s timed out after %s: %s", what, d.Round(time.Millisecond), last)}
}

// waitEnd is when a wait under ctx must give up.
func waitEnd(ctx context.Context) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl.Add(-waitReserve)
	}
	return time.Now().Add(waitTimeout)
}

// globRegexp turns a URL glob into an anchored regular expression that
// Go's regexp and JavaScript's RegExp read the same way: "**" matches any
// text, "*" any text without "/", everything else itself. "**" is
// "[\s\S]*", not ".*": "." matches no line feed in either engine
// without the s flag, and both read "[\s\S]" as every character.
func globRegexp(glob string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); {
		switch {
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(`[\s\S]*`)
			i += 2
		case glob[i] == '*':
			b.WriteString("[^/]*")
			i++
		default:
			j := i + 1
			for j < len(glob) && glob[j] != '*' {
				j++
			}
			b.WriteString(regexp.QuoteMeta(glob[i:j]))
			i = j
		}
	}
	b.WriteString("$")
	return b.String()
}

// stopWaiting reports errors that end a wait at once: page-side errors of
// the request (bad selector, stale ref) and the end of the operation.
func stopWaiting(ctx context.Context, err error) bool {
	var re *runtime.Error
	return errors.As(err, &re) || ctx.Err() != nil || errors.Is(err, core.ErrStaleEpoch) ||
		errors.Is(err, core.ErrLeaseEnded) || errors.Is(err, core.ErrOpEnded)
}

// waitSlices repeats the page's wait slices with args until one reports
// the condition or the wait ends.
func (w *w3bPage) waitSlices(ctx context.Context, what string, args map[string]any) (string, error) {
	start, end := time.Now(), waitEnd(ctx)
	last := ""
	for {
		left := time.Until(end)
		if left <= 0 {
			return "", waitTimedOut(what, last, time.Since(start))
		}
		slice := min(waitSlice, left)
		args["slice"] = slice.Milliseconds()
		sctx, cancel := context.WithTimeout(ctx, slice+time.Second)
		var v struct {
			OK    bool   `json:"ok"`
			State string `json:"state"`
		}
		err := w.rt.CallAsync(sctx, "wait", args, &v)
		cancel()
		switch {
		case err == nil && v.OK:
			return v.State, nil
		case err == nil:
			last = v.State
		case stopWaiting(ctx, err):
			return "", err
		default:
			// A navigation or swap ended the slice; the next one runs in
			// the new document.
			last = "the page did not answer the last check (" + err.Error() + ")"
			sleepUntil(ctx, earliest(time.Now().Add(waitPoll), end))
		}
	}
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func sleepUntil(ctx context.Context, t time.Time) {
	select {
	case <-time.After(time.Until(t)):
	case <-ctx.Done():
	}
}

// waitFn polls a JavaScript expression every waitPoll until it is truthy.
func (w *w3bPage) waitFn(ctx context.Context, expr string) error {
	start, end := time.Now(), waitEnd(ctx)
	last := ""
	for time.Now().Before(end) {
		pctx, cancel := context.WithTimeout(ctx, min(time.Second, time.Until(end)))
		raw, err := w.caller.Send(pctx, "Runtime.evaluate", map[string]any{"expression": "!!(" + expr + "\n)", "returnByValue": true})
		cancel()
		if err == nil {
			ok, verr := evalValue[bool](raw)
			if verr == nil && ok {
				return nil
			}
			last = "the expression is falsy"
			if verr != nil {
				last = "the expression threw " + verr.Error()
			}
		} else if stopWaiting(ctx, err) {
			return err
		} else if last == "" || !errors.Is(err, context.DeadlineExceeded) || time.Now().Before(end) {
			// A poll that the end of the wait cut short observed nothing;
			// the state observed before it stays the last state.
			last = "the page did not answer (" + err.Error() + ")"
		}
		sleepUntil(ctx, earliest(time.Now().Add(waitPoll), end))
	}
	return waitTimedOut("--fn", last, time.Since(start))
}

// waitNetworkIdle is "wait --load networkidle" (design 3.6): no request in
// flight for devtools.DefaultIdleQuiet, by the session's network capture,
// which the wait starts when it does not run (it then cannot see requests
// that started earlier, and says so). A wait that ends before the network
// is idle reports exit 3 with the requests still in flight.
func (w *w3bPage) waitNetworkIdle(ctx context.Context) (Result, error) {
	const what = "--load networkidle"
	wctx, cancel := context.WithDeadline(ctx, waitEnd(ctx))
	defer cancel()
	res, err := devtools.WaitNetworkIdle(wctx, w.ps, w.caller, 0)
	switch {
	case err != nil && ctx.Err() == nil && wctx.Err() != nil:
		return Result{}, waitTimedOut(what, fmt.Sprintf("%d request(s) still in flight", res.InFlight), res.Waited)
	case err != nil:
		return Result{}, fmt.Errorf("wait %s: %w", what, err)
	}
	text := "done: " + what
	if res.StartedCapture {
		text += " (this wait started the network capture, so requests that began before it were not seen)"
	}
	return Result{Text: text, Data: map[string]any{"ok": true, "state": "networkidle", "startedCapture": res.StartedCapture, "waitedMs": res.Waited.Milliseconds()}}, nil
}

func runWait(ctx context.Context, env Env, in Input) (Result, error) {
	t := arg(in, 0)
	text, url, load, fn := flagString(in, "text"), flagString(in, "url"), flagString(in, "load"), flagString(in, "fn")
	forms := 0
	for _, s := range []string{t, text, url, load, fn} {
		if s != "" {
			forms++
		}
	}
	if forms != 1 {
		return Result{}, usagef("wait takes exactly one of: <ms>, <target>, --text, --url, --load, --fn")
	}
	if ms, err := strconv.Atoi(t); err == nil {
		if ms < 0 {
			return Result{}, usagef("wait %d: want a positive number of milliseconds", ms)
		}
		d := time.Duration(ms) * time.Millisecond
		if end := waitEnd(ctx); time.Now().Add(d).After(end) {
			sleepUntil(ctx, end)
			return Result{}, waitTimedOut(t+" ms", "the wait is longer than the command deadline; raise --timeout", d)
		}
		sleepUntil(ctx, time.Now().Add(d))
		return Result{Text: fmt.Sprintf("waited %d ms", ms), Data: map[string]any{"ms": ms}}, ctx.Err()
	}
	w, err := openPage(ctx, env, "wait")
	if err != nil {
		return Result{}, err
	}
	defer w.close()
	var what string
	args := map[string]any{}
	switch {
	case fn != "":
		if err := w.waitFn(ctx, fn); err != nil {
			return Result{}, err
		}
		return Result{Text: "the expression is truthy", Data: map[string]any{"ok": true}}, nil
	case text != "":
		what, args["k"], args["text"] = fmt.Sprintf("--text %q", text), "text", text
	case url != "":
		what, args["k"], args["re"] = "--url "+url, "url", globRegexp(url)
	case load == "networkidle":
		return w.waitNetworkIdle(ctx)
	case load != "":
		what, args["k"], args["load"] = "--load "+load, "load", load
	default:
		spec, err := w.spec(ctx, t)
		if err != nil {
			return Result{}, err
		}
		state := flagString(in, "state")
		if state == "" {
			state = "visible"
		}
		what, args["k"], args["t"], args["state"] = t+" --state "+state, "target", spec, state
	}
	state, err := w.waitSlices(ctx, what, args)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: "done: " + what, Data: map[string]any{"ok": true, "state": state}}, nil
}
