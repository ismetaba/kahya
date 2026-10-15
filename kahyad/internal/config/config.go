// Package config loads kahyad's typed configuration.
//
// Load order (HANDOFF §4 stack, W12-01 step 1): built-in defaults, then
// <data_dir>/config.yaml if present, then environment variable overrides.
// Every configured filesystem path is tilde-expanded and, per HANDOFF §7 ⚑
// ("Dizin adları ASCII"), must be pure ASCII — a non-ASCII rune anywhere in
// a path fails startup rather than risking silent NFC/NFD mismatches in
// SQLite/glob comparisons.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Env values accepted for KAHYA_ENV.
const (
	EnvProd = "prod"
	EnvDev  = "dev"
)

// CredentialMode values accepted for Config.CredentialMode (W12-08).
// kahyad/internal/anthproxy defines its own copy of these two literals
// (rather than importing this package) so that package stays free of a
// config.Config dependency; keep the two in sync by hand if either ever
// changes - config_test.go and anthproxy's own tests each pin the literal
// string value independently, so a drift would fail both suites' tests, not
// silently pass.
const (
	CredentialModeKeychain    = "keychain"
	CredentialModePassthrough = "passthrough"
)

// Config is kahyad's fully-resolved runtime configuration.
type Config struct {
	DataDir              string `yaml:"data_dir"`
	Socket               string `yaml:"socket"`
	LogDir               string `yaml:"log_dir"`
	DBPath               string `yaml:"db_path"`
	MemoryDir            string `yaml:"memory_dir"`
	AnthropicUpstreamURL string `yaml:"anthropic_upstream_url"`
	EmbedPort            int    `yaml:"embed_port"`
	DefaultModel         string `yaml:"default_model"`
	TaskTimeoutMin       int    `yaml:"task_timeout_min"`
	ActiveEmbedModelVer  string `yaml:"active_embed_model_ver"`

	// WorkerCmd is the per-task Python worker command (HANDOFF §4 IPC ⚑,
	// W12-07): argv[0] is the executable, the rest its args. Default
	// points at the repo-local venv (defaultWorkerCmd); W12-07's own tests
	// and the manual verification flow override it via config.yaml to
	// point at a fake worker script instead. The real worker
	// (`kahya_worker`, W12-09) does not exist until that task lands - this
	// config key exists now purely so kahyad/internal/spawn has somewhere
	// to read the command from.
	WorkerCmd []string `yaml:"worker_cmd"`

	// EmbedCmd is cfg.embed_cmd (W12-11): the local MLX embedding
	// service's argv, same derivation pattern as WorkerCmd - defaults to
	// the repo-local mlx/embed/.venv/bin/python running mlx/embed/
	// server.py (its OWN venv, separate from worker/'s - HANDOFF §4's
	// three-process architecture). kahyad/internal/mlxsup.Supervisor
	// spawns this lazily, on first embedding need, never at boot.
	EmbedCmd []string `yaml:"embed_cmd"`

	// MCPBridgePath is the absolute path to the kahya-mcp stdio<->UDS
	// bridge binary (W12-05: kahyad/cmd/kahya-mcp, built to
	// bin/kahya-mcp). W12-09's worker execs this as its "kahya_memory" MCP
	// server's stdio command - it learns the path via the KAHYA_MCP_BRIDGE
	// env var spawn.BuildEnv sets from this field (docs/ipc.md §3).
	// Default points at "<repo>/bin/kahya-mcp", derived the same way
	// defaultWorkerCmd derives the worker's own venv path - two
	// directories up from the running kahyad executable.
	MCPBridgePath string `yaml:"mcp_bridge_path"`

	// PolicyPath is the absolute path to policy.yaml (W3-01): kahyad's
	// main.go loads this at boot, BEFORE the UDS listener accepts any
	// /policy/check request, via kahyad/internal/policy.Load. Defaults to
	// "<repo>/policy.yaml", derived the same way MCPBridgePath/WorkerCmd/
	// EmbedCmd are - two directories up from the running kahyad
	// executable. Override via config.yaml's policy_path key or
	// KAHYA_POLICY_PATH (primarily so a manual verification run can point
	// kahyad at a deliberately broken fixture without touching the real,
	// committed repo-root policy.yaml).
	PolicyPath string `yaml:"policy_path"`

	// EvalMiniBaselinePath is the absolute path to eval/mini-baseline.jsonl
	// (W5-05): the ~20-question retrieval mini-baseline `kahya eval mini`
	// runs against memory_search. Defaults to "<repo>/eval/mini-baseline.jsonl",
	// derived the same way PolicyPath/MCPBridgePath/WorkerCmd/EmbedCmd are -
	// two directories up from the running kahyad executable. Override via
	// config.yaml's eval_mini_baseline_path key or KAHYA_EVAL_MINI_BASELINE_PATH.
	EvalMiniBaselinePath string `yaml:"eval_mini_baseline_path"`

	// EvalRetrievalDatasetPath is the absolute path to the W78-01 full
	// retrieval-QA dataset (~50 items) `kahya eval retrieval` runs against
	// memory_search. The REAL dataset lives in the PRIVATE ~/Kahya memory
	// repo (it is derived from the user's real memory + W5-03 ritual labels,
	// so it must never be committed to this code repo). Defaults to
	// "<KahyaDir>/eval/retrieval/dataset.jsonl". Override via config.yaml's
	// eval_retrieval_dataset_path key or KAHYA_EVAL_RETRIEVAL_DATASET_PATH.
	EvalRetrievalDatasetPath string `yaml:"eval_retrieval_dataset_path"`

	// SearchRecencyWeight / SearchRecencyHalfLifeDays (FIX-MEM-8) are the
	// memory search's recency weight w in [0,1] and its half-life in days
	// (kahyad/internal/search.Config.RecencyWeight/RecencyHalfLifeDays:
	// score' = (1-w)*fused + w*exp(-ln2*ageDays/halfLife)). Default 0 (off)
	// and 90. A weight > 0 is a fusion change (HANDOFF §5 memory #5): kahyad
	// activates it at boot ONLY through the retrieval-eval gate
	// (search.Searcher.ActivateFusionConfig) - without a green
	// `kahya eval retrieval` result for that exact config it logs
	// search_recency_not_activated, keeps recency off, and lets the next
	// eval run score the candidate too. Outside the bounds Load fails.
	SearchRecencyWeight       float64 `yaml:"search_recency_weight"`
	SearchRecencyHalfLifeDays int     `yaml:"search_recency_half_life_days"`

	// SecretLaneMemoryDir (FIX-U-3; G13 secret_lane_memory_dir) is the
	// memory subdirectory - relative to MemoryDir, forward-slash, ASCII -
	// where memory_write lands content its local classifier flagged
	// secret-lane (<dir>/notes/<requested path>; MEM-6 ingest later uses
	// <dir>/ingest/). A secret_lane_globs entry MUST cover it
	// (policy.yaml: ~/Kahya/memory/gizli/**): memory_write refuses a secret
	// note it cannot land under a covering glob. Default "gizli". Load
	// fails on an absolute path, a ".." component, a dot-directory (the
	// indexer and consolidation skip those) or a non-ASCII rune.
	SecretLaneMemoryDir string `yaml:"secret_lane_memory_dir"`

	// EgressPort is the W3-05 egress proxy's listen port
	// (127.0.0.1:<EgressPort>) - the single gate every off-box byte
	// passes through (HANDOFF S5 safety #1). Default 3128 (the
	// conventional squid/forward-proxy port); mcp/shell's kahya-egress-fwd
	// Docker sidecar forwards to host.docker.internal:<EgressPort>.
	EgressPort int `yaml:"egress_port"`

	// DockerImageTag is the W3-04 shell sandbox image's tag (e.g.
	// "kahya-sandbox:0.1.0") — the SAME tag `make sandbox-image` builds
	// and mcp/shell.Runner's DigestChecker inspects before every run.
	DockerImageTag string `yaml:"docker_image_tag"`
	// DockerImageDigestPath is the absolute path to docker/sandbox/
	// IMAGE_DIGEST (W3-04's committed supply-chain pin) — derived the same
	// way PolicyPath/MCPBridgePath/WorkerCmd/EmbedCmd are (two directories
	// up from the running kahyad executable). An empty/missing/
	// not-yet-built file resolves to mcp/shell.LoadPinnedDigest's ""
	// return, which is itself Runner's fail-closed state (every
	// shell_docker run refused until `make sandbox-image` has run).
	DockerImageDigestPath string `yaml:"docker_image_digest_path"`

	// EgressSidecarDigestPath is the absolute path to docker/egress/
	// IMAGE_DIGEST (W3-05's committed pin for the kahya-egress-fwd
	// sidecar's alpine/socat image), derived the same way
	// DockerImageDigestPath is. An empty/missing file resolves to
	// mcp/shell.LoadPinnedDigest's "" return, which is itself
	// EgressNetworkEnsurer's fail-closed state (every needs_network:true
	// shell_docker run refused until this is pinned).
	EgressSidecarDigestPath string `yaml:"egress_sidecar_digest_path"`

	// ShellWorkdirRoots is mcp/shell.Runner.WorkdirRoots (BLOCKER 1 fix's
	// OPTIONAL, stricter opt-in allowlist): when non-empty, a shell_docker
	// task's canonical workdir must be one of these roots or a descendant,
	// REPLACING the default deny-rule posture (mcp/shell.validateWorkdir
	// otherwise rejects "/", $HOME, an ancestor of $HOME, a system dir, or
	// a sensitive $HOME subtree, and allows everything else including the
	// OS temp dir and ordinary $HOME subdirectories). Default empty (nil)
	// - the deny-rule posture applies. Each entry is tilde-expanded against
	// home, same as every other configured path. Env override
	// KAHYA_SHELL_WORKDIR_ROOTS is a comma-separated list.
	ShellWorkdirRoots []string `yaml:"shell_workdir_roots"`

	// LogLevel is the process-wide minimum log level: debug|info|warn|error
	// (default "info"). main.go passes the resolved value to
	// kahyad/internal/logx.SetLevel before logx.New. Env override
	// KAHYA_LOG_LEVEL; an unrecognized value fails Load closed, the same as
	// an invalid KAHYA_ENV (MINOR 5).
	LogLevel string `yaml:"log_level"`

	// UndoWindowSeconds is the W1 undo grace period's length, in seconds
	// (HANDOFF S4 ladder: "L2 | Eslikci | R, W1 (5-dk geri-alma +
	// defter)" - default 300, i.e. 5 minutes). main.go threads this into
	// kahyad/internal/policy.Engine.SetUndoWindowDuration; tests inject a
	// much shorter value to exercise undo-window expiry (and the fs
	// tool's own pre-image purge it triggers) without a real 5-minute
	// wait.
	UndoWindowSeconds int `yaml:"undo_window_seconds"`

	// ApprovalWaitSeconds is config key `approval_wait_seconds` (B-1): how
	// long a tool-side binding gate (POST /v1/mcp tools/call ->
	// Engine.Check -> needs_approval) parks the in-flight tool call waiting
	// for the human decision before answering the model. 0 disables
	// waiting (the call fails at once with the pending id; the hermetic
	// gate suites use this). Default 300. Must be >= 0 and <=
	// MaxApprovalWaitSeconds (600, the pending approval's own TTL), and -
	// when written explicitly in config.yaml - less than task_timeout_min*60
	// (Load).
	ApprovalWaitSeconds int `yaml:"approval_wait_seconds"`
	// AutonomyBootstrapLevels is config key `autonomy_bootstrap_levels`
	// (B-1, owner decision G01-2 option A): "tool/class/scope" -> level
	// rows seeded into autonomy_state at boot ONLY when no row exists yet
	// (kahyad/internal/policy.Engine.BootstrapLevels - equivalent to the
	// user running `kahya autonomy promote` once per key on first boot,
	// ledgered as autonomy_bootstrapped; an existing row, a demotion to L0
	// included, is never overridden). Default
	// {"memory_search/R/global": 1} so the assistant can read its own
	// memory on a fresh install; set to {} to disable.
	AutonomyBootstrapLevels map[string]int `yaml:"autonomy_bootstrap_levels"`

	// --- W12-08 cost governor (HANDOFF S4 flag, verbatim defaults) ---

	// DailyBudgetUSD / MonthlyBudgetUSD are kahyad/internal/anthproxy's
	// budget-block thresholds ("gunluk butce $10 / aylik $150").
	DailyBudgetUSD   float64 `yaml:"daily_budget_usd"`
	MonthlyBudgetUSD float64 `yaml:"monthly_budget_usd"`
	// TaskTokenCeiling is the per-task token ceiling (input+output+
	// cache_creation) - "gorev-basina 500K token tavani".
	TaskTokenCeiling int64 `yaml:"task_token_ceiling"`
	// DowngradeAtRatio is the fraction of DailyBudgetUSD at which the
	// Opus->Sonnet downgrade rung flips (default 0.8, i.e. $8 of $10).
	DowngradeAtRatio float64 `yaml:"downgrade_at_ratio"`
	// CacheHitAlarmThreshold is the daily cache-hit-ratio floor below
	// which (once >=20 calls that day) an alarm fires.
	CacheHitAlarmThreshold float64 `yaml:"cache_hit_alarm_threshold"`
	// EstRequestTokens is the fail-closed fallback per-request token
	// estimate kahyad/internal/anthproxy.Governor.CheckBeforeForward
	// reserves against TaskTokenCeiling/DailyBudgetUSD/MonthlyBudgetUSD
	// BEFORE forwarding, whenever the request's own max_tokens/body size
	// cannot be parsed (BLOCKER 2 fix: closes a check-then-act TOCTOU that
	// let a burst of concurrent requests jointly exceed a hard cap) - see
	// docs/ipc.md's W12-08 note and anthproxy.estimateRequestLocked's doc
	// comment for the full estimation strategy.
	EstRequestTokens int64 `yaml:"est_request_tokens"`
	// CredentialMode selects how kahyad/internal/anthproxy authenticates
	// to cfg.AnthropicUpstreamURL: "keychain" (original HANDOFF design -
	// kahyad reads kahya.anthropic from the macOS Keychain and injects
	// it) or "passthrough" (OWNER AUTH DECISION default - see
	// kahyad/internal/anthproxy's package doc comment for the full
	// rationale: the worker authenticates via its own Claude Code login,
	// so kahyad injects no credential and forwards the worker CLI's own
	// Authorization header unchanged after validating the per-task local
	// token; FIX-I-1: that token rides X-Kahya-Task-Token and
	// ANTHROPIC_API_KEY stays unset in the worker, see spawn.BuildEnv).
	CredentialMode string `yaml:"credential_mode"`

	// --- W3-07 Telegram approval bot ---

	// TelegramChatID/TelegramUserID are the single fixed chat_id/user_id
	// allowlist pair the Telegram bot's Go-side middleware enforces
	// (HANDOFF §5 safety #5 ⚑: "tek sabit chat_id/user_id allowlist'i Go
	// tarafinda uygulanir"). Either being zero (the default - no
	// config.yaml key, no env override) disables the bot entirely: every
	// approval/alarm falls back to the local (CLI) surface, exactly as if
	// the kahya.telegram Keychain item were absent. Populated by the
	// one-time manual setup (task spec step 7): the user messages the bot
	// once, the operator reads that update's chat/from ids back into
	// config.yaml.
	TelegramChatID int64 `yaml:"telegram_chat_id"`
	TelegramUserID int64 `yaml:"telegram_user_id"`
	// TelegramAPIURL overrides telebot's own default Bot API base
	// ("https://api.telegram.org") — empty (the default) means "use
	// telebot's own default". W3-10 gate-testing seam ONLY (see
	// kahyad/internal/telegram.Config.APIURL's doc comment): a hermetic
	// test's own config.yaml/KAHYA_TELEGRAM_API_URL points this at a local
	// fake Telegram Bot API server; never set in a real deployment.
	TelegramAPIURL string `yaml:"telegram_api_url"`

	// --- W3-08 secret-lane local Qwen3-30B-A3B server ---

	// QwenCmd is the local secret-lane server's base invocation
	// (kahyad/internal/mlxsup.Supervisor reuses this - "REUSE it for the
	// Qwen server, do NOT fork a second supervisor" per the task spec):
	// ["<repo>/mlx/qwen/.venv/bin/python", "-m", "mlx_lm.server"]. Unlike
	// EmbedCmd (a complete invocation - mlx/embed/server.py reads its own
	// port from an env var), this default deliberately stops short of a
	// full command line: mlx_lm.server is a third-party CLI whose
	// --model/--host/--port are ordinary argv flags, not env vars, so
	// main.go appends those itself from QwenModelPath/QwenPort at wiring
	// time.
	QwenCmd []string `yaml:"qwen_cmd"`
	// QwenModelPath is the local filesystem path to the pinned
	// Qwen3-30B-A3B-4bit snapshot (docs/models.md: `mlx-community/
	// Qwen3-30B-A3B-4bit`, revision `d388dead1515f5e085ef7a0431dd8fadf0886c57`)
	// - the exact on-disk snapshot directory the default Hugging Face
	// cache resolves that repo id + revision to, so mlx_lm.server loads
	// this EXACT pinned revision (never "whatever the cache happens to
	// have most recently"), and works fully offline.
	QwenModelPath string `yaml:"qwen_model_path"`
	// QwenModelName is the "model" field every OpenAI-compatible request to
	// the local server carries. Confirmed LIVE (KAHYA_MLX_TESTS=1):
	// mlx_lm.server does NOT accept an arbitrary label here the way a
	// hosted API might - it validates this against its own GET /v1/models
	// listing (which enumerates every MLX-shaped repo in the local
	// Hugging Face cache, not just the one loaded via --model) and 401s
	// on a mismatch. This must be the exact HF repo id docs/models.md
	// pins.
	QwenModelName string `yaml:"qwen_model_name"`
	// QwenPort is cfg.mlx.qwen_port (task spec: default 8765 - NOT 8080,
	// ComfyUI territory).
	QwenPort int `yaml:"qwen_port"`
	// QwenIdleTTLSeconds is cfg.mlx.idle_ttl (task spec default 600 = 10
	// minutes): with zero in-flight requests for at least this long, the
	// ~17GB server is unloaded (SIGTERM+reap, ledger `mlx_unloaded`) and
	// lazily reloaded on the next secret-lane request.
	QwenIdleTTLSeconds int `yaml:"qwen_idle_ttl_seconds"`
	// QwenClassifyBudgetMS is cfg.mlx.qwen_classify_budget_ms (FIX-P-4):
	// the per-call bound on ONE classification against an already-WARM
	// local Qwen (mlx.Config.ClassifyWarmBudget). Default 300 = the HANDOFF
	// S4 "<300ms" target; raise it after measuring this machine with
	// KAHYA_MLX_TESTS=1 (the live test saw 300-900ms warm). It never
	// bounds a cold load, and a timeout is fail-closed (the text counts as
	// secret-lane). Values < 1 fail Load.
	QwenClassifyBudgetMS int `yaml:"qwen_classify_budget_ms"`

	// Env is KAHYA_ENV ("prod" default | "dev"). It is env-only: there is
	// no config.yaml key for it, since it exists precisely so tests and the
	// W7-8 KAHYA_ENV=dev profile can redirect every path independent of any
	// on-disk config file.
	Env string `yaml:"-"`

	// --- W4-01 two-tier scheduler ---

	// Jobs is cfg.jobs (HANDOFF §4 stack ⚑ scheduling): the wall-clock jobs
	// kahyad installs as launchd LaunchAgents (StartCalendarInterval, NOT
	// an in-daemon cron — Go's darwin monotonic clock stops during sleep,
	// golang/go#24595). Each entry's Handler must name a Go handler
	// registered via kahyad/internal/scheduler.Scheduler.RegisterHandler;
	// LoadJobs resolves this list against that registry at boot. Default
	// empty: no job is declared until config.yaml opts one in (this task
	// ships only the built-in "smoke" handler for tests —
	// backup-nightly/memory-push/briefing/consolidation are declared by
	// their own later tasks, W4-06/W5-01/W5-02).
	Jobs []JobConfig `yaml:"jobs"`

	// --- W4-02 task durability ---

	// TaskRetryW1MaxAuto is `task.retry.w1_max_auto` (task spec, default 3):
	// the cap on receipt-less W1 tool-call auto-retries kahyad/internal/
	// task's resume scan applies per (task_id, tool_name, args_hash) triple
	// before it gives up and moves the task to blocked_user instead. Named
	// as a flat field (rather than a nested `task: {retry: {w1_max_auto:
	// ...}}}` YAML block) to match every other key in this file — this
	// codebase's config.yaml convention is one flat namespace of
	// lower_snake_case keys, not nested dotted paths (see e.g.
	// UndoWindowSeconds/TaskTimeoutMin above); `task_retry_w1_max_auto` is
	// this task's own key, spelled out for readability rather than as a
	// dotted string.
	TaskRetryW1MaxAuto int `yaml:"task_retry_w1_max_auto"`
	// TaskMaxCrashResumes is `task_max_crash_resumes` (FIX-I-2, owner
	// decision G10-2, default 3, must be >= 1): the cap on the resume
	// scan's crash re-dispatches per task (tasks.crash_resumes - a worker
	// that ended without finishing and with nothing parked or blocked).
	// Past it the task moves to blocked_user and the user decides via
	// `kahya task resolve <id>`. Cloud-park redispatches never count.
	TaskMaxCrashResumes int `yaml:"task_max_crash_resumes"`

	// --- W4-04 cloud-call error taxonomy / retry / task parking ---

	// CloudRetryMaxInline is `cloud.retry.max_inline` (task spec, default
	// 3): the max number of upstream attempts kahyad/internal/anthproxy's
	// retry transport makes for one logical model call before giving up
	// inline and parking the task (bekliyor-yeniden-deneme) instead. Flat
	// key, same convention as TaskRetryW1MaxAuto above.
	CloudRetryMaxInline int `yaml:"cloud_retry_max_inline"`
	// CloudRetryTaskSchedule is `cloud.retry.task_schedule` (task spec
	// default: "1m,5m,15m,60m, then hourly"): each entry is a
	// time.ParseDuration string; kahyad/internal/task indexes this list by
	// the task's own attempts count to pick next_retry_at's delay, and
	// simply keeps re-using the LAST entry once attempts exceeds the list
	// length - which is exactly "then hourly" when the last configured
	// entry is itself "60m", as the default is.
	CloudRetryTaskSchedule []string `yaml:"cloud_retry_task_schedule"`
	// CloudRetryGiveUpAfter is `cloud.retry.give_up_after` (task spec
	// default "24h"): a time.ParseDuration string. Once a task has spent
	// longer than this cumulatively parked in bekliyor-yeniden-deneme
	// (measured from the task's own created_at), the NEXT retry-exhaustion
	// gives up instead of parking again - task -> failed + the give-up
	// Turkish notification string, per task spec step 4.
	CloudRetryGiveUpAfter string `yaml:"cloud_retry_give_up_after"`

	// TriggerBinPath is the absolute path to the kahya-trigger binary
	// (kahyad/cmd/kahya-trigger) launchd execs for every declared job —
	// derived the same way MCPBridgePath/PolicyPath/etc. are (two
	// directories up from the running kahyad executable):
	// "<repo>/bin/kahya-trigger".
	TriggerBinPath string `yaml:"trigger_bin_path"`

	// --- W4-06 backups ---

	// KahyaDir is the ~/Kahya memory-repo root (task spec: "the Kahya repo
	// root is filepath.Dir(MemoryDir) = ~/Kahya") — kahyad/internal/backup.
	// Pusher's `git -C <KahyaDir> push origin HEAD` target and the repo
	// whose .gitignore backup.EnsureBackupDirIgnored keeps covering
	// BackupDir (FIX-P-1: at every boot and before every nightly
	// consolidation). Kept as its OWN
	// explicit field (not derived at use-time via
	// filepath.Dir(cfg.MemoryDir)) so a config.yaml override that moves
	// MemoryDir alone does not silently also move the git-push target
	// unless the operator means it to.
	KahyaDir string `yaml:"kahya_dir"`

	// BackupDir is ~/Kahya/backups (task spec step 1a: "Target
	// ~/Kahya/backups/brain-YYYYMMDD.db") — kahyad/internal/backup.
	// Snapshotter's nightly VACUUM INTO target directory.
	BackupDir string `yaml:"backup_dir"`

	// --- W4-05 ledger external anchor ---

	// AnchorRemote is `anchor.remote` (task spec, HANDOFF §5 safety #4 ⚑):
	// the append-only deploy-key git remote kahyad/internal/anchor.Pusher
	// pushes the running-digest anchor to. Default "" (unconfigured) makes
	// every anchor push a complete no-op (kahyad/internal/anchor.Pusher.
	// Run's own doc comment) - this is the ONLY gate main.go needs so
	// dev/test never pushes to a real remote, mirroring how W4-06 gates
	// its own real side effects. Genuinely user-dependent (the user must
	// create the private repo + provision the kahya.anchor Keychain item -
	// docs/runbooks/anchor-setup.md), so there is no sensible non-empty
	// default.
	AnchorRemote string `yaml:"anchor_remote"`
	// AnchorIntervalHours is `anchor.interval_hours` (task spec default 6):
	// the cadence kahyad/internal/scheduler.RegisterTick fires
	// kahyad/internal/anchor.Pusher.Run on. Also the stale-pending alarm's
	// own unit (task spec step 5: "older than 2 x interval_hours").
	AnchorIntervalHours int `yaml:"anchor_interval_hours"`
	// AnchorLocalFallbackPath is `anchor.local_fallback_path` (task spec
	// step 5, optional): when set, every anchor line is ALSO appended
	// there with O_APPEND - the different-uid, kernel-enforced
	// append-only file the docs/runbooks/anchor-setup.md offline-fallback
	// block describes (`chflags sappnd`). The line is appended BEFORE the
	// remote push is attempted, once per anchor_log row (retried next tick
	// on failure; FIX-P-15) - the offline leg of HANDOFF §5 safety #4.
	// Requires anchor_remote: with no remote configured Pusher.Run stays a
	// no-op and nothing is written here (decision G07-1). Default ""
	// (disabled).
	AnchorLocalFallbackPath string `yaml:"anchor_local_fallback_path"`

	// --- W4-07 acceptance gate: tick-interval knobs ---

	// ResumeScanIntervalSeconds is the kahyad/internal/task.Resume periodic
	// scan's own tick interval in seconds (main.go's "task_resume_scan"
	// RegisterTick spec, default 30 - unchanged production cadence). A
	// hermetic gate (tests/acceptance/w4, scripts/accept_w4.sh) overrides
	// this down to a couple of seconds via config.yaml so a CI-speed run
	// does not have to wait out the full 30s production cadence for the
	// resume scan to notice a crashed/killed worker's task.
	ResumeScanIntervalSeconds int `yaml:"resume_scan_interval_seconds"`
	// OutboxDispatchIntervalSeconds is kahyad/internal/outbox.Dispatcher's
	// own claim-and-dispatch tick interval in seconds (main.go's
	// "outbox_dispatch" RegisterTick spec, default 5 - unchanged production
	// cadence). Same CI-speed-gate override rationale as
	// ResumeScanIntervalSeconds above.
	OutboxDispatchIntervalSeconds int `yaml:"outbox_dispatch_interval_seconds"`

	// --- W5-01 morning briefing ---

	// BriefingHour/BriefingMinute are cfg.briefing.hour/cfg.briefing.minute
	// (task spec, default 08:30): the wall-clock time the "morning-briefing"
	// cfg.Jobs entry's own CalendarSpec is built from at defaults()-time
	// (below) - kept as their OWN fields, rather than requiring an operator
	// to hand-edit the jobs: list's Calendar block directly, so
	// `kahya-briefing-hour: 7` alone is enough to reschedule (`kahyad
	// -sync-jobs` still needs a re-run to push the new StartCalendarInterval
	// to launchd - this field only changes what THAT sync would install).
	BriefingHour   int `yaml:"briefing_hour"`
	BriefingMinute int `yaml:"briefing_minute"`
	// BriefingGHRepos is cfg.briefing.gh_repos: the "owner/repo" list
	// kahyad/internal/briefing's gh collector runs `gh pr list`/`gh run
	// list` against (task spec: "seed with the user's active repos, e.g.
	// gold-token"). Default empty - an operator opts in explicitly; an
	// empty list makes the gh section of the briefing simply produce zero
	// items, never an error.
	BriefingGHRepos []string `yaml:"briefing_gh_repos"`
	// BriefingFileGlobs is cfg.briefing.file_globs: the doublestar globs
	// (same syntax as policy.yaml's own glob fields) kahyad/internal/
	// briefing's file collector mtime-diffs since the last run. Default
	// empty - no glob, no items, never an error.
	BriefingFileGlobs []string `yaml:"briefing_file_globs"`
	// BriefingCalendarNames is cfg.briefing.calendar_names: which named
	// macOS Calendar.app calendars kahyad/internal/briefing's calendar
	// collector reads today's events from. Default empty means "every
	// calendar the Automation grant can see" - kahyad/internal/briefing's
	// own JXA snippet treats an empty list as "no name filter".
	BriefingCalendarNames []string `yaml:"briefing_calendar_names"`
	// BriefingMaxFileItems is cfg.briefing.max_file_items (FIX-P-23): at
	// most this many changed files (newest first) are reported by one
	// briefing. Default 50; each item costs one local classification call
	// and one prompt line. Values < 1 fail Load.
	BriefingMaxFileItems int `yaml:"briefing_max_file_items"`
	// Timezone is cfg.timezone (FIX-P-14): the IANA zone name every "which
	// calendar day is it" decision in kahyad uses (the briefing's
	// once-per-day gate, metrics/readiness day buckets, metrics absolute
	// --since dates, the cost governor's daily/monthly USD buckets). Empty
	// (default) = the daemon process's time.Local, i.e. the macOS system
	// zone - the user's own zone on a single-user Mac. Ledger timestamps
	// stay UTC regardless (store.InsertEventWithDigest). An unknown name
	// fails Load. Read it through Location(), never directly.
	Timezone string `yaml:"timezone"`

	// --- W5-02 nightly consolidation ---

	// ConsolidationAutoCommit is cfg.consolidation.auto_commit (HANDOFF §6
	// W5 flag: "otomatik commit W7 mini-eval yesiliyle acilir"). Default
	// false - suggestion mode (diff + `kahya consolidation approve`) for
	// the first 2 weeks, unconditionally. kahyad/internal/consolidation
	// REFUSES to honor true unless an "eval.mini.pass" ledger event (W78-01)
	// exists in the last 30 days - logs an error and stays in suggestion
	// mode otherwise (task spec acceptance criterion) - this field alone
	// never bypasses that runtime guard, it only expresses the operator's
	// intent once the guard would actually pass.
	ConsolidationAutoCommit bool `yaml:"consolidation_auto_commit"`

	// ConsolidationCloudBatchTokenBudget / ConsolidationLocalBatchTokenBudget
	// (FIX-B-5): the nightly consolidation sends each lane to its model in
	// batches whose estimated prompt tokens (kahyad/internal/consolidation
	// estimateFilesTokens: 1 token per 2 JSON-encoded bytes) stay under
	// this budget, never the whole lane in one call. A whole-file rewrite
	// answers with about as many tokens as it was given, so the budgets are
	// sized against each model's OUTPUT cap (consolidation/tokens.go has
	// the arithmetic): cloud 24000 (claude-haiku-5-5; the pinned CLI's
	// max_tokens is 32000; the prompt stays far below Haiku 5.5's 100K
	// price tier), local 5000 (Qwen3-30B-A3B, localMaxTokens 8192).
	// Bounds: cloud 1..MaxConsolidationCloudBatchTokenBudget, local
	// 1..MaxConsolidationLocalBatchTokenBudget; outside them Load fails.
	ConsolidationCloudBatchTokenBudget int `yaml:"consolidation_cloud_batch_token_budget"`
	ConsolidationLocalBatchTokenBudget int `yaml:"consolidation_local_batch_token_budget"`
	// ConsolidationMaxFileBytes (FIX-B-5): a memory file larger than this
	// is sent to no lane that night (ledger
	// consolidation.file_skipped_too_large). Default 262144; Load fails
	// below MinConsolidationMaxFileBytes.
	ConsolidationMaxFileBytes int `yaml:"consolidation_max_file_bytes"`
	// ConsolidationMaxBatchesPerLane (FIX-B-5): at most this many model
	// calls per lane per night (the nightly cost/time cap); the batches
	// beyond it are deferred to the next night, which takes them first.
	// Default 20; Load fails below 1.
	ConsolidationMaxBatchesPerLane int `yaml:"consolidation_max_batches_per_lane"`
	// ConsolidationChangedFilesFirst (FIX-B-5): order each lane's files as
	// changed since the last approved consolidation first, then their
	// same-directory neighbours, then the rest by git commit time. false =
	// commit time only. Default true.
	ConsolidationChangedFilesFirst bool `yaml:"consolidation_changed_files_first"`

	// --- G13 MEM-3 knowledge extraction ---

	// ExtractionEnabled gates the nightly "fact-extraction" job (02:30):
	// false = it runs and does nothing. A manual `kahya memory extract`
	// runs regardless. Default true.
	ExtractionEnabled bool `yaml:"extraction_enabled"`
	// ExtractionBatchMaxEpisodes caps how many memory episodes one run
	// extracts (decision MEM-3.c: 50); the rest follow on later nights.
	// Load fails below 1.
	ExtractionBatchMaxEpisodes int `yaml:"extraction_batch_max_episodes"`
	// ExtractionChunksPerCall is how many chunks one Reader call carries
	// (decision MEM-3.c: 6). Bounds 1..MaxExtractionChunksPerCall (the local
	// model's context); outside them Load fails.
	ExtractionChunksPerCall int `yaml:"extraction_chunks_per_call"`

	// --- W5-03 weekly truth ritual ---

	// RitualWeeklyWeekday/Hour/Minute are cfg.ritual.weekly_at's three
	// components (task spec, default Sunday 18:00 - HANDOFF §6 W5 flag)
	// the "truth-ritual" cfg.Jobs entry's own CalendarSpec is built from
	// at defaults()-time (below), mirroring BriefingHour/Minute's identical
	// "flat, overridable-without-hand-editing-the-jobs-block" rationale
	// (`kahyad -sync-jobs` still needs a re-run for a changed value to
	// reach launchd's own StartCalendarInterval). Weekday follows
	// launchd's own convention (0 = Sunday, matching CalendarSpec.Weekday).
	RitualWeeklyWeekday int `yaml:"ritual_weekly_weekday"`
	RitualWeeklyHour    int `yaml:"ritual_weekly_hour"`
	RitualWeeklyMinute  int `yaml:"ritual_weekly_minute"`

	// --- W6-01 Hammerspoon palette + local approval cards ---

	// HsCliPath is config key `ui.hs_cli` (task spec, verbatim key name;
	// flattened to this codebase's usual lower_snake_case single-namespace
	// convention as ui_hs_cli - matching TaskRetryW1MaxAuto's own doc
	// comment on why a dotted-looking spec key becomes one flat field
	// here): the absolute path to the Hammerspoon `hs` CLI
	// (kahyad/internal/ui.HSCli execs this) kahyad shells out to for (a)
	// every freshly minted pending_approvals row's approval card
	// (kahyad/internal/policy.Engine's SetPendingApprovalHook, wired in
	// main.go) and (b) the generic background/scheduled-task notification
	// path (kahyad/internal/ui.HSCli.SendNotification). Default
	// "/opt/homebrew/bin/hs" (Apple Silicon Homebrew prefix, matching
	// hammerspoon/kahya.lua's hs.ipc.cliInstall("/opt/homebrew") call -
	// see kahyad/internal/ui.DefaultHsCliPath, which duplicates this exact
	// literal by hand, mirroring kahyad/internal/anthproxy's
	// CredentialMode constants' identical "keep two packages' literals in
	// sync by hand rather than take a cross-package import" precedent).
	HsCliPath string `yaml:"ui_hs_cli"`

	// --- W6-05 TTS (`say -v Yelda`) ---

	// TTSEnabled is cfg.tts.enabled (default false, HANDOFF §4 stack TTS
	// row): behind this toggle, every LOCALLY delivered task result/
	// notification (kahyad/internal/notify.Speaker, wired into
	// kahyad/internal/ui.HSCli.SendNotification and kahyad/internal/
	// server's own interactive `kahya ask` completion) is additionally
	// voiced via `say -v TTSVoice`, AFTER the visual notification has
	// already been dispatched. `kahya ask --speak` speaks that ONE task's
	// result regardless of this toggle (Speaker.SpeakRequest.Force) -
	// never on the Telegram/remote channel either way (see
	// kahyad/internal/notify/tts.go's own package doc comment).
	TTSEnabled bool `yaml:"tts_enabled"`
	// TTSVoice is cfg.tts.voice (default "Yelda" - HANDOFF §4 ⚑).
	TTSVoice string `yaml:"tts_voice"`
	// TTSSayBin is cfg.tts.say_bin (default "/usr/bin/say"); tests point
	// this at a fake script (testdata/fake_say.py) so `make test` never
	// shells out to the real binary or produces real audio.
	TTSSayBin string `yaml:"tts_say_bin"`
	// TTSMaxChars is cfg.tts.max_chars (default 280): an utterance's text
	// is truncated to this many RUNES (never bytes - Turkish text) before
	// being spoken - the notification summary, not full outputs (task
	// spec).
	TTSMaxChars int `yaml:"tts_max_chars"`
	// TTSSpeakSecretLane is cfg.tts.speak_secret_lane (default false):
	// shoulder-surfing-conservative default - a secret-lane-labeled
	// (W3-08) result is not spoken unless this is explicitly true. Does
	// NOT touch the §5 lane routing itself (the bytes stay local either
	// way) - purely whether they are ALSO read aloud.
	TTSSpeakSecretLane bool `yaml:"tts_speak_secret_lane"`
}

// TmpDir is W6-02's push-to-talk temp-audio directory: `<data_dir>/tmp`
// (e.g. `~/Library/Application Support/Kahya/tmp`) - kahyad creates this
// 0700 at startup (main.go) and hammerspoon/kahya.lua's ffmpeg capture
// writes `ptt-<epoch>.wav` there; the worker's own delete-safety check
// (kahya_worker.__main__._maybe_delete_audio, via the KAHYA_TMP_DIR env
// var kahyad/internal/spawn.BuildEnv sets from this) only ever deletes a
// file whose PARENT directory is exactly this one - never a repo fixture,
// never any other path. Deliberately a METHOD, not its own configurable
// field: it always tracks cfg.DataDir (prod vs the W7-8 KAHYA_ENV=dev
// profile vs a test's own overridden DataDir) exactly the way LogDir/
// DBPath already do when not explicitly overridden - see Load's own
// "every data_dir-derived field" comment - with no separate config.yaml
// key or env override needed for it.
func (c Config) TmpDir() string {
	return filepath.Join(c.DataDir, "tmp")
}

// SelfProtectDenyGlobs returns fs_write_deny_globs entries DERIVED from this
// config's own runtime paths (B-4): a profile-aware guard, so a dev/redteam
// profile's DataDir/KahyaDir and the installed code tree are as protected
// as prod's hardcoded "~/Library/Application Support/Kahya/**" and
// "~/Kahya/**". main.go merges them with policy.FSWriteDenyGlobs for
// fs_write/fs_delete and the shell_docker workdir check; hostexec has its
// own git hardening and uses no deny globs.
//
// Every entry is absolute (config paths are already ~-expanded). A
// directory gets "/**" (the directory itself and its whole subtree); a
// file is listed exactly. Each path is listed as configured AND, when it
// differs, with its deepest existing ancestor's symlinks resolved — the
// form mcp/fs.Canonicalize hands MatchesAnyGlobCI (e.g. /var/folders ->
// /private/var/folders on macOS), so a symlinked home cannot slip past.
// A relative or empty path is skipped (it could never match a canonical
// absolute path). Overlap with policy.yaml's own globs is harmless
// (MatchesAnyGlobCI is an OR).
//
// Code tree (decision G04-2): only the sub-paths kahyad itself runs or
// trusts — never the whole repo, so a shell_docker task may still use a
// repo subdirectory such as kahyad/ or docs/ as its workdir (the repo's
// .git is covered by the mandatory "**/.git/**"). Two derivations, both
// kept so neither a moved binary nor a config override opens a hole:
//   - from the running executable (two directories up, like
//     defaultWorkerCmd): bin/ (kahyad, kahya-mcp, kahya-trigger), the
//     worker, embed and Qwen venvs, policy.yaml/policy.dev.yaml;
//   - from the configured commands themselves (B-4 review): each Python
//     child's venv (or bare interpreter) and its IMPORT ROOT — the
//     directory that is its cwd and first sys.path / PYTHONPATH entry
//     (PythonServiceDir; for the worker also spawn's PYTHONPATH dir, the
//     venv's parent), where any top-level module or package shadows the
//     stdlib: <repo>/worker (the kahya_worker package is imported from the
//     source tree, not installed), <repo>/mlx/embed (server.py),
//     <repo>/mlx/qwen — plus every binary kahyad execs by path
//     (MCPBridgePath, TriggerBinPath, HsCliPath, TTSSayBin);
//   - beyond the venv, the code every such child runs from its BASE
//     interpreter (pythonRuntimeProtect, B-4 review round 2): the
//     interpreter's install prefix — the stdlib (encodings, site, json …)
//     every start-up imports, lib-dynload, the framework/libpython dylib —
//     found through argv0's symlinks and pyvenv.cfg, and the non-system
//     dylibs those native images link; when the interpreter or such a
//     dylib lives in a Homebrew keg, the whole Homebrew prefix (its kegs
//     and the etc/ config their libraries load code from). These trees are
//     outside the repo, so G04-2 does not apply to them.
func (c Config) SelfProtectDenyGlobs() []string {
	var globs []string
	seen := make(map[string]bool)
	add := func(p string, subtree bool) {
		for _, form := range selfProtectForms(p) {
			g := form
			if subtree {
				g = filepath.Join(form, "**")
			}
			if !seen[g] {
				seen[g] = true
				globs = append(globs, g)
			}
		}
	}
	// Runtime data: brain.db (+ WAL/SHM), logs, undo pre-images, tmp, the
	// control socket's secret; the memory repo and its backups.
	for _, dir := range []string{c.DataDir, c.LogDir, c.KahyaDir, c.MemoryDir, c.BackupDir} {
		add(dir, true)
	}
	// An unset DBPath/Socket yields relative "-wal"/".secret" names, which
	// selfProtectForms skips.
	for _, f := range []string{c.DBPath, c.DBPath + "-wal", c.DBPath + "-shm", ControlSecretPath(c.Socket)} {
		add(f, false)
	}
	// Runtime trust anchors kahyad reads: the policy it boots with and the
	// committed image-digest pins.
	add(c.PolicyPath, false)
	add(c.DockerImageDigestPath, false)
	add(c.EgressSidecarDigestPath, false)
	if exe, err := os.Executable(); err == nil {
		repo := filepath.Dir(filepath.Dir(exe))
		add(filepath.Join(repo, "bin"), true)
		add(filepath.Join(repo, "worker", ".venv"), true)
		add(filepath.Join(repo, "mlx", "embed", ".venv"), true)
		add(filepath.Join(repo, "mlx", "qwen", ".venv"), true)
		add(filepath.Join(repo, "policy.yaml"), false)
		add(filepath.Join(repo, "policy.dev.yaml"), false)
	}
	for _, cmd := range [][]string{c.WorkerCmd, c.EmbedCmd, c.QwenCmd} {
		if len(cmd) == 0 {
			continue
		}
		if venv := pythonVenvDir(cmd[0]); venv != "" {
			add(venv, true)
		} else {
			add(cmd[0], false)
		}
		if dir := PythonServiceDir(cmd); dir != "" {
			add(dir, true)
		}
		// The base interpreter's install prefix (stdlib, lib-dynload,
		// framework dylib) and the libraries its native images link
		// (pyruntime.go, B-4 review round 2).
		subtrees, files := pythonRuntimeProtect(cmd[0])
		for _, s := range subtrees {
			add(s, true)
		}
		for _, f := range files {
			add(f, false)
		}
	}
	// The worker's venv parent is its cwd and first PYTHONPATH entry
	// whatever its argv shape (spawn.pythonWorkerDir).
	if len(c.WorkerCmd) > 0 {
		if venv := pythonVenvDir(c.WorkerCmd[0]); venv != "" {
			add(filepath.Dir(venv), true)
		}
	}
	for _, bin := range []string{c.MCPBridgePath, c.TriggerBinPath, c.HsCliPath, c.TTSSayBin} {
		add(bin, false)
	}
	return globs
}

// PythonServiceDir returns the working directory — and so the FIRST import
// root (sys.path[0]) — of a Python child kahyad spawns from cmd: for a
// script invocation [<python>, <script.py>, ...] the script's own
// directory (python puts it first on sys.path; main.go runs the child
// there); for a module invocation [<dir>/.venv/bin/python, "-m",
// <module>, ...] the venv's parent <dir> (main.go runs the child there,
// and for -m python puts the cwd first on sys.path). "" — the child
// inherits kahyad's own cwd — when cmd has fewer than two entries or a
// module invocation's interpreter is not inside a ".venv/bin/" layout.
// Only cmd[1] is inspected (no interpreter flags before the script or -m).
// main.go derives the embed and Qwen services' cwd from this function, so
// the cwd and SelfProtectDenyGlobs' import-root entry cannot drift apart;
// spawn.pythonWorkerDir derives the worker's the same way (venv parent).
func PythonServiceDir(cmd []string) string {
	if len(cmd) < 2 {
		return ""
	}
	if cmd[1] == "-m" {
		if venv := pythonVenvDir(cmd[0]); venv != "" {
			return filepath.Dir(venv)
		}
		return ""
	}
	return filepath.Dir(cmd[1])
}

// pythonVenvDir returns the ".venv" directory argv0 runs from
// ("<dir>/.venv/bin/python" -> "<dir>/.venv"), or "" when argv0 does not
// sit in that layout.
func pythonVenvDir(argv0 string) string {
	venv := filepath.Dir(filepath.Dir(argv0))
	if filepath.Base(venv) != ".venv" {
		return ""
	}
	return venv
}

// selfProtectForms returns p (cleaned) and, when it differs, p with its
// deepest existing ancestor's symlinks resolved. Empty or relative p
// yields nothing.
func selfProtectForms(p string) []string {
	if p == "" || !filepath.IsAbs(p) {
		return nil
	}
	p = filepath.Clean(p)
	out := []string{p}
	if r := resolveExistingAncestor(p); r != "" && r != p {
		out = append(out, r)
	}
	return out
}

// resolveExistingAncestor resolves the symlinks of p's deepest existing
// ancestor (p itself when it exists) and re-appends the not-yet-existing
// remainder verbatim; "" when nothing resolves.
func resolveExistingAncestor(p string) string {
	rest := ""
	cur := p
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// JobConfig is one cfg.jobs entry (W4-01 task spec step 1). Name must be
// DNS-label chars only (kahyad/internal/scheduler renders it into a
// "com.kahya.job.<name>" launchd Label and a "job-<name>.log" filename —
// both need this constraint to stay filesystem/launchd-safe); Calendar
// mirrors launchd's own StartCalendarInterval dict keys (a nil field means
// "every" for that unit, matching StartCalendarInterval's own documented
// omitted-key semantics); Handler names a Go handler registered via
// kahyad/internal/scheduler.Scheduler.RegisterHandler — not necessarily
// equal to Name (multiple jobs could in principle share one handler).
type JobConfig struct {
	Name     string       `yaml:"name"`
	Calendar CalendarSpec `yaml:"calendar"`
	Handler  string       `yaml:"handler"`
}

// CalendarSpec mirrors launchd's StartCalendarInterval dict (task spec
// step 1): Minute/Hour/Day/Weekday, each *int so "unset" (launchd's
// "every") is distinguishable from an explicit 0. Field names are
// capitalized to match launchd's own dict keys 1:1 — deliberately NOT
// this file's usual lower_snake_case yaml convention — so
// kahyad/internal/scheduler.RenderPlist's output and config.yaml's
// calendar: block read as the same StartCalendarInterval dict, just in
// two different serializations.
type CalendarSpec struct {
	Minute  *int `yaml:"Minute,omitempty"`
	Hour    *int `yaml:"Hour,omitempty"`
	Day     *int `yaml:"Day,omitempty"`
	Weekday *int `yaml:"Weekday,omitempty"`
}

// fileConfig mirrors Config for YAML unmarshalling, using pointers so we
// can distinguish "key absent" (nil) from "key present with zero value".
type fileConfig struct {
	DataDir                       *string         `yaml:"data_dir"`
	Socket                        *string         `yaml:"socket"`
	LogDir                        *string         `yaml:"log_dir"`
	DBPath                        *string         `yaml:"db_path"`
	MemoryDir                     *string         `yaml:"memory_dir"`
	AnthropicUpstreamURL          *string         `yaml:"anthropic_upstream_url"`
	EmbedPort                     *int            `yaml:"embed_port"`
	DefaultModel                  *string         `yaml:"default_model"`
	TaskTimeoutMin                *int            `yaml:"task_timeout_min"`
	ActiveEmbedModelVer           *string         `yaml:"active_embed_model_ver"`
	LogLevel                      *string         `yaml:"log_level"`
	UndoWindowSeconds             *int            `yaml:"undo_window_seconds"`
	ApprovalWaitSeconds           *int            `yaml:"approval_wait_seconds"`
	AutonomyBootstrapLevels       *map[string]int `yaml:"autonomy_bootstrap_levels"`
	WorkerCmd                     *[]string       `yaml:"worker_cmd"`
	EmbedCmd                      *[]string       `yaml:"embed_cmd"`
	MCPBridgePath                 *string         `yaml:"mcp_bridge_path"`
	PolicyPath                    *string         `yaml:"policy_path"`
	EvalMiniBaselinePath          *string         `yaml:"eval_mini_baseline_path"`
	EvalRetrievalDatasetPath      *string         `yaml:"eval_retrieval_dataset_path"`
	SearchRecencyWeight           *float64        `yaml:"search_recency_weight"`
	SearchRecencyHalfLifeDays     *int            `yaml:"search_recency_half_life_days"`
	SecretLaneMemoryDir           *string         `yaml:"secret_lane_memory_dir"`
	EgressPort                    *int            `yaml:"egress_port"`
	DockerImageTag                *string         `yaml:"docker_image_tag"`
	DockerImageDigestPath         *string         `yaml:"docker_image_digest_path"`
	EgressSidecarDigestPath       *string         `yaml:"egress_sidecar_digest_path"`
	ShellWorkdirRoots             *[]string       `yaml:"shell_workdir_roots"`
	DailyBudgetUSD                *float64        `yaml:"daily_budget_usd"`
	MonthlyBudgetUSD              *float64        `yaml:"monthly_budget_usd"`
	TaskTokenCeiling              *int64          `yaml:"task_token_ceiling"`
	DowngradeAtRatio              *float64        `yaml:"downgrade_at_ratio"`
	CacheHitAlarmThreshold        *float64        `yaml:"cache_hit_alarm_threshold"`
	CredentialMode                *string         `yaml:"credential_mode"`
	EstRequestTokens              *int64          `yaml:"est_request_tokens"`
	TelegramChatID                *int64          `yaml:"telegram_chat_id"`
	TelegramUserID                *int64          `yaml:"telegram_user_id"`
	TelegramAPIURL                *string         `yaml:"telegram_api_url"`
	QwenCmd                       *[]string       `yaml:"qwen_cmd"`
	QwenModelPath                 *string         `yaml:"qwen_model_path"`
	QwenModelName                 *string         `yaml:"qwen_model_name"`
	QwenPort                      *int            `yaml:"qwen_port"`
	QwenIdleTTLSeconds            *int            `yaml:"qwen_idle_ttl_seconds"`
	QwenClassifyBudgetMS          *int            `yaml:"qwen_classify_budget_ms"`
	Jobs                          *[]JobConfig    `yaml:"jobs"`
	TriggerBinPath                *string         `yaml:"trigger_bin_path"`
	TaskRetryW1MaxAuto            *int            `yaml:"task_retry_w1_max_auto"`
	TaskMaxCrashResumes           *int            `yaml:"task_max_crash_resumes"`
	CloudRetryMaxInline           *int            `yaml:"cloud_retry_max_inline"`
	CloudRetryTaskSchedule        *[]string       `yaml:"cloud_retry_task_schedule"`
	CloudRetryGiveUpAfter         *string         `yaml:"cloud_retry_give_up_after"`
	KahyaDir                      *string         `yaml:"kahya_dir"`
	BackupDir                     *string         `yaml:"backup_dir"`
	AnchorRemote                  *string         `yaml:"anchor_remote"`
	AnchorIntervalHours           *int            `yaml:"anchor_interval_hours"`
	AnchorLocalFallbackPath       *string         `yaml:"anchor_local_fallback_path"`
	ResumeScanIntervalSeconds     *int            `yaml:"resume_scan_interval_seconds"`
	OutboxDispatchIntervalSeconds *int            `yaml:"outbox_dispatch_interval_seconds"`
	BriefingHour                  *int            `yaml:"briefing_hour"`
	BriefingMinute                *int            `yaml:"briefing_minute"`
	BriefingGHRepos               *[]string       `yaml:"briefing_gh_repos"`
	BriefingFileGlobs             *[]string       `yaml:"briefing_file_globs"`
	BriefingCalendarNames         *[]string       `yaml:"briefing_calendar_names"`
	BriefingMaxFileItems          *int            `yaml:"briefing_max_file_items"`
	Timezone                      *string         `yaml:"timezone"`
	ConsolidationAutoCommit       *bool           `yaml:"consolidation_auto_commit"`
	ConsolidationCloudBatchTokens *int            `yaml:"consolidation_cloud_batch_token_budget"`
	ConsolidationLocalBatchTokens *int            `yaml:"consolidation_local_batch_token_budget"`
	ConsolidationMaxFileBytes     *int            `yaml:"consolidation_max_file_bytes"`
	ConsolidationMaxBatches       *int            `yaml:"consolidation_max_batches_per_lane"`
	ExtractionEnabled             *bool           `yaml:"extraction_enabled"`
	ExtractionBatchMaxEpisodes    *int            `yaml:"extraction_batch_max_episodes"`
	ExtractionChunksPerCall       *int            `yaml:"extraction_chunks_per_call"`
	ConsolidationChangedFirst     *bool           `yaml:"consolidation_changed_files_first"`
	RitualWeeklyWeekday           *int            `yaml:"ritual_weekly_weekday"`
	RitualWeeklyHour              *int            `yaml:"ritual_weekly_hour"`
	RitualWeeklyMinute            *int            `yaml:"ritual_weekly_minute"`
	HsCliPath                     *string         `yaml:"ui_hs_cli"`
	TTSEnabled                    *bool           `yaml:"tts_enabled"`
	TTSVoice                      *string         `yaml:"tts_voice"`
	TTSSayBin                     *string         `yaml:"tts_say_bin"`
	TTSMaxChars                   *int            `yaml:"tts_max_chars"`
	TTSSpeakSecretLane            *bool           `yaml:"tts_speak_secret_lane"`
}

// DefaultModelID is default_model's built-in value: HANDOFF §9's current
// Sonnet (the §4 chat/unknown row). A literal because config is a leaf
// package; kahyad/internal/server's TestModelIDsMatchHandoffSection9 pins
// it equal to router.ModelSonnet (FIX-G12-N2: claude-sonnet-5 ->
// claude-sonnet-5-5).
const DefaultModelID = "claude-sonnet-5-5"

// Load resolves Config from defaults, an optional config.yaml, and
// environment overrides, in that precedence order (lowest to highest).
func Load() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("config: resolve home dir: %w", err)
	}

	// W4-07 dev-profile plumbing: KAHYA_ENV is read HERE, before defaults()
	// ever runs (not only later, in applyEnv, which still runs and simply
	// re-assigns the SAME value to cfg.Env) - so the DEFAULT data_dir/
	// memory_dir/socket/kahya_dir/backup_dir themselves already reflect the
	// dev profile (~/Library/Application Support/Kahya-dev, ~/Kahya-dev,
	// kahyad-dev.sock) whenever KAHYA_ENV=dev, with no config.yaml key
	// needed for it (Config.Env's own doc comment: "env-only") - the
	// separate brain.db/memory/socket/launchd-label a dev profile requires
	// (HANDOFF §6 W7-8 ⚑: "ayrı brain.db + ayrı ~/Kahya-dev/memory").
	env := os.Getenv("KAHYA_ENV")
	if env == "" {
		env = EnvProd
	}

	cfg := defaults(home, env)

	// config.yaml itself, HOWEVER, has ONE canonical, env-INDEPENDENT
	// location: the prod (HOME-derived) data_dir, ALWAYS - never the
	// dev-profile data_dir, even under KAHYA_ENV=dev. Two reasons: (1) it is
	// the documented contract every hermetic gate that runs under
	// KAHYA_ENV=dev already relies on (tests/w3, tests/e2e - each redirects
	// HOME to a temp dir and writes ONE config.yaml under
	// <HOME>/Library/Application Support/Kahya; making the read location
	// track the dev profile silently stopped that config from ever being
	// read, disabling the Telegram bot / mock upstream and breaking those
	// gates). (2) The §6 W7-8 dev profile is about DATA isolation (a
	// separate brain.db/memory the dev process cannot confuse with prod) -
	// NOT a separate config file; a dev daemon reading the same config.yaml
	// as prod but writing its data under Kahya-dev is exactly the intended
	// separation (and refuseNonProdProfileOpeningProdDB below still guarantees
	// the DB path itself is never the prod one). At this point in the
	// pipeline data_dir has not yet been touched by the file or env layers,
	// so there is exactly one unambiguous place to look.
	fileCfgPath := filepath.Join(defaults(home, EnvProd).DataDir, "config.yaml")

	var explicitSocket, explicitLogDir, explicitDBPath bool
	// explicitApprovalWait (B-1): approval_wait_seconds' upper bound
	// (< task_timeout_min*60) is enforced only when config.yaml writes the
	// key - the default 300 must never fail boot for a config that merely
	// lowers task_timeout_min (a hermetic harness writes 2).
	var explicitApprovalWait bool

	if fc, ok, err := loadFile(fileCfgPath); err != nil {
		return Config{}, err
	} else if ok {
		applyFile(&cfg, fc, home, &explicitSocket, &explicitLogDir, &explicitDBPath)
		explicitApprovalWait = fc.ApprovalWaitSeconds != nil
	}

	applyEnv(&cfg, home, &explicitSocket, &explicitLogDir, &explicitDBPath)

	// Sync the config-field-driven schedules into cfg.Jobs. The
	// "truth-ritual" and "morning-briefing" job entries are seeded in
	// defaults() from the default constants; an operator override of
	// ritual_weekly_* / briefing_hour/minute (config.yaml or env) must
	// actually MOVE the scheduled launchd job, not merely sit in the
	// otherwise-unused Config field (W5-03 review). Done before validateJobs
	// so the reconciled CalendarSpec is validated. A user who replaced the
	// whole jobs: list keeps whatever they wrote for any OTHER entry; these
	// two named entries, if present, track their dedicated config keys.
	reconcileConfigDrivenJobCalendars(&cfg)

	if err := validateEnv(cfg.Env); err != nil {
		return Config{}, err
	}
	if err := validateLogLevel(cfg.LogLevel); err != nil {
		return Config{}, err
	}
	if err := validateCredentialMode(cfg.CredentialMode); err != nil {
		return Config{}, err
	}
	if err := validateJobs(cfg.Jobs); err != nil {
		return Config{}, err
	}
	if err := validateCloudRetry(cfg.CloudRetryMaxInline, cfg.CloudRetryTaskSchedule, cfg.CloudRetryGiveUpAfter); err != nil {
		return Config{}, err
	}
	if err := validateTaskResilience(cfg.TaskMaxCrashResumes); err != nil {
		return Config{}, err
	}
	if err := validateAnchorIntervalHours(cfg.AnchorIntervalHours); err != nil {
		return Config{}, err
	}
	if err := validateTickIntervals(cfg.ResumeScanIntervalSeconds, cfg.OutboxDispatchIntervalSeconds); err != nil {
		return Config{}, err
	}
	if err := validateApprovalWait(cfg.ApprovalWaitSeconds, cfg.TaskTimeoutMin, explicitApprovalWait); err != nil {
		return Config{}, err
	}
	if err := validateQwenClassifyBudgetMS(cfg.QwenClassifyBudgetMS); err != nil {
		return Config{}, err
	}
	if err := validateBriefingMaxFileItems(cfg.BriefingMaxFileItems); err != nil {
		return Config{}, err
	}
	if err := validateConsolidationBatching(cfg); err != nil {
		return Config{}, err
	}
	if err := validateTimezone(cfg.Timezone); err != nil {
		return Config{}, err
	}
	if err := validateSearchRecency(cfg.SearchRecencyWeight, cfg.SearchRecencyHalfLifeDays); err != nil {
		return Config{}, err
	}
	if err := validateSecretLaneMemoryDir(cfg.SecretLaneMemoryDir); err != nil {
		return Config{}, err
	}
	if err := validateExtraction(cfg.ExtractionBatchMaxEpisodes, cfg.ExtractionChunksPerCall); err != nil {
		return Config{}, err
	}

	// Any of the data_dir-derived fields not explicitly set by the file or
	// env layers follow the *final* data_dir (so overriding just
	// KAHYA_DATA_DIR moves socket/log_dir/db_path along with it). The
	// socket's own FILENAME (not just its directory) still reflects the dev
	// profile (kahyad-dev.sock, never plain kahyad.sock) whenever
	// cfg.Env==dev, so this fallback can never silently un-do defaults()'s
	// own dev-profile socket name merely because data_dir happened to be
	// overridden without socket also being set explicitly.
	if !explicitSocket {
		cfg.Socket = filepath.Join(cfg.DataDir, socketFileName(cfg.Env))
	}
	if !explicitLogDir {
		cfg.LogDir = filepath.Join(cfg.DataDir, "logs")
	}
	if !explicitDBPath {
		cfg.DBPath = filepath.Join(cfg.DataDir, "brain.db")
	}

	if err := validateASCIIPaths(cfg); err != nil {
		return Config{}, err
	}

	// W4-07 HARD CONSTRAINT: a dev-profile process must NEVER end up
	// pointed at the real production brain.db path, however that happened
	// (an operator hand-editing config.yaml, a stray KAHYA_DB_PATH/
	// KAHYA_DATA_DIR export left over from a prod shell) - fail Load
	// closed rather than let a dev kahyad silently read/write the real
	// database. Checked LAST, against the fully-resolved cfg.DBPath, so
	// every override layer (file, env, the data_dir-derived fallback just
	// above) has already had its say.
	if err := refuseNonProdProfileOpeningProdDB(cfg, home); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// reconcileConfigDrivenJobCalendars rewrites the CalendarSpec of the
// "truth-ritual" and "morning-briefing" cfg.Jobs entries (if present) from
// their dedicated config keys (ritual_weekly_weekday/hour/minute,
// briefing_hour/minute), so an override of those keys actually moves the
// scheduled job. Idempotent when the keys are at their defaults (the entry's
// calendar already equals them). Other job entries are untouched.
func reconcileConfigDrivenJobCalendars(cfg *Config) {
	for i := range cfg.Jobs {
		switch cfg.Jobs[i].Name {
		case "truth-ritual":
			cfg.Jobs[i].Calendar = CalendarSpec{
				Weekday: intPtr(cfg.RitualWeeklyWeekday),
				Hour:    intPtr(cfg.RitualWeeklyHour),
				Minute:  intPtr(cfg.RitualWeeklyMinute),
			}
		case "morning-briefing":
			cfg.Jobs[i].Calendar = CalendarSpec{
				Hour:   intPtr(cfg.BriefingHour),
				Minute: intPtr(cfg.BriefingMinute),
			}
		}
	}
}

// socketFileName is the control-socket's own filename for env. Production
// ("prod", the default) keeps the plain kahyad.sock; ANY other (non-prod)
// profile name gets its own "kahyad-<name>.sock" (e.g. dev -> kahyad-dev.sock,
// redteam -> kahyad-redteam.sock) - HANDOFF §6 W7-8 ⚑ dev-profile: a
// separate launchd label/socket per profile so a dev/red-team and a prod
// kahyad could in principle run side by side without colliding. Generic on
// purpose (W78-02 D1): W78-05's `restore` scratch profile reuses this exact
// resolution. The PROD filename (kahyad.sock) stays byte-identical to what
// it always was.
func socketFileName(env string) string {
	if env == EnvProd {
		return "kahyad.sock"
	}
	return "kahyad-" + env + ".sock"
}

// refuseNonProdProfileOpeningProdDB implements the W4-07 HARD CONSTRAINT
// (generalized in W78-02 D1 from dev-only to ANY non-prod profile): fail
// Load closed when cfg.Env is a non-prod profile (dev, redteam, restore, ...)
// but the fully-resolved cfg.DBPath is nonetheless the real production
// database path (defaults(home, EnvProd)'s own db_path) - see Load's own
// call-site comment for why this is checked last, against the final resolved
// value, not merely against whichever layer happened to set it. A prod Env
// is (correctly) allowed to open the prod db; only a profile that has NO
// business touching prod data is refused.
func refuseNonProdProfileOpeningProdDB(cfg Config, home string) error {
	if cfg.Env == EnvProd {
		return nil
	}
	prodDBPath := defaults(home, EnvProd).DBPath
	if cfg.DBPath == prodDBPath {
		return fmt.Errorf("config: KAHYA_ENV=%s refuses to open the production brain.db (%s) - a non-prod profile must use a separate db_path/KAHYA_DATA_DIR/KAHYA_DB_PATH", cfg.Env, prodDBPath)
	}
	return nil
}

// ProdDBPath returns the production brain.db path for the current user's
// home directory - the exact path a KAHYA_ENV=prod (or unset) Load resolves
// to. Exported for the W78-02 red-team harness's fail-closed guard (it must
// prove, at runtime, that the profile db it is about to open is NOT this
// prod path) and its "never opens the prod brain.db" regression test. Uses
// os.UserHomeDir, exactly like Load; a home-resolution failure surfaces as
// an error rather than a silently-wrong path.
func ProdDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: resolve home dir: %w", err)
	}
	return defaults(home, EnvProd).DBPath, nil
}

// ProdSocketPath returns the production kahyad control-socket path for the
// current user's home directory (the exact socket a KAHYA_ENV=prod Load
// resolves to, kahyad.sock). Exported for `kahya eval redteam` (W78-02 D5):
// the red-team CLI itself runs under KAHYA_ENV=dev, but the post-run
// eval.redteam.result SUMMARY row must be written by the PRODUCTION kahyad
// over ITS socket (the only production touchpoint), so the CLI dials this
// path for that one write regardless of the dev profile it otherwise uses.
func ProdSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: resolve home dir: %w", err)
	}
	return defaults(home, EnvProd).Socket, nil
}

// DefaultRedteamScenariosDir resolves "<repo>/eval/redteam/scenarios", using
// the exact same repo-root derivation as defaultPolicyPath (two directories
// up from the running executable). The committed red-team scenario set lives
// there; `kahya eval redteam` hashes it into the summary row's
// scenarios_sha256 so a run is tied to the exact attack definitions it
// exercised.
func DefaultRedteamScenariosDir() string {
	repoRoot := "."
	if exe, err := os.Executable(); err == nil {
		repoRoot = filepath.Dir(filepath.Dir(exe))
	}
	return filepath.Join(repoRoot, "eval", "redteam", "scenarios")
}

// defaultTickIntervalSeconds mirrors main.go's own pre-W4-07 hardcoded
// "@every 30s"/"@every 5s" tick specs - unchanged production cadence; only
// a hermetic gate's own config.yaml overlay ever sets these lower.
const (
	defaultResumeScanIntervalSeconds     = 30
	defaultOutboxDispatchIntervalSeconds = 5
)

// MaxApprovalWaitSeconds is approval_wait_seconds' upper bound (B-1): the
// 10-minute pending-approval TTL (kahyad/internal/policy
// pendingApprovalTTL - kept in sync by TestMaxApprovalWaitMatchesPendingTTL
// in kahyad/internal/policy).
const MaxApprovalWaitSeconds = 600

// validateApprovalWait fails Load closed (B-1) on a negative
// approval_wait_seconds, and - only when config.yaml wrote the key
// explicitly - on one that is not below task_timeout_min*60: a wait the
// task timeout always cuts short is a misconfiguration, not a choice. The
// DEFAULT (300) is never checked against task_timeout_min: a task that
// times out first kills its worker, the bridge connection closes, and the
// parked call ends as canceled without executing (kahyad/internal/server
// requestDoneKey).
//
// It is also capped at MaxApprovalWaitSeconds: a pending approval itself
// expires after 10 minutes (kahyad/internal/policy pendingApprovalTTL), so
// a longer wait could never be answered - it would only hold the worker.
func validateApprovalWait(waitSeconds, taskTimeoutMin int, explicit bool) error {
	if waitSeconds < 0 {
		return fmt.Errorf("config: approval_wait_seconds must be >= 0 (got %d)", waitSeconds)
	}
	if waitSeconds > MaxApprovalWaitSeconds {
		return fmt.Errorf("config: approval_wait_seconds must be <= %d, the pending approval's own TTL (got %d)", MaxApprovalWaitSeconds, waitSeconds)
	}
	if explicit && waitSeconds >= taskTimeoutMin*60 {
		return fmt.Errorf("config: approval_wait_seconds (%d) must be less than task_timeout_min*60 (%d)", waitSeconds, taskTimeoutMin*60)
	}
	return nil
}

// validateTickIntervals fails Load closed (same posture as every other
// validate* function above) on a non-positive tick interval - main.go
// renders these straight into a cron.ParseStandard "@every <n>s" spec,
// where n<=0 is nonsensical (and, for "@every 0s", would busy-loop the
// scheduler).
func validateTickIntervals(resumeScanSeconds, outboxDispatchSeconds int) error {
	if resumeScanSeconds < 1 {
		return fmt.Errorf("config: resume_scan_interval_seconds=%d invalid, must be >= 1", resumeScanSeconds)
	}
	if outboxDispatchSeconds < 1 {
		return fmt.Errorf("config: outbox_dispatch_interval_seconds=%d invalid, must be >= 1", outboxDispatchSeconds)
	}
	return nil
}

// validateQwenClassifyBudgetMS fails Load closed (same posture as every
// other validate* function above) on qwen_classify_budget_ms < 1
// (FIX-P-4): a zero or negative budget would fail every warm local
// classification closed - silently turning every briefing item and every
// task prompt into a secret-lane placeholder.
func validateQwenClassifyBudgetMS(ms int) error {
	if ms < 1 {
		return fmt.Errorf("config: qwen_classify_budget_ms must be >= 1, got %d", ms)
	}
	return nil
}

// validateBriefingMaxFileItems fails Load closed on
// briefing_max_file_items < 1 (FIX-P-23): the collector treats a
// non-positive cap as "use the default", so a 0 written on purpose would
// otherwise be silently ignored.
func validateBriefingMaxFileItems(n int) error {
	if n < 1 {
		return fmt.Errorf("config: briefing_max_file_items must be >= 1, got %d", n)
	}
	return nil
}

// FIX-B-5 nightly-consolidation batching defaults and bounds. The defaults
// equal kahyad/internal/consolidation's package defaults (pinned by
// consolidation's TestBatchBudgetsFitModelLimits, which also checks the
// bounds against the model limits in consolidation/tokens.go).
const (
	DefaultConsolidationCloudBatchTokenBudget = 24000
	DefaultConsolidationLocalBatchTokenBudget = 5000
	DefaultConsolidationMaxFileBytes          = 262144
	DefaultConsolidationMaxBatchesPerLane     = 20

	// MaxConsolidationCloudBatchTokenBudget keeps one cloud batch's prompt
	// (the batch plus the fixed rewrite instructions) under
	// claude-haiku-5-5's 100K-token price tier: above it the whole call is
	// billed at 5x ($0.50/$2.50 instead of $0.10/$0.50).
	MaxConsolidationCloudBatchTokenBudget = 90000
	// MaxConsolidationLocalBatchTokenBudget keeps one local batch plus its
	// 8192-token answer inside Qwen3-30B-A3B's native 32,768-token context.
	MaxConsolidationLocalBatchTokenBudget = 23000
	// MinConsolidationMaxFileBytes: a smaller cap would skip ordinary notes.
	MinConsolidationMaxFileBytes = 4096
)

// validateConsolidationBatching fails Load closed (FIX-B-5) on a batch
// budget outside its bounds, a max_file_bytes below 4096 or a
// max_batches_per_lane below 1: a zero would otherwise be silently replaced
// by the package default, and an over-large budget would leave the price
// tier or the context window.
func validateConsolidationBatching(cfg Config) error {
	if b := cfg.ConsolidationCloudBatchTokenBudget; b < 1 || b > MaxConsolidationCloudBatchTokenBudget {
		return fmt.Errorf("config: consolidation_cloud_batch_token_budget must be 1..%d, got %d", MaxConsolidationCloudBatchTokenBudget, b)
	}
	if b := cfg.ConsolidationLocalBatchTokenBudget; b < 1 || b > MaxConsolidationLocalBatchTokenBudget {
		return fmt.Errorf("config: consolidation_local_batch_token_budget must be 1..%d, got %d", MaxConsolidationLocalBatchTokenBudget, b)
	}
	if n := cfg.ConsolidationMaxFileBytes; n < MinConsolidationMaxFileBytes {
		return fmt.Errorf("config: consolidation_max_file_bytes must be >= %d, got %d", MinConsolidationMaxFileBytes, n)
	}
	if n := cfg.ConsolidationMaxBatchesPerLane; n < 1 {
		return fmt.Errorf("config: consolidation_max_batches_per_lane must be >= 1, got %d", n)
	}
	return nil
}

// G13 MEM-3 extraction defaults (decision MEM-3.c) and bounds. The
// defaults equal kahyad/internal/extract's package defaults (pinned by
// kahyad's TestExtractionDefaultsMatchExtractPackage).
const (
	DefaultExtractionBatchMaxEpisodes = 50
	DefaultExtractionChunksPerCall    = 6
	// MaxExtractionChunksPerCall keeps one call (chunks of at most 1600
	// runes, ~1K tokens each, plus the ~1K-token instructions and the
	// 4096-token answer) inside the local Qwen3-30B-A3B's 32,768-token
	// context - the secret-lane calls run there; the cloud Haiku 5.5's
	// 100K price tier is far above it.
	MaxExtractionChunksPerCall = 20
)

// validateExtraction fails Load closed (FIX-MEM-3) on a batch cap below 1
// or a chunks-per-call outside 1..MaxExtractionChunksPerCall: a zero would
// otherwise be silently replaced by the package default.
func validateExtraction(batchMaxEpisodes, chunksPerCall int) error {
	if batchMaxEpisodes < 1 {
		return fmt.Errorf("config: extraction_batch_max_episodes must be >= 1, got %d", batchMaxEpisodes)
	}
	if chunksPerCall < 1 || chunksPerCall > MaxExtractionChunksPerCall {
		return fmt.Errorf("config: extraction_chunks_per_call must be 1..%d, got %d", MaxExtractionChunksPerCall, chunksPerCall)
	}
	return nil
}

// DefaultSearchRecencyHalfLifeDays is search_recency_half_life_days'
// default (FIX-MEM-8); it equals kahyad/internal/search's
// DefaultRecencyHalfLifeDays (pinned by kahyad's
// TestSearchRecencyDefaultsMatchSearchPackage).
const DefaultSearchRecencyHalfLifeDays = 90

// validateSearchRecency fails Load closed (FIX-MEM-8) on a recency weight
// outside [0,1] (NaN included) or a half-life below 1 day.
func validateSearchRecency(weight float64, halfLifeDays int) error {
	if !(weight >= 0 && weight <= 1) {
		return fmt.Errorf("config: search_recency_weight must be 0..1, got %v", weight)
	}
	if halfLifeDays < 1 {
		return fmt.Errorf("config: search_recency_half_life_days must be >= 1, got %d", halfLifeDays)
	}
	return nil
}

// DefaultSecretLaneMemoryDir is secret_lane_memory_dir's default
// (FIX-U-3); it equals kahya/mcp/memory.DefaultSecretLaneDir (pinned by
// kahyad's TestSecretLaneMemoryDirDefaultMatchesMemoryPackage).
const DefaultSecretLaneMemoryDir = "gizli"

// validateSecretLaneMemoryDir fails Load closed (FIX-U-3) on a
// secret_lane_memory_dir that is empty, absolute, escapes MemoryDir (a
// ".." component), names a dot-directory (the indexer and consolidation
// skip those, so a redirected note would vanish from local recall), is not
// in clean forward-slash form, or contains a non-ASCII rune (CLAUDE.md:
// directory names are ASCII).
func validateSecretLaneMemoryDir(dir string) error {
	bad := func(why string) error {
		return fmt.Errorf("config: secret_lane_memory_dir %q: %s", dir, why)
	}
	if strings.TrimSpace(dir) == "" {
		return bad("must not be empty")
	}
	for _, r := range dir {
		if r > 127 {
			return bad("must be ASCII (HANDOFF §7: directory names are ASCII)")
		}
	}
	if strings.Contains(dir, "\\") || strings.HasPrefix(dir, "/") || filepath.IsAbs(dir) {
		return bad("must be a forward-slash path relative to memory_dir")
	}
	if path.Clean(dir) != dir {
		return bad("must be in clean form (no trailing slash, no . or empty components)")
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == ".." {
			return bad("must not escape memory_dir")
		}
		if strings.HasPrefix(seg, ".") {
			return bad("must not name a dot-directory (never indexed)")
		}
	}
	return nil
}

// validateTimezone fails Load closed on an unknown timezone name
// (FIX-P-14): kahyad must never silently run its day boundaries in a zone
// the operator did not ask for. Empty = time.Local (see Config.Timezone).
func validateTimezone(tz string) error {
	if tz == "" {
		return nil
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("config: timezone %q: %w", tz, err)
	}
	return nil
}

// Location resolves Timezone (see its doc comment) - the ONE calendar zone
// source for kahyad (G00 C-22). It never errors: Load already validated
// the name; a defensive fallback to time.Local keeps a caller that builds
// Config by hand (tests) working.
func (c Config) Location() *time.Location {
	if c.Timezone == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}

// defaults returns Config's built-in defaults for env ("prod", "dev", or
// any other validateEnv-accepted profile name - Load always passes a value
// validateEnv would accept; defaults itself does not re-validate it).
// W4-07/W78-02 D1: any non-"prod" env derives an entirely separate profile -
// data_dir/memory_dir/kahya_dir/backup_dir/socket all move to their own
// "-<name>"-suffixed paths (e.g. ~/Library/Application Support/Kahya-dev,
// ~/Kahya-dev, kahyad-dev.sock; ~/Library/Application Support/Kahya-redteam,
// kahyad-redteam.sock) - so a non-prod-profile kahyad never shares a single
// file on disk with a production one, by construction, before any override
// layer even runs. env=="prod" (or the empty string Load normalizes to it)
// keeps the exact production paths, byte-identical to what they always were.
func defaults(home, env string) Config {
	dataDirName, kahyaDirName := "Kahya", "Kahya"
	if env != EnvProd {
		// Any non-prod profile (dev, redteam, restore, ...) gets an entirely
		// separate "Kahya-<name>" data/memory tree (W78-02 D1 - generic on
		// purpose so W78-05's restore scratch profile reuses it). PROD
		// ("Kahya") is untouched, byte-identical to what it always was.
		dataDirName, kahyaDirName = "Kahya-"+env, "Kahya-"+env
	}
	dataDir := filepath.Join(home, "Library", "Application Support", dataDirName)
	kahyaDir := filepath.Join(home, kahyaDirName)
	return Config{
		DataDir:                  dataDir,
		Socket:                   filepath.Join(dataDir, socketFileName(env)),
		LogDir:                   filepath.Join(dataDir, "logs"),
		DBPath:                   filepath.Join(dataDir, "brain.db"),
		MemoryDir:                filepath.Join(kahyaDir, "memory"),
		EvalRetrievalDatasetPath: filepath.Join(kahyaDir, "eval", "retrieval", "dataset.jsonl"),
		AnthropicUpstreamURL:     "https://api.anthropic.com",
		EmbedPort:                8092,
		DefaultModel:             DefaultModelID,
		TaskTimeoutMin:           30,
		ActiveEmbedModelVer:      "qwen3-embedding-0.6b:512:v1",
		LogLevel:                 "info",
		UndoWindowSeconds:        300,
		// FIX-MEM-8: recency off until the operator opts in and a green
		// retrieval eval covers the recency config.
		SearchRecencyWeight:       0,
		SearchRecencyHalfLifeDays: DefaultSearchRecencyHalfLifeDays,
		// FIX-U-3: memory_write's secret-lane redirect target.
		SecretLaneMemoryDir: DefaultSecretLaneMemoryDir,
		// B-1 approval loop defaults (config.Config's own doc comments).
		ApprovalWaitSeconds:     300,
		AutonomyBootstrapLevels: map[string]int{"memory_search/R/global": 1},
		Env:                     env,
		WorkerCmd:               defaultWorkerCmd(),
		EmbedCmd:                defaultEmbedCmd(),
		MCPBridgePath:           defaultMCPBridgePath(),
		PolicyPath:              defaultPolicyPath(),
		EvalMiniBaselinePath:    defaultEvalMiniBaselinePath(),
		EgressPort:              3128,
		DockerImageTag:          "kahya-sandbox:0.1.0",
		DockerImageDigestPath:   defaultDockerImageDigestPath(),
		EgressSidecarDigestPath: defaultEgressSidecarDigestPath(),

		// W12-08 cost governor defaults (HANDOFF S4 flag, verbatim).
		DailyBudgetUSD:         10,
		MonthlyBudgetUSD:       150,
		TaskTokenCeiling:       500000,
		DowngradeAtRatio:       0.8,
		CacheHitAlarmThreshold: 0.5,
		CredentialMode:         CredentialModePassthrough,
		// BLOCKER 2's fail-closed reservation fallback (see the field's own
		// doc comment) - a conservative, committed default: big enough to
		// not fire on every ordinary call, small enough that a handful of
		// concurrently-reserved requests still can't blow far past the 500K
		// per-task ceiling before RecordUsage reconciles them.
		EstRequestTokens: 50_000,

		// W3-08 secret-lane local Qwen3-30B-A3B server defaults (task spec,
		// verbatim: port 8765 - NOT 8080/ComfyUI; idle_ttl 10 minutes).
		QwenCmd:            defaultQwenCmd(),
		QwenModelPath:      defaultQwenModelPath(home),
		QwenModelName:      "mlx-community/Qwen3-30B-A3B-4bit",
		QwenPort:           8765,
		QwenIdleTTLSeconds: 600,
		// FIX-P-4: the warm classification budget, HANDOFF S4 "<300ms".
		QwenClassifyBudgetMS: 300,

		// W4-01 scheduler defaults: no jobs declared out of the box (see
		// Config.Jobs's doc comment); TriggerBinPath follows the same
		// repo-root derivation as every other default*Path helper below.
		TriggerBinPath: defaultTriggerBinPath(),

		// W4-02 task durability default (task spec, verbatim: "default 3").
		TaskRetryW1MaxAuto: 3,
		// FIX-I-2 crash-resume cap (owner decision G10-2: 3).
		TaskMaxCrashResumes: 3,

		// W4-04 cloud-call error taxonomy defaults (task spec, verbatim):
		// max_inline=3; task_schedule 1m,5m,15m,60m (then hourly - see the
		// field's own doc comment for why re-using the last entry IS
		// "hourly" here); give_up_after=24h.
		CloudRetryMaxInline:    3,
		CloudRetryTaskSchedule: []string{"1m", "5m", "15m", "60m"},
		CloudRetryGiveUpAfter:  "24h",

		// W4-06 backups: KahyaDir/BackupDir follow MemoryDir's own
		// ~/Kahya (or, under KAHYA_ENV=dev, ~/Kahya-dev) derivation above
		// (KahyaDir = filepath.Dir(MemoryDir) in every default deployment);
		// backup-nightly/memory-push are the first two real cfg.Jobs
		// entries this codebase ships (task spec step 4, verbatim times) —
		// Config.Jobs's own doc comment names this task as the one that
		// adds them.
		KahyaDir:  kahyaDir,
		BackupDir: filepath.Join(kahyaDir, "backups"),
		Jobs: []JobConfig{
			{Name: "backup-nightly", Handler: "backup-nightly", Calendar: CalendarSpec{Hour: intPtr(3), Minute: intPtr(30)}},
			{Name: "memory-push", Handler: "memory-push", Calendar: CalendarSpec{Hour: intPtr(3), Minute: intPtr(45)}},
			// W5-01 morning briefing (HANDOFF §4 stack ⚑ scheduling: 08:30
			// briefing via launchd StartCalendarInterval). Hour/Minute are
			// the SAME defaultBriefingHour/Minute constants BriefingHour/
			// BriefingMinute below default to, kept in sync by construction
			// (both literals, never one field reading another in this same
			// composite literal).
			{Name: "morning-briefing", Handler: "morning-briefing", Calendar: CalendarSpec{Hour: intPtr(defaultBriefingHour), Minute: intPtr(defaultBriefingMinute)}},
			// W5-02 nightly consolidation (HANDOFF §6 W5 ⚑: 03:00 launchd
			// StartCalendarInterval job - never an in-daemon cron tick, same
			// wall-clock-scheduling rule backup-nightly/memory-push/
			// morning-briefing above already follow).
			{Name: "nightly-consolidation", Handler: "nightly-consolidation", Calendar: CalendarSpec{Hour: intPtr(3), Minute: intPtr(0)}},
			// G13 MEM-3 nightly knowledge extraction, 30 minutes before the
			// consolidation (the scheduler serializes runs per job NAME only, so
			// the earlier slot keeps the two from sharing the local model on a
			// normal night). extraction_enabled gates the handler, not this entry.
			{Name: "fact-extraction", Handler: "fact-extraction", Calendar: CalendarSpec{Hour: intPtr(2), Minute: intPtr(30)}},
			// W5-03 weekly truth ritual (HANDOFF §6 W5 ⚑: "haftalik
			// dogru/yanlis rituelinin hafif surumu"). Weekday follows
			// launchd's own StartCalendarInterval convention (0 = Sunday);
			// Hour/Minute are the SAME defaultRitualWeeklyHour/Minute
			// constants RitualWeeklyHour/RitualWeeklyMinute below default
			// to, kept in sync by construction (both literals, matching
			// morning-briefing's own comment above).
			{Name: "truth-ritual", Handler: "truth-ritual", Calendar: CalendarSpec{Weekday: intPtr(defaultRitualWeekday), Hour: intPtr(defaultRitualWeeklyHour), Minute: intPtr(defaultRitualWeeklyMinute)}},
		},

		// W4-05 ledger external anchor defaults (task spec, verbatim:
		// interval_hours default 6). AnchorRemote/AnchorLocalFallbackPath
		// default empty - see their own doc comments.
		AnchorIntervalHours: 6,

		// W4-07 acceptance-gate tick intervals (unchanged production
		// cadence - see each field's own doc comment).
		ResumeScanIntervalSeconds:     defaultResumeScanIntervalSeconds,
		OutboxDispatchIntervalSeconds: defaultOutboxDispatchIntervalSeconds,

		// W5-01 morning briefing defaults (task spec: hour/minute 08:30;
		// gh repo list/file globs/calendar names default empty - an
		// operator opts each source in explicitly via config.yaml; an
		// empty list degrades that section to zero collected items, never
		// an error - see each field's own doc comment).
		BriefingHour:   defaultBriefingHour,
		BriefingMinute: defaultBriefingMinute,
		// FIX-P-23: changed-file cap per briefing.
		BriefingMaxFileItems: 50,
		// FIX-P-14: "" = the daemon's time.Local (Config.Location).
		Timezone: "",

		// W5-02 nightly consolidation: suggestion mode is the ONLY mode for
		// the first 2 weeks, unconditionally (HANDOFF §6 W5 ⚑) - kahyad
		// refuses true here anyway (at run time) without a recent
		// eval.mini.pass ledger event, but the default itself is also false
		// so an operator must opt in explicitly even once that gate would
		// pass.
		ConsolidationAutoCommit: false,
		// FIX-B-5 batching defaults (see the fields' doc comments).
		ConsolidationCloudBatchTokenBudget: DefaultConsolidationCloudBatchTokenBudget,
		ConsolidationLocalBatchTokenBudget: DefaultConsolidationLocalBatchTokenBudget,
		ConsolidationMaxFileBytes:          DefaultConsolidationMaxFileBytes,
		ConsolidationMaxBatchesPerLane:     DefaultConsolidationMaxBatchesPerLane,
		ConsolidationChangedFilesFirst:     true,

		// G13 MEM-3 knowledge extraction (decision MEM-3.c).
		ExtractionEnabled:          true,
		ExtractionBatchMaxEpisodes: DefaultExtractionBatchMaxEpisodes,
		ExtractionChunksPerCall:    DefaultExtractionChunksPerCall,

		// W5-03 weekly truth ritual defaults (task spec: Sunday 18:00 -
		// HANDOFF §6 W5 flag).
		RitualWeeklyWeekday: defaultRitualWeekday,
		RitualWeeklyHour:    defaultRitualWeeklyHour,
		RitualWeeklyMinute:  defaultRitualWeeklyMinute,

		// W6-01 Hammerspoon palette default (task spec, verbatim).
		HsCliPath: defaultHsCliPath,

		// W6-05 TTS defaults (task spec, verbatim: enabled=false,
		// voice=Yelda, say_bin=/usr/bin/say, max_chars=280,
		// speak_secret_lane=false).
		TTSEnabled:         false,
		TTSVoice:           defaultTTSVoice,
		TTSSayBin:          defaultTTSSayBin,
		TTSMaxChars:        defaultTTSMaxChars,
		TTSSpeakSecretLane: false,
	}
}

// defaultBriefingHour/defaultBriefingMinute are the W5-01 morning-briefing
// job's wall-clock default (HANDOFF §6 W5, verbatim "08:30 brifingi") -
// named constants so the "morning-briefing" cfg.Jobs entry above and
// Config.BriefingHour/BriefingMinute's own defaults can never drift apart
// (both read this exact pair, never a separately hand-typed literal).
const (
	defaultBriefingHour   = 8
	defaultBriefingMinute = 30
)

// defaultRitualWeekday/Hour/Minute are the W5-03 weekly-truth-ritual job's
// wall-clock default (HANDOFF §6 W5, verbatim "haftalik dogru/yanlis
// rituelinin hafif surumu": Sunday 18:00) - named constants so the
// "truth-ritual" cfg.Jobs entry above and Config.RitualWeeklyWeekday/
// Hour/Minute's own defaults can never drift apart, mirroring
// defaultBriefingHour/Minute's identical rationale. Weekday follows
// launchd's own StartCalendarInterval convention (0 = Sunday).
const (
	defaultRitualWeekday      = 0
	defaultRitualWeeklyHour   = 18
	defaultRitualWeeklyMinute = 0
)

// defaultHsCliPath is config key ui.hs_cli's own default (W6-01, task
// spec verbatim) - kept as a literal, rather than importing
// kahyad/internal/ui just for its DefaultHsCliPath constant, matching
// this codebase's existing precedent of small, independently-owned
// literals kept in sync by hand across packages rather than one taking a
// dependency on the other purely for a constant (see Config.HsCliPath's
// own doc comment).
const defaultHsCliPath = "/opt/homebrew/bin/hs"

// defaultTTSVoice/defaultTTSSayBin/defaultTTSMaxChars are cfg.tts.voice/
// say_bin/max_chars's own defaults (W6-05, task spec verbatim: "Yelda",
// "/usr/bin/say", 280).
const (
	defaultTTSVoice    = "Yelda"
	defaultTTSSayBin   = "/usr/bin/say"
	defaultTTSMaxChars = 280
)

// intPtr returns a pointer to v — CalendarSpec's fields are all *int (nil
// means launchd's own StartCalendarInterval "every" semantics), so
// defaults' own backup-nightly/memory-push entries need a literal-friendly
// way to populate an explicit Hour/Minute.
func intPtr(v int) *int { return &v }

// defaultTriggerBinPath resolves the default "<repo>/bin/kahya-trigger"
// path, using the exact same repo-root derivation as defaultMCPBridgePath/
// defaultPolicyPath (see defaultWorkerCmd's doc comment).
func defaultTriggerBinPath() string {
	repoRoot := "."
	if exe, err := os.Executable(); err == nil {
		repoRoot = filepath.Dir(filepath.Dir(exe))
	}
	return filepath.Join(repoRoot, "bin", "kahya-trigger")
}

// defaultQwenCmd resolves the W3-08 base invocation
// ["<repo>/mlx/qwen/.venv/bin/python", "-m", "mlx_lm.server"], using the
// exact same repo-root derivation as defaultWorkerCmd/defaultEmbedCmd (see
// defaultWorkerCmd's doc comment) - mlx_lm.server is a THIRD-PARTY CLI
// module (unlike mlx/embed/server.py, this repo's own script), so this
// default deliberately stops at the base command; --model/--host/--port
// are appended by main.go at wiring time from QwenModelPath/QwenPort.
func defaultQwenCmd() []string {
	repoRoot := "."
	if exe, err := os.Executable(); err == nil {
		repoRoot = filepath.Dir(filepath.Dir(exe))
	}
	return []string{
		filepath.Join(repoRoot, "mlx", "qwen", ".venv", "bin", "python"),
		"-m", "mlx_lm.server",
	}
}

// defaultQwenModelPath resolves the pinned Qwen3-30B-A3B-4bit snapshot's
// on-disk path under the default Hugging Face cache layout (docs/
// models.md: `mlx-community/Qwen3-30B-A3B-4bit`, revision
// `d388dead1515f5e085ef7a0431dd8fadf0886c57`) - pointing mlx_lm.server
// directly at this exact snapshot directory (rather than passing the bare
// repo id) pins the EXACT revision docs/models.md committed to and works
// fully offline, matching W0-03's own download layout.
func defaultQwenModelPath(home string) string {
	return filepath.Join(home, ".cache", "huggingface", "hub",
		"models--mlx-community--Qwen3-30B-A3B-4bit", "snapshots",
		"d388dead1515f5e085ef7a0431dd8fadf0886c57")
}

// defaultWorkerCmd resolves the W12-07 step-fixed default
// ["<repo>/worker/.venv/bin/python","-m","kahya_worker"]. "<repo>" is
// derived from the running executable's own path rather than hardcoded:
// install-agent places the built binary at "<repo>/bin/kahyad" (see the
// launchd plist's __KAHYA_REPO_ROOT__/bin/kahyad substitution), so two
// directories up from os.Executable() is the repo root in every
// production deployment. If the executable's path cannot be resolved
// (should not happen outside of unusual sandboxes), "." is used as a
// last-resort repo root - Load() never fails just because this default
// couldn't be perfectly resolved, since every real caller either runs the
// installed binary (where the derivation is exact) or overrides
// worker_cmd explicitly via config.yaml (as every W12-07 test and the
// manual fake-worker verification flow do).
func defaultWorkerCmd() []string {
	repoRoot := "."
	if exe, err := os.Executable(); err == nil {
		repoRoot = filepath.Dir(filepath.Dir(exe))
	}
	return []string{
		filepath.Join(repoRoot, "worker", ".venv", "bin", "python"),
		"-m", "kahya_worker",
	}
}

// defaultEmbedCmd resolves the W12-11 step-fixed default
// ["<repo>/mlx/embed/.venv/bin/python","<repo>/mlx/embed/server.py"],
// using the exact same repo-root derivation as defaultWorkerCmd (see its
// doc comment). Unlike the worker (invoked as `-m kahya_worker`, a
// package import that needs no absolute script path), the embed service
// is a plain script, so BOTH argv entries are absolute paths here.
func defaultEmbedCmd() []string {
	repoRoot := "."
	if exe, err := os.Executable(); err == nil {
		repoRoot = filepath.Dir(filepath.Dir(exe))
	}
	return []string{
		filepath.Join(repoRoot, "mlx", "embed", ".venv", "bin", "python"),
		filepath.Join(repoRoot, "mlx", "embed", "server.py"),
	}
}

// defaultMCPBridgePath resolves the default "<repo>/bin/kahya-mcp" path,
// using the exact same repo-root derivation as defaultWorkerCmd (see its
// doc comment) - both the worker's venv python and the kahya-mcp bridge
// binary live at fixed, predictable locations relative to the installed
// kahyad binary.
func defaultMCPBridgePath() string {
	repoRoot := "."
	if exe, err := os.Executable(); err == nil {
		repoRoot = filepath.Dir(filepath.Dir(exe))
	}
	return filepath.Join(repoRoot, "bin", "kahya-mcp")
}

// defaultPolicyPath resolves the default "<repo>/policy.yaml" path, using
// the exact same repo-root derivation as defaultWorkerCmd/defaultEmbedCmd/
// defaultMCPBridgePath (see defaultWorkerCmd's doc comment) - policy.yaml
// lives at the repo root itself (committed, not under bin/), alongside
// every other top-level project file.
func defaultPolicyPath() string {
	repoRoot := "."
	if exe, err := os.Executable(); err == nil {
		repoRoot = filepath.Dir(filepath.Dir(exe))
	}
	return filepath.Join(repoRoot, "policy.yaml")
}

// defaultEvalMiniBaselinePath resolves the default "<repo>/eval/
// mini-baseline.jsonl" path, using the exact same repo-root derivation as
// defaultPolicyPath (see defaultWorkerCmd's doc comment) - the mini-eval
// baseline lives at the repo root's eval/ directory, committed alongside
// every other top-level project file.
func defaultEvalMiniBaselinePath() string {
	repoRoot := "."
	if exe, err := os.Executable(); err == nil {
		repoRoot = filepath.Dir(filepath.Dir(exe))
	}
	return filepath.Join(repoRoot, "eval", "mini-baseline.jsonl")
}

// defaultDockerImageDigestPath resolves the default "<repo>/docker/
// sandbox/IMAGE_DIGEST" path, using the exact same repo-root derivation as
// defaultWorkerCmd/defaultEmbedCmd/defaultMCPBridgePath/defaultPolicyPath
// (see defaultWorkerCmd's doc comment).
func defaultDockerImageDigestPath() string {
	repoRoot := "."
	if exe, err := os.Executable(); err == nil {
		repoRoot = filepath.Dir(filepath.Dir(exe))
	}
	return filepath.Join(repoRoot, "docker", "sandbox", "IMAGE_DIGEST")
}

// defaultEgressSidecarDigestPath resolves the default "<repo>/docker/
// egress/IMAGE_DIGEST" path (W3-05), using the exact same repo-root
// derivation as defaultDockerImageDigestPath (see defaultWorkerCmd's doc
// comment).
func defaultEgressSidecarDigestPath() string {
	repoRoot := "."
	if exe, err := os.Executable(); err == nil {
		repoRoot = filepath.Dir(filepath.Dir(exe))
	}
	return filepath.Join(repoRoot, "docker", "egress", "IMAGE_DIGEST")
}

func loadFile(path string) (fileConfig, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileConfig{}, false, nil
		}
		return fileConfig{}, false, fmt.Errorf("config: read %s: %w", path, err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(b, &fc); err != nil {
		return fileConfig{}, false, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return fc, true, nil
}

func applyFile(cfg *Config, fc fileConfig, home string, explicitSocket, explicitLogDir, explicitDBPath *bool) {
	if fc.DataDir != nil {
		cfg.DataDir = expandHome(*fc.DataDir, home)
	}
	if fc.Socket != nil {
		cfg.Socket = expandHome(*fc.Socket, home)
		*explicitSocket = true
	}
	if fc.LogDir != nil {
		cfg.LogDir = expandHome(*fc.LogDir, home)
		*explicitLogDir = true
	}
	if fc.DBPath != nil {
		cfg.DBPath = expandHome(*fc.DBPath, home)
		*explicitDBPath = true
	}
	if fc.MemoryDir != nil {
		cfg.MemoryDir = expandHome(*fc.MemoryDir, home)
	}
	if fc.AnthropicUpstreamURL != nil {
		cfg.AnthropicUpstreamURL = *fc.AnthropicUpstreamURL
	}
	if fc.EmbedPort != nil {
		cfg.EmbedPort = *fc.EmbedPort
	}
	if fc.DefaultModel != nil {
		cfg.DefaultModel = *fc.DefaultModel
	}
	if fc.TaskTimeoutMin != nil {
		cfg.TaskTimeoutMin = *fc.TaskTimeoutMin
	}
	if fc.ActiveEmbedModelVer != nil {
		cfg.ActiveEmbedModelVer = *fc.ActiveEmbedModelVer
	}
	if fc.LogLevel != nil {
		cfg.LogLevel = *fc.LogLevel
	}
	if fc.UndoWindowSeconds != nil {
		cfg.UndoWindowSeconds = *fc.UndoWindowSeconds
	}
	if fc.ApprovalWaitSeconds != nil {
		cfg.ApprovalWaitSeconds = *fc.ApprovalWaitSeconds
	}
	if fc.AutonomyBootstrapLevels != nil {
		cfg.AutonomyBootstrapLevels = *fc.AutonomyBootstrapLevels
	}
	if fc.WorkerCmd != nil {
		cfg.WorkerCmd = *fc.WorkerCmd
	}
	if fc.EmbedCmd != nil {
		cfg.EmbedCmd = *fc.EmbedCmd
	}
	if fc.MCPBridgePath != nil {
		cfg.MCPBridgePath = expandHome(*fc.MCPBridgePath, home)
	}
	if fc.PolicyPath != nil {
		cfg.PolicyPath = expandHome(*fc.PolicyPath, home)
	}
	if fc.EvalMiniBaselinePath != nil {
		cfg.EvalMiniBaselinePath = expandHome(*fc.EvalMiniBaselinePath, home)
	}
	if fc.EvalRetrievalDatasetPath != nil {
		cfg.EvalRetrievalDatasetPath = expandHome(*fc.EvalRetrievalDatasetPath, home)
	}
	if fc.SearchRecencyWeight != nil {
		cfg.SearchRecencyWeight = *fc.SearchRecencyWeight
	}
	if fc.SearchRecencyHalfLifeDays != nil {
		cfg.SearchRecencyHalfLifeDays = *fc.SearchRecencyHalfLifeDays
	}
	if fc.SecretLaneMemoryDir != nil {
		cfg.SecretLaneMemoryDir = *fc.SecretLaneMemoryDir
	}
	if fc.EgressPort != nil {
		cfg.EgressPort = *fc.EgressPort
	}
	if fc.DockerImageTag != nil {
		cfg.DockerImageTag = *fc.DockerImageTag
	}
	if fc.DockerImageDigestPath != nil {
		cfg.DockerImageDigestPath = expandHome(*fc.DockerImageDigestPath, home)
	}
	if fc.EgressSidecarDigestPath != nil {
		cfg.EgressSidecarDigestPath = expandHome(*fc.EgressSidecarDigestPath, home)
	}
	if fc.ShellWorkdirRoots != nil {
		cfg.ShellWorkdirRoots = expandHomeEach(*fc.ShellWorkdirRoots, home)
	}
	if fc.DailyBudgetUSD != nil {
		cfg.DailyBudgetUSD = *fc.DailyBudgetUSD
	}
	if fc.MonthlyBudgetUSD != nil {
		cfg.MonthlyBudgetUSD = *fc.MonthlyBudgetUSD
	}
	if fc.TaskTokenCeiling != nil {
		cfg.TaskTokenCeiling = *fc.TaskTokenCeiling
	}
	if fc.DowngradeAtRatio != nil {
		cfg.DowngradeAtRatio = *fc.DowngradeAtRatio
	}
	if fc.CacheHitAlarmThreshold != nil {
		cfg.CacheHitAlarmThreshold = *fc.CacheHitAlarmThreshold
	}
	if fc.CredentialMode != nil {
		cfg.CredentialMode = *fc.CredentialMode
	}
	if fc.EstRequestTokens != nil {
		cfg.EstRequestTokens = *fc.EstRequestTokens
	}
	if fc.TelegramChatID != nil {
		cfg.TelegramChatID = *fc.TelegramChatID
	}
	if fc.TelegramUserID != nil {
		cfg.TelegramUserID = *fc.TelegramUserID
	}
	if fc.TelegramAPIURL != nil {
		cfg.TelegramAPIURL = *fc.TelegramAPIURL
	}
	if fc.QwenCmd != nil {
		cfg.QwenCmd = *fc.QwenCmd
	}
	if fc.QwenModelPath != nil {
		cfg.QwenModelPath = expandHome(*fc.QwenModelPath, home)
	}
	if fc.QwenModelName != nil {
		cfg.QwenModelName = *fc.QwenModelName
	}
	if fc.QwenPort != nil {
		cfg.QwenPort = *fc.QwenPort
	}
	if fc.QwenIdleTTLSeconds != nil {
		cfg.QwenIdleTTLSeconds = *fc.QwenIdleTTLSeconds
	}
	if fc.QwenClassifyBudgetMS != nil {
		cfg.QwenClassifyBudgetMS = *fc.QwenClassifyBudgetMS
	}
	if fc.Jobs != nil {
		cfg.Jobs = *fc.Jobs
	}
	if fc.TriggerBinPath != nil {
		cfg.TriggerBinPath = expandHome(*fc.TriggerBinPath, home)
	}
	if fc.TaskRetryW1MaxAuto != nil {
		cfg.TaskRetryW1MaxAuto = *fc.TaskRetryW1MaxAuto
	}
	if fc.TaskMaxCrashResumes != nil {
		cfg.TaskMaxCrashResumes = *fc.TaskMaxCrashResumes
	}
	if fc.CloudRetryMaxInline != nil {
		cfg.CloudRetryMaxInline = *fc.CloudRetryMaxInline
	}
	if fc.CloudRetryTaskSchedule != nil {
		cfg.CloudRetryTaskSchedule = *fc.CloudRetryTaskSchedule
	}
	if fc.CloudRetryGiveUpAfter != nil {
		cfg.CloudRetryGiveUpAfter = *fc.CloudRetryGiveUpAfter
	}
	if fc.KahyaDir != nil {
		cfg.KahyaDir = expandHome(*fc.KahyaDir, home)
	}
	if fc.BackupDir != nil {
		cfg.BackupDir = expandHome(*fc.BackupDir, home)
	}
	if fc.AnchorRemote != nil {
		cfg.AnchorRemote = *fc.AnchorRemote
	}
	if fc.AnchorIntervalHours != nil {
		cfg.AnchorIntervalHours = *fc.AnchorIntervalHours
	}
	if fc.AnchorLocalFallbackPath != nil {
		cfg.AnchorLocalFallbackPath = expandHome(*fc.AnchorLocalFallbackPath, home)
	}
	if fc.ResumeScanIntervalSeconds != nil {
		cfg.ResumeScanIntervalSeconds = *fc.ResumeScanIntervalSeconds
	}
	if fc.OutboxDispatchIntervalSeconds != nil {
		cfg.OutboxDispatchIntervalSeconds = *fc.OutboxDispatchIntervalSeconds
	}
	if fc.BriefingHour != nil {
		cfg.BriefingHour = *fc.BriefingHour
	}
	if fc.BriefingMinute != nil {
		cfg.BriefingMinute = *fc.BriefingMinute
	}
	if fc.BriefingGHRepos != nil {
		cfg.BriefingGHRepos = *fc.BriefingGHRepos
	}
	if fc.BriefingFileGlobs != nil {
		cfg.BriefingFileGlobs = expandHomeEach(*fc.BriefingFileGlobs, home)
	}
	if fc.BriefingCalendarNames != nil {
		cfg.BriefingCalendarNames = *fc.BriefingCalendarNames
	}
	if fc.BriefingMaxFileItems != nil {
		cfg.BriefingMaxFileItems = *fc.BriefingMaxFileItems
	}
	if fc.Timezone != nil {
		cfg.Timezone = *fc.Timezone
	}
	if fc.ConsolidationAutoCommit != nil {
		cfg.ConsolidationAutoCommit = *fc.ConsolidationAutoCommit
	}
	if fc.ConsolidationCloudBatchTokens != nil {
		cfg.ConsolidationCloudBatchTokenBudget = *fc.ConsolidationCloudBatchTokens
	}
	if fc.ConsolidationLocalBatchTokens != nil {
		cfg.ConsolidationLocalBatchTokenBudget = *fc.ConsolidationLocalBatchTokens
	}
	if fc.ConsolidationMaxFileBytes != nil {
		cfg.ConsolidationMaxFileBytes = *fc.ConsolidationMaxFileBytes
	}
	if fc.ConsolidationMaxBatches != nil {
		cfg.ConsolidationMaxBatchesPerLane = *fc.ConsolidationMaxBatches
	}
	if fc.ExtractionEnabled != nil {
		cfg.ExtractionEnabled = *fc.ExtractionEnabled
	}
	if fc.ExtractionBatchMaxEpisodes != nil {
		cfg.ExtractionBatchMaxEpisodes = *fc.ExtractionBatchMaxEpisodes
	}
	if fc.ExtractionChunksPerCall != nil {
		cfg.ExtractionChunksPerCall = *fc.ExtractionChunksPerCall
	}
	if fc.ConsolidationChangedFirst != nil {
		cfg.ConsolidationChangedFilesFirst = *fc.ConsolidationChangedFirst
	}
	if fc.RitualWeeklyWeekday != nil {
		cfg.RitualWeeklyWeekday = *fc.RitualWeeklyWeekday
	}
	if fc.RitualWeeklyHour != nil {
		cfg.RitualWeeklyHour = *fc.RitualWeeklyHour
	}
	if fc.HsCliPath != nil {
		cfg.HsCliPath = expandHome(*fc.HsCliPath, home)
	}
	if fc.RitualWeeklyMinute != nil {
		cfg.RitualWeeklyMinute = *fc.RitualWeeklyMinute
	}
	if fc.TTSEnabled != nil {
		cfg.TTSEnabled = *fc.TTSEnabled
	}
	if fc.TTSVoice != nil {
		cfg.TTSVoice = *fc.TTSVoice
	}
	if fc.TTSSayBin != nil {
		cfg.TTSSayBin = expandHome(*fc.TTSSayBin, home)
	}
	if fc.TTSMaxChars != nil {
		cfg.TTSMaxChars = *fc.TTSMaxChars
	}
	if fc.TTSSpeakSecretLane != nil {
		cfg.TTSSpeakSecretLane = *fc.TTSSpeakSecretLane
	}
}

func applyEnv(cfg *Config, home string, explicitSocket, explicitLogDir, explicitDBPath *bool) {
	if v := os.Getenv("KAHYA_DATA_DIR"); v != "" {
		cfg.DataDir = expandHome(v, home)
	}
	if v := os.Getenv("KAHYA_SOCKET"); v != "" {
		cfg.Socket = expandHome(v, home)
		*explicitSocket = true
	}
	if v := os.Getenv("KAHYA_MEMORY_DIR"); v != "" {
		cfg.MemoryDir = expandHome(v, home)
	}
	if v := os.Getenv("KAHYA_DB_PATH"); v != "" {
		cfg.DBPath = expandHome(v, home)
		*explicitDBPath = true
	}
	// KAHYA_LOG_DIR is intentionally not part of the IPC/env contract (only
	// KAHYA_DATA_DIR, KAHYA_SOCKET, KAHYA_MEMORY_DIR, KAHYA_DB_PATH, and
	// KAHYA_ENV are); log_dir only moves via config.yaml or by following a
	// data_dir change.
	if v := os.Getenv("KAHYA_ENV"); v != "" {
		cfg.Env = v
	}
	if v := os.Getenv("KAHYA_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("KAHYA_POLICY_PATH"); v != "" {
		cfg.PolicyPath = expandHome(v, home)
	}
	if v := os.Getenv("KAHYA_EVAL_MINI_BASELINE_PATH"); v != "" {
		cfg.EvalMiniBaselinePath = expandHome(v, home)
	}
	if v := os.Getenv("KAHYA_EVAL_RETRIEVAL_DATASET_PATH"); v != "" {
		cfg.EvalRetrievalDatasetPath = expandHome(v, home)
	}
	if v := os.Getenv("KAHYA_EGRESS_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.EgressPort = p
		}
	}
	if v := os.Getenv("KAHYA_DOCKER_IMAGE_TAG"); v != "" {
		cfg.DockerImageTag = v
	}
	if v := os.Getenv("KAHYA_DOCKER_IMAGE_DIGEST_PATH"); v != "" {
		cfg.DockerImageDigestPath = expandHome(v, home)
	}
	if v := os.Getenv("KAHYA_EGRESS_SIDECAR_DIGEST_PATH"); v != "" {
		cfg.EgressSidecarDigestPath = expandHome(v, home)
	}
	if v := os.Getenv("KAHYA_SHELL_WORKDIR_ROOTS"); v != "" {
		cfg.ShellWorkdirRoots = expandHomeEach(strings.Split(v, ","), home)
	}
	if v := os.Getenv("KAHYA_TELEGRAM_CHAT_ID"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.TelegramChatID = id
		}
	}
	if v := os.Getenv("KAHYA_TELEGRAM_USER_ID"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.TelegramUserID = id
		}
	}
	if v := os.Getenv("KAHYA_TELEGRAM_API_URL"); v != "" {
		cfg.TelegramAPIURL = v
	}
	if v := os.Getenv("KAHYA_QWEN_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.QwenPort = p
		}
	}
	if v := os.Getenv("KAHYA_QWEN_MODEL_PATH"); v != "" {
		cfg.QwenModelPath = expandHome(v, home)
	}
	if v := os.Getenv("KAHYA_QWEN_IDLE_TTL_SECONDS"); v != "" {
		if s, err := strconv.Atoi(v); err == nil {
			cfg.QwenIdleTTLSeconds = s
		}
	}
	if v := os.Getenv("KAHYA_QWEN_CLASSIFY_BUDGET_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil {
			cfg.QwenClassifyBudgetMS = ms
		}
	}
	if v := os.Getenv("KAHYA_BRIEFING_MAX_FILE_ITEMS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.BriefingMaxFileItems = n
		}
	}
	if v := os.Getenv("KAHYA_TIMEZONE"); v != "" {
		cfg.Timezone = v
	}
	if v := os.Getenv("KAHYA_ANCHOR_REMOTE"); v != "" {
		cfg.AnchorRemote = v
	}
	if v := os.Getenv("KAHYA_ANCHOR_INTERVAL_HOURS"); v != "" {
		if h, err := strconv.Atoi(v); err == nil {
			cfg.AnchorIntervalHours = h
		}
	}
	if v := os.Getenv("KAHYA_ANCHOR_LOCAL_FALLBACK_PATH"); v != "" {
		cfg.AnchorLocalFallbackPath = expandHome(v, home)
	}
	if v := os.Getenv("KAHYA_UI_HS_CLI"); v != "" {
		cfg.HsCliPath = expandHome(v, home)
	}
}

// envNamePattern is the constraint a non-prod KAHYA_ENV profile name must
// satisfy (W78-02 D1): lowercase letters, digits, and hyphens only. This is
// deliberately strict enough that a profile name can NEVER contain a path
// separator, whitespace, "..", or any other rune that could make
// "Kahya-<name>" / "kahyad-<name>.sock" traverse out of its intended
// directory or collide with the prod paths - a malicious KAHYA_ENV export
// (e.g. "../Kahya", "prod ", "dev/../..") fails Load closed here rather than
// silently resolving somewhere dangerous.
var envNamePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// validateEnv fails Load closed (same posture as every other validate*
// function) on a malformed KAHYA_ENV. The empty string and "prod" both mean
// production (Load normalizes "" to EnvProd before this ever runs, but "" is
// accepted here too for total-ness); any other value must match
// envNamePattern to be usable as a "-<name>"-suffixed profile directory.
func validateEnv(env string) error {
	if env == "" || env == EnvProd {
		return nil
	}
	if !envNamePattern.MatchString(env) {
		return fmt.Errorf("config: KAHYA_ENV=%q invalid, must be %q or a profile name matching %s (lowercase letters/digits/hyphen only)", env, EnvProd, envNamePattern.String())
	}
	return nil
}

// validLogLevels are the only values Load accepts for LogLevel/
// KAHYA_LOG_LEVEL. logx maps these onto slog levels (see main.go); an
// invalid value fails Load closed, the same posture as validateEnv (MINOR
// 5 - fail-closed on unrecognized config, never silently default).
var validLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

func validateLogLevel(level string) error {
	if !validLogLevels[level] {
		return fmt.Errorf("config: log_level=%q invalid, must be one of debug|info|warn|error", level)
	}
	return nil
}

// validateCredentialMode fails Load closed (same posture as
// validateEnv/validateLogLevel) on any credential_mode value other than the
// two W12-08 defines - an unrecognized value must never silently fall back
// to either mode's behavior.
func validateCredentialMode(mode string) error {
	if mode != CredentialModeKeychain && mode != CredentialModePassthrough {
		return fmt.Errorf("config: credential_mode=%q invalid, must be %q or %q", mode, CredentialModeKeychain, CredentialModePassthrough)
	}
	return nil
}

// jobNamePattern is the DNS-label constraint cfg.jobs[].name must satisfy
// (task spec step 1) — letters/digits/hyphen, 1-63 chars, never
// starting/ending with a hyphen. kahyad/internal/scheduler renders a job
// name verbatim into a launchd Label (com.kahya.job.<name>) and a log
// filename (job-<name>.log), both of which need exactly this constraint;
// scheduler cannot depend back on this package's own validator (it
// already imports config), so this is the ONE place the constraint is
// enforced — at config.Load, fail-closed, before any job ever reaches
// kahyad/internal/scheduler at all.
var jobNamePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// validateJobs fails Load closed (same posture as validateEnv/
// validateLogLevel/validateCredentialMode) on any cfg.jobs entry with a
// non-DNS-label name, a duplicate name, or an empty handler.
func validateJobs(jobs []JobConfig) error {
	seen := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		if !jobNamePattern.MatchString(j.Name) {
			return fmt.Errorf("config: jobs[].name=%q invalid, must be DNS-label chars only (letters/digits/hyphen, 1-63 chars, no leading/trailing hyphen)", j.Name)
		}
		if seen[j.Name] {
			return fmt.Errorf("config: jobs[].name=%q declared more than once", j.Name)
		}
		seen[j.Name] = true
		if strings.TrimSpace(j.Handler) == "" {
			return fmt.Errorf("config: jobs[].handler must not be empty (job %q)", j.Name)
		}
	}
	return nil
}

// validateCloudRetry fails Load closed (same posture as validateEnv/
// validateLogLevel/validateCredentialMode/validateJobs) on an invalid
// W4-04 cloud-retry config: max_inline must be >= 1 (a call that can
// never even be attempted once is nonsensical), and every task_schedule
// entry plus give_up_after must be a valid time.ParseDuration string -
// kahyad/internal/task parses these at schedule-computation time, so a
// bad value must be caught here, at boot, rather than surfacing as a
// silent zero-delay retry loop deep inside a running task.
func validateCloudRetry(maxInline int, taskSchedule []string, giveUpAfter string) error {
	if maxInline < 1 {
		return fmt.Errorf("config: cloud_retry_max_inline=%d invalid, must be >= 1", maxInline)
	}
	for _, s := range taskSchedule {
		if _, err := time.ParseDuration(s); err != nil {
			return fmt.Errorf("config: cloud_retry_task_schedule entry %q invalid: %w", s, err)
		}
	}
	if len(taskSchedule) == 0 {
		return fmt.Errorf("config: cloud_retry_task_schedule must not be empty")
	}
	if _, err := time.ParseDuration(giveUpAfter); err != nil {
		return fmt.Errorf("config: cloud_retry_give_up_after=%q invalid: %w", giveUpAfter, err)
	}
	return nil
}

// validateTaskResilience fails Load closed (same posture as
// validateCloudRetry) on task_max_crash_resumes < 1 (FIX-I-2): a task that
// may never be resumed even once would turn every transient worker crash
// into blocked_user, and a non-positive cap cannot express "unlimited"
// (there is no unlimited - the cap is what stops the resume loop).
func validateTaskResilience(maxCrashResumes int) error {
	if maxCrashResumes < 1 {
		return fmt.Errorf("config: task_max_crash_resumes=%d invalid, must be >= 1", maxCrashResumes)
	}
	return nil
}

// validateAnchorIntervalHours fails Load closed (same posture as every
// other validate* function above) on anchor_interval_hours < 1 - the W4-05
// stale-pending alarm's own "2 x interval_hours" threshold (kahyad/
// internal/anchor.Pusher.checkStalePending) would be meaningless (or
// divide-by-zero-adjacent) for a non-positive interval.
func validateAnchorIntervalHours(hours int) error {
	if hours < 1 {
		return fmt.Errorf("config: anchor_interval_hours=%d invalid, must be >= 1", hours)
	}
	return nil
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// expandHomeEach applies expandHome to every entry of paths, trimming
// surrounding whitespace and dropping empty entries first (KAHYA_SHELL_
// WORKDIR_ROOTS is a comma-separated env value, so " /a , /b " must not
// yield a stray whitespace-only or empty root).
func expandHomeEach(paths []string, home string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, expandHome(p, home))
	}
	return out
}

// ExpandHome resolves a leading "~" or "~/" in path against the current
// user's home directory, applying the exact same expansion rule Load uses
// for every configured path (it delegates to the unexported expandHome).
// This is exported specifically so the kahya CLI's resolveSocket can expand
// a raw KAHYA_SOCKET env value identically to how Load's applyEnv expands
// it internally - without this, a "~/..." KAHYA_SOCKET value would make the
// CLI and kahyad dial two different paths. If the home directory cannot be
// resolved, path is returned unchanged (best-effort) rather than erroring;
// callers needing Load's stricter fail-closed posture should call Load
// itself.
func ExpandHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return expandHome(path, home)
}

func validateASCIIPaths(cfg Config) error {
	paths := map[string]string{
		"data_dir":   cfg.DataDir,
		"socket":     cfg.Socket,
		"log_dir":    cfg.LogDir,
		"db_path":    cfg.DBPath,
		"memory_dir": cfg.MemoryDir,
		"kahya_dir":  cfg.KahyaDir,
		"backup_dir": cfg.BackupDir,
	}
	// Deterministic order for reproducible error messages.
	for _, key := range []string{"data_dir", "socket", "log_dir", "db_path", "memory_dir", "kahya_dir", "backup_dir"} {
		p := paths[key]
		for _, r := range p {
			if r > 127 {
				return fmt.Errorf("config: %s=%q contains non-ASCII rune %q (HANDOFF §7: directory names must be ASCII)", key, p, r)
			}
		}
	}
	return nil
}

// ControlSecretHeader is the HTTP header every control-plane route of
// kahyad's UDS listener requires (G03 / B-3). Worker-plane routes
// (/policy/check, /v1/memory/search, /v1/mcp, /policy/consume-token)
// accept it too, but only need it from a caller kahyad cannot attribute
// to a live worker it spawned (G03 / B-14); /health never requires it.
// See kahyad/internal/server/controlauth.go and docs/ipc.md section 5a.
const ControlSecretHeader = "X-Kahya-Control-Secret"

// ControlSecretPath returns where kahyad writes the per-boot control
// secret for socketPath - "<socket>.secret", mirroring the
// "<socket>.lock" convention of server.AcquireStartupLock, so a dev
// profile (kahyad-dev.sock) automatically gets its own file. No config
// key exists for it on purpose: the path is derived from the socket path,
// which validateASCIIPaths already checks.
func ControlSecretPath(socketPath string) string { return socketPath + ".secret" }

// controlSecretHexLen is the exact length of a valid secret (32 random
// bytes, lowercase hex).
const controlSecretHexLen = 64

// ReadControlSecret reads and validates ControlSecretPath(socketPath).
// A missing file returns ("", nil) - the caller simply sends no header
// (an old daemon, or a fake test server, then decides). Any other read
// error, or malformed content, is returned as an error. This is a client
// convenience only: kahyad itself is the authority (it compares the
// header against the secret it holds in memory, never against the file).
func ReadControlSecret(socketPath string) (string, error) {
	path := ControlSecretPath(socketPath)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("config: read control secret: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if len(s) != controlSecretHexLen {
		return "", fmt.Errorf("config: control secret %s: want %d hex chars, got %d", path, controlSecretHexLen, len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("config: control secret %s: not hex: %w", path, err)
	}
	return s, nil
}
