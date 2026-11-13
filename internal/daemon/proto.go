package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nnemirovsky/iwdp-cli/internal/commands"
	"github.com/nnemirovsky/iwdp-cli/internal/core"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

// Protocol is the socket API version (design 2.2). A client that sees
// another number, or a daemon that runs another executable
// (Hello.Executable), asks an idle daemon to exit and starts its own.
// Protocol 2: /v1/op, /v1/raw, /v1/events and /v1/lease are gone, and
// RunRequest carries the absolute Deadline instead of a remaining time.
const Protocol = 2

// previousProtocol is the protocol of the daemons of the release before
// Protocol 2 (base 70e525d). Its GET /v1/hello, GET /v1/status and POST
// /v1/shutdown have the request and answer shapes of Protocol 2 (Hello
// without Executable), so 'daemon status' and 'daemon stop' (Options.NoStart)
// reach such a daemon after an upgrade.
const previousProtocol = 1

// Hello is the answer of GET /v1/hello.
type Hello struct {
	ID       string `json:"id"` // random per daemon process (core.Manager.DaemonID)
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	// Executable is the executable that the daemon runs, recorded at
	// its start (reusable); the in-process core of IWDP_NO_DAEMON has
	// none.
	Executable Executable `json:"executable,omitzero"`
	PID        int        `json:"pid"`
	StartedAt  time.Time  `json:"startedAt"`
	Transports []string   `json:"transports"`
	Leases     int        `json:"leases"`
}

// PageReq selects a page (core.PageRequest on the wire).
type PageReq struct {
	Transport string `json:"transport,omitempty"`
	Addr      string `json:"addr,omitempty"`
	Page      string `json:"page,omitempty"`
}

// Owner carries a client's session and ownership options (design 2.5).
type Owner struct {
	Session     string `json:"session,omitempty"`
	Reservation string `json:"reservation,omitempty"`
	WaitLeaseMs int64  `json:"waitLeaseMs,omitempty"`
	Takeover    bool   `json:"takeover,omitempty"`
}

// PageInfo describes the page an operation runs on.
type PageInfo struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// ErrorReply is an error on the wire.
type ErrorReply struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	NotSent bool   `json:"notSent,omitempty"`
	Timeout bool   `json:"timeout,omitempty"`
	// ActionTimeout: the deadline ended the attempts of an action
	// (core.ErrActionTimeout), and the message names their last reason.
	ActionTimeout bool `json:"actionTimeout,omitempty"`
}

// Item is one NDJSON line of the /v1/run stream.
type Item struct {
	Type  string          `json:"type"` // line, page (the operation's page), result, error
	Text  string          `json:"text,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	Page  *PageInfo       `json:"page,omitempty"`
	Error *ErrorReply     `json:"error,omitempty"`
}

// RunRequest is the body of POST /v1/run (a registry command, design 2.2).
// Argv is the command line after the global flags; the daemon parses it
// with the same registry spec as the client.
//
// Deadline is the caller's absolute deadline of the command: the daemon
// runs the operation under it, so a request that reaches the daemon late
// does not get more time, and one that arrives after it does no page
// work. The zero value means DefaultCommandTimeout from the request's
// arrival. Client and daemon run on one machine and read the same wall
// clock (the monotonic reading does not cross the socket); a step of
// that clock while a request is in flight shifts its deadline by the
// step. That limit is accepted.
//
// OnlyTab is --only-tab: the daemon runs the command on the only tab of
// the listing (core.Manager.OnlyTab), and a request with OnlyTab and a
// Page is a usage error. A client sends requests only to a daemon of its
// own executable (reusable), so no daemon of another build reads this
// request; Protocol stays 2.
type RunRequest struct {
	Argv     []string  `json:"argv"`
	Deadline time.Time `json:"deadline,omitzero"`
	Cwd      string    `json:"cwd"`
	JSON     bool      `json:"json,omitempty"`
	OnlyTab  bool      `json:"onlyTab,omitempty"`
	PageReq
	Owner
}

// RemoteError is an error that the daemon reported. It keeps the exit
// code, whether the call was never sent, and whether a deadline ended it.
type RemoteError struct{ E ErrorReply }

func (e *RemoteError) Error() string { return e.E.Message }

// ExitCode lets commands.ExitCodeOf and the CLI pick the exit code.
func (e *RemoteError) ExitCode() int { return e.E.Code }

func (e *RemoteError) Is(target error) bool {
	switch target {
	case webkit.ErrNotSent:
		return e.E.NotSent
	case context.DeadlineExceeded:
		return e.E.Timeout
	case core.ErrActionTimeout:
		return e.E.ActionTimeout
	}
	return false
}

// toErrorReply encodes err for the wire: its exit code (usage errors 2,
// coded errors their code, deadlines 124), whether the call was never sent,
// whether a deadline ended it, and whether it ended an action's attempts.
func toErrorReply(err error) *ErrorReply {
	r := &ErrorReply{Code: commands.ExitCodeOf(err), Message: err.Error()}
	r.NotSent = errors.Is(err, webkit.ErrNotSent)
	r.ActionTimeout = errors.Is(err, core.ErrActionTimeout)
	r.Timeout = errors.Is(err, context.DeadlineExceeded) || r.Code == core.CodeTimeout
	if r.Timeout && r.Code == commands.ExitError {
		r.Code = core.CodeTimeout
	}
	return r
}

func durMs(v int64, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return time.Duration(v) * time.Millisecond
}

func errorf(code int, format string, a ...any) *ErrorReply {
	return &ErrorReply{Code: code, Message: fmt.Sprintf(format, a...)}
}
