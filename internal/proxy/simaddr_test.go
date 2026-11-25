package proxy

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Item 6 of device check 4 (evidence: the iwdp-fd-research report): at
// start iwdp 1.9.2 connects a SIMULATOR inspector to localhost:27753.
// iwdp tries every address of localhost in turn: a refused 127.0.0.1 goes
// on to ::1, and where ::1 does not answer, that connect stays pending,
// and about 133 s later iwdp leaves a dead send queue that breaks one
// file-descriptor slot. "-s 127.0.0.1:27753" makes the connect fail at
// once. Linux runs no iOS Simulator, so every start of agent-ios passes it
// there; macOS keeps the default, which the simulator tests use.
func TestStartPassesTheSimulatorAddressOnLinuxOnly(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeServe, addr)
	if err := testStarter(addr, 10*time.Second).Start(context.Background()); err != nil {
		t.Fatalf("Start of the serve fake: %v", err)
	}
	starts := f.starts()
	if len(starts) != 1 {
		t.Fatalf("starts %q, want 1", starts)
	}
	got := strings.Contains(" "+starts[0]+" ", " -s 127.0.0.1:27753 ")
	if want := runtime.GOOS == "linux"; got != want {
		t.Fatalf("on %s the start ran ios_webkit_debug_proxy with %q; want -s 127.0.0.1:27753: %v", runtime.GOOS, starts[0], want)
	}
}

// The argument list on each system: Linux adds -s 127.0.0.1:27753, macOS
// does not.
func TestIWDPArgsPerSystem(t *testing.T) {
	const config = "null:9221,:9224-9229"
	for goos, want := range map[string]string{
		"linux":  "-F -c " + config + " -s 127.0.0.1:27753",
		"darwin": "-F -c " + config,
	} {
		if got := strings.Join(iwdpArgs(goos, config), " "); got != want {
			t.Errorf("iwdpArgs(%q) = %q, want %q", goos, got, want)
		}
	}
}

// wantManualIWDP is the iwdp command line that manual-start and recovery
// advice names on this system: on Linux it has -s 127.0.0.1:27753, as
// docs/known-limits.md tells a user who starts iwdp by hand; macOS keeps
// iwdp's default.
func wantManualIWDP() string {
	if runtime.GOOS == "linux" {
		return "ios_webkit_debug_proxy --no-frontend -s 127.0.0.1:27753"
	}
	return "ios_webkit_debug_proxy --no-frontend"
}

// The manual command line on each system: Linux adds -s 127.0.0.1:27753,
// macOS does not.
func TestManualCommandPerSystem(t *testing.T) {
	for goos, want := range map[string]string{
		"linux":  "ios_webkit_debug_proxy --no-frontend -s 127.0.0.1:27753",
		"darwin": "ios_webkit_debug_proxy --no-frontend",
	} {
		if got := manualCommand(goos); got != want {
			t.Errorf("manualCommand(%q) = %q, want %q", goos, got, want)
		}
	}
}

// Review round 2, F5: when Restart refuses another program's iwdp, its
// manual restart advice keeps the Linux simulator address; an iwdp started
// by that advice would otherwise get the broken slot again.
func TestRestartRefusalNamesTheManualCommandOfThisSystem(t *testing.T) {
	addr := freeAddr(t)
	f := installFakeIWDP(t, fakeFail, addr)
	foreign := foreignIWDP(t, addr)
	err := testStarter(addr, 2*time.Second).Restart(context.Background())
	checkUntouched(t, f, addr, foreign)
	if err == nil {
		t.Fatal("Restart succeeded against an iwdp that this program did not start")
	}
	if !strings.Contains(err.Error(), "then run '"+wantManualIWDP()+"'") {
		t.Fatalf("on %s the restart refusal %q does not name %q", runtime.GOOS, err, wantManualIWDP())
	}
}
