package webkit

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// resetFailed (sendWithReset; review devfix-R2 F5 item 4): the text names
// every failure as before, errors.Is and errors.As see only the cause, so
// the other failures do not change how a caller classifies the enable,
// and LogCauses gives internal/logsafe the others and the enable's error.
func TestResetFailedKeepsEveryFailureForTheLog(t *testing.T) {
	cause := fmt.Errorf("write: %w", context.DeadlineExceeded)
	refused := &ErrorData{Code: -32000, Message: "refused"}
	notSent := notSentError{errors.New("queue full")}
	enable := &ErrorData{Code: -32601, Message: "no such method"}
	for _, tc := range []struct {
		name   string
		err    error
		text   string
		is     error
		isNot  []error
		causes []error
	}{
		{
			"a cause, other failures and an enable error",
			resetFailed("Network.enable", "Network", cause, []error{refused, notSent}, enable),
			"Network.enable: resetting the Network agent, whose state this session did not know, failed: write: context deadline exceeded; refused; queue full (the enable: no such method)",
			context.DeadlineExceeded,
			[]error{ErrNotSent},
			[]error{refused, notSent, enable},
		},
		{
			"failures that WebKit answered, the enable accepted",
			resetFailed("Network.enable", "Network", nil, []error{refused}, nil),
			"Network.enable: resetting the Network agent, whose state this session did not know, failed: refused (the enable: <nil>)",
			nil,
			[]error{ErrNotSent},
			[]error{refused},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Error() != tc.text {
				t.Fatalf("text %q, want %q", tc.err.Error(), tc.text)
			}
			if tc.is != nil && !errors.Is(tc.err, tc.is) {
				t.Errorf("errors.Is(%v) = false, want the cause in the chain", tc.is)
			}
			for _, e := range tc.isNot {
				if errors.Is(tc.err, e) {
					t.Errorf("errors.Is(%v) = true: another failure joined the chain", e)
				}
			}
			var pe *ErrorData
			if errors.As(tc.err, &pe) {
				t.Errorf("errors.As found protocol error %v outside the cause", pe)
			}
			got := tc.err.(interface{ LogCauses() []error }).LogCauses()
			if fmt.Sprint(got) != fmt.Sprint(tc.causes) {
				t.Fatalf("LogCauses = %v, want %v", got, tc.causes)
			}
		})
	}
}
