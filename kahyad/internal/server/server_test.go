package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
)

// shortSocketDir returns a short-path temp dir suitable for unix sockets.
// macOS unix socket paths are capped around ~104 bytes; t.TempDir() nests
// deep enough (e.g. /private/var/folders/.../TestName/001/002) to blow
// past that, so tests bind sockets under a short os.MkdirTemp dir instead
// and clean it up themselves.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "k")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func testLogger(t *testing.T) *logx.Logger {
	t.Helper()
	l, err := logx.New(t.TempDir(), "test0000000000000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func testConfig(socketPath string) config.Config {
	return config.Config{Socket: socketPath}
}

// fakeDB is a minimal DBHealth stand-in so server tests don't need a real
// brain.db (and don't pull the sqlite/cgo driver into this package's tests).
type fakeDB struct {
	ok      bool
	version int64
	err     error
}

func (f fakeDB) Health(context.Context) (bool, int64, error) { return f.ok, f.version, f.err }

var healthyDB = fakeDB{ok: true, version: 1}

// unixHTTPClient returns an http.Client that dials socketPath for every
// request, matching how the real kahya CLI talks to kahyad.
func unixHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

func TestHealthEndpoint(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test-123", healthyDB)

	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()

	client := unixHTTPClient(socketPath)
	resp, err := client.Get("http://kahyad/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v, want ok", body["status"])
	}
	if body["version"] != "v-test-123" {
		t.Errorf("version field = %v, want v-test-123", body["version"])
	}
	if _, ok := body["pid"]; !ok {
		t.Error("pid field missing")
	}
	if _, ok := body["uptime_s"]; !ok {
		t.Error("uptime_s field missing")
	}
	if body["db"] != "ok" {
		t.Errorf("db field = %v, want ok", body["db"])
	}
	if body["schema_version"] != float64(1) {
		t.Errorf("schema_version field = %v, want 1", body["schema_version"])
	}

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket perm = %o, want 600", perm)
	}

	if err := srv.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after Shutdown: err=%v", err)
	}
}

// TestHealthEndpointReportsDBError guards the fail-closed reporting rule in
// handleHealth: a failing DB ping must surface as "db":"error", never as
// "ok" (HANDOFF §4/§5 fail-closed posture applied to health reporting).
func TestHealthEndpointReportsDBError(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	unhealthyDB := fakeDB{ok: false, version: 1, err: errors.New("ping failed")}
	srv := New(testConfig(socketPath), testLogger(t), "v-test-db-down", unhealthyDB)

	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp, err := unixHTTPClient(socketPath).Get("http://kahyad/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["db"] != "error" {
		t.Errorf("db field = %v, want error", body["db"])
	}
}

func TestStaleSocketTakeover(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")

	// Simulate a daemon that crashed without cleaning up: bind a real unix
	// listener, then close it WITHOUT unlinking, leaving an orphaned socket
	// file that nothing is listening on.
	staleLn, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("create stale listener: %v", err)
	}
	if ul, ok := staleLn.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if err := staleLn.Close(); err != nil {
		t.Fatalf("close stale listener: %v", err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("expected stale socket file to remain on disk: %v", err)
	}

	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() over stale socket should succeed, got: %v", err)
	}
	go srv.Serve()

	client := unixHTTPClient(socketPath)
	resp, err := client.Get("http://kahyad/health")
	if err != nil {
		t.Fatalf("GET /health after takeover: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if err := srv.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestSecondInstanceRefused(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")

	first := New(testConfig(socketPath), testLogger(t), "v-first", healthyDB)
	if err := first.Prepare(); err != nil {
		t.Fatalf("first Prepare() error = %v", err)
	}
	go first.Serve()
	defer first.Shutdown()

	// Give the first instance's listener a moment to be dial-able (bind
	// already happened synchronously in Prepare, but be defensive).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := unixHTTPClient(socketPath).Get("http://kahyad/health"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first instance never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}

	second := New(testConfig(socketPath), testLogger(t), "v-second", healthyDB)
	err := second.Prepare()
	if err == nil {
		t.Fatal("second Prepare() error = nil, want ErrAlreadyRunning")
	}
	if err != ErrAlreadyRunning {
		t.Fatalf("second Prepare() error = %v, want ErrAlreadyRunning", err)
	}
}

func TestRunGracefulShutdownOnContextCancel(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-run", healthyDB)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := unixHTTPClient(socketPath).Get("http://kahyad/health"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() returned error after cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after Run() shutdown: err=%v", err)
	}
}

// TestTakeoverRaceSingleWinner reproduces the TOCTOU hazard the startup
// flock exists to prevent: N concurrent startups against one stale socket
// file must yield exactly one bound listener; every loser must see
// ErrAlreadyRunning, and the winner's listener must be reachable at the
// canonical path (not orphaned by a later remove+rebind).
func TestTakeoverRaceSingleWinner(t *testing.T) {
	dir := shortSocketDir(t)
	sock := filepath.Join(dir, "s.sock")

	// Seed a stale socket file (no listener behind it).
	staleLn, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	// Close without unlinking: simulates a SIGKILLed daemon's leftover.
	staleLn.(*net.UnixListener).SetUnlinkOnClose(false)
	staleLn.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket file missing after seed: %v", err)
	}

	const n = 8
	type result struct {
		ln   net.Listener
		lock *os.File
		err  error
	}
	results := make(chan result, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			ln, lock, err := prepareListener(sock)
			results <- result{ln, lock, err}
		}()
	}
	close(start)

	var winners []result
	for i := 0; i < n; i++ {
		r := <-results
		if r.err == nil {
			winners = append(winners, r)
		} else if r.err != ErrAlreadyRunning {
			t.Errorf("loser returned unexpected error: %v", r.err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("want exactly 1 winner, got %d", len(winners))
	}
	w := winners[0]
	defer w.ln.Close()
	defer w.lock.Close()

	// The winner's listener must be the one reachable at the path: accept a
	// connection dialed against the canonical socket path.
	done := make(chan error, 1)
	go func() {
		conn, err := w.ln.Accept()
		if err == nil {
			conn.Close()
		}
		done <- err
	}()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial winner: %v", err)
	}
	conn.Close()
	if err := <-done; err != nil {
		t.Fatalf("winner accept: %v", err)
	}
}

// TestSocketDirPermsTightenedWhenPreexisting guards against the MkdirAll
// no-op: a pre-existing 0755 socket dir must be chmod'd to 0700 at startup.
func TestSocketDirPermsTightenedWhenPreexisting(t *testing.T) {
	dir := shortSocketDir(t)
	sockDir := filepath.Join(dir, "d")
	if err := os.Mkdir(sockDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sock := filepath.Join(sockDir, "s.sock")
	ln, lock, err := prepareListener(sock)
	if err != nil {
		t.Fatalf("prepareListener: %v", err)
	}
	defer ln.Close()
	defer lock.Close()

	fi, err := os.Stat(sockDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("socket dir perms = %o, want 0700", got)
	}
}
