// Package mlxsup implements a GENERIC kahyad-supervised child-process
// supervisor for the local MLX helper processes (HANDOFF §4 ⚑ supervision
// contract: "MLX süreçlerini kahyad süpervize eder ... kahyad portu
// config'te sabitler ve health-check yapar"; launchd only ever holds
// kahyad itself). W12-11 uses this for the embedding service
// (mlx/embed/server.py); W3-08 reuses it verbatim for the Qwen3-30B-A3B
// secret-lane server, which is why nothing in this package mentions
// embeddings, ports, or model_ver by name.
//
// A Supervisor owns exactly one child process at a time: it lazily spawns
// it on the first EnsureRunning call (not at kahyad boot - HANDOFF §6
// timing note / W12-11 step 2: "spawn on first embedding need"), polls its
// HTTP /health endpoint until it answers {"status":"ok",...} (2s interval,
// bounded by a startup grace period - "first model load is slow"), and
// restarts it with exponential backoff (capped) if it ever exits on its
// own without Stop having been called. Stop kills the entire process
// GROUP (same Setpgid/kill(-pgid) pattern as kahyad/internal/spawn, W12-07)
// so a grandchild the child itself spawned cannot survive kahyad shutdown
// either.
package mlxsup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"kahya/kahyad/internal/logx"
)

// State values a Supervisor can report (kahyad's /health "embed" field
// surfaces this verbatim, W12-11 step 2).
const (
	// StateDisabled means Config.Cmd was empty at construction: this
	// Supervisor will never spawn anything, ever. Terminal.
	StateDisabled = "disabled"
	// StateDown means no child process is currently running - either
	// never started, or the last one exited/was killed and a restart (if
	// any) has not yet been (re)spawned.
	StateDown = "down"
	// StateStarting means a child process is running but has not yet
	// answered /health with {"status":"ok"} for its CURRENT run.
	StateStarting = "starting"
	// StateOK means the current child process's /health last answered ok.
	StateOK = "ok"
)

// ErrDisabled is returned by EnsureRunning when Config.Cmd was empty.
var ErrDisabled = errors.New("mlxsup: supervisor disabled (empty cmd)")

// healthPollTimeout bounds a single /health HTTP round trip - short,
// because a slow/hanging health endpoint must not itself block startup
// detection or (transitively) a caller's Search call any longer than
// necessary.
const healthPollTimeout = 2 * time.Second

// Config configures one Supervisor. Every duration left at its zero value
// gets the documented default (New fills them in) so callers only need to
// override what W3-08 or a future caller actually cares about.
type Config struct {
	// Name identifies this supervisor in JSONL log lines and is the only
	// thing that varies between kahyad's embedding-service supervisor
	// (W12-11) and its future 30B secret-lane supervisor (W3-08).
	Name string
	// Cmd is the child's argv: argv[0] is the executable, the rest its
	// args. An empty Cmd means this Supervisor is permanently disabled
	// (StateDisabled) - it never spawns anything.
	Cmd []string
	// Dir is the child's working directory. Empty means "inherit
	// kahyad's own cwd" (exec.Cmd's zero-value behavior).
	Dir string
	// ExtraEnv is appended to os.Environ() for the child (e.g.
	// "KAHYA_EMBED_PORT=8092" - W12-11 wires the fixed config port this
	// way rather than hardcoding it in this package).
	ExtraEnv []string
	// HealthURL is the child's health-check endpoint, e.g.
	// "http://127.0.0.1:8092/health". Must answer JSON
	// {"status":"ok",...} (any other status, or a connection failure,
	// counts as "not ready yet").
	HealthURL string

	// PollInterval is the health-poll cadence during startup (default 2s
	// per HANDOFF §4 ⚑ / W12-11 step 2).
	PollInterval time.Duration
	// StartupGrace bounds how long EnsureRunning's poll loop tolerates a
	// freshly spawned child not yet answering /health before giving up
	// on THIS call (default 60s - "first model load is slow"). A later
	// EnsureRunning call resumes polling the SAME (still-starting) child
	// rather than spawning a second one.
	StartupGrace time.Duration
	// MinBackoff/MaxBackoff bound the exponential restart backoff after
	// an unexpected exit (default 1s -> 60s cap, doubling each time).
	MinBackoff time.Duration
	MaxBackoff time.Duration

	// Log is the boot-scoped logger every event=mlx_* line is written
	// through (never scoped to a request trace_id - a supervised
	// process's lifecycle spans many requests).
	Log *logx.Logger
}

// Supervisor is safe for concurrent use.
type Supervisor struct {
	cfg    Config
	client *http.Client

	mu         sync.Mutex
	state      string
	cmd        *exec.Cmd
	done       chan struct{} // closed by supervise() when cmd.Wait() returns
	stopped    bool
	backoff    time.Duration
	spawnCount int // incremented once per spawnLocked call; supervisor_test.go uses this to observe an autonomous restart without depending on State()'s exact timing
}

// spawnCountForTest returns how many times this Supervisor has spawned a
// child process (initial lazy start plus every autonomous restart).
// Exported only within this package, for supervisor_test.go.
func (s *Supervisor) spawnCountForTest() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawnCount
}

// New constructs a Supervisor from cfg, applying every documented default
// for a zero-valued duration field. It never spawns anything itself -
// call EnsureRunning to do that lazily.
func New(cfg Config) *Supervisor {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.StartupGrace <= 0 {
		cfg.StartupGrace = 60 * time.Second
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = 1 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 60 * time.Second
	}
	s := &Supervisor{
		cfg:     cfg,
		client:  &http.Client{Timeout: healthPollTimeout},
		backoff: cfg.MinBackoff,
	}
	if len(cfg.Cmd) == 0 {
		s.state = StateDisabled
	} else {
		s.state = StateDown
	}
	return s
}

// State reports the Supervisor's current status: "ok"|"starting"|"down"|
// "disabled" (kahyad's /health endpoint surfaces this verbatim).
func (s *Supervisor) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// EnsureRunning lazily spawns the child on its FIRST call (W12-11 step 2:
// "spawn on first embedding need, not at boot") and blocks until it
// answers /health ok, ctx is done, or Config.StartupGrace elapses since
// THIS call's own start - whichever comes first. A later call, made while
// an earlier spawn is still starting, resumes polling the SAME child
// (never spawns a second one) and can succeed quickly if the child
// finished loading in the meantime.
//
// Once the child has answered healthy at least once, subsequent calls
// return immediately without a fresh health probe - the cached state is
// trusted until either an actual embed/inference call fails (the caller's
// own problem to handle - HANDOFF-adjacent: search never hard-fails on
// the vector leg) or the child's process exits, which the background
// supervise goroutine always detects and reacts to independently of any
// caller being blocked here.
//
// A child that exits before ever answering healthy makes EnsureRunning
// return promptly (as soon as the exit is observed) rather than
// continuing to poll a dead process for the rest of StartupGrace: a
// misconfigured or crash-looping child (missing model, broken venv, ...)
// must not make every reindex/search silently pay the full startup-grace
// timeout on top of an otherwise-fast failure. The background supervise
// goroutine's own restart-with-backoff loop is what actually retries -
// this method only ever waits on the ONE spawn generation it observed at
// its own start.
func (s *Supervisor) EnsureRunning(ctx context.Context) error {
	s.mu.Lock()
	switch s.state {
	case StateDisabled:
		s.mu.Unlock()
		return ErrDisabled
	case StateOK:
		s.mu.Unlock()
		return nil
	}
	if s.cmd == nil {
		if err := s.spawnLocked(); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("mlxsup: spawn %s: %w", s.cfg.Name, err)
		}
	}
	done := s.done
	s.mu.Unlock()

	deadline := time.Now().Add(s.cfg.StartupGrace)
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if s.pingHealth() {
			s.mu.Lock()
			s.state = StateOK
			s.backoff = s.cfg.MinBackoff
			s.mu.Unlock()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mlxsup: %s did not become healthy within %s", s.cfg.Name, s.cfg.StartupGrace)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			// The specific process generation this call has been waiting
			// on already exited (supervise closes `done` right after
			// cmd.Wait() returns) - fail fast instead of polling a dead
			// process for the rest of StartupGrace. A LATER EnsureRunning
			// call picks up whatever the background restart-with-backoff
			// loop has spawned (or is about to) by then.
			return fmt.Errorf("mlxsup: %s exited before becoming healthy", s.cfg.Name)
		case <-ticker.C:
		}
	}
}

// Stop kills the current child's entire process GROUP (SIGKILL, negative
// pid) and marks the Supervisor stopped so the background supervise
// goroutine does not restart it - matching the halt-on-shutdown posture
// kahyad/internal/spawn already uses for per-task workers (W12-07). Safe
// to call when nothing was ever spawned (no-op) or more than once
// (idempotent). Blocks briefly (bounded by stopGrace) for the process to
// actually be reaped before returning, so a caller checking
// `pgrep -f <name>` immediately afterward reliably sees nothing.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	s.stopped = true
	cmd := s.cmd
	done := s.done
	if s.state != StateDisabled {
		s.state = StateDown
	}
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if done != nil {
		select {
		case <-done:
		case <-time.After(stopGrace):
		}
	}
}

// stopGrace bounds how long Stop waits for the killed process group to be
// reaped before giving up and returning anyway (mirrors
// kahyad/internal/spawn's drainGrace reasoning: SIGKILL is not
// instantaneous, but Stop must never hang indefinitely).
const stopGrace = 2 * time.Second

// spawnLocked starts cfg.Cmd as a new process-group leader and launches
// the background supervise goroutine that reaps it and (unless Stop was
// called) restarts it with backoff on an unexpected exit. Caller must
// hold s.mu; it is released by the time this returns, exactly as it was
// found (locked).
func (s *Supervisor) spawnLocked() error {
	cmd := exec.Command(s.cfg.Cmd[0], s.cfg.Cmd[1:]...)
	cmd.Dir = s.cfg.Dir
	cmd.Env = append(append([]string{}, os.Environ()...), s.cfg.ExtraEnv...)
	// New process group (id == the child's own pid): Stop's kill(-pgid)
	// then reaches every process in the group, including any
	// grandchildren the child itself spawns (same pattern as
	// kahyad/internal/spawn.Run, W12-07).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The child's stdout/stderr are diagnostics only (mirrors
	// kahyad/internal/spawn's treatment of worker stderr) - never parsed,
	// just kept out of kahyad's own stdout/stderr so a misbehaving child
	// cannot interleave raw text into kahyad's JSONL log stream. Discarding
	// here (rather than piping to the logger line-by-line) keeps this
	// generic package free of a line-scanning goroutine neither of this
	// task's two children (the embed server, the future 30B server) need
	// kahyad to parse.
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan struct{})
	s.cmd = cmd
	s.done = done
	s.state = StateStarting
	s.spawnCount++
	s.logger().Info("mlx_spawn", "name", s.cfg.Name, "pid", cmd.Process.Pid, "cmd", s.cfg.Cmd)

	go s.supervise(cmd, done)
	return nil
}

// supervise blocks on cmd.Wait() and reacts once the child exits: if this
// Supervisor was stopped intentionally (Stop) or cmd has already been
// superseded by a later spawn, it does nothing further. Otherwise it logs
// the unexpected exit and restarts with backoff - autonomously, without
// needing any caller currently blocked in EnsureRunning (HANDOFF §4 ⚑
// supervision contract: kahyad supervises MLX processes for their whole
// lifetime, not just at first use).
func (s *Supervisor) supervise(cmd *exec.Cmd, done chan struct{}) {
	waitErr := cmd.Wait()
	defer close(done)

	s.mu.Lock()
	current := s.cmd == cmd
	stopped := s.stopped
	if current {
		s.state = StateDown
	}
	s.mu.Unlock()

	if !current || stopped {
		return
	}

	s.logger().Warn("mlx_exit", "name", s.cfg.Name, "err", errString(waitErr))
	s.restartWithBackoff()
}

// restartWithBackoff sleeps the current backoff duration (doubling it,
// capped at Config.MaxBackoff, for the NEXT crash) and then respawns,
// unless Stop was called while it slept.
func (s *Supervisor) restartWithBackoff() {
	s.mu.Lock()
	wait := s.backoff
	next := s.backoff * 2
	if next > s.cfg.MaxBackoff {
		next = s.cfg.MaxBackoff
	}
	s.backoff = next
	s.mu.Unlock()

	s.logger().Info("mlx_restart_scheduled", "name", s.cfg.Name, "backoff_ms", wait.Milliseconds())
	time.Sleep(wait)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	if err := s.spawnLocked(); err != nil {
		s.logger().Error("mlx_restart_failed", "name", s.cfg.Name, "err", err.Error())
	}
}

// pingHealth issues one GET against cfg.HealthURL and reports whether it
// answered 200 with a JSON body whose "status" field is exactly "ok". Any
// connection failure, non-200 status, or malformed/other-status body is
// "not healthy" - the caller (EnsureRunning) treats all of these
// identically as "keep waiting".
func (s *Supervisor) pingHealth() bool {
	req, err := http.NewRequest(http.MethodGet, s.cfg.HealthURL, nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	return body.Status == "ok"
}

func (s *Supervisor) logger() *logx.Logger {
	if s.cfg.Log != nil {
		return s.cfg.Log
	}
	// Defensive fallback so a misconfigured caller gets discarded-but-safe
	// logging (a fresh in-memory-only logger would still need a log dir;
	// simplest safe fallback is a boot logger writing under os.TempDir())
	// rather than a nil-pointer panic on the very first event this package
	// tries to log. Every real caller (main.go) always supplies Log.
	l, err := logx.New(os.TempDir(), "")
	if err != nil {
		// Should not happen (os.TempDir() is always writable); if it
		// somehow does, there is nothing safer left to do than panic - a
		// nil logger would panic on first use anyway, just later and less
		// clearly.
		panic(fmt.Sprintf("mlxsup: fallback logger: %v", err))
	}
	return l
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
