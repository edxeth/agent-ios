// Package logcause builds errors that name a cause for the daemon log
// without making it part of their chain.
//
// fmt.Errorf("%w (%v)", ErrStalled, err) keeps err out of the chain on
// purpose: errors.Is(e, context.DeadlineExceeded) must stay false when err
// is a probe's deadline. But then err is only text, and internal/logsafe,
// which writes no error text, cannot classify it. Errorf builds the same
// error, with the same text and the same chain, and keeps each error that
// the format writes with a verb other than %w as a log cause (LogCauses),
// which logsafe renders and errors.Is and errors.As do not see.
//
// The package imports nothing of the program, so every package can use it.
package logcause

import (
	"fmt"
	"reflect"
)

// Errorf is fmt.Errorf(format, args...), with every error argument that
// the result does not wrap kept as a log cause.
func Errorf(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	var causes []error
	for _, a := range args {
		if e, ok := a.(error); ok && e != nil && !wraps(err, e) {
			causes = append(causes, e)
		}
	}
	if len(causes) == 0 {
		return err
	}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return &one{err: err, inner: u.Unwrap(), causes: causes}
	case interface{ Unwrap() []error }:
		return &many{err: err, inner: u.Unwrap(), causes: causes}
	}
	return &none{err: err, causes: causes}
}

// wraps reports whether err wraps e directly (one of its %w operands).
// An error of a type that is not comparable counts as not wrapped, so it
// can be both wrapped and a log cause.
func wraps(err, e error) bool {
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return same(u.Unwrap(), e)
	case interface{ Unwrap() []error }:
		for _, w := range u.Unwrap() {
			if same(w, e) {
				return true
			}
		}
	}
	return false
}

func same(a, b error) bool {
	t := reflect.TypeOf(a)
	return t == reflect.TypeOf(b) && t != nil && t.Comparable() && a == b
}

// none, one and many are the error of Errorf when fmt.Errorf wraps no
// error, one or several: each has the same text and unwraps to the same
// errors as fmt.Errorf's error, and adds LogCauses.
type none struct {
	err    error
	causes []error
}

func (e *none) Error() string      { return e.err.Error() }
func (e *none) LogCauses() []error { return e.causes }

type one struct {
	err, inner error
	causes     []error
}

func (e *one) Error() string      { return e.err.Error() }
func (e *one) Unwrap() error      { return e.inner }
func (e *one) LogCauses() []error { return e.causes }

type many struct {
	err    error
	inner  []error
	causes []error
}

func (e *many) Error() string      { return e.err.Error() }
func (e *many) Unwrap() []error    { return e.inner }
func (e *many) LogCauses() []error { return e.causes }
