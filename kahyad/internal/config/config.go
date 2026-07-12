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
	"fmt"
	"os"
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
	// rationale: the worker authenticates via its own Claude Code SDK
	// session, so kahyad injects no credential and simply forwards the
	// worker's own upstream auth header unchanged after validating the
	// per-task local token).
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
	// Pusher's `git -C <KahyaDir> push origin HEAD` target and
	// backup.EnsureGitignoreEntry's target directory. Kept as its OWN
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
	// block describes (`chflags sappnd`). Default "" (disabled).
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
	DataDir                       *string      `yaml:"data_dir"`
	Socket                        *string      `yaml:"socket"`
	LogDir                        *string      `yaml:"log_dir"`
	DBPath                        *string      `yaml:"db_path"`
	MemoryDir                     *string      `yaml:"memory_dir"`
	AnthropicUpstreamURL          *string      `yaml:"anthropic_upstream_url"`
	EmbedPort                     *int         `yaml:"embed_port"`
	DefaultModel                  *string      `yaml:"default_model"`
	TaskTimeoutMin                *int         `yaml:"task_timeout_min"`
	ActiveEmbedModelVer           *string      `yaml:"active_embed_model_ver"`
	LogLevel                      *string      `yaml:"log_level"`
	UndoWindowSeconds             *int         `yaml:"undo_window_seconds"`
	WorkerCmd                     *[]string    `yaml:"worker_cmd"`
	EmbedCmd                      *[]string    `yaml:"embed_cmd"`
	MCPBridgePath                 *string      `yaml:"mcp_bridge_path"`
	PolicyPath                    *string      `yaml:"policy_path"`
	EgressPort                    *int         `yaml:"egress_port"`
	DockerImageTag                *string      `yaml:"docker_image_tag"`
	DockerImageDigestPath         *string      `yaml:"docker_image_digest_path"`
	EgressSidecarDigestPath       *string      `yaml:"egress_sidecar_digest_path"`
	ShellWorkdirRoots             *[]string    `yaml:"shell_workdir_roots"`
	DailyBudgetUSD                *float64     `yaml:"daily_budget_usd"`
	MonthlyBudgetUSD              *float64     `yaml:"monthly_budget_usd"`
	TaskTokenCeiling              *int64       `yaml:"task_token_ceiling"`
	DowngradeAtRatio              *float64     `yaml:"downgrade_at_ratio"`
	CacheHitAlarmThreshold        *float64     `yaml:"cache_hit_alarm_threshold"`
	CredentialMode                *string      `yaml:"credential_mode"`
	EstRequestTokens              *int64       `yaml:"est_request_tokens"`
	TelegramChatID                *int64       `yaml:"telegram_chat_id"`
	TelegramUserID                *int64       `yaml:"telegram_user_id"`
	TelegramAPIURL                *string      `yaml:"telegram_api_url"`
	QwenCmd                       *[]string    `yaml:"qwen_cmd"`
	QwenModelPath                 *string      `yaml:"qwen_model_path"`
	QwenModelName                 *string      `yaml:"qwen_model_name"`
	QwenPort                      *int         `yaml:"qwen_port"`
	QwenIdleTTLSeconds            *int         `yaml:"qwen_idle_ttl_seconds"`
	Jobs                          *[]JobConfig `yaml:"jobs"`
	TriggerBinPath                *string      `yaml:"trigger_bin_path"`
	TaskRetryW1MaxAuto            *int         `yaml:"task_retry_w1_max_auto"`
	CloudRetryMaxInline           *int         `yaml:"cloud_retry_max_inline"`
	CloudRetryTaskSchedule        *[]string    `yaml:"cloud_retry_task_schedule"`
	CloudRetryGiveUpAfter         *string      `yaml:"cloud_retry_give_up_after"`
	KahyaDir                      *string      `yaml:"kahya_dir"`
	BackupDir                     *string      `yaml:"backup_dir"`
	AnchorRemote                  *string      `yaml:"anchor_remote"`
	AnchorIntervalHours           *int         `yaml:"anchor_interval_hours"`
	AnchorLocalFallbackPath       *string      `yaml:"anchor_local_fallback_path"`
	ResumeScanIntervalSeconds     *int         `yaml:"resume_scan_interval_seconds"`
	OutboxDispatchIntervalSeconds *int         `yaml:"outbox_dispatch_interval_seconds"`
}

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
	// separation (and refuseDevProfileOpeningProdDB below still guarantees
	// the DB path itself is never the prod one). At this point in the
	// pipeline data_dir has not yet been touched by the file or env layers,
	// so there is exactly one unambiguous place to look.
	fileCfgPath := filepath.Join(defaults(home, EnvProd).DataDir, "config.yaml")

	var explicitSocket, explicitLogDir, explicitDBPath bool

	if fc, ok, err := loadFile(fileCfgPath); err != nil {
		return Config{}, err
	} else if ok {
		applyFile(&cfg, fc, home, &explicitSocket, &explicitLogDir, &explicitDBPath)
	}

	applyEnv(&cfg, home, &explicitSocket, &explicitLogDir, &explicitDBPath)

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
	if err := validateAnchorIntervalHours(cfg.AnchorIntervalHours); err != nil {
		return Config{}, err
	}
	if err := validateTickIntervals(cfg.ResumeScanIntervalSeconds, cfg.OutboxDispatchIntervalSeconds); err != nil {
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
	if err := refuseDevProfileOpeningProdDB(cfg, home); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// socketFileName is the control-socket's own filename for env ("dev" gets
// its own kahyad-dev.sock, never plain kahyad.sock - HANDOFF §6 W7-8 ⚑
// dev-profile: a separate launchd label/socket per profile so a dev and a
// prod kahyad could in principle run side by side without colliding).
func socketFileName(env string) string {
	if env == EnvDev {
		return "kahyad-dev.sock"
	}
	return "kahyad.sock"
}

// refuseDevProfileOpeningProdDB implements the W4-07 HARD CONSTRAINT: fail
// Load closed when cfg.Env==dev but the fully-resolved cfg.DBPath is
// nonetheless the real production database path (defaults(home, EnvProd)'s
// own db_path) - see Load's own call-site comment for why this is checked
// last, against the final resolved value, not merely against whichever
// layer happened to set it.
func refuseDevProfileOpeningProdDB(cfg Config, home string) error {
	if cfg.Env != EnvDev {
		return nil
	}
	prodDBPath := defaults(home, EnvProd).DBPath
	if cfg.DBPath == prodDBPath {
		return fmt.Errorf("config: KAHYA_ENV=dev refuses to open the production brain.db (%s) - dev profile must use a separate db_path/KAHYA_DATA_DIR/KAHYA_DB_PATH", prodDBPath)
	}
	return nil
}

// defaultTickIntervalSeconds mirrors main.go's own pre-W4-07 hardcoded
// "@every 30s"/"@every 5s" tick specs - unchanged production cadence; only
// a hermetic gate's own config.yaml overlay ever sets these lower.
const (
	defaultResumeScanIntervalSeconds     = 30
	defaultOutboxDispatchIntervalSeconds = 5
)

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

// defaults returns Config's built-in defaults for env ("prod" or "dev" -
// Load always passes a value validateEnv would accept; defaults itself
// does not re-validate it). W4-07: env=="dev" derives an entirely separate
// profile - data_dir/memory_dir/kahya_dir/backup_dir/socket all move to
// their own "-dev"-suffixed paths (~/Library/Application Support/
// Kahya-dev, ~/Kahya-dev, kahyad-dev.sock) - so a dev-profile kahyad never
// shares a single file on disk with a production one, by construction,
// before any override layer even runs.
func defaults(home, env string) Config {
	dataDirName, kahyaDirName := "Kahya", "Kahya"
	if env == EnvDev {
		dataDirName, kahyaDirName = "Kahya-dev", "Kahya-dev"
	}
	dataDir := filepath.Join(home, "Library", "Application Support", dataDirName)
	kahyaDir := filepath.Join(home, kahyaDirName)
	return Config{
		DataDir:                 dataDir,
		Socket:                  filepath.Join(dataDir, socketFileName(env)),
		LogDir:                  filepath.Join(dataDir, "logs"),
		DBPath:                  filepath.Join(dataDir, "brain.db"),
		MemoryDir:               filepath.Join(kahyaDir, "memory"),
		AnthropicUpstreamURL:    "https://api.anthropic.com",
		EmbedPort:               8092,
		DefaultModel:            "claude-sonnet-5",
		TaskTimeoutMin:          30,
		ActiveEmbedModelVer:     "qwen3-embedding-0.6b:512:v1",
		LogLevel:                "info",
		UndoWindowSeconds:       300,
		Env:                     env,
		WorkerCmd:               defaultWorkerCmd(),
		EmbedCmd:                defaultEmbedCmd(),
		MCPBridgePath:           defaultMCPBridgePath(),
		PolicyPath:              defaultPolicyPath(),
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

		// W4-01 scheduler defaults: no jobs declared out of the box (see
		// Config.Jobs's doc comment); TriggerBinPath follows the same
		// repo-root derivation as every other default*Path helper below.
		TriggerBinPath: defaultTriggerBinPath(),

		// W4-02 task durability default (task spec, verbatim: "default 3").
		TaskRetryW1MaxAuto: 3,

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
		},

		// W4-05 ledger external anchor defaults (task spec, verbatim:
		// interval_hours default 6). AnchorRemote/AnchorLocalFallbackPath
		// default empty - see their own doc comments.
		AnchorIntervalHours: 6,

		// W4-07 acceptance-gate tick intervals (unchanged production
		// cadence - see each field's own doc comment).
		ResumeScanIntervalSeconds:     defaultResumeScanIntervalSeconds,
		OutboxDispatchIntervalSeconds: defaultOutboxDispatchIntervalSeconds,
	}
}

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
	if fc.Jobs != nil {
		cfg.Jobs = *fc.Jobs
	}
	if fc.TriggerBinPath != nil {
		cfg.TriggerBinPath = expandHome(*fc.TriggerBinPath, home)
	}
	if fc.TaskRetryW1MaxAuto != nil {
		cfg.TaskRetryW1MaxAuto = *fc.TaskRetryW1MaxAuto
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
}

func validateEnv(env string) error {
	if env != EnvProd && env != EnvDev {
		return fmt.Errorf("config: KAHYA_ENV=%q invalid, must be %q or %q", env, EnvProd, EnvDev)
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
