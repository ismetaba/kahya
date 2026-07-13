// select.go implements the W5-03 truth-ritual fact-sampling POLICY (task
// spec deliverable): a deterministic, documented, pure-Go decision over
// whatever ListActiveFacts already returned from brain.db - never a SQL
// WHERE clause encoding the priority rules themselves, so the policy stays
// readable and directly unit-testable.
//
// THE CORE SECURITY INVARIANT this file enforces (HANDOFF S5 safety #5
// Telegram bullet, verbatim: "gizli-serit etiketli tek bir bayt Telegram'a
// gonderilmez"): Select NEVER returns a secret-lane-tagged fact, and -
// fail-closed - NEVER returns a fact this package cannot positively
// classify as NOT secret-lane either. A fact's classification is derived
// from the SAME path-glob mechanism kahyad/internal/consolidation's own
// PartitionByLane already uses for memory files (policy.yaml's
// secret_lane_globs are FILE-PATH globs ONLY, HANDOFF S4 ordering
// invariant): walk every evidence row citing this fact back to the
// episode it came from, and check that episode's source_path. A fact with
// NO evidence row citing a resolvable episode path has NO classification
// record at all and is excluded on that basis alone - "no classification
// => treat as secret-lane => exclude" (task spec, verbatim).
package ritual

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store/sqlcgen"
)

// MaxQuestionsPerRun caps a single ritual run's sample size (HANDOFF S6 W5
// flag: "~10 olguluk 'bu dogru mu?'"; task spec deliverable: "cap at 10").
const MaxQuestionsPerRun = 10

// SamplerStore is the narrow brain.db read surface the sampler needs.
// *sqlcgen.Queries (via *store.Store) satisfies it directly, no adapter.
type SamplerStore interface {
	ListActiveFacts(ctx context.Context) ([]sqlcgen.Fact, error)
	ListEvidenceByFact(ctx context.Context, factID int64) ([]sqlcgen.ListEvidenceByFactRow, error)
	GetEpisodeByID(ctx context.Context, id int64) (sqlcgen.Episode, error)
	ListLastAskedAtAllFacts(ctx context.Context) ([]sqlcgen.ListLastAskedAtAllFactsRow, error)
}

var _ SamplerStore = (*sqlcgen.Queries)(nil)

// GlobMatcher is the narrow secret-lane-glob-matching seam this package
// needs - mirrors kahyad/internal/consolidation.GlobMatcher's identical
// shape/rationale (a test can inject a trivial fake without a real
// policy.yaml on disk).
type GlobMatcher interface {
	Match(pattern, path string) (bool, error)
}

// PolicyGlobMatcher adapts kahyad/internal/policy.MatchGlob (doublestar,
// byte-exact, no ASCII folding) - the production implementation.
type PolicyGlobMatcher struct{}

func (PolicyGlobMatcher) Match(pattern, path string) (bool, error) {
	return policy.MatchGlob(pattern, path)
}

// Sampler implements the sampling policy described in this file's own
// doc comment. Construct one with NewSampler per kahyad process.
type Sampler struct {
	store       SamplerStore
	memoryDir   string
	secretGlobs []string
	matcher     GlobMatcher
}

// NewSampler constructs a Sampler. matcher defaults to PolicyGlobMatcher{}
// when nil (production callers never need to pass one explicitly).
func NewSampler(store SamplerStore, memoryDir string, secretGlobs []string, matcher GlobMatcher) *Sampler {
	if matcher == nil {
		matcher = PolicyGlobMatcher{}
	}
	return &Sampler{store: store, memoryDir: memoryDir, secretGlobs: secretGlobs, matcher: matcher}
}

// scoredFact is one candidate fact plus the priority-ordering fields
// Select's sort key needs.
type scoredFact struct {
	fact          sqlcgen.Fact
	quarantined   bool
	thresholdDist float64
	lastAskedAt   string // "" == never asked (sorts first)
}

// Select returns up to MaxQuestionsPerRun eligible facts, in priority
// order: (1) quarantined agent_derived facts first, (2) among the rest,
// facts nearest the 0.3 injection threshold, (3) facts never probed or
// probed longest ago. Every tie (including a fact appearing in more than
// one tier equally) is broken by fact.ID ascending, so the result is
// FULLY deterministic for a given brain.db snapshot - re-running Select
// against unchanged data always returns the identical sample, never a
// randomized one.
func (s *Sampler) Select(ctx context.Context) ([]sqlcgen.Fact, error) {
	facts, err := s.store.ListActiveFacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("ritual: list active facts: %w", err)
	}

	lastAsked, err := s.lastAskedByFact(ctx)
	if err != nil {
		return nil, err
	}

	candidates := make([]scoredFact, 0, len(facts))
	for _, f := range facts {
		eligible, err := s.classify(ctx, f.ID)
		if err != nil {
			return nil, err
		}
		if !eligible {
			continue
		}
		candidates = append(candidates, scoredFact{
			fact:          f,
			quarantined:   f.SourceTier == factengine.TierAgentDerived && !f.ConfirmedAt.Valid,
			thresholdDist: math.Abs(f.Confidence - factengine.InjectionThresholdLogOdds),
			lastAskedAt:   lastAsked[f.ID],
		})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.quarantined != b.quarantined {
			return a.quarantined // quarantined (true) sorts before not-quarantined
		}
		if a.thresholdDist != b.thresholdDist {
			return a.thresholdDist < b.thresholdDist
		}
		if a.lastAskedAt != b.lastAskedAt {
			return a.lastAskedAt < b.lastAskedAt // "" (never asked) < any real timestamp
		}
		return a.fact.ID < b.fact.ID
	})

	n := len(candidates)
	if n > MaxQuestionsPerRun {
		n = MaxQuestionsPerRun
	}
	out := make([]sqlcgen.Fact, n)
	for i := 0; i < n; i++ {
		out[i] = candidates[i].fact
	}
	return out, nil
}

// lastAskedByFact returns fact_id -> most-recent asked_at across every
// eval_labels row ever inserted, for the "never/longest-ago probed"
// priority tier. A fact_id absent from the returned map has never been
// asked (Select's own sort treats that as "" - sorts first).
func (s *Sampler) lastAskedByFact(ctx context.Context) (map[int64]string, error) {
	rows, err := s.store.ListLastAskedAtAllFacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("ritual: list last asked_at: %w", err)
	}
	out := make(map[int64]string, len(rows))
	for _, r := range rows {
		if v, ok := r.LastAskedAt.(string); ok {
			out[r.FactID] = v
		}
	}
	return out, nil
}

// classify implements the fail-closed secret-lane exclusion described in
// this file's own package doc comment: factID is eligible iff it has at
// least one evidence row citing an episode with a resolvable, non-secret
// source_path, AND none of its cited episodes' paths match a secret-lane
// glob. Zero resolvable episode citations at all == no classification
// record == excluded.
func (s *Sampler) classify(ctx context.Context, factID int64) (eligible bool, err error) {
	rows, err := s.store.ListEvidenceByFact(ctx, factID)
	if err != nil {
		return false, fmt.Errorf("ritual: list evidence for fact %d: %w", factID, err)
	}

	classified := false
	for _, r := range rows {
		if !r.EpisodeID.Valid {
			continue
		}
		ep, err := s.store.GetEpisodeByID(ctx, r.EpisodeID.Int64)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return false, fmt.Errorf("ritual: get episode %d: %w", r.EpisodeID.Int64, err)
		}
		if !ep.SourcePath.Valid || ep.SourcePath.String == "" {
			continue
		}
		classified = true
		absPath := filepath.Join(s.memoryDir, filepath.FromSlash(ep.SourcePath.String))
		if s.matchesAnySecretGlob(absPath) {
			return false, nil // secret-lane tagged -> excluded, regardless of any OTHER evidence row
		}
	}
	if !classified {
		return false, nil // no classification record at all -> fail-closed exclude
	}
	return true, nil
}

// matchesAnySecretGlob reports whether absPath matches any entry in
// s.secretGlobs. A per-glob match error is treated as "no match" - every
// glob's syntax was already validated at policy.Load time (this mirrors
// kahyad/internal/consolidation's own matchesAnySecretGlob: over-blocking
// a path into "excluded" is always the safe direction, never the reverse).
func (s *Sampler) matchesAnySecretGlob(absPath string) bool {
	for _, g := range s.secretGlobs {
		ok, err := s.matcher.Match(g, absPath)
		if err != nil {
			continue
		}
		if ok {
			return true
		}
	}
	return false
}
