package factengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// retract_kind_test.go covers FIX-FINAL fact-history-ended-vs-corrected
// (owner decision 2026-10-16): every retraction records whether the fact
// ENDED (it held and no longer does) or was CORRECTED (it was wrong), with
// its own time, and a user denial always means corrected.

// TestClassifyRetractionSplitsCorrectionsFromEndings: the correction
// phrases are corrected, the "artık ... değil" shapes are ended, a text with
// both is a correction (never shown as history), and ordinary text is no
// retraction at all.
func TestClassifyRetractionSplitsCorrectionsFromEndings(t *testing.T) {
	cases := []struct {
		text string
		kind RetractionKind
		ok   bool
	}{
		{"Düzeltiyorum, Emre Acme'de çalışmıyor.", RetractionCorrected, true},
		{"duzeltiyorum: Moda degil Kadikoy", RetractionCorrected, true},
		{"Yanlış hatırlıyorsun, o Beta'da.", RetractionCorrected, true},
		{"Hayır, öyle değil.", RetractionCorrected, true},
		{"HAYIR YANLIŞ", RetractionCorrected, true},
		{"Kahveyi artık sevmiyorum.", RetractionEnded, true},
		{"Artık onun arkadaşı değilim.", RetractionEnded, true},
		{"Emre artik Acme'de degil.", RetractionEnded, true},
		// Both shapes: the safe reading is a correction.
		{"Düzeltiyorum: artık orada değil.", RetractionCorrected, true},
		{"Kahveyi çok seviyorum.", "", false},
	}
	for _, c := range cases {
		kind, ok := ClassifyRetraction(c.text)
		if kind != c.kind || ok != c.ok {
			t.Errorf("ClassifyRetraction(%q) = (%q, %v), want (%q, %v)", c.text, kind, ok, c.kind, c.ok)
		}
		if DetectRetraction(c.text) != c.ok {
			t.Errorf("DetectRetraction(%q) = %v, want %v", c.text, !c.ok, c.ok)
		}
	}
}

func TestParseRetractionKindIsExact(t *testing.T) {
	for _, s := range []string{"ended", "corrected"} {
		if k, err := ParseRetractionKind(s); err != nil || string(k) != s {
			t.Errorf("ParseRetractionKind(%q) = (%q, %v)", s, k, err)
		}
	}
	for _, s := range []string{"", "Ended", "sona-erdi", "wrong"} {
		if _, err := ParseRetractionKind(s); !errors.Is(err, ErrUnknownRetractionKind) {
			t.Errorf("ParseRetractionKind(%q) err = %v, want ErrUnknownRetractionKind", s, err)
		}
	}
}

// writeUserFact writes a user_asserted (subject, likes, object) fact from a
// clean session at the engine's current clock.
func writeUserFact(t *testing.T, e *Engine, st *store.Store, session, subject, object string) int64 {
	t.Helper()
	insertCleanSession(t, st, session)
	id, err := e.WriteFact(context.Background(), "tr-write", Candidate{
		Subject: subject, Predicate: "likes", Object: object,
		Provenance: ProvenanceUserAsserted, SessionID: session,
		Evidentiality: Witnessed, ExtractorVer: "user_direct_v1",
	})
	if err != nil {
		t.Fatalf("WriteFact(%s): %v", object, err)
	}
	return id
}

func setEngineClock(e *Engine, at string) time.Time {
	ts, _ := time.Parse(time.RFC3339, at)
	e.SetClock(func() time.Time { return ts })
	return ts
}

// TestRetractFactRecordsKindAndTime: both kinds are stored with the
// retraction's own time; valid_to is that time; the ledger row names the
// kind.
func TestRetractFactRecordsKindAndTime(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	setEngineClock(e, "2026-03-01T09:00:00Z")
	ended := writeUserFact(t, e, st, "s-ended", "user", "kahve")
	corrected := writeUserFact(t, e, st, "s-corr", "user", "çay")

	setEngineClock(e, "2026-07-01T10:00:00Z")
	if res, err := e.RetractFact(ctx, "tr-ended", "user", "likes", "kahve", "s-ended", RetractionEnded); err != nil || res != (Retraction{FactID: ended, Kind: RetractionEnded}) {
		t.Fatalf("RetractFact(ended) = (%+v, %v)", res, err)
	}
	if res, err := e.RetractFact(ctx, "tr-corr", "user", "likes", "çay", "s-corr", RetractionCorrected); err != nil || res != (Retraction{FactID: corrected, Kind: RetractionCorrected}) {
		t.Fatalf("RetractFact(corrected) = (%+v, %v)", res, err)
	}
	const at = "2026-07-01T10:00:00Z"
	for id, want := range map[int64]RetractionKind{ended: RetractionEnded, corrected: RetractionCorrected} {
		f, err := e.GetFact(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if f.Status != "retracted" || f.RetractionKind.String != string(want) || f.RetractedAt.String != at || f.ValidTo.String != at {
			t.Errorf("fact %d = status %q kind %v retracted_at %v valid_to %v; want retracted/%s at %s", id, f.Status, f.RetractionKind, f.RetractedAt, f.ValidTo, want, at)
		}
	}
	if !IsEnded(mustFact(t, e, ended)) || IsEnded(mustFact(t, e, corrected)) {
		t.Error("IsEnded must hold for the ended fact only")
	}
	for trace, want := range map[string]string{"tr-ended": "ended", "tr-corr": "corrected"} {
		var kind string
		if err := st.DB().QueryRow(`SELECT json_extract(payload, '$.kind') FROM events WHERE trace_id = ? AND kind = ?`, trace, EventFactRetracted).Scan(&kind); err != nil || kind != want {
			t.Errorf("%s ledger kind = %q, %v; want %q", trace, kind, err, want)
		}
	}
}

func mustFact(t *testing.T, e *Engine, id int64) sqlcgen.Fact {
	t.Helper()
	got, err := e.GetFact(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestRetractFactEndedKeepsAnEarlierValidTo: a fact that already carried an
// end in the past ended then, not at the retraction - the window must not
// grow. A correction always closes at the retraction time.
func TestRetractFactEndedKeepsAnEarlierValidTo(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	setEngineClock(e, "2026-01-01T00:00:00Z")
	insertCleanSession(t, st, "s1")
	id, err := e.WriteFact(ctx, "tr", Candidate{
		Subject: "Emre", Predicate: "works_at", Object: "Acme", Provenance: ProvenanceUserAsserted, SessionID: "s1",
		ExtractorVer: "user_direct_v1", ValidFrom: "2025-01-01T00:00:00Z", ValidTo: "2026-03-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	setEngineClock(e, "2026-09-01T00:00:00Z")
	if _, err := e.RetractFact(ctx, "tr", "Emre", "works_at", "Acme", "s1", RetractionEnded); err != nil {
		t.Fatal(err)
	}
	if f := mustFact(t, e, id); f.ValidTo.String != "2026-03-01T00:00:00Z" || f.RetractedAt.String != "2026-09-01T00:00:00Z" {
		t.Fatalf("ended fact valid_to/retracted_at = %v/%v, want the earlier end kept and the retraction time recorded", f.ValidTo, f.RetractedAt)
	}
}

// TestRetractFactEndedOnADeniedFactIsRecordedCorrected: the user said the
// fact was wrong (DenyFact: `kahya fact deny`, a ritual Yanlış), so an
// ended retraction of it is a correction - it never shows as history.
func TestRetractFactEndedOnADeniedFactIsRecordedCorrected(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	id := writeUserFact(t, e, st, "s1", "user", "kahve")
	if err := e.DenyFact(ctx, "tr-deny", id, "ritual-1"); err != nil {
		t.Fatal(err)
	}
	res, err := e.RetractFact(ctx, "tr-ret", "user", "likes", "kahve", "s1", RetractionEnded)
	if err != nil || res.Kind != RetractionCorrected {
		t.Fatalf("RetractFact(ended) on a denied fact = (%+v, %v), want recorded corrected", res, err)
	}
	if f := mustFact(t, e, id); f.RetractionKind.String != string(RetractionCorrected) || HistoryEligible(f) {
		t.Fatalf("denied fact kind = %v, history eligible = %v; want corrected, never history", f.RetractionKind, HistoryEligible(f))
	}
	var requested, reason string
	if err := st.DB().QueryRow(`SELECT json_extract(payload, '$.requested_kind'), json_extract(payload, '$.kind_reason') FROM events WHERE trace_id = 'tr-ret' AND kind = ?`, EventFactRetracted).Scan(&requested, &reason); err != nil || requested != "ended" || reason != "user_denial" {
		t.Fatalf("ledger requested_kind/kind_reason = %q/%q, %v", requested, reason, err)
	}
}

// TestDenyFactTurnsAnEndedRetractionIntoACorrection: denying a fact after
// it was retracted as ended re-records it as corrected; denying a corrected
// one leaves it corrected.
func TestDenyFactTurnsAnEndedRetractionIntoACorrection(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	id := writeUserFact(t, e, st, "s1", "user", "kahve")
	if _, err := e.RetractFact(ctx, "tr", "user", "likes", "kahve", "s1", RetractionEnded); err != nil {
		t.Fatal(err)
	}
	if f := mustFact(t, e, id); !HistoryEligible(f) {
		t.Fatalf("ended user_asserted fact must be history-eligible: %+v", f)
	}
	if err := e.DenyFact(ctx, "tr-deny", id, "cli:tr-deny"); err != nil {
		t.Fatal(err)
	}
	if f := mustFact(t, e, id); f.RetractionKind.String != string(RetractionCorrected) || HistoryEligible(f) || f.Status != "retracted" {
		t.Fatalf("after deny: status %q kind %v eligible %v; want retracted, corrected, not history", f.Status, f.RetractionKind, HistoryEligible(f))
	}
	var marked string
	if err := st.DB().QueryRow(`SELECT json_extract(payload, '$.retraction_kind') FROM events WHERE trace_id = 'tr-deny' AND kind = ?`, EventFactDenied).Scan(&marked); err != nil || marked != "corrected" {
		t.Fatalf("fact_denied retraction_kind = %q, %v; want corrected", marked, err)
	}
	// A second deny of the now-corrected fact changes nothing.
	if err := e.DenyFact(ctx, "tr-deny2", id, "cli:tr-deny2"); err != nil {
		t.Fatal(err)
	}
	if f := mustFact(t, e, id); f.RetractionKind.String != string(RetractionCorrected) {
		t.Fatalf("kind after second deny = %v", f.RetractionKind)
	}
}

// TestRetractFactRejectsAnUnknownKindBeforeAnyWrite: fail-closed - nothing
// is read or written for a kind outside the enum.
func TestRetractFactRejectsAnUnknownKindBeforeAnyWrite(t *testing.T) {
	e, st := newTestEngine(t)
	id := writeUserFact(t, e, st, "s1", "user", "kahve")
	if _, err := e.RetractFact(context.Background(), "tr", "user", "likes", "kahve", "s1", "gone"); !errors.Is(err, ErrUnknownRetractionKind) {
		t.Fatalf("RetractFact(unknown kind) err = %v", err)
	}
	if f := mustFact(t, e, id); f.Status != "active" || f.RetractionKind.Valid || countEvidenceRows(t, st, id) != 1 {
		t.Fatalf("fact after a refused retraction = %+v (evidence %d); want untouched", f, countEvidenceRows(t, st, id))
	}
}

// TestHistoryEligibleTrustRules: only an ended fact qualifies, with the
// tier half of InjectionEligible; a pre-0027 retraction (no kind) never
// does, and InjectionEligible stays false for every retracted fact.
func TestHistoryEligibleTrustRules(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	user := writeUserFact(t, e, st, "s1", "user", "kahve")
	agent, err := e.WriteFact(ctx, "tr", Candidate{Subject: "Emre", Predicate: "lives_in", Object: "Moda", ExtractorVer: "x_v1"})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := e.WriteFact(ctx, "tr", Candidate{Subject: "Emre", Predicate: "works_at", Object: "Acme", Provenance: ProvenanceExternalDoc, ExtractorVer: "x_v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ConfirmFact(ctx, "tr", confirmed); err != nil {
		t.Fatal(err)
	}
	legacy := writeUserFact(t, e, st, "s2", "user", "çay")
	for _, r := range []struct{ s, p, o string }{{"user", "likes", "kahve"}, {"Emre", "lives_in", "Moda"}, {"Emre", "works_at", "Acme"}} {
		if _, err := e.RetractFact(ctx, "tr", r.s, r.p, r.o, "", RetractionEnded); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().Exec(`UPDATE facts SET status = 'retracted', valid_to = updated_at WHERE id = ?`, legacy); err != nil {
		t.Fatal(err)
	}
	want := map[int64]bool{user: true, agent: false, confirmed: true, legacy: false}
	for id, eligible := range want {
		f := mustFact(t, e, id)
		if HistoryEligible(f) != eligible {
			t.Errorf("HistoryEligible(fact %d: tier %s confirmed %v kind %v) = %v, want %v", id, f.SourceTier, f.ConfirmedAt.Valid, f.RetractionKind, !eligible, eligible)
		}
		if InjectionEligible(f) {
			t.Errorf("InjectionEligible(retracted fact %d) = true; an ended fact is never injected", id)
		}
	}
}
