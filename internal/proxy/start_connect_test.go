package proxy

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Restart with an iPhone X attached, 2026-11-26: ios_webkit_debug_proxy
// 1.9.2 listens on its listing port first and answers only after it has
// connected the USB devices, which it does inside its one event loop. The
// start waited 5 s for an answer, then stopped the iwdp in the middle of
// that connection, and the next starts failed the same way, about 75 s
// without any iwdp. These tests use fakes that listen at once and answer
// late or never.

// connectStarter is a Starter for addr with the production readiness wait
// (startReadyWait: the fake must listen within it on a loaded host) and
// the connection wait connect.
func connectStarter(addr string, connect time.Duration) *Starter {
	s := testStarter(addr, startReadyWait)
	s.connectWait = connect
	return s
}

// A started iwdp that listens but answers only after the readiness wait,
// as one that connects a USB device, is waited for and recorded: Start
// succeeds and runs it once.
func TestStartWaitsForAListeningIWDPThatConnectsADevice(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeConnecting, addr)
	s := connectStarter(addr, 20*time.Second)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start of an iwdp that answers %s after its start: %v", fakeConnectingAnswer, err)
	}
	if got := f.starts(); len(got) != 1 {
		t.Fatalf("ios_webkit_debug_proxy ran %d times (%q), want 1", len(got), got)
	}
	if err := CheckListing(context.Background(), addr); err != nil {
		t.Fatalf("the started iwdp does not answer: %v", err)
	}
	rec, err := s.loadRecord()
	if err != nil || rec == nil || rec.PID != f.pids()[0] {
		t.Fatalf("record after the start = %+v, %v; want the started pid %d", rec, err, f.pids()[0])
	}
}

// keptAndRecorded fails t unless the one started fake runs on, its record
// names it, and its group owns the listener of addr, so that restart-iwdp
// can stop it.
func keptAndRecorded(t *testing.T, s *Starter, f *fakeIWDP, addr string) {
	t.Helper()
	pids := f.pids()
	if len(pids) != 1 {
		t.Fatalf("started pids %v, want one", pids)
	}
	if !alive(pids[0]) {
		t.Fatalf("the started iwdp %d was stopped, although it listened", pids[0])
	}
	rec, err := s.loadRecord()
	if err != nil || rec == nil || rec.PID != pids[0] {
		t.Fatalf("record = %+v, %v; want the kept pid %d", rec, err, pids[0])
	}
	if err := s.owns(context.Background(), rec, netip.MustParseAddrPort(addr)); err != nil {
		t.Fatalf("the kept iwdp does not pass restart's ownership check: %v", err)
	}
}

// A started iwdp that listens but does not answer within the connection
// wait is kept running and recorded, not stopped: stopping it in the
// middle of a device connection only makes the next start connect again.
// The error says so, and it starts no back-off.
func TestStartKeepsAListeningIWDPThatDoesNotAnswerInTime(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeStuck, addr)
	s := connectStarter(addr, 7*time.Second)
	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("Start of an iwdp that never answers succeeded")
	}
	for _, want := range []string{"listens on " + addr, "kept running", "restart-iwdp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "%!") {
		t.Errorf("error %q has a formatting fault", err)
	}
	keptAndRecorded(t, s, f, addr)
	if err := s.backoff(); err != nil {
		t.Errorf("a kept iwdp started the back-off: %v", err)
	}
}

// A caller that gives up while its started iwdp listens and connects a
// device gets its context error, and the iwdp is kept and recorded.
func TestStartKeepsAListeningIWDPWhenTheCallerGivesUp(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeStuck, addr)
	s := connectStarter(addr, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- s.Start(ctx) }()
	// The fake logs a request only when the start's readiness check found
	// its listener and asked for the listing.
	waitFile(t, filepath.Join(f.dir, "requests.log"))
	cancel()
	var err error
	select {
	case err = <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return after its context ended")
	}
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "kept running") || strings.Contains(err.Error(), "%!") {
		t.Fatalf("Start = %v, want context.Canceled and that the iwdp was kept running", err)
	}
	keptAndRecorded(t, s, f, addr)
}

// The advice of a start that did not answer comes from what the started
// iwdp logged while it ran: the "Unable to connect to ..." lines that
// iwdp prints when the start's SIGTERM ends it are its own shutdown, not
// a device that refused the inspector connection.
func TestStartAdviceIgnoresTheLinesOfTheStop(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeShutdownLines, addr)
	err := testStarter(addr, time.Second).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("Start = %v, want a readiness error", err)
	}
	path, perr := LogPath()
	if perr != nil {
		t.Fatal(perr)
	}
	if log := waitLog(t, path, inspectorRefused); !strings.Contains(log, inspectorRefused) {
		t.Fatalf("the fake did not log its shutdown lines (log %q); the test proves nothing", log)
	}
	if strings.Contains(err.Error(), "did not accept the Web Inspector connection") {
		t.Errorf("error %q blames the device for the lines of the stop", err)
	}
	for _, pid := range f.pids() {
		if alive(pid) {
			t.Errorf("process %d of the failed start is still running", pid)
		}
	}
	_ = os.Remove(path)
}
