package e2e_test

import "syscall"

// probeProcAttr returns the attributes of each owner probe
// (startOwnerProbe): SIGKILL when the test process ends without its
// cleanup, at a test timeout or a kill (Pdeathsig, PR_SET_PDEATHSIG).
// Without it, 96 probes of killed runs of
// TestSimTeardownSignalsOnlyAVerifiedIWDPProperty, a third of them
// ignoring SIGTERM, ran for a day and held 812 MiB (2026-11-12). The
// kernel sends the signal when the thread that started the process ends;
// the Go runtime ends a thread only when a goroutine locked to it
// (runtime.LockOSThread) returns, which startOwnerProbe's callers never
// do.
func probeProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
