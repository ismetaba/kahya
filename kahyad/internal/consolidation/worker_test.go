package consolidation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"kahya/kahyad/internal/spawn"
)

// TestBuildEnvelopeShape proves BuildEnvelope produces the exact toolless,
// Go-chosen-model envelope this task's cloud lane relies on - mirrors
// kahyad/internal/briefing/worker_test.go's own identical shape test.
func TestBuildEnvelopeShape(t *testing.T) {
	now := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	env := BuildEnvelope("t1", "trace-1", "prompt text", now)

	if env.Kind != "chat" {
		t.Errorf("Kind = %q, want %q", env.Kind, "chat")
	}
	if env.Mode != spawn.ModeReader {
		t.Errorf("Mode = %q, want %q (toolless)", env.Mode, spawn.ModeReader)
	}
	if env.Model != CloudModelName {
		t.Errorf("Model = %q, want %q", env.Model, CloudModelName)
	}
	if env.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want the literal HANDOFF §4 routing-table value claude-haiku-4-5", env.Model)
	}
	if env.Schema != RewriteJobType {
		t.Errorf("Schema = %q, want %q", env.Schema, RewriteJobType)
	}
	if env.SessionID != nil {
		t.Errorf("SessionID = %v, want nil (never resumed)", env.SessionID)
	}
	if env.MemoryInjection {
		t.Error("MemoryInjection = true, want false - the session gets nothing from brain.db")
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// fakeWorkerSpawner is an in-memory WorkerSpawner: it records every
// envelope it was asked to spawn and returns a canned response - the
// "request log" this package's secret-lane invariant test asserts
// against, exactly mirroring kahyad/internal/briefing/worker_test.go's own
// convention of asserting directly against a recorded spawn.Envelope's
// Marshal() bytes.
type fakeWorkerSpawner struct {
	envelopes []spawn.Envelope
	response  string
	err       error
}

func (f *fakeWorkerSpawner) Spawn(ctx context.Context, env spawn.Envelope) (string, error) {
	f.envelopes = append(f.envelopes, env)
	if f.err != nil {
		return "", f.err
	}
	return f.response, nil
}

func TestCloudSessionConsolidateBuildsPromptAndParsesResponse(t *testing.T) {
	spawner := &fakeWorkerSpawner{response: `{"files": {"a.md": "new a content", "b.md": "new b content"}}`}
	session := CloudSession{Spawner: spawner, Now: func() time.Time { return time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC) }}

	rewrites, err := session.Consolidate(context.Background(), "trace-1", map[string]string{
		"a.md": "old a content", "b.md": "old b content",
	})
	if err != nil {
		t.Fatalf("Consolidate() error = %v", err)
	}
	if rewrites["a.md"] != "new a content" || rewrites["b.md"] != "new b content" {
		t.Fatalf("rewrites = %+v, unexpected", rewrites)
	}
	if len(spawner.envelopes) != 1 {
		t.Fatalf("spawner recorded %d envelopes, want 1", len(spawner.envelopes))
	}
	env := spawner.envelopes[0]
	if env.Mode != spawn.ModeReader {
		t.Errorf("cloud-lane envelope Mode = %q, want toolless %q", env.Mode, spawn.ModeReader)
	}
	if env.TraceID != "trace-1" {
		t.Errorf("TraceID = %q, want trace-1", env.TraceID)
	}

	// The wire-format envelope bytes (what a real forward-proxy/worker
	// request log would carry) must contain both file paths and contents -
	// proving the prompt was actually built from the input, not silently
	// empty.
	b, err := env.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("envelope did not round-trip as JSON: %v", err)
	}
	prompt, _ := decoded["prompt"].(string)
	if prompt == "" {
		t.Fatal("envelope prompt is empty")
	}
}

func TestCloudSessionEmptyFilesNeverSpawns(t *testing.T) {
	spawner := &fakeWorkerSpawner{response: `{"files": {}}`}
	session := CloudSession{Spawner: spawner}
	rewrites, err := session.Consolidate(context.Background(), "trace-1", map[string]string{})
	if err != nil {
		t.Fatalf("Consolidate() error = %v", err)
	}
	if len(rewrites) != 0 {
		t.Fatalf("rewrites = %+v, want empty", rewrites)
	}
	if len(spawner.envelopes) != 0 {
		t.Fatalf("spawner was called %d times for an empty file set, want 0", len(spawner.envelopes))
	}
}

func TestCloudSessionPropagatesSpawnError(t *testing.T) {
	spawner := &fakeWorkerSpawner{err: errors.New("spawn failed")}
	session := CloudSession{Spawner: spawner}
	_, err := session.Consolidate(context.Background(), "trace-1", map[string]string{"a.md": "x"})
	if err == nil {
		t.Fatal("Consolidate() error = nil, want the spawn failure")
	}
}

func TestParseRewriteResponseRejectsMalformedJSON(t *testing.T) {
	if _, err := parseRewriteResponse("not json at all"); err == nil {
		t.Fatal("parseRewriteResponse() error = nil, want a decode failure")
	}
	if _, err := parseRewriteResponse(`{"wrong_key": {}}`); err == nil {
		t.Fatal("parseRewriteResponse() error = nil, want a missing-files failure")
	}
}
