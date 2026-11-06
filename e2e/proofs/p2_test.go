//go:build webkitgtk

package proofs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/webkit"
)

const (
	p2TargetText = "p2 target text"
	p2Title      = "P2 proof page"
	// p2Session is the agent-browser session of the proof; it is closed at the end.
	p2Session = "p2proof"
	// p2ClientTimeout bounds one client run; each script has its own 90 s limit.
	p2ClientTimeout = 120 * time.Second
)

const p2Page = `<!doctype html><html><head><title>` + p2Title + `</title></head><body><h1>P2</h1><div id="target">` + p2TargetText + `</div><button id="b">Button</button></body></html>`

// TestP2CDPBridge runs proof P2 (design 7.1): pinned playwright-core 1.65.0
// and puppeteer-core 25.16.0 connect to the minimal bridge (p2bridge_test.go)
// in front of WebKitGTK, with the ws form and the http form of 5.1, and
// agent-browser 0.41.2 gets the exact tokenized ws URL. Set P2_LOG_DIR to
// keep the logs; the Node directory p2node needs "npm ci" first.
func TestP2CDPBridge(t *testing.T) {
	nodeDir, err := filepath.Abs("p2node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(nodeDir, "node_modules", "playwright-core")); err != nil {
		t.Fatalf("run \"npm ci\" in %s first: %v", nodeDir, err)
	}
	logDir := os.Getenv("P2_LOG_DIR")
	if logDir == "" {
		logDir = t.TempDir()
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bridgeLog, err := os.Create(filepath.Join(logDir, "bridge.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer bridgeLog.Close()
	var logMu sync.Mutex
	start := time.Now()
	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(bridgeLog, "%8.3fs %s\n", time.Since(start).Seconds(), fmt.Sprintf(format, args...))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(p2Page))
	})
	pageURL := serve(t, mux) + "/page"
	tr, page := browser(t, pageURL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	s, err := webkit.Dial(ctx, func(ctx context.Context) (transport.MessageConn, error) { return tr.Open(ctx, page) },
		webkit.SessionOptions{TargetWait: 12 * time.Second, CallTimeout: 60 * time.Second})
	if err != nil {
		t.Fatalf("connecting to the page: %v", err)
	}
	defer s.Close()
	// The listing appears before the document loads; the bridge reads the
	// frame once, so wait for the loaded fixture.
	loaded := pageURL + " complete"
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		v, err := evalString(ctx, s, "location.href + ' ' + document.readyState")
		if err == nil && v == loaded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture page did not load within 15 s: %q, %v", v, err)
		}
	}
	b, err := newP2Bridge(ctx, s, page.DeviceID, page.ID, logf)
	if err != nil {
		t.Fatalf("starting the bridge: %v", err)
	}
	defer b.Close()
	logf("bridge ws form: %s", b.redact(b.WSURL()))
	logf("bridge http form: %s", b.redact(b.HTTPURL()))

	node := func(t *testing.T, name, script, url string) {
		out := runLogged(t, b.redact, filepath.Join(logDir, name+".log"), nodeDir, p2ClientTimeout, "node", script, url, p2TargetText)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		var res map[string]any
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err != nil {
			t.Fatalf("%s: no JSON result line: %v\n%s", name, err, out)
		}
		t.Logf("%s: %s", name, lines[len(lines)-1])
		if res["ok"] != true {
			t.Errorf("%s failed: %v", name, res["error"])
		}
	}
	t.Run("playwright-ws", func(t *testing.T) { node(t, "playwright-ws", "playwright.mjs", b.WSURL()) })
	t.Run("playwright-http", func(t *testing.T) { node(t, "playwright-http", "playwright.mjs", b.HTTPURL()) })
	t.Run("puppeteer-ws", func(t *testing.T) { node(t, "puppeteer-ws", "puppeteer.mjs", b.WSURL()) })
	t.Run("puppeteer-http", func(t *testing.T) { node(t, "puppeteer-http", "puppeteer.mjs", b.HTTPURL()) })

	t.Run("agent-browser-ws", func(t *testing.T) {
		defer func() {
			runLogged(t, b.redact, filepath.Join(logDir, "agent-browser-close.log"), nodeDir, 60*time.Second, "agent-browser", "--session", p2Session, "close")
		}()
		before := len(b.Upgrades())
		out := runLogged(t, b.redact, filepath.Join(logDir, "agent-browser-ws.log"), nodeDir, p2ClientTimeout, "agent-browser", "--session", p2Session, "--cdp", b.WSURL(), "get", "title")
		t.Logf("agent-browser get title: %q", strings.TrimSpace(out))
		want := b.redact(strings.TrimPrefix(b.WSURL(), "ws://"+b.addr))
		got := b.Upgrades()[before:]
		found := false
		for _, u := range got {
			if _, path, _ := strings.Cut(u, " "); strings.HasPrefix(path, want+" ") {
				found = true
			}
		}
		if !found {
			t.Errorf("agent-browser did not open the tokenized WebSocket path %s; upgrades: %v; requests: %v", want, got, b.Requests())
		}
		if !strings.Contains(out, p2Title) {
			t.Errorf("agent-browser get title printed %q, want the page title %q", out, p2Title)
		}
	})

	for _, l := range b.Stats() {
		logf("stat: %s", l)
	}
	for _, u := range b.Upgrades() {
		logf("upgrade: %s", u)
	}
	t.Logf("bridge log: %s", bridgeLog.Name())
}

// runLogged runs a command, writes its combined output to path and returns
// it, both with the bridge token redacted. A failed start, a nonzero exit or
// a timeout fails the test after the log is written; callers that clean up
// (agent-browser close) still run on every path because they are deferred.
func runLogged(t *testing.T, redact func(string) string, path, dir string, timeout time.Duration, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	redacted := redact(buf.String())
	if werr := os.WriteFile(path, []byte(redact(fmt.Sprintf("$ %s %s\nexit: %v\n%s", name, strings.Join(args, " "), err, redacted))), 0o644); werr != nil {
		t.Errorf("writing %s: %v", path, werr)
	}
	if err != nil {
		t.Errorf("%s %s: %v (context: %v); output in %s", name, redact(strings.Join(args, " ")), err, ctx.Err(), path)
	}
	return redacted
}
