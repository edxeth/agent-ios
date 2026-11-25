package webkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/quick"

	"github.com/edxeth/agent-ios/internal/transport"
)

// pipeConn is an in-memory routed MessageConn: it announces page-1 and
// answers every routed command with an empty result. It records each write.
type pipeConn struct {
	limits transport.Limits
	in     chan []byte
	mu     sync.Mutex
	writes [][]byte
	once   sync.Once
	closed chan struct{}
}

func newPipeConn(limit int) *pipeConn {
	c := &pipeConn{limits: transport.Limits{MaxOutgoing: limit, Routed: true}, in: make(chan []byte, 16), closed: make(chan struct{})}
	c.in <- []byte(`{"method":"Target.targetCreated","params":{"targetInfo":{"targetId":"page-1","type":"page"}}}`)
	return c
}

func (c *pipeConn) Limits() transport.Limits { return c.limits }

func (c *pipeConn) Write(ctx context.Context, msg []byte) error {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), msg...))
	c.mu.Unlock()
	var outer struct {
		Params struct{ Message string }
	}
	if err := json.Unmarshal(msg, &outer); err != nil {
		return err
	}
	var inner struct{ ID int64 }
	if err := json.Unmarshal([]byte(outer.Params.Message), &inner); err != nil {
		return err
	}
	reply, _ := json.Marshal(map[string]any{"method": "Target.dispatchMessageFromTarget", "params": map[string]string{
		"targetId": "page-1", "message": fmt.Sprintf(`{"id":%d,"result":{}}`, inner.ID),
	}})
	c.in <- reply
	return nil
}

func (c *pipeConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case m := <-c.in:
		return m, nil
	case <-c.closed:
		return nil, errors.New("closed")
	}
}

func (c *pipeConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *pipeConn) written() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.writes...)
}

// escapeHeavy is a string rich in quotes, backslashes and control
// characters, whose JSON-in-JSON encoding is much larger than its length.
type escapeHeavy string

func (escapeHeavy) Generate(r *rand.Rand, size int) reflect.Value {
	alphabet := []string{"\"", "\\", "\n", "\t", "\x00", "\x1f", "\u2028", "<", "a", "é", "\u00ff"}
	n := r.Intn(size*40 + 1)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(alphabet[r.Intn(len(alphabet))])
	}
	return reflect.ValueOf(escapeHeavy(b.String()))
}

// sendOnce starts a session on a fresh pipeConn and sends one evaluate of
// expr. Fresh sessions use the same ids, so equal inputs give equal bytes.
func sendOnce(t *testing.T, limit int, expr string) (*pipeConn, error) {
	t.Helper()
	c := newPipeConn(limit)
	s, err := NewSession(context.Background(), c, SessionOptions{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = s.Close() }()
	_, err = s.Send(context.Background(), "Runtime.evaluate", map[string]string{"expression": expr})
	return c, err
}

// 7.3 test 6: a message is written exactly when its Target-routed wire size
// is at or below MaxOutgoing; a refused message names its size and the
// limit and writes nothing.
func TestWireLimitIsCheckedOnSerializedOuterMessage(t *testing.T) {
	prop := func(e escapeHeavy) bool {
		expr := string(e)
		free, err := sendOnce(t, 0, expr)
		if err != nil {
			t.Logf("unlimited send: %v", err)
			return false
		}
		w := free.written()
		if len(w) != 1 {
			t.Logf("unlimited send wrote %d messages", len(w))
			return false
		}
		wire := w[0]
		// The written bytes are the Target-routed form of expr.
		var outer struct {
			Method string
			Params struct{ TargetID, Message string }
		}
		if err := json.Unmarshal(wire, &outer); err != nil || outer.Method != "Target.sendMessageToTarget" {
			t.Logf("outer message %q: %v", wire, err)
			return false
		}
		var inner struct {
			Params struct{ Expression string }
		}
		if err := json.Unmarshal([]byte(outer.Params.Message), &inner); err != nil || inner.Params.Expression != expr {
			t.Logf("inner message did not round-trip: %v", err)
			return false
		}
		size := len(wire)
		for _, limit := range []int{size - 1, size, size + 1} {
			c, err := sendOnce(t, limit, expr)
			n := len(c.written())
			if limit >= size {
				if err != nil || n != 1 {
					t.Logf("limit %d, size %d: err %v, writes %d; want written", limit, size, err, n)
					return false
				}
				continue
			}
			var wl *WireLimitError
			want := fmt.Sprintf("%d bytes on the wire; the limit for this transport is %d", size, limit)
			if !errors.As(err, &wl) || n != 0 || !strings.Contains(err.Error(), want) {
				t.Logf("limit %d, size %d: err %v, writes %d; want refused before writing with %q", limit, size, err, n, want)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatal(err)
	}
}
