// Package factengine implements W5-04: the SINGLE fact-write path every
// caller in kahyad must go through to create or update a row in the
// `facts` table (HANDOFF S5 memory #1-#3, quoted verbatim in the task
// spec):
//
//  1. Kaynak-guven kafesi: her olgu source_tier IN {user_edit(1.0) >
//     user_asserted(<=.95) > external_doc(<=.8) > screen(<=.7) >
//     agent_derived(<=.4)}. Ajan-turevi karantinada, kullanici
//     onaylayana dek profil kartindan/enjeksiyondan haric.
//  2. Bolunebilir, kanit-kapili varlik birlestirme: isim benzerligiyle
//     asla oto-birlestirme. En az bir ayirt edici kanit sart.
//     Merge-defteri + varlik-bolme operasyonu. Supheli ayni-isim -> yeni
//     gecici varlik.
//  3. Negatif kanit + log-odds guven: noisy-OR ratchet yok; ayni-oturum
//     tekrari tek kanit sayilir. Kullanici reddi guveni dusurur; <0.3
//     enjeksiyondan cikar. "Artik sevmiyorum" -> geri-cekme.
//
// THE CORE SECURITY INVARIANT this package enforces (the one a prompt
// injection attack targets): source_tier is assigned HERE, from Go-side
// provenance the CALL SITE asserts about ITSELF, never from anything an
// LLM extractor's own candidate struct claims. Candidate.ClaimedSourceTier
// exists purely so a caller passing through an extractor's raw output
// does not need to strip that field by hand - WriteFact always ignores it
// for tier assignment and ledgers a factengine.tier_clamped event
// whenever it would have implied a HIGHER tier than what Go-side
// provenance actually earned (HANDOFF S5 product principle: "modeli
// anahtarlari olmayan parlak-ama-fazla-ozguvenli bir junior gibi ele
// al" - a junior's own say-so about its credentials is never trusted).
//
// Candidate.Provenance is the only tier signal WriteFact actually acts
// on, and even THAT is gated: ProvenanceUserAsserted (the call site's own
// claim that this candidate is a direct user utterance) only survives as
// TierUserAsserted when the originating session_id has a CLEAN W4-03
// taint record (kahyad/internal/taint.Tracker.Get) - a missing record or
// any non-clean tier fails closed to TierAgentDerived, per the task
// spec's fail-closed mandate. TierUserEdit is not even a reachable
// Provenance value here at all: HANDOFF S5 memory #1 places it ABOVE
// user_asserted, reachable ONLY via a real `user <user@kahya.local>` git
// commit author on a ~/Kahya/memory file (kahyad/internal/indexer's own
// episode-level source_tier derivation, gitauthor.go in that package) -
// never via any runtime candidate this engine ever sees.
package factengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-sqlite3"

	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/taint"
)

// Source-trust lattice tiers (HANDOFF S5 memory #1). These are the ONLY
// five values facts.source_tier / episodes.source_tier may ever hold (the
// same enum migrations/0001_init_schema.sql's CHECK constraints
// enforce).
const (
	TierUserEdit     = "user_edit"
	TierUserAsserted = "user_asserted"
	TierExternalDoc  = "external_doc"
	TierScreen       = "screen"
	TierAgentDerived = "agent_derived"
)

// Provenance is the CALL SITE's own Go-side claim about where a candidate
// came from - the only signal WriteFact trusts for tier assignment (see
// the package doc for why, and for ProvenanceUserAsserted's additional
// taint gate). Deliberately has NO "user edit" value: that tier is never
// a runtime WriteFact input at all (package doc).
type Provenance string

const (
	// ProvenanceUserAsserted claims this candidate is a direct user
	// utterance from Candidate.SessionID - WriteFact still requires that
	// session to have a CLEAN W4-03 taint record before honoring this;
	// otherwise it fails closed to TierAgentDerived.
	ProvenanceUserAsserted Provenance = "user_asserted"
	// ProvenanceExternalDoc/ProvenanceScreen are set by an INGEST call
	// site (never an extractor) from the parsed content's own source
	// type.
	ProvenanceExternalDoc Provenance = "external_doc"
	ProvenanceScreen      Provenance = "screen"
	// ProvenanceAgentDerived is the default for everything an LLM
	// extractor (or kahyad's own deterministic promotion code, e.g.
	// hot-window detail-atom promotion) proposed on its own.
	ProvenanceAgentDerived Provenance = "agent_derived"
)

// Evidentiality enum values (HANDOFF S5 schema block: "-mis morfolojisi:
// witnessed|reported|inferred").
const (
	Witnessed = "witnessed"
	Reported  = "reported"
	Inferred  = "inferred"
)

// Log-odds constants (HANDOFF S5 memory #3, canonical per the task spec):
// each tier's cap is logit(that tier's max probability from rule #1), and
// each tier's SINGLE-evidence delta is defined to equal its own cap - one
// qualifying piece of evidence already saturates that tier's ceiling in
// one step (matching the task spec's "+2.94 (cap p=.95)" / "+1.39 (cap
// .8)" / "+0.85 (cap .7)" / "capped at .4 (-0.405)" pattern, where the
// quoted delta and the quoted cap are the same number). Computed via
// logit() rather than hand-copied so the numbers are provably consistent
// with the probabilities rule #1 actually specifies, not independently
// rounded constants that could drift apart.
func logit(p float64) float64 { return math.Log(p / (1 - p)) }

// Capped probabilities, HANDOFF S5 memory #1 rule verbatim.
const (
	capProbUserAsserted = 0.95
	capProbExternalDoc  = 0.8
	capProbScreen       = 0.7
	capProbAgentDerived = 0.4
)

// Log-odds caps derived from the probabilities above - exported so tests
// (and any future caller) can assert exact values rather than
// re-deriving them.
var (
	CapLogOddsUserAsserted = logit(capProbUserAsserted) // ~= +2.9444
	CapLogOddsExternalDoc  = logit(capProbExternalDoc)  // ~= +1.3863
	CapLogOddsScreen       = logit(capProbScreen)       // ~= +0.8473
	CapLogOddsAgentDerived = logit(capProbAgentDerived) // ~= -0.4055
)

// DenialLogOdds is the FIXED log-odds swing a user denial (or a
// retraction's own negative evidence row) contributes, independent of
// whatever tier originally supported the fact (HANDOFF S5 memory #3
// verbatim: "kullanici reddi ... -2.94").
const DenialLogOdds = -2.94

// InjectionThresholdLogOdds is HANDOFF S5 memory #3's verbatim injection
// cutoff: "enjeksiyon esigi p=0.3 => log-odds -0.847" (logit(0.3) ~=
// -0.8473, rounded to the task spec's own three-decimal literal so this
// package's threshold matches the spec byte-for-byte rather than a
// separately-rounded computed value).
const InjectionThresholdLogOdds = -0.847

// tierDelta returns tier's fixed per-evidence log-odds delta. TierUserEdit
// (and any unrecognized tier) has none - WriteFact never assigns
// TierUserEdit, so this only returns ok=false for a value that should be
// structurally unreachable.
func tierDelta(tier string) (delta float64, ok bool) {
	switch tier {
	case TierUserAsserted:
		return CapLogOddsUserAsserted, true
	case TierExternalDoc:
		return CapLogOddsExternalDoc, true
	case TierScreen:
		return CapLogOddsScreen, true
	case TierAgentDerived:
		return CapLogOddsAgentDerived, true
	default:
		return 0, false
	}
}

// Ledger event kinds this package writes (HANDOFF S5 safety #4: every
// trust decision is auditable).
const (
	// EventTierClamped fires whenever the tier WriteFact actually assigned
	// differs from what the candidate/provenance implied it wanted -
	// either an extractor's ClaimedSourceTier was ignored, or a
	// ProvenanceUserAsserted claim failed the taint gate. The single most
	// important forensic event this package produces: it is the proof
	// that a prompt-injected extractor could not mint trust.
	EventTierClamped   = "factengine.tier_clamped"
	EventFactDenied    = "factengine.fact_denied"
	EventFactRetracted = "factengine.fact_retracted"
	EventFactConfirmed = "factengine.fact_confirmed"
	EventEntityMerged  = "factengine.entity_merged"
	EventEntitySplit   = "factengine.entity_split"
)

// MaxFieldRunes is the S5 safety#2 free-text length cap this package
// applies to every subject/predicate/object/evidence field (HANDOFF S5
// safety#2: "serbest-metin alanlari uzunluk+karakter-sinifi kisitli" -
// the SAME posture Reader-seeded W-actions are held to, applied here to
// candidate facts since an extractor is exactly the kind of untrusted-
// enough-to-validate Go-side producer that rule describes).
const MaxFieldRunes = 500

// Candidate is what an extractor (or kahyad's own deterministic
// promotion code) proposes to WriteFact. See the package doc for the
// security model ClaimedSourceTier/Provenance encode.
type Candidate struct {
	Subject   string
	Predicate string
	Object    string

	// ClaimedSourceTier is whatever the EXTRACTOR's own struct said its
	// source_tier should be - ALWAYS IGNORED for tier assignment. Leave
	// empty for a candidate with no such field (e.g. kahyad's own
	// Go-authored candidates, which never claim anything).
	ClaimedSourceTier string

	// Provenance is the call site's OWN claim (never a model's) about
	// where this candidate came from. Empty defaults to
	// ProvenanceAgentDerived.
	Provenance Provenance

	// SessionID is the originating W4-03 session - required (and gated
	// via a clean taint record) for Provenance=ProvenanceUserAsserted to
	// actually stick; also the evidence-dedupe key (HANDOFF S5 memory
	// #3: same-session repeats are one evidence row).
	SessionID string

	// EpisodeID cites the raw episode this candidate was extracted from
	// (0 = none/not applicable).
	EpisodeID int64

	// Evidentiality is the extractor's own -mis-morphology-derived claim
	// (witnessed|reported|inferred); "" defaults to Inferred
	// (fail-conservative, HANDOFF S5 schema block).
	Evidentiality string

	Importance float64

	// ExtractorVer identifies what produced this candidate struct -
	// REQUIRED (a sentinel like "user_direct_v1"/"truth_ritual_v1" for a
	// non-extractor call site is fine, but it must never be empty).
	ExtractorVer string

	// Evidence is a free-form citation string (e.g. "episode:12,chunk:34")
	// - never a prior summary (HANDOFF S5 memory #4, enforced by
	// kahyad/internal/consolidation.ValidateSummaryEvidence at the
	// hot-window call site).
	Evidence string
}

// Store is the narrow brain.db surface this package needs -
// *sqlcgen.Queries satisfies it directly, with no adapter (matching
// kahyad/internal/taint.Store's and kahyad/internal/consolidation.
// FactStore's identical "one interface, one production adapter"
// convention).
type Store interface {
	GetActiveFactByTriple(ctx context.Context, arg sqlcgen.GetActiveFactByTripleParams) (sqlcgen.Fact, error)
	InsertFact(ctx context.Context, arg sqlcgen.InsertFactParams) (sqlcgen.Fact, error)
	GetFact(ctx context.Context, id int64) (sqlcgen.Fact, error)
	UpdateFactConfidence(ctx context.Context, arg sqlcgen.UpdateFactConfidenceParams) error
	ConfirmFact(ctx context.Context, arg sqlcgen.ConfirmFactParams) error
	RetractFact(ctx context.Context, arg sqlcgen.RetractFactParams) error
	ListEvidenceByFact(ctx context.Context, factID int64) ([]sqlcgen.ListEvidenceByFactRow, error)
	GetEvidenceByFactSessionPolarity(ctx context.Context, arg sqlcgen.GetEvidenceByFactSessionPolarityParams) (sqlcgen.GetEvidenceByFactSessionPolarityRow, error)
	InsertEvidence(ctx context.Context, arg sqlcgen.InsertEvidenceParams) (sqlcgen.InsertEvidenceRow, error)

	InsertEntity(ctx context.Context, arg sqlcgen.InsertEntityParams) (sqlcgen.InsertEntityRow, error)
	GetEntity(ctx context.Context, id int64) (sqlcgen.GetEntityRow, error)
	UpdateEntityStatus(ctx context.Context, arg sqlcgen.UpdateEntityStatusParams) error
	ListEntityIDsByAlias(ctx context.Context, alias string) ([]int64, error)
	InsertEntityAlias(ctx context.Context, arg sqlcgen.InsertEntityAliasParams) (sqlcgen.EntityAlias, error)
	ListEntityAliasesByEntity(ctx context.Context, entityID int64) ([]sqlcgen.EntityAlias, error)
	UpdateEntityAliasEntityByID(ctx context.Context, arg sqlcgen.UpdateEntityAliasEntityByIDParams) error
	InsertMergeLedger(ctx context.Context, arg sqlcgen.InsertMergeLedgerParams) (sqlcgen.MergeLedger, error)
	GetMergeLedger(ctx context.Context, id int64) (sqlcgen.MergeLedger, error)
}

var _ Store = (*sqlcgen.Queries)(nil)

// TaintChecker is the narrow W4-03 taint-tier read surface WriteFact
// needs to gate ProvenanceUserAsserted - *taint.Tracker satisfies it
// directly.
type TaintChecker interface {
	Get(ctx context.Context, sessionID string) (string, error)
}

var _ TaintChecker = (*taint.Tracker)(nil)

// Ledger is the append-only events sink this package writes to (HANDOFF
// S5 safety #4) - *kahyad/internal/store.Store already has this exact
// method shape.
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Engine is kahyad's single fact-write path (W5-04). Construct one with
// New per kahyad process and share it across every caller (W12-05
// memory_write, W5-02 hot-window promotion, W5-03 ritual answers, the CLI
// route handlers) - HANDOFF S4: kahyad is brain.db's only writer, and
// this package is that writer's ONE fact-shaped door.
type Engine struct {
	store  Store
	taint  TaintChecker
	ledger Ledger
	now    func() time.Time
}

// New constructs an Engine. taint/ledger may be nil in tests that do not
// exercise the paths needing them (a nil taint checker makes every
// ProvenanceUserAsserted candidate fail closed to TierAgentDerived,
// exactly as a missing session_taint ROW already would - see
// assignSourceTier; a nil ledger simply skips ledgering, matching every
// other "unwired dependency" convention in this codebase).
func New(store Store, taintChecker TaintChecker, ledger Ledger) *Engine {
	return &Engine{store: store, taint: taintChecker, ledger: ledger, now: time.Now}
}

// SetClock overrides Engine's clock (tests only).
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

func (e *Engine) nowRFC3339() string { return e.now().UTC().Format(time.RFC3339Nano) }

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
func nullInt64(i int64) sql.NullInt64    { return sql.NullInt64{Int64: i, Valid: i != 0} }

// validateCandidate implements the schema/length/charclass validation
// step (HANDOFF S5 safety#2) plus the extractor_ver-required rule (task
// spec step 2).
func validateCandidate(c *Candidate) error {
	if strings.TrimSpace(c.Subject) == "" {
		return errors.New("factengine: subject is required")
	}
	if strings.TrimSpace(c.Predicate) == "" {
		return errors.New("factengine: predicate is required")
	}
	if strings.TrimSpace(c.Object) == "" {
		return errors.New("factengine: object is required")
	}
	if strings.TrimSpace(c.ExtractorVer) == "" {
		return errors.New("factengine: extractor_ver is required")
	}
	if err := validateFreeText(c.Subject, MaxFieldRunes); err != nil {
		return fmt.Errorf("factengine: subject: %w", err)
	}
	if err := validateFreeText(c.Predicate, MaxFieldRunes); err != nil {
		return fmt.Errorf("factengine: predicate: %w", err)
	}
	if err := validateFreeText(c.Object, MaxFieldRunes); err != nil {
		return fmt.Errorf("factengine: object: %w", err)
	}
	if err := validateFreeText(c.Evidence, MaxFieldRunes); err != nil {
		return fmt.Errorf("factengine: evidence: %w", err)
	}
	return nil
}

// validateFreeText enforces the length+charclass cap (HANDOFF S5
// safety#2): valid UTF-8, at most maxRunes runes, no C0/DEL control
// characters other than plain tab/newline.
func validateFreeText(s string, maxRunes int) error {
	if !utf8.ValidString(s) {
		return errors.New("invalid UTF-8")
	}
	n := 0
	for _, r := range s {
		n++
		if n > maxRunes {
			return fmt.Errorf("exceeds %d-rune cap", maxRunes)
		}
		if r == 0x7f || (r < 0x20 && r != '\n' && r != '\t') {
			return fmt.Errorf("contains control character %U", r)
		}
	}
	return nil
}

// NormalizeEvidentiality validates the evidentiality enum, defaulting an
// empty value to Inferred (HANDOFF S5 schema block: "the engine ...
// defaults to inferred when absent, fail-conservative").
func NormalizeEvidentiality(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Inferred, nil
	}
	switch s {
	case Witnessed, Reported, Inferred:
		return s, nil
	default:
		return "", fmt.Errorf("factengine: invalid evidentiality %q", s)
	}
}

// assignSourceTier is the Go-side provenance decision (HANDOFF S5 memory
// #1, package doc): it NEVER reads c.ClaimedSourceTier for the assigned
// value - only to detect+report a mismatch worth ledgering. reason is ""
// when the assigned tier matches what the candidate/provenance implied
// (nothing to ledger); otherwise it names why a clamp happened.
func (e *Engine) assignSourceTier(ctx context.Context, c Candidate) (assigned, reason string) {
	prov := c.Provenance
	if prov == "" {
		prov = ProvenanceAgentDerived
	}

	switch prov {
	case ProvenanceUserAsserted:
		if c.SessionID == "" {
			return TierAgentDerived, "user_asserted_claim_missing_session_id"
		}
		tier := taint.TierTainted // fail-closed default if no checker wired
		var err error
		if e.taint != nil {
			tier, err = e.taint.Get(ctx, c.SessionID)
		}
		if err != nil || tier != taint.TierClean {
			return TierAgentDerived, "user_asserted_claim_untrusted_session"
		}
		return TierUserAsserted, tierClampReason(c, TierUserAsserted)
	case ProvenanceExternalDoc:
		return TierExternalDoc, tierClampReason(c, TierExternalDoc)
	case ProvenanceScreen:
		return TierScreen, tierClampReason(c, TierScreen)
	default:
		return TierAgentDerived, tierClampReason(c, TierAgentDerived)
	}
}

// tierClampReason reports whether c.ClaimedSourceTier (an extractor's own
// say-so, ALWAYS ignored for the actual assignment) disagrees with
// assigned - the "the model cannot mint trust" forensic signal.
func tierClampReason(c Candidate, assigned string) string {
	if c.ClaimedSourceTier != "" && c.ClaimedSourceTier != assigned {
		return "extractor_claimed_tier_ignored"
	}
	return ""
}

func (e *Engine) ledgerTierClamp(ctx context.Context, traceID string, c Candidate, assigned, reason string) {
	if e.ledger == nil {
		return
	}
	_ = e.ledger.LogEvent(ctx, traceID, EventTierClamped, map[string]any{
		"subject":              c.Subject,
		"predicate":            c.Predicate,
		"object":               c.Object,
		"claimed_source_tier":  c.ClaimedSourceTier,
		"assigned_source_tier": assigned,
		"provenance":           string(c.Provenance),
		"extractor_ver":        c.ExtractorVer,
		"reason":               reason,
	})
}

// WriteFact is kahyad's single fact-write path (package doc). It
// validates c, assigns source_tier from Go-side provenance (never from
// c.ClaimedSourceTier), finds-or-creates the (subject,predicate,object)
// fact, records one deduped evidence row, and recomputes confidence from
// the fact's full evidence trail (no noisy-OR - see recomputeConfidence).
func (e *Engine) WriteFact(ctx context.Context, traceID string, c Candidate) (int64, error) {
	if err := validateCandidate(&c); err != nil {
		return 0, err
	}
	evidentiality, err := NormalizeEvidentiality(c.Evidentiality)
	if err != nil {
		return 0, err
	}

	assigned, clampReason := e.assignSourceTier(ctx, c)
	if clampReason != "" {
		e.ledgerTierClamp(ctx, traceID, c, assigned, clampReason)
	}

	delta, ok := tierDelta(assigned)
	if !ok {
		return 0, fmt.Errorf("factengine: tier %q has no defined evidence weight", assigned)
	}

	now := e.nowRFC3339()
	fact, err := e.store.GetActiveFactByTriple(ctx, sqlcgen.GetActiveFactByTripleParams{
		Subject: c.Subject, Predicate: c.Predicate, Object: c.Object,
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		fact, err = e.store.InsertFact(ctx, sqlcgen.InsertFactParams{
			Subject: c.Subject, Predicate: c.Predicate, Object: c.Object,
			SourceTier:    assigned,
			Evidentiality: evidentiality,
			Confidence:    0, // set for real by recomputeConfidence below
			Importance:    c.Importance,
			Status:        "active",
			Evidence:      nullString(c.Evidence),
			ExtractorVer:  nullString(c.ExtractorVer),
			UpdatedAt:     now,
			CreatedAt:     now,
		})
		if err != nil {
			return 0, fmt.Errorf("factengine: insert fact: %w", err)
		}
	case err != nil:
		return 0, fmt.Errorf("factengine: lookup fact: %w", err)
	}

	if err := e.addEvidence(ctx, fact.ID, c.EpisodeID, c.SessionID, 1, delta); err != nil {
		return 0, fmt.Errorf("factengine: add evidence for fact %d: %w", fact.ID, err)
	}
	if err := e.recomputeConfidence(ctx, fact.ID); err != nil {
		return 0, fmt.Errorf("factengine: recompute confidence for fact %d: %w", fact.ID, err)
	}
	return fact.ID, nil
}

// addEvidence inserts one evidence row, deduped per (fact_id, session_id,
// polarity) - HANDOFF S5 memory #3: "ayni-oturum tekrari tek kanit
// sayilir". A candidate with no session_id (sessionID == "") is never
// deduped (SQL's own NULL != NULL semantics already make the dedupe
// SELECT below match nothing for a NULL session_id, which is the safe
// default: fewer things to conflate, not fewer evidence rows).
// isUniqueViolation reports whether err is a SQLite UNIQUE/PK constraint
// violation (the shape mattn/go-sqlite3 surfaces) - mirrors
// kahyad/internal/task.isUniqueConstraintViolation one package over.
func isUniqueViolation(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqlite3.ErrConstraint
	}
	return false
}

func (e *Engine) addEvidence(ctx context.Context, factID, episodeID int64, sessionID string, polarity int64, weight float64) error {
	sessCol := nullString(sessionID)
	if sessionID != "" {
		_, err := e.store.GetEvidenceByFactSessionPolarity(ctx, sqlcgen.GetEvidenceByFactSessionPolarityParams{
			FactID: factID, SessionID: sessCol, Polarity: polarity,
		})
		if err == nil {
			return nil // already evidenced by this session at this polarity
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("dedupe check: %w", err)
		}
	}
	_, err := e.store.InsertEvidence(ctx, sqlcgen.InsertEvidenceParams{
		FactID: factID, EpisodeID: nullInt64(episodeID), SessionID: sessCol,
		Polarity: polarity, Weight: weight, CreatedAt: e.nowRFC3339(),
	})
	// The SELECT above is only an early-out; idx_evidence_one_per_session_
	// polarity (migrations/0014) is the real guarantee. Two concurrent taps
	// of the same ritual button (telegram callbacks are NOT serialized) can
	// both pass that SELECT, but only one INSERT wins - the loser hits the
	// unique index and is treated as the same-session repeat it is
	// ("ayni-oturum tekrari tek kanit"), not an error. NULL-session rows are
	// outside the partial index and never take this path.
	if err != nil && sessionID != "" && isUniqueViolation(err) {
		return nil
	}
	return err
}

// recomputeConfidence implements HANDOFF S5 memory #3's "noisy-OR ratchet
// yok: evidence sums, per-session deduped, tier cap clamps" as an
// ACCUMULATE-THEN-CLAMP: it sums each DISTINCT piece of evidence's
// contribution and clamps only the FINAL total to the highest positive
// tier cap represented (the dedupe defensively re-applied here too, even
// though addEvidence already prevents a duplicate row from existing).
//
// The two polarities dedupe on DIFFERENT keys, on purpose:
//
//   - NEGATIVE (denial) rows dedupe by (session) so a same-session repeat is
//     one denial while independent-session denials ACCUMULATE downward -
//     each fresh session's denial adds another DenialLogOdds. Their sum is
//     negSum.
//
//   - POSITIVE (supporting) rows dedupe by (tier weight, session): a
//     same-tier same-session repeat is one observation, but the same tier
//     from a DISTINCT session counts again. A positive-cap tier
//     (user_asserted/external_doc/screen, weight >= 0) then contributes its
//     cap ONCE PER DISTINCT SESSION, so re-affirming a fact after it was
//     denied can raise confidence back up (a denied-but-still-active fact
//     recovers - DenyFact's contract). A NEGATIVE-weight tier (agent_derived,
//     logit(0.4) < 0) instead SATURATES at a single instance: piling on more
//     agent_derived observations must never drive confidence further DOWN
//     past that tier's own ceiling (the W5-04 review's #2/#3, preserved).
//     Their sum is posSum.
//
// confidence = posSum + negSum, then clamped DOWN to maxCap (the highest
// positive-tier weight seen) if it would exceed it - an upside clamp only,
// no floor, and the positive subtotal is NOT clamped before the negatives
// are added (clamping it first would re-freeze the positive contribution
// and re-break recovery).
func (e *Engine) recomputeConfidence(ctx context.Context, factID int64) error {
	rows, err := e.store.ListEvidenceByFact(ctx, factID)
	if err != nil {
		return err
	}

	seen := make(map[string]bool, len(rows))
	negSum := 0.0
	// posSessionsByTier counts the DISTINCT sessions that supplied each
	// positive tier weight (after per-(tier,session) dedupe).
	posSessionsByTier := make(map[float64]int)
	maxCap := 0.0
	hasCap := false
	for _, r := range rows {
		if r.Polarity > 0 {
			// Dedup positive rows by (tier weight, session): the tier's weight
			// IS its cap, so equal weights are the same tier; a distinct
			// session counts again, enabling recovery via re-affirmation.
			sk := r.SessionID.String
			if !r.SessionID.Valid || sk == "" {
				// No session context (e.g. hot-window promotion) - key by the
				// row's own identity so distinct observations are not collapsed.
				sk = "row:" + strconv.FormatInt(r.ID, 10)
			}
			key := "pos|" + strconv.FormatFloat(r.Weight, 'f', -1, 64) + "|" + sk
			if seen[key] {
				continue
			}
			seen[key] = true
			posSessionsByTier[r.Weight]++
			if !hasCap || r.Weight > maxCap {
				maxCap = r.Weight
				hasCap = true
			}
		} else {
			// Dedup negative rows by session so independent-session denials
			// accumulate while same-session repeats collapse to one.
			sk := r.SessionID.String
			if !r.SessionID.Valid || sk == "" {
				sk = "row:" + strconv.FormatInt(r.ID, 10)
			}
			key := "neg|" + sk
			if seen[key] {
				continue
			}
			seen[key] = true
			negSum += r.Weight
		}
	}

	posSum := 0.0
	for tierWeight, sessions := range posSessionsByTier {
		if tierWeight >= 0 {
			// Positive-cap tier ACCUMULATES across distinct sessions so
			// re-affirmation offsets denials.
			posSum += tierWeight * float64(sessions)
		} else {
			// Negative-weight tier (agent_derived) SATURATES at one instance.
			posSum += tierWeight
		}
	}

	confidence := posSum + negSum
	if hasCap && confidence > maxCap {
		confidence = maxCap
	}
	return e.store.UpdateFactConfidence(ctx, sqlcgen.UpdateFactConfidenceParams{
		Confidence: confidence, UpdatedAt: e.nowRFC3339(), ID: factID,
	})
}

// ConfirmFact implements `kahya fact confirm <id>` (or a W5-03 ritual
// Dogru answer): lifts the agent_derived quarantine half of
// InjectionEligible. Deliberately never touches source_tier/confidence -
// an agent_derived fact's confidence stays capped at that tier's ceiling
// forever (HANDOFF S5 memory #1).
func (e *Engine) ConfirmFact(ctx context.Context, traceID string, factID int64) error {
	if err := e.store.ConfirmFact(ctx, sqlcgen.ConfirmFactParams{
		ConfirmedAt: sql.NullString{String: e.nowRFC3339(), Valid: true},
		UpdatedAt:   e.nowRFC3339(),
		ID:          factID,
	}); err != nil {
		return fmt.Errorf("factengine: confirm fact %d: %w", factID, err)
	}
	if e.ledger != nil {
		_ = e.ledger.LogEvent(ctx, traceID, EventFactConfirmed, map[string]any{"fact_id": factID})
	}
	return nil
}

// DenyFact records a user denial (HANDOFF S5 memory #3: "kullanici reddi
// ... guveni dusurur") as a fixed -2.94 negative evidence row and
// recomputes confidence - it does NOT close the fact (unlike RetractFact
// in retract.go): a denied-but-still-active fact can still be reasserted
// later and its confidence can recover, whereas a RETRACTED fact is
// closed for good.
func (e *Engine) DenyFact(ctx context.Context, traceID string, factID int64, sessionID string) error {
	if err := e.addEvidence(ctx, factID, 0, sessionID, -1, DenialLogOdds); err != nil {
		return fmt.Errorf("factengine: deny fact %d: %w", factID, err)
	}
	if err := e.recomputeConfidence(ctx, factID); err != nil {
		return fmt.Errorf("factengine: recompute confidence for fact %d: %w", factID, err)
	}
	if e.ledger != nil {
		_ = e.ledger.LogEvent(ctx, traceID, EventFactDenied, map[string]any{"fact_id": factID, "session_id": sessionID})
	}
	return nil
}

// GetFact exposes the narrow read the CLI (`kahya fact confirm/retract`)
// and server route handlers need, without giving callers the whole Store
// surface.
func (e *Engine) GetFact(ctx context.Context, id int64) (sqlcgen.Fact, error) {
	return e.store.GetFact(ctx, id)
}

// InjectionEligible is THE injection-eligibility predicate (task spec
// deliverable, exported for W12-05 to swap into memory_search/injection):
//
//	eligible <=> status=active AND log-odds >= InjectionThresholdLogOdds
//	             AND (tier != agent_derived OR confirmed)
func InjectionEligible(f sqlcgen.Fact) bool {
	if f.Status != "active" {
		return false
	}
	if f.Confidence < InjectionThresholdLogOdds {
		return false
	}
	return TierInjectionEligible(f.SourceTier, f.ConfirmedAt.Valid)
}

// TierInjectionEligible is the TIER-ONLY half of InjectionEligible
// (HANDOFF S5 memory #1's quarantine rule alone, without the log-odds/
// status halves that only a facts.Fact row carries) - reusable by any
// caller that filters by source_tier without a full Fact row to hand,
// e.g. kahyad/internal/server's /v1/memory/search for_injection filter
// over CHUNK/episode hits (which carry only source_tier, no confidence/
// status of their own): eligible <=> tier != agent_derived OR confirmed.
func TierInjectionEligible(tier string, confirmed bool) bool {
	return tier != TierAgentDerived || confirmed
}

// ErrSecretLaneCloudExtraction is returned by GuardCloudExtraction when
// text is secret-lane classified - the caller MUST NOT proceed to a
// cloud-routed extractor call (HANDOFF S4 ordering invariant: no byte
// reaches a cloud model before secret-lane classification has completed
// locally). Its message IS the Turkish fail-closed notice
// (secretlane.MsgSecretLaneCloudBlocked) so a caller surfacing this error
// straight to the user needs no further translation.
var ErrSecretLaneCloudExtraction = errors.New(secretlane.MsgSecretLaneCloudBlocked)

// GuardCloudExtraction classifies text (kahyad/internal/secretlane's
// deterministic-pre-pass-then-local-Qwen classifier) and fails closed
// with ErrSecretLaneCloudExtraction whenever it is secret-lane content -
// the fact-extraction call site (a future W12-09 cloud-routed extractor
// session, or any other candidate-producing code path) MUST call this
// BEFORE routing text to claude-haiku-4-5 and must never proceed past a
// non-nil error here. A nil error means either the deterministic pre-pass
// found nothing AND classification is not required at all, or the local
// classifier itself ran and found the content ordinary - either way, safe
// to extract normally.
func GuardCloudExtraction(ctx context.Context, classifier *secretlane.Classifier, text string) error {
	if classifier == nil {
		// No classifier wired: ClassifyDeterministic alone is exactly as
		// strong a guarantee for the regex/lexicon hits it covers (IBAN/
		// TCKN/card/keyword) and never needs a live Qwen dependency -
		// matching that function's own doc comment on why it, not
		// Classifier.Classify, is the right call for a caller with no
		// classifier instance to hand.
		if secretlane.ClassifyDeterministic(text).SecretLane {
			return ErrSecretLaneCloudExtraction
		}
		return nil
	}
	verdict, _ := classifier.Classify(ctx, text)
	if verdict.SecretLane {
		return ErrSecretLaneCloudExtraction
	}
	return nil
}
