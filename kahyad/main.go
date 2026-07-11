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

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/buildinfo"
	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/egress"
	"kahya/kahyad/internal/embed"
	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/mlx"
	"kahya/kahyad/internal/mlxsup"
	"kahya/kahyad/internal/notify"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/secrets"
	"kahya/kahyad/internal/server"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/telegram"
	"kahya/kahyad/internal/traceid"
	mcpfs "kahya/mcp/fs"
	mcposascript "kahya/mcp/osascript"
	mcpshell "kahya/mcp/shell"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

// dispatch handles kahyad's argv-based subcommands. Currently the only one
// is "policy validate" (W3-01 acceptance criterion); any other argv
// (including none at all) runs the daemon itself (run()), matching every
// prior release's behavior exactly - kahyad has never taken positional
// arguments before this task.
func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "policy" {
		return runPolicyCLI(args[1:], stdout, stderr)
	}
	return run()
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
	policyEngine.SetPendingApprovalHook(func(info policy.PendingApprovalInfo) {
		// Fired synchronously from inside Check/Approve's own request path
		// (kahyad/internal/policy.Engine.pendingApprovalHook's doc
		// comment) - the actual Telegram send happens in its own
		// goroutine so a slow/blocked send can never delay a policy
		// decision.
		go tgBot.OnPendingApproval(context.Background(), info)
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
