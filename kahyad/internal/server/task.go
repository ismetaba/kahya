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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/mlx"
	"kahya/kahyad/internal/notify"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/router"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/taint"
	"kahya/kahyad/internal/task"
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
	// MsgSecretLaneModelCallFailed is the W3-08 secret-lane task's SSE
	// "error" event message when the local Qwen3-30B-A3B answer call
	// itself fails for a reason OTHER than ErrLocalModelUnavailable (which
	// gets its own exact fail-closed message - see handleSecretLaneTask).
	MsgSecretLaneModelCallFailed = "Yerel model çağrısı başarısız oldu. Ayrıntı: kahya log --trace %s"
	// MsgDerinDuringDowngrade is W4-08's spend-warning: prefixed as the
	// FIRST "delta" SSE event on a "derin düşün" task that is honored
	// despite an active 80% cost-governor downgrade (HANDOFF §4 cost
	// governor ⚑: an explicit derin opt-in is never itself downgraded, but
	// the user should know they are spending through an active downgrade).
	// Wording is this task's own choice (not byte-exact from any spec) -
	// English identifiers, Turkish user-facing text per CLAUDE.md.
	MsgDerinDuringDowngrade = "⚠️ Günlük bütçenin %80'i aşıldı; 'derin düşün' yine de çalıştırılıyor, ek maliyete dikkat edin."
)

// derinDusunPrefix is W4-08's deterministic, byte-exact Turkish opt-in
// prompt prefix (task spec: "derin düşün:", matched BYTE-EXACT in Go and
// STRIPPED from the prompt after detection - never model-detected). A
// prompt beginning with this prefix sets deep_think=true exactly like the
// `kahya ask --derin` flag does, and the prefix itself is removed before
// the (now-bare) prompt is classified/answered - the user never sees their
// own opt-in marker echoed back.
const derinDusunPrefix = "derin düşün:"

// detectAndStripDerinPrefix reports whether prompt begins with
// derinDusunPrefix (strings.HasPrefix - an ordinary byte-for-byte
// comparison, satisfying "matched BYTE-EXACT in Go") and, if so, returns
// the remainder with the prefix and any immediately-following whitespace
// removed.
func detectAndStripDerinPrefix(prompt string) (stripped string, matched bool) {
	if !strings.HasPrefix(prompt, derinDusunPrefix) {
		return prompt, false
	}
	return strings.TrimSpace(strings.TrimPrefix(prompt, derinDusunPrefix)), true
}

// TaskStore is the tasks-table persistence source POST /v1/task needs
// (W12-07 step 4). *store.Store's own sqlc-generated Queries
// (store.Store.Queries) satisfies this directly - InsertTask/
// UpdateTaskState/UpdateTaskSession already have exactly this shape, no
// adapter needed.
type TaskStore interface {
	InsertTask(ctx context.Context, arg sqlcgen.InsertTaskParams) (sqlcgen.Task, error)
	UpdateTaskState(ctx context.Context, arg sqlcgen.UpdateTaskStateParams) error
	UpdateTaskSession(ctx context.Context, arg sqlcgen.UpdateTaskSessionParams) error
	// SetTaskWorkerPGID persists the spawned worker's process-group id
	// (W6-03) - see that query's own doc comment in queries.sql for why
	// this exists ALONGSIDE the in-memory taskLiveRegistry: a worker
	// orphaned by a daemon crash/restart is still killable via this
	// column, since macOS has no PDEATHSIG.
	SetTaskWorkerPGID(ctx context.Context, arg sqlcgen.SetTaskWorkerPGIDParams) error
}

// SetTaskStore wires POST /v1/task's tasks-table persistence. Call before
// Prepare(); /v1/task answers 503 until this is set (the same
// "unwired dependency" posture as SetSearcher/SetReindexer/SetMCPMemory
// elsewhere in this package).
func (s *Server) SetTaskStore(ts TaskStore) {
	s.taskStore = ts
}

// SetSessionTaintDB wires the raw *sql.DB handle handleTask's OnSession
// callback needs to insert session_taint(tier=clean) in the SAME
// transaction as its own UpdateTaskSession write (W4-03 task spec step
// 1a). Call before Prepare(); main.go passes store.Store.DB() - the exact
// same underlying connection s.taskStore's sqlc Queries already run
// against, so a transaction opened here sees (and is seen by) the rest of
// this process consistently. nil (the default) is a documented no-op -
// see the Server.sessionTaintDB field's own doc comment.
func (s *Server) SetSessionTaintDB(db *sql.DB) {
	s.sessionTaintDB = db
}

// persistSessionStarted implements the W4-03 task spec step 1a: persist
// the worker-reported session_id onto this task's row (the pre-existing
// UpdateTaskSession write - W4-02) AND insert this task's OWN
// session_taint(tier=clean) row IN THE SAME DATABASE TRANSACTION (HANDOFF
// §5 safety #2: clean rows are born in exactly two places; a
// user-initiated task's own worker session, at the moment its session_id
// is first captured, is the FIRST of the two). Called only from
// handleTask's OnSession callback above - a FRESH spawn, never a resumed
// one (kahyad/internal/outbox.Dispatcher's own OnSession callback is a
// SEPARATE code path that never calls this, so a resumed unknown
// session_id correctly never gets a clean row inserted for it - the task
// spec's own "resume never inserts" rule holds structurally, by which
// code path even CAN reach this function, not by an extra runtime check).
//
// If sessionTaintDB was never wired (SetSessionTaintDB's "unwired
// dependency" doc comment), this falls back to the plain, non-
// transactional UpdateTaskSession call every pre-W4-03 caller/test already
// made - no session_taint row is inserted in that configuration.
func (s *Server) persistSessionStarted(ctx context.Context, traceID, taskID, sessionID string) error {
	now := rfc3339Now()
	if s.sessionTaintDB == nil {
		return s.taskStore.UpdateTaskSession(ctx, sqlcgen.UpdateTaskSessionParams{
			SessionID: sql.NullString{String: sessionID, Valid: true},
			UpdatedAt: now,
			ID:        taskID,
		})
	}

	tx, err := s.sessionTaintDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("session_started: begin tx: %w", err)
	}
	txq := sqlcgen.New(tx)
	if err := txq.UpdateTaskSession(ctx, sqlcgen.UpdateTaskSessionParams{
		SessionID: sql.NullString{String: sessionID, Valid: true},
		UpdatedAt: now,
		ID:        taskID,
	}); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("session_started: update task session: %w", err)
	}
	// A throwaway Tracker over the SAME tx-scoped Queries - see
	// kahyad/internal/taint.Store's own doc comment for why this is exactly
	// how a caller gets a transactional InsertClean without that package
	// needing to know anything about *sql.Tx itself. ledger is deliberately
	// nil here (NOT s.eventLogger): brain.db's connection pool is capped at
	// exactly 1 (kahyad/internal/store.Store's own doc comment), and this
	// goroutine is already holding that single connection via tx - a
	// ledger write through s.eventLogger's ORDINARY (non-tx) *sqlcgen.
	// Queries would try to acquire a SECOND connection from the same
	// exhausted pool and deadlock against itself. The should-never-happen
	// lower-attempt case is instead ledgered manually, below, AFTER
	// Rollback has released the connection.
	if err := taint.New(txq, nil).InsertClean(ctx, traceID, sessionID); err != nil {
		_ = tx.Rollback()
		if s.eventLogger != nil && errors.Is(err, taint.ErrLowerAttempt) {
			_ = s.eventLogger.LogEvent(ctx, traceID, taint.EventLowerAttempt, map[string]any{
				"event": taint.EventLowerAttempt, "session_id": sessionID,
			})
		}
		return fmt.Errorf("session_started: insert session_taint clean: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("session_started: commit: %w", err)
	}
	return nil
}

// SetAnthproxy wires POST /v1/task's per-task Anthropic forward-proxy +
// cost governor (W12-08, HANDOFF §4 IPC ⚑): governor is the ONE shared,
// in-process kahyad/internal/anthproxy.Governor (boot-rebuilt once in
// main.go via anthproxy.Boot, then reused across every task for the rest
// of the process's life); notifier is the alarm/notification sink
// (kahyad/internal/notify); credential is the CredentialSource matching
// cfg.CredentialMode ("keychain" or "passthrough" — see
// kahyad/internal/anthproxy's package doc comment for the OWNER AUTH
// DECISION this selects between); egressGateFactory may be nil
// (always-allow — pre-W3-05 behavior). Once set (main.go, W3-05), it is
// called ONCE per task, with that task's own taskID/traceID, to build the
// anthproxy.ProxyConfig.EgressGate closure THIS task's Proxy uses — the
// factory indirection exists because anthproxy.ProxyConfig.EgressGate's
// own fixed signature (func(*http.Request) error, from W12-08) carries no
// task/trace correlation of its own (the forwarded request is a real
// Anthropic API call, not a kahya-internal one), so task/trace identity
// is captured in a closure built fresh per task instead. Call before
// Prepare(); until this is set, handleTask answers 503 the same way it
// does when taskStore is unset (SetTaskStore's "unwired dependency"
// posture).
func (s *Server) SetAnthproxy(governor *anthproxy.Governor, notifier notify.Notifier, credential anthproxy.CredentialSource, egressGateFactory func(taskID, traceID string) func(*http.Request) error) {
	s.anthGovernor = governor
	s.anthNotifier = notifier
	s.anthCredential = credential
	s.anthEgressGateFactory = egressGateFactory
}

// SetSecretLane wires W3-08's local-only answer path and (for future
// ingestion points - see handleTask's own classification comment)
// Qwen-backed classifier (main.go, once kahyad/internal/mlx's Qwen3-30B-A3B
// supervisor is constructed). handleTask's OWN chat-prompt classification
// always runs secretlane.ClassifyDeterministic regardless of whether this
// method was ever called (it takes no model dependency at all); answerer
// is what actually answers a lane=="secret" task once classified - nil
// means a deterministic hit still gets DURABLY persisted onto the task row
// and the worker is still never spawned, but the SSE response reports
// MsgSecretLaneModelCallFailed rather than attempting anything (never a
// cloud fallback either way). markSensitiveRead is kahyad/internal/
// egress.Gate.MarkSensitiveRead's exact shape (task spec step 6:
// "classifier hit => POST /session/sensitive-read") - the SAME "keyed on
// trace_id" convention kahyad/internal/egress.NewAnthproxyEgressGateHook
// already uses; may be nil (best-effort, matching every other optional
// hook in this file). classifier itself is stored for a future ingestion
// point (memory_write/fs-read/mail-web Reader, W4-03) to consult via
// kahyad/internal/secretlane.Escalate - not read by handleTask today.
func (s *Server) SetSecretLane(classifier *secretlane.Classifier, answerer secretlane.Answerer, markSensitiveRead func(ctx context.Context, sessionKey, traceID string) error) {
	s.secretLaneClassifier = classifier
	s.secretLaneAnswerer = answerer
	s.markSensitiveRead = markSensitiveRead
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
	// DeepThink is W4-08's `kahya ask --derin` opt-in: true pins
	// claude-fable-5 (kahyad/internal/router.SelectModel), UNLESS the
	// classified lane is secret (which outranks it). Optional/backward-
	// compatible: absent/false is every pre-W4-08 request body's exact
	// existing behavior. The OTHER opt-in form - the byte-exact Turkish
	// prompt prefix "derin düşün:" - is detected server-side in handleTask
	// itself (never client-side), so it needs no request-body field of its
	// own.
	DeepThink bool `json:"deep_think"`
	// PaletteOpenedAt is W6-01's `kahya ask --palette-opened-at
	// <unix-seconds-float>` flag / hammerspoon/kahya.lua's
	// hs.timer.secondsSinceEpoch(), captured at hotkey press (BEFORE the
	// palette's own hs.chooser even opens). When present, handleTask
	// ledgers a kind="palette_open" event carrying this exact value in its
	// payload, under this task's own trace_id (logPaletteOpen) - the
	// north-star "palet-aç→ilk-token" metric's start timestamp (HANDOFF §6
	// metric definitions ⚑; the metric QUERY itself is W78-04, this task
	// only records the two timestamps). Optional/backward-compatible:
	// absent (nil) is every pre-W6-01 request body's exact existing
	// behavior - no palette_open event is ever written for it.
	PaletteOpenedAt *float64 `json:"palette_opened_at,omitempty"`
	// InputAudioPath is W6-02's `kahya ask --audio <path>` field: an
	// absolute path to a mono 16kHz wav. When present, handleTask
	// transcribes it ENTIRELY LOCALLY (transcribeAudioLocally, stt.go)
	// BEFORE the ordinary blank-prompt check/classification/envelope
	// construction below, and overwrites req.Prompt with the resulting
	// transcript - see handleTask's own doc comment on why this is what
	// keeps a spoken command routing exactly like the identical typed one
	// (no separate/bypass classification path for voice input). Optional/
	// backward-compatible: absent is every pre-W6-02 request body's exact
	// existing behavior.
	InputAudioPath string `json:"input_audio_path,omitempty"`
}

// rfc3339Now is time.Now().UTC() formatted as plain RFC3339 (no
// fractional seconds) - the envelope's created_at field and this file's
// tasks-table timestamps both use this one convention, matching the
// envelope.Validate parse layout exactly (time.RFC3339) with no ambiguity
// about fractional-second handling.
func rfc3339Now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// NewTaskProxy builds and starts a fresh per-task Anthropic forward-proxy
// listener for taskID/traceID - the ONE construction both handleTask
// (first spawn, below) and kahyad/internal/outbox.Dispatcher's
// AnthproxyOpener (a later cloud-lane REDISPATCH, wired in main.go) share,
// so the governor/notifier/credential/egress-gate/cost-governor/cloud-
// retry-callback wiring can never drift between the two call sites - the
// exact drift that used to leave a resumed cloud-lane task unable to
// reach the cloud at all (kahyad/internal/outbox's own package doc
// comment, closed by this task).
//
// Returns the base URL to set as the worker's ANTHROPIC_BASE_URL, the
// per-task local auth token (ANTHROPIC_API_KEY), and a close func the
// caller must call once done with this listener (mirrors proxy.Close).
func (s *Server) NewTaskProxy(taskID, traceID string) (baseURL, apiKey string, closeFn func() error, err error) {
	apiKey = spawn.NewAPIKey()
	var egressGate func(*http.Request) error
	if s.anthEgressGateFactory != nil {
		egressGate = s.anthEgressGateFactory(taskID, traceID)
	}
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
		EgressGate:     egressGate,
		// W4-04: cloud-call error taxonomy / retry / task parking.
		MaxInlineRetries: s.cfg.CloudRetryMaxInline,
		JSONLLog:         s.log,
		PauseBudget: func(ctx context.Context, pausedTaskID string) error {
			return s.taskStore.UpdateTaskState(ctx, sqlcgen.UpdateTaskStateParams{
				State: "paused_budget", UpdatedAt: rfc3339Now(), ID: pausedTaskID,
			})
		},
		OnCloudUnreachable: func(ctx context.Context, tid string) error {
			if s.taskCloudRetry == nil {
				return nil
			}
			return s.taskCloudRetry.ParkOrGiveUp(ctx, traceID, tid)
		},
		OnNonRetryableFailure: func(ctx context.Context, tid, reasonID string) error {
			if s.taskCloudRetry == nil {
				return nil
			}
			return s.taskCloudRetry.FailNonRetryable(ctx, traceID, tid, reasonID)
		},
	})
	if err != nil {
		return "", "", nil, err
	}
	baseURL, err = proxy.Start()
	if err != nil {
		return "", "", nil, err
	}
	return baseURL, apiKey, proxy.Close, nil
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

	traceID := req.TraceID
	if traceID == "" {
		traceID = traceIDFromContext(r)
	}
	log := s.log.With(traceID)

	// W6-02 ORDERING INVARIANT (HANDOFF §4 ⚑ / this task's own "no voice
	// bypass" requirement): when input_audio_path is present, transcribe
	// it ENTIRELY LOCALLY - transcribeAudioLocally (stt.go) spawns the
	// worker in envelope.Mode==spawn.ModeSTT, which never constructs a
	// ClaudeAgentOptions/ClaudeSDKClient/MCP server and never opens an
	// Anthropic-proxy listener, so this call cannot reach the network
	// however it is implemented on the worker side - BEFORE the ordinary
	// blank-prompt check, secretlane.ClassifyDeterministic, intent
	// routing, envelope construction, or either the lane==secret/
	// decision.Local/cloud branch further below ever runs. req.Prompt is
	// then overwritten with the resulting transcript, so every line of
	// code from the blank-prompt check onward is the EXACT SAME,
	// unmodified path an ordinary typed prompt already goes through -
	// there is no separate, audio-specific routing decision anywhere past
	// this block. A spoken finance-flavored command therefore reaches
	// secretlane.ClassifyDeterministic on the identical transcript text a
	// typed version of the same command would have produced, and is
	// classified/routed identically.
	// writeSSE/flusher, once non-nil, are THE one SSE stream this response
	// ever opens for this request - populated either here (the
	// input_audio_path branch, when present) or by the ordinary open-SSE
	// block further below, never both: that later block (and
	// handleSecretLaneTask's own copy, which now takes writeSSE as a
	// parameter instead of always opening its own) only builds its own
	// flusher/writeSSE when these are still nil, so a single HTTP response
	// never gets a second, superfluous WriteHeader call.
	var writeSSE func(event string, payload any)
	var flusher http.Flusher

	if strings.TrimSpace(req.InputAudioPath) != "" {
		if !filepath.IsAbs(req.InputAudioPath) {
			writeJSONError(w, http.StatusBadRequest, "input_audio_path must be an absolute path")
			return
		}

		fl, ok := w.(http.Flusher)
		if !ok {
			log.Error("task_streaming_unsupported")
			writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		flusher = fl
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		writeSSE = func(event string, payload any) {
			b, err := json.Marshal(payload)
			if err != nil {
				log.Error("task_sse_marshal_failed", "event", event, "err", err.Error())
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			flusher.Flush()
		}

		transcript, sttErr := s.transcribeAudioLocally(r.Context(), log, traceID, req.InputAudioPath)
		if sttErr != nil {
			// sttErr's Error() text is ALREADY the exact Turkish user-facing
			// string the worker itself decided (stt.py's MSG_MODEL_MISSING /
			// kahya_worker.__main__'s MSG_EMPTY_TRANSCRIPT, or a generic
			// fallback) - see transcribeAudioLocally's own doc comment. No
			// task row was ever created for this failed STT phase; nothing
			// further to persist.
			log.Error("stt_transcription_failed", "err", sttErr.Error())
			writeSSE("error", map[string]string{"message": sttErr.Error()})
			return
		}
		// Falls through to the ordinary text-prompt flow below with
		// req.Prompt now the transcript, verbatim, reusing this SAME
		// already-open SSE stream (writeSSE/flusher above) - never a
		// second one.
		req.Prompt = transcript
	}

	if strings.TrimSpace(req.Prompt) == "" {
		writeJSONError(w, http.StatusBadRequest, "prompt must not be empty")
		return
	}

	// dbCtx (the request's own context) is used for the writes that happen
	// BEFORE the worker is spawned, while the client is still known to be
	// connected - including, since W3-08, the ingest-time classifier itself
	// (see below): classification MUST complete, and its resulting lane
	// MUST be durably persisted onto the tasks row, strictly BEFORE any
	// worker is spawned or any Anthropic-proxy listener is opened (HANDOFF
	// §4 ⚑ ordering invariant). dbCtx must NOT be used for anything after
	// spawn.Run returns: r.Context() is cancelled the moment the underlying
	// connection closes for ANY reason - not only a clean client exit, but
	// also (verified live during this task's manual verification) the
	// CLI's OWN 30s idle-read timeout giving up and closing its side while
	// the server's task_timeout_min is still counting down. Since taskCtx
	// below is derived FROM dbCtx, that same disconnect also cancels
	// taskCtx early - which is desirable for spawn.Run (no orphan worker
	// survives a disappeared client) - but it must never take down the
	// bookkeeping that RECORDS the outcome: persistCtx (a plain background
	// context) is used for every write from OnSession onward specifically
	// so a disconnected/timed-out client can never prevent kahyad from
	// recording that the task ended (state + ledger).
	dbCtx := r.Context()
	persistCtx := context.Background()

	taskID := spawn.NewTaskID()
	now := rfc3339Now()

	// W6-01: record palette_open as early as possible - before
	// classification, envelope construction, or any worker/proxy exists
	// for this task - so its own timestamp reflects the ACTUAL hotkey-press
	// moment (req.PaletteOpenedAt) rather than anything this handler itself
	// does. Applies uniformly to every lane (cloud AND secret) since both
	// paths share this one call site.
	if req.PaletteOpenedAt != nil {
		s.logPaletteOpen(dbCtx, traceID, taskID, *req.PaletteOpenedAt)
	}

	// W3-08: classify BEFORE the envelope/task row are ever constructed -
	// the ordering invariant's strongest possible form: there is no
	// envelope, no task row, and no worker/proxy for this task to exist yet
	// at all until classification has completed.
	//
	// Deliberately uses secretlane.ClassifyDeterministic (regex/lexicon
	// pre-pass ONLY - IBAN/TCKN/card-number/CVV/keyword lexicon), NOT the
	// full Qwen-backed s.secretLaneClassifier: the task spec's own
	// ingestion-point list names memory_write content, fs reads flagged for
	// model consumption, and (W4-03) mail/web Reader input - it does NOT
	// name the raw chat prompt POST /v1/task carries. Requiring a live,
	// warm Qwen server just to have an ORDINARY cloud-routed conversation
	// would take a hard dependency this ingestion point was never meant to
	// have (and would defeat kahyad/internal/mlx's own "spawn lazily, only
	// on an ACTUAL secret-lane need" contract). The deterministic pre-pass
	// alone already needs no model at all and is exactly as strong a
	// guarantee on a MATCH (IBAN/TCKN/card/keyword hits are unconditionally
	// final, per HANDOFF's ordering invariant) - only a NON-match's fallback
	// behavior differs (see ClassifyDeterministic's own doc comment).
	// s.secretLaneClassifier (deterministic + Qwen fallback, fully wired to
	// the real local server in main.go) remains available for those THREE
	// named ingestion points once W4-03 lands them; a later Escalate call
	// (kahyad/internal/secretlane.Escalate) would STICKILY widen this exact
	// task's lane to secret if any of them find something the chat prompt
	// itself did not.
	//
	// SCOPE DECISION (post-review, explicit and deliberate - not a gap):
	// HANDOFF §4's ordering invariant reads "policy.yaml globları yalnız
	// dosya yolları için; mail/web gibi içerik-kaynaklı veride gizli-şerit
	// kararı yerel içerik-sınıflandırıcıyla alım anında verilir" - i.e. the
	// FULL local-classifier decision is specified for content-SOURCED /
	// ingested data (mail/web/files), decided AT INGEST TIME via the W4-03
	// Reader path. A user's own directly-typed chat prompt is a different
	// thing: it is user-authored direct input, not ingested content, so it
	// is handled by the deterministic pre-pass ONLY, here. This is what
	// keeps kahyad from loading the 30B-parameter Qwen model on every single
	// ordinary chat message (defeating on-demand load/idle-unload) and
	// keeps the hermetic W12-10 e2e gate model-free. The consequence, made
	// explicit rather than left implicit: a prompt the user types themselves
	// that describes a secret in natural language ONLY - no IBAN/TCKN/card
	// number/CVV, no sağlık/finans/kimlik lexicon term - is genuinely out of
	// the deterministic pre-pass's reach and will be routed by whatever the
	// normal (cloud) lane would do. That is the user's own choice in typing
	// it directly to the assistant, not a silent classifier miss; it is NOT
	// in scope for this ingestion point to catch, and this is intentionally
	// NOT being "fixed" by always-loading Qwen here (see
	// tasks/w3-policy-tools/W3-08-secret-lane-local.md's "Post-review scope
	// note").
	// W4-08: the "derin düşün" opt-in - a byte-exact Turkish prompt prefix,
	// detected and stripped HERE (deterministically, in Go - never
	// model-detected), before classification runs on the (now-bare)
	// prompt. --derin (taskRequest.DeepThink) and the prefix are two
	// independent ways to set the SAME flag; either (or both) sets it.
	deepThink := req.DeepThink
	if stripped, matched := detectAndStripDerinPrefix(req.Prompt); matched {
		deepThink = true
		req.Prompt = stripped
	}
	// A prompt consisting of ONLY the prefix (e.g. "derin düşün:" or
	// "derin düşün:   ") strips down to empty - re-check here (the initial
	// empty-prompt check above ran on the RAW, pre-strip prompt) so this
	// still surfaces the same clean 400 "prompt must not be empty" instead
	// of falling through to envelope.Validate()'s generic 500 further down.
	if strings.TrimSpace(req.Prompt) == "" {
		writeJSONError(w, http.StatusBadRequest, "prompt must not be empty")
		return
	}

	lane := spawn.LaneNormal
	category := ""
	verdict := secretlane.ClassifyDeterministic(req.Prompt)
	if verdict.SecretLane {
		lane = spawn.LaneSecret
		category = verdict.Category
		if s.markSensitiveRead != nil {
			_ = s.markSensitiveRead(dbCtx, traceID, traceID)
		}
	}
	log.Info("secretlane_classified", "lane", lane, "category", category, "reason", verdict.Reason)

	// W4-08: intent classification runs STRICTLY AFTER the secret-lane
	// classification above (never before) and, for this ordinary chat
	// prompt, is itself deterministic (IntentChat) - no CLI-declared task
	// kind exists yet on this endpoint, so this NEVER takes a live Qwen
	// dependency merely to answer a plain chat message (the same posture
	// secretlane.ClassifyDeterministic's own call above already has).
	// intentResult.Duration/Source still faithfully reflect that (Source
	// is always "deterministic" here) - the intent_classified event this
	// logs is what the ledger-ordering acceptance criterion
	// (intent_classified -> routing_decision -> model_call) requires for
	// EVERY task, not only ones that ever call the model to get one.
	intentResult, _ := router.ClassifyIntent(dbCtx, nil, router.ClassifyIntentInput{DeterministicIntent: router.IntentChat})
	router.LogIntentClassified(dbCtx, s.eventLogger, traceID, intentResult)

	downgraded := s.anthGovernor != nil && s.anthGovernor.Downgraded()
	routeInput := router.RouteInput{
		Intent: intentResult.Intent, Lane: lane, DeepThink: deepThink,
		Downgraded: downgraded, DefaultModel: s.cfg.DefaultModel,
	}
	decision := router.SelectModel(routeInput)
	router.LogRoutingDecision(dbCtx, s.eventLogger, traceID, routeInput, decision)

	// envelopeModel is ALWAYS a valid §9 cloud model, even when decision.
	// Local is true (schema validity only - mirrors the pre-existing
	// secret-lane precedent immediately below: envelope.Model is set
	// unconditionally, but a Local decision NEVER spawns a worker/opens an
	// Anthropic-proxy listener, so this value is never itself the
	// enforcement that local content stays local).
	envelopeModel := decision.Model
	if decision.Local {
		envelopeModel = s.cfg.DefaultModel
	}

	envelope := spawn.Envelope{
		SchemaVersion:   spawn.SchemaVersion,
		TaskID:          taskID,
		TraceID:         traceID,
		SessionID:       nil,
		Kind:            "chat",
		Prompt:          req.Prompt,
		Model:           envelopeModel,
		MemoryInjection: true,
		CreatedAt:       now,
		Lane:            lane,
		Category:        category,
		Intent:          intentResult.Intent,
		DeepThink:       deepThink,
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

	if _, err := s.taskStore.InsertTask(dbCtx, sqlcgen.InsertTaskParams{
		ID:             taskID,
		TraceID:        traceID,
		SessionID:      sql.NullString{},
		State:          "running",
		TaintTier:      "untrusted",
		Model:          sql.NullString{String: envelope.Model, Valid: true},
		Envelope:       sql.NullString{String: string(envelopeJSON), Valid: true},
		UpdatedAt:      now,
		CreatedAt:      now,
		Lane:           lane,
		SecretCategory: sql.NullString{String: category, Valid: category != ""},
	}); err != nil {
		log.Error("task_insert_failed", "err", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "task insert failed")
		return
	}

	if s.eventLogger != nil {
		if err := s.eventLogger.LogEvent(dbCtx, traceID, "task_spawned", map[string]any{
			"task_id": taskID, "model": envelope.Model, "lane": lane,
		}); err != nil {
			log.Warn("task_spawned_ledger_error", "err", err.Error())
		}
	}
	log.Info("task_spawned", "task_id", taskID, "model", envelope.Model, "lane", lane)

	// W3-08: a secret-lane task is answered ENTIRELY on-device
	// (kahyad/internal/secretlane.Answerer, backed by the local
	// Qwen3-30B-A3B server) - kahyad never spawns the claude-agent-sdk
	// worker or opens an Anthropic forward-proxy listener AT ALL for it.
	// This is the ordering invariant's strongest form: there is no code
	// path through which this task's content could even reach the worker
	// process, let alone the cloud - not merely a backstop that blocks it
	// after the fact (kahyad/internal/secretlane/answer.go's own doc
	// comment explains why this bypasses the worker rather than rerouting
	// the SDK itself to a local OpenAI endpoint). The W12-08 proxy backstop
	// (kahyad/internal/secretlane.NewProxyBackstopHook, wired in main.go)
	// is the SECOND, independent layer of defense in case that ever
	// changes.
	if lane == spawn.LaneSecret {
		// W4-08: decision (computed above, unconditionally, for every task
		// including this one) is ALREADY guaranteed Local==true here -
		// router.SelectModel's own matrix test
		// (TestSelectModelSecretLanePinsLocalForEveryIntent) proves
		// Lane==secret pins local for every Intent/DeepThink/Downgraded
		// combination. This is a defensive, log-only consistency check
		// ("assert SelectModel would agree" per the task spec) - it changes
		// no behavior; the actual enforcement is this branch itself, which
		// never spawns a worker regardless of what decision says.
		if !decision.Local {
			log.Error("routing_invariant_violated", "task_id", taskID, "reason", "secret_lane_not_local", "decision_model", decision.Model)
		}
		s.handleSecretLaneTask(w, log, taskID, traceID, req.Prompt, dbCtx, persistCtx, writeSSE)
		return
	}

	if decision.Local {
		// W4-08: the cost-governor's Sonnet->yerel downgrade rung (HANDOFF
		// §4 ⚑ cost governor: Opus->Sonnet->yerel at 80% of daily spend).
		// This task's OWN content classification stayed "normal" (lane, set
		// above, is never mislabeled secret just because the ROUTE happens
		// to be local this one time) - only THIS call's routing decision is
		// local, driven by governor.Downgraded(), never envelope.Model
		// (kept a valid §9 cloud model above for schema validity only, same
		// as the secret-lane precedent). Reuses the EXACT SAME local-answer
		// mechanism (kahyad/internal/secretlane.Answerer via
		// handleSecretLaneTask) - see kahyad/internal/router's own package
		// doc comment for why the local lane must never be represented as
		// an envelope.Model string instead.
		s.handleSecretLaneTask(w, log, taskID, traceID, req.Prompt, dbCtx, persistCtx, writeSSE)
		return
	}

	// W4-04: this cloud-lane task now genuinely enters the W4-02 status
	// state machine (intent -> executing) - previously this handler only
	// ever touched the older, free-form tasks.state field, leaving
	// tasks.status stuck at 'intent' forever in production (the resume
	// scan/outbox dispatcher's own candidate queries key off 'executing',
	// so bekliyor-yeniden-deneme could never actually be reached without
	// this). Best-effort: a transition failure here does not abort the
	// request (tasks.state-based SSE reporting below is unaffected either
	// way) - it is logged and the guarded post-spawn transition further
	// down simply has nothing to move if this never took hold.
	if s.taskMachine != nil {
		if err := s.taskMachine.Transition(dbCtx, traceID, taskID, task.StatusExecuting); err != nil {
			log.Warn("task_transition_executing_failed", "task_id", taskID, "err", err.Error())
		}
	}

	// W12-08/W4-04: open this task's own ephemeral forward-proxy listener
	// BEFORE the SSE response starts, so a failure here is still a plain
	// JSON 500 like every other pre-stream validation failure - never a
	// mid-stream SSE error event. NewTaskProxy is the SAME construction
	// kahyad/internal/outbox.Dispatcher's AnthproxyOpener uses at
	// REDISPATCH time (wired in main.go) - one shared helper, so the two
	// call sites can never drift apart.
	anthropicBaseURL, apiKey, closeProxy, err := s.NewTaskProxy(taskID, traceID)
	if err != nil {
		log.Error("anthproxy_new_failed", "task_id", taskID, "err", err.Error())
		if writeSSE != nil {
			// The input_audio_path phase already opened this response as
			// SSE (200 + text/event-stream) - a plain JSON error body at
			// this point would be malformed against that already-sent
			// Content-Type, so report the SAME failure as a terminal SSE
			// "error" event instead of writeJSONError's plain-JSON path.
			writeSSE("error", map[string]string{"message": fmt.Sprintf(MsgTaskUnexpectedExit, traceID)})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "anthropic proxy init failed")
		return
	}
	defer func() { _ = closeProxy() }()

	// writeSSE/flusher are only still nil here when the input_audio_path
	// phase above never ran (an ordinary typed-prompt request) - that
	// phase already opened this exact SSE stream on ITS OWN "flusher, ok"
	// check succeeding, so re-checking/re-opening here would be a
	// superfluous second WriteHeader on the very same response.
	if writeSSE == nil {
		fl, ok := w.(http.Flusher)
		if !ok {
			log.Error("task_streaming_unsupported")
			writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		flusher = fl
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		writeSSE = func(event string, payload any) {
			b, err := json.Marshal(payload)
			if err != nil {
				log.Error("task_sse_marshal_failed", "event", event, "err", err.Error())
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			flusher.Flush()
		}
	}

	// W4-08: an explicit "derin düşün" opt-in is NOT itself a router
	// choice subject to downgrade (router.RouteDecision.DerinDuringDowngrade
	// is only ever true here, on the cloud/claude-fable-5 path - never on
	// the lane==secret or decision.Local branches above, which already
	// returned) - it is honored, but the user is warned, in the output
	// itself, that they are spending through an active 80% downgrade.
	if decision.DerinDuringDowngrade {
		if s.eventLogger != nil {
			if err := s.eventLogger.LogEvent(dbCtx, traceID, "derin_during_downgrade", map[string]any{"task_id": taskID}); err != nil {
				log.Warn("derin_during_downgrade_ledger_error", "err", err.Error())
			}
		}
		writeSSE("delta", map[string]string{"text": MsgDerinDuringDowngrade + "\n"})
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

	// W4-02: register this task's worker pid with the live registry (if
	// wired) for the whole time spawn.Run is actually running it - the
	// resume scan's LiveChecker consults this so it never treats a task
	// the daemon itself is still actively running as crashed, and GET
	// /v1/task/status (`kahya task show <id>`) reports the live pid.
	if s.taskLiveRegistry != nil {
		defer s.taskLiveRegistry.Unregister(taskID)
	}

	// W6-01: first_token fires at most ONCE per task, on the FIRST OnDelta
	// callback below (the W12-08 forward-proxy path - see logFirstToken's
	// own doc comment) - a sync.Once rather than a plain bool guard since
	// nothing in this package promises OnDelta is invoked from only one
	// goroutine (spawn.Run's own contract does not make that guarantee
	// explicit), and firing this twice would be a spurious extra ledger
	// row, not a security issue, but is still avoided.
	var firstTokenOnce sync.Once

	outcome, runErr := spawn.Run(taskCtx, spawnCfg, envelope, spawn.Callbacks{
		OnStart: func(pid int) {
			log.Info("task_worker_started", "task_id", taskID, "pid", pid)
			if s.taskLiveRegistry != nil {
				s.taskLiveRegistry.Register(taskID, pid)
			}
			// W6-03: persist the worker's process-group id (spawn.Run always
			// starts it as its own new group leader via Setpgid, so pid ==
			// pgid) ALONGSIDE the in-memory registry above, so the halt
			// executor can still kill this worker's group even after a
			// daemon crash/restart emptied that in-memory registry (macOS
			// has no PDEATHSIG - migrations/0015_halt_semantics.sql's own
			// doc comment). Best-effort: a write failure here is logged,
			// never fatal to the task itself.
			if s.taskStore != nil {
				if err := s.taskStore.SetTaskWorkerPGID(persistCtx, sqlcgen.SetTaskWorkerPGIDParams{
					WorkerPgid: sql.NullInt64{Int64: int64(pid), Valid: true}, UpdatedAt: rfc3339Now(), ID: taskID,
				}); err != nil {
					log.Warn("task_worker_pgid_persist_failed", "task_id", taskID, "err", err.Error())
				}
			}
		},
		OnDelta: func(text string) {
			firstTokenOnce.Do(func() { s.logFirstToken(persistCtx, traceID, taskID) })
			writeSSE("delta", map[string]string{"text": text})
		},
		OnSession: func(sessionID string) {
			if sessionID == "" {
				return
			}
			if err := s.persistSessionStarted(persistCtx, traceID, taskID, sessionID); err != nil {
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

	// W4-04: guarded post-spawn status transition. A cloud-retry callback
	// (NewTaskProxy's OnCloudUnreachable/OnNonRetryableFailure) may ALREADY
	// have moved this task's status to bekliyor-yeniden-deneme or failed
	// SYNCHRONOUSLY, mid-spawn.Run, before this point ever runs - re-
	// fetching the CURRENT status and only transitioning when it is still
	// 'executing' avoids attempting an illegal/duplicate transition on top
	// of whatever the callback already decided (Machine.Transition's own
	// from==to no-op would make a same-status call harmless anyway, but a
	// task already parked in bekliyor-yeniden-deneme must NOT also be
	// forced to 'done'/'failed' here - it is correctly still in flight,
	// waiting for the outbox dispatcher's own later redelivery).
	//
	// W4-07 fix (defect the acceptance gate surfaced): spawn.Run returning
	// with anything OTHER than a clean StatusOK success no longer forces
	// this task straight to 'failed' here. Two independent problems with
	// the old unconditional "not OK -> failed" branch, both found building
	// this gate's scenario A (kill -9 the worker mid a W2 tool call):
	//
	//  1. spawn.Run returning only ever means the WORKER PROCESS exited -
	//     it says nothing about whether a side-effectful tool call that
	//     worker started is still genuinely executing INSIDE KAHYAD's own
	//     process. Receipts.Execute's effect runs on the HTTP goroutine
	//     that served the worker's /v1/mcp call, entirely independent of
	//     the worker process's own lifetime (kill -9 on just the worker
	//     pid - exactly how `kahya task show <id>`'s PID is meant to be
	//     killed - never touches that goroutine). Forcing 'failed' here
	//     strands the task in a TERMINAL state before kahyad/internal/
	//     task.Resume's own periodic scan ever gets a chance to evaluate
	//     it - defeating the entire W4-02 double-execution-safety
	//     guarantee (a resumed replay needs the task to still be
	//     'executing' for the resume scan to ever look at it at all).
	//  2. A DB-query-based guard here (an earlier version of this fix
	//     checked "is any tool_calls row still receipt-less" before
	//     deciding) does not actually work: kahyad/internal/store opens
	//     brain.db with a single connection (SetMaxOpenConns(1) - "long
	//     transactions block everything"), and Receipts.Execute holds
	//     that ONE connection for the effect's entire duration. A guard
	//     query issued the instant spawn.Run returns BLOCKS on that same
	//     connection until the in-flight effect's transaction commits -
	//     by which time the row is no longer receipt-less (confirmed
	//     empirically: such a guard query never once observed "in
	//     flight", it always returned right after the receipt had
	//     already landed).
	//
	// The fix that actually holds under that constraint: never decide
	// "failed" HERE at all - only 'done' (spawn.Run's own StatusOK,
	// requiring absolutely no further DB read to trust) is decided
	// eagerly; every non-OK outcome is left at 'executing', deferred
	// ENTIRELY to kahyad/internal/task.Resume's own periodic scan
	// (main.go's task_resume_scan tick, which runs on its own schedule and
	// is never blocked by THIS request's goroutine) - exactly the
	// authority table task spec step 6 already defines: no receipt-less
	// tool_calls row -> resume; W1 receipt-less within cap -> auto-retry;
	// W1 past cap or any W2/W3 receipt-less -> blocked_user. This also
	// means an ordinary transient worker crash (no tool call in flight at
	// all) now auto-resumes via that same scan rather than being
	// permanently marked 'failed' - the resume scan's own decision tree
	// already covers that case correctly (a genuinely broken worker still
	// eventually surfaces to the user via blocked_user/W1's retry cap, it
	// simply takes one more tick to get there).
	if s.taskMachine != nil && runErr == nil && outcome.Status == spawn.StatusOK {
		current, gerr := s.taskDurabilityStatus(persistCtx, taskID)
		if gerr == nil && current == task.StatusExecuting {
			if terr := s.taskMachine.Transition(persistCtx, traceID, taskID, task.StatusDone); terr != nil {
				log.Warn("task_transition_terminal_failed", "task_id", taskID, "to", task.StatusDone, "err", terr.Error())
			}
		}
	}

	writeSSE(m.sseEvent, m.ssePayload)
}

// taskDurabilityStatus reads taskID's CURRENT tasks.status - a tiny
// helper so the guarded post-spawn transition above never needs its own
// copy of a GetTaskByID-shaped interface; taskDurabilityStore already has
// exactly this read (task_durability.go's own TaskDurabilityStore).
func (s *Server) taskDurabilityStatus(ctx context.Context, taskID string) (string, error) {
	if s.taskDurabilityStore == nil {
		return "", fmt.Errorf("task durability store not available")
	}
	t, err := s.taskDurabilityStore.GetTaskByID(ctx, taskID)
	if err != nil {
		return "", err
	}
	return t.Status, nil
}

// logPaletteOpen implements W6-01: ledger a kind="palette_open" event
// under traceID/taskID, carrying paletteOpenedAt (the RAW unix-seconds
// float the client sent, e.g. hammerspoon/kahya.lua's
// hs.timer.secondsSinceEpoch() captured at hotkey press) in its payload -
// the north-star "palet-aç→ilk-token" metric's start timestamp (HANDOFF §6
// metric definitions ⚑). This event's own events.ts column is left at
// LogEvent's ordinary "now" (the moment this line of Go code runs, i.e.
// very early in handleTask) rather than being backdated to
// paletteOpenedAt itself - the payload is where the ACTUAL captured
// timestamp lives (for W78-04's later metric query to read); the ts
// column stays on this codebase's one universal "ledger rows are stamped
// with wall-clock write time" convention, which is also what keeps
// first_token.ts >= palette_open.ts trivially true by construction (both
// are logged in strict program order within this same request). Best-
// effort: a ledger failure here must never abort task dispatch.
func (s *Server) logPaletteOpen(ctx context.Context, traceID, taskID string, paletteOpenedAt float64) {
	if s.eventLogger == nil {
		return
	}
	if err := s.eventLogger.LogEvent(ctx, traceID, "palette_open", map[string]any{
		"task_id": taskID, "palette_opened_at": paletteOpenedAt,
	}); err != nil {
		s.log.With(traceID).Warn("palette_open_ledger_error", "err", err.Error())
	}
}

// logFirstToken implements W6-01: ledger a kind="first_token" event under
// traceID/taskID at the FIRST point kahyad relays this task's streamed
// model output - handleTask's own OnDelta callback (the W12-08 forward
// proxy path: every cloud AND stub-model task's deltas flow through
// spawn.Run's stdout-relay, which itself only ever carries bytes the
// worker received via that per-task proxy listener) and
// finishSecretLaneTask's own answer relay (the W3-08 secret-lane LOCAL
// path) both call this exactly once per task - see each call site's own
// doc comment for why calling this more than once per task would still be
// harmless (an append-only ledger tolerates it) but is avoided anyway
// (sync.Once at the OnDelta call site; finishSecretLaneTask's own success
// branch runs at most once by construction). Best-effort: a ledger
// failure here must never abort delta relay.
func (s *Server) logFirstToken(ctx context.Context, traceID, taskID string) {
	if s.eventLogger == nil {
		return
	}
	if err := s.eventLogger.LogEvent(ctx, traceID, "first_token", map[string]any{
		"task_id": taskID,
	}); err != nil {
		s.log.With(traceID).Warn("first_token_ledger_error", "err", err.Error())
	}
}

// handleSecretLaneTask answers a lane=="secret" task entirely via
// s.secretLaneAnswerer (kahyad/internal/secretlane.Answerer, the local
// Qwen3-30B-A3B server) - see handleTask's own call-site comment for why
// this bypasses the claude-agent-sdk worker/Anthropic-proxy path entirely
// rather than merely relying on the proxy backstop. Streams the EXACT
// SAME SSE contract kahyad/cmd/kahya/client.go already understands: zero
// or more "delta" events (here, always exactly one - the full answer,
// non-streamed, since mlx_lm.server's non-streaming response already
// arrives as one complete string) then a terminal "result" or "error"
// event.
// preOpened, when non-nil (W6-02: the input_audio_path phase already
// opened this exact response as SSE before classification ever ran - see
// handleTask's own doc comment), is reused AS-IS instead of opening a
// second SSE stream on the same response (which would be a superfluous,
// harmless-but-noisy second WriteHeader call). nil (every pre-W6-02
// caller) is this function's own original behavior: open its own SSE
// stream exactly as before.
func (s *Server) handleSecretLaneTask(w http.ResponseWriter, log *logx.Logger, taskID, traceID, prompt string, dbCtx, persistCtx context.Context, preOpened func(event string, payload any)) {
	writeSSE := preOpened
	if writeSSE == nil {
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

		writeSSE = func(event string, payload any) {
			b, err := json.Marshal(payload)
			if err != nil {
				log.Error("task_sse_marshal_failed", "event", event, "err", err.Error())
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			flusher.Flush()
		}
	}

	timeoutMin := s.cfg.TaskTimeoutMin
	taskCtx, cancel := context.WithTimeout(dbCtx, time.Duration(timeoutMin)*time.Minute)
	defer cancel()

	if s.secretLaneAnswerer == nil {
		s.finishSecretLaneTask(persistCtx, log, taskID, traceID, writeSSE, "", fmt.Errorf("secretlane: no local answerer wired"))
		return
	}

	answer, err := s.secretLaneAnswerer.Answer(taskCtx, prompt)
	s.finishSecretLaneTask(persistCtx, log, taskID, traceID, writeSSE, answer, err)
}

// finishSecretLaneTask persists the terminal task state/ledger row and
// writes the terminal SSE event for a secret-lane task - the "yerel model
// için bellek yok" fail-closed message (mlx.ErrLocalModelUnavailable,
// task spec's crown invariant) is distinguished from every other local
// answer failure so the user sees the EXACT documented Turkish string
// rather than a generic one.
func (s *Server) finishSecretLaneTask(persistCtx context.Context, log *logx.Logger, taskID, traceID string, writeSSE func(string, any), answer string, err error) {
	if err != nil {
		msg := fmt.Sprintf(MsgSecretLaneModelCallFailed, traceID)
		if errors.Is(err, mlx.ErrLocalModelUnavailable) {
			msg = fmt.Sprintf("%s (%s)", mlx.MsgNoLocalMemory, mlx.MsgNoLocalMemoryGuidance)
		}
		log.Error("secretlane_answer_failed", "task_id", taskID, "err", err.Error())
		if uerr := s.taskStore.UpdateTaskState(persistCtx, sqlcgen.UpdateTaskStateParams{
			State: "error", UpdatedAt: rfc3339Now(), ID: taskID,
		}); uerr != nil {
			log.Error("task_state_update_failed", "task_id", taskID, "err", uerr.Error())
		}
		if s.eventLogger != nil {
			if lerr := s.eventLogger.LogEvent(persistCtx, traceID, "task_error", map[string]any{
				"task_id": taskID, "status": "error",
			}); lerr != nil {
				log.Warn("task_ledger_error", "kind", "task_error", "err", lerr.Error())
			}
		}
		writeSSE("error", map[string]string{"message": msg})
		return
	}

	if uerr := s.taskStore.UpdateTaskState(persistCtx, sqlcgen.UpdateTaskStateParams{
		State: "done", UpdatedAt: rfc3339Now(), ID: taskID,
	}); uerr != nil {
		log.Error("task_state_update_failed", "task_id", taskID, "err", uerr.Error())
	}
	if s.eventLogger != nil {
		if lerr := s.eventLogger.LogEvent(persistCtx, traceID, "task_done", map[string]any{
			"task_id": taskID, "status": "ok",
		}); lerr != nil {
			log.Warn("task_ledger_error", "kind", "task_done", "err", lerr.Error())
		}
	}
	log.Info("task_done", "task_id", taskID, "processed_locally", true)

	// W6-01: first_token's OTHER relay point (see logFirstToken's own doc
	// comment) - the W3-08 secret-lane LOCAL path. mlx_lm.server's
	// non-streaming response arrives as one complete string, so this is
	// the first (and only) point kahyad relays this task's model output at
	// all; called unconditionally, exactly once, on this success branch
	// (finishSecretLaneTask itself runs at most once per task - no
	// sync.Once needed here, unlike the cloud/stub OnDelta call site,
	// which can fire multiple times per task).
	s.logFirstToken(persistCtx, traceID, taskID)

	writeSSE("delta", map[string]string{"text": answer})
	// processed_locally: true is the task spec's own CLI-badge field
	// ("🔒 yerel işlendi") - this is the ONLY code path that ever sets it
	// true (mapTaskOutcome's cloud-lane success payload always sets it
	// false).
	writeSSE("result", map[string]any{
		"status": "ok", "task_id": taskID, "session_id": "", "processed_locally": true,
	})
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
	case outcome.Status == spawn.StatusCloudUnreachable:
		// W4-04: kahyad/internal/task.CloudRetry has ALREADY parked the
		// task in bekliyor-yeniden-deneme (tasks.status), synchronously,
		// via NewTaskProxy's own OnCloudUnreachable callback, well before
		// this mapping ever runs - this branch only decides the free-form
		// tasks.state/SSE surface, using the SAME exact parked Turkish
		// string the notification channel already sent.
		return taskOutcomeMapping{
			finalState: "waiting_retry", ledgerKind: "task_waiting_retry",
			sseEvent:   "error",
			ssePayload: map[string]string{"message": task.MsgCloudParked},
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
				// processed_locally is always false here (W3-08 task spec:
				// "task result payload field processed_locally: true") -
				// this branch is ONLY ever reached for a cloud-routed
				// (lane=="normal") task; a lane=="secret" task's terminal
				// result comes from finishSecretLaneTask instead, which
				// sets this true.
				"status": "ok", "task_id": taskID, "session_id": outcome.SessionID, "processed_locally": false,
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

	// W4-03: req.SessionID (already part of this endpoint's frozen wire
	// schema - worker/kahya_worker/hooks.py's make_can_use_tool has sent it
	// on every /policy/check POST since W12-09) threads straight through
	// onto CheckInput.SessionID. A nil pointer (no session yet - e.g. the
	// very first tool call before the worker's init message carried a
	// session_id) becomes "".
	//
	// BLOCKER 1+2 fix (post-security-review): this worker-supplied value
	// is NOT what Engine.Check's taint-check hook actually decides on in
	// production - it is untrusted (a worker, or a compromised one, can
	// send any session_id it likes, including a forged clean one, or none
	// at all to make an old fail-open guard skip the check entirely).
	// Check instead resolves the session SERVER-SIDE from TaskID/TraceID
	// below via its own SessionResolver (main.go wires this alongside
	// SetTaintChecker) - SessionID here is retained only as CheckInput's
	// documented legacy fallback for callers/tests with no resolver wired
	// at all. See policy.CheckInput/policy.SessionResolver's own doc
	// comments for the full mechanism.
	sessionID := ""
	if req.SessionID != nil {
		sessionID = *req.SessionID
	}
	decision, err := s.policyEngine.Check(r.Context(), policy.CheckInput{
		Tool: req.ToolName, Scope: req.Scope, TaskID: req.TaskID, TraceID: traceID,
		SessionID: sessionID, ToolInput: req.ToolInput,
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
