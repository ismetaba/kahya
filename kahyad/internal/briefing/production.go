// production.go collects the small production adapters that wrap other
// kahyad packages' real implementations into this package's own narrow
// interfaces (Classifier/GlobMatcher/TaintWriter/TaskStore/Delivery/
// Ledger/DedupeChecker) - kept separate from briefing.go/gate.go/worker.go
// so those files never need to import policy/store/sqlcgen/taint directly
// for anything beyond what a hermetic test also needs.
package briefing

import (
	"context"
	"time"

	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store/sqlcgen"
)

// PolicyGlobMatcher is the production GlobMatcher: policy.yaml's
// secret_lane_globs (already `~`-expanded by policy.Load - HANDOFF §4 ⚑
// "policy.yaml globlari yalniz dosya yollari icin"), matched via
// policy.MatchGlob - the SAME doublestar matcher every other glob check in
// this codebase uses, never a second copy.
type PolicyGlobMatcher struct {
	Globs []string
}

// MatchesSecretLane implements GlobMatcher.
func (m PolicyGlobMatcher) MatchesSecretLane(path string) bool {
	for _, g := range m.Globs {
		if ok, err := policy.MatchGlob(g, path); err == nil && ok {
			return true
		}
	}
	return false
}

// eventCounter is the narrow sqlc surface StoreDedupeChecker needs.
// *sqlcgen.Queries (via *store.Store) satisfies this directly.
type eventCounter interface {
	CountEventsByKindBetween(ctx context.Context, arg sqlcgen.CountEventsByKindBetweenParams) (int64, error)
}

// StoreDedupeChecker is the production DedupeChecker: the once-per-day
// idempotency check reads directly off the append-only events ledger
// (kahyad is its only writer) rather than a second, independently-
// maintained "did we already run today" flag - a missed-run-fired-on-wake
// plus the regular scheduled run both consult the SAME ledger truth.
type StoreDedupeChecker struct {
	Store eventCounter
}

// AlreadyDelivered implements DedupeChecker: true iff a briefing.delivered
// row exists with created_at in [dayStart, dayEnd) - the LOCAL calendar
// day Run computed from Orchestrator.Location (FIX-P-14). The bounds go to
// SQL as UTC RFC3339Nano instants, the same form created_at is stored in.
func (d StoreDedupeChecker) AlreadyDelivered(ctx context.Context, dayStart, dayEnd time.Time) (bool, error) {
	n, err := d.Store.CountEventsByKindBetween(ctx, sqlcgen.CountEventsByKindBetweenParams{
		Kind:    EventDelivered,
		StartTs: dayStart.UTC().Format(time.RFC3339Nano),
		EndTs:   dayEnd.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
