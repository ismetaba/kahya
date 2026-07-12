// worker.go builds the W5-02 cloud-lane consolidation worker envelope and
// spawns it - the SAME toolless Reader-mode worker profile
// (worker/kahya_worker/__main__.py's _run_reader_session: no MCP servers,
// tools=[], a can_use_tool that denies everything unconditionally)
// kahyad/internal/briefing/worker.go and kahyad/internal/reader/
// cloud_model.go already reuse for their own one-shot, structured-JSON-
// output sessions, rather than inventing a fourth envelope Kind or a new
// worker code path (this package's own doc comment on worker/kahya_worker/
// consolidate.py explains why an interactive tool-loop session was NOT
// built for this task: Mode=ModeReader is strictly toolless, which is
// STRONGER than "no git access" - the worker touches no filesystem at all
// beyond its own process memory, and kahyad alone writes the returned
// rewrites into the worktree, worktree.go's own job).
//
// Model is claude-haiku-4-5 (HANDOFF §4 routing table row "Cikarim .
// geri-yazim (gizli-serit-disi onayli)") - Go-side, never worker-chosen
// (spawn.Envelope.Model), matching every other cloud-lane call site in
// this codebase.
package consolidation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"kahya/kahyad/internal/spawn"
)

// CloudModelName is the HANDOFF §4 routing-table model for the cloud
// (non-secret-lane) consolidation lane.
const CloudModelName = "claude-haiku-4-5"

// RewriteJobType is the Schema value this package's cloud-lane envelopes
// carry - purely informational (envelope.go: "the worker does nothing
// with Schema beyond echoing it into its own JSONL logs; schema
// enforcement is entirely Go-side" - parseRewriteResponse in session.go is
// that Go-side enforcement).
const RewriteJobType = "consolidation_rewrite_v1"

// defaultSpawnTimeout bounds one cloud-lane consolidation call when
// ProcessSpawner.Timeout is unset - mirrors kahyad/internal/briefing/
// worker.go's identical constant.
const defaultSpawnTimeout = 2 * time.Minute

// BuildEnvelope builds the toolless cloud-lane consolidation envelope for
// one run: Kind="chat" (unchanged, frozen contract - spawn/envelope.go/
// docs/ipc.md need no change), Mode=spawn.ModeReader (toolless),
// Model=CloudModelName (Go-side, never worker-chosen), fresh session every
// run (no resume - a nightly consolidation run has no notion of
// continuing a prior night's conversation), MemoryInjection=false (the
// prompt already carries every byte the session needs - there is nothing
// in brain.db this session should ever be handed, satisfying the WRITE
// BOUNDARY invariant on the READ side too).
func BuildEnvelope(taskID, traceID, prompt string, now time.Time) spawn.Envelope {
	return spawn.Envelope{
		SchemaVersion:   spawn.SchemaVersion,
		TaskID:          taskID,
		TraceID:         traceID,
		SessionID:       nil,
		Kind:            "chat",
		Prompt:          prompt,
		Model:           CloudModelName,
		MemoryInjection: false,
		CreatedAt:       now.UTC().Format(time.RFC3339),
		Mode:            spawn.ModeReader,
		Schema:          RewriteJobType,
	}
}

// WorkerSpawner is the narrow "run the toolless cloud-lane worker session,
// return its raw (unvalidated) text output" surface CloudSession needs.
// ProcessSpawner is the production implementation (spawn.Run against the
// real worker); every test in this package injects a fake that records
// the exact env it received - the secret-lane invariant test asserts
// directly against env.Marshal()'s JSON bytes, the real wire-format
// spawn.Envelope produces, exactly mirroring kahyad/internal/briefing/
// worker_test.go's own convention.
type WorkerSpawner interface {
	Spawn(ctx context.Context, env spawn.Envelope) (rawJSON string, err error)
}

// ProxyOpener opens a fresh per-task Anthropic forward-proxy listener -
// the same shape kahyad/internal/server.Server.NewTaskProxy and
// kahyad/internal/briefing.ProxyOpener already share, so a consolidation
// worker's model call passes through the identical W12-08 cost-governor/
// egress-gate/cache-hit machinery every other task's call does.
type ProxyOpener func(taskID, traceID string) (baseURL, apiKey string, closeFn func() error, err error)

// ProcessSpawner is the production WorkerSpawner.
type ProcessSpawner struct {
	Cmd            []string
	Socket         string
	LogDir         string
	MCPBridgePath  string
	CredentialMode string

	// ProxyOpener mints a fresh per-run Anthropic forward-proxy listener
	// (production - main.go sets this to srv.NewTaskProxy). nil falls back
	// to the static AnthropicBaseURL/APIKey fields unchanged.
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
		return "", fmt.Errorf("consolidation: invalid worker envelope: %w", err)
	}

	baseURL, apiKey := p.AnthropicBaseURL, p.APIKey
	if p.ProxyOpener != nil {
		u, k, closeProxy, err := p.ProxyOpener(env.TaskID, env.TraceID)
		if err != nil {
			return "", fmt.Errorf("consolidation: open per-task Anthropic proxy: %w", err)
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
		return "", fmt.Errorf("consolidation: spawn cloud-lane worker: %w", err)
	}
	if outcome.Status != spawn.StatusOK {
		msg := outcome.ErrMsg
		if msg == "" {
			msg = fmt.Sprintf("consolidation cloud-lane worker ended with status %q", outcome.Status)
		}
		return "", fmt.Errorf("consolidation: cloud-lane worker failed: %s", msg)
	}

	mu.Lock()
	content := strings.Join(deltas, "")
	mu.Unlock()
	return content, nil
}

// CloudSession is the production Session for the cloud (non-secret) lane.
type CloudSession struct {
	Spawner WorkerSpawner
	Now     func() time.Time // defaults to time.Now when nil
}

var _ Session = CloudSession{}

func (s CloudSession) Consolidate(ctx context.Context, traceID string, files map[string]string) (map[string]string, error) {
	if len(files) == 0 {
		return map[string]string{}, nil
	}
	prompt, err := buildRewritePrompt(files)
	if err != nil {
		return nil, err
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	env := BuildEnvelope(spawn.NewTaskID(), traceID, prompt, now())
	raw, err := s.Spawner.Spawn(ctx, env)
	if err != nil {
		return nil, err
	}
	return parseRewriteResponse(raw)
}
