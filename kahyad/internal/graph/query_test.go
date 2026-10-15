package graph

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"kahya/kahyad/internal/factengine"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return ts
}

func (f *fixture) query(q FactQuery) []FactRecord {
	f.t.Helper()
	f.g.DB = f.st.DB()
	out, err := f.g.QueryFacts(context.Background(), "tr-q", q)
	if err != nil {
		f.t.Fatalf("QueryFacts(%+v): %v", q, err)
	}
	return out
}

func recIDs(rs []FactRecord) []int64 {
	out := []int64{}
	for _, r := range rs {
		out = append(out, r.FactID)
	}
	return out
}

// quarantinedFact writes an unconfirmed agent_derived fact (the extractor's
// provenance): quarantined by HANDOFF §5 memory #1.
func (f *fixture) quarantinedFact(subj string, subjID int64, pred, obj string, ep int64) int64 {
	f.t.Helper()
	return f.factWith(factengine.Candidate{
		Subject: subj, Predicate: pred, Object: obj, Provenance: factengine.ProvenanceAgentDerived,
		EpisodeID: ep, SubjectEntityID: subjID, ExtractorVer: "test_v1",
	})
}

// TestFactQueryDefaultsExcludeQuarantined (FIX-MEM-4, HANDOFF §5 memory
// #1/#3): by default fact_query answers only injection-eligible facts - an
// unconfirmed agent_derived fact and one a denial pushed under p=0.3 stay
// out.
func TestFactQueryDefaultsExcludeQuarantined(t *testing.T) {
	f := newFixture(t)
	ep := f.episode("people/emre.md")
	emre := f.entity("Emre", "person")
	good := f.fact("Emre", emre, "works_at", "Acme", 0, ep)
	f.quarantinedFact("Emre", emre, "lives_in", "Moda", ep)
	denied := f.fact("Emre", emre, "owns", "tekne", 0, ep)
	if err := f.fe.DenyFact(context.Background(), "tr", denied, "deny-1"); err != nil {
		t.Fatalf("DenyFact: %v", err)
	}
	for _, cloud := range []bool{true, false} {
		got := f.query(FactQuery{EntityID: emre, CloudLane: cloud})
		if !reflect.DeepEqual(recIDs(got), []int64{good}) || got[0].Quarantined {
			t.Fatalf("cloud=%v: %+v, want only the eligible fact %d", cloud, got, good)
		}
	}
}

// TestFactQueryIncludeQuarantinedLabels (decision MEM-4.a): an explicit
// include_quarantined query also returns the hidden facts, each labelled.
func TestFactQueryIncludeQuarantinedLabels(t *testing.T) {
	f := newFixture(t)
	ep := f.episode("people/emre.md")
	emre := f.entity("Emre", "person")
	good := f.fact("Emre", emre, "works_at", "Acme", 0, ep)
	q := f.quarantinedFact("Emre", emre, "lives_in", "Moda", ep)

	got := f.query(FactQuery{EntityID: emre, IncludeQuarantined: true, CloudLane: true})
	labels := map[int64]bool{}
	for _, r := range got {
		labels[r.FactID] = r.Quarantined
	}
	if len(got) != 2 || labels[good] || !labels[q] {
		t.Fatalf("include_quarantined = %+v, want both, only %d labelled", got, q)
	}
}

// TestFactQueryAsOfBitemporal: as_of keeps the facts valid at that instant
// (valid_from <= as_of < valid_to), an unknown bound counts as open.
func TestFactQueryAsOfBitemporal(t *testing.T) {
	f := newFixture(t)
	ep := f.episode("people/emre.md")
	emre := f.entity("Emre", "person")
	window := f.factWith(factengine.Candidate{Subject: "Emre", Predicate: "works_at", Object: "Acme", Provenance: factengine.ProvenanceExternalDoc,
		EpisodeID: ep, SubjectEntityID: emre, ExtractorVer: "t", ValidFrom: "2026-01-01T00:00:00Z", ValidTo: "2026-06-01T00:00:00Z"})
	open := f.factWith(factengine.Candidate{Subject: "Emre", Predicate: "lives_in", Object: "Moda", Provenance: factengine.ProvenanceExternalDoc,
		EpisodeID: ep, SubjectEntityID: emre, ExtractorVer: "t"})
	later := f.factWith(factengine.Candidate{Subject: "Emre", Predicate: "works_at", Object: "Beta", Provenance: factengine.ProvenanceExternalDoc,
		EpisodeID: ep, SubjectEntityID: emre, ExtractorVer: "t", ValidFrom: "2026-06-01T00:00:00Z"})

	march := f.query(FactQuery{EntityID: emre, AsOf: mustTime(t, "2026-03-01T00:00:00Z")})
	if !sameIDs(recIDs(march), []int64{window, open}) {
		t.Fatalf("as_of 2026-03 = %v, want [%d %d] (window + open start)", recIDs(march), window, open)
	}
	july := f.query(FactQuery{EntityID: emre, AsOf: mustTime(t, "2026-07-01T00:00:00Z")})
	if !sameIDs(recIDs(july), []int64{open, later}) {
		t.Fatalf("as_of 2026-07 = %v, want [%d %d] (the window has ended)", recIDs(july), open, later)
	}
	// valid_to is exclusive: the window does not hold at its own end.
	if end := f.query(FactQuery{EntityID: emre, AsOf: mustTime(t, "2026-06-01T00:00:00Z")}); !sameIDs(recIDs(end), []int64{open, later}) {
		t.Fatalf("as_of = valid_to: %v, want [%d %d]", recIDs(end), open, later)
	}
	// since/until bound valid_from; an unknown valid_from never matches.
	if got := f.query(FactQuery{EntityID: emre, Since: mustTime(t, "2026-05-01T00:00:00Z")}); !reflect.DeepEqual(recIDs(got), []int64{later}) {
		t.Fatalf("since 2026-05 = %v, want [%d]", recIDs(got), later)
	}
	if got := f.query(FactQuery{EntityID: emre, Until: mustTime(t, "2026-06-01T00:00:00Z")}); !reflect.DeepEqual(recIDs(got), []int64{window}) {
		t.Fatalf("until 2026-06 = %v, want [%d]", recIDs(got), window)
	}
}

// TestFactQueryKnownAsOfUsesCreatedAt: known_as_of is transaction time - a
// fact written after that instant was not known then.
func TestFactQueryKnownAsOfUsesCreatedAt(t *testing.T) {
	f := newFixture(t)
	ep := f.episode("people/emre.md")
	emre := f.entity("Emre", "person")
	f.fe.SetClock(func() time.Time { return mustTime(t, "2026-04-10T09:00:00Z") })
	early := f.fact("Emre", emre, "works_at", "Acme", 0, ep)
	f.fe.SetClock(func() time.Time { return mustTime(t, "2026-05-20T09:00:00Z") })
	late := f.fact("Emre", emre, "lives_in", "Moda", 0, ep)

	got := f.query(FactQuery{EntityID: emre, KnownAsOf: mustTime(t, "2026-05-01T00:00:00Z")})
	if !reflect.DeepEqual(recIDs(got), []int64{early}) || got[0].KnownSince != "2026-04-10T09:00:00Z" {
		t.Fatalf("known_as_of 2026-05 = %+v, want only %d known since 2026-04-10", got, early)
	}
	if got := f.query(FactQuery{EntityID: emre}); !sameIDs(recIDs(got), []int64{early, late}) {
		t.Fatalf("no bound = %v, want both", recIDs(got))
	}
}

// TestFactQuerySubjectExactThenFolded: subject/object match byte-exact
// first, the folded (Turkish I/i, case) string only when that finds
// nothing; an entity name resolves through the folded alias key.
func TestFactQuerySubjectExactThenFolded(t *testing.T) {
	f := newFixture(t)
	ep := f.episode("places/ist.md")
	ist := f.entity("İstanbul", "place")
	fact := f.fact("İstanbul", ist, "part_of", "Türkiye", 0, ep)
	other := f.fact("istanbul", 0, "part_of", "Marmara", 0, ep)

	if got := f.query(FactQuery{Subject: "İstanbul"}); !reflect.DeepEqual(recIDs(got), []int64{fact}) {
		t.Fatalf("exact subject = %v, want [%d]", recIDs(got), fact)
	}
	if got := f.query(FactQuery{Subject: "ISTANBUL"}); !sameIDs(recIDs(got), []int64{fact, other}) {
		t.Fatalf("folded subject = %v, want both", recIDs(got))
	}
	if got := f.query(FactQuery{EntityName: "istanbul'da", Predicate: "part_of"}); !reflect.DeepEqual(recIDs(got), []int64{fact}) {
		t.Fatalf("entity name = %v, want the linked fact %d", recIDs(got), fact)
	}
	if got := f.query(FactQuery{EntityName: "Ankara"}); len(got) != 0 {
		t.Fatalf("unknown entity name = %v, want none", recIDs(got))
	}
	if got := f.query(FactQuery{Predicate: "part_of", Object: "Marmara"}); !reflect.DeepEqual(recIDs(got), []int64{other}) {
		t.Fatalf("predicate+object = %v, want [%d]", recIDs(got), other)
	}
}

func TestFactQueryMinTierAndLimit(t *testing.T) {
	f := newFixture(t)
	ep := f.episode("people/emre.md")
	emre := f.entity("Emre", "person")
	ext := f.fact("Emre", emre, "works_at", "Acme", 0, ep)
	q := f.quarantinedFact("Emre", emre, "lives_in", "Moda", ep)
	if got := f.query(FactQuery{EntityID: emre, MinTier: factengine.TierExternalDoc, IncludeQuarantined: true}); !reflect.DeepEqual(recIDs(got), []int64{ext}) {
		t.Fatalf("min_tier external_doc = %v, want [%d] (agent_derived %d below the floor)", recIDs(got), ext, q)
	}
	if got := f.query(FactQuery{EntityID: emre, IncludeQuarantined: true, Limit: 1}); len(got) != 1 {
		t.Fatalf("limit 1 = %v", recIDs(got))
	}
	f.g.DB = f.st.DB()
	if _, err := f.g.QueryFacts(context.Background(), "tr", FactQuery{MinTier: "trusted"}); !errors.Is(err, ErrUnknownTier) {
		t.Fatalf("unknown min_tier err = %v", err)
	}
}

// TestFactQueryCloudLaneDropsSecretPathFacts: a cloud-lane fact_query (a
// worker's tool call) never answers a fact citing a secret_lane_globs note;
// the local CLI lane does.
func TestFactQueryCloudLaneDropsSecretPathFacts(t *testing.T) {
	f := newFixture(t)
	normal := f.episode("people/emre.md")
	secret := f.episode("finans-emre.md")
	emre := f.entity("Emre", "person")
	visible := f.fact("Emre", emre, "works_at", "Acme", 0, normal)
	hidden := f.fact("Emre", emre, "has_loan_at", "BankX", 0, secret)

	if got := f.query(FactQuery{EntityID: emre, IncludeQuarantined: true, CloudLane: true}); !reflect.DeepEqual(recIDs(got), []int64{visible}) {
		t.Fatalf("cloud lane = %v, want only %d (secret %d dropped)", recIDs(got), visible, hidden)
	}
	if got := f.query(FactQuery{EntityID: emre}); !sameIDs(recIDs(got), []int64{visible, hidden}) {
		t.Fatalf("local lane = %v, want both", recIDs(got))
	}
}

func (f *fixture) lookup(name string, cloud bool) []EntityRecord {
	f.t.Helper()
	out, err := f.g.LookupEntities(context.Background(), "tr-l", name, 0, cloud)
	if err != nil {
		f.t.Fatalf("LookupEntities(%q): %v", name, err)
	}
	return out
}

func TestEntityLookupReturnsAliasesAndTopFacts(t *testing.T) {
	f := newFixture(t)
	ep := f.episode("people/emre.md")
	emre := f.entity("Emre Yılmaz", "person")
	f.alias(emre, "Emre")
	works := f.fact("Emre", emre, "works_at", "Acme", 0, ep)
	f.quarantinedFact("Emre Yılmaz", emre, "lives_in", "Moda", ep)
	f.entity("Emine", "person") // a prefix neighbour with no facts

	got := f.lookup("emre", true)
	if len(got) != 1 {
		t.Fatalf("lookup emre = %+v, want one entity", got)
	}
	e := got[0]
	if e.EntityID != emre || e.CanonicalName != "Emre Yılmaz" || e.Kind != "person" || e.FactCount != 2 {
		t.Fatalf("entity = %+v", e)
	}
	if !sameStrings(e.Aliases, []string{"Emre Yılmaz", "Emre"}) {
		t.Fatalf("aliases = %v", e.Aliases)
	}
	if len(e.TopFacts) != 1 || e.TopFacts[0].FactID != works || e.TopFacts[0].Quarantined {
		t.Fatalf("top facts = %+v, want only the eligible %d (never a quarantined one)", e.TopFacts, works)
	}
	// Local lane: every entity whose name has the prefix, facts or not.
	if local := f.lookup("em", false); len(local) != 2 {
		t.Fatalf("local lookup em = %+v, want Emre and Emine", local)
	}
	// Cloud lane: an entity with no cloud-eligible fact has no visible name.
	if cloud := f.lookup("em", true); len(cloud) != 1 || cloud[0].EntityID != emre {
		t.Fatalf("cloud lookup em = %+v, want only Emre", cloud)
	}
}

// TestEntityLookupCloudLaneHidesSecretAliasesAndCounts: in the cloud lane an
// alias only a secret note carries is neither matched nor listed, the
// canonical name is replaced when only a secret note uses it, and the
// secret facts are not counted.
func TestEntityLookupCloudLaneHidesSecretAliasesAndCounts(t *testing.T) {
	f := newFixture(t)
	normal := f.episode("people/emre.md")
	secret := f.episode("finans-borc.md")
	emre := f.entity("Dr. Emre", "person")
	f.alias(emre, "Emre")
	f.localAlias(emre, "Patron")
	f.fact("Emre", emre, "works_at", "Acme", 0, normal)
	f.localFact("Patron", emre, "owes", "BankX", 0, secret)
	f.localFact("Dr. Emre", emre, "treats", "hasta", 0, secret)

	if got := f.lookup("patron", true); len(got) != 0 {
		t.Fatalf("cloud lookup by a secret-only alias = %+v, want nothing", got)
	}
	got := f.lookup("emre", true)
	if len(got) != 1 {
		t.Fatalf("cloud lookup emre = %+v", got)
	}
	e := got[0]
	if e.CanonicalName != "Emre" || !reflect.DeepEqual(e.Aliases, []string{"Emre"}) || e.FactCount != 1 {
		t.Fatalf("cloud record = %+v, want canonical Emre, aliases [Emre], 1 fact", e)
	}
	local := f.lookup("patron", false)
	if len(local) != 1 || local[0].CanonicalName != "Dr. Emre" || len(local[0].Aliases) != 3 || local[0].FactCount != 3 {
		t.Fatalf("local record = %+v, want the full entity", local)
	}
}

func sameIDs(a, b []int64) bool {
	m := map[int64]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return len(a) == len(b)
}

func sameStrings(a, b []string) bool {
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return len(a) == len(b)
}
