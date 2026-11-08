package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// L05 (w1c backlog): when the caller cancels RunCLI's context with
// ErrInterrupted (a signal ended the command; the caller exits 130), RunCLI
// prints no "Error: ..." line for the step that the cancellation ended:
// the client step, building the environment, or the run. Another
// cancellation cause keeps the error line.
func TestRunCLIPrintsNoErrorForTheSignalsCancellation(t *testing.T) {
	stepErr := errors.New("step canceled")
	c := &Command{
		Name: "test signal step", Summary: "Test fixture: fail in the client step or the run.", Page: NoPage,
		ClientStep: func(ctx context.Context, in *Input, _ io.Reader) error {
			if len(in.Args) > 0 && in.Args[0] == "step" {
				return stepErr
			}
			return nil
		},
		Args: []Arg{{Name: "where", Help: "step, env or run"}},
		Run: func(ctx context.Context, _ Env, _ Input) (Result, error) {
			return Result{}, ctx.Err()
		},
	}
	Register(c)
	t.Cleanup(func() { Unregister(c) })
	for _, where := range []string{"step", "env", "run"} {
		for _, cause := range []error{ErrInterrupted, errors.New("another cause")} {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(cause)
			var out, errb bytes.Buffer
			code := RunCLI(ctx, []string{"test", "signal", "step", where}, &out, &errb, func(context.Context, *Invocation) (Env, error) {
				if where == "env" {
					return nil, errors.New("connecting: context canceled")
				}
				return fakeEnv{}, nil
			})
			if code != ExitError {
				t.Errorf("%s, cause %v: exit %d, want %d", where, cause, code, ExitError)
			}
			if printed := strings.Contains(errb.String(), "Error: "); printed == (cause == ErrInterrupted) {
				t.Errorf("%s, cause %v: stderr %q", where, cause, errb.String())
			}
		}
	}
}
