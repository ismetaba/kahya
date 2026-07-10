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

	// Env is KAHYA_ENV ("prod" default | "dev"). It is env-only: there is
	// no config.yaml key for it, since it exists precisely so tests and the
	// W7-8 KAHYA_ENV=dev profile can redirect every path independent of any
	// on-disk config file.
	Env string `yaml:"-"`
}

// fileConfig mirrors Config for YAML unmarshalling, using pointers so we
// can distinguish "key absent" (nil) from "key present with zero value".
type fileConfig struct {
	DataDir              *string `yaml:"data_dir"`
	Socket               *string `yaml:"socket"`
	LogDir               *string `yaml:"log_dir"`
	DBPath               *string `yaml:"db_path"`
	MemoryDir            *string `yaml:"memory_dir"`
	AnthropicUpstreamURL *string `yaml:"anthropic_upstream_url"`
	EmbedPort            *int    `yaml:"embed_port"`
	DefaultModel         *string `yaml:"default_model"`
	TaskTimeoutMin       *int    `yaml:"task_timeout_min"`
	ActiveEmbedModelVer  *string `yaml:"active_embed_model_ver"`
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
		DataDir:              dataDir,
		Socket:               filepath.Join(dataDir, "kahyad.sock"),
		LogDir:               filepath.Join(dataDir, "logs"),
		DBPath:               filepath.Join(dataDir, "brain.db"),
		MemoryDir:            filepath.Join(home, "Kahya", "memory"),
		AnthropicUpstreamURL: "https://api.anthropic.com",
		EmbedPort:            8092,
		DefaultModel:         "claude-sonnet-5",
		TaskTimeoutMin:       30,
		ActiveEmbedModelVer:  "qwen3-embedding-0.6b:512:v1",
		Env:                  EnvProd,
	}
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
}

func validateEnv(env string) error {
	if env != EnvProd && env != EnvDev {
		return fmt.Errorf("config: KAHYA_ENV=%q invalid, must be %q or %q", env, EnvProd, EnvDev)
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
