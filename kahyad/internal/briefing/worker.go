// worker.go builds the W5-01 briefing worker envelope and spawns it.
//
// The briefing session reuses the ALREADY-BUILT W4-03 toolless Reader-mode
// worker profile (worker/kahya_worker/__main__.py's _run_reader_session /
// _build_reader_options: no MCP servers, tools=[], a can_use_tool that
// denies everything unconditionally) rather than inventing a fourth
// envelope Kind or a new worker code path: Mode = spawn.ModeReader with
// Schema = SummaryJobType IS this task's "briefing session profile" - a
// fixed (model, toolless-mode, Turkish prompt template, schema label)
// recipe, exactly as generic and reusable as kahyad/internal/reader's own
// mail_summary_v1/webpage_extract_v1 jobs, just registered/validated in
// THIS package instead of that one (this package's own ordering-invariant
// gate already ran BEFORE any of this is ever built - see gate.go - so
// there is no need for reader.Runner's whole-blob secret-vs-normal
// classify-and-route machinery here; every byte reaching this file has
// already been proven safe for the cloud).
//
// Kind stays "chat" (spawn.Envelope.Validate's existing, frozen contract -
// see docs/ipc.md) precisely so NO change is needed to spawn/envelope.go
// or worker/kahya_worker/envelope.py: Mode=ModeReader + a toolless,
// zero-MCP-server session is already strictly stronger than the "R-class
// tools only" floor HANDOFF §5 safety #2 sets for an untrusted session -
// zero tools is a fortiori R-only.
package briefing

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"kahya/kahyad/internal/spawn"
)

// ModelName is the HANDOFF §4 routing-table model for the briefing
// summarizer - claude-haiku-4-5, set Go-side (spawn.Envelope.Model), never
// chosen by the worker. Duplicated as a plain literal (rather than
// importing kahyad/internal/router) matching this codebase's existing
// "a low-level IPC-contract-adjacent package duplicates a routing literal
// instead of adding a dependency" convention (spawn/envelope.go's own
// LaneSecret/LaneNormal doc comment; kahyad/internal/reader/cloud_model.go
// sources the SAME literal from router.SelectModel at package-init time
// instead - this package keeps it a bare constant since it has exactly
// one caller and one value, never a table of them).
const ModelName = "claude-haiku-4-5"

// SummaryJobType is the Schema value this package's envelopes carry - an
// informational label only (envelope.go: "the worker does nothing with
// Schema beyond echoing it into its own JSONL logs; schema enforcement is
// entirely Go-side" - ValidateBriefingSummaryV1 in summary.go is that
// Go-side enforcement).
const SummaryJobType = "briefing_summary_v1"

// defaultSpawnTimeout bounds one briefing worker call when
// ProcessSpawner.Timeout is unset - mirrors kahyad/internal/reader.
// WorkerCloudModel's own defaultCloudReaderTimeout.
const defaultSpawnTimeout = 2 * time.Minute

// BuildEnvelope builds the tainted, toolless briefing-summary envelope for
// one run: Kind="chat" (unchanged, frozen contract), Mode=spawn.ModeReader
// (toolless - this file's own doc comment), Model=ModelName (Go-side,
// never worker-chosen), SessionID=nil/Resume=false (a fresh session every
// run - the briefing has no notion of "resuming" a prior day's
// conversation), MemoryInjection=false (the prompt already carries every
// byte the summarizer needs; there is nothing in brain.db this untrusted
// session should ever be handed). now is threaded through explicitly
// (rather than time.Now() internally) so a test can assert CreatedAt
// without a real-time race.
func BuildEnvelope(taskID, traceID, prompt string, now time.Time) spawn.Envelope {
	return spawn.Envelope{
		SchemaVersion:   spawn.SchemaVersion,
		TaskID:          taskID,
		TraceID:         traceID,
		SessionID:       nil,
		Kind:            "chat",
		Prompt:          prompt,
		Model:           ModelName,
		MemoryInjection: false,
		CreatedAt:       now.UTC().Format(time.RFC3339),
		Mode:            spawn.ModeReader,
		Schema:          SummaryJobType,
	}
}

// WorkerSpawner is the narrow "run the tainted briefing worker session,
// return its raw (unvalidated) text output" surface Orchestrator.Run
// needs. ProcessSpawner is the production implementation (spawn.Run
// against the real worker); tests inject a fake that records the exact
// env it received - the ordering-invariant test asserts directly against
// env.Marshal()'s JSON bytes, the real wire-format spawn.Envelope
// produces, so "never appears in the worker envelope" is checked against
// the actual production type, not a stand-in.
type WorkerSpawner interface {
	Spawn(ctx context.Context, env spawn.Envelope) (rawJSON string, err error)
}

// ProxyOpener opens a fresh per-task Anthropic forward-proxy listener,
// exactly the shape kahyad/internal/server.Server.NewTaskProxy and
// kahyad/internal/outbox.Dispatcher.SetAnthproxyOpener already share
// (main.go wires ALL THREE call sites to the SAME srv.NewTaskProxy, so a
// briefing worker's model call passes through the identical W12-08 cost-
// governor/egress-gate/cache-hit machinery every other task's call does -
// never a second, differently-wired copy). The returned closeFn must be
// called once Spawn is done with the listener.
type ProxyOpener func(taskID, traceID string) (baseURL, apiKey string, closeFn func() error, err error)

// ProcessSpawner is the production WorkerSpawner: spawns the worker via
// kahyad/internal/spawn.Run exactly like an ordinary task/Reader-mode call
// does, collecting its streamed delta text into the single JSON object the
// caller then decodes/validates (summary.go).
type ProcessSpawner struct {
	Cmd            []string
	Socket         string
	LogDir         string
	MCPBridgePath  string
	CredentialMode string

	// ProxyOpener mints a fresh per-run Anthropic forward-proxy listener
	// (production - main.go sets this to srv.NewTaskProxy). When nil,
	// Spawn falls back to the static AnthropicBaseURL/APIKey fields below
	// unchanged for the whole lifetime of this ProcessSpawner value - this
	// package's own tests (a fake python script that never actually calls
	// the Claude Agent SDK, so no real proxy is ever dialed) rely on
	// exactly this fallback.
	ProxyOpener      ProxyOpener
	AnthropicBaseURL string
	APIKey           string
	// Timeout defaults to defaultSpawnTimeout when <= 0.
	Timeout time.Duration
}

var _ WorkerSpawner = ProcessSpawner{}

// Spawn implements WorkerSpawner.
func (p ProcessSpawner) Spawn(ctx context.Context, env spawn.Envelope) (string, error) {
	if err := env.Validate(); err != nil {
		return "", fmt.Errorf("briefing: invalid worker envelope: %w", err)
	}

	baseURL, apiKey := p.AnthropicBaseURL, p.APIKey
	if p.ProxyOpener != nil {
		u, k, closeProxy, err := p.ProxyOpener(env.TaskID, env.TraceID)
		if err != nil {
			return "", fmt.Errorf("briefing: open per-task Anthropic proxy: %w", err)
		}
		defer closeProxy()
		baseURL, apiKey = u, k
	}

	cfg := spawn.Config{
		Cmd: p.Cmd, Socket: p.Socket, LogDir: p.LogDir,
		AnthropicBaseURL: baseURL, APIKey: apiKey,
		MCPBridgePath: p.MCPBridgePath, CredentialMode: p.CredentialMode,
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultSpawnTimeout
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var mu sync.Mutex
	var deltas []string
	outcome, err := spawn.Run(callCtx, cfg, env, spawn.Callbacks{
		OnDelta: func(text string) {
			mu.Lock()
			deltas = append(deltas, text)
			mu.Unlock()
		},
	})
	if err != nil {
		return "", fmt.Errorf("briefing: spawn worker: %w", err)
	}
	if outcome.Status != spawn.StatusOK {
		msg := outcome.ErrMsg
		if msg == "" {
			msg = fmt.Sprintf("briefing worker ended with status %q", outcome.Status)
		}
		return "", fmt.Errorf("briefing: worker failed: %s", msg)
	}

	mu.Lock()
	content := strings.Join(deltas, "")
	mu.Unlock()
	return content, nil
}
