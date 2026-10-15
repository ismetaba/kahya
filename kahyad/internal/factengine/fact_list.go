// fact_list.go is the human review surface of the fact layer (G13 MEM-5):
// `kahya fact list [--quarantined] [--entity <ad|id>] [--since RFC3339]`
// over GET /v1/fact/list, and the id sets the bulk `kahya fact confirm`
// forms resolve (--episode, --all-quarantined --since). Read-only; the
// writes stay ConfirmFact / DenyFact, one id at a time, each ledgered. The
// output is a LOCAL surface (the CLI), never Telegram or a model prompt, so
// quarantined and secret-lane facts are listed too, labelled.
package factengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// Fact list bounds. MaxFactBatch is also the most ids one bulk confirm or
// deny call may touch (HANDOFF §5 memory #1: a human gate, not a firehose -
// a wider sweep is a narrower --since away).
const (
	DefaultFactListLimit = 50
	MaxFactBatch         = 500
	// factListScanCap bounds the rows an entity-filtered list scans (the
	// entity filter runs in Go over the newest rows).
	factListScanCap = 5000
)

// ErrFactNotFound is returned by ConfirmFact and DenyFact for an id that
// names no fact.
var ErrFactNotFound = errors.New("factengine: fact not found")

// FactQuarantined reports whether f waits for a human confirmation before
// any reflex injection (HANDOFF §5 memory #1, safety #3): its tier is one
// TierInjectionEligible quarantines and nobody confirmed it. The log-odds
// half of InjectionEligible is NOT part of it - a denied fact is excluded
// by its evidence, not waiting for a confirmation.
func FactQuarantined(f sqlcgen.Fact) bool {
	return !TierInjectionEligible(f.SourceTier, f.ConfirmedAt.Valid)
}

// FactListQuery is one `kahya fact list` read. Zero values mean "any".
type FactListQuery struct {
	// QuarantinedOnly keeps only the facts FactQuarantined reports.
	QuarantinedOnly bool
	// CreatedSince keeps facts created at or after it (second precision,
	// inclusive).
	CreatedSince time.Time
	// MaxID > 0 keeps only facts whose id is at most MaxID (QuarantinedOnly
	// only): fact ids only grow, so a confirm bounded by the largest id a
	// dry run listed can only touch a subset of that listing (FIX-FINAL
	// TUX-2).
	MaxID int64
	// EntityID / EntityName keep the facts linked to that entity as subject
	// or object; a name matches every active entity whose folded alias key
	// equals AliasKey(EntityName) (no such entity = no facts).
	EntityID   int64
	EntityName string
	// Limit caps the result (<= 0 = DefaultFactListLimit, at most
	// MaxFactBatch).
	Limit int
}

// FactListItem is one `kahya fact list` row. EvidencePath is the first
// memory note an evidence row cites ("" when none does).
type FactListItem struct {
	FactID        int64   `json:"fact_id"`
	Subject       string  `json:"subject"`
	Predicate     string  `json:"predicate"`
	Object        string  `json:"object"`
	SourceTier    string  `json:"source_tier"`
	Evidentiality string  `json:"evidentiality"`
	ConfidenceP   float64 `json:"confidence_p"`
	Quarantined   bool    `json:"quarantined"`
	CreatedAt     string  `json:"created_at"`
	EvidencePath  string  `json:"evidence_path"`
}

// createdSinceBound is the text bound the list queries compare created_at
// against: a UTC "YYYY-MM-DDTHH:MM:SS" prefix orders correctly against the
// stored RFC3339Nano strings (a stored value in the same second is longer,
// so it compares greater - inclusive), "" for no bound.
func createdSinceBound(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05")
}

// ListFacts answers a fact list read, newest first. truncated reports that
// more matching facts exist beyond the limit.
func (e *Engine) ListFacts(ctx context.Context, q FactListQuery) (items []FactListItem, truncated bool, err error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultFactListLimit
	}
	if limit > MaxFactBatch {
		limit = MaxFactBatch
	}
	facts, truncated, err := e.listFactRows(ctx, q, limit)
	if err != nil {
		return nil, false, err
	}
	items = make([]FactListItem, 0, len(facts))
	for _, f := range facts {
		path, err := e.firstEvidencePath(ctx, f.ID)
		if err != nil {
			return nil, false, err
		}
		items = append(items, FactListItem{
			FactID: f.ID, Subject: f.Subject, Predicate: f.Predicate, Object: f.Object,
			SourceTier: f.SourceTier, Evidentiality: f.Evidentiality,
			ConfidenceP: LogOddsToProbability(f.Confidence), Quarantined: FactQuarantined(f),
			CreatedAt: f.CreatedAt, EvidencePath: path,
		})
	}
	return items, truncated, nil
}

// QuarantinedFactIDsSince is the id set of `kahya fact confirm
// --all-quarantined --since <t>`: every quarantined fact created at or
// after since. More than MaxFactBatch is ErrFactBatchTooLarge - nothing is
// returned, so a caller never confirms a silently cut set.
func (e *Engine) QuarantinedFactIDsSince(ctx context.Context, since time.Time) ([]int64, error) {
	return e.QuarantinedFactIDsSinceThrough(ctx, since, 0)
}

// QuarantinedFactIDsSinceThrough is QuarantinedFactIDsSince bounded above
// by maxID (0 = no bound): `kahya fact confirm --all-quarantined --since S
// --upto N --yes` confirms only facts the dry run listed (FIX-FINAL TUX-2).
func (e *Engine) QuarantinedFactIDsSinceThrough(ctx context.Context, since time.Time, maxID int64) ([]int64, error) {
	facts, truncated, err := e.listFactRows(ctx, FactListQuery{QuarantinedOnly: true, CreatedSince: since, MaxID: maxID}, MaxFactBatch)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, ErrFactBatchTooLarge
	}
	ids := make([]int64, 0, len(facts))
	for _, f := range facts {
		ids = append(ids, f.ID)
	}
	return ids, nil
}

// FactIDsByEpisode is the id set of `kahya fact confirm --episode <id>`:
// every active fact an evidence row of that episode cites, lowest id first.
// More than MaxFactBatch is ErrFactBatchTooLarge.
func (e *Engine) FactIDsByEpisode(ctx context.Context, episodeID int64) ([]int64, error) {
	ids, err := e.store.ListFactIDsByEpisode(ctx, sql.NullInt64{Int64: episodeID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("factengine: facts of episode %d: %w", episodeID, err)
	}
	if len(ids) > MaxFactBatch {
		return nil, ErrFactBatchTooLarge
	}
	slices.Sort(ids)
	return ids, nil
}

// ErrFactBatchTooLarge is returned when a bulk selection names more than
// MaxFactBatch facts. Its text is the Turkish answer the route returns.
var ErrFactBatchTooLarge = fmt.Errorf("en fazla %d olgu", MaxFactBatch)

// listFactRows reads at most limit matching facts (newest first) and
// whether more exist.
func (e *Engine) listFactRows(ctx context.Context, q FactListQuery, limit int) ([]sqlcgen.Fact, bool, error) {
	entityIDs, filterEntity, err := e.listEntityFilter(ctx, q)
	if err != nil {
		return nil, false, err
	}
	if filterEntity && len(entityIDs) == 0 {
		return []sqlcgen.Fact{}, false, nil
	}
	read := int64(limit + 1)
	if filterEntity {
		read = factListScanCap
	}
	bound := createdSinceBound(q.CreatedSince)
	var rows []sqlcgen.Fact
	if q.QuarantinedOnly {
		rows, err = e.store.ListQuarantinedFacts(ctx, sqlcgen.ListQuarantinedFactsParams{CreatedSince: bound, MaxID: q.MaxID, RowLimit: read})
	} else {
		rows, err = e.store.ListActiveFactsCreatedSince(ctx, sqlcgen.ListActiveFactsCreatedSinceParams{CreatedSince: bound, RowLimit: read})
	}
	if err != nil {
		return nil, false, fmt.Errorf("factengine: list facts: %w", err)
	}
	out := make([]sqlcgen.Fact, 0, min(len(rows), limit+1))
	for _, f := range rows {
		if q.QuarantinedOnly && !FactQuarantined(f) {
			continue // the SQL tier filter and the Go predicate must agree; never list more than the predicate says
		}
		if filterEntity && !linksAny(f, entityIDs) {
			continue
		}
		out = append(out, f)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// listEntityFilter resolves q's entity filter: the ids (EntityID, or every
// active entity EntityName's folded alias key names - the local view, every
// alias counts: this is the user's own surface) and whether a filter applies.
func (e *Engine) listEntityFilter(ctx context.Context, q FactListQuery) (map[int64]bool, bool, error) {
	if q.EntityID == 0 && q.EntityName == "" {
		return nil, false, nil
	}
	ids := map[int64]bool{}
	if q.EntityName != "" {
		found, err := e.store.ListActiveEntityIDsByAliasFolded(ctx, sql.NullString{String: AliasKey(q.EntityName), Valid: true})
		if err != nil {
			return nil, false, fmt.Errorf("factengine: resolve entity %q: %w", q.EntityName, err)
		}
		for _, id := range found {
			if q.EntityID == 0 || id == q.EntityID {
				ids[id] = true
			}
		}
		return ids, true, nil
	}
	ids[q.EntityID] = true
	return ids, true, nil
}

func linksAny(f sqlcgen.Fact, ids map[int64]bool) bool {
	return (f.SubjectEntityID.Valid && ids[f.SubjectEntityID.Int64]) || (f.ObjectEntityID.Valid && ids[f.ObjectEntityID.Int64])
}

// firstEvidencePath is the alphabetically first memory note an evidence row
// of factID cites ("" when none does - e.g. a content-secret extraction
// that writes its evidence without the episode citation).
func (e *Engine) firstEvidencePath(ctx context.Context, factID int64) (string, error) {
	paths, err := e.store.ListEpisodePathsForFact(ctx, factID)
	if err != nil {
		return "", fmt.Errorf("factengine: evidence paths of fact %d: %w", factID, err)
	}
	first := ""
	for _, p := range paths {
		if !p.Valid || p.String == "" {
			continue
		}
		if first == "" || p.String < first {
			first = p.String
		}
	}
	return first, nil
}

// requireFact returns factID's row, or ErrFactNotFound.
func (e *Engine) requireFact(ctx context.Context, factID int64) (sqlcgen.Fact, error) {
	f, err := e.store.GetFact(ctx, factID)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlcgen.Fact{}, fmt.Errorf("%w: %d", ErrFactNotFound, factID)
	}
	if err != nil {
		return sqlcgen.Fact{}, fmt.Errorf("factengine: fact %d: %w", factID, err)
	}
	return f, nil
}
