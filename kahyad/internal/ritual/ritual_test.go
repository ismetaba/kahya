package ritual

import (
	"context"
	"fmt"
	"testing"
	"time"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/store"
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
// kanit sayilir") - the label is still updated (edited) both times.
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
