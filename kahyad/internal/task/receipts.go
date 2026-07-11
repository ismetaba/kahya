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
//
// BLOCKER 1 fix (concurrent Execute double-executing the real effect):
// the replay check, seq allocation, and intent insert used to be three
// separate un-transacted statements, so two concurrent Execute calls for
// the IDENTICAL (task_id, tool_name, args_hash) triple could both pass
// the replay guard, land at different seqs, and both run the real effect.
// Execute now (a) serializes same-process callers with an in-process
// keyed mutex (locks field) BEFORE the replay check even runs, and (b)
// relies on idx_tool_calls_live_unique (migrations/0008 - a partial
// UNIQUE(task_id, tool_name, args_hash) WHERE status IN
// ('intent','executing','receipt')) to make a second live row for the
// same key impossible at the DB level too, for cross-process safety - see
// claimIntent's own doc comment for how a unique-constraint hit is
// resolved (replay if a receipt has since appeared, retry once the rival
// resolves to 'failed').
//
// BLOCKER 4 fix (post-effect failure stranding a row at 'executing'
// forever): every failure path once the effect has run - a marshal
// error, the receipt UPDATE itself failing, or the commit failing - used
// to return without ever marking the tool_calls row 'failed', leaving it
// stuck at 'executing' with the replay guard never engaging. Every one of
// those paths now goes through failAndReturn, which is safe precisely
// because the effect's own DB writes live in the SAME transaction as the
// receipt write and have NOT committed at any of those failure points -
// rolling back (implicit on a failed Commit, explicit otherwise) undoes
// them cleanly, so marking the row 'failed' afterward can never strand a
// committed effect with no receipt.
package task

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mattn/go-sqlite3"

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

// ErrToolCallClaimTimeout is returned by Receipts.Execute when
// claimIntent's retry loop (BLOCKER 1 fix) exhausts every attempt without
// either winning the intent insert or ever observing the rival resolve
// (to a receipt to replay, or to 'failed' so a fresh insert can succeed).
// This should never happen from same-process concurrency - the in-process
// keyed mutex in Execute already serializes those callers before either
// ever reaches claimIntent - so reaching this in production would mean a
// genuinely different process/connection has been holding the live row
// for this exact key for the entire retry window.
var ErrToolCallClaimTimeout = errors.New("task: tool call intent claim timed out waiting on a concurrent in-flight attempt")

// claimIntentMaxAttempts/claimIntentInitialBackoff/claimIntentMaxBackoff
// bound claimIntent's retry loop (BLOCKER 1 fix) - see its own doc
// comment. Worst case total wait is a few seconds, never unbounded.
const (
	claimIntentMaxAttempts    = 50
	claimIntentInitialBackoff = 5 * time.Millisecond
	claimIntentMaxBackoff     = 200 * time.Millisecond
)

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

// toolCallKey is the in-process keyed-mutex key (BLOCKER 1 fix) for one
// (task_id, tool_name, args_hash) triple - the same triple
// idx_tool_calls_live_unique and GetReceiptToolCall are both scoped to.
// The NUL separator cannot appear in any of the three fields' normal
// values (task ids/tool names are ASCII identifiers, args_hash is hex),
// so this cannot collide two DIFFERENT triples onto the same key.
func toolCallKey(taskID, toolName, argsHash string) string {
	return taskID + "\x00" + toolName + "\x00" + argsHash
}

// isUniqueConstraintViolation reports whether err is a SQLite unique (or
// primary-key) constraint violation - the mattn/go-sqlite3 shape
// InsertToolCallIntent's hit against idx_tool_calls_live_unique
// (migrations/0008) surfaces as (BLOCKER 1 fix). claimIntent uses this to
// tell "a rival is already holding the live row for this key" apart from
// any other, genuine, insert failure (which it must not swallow).
func isUniqueConstraintViolation(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqlite3.ErrConstraint
	}
	return false
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

// keyedMutexes lets Receipts.Execute serialize concurrent calls for the
// exact same (task_id, tool_name, args_hash) key WITHIN THIS PROCESS
// (BLOCKER 1 fix) without holding one global lock across every unrelated
// tool call: Lock(key) blocks only a caller presenting the SAME key a
// call currently holds; a concurrent call for a DIFFERENT key proceeds
// immediately, fully uncontended. The zero value is not ready to use -
// construct with newKeyedMutexes.
type keyedMutexes struct {
	mu    sync.Mutex
	locks map[string]*keyedMutexEntry
}

// keyedMutexEntry is one key's lock plus a waiter count (guarded by the
// OWNING keyedMutexes.mu, never entry.mu itself) so the entry can be
// removed from the map once nothing references it anymore - this
// structure never grows unbounded across a long-running kahyad process's
// lifetime, no matter how many distinct tool-call keys it ever sees.
type keyedMutexEntry struct {
	mu      sync.Mutex
	waiters int
}

func newKeyedMutexes() *keyedMutexes {
	return &keyedMutexes{locks: make(map[string]*keyedMutexEntry)}
}

// Lock blocks until key is uncontended, then returns an unlock function
// the caller MUST invoke exactly once (typically via defer) to release
// it.
func (k *keyedMutexes) Lock(key string) (unlock func()) {
	k.mu.Lock()
	e, ok := k.locks[key]
	if !ok {
		e = &keyedMutexEntry{}
		k.locks[key] = e
	}
	e.waiters++
	k.mu.Unlock()

	e.mu.Lock()

	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Unlock()
			k.mu.Lock()
			e.waiters--
			if e.waiters == 0 {
				delete(k.locks, key)
			}
			k.mu.Unlock()
		})
	}
}

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
	// locks is Execute's in-process keyed mutex (BLOCKER 1 fix) - see
	// keyedMutexes' own doc comment.
	locks *keyedMutexes
}

// NewReceipts constructs a Receipts. db is the raw *sql.DB Execute opens
// its own transaction against (store.Store.DB()); q is the ordinary
// (non-tx) *sqlcgen.Queries handle (store.Store.Queries) used for every
// step OTHER than the final receipt commit, which runs inside Execute's
// own transaction via q's WithTx-equivalent - see Execute's own comment.
func NewReceipts(db *sql.DB, q *sqlcgen.Queries, ledger Ledger) *Receipts {
	return &Receipts{db: db, q: q, ledger: ledger, now: time.Now, locks: newKeyedMutexes()}
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
//
// BLOCKER 1 fix: every call for the exact same (task_id, tool_name,
// args_hash) key is serialized, in this process, by locks before the
// replay check even runs - see keyedMutexes' own doc comment and
// claimIntent's for the DB-level (cross-process) half of this guarantee.
// BLOCKER 4 fix: every failure path from the moment the effect has run
// onward goes through failAndReturn, so a tool_calls row can never again
// be left stranded at 'executing' with neither a receipt nor a 'failed'
// row - see failAndReturn's own doc comment.
func (r *Receipts) Execute(ctx context.Context, in ExecuteInput, effect EffectFunc) (result json.RawMessage, replayed bool, err error) {
	if in.Class == ClassR {
		return nil, false, ErrReadOnlyClass
	}

	unlock := r.locks.Lock(toolCallKey(in.TaskID, in.ToolName, in.ArgsHash))
	defer unlock()

	// Step 4: idempotent replay - a status='receipt' row for this exact
	// (task_id, tool_name, args_hash) triple already exists, from ANY
	// earlier attempt (any seq).
	if hitResult, seq, hit, cerr := r.checkReplay(ctx, in); cerr != nil {
		return nil, false, cerr
	} else if hit {
		r.ledgerReplayed(ctx, in, seq)
		return hitResult, true, nil
	}

	// Step 3: intent (claimIntent also resolves the BLOCKER 1 DB-level
	// unique-constraint race - see its own doc comment).
	row, replayResult, replaySeq, wasReplayed, err := r.claimIntent(ctx, in)
	if err != nil {
		return nil, false, err
	}
	if wasReplayed {
		r.ledgerReplayed(ctx, in, replaySeq)
		return replayResult, true, nil
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
		return nil, false, r.failAndReturn(ctx, row.ID, effErr)
	}

	// BLOCKER 4 fix: marshal BEFORE anything that cannot be rolled back.
	// tx has not committed at this point (nor at either point below) - a
	// failure here still cleanly undoes the effect's own writes, so
	// failAndReturn is always safe to call from here on.
	env := receiptEnvelope{Result: effectResult, ResultHash: HashArgs(effectResult)}
	envJSON, merr := json.Marshal(env)
	if merr != nil {
		_ = tx.Rollback()
		return nil, false, r.failAndReturn(ctx, row.ID, fmt.Errorf("task: marshal receipt envelope (task=%s tool=%s): %w", in.TaskID, in.ToolName, merr))
	}

	txq := sqlcgen.New(tx)
	if err := txq.MarkToolCallReceipt(ctx, sqlcgen.MarkToolCallReceiptParams{
		ReceiptJson: sql.NullString{String: string(envJSON), Valid: true},
		FinishedAt:  sql.NullString{String: r.nowRFC3339(), Valid: true},
		ID:          row.ID,
	}); err != nil {
		_ = tx.Rollback()
		return nil, false, r.failAndReturn(ctx, row.ID, fmt.Errorf("task: mark tool_calls receipt (id=%d): %w", row.ID, err))
	}
	if err := tx.Commit(); err != nil {
		// A failed Commit leaves tx done - database/sql's own contract -
		// so there is nothing left to Rollback here. Either way SQLite's
		// transaction is all-or-nothing: the effect's writes did NOT
		// durably land, so marking 'failed' is safe (see failAndReturn's
		// own doc comment).
		return nil, false, r.failAndReturn(ctx, row.ID, fmt.Errorf("task: commit receipt tx (task=%s tool=%s): %w", in.TaskID, in.ToolName, err))
	}

	return effectResult, false, nil
}

// checkReplay looks up an existing 'receipt' row for in's exact
// (task_id, tool_name, args_hash) triple (task spec step 4). hit=false,
// err=nil means no receipt exists (yet) - callers that need to
// distinguish "genuinely fresh" from "a rival attempt is currently live"
// do so themselves (claimIntent); this method only ever answers the
// narrower "is there a receipt to replay" question.
func (r *Receipts) checkReplay(ctx context.Context, in ExecuteInput) (result json.RawMessage, seq int64, hit bool, err error) {
	existing, err := r.q.GetReceiptToolCall(ctx, sqlcgen.GetReceiptToolCallParams{
		TaskID: in.TaskID, ToolName: in.ToolName, ArgsHash: in.ArgsHash,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, false, nil
		}
		return nil, 0, false, fmt.Errorf("task: replay lookup (task=%s tool=%s): %w", in.TaskID, in.ToolName, err)
	}
	var env receiptEnvelope
	if uerr := json.Unmarshal([]byte(existing.ReceiptJson.String), &env); uerr != nil {
		return nil, 0, false, fmt.Errorf("task: decode stored receipt (task=%s tool=%s): %w", in.TaskID, in.ToolName, uerr)
	}
	return env.Result, existing.Seq, true, nil
}

func (r *Receipts) ledgerReplayed(ctx context.Context, in ExecuteInput, seq int64) {
	r.ledgerRaw(ctx, in.TraceID, EventReplayed, map[string]any{
		"event": EventReplayed, "task_id": in.TaskID, "tool": in.ToolName, "args_hash": in.ArgsHash, "seq": seq,
	})
}

// claimIntent inserts a fresh tool_calls 'intent' row for in's
// (task_id, tool_name, args_hash) triple, at the next seq for that key
// (task spec step 3).
//
// BLOCKER 1 fix: idx_tool_calls_live_unique (migrations/0008) makes a
// second live (intent/executing/receipt) row for the exact same key
// impossible at the DB level. Execute's in-process keyed mutex already
// rules out a SAME-process rival ever reaching this method concurrently,
// so a unique-constraint hit here can only mean a DIFFERENT process (or
// connection) currently holds the live row for this key. This retries
// with a short bounded backoff instead of failing outright: each
// iteration either (a) wins the insert outright, (b) discovers the rival
// has since committed a receipt - returned directly (wasReplayed=true)
// for Execute to replay, exactly as if checkReplay had run one instant
// later - or (c) discovers the rival has since failed, which frees the
// unique slot so the next iteration's insert succeeds. Returns
// ErrToolCallClaimTimeout if none of those resolve within
// claimIntentMaxAttempts.
func (r *Receipts) claimIntent(ctx context.Context, in ExecuteInput) (row sqlcgen.ToolCall, replayResult json.RawMessage, replaySeq int64, wasReplayed bool, err error) {
	backoff := claimIntentInitialBackoff
	for attempt := 0; attempt < claimIntentMaxAttempts; attempt++ {
		seq, serr := r.q.NextToolCallSeq(ctx, sqlcgen.NextToolCallSeqParams{
			TaskID: in.TaskID, ToolName: in.ToolName, ArgsHash: in.ArgsHash,
		})
		if serr != nil {
			return sqlcgen.ToolCall{}, nil, 0, false, fmt.Errorf("task: next seq (task=%s tool=%s): %w", in.TaskID, in.ToolName, serr)
		}
		inserted, ierr := r.q.InsertToolCallIntent(ctx, sqlcgen.InsertToolCallIntentParams{
			TaskID: in.TaskID, Seq: seq, ToolName: in.ToolName, Class: string(in.Class),
			ArgsHash: in.ArgsHash, ApprovalTokenID: hashApprovalToken(in.ApprovalToken),
			CreatedAt: r.nowRFC3339(),
		})
		if ierr == nil {
			return inserted, nil, 0, false, nil
		}
		if !isUniqueConstraintViolation(ierr) {
			return sqlcgen.ToolCall{}, nil, 0, false, fmt.Errorf("task: insert tool_calls intent (task=%s tool=%s): %w", in.TaskID, in.ToolName, ierr)
		}

		if hitResult, hitSeq, hit, rerr := r.checkReplay(ctx, in); rerr != nil {
			return sqlcgen.ToolCall{}, nil, 0, false, rerr
		} else if hit {
			return sqlcgen.ToolCall{}, hitResult, hitSeq, true, nil
		}

		select {
		case <-ctx.Done():
			return sqlcgen.ToolCall{}, nil, 0, false, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < claimIntentMaxBackoff {
			backoff *= 2
		}
	}
	return sqlcgen.ToolCall{}, nil, 0, false, fmt.Errorf("%w (task=%s tool=%s)", ErrToolCallClaimTimeout, in.TaskID, in.ToolName)
}

// failAndReturn marks the tool_calls row id 'failed' then returns cause -
// every Execute failure path from the moment the effect has run onward
// (BLOCKER 4 fix) goes through this exact function, so tool_calls.status
// can never again be left stranded at 'executing' with neither a receipt
// nor a 'failed' row (the resume scan's replay guard would otherwise wait
// on such a row forever - it never becomes receipt-having, and
// ListReceiptlessToolCalls would keep finding it, but nothing ever moves
// it out of 'executing' either). This is safe to call from every one of
// those paths specifically because, by the time failAndReturn runs, the
// effect's own transaction has already been rolled back (or never
// committed at all on a failed Commit - database/sql's own contract) -
// marking 'failed' here can never strand a phantom COMMITTED effect with
// no receipt, only ever a genuinely rolled-back one that a fresh Execute
// retry can safely re-attempt from scratch. If marking failed ITSELF
// errors, that secondary error is wrapped around cause (never silently
// dropped); otherwise cause is returned completely unwrapped, so a caller
// checking the effect's own error identity (e.g. errors.Is against a
// tool-specific sentinel) still sees exactly that - matching this
// package's pre-existing behavior for a plain effect error.
func (r *Receipts) failAndReturn(ctx context.Context, id int64, cause error) error {
	if merr := r.q.MarkToolCallFailed(ctx, sqlcgen.MarkToolCallFailedParams{
		FinishedAt: sql.NullString{String: r.nowRFC3339(), Valid: true}, ID: id,
	}); merr != nil {
		return fmt.Errorf("task: mark tool_calls failed (id=%d) after %v: %w", id, cause, merr)
	}
	return cause
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
