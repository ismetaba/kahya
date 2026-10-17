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
	// unknown) and valid_to > as_of (or unknown). With AsOf (or KnownAsOf)
	// set, an ENDED fact (retracted as factengine.RetractionEnded) is a
	// candidate too: AsOf keeps it when its window [valid_from, valid_to)
	// contains AsOf (its valid_to is always set; an unknown valid_from is
	// open). A CORRECTED fact, or a pre-0027 retraction (no kind on file),
	// is never returned (FIX-FINAL fact-history-ended-vs-corrected).
	AsOf time.Time
	// KnownAsOf keeps the facts already KNOWN then: created_at <=
	// known_as_of (transaction time). An ended fact must also have been
	// still active then: retracted_at > known_as_of. With both AsOf and
	// KnownAsOf, an ended fact must pass both.
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
	// so every fact must pass memlane.CloudEligible, and only cloud-lane
	// aliases and links count (package doc): EntityID/EntityName match a
	// fact only through a link a cloud-lane call wrote, and a local-lane
	// link is reported as entity id 0.
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
	// Ended marks a fact that held and no longer does (retracted as
	// factengine.RetractionEnded); only an AsOf / KnownAsOf query returns
	// one. RetractedAt is then the retraction's transaction time.
	Ended       bool
	RetractedAt string
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
//
// With AsOf or KnownAsOf the ENDED facts are candidates too (see
// FactQuery.AsOf/KnownAsOf), each marked Ended; their trust filter is
// factengine.HistoryEligible (IncludeQuarantined adds the rest, labelled
// Quarantined), and the lane filter is the same. Without either bound the
// answer is active facts only, exactly as before. A corrected or pre-0027
// retracted fact is never a candidate.
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

	w := newWalk(ctx, g, Options{CloudLane: q.CloudLane}.normalized())
	entityIDs := []int64{q.EntityID}
	if q.EntityName != "" {
		ids, err := w.entityIDsByName(factengine.AliasKey(q.EntityName))
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
// folded equality (!exact), and applies the Go filters. In the cloud lane
// the entity filter matches only a side a cloud-lane call linked.
func (g *Graph) queryFactsPass(w *walk, q FactQuery, entityIDs []int64, exact bool, limit int) ([]FactRecord, error) {
	seen := map[int64]bool{}
	var cands []sqlcgen.Fact
	cloudOnly := 0
	if w.opts.CloudLane {
		cloudOnly = 1
	}
	withEnded := 0
	if q.historyBound() {
		withEnded = 1
	}
	for _, e := range entityIDs {
		subj, obj := "", ""
		if exact {
			subj, obj = q.Subject, q.Object
		}
		rows, err := g.DB.QueryContext(w.ctx, `
			SELECT id, subject, predicate, object, source_tier, evidentiality, confidence, importance,
			       valid_from, valid_to, status, evidence, extractor_ver, updated_at, created_at,
			       confirmed_at, subject_entity_id, object_entity_id, quote, subject_link_cloud, object_link_cloud,
			       retraction_kind, retracted_at
			FROM facts
			WHERE (status = 'active' OR (? = 1 AND status = 'retracted' AND retraction_kind = 'ended'))
			  AND (? = '' OR subject = ?) AND (? = '' OR predicate = ?) AND (? = '' OR object = ?)
			  AND (? = 0
			       OR (subject_entity_id = ? AND (? = 0 OR subject_link_cloud = 1))
			       OR (object_entity_id = ? AND (? = 0 OR object_link_cloud = 1)))
			ORDER BY confidence DESC, updated_at DESC, id ASC
			LIMIT ?`,
			withEnded, subj, subj, q.Predicate, q.Predicate, obj, obj, e, e, cloudOnly, e, cloudOnly, factQueryScanCap)
		if err != nil {
			return nil, fmt.Errorf("graph: fact query: %w", err)
		}
		for rows.Next() {
			var f sqlcgen.Fact
			if err := rows.Scan(&f.ID, &f.Subject, &f.Predicate, &f.Object, &f.SourceTier, &f.Evidentiality, &f.Confidence, &f.Importance,
				&f.ValidFrom, &f.ValidTo, &f.Status, &f.Evidence, &f.ExtractorVer, &f.UpdatedAt, &f.CreatedAt,
				&f.ConfirmedAt, &f.SubjectEntityID, &f.ObjectEntityID, &f.Quote, &f.SubjectLinkCloud, &f.ObjectLinkCloud,
				&f.RetractionKind, &f.RetractedAt); err != nil {
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
		var ended, eligible bool
		switch {
		case f.Status == "active":
			if !q.timeMatch(f) {
				continue
			}
			eligible = factengine.InjectionEligible(f)
		case factengine.IsEnded(f) && q.historyBound():
			if !q.endedTimeMatch(f) {
				continue
			}
			ended, eligible = true, factengine.HistoryEligible(f)
		default:
			// A corrected or kind-less (pre-0027) retraction: never, even if
			// the SQL ever let one through (fail-closed).
			continue
		}
		if !q.tierMatch(f) {
			continue
		}
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
		rec := w.toRecord(f, !eligible)
		if ended {
			rec.Ended, rec.RetractedAt = true, f.RetractedAt.String
		}
		out = append(out, rec)
	}
	return out, nil
}

// historyBound reports whether q asks a point-in-time question (AsOf or
// KnownAsOf) - the only queries that may answer ended facts.
func (q FactQuery) historyBound() bool { return !q.AsOf.IsZero() || !q.KnownAsOf.IsZero() }

// endedTimeMatch is timeMatch for an ENDED fact: the same since/until
// bounds on valid_from; AsOf inside its window [valid_from, valid_to) with
// valid_to REQUIRED (RetractFact always sets it; a missing or unparsable one
// fails closed); KnownAsOf requires created_at <= known_as_of < retracted_at
// (known then, and not yet retracted then; a missing or unparsable
// retracted_at fails closed).
func (q FactQuery) endedTimeMatch(f sqlcgen.Fact) bool {
	if !q.validFromMatch(f) {
		return false
	}
	if !q.AsOf.IsZero() {
		to, ok := parseStored(f.ValidTo)
		if !ok || !to.After(q.AsOf) || !validFromNotAfter(f, q.AsOf) {
			return false
		}
	}
	if !q.KnownAsOf.IsZero() {
		known, err := time.Parse(time.RFC3339Nano, f.CreatedAt)
		if err != nil || known.After(q.KnownAsOf) {
			return false
		}
		retracted, ok := parseStored(f.RetractedAt)
		if !ok || !retracted.After(q.KnownAsOf) {
			return false
		}
	}
	return true
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
	if !q.validFromMatch(f) {
		return false
	}
	if !q.AsOf.IsZero() {
		if !validFromNotAfter(f, q.AsOf) {
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

// validFromMatch applies Since (inclusive) / Until (exclusive) to
// valid_from; a fact with no (or an unparsable) valid_from never matches a
// bound.
func (q FactQuery) validFromMatch(f sqlcgen.Fact) bool {
	from, fromOK := parseStored(f.ValidFrom)
	if !q.Since.IsZero() && (!fromOK || from.Before(q.Since)) {
		return false
	}
	if !q.Until.IsZero() && (!fromOK || !from.Before(q.Until)) {
		return false
	}
	return true
}

// validFromNotAfter reports valid_from <= t, an unknown valid_from being
// open (the start of the fact is not known) and an unparsable one failing.
func validFromNotAfter(f sqlcgen.Fact, t time.Time) bool {
	if !f.ValidFrom.Valid || f.ValidFrom.String == "" {
		return true
	}
	from, ok := parseStored(f.ValidFrom)
	return ok && !from.After(t)
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

// toRecord is f as a FactRecord, its entity ids as the walk's lane sees
// them (walk.links: a local-lane link is 0 for a cloud-lane caller).
func (w *walk) toRecord(f sqlcgen.Fact, quarantined bool) FactRecord {
	subj, obj := w.links(f)
	return FactRecord{
		FactID: f.ID, Subject: f.Subject, Predicate: f.Predicate, Object: f.Object,
		SourceTier: f.SourceTier, Evidentiality: f.Evidentiality,
		ConfidenceP: factengine.LogOddsToProbability(f.Confidence),
		Quarantined: quarantined,
		ValidFrom:   f.ValidFrom.String, ValidTo: f.ValidTo.String, KnownSince: f.CreatedAt,
		Quote: f.Quote.String, Evidence: f.Evidence.String,
		SubjectEntityID: subj, ObjectEntityID: obj,
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
// cloudLane (a worker's tool call): only cloud-lane identity counts (see the
// package doc). The candidates are the entities with a cloud-lane alias
// matching the prefix; a name of an entity counts only when it is a
// cloud-lane alias AND VISIBLE - the folded subject/object string, on a side
// a cloud-lane call linked to the entity, of a cloud-eligible fact. An
// entity is returned only when one of its counted names matches the prefix;
// its aliases are the counted ones, its canonical name is replaced by its
// first counted alias when the canonical name itself does not count, and
// FactCount counts its cloud-linked cloud-eligible facts only - so neither
// an alias or link a secret note led to nor the existence of secret facts
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
	rows, err := g.entityCandidates(ctx, key, cloudLane)
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

// entityCandidates is LookupEntities' SQL candidate set: active entities
// with an alias whose folded key starts with key - in the cloud lane only a
// cloud-lane alias (ListCloudEntitiesByFolded), so the candidate LIMIT never
// depends on secret-lane aliases either.
func (g *Graph) entityCandidates(ctx context.Context, key string, cloudLane bool) ([]sqlcgen.ListEntitiesByFoldedRow, error) {
	pattern := sql.NullString{String: likePrefix(key), Valid: true}
	limit := int64(MaxEntityLookupLimit * 4)
	if !cloudLane {
		return g.Store.ListEntitiesByFolded(ctx, sqlcgen.ListEntitiesByFoldedParams{AliasFolded: pattern, Limit: limit})
	}
	rows, err := g.Store.ListCloudEntitiesByFolded(ctx, sqlcgen.ListCloudEntitiesByFoldedParams{AliasFolded: pattern, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]sqlcgen.ListEntitiesByFoldedRow, len(rows))
	for i, r := range rows {
		out[i] = sqlcgen.ListEntitiesByFoldedRow(r)
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

	// counts reports whether alias row a is a name this lane may show: every
	// alias locally; in the cloud lane a cloud-lane alias that is visible.
	counts := func(sqlcgen.EntityAlias) bool { return true }
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
			// factsOf read only facts cloud-linked to r.ID; links names
			// the sides that are.
			subj, obj := w.links(f)
			if subj == r.ID {
				names[factengine.AliasKey(f.Subject)] = true
			}
			if obj == r.ID {
				names[factengine.AliasKey(f.Object)] = true
			}
		}
		counts = func(a sqlcgen.EntityAlias) bool { return a.CloudLane == 1 && names[factengine.AliasKey(a.Alias)] }
	}

	rec := EntityRecord{EntityID: r.ID, Kind: r.Kind.String, Provisional: r.Provisional != 0, Aliases: []string{}, TopFacts: []FactRecord{}}
	matched, canonicalCounts := false, false
	for _, a := range aliases {
		if !counts(a) {
			continue
		}
		rec.Aliases = append(rec.Aliases, a.Alias)
		if a.Alias == r.CanonicalName {
			canonicalCounts = true
		}
		if strings.HasPrefix(factengine.AliasKey(a.Alias), key) {
			matched = true
		}
	}
	if !w.opts.CloudLane {
		canonicalCounts = true
	}
	switch {
	case canonicalCounts:
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
			rec.TopFacts = append(rec.TopFacts, w.toRecord(f, false))
		}
	}
	return rec, true, nil
}
