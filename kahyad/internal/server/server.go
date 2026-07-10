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
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/traceid"
)

// ErrAlreadyRunning is returned by Prepare/Run when a live kahyad instance
// already holds the configured socket and answers /health.
var ErrAlreadyRunning = errors.New("server: another kahyad instance is already running")

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

	ln   net.Listener
	http *http.Server

	started time.Time
}

// New constructs a Server bound to cfg.Socket. Call Prepare (or Run, which
// calls Prepare for you) to actually bind the listener.
func New(cfg config.Config, log *logx.Logger, version string) *Server {
	return &Server{cfg: cfg, log: log, version: version}
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
	ln, err := prepareListener(s.cfg.Socket)
	if err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			s.log.Error("already_running", "socket", s.cfg.Socket)
		}
		return err
	}
	s.ln = ln
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
	_ = os.Remove(s.cfg.Socket)
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

// prepareListener implements the socket takeover decision described on
// Prepare, returning a bound, chmod 0600 unix listener.
func prepareListener(socketPath string) (net.Listener, error) {
	if _, err := os.Stat(socketPath); err == nil {
		alive := probeHealth(socketPath)
		if alive {
			return nil, ErrAlreadyRunning
		}
		// Dead socket file: unlink before binding a fresh one.
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("server: remove stale socket %s: %w", socketPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("server: stat socket %s: %w", socketPath, err)
	}

	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return nil, fmt.Errorf("server: create socket dir: %w", err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("server: listen on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("server: chmod socket %s: %w", socketPath, err)
	}
	return ln, nil
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
	Status  string `json:"status"`
	PID     int    `json:"pid"`
	UptimeS int64  `json:"uptime_s"`
	Version string `json:"version"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{
		Status:  "ok",
		PID:     os.Getpid(),
		UptimeS: int64(time.Since(s.started).Seconds()),
		Version: s.version,
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
