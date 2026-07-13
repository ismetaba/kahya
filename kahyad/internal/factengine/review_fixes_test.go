package factengine

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"kahya/kahyad/internal/store/sqlcgen"
)

// TestSameSessionEvidenceDedupeUnderConcurrency is the regression test for
// the W5-03 review BLOCKER: the same-session evidence dedupe was a plain
// check-then-act SELECT/INSERT with no DB constraint, so concurrent ritual
// taps (telegram callbacks are not serialized) both passed the SELECT and
// both inserted an evidence row. idx_evidence_one_per_session_polarity now
// guarantees exactly one row per (fact, session, polarity).
func TestSameSessionEvidenceDedupeUnderConcurrency(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	sess := "sess-concurrent"
	insertCleanSession(t, st, sess)

	// Seed the fact once so every concurrent writer resolves the SAME fact.
	factID, err := e.WriteFact(ctx, "trace", Candidate{
		Subject: "Emre", Predicate: "sever", Object: "kahve",
		Provenance: ProvenanceUserAsserted, SessionID: sess, Evidentiality: Witnessed, ExtractorVer: "user_direct_v1",
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.WriteFact(ctx, "trace", Candidate{
				Subject: "Emre", Predicate: "sever", Object: "kahve",
				Provenance: ProvenanceUserAsserted, SessionID: sess, Evidentiality: Witnessed, ExtractorVer: "user_direct_v1",
			})
		}()
	}
	wg.Wait()

	if got := countEvidenceRows(t, st, factID); got != 1 {
		t.Fatalf("evidence rows for one (fact, session, polarity) after 12 concurrent same-session writes = %d, want exactly 1 (ayni-oturum tekrari tek kanit)", got)
	}

	// Deterministic guarantee (independent of goroutine scheduling): the DB
	// itself rejects a second (fact, session, polarity) row for a real
	// session. A raw InsertEvidence bypasses addEvidence's SELECT early-out,
	// so ONLY idx_evidence_one_per_session_polarity can stop it.
	dup := func() error {
		_, err := st.Queries.InsertEvidence(ctx, sqlcgen.InsertEvidenceParams{
			FactID: factID, SessionID: sql.NullString{String: sess, Valid: true},
			Polarity: 1, Weight: 2.94, CreatedAt: "2026-07-13T00:00:00Z",
		})
		return err
	}
	if err := dup(); !isUniqueViolation(err) {
		t.Fatalf("raw duplicate evidence insert error = %v, want a unique-constraint violation (the index is the real same-session dedupe guarantee)", err)
	}

	// A NULL-session row is OUTSIDE the partial index - distinct observations
	// (e.g. hot-window promotion) must NOT be collapsed, so two are allowed.
	for i := 0; i < 2; i++ {
		if _, err := st.Queries.InsertEvidence(ctx, sqlcgen.InsertEvidenceParams{
			FactID: factID, SessionID: sql.NullString{}, Polarity: 1, Weight: -0.4, CreatedAt: "2026-07-13T00:00:00Z",
		}); err != nil {
			t.Fatalf("NULL-session evidence insert %d should be allowed (outside the partial index): %v", i, err)
		}
	}
}

// TestMergeRejectsUnrelatedAndRetractedEvidence is the regression test for
// the W5-04 review BLOCKER: MergeEntities accepted ANY existing fact_id as
// distinguishing evidence - including a totally-unrelated fact and even a
// RETRACTED one - defeating the Turkish-namesake merge gate. The engine must
// refuse a cited fact that is not active or references neither entity.
func TestMergeRejectsUnrelatedAndRetractedEvidence(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()

	dstID, _, err := e.ResolveOrCreateEntity(ctx, "Emre", "person")
	if err != nil {
		t.Fatal(err)
	}
	srcID, _, err := e.ResolveOrCreateEntity(ctx, "Emre", "person")
	if err != nil {
		t.Fatal(err)
	}

	// (a) An UNRELATED fact (mentions neither Emre entity) must be rejected.
	unrelatedID, err := e.WriteFact(ctx, "trace", Candidate{
		Subject: "Zeynep Kaya", Predicate: "likes", Object: "kahve",
		Provenance: ProvenanceAgentDerived, Evidentiality: Inferred, ExtractorVer: "x-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.MergeEntities(ctx, "trace", dstID, srcID, unrelatedID, "user"); !errors.Is(err, ErrMergeEvidenceUnusable) {
		t.Fatalf("merge citing UNRELATED fact: err = %v, want ErrMergeEvidenceUnusable", err)
	}

	// (b) An Emre-referencing but RETRACTED fact must be rejected.
	sess := "sess-clean-1"
	insertCleanSession(t, st, sess)
	if _, err := e.WriteFact(ctx, "trace", Candidate{
		Subject: "Emre", Predicate: "works_on", Object: "gold-token NATS",
		Provenance: ProvenanceUserAsserted, SessionID: sess, Evidentiality: Witnessed, ExtractorVer: "user_direct_v1",
	}); err != nil {
		t.Fatal(err)
	}
	retractedID, err := e.RetractFact(ctx, "trace", "Emre", "works_on", "gold-token NATS", sess)
	if err != nil {
		t.Fatalf("RetractFact: %v", err)
	}
	if _, err := e.MergeEntities(ctx, "trace", dstID, srcID, retractedID, "user"); !errors.Is(err, ErrMergeEvidenceUnusable) {
		t.Fatalf("merge citing RETRACTED fact: err = %v, want ErrMergeEvidenceUnusable", err)
	}

	// (c) An active, Emre-referencing fact IS usable evidence - merge succeeds.
	goodID, err := e.WriteFact(ctx, "trace", Candidate{
		Subject: "Emre", Predicate: "email", Object: "emre@gold-token.example",
		Provenance: ProvenanceUserAsserted, SessionID: sess, Evidentiality: Witnessed, ExtractorVer: "user_direct_v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.MergeEntities(ctx, "trace", dstID, srcID, goodID, "user"); err != nil {
		t.Fatalf("merge citing a valid active related fact: err = %v, want success", err)
	}
}

// TestAgentDerivedSaturatesAtCapAcrossSessions is the regression test for the
// W5-04 review #3: agent_derived's tier cap is NEGATIVE (logit(0.4)); summing
// its per-evidence weight across independent sessions drove confidence BELOW
// the cap (more supporting evidence wrongly LOWERING it). Same-tier positive
// evidence must saturate AT the cap.
func TestAgentDerivedSaturatesAtCapAcrossSessions(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()

	var factID int64
	for _, sess := range []string{"s1", "s2", "s3"} {
		id, err := e.WriteFact(ctx, "trace", Candidate{
			Subject: "kullanici", Predicate: "hobi", Object: "yuzme",
			Provenance: ProvenanceAgentDerived, SessionID: sess, Evidentiality: Inferred, ExtractorVer: "x-v1",
		})
		if err != nil {
			t.Fatal(err)
		}
		factID = id
	}
	if got := countEvidenceRows(t, st, factID); got != 3 {
		t.Fatalf("evidence rows = %d, want 3 (three distinct sessions)", got)
	}
	fact, err := e.store.GetFact(ctx, factID)
	if err != nil {
		t.Fatal(err)
	}
	if !almostEqual(fact.Confidence, CapLogOddsAgentDerived) {
		t.Fatalf("confidence = %v, want the agent_derived cap %v (must SATURATE, not sum below it)", fact.Confidence, CapLogOddsAgentDerived)
	}
}
