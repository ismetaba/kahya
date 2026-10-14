package ritual

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

func newTestEngine(t *testing.T, st *store.Store, delivery Delivery) *Engine {
	t.Helper()
	sampler := NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil)
	fact := factengine.New(st.Queries, newTestTaint(st), st)
	return New(sampler, st.Queries, fact, newTestTaint(st), st, delivery)
}

func seedEligibleFact(t *testing.T, st *store.Store, n int) (fact struct{ ID int64 }) {
	t.Helper()
	ep := seedEpisode(t, st, fmt.Sprintf("notlar/gunluk-%d.md", n))
	f := seedFact(t, st, fmt.Sprintf("olgu-%d", n), "yuklem", "nesne", "user_asserted", 1.0, false)
	seedClassifyingEvidence(t, st, f.ID, ep)
	return struct{ ID int64 }{ID: f.ID}
}

// TestRunSendsUpToTenAndSharesOneTraceID: the core ask-step acceptance
// criterion - a seeded facts table (>10 eligible) yields <=10 sent
// questions; eval_labels rows with answered_at IS NULL match the number
// sent; every row (and every ritual.asked ledger line) shares ONE
// trace_id.
func TestRunSendsUpToTenAndSharesOneTraceID(t *testing.T) {
	st := newTestStore(t)
	for i := 0; i < 15; i++ {
		seedEligibleFact(t, st, i)
	}
	delivery := &fakeDelivery{sendResult: true}
	engine := newTestEngine(t, st, delivery)

	traceID := "run-trace-0000000000000000000001"
	asked, err := engine.Run(context.Background(), traceID)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if asked != MaxQuestionsPerRun {
		t.Fatalf("Run() asked = %d, want %d", asked, MaxQuestionsPerRun)
	}

	rows, err := st.Queries.ListEvalLabelsByTrace(context.Background(), traceID)
	if err != nil {
		t.Fatalf("ListEvalLabelsByTrace: %v", err)
	}
	if len(rows) != MaxQuestionsPerRun {
		t.Fatalf("eval_labels rows for trace = %d, want %d", len(rows), MaxQuestionsPerRun)
	}
	for _, r := range rows {
		if r.AnsweredAt.Valid {
			t.Fatalf("eval_label %d answered_at is set right after asking, want NULL", r.ID)
		}
		if r.TraceID != traceID {
			t.Fatalf("eval_label %d trace_id = %q, want %q", r.ID, r.TraceID, traceID)
		}
	}
	if got := countEventsOfKind(t, st, EventAsked); got != MaxQuestionsPerRun {
		t.Fatalf("ritual.asked events = %d, want %d", got, MaxQuestionsPerRun)
	}
	if len(delivery.calls) != MaxQuestionsPerRun {
		t.Fatalf("SendRitualQuestion calls = %d, want %d", len(delivery.calls), MaxQuestionsPerRun)
	}
	for _, c := range delivery.calls {
		if c.TraceID != traceID {
			t.Fatalf("SendRitualQuestion traceID = %q, want %q", c.TraceID, traceID)
		}
	}
}

// TestRunGateDeniedSendsNothingButStillLedgersAsked: when Delivery itself
// reports every send as denied (simulating the W3-05 egress gate stubbed
// to DENY), Run's returned "asked" count is 0 and no message is recorded
// as sent, yet the eval_labels rows and ritual.asked ledger lines still
// exist (asking is independent of whether the send itself got through).
func TestRunGateDeniedSendsNothingButStillLedgersAsked(t *testing.T) {
	st := newTestStore(t)
	seedEligibleFact(t, st, 0)
	delivery := &fakeDelivery{sendResult: false}
	engine := newTestEngine(t, st, delivery)

	traceID := "run-trace-0000000000000000000002"
	asked, err := engine.Run(context.Background(), traceID)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if asked != 0 {
		t.Fatalf("Run() asked = %d, want 0 (gate denied every send)", asked)
	}
	if len(delivery.calls) != 1 {
		t.Fatalf("SendRitualQuestion calls = %d, want 1 (still attempted)", len(delivery.calls))
	}
	rows, err := st.Queries.ListEvalLabelsByTrace(context.Background(), traceID)
	if err != nil {
		t.Fatalf("ListEvalLabelsByTrace: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("eval_labels rows = %d, want 1 (asked is recorded regardless of send result)", len(rows))
	}
}

// TestAnswerYanlisLowersConfidenceAndExitsInjection: a Yanlis answer
// inserts a negative-polarity evidence row and lowers log-odds
// confidence; once driven below the 0.3 threshold, InjectionEligible
// flips to false (the exact predicate memory_search's own injection
// filter is documented to consult).
func TestAnswerYanlisLowersConfidenceAndExitsInjection(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/gunluk.md")
	fact := seedFact(t, st, "Emre", "sever", "kahve", "user_asserted", 0.5, false)
	seedClassifyingEvidence(t, st, fact.ID, ep)
	if !factengine.InjectionEligible(getFact(t, st, fact.ID)) {
		t.Fatal("fixture fact should start injection-eligible")
	}

	delivery := &fakeDelivery{sendResult: true}
	engine := newTestEngine(t, st, delivery)
	traceID := "run-trace-0000000000000000000003"
	if _, err := engine.Run(context.Background(), traceID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	rows, err := st.Queries.ListEvalLabelsByTrace(context.Background(), traceID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListEvalLabelsByTrace: rows=%v err=%v", rows, err)
	}
	evalLabelID := rows[0].ID

	before := getFact(t, st, fact.ID)
	gotTrace, expired, err := engine.Answer(context.Background(), evalLabelID, LabelFalse)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if expired {
		t.Fatal("Answer() reported expired on a fresh question")
	}
	if gotTrace != traceID {
		t.Fatalf("Answer() traceID = %q, want %q", gotTrace, traceID)
	}

	after := getFact(t, st, fact.ID)
	if after.Confidence >= before.Confidence {
		t.Fatalf("confidence after Yanlis = %v, want < %v (before)", after.Confidence, before.Confidence)
	}
	if n := countEvidenceRows(t, st, fact.ID); n == 0 {
		t.Fatal("Yanlis inserted no evidence row at all")
	}
	var negRows int
	if err := st.DB().QueryRow(`SELECT count(*) FROM evidence WHERE fact_id = ? AND polarity = -1`, fact.ID).Scan(&negRows); err != nil {
		t.Fatalf("count negative evidence: %v", err)
	}
	if negRows != 1 {
		t.Fatalf("negative-polarity evidence rows = %d, want 1", negRows)
	}
	if after.Confidence >= factengine.InjectionThresholdLogOdds {
		// Fixture starts at 0.5 (log-odds 0) and the fixed denial delta is
		// -2.94, so it lands well below -0.847 in one Yanlis - guard this
		// assumption explicitly rather than silently passing on a
		// still-eligible confidence by coincidence.
		t.Fatalf("fixture confidence %v did not fall below the injection threshold %v after one Yanlis", after.Confidence, factengine.InjectionThresholdLogOdds)
	}
	if factengine.InjectionEligible(after) {
		t.Fatal("InjectionEligible() = true after confidence fell below threshold, want false")
	}
}

// TestAnswerDogruOnQuarantinedAgentDerivedLiftsInjectionEligibility: a
// Dogru answer on a quarantined (unconfirmed) agent_derived fact makes it
// injection-eligible, and the ritual.answered ledger event carries the
// remote channel label.
func TestAnswerDogruOnQuarantinedAgentDerivedLiftsInjectionEligibility(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/gunluk.md")
	fact := seedFact(t, st, "Emre", "sever", "kahve", "agent_derived", -0.2, false)
	seedClassifyingEvidence(t, st, fact.ID, ep)
	if factengine.InjectionEligible(getFact(t, st, fact.ID)) {
		t.Fatal("fixture fact should start NOT injection-eligible (quarantined)")
	}

	delivery := &fakeDelivery{sendResult: true}
	engine := newTestEngine(t, st, delivery)
	traceID := "run-trace-0000000000000000000004"
	if _, err := engine.Run(context.Background(), traceID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	rows, err := st.Queries.ListEvalLabelsByTrace(context.Background(), traceID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListEvalLabelsByTrace: rows=%v err=%v", rows, err)
	}
	evalLabelID := rows[0].ID

	if _, expired, err := engine.Answer(context.Background(), evalLabelID, LabelTrue); err != nil || expired {
		t.Fatalf("Answer() error=%v expired=%v", err, expired)
	}

	after := getFact(t, st, fact.ID)
	if !after.ConfirmedAt.Valid {
		t.Fatal("fact.ConfirmedAt not set after Dogru answer")
	}
	if !factengine.InjectionEligible(after) {
		t.Fatal("InjectionEligible() = false after Dogru confirmation, want true")
	}

	var payload string
	if err := st.DB().QueryRow(`SELECT payload FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1`, EventAnswered).Scan(&payload); err != nil {
		t.Fatalf("read ritual.answered payload: %v", err)
	}
	if !containsSubstr(payload, `"channel":"remote"`) {
		t.Fatalf("ritual.answered payload = %s, want it to carry channel=remote", payload)
	}
}

// TestAnswerDoubleTapSameButtonYieldsOneEvidenceRow: tapping the SAME
// button twice on the same question inserts exactly one evidence row for
// that fact+ritual run (HANDOFF S5 memory #3: "ayni-oturum tekrari tek
// kanit sayilir") - the repeat of the SAME label is a no-op (FIX-P-16).
func TestAnswerDoubleTapSameButtonYieldsOneEvidenceRow(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/gunluk.md")
	fact := seedFact(t, st, "Emre", "sever", "kahve", "user_asserted", 0.5, false)
	seedClassifyingEvidence(t, st, fact.ID, ep)

	delivery := &fakeDelivery{sendResult: true}
	engine := newTestEngine(t, st, delivery)
	traceID := "run-trace-0000000000000000000005"
	if _, err := engine.Run(context.Background(), traceID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	rows, err := st.Queries.ListEvalLabelsByTrace(context.Background(), traceID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListEvalLabelsByTrace: rows=%v err=%v", rows, err)
	}
	evalLabelID := rows[0].ID
	before := countEvidenceRows(t, st, fact.ID)

	if _, expired, err := engine.Answer(context.Background(), evalLabelID, LabelTrue); err != nil || expired {
		t.Fatalf("first Answer(): error=%v expired=%v", err, expired)
	}
	afterFirst := countEvidenceRows(t, st, fact.ID)
	if afterFirst != before+1 {
		t.Fatalf("evidence rows after first Dogru = %d, want %d", afterFirst, before+1)
	}

	if _, expired, err := engine.Answer(context.Background(), evalLabelID, LabelTrue); err != nil || expired {
		t.Fatalf("second Answer(): error=%v expired=%v", err, expired)
	}
	afterSecond := countEvidenceRows(t, st, fact.ID)
	if afterSecond != afterFirst {
		t.Fatalf("evidence rows after SECOND (repeat) Dogru = %d, want still %d", afterSecond, afterFirst)
	}
}

// TestAnswerExpiredChangesNothing: a callback arriving after the 72h
// expiry window changes nothing (label stays NULL/answered_at stays NULL,
// zero evidence rows, confidence unchanged) and ledgers
// ritual.expired_answer.
func TestAnswerExpiredChangesNothing(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/gunluk.md")
	fact := seedFact(t, st, "Emre", "sever", "kahve", "user_asserted", 0.5, false)
	seedClassifyingEvidence(t, st, fact.ID, ep)

	delivery := &fakeDelivery{sendResult: true}
	engine := newTestEngine(t, st, delivery)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	engine.SetClock(func() time.Time { return base })

	traceID := "run-trace-0000000000000000000006"
	if _, err := engine.Run(context.Background(), traceID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	rows, err := st.Queries.ListEvalLabelsByTrace(context.Background(), traceID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListEvalLabelsByTrace: rows=%v err=%v", rows, err)
	}
	evalLabelID := rows[0].ID
	beforeConfidence := getFact(t, st, fact.ID).Confidence
	beforeEvidenceRows := countEvidenceRows(t, st, fact.ID)

	// Jump the clock 73h forward - past ExpiryWindow.
	engine.SetClock(func() time.Time { return base.Add(73 * time.Hour) })

	gotTrace, expired, err := engine.Answer(context.Background(), evalLabelID, LabelTrue)
	if err != nil {
		t.Fatalf("Answer() error = %v", err)
	}
	if !expired {
		t.Fatal("Answer() expired = false, want true (73h > 72h window)")
	}
	if gotTrace != traceID {
		t.Fatalf("Answer() traceID = %q, want %q", gotTrace, traceID)
	}

	row, err := st.Queries.GetEvalLabel(context.Background(), evalLabelID)
	if err != nil {
		t.Fatalf("GetEvalLabel: %v", err)
	}
	if row.AnsweredAt.Valid {
		t.Fatal("answered_at got set on an expired callback")
	}
	if row.Label.Valid {
		t.Fatal("label got set on an expired callback")
	}
	if n := countEvidenceRows(t, st, fact.ID); n != beforeEvidenceRows {
		t.Fatalf("evidence rows after expired callback = %d, want unchanged %d", n, beforeEvidenceRows)
	}
	afterConfidence := getFact(t, st, fact.ID).Confidence
	if afterConfidence != beforeConfidence {
		t.Fatalf("confidence changed after expired callback: %v -> %v", beforeConfidence, afterConfidence)
	}
	if got := countEventsOfKind(t, st, EventExpiredAnswer); got != 1 {
		t.Fatalf("ritual.expired_answer events = %d, want 1", got)
	}
}

func containsSubstr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestRunNeverDeliversSecretLaneFactText (FIX-P-17): a fact whose evidence
// sits under an ORDINARY path but whose TEXT carries an IBAN is never sent
// to Telegram, opens no eval_labels row, and its exclusion is ledgered
// (content_deterministic) under the run's trace_id.
func TestRunNeverDeliversSecretLaneFactText(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/genel.md") // outside testSecretGlobs
	f := seedFact(t, st, "Ahmet'in IBAN'i", "su", "TR33 0006 1005 1978 6457 8413 26", "user_asserted", 1.0, false)
	seedClassifyingEvidence(t, st, f.ID, ep)
	delivery := &fakeDelivery{sendResult: true}
	engine := newTestEngine(t, st, delivery)
	engine.sampler.SetLedger(st)

	const traceID = "run-trace-p17-00000000000000000001"
	asked, err := engine.Run(context.Background(), traceID)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if asked != 0 || len(delivery.calls) != 0 {
		t.Fatalf("Run() asked = %d, deliveries = %+v; want 0 and none (secret-lane fact text)", asked, delivery.calls)
	}
	rows, err := st.Queries.ListEvalLabelsByTrace(context.Background(), traceID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("eval_labels rows = %d (err %v), want 0", len(rows), err)
	}
	if got := countEventsOfKind(t, st, EventFactExcludedSecretLane); got != 1 {
		t.Fatalf("%s events = %d, want 1", EventFactExcludedSecretLane, got)
	}
	ex := excludedRows(t, st)
	if len(ex) != 1 || ex[0].reason != ExcludeReasonContentDeterministic || ex[0].traceID != traceID || ex[0].factID != f.ID {
		t.Fatalf("exclusion rows = %+v, want one {fact %d, %s, %s}", ex, f.ID, ExcludeReasonContentDeterministic, traceID)
	}
}

// ---- FIX-P-16: the first answer binds (decision G09-4) ----

// runOneQuestion seeds one eligible fact at confidence, runs engine under
// traceID and returns the fact id and the single eval_labels row id.
func runOneQuestion(t *testing.T, st *store.Store, engine *Engine, traceID string, confidence float64) (factID, evalLabelID int64) {
	t.Helper()
	ep := seedEpisode(t, st, "notlar/gunluk.md")
	fact := seedFact(t, st, "Emre", "sever", "kahve", "user_asserted", confidence, false)
	seedClassifyingEvidence(t, st, fact.ID, ep)
	if _, err := engine.Run(context.Background(), traceID); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	rows, err := st.Queries.ListEvalLabelsByTrace(context.Background(), traceID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListEvalLabelsByTrace: rows=%v err=%v", rows, err)
	}
	return fact.ID, rows[0].ID
}

func evalLabel(t *testing.T, st *store.Store, id int64) sqlcgen.EvalLabel {
	t.Helper()
	row, err := st.Queries.GetEvalLabel(context.Background(), id)
	if err != nil {
		t.Fatalf("GetEvalLabel(%d): %v", id, err)
	}
	return row
}

// TestAnswerDogruThenYanlisRejectedWritesNoNegativeEvidence (FIX-P-16): a
// Yanlis tap after a Dogru answer on the same question is rejected with
// AlreadyAnsweredError and ledgered as ritual.answer_conflict - no
// negative evidence row, no confidence change, the label stays "true".
func TestAnswerDogruThenYanlisRejectedWritesNoNegativeEvidence(t *testing.T) {
	st := newTestStore(t)
	engine := newTestEngine(t, st, &fakeDelivery{sendResult: true})
	factID, evalLabelID := runOneQuestion(t, st, engine, "run-trace-p16-00000000000000000001", 0.5)

	if _, expired, err := engine.Answer(context.Background(), evalLabelID, LabelTrue); err != nil || expired {
		t.Fatalf("first Answer(Dogru): err=%v expired=%v", err, expired)
	}
	before := countEvidenceRows(t, st, factID)
	conf := getFact(t, st, factID).Confidence

	_, expired, err := engine.Answer(context.Background(), evalLabelID, LabelFalse)
	var aa AlreadyAnsweredError
	if !errors.As(err, &aa) {
		t.Fatalf("second Answer(Yanlis) err = %v, want AlreadyAnsweredError", err)
	}
	if aa.Previous != LabelTrue || aa.Attempted != LabelFalse {
		t.Fatalf("AlreadyAnsweredError = %+v, want Previous=true Attempted=false", aa)
	}
	if expired {
		t.Fatal("second Answer reported expired on a fresh question")
	}
	if n := countEvidenceRows(t, st, factID); n != before {
		t.Fatalf("evidence rows after the rejected Yanlis = %d, want unchanged %d", n, before)
	}
	if got := getFact(t, st, factID).Confidence; got != conf {
		t.Fatalf("confidence changed by the rejected Yanlis: %v -> %v", conf, got)
	}
	if row := evalLabel(t, st, evalLabelID); !row.Label.Valid || row.Label.String != LabelTrue {
		t.Fatalf("eval_label label = %+v, want still %q", row.Label, LabelTrue)
	}
	if got := countEventsOfKind(t, st, EventAnswerConflict); got != 1 {
		t.Fatalf("%s events = %d, want 1", EventAnswerConflict, got)
	}
	if got := countEventsOfKind(t, st, EventAnswered); got != 1 {
		t.Fatalf("%s events = %d, want 1", EventAnswered, got)
	}
}

// TestAnswerUnsureThenDogruAlsoRejected (FIX-P-16): "Emin degilim" is an
// answer too - a later Dogru is rejected and writes no evidence.
func TestAnswerUnsureThenDogruAlsoRejected(t *testing.T) {
	st := newTestStore(t)
	engine := newTestEngine(t, st, &fakeDelivery{sendResult: true})
	factID, evalLabelID := runOneQuestion(t, st, engine, "run-trace-p16-00000000000000000002", 0.5)
	before := countEvidenceRows(t, st, factID)

	if _, _, err := engine.Answer(context.Background(), evalLabelID, LabelUnsure); err != nil {
		t.Fatalf("Answer(Unsure): %v", err)
	}
	_, _, err := engine.Answer(context.Background(), evalLabelID, LabelTrue)
	var aa AlreadyAnsweredError
	if !errors.As(err, &aa) || aa.Previous != LabelUnsure {
		t.Fatalf("Answer(Dogru) after Unsure err = %v, want AlreadyAnsweredError{Previous: unsure}", err)
	}
	if n := countEvidenceRows(t, st, factID); n != before {
		t.Fatalf("evidence rows = %d, want unchanged %d (unsure writes none, the rejected Dogru none)", n, before)
	}
	if row := evalLabel(t, st, evalLabelID); row.Label.String != LabelUnsure {
		t.Fatalf("label = %q, want %q", row.Label.String, LabelUnsure)
	}
}

// TestAnswerSameLabelRepeatIsIdempotentNoop (FIX-P-16): repeating the SAME
// answer is a no-op - nil error, one evidence row, one ritual.answered.
func TestAnswerSameLabelRepeatIsIdempotentNoop(t *testing.T) {
	st := newTestStore(t)
	engine := newTestEngine(t, st, &fakeDelivery{sendResult: true})
	factID, evalLabelID := runOneQuestion(t, st, engine, "run-trace-p16-00000000000000000003", 0.5)
	before := countEvidenceRows(t, st, factID)

	for i := 0; i < 2; i++ {
		if _, expired, err := engine.Answer(context.Background(), evalLabelID, LabelTrue); err != nil || expired {
			t.Fatalf("Answer(Dogru) #%d: err=%v expired=%v", i+1, err, expired)
		}
	}
	if n := countEvidenceRows(t, st, factID); n != before+1 {
		t.Fatalf("evidence rows = %d, want %d", n, before+1)
	}
	if got := countEventsOfKind(t, st, EventAnswered); got != 1 {
		t.Fatalf("%s events = %d, want 1 (the repeat is a no-op)", EventAnswered, got)
	}
	if got := countEventsOfKind(t, st, EventAnswerConflict); got != 0 {
		t.Fatalf("%s events = %d, want 0", EventAnswerConflict, got)
	}
}

// TestAnswerConcurrentOppositeTapsExactlyOneWins (FIX-P-16): concurrent
// opposite taps race in SQL (ClaimEvalLabelAnswer), not in Go - exactly
// one answer is recorded with exactly one evidence row.
func TestAnswerConcurrentOppositeTapsExactlyOneWins(t *testing.T) {
	st := newTestStore(t)
	engine := newTestEngine(t, st, &fakeDelivery{sendResult: true})
	factID, evalLabelID := runOneQuestion(t, st, engine, "run-trace-p16-00000000000000000004", 0.5)
	before := countEvidenceRows(t, st, factID)

	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < 16; i++ {
		label := LabelTrue
		if i%2 == 1 {
			label = LabelFalse
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := engine.Answer(context.Background(), evalLabelID, label); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if successes < 1 {
		t.Fatal("no tap succeeded")
	}
	if got := countEventsOfKind(t, st, EventAnswered); got != 1 {
		t.Fatalf("%s events = %d, want exactly 1", EventAnswered, got)
	}
	if n := countEvidenceRows(t, st, factID); n != before+1 {
		t.Fatalf("evidence rows = %d, want %d (one winner, one evidence row)", n, before+1)
	}
	row := evalLabel(t, st, evalLabelID)
	if !row.AnsweredAt.Valid || (row.Label.String != LabelTrue && row.Label.String != LabelFalse) {
		t.Fatalf("eval_label = %+v, want answered with the winner's label", row)
	}
}

// failingDeny is a FactEngine whose DenyFact always fails.
type failingDeny struct{ FactEngine }

func (failingDeny) DenyFact(context.Context, string, int64, string) error {
	return errors.New("deny: injected failure")
}

// TestAnswerEvidenceFailureResetsClaim (FIX-P-16): when the evidence write
// fails after the claim, the claim is undone so the user can tap again.
func TestAnswerEvidenceFailureResetsClaim(t *testing.T) {
	st := newTestStore(t)
	real := factengine.New(st.Queries, newTestTaint(st), st)
	eng := New(NewSampler(st.Queries, testMemoryDir, testSecretGlobs, nil), st.Queries, failingDeny{FactEngine: real}, newTestTaint(st), st, &fakeDelivery{sendResult: true})
	factID, evalLabelID := runOneQuestion(t, st, eng, "run-trace-p16-00000000000000000005", 0.5)

	if _, _, err := eng.Answer(context.Background(), evalLabelID, LabelFalse); err == nil {
		t.Fatal("Answer(Yanlis) with a failing DenyFact returned nil, want the evidence error")
	}
	if row := evalLabel(t, st, evalLabelID); row.AnsweredAt.Valid || row.Label.Valid {
		t.Fatalf("eval_label after the failed evidence write = %+v, want unanswered (claim reset)", row)
	}
	if got := countEventsOfKind(t, st, EventAnswered); got != 0 {
		t.Fatalf("%s events = %d, want 0", EventAnswered, got)
	}

	retry := newTestEngine(t, st, &fakeDelivery{sendResult: true})
	if _, _, err := retry.Answer(context.Background(), evalLabelID, LabelFalse); err != nil {
		t.Fatalf("retried Answer(Yanlis): %v", err)
	}
	var negRows int
	if err := st.DB().QueryRow(`SELECT count(*) FROM evidence WHERE fact_id = ? AND polarity = -1`, factID).Scan(&negRows); err != nil {
		t.Fatal(err)
	}
	if negRows != 1 {
		t.Fatalf("negative evidence rows after the retry = %d, want 1", negRows)
	}
	if row := evalLabel(t, st, evalLabelID); row.Label.String != LabelFalse {
		t.Fatalf("label after the retry = %q, want %q", row.Label.String, LabelFalse)
	}
}
