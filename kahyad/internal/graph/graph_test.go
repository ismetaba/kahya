package graph

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/taint"
)

// testMemoryDir / testSecretGlobs mirror policy.yaml's secret_lane_globs
// shape (absolute globs over the memory dir; production uses
// ~/Kahya/memory/finans*.md).
const testMemoryDir = "/m"

var testSecretGlobs = []string{"/m/finans*.md", "/m/gizli/**"}

type policyMatcher struct{}

func (policyMatcher) Match(pattern, path string) (bool, error) {
	return policy.MatchGlob(pattern, path)
}

// fixture is a real temp brain.db with the real factengine (the only fact
// writer) and a Graph over it - the factengine/testhelpers_test.go pattern,
// copied (test helpers are not shared across packages).
type fixture struct {
	t  *testing.T
	st *store.Store
	fe *factengine.Engine
	g  *Graph
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fe := factengine.New(st.Queries, taint.New(st.Queries, st), st)
	lane := memlane.Classifier{Store: st.Queries, MemoryDir: testMemoryDir, SecretGlobs: testSecretGlobs, Matcher: policyMatcher{}}
	return &fixture{t: t, st: st, fe: fe, g: &Graph{Store: st.Queries, Lane: lane}}
}

// episode inserts an active memory_file episode at relPath.
func (f *fixture) episode(relPath string) int64 {
	f.t.Helper()
	row, err := f.st.Queries.InsertEpisode(context.Background(), sqlcgen.InsertEpisodeParams{
		Source: "memory_file", SourcePath: sql.NullString{String: relPath, Valid: true},
		SourceTier: factengine.TierUserAsserted, Status: "active",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		f.t.Fatalf("InsertEpisode(%s): %v", relPath, err)
	}
	return row.ID
}

// entity creates (or reuses) an entity the way a CLOUD-lane extraction call
// does - its first alias in the cloud view (migration 0025).
func (f *fixture) entity(name, kind string) int64 {
	f.t.Helper()
	id, _, _, err := f.fe.ResolveEntityForExtraction(context.Background(), "tr-test", name, kind, true)
	if err != nil {
		f.t.Fatalf("ResolveEntityForExtraction(%s): %v", name, err)
	}
	return id
}

// alias adds a cloud-lane alias (a cloud-lane extraction call's aliases[]).
func (f *fixture) alias(entityID int64, alias string) {
	f.t.Helper()
	if _, err := f.fe.AddAlias(context.Background(), "tr-test", entityID, alias, true); err != nil {
		f.t.Fatalf("AddAlias(%d, %s): %v", entityID, alias, err)
	}
}

// localAlias adds a local-only alias (a local-lane - e.g. secret-note -
// extraction call's aliases[]).
func (f *fixture) localAlias(entityID int64, alias string) {
	f.t.Helper()
	if _, err := f.fe.AddAlias(context.Background(), "tr-test", entityID, alias, false); err != nil {
		f.t.Fatalf("AddAlias(%d, %s, local): %v", entityID, alias, err)
	}
}

// fact writes an external_doc fact (p=0.8, injection-eligible, no session
// needed) citing episode ep (0 = no episode citation), linked to the given
// entity ids (0 = unlinked) by a cloud-lane call.
func (f *fixture) fact(subj string, subjID int64, pred, obj string, objID, ep int64) int64 {
	f.t.Helper()
	return f.factWith(factengine.Candidate{
		Subject: subj, Predicate: pred, Object: obj,
		Provenance: factengine.ProvenanceExternalDoc, EpisodeID: ep,
		SubjectEntityID: subjID, ObjectEntityID: objID, ExtractorVer: "test_v1",
	})
}

// localFact is fact written by a LOCAL-lane call: its links are
// local-only.
func (f *fixture) localFact(subj string, subjID int64, pred, obj string, objID, ep int64) int64 {
	f.t.Helper()
	return f.writeFact(factengine.Candidate{
		Subject: subj, Predicate: pred, Object: obj,
		Provenance: factengine.ProvenanceExternalDoc, EpisodeID: ep,
		SubjectEntityID: subjID, ObjectEntityID: objID, ExtractorVer: "test_v1",
	})
}

// factWith writes c as a cloud-lane call does (LinksCloudLane).
func (f *fixture) factWith(c factengine.Candidate) int64 {
	f.t.Helper()
	c.LinksCloudLane = true
	return f.writeFact(c)
}

func (f *fixture) writeFact(c factengine.Candidate) int64 {
	f.t.Helper()
	id, err := f.fe.WriteFact(context.Background(), "tr-test", c)
	if err != nil {
		f.t.Fatalf("WriteFact(%+v): %v", c, err)
	}
	return id
}

func (f *fixture) walk(q string, eps []int64, opts Options) ([]FactLine, []int64) {
	f.t.Helper()
	lines, ents, err := f.g.Neighbourhood(context.Background(), "tr-walk", q, eps, opts)
	if err != nil {
		f.t.Fatalf("Neighbourhood(%q): %v", q, err)
	}
	return lines, ents
}

func factIDs(lines []FactLine) []int64 {
	out := []int64{}
	for _, l := range lines {
		out = append(out, l.FactID)
	}
	return out
}

func hasFact(lines []FactLine, id int64) bool {
	for _, l := range lines {
		if l.FactID == id {
			return true
		}
	}
	return false
}

// emreAcmeKadikoy seeds a three-ring chain: Emre works_at Acme, Acme
// located_in Kadikoy, Kadikoy part_of Istanbul (all citing one ordinary
// note).
func (f *fixture) emreAcmeKadikoy() (emre, acme, kadikoy, istanbul, worksAt, locatedIn, partOf int64) {
	ep := f.episode("people/emre.md")
	emre = f.entity("Emre", "person")
	acme = f.entity("Acme", "org")
	kadikoy = f.entity("Kadıköy", "place")
	istanbul = f.entity("İstanbul", "place")
	worksAt = f.fact("Emre", emre, "works_at", "Acme", acme, ep)
	locatedIn = f.fact("Acme", acme, "located_in", "Kadıköy", kadikoy, ep)
	partOf = f.fact("Kadıköy", kadikoy, "part_of", "İstanbul", istanbul, ep)
	return
}

func TestNeighbourhoodSeedsFromAliasFoldedToken(t *testing.T) {
	f := newFixture(t)
	emre, acme, _, _, worksAt, _, _ := f.emreAcmeKadikoy()

	lines, ents := f.walk("Emre'nin şirketi", nil, Options{Hops: 0, CloudLane: true})
	if len(lines) != 1 || lines[0].FactID != worksAt {
		t.Fatalf("lines = %+v, want only Emre works_at Acme (fact %d)", lines, worksAt)
	}
	l := lines[0]
	if l.Subject != "Emre" || l.Predicate != "works_at" || l.Object != "Acme" || l.Hop != 0 ||
		l.SubjectEntityID != emre || l.ObjectEntityID != acme || l.SourceTier != factengine.TierExternalDoc {
		t.Fatalf("line = %+v", l)
	}
	if l.ConfidenceP < 0.79 || l.ConfidenceP > 0.81 {
		t.Fatalf("ConfidenceP = %v, want ~0.80 (external_doc cap)", l.ConfidenceP)
	}
	if !reflect.DeepEqual(ents, []int64{emre}) {
		t.Fatalf("entity ids = %v, want the seed only [%d]", ents, emre)
	}
}

func TestNeighbourhoodOneHopExpandsThroughSharedEntity(t *testing.T) {
	f := newFixture(t)
	emre, acme, _, _, worksAt, locatedIn, partOf := f.emreAcmeKadikoy()

	lines, ents := f.walk("Emre nerede çalışıyor", nil, Options{Hops: 1, CloudLane: true})
	if got := factIDs(lines); !reflect.DeepEqual(got, []int64{worksAt, locatedIn}) {
		t.Fatalf("facts = %v, want [%d %d] (hop 0 then hop 1)", got, worksAt, locatedIn)
	}
	if lines[1].Hop != 1 {
		t.Fatalf("Acme located_in Kadıköy hop = %d, want 1", lines[1].Hop)
	}
	if hasFact(lines, partOf) {
		t.Fatalf("hop-2 fact %d returned with Hops:1", partOf)
	}
	if !reflect.DeepEqual(ents, []int64{emre, acme}) {
		t.Fatalf("entity ids = %v, want [%d %d]", ents, emre, acme)
	}

	lines, _ = f.walk("Emre nerede çalışıyor", nil, Options{Hops: 0, CloudLane: true})
	if hasFact(lines, locatedIn) {
		t.Fatalf("Hops:0 returned the hop-1 fact: %+v", lines)
	}
}

func TestNeighbourhoodTwoHopsOnlyWhenEnabled(t *testing.T) {
	f := newFixture(t)
	_, _, _, _, worksAt, locatedIn, partOf := f.emreAcmeKadikoy()

	lines, _ := f.walk("Emre", nil, Options{Hops: 1, CloudLane: true})
	if hasFact(lines, partOf) {
		t.Fatalf("Hops:1 returned the third ring: %+v", lines)
	}
	lines, _ = f.walk("Emre", nil, Options{Hops: 2, CloudLane: true})
	if got := factIDs(lines); !reflect.DeepEqual(got, []int64{worksAt, locatedIn, partOf}) {
		t.Fatalf("Hops:2 facts = %v, want [%d %d %d]", got, worksAt, locatedIn, partOf)
	}
	if lines[2].Hop != 2 {
		t.Fatalf("third ring hop = %d, want 2", lines[2].Hop)
	}
	// Hops above MaxHops are clamped, never deeper.
	clamped, _ := f.walk("Emre", nil, Options{Hops: 9, CloudLane: true})
	if !reflect.DeepEqual(factIDs(clamped), factIDs(lines)) {
		t.Fatalf("Hops:9 = %v, want the Hops:2 result %v", factIDs(clamped), factIDs(lines))
	}
}

// TestNeighbourhoodExcludesQuarantinedAndLowConfidence: the keep rule is
// factengine.InjectionEligible - an unconfirmed agent_derived fact (HANDOFF
// §5 memory #1) and a fact a denial pushed under p=0.3 (memory #3) never
// come back; confirming lifts the quarantine.
func TestNeighbourhoodExcludesQuarantinedAndLowConfidence(t *testing.T) {
	f := newFixture(t)
	ep := f.episode("people/emre.md")
	emre := f.entity("Emre", "person")
	good := f.fact("Emre", emre, "likes", "çay", 0, ep)
	quarantined := f.factWith(factengine.Candidate{
		Subject: "Emre", Predicate: "lives_in", Object: "Moda",
		Provenance: factengine.ProvenanceAgentDerived, EpisodeID: ep,
		SubjectEntityID: emre, ExtractorVer: "test_v1",
	})
	denied := f.fact("Emre", emre, "owns", "tekne", 0, ep)
	if err := f.fe.DenyFact(context.Background(), "tr-test", denied, "deny-session"); err != nil {
		t.Fatalf("DenyFact: %v", err)
	}

	for _, cloud := range []bool{true, false} {
		lines, _ := f.walk("Emre", nil, Options{CloudLane: cloud})
		if got := factIDs(lines); !reflect.DeepEqual(got, []int64{good}) {
			t.Fatalf("cloud=%v facts = %v, want only the eligible fact %d (quarantined %d, denied %d)", cloud, got, good, quarantined, denied)
		}
	}

	if err := f.fe.ConfirmFact(context.Background(), "tr-test", quarantined); err != nil {
		t.Fatalf("ConfirmFact: %v", err)
	}
	lines, _ := f.walk("Emre", nil, Options{CloudLane: true})
	if !hasFact(lines, quarantined) {
		t.Fatalf("a confirmed agent_derived fact must be injectable: %+v", lines)
	}
}

// TestNeighbourhoodCloudLaneDropsSecretPathFacts: a fact citing a
// secret_lane_globs note (finans*.md) never reaches a cloud-lane block,
// whatever else cites it; a local-lane caller still gets it.
func TestNeighbourhoodCloudLaneDropsSecretPathFacts(t *testing.T) {
	f := newFixture(t)
	normal := f.episode("people/emre.md")
	secret := f.episode("finans-x.md")
	emre := f.entity("Emre", "person")
	visible := f.fact("Emre", emre, "works_at", "Acme", 0, normal)
	secretFact := f.fact("Emre", emre, "has_loan_at", "Banka", 0, secret)
	both := f.fact("Emre", emre, "drives", "araba", 0, normal)
	f.fact("Emre", emre, "drives", "araba", 0, secret) // same triple, secret citation added

	cloud, _ := f.walk("Emre", nil, Options{CloudLane: true})
	if got := factIDs(cloud); !reflect.DeepEqual(got, []int64{visible}) {
		t.Fatalf("cloud lane facts = %v, want only %d (secret %d, dual-cited %d dropped)", got, visible, secretFact, both)
	}
	local, _ := f.walk("Emre", nil, Options{CloudLane: false})
	if !hasFact(local, secretFact) || !hasFact(local, both) {
		t.Fatalf("local lane facts = %v, want the secret-path facts too", factIDs(local))
	}
}

// TestNeighbourhoodCloudLaneDropsFactWithoutEpisodeCitation: a fact whose
// evidence cites no episode (session only - e.g. a content-secret
// extraction call) has no classification record and is excluded from the
// cloud lane (fail-closed, memlane).
func TestNeighbourhoodCloudLaneDropsFactWithoutEpisodeCitation(t *testing.T) {
	f := newFixture(t)
	emre := f.entity("Emre", "person")
	uncited := f.factWith(factengine.Candidate{
		Subject: "Emre", Predicate: "works_at", Object: "Acme",
		Provenance: factengine.ProvenanceExternalDoc, SessionID: "sess-no-episode",
		SubjectEntityID: emre, ExtractorVer: "test_v1",
	})
	if lines, _ := f.walk("Emre", nil, Options{CloudLane: true}); len(lines) != 0 {
		t.Fatalf("cloud lane lines = %+v, want none (no citation => secret)", lines)
	}
	if lines, _ := f.walk("Emre", nil, Options{CloudLane: false}); !hasFact(lines, uncited) {
		t.Fatalf("local lane lines = %+v, want the uncited fact", lines)
	}
}

func TestNeighbourhoodDeterministicOrder(t *testing.T) {
	f := newFixture(t)
	ep := f.episode("people/emre.md")
	emre := f.entity("Emre", "person")
	for _, obj := range []string{"çay", "kahve", "su", "ayran", "boza"} {
		f.fact("Emre", emre, "likes", obj, 0, ep)
	}
	// Higher confidence ranks first within a hop.
	tracker := taint.New(f.st.Queries, f.st)
	if err := tracker.InsertClean(context.Background(), "tr-test", "clean-1"); err != nil {
		t.Fatalf("InsertClean: %v", err)
	}
	top := f.factWith(factengine.Candidate{
		Subject: "Emre", Predicate: "likes", Object: "simit",
		Provenance: factengine.ProvenanceUserAsserted, SessionID: "clean-1", EpisodeID: ep,
		SubjectEntityID: emre, ExtractorVer: "test_v1",
	})
	first, _ := f.walk("Emre", []int64{ep}, Options{Hops: 1, CloudLane: true})
	second, _ := f.walk("Emre", []int64{ep}, Options{Hops: 1, CloudLane: true})
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two walks differ:\n%+v\n%+v", first, second)
	}
	if len(first) != 6 || first[0].FactID != top {
		t.Fatalf("facts = %v, want 6 with the user_asserted fact %d first", factIDs(first), top)
	}
	for i := 2; i < len(first); i++ {
		if first[i].FactID < first[i-1].FactID {
			t.Fatalf("equal-confidence facts not in id order: %v", factIDs(first))
		}
	}
	capped, _ := f.walk("Emre", nil, Options{MaxFacts: 2, CloudLane: true})
	if got := factIDs(capped); !reflect.DeepEqual(got, factIDs(first)[:2]) {
		t.Fatalf("MaxFacts:2 = %v, want the first two %v", got, factIDs(first)[:2])
	}
}

// TestNeighbourhoodSeedsFromHitEpisodes: the second seed source - an entity
// the query never names is reached through a fact citing a chunk hit's
// episode.
func TestNeighbourhoodSeedsFromHitEpisodes(t *testing.T) {
	f := newFixture(t)
	ep := f.episode("people/emre.md")
	emre := f.entity("Emre", "person")
	acme := f.entity("Acme", "org")
	worksAt := f.fact("Emre", emre, "works_at", "Acme", acme, ep)

	if lines, _ := f.walk("iş yeri", nil, Options{CloudLane: true}); len(lines) != 0 {
		t.Fatalf("no seed at all, got %+v", lines)
	}
	lines, ents := f.walk("iş yeri", []int64{ep}, Options{CloudLane: true})
	if !reflect.DeepEqual(factIDs(lines), []int64{worksAt}) || !reflect.DeepEqual(ents, []int64{emre, acme}) {
		t.Fatalf("episode-seeded walk = facts %v ents %v, want [%d] [%d %d]", factIDs(lines), ents, worksAt, emre, acme)
	}
}

// TestNeighbourhoodCloudLaneSecretAliasNeverSeeds: an alias that only a
// secret note carries (the extractor added it from a finans*.md episode)
// is not a visible name, so a query using it seeds nothing in the cloud
// lane - the block never reveals which entity a secret alias belongs to. A
// local-lane caller still resolves it.
func TestNeighbourhoodCloudLaneSecretAliasNeverSeeds(t *testing.T) {
	f := newFixture(t)
	normal := f.episode("people/emre.md")
	secret := f.episode("finans-borc.md")
	emre := f.entity("Emre", "person")
	f.localAlias(emre, "Patron")
	worksAt := f.fact("Emre", emre, "works_at", "Acme", 0, normal)
	f.localFact("Patron", emre, "owes", "Banka", 0, secret)

	if lines, ents := f.walk("Patron ne yaptı", nil, Options{CloudLane: true}); len(lines) != 0 || len(ents) != 0 {
		t.Fatalf("cloud lane: a secret-only alias seeded %v / %v", factIDs(lines), ents)
	}
	if lines, _ := f.walk("Emre ne yaptı", nil, Options{CloudLane: true}); !reflect.DeepEqual(factIDs(lines), []int64{worksAt}) {
		t.Fatalf("cloud lane via the visible name = %v, want [%d]", factIDs(lines), worksAt)
	}
	if lines, _ := f.walk("Patron ne yaptı", nil, Options{CloudLane: false}); len(lines) != 2 {
		t.Fatalf("local lane via the secret alias = %v, want both facts", factIDs(lines))
	}
}

// TestNeighbourhoodCloudLaneNeverWalksThroughSecretFact: the walk follows
// only kept facts, so a secret-path link (Emre has_account_at BankX) never
// pulls BankX's own ordinary facts into a cloud block.
func TestNeighbourhoodCloudLaneNeverWalksThroughSecretFact(t *testing.T) {
	f := newFixture(t)
	normal := f.episode("people/emre.md")
	secret := f.episode("finans-hesap.md")
	emre := f.entity("Emre", "person")
	bank := f.entity("BankX", "org")
	worksAt := f.fact("Emre", emre, "works_at", "Acme", 0, normal)
	f.fact("Emre", emre, "has_account_at", "BankX", bank, secret)
	bankFact := f.fact("BankX", bank, "located_in", "Levent", 0, normal)

	cloud, ents := f.walk("Emre", nil, Options{Hops: 2, CloudLane: true})
	if !reflect.DeepEqual(factIDs(cloud), []int64{worksAt}) || hasFact(cloud, bankFact) {
		t.Fatalf("cloud lane facts = %v, want only [%d] (never through the secret link)", factIDs(cloud), worksAt)
	}
	if !reflect.DeepEqual(ents, []int64{emre}) {
		t.Fatalf("cloud lane entity ids = %v, want only the seed", ents)
	}
	local, _ := f.walk("Emre", nil, Options{Hops: 1, CloudLane: false})
	if !hasFact(local, bankFact) {
		t.Fatalf("local lane facts = %v, want the hop through BankX", factIDs(local))
	}
}

// TestNeighbourhoodCloudLaneFollowsOnlyCloudLaneLinks (FIX-MEM-1/MEM-4
// review, migration 0025): a fact row an ordinary note made cloud-eligible
// can still carry a link a LOCAL-lane call wrote first ("Doktor" linked to
// Ayşe Kaya through an alias only a secret note carries). The cloud lane
// never follows, seeds from or reports such a link: the fact is not Ayşe
// Kaya's, its own episode seeds nothing through it, and its entity id is
// reported as 0. The local lane follows every link.
func TestNeighbourhoodCloudLaneFollowsOnlyCloudLaneLinks(t *testing.T) {
	f := newFixture(t)
	ayseEp := f.episode("people/ayse.md")
	komsuEp := f.episode("inbox/komsu.md")
	ayse := f.entity("Ayşe Kaya", "person")
	acme := f.entity("Acme", "org")
	worksAt := f.fact("Ayşe Kaya", ayse, "works_at", "Acme", acme, ayseEp)
	livesIn := f.localFact("Doktor", ayse, "lives_in", "Kadıköy", 0, komsuEp)

	cloud, ents := f.walk("Ayşe Kaya nerede", nil, Options{Hops: 1, CloudLane: true})
	if !reflect.DeepEqual(factIDs(cloud), []int64{worksAt}) {
		t.Fatalf("cloud lane facts = %v, want only [%d] (the local-lane link of %d is never followed)", factIDs(cloud), worksAt, livesIn)
	}
	if !reflect.DeepEqual(ents, []int64{ayse, acme}) {
		t.Fatalf("cloud lane entity ids = %v", ents)
	}
	if lines, ents := f.walk("komşu", []int64{komsuEp}, Options{Hops: 1, CloudLane: true}); len(lines) != 0 || len(ents) != 0 {
		t.Fatalf("cloud lane episode seed through a local-lane link = %v / %v, want nothing", factIDs(lines), ents)
	}
	local, _ := f.walk("Ayşe Kaya nerede", nil, Options{Hops: 1, CloudLane: false})
	if !hasFact(local, livesIn) || !hasFact(local, worksAt) {
		t.Fatalf("local lane facts = %v, want both", factIDs(local))
	}

	recs := f.query(FactQuery{Predicate: "lives_in", CloudLane: true})
	if len(recs) != 1 || recs[0].FactID != livesIn || recs[0].SubjectEntityID != 0 {
		t.Fatalf("cloud fact_query lives_in = %+v, want the fact with subject entity 0 (a local-lane link)", recs)
	}
	if recs := f.query(FactQuery{Predicate: "lives_in"}); len(recs) != 1 || recs[0].SubjectEntityID != ayse {
		t.Fatalf("local fact_query lives_in = %+v, want subject entity %d", recs, ayse)
	}
	if recs := f.query(FactQuery{EntityID: ayse, CloudLane: true}); !reflect.DeepEqual(recIDs(recs), []int64{worksAt}) {
		t.Fatalf("cloud fact_query entity %d = %v, want only [%d]", ayse, recIDs(recs), worksAt)
	}
	if recs := f.query(FactQuery{EntityID: ayse}); !sameIDs(recIDs(recs), []int64{worksAt, livesIn}) {
		t.Fatalf("local fact_query entity %d = %v, want both", ayse, recIDs(recs))
	}

	got := f.lookup("ayşe", true)
	if len(got) != 1 || got[0].FactCount != 1 || !reflect.DeepEqual(got[0].Aliases, []string{"Ayşe Kaya"}) {
		t.Fatalf("cloud entity_lookup = %+v, want Ayşe Kaya with one fact and no \"Doktor\"", got)
	}
	if got := f.lookup("doktor", true); len(got) != 0 {
		t.Fatalf("cloud entity_lookup(doktor) = %+v, want nothing", got)
	}
}

// TestEntityLookupCloudLaneListsOnlyCloudLaneAliases (FIX-MEM-1/MEM-4
// review): an alias row a local-lane call added never matches or is listed
// in the cloud lane - even when a cloud-linked fact uses the same name.
func TestEntityLookupCloudLaneListsOnlyCloudLaneAliases(t *testing.T) {
	f := newFixture(t)
	ep := f.episode("people/emre.md")
	emre := f.entity("Emre", "person")
	f.localAlias(emre, "Patron")
	f.fact("Emre", emre, "works_at", "Acme", 0, ep)
	f.fact("Patron", emre, "likes", "çay", 0, ep)

	if got := f.lookup("patron", true); len(got) != 0 {
		t.Fatalf("cloud lookup by a local-only alias = %+v, want nothing", got)
	}
	got := f.lookup("emre", true)
	if len(got) != 1 || !reflect.DeepEqual(got[0].Aliases, []string{"Emre"}) {
		t.Fatalf("cloud lookup emre = %+v, want aliases [Emre] only", got)
	}
	if local := f.lookup("patron", false); len(local) != 1 || len(local[0].Aliases) != 2 {
		t.Fatalf("local lookup patron = %+v, want the entity with both aliases", local)
	}
}

func TestQueryAliasKeys(t *testing.T) {
	got := QueryAliasKeys("Ahmet Yılmaz'ın telefonu? (Emre) ev ne")
	// "Yılmaz'ın telefonu" keys to "yilmaz" (AliasKey drops everything
	// from the apostrophe on), a duplicate; "ev"/"ne" are too short alone.
	want := []string{"ahmet", "yilmaz", "telefonu", "emre", "ahmet yilmaz", "telefonu emre", "emre ev", "ev ne"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("QueryAliasKeys = %q, want %q", got, want)
	}
	if QueryAliasKeys("  ") != nil {
		t.Fatalf("blank query gave keys")
	}
}

type failingStore struct {
	Store
	err error
}

func (s failingStore) ListFactsByEntityHop(context.Context, sqlcgen.ListFactsByEntityHopParams) ([]sqlcgen.Fact, error) {
	return nil, s.err
}

// TestNeighbourhoodStoreErrorIsReturned: a read error is the caller's to
// handle (the server then injects chunks only) - never a partial, silently
// truncated section.
func TestNeighbourhoodStoreErrorIsReturned(t *testing.T) {
	f := newFixture(t)
	f.emreAcmeKadikoy()
	boom := errors.New("disk on fire")
	g := &Graph{Store: failingStore{Store: f.st.Queries, err: boom}, Lane: f.g.Lane}
	if _, _, err := g.Neighbourhood(context.Background(), "tr", "Emre", nil, Options{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store error", err)
	}
	var nilGraph *Graph
	if _, _, err := nilGraph.Neighbourhood(context.Background(), "tr", "Emre", nil, Options{}); err == nil {
		t.Fatalf("a nil Graph must refuse")
	}
}
