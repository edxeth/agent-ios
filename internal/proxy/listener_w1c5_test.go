package proxy

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// L02: the owner lookup names the processes that listen on a port: this
// system's (listenerPIDs) and darwin's lsof implementation, which runs on
// Linux too. Linux's /proc implementation is tested in
// listener_linux_w1c5_test.go.
func TestListenerPIDsNamesTheOwner(t *testing.T) {
	addr := freeAddr(t)
	child := startServer(t, addr)
	got, err := lsofListenerPIDs(context.Background(), uint16(portOf(t, addr)))
	if err != nil || !slices.Equal(got, []int{child}) {
		t.Errorf("lsofListenerPIDs(%s) = %v, %v; want [%d]", addr, got, err, child)
	}
	got, err = lsofListenerPIDs(context.Background(), uint16(portOf(t, freeAddr(t))))
	if err != nil || len(got) != 0 {
		t.Errorf("lsofListenerPIDs on a free port = %v, %v; want none", got, err)
	}
	got, err = listenerPIDs(context.Background(), uint16(portOf(t, addr)))
	if err != nil || !slices.Equal(got, []int{child}) {
		t.Errorf("listenerPIDs(%s) = %v, %v; want [%d]", addr, got, err, child)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := lsofListenerPIDs(ctx, uint16(portOf(t, addr))); err == nil {
		t.Errorf("lsofListenerPIDs with an ended context = %v, want its error", got)
	}
	if got, err := lsofGroupListens(ctx, child, endpoint(t, addr)); err == nil {
		t.Errorf("lsofGroupListens with an ended context = %v, want its error", got)
	}
}

// lsof -t prints one pid per line; anything else is an error.
func TestLsofPIDsParsesTheTerseOutput(t *testing.T) {
	got, err := lsofPIDs([]byte("31\n7\n"))
	if err != nil || !slices.Equal(got, []int{7, 31}) {
		t.Errorf("lsofPIDs = %v, %v; want [7 31]", got, err)
	}
	if got, err := lsofPIDs([]byte("7\nx\n")); err == nil {
		t.Errorf("lsofPIDs of a bad line = %v, want an error", got)
	}
}

// F5: lsof output that cannot be parsed fails the ownership check (a fake
// lsof first on PATH prints an address without a port).
func TestLsofGroupListensRejectsBadOutput(t *testing.T) {
	addr := freeAddr(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lsof"), []byte("#!/bin/sh\nprintf 'p1\\ng1\\ntIPv4\\nnno-port\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := lsofGroupListens(context.Background(), 1, endpoint(t, addr))
	if err == nil || !strings.Contains(err.Error(), "lsof for the listener") {
		t.Fatalf("lsofGroupListens with bad lsof output = %v, %v; want a parse error", got, err)
	}
}
