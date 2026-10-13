package server

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestPeerCredReturnsCallerPID (G03 / B-14) proves the platform
// getsockopt constants are right on THIS kernel (macOS LOCAL_PEERPID /
// Linux SO_PEERCRED): the server side of a UDS connection this very
// process opened reports this process's own pid, and connContext records
// the matching process group. No build tag - runs on both platforms.
func TestPeerCredReturnsCallerPID(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()

	client := dialUnixForTest(t, sock)
	defer client.Close()

	var serverConn net.Conn
	select {
	case c, ok := <-accepted:
		if !ok {
			t.Fatalf("accept failed")
		}
		serverConn = c
	case <-time.After(5 * time.Second):
		t.Fatalf("accept timed out")
	}
	defer serverConn.Close()

	pid, err := peerPIDFromConn(serverConn)
	if err != nil {
		t.Fatalf("peerPIDFromConn: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("peerPIDFromConn = %d, want this process's pid %d", pid, os.Getpid())
	}

	srv := New(testConfig(sock), testLogger(t), "v-peercred", healthyDB)
	info, _ := srv.connContext(context.Background(), serverConn).Value(peerKey{}).(peerInfo)
	if info.PID != os.Getpid() || info.PGID != syscall.Getpgrp() {
		t.Fatalf("connContext recorded %+v, want pid %d pgid %d", info, os.Getpid(), syscall.Getpgrp())
	}

	// A non-UDS conn is unattributable, never a guessed pid.
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, err := peerPIDFromConn(a); err == nil {
		t.Fatalf("peerPIDFromConn(net.Pipe) succeeded, want an error (not a *net.UnixConn)")
	}
	info, _ = srv.connContext(context.Background(), a).Value(peerKey{}).(peerInfo)
	if info.PID != 0 || info.PGID != 0 {
		t.Fatalf("connContext(net.Pipe) recorded %+v, want zero (unattributable)", info)
	}
}
