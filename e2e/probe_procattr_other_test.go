//go:build !linux

package e2e_test

import "syscall"

// probeProcAttr returns the attributes of each owner probe. Only Linux
// can end a child with its parent (PR_SET_PDEATHSIG); elsewhere the
// probes end only through the test's cleanup.
func probeProcAttr() *syscall.SysProcAttr { return nil }
