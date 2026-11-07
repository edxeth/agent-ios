//go:build !linux

package daemon

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// verifyPeer checks that the socket file belongs to uid (design 2.2, R16).
// Without SO_PEERCRED this is the owner of the socket that conn reached;
// the socket directory check already refuses a directory of another user.
func verifyPeer(_ net.Conn, sock string, uid int) error {
	fi, err := os.Lstat(sock)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnsafePeer, sock, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != uid || fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: %s is not a socket of uid %d; remove it", ErrUnsafePeer, sock, uid)
	}
	return nil
}
