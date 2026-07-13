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
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"kahya/kahyad/internal/anchor"
	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/backup"
	"kahya/kahyad/internal/briefing"
	"kahya/kahyad/internal/buildinfo"
	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/consolidation"
	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/embed"
	"kahya/kahyad/internal/eval"
	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/mlx"
	"kahya/kahyad/internal/mlxsup"
	"kahya/kahyad/internal/notify"
	"kahya/kahyad/internal/outbox"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/remembered"
	"kahya/kahyad/internal/ritual"
	"kahya/kahyad/internal/scheduler"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/secrets"
	"kahya/kahyad/internal/server"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/taint"
	"kahya/kahyad/internal/task"
	"kahya/kahyad/internal/telegram"
	"kahya/kahyad/internal/traceid"
	"kahya/kahyad/internal/ui"
	mcpfs "kahya/mcp/fs"
	mcposascript "kahya/mcp/osascript"
	mcpshell "kahya/mcp/shell"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

// dispatch handles kahyad's argv-based subcommands: "policy validate"
// (W3-01 acceptance criterion) and "-sync-jobs" (W4-01: renders/installs/
// removes launchd job LaunchAgents once, per cfg.Jobs, and exits - see
// runSyncJobs). Any other argv (including none at all) runs the daemon
// itself (run()).
func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "policy" {
		return runPolicyCLI(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "-sync-jobs" {
		return runSyncJobs(stdout, stderr)
	}
	return run()
}

// syncJobsRunnerFn resolves the scheduler.Runner runSyncJobs uses to talk
// to launchctl. A package-level func var — the exact same "swap it in a
// test" seam kahyad/internal/scheduler.currentUIDFn already uses — purely
// so a hermetic test can inject a fake Runner instead of shelling out to
// the real launchctl (MINOR 5 fix: runSyncJobs previously had zero
// automated coverage precisely because scheduler.NewExecRunner() was
// called inline here with no way to substitute it). Production code never
// reassigns this; it always resolves to the real scheduler.NewExecRunner().
var syncJobsRunnerFn = scheduler.NewExecRunner

// runSyncJobs implements the standalone `kahyad -sync-jobs` mode (W4-01
// task spec step 5): load config, sync ~/Library/LaunchAgents' set of
// com.kahya.job.<name> plists to exactly match cfg.Jobs, and exit -
// without opening brain.db or binding the UDS socket at all (this mode
// touches neither, so it is safe to run even while a real kahyad is
// already up). Idempotent: safe to run repeatedly, e.g. from a shell
// script after editing config.yaml's jobs: section, without restarting
// the daemon.
//
// MINOR 5: the launchctl Runner is resolved via syncJobsRunnerFn (not a
// direct scheduler.NewExecRunner() call) so main_test.go's
// TestRunSyncJobsRendersAndBootstrapsConfiguredJobs can inject a fake
// Runner and drive this dispatch path end to end (real config.Load, real
// scheduler.Sync decision logic, real plist rendering to a temp
// $HOME/Library/LaunchAgents) without ever shelling out to the real
// launchctl or touching the real user's LaunchAgents directory. The
// underlying decision logic (which plists get written/removed, when
// bootout+bootstrap fire) is scheduler.Sync's own responsibility and is
// already thoroughly unit-tested in kahyad/internal/scheduler/
// launchd_test.go via the same fakeRunner pattern; this test's job is
// narrower — proving runSyncJobs' own argv/config/exit-code wiring around
// that already-tested logic is correct.
func runSyncJobs(stdout, stderr io.Writer) int {
	traceID := traceid.New()

	cfg, err := config.Load()
	if err != nil {
		bootFailLine(traceID, "config_load_failed", err)
		return 1
	}
	logx.SetLevel(parseLogLevel(cfg.LogLevel))
	log, err := logx.New(cfg.LogDir, traceID)
	if err != nil {
		bootFailLine(traceID, "logger_init_failed", err)
		return 1
	}
	defer log.Close()

	home, err := os.UserHomeDir()
	if err != nil {
		log.Error("sync_jobs_home_dir_failed", "err", err.Error())
		return 1
	}
	opts := scheduler.SyncOptions{
		LaunchAgentsDir: filepath.Join(home, "Library", "LaunchAgents"),
		JobLogDir:       filepath.Join(home, "Library", "Logs", "Kahya"),
		TriggerBinPath:  cfg.TriggerBinPath,
	}
	if err := scheduler.Sync(cfg.Jobs, opts, syncJobsRunnerFn(), log); err != nil {
		log.Error("sync_jobs_failed", "err", err.Error())
		return 1
	}
	log.Info("sync_jobs_done", "jobs", len(cfg.Jobs))
	return 0
}

// runPolicyCLI implements `kahyad policy validate [path]` (W3-01): loads
// and strictly validates a policy.yaml, printing the tool count and
// exiting 0 on success, or the validation error (Turkish, per CLAUDE.md's
// user-facing-string language policy) and exiting 1 on any failure. path
// defaults to policy.DefaultPath() (the repo-root policy.yaml derived
// from this binary's own install location, kahyad/internal/policy.
// DefaultPath's doc comment) when omitted - matching the task spec's "a
// policy.yaml path arg or the default repo-root one".
func runPolicyCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "validate" {
		fmt.Fprintln(stderr, "usage: kahyad policy validate [path]")
		return 2
	}
	path := policy.DefaultPath()
	if len(args) > 1 {
		path = args[1]
	}
	pol, err := policy.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "policy.yaml geçersiz: %s\n", err.Error())
		return 1
	}
	fmt.Fprintf(stdout, "policy.yaml geçerli: %d araç tanımlı.\n", len(pol.Tools))
	return 0
}

// run contains the daemon's logic and returns the process exit code, so
// defers (closing the log file) actually execute before the process
// exits.
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

	// W6-02: <data_dir>/tmp (e.g. ~/Library/Application Support/Kahya/tmp),
	// 0700 - hammerspoon/kahya.lua's ffmpeg push-to-talk capture writes
	// ptt-<epoch>.wav there; the worker's own mode="stt" delete-safety
	// check (kahya_worker.__main__._maybe_delete_audio, via KAHYA_TMP_DIR -
	// kahyad/internal/spawn.BuildEnv) only ever deletes a file whose parent
	// directory is exactly this one. Best-effort: a failure here is logged,
	// never fatal to boot - every OTHER capability (health, log tailing,
	// ordinary typed-prompt tasks) must not go down over this directory
	// alone failing to create.
	if err := os.MkdirAll(cfg.TmpDir(), 0o700); err != nil {
		log.Error("tmp_dir_create_failed", "path", cfg.TmpDir(), "err", err.Error())
	} else if err := os.Chmod(cfg.TmpDir(), 0o700); err != nil {
		// MkdirAll leaves an EXISTING directory's mode untouched (matching
		// kahyad/internal/logx.New's own identical enforce-0700-on-every-
		// boot precedent for LogDir) - explicit Chmod so a pre-existing,
		// more-permissive tmp dir is tightened back down on every restart.
		log.Error("tmp_dir_chmod_failed", "path", cfg.TmpDir(), "err", err.Error())
	}

	// W3-01: load + strictly validate policy.yaml BEFORE the UDS listener
	// (bound further below, inside srv.Run's Prepare) can accept a single
	// /policy/check or /v1/mcp request. Any error here — a missing file, an
	// unknown YAML key, a bad class, a W3 tool marked reversible, a missing
	// mandatory fs_write_deny_globs entry, anything — means kahyad boots
	// into PERMANENT deny-all mode (tasks/README.md global convention:
	// policy error => DENY, never a permissive fallback) rather than
	// refusing to start at all: refusing to start would silently take down
	// every other already-working capability (health checks, log
	// tailing...) over a policy typo, when fail-closed on the tool-gating
	// surface alone is the more precise fix. policyLoadErr is consulted
	// again just below, once srv exists, to flip SetDenyAll.
	pol, policyLoadErr := policy.Load(cfg.PolicyPath)
	if policyLoadErr != nil {
		log.Error("policy_load_failed", "path", cfg.PolicyPath, "err", policyLoadErr.Error())
	} else {
		log.Info("policy_loaded", "path", cfg.PolicyPath, "tools", len(pol.Tools))
	}

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
	if policyLoadErr != nil {
		srv.SetDenyAll()
	}

	// Local MLX embedding service (W12-11): kahyad-supervised, lazily
	// spawned on first embedding need (never at boot - HANDOFF §6 timing
	// note), 127.0.0.1-only (HANDOFF §4 ⚑ supervision contract). embedDir
	// is the mlx/embed directory the service's own script/venv live under
	// - derived from cfg.EmbedCmd (a two-element
	// [".../.venv/bin/python", ".../server.py"] slice) so the child runs
	// with that directory as its cwd regardless of kahyad's own.
	embedSup := mlxsup.New(mlxsup.Config{
		Name:      "embed",
		Cmd:       cfg.EmbedCmd,
		Dir:       embedServiceDir(cfg.EmbedCmd),
		ExtraEnv:  []string{fmt.Sprintf("KAHYA_EMBED_PORT=%d", cfg.EmbedPort)},
		HealthURL: fmt.Sprintf("http://127.0.0.1:%d/health", cfg.EmbedPort),
		Log:       log,
	})
	srv.SetEmbedHealth(embedSup)
	embedClient := embed.New(fmt.Sprintf("http://127.0.0.1:%d", cfg.EmbedPort), embedSup)

	// /v1/memory/search (W12-03 step 4; W12-11 step 4 adds the KNN leg).
	// Its own JSONL logging is scoped per-request (from the request
	// body's trace_id, or a freshly minted one), not to bootTraceID.
	searcher := search.New(st.DB(), log, search.DefaultConfig())
	searcher.SetEmbedder(embedClient, cfg.ActiveEmbedModelVer)
	srv.SetSearcher(searcher)

	// /v1/reindex (W12-04 step 5; W12-11 steps 3/5 add the vector
	// backfill/re_embed pass). idx is shared between this route and the
	// boot-time incremental reindex kicked off just below, so its internal
	// mutex correctly serializes the two against each other. It is ALSO
	// shared with /v1/mcp's memory_write/memory_forget tools (W12-05,
	// SetMCPMemory below): a single Indexer instance means its mutex
	// correctly serializes a full corpus reindex against a single-file
	// ReindexFile call regardless of which route triggered which.
	// reindexBackfiller composes idx with a Backfiller so both the
	// boot-time hook below and POST /v1/reindex keep chunk_vec in
	// lockstep with chunks - see kahyad/internal/embed's package doc.
	idx := indexer.New(st.DB(), cfg.MemoryDir, log)
	backfiller := embed.NewBackfiller(st.DB(), embedClient, cfg.ActiveEmbedModelVer, log, st)
	reindexBackfiller := embed.NewReindexBackfiller(idx, backfiller, log)
	srv.SetReindexer(reindexBackfiller)

	// POST /v1/mcp (W12-05): the append-only events ledger (policy_decision,
	// hafiza_injected, and - passed through to mcp/memory.Server - its own
	// memory_write/memory_forget rows) and the memory tools themselves.
	srv.SetEventLogger(st)
	srv.SetMCPMemory(cfg.MemoryDir, idx)

	// W3-02: the binding autonomy-ladder decision engine, consulted by
	// POST /policy/check, POST /policy/consume-token, POST /policy/
	// feedback, GET /policy/state, POST /policy/promote, POST /policy/undo,
	// and mcp.go's /v1/mcp policyGateMiddleware. pol is the SAME loaded
	// (or, on load failure, zero-value) Policy resolved above for
	// deny-all's own decision - constructed unconditionally so
	// s.policyEngine is never a bare nil even in deny-all mode (denyAll
	// itself already short-circuits every caller before the engine would
	// ever be consulted; see server.Server.SetDenyAll's doc comment).
	policyEngine := policy.NewEngine(pol, st.Queries, st)
	// MINOR fix: cfg.UndoWindowSeconds (default 300 = 5 minutes, config.
	// defaults's doc comment) replaces policy.Engine's own hardcoded
	// undoWindowDuration const - production behavior is unchanged at the
	// default, but a shorter config.yaml value (or a test's direct
	// SetUndoWindowDuration call) now actually takes effect end-to-end.
	policyEngine.SetUndoWindowDuration(time.Duration(cfg.UndoWindowSeconds) * time.Second)
	srv.SetPolicyEngine(policyEngine)

	// W3-03: fs_read/fs_write/fs_delete, registered onto the SAME shared
	// /v1/mcp server as the memory tools (server.SetFSTool's doc
	// comment). fsPolicyClient satisfies mcp/fs.PolicyClient with a
	// direct in-process call onto policyEngine (this task's own
	// ambiguity-decision note: mcp/fs cannot import kahyad/internal/
	// policy directly - Go's internal-package import boundary - so this
	// adapter lives on the server package's side instead); srv.DenyAll
	// is threaded through so a policy.yaml load failure at boot still
	// denies every fs operation even though fs_read/fs_write/fs_delete
	// bypass policyGateMiddleware's own deny-all short-circuit (they run
	// their own gate chain - see mcp.go's fsOwnedTools). undoDir is
	// cfg.DataDir/undo (fallback pre-image copies for a fs_write target
	// NOT inside any git work tree); policyEngine's undo-window-expiry
	// hook purges those copies once their 5-minute window lapses.
	home, err := os.UserHomeDir()
	if err != nil {
		log.Error("fs_tool_home_dir_failed", "err", err.Error())
		return 1
	}

	// W3-05: the egress gate - the single decision engine every off-box
	// byte passes through (HANDOFF §5 safety #1 flag). notifier is built
	// here (rather than down where W12-08 originally built it) precisely
	// so BOTH the gate and the anthproxy governor below share the exact
	// same Notifier instance. sessions is the per-process, session-
	// lifetime "has this session done a secret-lane read" tracker
	// (durable cross-restart taint is W4-03) - fsTool's
	// SensitiveMarker below and every future in-process caller (the
	// Telegram sender W3-07, the ledger anchor push W4-05) all reach the
	// SAME egressGate, never a second copy.
	notifier := notify.New(log, st)
	sessions := egress.NewSensitiveTracker()
	egressBudget := egress.NewSQLBudget(st.Queries)
	egressGate := egress.NewGate(pol.Egress, sessions, egressBudget, st, notifier, log)
	srv.SetEgressGate(egressGate)

	// W3-07: the Telegram approval bot. New NEVER errors - an unconfigured
	// telegram.chat_id/telegram.user_id pair, a missing/locked
	// kahya.telegram Keychain item, or a telebot construction failure all
	// resolve to a DISABLED bot (event=telegram_disabled), never a boot
	// failure - every other subsystem above/below is wired identically
	// either way. tgBot consumes the SAME egressGate/policyEngine/st every
	// other in-process caller uses (HANDOFF §5 safety #1: approval cards
	// are egress too).
	tgCfg := telegram.Config{ChatID: cfg.TelegramChatID, UserID: cfg.TelegramUserID, APIURL: cfg.TelegramAPIURL}
	tgBot := telegram.New(tgCfg, buildTelegramTokenSource(cfg, log), st, egressGate, policyEngine, notifier, home, pol.SecretLaneGlobs, log)

	// W6-01: the Hammerspoon CLI exec bridge - kahyad's LOCAL approval-card
	// and background/scheduled-task-notification surface (HANDOFF §4 IPC
	// ⚑: "yerelde Hammerspoon hs.notify ... arka-plan görev sonuçları aynı
	// kanaldan trace_id ile döner"). localDelivery fans a
	// SendNotification call out to BOTH tgBot (remote) and hsCli (local) -
	// see kahyad/internal/ui.FanOutDelivery's own doc comment.
	hsCli := ui.New(cfg.HsCliPath, log)
	localDelivery := ui.FanOutDelivery{Primary: tgBot, Local: hsCli}

	policyEngine.SetPendingApprovalHook(func(info policy.PendingApprovalInfo) {
		// Fired synchronously from inside Check/Approve's own request path
		// (kahyad/internal/policy.Engine.pendingApprovalHook's doc
		// comment) - the actual Telegram/Hammerspoon sends each happen in
		// their own goroutine so a slow/blocked send can never delay a
		// policy decision.
		go tgBot.OnPendingApproval(context.Background(), info)
		go hsCli.ShowApproval(context.Background(), info.TraceID, info.ID)
	})
	log.Info("telegram_bot_wired", "enabled", tgBot.Enabled())

	// BLOCKER B/C: the shared, daemon-lifetime forward-proxy (egress.Proxy,
	// started below) cannot infer which task a needs_network:true
	// container's connection belongs to on its own — egressTokens maps the
	// per-task credential mcp/shell.Runner mints (via
	// server.NewEgressTokenRegistrar below) to that task's own trace_id,
	// so the proxy can attribute the connection to the SAME
	// SensitiveTracker key mcp/fs's fs_read seam and the anthproxy
	// egress-gate factory both use. See egress.ProxySessionRegistry's own
	// doc comment.
	egressTokens := egress.NewProxySessionRegistry()

	fsPolicyClient := server.NewFSPolicyClient(policyEngine, srv.DenyAll)
	fsTool := mcpfs.New(home, pol.FSWriteDenyGlobs, pol.SecretLaneGlobs, filepath.Join(cfg.DataDir, "undo"), fsPolicyClient, st, server.NewFSLogger(log), server.NewEgressSensitiveMarker(egressGate))
	// W3-10 gate-test fix: fs_read's secret-lane detection was path-glob
	// only (HANDOFF's ordering invariant scopes policy.yaml's globs to
	// paths) - this wires the SEPARATE, content-based half so a file whose
	// PATH is unremarkable but whose CONTENT is finans/sağlık/kimlik still
	// marks the session sensitive (see mcp/fs.Server.ContentClassifier's
	// own doc comment).
	fsTool.ContentClassifier = server.NewFSContentClassifier()
	policyEngine.SetUndoExpiryHook(fsTool.PurgeExpired)
	srv.SetFSTool(fsTool)

	// W3-04: shell_docker/shell_host, registered onto the SAME shared
	// /v1/mcp server as memory's/fs's own tools (server.SetShellTool's doc
	// comment). fsPolicyClient/server.NewFSLogger(log) are REUSED directly
	// (not re-adapted) - mcp/shell.PolicyClient/Logger are type aliases of
	// mcp/fs's own interfaces (kahyad/internal/server/shell.go's doc
	// comment), so the exact values already built for the fs tool above
	// satisfy mcp/shell's dependencies with zero new adapter code.
	// pinnedDigest is docker/sandbox/IMAGE_DIGEST's committed content - a
	// missing/not-yet-built file resolves to "" (mcpshell.LoadPinnedDigest's
	// documented fail-closed return), not a boot error, since an
	// un-built sandbox image should refuse shell_docker at RUN time, not
	// take down the rest of the daemon.
	pinnedDigest, err := mcpshell.LoadPinnedDigest(cfg.DockerImageDigestPath)
	if err != nil {
		log.Error("shell_pinned_digest_load_failed", "path", cfg.DockerImageDigestPath, "err", err.Error())
	}
	shellRunner := mcpshell.NewRunner(home, cfg.DockerImageTag, pinnedDigest, pol.FSWriteDenyGlobs, cfg.ShellWorkdirRoots, fsPolicyClient, st, server.NewFSLogger(log))

	// W3-05: needs_network:true jobs attach to the kahya-egress internal
	// Docker network instead of being refused outright (W3-04's
	// fail-closed placeholder) - egressSidecarDigest is docker/egress/
	// IMAGE_DIGEST's committed content, the SAME "missing pin -> refuse
	// at RUN time, not at boot" posture pinnedDigest already uses above.
	// shellRunner.Exec is reused directly (not a second Executor
	// implementation) for the sidecar's own docker network/run/exec
	// calls.
	egressSidecarDigest, err := mcpshell.LoadPinnedDigest(cfg.EgressSidecarDigestPath)
	if err != nil {
		log.Error("egress_sidecar_digest_load_failed", "path", cfg.EgressSidecarDigestPath, "err", err.Error())
	}
	shellRunner.SetEgressEnsurer(mcpshell.NewEgressNetworkEnsurer(shellRunner.Exec, egressSidecarDigest), cfg.EgressPort)
	// BLOCKER B/C: bind every needs_network:true container Runner.Run
	// starts to its own task's trace_id, via the SAME egressTokens
	// registry the egress.Proxy listener below consults.
	shellRunner.SetEgressTokenRegistrar(server.NewEgressTokenRegistrar(egressTokens))

	hostExec := mcpshell.NewHostExec(home, fsPolicyClient, st, server.NewFSLogger(log), nil)
	srv.SetShellTool(mcpshell.New(shellRunner, hostExec))
	if shellRunner.Health != nil && !shellRunner.Health.Healthy(context.Background()) {
		log.Warn("docker_unavailable_at_boot", "hint", "make docker-up")
	} else {
		log.Info("docker_health_checked", "healthy", true)
	}

	// W3-09: applescript_run/jxa_run/shortcuts_run, registered onto the
	// SAME shared /v1/mcp server as memory's/fs's/shell's own tools
	// (server.SetOsascriptTool's doc comment). fsPolicyClient/
	// server.NewFSLogger(log) are REUSED directly (not re-adapted) -
	// mcp/osascript.PolicyClient/Logger are type aliases of mcp/fs's own
	// interfaces (kahyad/internal/server/osascript.go's doc comment), so
	// the exact values already built for the fs/shell tools above satisfy
	// mcp/osascript's dependencies with zero new adapter code. home is the
	// SAME real user home directory fsTool/shellRunner already resolved
	// above (shortcuts_run's --input-path canonicalization needs it;
	// applescript_run/jxa_run do not).
	osascriptRunner := mcposascript.NewRunner(home, fsPolicyClient, st, server.NewFSLogger(log))
	srv.SetOsascriptTool(mcposascript.New(osascriptRunner))

	// POST /v1/task (W12-07): st.Queries already has exactly the
	// InsertTask/UpdateTaskState/UpdateTaskSession method shape
	// server.TaskStore needs, so it satisfies the interface directly with
	// no adapter.
	srv.SetTaskStore(st.Queries)
	// W4-03: the raw *sql.DB handle handleTask's OnSession callback opens
	// its own transaction against, so a freshly-spawned user-initiated
	// task's session_taint(tier=clean) row commits atomically with its own
	// tasks.session_id write (task spec step 1a) - see
	// server.Server.SetSessionTaintDB's doc comment.
	srv.SetSessionTaintDB(st.DB())

	// W4-03: the session-tier tracker Engine.Check's taint-check hook
	// consults, and the SAME instance kahyad/internal/reader/actor_seed.go
	// (wired below, alongside the Reader runner) writes clean rows
	// through for a freshly Actor-seeded session.
	taintTracker := taint.New(st.Queries, st)
	policyEngine.SetTaintChecker(taintTracker)
	// BLOCKER 1+2 fix (post-security-review): the server-side session
	// resolver Check's taint-check hook uses to determine WHICH session a
	// request belongs to, from trace_id/task_id alone - never from the
	// worker-supplied (untrusted, and on /v1/mcp absent entirely)
	// session_id on the wire. MUST be wired together with SetTaintChecker
	// above (SetSessionResolver's own doc comment: an unwired resolver is
	// a documented legacy/test fallback, never a production posture).
	policyEngine.SetSessionResolver(policy.NewStoreSessionResolver(st.Queries))

	// W5-04: the SINGLE fact-write path (source-trust lattice + log-odds
	// confidence + retraction + evidence-gated entity merge/split) -
	// shares taintTracker above (the SAME W4-03 clean/tainted read
	// factengine.Engine's ProvenanceUserAsserted gate consults) and st
	// itself as the append-only events ledger. Wired into both the
	// nightly hot-window promotion (HotWindow below) and the CLI-facing
	// /v1/fact*, /v1/entity* routes (SetFactEngine).
	factEngine := factengine.New(st.Queries, taintTracker, st)
	srv.SetFactEngine(factEngine)

	// W5-03: the "hatirladi ani" (remembered-moment) marking flow's single
	// write path - shared by POST /v1/remembered (`kahya remembered
	// --trace <id>`, channel=local) and the Telegram "🌟 Hatırladı" button
	// (channel=remote, called in-process below), never a second
	// implementation. st.Queries satisfies remembered.Store (the
	// trace-exists read), st itself satisfies remembered.Ledger (the
	// idempotent write, gated by migrations/0013's own partial unique
	// index).
	rememberedMarker := remembered.New(st.Queries, st, log)
	srv.SetRememberedMarker(rememberedMarker)
	tgBot.SetRememberedMarker(rememberedMarker)

	// W5-03: the weekly truth ritual (HANDOFF §6 W5 ⚑). The sampler
	// (select.go's fail-closed secret-lane exclusion policy) shares the
	// SAME pol.SecretLaneGlobs/cfg.MemoryDir every other secret-lane-aware
	// subsystem above uses (briefing/consolidation); the engine shares
	// factEngine/taintTracker/st (the run's own clean evidence-write
	// session, registered once per run) and delivers through tgBot -
	// SetRitualAnswerer wires the SAME tgBot's ritual-answer buttons back
	// into this exact Engine, so a Telegram tap and `kahya job run
	// truth-ritual` drive the identical code path.
	ritualSampler := ritual.NewSampler(st.Queries, cfg.MemoryDir, pol.SecretLaneGlobs, nil)
	ritualEngine := ritual.New(ritualSampler, st.Queries, factEngine, taintTracker, st, tgBot)
	tgBot.SetRitualAnswerer(ritualEngine)

	// W4-02: task durability state machine + receipts + resume scan +
	// outbox dispatcher (HANDOFF §6 W4 ⚑). taskLive is the live-worker-PID
	// registry shared by the resume scan's LiveChecker, the outbox
	// dispatcher's own re-spawn bookkeeping, and GET /v1/task/status
	// (`kahya task show <id>`'s PID field - the W4-07 gate script kills
	// the worker via this exact PID); server.SetTaskDurability wires GET
	// /v1/task/status + POST /v1/task/resolve.
	taskMachine := task.NewMachine(st.Queries, st)
	// Every transition is also logged as JSONL (HANDOFF §4 ⚑: "her satir
	// trace_id iceren JSONL"), alongside the DB ledger row Machine already
	// writes - the task spec's own grep-test acceptance criterion needs
	// both to exist and agree.
	taskMachine.SetJSONLLogger(log)
	taskResolver := task.NewResolver(st.Queries, st.Queries, taskMachine)
	taskLive := task.NewLiveRegistry()
	srv.SetTaskDurability(taskResolver, st.Queries, taskLive)
	srv.SetTaskLiveRegistry(taskLive)

	// W4-07 acceptance gate: the dev-only w2_slow_stub MCP tool
	// (kahyad/internal/server/devstub.go) is the ONLY thing that drives the
	// REAL kahyad/internal/task.Receipts intent->executing->receipt
	// lifecycle end to end - fs_write/shell_docker/applescript_run do not
	// (yet) call Receipts.Execute at all (see receipts.go's own doc comment
	// history; the memory note "kahya-w4-receipt-gap" tracks closing that
	// gap for the REAL tools separately, before W78-06 dogfood). Registered
	// onto the shared MCP server ONLY under KAHYA_ENV=dev - never
	// reachable in production regardless of what a (also dev-only)
	// policy.yaml overlay declares the tool as.
	if cfg.Env == config.EnvDev {
		taskReceipts := task.NewReceipts(st.DB(), st.Queries, st)
		srv.SetDevStub(taskReceipts)
	}

	// W4-04: cloud-call error taxonomy - task-side park/give-up/fail-
	// immediately decision (kahyad/internal/task.CloudRetry), driven by
	// kahyad/internal/anthproxy's inline retry loop via
	// server.Server.NewTaskProxy's OnCloudUnreachable/OnNonRetryableFailure
	// callbacks (task.go's own doc comment). config.validateCloudRetry
	// already guarantees every cfg.CloudRetryTaskSchedule entry and
	// cfg.CloudRetryGiveUpAfter parse as valid durations at Load() time, so
	// the two parse errors below truly cannot fire in production - they
	// exist only so a future caller of task.NewCloudRetry directly (e.g. a
	// test) is never silently handed a mis-parsed zero-value schedule.
	cloudRetrySchedule := make([]time.Duration, 0, len(cfg.CloudRetryTaskSchedule))
	for _, s := range cfg.CloudRetryTaskSchedule {
		d, err := time.ParseDuration(s)
		if err != nil {
			log.Error("cloud_retry_task_schedule_invalid", "entry", s, "err", err.Error())
			return 1
		}
		cloudRetrySchedule = append(cloudRetrySchedule, d)
	}
	cloudRetryGiveUpAfter, err := time.ParseDuration(cfg.CloudRetryGiveUpAfter)
	if err != nil {
		log.Error("cloud_retry_give_up_after_invalid", "value", cfg.CloudRetryGiveUpAfter, "err", err.Error())
		return 1
	}
	taskCloudRetry := task.NewCloudRetry(st.Queries, st.Queries, taskMachine, st, notifier, cloudRetrySchedule, cloudRetryGiveUpAfter)
	srv.SetTaskCloudRetry(taskMachine, taskCloudRetry)

	// POST /v1/task's per-task Anthropic forward-proxy + cost governor
	// (W12-08). notifier (built above, alongside the egress gate)
	// logs+ledgers alarms; governorNotifier additionally fans every ALARM-
	// class event out to Telegram (W3-07: task-paused-at-ceiling, 80%
	// daily-budget downgrade, cache-hit degradation, daily spend) when
	// tgBot is enabled, degrading to notifier's own JSONL+ledger-only
	// behavior otherwise (telegram.NewAlarmNotifier's own doc comment).
	// governor is rebuilt once here from every historical model_call
	// ledger event, then shared across every task for the rest of the
	// process's life (kahyad/internal/anthproxy.Governor is safe for
	// concurrent use). credential is selected by cfg.credential_mode -
	// see kahyad/internal/anthproxy's package doc comment for the OWNER
	// AUTH DECISION this selects between (passthrough is the owner-
	// decision default; keychain remains fully implemented as a valid
	// fallback).
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
	governorNotifier := telegram.NewAlarmNotifier(notifier, tgBot)
	governor := anthproxy.Boot(bootEvents, limits, nil, governorNotifier)
	log.Info("anthproxy_governor_booted", "events_replayed", len(bootEvents), "credential_mode", cfg.CredentialMode)

	credential := buildCredentialSource(cfg, log)
	// W3-05: the Anthropic forward-proxy becomes a CLIENT of egressGate -
	// every /v1/messages call this task's worker makes is gated on
	// host=<AnthropicUpstreamURL's own host:port>, nbytes=the request's
	// Content-Length, session keyed on trace_id (BLOCKER B/C fix — see
	// egress.NewAnthproxyEgressGateHook's own doc comment for why
	// SessionID must be the task's trace_id, never left empty) BEFORE
	// anthproxy ever forwards it (HANDOFF §4: "model-cagrisi egress kapisi
	// bu proxy noktasinda uygulanir"). A sensitive-read-tainted session
	// cannot even reach the upstream host if it were ever off-allowlist;
	// the stronger "no secret-lane byte to cloud" content rule is W3-08's
	// classifier + backstop, wired below.
	anthropicHost, anthropicPort := hostPortFromURL(cfg.AnthropicUpstreamURL)
	egressGateFactory := egress.NewAnthproxyEgressGateHook(egressGate, anthropicHost, anthropicPort)

	// W3-08: the local Qwen3-30B-A3B secret-lane server - kahyad-supervised
	// (mlxsup, REUSED verbatim, never a second supervisor implementation),
	// spawned lazily on the FIRST request that needs it (HANDOFF §6 timing
	// note, same "not at boot" posture as the W12-11 embed service above),
	// fail-closed on insufficient free memory (kahyad/internal/mlx.
	// HasSufficientMemory), idle-TTL unloaded after cfg.qwen_idle_ttl_seconds
	// with zero in-flight requests. qwenDir is the mlx/qwen directory the
	// service's own venv lives under - derived from cfg.QwenCmd the exact
	// same way embedServiceDir derives mlx/embed's, two lines above.
	qwenArgv := append(append([]string{}, cfg.QwenCmd...),
		"--model", cfg.QwenModelPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(cfg.QwenPort),
	)
	qwenSup := mlx.New(mlx.Config{
		Cmd:     qwenArgv,
		Dir:     embedServiceDir(cfg.QwenCmd),
		Host:    "127.0.0.1",
		Port:    cfg.QwenPort,
		IdleTTL: time.Duration(cfg.QwenIdleTTLSeconds) * time.Second,
		OnUnloaded: func() {
			if err := st.LogEvent(context.Background(), bootTraceID, "mlx_unloaded", map[string]any{"name": "qwen"}); err != nil {
				log.Warn("mlx_unloaded_ledger_error", "err", err.Error())
			}
		},
		Log: log,
	})
	secretLaneClassifier := secretlane.NewClassifier(mlx.NewQwenClassifierAdapter(qwenSup, cfg.QwenModelName))
	secretLaneAnswerer := mlx.NewAnswererAdapter(qwenSup, cfg.QwenModelName)
	markSensitiveRead := func(ctx context.Context, sessionKey, traceID string) error {
		return egressGate.MarkSensitiveRead(ctx, sessionKey, traceID)
	}
	srv.SetSecretLane(secretLaneClassifier, secretLaneAnswerer, markSensitiveRead)

	// W3-08 proxy backstop: the W12-08 chokepoint's SECOND, independent
	// enforcement layer (the FIRST is that a secret-lane task never spawns
	// a worker at all - task.go's own handleSecretLaneTask branch). Even if
	// that first layer were ever bypassed (a future change routes a
	// secret-lane task through the worker after all), this consults the
	// tasks-row lane by task_id on EVERY forwarded request and refuses with
	// 403 + ledger secretlane_cloud_blocked - "gizli-şerit içerik bulut
	// çağrısına çıkamıyor" (§6 W3 gate). Composed with (runs BEFORE) the
	// existing allowlist/budget/sensitive-read gate above - one combined
	// factory, still exactly one anthEgressGateFactory field.
	secretLaneLookup := server.NewSecretLaneStoreAdapter(st.Queries)
	secretLaneBackstopFactory := secretlane.NewProxyBackstopHook(secretLaneLookup, st)
	combinedEgressGateFactory := func(taskID, traceID string) func(*http.Request) error {
		secretHook := secretLaneBackstopFactory(taskID, traceID)
		allowlistHook := egressGateFactory(taskID, traceID)
		return func(r *http.Request) error {
			if err := secretHook(r); err != nil {
				return err
			}
			return allowlistHook(r)
		}
	}
	srv.SetAnthproxy(governor, governorNotifier, credential, combinedEgressGateFactory)

	// W3-05: the egress proxy itself - the ONLY route out of the
	// kahya-egress Docker network mcp/shell's needs_network container
	// jobs attach to (HANDOFF §5 safety #1 flag: "aksi halde container
	// ici curl allowlist'i atlar"). Listens on 127.0.0.1:<cfg.EgressPort>
	// for the whole daemon's lifetime (unlike the per-task anthproxy
	// listener above, this one is shared by every container job).
	egressProxy := egress.NewProxy(egressGate, cfg.EgressPort)
	// BLOCKER B/C: the SAME token->trace_id registry mcp/shell.Runner
	// registers into (via server.NewEgressTokenRegistrar above) is what
	// this listener consults to attribute each inbound connection's
	// Proxy-Authorization credential to a task's trace_id.
	egressProxy.Tokens = egressTokens
	if err := egressProxy.Start(); err != nil {
		log.Error("egress_proxy_start_failed", "port", cfg.EgressPort, "err", err.Error())
		return 1
	}
	defer egressProxy.Close()
	log.Info("egress_proxy_started", "addr", egressProxy.Addr)

	// W4-06: idempotent Time Machine setup (task spec step 3) - exclude
	// the live WAL brain.db (so Time Machine only ever backs up the
	// VACUUM snapshot below) and detect a missing backup destination.
	// Bounded by tmCtx (10s) and NEVER allowed to fail/block boot: any
	// tmutil error is logged by TimeMachine itself and otherwise ignored.
	// governorNotifier (not the bare `notifier`) is reused here so the
	// no-offsite alarm reaches Telegram exactly like a cost-governor
	// alarm does - HANDOFF §6 backup ⚑'s "sıfır veri-kaybı" prerequisite
	// is exactly the kind of risk that must not wait for the user to
	// notice a JSONL line. Skipped entirely outside cfg.Env==prod - the
	// SAME "dev is a fully sandboxed profile" posture buildCredentialSource/
	// buildTelegramTokenSource already apply to secrets above: a
	// KAHYA_ENV=dev process (every hermetic gate test under tests/e2e,
	// tests/w3, the eventual W78-02 red-team profile) must never shell out
	// to the real /usr/bin/tmutil or touch the real machine's Time Machine
	// exclusion list/alarm channel (HARD CONSTRAINT: no real tmutil call
	// in any code path that runs during `make test`).
	if cfg.Env == config.EnvProd {
		tmCtx, tmCancel := context.WithTimeout(context.Background(), 10*time.Second)
		tm := backup.NewTimeMachine(backup.NewExecTMRunner(), governorNotifier, backup.RealClock{}, log)
		tm.EnsureExclusions(tmCtx, cfg.DBPath, cfg.BackupDir)
		tm.CheckOffsite(tmCtx, bootTraceID)
		tmCancel()
	}

	// W4-01: the two-tier scheduler - job registry (scheduler.New itself
	// registers the built-in "smoke" handler) + trigger dispatch, wired to
	// POST /jobs/trigger/{name} (kahyad/internal/server/jobs.go). LoadJobs
	// resolves cfg.Jobs against every handler registered by this point in
	// boot.
	sched := scheduler.New(st, log)

	// W4-06: the backup-nightly (VACUUM INTO + verify + prune) and
	// memory-push (`git -C ~/Kahya push`) job handlers - registered
	// BEFORE LoadJobs below so cfg.Jobs' own default backup-nightly
	// (03:30)/memory-push (03:45) entries (config.defaults' doc comment)
	// resolve to real handlers on the very first boot, not just after a
	// later `kahyad -sync-jobs`/restart.
	snapshotter := backup.NewSnapshotter(st, governorNotifier, cfg.BackupDir)
	pusher := backup.NewPusher(backup.NewExecGitRunner(), governorNotifier, cfg.KahyaDir)
	sched.RegisterHandler("backup-nightly", func(ctx context.Context) error {
		return snapshotter.Run(ctx, scheduler.TraceIDFromContext(ctx))
	})
	sched.RegisterHandler("memory-push", func(ctx context.Context) error {
		return pusher.Run(ctx, scheduler.TraceIDFromContext(ctx))
	})

	// W5-01: the 08:30 morning briefing (HANDOFF §4 stack ⚑ scheduling /
	// §5 safety #2: the briefing session is untrusted by design - it can
	// only ever notify, never execute a W-class tool). Reuses the SAME
	// secretLaneClassifier/taintTracker/egressGate/tgBot/st instances
	// every other subsystem shares above - never a second copy of any of
	// them. srv.NewTaskProxy (the exact same per-task Anthropic forward-
	// proxy construction handleTask/taskDispatcher both already use) mints
	// a fresh listener for each briefing run's own worker call, so the
	// briefing model call passes through the identical W12-08 cost-
	// governor/egress-gate/cache-hit machinery every other task's call
	// does.
	briefingOrchestrator := &briefing.Orchestrator{
		Cfg: briefing.Config{
			GHRepos:       cfg.BriefingGHRepos,
			FileGlobs:     cfg.BriefingFileGlobs,
			CalendarNames: cfg.BriefingCalendarNames,
		},
		Classifier: secretLaneClassifier,
		Globs:      briefing.PolicyGlobMatcher{Globs: pol.SecretLaneGlobs},
		GH: briefing.GHCollector{
			Runner: briefing.ExecGHRunner{HTTPSProxy: fmt.Sprintf("http://127.0.0.1:%d", cfg.EgressPort)},
			Repos:  cfg.BriefingGHRepos,
		},
		Calendar:  briefing.ExecCalendarRunner{},
		Files:     briefing.FileGlobCollector{Globs: cfg.BriefingFileGlobs},
		FileState: briefing.FileScanState{Path: filepath.Join(cfg.DataDir, "briefing_file_scan_state.txt")},
		Taint:     taintTracker,
		Spawner: briefing.ProcessSpawner{
			Cmd: cfg.WorkerCmd, Socket: cfg.Socket, LogDir: cfg.LogDir,
			MCPBridgePath: cfg.MCPBridgePath, CredentialMode: cfg.CredentialMode,
			ProxyOpener: func(taskID, traceID string) (string, string, func() error, error) {
				return srv.NewTaskProxy(taskID, traceID)
			},
		},
		// W6-01: fans out to both Telegram (remote) and Hammerspoon (local) - see localDelivery's own doc comment above.
		Delivery:  localDelivery,
		TaskStore: st.Queries,
		Ledger:    st,
		Dedupe:    briefing.StoreDedupeChecker{Store: st.Queries},
		Log:       log,
	}
	sched.RegisterHandler("morning-briefing", func(ctx context.Context) error {
		_, err := briefingOrchestrator.Run(ctx, scheduler.TraceIDFromContext(ctx))
		return err
	})

	// W5-02: the 03:00 nightly consolidation (HANDOFF §6 W5 ⚑ + §5 memory
	// #4). Cloud lane reuses the SAME toolless Reader-mode worker profile/
	// per-task forward-proxy construction as the briefing orchestrator
	// above (srv.NewTaskProxy); local (secret) lane calls the SAME
	// kahyad-supervised Qwen3-30B-A3B server (qwenSup) directly over HTTP,
	// never through the worker (HANDOFF §4 ⚑: "makineden cikmaz"). pusher
	// is the EXACT SAME W4-06 memory-push handler already registered
	// above - consolidation's own approve step invokes it again at the
	// end of a successful merge (task spec step 8), never a second git-
	// push implementation.
	consolidator := &consolidation.Consolidator{
		Cfg: consolidation.Config{
			KahyaDir:        cfg.KahyaDir,
			MemoryDir:       cfg.MemoryDir,
			SecretLaneGlobs: pol.SecretLaneGlobs,
			AutoCommit:      cfg.ConsolidationAutoCommit,
		},
		Git: backup.NewExecGitRunner(),
		Cloud: consolidation.CloudSession{Spawner: consolidation.ProcessSpawner{
			Cmd: cfg.WorkerCmd, Socket: cfg.Socket, LogDir: cfg.LogDir,
			MCPBridgePath: cfg.MCPBridgePath, CredentialMode: cfg.CredentialMode,
			ProxyOpener: func(taskID, traceID string) (string, string, func() error, error) {
				return srv.NewTaskProxy(taskID, traceID)
			},
		}},
		Local: consolidation.LocalSession{Sup: qwenSup, Model: cfg.QwenModelName},
		// W6-01: fans out to both Telegram (remote) and Hammerspoon (local) - see localDelivery's own doc comment above.
		Notifier:    localDelivery,
		EventLogger: st,
		EventReader: consolidation.StoreEventReader{Q: st.Queries},
		Reindexer:   reindexBackfiller,
		Pusher:      pusher,
		HotWindow:   consolidation.StoreFactWriter{Q: st.Queries, Engine: factEngine},
		Log:         log,
	}
	sched.RegisterHandler("nightly-consolidation", func(ctx context.Context) error {
		return consolidator.Run(ctx, scheduler.TraceIDFromContext(ctx))
	})
	srv.SetConsolidation(consolidator)

	// W5-05: `kahya eval mini` (/v1/eval/mini/run) - the ~20-question
	// retrieval mini-baseline. Searcher is adapted from the SAME searcher
	// value /v1/memory/search itself calls (three lines, converting
	// search.Hit -> eval.Hit - kahyad/internal/eval deliberately has no
	// compile-time dependency on kahyad/internal/search, see eval/
	// runner.go's own doc comment); EventLogger/EventReader are the SAME
	// events-ledger seam consolidation's own Consolidator uses above.
	evalRunner := &eval.Runner{
		BaselinePath: cfg.EvalMiniBaselinePath,
		Searcher: eval.SearcherFunc(func(ctx context.Context, traceID, query string, k int) ([]eval.Hit, error) {
			hits, err := searcher.Search(ctx, traceID, query, k)
			if err != nil {
				return nil, err
			}
			out := make([]eval.Hit, len(hits))
			for i, h := range hits {
				out[i] = eval.Hit{Path: h.Path, Text: h.Text}
			}
			return out, nil
		}),
		EventLogger: st,
		EventReader: eval.StoreEventReader{Q: st.Queries},
	}
	srv.SetEvalMiniRunner(evalRunner)

	// W5-03: the Sunday 18:00 weekly truth ritual - `kahya job run
	// truth-ritual` (manual trigger) and the launchd-scheduled run both
	// reach this SAME handler via scheduler.Trigger's one dispatch route
	// (jobs.go's own doc comment), exactly like every other job here.
	sched.RegisterHandler("truth-ritual", func(ctx context.Context) error {
		_, err := ritualEngine.Run(ctx, scheduler.TraceIDFromContext(ctx))
		return err
	})

	sched.LoadJobs(cfg.Jobs)
	srv.SetScheduler(sched)

	// W4-02: the resume scan (kahyad/internal/task.Resume) and the outbox
	// redelivery dispatcher (kahyad/internal/outbox.Dispatcher) share the
	// SAME taskMachine/taskLive built above. dispatcherSpawnCfg mirrors
	// POST /v1/task's own spawn.Config fields (WorkerCmd/Socket/LogDir/
	// MCPBridgePath/CredentialMode); AnthropicBaseURL/APIKey are left unset
	// on this shared base value - each redispatch instead gets its OWN
	// fresh pair from SetAnthproxyOpener below (W4-04's fix for the gap
	// this comment used to describe as "intentionally deferred").
	dispatcherSpawnCfg := spawn.Config{
		Cmd: cfg.WorkerCmd, Socket: cfg.Socket, LogDir: cfg.LogDir,
		MCPBridgePath: cfg.MCPBridgePath, CredentialMode: cfg.CredentialMode,
		TmpDir: cfg.TmpDir(),
	}
	taskResume := task.NewResume(st.Queries, st.Queries, st.Queries, taskMachine, notifier, taskLive, cfg.TaskRetryW1MaxAuto)
	taskDispatcher := outbox.NewDispatcher(st.Queries, st, taskMachine, dispatcherSpawnCfg, taskLive)
	// W4-04: a redispatched cloud-lane task needs its OWN fresh Anthropic
	// forward-proxy listener to make any model call at all - srv.
	// NewTaskProxy is the EXACT SAME construction handleTask uses at first
	// spawn (kahyad/internal/server/task.go's own doc comment), so the two
	// call sites can never drift apart.
	taskDispatcher.SetAnthproxyOpener(func(_ context.Context, taskID, traceID string) (string, string, func() error, error) {
		return srv.NewTaskProxy(taskID, traceID)
	})

	runResumeScan := func(ctx context.Context) {
		if _, err := taskResume.Scan(ctx); err != nil {
			log.Error("task_resume_scan_failed", "err", err.Error())
		}
	}
	runOutboxDispatch := func(ctx context.Context) {
		if _, err := taskDispatcher.ClaimAndDispatch(ctx); err != nil {
			log.Error("outbox_dispatch_failed", "err", err.Error())
		}
	}
	// Startup scan: every task.go's own comment on "no live worker PID"
	// applies in its strongest form here - taskLive is still EMPTY (no
	// spawn has registered into it yet this process), so every
	// tasks.status='executing' row left over from a previous kahyad
	// process (crash, SIGKILL, machine restart) is correctly treated as
	// interrupted.
	runResumeScan(context.Background())
	// W4-07: the tick intervals themselves are now cfg-driven (default 30s/
	// 5s, unchanged from the literal specs this replaced) so a hermetic
	// acceptance gate (tests/acceptance/w4, scripts/accept_w4.sh) can
	// override them down to a couple of seconds via config.yaml, instead of
	// a CI-speed run having to wait out the full production cadence for the
	// resume scan/outbox dispatcher to notice a crashed/killed worker.
	if err := sched.RegisterTick("task_resume_scan", fmt.Sprintf("@every %ds", cfg.ResumeScanIntervalSeconds), runResumeScan); err != nil {
		log.Error("task_resume_scan_tick_register_failed", "err", err.Error())
	}
	if err := sched.RegisterTick("outbox_dispatch", fmt.Sprintf("@every %ds", cfg.OutboxDispatchIntervalSeconds), runOutboxDispatch); err != nil {
		log.Error("outbox_dispatch_tick_register_failed", "err", err.Error())
	}

	// W4-05: the ledger external anchor (HANDOFF §5 safety #4 flag). Both
	// Pusher and Verifier are ALWAYS constructed - the ONLY gate on any
	// real git/SSH activity is cfg.AnchorRemote being non-empty
	// (anchor.Pusher.Run's own doc comment), mirroring how the W4-06
	// TimeMachine setup above gates its own real side effects so dev/test
	// never touches a real remote. anchorRepoDir is the task spec's own
	// fixed local working tree (~/Library/Application Support/Kahya/
	// anchor-repo). anchor.NewPusher/NewVerifier each read the real
	// kahya.anchor Keychain deploy key internally - this package never
	// touches that identity directly, per HANDOFF §5 safety #4's
	// Keychain-isolation rule (kahyad/internal/anchor's own import-guard
	// test enforces it permanently). governorNotifier
	// (not the bare `notifier`) is reused here so a stale-pending/mismatch
	// alarm reaches Telegram exactly like every other alarm-class event
	// does.
	anchorRepoDir := filepath.Join(cfg.DataDir, "anchor-repo")
	anchorGitRunner := anchor.NewExecGitRunner()
	warnAnchorKeyOverrideIgnored := func() {
		log.Warn("anchor_key_override_ignored", "reason", "KAHYA_ANCHOR_KEY_OVERRIDE set outside KAHYA_ENV=dev")
	}
	anchorPusher := anchor.NewPusher(st.Queries, st, governorNotifier, anchorGitRunner, cfg.AnchorRemote, anchorRepoDir, cfg.AnchorLocalFallbackPath, cfg.AnchorIntervalHours, cfg.Env, warnAnchorKeyOverrideIgnored)
	anchorPusher.SetJSONLLogger(log)
	anchorVerifier := anchor.NewVerifier(st.Queries, st, governorNotifier, anchorGitRunner, cfg.AnchorRemote, anchorRepoDir, cfg.Env, warnAnchorKeyOverrideIgnored)
	anchorVerifier.SetJSONLLogger(log)
	srv.SetLedgerVerifier(anchorVerifier)

	// Startup anchor push (task spec step 3: "once at startup ... plus
	// once at graceful shutdown" - the shutdown half runs further down,
	// right before the embed/qwen supervisors stop).
	if err := anchorPusher.Run(context.Background(), bootTraceID); err != nil {
		log.Error("anchor_push_startup_failed", "err", err.Error())
	}
	if err := sched.RegisterTick("anchor_push", fmt.Sprintf("@every %dh", cfg.AnchorIntervalHours), func(ctx context.Context) {
		if err := anchorPusher.Run(ctx, scheduler.TraceIDFromContext(ctx)); err != nil {
			log.Error("anchor_push_tick_failed", "err", err.Error())
		}
	}); err != nil {
		log.Error("anchor_push_tick_register_failed", "err", err.Error())
	}

	sched.StartTicks()
	defer sched.StopTicks()

	// Startup sync (task spec step 5): keep ~/Library/LaunchAgents' set of
	// com.kahya.job.<name> plists in exact sync with cfg.Jobs on every
	// normal boot too - not only via the standalone `kahyad -sync-jobs`
	// flag (runSyncJobs below, for scripting/idempotent re-sync without a
	// full daemon restart). Best-effort: a launchctl hiccup here must
	// never prevent the rest of the daemon from serving.
	schedSyncOpts := scheduler.SyncOptions{
		LaunchAgentsDir: filepath.Join(home, "Library", "LaunchAgents"),
		JobLogDir:       filepath.Join(home, "Library", "Logs", "Kahya"),
		TriggerBinPath:  cfg.TriggerBinPath,
	}
	if err := scheduler.Sync(cfg.Jobs, schedSyncOpts, scheduler.NewExecRunner(), log); err != nil {
		log.Error("job_sync_failed", "err", err.Error())
	}

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
		// idx.Reindex (inside reindexBackfiller.Reindex) logs
		// event=reindex_done (or event=reindex_cancelled if ctx was
		// cancelled mid-run) itself, scoped to bootTraceID since it is
		// passed in non-empty here - see
		// kahyad/internal/indexer.Indexer.Reindex's doc comment - so there
		// is nothing left to log a second time on success. full=false,
		// reEmbed=false: an ordinary boot-time incremental reindex, not a
		// forced full rechunk or a version-switch re-embed.
		if _, err := reindexBackfiller.Reindex(ctx, bootTraceID, false, false); err != nil {
			log.With(bootTraceID).Error("reindex_failed", "err", err.Error())
		}
	}()

	// W3-02: sweep expired undo_windows rows (deadline passed, still
	// state="open" -> "expired" + ledger undo_window_expired) every 30s,
	// for as long as kahyad runs. Same ctx-cancelled-then-joined pattern as
	// the boot reindex goroutine above (BLOCKER 2's reasoning applies
	// identically: a sweep must never race st.Close() on shutdown).
	var undoSweepDone sync.WaitGroup
	undoSweepDone.Add(1)
	go func() {
		defer undoSweepDone.Done()
		policyEngine.RunUndoSweeper(ctx, 30*time.Second)
	}()

	// W3-07: the Telegram bot's long-polling loop - Start blocks internally
	// until ctx is cancelled (it stops its own poller from a ctx.Done()
	// goroutine), a no-op on a disabled bot. Same ctx-cancelled-then-joined
	// pattern as the boot reindex goroutine/undo sweeper above.
	var telegramDone sync.WaitGroup
	telegramDone.Add(1)
	go func() {
		defer telegramDone.Done()
		tgBot.Start(ctx)
	}()

	runErr := srv.Run(ctx)

	// Cancel ctx explicitly (a no-op if a shutdown signal already fired
	// it) and WAIT for the boot reindex goroutine (and the undo-window
	// sweeper, and the Telegram bot's long-poll loop) to observe the
	// cancellation and finish any in-flight work, before this function
	// returns - so this always completes strictly before the deferred
	// st.Close() call above (BLOCKER 2).
	stop()
	reindexDone.Wait()
	undoSweepDone.Wait()
	telegramDone.Wait()

	// W4-05: one last anchor push at graceful shutdown (task spec step 3's
	// other half - see the startup call above). Deliberately AFTER every
	// other goroutine above has been joined (so it observes the ledger's
	// truly final state for this process) but BEFORE the deferred
	// st.Close() runs (brain.db is still open here).
	if err := anchorPusher.Run(context.Background(), traceid.New()); err != nil {
		log.Error("anchor_push_shutdown_failed", "err", err.Error())
	}

	// Stop kills the embed service's entire process GROUP (SIGKILL) and
	// suppresses its restart-with-backoff loop - launchd holds only
	// kahyad itself (HANDOFF §4 ⚑), so kahyad shutting down must never
	// leave an orphaned mlx/embed/server.py behind. A no-op if it was
	// never lazily started (StateDisabled/StateDown).
	embedSup.Stop()
	// Same shutdown contract for the W3-08 secret-lane Qwen3-30B-A3B
	// server - also stops its idle-TTL monitor goroutine.
	qwenSup.Stop()

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

// embedServiceDir derives the working directory the embed service child
// process should run with: the directory containing cmd's own script
// path (cmd[1], e.g. ".../mlx/embed/server.py" -> ".../mlx/embed"). Empty
// cmd (or a single-element cmd - should not happen with
// config.defaultEmbedCmd or any sane override) leaves the child running
// with kahyad's own cwd instead (mlxsup.Config.Dir's documented zero-value
// behavior) - a harmless fallback, since the embed service locates its
// model purely via the user-home-based HF cache, never via a relative
// path of its own.
func embedServiceDir(cmd []string) string {
	if len(cmd) < 2 {
		return ""
	}
	return filepath.Dir(cmd[1])
}

// hostPortFromURL resolves rawURL's host and port (defaulting to 80/443
// per scheme when no explicit port is present) - used to derive the
// egress.Target the W3-05 gate checks cfg.AnthropicUpstreamURL's OWN
// host:port against (e.g. "https://api.anthropic.com" ->
// ("api.anthropic.com", 443)). An unparsable rawURL (should not happen
// with a validated config.Config) returns ("", 0) - Check's own
// CanonicalizeHost then rejects the empty host, and the gate's ordinary
// fail-closed "not in allowlist" denial applies, never a permissive
// fallback.
func hostPortFromURL(rawURL string) (host string, port int) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "", 0
	}
	host = u.Hostname()
	portStr := u.Port()
	if portStr == "" {
		if u.Scheme == "http" {
			portStr = "80"
		} else {
			portStr = "443"
		}
	}
	port, _ = strconv.Atoi(portStr)
	return host, port
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

// telegramTokenOverrideEnvVar is KAHYA_TELEGRAM_TOKEN_OVERRIDE (W3-10
// gate-testing seam only, mirroring KAHYA_ANTHROPIC_KEY_OVERRIDE's exact
// posture — kahyad/internal/anthproxy's keychainSource.UpstreamAuth):
// substitutes a real kahya.telegram Keychain read ONLY when cfg.Env=="dev",
// so a hermetic child-kahyad gate test never needs a real BotFather token
// or Keychain item. Ignored — loudly — outside dev, exactly like the
// Anthropic key override.
const telegramTokenOverrideEnvVar = "KAHYA_TELEGRAM_TOKEN_OVERRIDE"

// devTelegramTokenSource wraps a real telegram.TokenReader (secrets.
// NewTelegram()) with the dev-only override above.
type devTelegramTokenSource struct {
	real telegram.TokenReader
	env  string
	log  *logx.Logger
}

func (s devTelegramTokenSource) Read() (string, error) {
	if override := os.Getenv(telegramTokenOverrideEnvVar); override != "" {
		if s.env == config.EnvDev {
			return override, nil
		}
		if s.log != nil {
			s.log.Warn("telegram_token_override_ignored", "reason", "KAHYA_TELEGRAM_TOKEN_OVERRIDE set outside KAHYA_ENV=dev")
		}
	}
	return s.real.Read()
}

// buildTelegramTokenSource selects the telegram.TokenReader tgBot reads its
// BotFather token from: the real kahya.telegram Keychain item, with the
// dev-only KAHYA_TELEGRAM_TOKEN_OVERRIDE substitution available only under
// cfg.Env=="dev" (W3-10's hermetic gate tests — no real Keychain item or
// live token ever required to exercise the Telegram bot's wiring).
func buildTelegramTokenSource(cfg config.Config, log *logx.Logger) telegram.TokenReader {
	return devTelegramTokenSource{real: secrets.NewTelegram(), env: cfg.Env, log: log}
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
