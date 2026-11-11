package logsafe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/quick"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nnemirovsky/iwdp-cli/internal/logcause"
	"github.com/nnemirovsky/iwdp-cli/internal/refs"
	"github.com/nnemirovsky/iwdp-cli/internal/runtime"
	"github.com/nnemirovsky/iwdp-cli/internal/webkit"
)

const (
	title   = "Private Review Title"
	pageURL = "https://private-review.example/account?secret=fixture"
)

// stepError is an error type of the program that wraps nothing.
type stepError struct{ what string }

func (e *stepError) Error() string { return "step " + e.what + " failed" }

// The contract of Error (the package documentation; review devfix-R2 F5):
// only what the types of the chain say, never the text of an error.
func TestErrorWritesOnlyTheClassificationOfTypes(t *testing.T) {
	pageErr := &webkit.ErrorData{Code: -32000, Message: "cleanup failed on " + pageURL + " (" + title + ")", Data: json.RawMessage(strconv.Quote(title))}
	refused := fmt.Sprintf("errno %d (connection refused)", uintptr(syscall.ECONNREFUSED))
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "<nil>"},
		{
			"protocol error of a cleanup: the operation text is withheld, the method of the request kept",
			fmt.Errorf("releasing desired state of %s: %w", "console:review", &webkit.CallError{Method: "Console.disable", Err: pageErr}),
			"Console.disable: WebKit protocol error -32000",
		},
		{
			"a method-shaped prefix of text is not a method",
			fmt.Errorf("%s: %w", "Console.disable", pageErr),
			"WebKit protocol error -32000",
		},
		{"sentinel by its name", fmt.Errorf("connecting to page %q: %w", title, webkit.ErrSessionClosed), "webkit.ErrSessionClosed"},
		{"deadline", fmt.Errorf("navigating to %s: %w", pageURL, context.DeadlineExceeded), "context.DeadlineExceeded"},
		{"cancel", fmt.Errorf("loading data:text/html,%s: %w", "secret", context.Canceled), "context.Canceled"},
		{
			"an iwdp page URL is withheld like any text",
			fmt.Errorf("connecting to %s: %w", "ws://127.0.0.1:9224/devtools/page/3?x=1", syscall.ECONNREFUSED),
			refused,
		},
		{"a sentinel found by its Is method", fmt.Errorf("Page.reload: %w", notSentLike{io.EOF}), "webkit.ErrNotSent: io.EOF"},
		{"fault of the program", fmt.Errorf("handler: %w", fault()), "Go runtime error"},
		{"page text in a plain error", errors.New("cleanup failed for " + pageURL + " (" + title + ")"), "unclassified"},
		{"page exception in a wrapper of a plain error", fmt.Errorf("runtime call release: %w", fmt.Errorf("page threw %s", title)), "unclassified"},
		{"typed error that wraps nothing", &stepError{title}, "unclassified *logsafe.stepError"},
		{
			"network failure: the operation of the net error, not its address",
			fmt.Errorf("Console.enable: %w", &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(10, 1, 2, 3), Port: 3000}, Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}),
			"net dial: " + refused,
		},
		{
			"request URL of a listing read",
			&url.Error{Op: "Get", URL: "http://127.0.0.1:9221/json?" + title, Err: os.ErrDeadlineExceeded},
			"os.ErrDeadlineExceeded",
		},
		{"WebSocket close reason", fmt.Errorf("reading: %w", &websocket.CloseError{Code: 1001, Text: title}), "websocket close 1001"},
		{"runtime error of a known code", fmt.Errorf("runtime call x: %w", &runtime.Error{Code: runtime.CodeStaleOwner, Message: title}), "runtime error stale_owner"},
		{"runtime error of an unknown code", &runtime.Error{Code: "private_account_title", Message: title}, "runtime error"},
		{"ref error of a known code", &refs.Error{Code: "stale_ref", Ref: 3, Message: title}, "ref error stale_ref"},
		{"ref error of an unknown code", &refs.Error{Code: "private_account_title", Ref: 3, Message: title}, "ref error"},
		{"JSON syntax error", fmt.Errorf("decoding reply: %w", json.Unmarshal([]byte(`{"a":x}`), new(any))), "JSON syntax error at offset 6"},
		{"JSON type error", json.Unmarshal([]byte(`{"A":"`+title+`"}`), new(struct{ A int })), "JSON type error into int"},
		{
			"joined errors",
			errors.Join(&webkit.CallError{Method: "Network.disable", Err: pageErr}, fmt.Errorf("page %q: %w", title, webkit.ErrPageClosed)),
			"[Network.disable: WebKit protocol error -32000; webkit.ErrPageClosed]",
		},
		{
			"two wrapped errors in one Errorf",
			fmt.Errorf("first %w, then %q %w", webkit.ErrCallTimeout, title, pageErr),
			"[webkit.ErrCallTimeout; WebKit protocol error -32000]",
		},
		{
			"a cause for the log only",
			fmt.Errorf("probe: %w", logcause.Errorf("%w (%v)", webkit.ErrSessionStalled, fmt.Errorf("%q: %w", title, pageErr))),
			"webkit.ErrSessionStalled (also: WebKit protocol error -32000)",
		},
		{"log causes of an error that wraps nothing", causes{errors.New(title), io.EOF}, "[unclassified; io.EOF]"},
		{
			"an error of Errorf: its text with safe arguments",
			fmt.Errorf("release: %w", Errorf("frame %s (context %d, %v): %w", ID("kid"), 7, time.Second, &webkit.CallError{Method: "Runtime.disable", Err: pageErr})),
			"frame kid (context 7, 1s): Runtime.disable: WebKit protocol error -32000",
		},
		{"an argument of Errorf of another kind", Errorf("frame %s: %w", title, io.EOF), "frame unclassified: io.EOF"},
		{"an error of Errorf that wraps nothing", Errorf("frame %s gone", ID("kid")), "frame kid gone"},
		{"an error of Errorf that wraps two", Errorf("frame %s: %w: %w", ID("kid"), webkit.ErrNotSent, context.Canceled), "frame kid: webkit.ErrNotSent: context.Canceled"},
		{"a JSON type error without a Go type", &json.UnmarshalTypeError{Value: "string " + title}, "JSON type error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Error(tc.err)
			if got != tc.want {
				t.Fatalf("Error = %q, want %q", got, tc.want)
			}
		})
	}
}

// An error of Errorf keeps the chain of fmt.Errorf's error: errors.Is
// sees what it wraps.
func TestErrorfKeepsTheChain(t *testing.T) {
	two := Errorf("frame %s: %w: %w", ID("kid"), webkit.ErrNotSent, context.Canceled)
	if !errors.Is(two, webkit.ErrNotSent) || !errors.Is(two, context.Canceled) || two.Error() != "frame kid: not sent: context canceled" {
		t.Fatalf("Errorf with two %%w = %q, errors.Is %v %v", two, errors.Is(two, webkit.ErrNotSent), errors.Is(two, context.Canceled))
	}
	if one := Errorf("frame %s: %w", ID("kid"), io.EOF); !errors.Is(one, io.EOF) || errors.Unwrap(one) != io.EOF {
		t.Fatalf("Errorf with one %%w does not unwrap to it: %v", one)
	}
	if none := Errorf("frame %s gone", ID("kid")); errors.Unwrap(none) != nil || none.Error() != "frame kid gone" {
		t.Fatalf("Errorf without %%w = %v", none)
	}
}

// notSentLike is an error that is webkit.ErrNotSent by its Is method, as
// webkit's not-sent errors are.
type notSentLike struct{ err error }

func (e notSentLike) Error() string        { return "not sent: " + e.err.Error() }
func (e notSentLike) Unwrap() error        { return e.err }
func (e notSentLike) Is(target error) bool { return target == webkit.ErrNotSent }

// causes is an error that names its log causes only.
type causes []error

func (e causes) Error() string      { return "several" }
func (e causes) LogCauses() []error { return e }

// fault returns the Go runtime error of an index out of range.
func fault() (err error) {
	defer func() { err = recover().(error) }()
	s := []int{1}
	i := 3
	_ = s[i]
	return nil
}

// Panic: a recovered panic value without page text; an error as Error
// renders it, any other value, a message too, by its type only. (It
// replaces cdp's panicText and its test.)
func TestPanicOfEachKindOfValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    any
		want string
	}{
		{"error", fmt.Errorf("reading %q: %w", title, errors.New(title)), Unclassified},
		{"fault of the program", fault(), "Go runtime error"},
		{"message", fmt.Sprintf("handler bug at %s on %q", pageURL, title), "a panic value of type string"},
		{"other value", struct{ Title string }{title}, "a panic value of type struct { Title string }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Panic(tc.p); got != tc.want {
				t.Fatalf("Panic = %q, want %q", got, tc.want)
			}
		})
	}
}

// A sentinel of another package is named once it is registered.
func TestRegisterNamesASentinel(t *testing.T) {
	sentinel := errors.New("the page lease ended")
	wrapped := fmt.Errorf("Begin: %w", sentinel)
	if got := Error(wrapped); got != Unclassified {
		t.Fatalf("Error before Register = %q, want %q", got, Unclassified)
	}
	Register(map[string]error{"test.errLeaseEnded": sentinel})
	t.Cleanup(func() { delete(sentinels, sentinel) })
	if got := Error(wrapped); got != "test.errLeaseEnded" {
		t.Fatalf("Error after Register = %q, want the sentinel's name", got)
	}
}

// sliceError is an error type that is not comparable: a sentinel lookup of
// its value would panic.
type sliceError []string

func (e sliceError) Error() string { return strings.Join(e, ",") }

func TestErrorOfANotComparableType(t *testing.T) {
	if got := Error(sliceError{title}); got != "unclassified logsafe.sliceError" {
		t.Fatalf("Error = %q", got)
	}
}

// pageError builds a random error chain from the shapes in which page text
// reaches the program's errors, with page as that text: wrappers that
// quote it or format it (%q, %s, %v, a URL), WebKit protocol errors and
// runtime errors that carry it, plain errors made of it, joins, log
// causes, and the typed errors that Error classifies.
func pageError(r *rand.Rand, page string, depth int) error {
	leaf := func() error {
		switch r.Intn(9) {
		case 0:
			return &webkit.ErrorData{Code: r.Intn(70000) - 35000, Message: page, Data: json.RawMessage(strconv.Quote(page))}
		case 1:
			return &runtime.Error{Code: page, Message: page}
		case 2:
			return &refs.Error{Code: page, Ref: r.Intn(9), Message: page}
		case 3:
			return errors.New(page)
		case 4:
			return &stepError{page}
		case 5:
			return &websocket.CloseError{Code: 1000 + r.Intn(16), Text: page}
		case 6:
			return []error{webkit.ErrPageClosed, context.Canceled, io.EOF, syscall.ECONNRESET}[r.Intn(4)]
		case 7:
			return fault()
		default:
			return &runtime.Error{Code: runtime.CodeStaleOwner, Message: page}
		}
	}
	if depth == 0 {
		return leaf()
	}
	inner := func() error { return pageError(r, page, depth-1) }
	switch r.Intn(10) {
	case 0:
		return fmt.Errorf("page %q: %w", page, inner())
	case 1:
		return fmt.Errorf("navigating to http://localhost:3000/json/%s: %w", page, inner())
	case 2:
		return fmt.Errorf("%w (%v)", inner(), inner())
	case 3:
		return &webkit.CallError{Method: "Runtime.evaluate", Err: inner()}
	case 4:
		return errors.Join(inner(), inner())
	case 5:
		return logcause.Errorf("%w (%v)", inner(), inner())
	case 6:
		return &net.OpError{Op: "read", Net: "tcp", Err: inner()}
	case 7:
		return notSentLike{inner()}
	case 8:
		return Errorf("frame %s %d (%s): %w", ID("fixed"), r.Intn(9), page, inner())
	default:
		return leaf()
	}
}

// Property (review devfix-R2 F5): Error is a function of the types of the
// chain, not of the text in it. Two errors of the same random shape that
// differ only in their page text render the same, and neither rendering
// holds the text. The texts are marked, so that they cannot equal a known
// code or a fixed text of Error.
func TestErrorDependsOnlyOnTypesProperty(t *testing.T) {
	const marker = "QZXQ"
	prop := func(seed int64, a, b string, depth uint8) bool {
		pa, pb := marker+a+marker, marker+b+"x"+marker
		d := int(depth % 5)
		ea := pageError(rand.New(rand.NewSource(seed)), pa, d)
		eb := pageError(rand.New(rand.NewSource(seed)), pb, d)
		ga, gb := Error(ea), Error(eb)
		if ga != gb || strings.Contains(ga, marker) {
			t.Logf("Error(%q) = %q\nError(%q) = %q", ea.Error(), ga, eb.Error(), gb)
			return false
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 3000, Rand: rand.New(rand.NewSource(20261109))}); err != nil {
		t.Fatal(err)
	}
}
