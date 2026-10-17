package ritual

import (
	"context"
	"testing"

	"kahya/kahyad/internal/factengine"
)

// TestAnswerYanlisMakesALaterEndedRetractionACorrection (FIX-FINAL
// fact-history-ended-vs-corrected, owner decision 2026-10-16): a ritual
// Yanlış answer says the fact was WRONG. When the fact is later retracted
// as ended (an extracted "artık ... değil", `kahya fact retract
// --sona-erdi`), the retraction is recorded as corrected - the fact is
// never shown as history.
func TestAnswerYanlisMakesALaterEndedRetractionACorrection(t *testing.T) {
	st := newTestStore(t)
	engine := newTestEngine(t, st, &fakeDelivery{sendResult: true})
	factID, evalLabelID := runOneQuestion(t, st, engine, "run-trace-history-000000000000000001", 0.5)
	if _, expired, err := engine.Answer(context.Background(), evalLabelID, LabelFalse); err != nil || expired {
		t.Fatalf("Answer(Yanlış): err=%v expired=%v", err, expired)
	}

	fe := factengine.New(st.Queries, newTestTaint(st), st)
	res, err := fe.RetractFact(context.Background(), "tr-ended", "Emre", "sever", "kahve", "", factengine.RetractionEnded)
	if err != nil {
		t.Fatalf("RetractFact(ended): %v", err)
	}
	if res.FactID != factID || res.Kind != factengine.RetractionCorrected {
		t.Fatalf("RetractFact(ended) after a Yanlış answer = %+v, want fact %d recorded corrected", res, factID)
	}
	if f := getFact(t, st, factID); factengine.IsEnded(f) || factengine.HistoryEligible(f) {
		t.Fatalf("fact after Yanlış + ended retraction = kind %v; must never count as ended history", f.RetractionKind)
	}
}

// TestAnswerYanlisOnAnEndedFactIsACorrection: the ritual samples active
// facts only, but a denial reaching a fact already retracted as ended
// (DenyFact - the same call a Yanlış answer makes) re-records it as
// corrected.
func TestAnswerYanlisOnAnEndedFactIsACorrection(t *testing.T) {
	st := newTestStore(t)
	ep := seedEpisode(t, st, "notlar/gunluk.md")
	fact := seedFact(t, st, "Emre", "sever", "kahve", "user_asserted", 0.5, false)
	seedClassifyingEvidence(t, st, fact.ID, ep)
	fe := factengine.New(st.Queries, newTestTaint(st), st)
	if _, err := fe.RetractFact(context.Background(), "tr", "Emre", "sever", "kahve", "", factengine.RetractionEnded); err != nil {
		t.Fatal(err)
	}
	if !factengine.IsEnded(getFact(t, st, fact.ID)) {
		t.Fatal("fixture fact should be ended")
	}
	if err := fe.DenyFact(context.Background(), "tr-yanlis", fact.ID, "run-trace-history-000000000000000002"); err != nil {
		t.Fatal(err)
	}
	if f := getFact(t, st, fact.ID); f.RetractionKind.String != string(factengine.RetractionCorrected) {
		t.Fatalf("kind after a denial = %v, want corrected", f.RetractionKind)
	}
}
