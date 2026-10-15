// Package ritual implements W5-03: the weekly "truth ritual" (HANDOFF S6
// W5 flag, verbatim: "Haftalik dogru/yanlis rituelinin hafif surumu
// (Telegram'dan ~10 olguluk 'bu dogru mu?', W3 botunu yeniden kullanir)
// W5'te baslar; W7-8 eval kumesinin etiketleri buradan gelir").
//
// Engine.Run picks up to select.go's MaxQuestionsPerRun eligible facts
// (the sampling POLICY lives entirely in select.go - this file is send +
// answer plumbing only), mints one eval_labels row per fact under a SINGLE
// trace_id, and sends each question through Delivery (the W3-07 Telegram
// bot, kept fully decoupled here - this package never imports
// kahyad/internal/telegram, matching kahyad/internal/briefing's identical
// Delivery-interface posture).
//
// Engine.Answer is the callback-side handler a Telegram button tap
// drives: the FIRST answer to a question binds (FIX-P-16, decision G09-4):
// it claims the eval_labels row in SQL (never a second row for the same
// question, never a second answer - HANDOFF S5 memory #3: "ayni-oturum
// tekrari tek kanit sayilir"), and applies the log-odds update THROUGH
// kahyad/internal/factengine - the single fact-write path (HANDOFF S4:
// kahyad is brain.db's only writer) - never a private, duplicated
// log-odds implementation:
//
//   - Dogru (true): factengine.WriteFact at user_asserted weight (a
//     direct, allowlisted Telegram button tap from THIS ritual's own run
//     session IS a direct user assertion - Run registers that session
//     clean via TaintRegistrar so WriteFact's own taint gate honors it),
//     THEN factengine.ConfirmFact unconditionally - the human-confirmation
//     marker that lifts an agent_derived fact's quarantine
//     (kahyad/internal/factengine.InjectionEligible), engine.go's own doc
//     comment names this exact call site.
//   - Yanlis (false): factengine.DenyFact - the fixed -2.94 negative
//     evidence row (HANDOFF S5 memory #3: "kullanici reddi guveni
//     dusurur").
//   - Emin degilim (unsure): no evidence row at all - the label is
//     recorded, confidence is untouched.
package ritual

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/taint"
)

// Label values Answer accepts - identical to eval_labels.label's own CHECK
// constraint (migrations/0013_eval_labels.sql).
const (
	LabelTrue   = "true"
	LabelFalse  = "false"
	LabelUnsure = "unsure"
)

// ChannelRemote is every ritual answer's channel - this ritual is
// Telegram-only (HANDOFF S5 safety #5: "Telegram-kaynakli onaylar
// defterde remote etiketli").
const ChannelRemote = "remote"

// ExtractorVer is the factengine.Candidate.ExtractorVer sentinel this
// package's own WriteFact calls carry - factengine's package doc comment
// names exactly this value as the non-extractor-call-site convention.
const ExtractorVer = "truth_ritual_v1"

// ExpiryWindow is the 72h unanswered-question expiry (task spec
// deliverable, verbatim: "unanswered questions expire after 72h").
const ExpiryWindow = 72 * time.Hour

// Ledger event kinds this package appends (HANDOFF S5 safety #4).
const (
	EventAsked         = "ritual.asked"
	EventAnswered      = "ritual.answered"
	EventExpiredAnswer = "ritual.expired_answer"
	// EventAnswerConflict (FIX-P-16): a tap with a DIFFERENT label on an
	// already-answered question, rejected.
	EventAnswerConflict = "ritual.answer_conflict"
)

// AlreadyAnsweredError is returned by Answer when the question was already
// answered with a DIFFERENT label (FIX-P-16). kahyad/internal/telegram
// detects it structurally (interface{ AlreadyAnswered() (string, bool) })
// without importing this package - the same no-cross-import posture as
// RitualAnswerer.
type AlreadyAnsweredError struct{ Previous, Attempted string }

func (e AlreadyAnsweredError) Error() string {
	return fmt.Sprintf("ritual: already answered %q, attempted %q", e.Previous, e.Attempted)
}

// AlreadyAnswered reports the binding (previous) label.
func (e AlreadyAnsweredError) AlreadyAnswered() (previous string, ok bool) { return e.Previous, true }

// FactEngine is the narrow kahyad/internal/factengine.Engine surface
// Answer drives - *factengine.Engine satisfies it directly, with no
// adapter.
type FactEngine interface {
	WriteFact(ctx context.Context, traceID string, c factengine.Candidate) (int64, error)
	ConfirmFact(ctx context.Context, traceID string, factID int64) error
	DenyFact(ctx context.Context, traceID string, factID int64, sessionID string) error
	GetFact(ctx context.Context, id int64) (sqlcgen.Fact, error)
}

var _ FactEngine = (*factengine.Engine)(nil)

// EvalLabelStore is the narrow eval_labels read/write surface this
// package needs. *sqlcgen.Queries (via *store.Store) satisfies it
// directly, with no adapter. ClaimEvalLabelAnswer is the atomic
// first-answer-wins write; ResetEvalLabelAnswer undoes a claim whose
// evidence write failed (FIX-P-16).
type EvalLabelStore interface {
	InsertEvalLabel(ctx context.Context, arg sqlcgen.InsertEvalLabelParams) (sqlcgen.EvalLabel, error)
	GetEvalLabel(ctx context.Context, id int64) (sqlcgen.EvalLabel, error)
	ClaimEvalLabelAnswer(ctx context.Context, arg sqlcgen.ClaimEvalLabelAnswerParams) (int64, error)
	ResetEvalLabelAnswer(ctx context.Context, id int64) error
}

var _ EvalLabelStore = (*sqlcgen.Queries)(nil)

// TaintRegistrar is the narrow W4-03 taint-tier write surface Run uses to
// register its OWN run trace_id as a clean session BEFORE any answer ever
// arrives - *taint.Tracker satisfies it directly. This is what makes a
// later Dogru answer's factengine.WriteFact call (Answer, below) actually
// honor Provenance=ProvenanceUserAsserted at its full user_asserted
// weight, rather than failing closed to agent_derived for lack of any
// taint record at all (factengine's own assignSourceTier doc comment).
// The registered session_id is scoped to ONLY this evidence-write
// purpose - it is never handed to a worker, never seeded with untrusted
// content, and never reused for anything else - so this is a legitimate,
// narrowly-scoped fourth session_taint "birth place" alongside the three
// kahyad/internal/taint's own package doc comment already names (a direct,
// allowlisted Telegram button tap IS a direct user assertion, exactly the
// provenance user_asserted represents).
type TaintRegistrar interface {
	InsertClean(ctx context.Context, traceID, sessionID string) error
}

var _ TaintRegistrar = (*taint.Tracker)(nil)

// Ledger is the append-only events sink this package writes to (HANDOFF
// S5 safety #4). *kahyad/internal/store.Store already has exactly this
// method shape.
type Ledger interface {
	LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error
}

// Delivery is the narrow "send one ritual question, with its answer
// buttons, through the W3-05-gated Telegram send path" surface Run needs
// - kahyad/internal/telegram.Bot.SendRitualQuestion satisfies this
// directly (mirrors kahyad/internal/briefing.Delivery's identical
// decoupling: this package never imports kahyad/internal/telegram).
// Returns true iff the question actually reached Telegram (false on a
// gate-denied/failed send - Run still records the eval_labels row and
// ledgers ritual.asked either way, exactly mirroring how
// kahyad/internal/briefing.Orchestrator treats its own Delivery result).
type Delivery interface {
	SendRitualQuestion(ctx context.Context, traceID string, evalLabelID int64, factText string) bool
}

// Engine ties the sampler, eval_labels store, factengine, taint
// registrar, ledger, and Telegram delivery together (W5-03). Construct
// one with New per kahyad process.
type Engine struct {
	sampler  *Sampler
	store    EvalLabelStore
	fact     FactEngine
	taintReg TaintRegistrar
	ledger   Ledger
	delivery Delivery
	now      func() time.Time
}

// New constructs an Engine. delivery/taintReg/ledger may be nil (tests
// that do not exercise those paths) - matches this codebase's usual
// "unwired dependency" posture: a nil delivery makes every question
// resolve to sent=false (still recorded in eval_labels/ledgered); a nil
// taintReg simply skips run-session registration (every Dogru answer then
// falls back to agent_derived weight, matching factengine's own
// documented fail-closed default for an unregistered session); a nil
// ledger simply skips ledgering.
func New(sampler *Sampler, store EvalLabelStore, fact FactEngine, taintReg TaintRegistrar, ledger Ledger, delivery Delivery) *Engine {
	return &Engine{sampler: sampler, store: store, fact: fact, taintReg: taintReg, ledger: ledger, delivery: delivery, now: time.Now}
}

// SetClock overrides Engine's clock (tests only).
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

func (e *Engine) nowRFC3339() string { return e.now().UTC().Format(time.RFC3339Nano) }

// factText renders a fact's (subject, predicate, object) triple as the
// plain-Turkish question body ("Bu dogru mu?\n\n<fact text>"'s own <fact
// text> half) - a straightforward whitespace join, since every field is
// already free Turkish text an extractor (or a direct user assertion)
// produced, not a template needing further grammar. It IS memlane.FactText,
// the text the sampler's secret-lane classification checks (FIX-P-17), so
// the classified text and the sent text can never drift apart.
func factText(f sqlcgen.Fact) string {
	return memlane.FactText(f)
}

// Run selects up to MaxQuestionsPerRun eligible facts (select.go) and
// sends one Telegram question per fact under traceID - every eval_labels
// row this run mints, and every ritual.asked ledger event, shares that
// SAME trace_id. Returns the number of questions that actually reached
// Telegram (asked <= len(selected) <= MaxQuestionsPerRun) - a gate-denied
// or failed send still leaves the eval_labels row on file (answered_at
// NULL, exactly as if it had never been sent) and is still ledgered, but
// does not count toward the returned total.
func (e *Engine) Run(ctx context.Context, traceID string) (asked int, err error) {
	facts, err := e.sampler.Select(ctx, traceID)
	if err != nil {
		return 0, err
	}
	if len(facts) == 0 {
		return 0, nil
	}

	// Register this run's OWN trace_id as a clean taint session ONCE,
	// before any question is even sent - every later Dogru answer's
	// WriteFact call (recordTrue) reuses this SAME trace_id as its
	// SessionID (see TaintRegistrar's own doc comment for why this is a
	// legitimate clean-by-construction session, and factengine's evidence
	// dedup rationale for why sharing one session_id across every fact in
	// this run is exactly right: dedup keys on (fact_id, session_id,
	// polarity), so distinct facts never collide, while a double-tap on
	// the SAME fact+polarity correctly dedupes to one evidence row).
	if e.taintReg != nil {
		if regErr := e.taintReg.InsertClean(ctx, traceID, traceID); regErr != nil && !errors.Is(regErr, taint.ErrLowerAttempt) {
			return 0, fmt.Errorf("ritual: register run session: %w", regErr)
		}
	}

	askedAt := e.nowRFC3339()
	for _, f := range facts {
		qText := factText(f)
		row, insertErr := e.store.InsertEvalLabel(ctx, sqlcgen.InsertEvalLabelParams{
			FactID:       f.ID,
			QuestionText: qText,
			AskedAt:      askedAt,
			Channel:      sql.NullString{String: ChannelRemote, Valid: true},
			TraceID:      traceID,
			CreatedAt:    askedAt,
		})
		if insertErr != nil {
			return asked, fmt.Errorf("ritual: insert eval_label for fact %d: %w", f.ID, insertErr)
		}

		sent := false
		if e.delivery != nil {
			sent = e.delivery.SendRitualQuestion(ctx, traceID, row.ID, qText)
		}
		e.ledgerEvent(ctx, traceID, EventAsked, map[string]any{
			"fact_id": f.ID, "eval_label_id": row.ID, "sent": sent,
		})
		if sent {
			asked++
		}
	}
	return asked, nil
}

// Answer processes one ritual-question button tap identified by
// evalLabelID, applying label (LabelTrue/LabelFalse/LabelUnsure). It
// matches kahyad/internal/telegram.RitualAnswerer's method shape exactly
// (ctx, int64, string) -> (string, bool, error) - Go's structural
// interface satisfaction needs no adapter or import on either side.
//
// Returns the run's own trace_id (so a Telegram toast can egress-check
// under the SAME session an expired/answered card's own SendRitualQuestion
// call used) and expired=true when the 72h window (measured from
// asked_at) had already elapsed - in that case NOTHING else in this
// function runs: no label/answered_at update, no evidence row, no
// confidence change, only a ritual.expired_answer ledger line (task spec
// deliverable, verbatim).
//
// The first answer wins (FIX-P-16, decision G09-4): a repeat of the SAME
// label is an idempotent no-op (nil, nothing written); a DIFFERENT label
// on an answered question is rejected with AlreadyAnsweredError +
// ritual.answer_conflict and writes nothing. The claim happens in SQL
// (ClaimEvalLabelAnswer) BEFORE any evidence write, so two concurrent taps
// can never both write evidence; if the evidence write then fails, the
// claim is reset so the user can tap again.
func (e *Engine) Answer(ctx context.Context, evalLabelID int64, label string) (traceID string, expired bool, err error) {
	if label != LabelTrue && label != LabelFalse && label != LabelUnsure {
		return "", false, fmt.Errorf("ritual: unknown label %q", label)
	}

	row, err := e.store.GetEvalLabel(ctx, evalLabelID)
	if err != nil {
		return "", false, fmt.Errorf("ritual: get eval_label %d: %w", evalLabelID, err)
	}
	traceID = row.TraceID

	askedAt, parseErr := time.Parse(time.RFC3339Nano, row.AskedAt)
	if parseErr == nil && e.now().Sub(askedAt) > ExpiryWindow {
		e.ledgerEvent(ctx, traceID, EventExpiredAnswer, map[string]any{
			"fact_id": row.FactID, "eval_label_id": evalLabelID, "label": label,
		})
		return traceID, true, nil
	}

	if row.AnsweredAt.Valid {
		return traceID, false, e.alreadyAnswered(ctx, traceID, row, label, false)
	}
	n, err := e.store.ClaimEvalLabelAnswer(ctx, sqlcgen.ClaimEvalLabelAnswerParams{
		Label:      sql.NullString{String: label, Valid: true},
		AnsweredAt: sql.NullString{String: e.nowRFC3339(), Valid: true},
		ID:         evalLabelID,
	})
	if err != nil {
		return traceID, false, fmt.Errorf("ritual: claim eval_label %d: %w", evalLabelID, err)
	}
	if n == 0 {
		// Lost a concurrent race: another tap claimed the row between the
		// read above and the claim. Read the winner back.
		again, gerr := e.store.GetEvalLabel(ctx, evalLabelID)
		if gerr != nil {
			return traceID, false, fmt.Errorf("ritual: re-read eval_label %d: %w", evalLabelID, gerr)
		}
		if !again.AnsweredAt.Valid {
			// The winner's evidence write failed and reset its claim in
			// between: nothing is answered, so this is a plain failure
			// (the user taps again), never "already answered".
			return traceID, false, fmt.Errorf("ritual: eval_label %d claim was reset concurrently", evalLabelID)
		}
		return traceID, false, e.alreadyAnswered(ctx, traceID, again, label, true)
	}

	var evErr error
	switch label {
	case LabelTrue:
		evErr = e.recordTrue(ctx, traceID, row)
	case LabelFalse:
		if err := e.fact.DenyFact(ctx, traceID, row.FactID, traceID); err != nil {
			evErr = fmt.Errorf("ritual: deny fact %d: %w", row.FactID, err)
		}
	case LabelUnsure:
		// No evidence write at all - the claimed label is the whole answer.
	}
	if evErr != nil {
		// Let the user retry instead of leaving the question "answered"
		// with no evidence. A retry is safe: factengine dedupes evidence
		// on (fact_id, session_id, polarity).
		if rerr := e.store.ResetEvalLabelAnswer(ctx, evalLabelID); rerr != nil {
			return traceID, false, errors.Join(evErr, fmt.Errorf("ritual: reset eval_label %d: %w", evalLabelID, rerr))
		}
		return traceID, false, evErr
	}

	e.ledgerEvent(ctx, traceID, EventAnswered, map[string]any{
		"fact_id": row.FactID, "eval_label_id": evalLabelID, "label": label, "channel": ChannelRemote,
	})
	return traceID, false, nil
}

// alreadyAnswered decides a tap on an answered question (row is the
// current row): the SAME label is an idempotent no-op (nil); a different
// one ledgers ritual.answer_conflict and returns AlreadyAnsweredError.
// race marks the case where the row was claimed by a concurrent tap.
func (e *Engine) alreadyAnswered(ctx context.Context, traceID string, row sqlcgen.EvalLabel, label string, race bool) error {
	if row.Label.Valid && row.Label.String == label {
		return nil
	}
	payload := map[string]any{
		"fact_id": row.FactID, "eval_label_id": row.ID, "previous": row.Label.String, "attempted": label,
	}
	if race {
		payload["race"] = true
	}
	e.ledgerEvent(ctx, traceID, EventAnswerConflict, payload)
	return AlreadyAnsweredError{Previous: row.Label.String, Attempted: label}
}

// recordTrue implements the Dogru half of Answer (package doc comment):
// positive evidence at user_asserted weight THROUGH factengine.WriteFact,
// then factengine.ConfirmFact unconditionally (harmless on a non-
// agent_derived fact; lifts an agent_derived fact's quarantine).
func (e *Engine) recordTrue(ctx context.Context, traceID string, row sqlcgen.EvalLabel) error {
	fact, err := e.fact.GetFact(ctx, row.FactID)
	if err != nil {
		return fmt.Errorf("ritual: get fact %d: %w", row.FactID, err)
	}
	_, err = e.fact.WriteFact(ctx, traceID, factengine.Candidate{
		Subject:       fact.Subject,
		Predicate:     fact.Predicate,
		Object:        fact.Object,
		Provenance:    factengine.ProvenanceUserAsserted,
		SessionID:     traceID,
		Evidentiality: factengine.Witnessed,
		ExtractorVer:  ExtractorVer,
		Evidence:      fmt.Sprintf("ritual:%d", row.ID),
	})
	if err != nil {
		return fmt.Errorf("ritual: write positive evidence for fact %d: %w", row.FactID, err)
	}
	if err := e.fact.ConfirmFact(ctx, traceID, row.FactID); err != nil {
		return fmt.Errorf("ritual: confirm fact %d: %w", row.FactID, err)
	}
	return nil
}

func (e *Engine) ledgerEvent(ctx context.Context, traceID, kind string, payload map[string]any) {
	if e.ledger == nil {
		return
	}
	_ = e.ledger.LogEvent(ctx, traceID, kind, payload)
}
