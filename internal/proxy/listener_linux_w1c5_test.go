//go:build linux

package proxy

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// L02: Linux's /proc owner lookup names the processes that listen on a
// port.
func TestProcListenerPIDsNamesTheOwner(t *testing.T) {
	addr := freeAddr(t)
	child := startServer(t, addr)
	got, err := procListenerPIDs(uint16(portOf(t, addr)))
	if err != nil || !slices.Equal(got, []int{child}) {
		t.Errorf("procListenerPIDs(%s) = %v, %v; want [%d]", addr, got, err, child)
	}
	got, err = procListenerPIDs(uint16(portOf(t, freeAddr(t))))
	if err != nil || len(got) != 0 {
		t.Errorf("procListenerPIDs on a free port = %v, %v; want none", got, err)
	}
}

// The socket table is read once for both /proc lookups; a table that
// cannot be read fails both.
func TestProcLookupsFailWithoutTheSocketTable(t *testing.T) {
	addr := freeAddr(t)
	child := startServer(t, addr)
	old := socketTables
	t.Cleanup(func() { socketTables = old })
	socketTables.tcp = filepath.Join(t.TempDir(), "missing")
	if got, err := procGroupListens(context.Background(), child, endpoint(t, addr)); err == nil {
		t.Errorf("procGroupListens without the socket table = %v, want an error", got)
	}
	if got, err := procListenerPIDs(uint16(portOf(t, addr))); err == nil {
		t.Errorf("procListenerPIDs without the socket table = %v, want an error", got)
	}
}

// F5: a process of the group whose fds cannot be read (init, group 1, is
// not this user's) fails the /proc ownership check instead of counting as
// foreign or own. Nothing is signaled; /proc is only read.
func TestProcGroupListensFailsOnUnreadableFds(t *testing.T) {
	addr := freeAddr(t)
	serveListing(t, addr)
	got, err := procGroupListens(context.Background(), 1, endpoint(t, addr))
	if err == nil || !strings.Contains(err.Error(), "/fd") {
		t.Fatalf("procGroupListens(group 1) = %v, %v; want an error reading its fds", got, err)
	}
}

// F5: procListenerPIDs fails when the process directory cannot be read.
func TestProcListenerPIDsFailsWithoutProc(t *testing.T) {
	addr := freeAddr(t)
	serveListing(t, addr)
	old := procFS
	t.Cleanup(func() { procFS = old })
	procFS = filepath.Join(t.TempDir(), "missing")
	if got, err := procListenerPIDs(uint16(portOf(t, addr))); err == nil {
		t.Errorf("procListenerPIDs without %s = %v, want an error", procFS, got)
	}
}
