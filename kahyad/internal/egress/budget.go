// budget.go implements the W3-05 per-host daily byte-budget counter
// (HANDOFF §5 safety #1: "hacim butcesine tabi"), persisted in the
// egress_budget(host, day, bytes) table (migrations/
// 0004_egress_budget.sql) so a kahyad restart never resets a host's
// counter mid-day.
//
// "day" is ALWAYS the Mac's LOCAL wall-clock day —
// time.Now().Format("2006-01-02") (Gate.now, overridable via SetClock) —
// deliberately NOT UTC: an operator reasons about "today's egress
// budget" in their own local day, and a bare UTC boundary would roll a
// budget over at an arbitrary, confusing local wall-clock moment (e.g.
// 03:00 in Istanbul, DST-dependent). This mirrors the same
// time.Now()-based, monotonic-clock-avoiding convention HANDOFF §4's
// stack row already uses for launchd's StartCalendarInterval wall-clock
// jobs, for the same reason: a human-meaningful calendar boundary, not a
// machine-internal one.
package egress

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"kahya/kahyad/internal/store/sqlcgen"
)

// Store is the narrow persistence surface SQLBudget needs. *sqlcgen.Queries
// (via *store.Store) satisfies it directly, with no adapter — the same
// pattern kahyad/internal/policy.Store already uses for autonomy_state.
type Store interface {
	GetEgressBudget(ctx context.Context, arg sqlcgen.GetEgressBudgetParams) (sqlcgen.EgressBudget, error)
	InsertEgressBudget(ctx context.Context, arg sqlcgen.InsertEgressBudgetParams) error
	IncrementEgressBudget(ctx context.Context, arg sqlcgen.IncrementEgressBudgetParams) (int64, error)
}

var _ Store = (*sqlcgen.Queries)(nil)

// SQLBudget is the production Budget implementation (Gate.budget),
// backed by the egress_budget table.
type SQLBudget struct {
	store Store
}

// NewSQLBudget constructs a SQLBudget. store is typically *store.Store's
// generated Queries (kahyad's single sqlc query surface — *store.Store
// itself does not satisfy Store directly, only its embedded Queries
// field does, matching every other kahyad/internal package's convention
// of taking *sqlcgen.Queries, not *store.Store, as the persistence
// dependency).
func NewSQLBudget(store Store) *SQLBudget {
	return &SQLBudget{store: store}
}

// Bytes implements Budget: 0 (no error) when no row exists yet for
// host/day — a host that has never sent a byte today has, definitionally,
// used none of its budget.
func (b *SQLBudget) Bytes(ctx context.Context, host, day string) (int64, error) {
	row, err := b.store.GetEgressBudget(ctx, sqlcgen.GetEgressBudgetParams{Host: host, Day: day})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("egress: get budget %s/%s: %w", host, day, err)
	}
	return row.Bytes, nil
}

// Add implements Budget: increments (host, day)'s counter by n, via an
// application-level upsert (the same "UPDATE, fall back to INSERT on 0
// rows affected" pattern kahyad/internal/policy.Engine already uses for
// autonomy_state/UpdateAutonomyState), and returns the new total.
func (b *SQLBudget) Add(ctx context.Context, host, day string, n int64) (int64, error) {
	rows, err := b.store.IncrementEgressBudget(ctx, sqlcgen.IncrementEgressBudgetParams{Bytes: n, Host: host, Day: day})
	if err != nil {
		return 0, fmt.Errorf("egress: increment budget %s/%s: %w", host, day, err)
	}
	if rows == 0 {
		if err := b.store.InsertEgressBudget(ctx, sqlcgen.InsertEgressBudgetParams{Host: host, Day: day, Bytes: n}); err != nil {
			return 0, fmt.Errorf("egress: insert budget %s/%s: %w", host, day, err)
		}
		return n, nil
	}
	return b.Bytes(ctx, host, day)
}
