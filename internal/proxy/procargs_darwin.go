//go:build darwin

package proxy

import "golang.org/x/sys/unix"

// kernProcargs2 reads the KERN_PROCARGS2 buffer of process pid with
// sysctl: its arguments and its exec environment (procargsEnv). darwin
// returns it only for this user's processes, or to root.
func kernProcargs2(pid int) ([]byte, error) {
	return unix.SysctlRaw("kern.procargs2", pid)
}
