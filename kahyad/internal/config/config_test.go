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
	for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH", "KAHYA_ENV", "KAHYA_LOG_LEVEL"} {
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
	if len(cfg.WorkerCmd) != 3 || cfg.WorkerCmd[1] != "-m" || cfg.WorkerCmd[2] != "kahya_worker" {
		t.Errorf("WorkerCmd = %v, want [<...>/worker/.venv/bin/python -m kahya_worker]", cfg.WorkerCmd)
	}
	if want := filepath.Join("worker", ".venv", "bin", "python"); !strings.HasSuffix(cfg.WorkerCmd[0], want) {
		t.Errorf("WorkerCmd[0] = %q, want suffix %q", cfg.WorkerCmd[0], want)
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
