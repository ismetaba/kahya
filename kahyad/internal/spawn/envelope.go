// Package spawn implements kahyad's per-task worker process lifecycle
// (HANDOFF §4 ⚑ IPC contract): one Python worker subprocess per task, a
// JSON envelope written to its stdin then stdin closed, trace_id/task_id
// propagated via the environment, its JSONL stdout protocol relayed live,
// and its whole process GROUP (Setpgid) killed on timeout so nothing is
// ever left orphaned. This file (envelope.go) is the envelope struct and
// its validation; spawn.go is the process lifecycle itself.
//
// The full contract (envelope shape, worker env, stdout protocol) is
// frozen in docs/ipc.md - that file is the deliverable "IPC sözleşmesi";
// this code must match it exactly, not the other way around.
package spawn

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion is envelope v1's schema_version, frozen in docs/ipc.md.
// Bump only alongside a documented, backward-compatible migration plan -
// never silently.
const SchemaVersion = 1

// LaneSecret/LaneNormal are the two values Envelope.Lane may hold (W3-08).
// This mirrors kahyad/internal/secretlane's OWN identical constants rather
// than importing that package (the same "duplicate two literals rather
// than add a dependency" convention kahyad/internal/anthproxy already uses
// for config.CredentialModeKeychain/Passthrough - see that package's own
// doc comment) - envelope.go is a low-level IPC-contract file that should
// not depend on the higher-level secretlane policy package; keep the two
// copies in sync by hand if either ever changes.
const (
	LaneSecret = "secret"
	LaneNormal = "normal"
)

// AllowedModels is the HANDOFF §9 cloud model set envelope.Model is
// validated against. The routing decision is Go's, never the prompt's
// (HANDOFF §4: "karar Go kodunda, istemde değil") - a static
// cfg.default_model is correct for W1-2; the full intent router landing in
// W4-08 will still only ever pick from this same set.
var AllowedModels = map[string]bool{
	"claude-opus-4-8":  true,
	"claude-sonnet-5":  true,
	"claude-haiku-4-5": true,
	"claude-fable-5":   true,
}

// Envelope is the single JSON object written to a worker's stdin (then
// stdin is closed) - one per spawned task (HANDOFF §4 IPC ⚑, frozen in
// docs/ipc.md). SessionID is a pointer specifically so json.Marshal
// renders it as the literal JSON `null` when nil (per the frozen schema:
// "session_id always present, null for new tasks") without any bespoke
// marshaling code - encoding/json already does this for a nil pointer.
type Envelope struct {
	SchemaVersion   int     `json:"schema_version"`
	TaskID          string  `json:"task_id"`
	TraceID         string  `json:"trace_id"`
	SessionID       *string `json:"session_id"`
	Kind            string  `json:"kind"`
	Prompt          string  `json:"prompt"`
	Model           string  `json:"model"`
	MemoryInjection bool    `json:"memory_injection"`
	CreatedAt       string  `json:"created_at"`

	// Lane is W3-08's secret-lane routing decision: "secret" | "normal".
	// kahyad's ingest-time classifier (kahyad/internal/secretlane) decides
	// this BEFORE the worker is ever spawned (HANDOFF §4 ⚑ ordering
	// invariant) - the worker reads it, it NEVER chooses or overrides it.
	// Empty is treated identically to "normal" (LaneNormal) by every
	// reader - Validate accepts empty specifically so every envelope built
	// before W3-08 (and every existing test fixture) keeps validating
	// unchanged; a real POST /v1/task handler always sets this explicitly
	// now (never leaves it blank).
	Lane string `json:"lane,omitempty"`
	// Category is the secret-lane category the classifier assigned
	// ("finans"|"saglik"|"kimlik"|"none") - informational only (Telegram
	// redaction / CLI badge / logs), never itself a security boundary.
	Category string `json:"category,omitempty"`

	// Resume is W4-02's session-resume flag: true iff kahyad/internal/
	// outbox.Dispatcher is re-spawning a worker for a task that already has
	// a persisted SessionID (HANDOFF §4 IPC ⚑: "W4 oturum devami session_id
	// ile"). false (the default, omitted from the wire form) is every
	// ordinary first-spawn envelope - the exact same shape every pre-W4-02
	// caller/test already builds. When true, SessionID MUST be non-nil and
	// non-empty (Validate enforces this); the worker (kahya_worker.
	// __main__._build_options) then constructs ClaudeAgentOptions with
	// resume=<session_id> instead of starting a fresh conversation - see
	// docs/ipc.md's W4-02 note.
	Resume bool `json:"resume,omitempty"`

	// Mode is W4-03's Reader-mode flag: "" (the default, every pre-W4-03
	// envelope/test) is an ordinary Actor/chat session exactly as before;
	// ModeReader spawns the worker TOOLLESS (no MCP servers, no memory
	// injection hook, no can_use_tool wiring at all - kahya_worker.
	// __main__._run_reader_session) for the cloud-Haiku half of the W4-03
	// Reader/Actor split (kahyad/internal/reader.Runner never picks this
	// path for secret-lane content - that goes straight to the local Qwen
	// server over HTTP, never through this envelope/worker at all - see
	// that package's own doc comment). Schema MUST be non-empty when Mode
	// is ModeReader (Validate enforces this) - it names the registered Go
	// struct (kahyad/internal/reader.JobTypeMailSummary/
	// JobTypeWebpageExtract) the caller will parse the worker's single JSON
	// object response into; the worker itself does nothing with Schema
	// beyond echoing it into its own JSONL logs - schema enforcement is
	// entirely Go-side, after this envelope's own job is done.
	Mode string `json:"mode,omitempty"`
	// Schema names the registered Reader job type (see Mode's own doc
	// comment). Empty unless Mode == ModeReader.
	Schema string `json:"schema,omitempty"`
}

// ModeReader is Envelope.Mode's one non-empty value (W4-03).
const ModeReader = "reader"

// NewTaskID mints a task_id shaped "t_<hex32>" (16 random bytes, hex
// encoded - the same entropy/encoding convention as
// kahyad/internal/traceid.New, just prefixed so a task_id and a trace_id
// are never visually confusable in logs/ledger rows that carry both).
func NewTaskID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read only fails if the OS entropy source is broken;
		// there is no safe fallback for a security-relevant identifier.
		panic(fmt.Sprintf("spawn: crypto/rand unavailable: %v", err))
	}
	return "t_" + hex.EncodeToString(b)
}

// NewAPIKey mints a per-task ANTHROPIC_API_KEY value
// ("kahya-task-<hex32>") - a random token, NOT a real Anthropic key. The
// real key never leaves kahyad (HANDOFF §4 IPC ⚑); once W12-08's per-task
// forward-proxy listener lands, it rejects any inbound request whose key
// does not match this exact token, so no other local process can spend
// through kahyad's real key just by guessing ANTHROPIC_API_KEY.
func NewAPIKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("spawn: crypto/rand unavailable: %v", err))
	}
	return "kahya-task-" + hex.EncodeToString(b)
}

// Validate checks every envelope invariant this task freezes: exactly
// SchemaVersion, non-blank task_id/trace_id, kind=="chat" (the only kind
// W1-2 ever sends - background/scheduled kinds are later work), a
// non-blank prompt, model in AllowedModels, and a created_at that parses
// as RFC3339.
func (e Envelope) Validate() error {
	if e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("spawn: schema_version = %d, want %d", e.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(e.TaskID) == "" {
		return fmt.Errorf("spawn: task_id must not be empty")
	}
	if strings.TrimSpace(e.TraceID) == "" {
		return fmt.Errorf("spawn: trace_id must not be empty")
	}
	if e.Kind != "chat" {
		return fmt.Errorf("spawn: kind = %q, want \"chat\"", e.Kind)
	}
	if strings.TrimSpace(e.Prompt) == "" {
		return fmt.Errorf("spawn: prompt must not be empty")
	}
	if !AllowedModels[e.Model] {
		return fmt.Errorf("spawn: model = %q not in the HANDOFF §9 cloud model set", e.Model)
	}
	if _, err := time.Parse(time.RFC3339, e.CreatedAt); err != nil {
		return fmt.Errorf("spawn: created_at = %q not RFC3339: %w", e.CreatedAt, err)
	}
	if e.Lane != "" && e.Lane != LaneSecret && e.Lane != LaneNormal {
		return fmt.Errorf("spawn: lane = %q, want %q, %q, or empty", e.Lane, LaneSecret, LaneNormal)
	}
	if e.Resume && (e.SessionID == nil || strings.TrimSpace(*e.SessionID) == "") {
		return fmt.Errorf("spawn: resume = true requires a non-empty session_id")
	}
	if e.Mode != "" && e.Mode != ModeReader {
		return fmt.Errorf("spawn: mode = %q, want %q or empty", e.Mode, ModeReader)
	}
	if e.Mode == ModeReader && strings.TrimSpace(e.Schema) == "" {
		return fmt.Errorf("spawn: mode = %q requires a non-empty schema", ModeReader)
	}
	return nil
}

// Marshal encodes the envelope exactly as written to the worker's stdin.
func (e Envelope) Marshal() ([]byte, error) {
	return json.Marshal(e)
}
