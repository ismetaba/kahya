package osascript

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

// TestProcessGroupExecutorKillsOnTimeout proves the REAL production
// Executor (processGroupExecutor) actually kills a hung child on context
// deadline — using a plain `sleep` subprocess (never osascript/Finder, so
// this needs no TCC Automation grant and touches no "real app" at all;
// it is a test of THIS package's own process-management code, exactly
// like runner_test.go's stub-executor timeout test, just with the real
// exec.Cmd machinery underneath instead of a fake).
func TestProcessGroupExecutorKillsOnTimeout(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no `sleep` binary on PATH")
	}

	e := processGroupExecutor{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := e.Run(ctx, "sleep", []string{"30"}, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	// The whole point of the process-GROUP kill: `sleep 30` must be
	// stopped well before its own 30s would elapse — a generous 5s bound
	// still proves the kill actually happened rather than this test just
	// waiting out the real sleep duration.
	if elapsed > 5*time.Second {
		t.Fatalf("Run took %v after a 50ms deadline — the process was not actually killed", elapsed)
	}
}

// TestProcessGroupExecutorHappyPath proves the ordinary (non-timeout)
// path captures stdout/stderr/exit code correctly, stdin included, using
// a real (harmless) subprocess.
func TestProcessGroupExecutorHappyPath(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("no `cat` binary on PATH")
	}

	e := processGroupExecutor{}
	res, err := e.Run(context.Background(), "cat", nil, []byte("merhaba"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(res.Stdout) != "merhaba" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "merhaba")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestProcessGroupExecutorNonZeroExit proves a non-zero exit is reported
// as ExitCode, not as a Go error (mirrors mcp/shell.processExecutor's
// identical contract).
func TestProcessGroupExecutorNonZeroExit(t *testing.T) {
	if _, err := exec.LookPath("false"); err != nil {
		t.Skip("no `false` binary on PATH")
	}
	e := processGroupExecutor{}
	res, err := e.Run(context.Background(), "false", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v, want nil error for a plain non-zero exit", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want non-zero")
	}
}
