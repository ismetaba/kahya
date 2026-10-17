package factengine

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/taint"
)

// retract_race_test.go (FIX-FINAL fact-history-ended-vs-corrected, review):
// a user denial (DenyFact: `kahya fact deny`, a ritual Yanlış answer) that
// races an ENDED retraction of the same fact must still leave it corrected -
// whichever statement lands first. The kind is decided in the retraction's
// own guarded UPDATE (atomic with the denial check), and DenyFact re-marks
// an ended retraction after its evidence row.

// hookStore runs a hook once just before the RetractFact UPDATE or just
// before one negative evidence INSERT of session hookSession: the exact
// interleavings a concurrent caller can produce on the single brain.db
// connection.
type hookStore struct {
	*sqlcgen.Queries
	beforeRetract  func()
	hookSession    string
	beforeEvidence func()
}

func (h *hookStore) RetractFact(ctx context.Context, arg sqlcgen.RetractFactParams) (sql.NullString, error) {
	if b := h.beforeRetract; b != nil {
		h.beforeRetract = nil
		b()
	}
	return h.Queries.RetractFact(ctx, arg)
}

func (h *hookStore) InsertEvidence(ctx context.Context, arg sqlcgen.InsertEvidenceParams) (sqlcgen.InsertEvidenceRow, error) {
	if b := h.beforeEvidence; b != nil && arg.Polarity < 0 && arg.SessionID.String == h.hookSession {
		h.beforeEvidence = nil
		b()
	}
	return h.Queries.InsertEvidence(ctx, arg)
}

func newHookedEngine(t *testing.T) (*Engine, *hookStore, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	hs := &hookStore{Queries: st.Queries}
	return New(hs, taint.New(st.Queries, st), st), hs, st
}

func negativeRows(t *testing.T, st *store.Store, factID int64) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM evidence WHERE fact_id = ? AND polarity < 0`, factID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRetractFactEndedRacingADenialIsRecordedCorrected: the denial lands
// after RetractFact read the fact and before its UPDATE.
func TestRetractFactEndedRacingADenialIsRecordedCorrected(t *testing.T) {
	e, hs, st := newHookedEngine(t)
	ctx := context.Background()
	id := writeUserFact(t, e, st, "s1", "user", "kahve")
	hs.beforeRetract = func() {
		if err := e.DenyFact(ctx, "tr-deny", id, "ritual-1"); err != nil {
			t.Errorf("DenyFact: %v", err)
		}
	}
	res, err := e.RetractFact(ctx, "tr-ret", "user", "likes", "kahve", "s1", RetractionEnded)
	if err != nil {
		t.Fatal(err)
	}
	f := mustFact(t, e, id)
	if res.Kind != RetractionCorrected || f.RetractionKind.String != string(RetractionCorrected) || IsEnded(f) {
		t.Fatalf("a denied fact retracted as ended: result %+v, stored kind %v; want corrected", res, f.RetractionKind)
	}
	if f.ValidTo.String != f.RetractedAt.String {
		t.Fatalf("corrected valid_to = %q, want the retraction time %q", f.ValidTo.String, f.RetractedAt.String)
	}
	var requested, reason string
	if err := st.DB().QueryRow(`SELECT json_extract(payload, '$.requested_kind'), json_extract(payload, '$.kind_reason') FROM events WHERE trace_id = 'tr-ret' AND kind = ?`, EventFactRetracted).Scan(&requested, &reason); err != nil || requested != "ended" || reason != "user_denial" {
		t.Fatalf("ledger requested_kind/kind_reason = %q/%q, %v", requested, reason, err)
	}
}

// TestDenyFactRacingAnEndedRetractionIsRecordedCorrected: the ended
// retraction runs to completion after DenyFact read the (still active)
// fact and before its evidence row.
func TestDenyFactRacingAnEndedRetractionIsRecordedCorrected(t *testing.T) {
	e, hs, st := newHookedEngine(t)
	ctx := context.Background()
	id := writeUserFact(t, e, st, "s1", "user", "kahve")
	hs.hookSession = "ritual-1"
	hs.beforeEvidence = func() {
		if res, err := e.RetractFact(ctx, "tr-ret", "user", "likes", "kahve", "s1", RetractionEnded); err != nil || res.Kind != RetractionEnded {
			t.Errorf("RetractFact(ended) before the denial row = (%+v, %v)", res, err)
		}
	}
	if err := e.DenyFact(ctx, "tr-deny", id, "ritual-1"); err != nil {
		t.Fatal(err)
	}
	f := mustFact(t, e, id)
	if f.Status != "retracted" || f.RetractionKind.String != string(RetractionCorrected) || IsEnded(f) {
		t.Fatalf("a fact denied while it was retracted as ended: status %q kind %v; want retracted corrected", f.Status, f.RetractionKind)
	}
	if n := negativeRows(t, st, id); n != 2 {
		t.Fatalf("negative evidence rows = %d, want 2 (retraction + denial)", n)
	}
	var marked string
	if err := st.DB().QueryRow(`SELECT json_extract(payload, '$.retraction_kind') FROM events WHERE trace_id = 'tr-deny' AND kind = ?`, EventFactDenied).Scan(&marked); err != nil || marked != "corrected" {
		t.Fatalf("fact_denied retraction_kind = %q, %v; want corrected", marked, err)
	}
}

// TestConcurrentDenyAndEndedRetractionNeverLeavesADeniedFactEnded: the
// same two calls on real goroutines, no hook.
func TestConcurrentDenyAndEndedRetractionNeverLeavesADeniedFactEnded(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	insertCleanSession(t, st, "s1")
	const n = 60
	for i := 0; i < n; i++ {
		obj := fmt.Sprintf("kahve-%d", i)
		id, err := e.WriteFact(ctx, "tr-write", Candidate{Subject: "user", Predicate: "likes", Object: obj,
			Provenance: ProvenanceUserAsserted, SessionID: "s1", Evidentiality: Witnessed, ExtractorVer: "user_direct_v1"})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var rerr, derr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, rerr = e.RetractFact(ctx, "tr-ret", "user", "likes", obj, "", RetractionEnded)
		}()
		go func() { defer wg.Done(); derr = e.DenyFact(ctx, "tr-deny", id, "ritual-1") }()
		wg.Wait()
		if rerr != nil || derr != nil {
			t.Fatalf("iteration %d: retract %v, deny %v", i, rerr, derr)
		}
		if f := mustFact(t, e, id); f.RetractionKind.String != string(RetractionCorrected) {
			t.Fatalf("iteration %d: a denied fact is recorded %v; want corrected", i, f.RetractionKind)
		}
	}
}

// TestIsHistoryRequiresExactlyTheRetractionsOwnNegativeRow: the read side's
// half - an ended fact is history only while its one negative evidence row
// is its retraction's own; a further one (a user denial whose re-mark did
// not run) or none at all counts as corrected, never shown, whatever its
// confidence. A corrected or kind-less retraction never is.
func TestIsHistoryRequiresExactlyTheRetractionsOwnNegativeRow(t *testing.T) {
	ended := sqlcgen.Fact{Status: "retracted", RetractionKind: sql.NullString{String: string(RetractionEnded), Valid: true},
		SourceTier: TierUserAsserted, Confidence: 10}
	for negs, want := range map[int64]bool{0: false, 1: true, 2: false, 3: false} {
		if got := IsHistory(ended, negs); got != want {
			t.Errorf("IsHistory(ended, %d negative rows) = %v, want %v", negs, got, want)
		}
		if got := HistoryEligible(ended, negs); got != want {
			t.Errorf("HistoryEligible(ended, %d negative rows) = %v, want %v", negs, got, want)
		}
	}
	corrected := ended
	corrected.RetractionKind.String = string(RetractionCorrected)
	legacy := ended
	legacy.RetractionKind = sql.NullString{}
	for _, f := range []sqlcgen.Fact{corrected, legacy} {
		if IsHistory(f, 1) || HistoryEligible(f, 1) {
			t.Errorf("IsHistory/HistoryEligible(kind %v, 1) = true; never history", f.RetractionKind)
		}
	}
}
