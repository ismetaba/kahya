package factengine

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

// G13 MEM-2 phase 2: the entity read side behind `kahya entity list/show`.

func TestListEntitiesPrefixFoldedAndProvisionalFilter(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()
	emre, _, _, err := e.ResolveEntityForExtraction(ctx, "tr", "Emre", EntityKindPerson)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddAlias(ctx, "tr", emre, "Emre Bey"); err != nil {
		t.Fatal(err)
	}
	acme, _, _, err := e.ResolveEntityForExtraction(ctx, "tr", "Acme", EntityKindOrg)
	if err != nil {
		t.Fatal(err)
	}
	// A second "Emre" of another kind is a provisional namesake.
	emre2, _, prov, err := e.ResolveEntityForExtraction(ctx, "tr", "EMRE", EntityKindOrg)
	if err != nil || !prov {
		t.Fatalf("namesake = %d provisional %v (%v), want provisional", emre2, prov, err)
	}

	got, err := e.ListEntities(ctx, "em", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != emre || got[1].ID != emre2 {
		t.Fatalf("prefix em = %+v, want Emre then the provisional EMRE", got)
	}
	if !reflect.DeepEqual(got[0].Aliases, []string{"Emre", "Emre Bey"}) || got[0].Kind != EntityKindPerson || got[0].Provisional {
		t.Fatalf("Emre row = %+v", got[0])
	}
	// Turkish case folding: "İ"/"i" and the apostrophe suffix of the query.
	if got, err := e.ListEntities(ctx, "EMRE'nin", false, 0); err != nil || len(got) != 2 {
		t.Fatalf("folded query = %+v, %v; want both Emre rows", got, err)
	}
	if got, err := e.ListEntities(ctx, "", true, 0); err != nil || len(got) != 1 || got[0].ID != emre2 || !got[0].Provisional {
		t.Fatalf("provisional only = %+v, %v", got, err)
	}
	if got, err := e.ListEntities(ctx, "", false, 0); err != nil || len(got) != 3 || got[1].ID != acme {
		t.Fatalf("all = %+v, %v; want 3 in id order", got, err)
	}
	if got, err := e.ListEntities(ctx, "", false, 1); err != nil || len(got) != 1 {
		t.Fatalf("limit 1 = %+v, %v", got, err)
	}
	if got, err := e.ListEntities(ctx, "zz", false, 0); err != nil || len(got) != 0 {
		t.Fatalf("no match = %+v, %v", got, err)
	}
}

func TestShowEntityAliasesFactsAndQuarantine(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	emre, _, _, _ := e.ResolveEntityForExtraction(ctx, "tr", "Emre", EntityKindPerson)
	acme, _, _, _ := e.ResolveEntityForExtraction(ctx, "tr", "Acme", EntityKindOrg)
	insertCleanSession(t, st, "clean-1")
	userFact, err := e.WriteFact(ctx, "tr", Candidate{
		Subject: "Emre", Predicate: "works_at", Object: "Acme", Provenance: ProvenanceUserAsserted,
		SessionID: "clean-1", ExtractorVer: "test_v1", SubjectEntityID: emre, ObjectEntityID: acme,
		ValidFrom: "2026-05-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	agentFact, err := e.WriteFact(ctx, "tr", Candidate{
		Subject: "Emre", Predicate: "lives_in", Object: "Kadıköy", ExtractorVer: "test_v1", SubjectEntityID: emre,
	})
	if err != nil {
		t.Fatal(err)
	}

	d, err := e.ShowEntity(ctx, emre)
	if err != nil {
		t.Fatal(err)
	}
	if d.CanonicalName != "Emre" || d.Kind != EntityKindPerson || d.ActiveFacts != 2 || !reflect.DeepEqual(d.Aliases, []string{"Emre"}) {
		t.Fatalf("detail = %+v", d.EntitySummary)
	}
	if len(d.Facts) != 2 || d.Facts[0].FactID != userFact || d.Facts[1].FactID != agentFact {
		t.Fatalf("facts = %+v, want the user fact (higher confidence) first", d.Facts)
	}
	uf, af := d.Facts[0], d.Facts[1]
	if uf.Quarantined || uf.SourceTier != TierUserAsserted || math.Abs(uf.ConfidenceP-0.95) > 0.001 || uf.ValidFrom == "" {
		t.Fatalf("user fact = %+v, want unquarantined user_asserted p=0.95 with valid_from", uf)
	}
	if !af.Quarantined || af.SourceTier != TierAgentDerived || math.Abs(af.ConfidenceP-0.4) > 0.001 {
		t.Fatalf("agent fact = %+v, want quarantined agent_derived p=0.40", af)
	}
	if err := e.ConfirmFact(ctx, "tr", agentFact); err != nil {
		t.Fatal(err)
	}
	if d, _ = e.ShowEntity(ctx, emre); d.Facts[1].Quarantined {
		t.Fatal("a confirmed agent_derived fact is still shown quarantined")
	}
	if a, err := e.ShowEntity(ctx, acme); err != nil || a.ActiveFacts != 1 || len(a.Facts) != 1 || a.Facts[0].FactID != userFact {
		t.Fatalf("Acme detail = %+v, %v; want the fact it is the object of", a, err)
	}
}

func TestShowEntityUnknownIsErrEntityNotFound(t *testing.T) {
	e, _ := newTestEngine(t)
	if _, err := e.ShowEntity(context.Background(), 4242); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("ShowEntity(unknown) = %v, want ErrEntityNotFound", err)
	}
}

func TestLogOddsToProbability(t *testing.T) {
	for lo, want := range map[float64]float64{0: 0.5, CapLogOddsUserAsserted: 0.95, CapLogOddsAgentDerived: 0.4, InjectionThresholdLogOdds: 0.3} {
		if got := LogOddsToProbability(lo); math.Abs(got-want) > 0.001 {
			t.Errorf("LogOddsToProbability(%v) = %v, want %v", lo, got, want)
		}
	}
}
