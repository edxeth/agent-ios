//go:build !darwin

package proxy

import (
	"fmt"
	"runtime"
)

// kernProcargs2 is darwin's reader of a process's arguments and exec
// environment (procargs_darwin.go); other systems have no KERN_PROCARGS2.
func kernProcargs2(pid int) ([]byte, error) {
	return nil, fmt.Errorf("sysctl KERN_PROCARGS2 of process %d: not available on %s", pid, runtime.GOOS)
}
