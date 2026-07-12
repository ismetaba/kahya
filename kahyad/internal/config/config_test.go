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
	for _, k := range []string{"KAHYA_DATA_DIR", "KAHYA_SOCKET", "KAHYA_MEMORY_DIR", "KAHYA_DB_PATH", "KAHYA_ENV", "KAHYA_LOG_LEVEL", "KAHYA_SHELL_WORKDIR_ROOTS", "KAHYA_TELEGRAM_CHAT_ID", "KAHYA_TELEGRAM_USER_ID"} {
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
	// W3-05 egress proxy default port.
	if cfg.EgressPort != 3128 {
		t.Errorf("EgressPort = %d, want 3128", cfg.EgressPort)
	}
	// W3-07: empty chat_id/user_id by default => bot disabled.
	if cfg.TelegramChatID != 0 {
		t.Errorf("TelegramChatID = %d, want 0 (disabled by default)", cfg.TelegramChatID)
	}
	if cfg.TelegramUserID != 0 {
		t.Errorf("TelegramUserID = %d, want 0 (disabled by default)", cfg.TelegramUserID)
	}
	// W4-02: receipt-less W1 auto-retry cap default.
	if cfg.TaskRetryW1MaxAuto != 3 {
		t.Errorf("TaskRetryW1MaxAuto = %d, want 3", cfg.TaskRetryW1MaxAuto)
	}
	// W4-04: cloud-call error taxonomy defaults (task spec, verbatim).
	if cfg.CloudRetryMaxInline != 3 {
		t.Errorf("CloudRetryMaxInline = %d, want 3", cfg.CloudRetryMaxInline)
	}
	if got, want := cfg.CloudRetryTaskSchedule, []string{"1m", "5m", "15m", "60m"}; len(got) != len(want) {
		t.Errorf("CloudRetryTaskSchedule = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("CloudRetryTaskSchedule[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	}
	if cfg.CloudRetryGiveUpAfter != "24h" {
		t.Errorf("CloudRetryGiveUpAfter = %q, want 24h", cfg.CloudRetryGiveUpAfter)
	}
	// W4-06: KahyaDir/BackupDir default to ~/Kahya and ~/Kahya/backups.
	if want := filepath.Join(home, "Kahya"); cfg.KahyaDir != want {
		t.Errorf("KahyaDir = %q, want %q", cfg.KahyaDir, want)
	}
	if want := filepath.Join(home, "Kahya", "backups"); cfg.BackupDir != want {
		t.Errorf("BackupDir = %q, want %q", cfg.BackupDir, want)
	}
}

// TestLoadDefaultJobsIncludeBackupNightlyAndMemoryPush proves W4-06's own
// two default cfg.jobs entries (task spec step 4, verbatim times) are
// present out of the box — config.Config.Jobs's doc comment names this
// task as the one that adds them.
func TestLoadDefaultJobsIncludeBackupNightlyAndMemoryPush(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.Jobs) != 2 {
		t.Fatalf("Jobs = %+v, want exactly 2 default entries", cfg.Jobs)
	}

	byName := make(map[string]JobConfig, len(cfg.Jobs))
	for _, j := range cfg.Jobs {
		byName[j.Name] = j
	}

	backup, ok := byName["backup-nightly"]
	if !ok {
		t.Fatalf("Jobs missing backup-nightly: %+v", cfg.Jobs)
	}
	if backup.Handler != "backup-nightly" {
		t.Errorf("backup-nightly.Handler = %q, want %q", backup.Handler, "backup-nightly")
	}
	if backup.Calendar.Hour == nil || *backup.Calendar.Hour != 3 {
		t.Errorf("backup-nightly.Calendar.Hour = %v, want 3", backup.Calendar.Hour)
	}
	if backup.Calendar.Minute == nil || *backup.Calendar.Minute != 30 {
		t.Errorf("backup-nightly.Calendar.Minute = %v, want 30", backup.Calendar.Minute)
	}

	push, ok := byName["memory-push"]
	if !ok {
		t.Fatalf("Jobs missing memory-push: %+v", cfg.Jobs)
	}
	if push.Handler != "memory-push" {
		t.Errorf("memory-push.Handler = %q, want %q", push.Handler, "memory-push")
	}
	if push.Calendar.Hour == nil || *push.Calendar.Hour != 3 {
		t.Errorf("memory-push.Calendar.Hour = %v, want 3", push.Calendar.Hour)
	}
	if push.Calendar.Minute == nil || *push.Calendar.Minute != 45 {
		t.Errorf("memory-push.Calendar.Minute = %v, want 45", push.Calendar.Minute)
	}

	// validateJobs must still accept the default set (DNS-label names, no
	// duplicates, non-empty handlers) — Load() above already proves this
	// implicitly (it would have failed closed otherwise), asserted here
	// explicitly too for a clearer failure message if it ever regresses.
	if err := validateJobs(cfg.Jobs); err != nil {
		t.Errorf("validateJobs(defaults) error = %v, want nil", err)
	}
}

// TestLoadFileOverridesKahyaDirAndBackupDir proves both W4-06 fields are
// real, independently tilde-expanding config.yaml keys, same convention
// as every other configured path in this file.
func TestLoadFileOverridesKahyaDirAndBackupDir(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlContent := "kahya_dir: ~/CustomKahya\nbackup_dir: ~/CustomKahya/snapshots\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if want := filepath.Join(home, "CustomKahya"); cfg.KahyaDir != want {
		t.Errorf("KahyaDir = %q, want %q", cfg.KahyaDir, want)
	}
	if want := filepath.Join(home, "CustomKahya", "snapshots"); cfg.BackupDir != want {
		t.Errorf("BackupDir = %q, want %q", cfg.BackupDir, want)
	}
}

// TestLoadFileOverridesCloudRetryKeys proves the three W4-04 cloud_retry_*
// keys are real, independently-overridable config.yaml keys.
func TestLoadFileOverridesCloudRetryKeys(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlBody := "cloud_retry_max_inline: 5\n" +
		"cloud_retry_task_schedule: [\"30s\", \"2m\"]\n" +
		"cloud_retry_give_up_after: \"1h\"\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.CloudRetryMaxInline != 5 {
		t.Errorf("CloudRetryMaxInline = %d, want 5", cfg.CloudRetryMaxInline)
	}
	if want := []string{"30s", "2m"}; len(cfg.CloudRetryTaskSchedule) != 2 || cfg.CloudRetryTaskSchedule[0] != want[0] || cfg.CloudRetryTaskSchedule[1] != want[1] {
		t.Errorf("CloudRetryTaskSchedule = %v, want %v", cfg.CloudRetryTaskSchedule, want)
	}
	if cfg.CloudRetryGiveUpAfter != "1h" {
		t.Errorf("CloudRetryGiveUpAfter = %q, want 1h", cfg.CloudRetryGiveUpAfter)
	}
}

// TestLoadFailsClosedOnInvalidCloudRetryScheduleEntry proves a malformed
// cloud_retry_task_schedule entry fails Load outright (fail-closed),
// rather than surfacing as a silent zero-delay retry loop later.
func TestLoadFailsClosedOnInvalidCloudRetryScheduleEntry(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("cloud_retry_task_schedule: [\"not-a-duration\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want an error for the malformed schedule entry")
	}
}

// TestLoadFailsClosedOnInvalidCloudRetryMaxInline proves cloud_retry_max_
// inline < 1 fails Load outright.
func TestLoadFailsClosedOnInvalidCloudRetryMaxInline(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("cloud_retry_max_inline: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want an error for max_inline=0")
	}
}

// TestLoadFileOverridesTaskRetryW1MaxAuto proves task_retry_w1_max_auto is
// a real, independently-overridable config.yaml key (W4-02).
func TestLoadFileOverridesTaskRetryW1MaxAuto(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("task_retry_w1_max_auto: 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.TaskRetryW1MaxAuto != 5 {
		t.Errorf("TaskRetryW1MaxAuto = %d, want 5", cfg.TaskRetryW1MaxAuto)
	}
}

// TestLoadFileOverridesTelegramIDs proves telegram_chat_id/telegram_user_id
// are real, independently-overridable config.yaml keys (W3-07: the single
// fixed chat_id/user_id allowlist pair).
func TestLoadFileOverridesTelegramIDs(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlContent := "telegram_chat_id: 123456789\ntelegram_user_id: 987654321\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.TelegramChatID != 123456789 {
		t.Errorf("TelegramChatID = %d, want 123456789", cfg.TelegramChatID)
	}
	if cfg.TelegramUserID != 987654321 {
		t.Errorf("TelegramUserID = %d, want 987654321", cfg.TelegramUserID)
	}
}

// TestLoadEnvOverridesTelegramIDs proves KAHYA_TELEGRAM_CHAT_ID/
// KAHYA_TELEGRAM_USER_ID override both the default and any config.yaml
// value, matching every other env-override key's precedence.
func TestLoadEnvOverridesTelegramIDs(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("telegram_chat_id: 111\ntelegram_user_id: 222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAHYA_TELEGRAM_CHAT_ID", "555")
	t.Setenv("KAHYA_TELEGRAM_USER_ID", "666")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.TelegramChatID != 555 {
		t.Errorf("TelegramChatID = %d, want 555 (env override)", cfg.TelegramChatID)
	}
	if cfg.TelegramUserID != 666 {
		t.Errorf("TelegramUserID = %d, want 666 (env override)", cfg.TelegramUserID)
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

func TestLoadEgressPortFileAndEnvOverride(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte("egress_port: 4000\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.EgressPort != 4000 {
		t.Errorf("EgressPort = %d, want 4000 (file override)", cfg.EgressPort)
	}

	t.Setenv("KAHYA_EGRESS_PORT", "4001")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.EgressPort != 4001 {
		t.Errorf("EgressPort = %d, want 4001 (env beats file)", cfg.EgressPort)
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

// jobsTestIntPtr is a local *int helper for building config.CalendarSpec
// literals in this file's table-driven tests (mirrors kahyad/internal/
// scheduler's own intPtr test helper — a separate package, so it cannot be
// reused directly).
func jobsTestIntPtr(v int) *int { return &v }

// TestValidateJobs is the MINOR 4 fix: validateJobs' fail-closed rules
// (DNS-label name, duplicate name, empty handler) previously had zero
// dedicated test coverage, despite every other Load-level fail-closed
// rule in this file (KAHYA_ENV, log_level, credential_mode) already having
// its own rejection test. Table-driven, one (or more) subtest per
// validateJobs branch — calls validateJobs directly (same package) rather
// than round-tripping through a config.yaml fixture for every case.
func TestValidateJobs(t *testing.T) {
	validCalendar := CalendarSpec{Minute: jobsTestIntPtr(30), Hour: jobsTestIntPtr(8)}

	tests := []struct {
		name    string
		jobs    []JobConfig
		wantErr bool
		errSub  string
	}{
		{
			name: "valid single job with a full calendar spec",
			jobs: []JobConfig{{Name: "nightly-backup", Handler: "backup", Calendar: validCalendar}},
		},
		{
			name: "valid multiple distinct jobs",
			jobs: []JobConfig{
				{Name: "smoke", Handler: "smoke"},
				{Name: "briefing", Handler: "briefing", Calendar: validCalendar},
			},
		},
		{
			name: "empty jobs list",
			jobs: nil,
		},
		{
			name:    "name containing a space rejected",
			jobs:    []JobConfig{{Name: "night ly", Handler: "backup"}},
			wantErr: true,
			errSub:  "invalid, must be DNS-label",
		},
		{
			name:    "name containing .. rejected",
			jobs:    []JobConfig{{Name: "night..ly", Handler: "backup"}},
			wantErr: true,
			errSub:  "invalid, must be DNS-label",
		},
		{
			name:    "name with a leading hyphen rejected",
			jobs:    []JobConfig{{Name: "-nightly", Handler: "backup"}},
			wantErr: true,
			errSub:  "invalid, must be DNS-label",
		},
		{
			name:    "name with a trailing hyphen rejected",
			jobs:    []JobConfig{{Name: "nightly-", Handler: "backup"}},
			wantErr: true,
			errSub:  "invalid, must be DNS-label",
		},
		{
			name:    "name containing an XML metacharacter rejected",
			jobs:    []JobConfig{{Name: "night<ly", Handler: "backup"}},
			wantErr: true,
			errSub:  "invalid, must be DNS-label",
		},
		{
			name:    "empty name rejected",
			jobs:    []JobConfig{{Name: "", Handler: "backup"}},
			wantErr: true,
			errSub:  "invalid, must be DNS-label",
		},
		{
			// jobNamePattern is `[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?`
			// — mixed/upper case IS accepted (the DNS-label constraint here
			// is about character class + hyphen placement, not casing) —
			// documented explicitly so a future stricter-casing change
			// doesn't silently break this on purpose.
			name: "mixed-case name accepted (pattern allows upper+lower)",
			jobs: []JobConfig{{Name: "Nightly-Backup", Handler: "backup"}},
		},
		{
			name: "duplicate name rejected",
			jobs: []JobConfig{
				{Name: "nightly", Handler: "backup"},
				{Name: "nightly", Handler: "backup2"},
			},
			wantErr: true,
			errSub:  "declared more than once",
		},
		{
			name:    "empty handler rejected",
			jobs:    []JobConfig{{Name: "nightly", Handler: ""}},
			wantErr: true,
			errSub:  "handler must not be empty",
		},
		{
			name:    "whitespace-only handler rejected",
			jobs:    []JobConfig{{Name: "nightly", Handler: "   "}},
			wantErr: true,
			errSub:  "handler must not be empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateJobs(tc.jobs)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateJobs() error = nil, want error containing %q", tc.errSub)
				}
				if !strings.Contains(err.Error(), tc.errSub) {
					t.Errorf("validateJobs() error = %q, want it to contain %q", err.Error(), tc.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateJobs() error = %v, want nil", err)
			}
		})
	}
}

// TestLoadFileParsesJobsAndCalendarSpec proves cfg.jobs YAML round-trips
// through the full Load pipeline into Config.Jobs, including a complete
// CalendarSpec (MINOR 4: "a valid CalendarSpec parses") and that an unset
// calendar field stays nil (never a spurious zero value).
func TestLoadFileParsesJobsAndCalendarSpec(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlContent := "jobs:\n" +
		"  - name: briefing\n" +
		"    handler: briefing\n" +
		"    calendar:\n" +
		"      Hour: 8\n" +
		"      Minute: 30\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Jobs) != 1 {
		t.Fatalf("Jobs = %v, want exactly 1 entry", cfg.Jobs)
	}
	job := cfg.Jobs[0]
	if job.Name != "briefing" || job.Handler != "briefing" {
		t.Errorf("Jobs[0] = %+v, want Name=briefing Handler=briefing", job)
	}
	if job.Calendar.Hour == nil || *job.Calendar.Hour != 8 {
		t.Errorf("Jobs[0].Calendar.Hour = %v, want 8", job.Calendar.Hour)
	}
	if job.Calendar.Minute == nil || *job.Calendar.Minute != 30 {
		t.Errorf("Jobs[0].Calendar.Minute = %v, want 30", job.Calendar.Minute)
	}
	if job.Calendar.Day != nil {
		t.Errorf("Jobs[0].Calendar.Day = %v, want nil (unset)", job.Calendar.Day)
	}
	if job.Calendar.Weekday != nil {
		t.Errorf("Jobs[0].Calendar.Weekday = %v, want nil (unset)", job.Calendar.Weekday)
	}
}

// TestLoadRejectsInvalidJobName proves Load's fail-closed posture actually
// extends to cfg.jobs end to end (not merely validateJobs in isolation,
// covered above) — an invalid job name in config.yaml must fail Load
// itself, the same as every other fail-closed config rule in this file.
func TestLoadRejectsInvalidJobName(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := filepath.Join(home, "Library", "Application Support", "Kahya")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlContent := "jobs:\n  - name: \"bad name\"\n    handler: smoke\n"
	if err := os.WriteFile(filepath.Join(dataDir, "config.yaml"), []byte(yamlContent), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want rejection of an invalid jobs[].name")
	}
}
