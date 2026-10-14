//go:build darwin

package server

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerPIDFromConn returns the pid of the process on the other end of a
// UDS connection (macOS: getsockopt(SOL_LOCAL, LOCAL_PEERPID)). It
// returns an error for any non-*net.UnixConn or a syscall failure (e.g.
// ENOTCONN once the peer is gone) - the caller (connContext) then leaves
// the connection unresolved, which withControlAuth refuses (fail-closed).
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

// peerSessionID returns the session id of process pid (getsid(2)) - the
// key UDS caller attribution maps to a live worker (G03 / B-14: spawn.Run
// makes every worker a session leader, and unlike a process group a
// session cannot be joined from outside it). An error (the peer already
// exited, ESRCH) leaves the caller unresolved, which controlauth.go
// refuses on every non-open route.
func peerSessionID(pid int) (int, error) {
	return unix.Getsid(pid)
}
