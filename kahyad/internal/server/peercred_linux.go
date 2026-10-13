//go:build linux

package server

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerPIDFromConn returns the pid of the process on the other end of a
// UDS connection (Linux: getsockopt(SOL_SOCKET, SO_PEERCRED)). It returns
// an error for any non-*net.UnixConn or a syscall failure - the caller
// (connContext) treats that as "unattributable", fail-closed.
func peerPIDFromConn(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("server: conn is not *net.UnixConn (%T)", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var ucred *unix.Ucred
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		ucred, opErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if opErr != nil {
		return 0, opErr
	}
	return int(ucred.Pid), nil
}
