package commands

import (
	"bytes"
	"context"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

// Design 2.5: --reservation (AGENT_IOS_DEVICE_RESERVATION), --wait-lease and
// --takeover are global flags of every command.
func TestParseOwnershipGlobals(t *testing.T) {
	withFixtures(t)
	env := func(k string) string {
		if k == "AGENT_IOS_DEVICE_RESERVATION" {
			return "orchestrator"
		}
		return ""
	}
	inv, err := Parse([]string{"fixture", "click", "a"}, env)
	if err != nil || inv.Input.Global.Reservation != "orchestrator" {
		t.Fatalf("env reservation: %+v, %v", inv, err)
	}
	inv, err = Parse([]string{"--reservation", "w1", "--wait-lease=2s", "--takeover", "fixture", "click", "a"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if g := inv.Input.Global; g.Reservation != "w1" || g.WaitLease != 2*time.Second || !g.Takeover {
		t.Fatalf("globals = %+v, want reservation w1, wait 2s, takeover", g)
	}
	if _, err := Parse([]string{"--wait-lease", "soon", "fixture"}, env); ExitCodeOf(err) != ExitUsage {
		t.Fatalf("bad --wait-lease: %v (exit %d), want a usage error", err, ExitCodeOf(err))
	}
}

// randomInput builds a valid Input for c from r: values that start with
// "-", contain "=" or spaces, and empty strings are included on purpose.
func randomInput(c *Command, r *rand.Rand) Input {
	in := Input{Flags: map[string]any{}}
	pick := func() string {
		vals := []string{"", "-x", "--y", "a=b", "two words", "--", "@e1", "ü", "-", "=", "--force"}
		if r.Intn(3) == 0 {
			b := make([]byte, r.Intn(6))
			for i := range b {
				b[i] = byte(32 + r.Intn(95))
			}
			return string(b)
		}
		return vals[r.Intn(len(vals))]
	}
	for _, f := range c.Flags {
		if r.Intn(2) == 0 {
			continue
		}
		switch f.Type {
		case FlagInt:
			in.Flags[f.Name] = r.Intn(2000) - 1000
		case FlagBool:
			in.Flags[f.Name] = r.Intn(2) == 0
		case FlagDuration:
			in.Flags[f.Name] = time.Duration(r.Int63n(int64(time.Hour)))
		case FlagEnum:
			in.Flags[f.Name] = f.Enum[r.Intn(len(f.Enum))]
		default:
			in.Flags[f.Name] = pick()
		}
	}
	for i, a := range c.Args {
		if a.Variadic {
			for n := r.Intn(4); n > 0; n-- {
				in.Args = append(in.Args, pick())
			}
			break
		}
		if !a.Required && r.Intn(2) == 0 {
			break
		}
		_ = i
		in.Args = append(in.Args, pick())
	}
	return in
}

// Property (design 2.2, /v1/run sends argv): Parse(Argv(c, in)) gives back
// the command, its flags and its arguments for any valid input, so a
// command line reaches the daemon unchanged.
func TestArgvRoundTripsThroughParse(t *testing.T) {
	withFixtures(t)
	for _, c := range fixtures() {
		c, _ := Lookup(strings.Fields(c.Name))
		prop := func(seed int64) bool {
			in := randomInput(c, rand.New(rand.NewSource(seed)))
			inv, err := Parse(Argv(c, in), func(string) string { return "" })
			if err != nil || inv.Cmd != c {
				t.Logf("argv %q: %v", Argv(c, in), err)
				return false
			}
			want := map[string]any{}
			for _, f := range c.Flags { // defaults are filled in by Parse
				if f.Default != "" {
					want[f.Name], _ = parseFlagValue(f, f.Default)
				}
			}
			for k, v := range in.Flags {
				want[k] = v
			}
			if !reflect.DeepEqual(inv.Input.Flags, want) || !reflect.DeepEqual(nilIfEmpty(inv.Input.Args), nilIfEmpty(in.Args)) {
				t.Logf("argv %q: got flags %v args %q, want %v %q", Argv(c, in), inv.Input.Flags, inv.Input.Args, want, in.Args)
				return false
			}
			return true
		}
		if err := quick.Check(prop, &quick.Config{MaxCount: 500}); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

type remoteEnv struct {
	fakeEnv
	got []string
}

func (e *remoteEnv) RunRemote(_ context.Context, c *Command, in Input) (Result, error) {
	e.got = Argv(c, in)
	return Result{Text: "remote ran"}, nil
}

// Design 2.10: the CLI parses with the registry spec, then sends the command
// to the daemon; a Local command (daemon lifecycle) runs in the client.
func TestRunCLIRunsRemoteEnvCommandsRemotely(t *testing.T) {
	withFixtures(t)
	local := &Command{
		Name: "fixture local", Summary: "Runs in the client.", Local: true,
		Run: func(context.Context, Env, Input) (Result, error) { return Result{Text: "local ran"}, nil },
	}
	Register(local)
	t.Cleanup(func() { Unregister(local) })
	env := &remoteEnv{}
	newEnv := func(context.Context, *Invocation) (Env, error) { return env, nil }
	var out, errb bytes.Buffer
	if code := RunCLIStarted(context.Background(), []string{"--session", "s1", "fixture", "click", "--force", "@e1"}, &out, &errb, nil, newEnv); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if out.String() != "remote ran\n" || runs != 0 || strings.Join(env.got, " ") != "fixture click --button=left --force=true -- @e1" {
		t.Fatalf("stdout %q, local runs %d, remote argv %q", out.String(), runs, env.got)
	}
	out.Reset()
	if code := RunCLIStarted(context.Background(), []string{"fixture", "local"}, &out, &errb, nil, newEnv); code != 0 || out.String() != "local ran\n" {
		t.Fatalf("local command: exit %d, stdout %q", code, out.String())
	}
}
