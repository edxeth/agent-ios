package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/nnemirovsky/agent-ios/internal/core"
)

// Level is the kind of CDP session a method exists on.
type Level uint8

const (
	// OnBrowser: the browser session (messages without a sessionId on a
	// browser connection).
	OnBrowser Level = 1 << iota
	// OnTab: a tab target's session (design 5.2).
	OnTab
	// OnPage: a page target's session, or the root of a page connection
	// (/devtools/page/<targetId>).
	OnPage
)

// Later finishes a command after its handler returned, so that the next
// message of the same CDP session is not held behind a wait (for example
// Runtime.awaitPromise). Replies keep their ids, so clients match them.
type Later func(ctx context.Context) (any, error)

// Handler handles one CDP command in the arrival order of its CDP session
// (Playwright and Puppeteer depend on it, proof P2 item 6). It returns the
// result, or a Later that produces it, or an error.
type Handler func(ctx context.Context, c *Call) (any, Later, error)

// Method is one translated CDP method. Every method the bridge answers is
// in this table; nothing else is forwarded to WebKit (design 5.4).
type Method struct {
	Name   string // "Domain.method"
	Levels Level
	// Handle is nil for a method that has no effect on iOS but that
	// clients send on every attach: it succeeds with an empty result
	// (design 5.4).
	Handle Handler
}

var (
	tableMu sync.RWMutex
	table   = map[string]Method{}
	events  = map[string][]string{} // domain -> event names, for /json/protocol
)

// register adds methods to the table. Each domain file calls it from
// init(); a duplicate name is a programming error.
func register(ms ...Method) {
	tableMu.Lock()
	defer tableMu.Unlock()
	for _, m := range ms {
		if _, dup := table[m.Name]; dup || !strings.Contains(m.Name, ".") || m.Levels == 0 {
			panic(fmt.Sprintf("cdp: bad or duplicate method %q", m.Name))
		}
		table[m.Name] = m
	}
}

// registerEvents lists the events of a domain for /json/protocol.
func registerEvents(domain string, names ...string) {
	tableMu.Lock()
	defer tableMu.Unlock()
	events[domain] = append(events[domain], names...)
}

// lookup returns the method name on a session of level l.
func lookup(name string, l Level) (Method, bool) {
	tableMu.RLock()
	defer tableMu.RUnlock()
	m, ok := table[name]
	if !ok || m.Levels&l == 0 {
		return Method{}, false
	}
	return m, true
}

// protocol is the supported subset in the form of Chrome's /json/protocol,
// generated from the table.
func protocol() map[string]any {
	tableMu.RLock()
	defer tableMu.RUnlock()
	byDomain := map[string][]string{}
	for name := range table {
		d, m, _ := strings.Cut(name, ".")
		byDomain[d] = append(byDomain[d], m)
	}
	for d := range events {
		if _, ok := byDomain[d]; !ok {
			byDomain[d] = nil
		}
	}
	domains := make([]string, 0, len(byDomain))
	for d := range byDomain {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	out := make([]map[string]any, 0, len(domains))
	for _, d := range domains {
		cmds := append([]string(nil), byDomain[d]...)
		sort.Strings(cmds)
		cs := make([]map[string]string, len(cmds))
		for i, c := range cmds {
			cs[i] = map[string]string{"name": c}
		}
		evs := append([]string(nil), events[d]...)
		sort.Strings(evs)
		es := make([]map[string]string, len(evs))
		for i, e := range evs {
			es[i] = map[string]string{"name": e}
		}
		out = append(out, map[string]any{"domain": d, "commands": cs, "events": es})
	}
	return map[string]any{"version": map[string]string{"major": "1", "minor": "3"}, "domains": out}
}

// Error is a CDP error reply.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// CDP error codes (JSON-RPC, as Chrome uses them).
const (
	codeNotFound      = -32601
	codeInvalidParams = -32602
	codeServer        = -32000
)

// notFound is the reply to an unknown or unsupported method: the text
// Chrome uses for a missing method, so that clients that probe optional
// features continue (design 5.4).
func notFound(method string) *Error {
	return &Error{Code: codeNotFound, Message: fmt.Sprintf("'%s' wasn't found", method)}
}

func serverError(format string, a ...any) *Error {
	return &Error{Code: codeServer, Message: fmt.Sprintf(format, a...)}
}

// toError turns a handler error into a CDP error. Core errors (lease,
// reservation, no page) keep their message, which tells the user what to do.
func toError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var ce *core.CodedError
	if errors.As(err, &ce) {
		return &Error{Code: codeServer, Message: ce.Error()}
	}
	return &Error{Code: codeServer, Message: err.Error()}
}

// Call is one CDP command.
type Call struct {
	ID        int64
	Method    string
	Params    json.RawMessage
	SessionID string

	conn    *conn
	session *session // nil on the browser session
	after   func()   // runs after the reply is written
}

// decode reads the params into v; bad params give Chrome's
// "Invalid parameters" error.
func (c *Call) decode(v any) error {
	if len(c.Params) == 0 || string(c.Params) == "null" {
		return nil
	}
	if err := json.Unmarshal(c.Params, v); err != nil {
		return &Error{Code: codeInvalidParams, Message: fmt.Sprintf("Invalid parameters: %v", err)}
	}
	return nil
}
