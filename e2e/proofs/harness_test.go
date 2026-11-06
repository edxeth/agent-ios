//go:build webkitgtk

// Package proofs holds the W1a contract proofs P1 and P2 (design
// docs/design/agentic-parity.md, section 7.1). They run against a local
// WebKitGTK MiniBrowser under broadwayd:
//
//	go test -tags=webkitgtk ./e2e/proofs/ -v -count=1
//
// W5 deletes this directory when its own suite covers the same scenarios.
package proofs

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/nnemirovsky/iwdp-mcp/internal/transport"
	"github.com/nnemirovsky/iwdp-mcp/internal/transport/webkitgtk"
)

const miniBrowser = "/usr/lib/webkit2gtk-4.1/MiniBrowser"

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// serve starts an HTTP server on a free local port and returns its origin.
func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + l.Addr().String()
}

// startProcess starts a command in its own process group and kills the
// group at cleanup, so no MiniBrowser or broadwayd survives the test.
func startProcess(t *testing.T, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
}

// browser starts broadwayd and MiniBrowser on startURL and returns the
// webkitgtk transport and the page.
func browser(t *testing.T, startURL string) (*webkitgtk.Transport, transport.Page) {
	t.Helper()
	if _, err := os.Stat(miniBrowser); err != nil {
		t.Fatalf("WebKitGTK MiniBrowser not found at %s: %v", miniBrowser, err)
	}
	display := fmt.Sprintf(":%d", 40+time.Now().Nanosecond()%50)
	startProcess(t, nil, "broadwayd", display)
	time.Sleep(500 * time.Millisecond)
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	startProcess(t, []string{"GDK_BACKEND=broadway", "BROADWAY_DISPLAY=" + display, "WEBKIT_INSPECTOR_HTTP_SERVER=" + addr}, miniBrowser, startURL)
	tr := webkitgtk.New(addr)
	dev := "webkitgtk-" + addr
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		pages, err := tr.Pages(ctx, dev)
		cancel()
		if err == nil && len(pages) > 0 {
			return tr, pages[0]
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("MiniBrowser did not list a page at %s within 15 s", addr)
	return nil, transport.Page{}
}
