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
// classify as NOT secret-lane either. A fact is classified on BOTH halves
// of HANDOFF S4's ordering invariant:
//
//   - PATH: the SAME path-glob mechanism kahyad/internal/consolidation's
//     own PartitionByLane already uses for memory files (policy.yaml's
//     secret_lane_globs are FILE-PATH globs ONLY): walk every evidence row
//     citing this fact back to the episode it came from, and check that
//     episode's source_path. A fact with NO evidence row citing a
//     resolvable episode path has NO classification record at all and is
//     excluded on that basis alone - "no classification => treat as
//     secret-lane => exclude" (task spec, verbatim).
//   - CONTENT (FIX-P-17): the exact text Run would send to Telegram
//     (factText) is classified locally - always by the deterministic
//     pre-pass (secretlane.ClassifyDeterministic: IBAN/TCKN/card/CVV and
//     the finans/saglik/kimlik lexicon), plus an optional FactClassifier
//     on top (fail-closed: any error excludes). Facts are path-independent
//     (an IBAN can sit in a note under an ordinary path), so the path
//     check alone let such a fact's text reach Telegram.
//
// Every excluded fact is ledgered (EventFactExcludedSecretLane, with the
// run's trace_id) when a ledger is wired, so a silently short ritual is
// explainable from the events table.
//
// G13 common ground (G00 C-20): the classification itself lives in
// kahyad/internal/memlane (memlane.Classifier.Classify), moved there in this
// post-FIX-P-17 form so the later cloud uses of facts apply the same rule;
// this file keeps the sampling policy and the exclusion ledger. One change
// came with the move: a secret-lane glob that cannot be evaluated now
// excludes the fact (fail-closed) instead of counting as "no match".
package ritual

import (
	"context"
	"fmt"
	"math"
	"sort"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store/sqlcgen"
)

// MaxQuestionsPerRun caps a single ritual run's sample size (HANDOFF S6 W5
// flag: "~10 olguluk 'bu dogru mu?'"; task spec deliverable: "cap at 10").
const MaxQuestionsPerRun = 10

// SamplerStore is the narrow brain.db read surface the sampler needs.
// *sqlcgen.Queries (via *store.Store) satisfies it directly, no adapter.
// memlane.Store (ListEpisodePathsForFact) is the classification read.
type SamplerStore interface {
	memlane.Store
	ListActiveFacts(ctx context.Context) ([]sqlcgen.Fact, error)
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

// FactClassifier is the FIX-P-17 content seam on top of the deterministic
// pre-pass: it decides whether the text that WOULD be sent to Telegram
// (factText) is finans/saglik/kimlik content. Optional - nil means
// "deterministic pre-pass only", never "skip classification". kahyad wires
// none (owner decision G02-4: deterministic-only, so memory pressure on
// the local model can never empty a ritual); a wired one must be local-only
// and is fail-closed: an error excludes the fact.
type FactClassifier interface {
	ClassifySecretLane(ctx context.Context, text string) (secret bool, err error)
}

// EventFactExcludedSecretLane is ledgered once per fact Select excludes on
// secret-lane grounds (payload: fact_id, reason), under the run's trace_id.
const EventFactExcludedSecretLane = "ritual.fact_excluded_secret_lane"

// Exclusion reasons (EventFactExcludedSecretLane payload "reason") -
// memlane's verdict reasons, unchanged.
const (
	ExcludeReasonPathGlob             = memlane.ReasonPathGlob
	ExcludeReasonNoClassification     = memlane.ReasonNoClassification
	ExcludeReasonContentDeterministic = memlane.ReasonContentDeterministic
	ExcludeReasonContentClassifier    = memlane.ReasonContentClassifier
	ExcludeReasonClassifyFailed       = memlane.ReasonClassifyFailed
)

// Sampler implements the sampling policy described in this file's own
// doc comment. Construct one with NewSampler per kahyad process.
type Sampler struct {
	store  SamplerStore
	lane   memlane.Classifier
	ledger Ledger // nil => exclusions are not ledgered
}

// SetFactClassifier wires an optional content classifier on top of the
// deterministic pre-pass (see FactClassifier). Production wires none.
func (s *Sampler) SetFactClassifier(c FactClassifier) { s.lane.Content = c }

// SetLedger wires the exclusion ledger (*store.Store in production).
func (s *Sampler) SetLedger(l Ledger) { s.ledger = l }

// NewSampler constructs a Sampler. matcher defaults to PolicyGlobMatcher{}
// when nil (production callers never need to pass one explicitly).
func NewSampler(store SamplerStore, memoryDir string, secretGlobs []string, matcher GlobMatcher) *Sampler {
	if matcher == nil {
		matcher = PolicyGlobMatcher{}
	}
	return &Sampler{store: store, lane: memlane.Classifier{
		Store: store, MemoryDir: memoryDir, SecretGlobs: secretGlobs, Matcher: matcher,
	}}
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
// randomized one. traceID is the run's trace_id: every exclusion ledger row
// carries it.
func (s *Sampler) Select(ctx context.Context, traceID string) ([]sqlcgen.Fact, error) {
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
		eligible, err := s.classify(ctx, traceID, f)
		if err != nil {
			return nil, err
		}
		if !eligible {
			continue
		}
		candidates = append(candidates, scoredFact{
			fact:          f,
			quarantined:   factengine.FactQuarantined(f),
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
// this file's own package doc comment through memlane.Classifier.Classify:
// f is eligible iff it has at least one evidence row citing an episode with
// a resolvable, non-secret source_path, NONE of its cited episodes' paths
// match a secret-lane glob, AND its text (factText - exactly what Run
// sends; memlane.FactText is the same string) is not secret-lane content.
// Every exclusion is ledgered (ledgerExcluded).
func (s *Sampler) classify(ctx context.Context, traceID string, f sqlcgen.Fact) (eligible bool, err error) {
	v, err := s.lane.Classify(ctx, f)
	if err != nil {
		return false, fmt.Errorf("ritual: classify fact %d: %w", f.ID, err)
	}
	if !v.Eligible {
		s.ledgerExcluded(ctx, traceID, f.ID, v.Reason)
		return false, nil
	}
	return true, nil
}

// ledgerExcluded records one EventFactExcludedSecretLane row (best-effort:
// the exclusion itself never depends on the ledger write).
func (s *Sampler) ledgerExcluded(ctx context.Context, traceID string, factID int64, reason string) {
	if s.ledger == nil {
		return
	}
	_ = s.ledger.LogEvent(ctx, traceID, EventFactExcludedSecretLane, map[string]any{"fact_id": factID, "reason": reason})
}
