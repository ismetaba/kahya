package factengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// TestListQuarantinedFactsMatchesTierPredicate pins the SQL quarantine
// filter (ListQuarantinedFacts, the `kahya fact list --quarantined` /
// `--all-quarantined` source) to the Go predicate FactQuarantined /
// TierInjectionEligible over every tier x confirmed combination: a human
// review list must show exactly the facts reflex injection keeps out for
// want of a confirmation - no more (a confirmed or trusted fact is not
// waiting), no less (an unlisted quarantined fact could never be confirmed
// in bulk).
func TestListQuarantinedFactsMatchesTierPredicate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	want := map[int64]bool{}
	for _, tier := range []string{TierUserEdit, TierUserAsserted, TierExternalDoc, TierScreen, TierAgentDerived} {
		for _, confirmed := range []bool{false, true} {
			f, err := st.Queries.InsertFact(ctx, sqlcgen.InsertFactParams{
				Subject: "s-" + tier, Predicate: "works_at", Object: fmt.Sprint(confirmed), SourceTier: tier,
				Evidentiality: "inferred", Status: "active", UpdatedAt: now, CreatedAt: now,
			})
			if err != nil {
				t.Fatal(err)
			}
			if confirmed {
				if err := st.Queries.ConfirmFact(ctx, sqlcgen.ConfirmFactParams{ConfirmedAt: sql.NullString{String: now, Valid: true}, UpdatedAt: now, ID: f.ID}); err != nil {
					t.Fatal(err)
				}
				f.ConfirmedAt = sql.NullString{String: now, Valid: true}
			}
			if FactQuarantined(f) != !TierInjectionEligible(tier, confirmed) {
				t.Fatalf("FactQuarantined(%s, confirmed=%v) disagrees with TierInjectionEligible", tier, confirmed)
			}
			want[f.ID] = FactQuarantined(f)
		}
	}
	rows, err := st.Queries.ListQuarantinedFacts(ctx, sqlcgen.ListQuarantinedFactsParams{RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	for id, quarantined := range want {
		if got[id] != quarantined {
			t.Errorf("fact %d: listed=%v, quarantined by the predicate=%v", id, got[id], quarantined)
		}
	}
}

// TestConfirmAndDenyUnknownFactAreNotFound: confirming or denying an id
// that names no fact is ErrFactNotFound and ledgers nothing (a bulk confirm
// must never count a no-op as a confirmation).
func TestConfirmAndDenyUnknownFactAreNotFound(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	if err := e.ConfirmFact(ctx, "tr", 424242); !errors.Is(err, ErrFactNotFound) {
		t.Fatalf("ConfirmFact(unknown) = %v, want ErrFactNotFound", err)
	}
	if err := e.DenyFact(ctx, "tr", 424242, "s"); !errors.Is(err, ErrFactNotFound) {
		t.Fatalf("DenyFact(unknown) = %v, want ErrFactNotFound", err)
	}
	if n := countEventsOfKind(t, st, EventFactConfirmed) + countEventsOfKind(t, st, EventFactDenied); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}

// TestListFactsFiltersAndBulkSelections covers the review reads: newest
// first, the created-since bound (inclusive, second precision), the
// entity filter (id, or name through the folded alias key), the evidence
// path, truncation, and the bulk id sets with their MaxFactBatch cap.
func TestListFactsFiltersAndBulkSelections(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	ep, err := st.Queries.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{
		Source: "memory_file", SourcePath: sql.NullString{String: "inbox/b.md", Valid: true},
		SourceTier: "agent_derived", Status: "active", CreatedAt: "2026-10-08T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	e.SetClock(func() time.Time { return base.Add(500 * time.Millisecond) })
	emre, _, _, err := e.ResolveEntityForExtraction(ctx, "tr", "Emre", EntityKindPerson, false)
	if err != nil {
		t.Fatal(err)
	}
	first, err := e.WriteFact(ctx, "tr", Candidate{Subject: "Emre", Predicate: "works_at", Object: "Acme", EpisodeID: ep.ID, SessionID: "s1", SubjectEntityID: emre, ExtractorVer: "test_v1"})
	if err != nil {
		t.Fatal(err)
	}
	e.SetClock(func() time.Time { return base.Add(time.Hour) })
	second, err := e.WriteFact(ctx, "tr", Candidate{Subject: "Ayla", Predicate: "works_at", Object: "Beta", SessionID: "s2", ExtractorVer: "test_v1"})
	if err != nil {
		t.Fatal(err)
	}

	items, truncated, err := e.ListFacts(ctx, FactListQuery{QuarantinedOnly: true})
	if err != nil || truncated || len(items) != 2 || items[0].FactID != second || items[1].FactID != first {
		t.Fatalf("ListFacts = %+v %v %v, want [%d %d] newest first", items, truncated, err, second, first)
	}
	if items[1].EvidencePath != "inbox/b.md" || !items[1].Quarantined || items[1].ConfidenceP <= 0 {
		t.Fatalf("first item = %+v, want its note, quarantined, a probability", items[1])
	}
	// The bound is inclusive at second precision: a fact created at
	// 12:00:00.5 is "since 12:00:00".
	if ids, err := e.QuarantinedFactIDsSince(ctx, base); err != nil || len(ids) != 2 {
		t.Fatalf("QuarantinedFactIDsSince(12:00:00) = %v %v, want both", ids, err)
	}
	if ids, err := e.QuarantinedFactIDsSince(ctx, base.Add(time.Minute)); err != nil || len(ids) != 1 || ids[0] != second {
		t.Fatalf("QuarantinedFactIDsSince(12:01) = %v %v, want [%d]", ids, err, second)
	}
	for _, q := range []FactListQuery{{EntityID: emre}, {EntityName: "EMRE"}} {
		if items, _, err := e.ListFacts(ctx, q); err != nil || len(items) != 1 || items[0].FactID != first {
			t.Fatalf("ListFacts(%+v) = %+v %v, want [%d]", q, items, err, first)
		}
	}
	if items, _, err := e.ListFacts(ctx, FactListQuery{EntityName: "Kimse"}); err != nil || len(items) != 0 {
		t.Fatalf("unknown entity = %+v %v, want none", items, err)
	}
	if items, truncated, err := e.ListFacts(ctx, FactListQuery{Limit: 1}); err != nil || len(items) != 1 || !truncated {
		t.Fatalf("limit 1 = %+v %v %v, want one, truncated", items, truncated, err)
	}
	if ids, err := e.FactIDsByEpisode(ctx, ep.ID); err != nil || len(ids) != 1 || ids[0] != first {
		t.Fatalf("FactIDsByEpisode = %v %v, want [%d]", ids, err, first)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i := 0; i < MaxFactBatch; i++ {
		if _, err := st.Queries.InsertFact(ctx, sqlcgen.InsertFactParams{
			Subject: fmt.Sprintf("s%d", i), Predicate: "works_at", Object: "o", SourceTier: TierAgentDerived,
			Evidentiality: "inferred", Status: "active", UpdatedAt: now, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if ids, err := e.QuarantinedFactIDsSince(ctx, base); !errors.Is(err, ErrFactBatchTooLarge) || ids != nil {
		t.Fatalf("QuarantinedFactIDsSince(%d facts) = %d ids, %v; want ErrFactBatchTooLarge", MaxFactBatch+2, len(ids), err)
	}
	if ErrFactBatchTooLarge.Error() != "en fazla 500 olgu" {
		t.Fatalf("ErrFactBatchTooLarge = %q, want the handbook's Turkish text", ErrFactBatchTooLarge.Error())
	}
}
