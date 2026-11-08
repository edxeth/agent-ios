package cdp

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// Design 5.1 (cdp start, get cdp-url, daemon flag --cdp-port): StartDefault
// starts on the configured port and keeps a running listener; Start keeps
// a running listener on its own port and refuses another port or other
// allowed origins (naming cdp stop); a port outside 0..65535 is refused.
func TestStartKeepsOrRefusesARunningListener(t *testing.T) {
	port := freeLoopbackPort(t)
	s := New(Options{Port: port})
	t.Cleanup(func() { s.Stop() })
	want := "127.0.0.1:" + strconv.Itoa(port)
	if addr, err := s.StartDefault([]string{" http://a.test ", ""}); err != nil || addr != want {
		t.Fatalf("StartDefault = %q, %v; want %s (Options.Port)", addr, err, want)
	}
	if st := s.Status(); len(st.AllowOrigins) != 1 || st.AllowOrigins[0] != "http://a.test" {
		t.Fatalf("allowed origins %v, want [http://a.test]", st.AllowOrigins)
	}
	for _, tc := range []struct {
		port    int
		origins []string
	}{{0, nil}, {port, nil}, {port, []string{"http://a.test"}}} {
		if addr, err := s.Start(tc.port, tc.origins); err != nil || addr != want {
			t.Errorf("Start(%d, %v) while running = %q, %v; want the running %s", tc.port, tc.origins, addr, err, want)
		}
	}
	if addr, err := s.StartDefault(nil); err != nil || addr != want {
		t.Errorf("StartDefault while running = %q, %v; want %s", addr, err, want)
	}
	if _, err := s.Start(freeLoopbackPort(t), nil); err == nil || !strings.Contains(err.Error(), "cdp stop") {
		t.Errorf("Start on another port while running: %v, want an error naming cdp stop", err)
	}
	if _, err := s.Start(0, []string{"http://b.test"}); err == nil || !strings.Contains(err.Error(), "cdp stop") {
		t.Errorf("Start with other origins while running: %v, want an error naming cdp stop", err)
	}
	for _, bad := range []int{-1, 65536} {
		if _, err := s.Start(bad, nil); err == nil || !strings.Contains(err.Error(), "want 0 to 65535") {
			t.Errorf("Start(%d): %v, want a range error", bad, err)
		}
	}
	s.Stop()
	if s.Running() {
		t.Fatal("Running after Stop")
	}
	// A busy port is reported with the hint to pick another.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	busy := l.Addr().(*net.TCPAddr).Port
	if _, err := s.Start(busy, nil); err == nil || !strings.Contains(err.Error(), "--port") {
		t.Fatalf("Start on a busy port: %v, want an error with the --port hint", err)
	}
}
