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
// those paths now goes through failAndReturn (commitReceipt). For
// ExecuteInTx that is safe precisely because the effect's own DB writes
// live in the SAME transaction as the receipt write and have NOT
// committed at any of those failure points - rolling back (implicit on a
// failed Commit, explicit otherwise) undoes them cleanly, so marking the
// row 'failed' afterward can never strand a committed DB effect with no
// receipt. For Execute (an external effect) the side effect lives outside
// brain.db and no transaction ever covered it - unchanged by FIX-I-12.
//
// FIX-I-12 (single connection pinned across a slow effect): brain.db has
// ONE pooled connection (store.Open: SetMaxOpenConns(1)). Execute used to
// BeginTx before running the effect and commit only after it returned, so
// a slow EXTERNAL effect (filesystem, Docker, AppleScript, the W4-07
// w2_slow_stub sleep) checked that connection out for its whole duration -
// /policy/check's Engine.Check (which ledgers every decision), the ledger
// and every other kahyad DB call queued behind it, and the worker's 5 s
// UDS budget turned that into a spurious fail-closed DENY. Execute now
// runs the effect with NO transaction and NO connection checked out (tx is
// nil) and writes the receipt in its own short statement afterwards
// (commitReceipt); ExecuteInTx keeps the old in-transaction semantics for
// SHORT effects that write brain.db and must commit atomically with their
// receipt. Both share prepare (replay check, intent claim, mark executing)
// under the same keyed lock, so the BLOCKER 1 guarantees are identical.
// Crash semantics are unchanged: an effect that finished but whose receipt
// never committed leaves the row 'executing' and receipt-less, which the
// resume scan (resolve.go) handles - W1 auto-retry, W2/W3 never (HANDOFF
// §6 W4 ⚑); the transaction was never durable across a crash anyway.
//
// FIX-I-4 (the W4 receipt gap): the REAL side-effectful tools - fs_write,
// fs_delete, shell_docker, shell_host, applescript_run, jxa_run,
// shortcuts_run (mcp/fs, mcp/shell, mcp/osascript, through
// kahyad/internal/server's receipt adapter) and every non-R tool on the
// generic /v1/mcp path (memory_write/memory_forget, the dev w2_slow_stub) -
// now run their effect through ExecuteDetached, keyed on the canonical
// tool-input bytes, and call Replay BEFORE their policy check: a resumed
// session re-issuing a completed call gets the stored result, no second
// execution, no second approval. The tool_calls intent/executing row is the
// pre-op journal, the receipt the post-op record.
//
// FIX-FINAL receipt-replay-per-run (owner decision 2026-10-16): receipts
// replay only on RESUME. Every tool_calls row records the worker run
// (incarnation) that made it (tool_calls.run, migrations/0026): a task's
// first worker spawn is run 1 and every later spawn of the same task (an
// outbox redispatch - crash resume, cloud-retry redispatch, `kahya task
// resolve --retry`) the next one; the counter is tasks.worker_run, bumped
// kahyad-side before the worker starts and resolved here once per tool call
// (currentRun, WithRunPin) - never taken from the worker. A replay answers
// only a receipt an EARLIER run wrote that the current run has not consumed
// yet (oldest first; tool_calls.replayed_run records the consuming run), so
// each earlier receipt is replayed at most once per run: after a crash
// following X, Y, X the resumed run's X, Y, X replay those three and a 4th X
// executes. Inside one live run an identical call executes normally - a new
// row, a normal policy decision - and many receipts of one key are legal;
// idx_tool_calls_live_unique still allows only ONE in-flight
// (intent/executing) attempt per key, so two executions of the identical
// call never overlap.
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

	"kahya/kahyad/internal/logx"
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

// EventReceiptPersistFailed is ledgered (and logged as JSONL - the DB the
// ledger lives in may be the very thing failing) when ExecuteDetached's
// effect HAS run but its receipt could not be written (FIX-I-4).
const EventReceiptPersistFailed = "tool.receipt_persist_failed"

// ErrReceiptPersistFailed: ExecuteDetached's effect HAS run but its receipt
// could not be committed (FIX-I-4). The tool_calls row is deliberately left
// at 'executing' - never 'failed', which would free the (task_id, tool_name,
// args_hash) key and let the very next identical call (a model retrying the
// error, a resumed session re-issuing it) run an effect that already
// happened. The receipt-less 'executing' row then goes to the resume scan,
// which applies HANDOFF §6 W4: W1 may be auto-retried (undoable), W2/W3
// block on the user.
var ErrReceiptPersistFailed = errors.New("task: effect ran but its receipt could not be persisted")

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

// ErrReplayConsumeContended is returned when a replay found an earlier
// run's receipt but kept losing the compare-and-set that consumes it for the
// current run (MarkToolCallReplayed) - something else consumed every
// candidate it saw replayConsumeMaxAttempts times in a row. The keyed mutex
// rules this out inside one process; the caller refuses the call
// (fail-closed: it neither replays nor executes).
var ErrReplayConsumeContended = errors.New("task: replay could not consume an earlier run's receipt (contended)")

// replayConsumeMaxAttempts bounds checkReplay's lookup/consume loop.
const replayConsumeMaxAttempts = 8

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
	// GetReplayableReceiptToolCall / MarkToolCallReplayed are the per-run
	// replay lookup and its consume step (FIX-FINAL receipt-replay-per-run).
	GetReplayableReceiptToolCall(ctx context.Context, arg sqlcgen.GetReplayableReceiptToolCallParams) (sqlcgen.ToolCall, error)
	MarkToolCallReplayed(ctx context.Context, arg sqlcgen.MarkToolCallReplayedParams) (int64, error)
	// GetTaskWorkerRun reads tasks.worker_run, the task's current worker
	// run (currentRun).
	GetTaskWorkerRun(ctx context.Context, id string) (int64, error)
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
// idx_tool_calls_live_unique and GetReplayableReceiptToolCall are both
// scoped to.
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
// an error.
//
// tx is nil for Execute (FIX-I-12): external effects - a filesystem
// write, an AppleScript run, a Docker exec, an HTTP call - run with no
// transaction and no pooled brain.db connection checked out, so a slow
// effect never blocks /policy/check or the ledger. Execute commits the
// receipt immediately once effect returns, which is the closest a
// single-database write can get to atomicity when the side effect itself
// is not a database write at all (no two-phase commit across brain.db and
// the filesystem is attempted or claimed here).
//
// Use ExecuteInTx for an effect that writes brain.db and must commit
// atomically with its receipt (task spec step 3: "in the same transaction
// that commits the tool's DB effects" - e.g. mutating autonomy_state,
// inserting a memory fact): there tx is a *sql.Tx already open against the
// SAME database and the effect must perform its writes through it. Such an
// effect must be short (milliseconds) because it holds the store's only
// connection for its whole duration.
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

// runPinKey is the context key of a tool call's run pin (WithRunPin).
type runPinKey struct{}

// runPin holds the worker run each task id resolved to for ONE tool call.
type runPin struct {
	mu   sync.Mutex
	runs map[string]int64
}

// WithRunPin returns ctx carrying a run pin for ONE tool call (FIX-FINAL
// receipt-replay-per-run): the first Receipts step under it (normally Replay,
// before the policy check) resolves the task's worker run from
// tasks.worker_run, and every later step of the same call (Execute after the
// policy check and any approval wait) reuses that value. So a call is always
// recorded under the run that issued it, even if the task is redispatched -
// and tasks.worker_run bumped - while the call waits on an approval: a later
// run then replays it instead of executing it a second time.
// kahyad/internal/server installs one per /v1/mcp tools/call
// (policyGateMiddleware); without a pin every step reads tasks.worker_run
// itself.
func WithRunPin(ctx context.Context) context.Context {
	return context.WithValue(ctx, runPinKey{}, &runPin{runs: make(map[string]int64)})
}

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

	// inflight counts, per task id, the Execute/ExecuteInTx/ExecuteDetached
	// calls currently between their keyed-lock acquisition and their final
	// receipt/failed write (FIX-I-12) - see IsLive.
	inflightMu sync.Mutex
	inflight   map[string]int

	// jsonl is the OPTIONAL JSONL sink for EventReceiptPersistFailed
	// (FIX-I-4): that failure means brain.db did not take a write, so the
	// ledger row alone may be lost too. nil = ledger only (tests).
	jsonl *logx.Logger
}

// SetJSONLLogger wires the JSONL sink ExecuteDetached reports a receipt
// persistence failure to (FIX-I-4), alongside the ledger row.
func (r *Receipts) SetJSONLLogger(l *logx.Logger) { r.jsonl = l }

// IsLive reports whether one of taskID's side-effectful tool calls is
// executing inside THIS kahyad process right now (FIX-I-12). It makes
// *Receipts a LiveChecker for the resume scan (AnyLive): before FIX-I-12 a
// periodic scan simply could not run while an effect was in flight -
// Execute's transaction pinned brain.db's single connection until the
// receipt committed - so it never saw that call's 'executing',
// receipt-less row. With the effect now outside any transaction the scan
// does run, and without this it would mistake an in-process call whose
// worker died (W4-07 scenario A: kill -9 mid-call; the effect still runs to
// completion on kahyad's own goroutine) for an interrupted one and block a
// W2/W3 task (or fail its row) although the receipt is about to land. A
// kahyad crash empties this map with everything else, so the startup scan
// still sees every genuinely interrupted call.
func (r *Receipts) IsLive(taskID string) bool {
	r.inflightMu.Lock()
	defer r.inflightMu.Unlock()
	return r.inflight[taskID] > 0
}

// beginEffect / endEffect bracket one Execute/ExecuteInTx call for IsLive.
func (r *Receipts) beginEffect(taskID string) {
	r.inflightMu.Lock()
	defer r.inflightMu.Unlock()
	if r.inflight == nil {
		r.inflight = make(map[string]int)
	}
	r.inflight[taskID]++
}

func (r *Receipts) endEffect(taskID string) {
	r.inflightMu.Lock()
	defer r.inflightMu.Unlock()
	if r.inflight[taskID] <= 1 {
		delete(r.inflight, taskID)
		return
	}
	r.inflight[taskID]--
}

// NewReceipts constructs a Receipts. db is the raw *sql.DB ExecuteInTx
// opens its transaction against (store.Store.DB()); q is the ordinary
// (non-tx) *sqlcgen.Queries handle (store.Store.Queries) used for every
// step outside that transaction - including Execute's own receipt write
// (FIX-I-12: Execute holds no transaction at all).
func NewReceipts(db *sql.DB, q *sqlcgen.Queries, ledger Ledger) *Receipts {
	return &Receipts{db: db, q: q, ledger: ledger, now: time.Now, locks: newKeyedMutexes()}
}

// SetClock overrides Receipts' clock (tests only).
func (r *Receipts) SetClock(now func() time.Time) { r.now = now }

func (r *Receipts) nowRFC3339() string { return r.now().UTC().Format(time.RFC3339Nano) }

// currentRun resolves taskID's worker run for this tool call (FIX-FINAL
// receipt-replay-per-run): the value pinned on ctx (WithRunPin) if a step of
// the same call already resolved it, else tasks.worker_run - kahyad-side
// state, never a worker-supplied value. A task id with no tasks row is run
// 0: tool_calls.task_id references tasks(id), so such a task has no row to
// replay (every lookup is a miss) and an Execute fails closed on the FK. Any
// other read error is returned and the call is refused (fail-closed).
func (r *Receipts) currentRun(ctx context.Context, taskID string) (int64, error) {
	pin, _ := ctx.Value(runPinKey{}).(*runPin)
	if pin != nil {
		pin.mu.Lock()
		defer pin.mu.Unlock()
		if run, ok := pin.runs[taskID]; ok {
			return run, nil
		}
	}
	run, err := r.q.GetTaskWorkerRun(ctx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		run, err = 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("task: worker run lookup (task=%s): %w", taskID, err)
	}
	if pin != nil {
		pin.runs[taskID] = run
	}
	return run, nil
}

// Execute is the whole W4-02 receipt lifecycle for one side-effectful
// tool call whose effect lives OUTSIDE brain.db: idempotent-replay check
// (step 4) -> insert tool_calls status=intent (step 3) -> mark executing
// -> run effect with NO transaction (tx is nil, FIX-I-12) -> on success,
// write the receipt (commitReceipt); on failure, mark failed and return
// the effect's error.
//
// replayed reports whether result came from a completed attempt of an
// EARLIER worker run of the task that this run had not consumed yet (effect
// was NOT invoked this call) - this is the double-execution-safety mechanism
// the whole W4-02 durability story rests on. A replay hit ledgers
// EventReplayed and returns immediately; effect is never called at all in
// that case. An identical call whose only receipts belong to the current run
// (or were already consumed by it) executes (FIX-FINAL
// receipt-replay-per-run).
//
// BLOCKER 1 fix: every call for the exact same (task_id, tool_name,
// args_hash) key is serialized, in this process, by locks before the
// replay check even runs - see keyedMutexes' own doc comment and
// claimIntent's for the DB-level (cross-process) half of this guarantee.
// The keyed lock (an in-process mutex, never a DB connection) is still
// held across the effect.
// BLOCKER 4 fix: every failure path from the moment the effect has run
// onward goes through failAndReturn, so a tool_calls row can never again
// be left stranded at 'executing' with neither a receipt nor a 'failed'
// row - see failAndReturn's own doc comment.
func (r *Receipts) Execute(ctx context.Context, in ExecuteInput, effect EffectFunc) (result json.RawMessage, replayed bool, err error) {
	return r.executeExternal(ctx, in, effect, false)
}

// DetachedEffectFunc is the effect of a REAL side-effectful tool, run by
// ExecuteDetached (FIX-I-4): not a brain.db write - a file write or Trash
// move, a docker run, an osascript, a host command, an MCP tool handler.
type DetachedEffectFunc func(ctx context.Context) (json.RawMessage, error)

// ExecuteDetached is Execute (nil tx, FIX-I-12 - built on the same
// executeExternal) for the real kahyad-owned tools (FIX-I-4, C-7): replay ->
// intent -> executing -> effect with NO transaction and NO brain.db
// connection held -> receipt in its own short statement, under the same
// keyed mutex, unique-index claim and IsLive bracket as Execute. The one
// difference is what happens when the effect HAS run but its receipt cannot
// be written: Execute marks the row 'failed' (BLOCKER 4), which for an
// external effect frees the key so the next identical call runs it again;
// ExecuteDetached leaves the row 'executing', ledgers (and JSONL-logs)
// EventReceiptPersistFailed and returns the effect's result together with
// ErrReceiptPersistFailed - the resume scan then applies HANDOFF §6 W4 to
// the receipt-less row (W1 auto-retry, W2/W3 never). An effect that itself
// reports an error still marks the row 'failed' (Execute's semantics: the
// tool said its effect did not complete).
func (r *Receipts) ExecuteDetached(ctx context.Context, in ExecuteInput, effect DetachedEffectFunc) (result json.RawMessage, replayed bool, err error) {
	return r.executeExternal(ctx, in, func(ctx context.Context, _ *sql.Tx) (json.RawMessage, error) {
		return effect(ctx)
	}, true)
}

// Replay answers "did an EARLIER worker run of this task complete this exact
// (task_id, tool_name, args_hash) call, with a receipt the current run has
// not consumed yet?" WITHOUT claiming an intent row (FIX-I-4, narrowed to
// resume by FIX-FINAL receipt-replay-per-run). A side-effectful tool calls
// it BEFORE its policy check on every call, so a resumed session re-issuing
// an already-completed call neither re-executes it nor asks for (and burns)
// a second approval: a hit consumes that receipt for the current run,
// returns the stored result and ledgers EventReplayed. A miss writes nothing
// - and an identical call inside the live run that wrote the receipt is
// always a miss. The run is resolved (and pinned on ctx, WithRunPin) BEFORE
// the keyed lock, so waiting on an in-flight identical call never moves
// this call into a later run. Runs under the same keyed mutex as Execute*,
// so it never observes a half-written attempt of its own process.
func (r *Receipts) Replay(ctx context.Context, in ExecuteInput) (result json.RawMessage, hit bool, err error) {
	run, err := r.currentRun(ctx, in.TaskID)
	if err != nil {
		return nil, false, err
	}
	unlock := r.locks.Lock(toolCallKey(in.TaskID, in.ToolName, in.ArgsHash))
	defer unlock()
	res, row, ok, err := r.checkReplay(ctx, in, run)
	if err != nil || !ok {
		return nil, false, err
	}
	r.ledgerReplayed(ctx, in, row, run)
	return res, true, nil
}

// executeExternal is Execute/ExecuteDetached's shared lifecycle (see
// Execute's doc comment). detached selects ExecuteDetached's handling of a
// receipt that cannot be persisted after the effect ran.
func (r *Receipts) executeExternal(ctx context.Context, in ExecuteInput, effect EffectFunc, detached bool) (json.RawMessage, bool, error) {
	if in.Class == ClassR {
		return nil, false, ErrReadOnlyClass
	}
	run, err := r.currentRun(ctx, in.TaskID)
	if err != nil {
		return nil, false, err
	}

	unlock := r.locks.Lock(toolCallKey(in.TaskID, in.ToolName, in.ArgsHash))
	defer unlock()
	// Registered before the intent row exists and released only after the
	// receipt/failed write: the resume scan never sees this call's row
	// 'executing' without also seeing the task live (IsLive).
	r.beginEffect(in.TaskID)
	defer r.endEffect(in.TaskID)

	row, replayResult, wasReplayed, err := r.prepare(ctx, in, run)
	if err != nil {
		return nil, false, err
	}
	if wasReplayed {
		return replayResult, true, nil
	}

	// FIX-I-12: the effect runs with NO transaction and NO pooled
	// connection checked out - a slow external side effect must never pin
	// brain.db's single connection (store.Open: SetMaxOpenConns(1));
	// /policy/check, the ledger and every other kahyad write keep flowing
	// while it runs.
	effectResult, effErr := effect(ctx, nil)
	if effErr != nil {
		return nil, false, r.failAndReturn(ctx, row.ID, effErr)
	}
	if !detached {
		return r.commitReceipt(ctx, row.ID, in, effectResult, nil)
	}
	return r.commitDetachedReceipt(ctx, row, in, effectResult)
}

// commitDetachedReceipt writes ExecuteDetached's receipt in one autocommit
// statement. Any failure here happens AFTER the external effect ran: the
// row stays 'executing' (see ErrReceiptPersistFailed) and the failure is
// recorded on both channels.
func (r *Receipts) commitDetachedReceipt(ctx context.Context, row sqlcgen.ToolCall, in ExecuteInput, effectResult json.RawMessage) (json.RawMessage, bool, error) {
	envJSON, merr := json.Marshal(receiptEnvelope{Result: effectResult, ResultHash: HashArgs(effectResult)})
	if merr == nil {
		merr = r.q.MarkToolCallReceipt(ctx, sqlcgen.MarkToolCallReceiptParams{
			ReceiptJson: sql.NullString{String: string(envJSON), Valid: true},
			FinishedAt:  sql.NullString{String: r.nowRFC3339(), Valid: true},
			ID:          row.ID,
		})
	}
	if merr == nil {
		return effectResult, false, nil
	}
	if r.jsonl != nil {
		r.jsonl.With(in.TraceID).Error(EventReceiptPersistFailed,
			"task_id", in.TaskID, "tool", in.ToolName, "tool_call_id", row.ID, "seq", row.Seq, "err", merr.Error())
	}
	r.ledgerRaw(ctx, in.TraceID, EventReceiptPersistFailed, map[string]any{
		"event": EventReceiptPersistFailed, "task_id": in.TaskID, "tool": in.ToolName,
		"tool_call_id": row.ID, "seq": row.Seq, "args_hash": in.ArgsHash, "err": merr.Error(),
	})
	return effectResult, false, fmt.Errorf("%w (task=%s tool=%s): %v", ErrReceiptPersistFailed, in.TaskID, in.ToolName, merr)
}

// ExecuteInTx is Execute for an effect that WRITES brain.db (autonomy_state,
// facts, ...) and must commit atomically with its receipt: the effect runs
// inside a fresh transaction it receives as tx, and the receipt row is
// written in that SAME transaction before the single commit (task spec
// step 3). The effect MUST be short (milliseconds): it holds the store's
// only connection (store.Open: SetMaxOpenConns(1)) for its whole duration,
// so a slow effect here would block /policy/check and the ledger exactly
// as FIX-I-12 removed from Execute. External (filesystem, Docker,
// AppleScript, HTTP) effects use Execute. Replay, intent claim and the
// BLOCKER 1/4 guarantees are identical to Execute's (shared prepare /
// commitReceipt).
func (r *Receipts) ExecuteInTx(ctx context.Context, in ExecuteInput, effect EffectFunc) (result json.RawMessage, replayed bool, err error) {
	if in.Class == ClassR {
		return nil, false, ErrReadOnlyClass
	}
	run, err := r.currentRun(ctx, in.TaskID)
	if err != nil {
		return nil, false, err
	}

	unlock := r.locks.Lock(toolCallKey(in.TaskID, in.ToolName, in.ArgsHash))
	defer unlock()
	// Registered before the intent row exists and released only after the
	// receipt/failed write: the resume scan never sees this call's row
	// 'executing' without also seeing the task live (IsLive).
	r.beginEffect(in.TaskID)
	defer r.endEffect(in.TaskID)

	row, replayResult, wasReplayed, err := r.prepare(ctx, in, run)
	if err != nil {
		return nil, false, err
	}
	if wasReplayed {
		return replayResult, true, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		// The effect never ran: marking the row 'failed' is exactly true
		// and frees the key for a fresh attempt.
		return nil, false, r.failAndReturn(ctx, row.ID, fmt.Errorf("task: begin tx (task=%s tool=%s): %w", in.TaskID, in.ToolName, err))
	}

	effectResult, effErr := effect(ctx, tx)
	if effErr != nil {
		_ = tx.Rollback()
		return nil, false, r.failAndReturn(ctx, row.ID, effErr)
	}
	return r.commitReceipt(ctx, row.ID, in, effectResult, tx)
}

// prepare is the shared front half of Execute/ExecuteInTx, run with the
// caller already holding the keyed lock: idempotent replay (step 4) ->
// intent claim (step 3; claimIntent also resolves the BLOCKER 1 DB-level
// unique-constraint race) -> mark executing (an autocommit statement, as
// before). replayed=true means replayResult is a stored receipt's result
// and EventReplayed has been ledgered; the caller must not run the effect.
// run is the call's worker run (currentRun): the intent row records it.
func (r *Receipts) prepare(ctx context.Context, in ExecuteInput, run int64) (row sqlcgen.ToolCall, replayResult json.RawMessage, replayed bool, err error) {
	// Step 4: idempotent replay - an earlier run completed this exact
	// (task_id, tool_name, args_hash) call and the current run has not
	// consumed that receipt yet (an identical call of an earlier run that
	// completed while this one waited on its policy decision is caught here
	// too).
	if hitResult, hitRow, hit, cerr := r.checkReplay(ctx, in, run); cerr != nil {
		return sqlcgen.ToolCall{}, nil, false, cerr
	} else if hit {
		r.ledgerReplayed(ctx, in, hitRow, run)
		return sqlcgen.ToolCall{}, hitResult, true, nil
	}

	// Step 3: intent.
	row, claimedReplay, replayRow, wasReplayed, err := r.claimIntent(ctx, in, run)
	if err != nil {
		return sqlcgen.ToolCall{}, nil, false, err
	}
	if wasReplayed {
		r.ledgerReplayed(ctx, in, replayRow, run)
		return sqlcgen.ToolCall{}, claimedReplay, true, nil
	}

	// executing.
	if err := r.q.MarkToolCallExecuting(ctx, sqlcgen.MarkToolCallExecutingParams{
		StartedAt: sql.NullString{String: r.nowRFC3339(), Valid: true}, ID: row.ID,
	}); err != nil {
		return sqlcgen.ToolCall{}, nil, false, fmt.Errorf("task: mark tool_calls executing (id=%d): %w", row.ID, err)
	}
	return row, nil, false, nil
}

// commitReceipt marshals the receipt envelope and writes the receipt row
// (BLOCKER 4 semantics preserved: every failure path from here marks the
// row 'failed'). With tx == nil (Execute) the receipt is ONE short
// autocommit statement through the ordinary store - no connection is held
// beyond it; with a tx (ExecuteInTx) it is written in that transaction,
// which is then committed, so the effect's own DB writes and the receipt
// land together or not at all.
func (r *Receipts) commitReceipt(ctx context.Context, id int64, in ExecuteInput, effectResult json.RawMessage, tx *sql.Tx) (json.RawMessage, bool, error) {
	rollback := func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}

	// BLOCKER 4 fix: marshal BEFORE anything that cannot be rolled back.
	// An open tx has not committed at this point (nor at either point
	// below) - a failure here still cleanly undoes the effect's own DB
	// writes, so failAndReturn is always safe to call from here on.
	env := receiptEnvelope{Result: effectResult, ResultHash: HashArgs(effectResult)}
	envJSON, merr := json.Marshal(env)
	if merr != nil {
		rollback()
		return nil, false, r.failAndReturn(ctx, id, fmt.Errorf("task: marshal receipt envelope (task=%s tool=%s): %w", in.TaskID, in.ToolName, merr))
	}

	params := sqlcgen.MarkToolCallReceiptParams{
		ReceiptJson: sql.NullString{String: string(envJSON), Valid: true},
		FinishedAt:  sql.NullString{String: r.nowRFC3339(), Valid: true},
		ID:          id,
	}
	if tx == nil {
		if err := r.q.MarkToolCallReceipt(ctx, params); err != nil {
			return nil, false, r.failAndReturn(ctx, id, fmt.Errorf("task: mark tool_calls receipt (id=%d): %w", id, err))
		}
		return effectResult, false, nil
	}

	if err := sqlcgen.New(tx).MarkToolCallReceipt(ctx, params); err != nil {
		rollback()
		return nil, false, r.failAndReturn(ctx, id, fmt.Errorf("task: mark tool_calls receipt (id=%d): %w", id, err))
	}
	if err := tx.Commit(); err != nil {
		// A failed Commit leaves tx done - database/sql's own contract -
		// so there is nothing left to Rollback here. Either way SQLite's
		// transaction is all-or-nothing: the effect's writes did NOT
		// durably land, so marking 'failed' is safe (see failAndReturn's
		// own doc comment).
		return nil, false, r.failAndReturn(ctx, id, fmt.Errorf("task: commit receipt tx (task=%s tool=%s): %w", in.TaskID, in.ToolName, err))
	}
	return effectResult, false, nil
}

// checkReplay looks up the oldest receipt of in's exact (task_id,
// tool_name, args_hash) triple that an EARLIER worker run wrote and run has
// not consumed yet (task spec step 4, FIX-FINAL receipt-replay-per-run),
// and consumes it for run (MarkToolCallReplayed, a compare-and-set:
// another replay of run that consumed it first sends the loop to the next
// candidate). hit=false, err=nil means there is nothing to replay - callers
// that need to distinguish "genuinely fresh" from "a rival attempt is
// currently live" do so themselves (claimIntent). A stored receipt that
// cannot be decoded is an error and is NOT consumed.
func (r *Receipts) checkReplay(ctx context.Context, in ExecuteInput, run int64) (result json.RawMessage, row sqlcgen.ToolCall, hit bool, err error) {
	for attempt := 0; attempt < replayConsumeMaxAttempts; attempt++ {
		existing, err := r.q.GetReplayableReceiptToolCall(ctx, sqlcgen.GetReplayableReceiptToolCallParams{
			TaskID: in.TaskID, ToolName: in.ToolName, ArgsHash: in.ArgsHash, CurrentRun: run,
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, sqlcgen.ToolCall{}, false, nil
			}
			return nil, sqlcgen.ToolCall{}, false, fmt.Errorf("task: replay lookup (task=%s tool=%s): %w", in.TaskID, in.ToolName, err)
		}
		var env receiptEnvelope
		if uerr := json.Unmarshal([]byte(existing.ReceiptJson.String), &env); uerr != nil {
			return nil, sqlcgen.ToolCall{}, false, fmt.Errorf("task: decode stored receipt (task=%s tool=%s): %w", in.TaskID, in.ToolName, uerr)
		}
		n, err := r.q.MarkToolCallReplayed(ctx, sqlcgen.MarkToolCallReplayedParams{CurrentRun: run, ID: existing.ID})
		if err != nil {
			return nil, sqlcgen.ToolCall{}, false, fmt.Errorf("task: consume replayed receipt (id=%d run=%d): %w", existing.ID, run, err)
		}
		if n == 1 {
			return env.Result, existing, true, nil
		}
	}
	return nil, sqlcgen.ToolCall{}, false, fmt.Errorf("%w (task=%s tool=%s run=%d)", ErrReplayConsumeContended, in.TaskID, in.ToolName, run)
}

// ledgerReplayed records a replay: seq/tool_call_id/receipt_run name the
// earlier run's receipt that answered, run the run it was replayed for.
func (r *Receipts) ledgerReplayed(ctx context.Context, in ExecuteInput, receipt sqlcgen.ToolCall, run int64) {
	r.ledgerRaw(ctx, in.TraceID, EventReplayed, map[string]any{
		"event": EventReplayed, "task_id": in.TaskID, "tool": in.ToolName, "args_hash": in.ArgsHash,
		"seq": receipt.Seq, "tool_call_id": receipt.ID, "receipt_run": receipt.Run, "run": run,
	})
}

// claimIntent inserts a fresh tool_calls 'intent' row for in's
// (task_id, tool_name, args_hash) triple, at the next seq for that key and
// under the call's worker run (task spec step 3).
//
// BLOCKER 1 fix: idx_tool_calls_live_unique (migrations/0008, narrowed to
// the in-flight statuses by 0026) makes a second IN-FLIGHT
// (intent/executing) row for the exact same key impossible at the DB level. Execute's in-process keyed mutex already
// rules out a SAME-process rival ever reaching this method concurrently,
// so a unique-constraint hit here can only mean a DIFFERENT process (or
// connection) currently holds the live row for this key. This retries
// with a short bounded backoff instead of failing outright: each
// iteration either (a) wins the insert outright, (b) discovers the rival
// was an EARLIER run's attempt that has since committed a receipt -
// returned directly (wasReplayed=true) for Execute to replay, exactly as if
// checkReplay had run one instant later - or (c) discovers the rival has
// since resolved (failed, or a receipt of the current run: an identical
// call executes again inside a live run), which frees the unique slot so
// the next iteration's insert succeeds. Returns ErrToolCallClaimTimeout if
// none of those resolve within claimIntentMaxAttempts.
func (r *Receipts) claimIntent(ctx context.Context, in ExecuteInput, run int64) (row sqlcgen.ToolCall, replayResult json.RawMessage, replayRow sqlcgen.ToolCall, wasReplayed bool, err error) {
	backoff := claimIntentInitialBackoff
	for attempt := 0; attempt < claimIntentMaxAttempts; attempt++ {
		seq, serr := r.q.NextToolCallSeq(ctx, sqlcgen.NextToolCallSeqParams{
			TaskID: in.TaskID, ToolName: in.ToolName, ArgsHash: in.ArgsHash,
		})
		if serr != nil {
			return sqlcgen.ToolCall{}, nil, sqlcgen.ToolCall{}, false, fmt.Errorf("task: next seq (task=%s tool=%s): %w", in.TaskID, in.ToolName, serr)
		}
		inserted, ierr := r.q.InsertToolCallIntent(ctx, sqlcgen.InsertToolCallIntentParams{
			TaskID: in.TaskID, Seq: seq, ToolName: in.ToolName, Class: string(in.Class),
			ArgsHash: in.ArgsHash, ApprovalTokenID: hashApprovalToken(in.ApprovalToken),
			CreatedAt: r.nowRFC3339(), Run: run,
		})
		if ierr == nil {
			return inserted, nil, sqlcgen.ToolCall{}, false, nil
		}
		if !isUniqueConstraintViolation(ierr) {
			return sqlcgen.ToolCall{}, nil, sqlcgen.ToolCall{}, false, fmt.Errorf("task: insert tool_calls intent (task=%s tool=%s): %w", in.TaskID, in.ToolName, ierr)
		}

		if hitResult, hitRow, hit, rerr := r.checkReplay(ctx, in, run); rerr != nil {
			return sqlcgen.ToolCall{}, nil, sqlcgen.ToolCall{}, false, rerr
		} else if hit {
			return sqlcgen.ToolCall{}, hitResult, hitRow, true, nil
		}

		select {
		case <-ctx.Done():
			return sqlcgen.ToolCall{}, nil, sqlcgen.ToolCall{}, false, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < claimIntentMaxBackoff {
			backoff *= 2
		}
	}
	return sqlcgen.ToolCall{}, nil, sqlcgen.ToolCall{}, false, fmt.Errorf("%w (task=%s tool=%s)", ErrToolCallClaimTimeout, in.TaskID, in.ToolName)
}

// failAndReturn marks the tool_calls row id 'failed' then returns cause -
// every Execute failure path from the moment the effect has run onward
// (BLOCKER 4 fix) goes through this exact function, so tool_calls.status
// can never again be left stranded at 'executing' with neither a receipt
// nor a 'failed' row (the resume scan's replay guard would otherwise wait
// on such a row forever - it never becomes receipt-having, and
// ListReceiptlessToolCalls would keep finding it, but nothing ever moves
// it out of 'executing' either). This is safe to call from every one of
// those paths specifically because, by the time failAndReturn runs, an
// ExecuteInTx effect's own transaction has already been rolled back (or
// never committed at all on a failed Commit - database/sql's own
// contract) - marking 'failed' here can never strand a phantom COMMITTED
// DB effect with no receipt, only ever a genuinely rolled-back one that a
// fresh retry can safely re-attempt from scratch. (An Execute effect's
// side effect lives outside brain.db and was never covered by a
// transaction - before or after FIX-I-12; ExecuteDetached, which every REAL
// tool uses since FIX-I-4, therefore never calls this after its effect ran:
// see ErrReceiptPersistFailed.) If marking failed ITSELF
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
