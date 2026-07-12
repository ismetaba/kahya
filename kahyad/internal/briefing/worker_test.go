package briefing

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/spawn"
)

// TestBuildEnvelopeShape proves BuildEnvelope produces the exact envelope
// shape this task's own worker profile relies on: Kind stays "chat"
// (unchanged, frozen IPC contract), Mode=spawn.ModeReader (toolless),
// Model=ModelName (claude-haiku-4-5, Go-side), Schema=SummaryJobType, a
// fresh (never-resumed) session.
func TestBuildEnvelopeShape(t *testing.T) {
	now := time.Date(2026, 7, 12, 8, 30, 0, 0, time.UTC)
	env := BuildEnvelope("t1", "trace-1", "prompt text", now)

	if env.Kind != "chat" {
		t.Errorf("Kind = %q, want %q", env.Kind, "chat")
	}
	if env.Mode != spawn.ModeReader {
		t.Errorf("Mode = %q, want %q (toolless)", env.Mode, spawn.ModeReader)
	}
	if env.Model != ModelName {
		t.Errorf("Model = %q, want %q", env.Model, ModelName)
	}
	if env.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want the literal HANDOFF §4 routing-table value claude-haiku-4-5", env.Model)
	}
	if env.Schema != SummaryJobType {
		t.Errorf("Schema = %q, want %q", env.Schema, SummaryJobType)
	}
	if env.SessionID != nil {
		t.Errorf("SessionID = %v, want nil (never resumed)", env.SessionID)
	}
	if env.Resume {
		t.Error("Resume = true, want false")
	}
	if env.MemoryInjection {
		t.Error("MemoryInjection = true, want false (untrusted session gets nothing from brain.db)")
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestProcessSpawnerBuildsEnvelopeAndAccumulatesDeltas exercises the
// production WorkerSpawner against a small fake python script (mirroring
// kahyad/internal/spawn's and kahyad/internal/reader's own testdata
// convention) - no claude-agent-sdk/network call involved.
func TestProcessSpawnerBuildsEnvelopeAndAccumulatesDeltas(t *testing.T) {
	script := filepath.Join("testdata", "fake_briefing_worker.py")

	p := ProcessSpawner{Cmd: []string{"python3", script}}
	env := BuildEnvelope("t1", "trace-1", "prompt text", time.Now())

	rawJSON, err := p.Spawn(context.Background(), env)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	var v BriefingSummaryV1
	if err := json.Unmarshal([]byte(rawJSON), &v); err != nil {
		t.Fatalf("Spawn() returned non-JSON content (deltas not correctly concatenated?): %v\nrawJSON=%q", err, rawJSON)
	}
	if len(v.Lines) != 2 || !strings.Contains(v.Lines[0], "PR") {
		t.Fatalf("Lines = %+v, unexpected", v.Lines)
	}
}

// TestProcessSpawnerPropagatesWorkerErrorStatus proves a worker that
// exits with an "error" protocol line surfaces as a Go error, never a
// silently-empty success.
func TestProcessSpawnerPropagatesWorkerErrorStatus(t *testing.T) {
	script := filepath.Join("testdata", "fake_briefing_error_worker.py")
	p := ProcessSpawner{Cmd: []string{"python3", script}}
	env := BuildEnvelope("t1", "trace-1", "prompt text", time.Now())

	if _, err := p.Spawn(context.Background(), env); err == nil {
		t.Fatal("Spawn: expected an error for a worker that reports status=error")
	}
}

// TestProcessSpawnerRejectsInvalidEnvelopeBeforeSpawning proves an
// invalid envelope (e.g. an empty prompt) is rejected BEFORE any process
// is ever spawned.
func TestProcessSpawnerRejectsInvalidEnvelopeBeforeSpawning(t *testing.T) {
	p := ProcessSpawner{Cmd: []string{"python3", filepath.Join("testdata", "fake_briefing_worker.py")}}
	env := BuildEnvelope("t1", "trace-1", "", time.Now())
	if _, err := p.Spawn(context.Background(), env); err == nil {
		t.Fatal("Spawn: expected an error for an invalid (empty-prompt) envelope")
	}
}

// TestProcessSpawnerUsesProxyOpenerWhenSet proves Spawn calls ProxyOpener
// (with the envelope's own task_id/trace_id) to mint the per-run
// Anthropic base URL/API key, uses them for that one call, and always
// invokes the returned closeFn - mirroring
// kahyad/internal/outbox.Dispatcher's own SetAnthproxyOpener contract, so
// main.go can wire this to the SAME srv.NewTaskProxy every other call
// site uses.
func TestProcessSpawnerUsesProxyOpenerWhenSet(t *testing.T) {
	var gotTaskID, gotTraceID string
	closed := false
	p := ProcessSpawner{
		Cmd: []string{"python3", filepath.Join("testdata", "fake_briefing_worker.py")},
		ProxyOpener: func(taskID, traceID string) (string, string, func() error, error) {
			gotTaskID, gotTraceID = taskID, traceID
			return "http://127.0.0.1:0", "fake-key", func() error { closed = true; return nil }, nil
		},
	}
	env := BuildEnvelope("t-proxy", "trace-proxy", "prompt text", time.Now())

	if _, err := p.Spawn(context.Background(), env); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if gotTaskID != "t-proxy" || gotTraceID != "trace-proxy" {
		t.Errorf("ProxyOpener called with (%q, %q), want (t-proxy, trace-proxy)", gotTaskID, gotTraceID)
	}
	if !closed {
		t.Error("ProxyOpener's closeFn was never called")
	}
}

// TestProcessSpawnerProxyOpenerErrorFailsClosed proves a ProxyOpener
// failure (e.g. the per-task listener could not be started) surfaces as a
// Spawn error, never a silent fallback to unproxied/uncredentialed
// worker traffic.
func TestProcessSpawnerProxyOpenerErrorFailsClosed(t *testing.T) {
	p := ProcessSpawner{
		Cmd: []string{"python3", filepath.Join("testdata", "fake_briefing_worker.py")},
		ProxyOpener: func(taskID, traceID string) (string, string, func() error, error) {
			return "", "", nil, errors.New("simulated proxy open failure")
		},
	}
	env := BuildEnvelope("t-proxy-err", "trace-proxy-err", "prompt text", time.Now())
	if _, err := p.Spawn(context.Background(), env); err == nil {
		t.Fatal("Spawn: expected an error when ProxyOpener fails")
	}
}
