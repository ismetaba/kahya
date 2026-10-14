// sensitive.go implements the W3-05 per-session sensitive-read taint
// flag (HANDOFF §5 safety #1: "Ayni oturumda hassas okuma varsa
// allowlist-disi egress sert bloke"). This is the SAME "taint only rises,
// never falls" direction as §5 safety #2's untrusted-content taint.
//
// Durable since FIX-B-7 (review finding B-7): the production tracker
// (NewDurableSensitiveTracker) writes every mark through to brain.db's
// sensitive_reads table (migrations/0019_sensitive_reads.sql) and reads a
// cache miss back from it, so a kahyad restart followed by the resume
// scan - which keeps the task's trace_id, the session key - still sees
// the mark. W3-05's original "in memory only, persistence is W4-03"
// deferral is historical: W4-03 never added it.
//
// mcp/fs's fs_read handler (the secret_lane_read seam W3-03 left) marks a
// session sensitive the moment it reads a path matching
// policy.yaml's secret_lane_globs — see kahyad/internal/server/egress.go's
// SensitiveReadMarker adapter, which is what actually wires fs_read to
// this tracker (via POST /session/sensitive-read for any future
// out-of-process caller, or directly in-process today, exactly like
// mcp/fs.PolicyClient's own "in-process today, real HTTP later" seam).
package egress

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// SensitiveStore is the narrow persistence surface SensitiveTracker needs
// (the sensitive_reads table, migrations/0019). *sqlcgen.Queries satisfies
// it directly - the same pattern budget.go's Store uses for egress_budget.
type SensitiveStore interface {
	InsertSensitiveRead(ctx context.Context, arg sqlcgen.InsertSensitiveReadParams) error
	GetSensitiveRead(ctx context.Context, sessionKey string) (sqlcgen.SensitiveRead, error)
	CountSensitiveReads(ctx context.Context) (int64, error)
}

var _ SensitiveStore = (*sqlcgen.Queries)(nil)

// ReasonSecretLaneRead is the reason column value Gate.MarkSensitiveRead
// writes (the only production reason today).
const ReasonSecretLaneRead = "secret_lane_read"

// SensitiveTracker is the per-session "has this session done a
// secret-lane read" flag: an in-memory cache in front of an OPTIONAL
// durable store. Rises only, never clears. Safe for concurrent use.
type SensitiveTracker struct {
	mu     sync.Mutex
	marked map[string]bool
	store  SensitiveStore // nil => in-memory only (tests, red-team harness)
	now    func() time.Time
}

// NewSensitiveTracker constructs an in-memory-only tracker (tests and
// kahyad/internal/eval's red-team harness). Production uses
// NewDurableSensitiveTracker.
func NewSensitiveTracker() *SensitiveTracker {
	return &SensitiveTracker{marked: make(map[string]bool), now: time.Now}
}

// NewDurableSensitiveTracker constructs the production tracker: every
// MarkCtx is written through to store BEFORE it is cached, and every
// IsMarkedCtx miss is read through from store (FIX-B-7: survives a kahyad
// restart + resume, which keeps the task's trace_id).
func NewDurableSensitiveTracker(store SensitiveStore) *SensitiveTracker {
	t := NewSensitiveTracker()
	t.store = store
	return t
}

// Mark flags sessionID sensitive IN MEMORY ONLY (kept for tests and for
// MarkCtx's cache step; production callers use MarkCtx). Rises only —
// calling Mark on an already-marked session is a no-op that returns
// changed=false. An empty sessionID is a deliberate no-op (there is no
// session to attribute the read to) and always reports changed=false.
func (t *SensitiveTracker) Mark(sessionID string) (changed bool) {
	if sessionID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.marked[sessionID] {
		return false
	}
	t.marked[sessionID] = true
	return true
}

// MarkCtx persists the mark FIRST (when a store is wired), then caches
// it. A persistence error is returned WITHOUT caching, so the caller
// (Gate.MarkSensitiveRead -> mcp/fs HandleRead) fails closed: no
// secret-lane bytes are released on the strength of a mark that may not
// survive the process. changed reports only whether the in-memory cache
// changed - a fresh tracker after a restart reports true for a session
// whose row already exists (the INSERT is then a DO NOTHING no-op).
func (t *SensitiveTracker) MarkCtx(ctx context.Context, sessionID, traceID, reason string) (changed bool, err error) {
	if sessionID == "" {
		return false, nil
	}
	if t.store != nil {
		if err := t.store.InsertSensitiveRead(ctx, sqlcgen.InsertSensitiveReadParams{
			SessionKey: sessionID,
			TraceID:    traceID,
			Reason:     sql.NullString{String: reason, Valid: reason != ""},
			MarkedAt:   t.now().UTC().Format(time.RFC3339Nano),
		}); err != nil {
			return false, fmt.Errorf("egress: persist sensitive read for %s: %w", sessionID, err)
		}
	}
	return t.Mark(sessionID), nil
}

// IsMarked reports the IN-MEMORY flag only (tests). Production decisions
// go through IsMarkedCtx. An empty sessionID is always unmarked.
func (t *SensitiveTracker) IsMarked(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.marked[sessionID]
}

// IsMarkedCtx answers memory first, then the store (caching a hit).
// sql.ErrNoRows => false. ANY other store error => (true, err): a taint
// check that cannot be answered must never behave as "clean"
// (tasks/README.md fail-closed convention; mirrors taint.Tracker.Get).
// A miss is deliberately NOT cached: the next Check asks the store again
// (one primary-key SELECT).
func (t *SensitiveTracker) IsMarkedCtx(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	if t.IsMarked(sessionID) {
		return true, nil
	}
	if t.store == nil {
		return false, nil
	}
	_, err := t.store.GetSensitiveRead(ctx, sessionID)
	switch {
	case err == nil:
		t.Mark(sessionID)
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return true, fmt.Errorf("egress: lookup sensitive read for %s: %w", sessionID, err)
	}
}

// Count reports the number of persisted marks (boot-time observability
// only; 0 with no store).
func (t *SensitiveTracker) Count(ctx context.Context) (int64, error) {
	if t.store == nil {
		return 0, nil
	}
	return t.store.CountSensitiveReads(ctx)
}
