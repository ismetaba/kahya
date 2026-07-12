// Package taint implements the W4-03 session taint-tier ledger (HANDOFF
// §5 safety #2 flag, quoted verbatim): "taint katmani session_id
// anahtariyla SQLite'ta kalici saklanir, resume'da yeniden yuklenir ve
// yalniz yukselir - asla dusmez; kayit yoksa oturum guvenilmez sayilir
// (fail-closed)".
//
// Two tiers exist: TierClean and TierTainted. A session_taint row is born
// in EXACTLY TWO places (never a third, or the fail-closed default below
// would make every freshly-spawned session permanently untrusted, and no
// W-action could ever run):
//
//  1. kahyad/internal/server's OnSession callback, the moment kahyad
//     captures a worker-reported session_id for a USER-INITIATED task
//     (W4-02 step 5's own "session_started" persistence) - InsertClean is
//     called in the SAME database transaction as that persistence, via a
//     Tracker constructed over a transaction-scoped Store (see that
//     package's own doc comment for the exact wiring). A RESUMED task
//     never calls this a second time for the same session_id - the row
//     from its first spawn is the only one that will ever exist for it.
//  2. kahyad/internal/reader/actor_seed.go's Spawn: every time a Reader
//     episode's validated, schema-checked output seeds a brand-new Actor
//     session, that fresh session_id gets its own clean row here too.
//
// A Reader session's OWN session_id (the toolless session that parses
// untrusted bytes) is never born clean - kahyad/internal/reader.Run calls
// Raise on it immediately at spawn, since a Reader session exists
// specifically to eat untrusted content (HANDOFF's operational
// definition: "Okuyucu oturumlari spawn'da tainted satir alir").
//
// Get's fail-closed default (a session_id with NO row at all resolves to
// TierTainted, never an error the caller might mistake for "clean by
// default") is this package's single most important invariant - see its
// own doc comment.
package taint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mattn/go-sqlite3"

	"kahya/kahyad/internal/store/sqlcgen"
)

// Tier values session_taint.tier may hold (migrations/0009's CHECK
// constraint enforces the same two-value enum at the SQL level).
const (
	TierClean   = "clean"
	TierTainted = "tainted"
)

// Ledger event kinds this package appends (HANDOFF §5 safety #4 - every
// taint transition/rejected-lowering-attempt is durably auditable).
const (
	// EventRaised fires on every successful Raise call (task spec step 1,
	// verbatim: "Every Raise ledgers taint.raised (session_id, reason,
	// trace_id)").
	EventRaised = "taint.raised"
	// EventLowerAttempt fires whenever InsertClean's plain INSERT hits an
	// existing row (any tier) for sessionID - task spec step 1: "a
	// lowering attempt returns an error + ledgers taint.lower_attempt".
	EventLowerAttempt = "taint.lower_attempt"
)

// ErrLowerAttempt is returned by InsertClean when sessionID already has a
// session_taint row (clean or tainted) - there is no API in this package
// that can ever downgrade an existing row back to clean; a caller hitting
// this on a session_id it believed was brand-new has a bug worth
// surfacing, not something to swallow.
var ErrLowerAttempt = errors.New("taint: refusing to insert a clean row over an existing session_taint row")

// Store is the narrow session_taint persistence surface Tracker needs.
// *sqlcgen.Queries (via *store.Store, OR a *sql.Tx-scoped Queries via
// sqlcgen.New(tx)) satisfies this directly, with no adapter - the SAME
// "pass a tx-scoped Queries value to get transactional behavior" pattern
// kahyad/internal/task.Receipts.Execute already establishes (see that
// file's own EffectFunc doc comment). Constructing a fresh Tracker over a
// tx-scoped Store is exactly how kahyad/internal/server's OnSession
// callback inserts this task's session_taint(tier=clean) row in the SAME
// transaction as its own UpdateTaskSession call (task spec step 1a).
type Store interface {
	GetSessionTaint(ctx context.Context, sessionID string) (sqlcgen.SessionTaint, error)
	InsertSessionTaintClean(ctx context.Context, arg sqlcgen.InsertSessionTaintCleanParams) error
	RaiseSessionTaint(ctx context.Context, arg sqlcgen.RaiseSessionTaintParams) error
}

var _ Store = (*sqlcgen.Queries)(nil)

// Ledger is the append-only events sink this package writes to.
// *store.Store already has exactly this method shape (kahyad/internal/
// store.Store.LogEvent), so it satisfies this with no adapter - mirroring
// every other narrow Ledger interface in this codebase
// (kahyad/internal/policy.Ledger, kahyad/internal/task.Ledger, ...).
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Tracker is the W4-03 taint-tier decision/mutation surface: one per
// kahyad process for the ordinary (non-transactional) Store, or a
// throwaway value constructed over a *sql.Tx-scoped Store wherever a
// caller needs the clean-row insert to commit atomically with something
// else (see Store's own doc comment).
type Tracker struct {
	store  Store
	ledger Ledger
	// now is time.Now by default; tests substitute a fixed clock.
	now func() time.Time
}

// New constructs a Tracker. ledger may be nil (every Raise/InsertClean
// call still performs its DB write; the corresponding ledger event is
// simply skipped - matches this codebase's "unwired dependency" posture
// elsewhere, e.g. kahyad/internal/policy.Engine's own nil-ledger
// tolerance).
func New(store Store, ledger Ledger) *Tracker {
	return &Tracker{store: store, ledger: ledger, now: time.Now}
}

// SetClock overrides Tracker's clock (tests only).
func (t *Tracker) SetClock(now func() time.Time) { t.now = now }

func (t *Tracker) nowRFC3339() string { return t.now().UTC().Format(time.RFC3339Nano) }

// Get resolves sessionID's current tier. HANDOFF §5 safety #2's verbatim
// fail-closed rule: a session_id with NO row AT ALL resolves to
// TierTainted (err is nil in that case - the absence of a row is the
// DEFINED behavior, not a failure this package reports as one). Any OTHER
// read error (a genuine DB problem) ALSO resolves to TierTainted, with
// that error returned for the caller to log - this package's own posture
// mirrors kahyad/internal/policy.Engine's "any state-read error is itself
// a fail-closed DENY" convention: a taint check that could not be
// answered must never silently behave as if the session were clean.
func (t *Tracker) Get(ctx context.Context, sessionID string) (string, error) {
	row, err := t.store.GetSessionTaint(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TierTainted, nil
		}
		return TierTainted, fmt.Errorf("taint: get session_taint(%s): %w", sessionID, err)
	}
	return row.Tier, nil
}

// InsertClean inserts a brand-new session_taint row at tier=clean for
// sessionID - the ONLY way a clean row is ever created (see this
// package's doc comment for the exact two call sites). A plain INSERT: if
// sessionID already has ANY row (clean or tainted), the PRIMARY KEY
// conflict is treated as a lowering attempt - ledgered EventLowerAttempt
// and rejected with ErrLowerAttempt, never silently overwritten.
func (t *Tracker) InsertClean(ctx context.Context, traceID, sessionID string) error {
	err := t.store.InsertSessionTaintClean(ctx, sqlcgen.InsertSessionTaintCleanParams{
		SessionID: sessionID,
		UpdatedAt: t.nowRFC3339(),
	})
	if err == nil {
		return nil
	}
	if isUniqueConstraintViolation(err) {
		t.ledgerRaw(ctx, traceID, EventLowerAttempt, map[string]any{
			"event": EventLowerAttempt, "session_id": sessionID,
		})
		return fmt.Errorf("%w: session_id=%s", ErrLowerAttempt, sessionID)
	}
	return fmt.Errorf("taint: insert session_taint clean(%s): %w", sessionID, err)
}

// Raise upserts sessionID to tier=tainted (creating the row if it did not
// exist yet, or flipping an existing 'clean' row - an already-'tainted'
// row simply gets its reason/updated_at refreshed) and ledgers
// EventRaised. This is the ONLY tier-transition this package ever
// performs on an existing row - there is no statement anywhere in this
// package (or its generated SQL) that can ever lower tier back to clean.
func (t *Tracker) Raise(ctx context.Context, traceID, sessionID, reason string) error {
	if err := t.store.RaiseSessionTaint(ctx, sqlcgen.RaiseSessionTaintParams{
		SessionID: sessionID,
		Reason:    sql.NullString{String: reason, Valid: reason != ""},
		UpdatedAt: t.nowRFC3339(),
	}); err != nil {
		return fmt.Errorf("taint: raise session_taint(%s): %w", sessionID, err)
	}
	t.ledgerRaw(ctx, traceID, EventRaised, map[string]any{
		"event": EventRaised, "session_id": sessionID, "reason": reason,
	})
	return nil
}

func (t *Tracker) ledgerRaw(ctx context.Context, traceID, kind string, payload map[string]any) {
	if t.ledger == nil {
		return
	}
	_ = t.ledger.LogEvent(ctx, traceID, kind, payload)
}

// isUniqueConstraintViolation reports whether err is a SQLite unique (or
// primary-key) constraint violation - the mattn/go-sqlite3 shape
// InsertSessionTaintClean's hit against session_taint's PRIMARY KEY
// surfaces as. Mirrors kahyad/internal/task/receipts.go's identically-
// named helper (duplicated here rather than imported - that helper is
// unexported and this package must not depend on kahyad/internal/task).
func isUniqueConstraintViolation(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqlite3.ErrConstraint
	}
	return false
}
