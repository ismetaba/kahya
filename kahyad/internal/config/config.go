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
	"strings"

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

	// Env is KAHYA_ENV ("prod" default | "dev"). It is env-only: there is
	// no config.yaml key for it, since it exists precisely so tests and the
	// W7-8 KAHYA_ENV=dev profile can redirect every path independent of any
	// on-disk config file.
	Env string `yaml:"-"`
}

// fileConfig mirrors Config for YAML unmarshalling, using pointers so we
// can distinguish "key absent" (nil) from "key present with zero value".
type fileConfig struct {
	DataDir                *string   `yaml:"data_dir"`
	Socket                 *string   `yaml:"socket"`
	LogDir                 *string   `yaml:"log_dir"`
	DBPath                 *string   `yaml:"db_path"`
	MemoryDir              *string   `yaml:"memory_dir"`
	AnthropicUpstreamURL   *string   `yaml:"anthropic_upstream_url"`
	EmbedPort              *int      `yaml:"embed_port"`
	DefaultModel           *string   `yaml:"default_model"`
	TaskTimeoutMin         *int      `yaml:"task_timeout_min"`
	ActiveEmbedModelVer    *string   `yaml:"active_embed_model_ver"`
	LogLevel               *string   `yaml:"log_level"`
	UndoWindowSeconds      *int      `yaml:"undo_window_seconds"`
	WorkerCmd              *[]string `yaml:"worker_cmd"`
	EmbedCmd               *[]string `yaml:"embed_cmd"`
	MCPBridgePath          *string   `yaml:"mcp_bridge_path"`
	PolicyPath             *string   `yaml:"policy_path"`
	DockerImageTag         *string   `yaml:"docker_image_tag"`
	DockerImageDigestPath  *string   `yaml:"docker_image_digest_path"`
	DailyBudgetUSD         *float64  `yaml:"daily_budget_usd"`
	MonthlyBudgetUSD       *float64  `yaml:"monthly_budget_usd"`
	TaskTokenCeiling       *int64    `yaml:"task_token_ceiling"`
	DowngradeAtRatio       *float64  `yaml:"downgrade_at_ratio"`
	CacheHitAlarmThreshold *float64  `yaml:"cache_hit_alarm_threshold"`
	CredentialMode         *string   `yaml:"credential_mode"`
	EstRequestTokens       *int64    `yaml:"est_request_tokens"`
}

// Load resolves Config from defaults, an optional config.yaml, and
// environment overrides, in that precedence order (lowest to highest).
func Load() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("config: resolve home dir: %w", err)
	}

	cfg := defaults(home)

	// The config file lives under the *default* data_dir: at this point in
	// the pipeline data_dir has not yet been touched by the file or env
	// layers, so there is exactly one unambiguous place to look.
	fileCfgPath := filepath.Join(cfg.DataDir, "config.yaml")

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

	// Any of the data_dir-derived fields not explicitly set by the file or
	// env layers follow the *final* data_dir (so overriding just
	// KAHYA_DATA_DIR moves socket/log_dir/db_path along with it).
	if !explicitSocket {
		cfg.Socket = filepath.Join(cfg.DataDir, "kahyad.sock")
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

	return cfg, nil
}

func defaults(home string) Config {
	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	return Config{
		DataDir:               dataDir,
		Socket:                filepath.Join(dataDir, "kahyad.sock"),
		LogDir:                filepath.Join(dataDir, "logs"),
		DBPath:                filepath.Join(dataDir, "brain.db"),
		MemoryDir:             filepath.Join(home, "Kahya", "memory"),
		AnthropicUpstreamURL:  "https://api.anthropic.com",
		EmbedPort:             8092,
		DefaultModel:          "claude-sonnet-5",
		TaskTimeoutMin:        30,
		ActiveEmbedModelVer:   "qwen3-embedding-0.6b:512:v1",
		LogLevel:              "info",
		UndoWindowSeconds:     300,
		Env:                   EnvProd,
		WorkerCmd:             defaultWorkerCmd(),
		EmbedCmd:              defaultEmbedCmd(),
		MCPBridgePath:         defaultMCPBridgePath(),
		PolicyPath:            defaultPolicyPath(),
		DockerImageTag:        "kahya-sandbox:0.1.0",
		DockerImageDigestPath: defaultDockerImageDigestPath(),

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
	}
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
	if fc.DockerImageTag != nil {
		cfg.DockerImageTag = *fc.DockerImageTag
	}
	if fc.DockerImageDigestPath != nil {
		cfg.DockerImageDigestPath = expandHome(*fc.DockerImageDigestPath, home)
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
	if v := os.Getenv("KAHYA_DOCKER_IMAGE_TAG"); v != "" {
		cfg.DockerImageTag = v
	}
	if v := os.Getenv("KAHYA_DOCKER_IMAGE_DIGEST_PATH"); v != "" {
		cfg.DockerImageDigestPath = expandHome(v, home)
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

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
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
	}
	// Deterministic order for reproducible error messages.
	for _, key := range []string{"data_dir", "socket", "log_dir", "db_path", "memory_dir"} {
		p := paths[key]
		for _, r := range p {
			if r > 127 {
				return fmt.Errorf("config: %s=%q contains non-ASCII rune %q (HANDOFF §7: directory names must be ASCII)", key, p, r)
			}
		}
	}
	return nil
}
