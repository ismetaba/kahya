package graph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/textnorm"
)

// Structured reads (G13 MEM-4): fact_query and entity_lookup, and the
// local `kahya fact query`. Read-only, like the walk; the same trust and
// lane rules apply (see the package doc).

// Bounds of the structured reads.
const (
	// DefaultFactQueryLimit / MaxFactQueryLimit bound QueryFacts' result.
	DefaultFactQueryLimit = 20
	MaxFactQueryLimit     = 100
	// factQueryScanCap bounds the SQL candidate set the Go-side filters
	// (time windows, tier, trust, lane) run over.
	factQueryScanCap = 5000
	// DefaultEntityLookupLimit / MaxEntityLookupLimit bound LookupEntities.
	DefaultEntityLookupLimit = 10
	MaxEntityLookupLimit     = 50
	// entityLookupFactLimit is how many facts of one entity LookupEntities
	// reads (for its visible names, counts and top facts).
	entityLookupFactLimit = 100
	// entityTopFacts is EntityRecord.TopFacts' length cap.
	entityTopFacts = 5
)

// ErrUnknownTier is returned for a MinTier outside the trust lattice.
var ErrUnknownTier = errors.New("graph: min_tier must be one of user_edit, user_asserted, external_doc, screen, agent_derived")

// FactQuery is one structured fact read. Every zero value means "any".
type FactQuery struct {
	// Subject/Object match byte-exact first; when that finds nothing, the
	// textnorm.Fold-equal strings (Turkish I/i, case) match instead.
	Subject, Predicate, Object string
	// EntityID keeps facts touching that entity as subject or object.
	EntityID int64
	// EntityName (the local CLI's --entity <ad>) keeps facts touching any
	// active entity whose folded alias key equals AliasKey(EntityName);
	// no such entity = no facts.
	EntityName string
	// Since/Until bound valid_from (Since inclusive, Until exclusive; a
	// fact with no valid_from never matches a bound).
	Since, Until time.Time
	// AsOf keeps the facts VALID at that instant: valid_from <= as_of (or
	// unknown) and valid_to > as_of (or unknown).
	AsOf time.Time
	// KnownAsOf keeps the facts already KNOWN then: created_at <=
	// known_as_of (transaction time).
	KnownAsOf time.Time
	// MinTier is the trust-lattice floor (factengine.TierRank).
	MinTier string
	// IncludeQuarantined also returns the facts factengine.
	// InjectionEligible would hide, labelled Quarantined.
	IncludeQuarantined bool
	// Limit caps the result (<= 0 = DefaultFactQueryLimit, at most
	// MaxFactQueryLimit).
	Limit int
	// CloudLane: the answer goes to a cloud model (a worker's tool call),
	// so every fact must pass memlane.CloudEligible.
	CloudLane bool
}

// FactRecord is one structured-read fact.
type FactRecord struct {
	FactID          int64
	Subject         string
	Predicate       string
	Object          string
	SourceTier      string
	Evidentiality   string
	ConfidenceP     float64
	Quarantined     bool
	ValidFrom       string
	ValidTo         string
	KnownSince      string
	Quote           string
	Evidence        string
	SubjectEntityID int64
	ObjectEntityID  int64
}

// EntityRecord is one entity_lookup result.
type EntityRecord struct {
	EntityID      int64
	CanonicalName string
	Kind          string
	Provisional   bool
	Aliases       []string
	FactCount     int64
	TopFacts      []FactRecord
}

// QueryFacts answers a structured fact read: active facts matching the
// equality filters (SQL), then the time windows, tier floor, trust filter
// (InjectionEligible unless IncludeQuarantined) and, with CloudLane, the
// lane filter (Go), ordered by confidence desc, updated_at desc, id asc and
// cut at Limit. The ordering and filters are deterministic.
func (g *Graph) QueryFacts(ctx context.Context, traceID string, q FactQuery) ([]FactRecord, error) {
	if g == nil || g.DB == nil || g.Store == nil {
		return nil, errors.New("graph: no store wired")
	}
	if q.MinTier != "" {
		if _, ok := factengine.TierRank(q.MinTier); !ok {
			return nil, fmt.Errorf("%w (got %q)", ErrUnknownTier, q.MinTier)
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultFactQueryLimit
	}
	if limit > MaxFactQueryLimit {
		limit = MaxFactQueryLimit
	}

	entityIDs := []int64{q.EntityID}
	if q.EntityName != "" {
		ids, err := g.Store.ListActiveEntityIDsByAliasFolded(ctx, sql.NullString{String: factengine.AliasKey(q.EntityName), Valid: true})
		if err != nil {
			return nil, fmt.Errorf("graph: resolve entity %q: %w", q.EntityName, err)
		}
		if q.EntityID != 0 {
			ids = intersect(ids, q.EntityID)
		}
		if len(ids) == 0 {
			return []FactRecord{}, nil
		}
		entityIDs = ids
	}

	w := newWalk(ctx, g, Options{CloudLane: q.CloudLane}.normalized())
	exact := q.Subject != "" || q.Object != ""
	out, err := g.queryFactsPass(w, q, entityIDs, exact, limit)
	if err != nil {
		return nil, err
	}
	if exact && len(out) == 0 {
		// Second pass: the folded subject/object strings.
		if out, err = g.queryFactsPass(w, q, entityIDs, false, limit); err != nil {
			return nil, err
		}
	}
	if g.Log != nil {
		g.Log.With(traceID).Info("graph_fact_query", "results", len(out), "cloud_lane", q.CloudLane,
			"include_quarantined", q.IncludeQuarantined, "entity_filter", q.EntityID != 0 || q.EntityName != "")
	}
	return out, nil
}

// queryFactsPass runs the SQL for each entity id (0 = no entity filter),
// with subject/object as byte-exact SQL equality (exact) or as a Go-side
// folded equality (!exact), and applies the Go filters.
func (g *Graph) queryFactsPass(w *walk, q FactQuery, entityIDs []int64, exact bool, limit int) ([]FactRecord, error) {
	seen := map[int64]bool{}
	var cands []sqlcgen.Fact
	for _, e := range entityIDs {
		subj, obj := "", ""
		if exact {
			subj, obj = q.Subject, q.Object
		}
		rows, err := g.DB.QueryContext(w.ctx, `
			SELECT id, subject, predicate, object, source_tier, evidentiality, confidence, importance,
			       valid_from, valid_to, status, evidence, extractor_ver, updated_at, created_at,
			       confirmed_at, subject_entity_id, object_entity_id, quote
			FROM facts
			WHERE status = 'active'
			  AND (? = '' OR subject = ?) AND (? = '' OR predicate = ?) AND (? = '' OR object = ?)
			  AND (? = 0 OR subject_entity_id = ? OR object_entity_id = ?)
			ORDER BY confidence DESC, updated_at DESC, id ASC
			LIMIT ?`,
			subj, subj, q.Predicate, q.Predicate, obj, obj, e, e, e, factQueryScanCap)
		if err != nil {
			return nil, fmt.Errorf("graph: fact query: %w", err)
		}
		for rows.Next() {
			var f sqlcgen.Fact
			if err := rows.Scan(&f.ID, &f.Subject, &f.Predicate, &f.Object, &f.SourceTier, &f.Evidentiality, &f.Confidence, &f.Importance,
				&f.ValidFrom, &f.ValidTo, &f.Status, &f.Evidence, &f.ExtractorVer, &f.UpdatedAt, &f.CreatedAt,
				&f.ConfirmedAt, &f.SubjectEntityID, &f.ObjectEntityID, &f.Quote); err != nil {
				rows.Close()
				return nil, fmt.Errorf("graph: fact query scan: %w", err)
			}
			if !seen[f.ID] {
				seen[f.ID] = true
				cands = append(cands, f)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("graph: fact query rows: %w", err)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		if a.UpdatedAt != b.UpdatedAt {
			return a.UpdatedAt > b.UpdatedAt
		}
		return a.ID < b.ID
	})

	out := []FactRecord{}
	for _, f := range cands {
		if len(out) == limit {
			break
		}
		if !exact && !foldedMatch(q, f) {
			continue
		}
		if !q.timeMatch(f) || !q.tierMatch(f) {
			continue
		}
		eligible := factengine.InjectionEligible(f)
		if !eligible && !q.IncludeQuarantined {
			continue
		}
		ok, err := w.laneOK(f)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, toRecord(f, !eligible))
	}
	return out, nil
}

func foldedMatch(q FactQuery, f sqlcgen.Fact) bool {
	if q.Subject != "" && textnorm.Fold(f.Subject) != textnorm.Fold(q.Subject) {
		return false
	}
	return q.Object == "" || textnorm.Fold(f.Object) == textnorm.Fold(q.Object)
}

// timeMatch applies the four bitemporal bounds; a stored timestamp that
// does not parse never satisfies a bound (fail-closed for a filter).
func (q FactQuery) timeMatch(f sqlcgen.Fact) bool {
	from, fromOK := parseStored(f.ValidFrom)
	if !q.Since.IsZero() && (!fromOK || from.Before(q.Since)) {
		return false
	}
	if !q.Until.IsZero() && (!fromOK || !from.Before(q.Until)) {
		return false
	}
	if !q.AsOf.IsZero() {
		if f.ValidFrom.Valid && f.ValidFrom.String != "" && (!fromOK || from.After(q.AsOf)) {
			return false
		}
		if f.ValidTo.Valid && f.ValidTo.String != "" {
			to, ok := parseStored(f.ValidTo)
			if !ok || !to.After(q.AsOf) {
				return false
			}
		}
	}
	if !q.KnownAsOf.IsZero() {
		known, err := time.Parse(time.RFC3339Nano, f.CreatedAt)
		if err != nil || known.After(q.KnownAsOf) {
			return false
		}
	}
	return true
}

func (q FactQuery) tierMatch(f sqlcgen.Fact) bool {
	if q.MinTier == "" {
		return true
	}
	floor, _ := factengine.TierRank(q.MinTier)
	r, ok := factengine.TierRank(f.SourceTier)
	return ok && r >= floor
}

func parseStored(s sql.NullString) (time.Time, bool) {
	if !s.Valid || s.String == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	return t, err == nil
}

func toRecord(f sqlcgen.Fact, quarantined bool) FactRecord {
	return FactRecord{
		FactID: f.ID, Subject: f.Subject, Predicate: f.Predicate, Object: f.Object,
		SourceTier: f.SourceTier, Evidentiality: f.Evidentiality,
		ConfidenceP: factengine.LogOddsToProbability(f.Confidence),
		Quarantined: quarantined,
		ValidFrom:   f.ValidFrom.String, ValidTo: f.ValidTo.String, KnownSince: f.CreatedAt,
		Quote: f.Quote.String, Evidence: f.Evidence.String,
		SubjectEntityID: f.SubjectEntityID.Int64, ObjectEntityID: f.ObjectEntityID.Int64,
	}
}

func intersect(ids []int64, want int64) []int64 {
	for _, id := range ids {
		if id == want {
			return []int64{want}
		}
	}
	return nil
}

// LookupEntities answers entity_lookup: active entities with a name whose
// folded key starts with AliasKey(name), lowest id first, at most k (<= 0 =
// DefaultEntityLookupLimit, at most MaxEntityLookupLimit), each with its
// aliases, its active-fact count and up to 5 injection-eligible facts.
//
// cloudLane (a worker's tool call): only VISIBLE names count - the folded
// subject/object string, on the entity's own side, of a cloud-eligible fact
// (see the package doc). An entity is returned only when one of its visible
// names matches the prefix; its aliases are the visible ones, its canonical
// name is replaced by its first visible alias when the canonical name itself
// is not visible, and FactCount counts its cloud-eligible facts only - so
// neither an alias a secret note added nor the existence of secret facts
// leaves the machine.
func (g *Graph) LookupEntities(ctx context.Context, traceID, name string, k int, cloudLane bool) ([]EntityRecord, error) {
	if g == nil || g.Store == nil {
		return nil, errors.New("graph: no store wired")
	}
	key := factengine.AliasKey(name)
	if key == "" {
		return nil, errors.New("graph: entity name must not be empty")
	}
	if k <= 0 {
		k = DefaultEntityLookupLimit
	}
	if k > MaxEntityLookupLimit {
		k = MaxEntityLookupLimit
	}
	rows, err := g.Store.ListEntitiesByFolded(ctx, sqlcgen.ListEntitiesByFoldedParams{
		AliasFolded: sql.NullString{String: likePrefix(key), Valid: true}, Limit: int64(MaxEntityLookupLimit * 4),
	})
	if err != nil {
		return nil, fmt.Errorf("graph: entity lookup: %w", err)
	}
	w := newWalk(ctx, g, Options{CloudLane: cloudLane, PerHopLimit: entityLookupFactLimit}.normalized())
	out := []EntityRecord{}
	for _, r := range rows {
		if len(out) == k {
			break
		}
		rec, ok, err := g.entityRecord(w, r, key)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, rec)
		}
	}
	if g.Log != nil {
		g.Log.With(traceID).Info("graph_entity_lookup", "candidates", len(rows), "results", len(out), "cloud_lane", cloudLane)
	}
	return out, nil
}

// likePrefix is key as a LIKE prefix pattern. LIKE's own wildcards in key
// are left as they are: every candidate is re-checked with a byte-exact
// strings.HasPrefix in entityRecord, so they can only widen the SQL
// candidate set, never the answer.
func likePrefix(key string) string { return key + "%" }

func (g *Graph) entityRecord(w *walk, r sqlcgen.ListEntitiesByFoldedRow, key string) (EntityRecord, bool, error) {
	facts, err := w.factsOf(r.ID)
	if err != nil {
		return EntityRecord{}, false, err
	}
	aliases, err := g.Store.ListEntityAliasesByEntity(w.ctx, r.ID)
	if err != nil {
		return EntityRecord{}, false, fmt.Errorf("graph: aliases of entity %d: %w", r.ID, err)
	}

	visible := func(string) bool { return true }
	laneFacts := facts
	if w.opts.CloudLane {
		names := map[string]bool{}
		laneFacts = nil
		for _, f := range facts {
			ok, err := w.laneOK(f)
			if err != nil {
				return EntityRecord{}, false, err
			}
			if !ok {
				continue
			}
			laneFacts = append(laneFacts, f)
			if f.SubjectEntityID.Valid && f.SubjectEntityID.Int64 == r.ID {
				names[factengine.AliasKey(f.Subject)] = true
			}
			if f.ObjectEntityID.Valid && f.ObjectEntityID.Int64 == r.ID {
				names[factengine.AliasKey(f.Object)] = true
			}
		}
		visible = func(n string) bool { return names[factengine.AliasKey(n)] }
	}

	rec := EntityRecord{EntityID: r.ID, Kind: r.Kind.String, Provisional: r.Provisional != 0, Aliases: []string{}, TopFacts: []FactRecord{}}
	matched := false
	for _, a := range aliases {
		if !visible(a.Alias) {
			continue
		}
		rec.Aliases = append(rec.Aliases, a.Alias)
		if strings.HasPrefix(factengine.AliasKey(a.Alias), key) {
			matched = true
		}
	}
	switch {
	case visible(r.CanonicalName):
		rec.CanonicalName = r.CanonicalName
		if strings.HasPrefix(factengine.AliasKey(r.CanonicalName), key) {
			matched = true
		}
	case len(rec.Aliases) > 0:
		rec.CanonicalName = rec.Aliases[0]
	}
	if !matched || rec.CanonicalName == "" {
		return EntityRecord{}, false, nil
	}

	if w.opts.CloudLane {
		rec.FactCount = int64(len(laneFacts))
	} else {
		ref := sql.NullInt64{Int64: r.ID, Valid: true}
		n, err := g.Store.CountActiveFactsForEntity(w.ctx, sqlcgen.CountActiveFactsForEntityParams{SubjectEntityID: ref, ObjectEntityID: ref})
		if err != nil {
			return EntityRecord{}, false, fmt.Errorf("graph: fact count of entity %d: %w", r.ID, err)
		}
		rec.FactCount = n
	}
	for _, f := range laneFacts {
		if len(rec.TopFacts) == entityTopFacts {
			break
		}
		if factengine.InjectionEligible(f) {
			rec.TopFacts = append(rec.TopFacts, toRecord(f, false))
		}
	}
	return rec, true, nil
}
