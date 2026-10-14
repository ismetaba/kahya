package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/task"
)

// testProcessSID is this test process's own session id - what a test
// that plays a live worker from inside this process registers it under
// (spawn.Run registers a real worker with sid == pid; the test process is
// not a session leader, so it registers its actual session).
func testProcessSID(t *testing.T) int {
	t.Helper()
	sid, err := unix.Getsid(0)
	if err != nil || sid <= 0 {
		t.Fatalf("Getsid(0) = %d, %v", sid, err)
	}
	return sid
}

// acceptOne accepts the next connection on ln, failing the test after 5s.
func acceptOne(t *testing.T, ln net.Listener) net.Conn {
	t.Helper()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	select {
	case c, ok := <-accepted:
		if !ok {
			t.Fatalf("accept failed")
		}
		return c
	case <-time.After(5 * time.Second):
		t.Fatalf("accept timed out")
	}
	return nil
}

// TestPeerCredReturnsCallerPID (G03 / B-14) proves the platform
// getsockopt constants are right on THIS kernel (macOS LOCAL_PEERPID /
// Linux SO_PEERCRED): the server side of a UDS connection this very
// process opened reports this process's own pid, and connContext records
// the matching session id (the attribution key) and process group. No
// build tag - runs on both platforms.
func TestPeerCredReturnsCallerPID(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	client := dialUnixForTest(t, sock)
	defer client.Close()
	serverConn := acceptOne(t, ln)
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
	if info.PID != os.Getpid() || info.PGID != syscall.Getpgrp() || info.SID != testProcessSID(t) {
		t.Fatalf("connContext recorded %+v, want pid %d pgid %d sid %d", info, os.Getpid(), syscall.Getpgrp(), testProcessSID(t))
	}

	// A non-UDS conn is unresolved, never a guessed pid or session.
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, err := peerPIDFromConn(a); err == nil {
		t.Fatalf("peerPIDFromConn(net.Pipe) succeeded, want an error (not a *net.UnixConn)")
	}
	info, _ = srv.connContext(context.Background(), a).Value(peerKey{}).(peerInfo)
	if info != (peerInfo{}) {
		t.Fatalf("connContext(net.Pipe) recorded %+v, want zero (unresolved)", info)
	}
}

// TestConnContextUnresolvedWhenPeerExitedBeforeAccept (G03 / B-14, robust
// review): a process that connects, forks a child that keeps the
// connection's fd, and exits before kahyad accepts must NOT come out as a
// resolved peer in somebody's session - the pid the kernel reports for
// the connection is dead, so its session is unknowable. connContext
// leaves it unresolved (SID 0), which withControlAuth refuses
// (TestUnresolvedPeerRefusedEvenWithSecret).
func TestConnContextUnresolvedWhenPeerExitedBeforeAccept(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "g.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	script := strings.Join([]string{
		"import os, socket, sys, time",
		"s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)",
		"s.connect(sys.argv[1])",
		"if os.fork() == 0:",
		"    time.sleep(30)",
		"    os._exit(0)",
		"os._exit(0)",
	}, "\n")
	cmd := exec.Command("python3", "-c", script, sock)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start connector: %v", err)
	}
	connector := cmd.Process.Pid
	defer func() { _ = syscall.Kill(-connector, syscall.SIGKILL) }() // the fd-holding child
	if err := cmd.Wait(); err != nil {
		t.Fatalf("connector: %v", err)
	}

	serverConn := acceptOne(t, ln) // only now - the connector is gone
	defer serverConn.Close()
	srv := New(testConfig(sock), testLogger(t), "v-peercred-gone", healthyDB)
	info, _ := srv.connContext(context.Background(), serverConn).Value(peerKey{}).(peerInfo)
	t.Logf("connector pid %d; recorded peer info %+v", connector, info)
	if info.SID != 0 {
		t.Fatalf("connContext for a peer that exited before accept = %+v, want SID 0 (unresolved)", info)
	}
}

// TestUnresolvedPeerRefusedEvenWithSecret (G03 / B-14, robust review): a
// request whose connection has no resolved peer session is refused on
// worker AND control routes even with the correct control secret
// (reason unresolved_peer) - kahyad cannot rule out a live worker there.
// /health stays open; a resolved, unattributed peer with the secret is
// served (positive control).
func TestUnresolvedPeerRefusedEvenWithSecret(t *testing.T) {
	led := &fakeEventLogger{}
	srv := New(testConfig(filepath.Join(shortSocketDir(t), "u.sock")), testLogger(t), "v-unresolved", healthyDB)
	srv.SetEventLogger(led)
	srv.SetTaskLiveRegistry(task.NewLiveRegistry())
	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	srv.controlSecret = secret

	served := 0
	h := srv.withTraceLogging(srv.withControlAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	})))
	do := func(path string, peer *peerInfo) int {
		req := httptest.NewRequest(http.MethodPost, "http://kahyad"+path, strings.NewReader("{}"))
		req.Header.Set(config.ControlSecretHeader, secret)
		if peer != nil {
			req = req.WithContext(context.WithValue(req.Context(), peerKey{}, *peer))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden && !strings.Contains(rec.Body.String(), MsgControlSecretRejected) {
			t.Errorf("%s: 403 body %q, want MsgControlSecretRejected", path, rec.Body.String())
		}
		return rec.Code
	}

	for _, peer := range []*peerInfo{nil, {}, {PID: 4242}, {PID: 4242, PGID: 4242}} {
		for _, path := range []string{"/policy/promote", "/policy/state", "/policy/check", "/v1/mcp"} {
			if code := do(path, peer); code != http.StatusForbidden {
				t.Errorf("%s with secret, peer %+v = %d, want 403 (unresolved peer)", path, peer, code)
			}
		}
	}
	if served != 0 {
		t.Fatalf("handler ran %d times for unresolved peers, want 0", served)
	}
	rejected := led.eventsOfKind("control_auth_rejected")
	if len(rejected) != 16 {
		t.Fatalf("control_auth_rejected events = %d, want 16", len(rejected))
	}
	for _, e := range rejected {
		if e.payload["reason"] != controlRejectUnresolvedPeer {
			t.Fatalf("control_auth_rejected reason = %v, want %s", e.payload["reason"], controlRejectUnresolvedPeer)
		}
	}

	if code := do("/health", nil); code != http.StatusOK {
		t.Errorf("/health with no peer info = %d, want 200 (open route)", code)
	}
	resolved := &peerInfo{PID: os.Getpid(), PGID: syscall.Getpgrp(), SID: testProcessSID(t)}
	if code := do("/policy/promote", resolved); code != http.StatusOK {
		t.Errorf("/policy/promote from a resolved, unattributed peer with the secret = %d, want 200", code)
	}
	if served != 2 {
		t.Fatalf("handler ran %d times, want 2 (/health + resolved peer)", served)
	}
}
