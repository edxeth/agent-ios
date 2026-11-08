package webkit_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// W3b final repair round 4 (the W2 "not written" convention, ErrNotSent):
// every failure of Client.Send before its message is written carries
// ErrNotSent, with the text it had before; a write notice
// (WithWriteNotice) is set only once the message was written, also when
// no reply follows.

func TestClientMarksFailuresBeforeTheWrite(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.EnableTargetRouting("page-1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := webkit.NewClient(ctx, mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	cases := []struct {
		name   string
		send   func(ctx context.Context) error
		text   string
		closes bool
	}{
		{"params that do not marshal", func(ctx context.Context) error {
			_, err := client.Send(ctx, "Runtime.evaluate", map[string]any{"x": make(chan int)})
			return err
		}, "marshaling params: json: unsupported type: chan int", false},
		{"a method that hangs through Target routing", func(ctx context.Context) error {
			_, err := client.Send(ctx, "CSS.enable", nil)
			return err
		}, "CSS.enable: ", false},
		{"a caller that has given up", func(context.Context) error {
			ended, stop := context.WithCancel(context.Background())
			stop()
			_, err := client.Send(ended, "Runtime.enable", nil)
			return err
		}, "Runtime.enable: not sent", false},
		{"a closed client", func(ctx context.Context) error {
			_ = client.Close()
			_, err := client.Send(ctx, "Runtime.enable", nil)
			return err
		}, "connection closed", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			written := new(atomic.Bool)
			err := c.send(webkit.WithWriteNotice(ctx, written))
			if !errors.Is(err, webkit.ErrNotSent) || !strings.HasPrefix(err.Error(), c.text) {
				t.Fatalf("err = %v, want ErrNotSent with the text %q", err, c.text)
			}
			if written.Load() {
				t.Fatal("the write notice is set for a call that was not written")
			}
		})
	}
}

func TestClientWriteNotice(t *testing.T) {
	mock := testutil.NewMockServer()
	defer mock.Close()
	mock.HandleFunc("Test.method", map[string]string{"key": "value"})
	mock.NeverRespond("Test.silent")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := webkit.NewClient(ctx, mock.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	written := new(atomic.Bool)
	if _, err := client.Send(webkit.WithWriteNotice(ctx, written), "Test.method", nil); err != nil || !written.Load() {
		t.Fatalf("answered call: err %v, written %v; want nil and true", err, written.Load())
	}
	written = new(atomic.Bool)
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	_, err = client.Send(webkit.WithWriteNotice(short, written), "Test.silent", nil)
	if err == nil || errors.Is(err, webkit.ErrNotSent) || !written.Load() {
		t.Fatalf("unanswered call: err %v, written %v; want an error without ErrNotSent and true", err, written.Load())
	}
}

func TestSessionWriteNotice(t *testing.T) {
	f := fakewebkit.New(fakewebkit.Options{})
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) {
		return webkit.DialWebSocket(ctx, f.URL, transport.Limits{Routed: true})
	}, webkit.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	written := new(atomic.Bool)
	if _, err := s.Send(webkit.WithWriteNotice(ctx, written), "Runtime.evaluate", map[string]any{"expression": "1"}); err != nil || !written.Load() {
		t.Fatalf("answered call: err %v, written %v; want nil and true", err, written.Load())
	}
	written = new(atomic.Bool)
	_, err = s.Send(webkit.WithWriteNotice(ctx, written), "Runtime.evaluate", map[string]any{"x": make(chan int)})
	if !errors.Is(err, webkit.ErrNotSent) || !strings.HasPrefix(err.Error(), "Runtime.evaluate: marshaling params: ") || written.Load() {
		t.Fatalf("params that do not marshal: err %v, written %v; want ErrNotSent and false", err, written.Load())
	}
	written = new(atomic.Bool)
	f.Stall("Runtime.evaluate")
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	_, err = s.Send(webkit.WithWriteNotice(short, written), "Runtime.evaluate", map[string]any{"expression": "2"})
	if err == nil || errors.Is(err, webkit.ErrNotSent) || !written.Load() {
		t.Fatalf("unanswered call: err %v, written %v; want an error without ErrNotSent and true", err, written.Load())
	}
}
