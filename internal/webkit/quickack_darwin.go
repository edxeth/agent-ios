package webkit

import (
	"net"

	"golang.org/x/sys/unix"
)

// quickAcker makes c acknowledge received data at once (quickack_linux.go
// explains why). On macOS, TCP_SENDMOREACKS (tcp(4)) stays set for the
// connection's life, so nothing runs after a read: it returns nil.
// Not measured on macOS (FIX-R measured Linux).
func quickAcker(c net.Conn) func() {
	if tc, ok := c.(*net.TCPConn); ok {
		if rc, err := tc.SyscallConn(); err == nil {
			_ = rc.Control(func(fd uintptr) { _ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_SENDMOREACKS, 1) })
		}
	}
	return nil
}
