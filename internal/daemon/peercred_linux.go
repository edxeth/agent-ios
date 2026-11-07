package daemon

import (
	"fmt"
	"net"
	"syscall"
)

// verifyPeer checks with SO_PEERCRED that the process at the other end of
// conn runs as uid (design 2.2, R16): a socket of another user is never
// trusted, whatever its directory says.
func verifyPeer(conn net.Conn, sock string, uid int) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("%w: %s is not a Unix socket connection", ErrUnsafePeer, sock)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnsafePeer, sock, err)
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnsafePeer, sock, err)
	}
	if credErr != nil {
		return fmt.Errorf("%w: reading the peer of %s: %w", ErrUnsafePeer, sock, credErr)
	}
	if int(cred.Uid) != uid {
		return fmt.Errorf("%w: the process at %s (pid %d) runs as uid %d, not %d; remove the socket", ErrUnsafePeer, sock, cred.Pid, cred.Uid, uid)
	}
	return nil
}
