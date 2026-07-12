// retract.go implements HANDOFF S5 memory #3's retraction half: "Artik
// sevmiyorum" -> geri-cekme" (deterministic Turkish retraction cue
// detection) plus fact closure (valid_to=now, status=retracted, a
// negative evidence row) - NEVER a DELETE, matching the two-temporal
// (valid_from/valid_to) columns' whole reason for existing (HANDOFF S5
// schema block).
package factengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/textnorm"
)

// ErrNoActiveFact is returned by RetractFact when no active fact matches
// the given (subject, predicate, object) triple - there is nothing to
// close.
var ErrNoActiveFact = errors.New("factengine: no active fact matches subject/predicate/object")

// retractionFoldAll folds every entry of words via textnorm.Fold, mirroring
// kahyad/internal/secretlane's/consolidation's own foldAll helpers (kept
// as an independent copy here rather than imported - each of those is
// unexported in its own package).
func retractionFoldAll(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = textnorm.Fold(w)
	}
	return out
}

// retractionPhrases are explicit, representative (never exhaustive -
// tasks/README.md forbids hand-rolled morphology tables) Turkish
// retraction/correction cue phrases, folded via textnorm.Fold. Both the
// proper-diacritic and common ASCII-substituted spelling are listed for
// every entry containing g/s/c-with-cedilla-or-breve letters Fold does
// NOT touch (ğ/ş/ç - only the I-family folds, see textnorm.Fold's own
// doc comment) - the SAME dual-spelling convention kahyad/internal/
// secretlane's own keyword lexicon uses.
var retractionPhrases = retractionFoldAll([]string{
	"artık sevmiyorum", "artik sevmiyorum",
	"düzeltiyorum", "duzeltiyorum",
	"yanlış hatırlıyorsun", "yanlis hatirliyorsun",
	"hayır, öyle değil", "hayir, oyle degil",
	"hayır yanlış", "hayir yanlis",
})

// DetectRetraction reports whether text matches a deterministic
// retraction/correction pattern (HANDOFF S5 memory #3): either an
// explicit phrase from retractionPhrases, or the general "artik ...
// degil" shape (task spec: "artik ... degil") - text containing BOTH cue
// words anywhere, not necessarily adjacent, since "artik" and "degil" can
// be separated by the retracted predicate itself (e.g. "Kahveyi artik
// sevmiyorum" has no literal "degil" at all - covered by the explicit
// phrase above instead - while "Artik onun arkadasi degilim" splits the
// two cue words around the retracted claim).
func DetectRetraction(text string) bool {
	folded := textnorm.Fold(text)
	for _, kw := range retractionPhrases {
		if strings.Contains(folded, kw) {
			return true
		}
	}
	hasArtik := strings.Contains(folded, "artik")
	hasDegil := strings.Contains(folded, "değil") || strings.Contains(folded, "degil")
	return hasArtik && hasDegil
}

// RetractFact closes the ACTIVE fact matching (subject, predicate,
// object): status='retracted', valid_to=now (never a DELETE), plus a
// negative evidence row from sessionID (HANDOFF S5 memory #3's own
// "gercek yanginin ancak insan reddiyle sondurulmesi" posture: a
// retraction IS a negative evidence event, not merely a status flip).
// Returns ErrNoActiveFact if no active fact matches the triple - there is
// nothing to retract.
func (e *Engine) RetractFact(ctx context.Context, traceID, subject, predicate, object, sessionID string) (int64, error) {
	fact, err := e.store.GetActiveFactByTriple(ctx, sqlcgen.GetActiveFactByTripleParams{
		Subject: subject, Predicate: predicate, Object: object,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNoActiveFact
		}
		return 0, fmt.Errorf("factengine: retract lookup: %w", err)
	}

	now := e.nowRFC3339()
	if err := e.store.RetractFact(ctx, sqlcgen.RetractFactParams{
		ValidTo: sql.NullString{String: now, Valid: true}, UpdatedAt: now, ID: fact.ID,
	}); err != nil {
		return 0, fmt.Errorf("factengine: retract fact %d: %w", fact.ID, err)
	}

	if err := e.addEvidence(ctx, fact.ID, 0, sessionID, -1, DenialLogOdds); err != nil {
		return 0, fmt.Errorf("factengine: retract evidence for fact %d: %w", fact.ID, err)
	}
	if err := e.recomputeConfidence(ctx, fact.ID); err != nil {
		return 0, fmt.Errorf("factengine: recompute confidence for retracted fact %d: %w", fact.ID, err)
	}

	if e.ledger != nil {
		_ = e.ledger.LogEvent(ctx, traceID, EventFactRetracted, map[string]any{
			"fact_id": fact.ID, "subject": subject, "predicate": predicate, "object": object,
		})
	}
	return fact.ID, nil
}
