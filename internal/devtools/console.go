package devtools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// DefaultMaxMessages bounds the console buffer of a session. WebKit itself
// keeps the last 100 messages and replays them on Console.enable (MAP 1.7).
const DefaultMaxMessages = 1000

// errorMarker starts the console.debug text of the error hooks (ErrorHooks).
const errorMarker = "[iwdp:error] "

// noScriptURL is the url that iOS 16.7 WebKit gives a console message and
// its stack frames for a script without a URL, such as code that
// Runtime.evaluate ran: the string "undefined" (device check of
// 2026-11-12, "errors" printed "(undefined:1:86)"). It is stored as "".
const noScriptURL = "undefined"

// AnonymousScript names the script of a location without a URL in the
// printed console messages, errors and stack frames.
const AnonymousScript = "<anonymous>"

// scriptURL returns u, or "" for WebKit's url of a script without one.
func scriptURL(u string) string {
	if u == noScriptURL {
		return ""
	}
	return u
}

// ErrorHooks is the bootstrap part (and the current-document install) that
// reports uncaught errors and unhandled rejections with their stack
// through console.debug, so that the console capture sees them even when
// WebKit's own message has no stack (design 4.2 "errors").
const ErrorHooks = `(function () {
  if (window.__iwdpErrorHooks) return;
  window.__iwdpErrorHooks = true;
  var debug = console.debug.bind(console), str = JSON.stringify;
  function send(o) { try { debug("` + errorMarker + `" + str(o)); } catch (e) {} }
  window.addEventListener("error", function (e) {
    if (typeof ErrorEvent === "undefined" || !(e instanceof ErrorEvent)) return;
    var x = e.error;
    send({kind: "error", message: String(e.message || (x && x.message) || x), url: e.filename || "", line: e.lineno || 0, column: e.colno || 0, stack: x && x.stack ? String(x.stack) : ""});
  }, true);
  window.addEventListener("unhandledrejection", function (e) {
    var r = e.reason;
    send({kind: "unhandledrejection", message: r && r.message !== undefined ? String(r.message) : String(r), stack: r && r.stack ? String(r.stack) : ""});
  });
})();`

// Message is one console message.
type Message struct {
	ID          int     `json:"id"`
	Source      string  `json:"source"`
	Level       string  `json:"level"`
	Type        string  `json:"type,omitempty"`
	Text        string  `json:"text"`
	URL         string  `json:"url,omitempty"`
	Line        int     `json:"line,omitempty"`
	Column      int     `json:"column,omitempty"`
	RepeatCount int     `json:"repeatCount,omitempty"`
	Timestamp   float64 `json:"timestamp,omitempty"`
	Stack       string  `json:"stack,omitempty"`
}

// PageError is an uncaught error or unhandled rejection.
type PageError struct {
	ID      int    `json:"id"`
	Kind    string `json:"kind"` // error, unhandledrejection, or console (a WebKit javascript error message)
	Message string `json:"message"`
	URL     string `json:"url,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Stack   string `json:"stack,omitempty"`
}

// Console is the console capture of one session on one page.
type Console struct {
	p       *Page
	session string
	owner   core.Owner

	mu       sync.Mutex
	max      int
	msgs     []Message
	errs     []PageError
	seen     map[string]bool // raw messageAdded params of stored messages
	seenFIFO []string
	next     int
	dropped  int
	changed  chan struct{}
	unsub    []func()
}

// StartConsole returns the console capture of session, starting it when
// it does not run: subscribe, then Console.enable as owner
// "console:<session>". WebKit replays its last 100 messages on enable;
// a message already stored (same raw event, timestamp included) is not
// stored again, so a reconnect or re-enable adds no duplicates (7.7 test 4).
// A start is one desired-state change, so provisional targets get
// Console.enable too.
func (p *Page) StartConsole(ctx context.Context, c webkit.Caller, session string) (*Console, error) {
	if k := p.Console(session); k != nil {
		return k, nil
	}
	var k *Console
	err := p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		var err error
		k, err = p.startConsole(ctx, c, session)
		return err
	})
	if err != nil {
		return nil, err
	}
	return k, nil
}

// startConsole is StartConsole inside its change. The change comes before
// p.mu, as in a lease release, whose change runs the capture's Release.
func (p *Page) startConsole(ctx context.Context, c webkit.Caller, session string) (*Console, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k := p.consoles[session]; k != nil {
		return k, nil
	}
	k := &Console{
		p: p, session: session, owner: owner("console", session), max: DefaultMaxMessages,
		seen: map[string]bool{}, changed: make(chan struct{}),
	}
	sub := subscribeInOrder(p.ps.Caller)
	k.unsub = append(k.unsub, sub("Console.messageAdded", k.added), sub("Console.messageRepeatCountUpdated", k.repeated))
	if err := acquireDomain(ctx, p.ps, c, k.owner, "Console"); err != nil {
		k.stop()
		return nil, err
	}
	p.ps.State.Put(core.Entry{
		Key: "devtools:console:" + session, Owner: k.owner, Apply: noop,
		Release: func(context.Context, core.Sender) error { p.dropConsole(session, k); return nil },
	})
	if !p.ps.State.HasEntry("devtools:console:" + session) {
		// The operation ended, so the change recorded nothing (core-events
		// r3): no release would stop this capture.
		k.stop()
		return nil, fmt.Errorf("console capture of session %q: the operation ended before the capture was recorded", session)
	}
	p.consoles[session] = k
	return k, nil
}

// Console returns the running console capture of session, or nil.
func (p *Page) Console(session string) *Console {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.consoles[session]
}

func (p *Page) dropConsole(session string, k *Console) {
	p.mu.Lock()
	if p.consoles[session] == k {
		delete(p.consoles, session)
	}
	p.mu.Unlock()
	k.stop()
}

func (k *Console) stop() {
	k.mu.Lock()
	unsub := k.unsub
	k.unsub = nil
	k.mu.Unlock()
	for _, u := range unsub {
		u()
	}
}

// SetMax changes the buffer bound (tests).
func (k *Console) SetMax(n int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.max = n
	k.trimLocked()
}

type wipConsoleMessage struct {
	Source      string  `json:"source"`
	Level       string  `json:"level"`
	Text        string  `json:"text"`
	Type        string  `json:"type"`
	URL         string  `json:"url"`
	Line        int     `json:"line"`
	Column      int     `json:"column"`
	RepeatCount int     `json:"repeatCount"`
	Timestamp   float64 `json:"timestamp"`
	StackTrace  *struct {
		CallFrames []struct {
			FunctionName string `json:"functionName"`
			URL          string `json:"url"`
			LineNumber   int    `json:"lineNumber"`
			ColumnNumber int    `json:"columnNumber"`
		} `json:"callFrames"`
	} `json:"stackTrace"`
}

func (k *Console) added(_ string, params json.RawMessage) {
	var v struct {
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(params, &v) != nil || len(v.Message) == 0 {
		return
	}
	var m wipConsoleMessage
	if json.Unmarshal(v.Message, &m) != nil {
		return
	}
	key := string(v.Message)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.seen[key] {
		return // replayed by Console.enable after a reconnect or re-enable
	}
	k.seen[key] = true
	k.seenFIFO = append(k.seenFIFO, key)
	k.next++
	if strings.HasPrefix(m.Text, errorMarker) {
		var e PageError
		if json.Unmarshal([]byte(strings.TrimPrefix(m.Text, errorMarker)), &e) == nil {
			e.ID = k.next
			e.URL = scriptURL(e.URL)
			k.errs = append(k.errs, e)
		}
	} else {
		msg := Message{
			ID: k.next, Source: m.Source, Level: m.Level, Type: m.Type, Text: m.Text, URL: scriptURL(m.URL),
			Line: m.Line, Column: m.Column, RepeatCount: m.RepeatCount, Timestamp: m.Timestamp,
		}
		if m.StackTrace != nil {
			var b strings.Builder
			for _, f := range m.StackTrace.CallFrames {
				name := f.FunctionName
				if name == "" {
					name = "(anonymous)"
				}
				loc := scriptURL(f.URL)
				if loc == "" {
					loc = AnonymousScript
				}
				fmt.Fprintf(&b, "    at %s (%s:%d:%d)\n", name, loc, f.LineNumber, f.ColumnNumber)
			}
			msg.Stack = strings.TrimRight(b.String(), "\n")
		}
		k.msgs = append(k.msgs, msg)
		if m.Level == "error" && m.Source == "javascript" {
			k.errs = append(k.errs, PageError{ID: k.next, Kind: "console", Message: m.Text, URL: msg.URL, Line: m.Line, Column: m.Column, Stack: msg.Stack})
		}
	}
	k.trimLocked()
	close(k.changed)
	k.changed = make(chan struct{})
}

func (k *Console) repeated(_ string, params json.RawMessage) {
	var v struct {
		Count int `json:"count"`
	}
	if json.Unmarshal(params, &v) != nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if n := len(k.msgs); n > 0 {
		k.msgs[n-1].RepeatCount = v.Count
	}
}

func (k *Console) trimLocked() {
	for len(k.msgs) > k.max {
		k.msgs = k.msgs[1:]
		k.dropped++
	}
	for len(k.errs) > k.max {
		k.errs = k.errs[1:]
	}
	// The replay set needs only what WebKit can replay: its last 100
	// messages; keep a margin over the buffer.
	for len(k.seenFIFO) > 2*k.max+100 {
		delete(k.seen, k.seenFIFO[0])
		k.seenFIFO = k.seenFIFO[1:]
	}
}

// ConsoleFilter selects messages. Zero fields match everything.
type ConsoleFilter struct {
	Level string // minimum level: debug, log, info, warning, error
	Since int    // only messages with a larger id
}

var levelRank = map[string]int{"debug": 0, "log": 1, "info": 1, "warning": 2, "error": 3}

// Messages returns the stored messages that match f and the number of
// messages dropped by the bound since the capture started.
func (k *Console) Messages(f ConsoleFilter) (msgs []Message, dropped int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	min, filter := levelRank[strings.ToLower(f.Level)], f.Level != ""
	for _, m := range k.msgs {
		if m.ID <= f.Since || (filter && levelRank[m.Level] < min) {
			continue
		}
		msgs = append(msgs, m)
	}
	return msgs, k.dropped
}

// Errors returns uncaught errors and unhandled rejections. A WebKit
// javascript error message that a hook record of the same error already
// reports (its text contains the record's message) is left out.
func (k *Console) Errors() []PageError {
	k.mu.Lock()
	defer k.mu.Unlock()
	var hooks []PageError
	for _, e := range k.errs {
		if e.Kind != "console" {
			hooks = append(hooks, e)
		}
	}
	var out []PageError
	for _, e := range k.errs {
		if e.Kind == "console" {
			dup := false
			for _, h := range hooks {
				if h.Message != "" && strings.Contains(e.Message, h.Message) {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// Clear empties the buffer (messages and errors); WebKit's own store is
// cleared too, so a later re-enable replays nothing old.
func (k *Console) Clear(ctx context.Context, c webkit.Caller) error {
	k.mu.Lock()
	k.msgs, k.errs, k.dropped = nil, nil, 0
	k.mu.Unlock()
	_, err := c.Send(ctx, "Console.clearMessages", nil)
	if err != nil {
		return fmt.Errorf("Console.clearMessages: %w", err)
	}
	return nil
}

// ClearErrors empties the error list only.
func (k *Console) ClearErrors() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.errs = nil
}

// Wait returns when a message arrives after id since or ctx ends.
func (k *Console) Wait(ctx context.Context, since int) {
	for {
		k.mu.Lock()
		last, ch := k.next, k.changed
		k.mu.Unlock()
		if last > since {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ch:
		}
	}
}

// StartErrors starts the console capture and installs the error hooks as
// the bootstrap part "errors:<session>" (new documents) and in the current
// document (owner "errors:<session>"), as one desired-state change.
func (p *Page) StartErrors(ctx context.Context, c webkit.Caller, session string) (k *Console, err error) {
	err = p.ps.State.Change(ctx, c, func(c webkit.Caller) error {
		var cerr error
		k, cerr = p.startErrors(ctx, c, session)
		return cerr
	})
	if err != nil {
		return nil, err
	}
	return k, nil
}

// startErrors is StartErrors inside its change.
func (p *Page) startErrors(ctx context.Context, c webkit.Caller, session string) (*Console, error) {
	k, err := p.StartConsole(ctx, c, session)
	if err != nil {
		return nil, err
	}
	o := owner("errors", session)
	name := "errors:" + session
	if !containsString(p.ps.Boot.Names(), name) {
		if err := p.ps.State.SetPart(o, name, core.RankShim, ErrorHooks); err != nil {
			return nil, err
		}
		if _, err := c.Send(ctx, "Page.setBootstrapScript", map[string]string{"source": p.ps.Boot.Script()}); err != nil {
			return nil, fmt.Errorf("Page.setBootstrapScript: %w", err)
		}
	}
	if _, err := c.Send(ctx, "Runtime.evaluate", map[string]any{"expression": ErrorHooks, "returnByValue": true}); err != nil {
		return nil, fmt.Errorf("installing the error hooks in the current document: %w", err)
	}
	return k, nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
