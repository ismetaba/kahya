// Package graph is the retrieval graph layer over brain.db's facts/
// entities tables (G13 MEM-1): it reads the neighbourhood of the entities a
// query names (or a chunk hit's episode cites) so the <hafiza> injection
// block can carry the facts the knowledge graph holds about them, not only
// chunk text.
//
// It is READ-ONLY (kahyad stays brain.db's only writer; every write goes
// through kahyad/internal/factengine) and applies two filters to every fact
// before it is kept, in Go, never in a prompt:
//
//   - trust: factengine.InjectionEligible - active, log-odds at or above
//     the HANDOFF §5 memory #3 threshold (p >= 0.3), and never an
//     unconfirmed agent_derived fact (memory #1 quarantine);
//   - lane: for a cloud-lane caller (Options.CloudLane - every block that
//     reaches a cloud model), memlane.Classifier.CloudEligible: a fact citing
//     a secret_lane_globs path, a fact with no resolvable citation at all,
//     and a fact whose own text classifies secret stay on the machine
//     (fail-closed, HANDOFF §4 ordering invariant, §5 product principle
//     "gizlilik kodda").
//
// In the cloud lane the WALK itself also only goes through what may leave
// the machine. Which entity a name means can itself be secret ("Doktorum
// Ayse Kaya" in a finans/saglik/kimlik note), so the cloud lane uses only
// CLOUD-LANE identity (migration 0025, kahyad/internal/factengine's package
// doc): an alias counts only when a cloud-lane extraction call added it
// (entity_aliases.cloud_lane), and a fact's subject/object link only when a
// cloud-lane call wrote it (facts.subject_link_cloud/object_link_cloud) -
// such a call resolved the name in the cloud view, from bytes classified
// non-secret locally. A link a local-lane call wrote (it may have resolved
// "Doktor" through the secret note's alias) is never followed, seeded from
// or reported: a fact reached through it is not that entity's fact, and its
// entity id is reported as 0. On top of that an entity is reached only
// through a kept fact, through a cloud-eligible fact citing a chunk hit's
// episode, or through a query word that is one of its VISIBLE names (the
// folded subject/object string, on a cloud-linked side of a cloud-eligible
// fact, of a cloud-view alias). So which facts, names and entity ids a
// cloud-lane answer carries never depends on a secret association.
package graph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/store/sqlcgen"
)

// Defaults and bounds of one Neighbourhood walk.
const (
	// MaxHops is the deepest walk Options.Hops may ask for.
	MaxHops = 2
	// DefaultMaxFacts is Options.MaxFacts' fallback (config
	// hafiza_facts_max_lines' default, mcp/memory.MaxFactLines).
	DefaultMaxFacts = 12
	// DefaultPerHopLimit is Options.PerHopLimit's fallback: the SQL LIMIT
	// on the facts read per entity (highest confidence first).
	DefaultPerHopLimit = 50
	// maxFrontier caps the entities one hop expands (lowest ids first), so
	// a hub entity with many neighbours cannot turn one prompt into
	// hundreds of queries.
	maxFrontier = 64
	// maxQueryTokens caps the query words looked up as alias seeds.
	maxQueryTokens = 32
	// minSeedKeyRunes: a folded alias key shorter than this never seeds
	// ("ev", "o" ...); chunk-episode seeding still works for such names.
	minSeedKeyRunes = 3
	// maxEpisodeSeedFacts caps the facts read from the chunk hits'
	// episodes as the second seed source.
	maxEpisodeSeedFacts = 200
)

// Store is the brain.db read surface the walk needs; *sqlcgen.Queries
// satisfies it.
type Store interface {
	ListActiveEntityIDsByAliasFolded(ctx context.Context, aliasFolded sql.NullString) ([]int64, error)
	ListFactsCitingEpisodes(ctx context.Context, episodeIds []sql.NullInt64) ([]sqlcgen.ListFactsCitingEpisodesRow, error)
	ListFactsByEntityHop(ctx context.Context, arg sqlcgen.ListFactsByEntityHopParams) ([]sqlcgen.Fact, error)
	GetFact(ctx context.Context, id int64) (sqlcgen.Fact, error)

	// The cloud view (migration 0025): cloud-lane aliases and links only.
	ListActiveCloudEntityIDsByAliasFolded(ctx context.Context, aliasFolded sql.NullString) ([]int64, error)
	ListFactsByEntityCloudLinks(ctx context.Context, arg sqlcgen.ListFactsByEntityCloudLinksParams) ([]sqlcgen.Fact, error)

	// The structured reads (query.go, MEM-4).
	ListEntitiesByFolded(ctx context.Context, arg sqlcgen.ListEntitiesByFoldedParams) ([]sqlcgen.ListEntitiesByFoldedRow, error)
	ListCloudEntitiesByFolded(ctx context.Context, arg sqlcgen.ListCloudEntitiesByFoldedParams) ([]sqlcgen.ListCloudEntitiesByFoldedRow, error)
	ListEntityAliasesByEntity(ctx context.Context, entityID int64) ([]sqlcgen.EntityAlias, error)
	CountActiveFactsForEntity(ctx context.Context, arg sqlcgen.CountActiveFactsForEntityParams) (int64, error)
}

var _ Store = (*sqlcgen.Queries)(nil)

// FactLine is one injectable fact, already trust- and lane-filtered, in the
// shape mcp/memory.RenderKeptWithFacts prints (kahyad/internal/server
// converts it to mcp/memory.FactLine at the import boundary).
type FactLine struct {
	FactID        int64
	Subject       string
	Predicate     string
	Object        string
	SourceTier    string
	Evidentiality string
	// ConfidenceP is the probability form of the log-odds confidence
	// (factengine.LogOddsToProbability); the block prints 2 decimals.
	ConfidenceP float64
	// ValidFrom/ValidTo are the stored RFC3339 bounds, "" when NULL.
	ValidFrom string
	ValidTo   string
	// Hop is 0 for a seed entity's own fact, 1 one hop out, 2 two.
	Hop int
	// SubjectEntityID/ObjectEntityID are the fact's entity links, 0 when
	// unlinked - and, for a cloud-lane caller, 0 for a link a local-lane
	// call wrote (see the package doc).
	SubjectEntityID int64
	ObjectEntityID  int64
}

// Options shapes one walk.
type Options struct {
	// Hops is how far the walk goes past the seed entities' own facts
	// (config hafiza_graph_hops), clamped to [0, MaxHops]. The on/off switch
	// is config hafiza_graph_enabled at the call site, never a magic value
	// here: Hops 0 still returns the seeds' own facts.
	Hops int
	// MaxFacts caps the returned lines (<= 0 = DefaultMaxFacts).
	MaxFacts int
	// PerHopLimit caps the facts read per entity (<= 0 =
	// DefaultPerHopLimit).
	PerHopLimit int
	// CloudLane: the block reaches a cloud model, so every kept fact must
	// pass memlane.CloudEligible and the walk uses only cloud-lane aliases
	// and links of cloud-eligible facts (see the package doc). false = a
	// local-only caller (the secret lane, MEM-7): no lane filter, every
	// alias and link.
	CloudLane bool
}

func (o Options) normalized() Options {
	if o.Hops < 0 {
		o.Hops = 0
	}
	if o.Hops > MaxHops {
		o.Hops = MaxHops
	}
	if o.MaxFacts <= 0 {
		o.MaxFacts = DefaultMaxFacts
	}
	if o.PerHopLimit <= 0 {
		o.PerHopLimit = DefaultPerHopLimit
	}
	return o
}

// Graph is the read-only graph layer. Lane is the ONE memory-side lane rule
// (kahyad/main.go's laneClassifier); DB is brain.db for the hand-written
// structured fact query (query.go; the walk needs only Store); Log may be
// nil.
type Graph struct {
	Store Store
	DB    *sql.DB
	Lane  memlane.Classifier
	Log   *logx.Logger
}

// Neighbourhood returns the ranked FactLines for query q, given the chunk
// hits' episode ids, plus every entity id the walk read facts of (ascending).
//
// Seed entities (hop 0):
//
//	a) the query's words: every whitespace token (surrounding punctuation
//	   dropped) and every adjacent 2-gram, keyed by factengine.AliasKey
//	   (folded, Turkish suffix after an apostrophe trimmed - "Emre'nin" ->
//	   "emre"; keys under 3 runes are skipped), looked up with
//	   ListActiveEntityIDsByAliasFolded (CloudLane: the cloud-lane aliases,
//	   ListActiveCloudEntityIDsByAliasFolded, and the key must be a visible
//	   name of the entity);
//	b) the subject/object entities of the active facts citing the hits'
//	   episodes (ListFactsCitingEpisodes; CloudLane: cloud-eligible facts'
//	   cloud-lane links only).
//
// Hop 0 reads the seeds' own facts; hop k (1 <= k <= Hops) reads the facts
// of the entities at the other end of the facts KEPT at hop k-1 that were
// not visited yet (CloudLane: through cloud-lane links only, and an
// entity's facts are those linked to it on a cloud-lane side). A fact is
// kept when it passes factengine.InjectionEligible and, with CloudLane,
// memlane.CloudEligible.
// Ranking: hop ascending, then confidence descending, valid_from
// descending, fact id ascending; cut at MaxFacts (once MaxFacts lines are
// kept no deeper hop is read - it could never rank above them). The result
// is deterministic for a fixed brain.db. A store error is returned (the
// caller then injects chunks only).
func (g *Graph) Neighbourhood(ctx context.Context, traceID, q string, hitEpisodeIDs []int64, opts Options) ([]FactLine, []int64, error) {
	start := time.Now()
	if g == nil || g.Store == nil {
		return nil, nil, errors.New("graph: no store wired")
	}
	opts = opts.normalized()
	w := newWalk(ctx, g, opts)

	seeds, err := w.seeds(q, hitEpisodeIDs)
	if err != nil {
		return nil, nil, err
	}

	visited := map[int64]bool{}
	seenFact := map[int64]bool{}
	var kept []FactLine
	var perHop [MaxHops + 1]int
	frontier := seeds
	for hop := 0; hop <= opts.Hops && len(frontier) > 0 && len(kept) < opts.MaxFacts; hop++ {
		if len(frontier) > maxFrontier {
			frontier = frontier[:maxFrontier]
		}
		var cands []sqlcgen.Fact
		for _, e := range frontier {
			visited[e] = true
			facts, err := w.factsOf(e)
			if err != nil {
				return nil, nil, err
			}
			for _, f := range facts {
				if !seenFact[f.ID] {
					seenFact[f.ID] = true
					cands = append(cands, f)
				}
			}
		}
		sortFacts(cands)
		next := map[int64]bool{}
		for _, f := range cands {
			if len(kept) >= opts.MaxFacts {
				break
			}
			ok, err := w.injectable(f)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				continue
			}
			kept = append(kept, w.toLine(f, hop))
			perHop[hop]++
			for _, other := range w.linkedEnds(f) {
				if !visited[other] {
					next[other] = true
				}
			}
		}
		frontier = sortedIDs(next)
	}

	entityIDs := sortedIDs(visited)
	if g.Log != nil {
		log := g.Log.With(traceID)
		log.Debug("graph_neighbourhood_query", "query", q)
		log.Info("graph_neighbourhood",
			"seed_entities", len(seeds),
			"hop0_facts", perHop[0],
			"hop1_facts", perHop[1],
			"hop2_facts", perHop[2],
			"kept", len(kept),
			"cloud_lane", opts.CloudLane,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	}
	return kept, entityIDs, nil
}

// walk is one Neighbourhood/LookupEntities call's state: per-call caches of
// each entity's facts and each fact's lane verdict, so a fact read twice
// (seed check, then hop) is classified once.
type walk struct {
	ctx     context.Context
	g       *Graph
	opts    Options
	facts   map[int64][]sqlcgen.Fact
	verdict map[int64]bool
}

func newWalk(ctx context.Context, g *Graph, opts Options) *walk {
	return &walk{ctx: ctx, g: g, opts: opts, facts: map[int64][]sqlcgen.Fact{}, verdict: map[int64]bool{}}
}

// factsOf returns entity e's active facts (subject or object side, highest
// confidence first, at most PerHopLimit), cached per call. In the cloud lane
// only the facts linked to e on a cloud-lane side (ListFactsByEntityCloudLinks).
func (w *walk) factsOf(e int64) ([]sqlcgen.Fact, error) {
	if fs, ok := w.facts[e]; ok {
		return fs, nil
	}
	ref := sql.NullInt64{Int64: e, Valid: true}
	var fs []sqlcgen.Fact
	var err error
	if w.opts.CloudLane {
		fs, err = w.g.Store.ListFactsByEntityCloudLinks(w.ctx, sqlcgen.ListFactsByEntityCloudLinksParams{SubjectEntityID: ref, ObjectEntityID: ref, Limit: int64(w.opts.PerHopLimit)})
	} else {
		fs, err = w.g.Store.ListFactsByEntityHop(w.ctx, sqlcgen.ListFactsByEntityHopParams{SubjectEntityID: ref, ObjectEntityID: ref, Limit: int64(w.opts.PerHopLimit)})
	}
	if err != nil {
		return nil, fmt.Errorf("graph: facts of entity %d: %w", e, err)
	}
	w.facts[e] = fs
	return fs, nil
}

// laneOK reports whether f may reach the caller's lane: always in the local
// lane; in the cloud lane only when memlane.CloudEligible says so (a store
// error is returned - fail-closed, the caller drops the whole section).
func (w *walk) laneOK(f sqlcgen.Fact) (bool, error) {
	if !w.opts.CloudLane {
		return true, nil
	}
	if v, ok := w.verdict[f.ID]; ok {
		return v, nil
	}
	ok, err := w.g.Lane.CloudEligible(w.ctx, f)
	if err != nil {
		return false, fmt.Errorf("graph: lane of fact %d: %w", f.ID, err)
	}
	w.verdict[f.ID] = ok
	return ok, nil
}

// injectable is the keep rule: trust first (no store read), then lane.
func (w *walk) injectable(f sqlcgen.Fact) (bool, error) {
	if !factengine.InjectionEligible(f) {
		return false, nil
	}
	return w.laneOK(f)
}

// visibleName reports whether key is one of entity e's names a cloud-lane
// caller may see: the folded subject string (when e is the cloud-linked
// subject) or the folded object string (when e is the cloud-linked object)
// of one of e's active facts that is cloud-eligible. Trust is not required -
// a quarantined fact from an ordinary note still shows the name appears in
// ordinary content. Only the facts factsOf reads are considered (a name
// visible only past the PerHopLimit is treated as not visible: fail-closed).
func (w *walk) visibleName(e int64, key string) (bool, error) {
	facts, err := w.factsOf(e)
	if err != nil {
		return false, err
	}
	for _, f := range facts {
		if !w.sideNamed(f, e, key) {
			continue
		}
		ok, err := w.laneOK(f)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// sideNamed reports whether fact f names entity e as key on a side linked
// to e (in the cloud lane: a cloud-lane link).
func (w *walk) sideNamed(f sqlcgen.Fact, e int64, key string) bool {
	subj, obj := w.links(f)
	if subj == e && factengine.AliasKey(f.Subject) == key {
		return true
	}
	return obj == e && factengine.AliasKey(f.Object) == key
}

// links returns f's subject/object entity ids as this walk's lane sees them:
// every stored link in the local lane; in the cloud lane only a side a
// cloud-lane call linked (facts.subject_link_cloud/object_link_cloud), 0 for
// the other (a local-lane link may stand for a secret association).
func (w *walk) links(f sqlcgen.Fact) (subject, object int64) {
	return linkedID(f.SubjectEntityID, f.SubjectLinkCloud, w.opts.CloudLane), linkedID(f.ObjectEntityID, f.ObjectLinkCloud, w.opts.CloudLane)
}

// linkedID is one side's entity id for a lane: 0 when unlinked, or when
// cloudLane and the link is not a cloud-lane one.
func linkedID(id sql.NullInt64, cloudFlag int64, cloudLane bool) int64 {
	if !id.Valid || (cloudLane && cloudFlag != 1) {
		return 0
	}
	return id.Int64
}

// linkedEnds is the entities fact f leads to in this walk's lane.
func (w *walk) linkedEnds(f sqlcgen.Fact) []int64 {
	var out []int64
	subj, obj := w.links(f)
	if subj != 0 {
		out = append(out, subj)
	}
	if obj != 0 {
		out = append(out, obj)
	}
	return out
}

// entityIDsByName resolves a folded alias key in this walk's lane: every
// active entity with that alias key, or in the cloud lane only those with a
// cloud-lane alias of it.
func (w *walk) entityIDsByName(key string) ([]int64, error) {
	if w.opts.CloudLane {
		return w.g.Store.ListActiveCloudEntityIDsByAliasFolded(w.ctx, sql.NullString{String: key, Valid: true})
	}
	return w.g.Store.ListActiveEntityIDsByAliasFolded(w.ctx, sql.NullString{String: key, Valid: true})
}

// seeds returns the walk's hop-0 entities, ascending.
func (w *walk) seeds(q string, hitEpisodeIDs []int64) ([]int64, error) {
	set := map[int64]bool{}
	for _, key := range QueryAliasKeys(q) {
		ids, err := w.entityIDsByName(key)
		if err != nil {
			return nil, fmt.Errorf("graph: alias seed: %w", err)
		}
		for _, id := range ids {
			if set[id] {
				continue
			}
			if w.opts.CloudLane {
				ok, err := w.visibleName(id, key)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
			}
			set[id] = true
		}
	}

	eps := uniqueEpisodeIDs(hitEpisodeIDs)
	if len(eps) > 0 {
		rows, err := w.g.Store.ListFactsCitingEpisodes(w.ctx, eps)
		if err != nil {
			return nil, fmt.Errorf("graph: episode seed: %w", err)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		if len(rows) > maxEpisodeSeedFacts {
			rows = rows[:maxEpisodeSeedFacts]
		}
		for _, r := range rows {
			subj := linkedID(r.SubjectEntityID, r.SubjectLinkCloud, w.opts.CloudLane)
			obj := linkedID(r.ObjectEntityID, r.ObjectLinkCloud, w.opts.CloudLane)
			if subj == 0 && obj == 0 {
				continue
			}
			if w.opts.CloudLane {
				f, err := w.g.Store.GetFact(w.ctx, r.ID)
				if err != nil {
					return nil, fmt.Errorf("graph: episode seed fact %d: %w", r.ID, err)
				}
				ok, err := w.laneOK(f)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
			}
			if subj != 0 {
				set[subj] = true
			}
			if obj != 0 {
				set[obj] = true
			}
		}
	}
	return sortedIDs(set), nil
}

// QueryAliasKeys is the alias-seed key list of a query: each whitespace
// token with its surrounding punctuation dropped, then every adjacent pair
// of tokens joined by one space, each keyed by factengine.AliasKey and kept
// when at least minSeedKeyRunes runes long; first occurrence order, no
// duplicates, at most maxQueryTokens tokens considered.
func QueryAliasKeys(q string) []string {
	var toks []string
	for _, raw := range strings.Fields(q) {
		t := strings.TrimFunc(raw, func(r rune) bool { return !isNameRune(r) })
		if t != "" {
			toks = append(toks, t)
		}
		if len(toks) == maxQueryTokens {
			break
		}
	}
	seen := map[string]bool{}
	var keys []string
	add := func(s string) {
		k := factengine.AliasKey(s)
		if utf8.RuneCountInString(k) < minSeedKeyRunes || seen[k] {
			return
		}
		seen[k] = true
		keys = append(keys, k)
	}
	for _, t := range toks {
		add(t)
	}
	for i := 0; i+1 < len(toks); i++ {
		add(toks[i] + " " + toks[i+1])
	}
	return keys
}

// isNameRune: letters, digits and the two apostrophes AliasKey trims at.
func isNameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '’'
}

func uniqueEpisodeIDs(ids []int64) []sql.NullInt64 {
	seen := map[int64]bool{}
	var out []sql.NullInt64
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, sql.NullInt64{Int64: id, Valid: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Int64 < out[j].Int64 })
	return out
}

// sortFacts is the within-hop ranking: confidence desc, valid_from desc
// (NULL last), id asc.
func sortFacts(fs []sqlcgen.Fact) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		if a.ValidFrom.String != b.ValidFrom.String {
			return a.ValidFrom.String > b.ValidFrom.String
		}
		return a.ID < b.ID
	})
}

func (w *walk) toLine(f sqlcgen.Fact, hop int) FactLine {
	subj, obj := w.links(f)
	return FactLine{
		FactID: f.ID, Subject: f.Subject, Predicate: f.Predicate, Object: f.Object,
		SourceTier: f.SourceTier, Evidentiality: f.Evidentiality,
		ConfidenceP: factengine.LogOddsToProbability(f.Confidence),
		ValidFrom:   f.ValidFrom.String, ValidTo: f.ValidTo.String,
		Hop:             hop,
		SubjectEntityID: subj, ObjectEntityID: obj,
	}
}

func sortedIDs(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
