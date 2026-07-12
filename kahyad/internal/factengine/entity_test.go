package factengine

import (
	"context"
	"testing"
)

// TestNamesakeFixtureYieldsTwoDistinctEntitiesNoAutoMerge is the task
// spec's byte-exact namesake fixture: "Emre (gold-token ekibinden) NATS
// konusunda yardimci oldu." and "Spor salonundan Emre yarin
// gelemeyecekmis." -> two distinct entities, no auto-merge, second is
// provisional.
func TestNamesakeFixtureYieldsTwoDistinctEntitiesNoAutoMerge(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()

	// "Emre (gold-token ekibinden) NATS konusunda yardımcı oldu."
	firstID, firstProvisional, err := e.ResolveOrCreateEntity(ctx, "Emre", "person")
	if err != nil {
		t.Fatalf("ResolveOrCreateEntity(first Emre) error = %v", err)
	}
	if firstProvisional {
		t.Error("the first-ever registration of a name must not be provisional")
	}

	// "Spor salonundan Emre yarın gelemeyecekmiş."
	secondID, secondProvisional, err := e.ResolveOrCreateEntity(ctx, "Emre", "person")
	if err != nil {
		t.Fatalf("ResolveOrCreateEntity(second Emre) error = %v", err)
	}
	if firstID == secondID {
		t.Fatal("name similarity alone must never auto-merge - got the SAME entity id twice")
	}
	if !secondProvisional {
		t.Error("a second, suspicious same-name entity must be provisional")
	}

	var entityCount int
	if err := st.DB().QueryRow(`SELECT count(*) FROM entities`).Scan(&entityCount); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if entityCount != 2 {
		t.Errorf("entities count = %d, want 2", entityCount)
	}
	if got := countMergeLedgerRows(t, st); got != 0 {
		t.Errorf("merge_ledger rows without any evidence-backed merge = %d, want 0", got)
	}
}

// TestMergeRequiresEvidenceFactID proves HANDOFF S5 memory #2's "en az
// bir ayirt edici kanit sart" in Go: no fact_id at all, or one that does
// not exist, refuses the merge - name similarity is never sufficient.
func TestMergeRequiresEvidenceFactID(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()

	aID, _, err := e.ResolveOrCreateEntity(ctx, "Emre", "person")
	if err != nil {
		t.Fatalf("ResolveOrCreateEntity(a) error = %v", err)
	}
	bID, _, err := e.ResolveOrCreateEntity(ctx, "Emre", "person")
	if err != nil {
		t.Fatalf("ResolveOrCreateEntity(b) error = %v", err)
	}

	if _, err := e.MergeEntities(ctx, "trace", aID, bID, 0, "user"); err != ErrMergeRequiresEvidence {
		t.Fatalf("MergeEntities(evidenceFactID=0) error = %v, want ErrMergeRequiresEvidence", err)
	}
	if _, err := e.MergeEntities(ctx, "trace", aID, bID, 999999, "user"); err != ErrMergeRequiresEvidence {
		t.Fatalf("MergeEntities(nonexistent fact_id) error = %v, want ErrMergeRequiresEvidence", err)
	}
	if got := countMergeLedgerRows(t, st); got != 0 {
		t.Errorf("merge_ledger rows after refused merges = %d, want 0", got)
	}
}

// TestMergeThenSplitRoundTripsExactlyTwoLedgerRows: shared distinguishing
// evidence + `kahya entity merge` merges the namesake pair; `kahya entity
// split` restores both - a full round trip is exactly 2 merge_ledger rows
// (one merge, one split).
func TestMergeThenSplitRoundTripsExactlyTwoLedgerRows(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	sessionID := "session-emre"
	insertCleanSession(t, st, sessionID)

	dstID, _, err := e.ResolveOrCreateEntity(ctx, "Emre", "person")
	if err != nil {
		t.Fatalf("ResolveOrCreateEntity(dst) error = %v", err)
	}
	srcID, srcProvisional, err := e.ResolveOrCreateEntity(ctx, "Emre", "person")
	if err != nil {
		t.Fatalf("ResolveOrCreateEntity(src) error = %v", err)
	}
	if !srcProvisional {
		t.Fatal("second Emre must be provisional before disambiguation")
	}

	// The shared distinguishing evidence: a fact tying both mentions to
	// the same gold-token/NATS context.
	evidenceFactID, err := e.WriteFact(ctx, "trace-evidence", Candidate{
		Subject: "Emre", Predicate: "works_on", Object: "gold-token NATS",
		Provenance: ProvenanceUserAsserted, SessionID: sessionID,
		Evidentiality: Witnessed, ExtractorVer: "user_direct_v1",
	})
	if err != nil {
		t.Fatalf("WriteFact(evidence) error = %v", err)
	}

	mergeID, err := e.MergeEntities(ctx, "trace-merge", dstID, srcID, evidenceFactID, "user")
	if err != nil {
		t.Fatalf("MergeEntities error = %v", err)
	}
	if mergeID == 0 {
		t.Fatal("MergeEntities returned merge_ledger id 0")
	}

	mergedEntity, err := st.Queries.GetEntity(ctx, srcID)
	if err != nil {
		t.Fatalf("GetEntity(src) after merge error = %v", err)
	}
	if mergedEntity.Status != "merged" {
		t.Errorf("src entity status after merge = %q, want %q", mergedEntity.Status, "merged")
	}

	splitID, err := e.SplitEntities(ctx, "trace-split", mergeID, "user")
	if err != nil {
		t.Fatalf("SplitEntities error = %v", err)
	}
	if splitID == 0 {
		t.Fatal("SplitEntities returned merge_ledger id 0")
	}

	restoredEntity, err := st.Queries.GetEntity(ctx, srcID)
	if err != nil {
		t.Fatalf("GetEntity(src) after split error = %v", err)
	}
	if restoredEntity.Status != "active" {
		t.Errorf("src entity status after split = %q, want %q", restoredEntity.Status, "active")
	}

	if got := countMergeLedgerRows(t, st); got != 2 {
		t.Errorf("merge_ledger rows after merge+split round trip = %d, want 2", got)
	}

	var mergeOps, splitOps int
	if err := st.DB().QueryRow(`SELECT count(*) FROM merge_ledger WHERE op = 'merge'`).Scan(&mergeOps); err != nil {
		t.Fatalf("count merge ops: %v", err)
	}
	if err := st.DB().QueryRow(`SELECT count(*) FROM merge_ledger WHERE op = 'split'`).Scan(&splitOps); err != nil {
		t.Fatalf("count split ops: %v", err)
	}
	if mergeOps != 1 || splitOps != 1 {
		t.Errorf("merge_ledger ops = %d merge / %d split, want 1/1", mergeOps, splitOps)
	}
}

func TestSplitRefusesNonMergeRecord(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	sessionID := "session-x"
	insertCleanSession(t, st, sessionID)

	dstID, _, _ := e.ResolveOrCreateEntity(ctx, "Emre", "person")
	srcID, _, _ := e.ResolveOrCreateEntity(ctx, "Emre", "person")
	evidenceFactID, err := e.WriteFact(ctx, "trace", Candidate{
		Subject: "Emre", Predicate: "works_on", Object: "gold-token NATS",
		Provenance: ProvenanceUserAsserted, SessionID: sessionID,
		Evidentiality: Witnessed, ExtractorVer: "user_direct_v1",
	})
	if err != nil {
		t.Fatalf("WriteFact error = %v", err)
	}
	mergeID, err := e.MergeEntities(ctx, "trace", dstID, srcID, evidenceFactID, "user")
	if err != nil {
		t.Fatalf("MergeEntities error = %v", err)
	}
	splitID, err := e.SplitEntities(ctx, "trace", mergeID, "user")
	if err != nil {
		t.Fatalf("SplitEntities error = %v", err)
	}

	if _, err := e.SplitEntities(ctx, "trace", splitID, "user"); err != ErrNotAMergeRecord {
		t.Fatalf("SplitEntities(a split record) error = %v, want ErrNotAMergeRecord", err)
	}
}
