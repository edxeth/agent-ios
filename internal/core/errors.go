package core

import (
	"errors"

	"github.com/nnemirovsky/agent-ios/internal/logsafe"
)

// Exit codes that core errors choose (design 2.10); they equal the
// commands package's ExitTimeout, ExitBusy, ExitNoPage and ExitReservation.
const (
	CodeTimeout     = 124 // an expired command deadline, like timeout(1) (decision D9)
	CodeBusy        = 4   // page busy or lost
	CodeNoPage      = 5   // no device or page
	CodeReservation = 6   // device reservation missing
)

// CodedError is an error with the exit code a client reports for it.
type CodedError struct {
	Code int
	Err  error
}

func (e *CodedError) Error() string { return e.Err.Error() }
func (e *CodedError) Unwrap() error { return e.Err }

// ExitCode lets commands.ExitCodeOf pick the exit code.
func (e *CodedError) ExitCode() int { return e.Code }

// The daemon log names core's sentinel errors (logsafe).
func init() {
	logsafe.Register(map[string]error{
		"core.ErrLost":               ErrLost,
		"core.ErrLeased":             ErrLeased,
		"core.ErrPageBusy":           ErrPageBusy,
		"core.ErrNoReservation":      ErrNoReservation,
		"core.ErrNoPage":             ErrNoPage,
		"core.ErrRecoveryFailed":     ErrRecoveryFailed,
		"core.ErrStaleEpoch":         ErrStaleEpoch,
		"core.ErrLeaseEnded":         ErrLeaseEnded,
		"core.ErrOpEnded":            ErrOpEnded,
		"core.ErrNoDeadline":         ErrNoDeadline,
		"core.ErrControlRefused":     ErrControlRefused,
		"core.ErrAttemptClosed":      ErrAttemptClosed,
		"core.ErrOutcomeUnknown":     ErrOutcomeUnknown,
		"core.ErrReplyLost":          ErrReplyLost,
		"core.ErrClockUncertain":     ErrClockUncertain,
		"core.ErrDocumentChanged":    ErrDocumentChanged,
		"core.ErrReserved":           ErrReserved,
		"core.ErrRequestReset":       ErrRequestReset,
		"core.errContextNotReported": errContextNotReported,
		"core.errFrameGone":          errFrameGone,
		"core.errResumeUnknown":      errResumeUnknown,
		"core.errProbeCanceled":      errProbeCanceled,
		"core.errShuttingDown":       errShuttingDown,
		"core.errLeaseChanged":       errLeaseChanged,
		"core.errChangeRefused":      errChangeRefused,
	})
}

// Errors of page ownership and leases (design 2.5).
var (
	// ErrLost: the page connection closed without a request from the daemon.
	ErrLost = errors.New("connection to page was closed by someone else or by the device; ownership is unknown; run the command again with --takeover once the device owner agrees")
	// ErrLeased: another session holds the page lease.
	ErrLeased = errors.New("page is leased by another session")
	// ErrPageBusy: the listing shows that another inspector client holds the page.
	ErrPageBusy = errors.New("the page is held by another inspector client (busy hint of the iwdp listing); close that inspector or pick another page")
	// ErrNoReservation: a physical device needs a reservation (design 2.5 rule 1).
	ErrNoReservation = errors.New("no device reservation")
	// ErrNoPage: no page matched.
	ErrNoPage = errors.New("no matching page")
	// ErrRecoveryFailed: the background recovery of a stalled page failed.
	ErrRecoveryFailed = errors.New("the page stopped answering and the reconnect failed")
)
