//go:build unix && !linux

package broadwayshm

import "fmt"

// RemoveIfSame needs renameat2, which only Linux has; GTK's Broadway
// buffers in /dev/shm are a Linux matter, and the harness records none
// elsewhere.
func RemoveIfSame(c Created) error {
	return fmt.Errorf("removing %s: only Linux is supported", c.Path)
}
