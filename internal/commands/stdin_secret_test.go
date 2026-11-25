package commands

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// stdinPipe replaces os.Stdin with a pipe that holds content until the
// test ends. With closeWriter the reader then sees end of file; without
// it, a read past content blocks, as a terminal or an open pipe does.
func stdinPipe(t *testing.T, content string, closeWriter bool) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		_ = w.Close()
		_ = r.Close()
	})
	if _, err := w.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if closeWriter {
		_ = w.Close()
	}
}

// runCLIWithin runs args through RunCLIStarted and fails the test when it has
// not returned within limit. envBuilt reports whether the command got as
// far as building its environment (a connection to the daemon).
func runCLIWithin(t *testing.T, limit time.Duration, args []string, newEnv func(context.Context, *Invocation) (Env, error)) (code int, out string, envBuilt bool) {
	t.Helper()
	type result struct {
		code int
		out  string
	}
	built := make(chan struct{}, 1)
	done := make(chan result, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := RunCLIStarted(context.Background(), args, &stdout, &stderr, nil, func(ctx context.Context, inv *Invocation) (Env, error) {
			built <- struct{}{}
			return newEnv(ctx, inv)
		})

		done <- result{code, stdout.String() + stderr.String()}
	}()
	select {
	case res := <-done:
		return res.code, res.out, len(built) > 0
	case <-time.After(limit):
		t.Fatalf("%v: still running after %v", args, limit)
		return 0, "", false
	}
}

// Design 2.10 and the deadline rule of docs/guide.md (every agent-ios command
// runs under its deadline; a deadline error exits 124): the client step of
// fill --stdin reads under the command deadline, so a standard input that
// never ends stops the command at the deadline with exit 124. What was
// read reaches no output, and the command never reaches the daemon.
func TestFillStdinEndsAtTheDeadline(t *testing.T) {
	const partial = "partial-s3cret"
	stdinPipe(t, partial, false)
	code, out, envBuilt := runCLIWithin(t, 3*time.Second, []string{"--timeout", "50ms", "fill", "#pw", "--stdin"},
		func(context.Context, *Invocation) (Env, error) { return nil, errors.New("no daemon in this test") })
	if code != ExitTimeout || envBuilt || strings.Contains(out, partial) {
		t.Fatalf("exit %d, env built %v, output %q; want exit %d before any connection, without the partial input", code, envBuilt, out, ExitTimeout)
	}
}
