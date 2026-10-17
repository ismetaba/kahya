package spawn

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunRealWorkerConfigEnvRefusalIsATerminalErrorLine (FIX-FINAL
// tux-worker-config-env-refusal-silent-resume-loop): the REAL worker refuses
// to start when the user's own Claude Code config (~/.claude.json here, the
// org's managed settings alike) sets a routing name such as HTTPS_PROXY.
// That refusal used to exit 2 with no protocol line, which kahyad read as a
// crash: the task stayed 'executing' and the resume scan respawned the same
// doomed worker task_max_crash_resumes times. It is now a terminal error
// line naming the file and the variable (never the value), so Run reports
// StatusError WITH that message and kahyad fails the task once
// (server.decidePostSpawn / the outbox's worker-error rule).
func TestRunRealWorkerConfigEnvRefusalIsATerminalErrorLine(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	pythonPath := filepath.Join(repoRoot, "worker", ".venv", "bin", "python")
	if _, statErr := os.Stat(pythonPath); statErr != nil {
		t.Skipf("worker/.venv/bin/python not present (%v) - run `make venv` to exercise this test", statErr)
	}

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"env": {"HTTPS_PROXY": "http://proxy.corp.example:8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	cfg := Config{
		Cmd:              []string{pythonPath, "-m", "kahya_worker"},
		Socket:           filepath.Join(t.TempDir(), "kahyad.sock"),
		LogDir:           t.TempDir(),
		AnthropicBaseURL: "https://upstream.invalid",
		APIKey:           "kahya-task-" + "c0ffee00c0ffee00c0ffee00c0ffee00",
		CredentialMode:   CredentialModePassthroughLiteral,
	}
	env := testEnvelope(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	outcome, err := Run(ctx, cfg, env, Callbacks{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := "Claude Code yapılandırması (~/.claude.json) HTTPS_PROXY ortam değişkenini ayarlıyor; " +
		"Kâhya işçisi güvenlik gereği başlatılmadı — bu anahtarı oradan kaldırıp yeniden deneyin. " +
		"Ayrıntı: kahya log --trace " + env.TraceID
	if outcome.Status != StatusError || outcome.ErrMsg != want {
		t.Fatalf("outcome = %+v, want StatusError with ErrMsg %q (an empty ErrMsg is the old silent exit kahyad took for a crash)", outcome, want)
	}
}
