// retract.go implements HANDOFF S5 memory #3's retraction half: "Artik
// sevmiyorum" -> geri-cekme" (deterministic Turkish retraction cue
// detection) plus fact closure (valid_to set, status=retracted, a
// negative evidence row) - NEVER a DELETE, matching the two-temporal
// (valid_from/valid_to) columns' whole reason for existing (HANDOFF S5
// schema block). Every retraction records its kind - ended (the fact held
// and no longer does: its window stays queryable history) or corrected
// (the fact was wrong: never shown again) - and its own time (migration
// 0027, FIX-FINAL fact-history-ended-vs-corrected).
package factengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/textnorm"
)

// ErrNoActiveFact is returned by RetractFact when no active fact matches
// the given (subject, predicate, object) triple - there is nothing to
// close.
var ErrNoActiveFact = errors.New("factengine: no active fact matches subject/predicate/object")

// retractionFoldAll folds every entry of words via textnorm.Fold, mirroring
// kahyad/internal/secretlane's lexicon pre-folding (its unexported entries
// helper; kept as an independent copy here rather than imported).
func retractionFoldAll(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = textnorm.Fold(w)
	}
	return out
}

// correctionPhrases and endedPhrases are explicit, representative (never
// exhaustive - tasks/README.md forbids hand-rolled morphology tables)
// Turkish retraction cue phrases, folded via textnorm.Fold. Both the
// proper-diacritic and common ASCII-substituted spelling are listed for
// every entry containing g/s/c-with-cedilla-or-breve letters Fold does
// NOT touch (ğ/ş/ç - only the I-family folds, see textnorm.Fold's own
// doc comment) - the SAME dual-spelling convention kahyad/internal/
// secretlane's own keyword lexicon uses.
//
// FIX-FINAL fact-history-ended-vs-corrected (owner decision 2026-10-16)
// splits them by what the user means: a CORRECTION says the fact was
// wrong (it never held); an ENDED cue says it held and no longer does.
var (
	correctionPhrases = retractionFoldAll([]string{
		"düzeltiyorum", "duzeltiyorum",
		"yanlış hatırlıyorsun", "yanlis hatirliyorsun",
		"hayır, öyle değil", "hayir, oyle degil",
		"hayır yanlış", "hayir yanlis",
	})
	endedPhrases = retractionFoldAll([]string{
		"artık sevmiyorum", "artik sevmiyorum",
	})
)

// RetractionKind records why a fact was retracted (facts.retraction_kind,
// migration 0027). Every retraction path names one.
type RetractionKind string

const (
	// RetractionEnded: the fact HELD and no longer does ("artık ... değil",
	// an extractor answer with retraction:true, `kahya fact retract
	// --sona-erdi`). Its validity window stays queryable as history: an
	// as_of / known_as_of fact_query returns it, labelled.
	RetractionEnded RetractionKind = "ended"
	// RetractionCorrected: the fact was WRONG ("düzeltiyorum", "yanlış
	// hatırlıyorsun", `kahya fact retract` by default, a fact the user
	// denied). It is never returned again, not even as history. A
	// pre-0027 retraction (retraction_kind NULL) counts as corrected.
	RetractionCorrected RetractionKind = "corrected"
)

// ErrUnknownRetractionKind is returned for a kind other than
// RetractionEnded / RetractionCorrected.
var ErrUnknownRetractionKind = errors.New("factengine: retraction kind must be ended or corrected")

// ParseRetractionKind validates s as a RetractionKind (exact spelling).
func ParseRetractionKind(s string) (RetractionKind, error) {
	switch k := RetractionKind(s); k {
	case RetractionEnded, RetractionCorrected:
		return k, nil
	default:
		return "", fmt.Errorf("%w (got %q)", ErrUnknownRetractionKind, s)
	}
}

// Retraction is RetractFact's result: the closed fact and the kind that was
// actually recorded (an ENDED request on a fact the user had denied is
// recorded as corrected - see RetractFact).
type Retraction struct {
	FactID int64
	Kind   RetractionKind
}

// ClassifyRetraction reports whether text matches a deterministic
// retraction pattern (HANDOFF S5 memory #3) and which kind it is:
//
//   - a correction phrase (correctionPhrases) => RetractionCorrected;
//   - otherwise an ended phrase (endedPhrases), or the general "artik ...
//     degil" shape (task spec: "artik ... degil") - text containing BOTH
//     cue words anywhere, not necessarily adjacent, since "artik" and
//     "degil" can be separated by the retracted predicate itself (e.g.
//     "Kahveyi artik sevmiyorum" has no literal "degil" at all - covered by
//     the explicit phrase instead - while "Artik onun arkadasi degilim"
//     splits the two cue words around the retracted claim) =>
//     RetractionEnded.
//
// A text carrying both shapes is a correction: the safe reading (a
// corrected fact is never shown again; an ended one is shown as history).
func ClassifyRetraction(text string) (RetractionKind, bool) {
	folded := textnorm.Fold(text)
	for _, kw := range correctionPhrases {
		if strings.Contains(folded, kw) {
			return RetractionCorrected, true
		}
	}
	for _, kw := range endedPhrases {
		if strings.Contains(folded, kw) {
			return RetractionEnded, true
		}
	}
	hasArtik := strings.Contains(folded, "artik")
	hasDegil := strings.Contains(folded, "değil") || strings.Contains(folded, "degil")
	if hasArtik && hasDegil {
		return RetractionEnded, true
	}
	return "", false
}

// DetectRetraction reports whether text matches a deterministic
// retraction/correction pattern of either kind (ClassifyRetraction).
func DetectRetraction(text string) bool {
	_, ok := ClassifyRetraction(text)
	return ok
}

// RetractFact closes the ACTIVE fact matching (subject, predicate,
// object): status='retracted', valid_to set (never a DELETE), plus a
// negative evidence row from sessionID (HANDOFF S5 memory #3's own
// "gercek yanginin ancak insan reddiyle sondurulmesi" posture: a
// retraction IS a negative evidence event, not merely a status flip).
// Returns ErrNoActiveFact if no active fact matches the triple - there is
// nothing to retract.
//
// kind (migration 0027, FIX-FINAL fact-history-ended-vs-corrected) is
// recorded with the retraction's own time (retracted_at):
//
//   - RetractionEnded keeps the fact's validity window as history:
//     valid_to becomes the retraction time, or stays at an EARLIER valid_to
//     the fact already carried (the fact ended then, not now). An ended
//     request on a fact that carries a user denial (DenyFact: `kahya fact
//     deny`, a ritual Yanlış answer) is recorded as RetractionCorrected -
//     the user said it was wrong, so it never shows as history. The store's
//     guarded UPDATE decides this atomically with the denial check (a
//     denial cannot slip in between a read and the write); a denial landing
//     after the UPDATE is caught by DenyFact, which re-marks the row.
//   - RetractionCorrected sets valid_to to the retraction time.
//
// The returned Retraction.Kind is the kind the UPDATE recorded. Any other
// kind is ErrUnknownRetractionKind and nothing is written.
func (e *Engine) RetractFact(ctx context.Context, traceID, subject, predicate, object, sessionID string, kind RetractionKind) (Retraction, error) {
	requested, err := ParseRetractionKind(string(kind))
	if err != nil {
		return Retraction{}, err
	}
	fact, err := e.store.GetActiveFactByTriple(ctx, sqlcgen.GetActiveFactByTripleParams{
		Subject: subject, Predicate: predicate, Object: object,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Retraction{}, ErrNoActiveFact
		}
		return Retraction{}, fmt.Errorf("factengine: retract lookup: %w", err)
	}

	nowT := e.now().UTC()
	now := nowT.Format(time.RFC3339Nano)
	endedValidTo := now
	if fact.ValidTo.Valid {
		if t, err := time.Parse(time.RFC3339Nano, fact.ValidTo.String); err == nil && t.Before(nowT) {
			endedValidTo = fact.ValidTo.String
		}
	}
	got, err := e.store.RetractFact(ctx, sqlcgen.RetractFactParams{
		RequestedKind: string(requested),
		EndedValidTo:  endedValidTo,
		RetractedAt:   now,
		UpdatedAt:     now,
		ID:            fact.ID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		// A concurrent retraction closed it between the lookup and here.
		return Retraction{}, ErrNoActiveFact
	}
	if err != nil {
		return Retraction{}, fmt.Errorf("factengine: retract fact %d: %w", fact.ID, err)
	}
	recorded, err := ParseRetractionKind(got.String)
	if err != nil {
		// The CHECK constraint makes this unreachable; never report a kind
		// the row does not carry.
		return Retraction{}, fmt.Errorf("factengine: retract fact %d recorded kind: %w", fact.ID, err)
	}

	if err := e.addEvidence(ctx, fact.ID, 0, sessionID, -1, DenialLogOdds); err != nil {
		return Retraction{}, fmt.Errorf("factengine: retract evidence for fact %d: %w", fact.ID, err)
	}
	if err := e.recomputeConfidence(ctx, fact.ID); err != nil {
		return Retraction{}, fmt.Errorf("factengine: recompute confidence for retracted fact %d: %w", fact.ID, err)
	}

	if e.ledger != nil {
		payload := map[string]any{
			"fact_id": fact.ID, "subject": subject, "predicate": predicate, "object": object,
			"kind": string(recorded),
		}
		if recorded != requested {
			payload["requested_kind"] = string(requested)
			payload["kind_reason"] = "user_denial"
		}
		_ = e.ledger.LogEvent(ctx, traceID, EventFactRetracted, payload)
	}
	return Retraction{FactID: fact.ID, Kind: recorded}, nil
}

// IsEnded reports whether f was retracted as RetractionEnded (it held and
// no longer does). A corrected fact, or a pre-0027 retraction with no kind
// on file, is not.
func IsEnded(f sqlcgen.Fact) bool {
	return f.Status == "retracted" && f.RetractionKind.Valid && f.RetractionKind.String == string(RetractionEnded)
}

// IsHistory reports whether f - carrying negativeEvidence negative
// evidence rows (evidence.polarity < 0) - may be shown as history at all:
// an ENDED retraction whose ONLY negative row is its retraction's own.
// RetractFact records an ended kind only on a fact with no negative row
// and adds exactly one; every further negative row is a user denial
// (DenyFact), and a denial means corrected (DenyFact re-marks the row).
// So any other count - a denial whose re-mark failed or never ran, or a
// retraction whose own evidence row is missing - is treated as corrected:
// never shown (fail-closed). This is the read side's half of the
// ended-vs-corrected rule; the structured reads call it before anything
// else, and neither include_quarantined nor a lane can reopen it.
func IsHistory(f sqlcgen.Fact, negativeEvidence int64) bool {
	return IsEnded(f) && negativeEvidence == 1
}

// HistoryEligible is the trust predicate for a history fact in the
// structured reads (graph.QueryFacts with as_of / known_as_of): the
// counterpart of InjectionEligible for a fact that is no longer active. It
// never makes a fact injectable - InjectionEligible stays status=active
// only, so an ended fact never reaches <hafiza> or a profile card.
//
//	eligible <=> IsHistory(f, negativeEvidence)
//	             AND log-odds without the retraction's own negative row
//	                 (confidence - DenialLogOdds) >= InjectionThresholdLogOdds
//	             AND (tier in {user_edit, user_asserted} OR confirmed)
//
// IsHistory pins the fact's negative evidence to exactly the retraction's
// own row, so the confidence half discounts exactly that row: it holds for
// every ended fact whose positive evidence held. A fact failing IsHistory
// is never eligible (and is never shown, even as quarantined - see
// IsHistory).
func HistoryEligible(f sqlcgen.Fact, negativeEvidence int64) bool {
	if !IsHistory(f, negativeEvidence) {
		return false
	}
	if f.Confidence-DenialLogOdds < InjectionThresholdLogOdds {
		return false
	}
	return TierInjectionEligible(f.SourceTier, f.ConfirmedAt.Valid)
}
