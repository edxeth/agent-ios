package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nnemirovsky/agent-ios/internal/commands"
	"github.com/nnemirovsky/agent-ios/internal/core"
	"github.com/nnemirovsky/agent-ios/internal/webkit/testutil/fakewebkit"
)

// startInProcess runs a daemon in this process on a private socket
// directory and returns client options that never start another one.
func startInProcess(t *testing.T, ro RunOptions) Options {
	t.Helper()
	base := shortDir(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	ro.Dir = filepath.Join(base, "agent-ios")
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

// leaseFixture is a registry command that needs a page and sends each of
// its arguments, a protocol method without parameters, through the
// operation's caller. For "<Domain>.enable" the session's "test" owner
// acquires the domain first, as a command that enables a domain does, so
// the release of the session sends "<Domain>.disable". The session's lease
// stays after the command (design 2.5). It is registered at init, so a
// daemon process started from this test binary (spawnOptions) has it too.
var leaseFixture = []string{"fixture", "lease"}

func init() {
	commands.Register(&commands.Command{
		Name: strings.Join(leaseFixture, " "), Summary: "Test fixture: lease the page and send methods.", Page: commands.AnyPage, Mutates: true,
		Args: []commands.Arg{{Name: "methods", Help: "protocol methods to send", Variadic: true}},
		Run: func(ctx context.Context, env commands.Env, in commands.Input) (commands.Result, error) {
			ps, err := env.Page(ctx)
			if err != nil {
				return commands.Result{}, err
			}
			c := env.(interface{ Op() *core.Op }).Op().Caller()
			for _, m := range in.Args {
				if domain, ok := strings.CutSuffix(m, ".enable"); ok {
					ps.State.Acquire(core.SessionOwner("test", in.Global.Session), domain)
				}
				if _, err := c.Send(ctx, m, nil); err != nil {
					return commands.Result{}, err
				}
			}
			return commands.Result{}, nil
		},
	})
}

// leasePage runs leaseFixture on page for owner through /v1/run within d.
func leasePage(t *testing.T, c *Client, page string, owner Owner, d time.Duration, methods ...string) error {
	t.Helper()
	_, err := c.Run(ctxFor(t, d), RunRequest{Argv: append(append([]string{}, leaseFixture...), methods...), PageReq: PageReq{Page: page}, Owner: owner}, nil)
	return err
}

func newFake(t *testing.T, o fakewebkit.Options) *fakewebkit.Server {
	t.Helper()
	f := fakewebkit.New(o)
	t.Cleanup(f.Close)
	f.Result("Runtime.evaluate", map[string]any{"result": map[string]any{"type": "number", "value": 2}})
	return f
}

// Design 2.5: another session gets the lease error (exit 4) at once.
func TestOtherSessionGetsLeaseError(t *testing.T) {
	o := startInProcess(t, RunOptions{})
	c := connect(t, o)
	f := newFake(t, fakewebkit.Options{})
	if err := leasePage(t, c, f.URL, Owner{Session: "s1"}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	err := leasePage(t, c, f.URL, Owner{Session: "s2"}, 5*time.Second)
	if commands.ExitCodeOf(err) != 4 || err == nil || !strings.Contains(err.Error(), "leased by session \"s1\"") {
		t.Fatalf("s2: %v (exit %d), want the lease error with exit 4", err, commands.ExitCodeOf(err))
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
