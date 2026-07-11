// supervisor.go is this package's other half (see memcheck.go's package
// doc comment): kahyad's supervisor for the local `mlx_lm.server` process
// serving Qwen3-30B-A3B-4bit, the secret-lane model (HANDOFF §4 IPC ⚑ MLX
// supervision + §4 ⚑ memory pressure + §5 product principle "gizlilik
// kodda").
//
// This is deliberately NOT a second process-supervision implementation:
// every actual spawn/health-poll/crash-restart-with-backoff mechanic is
// kahyad/internal/mlxsup.Supervisor, REUSED verbatim (the W3-08 task spec
// is explicit: "REUSE it for the Qwen server, do NOT fork a second
// supervisor") - this file only adds the two things that generic package
// intentionally does NOT know about: the fail-closed free-memory gate
// (memcheck.go) that must run BEFORE every EnsureRunning attempt, and the
// idle-TTL unload policy (in-flight request tracking + a periodic
// idle-check that calls mlxsup's StopForIdle) that only makes sense for a
// 17GB-resident model sharing a machine with ComfyUI/Wan - mlx/embed's
// tiny Qwen3-Embedding-0.6B service (W12-11) has no such policy and is
// unaffected by anything in this package.
package mlx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/mlxsup"
)

// Turkish user-facing strings (CLAUDE.md language policy), byte-exact per
// the W3-08 task spec's own fail-closed wording.
const (
	// MsgNoLocalMemory is the FAIL-CLOSED message - byte-exact, never
	// paraphrased: "yerel model için bellek yok".
	MsgNoLocalMemory = "yerel model için bellek yok"
	// MsgNoLocalMemoryGuidance is the accompanying guidance line - byte-
	// exact: "ComfyUI/Wan kapatıp tekrar deneyin".
	MsgNoLocalMemoryGuidance = "ComfyUI/Wan kapatıp tekrar deneyin"
)

// ErrLocalModelUnavailable is the ONE typed sentinel every failure mode in
// this package collapses to: insufficient free memory (memcheck.go) OR the
// child process failing to spawn/become healthy/exceeding MaxRestarts
// (mlxsup). HANDOFF §4 ⚑'s crown invariant is that NONE of these ever
// reroutes secret-lane work to the cloud - callers (kahyad/internal/
// secretlane) must pause/refuse the task on this error, never fall back to
// an Anthropic call. errors.Is-comparable so both this package's own tests
// and secretlane's ordering-invariant test can assert exactly that
// structurally (see supervisor_test.go's
// TestEnsureRunningMemcheckInsufficientNeverTouchesProxy).
var ErrLocalModelUnavailable = errors.New(MsgNoLocalMemory)

// Config configures one Supervisor - the single Qwen3-30B-A3B secret-lane
// server kahyad ever runs (HANDOFF §4 ⚑: local fleet is locked to exactly
// three models; this is the third).
type Config struct {
	// Cmd is the mlx_lm.server invocation (mlx/qwen/.venv/bin/mlx_lm.server
	// --model <Qwen3-30B-A3B-4bit path> --host 127.0.0.1 --port
	// <cfg.mlx.qwen_port>) - built by the caller (main.go), not this
	// package, mirroring how kahyad/internal/mlxsup.Config.Cmd already
	// works for the embedding service (W12-11).
	Cmd []string
	Dir string
	// Host/Port are the SAME host:port Cmd's own --host/--port flags were
	// built with - this package derives HealthURL/BaseURL from these
	// rather than re-parsing Cmd.
	Host string
	Port int

	// StartupGrace bounds a single EnsureRunning call's poll loop (default
	// 120s - task spec: "timeout 120s, cold load streams 17GB").
	StartupGrace time.Duration
	PollInterval time.Duration
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
	// MaxRestarts is kahyad/internal/mlxsup.Config.MaxRestarts (default 3 -
	// task spec: "crash -> respawn backoff max 3 then fail-closed").
	MaxRestarts int

	// IdleTTL is how long the server may sit with ZERO in-flight requests
	// before this package unloads it (SIGTERM+reap - default 10m, task
	// spec / cfg.mlx.idle_ttl). 0 disables idle-unload entirely (the
	// server, once loaded, stays resident until kahyad shuts down) - never
	// the production default, but useful for a caller that wants the old
	// always-resident behavior.
	IdleTTL time.Duration
	// IdleCheckInterval is the idle-monitor's poll cadence (default 30s).
	IdleCheckInterval time.Duration

	// MemCheck is the free-memory gate (memcheck.go's CheckFunc) consulted
	// before every spawn attempt. nil means RealVMStatCheck (production);
	// tests inject a fake to drive the fail-closed path deterministically
	// without depending on this machine's actual memory state.
	MemCheck CheckFunc

	// OnUnloaded is called (best-effort, synchronously) every time the
	// idle-TTL monitor actually unloads the server - the caller's hook to
	// write the `mlx_unloaded` ledger event (task spec acceptance
	// criterion) without this package taking a direct store/ledger
	// dependency of its own (mirrors kahyad/internal/mlxsup's own
	// "generic, kahyad-agnostic" posture).
	OnUnloaded func()

	Log *logx.Logger
}

// Supervisor is safe for concurrent use.
type Supervisor struct {
	cfg Config
	sup *mlxsup.Supervisor

	mu        sync.Mutex
	inFlight  int
	idleSince time.Time // zero value = "not currently idle-eligible" (never became healthy yet, or idle-unload already fired)

	stopOnce   sync.Once
	stopIdleCh chan struct{}
}

// New constructs a Supervisor. It never spawns anything itself - the first
// EnsureRunning call does that lazily (HANDOFF §6 timing note: "spawn on
// first secret-lane request").
func New(cfg Config) *Supervisor {
	if cfg.StartupGrace <= 0 {
		cfg.StartupGrace = 120 * time.Second
	}
	if cfg.MaxRestarts <= 0 {
		cfg.MaxRestarts = 3
	}
	if cfg.IdleCheckInterval <= 0 {
		cfg.IdleCheckInterval = 30 * time.Second
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}

	sup := mlxsup.New(mlxsup.Config{
		Name:         "qwen",
		Cmd:          cfg.Cmd,
		Dir:          cfg.Dir,
		HealthURL:    fmt.Sprintf("http://%s:%d/v1/models", cfg.Host, cfg.Port),
		PollInterval: cfg.PollInterval,
		StartupGrace: cfg.StartupGrace,
		MinBackoff:   cfg.MinBackoff,
		MaxBackoff:   cfg.MaxBackoff,
		MaxRestarts:  cfg.MaxRestarts,
		Log:          cfg.Log,
	})

	s := &Supervisor{cfg: cfg, sup: sup, stopIdleCh: make(chan struct{})}
	if cfg.IdleTTL > 0 {
		go s.runIdleMonitor()
	}
	return s
}

// BaseURL is the OpenAI-compatible base URL the secret-lane classifier/
// worker directs its calls to once EnsureRunning has succeeded
// ("http://127.0.0.1:<qwen_port>/v1" - task spec step 4).
func (s *Supervisor) BaseURL() string {
	return fmt.Sprintf("http://%s:%d/v1", s.cfg.Host, s.cfg.Port)
}

// State reports the underlying mlxsup.Supervisor's state verbatim
// ("ok"|"starting"|"down"|"disabled"|"failed").
func (s *Supervisor) State() string { return s.sup.State() }

// EnsureRunning is the ONE entrypoint every secret-lane caller uses before
// sending Qwen a request (task spec step 1): first the fail-closed
// memcheck (memcheck.go) - insufficient free memory returns
// ErrLocalModelUnavailable WITHOUT EVER attempting to spawn/contact
// anything, structurally guaranteeing no code path from this failure mode
// to the Anthropic proxy (there IS no such path in this package at all).
// Only once memcheck passes does this delegate to the underlying
// mlxsup.Supervisor's own spawn/poll/health machinery; ANY failure there
// (spawn error, startup-grace timeout, MaxRestarts exceeded) is likewise
// collapsed to the SAME ErrLocalModelUnavailable sentinel - callers never
// need to distinguish "why" local is unavailable, only that it is, and
// that the fail-closed response is identical either way.
func (s *Supervisor) EnsureRunning(ctx context.Context) error {
	ok, _, err := HasSufficientMemory(s.cfg.MemCheck)
	if err != nil || !ok {
		return ErrLocalModelUnavailable
	}

	if err := s.sup.EnsureRunning(ctx); err != nil {
		return fmt.Errorf("%w: %s", ErrLocalModelUnavailable, err.Error())
	}

	s.mu.Lock()
	if s.inFlight == 0 {
		s.idleSince = time.Now()
	}
	s.mu.Unlock()
	return nil
}

// Do is the one entrypoint every actual Qwen HTTP call (classification,
// local answering) should be wrapped in: it calls EnsureRunning (fail-
// closed memcheck + spawn/health-poll - task spec step 1/2), tracks the
// call as in-flight against the idle-TTL monitor (Acquire/release), and -
// ONLY when the server was ALREADY warm before this call started - bounds
// fn's own ctx to warmBudget (task spec: "300ms budget after WARM load").
// A cold server (State() != "ok" before EnsureRunning) gets NO such bound
// here: the caller's own ctx is passed through unchanged, so a cold load
// classification WAITS for EnsureRunning's own StartupGrace rather than
// racing an unrelated 300ms clock (task spec ordering invariant: "cold
// model means the classification WAITS for load or fails closed - it
// never skips ahead to cloud"). warmBudget<=0 disables the budget
// entirely (fn always gets ctx unchanged).
func (s *Supervisor) Do(ctx context.Context, warmBudget time.Duration, fn func(ctx context.Context) error) error {
	warmBefore := s.State() == mlxsup.StateOK

	if err := s.EnsureRunning(ctx); err != nil {
		return err
	}

	release := s.Acquire()
	defer release()

	callCtx := ctx
	if warmBefore && warmBudget > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, warmBudget)
		defer cancel()
	}
	return fn(callCtx)
}

// Acquire marks one request as in-flight against the Qwen server -
// callers (the secret-lane classifier, and eventually the worker's
// OpenAI-compatible HTTP client) must call this immediately before
// issuing a real request and call the returned release func immediately
// after it completes (success or failure alike). This is what the
// idle-TTL monitor consults: "ZERO in-flight requests" (task spec step 1)
// is only ever true between a release() and the next Acquire().
func (s *Supervisor) Acquire() (release func()) {
	s.mu.Lock()
	s.inFlight++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.inFlight--
		if s.inFlight <= 0 {
			s.inFlight = 0
			s.idleSince = time.Now()
		}
		s.mu.Unlock()
	}
}

// runIdleMonitor is the background goroutine started by New whenever
// Config.IdleTTL > 0: every IdleCheckInterval, if the server is currently
// healthy, has zero in-flight requests, and has been idle for at least
// IdleTTL, it unloads the server (mlxsup.StopForIdle - SIGTERM+reap,
// leaving it eligible for a later EnsureRunning to lazily bring back) and
// invokes Config.OnUnloaded (the caller's `mlx_unloaded` ledger hook).
func (s *Supervisor) runIdleMonitor() {
	ticker := time.NewTicker(s.cfg.IdleCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopIdleCh:
			return
		case <-ticker.C:
			s.checkIdle()
		}
	}
}

func (s *Supervisor) checkIdle() {
	s.mu.Lock()
	due := s.sup.State() == mlxsup.StateOK &&
		s.inFlight == 0 &&
		!s.idleSince.IsZero() &&
		time.Since(s.idleSince) >= s.cfg.IdleTTL
	if due {
		// Reset immediately so a slow StopForIdle (or a concurrent tick)
		// can never fire twice for the same idle period.
		s.idleSince = time.Time{}
	}
	s.mu.Unlock()

	if !due {
		return
	}
	// NOTE (observed live, KAHYA_MLX_TESTS=1): mlxsup.Supervisor.
	// StopForIdle sets its OWN State() to "down" BEFORE it finishes
	// SIGKILLing/reaping the child (which, for a real ~16GB process, can
	// take a non-trivial fraction of a second to a couple of seconds,
	// bounded by mlxsup's own stopGrace) - so a caller polling State()
	// alone can observe "down" a short while BEFORE OnUnloaded fires
	// below. This is harmless for kahyad's real usage (the `mlx_unloaded`
	// ledger event is still written, just not perfectly atomically with
	// the state transition), but a caller that specifically wants to know
	// "has OnUnloaded fired yet" must wait on ITS OWN signal (e.g. a
	// counter), never infer it from State() alone - see mlx/live_test.go's
	// own comment on this exact point.
	s.sup.StopForIdle()
	if s.cfg.OnUnloaded != nil {
		s.cfg.OnUnloaded()
	}
}

// Stop permanently shuts the server down (kahyad process exit) and stops
// the idle monitor goroutine - mirrors kahyad/internal/mlxsup.Supervisor.
// Stop's own "launchd holds only kahyad" shutdown contract; safe to call
// even when nothing was ever spawned, and more than once.
func (s *Supervisor) Stop() {
	s.stopOnce.Do(func() { close(s.stopIdleCh) })
	s.sup.Stop()
}
