// session_resolver.go implements the W4-03 BLOCKER 1+2 fix's production
// SessionResolver (engine.go's own doc comment on that interface): the
// tasks table is the ONLY place a request's session identity may
// legitimately come from, since kahyad itself is the one process that
// writes tasks.session_id, at session_started (W4-02 step 5's
// persistSessionStarted) - never the worker, and never anything decoded
// off the wire.
package policy

import (
	"context"
	"database/sql"
	"errors"

	"kahya/kahyad/internal/store/sqlcgen"
)

// sessionTaskStore is the narrow tasks-table read StoreSessionResolver
// needs. *sqlcgen.Queries (via *store.Store) satisfies this directly, with
// no adapter - the same "consumer defines the interface it needs"
// convention this package's own Store/Ledger/TaintChecker types already
// use.
type sessionTaskStore interface {
	GetTaskByID(ctx context.Context, id string) (sqlcgen.Task, error)
	GetTaskSessionByTrace(ctx context.Context, traceID string) (sql.NullString, error)
}

var _ sessionTaskStore = (*sqlcgen.Queries)(nil)

// StoreSessionResolver is the production SessionResolver: it resolves a
// request's session identity from the tasks table, preferring the exact
// primary-key lookup (task_id, when the caller has one - task.go's
// handlePolicyCheck always does) and falling back to the most-recently-
// updated task for the given trace_id otherwise (mcp.go's
// policyGateMiddleware, which has no reliable task_id of its own - see
// that file's own doc comment on why trace_id alone is sufficient there:
// kahyad/internal/spawn.spawn sets the worker's KAHYA_TRACE_ID env to this
// exact task's own trace_id, and kahya-mcp's bridge propagates that
// unchanged as the X-Kahya-Trace-Id header on every /v1/mcp POST).
type StoreSessionResolver struct {
	store sessionTaskStore
}

// NewStoreSessionResolver constructs a StoreSessionResolver over store
// (main.go passes st.Queries - the SAME *sqlcgen.Queries instance every
// other in-process caller reads/writes tasks through).
func NewStoreSessionResolver(store sessionTaskStore) *StoreSessionResolver {
	return &StoreSessionResolver{store: store}
}

// ResolveSession implements policy.SessionResolver. It tries taskID first
// (an exact primary-key lookup - the authoritative id when the caller has
// one), then falls back to traceID (GetTaskSessionByTrace's own "most
// recently updated wins" tie-break, matching GetTaskBySession's identical,
// pre-existing convention elsewhere in this schema). Any of "no matching
// row", "matching row but session_id is still NULL" (no session_started
// yet for this task), or both ids being empty resolve to ("", nil) -
// Check's own fail-closed contract (SessionResolver's doc comment) is what
// turns that into a DENY, not this method. Only a genuine, non-ErrNoRows
// read failure is returned as an error.
func (r *StoreSessionResolver) ResolveSession(ctx context.Context, traceID, taskID string) (string, error) {
	if taskID != "" {
		task, err := r.store.GetTaskByID(ctx, taskID)
		switch {
		case err == nil:
			if task.SessionID.Valid && task.SessionID.String != "" {
				return task.SessionID.String, nil
			}
			// Row matched but no session recorded yet (or an empty
			// string, which is not a real session): fall through to
			// traceID below rather than returning "" immediately - a
			// caller MAY still resolve via trace_id (e.g. a retry that
			// shares the original trace_id but minted a fresh task_id
			// with no session of its own yet).
		case errors.Is(err, sql.ErrNoRows):
			// No task row at all for this task_id - fall through to
			// traceID below.
		default:
			return "", err
		}
	}

	if traceID == "" {
		return "", nil
	}
	sessionID, err := r.store.GetTaskSessionByTrace(ctx, traceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if !sessionID.Valid {
		return "", nil
	}
	return sessionID.String, nil
}
