// kahyad is the Kâhya control-plane daemon (HANDOFF §4): intent router,
// task/saga state machine, policy engine, cost governor, ledger, scheduler,
// and the SQLite memory index. This file is the daemon entrypoint:
// load config → init logger → start the UDS HTTP server → block on
// SIGTERM/SIGINT → graceful shutdown (W12-01 step 1/4).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/buildinfo"
	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/notify"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/secrets"
	"kahya/kahyad/internal/server"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/traceid"
)

func main() {
	os.Exit(run())
}

// run contains main's logic and returns the process exit code, so defers
// (closing the log file) actually execute before the process exits.
func run() int {
	bootTraceID := traceid.New()

	cfg, err := config.Load()
	if err != nil {
		bootFailLine(bootTraceID, "config_load_failed", err)
		return 1
	}

	// MINOR 5: the resolved log level must be in place before logx.New
	// constructs the boot logger's handler, since New shares the package-
	// wide slog.LevelVar every Logger reads from.
	logx.SetLevel(parseLogLevel(cfg.LogLevel))

	log, err := logx.New(cfg.LogDir, bootTraceID)
	if err != nil {
		bootFailLine(bootTraceID, "logger_init_failed", err)
		return 1
	}
	defer log.Close()

	log.Info("boot",
		"version", buildinfo.Version,
		"env", cfg.Env,
		"socket", cfg.Socket,
		"pid", os.Getpid(),
	)

	// Serialize the WHOLE startup — including migrations — across
	// processes: kahyad is brain.db's only writer, so at most one kahyad
	// may exist from the first DB byte on. Without this, two racing boots
	// (double launch, launchd respawn overlap) race goose.Up on a fresh DB.
	lock, err := server.AcquireStartupLock(cfg.Socket)
	if err != nil {
		if errors.Is(err, server.ErrAlreadyRunning) {
			log.Error("already_running", "socket", cfg.Socket)
			return 1
		}
		log.Error("startup_lock_failed", "err", err.Error())
		return 1
	}

	// Migrate before anything else can serve a request: a half-migrated
	// brain.db must never be reachable (HANDOFF §4 ⚑ fail-closed).
	st, err := store.Open(cfg)
	if err != nil {
		if errors.Is(err, store.ErrSQLiteFeatureMissing) {
			log.Error("sqlite_feature_missing", "err", err.Error())
		} else {
			log.Error("migrate_failed", "err", err.Error())
		}
		return 1
	}
	defer st.Close() // checkpoints the WAL (TRUNCATE) on every exit path
	log.Info("migrations_applied", "schema_version", st.SchemaVersion())

	srv := server.New(cfg, log, buildinfo.Version, st)
	srv.AdoptStartupLock(lock)
	// /v1/memory/search (W12-03 step 4). Its own JSONL logging is scoped
	// per-request (from the request body's trace_id, or a freshly minted
	// one), not to bootTraceID.
	srv.SetSearcher(search.New(st.DB(), log, search.DefaultConfig()))

	// /v1/reindex (W12-04 step 5). idx is shared between this route and the
	// boot-time incremental reindex kicked off just below, so its internal
	// mutex correctly serializes the two against each other. It is ALSO
	// shared with /v1/mcp's memory_write/memory_forget tools (W12-05,
	// SetMCPMemory below): a single Indexer instance means its mutex
	// correctly serializes a full corpus reindex against a single-file
	// ReindexFile call regardless of which route triggered which.
	idx := indexer.New(st.DB(), cfg.MemoryDir, log)
	srv.SetReindexer(idx)

	// POST /v1/mcp (W12-05): the append-only events ledger (policy_decision,
	// hafiza_injected, and - passed through to mcp/memory.Server - its own
	// memory_write/memory_forget rows) and the memory tools themselves.
	srv.SetEventLogger(st)
	srv.SetMCPMemory(cfg.MemoryDir, idx)

	// POST /v1/task (W12-07): st.Queries already has exactly the
	// InsertTask/UpdateTaskState/UpdateTaskSession method shape
	// server.TaskStore needs, so it satisfies the interface directly with
	// no adapter.
	srv.SetTaskStore(st.Queries)

	// POST /v1/task's per-task Anthropic forward-proxy + cost governor
	// (W12-08). notifier logs+ledgers alarms (Telegram delivery is
	// W3-07); governor is rebuilt once here from every historical
	// model_call ledger event, then shared across every task for the
	// rest of the process's life (kahyad/internal/anthproxy.Governor is
	// safe for concurrent use). credential is selected by
	// cfg.credential_mode - see kahyad/internal/anthproxy's package doc
	// comment for the OWNER AUTH DECISION this selects between
	// (passthrough is the owner-decision default; keychain remains fully
	// implemented as a valid fallback).
	notifier := notify.New(log, st)
	bootEvents, err := loadAnthproxyBootEvents(context.Background(), st)
	if err != nil {
		log.Error("anthproxy_boot_events_failed", "err", err.Error())
		return 1
	}
	limits := anthproxy.Limits{
		DailyBudgetUSD:         cfg.DailyBudgetUSD,
		MonthlyBudgetUSD:       cfg.MonthlyBudgetUSD,
		TaskTokenCeiling:       cfg.TaskTokenCeiling,
		DowngradeAtRatio:       cfg.DowngradeAtRatio,
		CacheHitAlarmThreshold: cfg.CacheHitAlarmThreshold,
		EstRequestTokens:       cfg.EstRequestTokens,
	}
	governor := anthproxy.Boot(bootEvents, limits, nil, notifier)
	log.Info("anthproxy_governor_booted", "events_replayed", len(bootEvents), "credential_mode", cfg.CredentialMode)

	credential := buildCredentialSource(cfg, log)
	// EgressGate is nil for now (returns nil/always-allow) - the model-call
	// egress gate lands in W3-05; the hook shape is fixed here so that
	// task lands with no wiring changes to this file.
	srv.SetAnthproxy(governor, notifier, credential, nil)

	// ctx is created here (BEFORE the boot reindex goroutine below is
	// spawned, not after) so that goroutine can share the SAME
	// signal-cancelled context srv.Run uses, instead of running on an
	// unrelated context.Background() that shutdown could never reach
	// (BLOCKER 2).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Incremental reindex on every boot, after migrations (W12-04 step:
	// "Startup hook in main.go"). Async: the memory corpus can be large
	// enough that blocking Serve on it would delay every other request
	// (including /health) for no reason - a boot-time reindex and a
	// concurrent POST /v1/reindex both funnel through idx's own mutex
	// regardless of goroutine scheduling.
	//
	// BLOCKER 2: reindexDone is joined (Wait()) below, AFTER srv.Run
	// returns but BEFORE run() itself returns - i.e. strictly before the
	// deferred st.Close() above ever executes. Previously nothing joined
	// this goroutine and it ran on context.Background(), so a shutdown
	// signal could let st.Close() close brain.db while this goroutine was
	// still mid-reindex, producing spurious "database is closed" file
	// errors; now shutdown cancels ctx (idx.Reindex checks ctx.Err()
	// between files and stops early) and then actually waits for it to
	// finish before the DB is allowed to close.
	var reindexDone sync.WaitGroup
	reindexDone.Add(1)
	go func() {
		defer reindexDone.Done()
		// idx.Reindex logs event=reindex_done (or event=reindex_cancelled
		// if ctx was cancelled mid-run) itself, scoped to bootTraceID since
		// it is passed in non-empty here - see
		// kahyad/internal/indexer.Indexer.Reindex's doc comment - so there
		// is nothing left to log a second time on success.
		if _, err := idx.Reindex(ctx, bootTraceID, false); err != nil {
			log.With(bootTraceID).Error("reindex_failed", "err", err.Error())
		}
	}()

	runErr := srv.Run(ctx)

	// Cancel ctx explicitly (a no-op if a shutdown signal already fired
	// it) and WAIT for the boot reindex goroutine to observe the
	// cancellation and finish its in-flight per-file transaction, before
	// this function returns - so this always completes strictly before the
	// deferred st.Close() call above (BLOCKER 2).
	stop()
	reindexDone.Wait()

	if runErr != nil {
		if errors.Is(runErr, server.ErrAlreadyRunning) {
			// server.Run already logged event=already_running.
			return 1
		}
		log.Error("fatal", "err", runErr.Error())
		return 1
	}

	log.Info("shutdown_complete")
	return 0
}

// parseLogLevel maps config.Config.LogLevel's four validated values
// (config.validateLogLevel already fails Load closed on anything else) onto
// their slog.Level, defaulting to Info for safety if an unrecognized value
// ever reached here regardless.
func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// loadAnthproxyBootEvents reads every historical kind='model_call' ledger
// event and decodes it into an anthproxy.BootEvent (W12-08 step 3: "SELECT
// sums for today/this month/per task ... then maintained in memory") -
// this is the one place a sqlcgen.Event row is converted into
// anthproxy.BootEvent, keeping kahyad/internal/anthproxy itself
// store-agnostic (it never imports sqlcgen). A row whose payload fails to
// decode, or whose ts fails to parse, is skipped rather than failing boot
// entirely - one malformed historical row must never block the daemon
// from starting.
func loadAnthproxyBootEvents(ctx context.Context, st *store.Store) ([]anthproxy.BootEvent, error) {
	rows, err := st.Queries.ListEventsByKind(ctx, anthproxy.EventModelCall)
	if err != nil {
		return nil, fmt.Errorf("list model_call events: %w", err)
	}
	events := make([]anthproxy.BootEvent, 0, len(rows))
	for _, row := range rows {
		ts, err := time.Parse(time.RFC3339Nano, row.Ts)
		if err != nil {
			continue
		}
		var rec anthproxy.ModelCallRecord
		if err := json.Unmarshal([]byte(row.Payload), &rec); err != nil {
			continue
		}
		events = append(events, anthproxy.BootEvent{Ts: ts, Record: rec})
	}
	return events, nil
}

// buildCredentialSource selects the anthproxy.CredentialSource matching
// cfg.CredentialMode (config.Load already fails closed on any other
// value). In keychain mode, KAHYA_ANTHROPIC_KEY_OVERRIDE substitutes the
// real Keychain read only when cfg.Env=="dev" (W12-10's hermetic gate);
// any other value in prod is ignored with a loud
// event=key_override_ignored warn line, never silently trusted.
func buildCredentialSource(cfg config.Config, log *logx.Logger) anthproxy.CredentialSource {
	if cfg.CredentialMode == config.CredentialModePassthrough {
		return anthproxy.NewPassthroughCredentialSource()
	}
	warnOverrideIgnored := func() {
		log.Warn("key_override_ignored", "reason", "KAHYA_ANTHROPIC_KEY_OVERRIDE set outside KAHYA_ENV=dev")
	}
	return anthproxy.NewKeychainCredentialSource(secrets.New(), cfg.Env, warnOverrideIgnored)
}

// bootFailLine emits a hand-rolled JSONL error line for failures that occur
// before the real logger exists (config load, logger init). Even these
// bootstrap paths must honor the "every line is JSONL with a trace_id"
// invariant (HANDOFF §4 ⚑).
func bootFailLine(traceID, event string, err error) {
	line, _ := json.Marshal(map[string]string{
		"ts":       time.Now().UTC().Format(time.RFC3339Nano),
		"level":    "ERROR",
		"event":    event,
		"trace_id": traceID,
		"error":    err.Error(),
	})
	fmt.Fprintln(os.Stderr, string(line))
}
