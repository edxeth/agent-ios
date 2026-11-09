package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// emptyReplyListing accepts each request on its listing and closes the
// connection without a reply, as the iwdp of the second device check did
// while a page WebSocket was open.
func emptyReplyListing(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijacking the listing request: %v", err)
			return
		}
		_ = c.Close()
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// F2 of the second device check: the error names the observed failure, and
// gives the time limit only when the limit was hit.
func TestCheckListingNamesTheObservedFailure(t *testing.T) {
	t.Run("empty reply", func(t *testing.T) {
		t.Parallel()
		addr := emptyReplyListing(t)
		err := CheckListing(context.Background(), addr)
		if err == nil || errors.Is(err, ErrNotListening) {
			t.Fatalf("CheckListing on an owned port with an empty reply = %v, want an error other than ErrNotListening", err)
		}
		for _, want := range []string{addr, "closed the connection without an answer", "empty reply", "WebSocket to one of its pages is open", "iwdp-cli daemon stop", "no second iwdp"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
		for _, bad := range []string{"within", "busy or stuck"} {
			if strings.Contains(err.Error(), bad) {
				t.Errorf("error %q contains %q, but no limit was hit and nothing hung", err, bad)
			}
		}
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		addr := silentListing(t)
		err := CheckListing(context.Background(), addr)
		for _, want := range []string{addr, "does not answer GET /json within " + ProbeBudget.String(), "busy or stuck", "no second iwdp"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error %v does not contain %q", err, want)
			}
		}
		if err != nil && strings.Contains(err.Error(), "without an answer") {
			t.Errorf("error %q reports an empty reply for a listing that never answered", err)
		}
	})
	t.Run("other", func(t *testing.T) {
		t.Parallel()
		// A port that answers with something other than HTTP.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_, _ = c.Write([]byte("SSH-2.0-not-http\r\n\r\n"))
				_ = c.Close()
			}
		}()
		err = CheckListing(context.Background(), l.Addr().String())
		for _, want := range []string{l.Addr().String(), "failed", "no second iwdp"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error %v does not contain %q", err, want)
			}
		}
		for _, bad := range []string{"within", "without an answer"} {
			if err != nil && strings.Contains(err.Error(), bad) {
				t.Errorf("error %q contains %q", err, bad)
			}
		}
	})
}

// Review F5: a read of the listing that fails after its check passed is
// classified as CheckListing classifies the check (ReadFailure): an empty
// reply with the advice, a timeout with the read's limit, and
// ErrNotListening when nothing owns the address. The read's own advice
// ("Is ios_webkit_debug_proxy running?") is dropped.
func TestReadFailureClassifiesAFailedRead(t *testing.T) {
	read := func(addr string) error {
		t.Helper()
		_, err := listDevicesFromURL("http://" + addr + "/json")
		if err == nil {
			t.Fatalf("reading %s succeeded", addr)
		}
		return err
	}
	check := func(t *testing.T, err error, want, bad []string) {
		t.Helper()
		if err == nil || errors.Is(err, ErrNotListening) {
			t.Fatalf("ReadFailure = %v, want an error of an owned port", err)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("error %q does not contain %q", err, w)
			}
		}
		for _, b := range bad {
			if strings.Contains(err.Error(), b) {
				t.Errorf("error %q contains %q", err, b)
			}
		}
	}
	t.Run("empty reply", func(t *testing.T) {
		t.Parallel()
		addr := emptyReplyListing(t)
		err := ReadFailure(context.Background(), addr, read(addr))
		check(t, err, []string{addr, "closed the connection without an answer", "iwdp-cli daemon stop"}, []string{"within", "running?"})
	})
	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		addr := silentListing(t)
		err := ReadFailure(context.Background(), addr, read(addr))
		check(t, err, []string{addr, "does not answer GET /json within " + readTimeout.String(), "busy or stuck"}, []string{"without an answer", "running?"})
	})
	t.Run("unowned", func(t *testing.T) {
		t.Parallel()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		if err := ReadFailure(context.Background(), addr, read(addr)); !errors.Is(err, ErrNotListening) {
			t.Fatalf("ReadFailure on a port that nothing owns = %v, want ErrNotListening", err)
		}
	})
}

// Review F6: a read that received an HTTP 200 answer whose body is not the
// expected JSON (empty, or cut short) failed while decoding, not in the
// request: ReadFailure names the decoding failure, never an empty reply,
// and gives no advice about open page WebSockets.
func TestReadFailureNamesADecodingFailure(t *testing.T) {
	for _, kind := range []string{"device", "page"} {
		for _, body := range []string{"", "["} {
			t.Run(kind+" list, body "+strconv.Quote(body), func(t *testing.T) {
				t.Parallel()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				}))
				t.Cleanup(srv.Close)
				addr := strings.TrimPrefix(srv.URL, "http://")
				var err error
				if kind == "device" {
					_, err = listDevicesFromURL(srv.URL + "/json")
				} else {
					_, p, splitErr := net.SplitHostPort(addr)
					if splitErr != nil {
						t.Fatal(splitErr)
					}
					n, convErr := strconv.Atoi(p)
					if convErr != nil {
						t.Fatal(convErr)
					}
					_, err = ListPages(n)
				}
				if err == nil {
					t.Fatalf("reading the %s list of body %q succeeded", kind, body)
				}
				got := ReadFailure(context.Background(), addr, err)
				if got == nil || errors.Is(got, ErrNotListening) {
					t.Fatalf("ReadFailure = %v, want an error of an owned port", got)
				}
				for _, want := range []string{addr, "answered GET /json", "decoding " + kind + " list", "no second iwdp"} {
					if !strings.Contains(got.Error(), want) {
						t.Errorf("error %q does not contain %q", got, want)
					}
				}
				for _, bad := range []string{"without an answer", "empty reply", "WebSocket", "daemon stop", "within"} {
					if strings.Contains(got.Error(), bad) {
						t.Errorf("error %q contains %q, but an answer was received and could not be decoded", got, bad)
					}
				}
			})
		}
	}
}
