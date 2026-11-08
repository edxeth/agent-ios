package commands

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/nnemirovsky/iwdp-mcp/internal/cdp"
)

// cdpEnv is the Env of a daemon with a CDP endpoint (runEnv.CDP).
type cdpEnv struct {
	fakeEnv
	srv *cdp.Server
}

func (e cdpEnv) CDP() *cdp.Server { return e.srv }

func runCmd(t *testing.T, env Env, argv ...string) (Result, error) {
	t.Helper()
	inv, err := Parse(argv, func(string) string { return "" })
	if err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	return inv.Cmd.Run(context.Background(), env, inv.Input)
}

// The cdp commands and get cdp-url run in the daemon, which owns the CDP
// endpoint (design 4.2, 5.1); elsewhere they fail and change nothing, and
// close has no endpoint records to revoke.
func TestCDPCommandsNeedTheDaemon(t *testing.T) {
	for _, argv := range [][]string{{"cdp", "start"}, {"cdp", "stop"}, {"cdp", "status"}, {"cdp", "log"}, {"get", "cdp-url"}} {
		if _, err := runCmd(t, fakeEnv{}, argv...); !errors.Is(err, errNeedsDaemon) {
			t.Errorf("%s outside the daemon: %v, want %v", strings.Join(argv, " "), err, errNeedsDaemon)
		}
	}
	if n := revokeCDP(fakeEnv{}, "s1"); n != 0 {
		t.Fatalf("revokeCDP outside the daemon = %d, want 0", n)
	}
}

// cdp start --allow-origin, cdp status and cdp log texts (design 5.1,
// 5.4): the status names the address and the allowed origins; the log
// says when no unknown method arrived.
func TestCDPCommandTexts(t *testing.T) {
	env := cdpEnv{srv: cdp.New(cdp.Options{})}
	t.Cleanup(func() { env.srv.Stop() })
	st, err := runCmd(t, env, "cdp", "status")
	if err != nil || !strings.Contains(st.Text, "not running") {
		t.Fatalf("cdp status before start: %q, %v", st.Text, err)
	}
	res, err := runCmd(t, env, "cdp", "start", "--port", "0", "--allow-origin", "http://a.test,http://b.test")
	if err != nil {
		t.Fatal(err)
	}
	addr := res.Data.(map[string]any)["addr"].(string)
	if !strings.HasPrefix(addr, "127.0.0.1:") || !strings.Contains(res.Text, addr) {
		t.Fatalf("cdp start: %q (addr %q)", res.Text, addr)
	}
	st, err = runCmd(t, env, "cdp", "status")
	if err != nil || !strings.Contains(st.Text, "listening on "+addr) || !strings.Contains(st.Text, "allowed origins: http://a.test, http://b.test") || !strings.Contains(st.Text, "Endpoint records: 0") {
		t.Fatalf("cdp status: %q, %v", st.Text, err)
	}
	lg, err := runCmd(t, env, "cdp", "log")
	if err != nil || lg.Text != "No unknown CDP methods were received." {
		t.Fatalf("cdp log without unknown methods: %q, %v", lg.Text, err)
	}
	if _, err := runCmd(t, env, "cdp", "start", "--allow-origin", "http://c.test"); err == nil || !strings.Contains(err.Error(), "cdp stop") {
		t.Fatalf("cdp start with other origins while running: %v, want an error naming cdp stop", err)
	}
	stop, err := runCmd(t, env, "cdp", "stop")
	if err != nil || stop.Text != "Stopped the CDP endpoint: 0 endpoint record(s) revoked, 0 connection(s) closed." {
		t.Fatalf("cdp stop: %q, %v", stop.Text, err)
	}
}

// get cdp-url starts the endpoint on the configured port when it does not
// run (design 5.1); a busy port fails the command with the hint to pick
// another port, and no URL is printed.
func TestGetCDPURLReportsAFailedStart(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	env := cdpEnv{srv: cdp.New(cdp.Options{Port: l.Addr().(*net.TCPAddr).Port})}
	t.Cleanup(func() { env.srv.Stop() })
	res, err := runCmd(t, env, "get", "cdp-url")
	if err == nil || !strings.Contains(err.Error(), "--port") || res.Text != "" {
		t.Fatalf("get cdp-url with the configured port busy: %q, %v; want an error with the --port hint", res.Text, err)
	}
	if env.srv.Running() {
		t.Fatal("the endpoint runs after the failed start")
	}
}
