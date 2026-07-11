// receipts.go implements the W4-02 tool-call intent/executing/receipt
// lifecycle (task spec step 3) and the idempotent-replay lookup (step 4)
// that makes a resumed task double-execution-safe: before a genuine
// execution attempt, Receipts.Execute checks whether a DURABLE receipt
// already exists for this exact (task_id, tool_name, args_hash) triple -
// if so, it returns the stored result WITHOUT running the effect again
// and ledgers tool.replayed.
//
// R-class tool calls never go through this file at all (task spec step 3:
// "R-class calls get no rows - no side effects to protect") - a caller
// wiring an R-class tool simply runs it directly, with no Receipts
// involvement.
package task

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store/sqlcgen"
)

// tool_calls.status values (migrations/0007_task_durability.sql).
const (
	CallStatusIntent    = "intent"
	CallStatusExecuting = "executing"
	CallStatusReceipt   = "receipt"
	CallStatusFailed    = "failed"
)

// EventReplayed is the ledger event kind an idempotent-replay hit
// appends (task spec step 4, verbatim: "ledger event tool.replayed").
const EventReplayed = "tool.replayed"

// ErrReadOnlyClass is returned by Receipts.Execute when called with class
// R: R-class calls have no side effect to protect and must never get a
// tool_calls row (task spec step 3) - call the tool's effect directly
// instead of routing it through this method.
var ErrReadOnlyClass = errors.New("task: Execute must not be called for class R (no tool_calls row - call the effect directly)")

// ToolCallStore is the narrow tool_calls-table persistence surface
// Receipts needs. *sqlcgen.Queries (via *store.Store, or a *sql.Tx-scoped
// Queries via Queries.WithTx) satisfies this directly, with no adapter.
type ToolCallStore interface {
	NextToolCallSeq(ctx context.Context, arg sqlcgen.NextToolCallSeqParams) (int64, error)
	InsertToolCallIntent(ctx context.Context, arg sqlcgen.InsertToolCallIntentParams) (sqlcgen.ToolCall, error)
	MarkToolCallExecuting(ctx context.Context, arg sqlcgen.MarkToolCallExecutingParams) error
	MarkToolCallReceipt(ctx context.Context, arg sqlcgen.MarkToolCallReceiptParams) error
	MarkToolCallFailed(ctx context.Context, arg sqlcgen.MarkToolCallFailedParams) error
	GetReceiptToolCall(ctx context.Context, arg sqlcgen.GetReceiptToolCallParams) (sqlcgen.ToolCall, error)
	ListReceiptlessToolCalls(ctx context.Context, taskID string) ([]sqlcgen.ToolCall, error)
	CountToolCallAttempts(ctx context.Context, arg sqlcgen.CountToolCallAttemptsParams) (int64, error)
	ListToolCallsByTask(ctx context.Context, taskID string) ([]sqlcgen.ToolCall, error)
}

var _ ToolCallStore = (*sqlcgen.Queries)(nil)

// receiptEnvelope is tool_calls.receipt_json's exact on-disk shape (task
// spec step 3: "receipt_json (result + result hash)"). ResultHash lets a
// caller verify a replayed result was never silently corrupted at rest,
// independent of re-deriving it from Result itself.
type receiptEnvelope struct {
	Result     json.RawMessage `json:"result"`
	ResultHash string          `json:"result_hash"`
}

// HashArgs returns sha256(hex) of b - the convention this package expects
// ExecuteInput.ArgsHash to already be (a stable hash of the tool call's
// exact input bytes). WYSIWYE canonicalization, where it matters, is the
// caller's own concern - a tool wired through the normal ladder/token flow
// (kahyad/internal/policy.Engine) already canonicalizes before hashing;
// this helper is the plain, no-canonicalization fallback for a caller that
// has nothing else to reach for (e.g. a test's stub tool).
func HashArgs(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hashApprovalToken returns the sha256(hex) of a raw one-time approval
// token, or the zero sql.NullString for an empty token - tool_calls never
// stores a raw token (mirrors kahyad/internal/policy/tokens.go's own
// "only the hash is ever persisted" posture for approval_tokens).
func hashApprovalToken(token string) sql.NullString {
	if token == "" {
		return sql.NullString{}
	}
	sum := sha256.Sum256([]byte(token))
	return sql.NullString{String: hex.EncodeToString(sum[:]), Valid: true}
}

// EffectFunc performs a side-effectful tool's actual execution and
// returns its result (embedded, as raw JSON, in the durable receipt) or
// an error. tx is a *sql.Tx already open against the SAME database
// Receipts itself uses: an effect that ALSO writes to brain.db (e.g.
// mutating autonomy_state, inserting a memory fact) should perform those
// writes through tx so they commit in the EXACT SAME transaction as the
// receipt row (task spec step 3: "in the same transaction that commits
// the tool's DB effects"). An effect whose side effect lives OUTSIDE
// brain.db (a filesystem write, an AppleScript run, a Docker exec, ...)
// simply ignores tx - Execute still commits the receipt IMMEDIATELY once
// effect returns, which is the closest a single-database transaction can
// get to atomicity when the side effect itself is not a database write at
// all (no two-phase commit across brain.db and the filesystem is
// attempted or claimed here).
type EffectFunc func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error)

// ExecuteInput is Receipts.Execute's input: everything the tool_calls
// intent row needs, resolved by the caller's own policy-check step
// (kahyad/internal/policy.Engine.Check/ConsumeToken) BEFORE Execute is
// ever called - Execute performs no policy decision of its own.
type ExecuteInput struct {
	TaskID   string
	TraceID  string
	ToolName string
	// Class must be W1, W2, or W3 - never R (ErrReadOnlyClass).
	Class ActionClass
	// ArgsHash is a stable hash of the tool call's exact input bytes (see
	// HashArgs). The idempotent-replay lookup and the auto-retry cap are
	// both keyed on (TaskID, ToolName, ArgsHash).
	ArgsHash string
	// ApprovalToken is the RAW one-time approval token this call's policy
	// decision minted (empty is accepted - a caller that has none, e.g. a
	// test). Never stored raw - see hashApprovalToken. Approval tokens are
	// one-time (W3-02) and are NEVER reused on retry: a replay hit here
	// needs no token at all (nothing re-executes); a genuine re-execution
	// attempt must have already gone back through the normal
	// Check/ConsumeToken flow to obtain a FRESH token before ever calling
	// Execute again for the same triple.
	ApprovalToken string
}

// ActionClass mirrors kahyad/internal/policy.ActionClass's four values
// (this package could import that type directly - both live under
// kahyad/internal/ - but keeps its own narrow alias so a caller need not
// import kahyad/internal/policy just to call Execute; the underlying
// string values are identical and interchangeable with policy.ActionClass
// by construction, see the class constants below).
type ActionClass = policy.ActionClass

// Class constants, identical to kahyad/internal/policy's own (re-exported
// here so callers of this package need not import kahyad/internal/policy
// merely to name a class).
const (
	ClassR  = policy.ClassR
	ClassW1 = policy.ClassW1
	ClassW2 = policy.ClassW2
	ClassW3 = policy.ClassW3
)

// Receipts drives the tool_calls intent -> executing -> {receipt, failed}
// lifecycle for every side-effectful (W1/W2/W3) kahyad-owned tool
// execution, and the idempotent-replay lookup that protects a resumed
// task from double-executing an interrupted call.
type Receipts struct {
	db     *sql.DB
	q      ToolCallStore
	ledger Ledger
	// now is time.Now by default; tests substitute a fixed clock.
	now func() time.Time
}

// NewReceipts constructs a Receipts. db is the raw *sql.DB Execute opens
// its own transaction against (store.Store.DB()); q is the ordinary
// (non-tx) *sqlcgen.Queries handle (store.Store.Queries) used for every
// step OTHER than the final receipt commit, which runs inside Execute's
// own transaction via q's WithTx-equivalent - see Execute's own comment.
func NewReceipts(db *sql.DB, q *sqlcgen.Queries, ledger Ledger) *Receipts {
	return &Receipts{db: db, q: q, ledger: ledger, now: time.Now}
}

// SetClock overrides Receipts' clock (tests only).
func (r *Receipts) SetClock(now func() time.Time) { r.now = now }

func (r *Receipts) nowRFC3339() string { return r.now().UTC().Format(time.RFC3339Nano) }

// Execute is the whole W4-02 receipt lifecycle for one side-effectful
// tool call: idempotent-replay check (step 4) -> insert tool_calls
// status=intent (step 3) -> mark executing -> run effect inside a fresh
// transaction -> on success, mark receipt (in that SAME transaction) and
// commit; on failure, mark failed and return the effect's error.
//
// replayed reports whether result came from an EARLIER completed attempt
// (effect was NOT invoked this call) - this is the double-execution-safety
// mechanism the whole W4-02 durability story rests on. A replay hit
// ledgers EventReplayed and returns immediately; effect is never called at
// all in that case.
func (r *Receipts) Execute(ctx context.Context, in ExecuteInput, effect EffectFunc) (result json.RawMessage, replayed bool, err error) {
	if in.Class == ClassR {
		return nil, false, ErrReadOnlyClass
	}

	// Step 4: idempotent replay - a status='receipt' row for this exact
	// (task_id, tool_name, args_hash) triple already exists, from ANY
	// earlier attempt (any seq).
	existing, err := r.q.GetReceiptToolCall(ctx, sqlcgen.GetReceiptToolCallParams{
		TaskID: in.TaskID, ToolName: in.ToolName, ArgsHash: in.ArgsHash,
	})
	if err == nil {
		var env receiptEnvelope
		if uerr := json.Unmarshal([]byte(existing.ReceiptJson.String), &env); uerr != nil {
			return nil, false, fmt.Errorf("task: decode stored receipt (task=%s tool=%s): %w", in.TaskID, in.ToolName, uerr)
		}
		r.ledgerRaw(ctx, in.TraceID, EventReplayed, map[string]any{
			"event": EventReplayed, "task_id": in.TaskID, "tool": in.ToolName, "args_hash": in.ArgsHash, "seq": existing.Seq,
		})
		return env.Result, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("task: replay lookup (task=%s tool=%s): %w", in.TaskID, in.ToolName, err)
	}

	// Step 3: intent.
	seq, err := r.q.NextToolCallSeq(ctx, sqlcgen.NextToolCallSeqParams{
		TaskID: in.TaskID, ToolName: in.ToolName, ArgsHash: in.ArgsHash,
	})
	if err != nil {
		return nil, false, fmt.Errorf("task: next seq (task=%s tool=%s): %w", in.TaskID, in.ToolName, err)
	}
	row, err := r.q.InsertToolCallIntent(ctx, sqlcgen.InsertToolCallIntentParams{
		TaskID: in.TaskID, Seq: seq, ToolName: in.ToolName, Class: string(in.Class),
		ArgsHash: in.ArgsHash, ApprovalTokenID: hashApprovalToken(in.ApprovalToken),
		CreatedAt: r.nowRFC3339(),
	})
	if err != nil {
		return nil, false, fmt.Errorf("task: insert tool_calls intent (task=%s tool=%s): %w", in.TaskID, in.ToolName, err)
	}

	// executing.
	if err := r.q.MarkToolCallExecuting(ctx, sqlcgen.MarkToolCallExecutingParams{
		StartedAt: sql.NullString{String: r.nowRFC3339(), Valid: true}, ID: row.ID,
	}); err != nil {
		return nil, false, fmt.Errorf("task: mark tool_calls executing (id=%d): %w", row.ID, err)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("task: begin tx (task=%s tool=%s): %w", in.TaskID, in.ToolName, err)
	}

	effectResult, effErr := effect(ctx, tx)
	if effErr != nil {
		_ = tx.Rollback()
		if merr := r.q.MarkToolCallFailed(ctx, sqlcgen.MarkToolCallFailedParams{
			FinishedAt: sql.NullString{String: r.nowRFC3339(), Valid: true}, ID: row.ID,
		}); merr != nil {
			return nil, false, fmt.Errorf("task: mark tool_calls failed (id=%d) after effect error %v: %w", row.ID, effErr, merr)
		}
		return nil, false, effErr
	}

	env := receiptEnvelope{Result: effectResult, ResultHash: HashArgs(effectResult)}
	envJSON, merr := json.Marshal(env)
	if merr != nil {
		_ = tx.Rollback()
		return nil, false, fmt.Errorf("task: marshal receipt envelope (task=%s tool=%s): %w", in.TaskID, in.ToolName, merr)
	}

	txq := sqlcgen.New(tx)
	if err := txq.MarkToolCallReceipt(ctx, sqlcgen.MarkToolCallReceiptParams{
		ReceiptJson: sql.NullString{String: string(envJSON), Valid: true},
		FinishedAt:  sql.NullString{String: r.nowRFC3339(), Valid: true},
		ID:          row.ID,
	}); err != nil {
		_ = tx.Rollback()
		return nil, false, fmt.Errorf("task: mark tool_calls receipt (id=%d): %w", row.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("task: commit receipt tx (task=%s tool=%s): %w", in.TaskID, in.ToolName, err)
	}

	return effectResult, false, nil
}

// ListToolCalls returns every tool_calls row for taskID, oldest attempt
// first (`kahya task show <id>`'s own listing).
func (r *Receipts) ListToolCalls(ctx context.Context, taskID string) ([]sqlcgen.ToolCall, error) {
	return r.q.ListToolCallsByTask(ctx, taskID)
}

func (r *Receipts) ledgerRaw(ctx context.Context, traceID, kind string, payload map[string]any) {
	if r.ledger == nil {
		return
	}
	_ = r.ledger.LogEvent(ctx, traceID, kind, payload)
}
