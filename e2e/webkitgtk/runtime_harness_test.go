//go:build webkitgtk

// Package webkitgtk holds the local end-to-end suites that run against a
// WebKitGTK MiniBrowser under broadwayd (design docs/design/agentic-parity.md,
// section 8.2):
//
//	go test -tags=webkitgtk ./e2e/webkitgtk/ -v -count=1
//
// The runtime suite (W3a) shares one browser and one page connection:
// WebKitGTK refuses quick reconnects (spike A step B).
package webkitgtk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/commands"
	"github.com/nnemirovsky/iwdp-mcp/internal/core"
	"github.com/nnemirovsky/iwdp-mcp/internal/refs"
	iwdpruntime "github.com/nnemirovsky/iwdp-mcp/internal/runtime"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/webkitgtk"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

const miniBrowser = "/usr/lib/webkit2gtk-4.1/MiniBrowser"

var (
	origin string          // fixture server origin
	sess   *webkit.Session // the one page connection of the suite
)

func fixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "fixtures", "runtime")
}

func TestMain(m *testing.M) {
	cleanup, err := setupCLISuite()
	if err != nil {
		log.Printf("webkitgtk suite: %v", err)
		os.Exit(1)
	}
	code, err := runSuite(m)
	if err != nil {
		log.Printf("webkitgtk suite: %v", err)
		code = 1
	}
	cleanup()
	os.Exit(code)
}

// runSuite starts the fixture server, broadwayd and MiniBrowser, runs the
// tests and stops every process it started.
func runSuite(m *testing.M) (int, error) {
	if _, err := os.Stat(miniBrowser); err != nil {
		return 1, fmt.Errorf("WebKitGTK MiniBrowser not found at %s: %w", miniBrowser, err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 1, err
	}
	srv := &http.Server{Handler: http.FileServer(http.Dir(fixtureDir()))}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()
	origin = "http://" + l.Addr().String()

	var procs []*exec.Cmd
	defer func() {
		for _, c := range procs {
			_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			_ = c.Wait()
		}
	}()
	start := func(env []string, name string, args ...string) error {
		c := exec.Command(name, args...)
		c.Env = append(os.Environ(), env...)
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			return fmt.Errorf("starting %s: %w", name, err)
		}
		procs = append(procs, c)
		return nil
	}
	shimDir, err := os.MkdirTemp("", "iwdp-w3a-shim-")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(shimDir)
	shim, err := buildDPIShim(shimDir)
	if err != nil {
		return 1, err
	}
	display := fmt.Sprintf(":%d", 100+time.Now().Nanosecond()%50)
	if err := start(nil, "broadwayd", display); err != nil {
		return 1, err
	}
	time.Sleep(500 * time.Millisecond)
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 1, err
	}
	addr := pl.Addr().String()
	pl.Close()
	if err := start([]string{"GDK_BACKEND=broadway", "BROADWAY_DISPLAY=" + display, "WEBKIT_INSPECTOR_HTTP_SERVER=" + addr, "LD_PRELOAD=" + shim}, miniBrowser, origin+"/forms.html"); err != nil {
		return 1, err
	}
	tr := webkitgtk.New(addr)
	var page transport.Page
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		pages, err := tr.Pages(ctx, "webkitgtk-"+addr)
		cancel()
		if err == nil && len(pages) > 0 {
			page = pages[0]
			break
		}
	}
	if page.ID == "" {
		return 1, fmt.Errorf("MiniBrowser did not list a page at %s within 15 s", addr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err = webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) { return tr.Open(ctx, page) },
		webkit.SessionOptions{TargetWait: 12 * time.Second})
	if err != nil {
		return 1, fmt.Errorf("connecting to the page: %w", err)
	}
	defer sess.Close()
	// Page events give the session its document generations (design 2.7).
	if _, err := sess.Send(ctx, "Page.enable", nil); err != nil {
		return 1, fmt.Errorf("Page.enable: %w", err)
	}
	return m.Run(), nil
}

// dpiShim fixes the page geometry of WebKitGTK 2.54 under broadway.
// WebKit sets the page zoom to the font DPI / 96, and under GTK 3 it reads
// the DPI from gdk_screen_get_resolution without a check
// (SystemSettingsManagerProxyGtk.cpp, WebKitWebViewBase.cpp
// refreshInternalScaling). The broadway backend never sets a resolution,
// so it stays -1: devicePixelRatio is -1/96, innerWidth is -98304, every
// box is empty and elementFromPoint finds nothing. GTK copies
// gtk-xft-dpi into the resolution only when the application sets it, so
// GDK_DPI_SCALE and settings.ini do not help; preloading this function
// does. screen.width stays 0 (the broadway monitor has no size); no test
// reads it.
const dpiShim = "#include <gdk/gdk.h>\ngdouble gdk_screen_get_resolution(GdkScreen *s) { (void)s; return 96.0; }\n"

// buildDPIShim compiles dpiShim into dir and returns the library path.
func buildDPIShim(dir string) (string, error) {
	src, lib := filepath.Join(dir, "dpi.c"), filepath.Join(dir, "libdpi.so")
	if err := os.WriteFile(src, []byte(dpiShim), 0o600); err != nil {
		return "", err
	}
	flags, err := exec.Command("pkg-config", "--cflags", "gdk-3.0").Output()
	if err != nil {
		return "", fmt.Errorf("pkg-config --cflags gdk-3.0 (needed for the WebKitGTK DPI shim): %w", err)
	}
	args := append([]string{"-shared", "-fPIC", "-o", lib, src}, strings.Fields(string(flags))...)
	if out, err := exec.Command("gcc", args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("compiling the WebKitGTK DPI shim with gcc: %v: %s", err, out)
	}
	return lib, nil
}

// eval evaluates expr in the page and returns its value as JSON.
func eval(t *testing.T, expr string) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := sess.Send(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true})
	if err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
	var r struct {
		Result struct {
			Value       json.RawMessage `json:"value"`
			Description string          `json:"description"`
		} `json:"result"`
		WasThrown bool `json:"wasThrown"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.WasThrown {
		t.Fatalf("evaluate %q: %s (%v)", expr, raw, err)
	}
	return r.Result.Value
}

// navigate loads a fixture page and waits until it is complete; wait is an
// extra condition (JavaScript) to wait for. It also waits for the session's
// document generation to change: when the page already shows url, the
// readiness condition is true in the old document before the new one
// commits, and a later frameNavigated would make the refs of a snapshot
// taken in between stale.
func navigate(t *testing.T, path, wait string) {
	t.Helper()
	url := origin + "/" + path
	before := sess.Generation()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, _ = sess.Send(ctx, "Runtime.evaluate", map[string]any{"expression": fmt.Sprintf("location.href = %q", url)})
	cancel()
	cond := fmt.Sprintf("location.href === %q && document.readyState === 'complete'", url)
	if wait != "" {
		cond += " && (" + wait + ")"
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		raw, err := sess.Send(ctx, "Runtime.evaluate", map[string]any{"expression": cond, "returnByValue": true})
		cancel()
		if err == nil && strings.Contains(string(raw), `"value":true`) && sess.Generation() != before {
			return
		}
	}
	t.Fatalf("page %s did not load within 10 s", url)
}

// pageSession is what the daemon would hand a command for one logical
// session: the shared connection, a runtime with the session's owner and
// the session's ref table.
func pageSession(daemon, session string, epoch uint64) *core.PageSession {
	rt := iwdpruntime.New(sess, sess.Generation)
	rt.SetOwner(iwdpruntime.Owner{Daemon: daemon, Session: session, Epoch: epoch})
	return &core.PageSession{Caller: sess, Runtime: rt, Refs: refs.NewTable()}
}

type env struct{ ps *core.PageSession }

func (e env) Page(context.Context) (*core.PageSession, error) { return e.ps, nil }
func (env) Emit(string)                                       {}
func (env) ResolvePath(p string) string                       { return p }

// run runs a registry command line against ps and returns its exit code,
// stdout and stderr.
func run(t *testing.T, ps *core.PageSession, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := commands.RunCLI(context.Background(), append([]string{"--timeout", "20s"}, args...), &out, &errb,
		func(context.Context, *commands.Invocation) (commands.Env, error) { return env{ps}, nil })
	return code, strings.TrimRight(out.String(), "\n"), errb.String()
}

// mustRun runs a command that has to succeed.
func mustRun(t *testing.T, ps *core.PageSession, args ...string) string {
	t.Helper()
	code, out, errOut := run(t, ps, args...)
	if code != 0 {
		t.Fatalf("%s: exit %d: %s", strings.Join(args, " "), code, errOut)
	}
	return out
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(strings.ReplaceAll(string(data), "{{ORIGIN}}", origin), "\n")
}
