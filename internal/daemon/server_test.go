package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit/testutil/fakewebkit"
)

// startInProcess runs a daemon in this process on a private socket
// directory and returns client options that never start another one.
func startInProcess(t *testing.T, ro RunOptions) Options {
	t.Helper()
	base := shortDir(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	ro.Dir = filepath.Join(base, "iwdp")
	if ro.Version == "" {
		ro.Version = "test"
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, ro) }()
	o := Options{Dir: ro.Dir, NoStart: true, Version: "test"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := Connect(ctxFor(t, time.Second), o)
		if err == nil {
			c.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("daemon ended at start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("in-process daemon did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("in-process daemon ended with an error: %v", err)
			}
		case <-time.After(6 * time.Second):
			t.Errorf("in-process daemon did not stop within 6 s")
		}
	})
	return o
}

func connect(t *testing.T, o Options) *Client {
	t.Helper()
	c, err := Connect(ctxFor(t, 5*time.Second), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func openPage(t *testing.T, c *Client, f *fakewebkit.Server, session string, d time.Duration) (*PageCaller, error) {
	t.Helper()
	return c.OpenPage(ctxFor(t, d), PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: session}, Name: "test"})
}

func newFake(t *testing.T, o fakewebkit.Options) *fakewebkit.Server {
	t.Helper()
	f := fakewebkit.New(o)
	t.Cleanup(f.Close)
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 2}})
	return f
}

// The legacy bridge (design 2.10): a fixed operation per CLI command, raw
// calls inside it, events through /v1/events, and Close ends the
// operation while the lease stays with the session.
func TestPageCallerFixedOperation(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	pc, err := openPage(t, c, f, "s1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !pc.IsTargetRouted() {
		t.Errorf("IsTargetRouted = false for a fake that announces its target")
	}
	got := make(chan string, 4)
	unsub := pc.Subscribe("Console.messageAdded", func(method string, params json.RawMessage) { got <- string(params) })
	res, err := pc.Send(ctxFor(t, 5*time.Second), "Runtime.evaluate", map[string]any{"expression": "1+1"})
	if err != nil || !strings.Contains(string(res), "\"value\":2") {
		t.Fatalf("Runtime.evaluate: %s, %v", res, err)
	}
	f.Emit("Console.messageAdded", map[string]any{"message": map[string]any{"text": "hi"}})
	select {
	case p := <-got:
		if !strings.Contains(p, "hi") {
			t.Fatalf("event params %s", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("event did not arrive through /v1/events")
	}
	unsub()
	if err := pc.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pc.Done():
	default:
		t.Error("Done is not closed after Close")
	}
	st, err := c.Status(ctxFor(t, 5*time.Second))
	if err != nil || len(st.Pages) != 1 || st.Pages[0].Session != "s1" || st.Pages[0].Lease != "active" {
		t.Fatalf("status after Close: %+v, %v; want s1's lease kept", st, err)
	}
}

// An open legacy operation is exclusive (design 2.10): a second command of
// the same session waits for it, and its deadline exits 124 (D9).
func TestOperationIsExclusiveUntilClosed(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	a, err := openPage(t, c, f, "s1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = openPage(t, c, f, "s1", 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || commands.ExitCodeOf(err) != 124 {
		t.Fatalf("second operation while the first is open: %v (exit %d), want a deadline error with exit 124", err, commands.ExitCodeOf(err))
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("second operation ended after %v with a 300ms deadline", el)
	}
	_ = a.Close()
	b, err := openPage(t, c, f, "s1", 2*time.Second)
	if err != nil {
		t.Fatalf("after Close: %v", err)
	}
	_ = b.Close()
}

// Design 2.5: another session gets the lease error (exit 4) at once.
func TestOtherSessionGetsLeaseError(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	a, err := openPage(t, c, f, "s1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	_, err = openPage(t, c, f, "s2", 5*time.Second)
	if commands.ExitCodeOf(err) != 4 || err == nil || !strings.Contains(err.Error(), "leased by session \"s1\"") {
		t.Fatalf("s2: %v (exit %d), want the lease error with exit 4", err, commands.ExitCodeOf(err))
	}
}

// Design 2.5 rule 3: a page connection closed by someone else closes Done
// and the next command fails with exit 4 and "ownership is unknown".
func TestPageCallerDoneWhenPageLost(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	pc, err := openPage(t, c, f, "s1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	f.CloseConnections()
	select {
	case <-pc.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done not closed after the page connection closed")
	}
	_, err = pc.Send(ctxFor(t, 2*time.Second), "Runtime.evaluate", map[string]any{"expression": "1"})
	if commands.ExitCodeOf(err) != 4 || !strings.Contains(err.Error(), "ownership is unknown") {
		t.Fatalf("send after the loss: %v (exit %d)", err, commands.ExitCodeOf(err))
	}
	if n := f.ConnectionCount(); n != 1 {
		t.Fatalf("the daemon reconnected by itself: %d connections", n)
	}
}

// After EndOp (a streaming command after its enable step) each call runs
// as its own one-call operation, so other commands are not blocked.
func TestEndOpLeavesOneCallOperations(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	pc, err := openPage(t, c, f, "s1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	pc.EndOp()
	other, err := openPage(t, c, f, "s1", time.Second)
	if err != nil {
		t.Fatalf("operation after EndOp: %v", err)
	}
	_ = other.Close()
	if _, err := pc.Send(ctxFor(t, 2*time.Second), "Runtime.evaluate", map[string]any{"expression": "1"}); err != nil {
		t.Fatalf("one-call send after EndOp: %v", err)
	}
}

// The MCP server's scopes (design 2.10): every call of one tool call runs in
// one exclusive operation that begins at the first call and ends with the
// scope, so another tool call of the session waits for it.
func TestScopeRunsOneOperationPerToolCall(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	pc, err := c.AttachPage(ctxFor(t, 5*time.Second), PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: "mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	ctxA, endA := pc.Scope(ctxFor(t, 5*time.Second))
	if _, err := pc.Send(ctxA, "Runtime.evaluate", map[string]any{"expression": "1"}); err != nil {
		t.Fatal(err)
	}
	ctxB, endB := pc.Scope(ctxFor(t, 5*time.Second))
	sent := make(chan time.Time, 1)
	go func() {
		_, _ = pc.Send(ctxB, "Runtime.evaluate", map[string]any{"expression": "2"})
		sent <- time.Now()
	}()
	time.Sleep(300 * time.Millisecond)
	ended := time.Now()
	endA()
	select {
	case at := <-sent:
		if at.Before(ended) {
			t.Fatalf("scope B's call ran while scope A's operation was open")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scope B's call did not run after scope A ended")
	}
	endB()
	// Without a scope, a call is a one-call operation.
	if _, err := pc.Send(ctxFor(t, 2*time.Second), "Runtime.evaluate", map[string]any{"expression": "3"}); err != nil {
		t.Fatal(err)
	}
}

// A stalled call ends at the caller's deadline as a deadline error that
// names the method (W0's timeout message), exit 124 (D9).
func TestSendDeadlineNamesMethod(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	f.Stall("Runtime.evaluate")
	ctx := ctxFor(t, 500*time.Millisecond)
	pc, err := c.OpenPage(ctx, PageOptions{Page: PageReq{Page: f.URL}, Owner: Owner{Session: "s1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	start := time.Now()
	_, err = pc.Send(ctx, "Runtime.evaluate", map[string]any{"expression": "6*7"})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "Runtime.evaluate") || commands.ExitCodeOf(err) != 124 {
		t.Fatalf("stalled call: %v (exit %d)", err, commands.ExitCodeOf(err))
	}
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Fatalf("stalled call returned after %v with a 500ms deadline", el)
	}
	n := 0
	for _, r := range f.ReceivedMethod("Runtime.evaluate") {
		if strings.Contains(string(r.Params), "6*7") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the fake received %d evaluations of 6*7, want 1 (no retry; probes excluded)", n)
	}
}

// Raw-call gates run in the daemon (design 2.10): the CSS hang methods are
// refused before sending, and the error says the call was not sent.
func TestRawGateRefusesHangMethod(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	pc, err := openPage(t, c, f, "s1", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	_, err = pc.Send(ctxFor(t, 2*time.Second), "CSS.getAllStyleSheets", nil)
	if !errors.Is(err, webkit.ErrNotSent) || len(f.ReceivedMethod("CSS.getAllStyleSheets")) != 0 {
		t.Fatalf("CSS.getAllStyleSheets: %v; the fake received %d", err, len(f.ReceivedMethod("CSS.getAllStyleSheets")))
	}
}

// /v1/run runs a registry command in the daemon with the caller's session
// and streams its lines before the result.
func TestRunRegistryCommand(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	var lines []string
	cmd := &commands.Command{
		Name: "fixture daemon echo", Summary: "Echo the session.",
		Args: []commands.Arg{{Name: "words", Help: "words to echo", Variadic: true}},
		Run: func(_ context.Context, env commands.Env, in commands.Input) (commands.Result, error) {
			env.Emit("line one")
			if _, ok := env.(interface{ Manager() *core.Manager }); !ok {
				return commands.Result{}, errors.New("the daemon Env has no Manager")
			}
			return commands.Result{Text: "session " + in.Global.Session + " " + strings.Join(in.Args, ","), Data: map[string]string{"s": in.Global.Session}}, nil
		},
	}
	commands.Register(cmd)
	t.Cleanup(func() { commands.Unregister(cmd) })
	res, err := c.Run(ctxFor(t, 5*time.Second), RunRequest{Argv: []string{"fixture", "daemon", "echo", "--", "-a", "b"}, Owner: Owner{Session: "s9"}},
		func(line string) { lines = append(lines, line) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "session s9 -a,b" || string(res.Data) != "{\"s\":\"s9\"}" || strings.Join(lines, "|") != "line one" {
		t.Fatalf("run: %+v, lines %q", res, lines)
	}
	_, err = c.Run(ctxFor(t, 5*time.Second), RunRequest{Argv: []string{"fixture", "daemon", "echo", "--all"}}, nil)
	if commands.ExitCodeOf(err) != 2 {
		t.Fatalf("unknown flag through /v1/run: %v (exit %d), want 2", err, commands.ExitCodeOf(err))
	}
}

// The hello carries a stable daemon identity (a W3a hook for
// runtime.DaemonID) that differs between daemon processes.
func TestHelloCarriesDaemonID(t *testing.T) {
	a := connect(t, startInProcess(t, RunOptions{}))
	b := connect(t, startInProcess(t, RunOptions{}))
	if a.Hello.ID == "" || a.Hello.ID == b.Hello.ID || a.Hello.Protocol != Protocol {
		t.Fatalf("hello ids %q and %q", a.Hello.ID, b.Hello.ID)
	}
}
