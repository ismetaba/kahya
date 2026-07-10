// Package server implements kahyad's control-plane HTTP-over-UDS listener
// (HANDOFF §4 ⚑ IPC contract). This skeleton (W12-01) owns the socket
// lifecycle, /health, and graceful shutdown; later tasks (W12-02..09) mount
// additional routes (e.g. /policy/check) onto the same *http.Server.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/traceid"
)

// ErrAlreadyRunning is returned by Prepare/Run when a live kahyad instance
// already holds the configured socket and answers /health.
var ErrAlreadyRunning = errors.New("server: another kahyad instance is already running")

// DBHealth is the health data source /health reports under "db" and
// "schema_version" (W12-02). It is a narrow interface — rather than the
// concrete *store.Store — so this package does not have to pull in the
// sqlite/cgo dependency just to serve HTTP; *store.Store satisfies it
// without any adapter code.
type DBHealth interface {
	Health(ctx context.Context) (ok bool, schemaVersion int64, err error)
}

const (
	healthCheckDialTimeout = 500 * time.Millisecond
	healthCheckTimeout     = 1 * time.Second
	readHeaderTimeout      = 5 * time.Second
	shutdownTimeout        = 5 * time.Second
)

// Server is kahyad's HTTP-over-UDS control-plane server.
type Server struct {
	cfg     config.Config
	log     *logx.Logger
	version string
	db      DBHealth

	ln   net.Listener
	http *http.Server
	// lock is the exclusive startup flock on <socket>.lock, held for the
	// whole daemon lifetime. It serializes socket takeover across processes
	// and proves the socket at cfg.Socket is ours to unlink on Shutdown.
	// The kernel releases it on any process death, including SIGKILL.
	lock *os.File

	started time.Time
}

// New constructs a Server bound to cfg.Socket, reporting db's health at
// /health. Call Prepare (or Run, which calls Prepare for you) to actually
// bind the listener.
func New(cfg config.Config, log *logx.Logger, version string, db DBHealth) *Server {
	return &Server{cfg: cfg, log: log, version: version, db: db}
}

// Prepare resolves the socket takeover logic (HANDOFF §4 IPC step 3) and
// binds the listener, but does not yet start serving:
//   - socket file missing → bind fresh.
//   - socket file present and a live daemon answers /health → return
//     ErrAlreadyRunning (already logged as event=already_running).
//   - socket file present but dead (dial fails) → unlink and bind fresh.
//
// The bound socket is chmod'd 0600.
func (s *Server) Prepare() error {
	ln, lock, err := prepareListener(s.cfg.Socket)
	if err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			s.log.Error("already_running", "socket", s.cfg.Socket)
		}
		return err
	}
	s.ln = ln
	s.lock = lock
	s.started = time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)

	s.http = &http.Server{
		Handler:           s.withTraceLogging(mux),
		ReadHeaderTimeout: readHeaderTimeout,
	}
	return nil
}

// Serve blocks accepting connections until Shutdown is called or a fatal
// listener error occurs. It returns nil on a clean shutdown.
func (s *Server) Serve() error {
	err := s.http.Serve(s.ln)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the HTTP server (5s budget) and unlinks the
// socket file. It does not log shutdown_complete — the caller (main.go)
// logs that on its boot-scoped logger after Shutdown returns, since it is
// not tied to any single request.
func (s *Server) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := s.http.Shutdown(ctx)
	// Safe: we have held the startup flock since Prepare, so no other
	// daemon can have bound this path in the meantime.
	_ = os.Remove(s.cfg.Socket)
	if s.lock != nil {
		_ = s.lock.Close() // releases the flock; the .lock file stays (never unlink a lock file)
		s.lock = nil
	}
	return err
}

// Run prepares, serves, and blocks until ctx is cancelled, at which point
// it performs a graceful Shutdown. It returns ErrAlreadyRunning immediately
// if another instance already holds the socket.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Prepare(); err != nil {
		return err
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve() }()

	select {
	case <-ctx.Done():
		return s.Shutdown()
	case err := <-serveErr:
		return err
	}
}

// acquireStartupLock takes the exclusive cross-process flock that serializes
// socket takeover. It also creates the socket directory and tightens it to
// 0700 even when it pre-existed with looser permissions (MkdirAll alone is a
// no-op on an existing directory's mode).
func acquireStartupLock(socketPath string) (*os.File, error) {
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("server: create socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("server: chmod socket dir %s: %w", dir, err)
	}

	f, err := os.OpenFile(socketPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("server: open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			// Another instance holds the lock: it is either serving already
			// or mid-startup and about to. Either way, we must not start.
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("server: flock %s.lock: %w", socketPath, err)
	}
	return f, nil
}

// prepareListener implements the socket takeover decision described on
// Prepare, returning a bound, chmod 0600 unix listener plus the held
// startup lock. The flock makes the stat→probe→remove→listen sequence
// atomic across processes — without it, two racing startups can both
// conclude the socket is dead, both bind, and later unlink each other's
// live socket.
func prepareListener(socketPath string) (net.Listener, *os.File, error) {
	lock, err := acquireStartupLock(socketPath)
	if err != nil {
		return nil, nil, err
	}

	if _, err := os.Stat(socketPath); err == nil {
		alive := probeHealth(socketPath)
		if alive {
			lock.Close()
			return nil, nil, ErrAlreadyRunning
		}
		// Dead socket file: unlink before binding a fresh one.
		if err := os.Remove(socketPath); err != nil {
			lock.Close()
			return nil, nil, fmt.Errorf("server: remove stale socket %s: %w", socketPath, err)
		}
	} else if !os.IsNotExist(err) {
		lock.Close()
		return nil, nil, fmt.Errorf("server: stat socket %s: %w", socketPath, err)
	}

	// Tighten the umask so the socket is never observable with wider
	// permissions than 0600, even before the explicit chmod below. The
	// enclosing directory is already 0700, so this is defense in depth.
	oldMask := syscall.Umask(0o177)
	ln, err := net.Listen("unix", socketPath)
	syscall.Umask(oldMask)
	if err != nil {
		lock.Close()
		return nil, nil, fmt.Errorf("server: listen on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		lock.Close()
		return nil, nil, fmt.Errorf("server: chmod socket %s: %w", socketPath, err)
	}
	return ln, lock, nil
}

// probeHealth dials socketPath and asks /health; it returns true only if a
// live daemon answers 200. Any dial/request error is treated as "not
// alive" — the caller then unlinks and rebinds. This mirrors the fail-safe
// posture elsewhere in the system: ambiguity resolves toward "take over a
// dead socket" rather than "refuse to start forever".
func probeHealth(socketPath string) bool {
	client := &http.Client{
		Timeout: healthCheckTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: healthCheckDialTimeout}
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
	resp, err := client.Get("http://kahyad/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

type healthResponse struct {
	Status        string `json:"status"`
	PID           int    `json:"pid"`
	UptimeS       int64  `json:"uptime_s"`
	Version       string `json:"version"`
	DB            string `json:"db"`
	SchemaVersion int64  `json:"schema_version"`
}

// handleHealth reports process liveness plus brain.db reachability and
// schema version (W12-02 step "extend /health"). db is "ok" only when a
// live ping against brain.db succeeds; any ping failure or a nil db (should
// never happen outside of misconfigured tests) reports "error" — this
// endpoint never claims the database is fine when it hasn't verified that.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	dbStatus := "error"
	var schemaVersion int64
	if s.db != nil {
		ok, version, err := s.db.Health(r.Context())
		schemaVersion = version
		if err == nil && ok {
			dbStatus = "ok"
		}
	}

	resp := healthResponse{
		Status:        "ok",
		PID:           os.Getpid(),
		UptimeS:       int64(time.Since(s.started).Seconds()),
		Version:       s.version,
		DB:            dbStatus,
		SchemaVersion: schemaVersion,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// statusRecorder captures the status code written by a downstream handler
// so middleware can log it after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// withTraceLogging assigns/propagates a trace_id and logs event=http_request
// for every handled request (HANDOFF §4 IPC step 3).
func (s *Server) withTraceLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Kahya-Trace-Id")
		if id == "" {
			id = traceid.New()
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.With(id).Info("http_request",
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
