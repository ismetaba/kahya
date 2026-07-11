// task.go implements POST /v1/task (HANDOFF §4 IPC ⚑; SSE contract frozen
// by W12-06's kahya CLI client) and POST /policy/check (HANDOFF §4 IPC ⚑:
// HTTP-over-UDS, 5s timeout, fail-closed). Both are this task's (W12-07)
// deliverable. /policy/check reuses the EXACT SAME interim policy table
// mcp.go's /v1/mcp gate already consults (kahyad/internal/policy) - one
// table, two mount points, never a second copy (see that package's doc
// comment).
//
// The full contract - envelope, worker env, worker stdout protocol,
// /policy/check schema, and this file's SSE event shapes - is frozen in
// docs/ipc.md; that file is the deliverable "IPC sözleşmesi", not this
// code.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/notify"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store/sqlcgen"
)

// Request body size caps (BLOCKER 3 hardening: an unbounded body reaching
// json.Decode can blow either endpoint's fail-closed latency budget - a
// 64MB /policy/check body alone takes several seconds just to read, well
// past the documented 5s caller-side timeout - so both bodies are capped
// via http.MaxBytesReader BEFORE decoding, not after).
const (
	// policyCheckMaxBody caps POST /policy/check's request body: a real
	// tool_input is tiny, so 1 MiB is already generous - reading it can
	// never meaningfully eat into the 5s fail-closed budget.
	policyCheckMaxBody = 1 << 20 // 1 MiB
	// taskBodyMaxBytes caps POST /v1/task's request body (prompt +
	// optional trace_id): generous even for a very long prompt, but
	// bounded so an oversized body can't tie up the daemon decoding it.
	taskBodyMaxBytes = 8 << 20 // 8 MiB
)

// Turkish user-facing strings this file can emit (HANDOFF §3 language
// policy - byte-exact per the W12-07 task spec, never paraphrased).
const (
	// MsgTaskTimeout is the SSE "error" event message when task_timeout_min
	// elapses before the worker finishes (%d = cfg.task_timeout_min).
	MsgTaskTimeout = "Görev zaman aşımına uğradı (%d dk)."
	// MsgTaskUnexpectedExit is the SSE "error" event message when the
	// worker process ends (any exit code) without ever sending a terminal
	// "result"/"error" stdout line, OR when kahyad itself could not even
	// manage the process (spawn.Run's own error return) - both cases point
	// the user at the same diagnostic command (%s = trace_id).
	MsgTaskUnexpectedExit = "Görev beklenmedik şekilde sonlandı. Ayrıntı: kahya log --trace %s"
)

// TaskStore is the tasks-table persistence source POST /v1/task needs
// (W12-07 step 4). *store.Store's own sqlc-generated Queries
// (store.Store.Queries) satisfies this directly - InsertTask/
// UpdateTaskState/UpdateTaskSession already have exactly this shape, no
// adapter needed.
type TaskStore interface {
	InsertTask(ctx context.Context, arg sqlcgen.InsertTaskParams) (sqlcgen.Task, error)
	UpdateTaskState(ctx context.Context, arg sqlcgen.UpdateTaskStateParams) error
	UpdateTaskSession(ctx context.Context, arg sqlcgen.UpdateTaskSessionParams) error
}

// SetTaskStore wires POST /v1/task's tasks-table persistence. Call before
// Prepare(); /v1/task answers 503 until this is set (the same
// "unwired dependency" posture as SetSearcher/SetReindexer/SetMCPMemory
// elsewhere in this package).
func (s *Server) SetTaskStore(ts TaskStore) {
	s.taskStore = ts
}

// SetAnthproxy wires POST /v1/task's per-task Anthropic forward-proxy +
// cost governor (W12-08, HANDOFF §4 IPC ⚑): governor is the ONE shared,
// in-process kahyad/internal/anthproxy.Governor (boot-rebuilt once in
// main.go via anthproxy.Boot, then reused across every task for the rest
// of the process's life); notifier is the alarm/notification sink
// (kahyad/internal/notify); credential is the CredentialSource matching
// cfg.CredentialMode ("keychain" or "passthrough" — see
// kahyad/internal/anthproxy's package doc comment for the OWNER AUTH
// DECISION this selects between); egressGate may be nil (always-allow)
// until W3-05 fills in the real allowlist. Call before Prepare(); until
// this is set, handleTask answers 503 the same way it does when taskStore
// is unset (SetTaskStore's "unwired dependency" posture).
func (s *Server) SetAnthproxy(governor *anthproxy.Governor, notifier notify.Notifier, credential anthproxy.CredentialSource, egressGate func(*http.Request) error) {
	s.anthGovernor = governor
	s.anthNotifier = notifier
	s.anthCredential = credential
	s.anthEgressGate = egressGate
}

// taskRequest is POST /v1/task's request body - the exact shape
// kahyad/cmd/kahya/client.go already POSTs (W12-06 contract): {"prompt",
// "trace_id"}. trace_id follows the same optional-override pattern as
// memorySearchRequest.TraceID: when absent, handleTask falls back to
// withTraceLogging's own resolved trace id instead of minting an
// uncorrelated one.
type taskRequest struct {
	Prompt  string `json:"prompt"`
	TraceID string `json:"trace_id"`
}

// rfc3339Now is time.Now().UTC() formatted as plain RFC3339 (no
// fractional seconds) - the envelope's created_at field and this file's
// tasks-table timestamps both use this one convention, matching the
// envelope.Validate parse layout exactly (time.RFC3339) with no ambiguity
// about fractional-second handling.
func rfc3339Now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// handleTask implements POST /v1/task (W12-07 step 4): validates the
// prompt, mints a task_id/envelope, inserts the tasks row (state=
// "running"), ledgers task_spawned, then switches into an SSE response and
// spawns the worker (kahyad/internal/spawn), relaying delta/session events
// live and closing out with exactly one terminal "result" (status="ok") or
// "error" (Turkish message) SSE event - matching kahyad/cmd/kahya/
// client.go's StreamTask parser exactly (W12-06 contract).
func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	if s.taskStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "task engine not available")
		return
	}
	if s.anthGovernor == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "anthropic forward-proxy not available")
		return
	}

	// BLOCKER 3: cap the body BEFORE decoding - this happens before the SSE
	// response even starts (headers are written further below), so an
	// oversized body is rejected with a plain JSON error same as any other
	// pre-SSE validation failure, never left to decode arbitrarily large
	// input first.
	r.Body = http.MaxBytesReader(w, r.Body, taskBodyMaxBytes)
	var req taskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		status := http.StatusBadRequest
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSONError(w, status, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeJSONError(w, http.StatusBadRequest, "prompt must not be empty")
		return
	}

	traceID := req.TraceID
	if traceID == "" {
		traceID = traceIDFromContext(r)
	}
	log := s.log.With(traceID)

	taskID := spawn.NewTaskID()
	now := rfc3339Now()
	envelope := spawn.Envelope{
		SchemaVersion:   spawn.SchemaVersion,
		TaskID:          taskID,
		TraceID:         traceID,
		SessionID:       nil,
		Kind:            "chat",
		Prompt:          req.Prompt,
		Model:           s.cfg.DefaultModel,
		MemoryInjection: true,
		CreatedAt:       now,
	}
	if err := envelope.Validate(); err != nil {
		// Only reachable via a misconfigured cfg.default_model - prompt/
		// task_id/trace_id/created_at above are always well-formed by
		// construction.
		log.Error("task_envelope_invalid", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "invalid task envelope")
		return
	}
	envelopeJSON, err := envelope.Marshal()
	if err != nil {
		log.Error("task_envelope_marshal_failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "marshal envelope")
		return
	}

	// dbCtx (the request's own context) is used for the writes that happen
	// BEFORE the worker is spawned, while the client is still known to be
	// connected. It must NOT be used for anything after spawn.Run returns:
	// r.Context() is cancelled the moment the underlying connection closes
	// for ANY reason - not only a clean client exit, but also (verified
	// live during this task's manual verification) the CLI's OWN 30s
	// idle-read timeout giving up and closing its side while the server's
	// task_timeout_min is still counting down. Since taskCtx below is
	// derived FROM dbCtx, that same disconnect also cancels taskCtx early -
	// which is desirable for spawn.Run (no orphan worker survives a
	// disappeared client) - but it must never take down the bookkeeping
	// that RECORDS the outcome: persistCtx (a plain background context)
	// is used for every write from OnSession onward specifically so a
	// disconnected/timed-out client can never prevent kahyad from
	// recording that the task ended (state + ledger).
	dbCtx := r.Context()
	persistCtx := context.Background()

	if _, err := s.taskStore.InsertTask(dbCtx, sqlcgen.InsertTaskParams{
		ID:        taskID,
		TraceID:   traceID,
		SessionID: sql.NullString{},
		State:     "running",
		TaintTier: "untrusted",
		Model:     sql.NullString{String: envelope.Model, Valid: true},
		Envelope:  sql.NullString{String: string(envelopeJSON), Valid: true},
		UpdatedAt: now,
		CreatedAt: now,
	}); err != nil {
		log.Error("task_insert_failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "task insert failed")
		return
	}

	if s.eventLogger != nil {
		if err := s.eventLogger.LogEvent(dbCtx, traceID, "task_spawned", map[string]any{
			"task_id": taskID, "model": envelope.Model,
		}); err != nil {
			log.Warn("task_spawned_ledger_error", "err", err.Error())
		}
	}
	log.Info("task_spawned", "task_id", taskID, "model", envelope.Model)

	// W12-08: open this task's own ephemeral forward-proxy listener BEFORE
	// the SSE response starts, so a failure here is still a plain JSON
	// 500 like every other pre-stream validation failure - never a
	// mid-stream SSE error event. apiKey is minted once and used both as
	// the proxy's expected local auth token AND the worker's own
	// ANTHROPIC_API_KEY (docs/ipc.md: "kahya-task-<hex32>" - the real key
	// never leaves kahyad).
	apiKey := spawn.NewAPIKey()
	proxy, err := anthproxy.New(anthproxy.ProxyConfig{
		TaskID:         taskID,
		TraceID:        traceID,
		Token:          apiKey,
		UpstreamURL:    s.cfg.AnthropicUpstreamURL,
		CredentialMode: s.cfg.CredentialMode,
		Credential:     s.anthCredential,
		Governor:       s.anthGovernor,
		Notifier:       s.anthNotifier,
		EventLedger:    s.eventLogger,
		EgressGate:     s.anthEgressGate,
		PauseBudget: func(ctx context.Context, pausedTaskID string) error {
			return s.taskStore.UpdateTaskState(ctx, sqlcgen.UpdateTaskStateParams{
				State: "paused_budget", UpdatedAt: rfc3339Now(), ID: pausedTaskID,
			})
		},
	})
	if err != nil {
		log.Error("anthproxy_new_failed", "task_id", taskID, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "anthropic proxy init failed")
		return
	}
	anthropicBaseURL, err := proxy.Start()
	if err != nil {
		log.Error("anthproxy_start_failed", "task_id", taskID, "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "anthropic proxy start failed")
		return
	}
	defer proxy.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Error("task_streaming_unsupported")
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	writeSSE := func(event string, payload any) {
		b, err := json.Marshal(payload)
		if err != nil {
			log.Error("task_sse_marshal_failed", "event", event, "err", err.Error())
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	timeoutMin := s.cfg.TaskTimeoutMin
	taskCtx, cancel := context.WithTimeout(dbCtx, time.Duration(timeoutMin)*time.Minute)
	defer cancel()

	spawnCfg := spawn.Config{
		Cmd:    s.cfg.WorkerCmd,
		Socket: s.cfg.Socket,
		LogDir: s.cfg.LogDir,
		// W12-08: the worker talks only to THIS task's own ephemeral
		// forward-proxy listener, never the real upstream directly - see
		// the anthproxy.New/Start call above. apiKey is the same local
		// token that listener validates every inbound request against.
		AnthropicBaseURL: anthropicBaseURL,
		APIKey:           apiKey,
		// W12-09: the worker execs this path as its "kahya_memory" MCP
		// server's stdio command, and reads CredentialMode to decide which
		// startup env assertions apply (see kahya_worker.__main__).
		MCPBridgePath:  s.cfg.MCPBridgePath,
		CredentialMode: s.cfg.CredentialMode,
	}

	outcome, runErr := spawn.Run(taskCtx, spawnCfg, envelope, spawn.Callbacks{
		OnStart: func(pid int) {
			log.Info("task_worker_started", "task_id", taskID, "pid", pid)
		},
		OnDelta: func(text string) {
			writeSSE("delta", map[string]string{"text": text})
		},
		OnSession: func(sessionID string) {
			if sessionID == "" {
				return
			}
			if err := s.taskStore.UpdateTaskSession(persistCtx, sqlcgen.UpdateTaskSessionParams{
				SessionID: sql.NullString{String: sessionID, Valid: true},
				UpdatedAt: rfc3339Now(),
				ID:        taskID,
			}); err != nil {
				log.Warn("task_session_update_failed", "task_id", taskID, "err", err.Error())
			}
		},
		OnStderr: func(line string) {
			log.Warn("worker_stderr", "task_id", taskID, "line", line)
		},
	})

	if runErr != nil {
		log.Error("task_spawn_error", "task_id", taskID, "err", runErr.Error())
	}
	m := mapTaskOutcome(runErr, outcome, traceID, taskID, timeoutMin)

	if err := s.taskStore.UpdateTaskState(persistCtx, sqlcgen.UpdateTaskStateParams{
		State: m.finalState, UpdatedAt: rfc3339Now(), ID: taskID,
	}); err != nil {
		log.Error("task_state_update_failed", "task_id", taskID, "err", err.Error())
	}
	if s.eventLogger != nil {
		if err := s.eventLogger.LogEvent(persistCtx, traceID, m.ledgerKind, map[string]any{
			"task_id": taskID, "status": outcome.Status,
		}); err != nil {
			log.Warn("task_ledger_error", "kind", m.ledgerKind, "err", err.Error())
		}
	}
	log.Info(m.ledgerKind, "task_id", taskID)

	writeSSE(m.sseEvent, m.ssePayload)
}

// taskOutcomeMapping is spawn.Run's terminal outcome translated into the
// tasks-table state, ledger event kind, and SSE event kahyad answers with
// - factored out of handleTask into a pure function (mapTaskOutcome) so
// every branch (including the timeout one, which would otherwise need a
// real elapsed task_timeout_min - minutes-granularity, far too slow for a
// test) can be unit-tested directly against a synthetic spawn.Outcome.
type taskOutcomeMapping struct {
	finalState string
	ledgerKind string
	sseEvent   string
	ssePayload any
}

// mapTaskOutcome implements W12-07 step 4's terminal-state rules: runErr!=
// nil (kahyad itself could not manage the process) and
// outcome.Status==StatusError-with-no-message (worker exited, any code,
// without ever sending a terminal result/error line) both surface the same
// generic MsgTaskUnexpectedExit; StatusTimeout surfaces MsgTaskTimeout;
// StatusError-with-a-message passes the worker's own Turkish message
// through verbatim; anything else is the StatusOK success path.
func mapTaskOutcome(runErr error, outcome spawn.Outcome, traceID, taskID string, timeoutMin int) taskOutcomeMapping {
	switch {
	case runErr != nil:
		return taskOutcomeMapping{
			finalState: "error", ledgerKind: "task_error",
			sseEvent:   "error",
			ssePayload: map[string]string{"message": fmt.Sprintf(MsgTaskUnexpectedExit, traceID)},
		}
	case outcome.Status == spawn.StatusTimeout:
		return taskOutcomeMapping{
			finalState: "error", ledgerKind: "task_timeout",
			sseEvent:   "error",
			ssePayload: map[string]string{"message": fmt.Sprintf(MsgTaskTimeout, timeoutMin)},
		}
	case outcome.Status == spawn.StatusError:
		msg := outcome.ErrMsg
		if msg == "" {
			msg = fmt.Sprintf(MsgTaskUnexpectedExit, traceID)
		}
		return taskOutcomeMapping{
			finalState: "error", ledgerKind: "task_error",
			sseEvent:   "error",
			ssePayload: map[string]string{"message": msg},
		}
	default:
		return taskOutcomeMapping{
			finalState: "done", ledgerKind: "task_done",
			sseEvent: "result",
			ssePayload: map[string]any{
				"status": "ok", "task_id": taskID, "session_id": outcome.SessionID,
			},
		}
	}
}

// policyCheckRequest is POST /policy/check's request body (HANDOFF §4 IPC
// ⚑, frozen in docs/ipc.md; W3-02 adds scope - the ladder's third key
// dimension alongside tool/class, class itself is NEVER accepted from the
// caller, only ever resolved from the loaded policy.yaml).
type policyCheckRequest struct {
	TraceID   string          `json:"trace_id"`
	TaskID    string          `json:"task_id"`
	SessionID *string         `json:"session_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	Scope     string          `json:"scope,omitempty"`
}

// policyCheckResponse is POST /policy/check's response body (W3-02 adds
// pending_approval_id/token). Reason/PendingApprovalID/Token are omitted
// (not just empty) whenever they don't apply to the returned decision:
// PendingApprovalID only appears on "needs_approval"; Token only appears
// on "allow" for a side-effectful (non-R) class, so the calling tool can
// present it to POST /policy/consume-token before executing.
type policyCheckResponse struct {
	Decision          string `json:"decision"`
	Reason            string `json:"reason,omitempty"`
	Rule              string `json:"rule"`
	PendingApprovalID string `json:"pending_approval_id,omitempty"`
	Token             string `json:"token,omitempty"`
}

// malformedBodyDeny is the fixed response body a malformed POST
// /policy/check request gets: HTTP 400, but the body still says "deny" -
// per the task spec, "so a sloppy client can't parse an allow out of an
// error" - fail-closed applies to the transport layer too.
var malformedBodyDeny = policyCheckResponse{
	Decision: policy.ResultDeny,
	Reason:   "Geçersiz istek gövdesi (fail-closed).",
	Rule:     policy.RuleLadderV1,
}

// handlePolicyCheck implements POST /policy/check (HANDOFF §4 IPC ⚑:
// HTTP-over-UDS, 5s caller-side timeout, fail-closed on any error/timeout).
// It consults kahyad/internal/policy's W3-02 autonomy-ladder Engine - the
// EXACT SAME one mcp.go's /v1/mcp gate already consults (one engine, two
// mount points). No I/O beyond what Engine.Check itself does (one
// autonomy_state read, at most one approval_tokens/undo_windows write, one
// ledger insert), well inside the caller's 5s budget.
func (s *Server) handlePolicyCheck(w http.ResponseWriter, r *http.Request) {
	// BLOCKER 3: cap the body BEFORE decoding - the endpoint's whole 5s
	// fail-closed budget (HANDOFF §4 IPC ⚑) must never be at risk from an
	// oversized body alone (a 64MB body can take several seconds to read
	// on its own); a real tool_input is tiny, so 1 MiB is already
	// generous.
	r.Body = http.MaxBytesReader(w, r.Body, policyCheckMaxBody)

	var req policyCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Malformed body, or one that exceeded policyCheckMaxBody: no
		// tool_name/tool_input can be trusted from it, but the trace_id
		// withTraceLogging already resolved from the X-Kahya-Trace-Id
		// header (independent of the body) is still recorded, best-effort,
		// against the fail-closed "deny" actually returned below - so
		// evidence that a fail-closed deny happened is never silently
		// dropped just because the body itself couldn't be parsed.
		s.ledgerPolicyCheckDecodeFailure(r, err)
		status := http.StatusBadRequest
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, status, malformedBodyDeny)
		return
	}

	traceID := req.TraceID
	if traceID == "" {
		traceID = traceIDFromContext(r)
	}

	// W3-01: deny-all mode overrides the ladder engine entirely - even
	// memory_search - whenever policy.yaml failed to load/validate at
	// boot (see kahyad/internal/server.Server.SetDenyAll's doc comment).
	// Engine.Check is never called in this mode, so the deny is ledgered
	// here by hand instead (mirrors mcp.go's policyGateMiddleware).
	if s.denyAll {
		if s.eventLogger != nil {
			payload := map[string]any{
				"event": "policy_decision", "tool": policy.Canonicalize(req.ToolName),
				"task_id": req.TaskID, "decision": policy.ResultDeny,
				"rule": policy.RuleDenyAllV1, "reason": policy.ReasonDenyAll,
			}
			if err := s.eventLogger.LogEvent(r.Context(), traceID, "policy_decision", payload); err != nil {
				s.log.With(traceID).Warn("policy_decision_ledger_error", "err", err.Error())
			}
		}
		writeJSON(w, http.StatusOK, policyCheckResponse{Decision: policy.ResultDeny, Reason: policy.ReasonDenyAll, Rule: policy.RuleDenyAllV1})
		return
	}
	if s.policyEngine == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "policy engine not available")
		return
	}

	decision, err := s.policyEngine.Check(r.Context(), policy.CheckInput{
		Tool: req.ToolName, Scope: req.Scope, TaskID: req.TaskID, TraceID: traceID, ToolInput: req.ToolInput,
	})
	if err != nil {
		// Engine.Check already returned a fail-closed Deny decision AND
		// ledgered it even on error (its own doc comment) - err here is
		// purely diagnostic (e.g. a DB error), logged but not re-ledgered.
		s.log.With(traceID).Warn("policy_check_engine_error", "tool", req.ToolName, "err", err.Error())
	}

	writeJSON(w, http.StatusOK, policyCheckResponse{
		Decision:          decision.Result,
		Reason:            decision.Reason,
		Rule:              decision.Rule,
		PendingApprovalID: decision.PendingApprovalID,
		Token:             decision.Token,
	})
}

// ledgerPolicyCheckDecodeFailure records a best-effort policy_decision
// ledger row for a /policy/check request whose body could not be decoded
// at all - either genuinely malformed JSON, or one that tripped
// policyCheckMaxBody (BLOCKER 3). Nothing in the body itself (tool_name,
// tool_input, the body's own trace_id/task_id) can be trusted at this
// point, but the trace_id withTraceLogging already resolved from the
// X-Kahya-Trace-Id header is independent of the body and still available -
// fail-closed applies to the ledger too: record that a deny happened
// rather than silently drop the only evidence of it. Best-effort only:
// never blocks the response, and does nothing at all if no trace_id can
// be recovered (should not happen in production - every route is mounted
// through withTraceLogging in Prepare, which always resolves one).
func (s *Server) ledgerPolicyCheckDecodeFailure(r *http.Request, decodeErr error) {
	if s.eventLogger == nil {
		return
	}
	traceID := traceIDFromContext(r)
	if traceID == "" {
		return
	}
	payload := map[string]any{
		"trace_id": traceID,
		"decision": malformedBodyDeny.Decision,
		"rule":     malformedBodyDeny.Rule,
		"reason":   malformedBodyDeny.Reason,
		"error":    decodeErr.Error(),
	}
	if err := s.eventLogger.LogEvent(r.Context(), traceID, "policy_decision", payload); err != nil {
		s.log.With(traceID).Warn("policy_decision_ledger_error", "err", err.Error())
	}
}

// writeJSON writes v as a JSON body with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
