package reader

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kahya/kahyad/internal/spawn"
)

// TestWorkerCloudModelBuildsReaderEnvelopeAndAccumulatesDeltas is the
// envelope-shape test covering the deferred cloud lane's own logic (task
// spec: "its logic is covered by the proxy-counter + envelope tests" -
// this is the envelope half). No claude-agent-sdk/network call is
// involved - a small fake python script (mirroring kahyad/internal/spawn's
// own testdata/echo_worker.py convention) stands in for the real worker.
func TestWorkerCloudModelBuildsReaderEnvelopeAndAccumulatesDeltas(t *testing.T) {
	script := filepath.Join("testdata", "fake_reader_worker.py")

	var mu sync.Mutex
	var stderrLines []string
	m := WorkerCloudModel{
		Cmd: []string{"python3", script},
		OnStderr: func(line string) {
			mu.Lock()
			stderrLines = append(stderrLines, line)
			mu.Unlock()
		},
	}

	content, err := m.Read(context.Background(), JobTypeMailSummary, []byte("test icerik"), "trace-cloud-model")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	var v MailSummaryV1
	if err := json.Unmarshal([]byte(content), &v); err != nil {
		t.Fatalf("Read() returned non-JSON content (deltas not correctly concatenated?): %v\ncontent=%q", err, content)
	}
	if v.Summary != "canned test summary" {
		t.Errorf("Summary = %q, want %q", v.Summary, "canned test summary")
	}

	mu.Lock()
	diag := strings.Join(stderrLines, "\n")
	mu.Unlock()
	if !strings.Contains(diag, "mode=reader") {
		t.Errorf("worker stderr diagnostics = %q, want it to contain mode=reader", diag)
	}
	if !strings.Contains(diag, "schema="+JobTypeMailSummary) {
		t.Errorf("worker stderr diagnostics = %q, want it to contain schema=%s", diag, JobTypeMailSummary)
	}
	if !strings.Contains(diag, "model="+defaultCloudReaderModel) {
		t.Errorf("worker stderr diagnostics = %q, want it to contain model=%s (the HANDOFF §4 routing default)", diag, defaultCloudReaderModel)
	}
	// prompt_len must be large (system instructions + separator + content),
	// never just the 11-byte raw content alone - proving the combined
	// system-instructions+content string actually reached the envelope.
	if strings.Contains(diag, "prompt_len=11") {
		t.Errorf("worker stderr diagnostics = %q, want prompt_len to include the system instructions, not just the raw content", diag)
	}
}

// TestWorkerCloudModelRejectsInvalidEnvelopeBeforeSpawning proves an
// unregistered job type is rejected BEFORE any process is ever spawned
// (systemPromptFor's own error path).
func TestWorkerCloudModelRejectsInvalidEnvelopeBeforeSpawning(t *testing.T) {
	m := WorkerCloudModel{Cmd: []string{"python3", filepath.Join("testdata", "fake_reader_worker.py")}}
	if _, err := m.Read(context.Background(), "unknown_job_type_v1", []byte("x"), "trace-x"); err == nil {
		t.Fatal("Read: expected an error for an unregistered job type")
	}
}

// TestWorkerCloudModelPropagatesWorkerErrorStatus proves a worker that
// exits with an "error" protocol line surfaces as a Go error, not a
// silently-empty success.
func TestWorkerCloudModelPropagatesWorkerErrorStatus(t *testing.T) {
	script := filepath.Join("testdata", "fake_reader_error_worker.py")
	m := WorkerCloudModel{Cmd: []string{"python3", script}}
	if _, err := m.Read(context.Background(), JobTypeMailSummary, []byte("x"), "trace-err"); err == nil {
		t.Fatal("Read: expected an error for a worker that reports status=error")
	}
}

// sanity: spawn.ModeReader is exactly "reader" (this test file's own
// diagnostics compare against the literal string via the fake worker
// script, not this constant, so this pins the constant's value too).
func TestSpawnModeReaderConstant(t *testing.T) {
	if spawn.ModeReader != "reader" {
		t.Fatalf("spawn.ModeReader = %q, want %q", spawn.ModeReader, "reader")
	}
}
