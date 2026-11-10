//go:build cdpcompat

package cdpcompat

import "syscall"

// ownedProcAttr returns the attributes of each browser process that this
// suite starts (MiniBrowser, broadwayd): its own process group, which the
// cleanup kills together with the processes that MiniBrowser started, and
// SIGKILL when the test process ends without its cleanup, at a test
// timeout or a kill (Pdeathsig, PR_SET_PDEATHSIG). WebKit's web and
// network processes end when MiniBrowser ends
// (TestBrowserEndsWithItsTestProcess). The kernel sends the signal when
// the thread that started the process ends; the Go runtime ends a thread
// only when a goroutine locked to it (runtime.LockOSThread) returns, which
// no starter here does.
func ownedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
