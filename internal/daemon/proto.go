package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

// Protocol is the socket API version (design 2.2). A client that sees
// another number asks an idle daemon to exit and starts its own.
const Protocol = 1

// Hello is the answer of GET /v1/hello.
type Hello struct {
	ID         string    `json:"id"` // random per daemon process (core.Manager.DaemonID)
	Version    string    `json:"version"`
	Protocol   int       `json:"protocol"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"startedAt"`
	Transports []string  `json:"transports"`
	Leases     int       `json:"leases"`
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

// OpRequest is the body of POST /v1/op.
type OpRequest struct {
	Op        string `json:"op"` // begin or end
	OpID      string `json:"opId,omitempty"`
	Name      string `json:"name,omitempty"`
	Class     string `json:"class,omitempty"` // read, mutate, exclusive (default)
	TimeoutMs int64  `json:"timeoutMs,omitempty"`
	PageReq
	Owner
}

// PageInfo describes the page an operation runs on.
type PageInfo struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Routed bool   `json:"routed"`
	// Epoch is the lease epoch that the caller acquired (a lease or an
	// operation); control messages outside an operation carry it.
	Epoch uint64 `json:"epoch,omitempty"`
}

// OpReply is the first item of the begin stream.
type OpReply struct {
	OpID     string    `json:"opId"`
	Epoch    uint64    `json:"epoch"`
	Deadline time.Time `json:"deadline"`
	Page     PageInfo  `json:"page"`
}

// RawRequest is the body of POST /v1/raw. Without OpID the daemon runs the
// call as its own exclusive operation on the selected page. A control-lane
// message (design 2.10) runs in no queued operation: it names the open
// operation it was sent from (OpID), or the attached page (Key) and the
// lease epoch its caller acquired (Epoch); the scheduler refuses it when
// that epoch is not the page's current one.
type RawRequest struct {
	OpID      string          `json:"opId,omitempty"`
	Key       string          `json:"key,omitempty"`
	Epoch     uint64          `json:"epoch,omitempty"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	TimeoutMs int64           `json:"timeoutMs,omitempty"`
	PageReq
	Owner
}

// ErrorReply is an error on the wire.
type ErrorReply struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	NotSent bool   `json:"notSent,omitempty"`
	Timeout bool   `json:"timeout,omitempty"`
}

// RawReply is the answer of POST /v1/raw.
type RawReply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrorReply     `json:"error,omitempty"`
}

// LeaseRequest is the body of POST /v1/lease.
type LeaseRequest struct {
	Op        string `json:"op"` // acquire, release, break, renew, takeover
	TimeoutMs int64  `json:"timeoutMs,omitempty"`
	PageReq
	Owner
}

// Item is one NDJSON line of a stream.
type Item struct {
	Type    string          `json:"type"` // op, subscribed, event, line, result, error, closed
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Text    string          `json:"text,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Op      *OpReply        `json:"op,omitempty"`
	Page    *PageInfo       `json:"page,omitempty"`
	Error   *ErrorReply     `json:"error,omitempty"`
	Dropped int64           `json:"dropped,omitempty"`
}

// RunRequest is the body of POST /v1/run (a registry command, design 2.2).
// Argv is the command line after the global flags; the daemon parses it
// with the same registry spec as the client.
type RunRequest struct {
	Argv      []string `json:"argv"`
	TimeoutMs int64    `json:"timeoutMs"`
	Cwd       string   `json:"cwd"`
	JSON      bool     `json:"json,omitempty"`
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
	}
	return false
}

// toErrorReply encodes err for the wire: its exit code (usage errors 2,
// coded errors their code, deadlines 124), whether the call was never sent
// and whether a deadline ended it.
func toErrorReply(err error) *ErrorReply {
	r := &ErrorReply{Code: commands.ExitCodeOf(err), Message: err.Error()}
	r.NotSent = errors.Is(err, webkit.ErrNotSent)
	r.Timeout = errors.Is(err, context.DeadlineExceeded) || r.Code == core.CodeTimeout
	if r.Timeout && r.Code == commands.ExitError {
		r.Code = core.CodeTimeout
	}
	return r
}

func ms(d time.Duration) int64 { return int64(d / time.Millisecond) }

func durMs(v int64, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return time.Duration(v) * time.Millisecond
}

func errorf(code int, format string, a ...any) *ErrorReply {
	return &ErrorReply{Code: code, Message: fmt.Sprintf(format, a...)}
}
