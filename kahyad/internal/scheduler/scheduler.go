// Package scheduler implements kahyad's two-tier scheduler (W4-01,
// HANDOFF §4 stack ⚑ scheduling):
//
//   - Wall-clock jobs (nightly backup, the 08:30 briefing, nightly
//     consolidation) are declared in config.Config.Jobs and run via
//     launchd LaunchAgents this package installs (RenderPlist/Sync in
//     launchd.go) — launchd's own StartCalendarInterval coalesces any
//     interval missed while the machine slept into exactly one run on
//     wake.
//   - Short-interval in-daemon work (outbox scans, anchor pushes,
//     idle-TTL checks) uses RegisterTick, a thin wrapper over
//     robfig/cron/v3 that only ever runs while this process is alive.
//
// ⚑ Hard rule (HANDOFF §4 stack, golang/go#24595): Go's darwin monotonic
// clock STOPS while the machine sleeps, so an in-daemon cron silently
// misses wall-clock deadlines across sleep/wake. RegisterTick must NEVER
// be used for anything that needs to fire at a specific wall-clock time —
// that is what the jobs:/launchd half of this package is for. Ticks are
// for work that tolerates being silently skipped across a sleep cycle.
//
// This package is the ONE place job registry + trigger dispatch + tick
// registration live: kahyad/internal/server mounts a thin HTTP layer over
// Trigger (POST /jobs/trigger/{name}), and every later task that adds a
// wall-clock job (W4-05 anchor, W4-06 backups, W5-01 briefing, W5-02
// consolidation) only ever calls RegisterHandler + declares a jobs: entry
// — none of them touch launchd or cron directly.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/traceid"
)

// Ledger event kinds this package appends (HANDOFF §5 safety #4
// append-only events ledger). Exported so kahyad/internal/server's tests
// (and any future caller) can assert against the exact string rather than
// a locally-duplicated literal.
const (
	EventJobTriggered = "job.triggered"
	EventJobCompleted = "job.completed"
	EventJobFailed    = "job.failed"
)

// smokeHandlerName is the one built-in handler this package ships (task
// spec step 1: "Ship a built-in smoke handler (writes one ledger event)
// for tests"). A jobs: config entry with handler: smoke resolves to it.
const smokeHandlerName = "smoke"

// ErrUnknownJob is returned by Trigger when name is not a currently
// resolvable job (either never declared in config.Config.Jobs, or
// declared with a Handler name LoadJobs could not resolve against a
// registered handler). kahyad/internal/server's POST /jobs/trigger/{name}
// handler maps this to a 404.
var ErrUnknownJob = errors.New("scheduler: unknown job")

// Handler is a Go-side job implementation, registered by name via
// RegisterHandler and referenced from config.Config.Jobs' Handler field.
// ctx carries the run's trace_id — retrieve it with TraceIDFromContext if
// the handler needs to log/ledger under it itself (the built-in smoke
// handler does exactly this).
type Handler func(ctx context.Context) error

// EventLogger is the append-only events ledger sink Trigger appends
// job.triggered/job.completed/job.failed rows to (HANDOFF §5 safety #4).
// *kahyad/internal/store.Store already has exactly this method shape, so
// it satisfies this interface with no adapter code — mirroring
// kahyad/internal/server.EventLogger's identical seam.
type EventLogger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// tickTraceIDKey is the unexported context key Trigger/RegisterTick stash
// a run's minted trace_id under, so a Handler/tick func can correlate its
// own logging with the run that invoked it (TraceIDFromContext).
type tickTraceIDKey struct{}

// WithTraceID returns a context carrying traceID, retrievable via
// TraceIDFromContext. Exported so a future package (a real backup/
// briefing/consolidation handler) can build its own child contexts the
// same way this package does internally.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, tickTraceIDKey{}, traceID)
}

// TraceIDFromContext returns the trace_id WithTraceID attached to ctx, or
// "" if none was ever attached.
func TraceIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(tickTraceIDKey{}).(string)
	return id
}

// Scheduler is kahyad's two-tier job dispatcher: a registry of named Go
// handlers, a jobs-config-resolved dispatch table Trigger consults, and a
// robfig/cron/v3 instance for RegisterTick's short-interval ticks.
type Scheduler struct {
	mu             sync.RWMutex
	handlersByName map[string]Handler // Go handler name -> fn (RegisterHandler)
	jobHandlers    map[string]Handler // job name -> fn (LoadJobs-resolved)

	log   EventLogger
	jsonl *logx.Logger

	cron *cron.Cron
}

// New constructs a Scheduler. log/jsonl may be nil (tests that only
// exercise RegisterTick, say) — every ledger/JSONL write below is
// best-effort and skipped when its sink is nil, matching this codebase's
// existing "unwired dependency" convention (kahyad/internal/server's
// SetSearcher/SetReindexer et al.). The built-in "smoke" handler
// (task spec step 1) is registered here, before New returns, so it is
// always available regardless of what the caller registers afterward.
func New(log EventLogger, jsonl *logx.Logger) *Scheduler {
	s := &Scheduler{
		handlersByName: make(map[string]Handler),
		jobHandlers:    make(map[string]Handler),
		log:            log,
		jsonl:          jsonl,
		cron:           cron.New(),
	}
	s.RegisterHandler(smokeHandlerName, s.smokeHandler)
	return s
}

// smokeHandler is the built-in "smoke" handler: it writes exactly one
// ledger event (event=job_smoke_ran) so a trigger-endpoint/live-
// verification test has something observable beyond the job.triggered/
// job.completed pair Trigger itself always writes.
func (s *Scheduler) smokeHandler(ctx context.Context) error {
	if s.log == nil {
		return nil
	}
	return s.log.LogEvent(ctx, TraceIDFromContext(ctx), "job_smoke_ran", map[string]any{})
}

// RegisterHandler registers fn under name — a config.JobConfig's Handler
// field names one of these. Call before LoadJobs (or re-call LoadJobs
// afterward) for a newly registered handler to take effect; both are
// safe to call at any time (guarded by the same mutex Trigger reads
// under).
func (s *Scheduler) RegisterHandler(name string, fn Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlersByName[name] = fn
}

// LoadJobs resolves every jobs config entry's Handler name against the
// registered handlers (RegisterHandler) into the job-name -> Handler
// dispatch table Trigger consults. A job whose Handler name is not
// registered is skipped — logged as an error, never a boot failure —
// so one bad/forward-declared jobs: entry (e.g. "briefing" before W5-01
// registers its handler) never takes down the rest of the daemon; it
// simply answers 404 at /jobs/trigger/<that name> until its handler is
// registered and LoadJobs is called again (main.go calls it once, at
// boot, after every RegisterHandler call).
func (s *Scheduler) LoadJobs(jobs []config.JobConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jh := make(map[string]Handler, len(jobs))
	for _, j := range jobs {
		fn, ok := s.handlersByName[j.Handler]
		if !ok {
			if s.jsonl != nil {
				s.jsonl.Error("job_handler_unknown", "job_name", j.Name, "handler", j.Handler)
			}
			continue
		}
		jh[j.Name] = fn
	}
	s.jobHandlers = jh
}

// Trigger dispatches job name asynchronously: it appends an
// EventJobTriggered ledger row (and a matching JSONL line) synchronously
// — so a caller that gets a successful return already has that row
// visible in brain.db — then runs the resolved Handler in its own
// goroutine, appending EventJobCompleted or EventJobFailed once it
// returns. It returns ErrUnknownJob (never runs anything) when name is
// not in the LoadJobs-resolved dispatch table; kahyad/internal/server's
// POST /jobs/trigger/{name} handler is the ONE caller of this method in
// production — both a launchd-scheduled run (via kahya-trigger) and a
// manual `kahya-trigger <name>` invocation go through that exact same
// HTTP route, so there is exactly one dispatch code path (task spec step
// 3's rationale).
func (s *Scheduler) Trigger(ctx context.Context, traceID, name string) error {
	s.mu.RLock()
	fn, ok := s.jobHandlers[name]
	s.mu.RUnlock()
	if !ok {
		return ErrUnknownJob
	}

	payload := map[string]any{"job_name": name, "trace_id": traceID}
	if s.log != nil {
		if err := s.log.LogEvent(ctx, traceID, EventJobTriggered, payload); err != nil && s.jsonl != nil {
			s.jsonl.With(traceID).Warn("job_triggered_ledger_error", "job_name", name, "err", err.Error())
		}
	}
	if s.jsonl != nil {
		s.jsonl.With(traceID).Info("job_triggered", "job_name", name)
	}

	go func() {
		// A fresh, independent context: the run must survive the
		// triggering HTTP request's own context being cancelled (the
		// caller disconnecting must not abort an in-flight job), the same
		// "detach async work from the request context" posture
		// kahyad/internal/server's handleTask already uses for its own
		// worker spawn.
		runCtx := WithTraceID(context.Background(), traceID)
		runErr := fn(runCtx)

		kind := EventJobCompleted
		event := "job_completed"
		resultPayload := map[string]any{"job_name": name, "trace_id": traceID}
		if runErr != nil {
			kind = EventJobFailed
			event = "job_failed"
			resultPayload["error"] = runErr.Error()
		}
		if s.log != nil {
			if err := s.log.LogEvent(context.Background(), traceID, kind, resultPayload); err != nil && s.jsonl != nil {
				s.jsonl.With(traceID).Warn("job_result_ledger_error", "job_name", name, "err", err.Error())
			}
		}
		if s.jsonl != nil {
			if runErr != nil {
				s.jsonl.With(traceID).Error(event, "job_name", name, "err", runErr.Error())
			} else {
				s.jsonl.With(traceID).Info(event, "job_name", name)
			}
		}
	}()
	return nil
}

// everySpecPrefix is RegisterTick's own short-interval descriptor prefix:
// "@every <duration>", where <duration> is anything time.ParseDuration
// accepts (e.g. "100ms", "30s"). Deliberately parsed and scheduled here
// rather than delegated to cron.Cron.AddFunc's identically-spelled
// "@every" descriptor: robfig/cron/v3's own Every() helper floors any
// delay under 1 second up to 1 second ("Delays of less than a second are
// not supported", constantdelay.go) — fine for the wall-clock-adjacent
// cadences RegisterTick's doc comment describes, but it would silently
// break this package's own sub-second test cadence (and any future
// sub-second internal tick). everySchedule below reimplements the same
// fixed-delay Schedule with NO such floor.
const everySpecPrefix = "@every "

// everySchedule is a robfig/cron/v3 Schedule that fires exactly every
// delay, with no minimum — see everySpecPrefix's doc comment for why this
// package does not use cron.Every.
type everySchedule struct {
	delay time.Duration
}

func (e everySchedule) Next(t time.Time) time.Time {
	return t.Add(e.delay)
}

// parseEverySpec parses spec as an "@every <duration>" descriptor,
// reporting ok=false for anything else (including a malformed duration —
// RegisterTick falls back to treating those as a standard cron
// expression, so the original parse error still surfaces to the caller).
func parseEverySpec(spec string) (time.Duration, bool) {
	if !strings.HasPrefix(spec, everySpecPrefix) {
		return 0, false
	}
	d, err := time.ParseDuration(strings.TrimPrefix(spec, everySpecPrefix))
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// RegisterTick registers fn to run on spec — either a standard 5-field
// cron expression, or an "@every <duration>" descriptor (see
// everySpecPrefix's doc comment for why THIS package parses "@every"
// itself rather than deferring to robfig/cron/v3's own, which floors
// sub-second delays) — for as long as this process is alive. It mints a
// fresh per-run trace_id and logs event=tick_fired (JSONL) before invoking
// fn with a context carrying that trace_id (TraceIDFromContext).
//
// ⚑ HARD RULE (HANDOFF §4 stack, golang/go#24595): this is for
// short-interval work that tolerates silently NOT firing across a sleep/
// wake cycle (Go's darwin monotonic clock stops while the machine
// sleeps, so a cron tick scheduled for a specific wall-clock moment can
// simply never fire if that moment falls during sleep). Anything that
// must run at a specific wall-clock time — a nightly backup, a morning
// briefing, nightly consolidation — MUST be a declared config.Config.Jobs
// entry synced to a launchd StartCalendarInterval LaunchAgent (Sync/
// RenderPlist, launchd.go) instead, never a tick registered here.
// StartTicks/StopTicks control whether registered ticks are actually
// firing; RegisterTick itself only ever registers — call StartTicks once,
// after every RegisterTick call, to begin firing.
func (s *Scheduler) RegisterTick(name, spec string, fn func(ctx context.Context)) error {
	job := cron.FuncJob(func() {
		traceID := traceid.New()
		if s.jsonl != nil {
			s.jsonl.With(traceID).Info("tick_fired", "name", name)
		}
		fn(WithTraceID(context.Background(), traceID))
	})

	if delay, ok := parseEverySpec(spec); ok {
		s.cron.Schedule(everySchedule{delay: delay}, job)
		return nil
	}

	if _, err := s.cron.AddJob(spec, job); err != nil {
		return fmt.Errorf("scheduler: register tick %q (spec %q): %w", name, spec, err)
	}
	return nil
}

// StartTicks starts firing every tick registered so far (a no-op if
// already started, or if none are registered — robfig/cron/v3's own
// documented behavior).
func (s *Scheduler) StartTicks() {
	s.cron.Start()
}

// StopTicks stops the tick scheduler: no further ticks fire, and any
// tick currently running is allowed to finish (robfig/cron/v3's own
// graceful-stop contract). Safe to call even if StartTicks was never
// called.
func (s *Scheduler) StopTicks() {
	<-s.cron.Stop().Done()
}
