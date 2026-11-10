//go:build cdpcompat && !linux

package cdpcompat

import "syscall"

// ownedProcAttr returns the attributes of each browser process that this
// suite starts (MiniBrowser, broadwayd): its own process group, which the
// cleanup kills. Only Linux can also end it with the test process
// (procattr_linux_test.go); WebKitGTK's MiniBrowser runs there.
func ownedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
