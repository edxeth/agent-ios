//go:build webkitgtk

package webkitgtk

import "syscall"

// ownedProcAttr returns the attributes of each process that this harness
// starts (MiniBrowser, broadwayd): its own process group, which the
// cleanup kills together with the processes that MiniBrowser started, and
// SIGKILL when the test process ends without its cleanup, at a test
// timeout or a kill (Pdeathsig, PR_SET_PDEATHSIG). Without it two
// MiniBrowser processes with their children once ran for hours after
// their test runs. WebKit's web and network processes end when
// MiniBrowser ends (TestBrowserEndsWithItsTestProcess). The kernel sends
// the signal when the thread that started the process ends; the Go
// runtime ends a thread only when a goroutine locked to it
// (runtime.LockOSThread) returns, which no starter here does.
//
// Known limit: a MiniBrowser that ends through Pdeathsig, because the test
// process itself died (a go test timeout, a kill), leaves the GTK
// Broadway buffers that broadwayd never mapped in /dev/shm until a reboot,
// 3 MiB each: no cleanup of the test process runs then. Every other end
// of a MiniBrowser removes them (broadwayshm). The recorder log of such a
// browser stays in the test's temporary directory (bdwshm-*.log); remove
// leftovers only through it (broadwayshm.End), never by
// pattern: other runs' browsers hold buffers of the same form.
func ownedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
