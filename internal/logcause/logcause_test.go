package logcause

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

// Errorf (package documentation): the text and the chain of fmt.Errorf's
// error, and each error formatted with another verb than %w as a log
// cause.
func TestErrorfKeepsTextAndChainAndAddsCauses(t *testing.T) {
	stalled := errors.New("stalled")
	cause := fmt.Errorf("probe: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name   string
		err    error
		text   string
		is     []error
		isNot  []error
		causes []error
	}{
		{"one wrapped, one cause", Errorf("%w (%v)", stalled, cause), "stalled (probe: context deadline exceeded)", []error{stalled}, []error{context.DeadlineExceeded}, []error{cause}},
		{"two wrapped, one cause", Errorf("%w: %w (%s)", stalled, io.EOF, cause), "stalled: EOF (probe: context deadline exceeded)", []error{stalled, io.EOF}, []error{context.DeadlineExceeded}, []error{cause}},
		{"no wrapped error", Errorf("failed (%v)", cause), "failed (probe: context deadline exceeded)", nil, []error{context.DeadlineExceeded}, []error{cause}},
		{"nil is no cause", Errorf("failed (%v)", error(nil)), "failed (<nil>)", nil, nil, nil},
		{"wrapped only", Errorf("x: %w", io.EOF), "x: EOF", []error{io.EOF}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Error() != tc.text {
				t.Fatalf("text %q, want %q", tc.err.Error(), tc.text)
			}
			for _, e := range tc.is {
				if !errors.Is(tc.err, e) {
					t.Errorf("errors.Is(%v) = false", e)
				}
			}
			for _, e := range tc.isNot {
				if errors.Is(tc.err, e) {
					t.Errorf("errors.Is(%v) = true: a log cause joined the chain", e)
				}
			}
			var got []error
			if c, ok := tc.err.(interface{ LogCauses() []error }); ok {
				got = c.LogCauses()
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.causes) {
				t.Fatalf("LogCauses = %v, want %v", got, tc.causes)
			}
		})
	}
}

// notComparable is an error type whose values == cannot compare.
type notComparable []string

func (e notComparable) Error() string { return "nc" }

// A wrapped error of a type that is not comparable does not panic; it is
// also kept as a cause, since Errorf cannot tell it from the wrapped one.
func TestErrorfWithAnErrorThatIsNotComparable(t *testing.T) {
	err := Errorf("x: %w", notComparable{"a"})
	if err.Error() != "x: nc" || !errors.As(err, new(notComparable)) {
		t.Fatalf("Errorf = %v", err)
	}
}
