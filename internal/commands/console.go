package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/devtools"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// console and errors: the daemon's console buffer (design
// docs/design/agentic-parity.md, section 4.2 "Console and errors", 7.7
// test 4). The capture starts with the lease: WebKit replays its last 100
// messages on Console.enable (MAP 1.7), so the buffer holds at most those
// plus everything since the lease. There is no live stream: run
// console --since "$last_id" again, with the id of the last message seen,
// for the messages after it.
func init() {
	Register(&Command{
		Name:    "console",
		Summary: "Print the console messages captured since the lease (and up to 100 that WebKit replays). For new messages only, run it again as console --since \"$last_id\", with the id of the last message that it printed.",
		Flags: []Flag{
			{Name: "clear", Type: FlagBool, Help: "empty the buffer after printing"},
			{Name: "level", Type: FlagEnum, Enum: []string{"debug", "log", "info", "warning", "error"}, Help: "print only messages of this level or above"},
			{Name: "since", Type: FlagInt, Help: "print only messages with an id above this one"},
		},
		Page:    LeasedPage,
		Mutates: true, // starts the capture when it does not run
		Output:  OutputText,
		Run:     runConsole,
	})
	Register(&Command{
		Name:    "console clear",
		Summary: "Empty the console buffer and WebKit's stored messages.",
		Page:    LeasedPage,
		Mutates: true,
		Output:  OutputText,
		Run: func(ctx context.Context, env Env, _ Input) (Result, error) {
			d, err := devtoolsForCommand(ctx, env)
			if err != nil {
				return Result{}, err
			}
			k, err := d.p.StartConsole(ctx, d.c, d.session)
			if err != nil {
				return Result{}, err
			}
			if err := k.Clear(ctx, d.c); err != nil {
				return Result{}, err
			}
			return Result{Text: "Console buffer cleared."}, nil
		},
	})
}

// devtoolsCmd is what a DevTools command works with: the page's DevTools
// state, the caller of the command's operation (fenced by its lease epoch,
// design 2.10) and the session that holds the lease.
type devtoolsCmd struct {
	p       *devtools.Page
	c       webkit.Caller
	session string
}

// devtoolsForCommand returns the DevTools state of the command's page.
func devtoolsForCommand(ctx context.Context, env Env) (devtoolsCmd, error) {
	ps, err := env.Page(ctx)
	if err != nil {
		return devtoolsCmd{}, err
	}
	p, err := devtools.For(ps)
	if err != nil {
		return devtoolsCmd{}, err
	}
	session, err := p.Session()
	if err != nil {
		return devtoolsCmd{}, err
	}
	c := webkit.Caller(ps.Caller)
	if oe, ok := env.(interface{ Op() *core.Op }); ok && oe.Op() != nil {
		c = oe.Op().Caller()
	}
	return devtoolsCmd{p: p, c: c, session: session}, nil
}

func runConsole(ctx context.Context, env Env, in Input) (Result, error) {
	d, err := devtoolsForCommand(ctx, env)
	if err != nil {
		return Result{}, err
	}
	k, err := d.p.StartConsole(ctx, d.c, d.session)
	if err != nil {
		return Result{}, err
	}
	since, _ := in.Flags["since"].(int)
	all, dropped := k.Messages(devtools.ConsoleFilter{Level: flagString(in, "level"), Since: since})
	var msgs []devtools.Message
	for _, m := range all {
		if !IsRuntimeMessage(m.Text) {
			msgs = append(msgs, m)
		}
	}
	var b strings.Builder
	if dropped > 0 {
		fmt.Fprintf(&b, "(%d older messages were dropped; the buffer keeps the last %d)\n", dropped, devtools.DefaultMaxMessages)
	}
	for _, m := range msgs {
		b.WriteString(formatConsoleMessage(m))
		b.WriteByte('\n')
	}
	if len(msgs) == 0 {
		b.WriteString("No console messages.")
	}
	if flagBool(in, "clear") {
		if err := k.Clear(ctx, d.c); err != nil {
			return Result{}, err
		}
	}
	if msgs == nil {
		msgs = []devtools.Message{}
	}
	return Result{Text: strings.TrimRight(b.String(), "\n"), Data: map[string]any{"messages": msgs, "dropped": dropped}}, nil
}

// runtimeMarker starts the text of the injected runtime's own
// console.debug messages: the dialog override's "__iwdp_dialog__ {...}"
// records (internal/runtime 80-dialogs.js) and the CDP page hooks
// "__iwdp_nav__ " and "__iwdp_bind__ ". The CDP endpoint drops the same
// prefix (internal/cdp consoleMessage).
const runtimeMarker = "__iwdp_"

// IsRuntimeMessage reports whether a console message text is one of the
// runtime's own messages, which the console commands leave out.
func IsRuntimeMessage(text string) bool { return strings.HasPrefix(text, runtimeMarker) }

// formatConsoleMessage renders one message as "#id [level] text (url:line)"
// with its stack, if any, on the following lines.
func formatConsoleMessage(m devtools.Message) string {
	s := fmt.Sprintf("#%d [%s] %s", m.ID, m.Level, m.Text)
	switch {
	case m.URL != "":
		s += fmt.Sprintf(" (%s:%d)", m.URL, m.Line)
	case m.Line > 0:
		s += fmt.Sprintf(" (%s:%d)", devtools.AnonymousScript, m.Line)
	}
	if m.RepeatCount > 1 {
		s += fmt.Sprintf(" x%d", m.RepeatCount)
	}
	if m.Stack != "" {
		s += "\n" + m.Stack
	}
	return s
}
