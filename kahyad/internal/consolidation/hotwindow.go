// hotwindow.go implements HANDOFF §5 memory #4 (quoted verbatim in the
// task spec):
//
//	90+ gun sicak pencere + ayrinti-atomu: 48 saat degil >=90 gun.
//	Sogutmadan once sayi/tarih/alinti/karar/soz'ler yapilandirilmis
//	olgulara terfi. Her ozet HAM KANITTAN uretilir, asla alt-ozetten.
//
// PromoteHotWindow scans every active, not-yet-cooled episode older than
// HotWindowDays, extracts detail atoms (numbers/dates/quotes/decisions-
// and-promises) from its RAW chunk text, writes each as a quarantined
// agent_derived candidate fact citing that raw episode/chunk as evidence
// (kahyad's own fact-write path - this is Go code inside kahyad itself,
// never the consolidation session/worker, so it does not violate the
// WRITE BOUNDARY invariant the SESSION is held to: HANDOFF's own carve-out
// is that kahyad, the sole writer of brain.db, may always write brain.db;
// what must never happen is the untrusted/toolless consolidation SESSION
// touching it directly - session.go's own doc comment), and ONLY THEN
// marks the episode cooled - so a crash between promotion and the cooled
// stamp simply re-promotes on the next run rather than silently losing an
// episode's atoms forever.
//
// ValidateSummaryEvidence is the Go-side guard for the OTHER half of the
// §5 memory #4 rule ("her ozet ham kanittan uretilir, asla alt-ozetten"):
// it rejects any evidence list that cites a prior summary rather than a
// raw episode/chunk - every fact PromoteHotWindow inserts is built from
// exactly two EvidenceRefs (its own episode + chunk), so this call can
// never actually fail in this file; it exists as the single, reusable,
// independently-tested enforcement point kahyad/internal/consolidation's
// OWN markdown-merge summaries are held to as well (a merged topic file
// citing "the file I merged yesterday" instead of the original raw notes
// would fail this same check).
package consolidation

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// nullString converts s to sql.NullString, Valid only when s is non-empty
// (matches every other nullable-TEXT-column convention already used
// throughout kahyad/internal/store's own generated code).
func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// HotWindowDays is HANDOFF §5 memory #4's fixed threshold ("48 saat degil
// >=90 gun").
const HotWindowDays = 90

// HotWindowExtractorVersion is facts.extractor_ver for every fact this
// file writes - bump this constant (never silently) if the detail-atom
// extraction rules below ever change, so existing rows stay attributable
// to the rules that actually produced them.
const HotWindowExtractorVersion = "hotwindow-v1"

// quarantinedSourceTier / inferredEvidentiality are the two fixed fact
// fields every hot-window candidate carries (HANDOFF §5 memory #1: agent-
// derived facts are quarantined from the profile card/injection until a
// human confirms them - kahyad/internal/server's own quarantinedSourceTier
// constant is the enforcement point that reads this same string back out).
const (
	agentDerivedSourceTier = "agent_derived"
	inferredEvidentiality  = "inferred"
)

// --- detail-atom extraction ---

// AtomKind names one of the four §5 memory #4 detail-atom categories
// (sayi/tarih/alinti/karar-soz), transliterated to English per CLAUDE.md's
// "code/identifiers: English" rule - these are internal predicate/log
// values, never user-facing strings.
type AtomKind string

const (
	AtomNumber            AtomKind = "contains_number"
	AtomDate              AtomKind = "contains_date"
	AtomQuote             AtomKind = "contains_quote"
	AtomDecisionOrPromise AtomKind = "contains_decision_or_promise"
)

// DetailAtom is one extracted atom: Kind + the exact substring extracted.
type DetailAtom struct {
	Kind AtomKind
	Text string
}

// currencyAmountRe extracts a number anchored to a currency marker
// (Turkish Lira sign/abbreviation, USD/EUR symbols) - a bare number with
// no currency/unit context is noise at this granularity, so this
// package deliberately does NOT extract every stray digit run, only
// amounts (task spec category "sayi" - numbers meaningful enough to
// promote to a structured fact).
var currencyAmountRe = regexp.MustCompile(`\b\d+(?:[.,]\d+)?\s?(?:TL|USD|EUR|₺|\$|€)\b`)

// dateRe matches the common date shapes this codebase's own memory notes
// use: DD.MM.YYYY, DD/MM/YYYY, or YYYY-MM-DD.
var dateRe = regexp.MustCompile(`\b\d{1,2}[./]\d{1,2}[./]\d{4}\b|\b\d{4}-\d{2}-\d{2}\b`)

// quoteRe matches text wrapped in Turkish curly quotes ("..." - U+201C/
// U+201D) or plain straight double quotes.
var quoteRe = regexp.MustCompile(`"([^"]{2,300})"|\x{201C}([^\x{201D}]{2,300})\x{201D}`)

// decisionOrPromiseKeywords is a small, representative (never exhaustive
// - tasks/README.md forbids hand-rolled morphology, this is a lexicon,
// same posture as kahyad/internal/secretlane's own keyword tables) set of
// Turkish decision/promise cue phrases. A sentence containing one of
// these is extracted whole.
var decisionOrPromiseKeywords = []string{
	"karar verdim", "karar verdik", "karar aldım", "karar aldık",
	"söz veriyorum", "söz veriyorum ki", "taahhüt ediyorum",
}

// ExtractDetailAtoms scans text (one raw chunk's content) for every
// detail-atom category and returns them all, in a stable order (numbers,
// dates, quotes, decisions/promises) so promotion output is deterministic
// for a given input.
func ExtractDetailAtoms(text string) []DetailAtom {
	var atoms []DetailAtom
	for _, m := range currencyAmountRe.FindAllString(text, -1) {
		atoms = append(atoms, DetailAtom{Kind: AtomNumber, Text: m})
	}
	for _, m := range dateRe.FindAllString(text, -1) {
		atoms = append(atoms, DetailAtom{Kind: AtomDate, Text: m})
	}
	for _, m := range quoteRe.FindAllStringSubmatch(text, -1) {
		quoted := m[1]
		if quoted == "" {
			quoted = m[2]
		}
		atoms = append(atoms, DetailAtom{Kind: AtomQuote, Text: quoted})
	}
	for _, sentence := range splitSentences(text) {
		lower := strings.ToLower(sentence)
		for _, kw := range decisionOrPromiseKeywords {
			if strings.Contains(lower, kw) {
				atoms = append(atoms, DetailAtom{Kind: AtomDecisionOrPromise, Text: strings.TrimSpace(sentence)})
				break
			}
		}
	}
	return atoms
}

// splitSentences is a deliberately simple sentence splitter (on '.', '!',
// '?') - good enough for cue-phrase extraction, not a linguistic
// sentence boundary detector.
func splitSentences(text string) []string {
	return regexp.MustCompile(`[.!?]+`).Split(text, -1)
}

// --- evidence rule: never a prior summary ---

// EvidenceKind names what an EvidenceRef points at.
type EvidenceKind string

const (
	EvidenceEpisode EvidenceKind = "episode"
	EvidenceChunk   EvidenceKind = "chunk"
	// EvidenceSummary marks a reference to a PRIOR consolidated/merged
	// output rather than raw capture-moment data - ValidateSummaryEvidence
	// rejects any evidence list containing one of these.
	EvidenceSummary EvidenceKind = "summary"
)

// EvidenceRef is one citation a fact/summary's evidence is built from.
type EvidenceRef struct {
	Kind EvidenceKind
	ID   int64
}

// ErrSummaryFromSummary is returned by ValidateSummaryEvidence.
var ErrSummaryFromSummary = fmt.Errorf("consolidation: evidence must cite raw episode/chunk data, never a prior summary")

// ValidateSummaryEvidence rejects refs containing an EvidenceSummary
// entry (HANDOFF §5 memory #4: "her ozet ham kanittan uretilir, asla
// alt-ozetten").
func ValidateSummaryEvidence(refs []EvidenceRef) error {
	for _, r := range refs {
		if r.Kind == EvidenceSummary {
			return ErrSummaryFromSummary
		}
	}
	return nil
}

// formatEvidence renders refs as facts.evidence's free-form TEXT value
// ("episode:12,chunk:34").
func formatEvidence(refs []EvidenceRef) string {
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = fmt.Sprintf("%s:%d", r.Kind, r.ID)
	}
	return strings.Join(parts, ",")
}

// --- promotion pipeline ---

// Episode is the narrow episode shape hot-window promotion needs -
// deliberately NOT kahyad/internal/store/sqlcgen's generated row type, so
// this package's own tests can drive PromoteHotWindow against a trivial
// in-memory FactStore fake with no brain.db dependency at all.
type Episode struct {
	ID        int64
	CreatedAt time.Time
}

// Chunk is the narrow chunk shape hot-window promotion needs.
type Chunk struct {
	ID   int64
	Text string
}

// CandidateFact is one hot-window-promoted fact, ready for FactStore.
// InsertFact. SourceTier/Evidentiality are always agentDerivedSourceTier/
// inferredEvidentiality (PromoteHotWindow sets both; CandidateFact simply
// carries whatever the caller put there, same as every other field).
type CandidateFact struct {
	Subject       string
	Predicate     string
	Object        string
	SourceTier    string
	Evidentiality string
	Confidence    float64
	Importance    float64
	Evidence      string
	ExtractorVer  string
}

// FactStore is the narrow brain.db seam PromoteHotWindow needs -
// StoreFactWriter (below) adapts *kahyad/internal/store/sqlcgen.Queries;
// tests inject an in-memory fake instead.
type FactStore interface {
	ListUncooledEpisodesOlderThan(ctx context.Context, cutoff time.Time) ([]Episode, error)
	ListChunksByEpisode(ctx context.Context, episodeID int64) ([]Chunk, error)
	InsertFact(ctx context.Context, f CandidateFact) (int64, error)
	MarkEpisodeCooled(ctx context.Context, episodeID int64, at time.Time) error
}

// PromoteHotWindow runs one hot-window pass: every active, uncooled
// episode with created_at <= now-HotWindowDays gets its raw chunk text
// scanned for detail atoms, each promoted to a quarantined agent_derived
// fact (evidence = that exact episode+chunk, never a prior summary -
// enforced via ValidateSummaryEvidence before every InsertFact call), and
// ONLY THEN is the episode marked cooled. Returns the total fact count
// promoted across every episode this pass touched.
func PromoteHotWindow(ctx context.Context, store FactStore, now time.Time) (int, error) {
	cutoff := now.AddDate(0, 0, -HotWindowDays)
	episodes, err := store.ListUncooledEpisodesOlderThan(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("consolidation: list uncooled episodes: %w", err)
	}

	promoted := 0
	for _, ep := range episodes {
		chunks, err := store.ListChunksByEpisode(ctx, ep.ID)
		if err != nil {
			return promoted, fmt.Errorf("consolidation: list chunks for episode %d: %w", ep.ID, err)
		}
		for _, ch := range chunks {
			for _, atom := range ExtractDetailAtoms(ch.Text) {
				refs := []EvidenceRef{{Kind: EvidenceEpisode, ID: ep.ID}, {Kind: EvidenceChunk, ID: ch.ID}}
				if err := ValidateSummaryEvidence(refs); err != nil {
					return promoted, err
				}
				fact := CandidateFact{
					Subject:       fmt.Sprintf("episode:%d", ep.ID),
					Predicate:     string(atom.Kind),
					Object:        atom.Text,
					SourceTier:    agentDerivedSourceTier,
					Evidentiality: inferredEvidentiality,
					Confidence:    0, // log-odds 0 == p=0.5: an unconfirmed candidate, deliberately neutral
					Importance:    0,
					Evidence:      formatEvidence(refs),
					ExtractorVer:  HotWindowExtractorVersion,
				}
				if _, err := store.InsertFact(ctx, fact); err != nil {
					return promoted, fmt.Errorf("consolidation: insert candidate fact for episode %d: %w", ep.ID, err)
				}
				promoted++
			}
		}
		if err := store.MarkEpisodeCooled(ctx, ep.ID, now); err != nil {
			return promoted, fmt.Errorf("consolidation: mark episode %d cooled: %w", ep.ID, err)
		}
	}
	return promoted, nil
}

// StoreFactWriter adapts *sqlcgen.Queries to FactStore - the production
// implementation.
type StoreFactWriter struct {
	Q *sqlcgen.Queries
}

var _ FactStore = StoreFactWriter{}

func (w StoreFactWriter) ListUncooledEpisodesOlderThan(ctx context.Context, cutoff time.Time) ([]Episode, error) {
	rows, err := w.Q.ListUncooledEpisodesOlderThan(ctx, cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	out := make([]Episode, 0, len(rows))
	for _, r := range rows {
		createdAt, err := time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil {
			// A malformed created_at is this codebase's own bug, not a
			// caller error - fail-safe by simply skipping the row (never
			// promote off a timestamp we cannot trust) rather than aborting
			// the whole pass.
			continue
		}
		out = append(out, Episode{ID: r.ID, CreatedAt: createdAt})
	}
	return out, nil
}

func (w StoreFactWriter) ListChunksByEpisode(ctx context.Context, episodeID int64) ([]Chunk, error) {
	rows, err := w.Q.ListChunksByEpisode(ctx, episodeID)
	if err != nil {
		return nil, err
	}
	out := make([]Chunk, len(rows))
	for i, r := range rows {
		out[i] = Chunk{ID: r.ID, Text: r.Text}
	}
	return out, nil
}

func (w StoreFactWriter) InsertFact(ctx context.Context, f CandidateFact) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	row, err := w.Q.InsertFact(ctx, sqlcgen.InsertFactParams{
		Subject:       f.Subject,
		Predicate:     f.Predicate,
		Object:        f.Object,
		SourceTier:    f.SourceTier,
		Evidentiality: f.Evidentiality,
		Confidence:    f.Confidence,
		Importance:    f.Importance,
		Status:        "active",
		Evidence:      nullString(f.Evidence),
		ExtractorVer:  nullString(f.ExtractorVer),
		UpdatedAt:     now,
		CreatedAt:     now,
	})
	if err != nil {
		return 0, err
	}
	return row.ID, nil
}

func (w StoreFactWriter) MarkEpisodeCooled(ctx context.Context, episodeID int64, at time.Time) error {
	return w.Q.MarkEpisodeCooled(ctx, sqlcgen.MarkEpisodeCooledParams{
		CooledAt: nullString(at.UTC().Format(time.RFC3339)),
		ID:       episodeID,
	})
}
