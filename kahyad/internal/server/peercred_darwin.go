//go:build darwin

package server

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerPIDFromConn returns the pid of the process on the other end of a
// UDS connection (macOS: getsockopt(SOL_LOCAL, LOCAL_PEERPID)). It
// returns an error for any non-*net.UnixConn or a syscall failure - the
// caller (connContext) treats that as "unattributable", fail-closed.
// getpeereid(2) gives only uid/gid, never a pid, so it is not enough here.
func peerPIDFromConn(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("server: conn is not *net.UnixConn (%T)", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		pid, opErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, err
	}
	if opErr != nil {
		return 0, opErr
	}
	return pid, nil
}
