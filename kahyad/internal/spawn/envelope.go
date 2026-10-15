// Package spawn implements kahyad's per-task worker process lifecycle
// (HANDOFF §4 ⚑ IPC contract): one Python worker subprocess per task, a
// JSON envelope written to its stdin then stdin closed, trace_id/task_id
// propagated via the environment, its JSONL stdout protocol relayed live,
// and its whole process GROUP (it is started as its own session and
// group leader, Setsid) killed when the worker exits or times out, so no
// descendant that stayed in the group is left orphaned (a child that
// leaves the group with setpgid/setsid escapes that reap - see
// docs/ipc.md section 5a). This file (envelope.go) is the envelope struct and
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

	"kahya/kahyad/internal/router"
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
// (HANDOFF §4: "karar Go kodunda, istemde değil") - the intent router
// (W4-08) only ever picks from router.CurrentModels(). Since the G12-N2
// bump (owner decision G12-5 = B) the set is the current generation PLUS
// the legacy one (router.LegacyModels(), owner decision G12-7): a resumed
// task's envelope carries the tasks.model it was created with, so a
// legacy id must keep validating until no non-terminal task uses one; it
// is never selected for a new task. The worker mirror
// (worker/kahya_worker/envelope.py ALLOWED_MODELS) and the proxy's priced
// set are pinned equal to this one by kahyad/internal/server's
// TestModelIDsMatchHandoffSection9 / TestSpawnAllowedModelsMatchProxyDefault.
var AllowedModels = buildAllowedModels()

func buildAllowedModels() map[string]bool {
	out := map[string]bool{}
	for _, m := range router.CurrentModels() {
		out[m] = true
	}
	for _, m := range router.LegacyModels() {
		out[m] = true
	}
	return out
}

// CLIAuxiliaryModel is the model BuildEnv pins the bundled claude CLI's
// OWN auxiliary calls to (summaries, titles, compaction helpers;
// ANTHROPIC_SMALL_FAST_MODEL / ANTHROPIC_DEFAULT_HAIKU_MODEL, FIX-I-10).
// Left alone the CLI picks a DATED Haiku id the proxy's allowlist and
// pricing table do not list. Derived from a router constant (never a new
// literal); AllowedModels[CLIAuxiliaryModel] is pinned by a test.
//
// FIX-G12-N2: deliberately the LEGACY Haiku (claude-haiku-4-5), not
// router.ModelHaiku. The pinned CLI was built before Haiku 5.5 and its
// auxiliary requests may carry thinking.budget_tokens, non-default
// sampling parameters or an assistant prefill - all of which Haiku 5.5
// rejects with a 400 - so moving the auxiliary calls is a separate step
// gated on a live check (claude-haiku-5-5 here, no proxy.model_rejected /
// 400 in kahya log). The legacy id stays allowlisted and priced for as
// long as this points at it.
const CLIAuxiliaryModel = router.LegacyModelHaiku

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

	// Intent is W4-08's routing-table intent bucket
	// (kahyad/internal/router's Intent* constants - duplicated here as a
	// plain string field, never a router.RouteInput/RouteDecision type:
	// this low-level IPC-contract file must not depend on that
	// higher-level routing package, mirroring LaneSecret/LaneNormal's own
	// identical "duplicate rather than import" convention above).
	// Informational only (logs/CLI/ledger) - the routing DECISION already
	// happened server-side, before this envelope was built; the worker
	// never reads this to choose anything (§4: "karar Go kodunda, istemde
	// değil"). Optional/backward-compatible: omitted from the wire form
	// when empty, so every pre-W4-08 envelope/test still validates
	// unchanged.
	Intent string `json:"intent,omitempty"`
	// DeepThink is the "derin düşün" opt-in (W4-08): true iff the caller
	// requested it via the `kahya ask --derin` flag or the byte-exact
	// Turkish prompt prefix "derin düşün:" (detected and stripped
	// server-side, kahyad/internal/server's POST /v1/task handler - NEVER
	// model-detected). When true, kahyad/internal/router.SelectModel pins
	// Model to router.ModelFable UNCONDITIONALLY unless Lane=="secret"
	// (which outranks it) - this field itself carries no enforcement, it is
	// only the audit trail of why Model ended up being Fable. Optional/
	// backward-compatible: omitted when false.
	DeepThink bool `json:"deep_think,omitempty"`

	// InputAudioPath is W6-02's push-to-talk field: the absolute path to a
	// mono 16kHz wav to transcribe locally (worker/kahya_worker/stt.py,
	// `language="tr"` fixed - HANDOFF §4 stack row) before anything else
	// happens for this task. NEVER itself the ordering-invariant
	// enforcement: kahyad/internal/server's own stt-phase caller
	// (transcribeAudioLocally, stt.go) spawns a SEPARATE, always-local
	// Mode==ModeSTT envelope FIRST and only builds the REAL "chat" envelope
	// (which never carries this field) from the resulting transcript, once
	// classification has already run on it exactly like any typed prompt -
	// see that file's own doc comment for the full ordering argument. Set
	// ONLY on a Mode==ModeSTT envelope; empty/omitted on every other kind
	// (including the real chat envelope built from the transcript).
	InputAudioPath string `json:"input_audio_path,omitempty"`
}

// ModeReader is Envelope.Mode's W4-03 non-empty value: a toolless,
// cloud-Haiku Reader session (worker/kahya_worker/__main__.py's
// _run_reader_session).
const ModeReader = "reader"

// ModeSTT is Envelope.Mode's W6-02 non-empty value: a toolless, CLOUD-LESS
// local transcription session (worker/kahya_worker/__main__.py's
// _run_stt_only) - requires InputAudioPath non-blank (Validate below).
const ModeSTT = "stt"

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
	if e.Mode != "" && e.Mode != ModeReader && e.Mode != ModeSTT {
		return fmt.Errorf("spawn: mode = %q, want %q, %q, or empty", e.Mode, ModeReader, ModeSTT)
	}
	if e.Mode == ModeReader && strings.TrimSpace(e.Schema) == "" {
		return fmt.Errorf("spawn: mode = %q requires a non-empty schema", ModeReader)
	}
	if e.Mode == ModeSTT && strings.TrimSpace(e.InputAudioPath) == "" {
		return fmt.Errorf("spawn: mode = %q requires a non-empty input_audio_path", ModeSTT)
	}
	return nil
}

// Marshal encodes the envelope exactly as written to the worker's stdin.
func (e Envelope) Marshal() ([]byte, error) {
	return json.Marshal(e)
}
