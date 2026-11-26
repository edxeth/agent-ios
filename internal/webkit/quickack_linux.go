package webkit

import (
	"net"

	"golang.org/x/sys/unix"
)

// quickAcker makes c acknowledge received data at once and returns the
// function that cutConn.Read calls after every read, or nil. Linux leaves
// quick-acknowledgement mode by itself, so TCP_QUICKACK is set again
// after every read. Not a TCP connection: nil.
func quickAcker(c net.Conn) func() {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return nil
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return nil
	}
	set := func(fd uintptr) { _ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1) }
	ack := func() { _ = rc.Control(set) }
	ack()
	return ack
}
