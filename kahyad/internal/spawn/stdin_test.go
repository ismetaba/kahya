package spawn

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// waitForSpawnGroupEmpty polls `pgrep -g <pgid>` until the process group is
// empty (the inline pattern TestRunKillsProcessGroupOnTimeout uses, factored
// out for the I-15 tests).
func waitForSpawnGroupEmpty(t *testing.T, pgid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		out, _ := exec.Command("pgrep", "-g", strconv.Itoa(pgid)).CombinedOutput()
		if strings.TrimSpace(string(out)) == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still alive: %s", pgid, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunHonorsCtxWhenWorkerNeverReadsStdin (I-15): a worker that is alive
// but never reads its envelope (stuck before its first read) must not pin
// Run inside the envelope stdin write. With a 1 MiB prompt the payload is far
// larger than any pipe buffer (64 KiB on macOS/Linux), so a synchronous write
// blocks until somebody drains the pipe - and before I-15 the ctx.Done select
// (the only place the task timeout and the process-group kill live) was
// never reached: Run never returned, the worker stayed registered as live,
// and neither a timeout nor a halt could end it.
func TestRunHonorsCtxWhenWorkerNeverReadsStdin(t *testing.T) {
	env := testEnvelope(t)
	env.Prompt = strings.Repeat("a", 1<<20) // 1 MiB > any pipe buffer
	cfg := testConfig("testdata/no_stdin_read_worker.py")

	var pid atomic.Int64
	// Belt and braces for the red state (Run stuck in Write): never leave a
	// sleeping fixture behind after the test fails.
	t.Cleanup(func() {
		if p := pid.Load(); p > 0 {
			_ = syscall.Kill(-int(p), syscall.SIGKILL)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	type result struct {
		outcome Outcome
		err     error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		o, err := Run(ctx, cfg, env, Callbacks{OnStart: func(p int) { pid.Store(int64(p)) }})
		done <- result{o, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Run() error = %v", r.err)
		}
		if r.outcome.Status != StatusTimeout {
			t.Fatalf("outcome.Status = %q, want %q", r.outcome.Status, StatusTimeout)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("Run returned after %v, want well under 5s", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run blocked in the envelope stdin write past the ctx deadline (worker never read stdin)")
	}

	p := int(pid.Load())
	if p == 0 {
		t.Fatal("OnStart never called with a non-zero pid")
	}
	// The ctx.Done branch SIGKILLs the whole group: nothing may survive.
	waitForSpawnGroupEmpty(t, p, 3*time.Second)
}

// TestRunLargeEnvelopeDeliveredIntact (I-15): moving the envelope write to
// its own goroutine must not change what a well-behaved worker reads - a
// 1 MiB prompt still arrives byte-exact (the echo fixture writes the raw
// stdin bytes back as its first delta), followed by the normal ok result.
func TestRunLargeEnvelopeDeliveredIntact(t *testing.T) {
	env := testEnvelope(t)
	// Keep the byte-exact Turkish prompt in the payload and pad it past any
	// pipe buffer so the write genuinely spans many reader wake-ups.
	const pad = "ğüşiöç" // 11 UTF-8 bytes
	env.Prompt = "Kadıköy'deki randevuyu hatırlat " + strings.Repeat(pad, (1<<20)/len(pad)+1)
	wantPayload, err := env.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(wantPayload) < 1<<20 {
		t.Fatalf("payload is %d bytes, want >= 1 MiB", len(wantPayload))
	}
	cfg := testConfig("testdata/echo_worker.py")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var deltas []string
	outcome, err := Run(ctx, cfg, env, Callbacks{
		OnDelta: func(text string) { deltas = append(deltas, text) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Status != StatusOK {
		t.Fatalf("outcome.Status = %q, want %q", outcome.Status, StatusOK)
	}
	if len(deltas) == 0 {
		t.Fatal("no delta lines: the echo worker never received its envelope")
	}
	if deltas[0] != string(wantPayload) {
		t.Fatalf("envelope echo mismatch: got %d bytes, want %d bytes (byte-exact)", len(deltas[0]), len(wantPayload))
	}
}

// TestRunRelaysEnvelopeWriteFailureAsDiagnostics (I-15): a worker that exits
// without reading a larger-than-pipe-buffer envelope breaks the write. That
// is not fatal on its own - the missing terminal line decides the outcome
// (StatusError) - but the failure is relayed once as a stderr diagnostics
// line, from Run's own goroutine (never after Run returned).
func TestRunRelaysEnvelopeWriteFailureAsDiagnostics(t *testing.T) {
	env := testEnvelope(t)
	env.Prompt = strings.Repeat("a", 1<<20)
	cfg := testConfig("-c")
	cfg.Cmd = []string{"python3", "-c", "import sys; sys.exit(0)"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stderrLines []string
	outcome, err := Run(ctx, cfg, env, Callbacks{
		OnStderr: func(line string) { stderrLines = append(stderrLines, line) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Status != StatusError {
		t.Fatalf("outcome.Status = %q, want %q (exit without a terminal line)", outcome.Status, StatusError)
	}
	var relayed int
	for _, l := range stderrLines {
		if strings.HasPrefix(l, "spawn: envelope stdin write: ") {
			relayed++
		}
	}
	if relayed != 1 {
		t.Fatalf("envelope write failure relayed %d times, want 1; stderr lines = %q", relayed, stderrLines)
	}
}
