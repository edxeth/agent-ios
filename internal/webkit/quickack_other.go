//go:build !linux && !darwin

package webkit

import "net"

// quickAcker: only Linux and macOS have the socket options
// (quickack_linux.go, quickack_darwin.go).
func quickAcker(net.Conn) func() { return nil }
