// Package logsafe renders errors for the daemon log from their types only.
//
// The daemon log is a file on disk, so it never holds a page's title, URL
// or messages, which can be private (device check 3, P2-C). The text of an
// error (its Error method) can hold all of them: WebKit protocol errors
// and runtime errors quote what the page or WebKit said, core's errors
// name a page by its title, and a page's exception text ends up in plain
// error strings. So Error never writes the text of an error. It writes the
// classification that the types of the errors in the wrap chain give
// (Error lists them), and a fixed marker for every error it cannot
// classify. Nothing is inferred from text.
//
// The operation that failed is named by the log call itself: its format
// string is a constant, and its other arguments are of safe kinds (a
// rendered error, an ID, a Label, a number, a duration, a state). The sink
// check in sinks_test.go enforces this for the packages of the daemon.
package logsafe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	goruntime "runtime"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"

	"github.com/gorilla/websocket"

	"github.com/edxeth/agent-ios/internal/refs"
	"github.com/edxeth/agent-ios/internal/runtime"
	"github.com/edxeth/agent-ios/internal/transport"
	"github.com/edxeth/agent-ios/internal/webkit"
)

// Unclassified is the marker of an error that Error cannot classify by
// its type and that wraps nothing.
const Unclassified = "unclassified"

// Label is a constant text of the program that names an operation or a
// kind in a log line ("the frame tree", "console"). The sink check allows
// a Label argument and fails for a conversion of a value that is not a
// constant to Label.
type Label string

// ID is an identifier in a log line: a page, target or request id, a
// session name, a lease owner key, a device label, a CDP connection name,
// a protocol method name, a hash of a value that a client sent; or a
// configured value: an address from the configuration, the version, the
// transport kind. It is never a page's title, URL or text, and never text
// of an error. The sink check allows an ID argument; each conversion to ID
// states that the converted value is such an identifier.
type ID string

// sentinels are the errors that Error names (Register), by their names.
var sentinels = map[error]string{}

func init() {
	Register(map[string]error{
		"context.Canceled": context.Canceled, "context.DeadlineExceeded": context.DeadlineExceeded,
		"io.EOF": io.EOF, "io.ErrUnexpectedEOF": io.ErrUnexpectedEOF,
		"net.ErrClosed": net.ErrClosed, "os.ErrDeadlineExceeded": os.ErrDeadlineExceeded,
		"http.ErrServerClosed":      http.ErrServerClosed,
		"websocket.ErrBadHandshake": websocket.ErrBadHandshake, "websocket.ErrCloseSent": websocket.ErrCloseSent,
		"websocket.ErrReadLimit":  websocket.ErrReadLimit,
		"transport.ErrNotWritten": transport.ErrNotWritten, "runtime.ErrNotInstalled": runtime.ErrNotInstalled,
		"webkit.ErrNoTarget": webkit.ErrNoTarget, "webkit.ErrTargetSwapped": webkit.ErrTargetSwapped,
		"webkit.ErrTargetAbandoned": webkit.ErrTargetAbandoned, "webkit.ErrPageClosed": webkit.ErrPageClosed,
		"webkit.ErrSessionClosed": webkit.ErrSessionClosed, "webkit.ErrCallTimeout": webkit.ErrCallTimeout,
		"webkit.ErrSessionStalled": webkit.ErrSessionStalled, "webkit.ErrNotSent": webkit.ErrNotSent,
		"webkit.ErrTargetNotPaused": webkit.ErrTargetNotPaused, "webkit.ErrHangsThroughTarget": webkit.ErrHangsThroughTarget,
		"webkit.ErrWriteFailed": webkit.ErrWriteFailed, "webkit.ErrWriteTimeout": webkit.ErrWriteTimeout,
		"webkit.ErrWriteBroken": webkit.ErrWriteBroken,
	})
}

// Register adds sentinel errors, each under its name ("core.ErrLost"),
// which Error writes for it. A package registers its sentinels in its init
// function; Register is not safe for use after init.
func Register(named map[string]error) {
	for name, err := range named {
		sentinels[err] = name
	}
}

// knownCodes are the runtime and ref error codes that the program defines.
// A code comes from the page's reply, so any other code is not written.
var knownCodes = map[string]bool{
	runtime.CodeStaleOwner: true, "unknown_ref": true, "stale_ref": true,
	"unsupported": true, "not_found": true, "ambiguous": true,
	// Form refusals and upload preconditions (agent-browser 0.45 codes).
	"not_editable": true, "read_only": true, "disabled": true, "not_checkable": true, "option_not_found": true,
	"not_multiple": true, "malformed_value": true, "not_file_input": true, "file_not_found": true,
	// Precondition codes of diff snapshot and network har stop
	// (agent-browser 0.45).
	"no_baseline": true, "baseline_mismatch": true, "no_har_recording": true,
}

// Error renders err for the daemon log. It walks err's wrap chain (Unwrap,
// also the multiple form of errors.Join and fmt.Errorf) and writes, for
// each error in it:
//   - a registered sentinel (Register, found by identity or its Is
//     method): its name, for example "webkit.ErrSessionStalled";
//   - *webkit.ErrorData: "WebKit protocol error <code>";
//   - *webkit.CallError: the method of the request that the program sent;
//   - *runtime.Error and *refs.Error: "runtime error <code>" and "ref
//     error <code>" for a code in knownCodes, else without the code;
//   - *websocket.CloseError: "websocket close <code>";
//   - syscall.Errno: "errno <n> (<its fixed text>)";
//   - *net.OpError: "net <op>" (dial, read, write); not its addresses;
//   - *json.SyntaxError: its offset; *json.UnmarshalTypeError: the Go type;
//   - a fault of the Go runtime: "Go runtime error";
//   - an error of Errorf: its text, with its arguments in safe forms;
//   - another error that wraps others: nothing of its own, since its text
//     can name a page;
//   - another error that wraps nothing: "unclassified", with its Go type
//     when it is not a plain errors.New or fmt.Errorf error.
//
// An error that wraps one error is followed by ": " and that error, one
// that wraps several by the list "[a; b]". The errors that an error names
// for the log only (its LogCauses method, internal/logcause) follow as
// " (also: a; b)".
func Error(err error) string {
	if err == nil {
		return "<nil>"
	}
	return render(err)
}

// logCauser is an error that names errors that explain it but that
// errors.Is and errors.As must not see (internal/logcause, webkit's agent
// reset failure). Error renders them after the error.
type logCauser interface{ LogCauses() []error }

// Errorf returns fmt.Errorf(format, args...), an error whose whole text
// the log may hold: format is a constant of the program (a Label), and
// each argument is an ID, a Label, a number or an error. Error writes its
// text with each error argument rendered by Error and any argument of
// another kind as "unclassified"; it does not walk what the error wraps,
// which the text names already. errors.Is and errors.As see what
// fmt.Errorf wraps. Use it for an error that names ids the log needs (a
// frame, an interception rule).
func Errorf(format Label, args ...any) error {
	err := fmt.Errorf(string(format), args...)
	s := &safeError{format: format, args: args, err: err}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return &safeOne{safeError: s, inner: u.Unwrap()}
	case interface{ Unwrap() []error }:
		return &safeMany{safeError: s, inner: u.Unwrap()}
	}
	return s
}

// safeError is an error of Errorf; safeOne and safeMany are the ones that
// wrap one error or several.
type safeError struct {
	format Label
	args   []any
	err    error
}

func (e *safeError) Error() string { return e.err.Error() }

// logText is the error's text with each argument in its safe form.
func (e *safeError) logText() string {
	args := make([]any, len(e.args))
	for i, a := range e.args {
		args[i] = safeArg(a)
	}
	return fmt.Sprintf(strings.ReplaceAll(string(e.format), "%w", "%v"), args...)
}

type safeOne struct {
	*safeError
	inner error
}

func (e *safeOne) Unwrap() error { return e.inner }

type safeMany struct {
	*safeError
	inner []error
}

func (e *safeMany) Unwrap() []error { return e.inner }

// safeArg is an argument of Errorf as the log may write it.
func safeArg(a any) any {
	switch v := a.(type) {
	case error:
		return Error(v)
	case ID, Label:
		return v
	}
	switch reflect.ValueOf(a).Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return a
	}
	return Unclassified
}

// Panic renders a recovered panic value for the daemon log: an error as
// Error renders it, any other value (a message too) by its type only,
// since the value can hold page text.
func Panic(p any) string {
	if v, ok := p.(error); ok {
		return Error(v)
	}
	return fmt.Sprintf("a panic value of type %T", p)
}

// Stack is the stack of the calling goroutine (runtime/debug.Stack) for a
// log line: function names, files, lines and the words of the arguments,
// no value that an argument points to.
func Stack() string { return string(debug.Stack()) }

func render(err error) string {
	if s, ok := err.(interface{ logText() string }); ok {
		return s.logText()
	}
	own, ok := classified(err)
	var inner []error
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		if e := u.Unwrap(); e != nil {
			inner = []error{e}
		}
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if e != nil {
				inner = append(inner, e)
			}
		}
	}
	var wrapped string
	switch len(inner) {
	case 0:
	case 1:
		wrapped = render(inner[0])
	default:
		wrapped = "[" + renderList(inner) + "]"
	}
	var causes []error
	if c, isCauser := err.(logCauser); isCauser {
		for _, e := range c.LogCauses() {
			if e != nil {
				causes = append(causes, e)
			}
		}
	}
	var s string
	switch {
	case ok && wrapped != "":
		s = own + ": " + wrapped
	case ok:
		s = own
	case wrapped != "":
		s = wrapped
	case len(causes) > 0:
		return "[" + renderList(causes) + "]"
	default:
		return unclassified(err)
	}
	if len(causes) > 0 {
		s += " (also: " + renderList(causes) + ")"
	}
	return s
}

func renderList(errs []error) string {
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = render(e)
	}
	return strings.Join(parts, "; ")
}

// classified renders err itself, not what it wraps, when its identity or
// type says what it is.
func classified(err error) (string, bool) {
	if name, ok := sentinel(err); ok {
		return name, true
	}
	switch e := err.(type) {
	case *webkit.ErrorData:
		return fmt.Sprintf("WebKit protocol error %d", e.Code), true
	case *webkit.CallError:
		return e.Method, true
	case *runtime.Error:
		return withCode("runtime error", e.Code), true
	case *refs.Error:
		return withCode("ref error", e.Code), true
	case *websocket.CloseError:
		return fmt.Sprintf("websocket close %d", e.Code), true
	case syscall.Errno:
		return fmt.Sprintf("errno %d (%s)", uintptr(e), e.Error()), true
	case *net.OpError:
		return "net " + e.Op, true
	case *json.SyntaxError:
		return fmt.Sprintf("JSON syntax error at offset %d", e.Offset), true
	case *json.UnmarshalTypeError:
		if e.Type == nil {
			return "JSON type error", true
		}
		return "JSON type error into " + e.Type.String(), true
	case goruntime.Error:
		return "Go runtime error", true
	}
	return "", false
}

func sentinel(err error) (string, bool) {
	if reflect.TypeOf(err).Comparable() {
		if name, ok := sentinels[err]; ok {
			return name, true
		}
	}
	if is, ok := err.(interface{ Is(error) bool }); ok {
		// The first name in order, so that an error that is several
		// sentinels renders the same each time.
		var names []string
		for s, name := range sentinels {
			if is.Is(s) {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			return slices.Min(names), true
		}
	}
	return "", false
}

func withCode(kind, code string) string {
	if knownCodes[code] {
		return kind + " " + code
	}
	return kind
}

func unclassified(err error) string {
	var plain interface{ Error() string } = errors.New("")
	if reflect.TypeOf(err) == reflect.TypeOf(plain) {
		return Unclassified
	}
	return fmt.Sprintf("%s %T", Unclassified, err)
}
