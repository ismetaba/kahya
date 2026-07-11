package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv resets every config-relevant env var to unset so tests don't leak
// state from the invoking shell or bleed between subtests.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH", "KAHYA_ENV", "KAHYA_LOG_LEVEL", "KAHYA_SHELL_WORKDIR_ROOTS"} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantDataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if cfg.DataDir != wantDataDir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, wantDataDir)
	}
	if want := filepath.Join(wantDataDir, "kahyad.sock"); cfg.Socket != want {
		t.Errorf("Socket = %q, want %q", cfg.Socket, want)
	}
	if want := filepath.Join(wantDataDir, "logs"); cfg.LogDir != want {
		t.Errorf("LogDir = %q, want %q", cfg.LogDir, want)
	}
	if want := filepath.Join(wantDataDir, "brain.db"); cfg.DBPath != want {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, want)
	}
	if want := filepath.Join(home, "Kahya", "memory"); cfg.MemoryDir != want {
		t.Errorf("MemoryDir = %q, want %q", cfg.MemoryDir, want)
	}
	if cfg.AnthropicUpstreamURL != "https://api.anthropic.com" {
		t.Errorf("AnthropicUpstreamURL = %q", cfg.AnthropicUpstreamURL)
	}
	if cfg.EmbedPort != 8092 {
		t.Errorf("EmbedPort = %d, want 8092", cfg.EmbedPort)
	}
	if cfg.DefaultModel != "claude-sonnet-5" {
		t.Errorf("DefaultModel = %q", cfg.DefaultModel)
	}
	if cfg.TaskTimeoutMin != 30 {
		t.Errorf("TaskTimeoutMin = %d, want 30", cfg.TaskTimeoutMin)
	}
	if cfg.ActiveEmbedModelVer != "qwen3-embedding-0.6b:512:v1" {
		t.Errorf("ActiveEmbedModelVer = %q", cfg.ActiveEmbedModelVer)
	}
	if cfg.Env != EnvProd {
		t.Errorf("Env = %q, want %q", cfg.Env, EnvProd)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.UndoWindowSeconds != 300 {
		t.Errorf("UndoWindowSeconds = %d, want 300", cfg.UndoWindowSeconds)
	}
	if len(cfg.WorkerCmd) != 3 || cfg.WorkerCmd[1] != "-m" || cfg.WorkerCmd[2] != "kahya_worker" {
		t.Errorf("WorkerCmd = %v, want [<...>/worker/.venv/bin/python -m kahya_worker]", cfg.WorkerCmd)
	}
	if want := filepath.Join("worker", ".venv", "bin", "python"); !strings.HasSuffix(cfg.WorkerCmd[0], want) {
		t.Errorf("WorkerCmd[0] = %q, want suffix %q", cfg.WorkerCmd[0], want)
	}
	if len(cfg.EmbedCmd) != 2 {
		t.Fatalf("EmbedCmd = %v, want exactly 2 elements (python, server.py)", cfg.EmbedCmd)
	}
	if want := filepath.Join("mlx", "embed", ".venv", "bin", "python"); !strings.HasSuffix(cfg.EmbedCmd[0], want) {
		t.Errorf("EmbedCmd[0] = %q, want suffix %q", cfg.EmbedCmd[0], want)
	}
	if want := filepath.Join("mlx", "embed", "server.py"); !strings.HasSuffix(cfg.EmbedCmd[1], want) {
		t.Errorf("EmbedCmd[1] = %q, want suffix %q", cfg.EmbedCmd[1], want)
	}
	// BLOCKER 2's fail-closed reservation-estimate fallback (see the
	// field's own doc comment) - committed default 50000.
	if cfg.EstRequestTokens != 50_000 {
		t.Errorf("EstRequestTokens = %d, want 50000", cfg.EstRequestTokens)
	}
}

func TestLoadFileOverridesDefaults(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	customDataDir := filepath.Join(home, "custom-data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlContent := "embed_port: 9999\n" +
		"default_model: \"custom-model\"\n" +
		"data_dir: \"" + customDataDir + "\"\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.EmbedPort != 9999 {
		t.Errorf("EmbedPort = %d, want 9999 (file should override default)", cfg.EmbedPort)
	}
	if cfg.DefaultModel != "custom-model" {
		t.Errorf("DefaultModel = %q, want custom-model", cfg.DefaultModel)
	}
	if cfg.DataDir != customDataDir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, customDataDir)
	}
	// Socket wasn't set in the file, so it must follow the file's data_dir.
	if want := filepath.Join(customDataDir, "kahyad.sock"); cfg.Socket != want {
		t.Errorf("Socket = %q, want %q (should derive from file's data_dir)", cfg.Socket, want)
	}
	// task_timeout_min wasn't set in the file, default should survive.
	if cfg.TaskTimeoutMin != 30 {
		t.Errorf("TaskTimeoutMin = %d, want 30 (untouched default)", cfg.TaskTimeoutMin)
	}
	// undo_window_seconds wasn't set in the file, default should survive.
	if cfg.UndoWindowSeconds != 300 {
		t.Errorf("UndoWindowSeconds = %d, want 300 (untouched default)", cfg.UndoWindowSeconds)
	}
}

// TestLoadFileOverridesUndoWindowSeconds proves undo_window_seconds is a
// real, independently-overridable config.yaml key (MINOR fix: the
// purge-on-expiry acceptance criterion calls for "inject a short window
// via config").
func TestLoadFileOverridesUndoWindowSeconds(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("undo_window_seconds: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.UndoWindowSeconds != 2 {
		t.Errorf("UndoWindowSeconds = %d, want 2 (file override)", cfg.UndoWindowSeconds)
	}
}

func TestLoadFileOverridesWorkerCmd(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlContent := "worker_cmd:\n  - \"/tmp/fake-worker.sh\"\n  - \"--flag\"\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"/tmp/fake-worker.sh", "--flag"}
	if len(cfg.WorkerCmd) != len(want) {
		t.Fatalf("WorkerCmd = %v, want %v", cfg.WorkerCmd, want)
	}
	for i := range want {
		if cfg.WorkerCmd[i] != want[i] {
			t.Errorf("WorkerCmd[%d] = %q, want %q", i, cfg.WorkerCmd[i], want[i])
		}
	}
}

func TestLoadFileOverridesEmbedCmd(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlContent := "embed_cmd:\n  - \"/tmp/fake-embed.sh\"\n  - \"--flag\"\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"/tmp/fake-embed.sh", "--flag"}
	if len(cfg.EmbedCmd) != len(want) {
		t.Fatalf("EmbedCmd = %v, want %v", cfg.EmbedCmd, want)
	}
	for i := range want {
		if cfg.EmbedCmd[i] != want[i] {
			t.Errorf("EmbedCmd[%d] = %q, want %q", i, cfg.EmbedCmd[i], want[i])
		}
	}
}

func TestLoadEnvOverridesFileAndDefaults(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fileDataDir := filepath.Join(home, "from-file")
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("data_dir: \""+fileDataDir+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	envDataDir := filepath.Join(home, "from-env")
	envSocket := filepath.Join(home, "explicit.sock")
	envMemoryDir := filepath.Join(home, "env-memory")
	envDBPath := filepath.Join(home, "env-brain.db")
	t.Setenv("KAHYA_DATA_DIR", envDataDir)
	t.Setenv("KAHYA_SOCKET", envSocket)
	t.Setenv("KAHYA_MEMORY_DIR", envMemoryDir)
	t.Setenv("KAHYA_DB_PATH", envDBPath)
	t.Setenv("KAHYA_ENV", "dev")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.DataDir != envDataDir {
		t.Errorf("DataDir = %q, want %q (env beats file)", cfg.DataDir, envDataDir)
	}
	if cfg.Socket != envSocket {
		t.Errorf("Socket = %q, want %q (explicit env socket)", cfg.Socket, envSocket)
	}
	if cfg.MemoryDir != envMemoryDir {
		t.Errorf("MemoryDir = %q, want %q", cfg.MemoryDir, envMemoryDir)
	}
	if cfg.DBPath != envDBPath {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, envDBPath)
	}
	if cfg.Env != EnvDev {
		t.Errorf("Env = %q, want dev", cfg.Env)
	}
	// LogDir wasn't overridden anywhere, so it must follow the final (env)
	// data_dir, not the file's data_dir.
	if want := filepath.Join(envDataDir, "logs"); cfg.LogDir != want {
		t.Errorf("LogDir = %q, want %q (should derive from final data_dir)", cfg.LogDir, want)
	}
}

func TestLoadExpandsTilde(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_MEMORY_DIR", "~/tilde-memory")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := filepath.Join(home, "tilde-memory")
	if cfg.MemoryDir != want {
		t.Errorf("MemoryDir = %q, want %q (tilde expansion)", cfg.MemoryDir, want)
	}
}

// TestLoadShellWorkdirRootsDefaultEmpty proves BLOCKER 1 fix's config
// surface defaults to empty (nil) - config.go's applyFile/applyEnv only
// ever populate it when the file or env layer explicitly sets it; mcp/shell.
// Runner treats an empty WorkdirRoots as "apply the deny-rule posture",
// never as "allow nothing" or "allow everything".
func TestLoadShellWorkdirRootsDefaultEmpty(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.ShellWorkdirRoots) != 0 {
		t.Errorf("ShellWorkdirRoots = %v, want empty by default", cfg.ShellWorkdirRoots)
	}
}

// TestLoadShellWorkdirRootsFromFile proves shell_workdir_roots is a real,
// independently-settable config.yaml key, and that each entry is
// tilde-expanded exactly like every other configured path.
func TestLoadShellWorkdirRootsFromFile(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlContent := "shell_workdir_roots:\n  - \"~/allowed-a\"\n  - \"/absolute/allowed-b\"\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{filepath.Join(home, "allowed-a"), "/absolute/allowed-b"}
	if len(cfg.ShellWorkdirRoots) != len(want) || cfg.ShellWorkdirRoots[0] != want[0] || cfg.ShellWorkdirRoots[1] != want[1] {
		t.Errorf("ShellWorkdirRoots = %v, want %v", cfg.ShellWorkdirRoots, want)
	}
}

// TestLoadShellWorkdirRootsEnvOverridesFile proves KAHYA_SHELL_WORKDIR_ROOTS
// (a comma-separated list) overrides config.yaml's shell_workdir_roots,
// trims whitespace around each entry, drops empty entries, and
// tilde-expands each surviving one - the same precedence/expansion rule
// every other configured path follows.
func TestLoadShellWorkdirRootsEnvOverridesFile(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("shell_workdir_roots:\n  - \"/from-file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAHYA_SHELL_WORKDIR_ROOTS", " ~/from-env-a ,/from-env-b,, ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{filepath.Join(home, "from-env-a"), "/from-env-b"}
	if len(cfg.ShellWorkdirRoots) != len(want) || cfg.ShellWorkdirRoots[0] != want[0] || cfg.ShellWorkdirRoots[1] != want[1] {
		t.Errorf("ShellWorkdirRoots = %v, want %v (env overrides file, trims, drops empties)", cfg.ShellWorkdirRoots, want)
	}
}

func TestLoadRejectsNonASCIIPath(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_MEMORY_DIR", filepath.Join(home, "Kâhya", "memory"))

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want non-ASCII rejection")
	}
}

func TestLoadRejectsInvalidEnv(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_ENV", "staging")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want rejection of invalid KAHYA_ENV")
	}
}

// TestLoadAcceptsValidLogLevels guards MINOR 5: every one of the four
// documented KAHYA_LOG_LEVEL values must load cleanly and round-trip onto
// Config.LogLevel unchanged.
func TestLoadAcceptsValidLogLevels(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error"} {
		t.Run(lvl, func(t *testing.T) {
			clearEnv(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("KAHYA_LOG_LEVEL", lvl)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v, want KAHYA_LOG_LEVEL=%q accepted", err, lvl)
			}
			if cfg.LogLevel != lvl {
				t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, lvl)
			}
		})
	}
}

// TestLoadRejectsInvalidLogLevel guards MINOR 5's fail-closed posture: an
// unrecognized KAHYA_LOG_LEVEL must fail Load, the same as an invalid
// KAHYA_ENV, never silently fall back to a default.
func TestLoadRejectsInvalidLogLevel(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KAHYA_LOG_LEVEL", "verbose")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want rejection of invalid KAHYA_LOG_LEVEL")
	}
}
