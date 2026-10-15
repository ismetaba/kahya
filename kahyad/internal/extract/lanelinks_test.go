package extract

import (
	"context"
	"strings"
	"testing"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/graph"
	"kahya/kahyad/internal/reader"
)

// laneLinkNote is one memory note of the lane-link scenarios below: its
// path, its single chunk's text, and the answer the (fake) extractor gives
// for it.
type laneLinkNote struct {
	path, text string
	answer     func(chunkID int64) reader.FactExtractV1
}

// runLaneLinkNotes extracts the notes in order through the REAL runner
// (path glob, guard classifier, lane split, ResolveEntityForExtraction,
// AddAlias, WriteFact) and returns each note's episode id plus a graph
// layer over the same brain.db with the runner's own lane rule.
func runLaneLinkNotes(t *testing.T, marker string, prov factengine.Provenance, notes []laneLinkNote) (*fixture, *graph.Graph, map[string]int64) {
	t.Helper()
	byText := map[string]laneLinkNote{}
	for _, n := range notes {
		byText[n.text] = n
	}
	f := newFixture(t, func(_ int, chunks []reader.ExtractChunk, forceSecret bool) (reader.FactExtractV1, string, error) {
		n, ok := byText[chunks[0].Text]
		if !ok {
			t.Fatalf("unexpected chunk %q", chunks[0].Text)
		}
		return n.answer(chunks[0].ID), laneOf(forceSecret), nil
	})
	f.secretOn = marker
	eps := map[string]int64{}
	for _, n := range notes {
		ep, _ := f.seedEpisode(n.path, n.text)
		if _, err := f.runner.ExtractEpisode(context.Background(), "tr-lanelinks", ep, prov); err != nil {
			t.Fatalf("ExtractEpisode(%s): %v", n.path, err)
		}
		eps[n.path] = ep
	}
	return f, &graph.Graph{Store: f.st.Queries, DB: f.st.DB(), Lane: f.runner.Lane}, eps
}

func ent(name, kind string, aliases ...string) reader.ExtractEntityV1 {
	return reader.ExtractEntityV1{Name: name, Kind: kind, Aliases: aliases}
}

func triple(chunkID int64, s, p, o string) reader.ExtractFactV1 {
	return reader.ExtractFactV1{ChunkID: chunkID, Subject: s, Predicate: p, Object: o, Evidentiality: factengine.Witnessed, Importance: 0.5}
}

// assertCloudLookupNeverPairs fails when a cloud-lane entity_lookup for
// query returns an entity carrying the name hidden (as canonical name or
// alias): the two names are tied together only by a secret-lane note.
func assertCloudLookupNeverPairs(t *testing.T, g *graph.Graph, query, hidden string) []graph.EntityRecord {
	t.Helper()
	recs, err := g.LookupEntities(context.Background(), "tr-lookup", query, 0, true)
	if err != nil {
		t.Fatalf("LookupEntities(%q): %v", query, err)
	}
	for _, r := range recs {
		if r.CanonicalName == hidden {
			t.Fatalf("cloud entity_lookup(%q) = %+v: canonical name %q comes from a secret-lane association", query, r, hidden)
		}
		for _, a := range r.Aliases {
			if a == hidden {
				t.Fatalf("cloud entity_lookup(%q) = %+v: alias %q comes from a secret-lane association", query, r, hidden)
			}
		}
	}
	return recs
}

func assertNoPredicate(t *testing.T, what string, lines []graph.FactLine, preds ...string) {
	t.Helper()
	for _, l := range lines {
		for _, p := range preds {
			if l.Predicate == p {
				t.Fatalf("%s carries %s | %s | %s: the walk crossed a link only a secret-lane note establishes (lines %+v)", what, l.Subject, l.Predicate, l.Object, lines)
			}
		}
	}
}

func walkCloud(t *testing.T, g *graph.Graph, q string, eps []int64) []graph.FactLine {
	t.Helper()
	lines, _, err := g.Neighbourhood(context.Background(), "tr-walk", q, eps, graph.Options{Hops: 1, CloudLane: true})
	if err != nil {
		t.Fatalf("Neighbourhood(%q): %v", q, err)
	}
	return lines
}

// cloudFactEntities returns the non-zero entity ids the cloud-lane
// fact_query reports for the fact with predicate pred.
func cloudFactEntities(t *testing.T, g *graph.Graph, pred string) map[int64]bool {
	t.Helper()
	recs, err := g.QueryFacts(context.Background(), "tr-fq", graph.FactQuery{Predicate: pred, IncludeQuarantined: true, CloudLane: true})
	if err != nil {
		t.Fatalf("QueryFacts(%s): %v", pred, err)
	}
	if len(recs) != 1 {
		t.Fatalf("cloud fact_query %s = %+v, want the one cloud-eligible fact", pred, recs)
	}
	out := map[int64]bool{}
	for _, id := range []int64{recs[0].SubjectEntityID, recs[0].ObjectEntityID} {
		if id != 0 {
			out[id] = true
		}
	}
	return out
}

func assertDisjoint(t *testing.T, a, b map[int64]bool, what string) {
	t.Helper()
	for id := range a {
		if b[id] {
			t.Fatalf("%s: cloud fact_query reports entity %d on both facts - the shared id reveals an association only a secret-lane note establishes", what, id)
		}
	}
}

// TestCloudLaneEntityLinksNeverComeFromASecretNoteAlias (FIX-MEM-1/MEM-4
// review): a secret-path note (finans-doktor.md: "Doktorum Ayşe Kaya") is
// extracted on the local model and gives the entity "Ayşe Kaya" the alias
// "Doktor". Two ordinary notes - "Doktor ile kahve içtim" and "Ayşe Kaya
// Kadıköy'de yaşıyor" - are extracted on the cloud lane. Nothing a cloud
// model may see ties "Doktor" to "Ayşe Kaya", so no cloud-lane read may tie
// them: entity_lookup lists neither name under the other, a <olgular> walk
// seeded from the coffee note (or the name "Doktor") never pulls in the
// lives_in fact, and fact_query reports no shared entity id. The local lane
// keeps the association (the user's own surface).
func TestCloudLaneEntityLinksNeverComeFromASecretNoteAlias(t *testing.T) {
	notes := []laneLinkNote{
		{path: "finans-doktor.md", text: "Doktorum Ayşe Kaya, muayene ücreti 2000 TL.", answer: func(c int64) reader.FactExtractV1 {
			return reader.FactExtractV1{
				Entities: []reader.ExtractEntityV1{ent("Ayşe Kaya", factengine.EntityKindPerson, "Doktor")},
				Facts:    []reader.ExtractFactV1{triple(c, "Ayşe Kaya", "is", "kullanıcının doktoru")},
			}
		}},
		{path: "inbox/2026-07-10.md", text: "Doktor ile kahve içtim.", answer: func(c int64) reader.FactExtractV1 {
			return reader.FactExtractV1{
				Entities: []reader.ExtractEntityV1{ent("Doktor", factengine.EntityKindPerson), ent("Kullanıcı", factengine.EntityKindPerson)},
				Facts:    []reader.ExtractFactV1{triple(c, "Kullanıcı", "had_coffee_with", "Doktor")},
			}
		}},
		{path: "people/ayse.md", text: "Ayşe Kaya Kadıköy'de yaşıyor.", answer: func(c int64) reader.FactExtractV1 {
			return reader.FactExtractV1{
				Entities: []reader.ExtractEntityV1{ent("Ayşe Kaya", factengine.EntityKindPerson), ent("Kadıköy", factengine.EntityKindPlace)},
				Facts:    []reader.ExtractFactV1{triple(c, "Ayşe Kaya", "lives_in", "Kadıköy")},
			}
		}},
	}
	for _, prov := range []factengine.Provenance{factengine.ProvenanceExternalDoc, factengine.ProvenanceAgentDerived} {
		t.Run(string(prov), func(t *testing.T) {
			f, g, eps := runLaneLinkNotes(t, "", prov, notes)
			if !f.reader.call(0).forceSecret || f.reader.call(1).forceSecret || f.reader.call(2).forceSecret {
				t.Fatal("scenario: the secret-path note must run locally and the two ordinary notes on the cloud lane")
			}

			doktor := assertCloudLookupNeverPairs(t, g, "doktor", "Ayşe Kaya")
			if len(doktor) != 1 || doktor[0].CanonicalName != "Doktor" {
				t.Fatalf("cloud entity_lookup(doktor) = %+v, want the coffee note's own entity \"Doktor\"", doktor)
			}
			assertCloudLookupNeverPairs(t, g, "ayşe", "Doktor")
			assertDisjoint(t, cloudFactEntities(t, g, "had_coffee_with"), cloudFactEntities(t, g, "lives_in"), "coffee vs lives_in")

			if prov == factengine.ProvenanceExternalDoc {
				coffee := walkCloud(t, g, "dün kahve içtim", []int64{eps["inbox/2026-07-10.md"]})
				if len(coffee) != 1 || coffee[0].Predicate != "had_coffee_with" {
					t.Fatalf("cloud <olgular> for the coffee note = %+v, want only the coffee fact", coffee)
				}
				assertNoPredicate(t, "cloud <olgular> for \"Doktor\"", walkCloud(t, g, "Doktor kim", nil), "lives_in", "is")
				assertNoPredicate(t, "cloud <olgular> for \"Ayşe Kaya\"", walkCloud(t, g, "Ayşe Kaya nerede", nil), "had_coffee_with", "is")
			}

			// The local lane (the user's own surface) keeps what the secret
			// note says.
			local, err := g.LookupEntities(context.Background(), "tr-local", "doktor", 0, false)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range local {
				if r.CanonicalName == "Ayşe Kaya" && strings.Contains(strings.Join(r.Aliases, "|"), "Doktor") {
					found = true
				}
			}
			if !found {
				t.Fatalf("local entity_lookup(doktor) = %+v, want the secret note's Ayşe Kaya / Doktor entity", local)
			}
		})
	}
}

// TestCloudLaneIgnoresALocalLinkOnASharedFactRow (FIX-MEM-1/MEM-4 review):
// the same triple stated by a secret-content note (extracted locally, which
// links "Doktor" to Ayşe Kaya through the alias its own answer gives) and
// later by an ordinary note shares one fact row; the ordinary citation
// makes that row cloud-eligible, but its subject link was written by the
// LOCAL call. A cloud-lane read never follows that link: the neighbour
// note's walk never reaches Ayşe Kaya's facts, entity_lookup never pairs
// the names, and fact_query reports no shared entity id.
func TestCloudLaneIgnoresALocalLinkOnASharedFactRow(t *testing.T) {
	const marker = "ozel-isaret-77" // only the local classifier calls it secret
	notes := []laneLinkNote{
		{path: "people/ayse.md", text: "Ayşe Kaya Acme'de çalışıyor.", answer: func(c int64) reader.FactExtractV1 {
			return reader.FactExtractV1{
				Entities: []reader.ExtractEntityV1{ent("Ayşe Kaya", factengine.EntityKindPerson), ent("Acme", factengine.EntityKindOrg)},
				Facts:    []reader.ExtractFactV1{triple(c, "Ayşe Kaya", "works_at", "Acme")},
			}
		}},
		{path: "inbox/saglik.md", text: "Doktorum Ayşe Kaya Kadıköy'de oturuyor, " + marker + ".", answer: func(c int64) reader.FactExtractV1 {
			return reader.FactExtractV1{
				Entities: []reader.ExtractEntityV1{ent("Ayşe Kaya", factengine.EntityKindPerson, "Doktor"), ent("Kadıköy", factengine.EntityKindPlace)},
				Facts:    []reader.ExtractFactV1{triple(c, "Doktor", "lives_in", "Kadıköy")},
			}
		}},
		{path: "inbox/komsu.md", text: "Doktor Kadıköy'de oturuyor.", answer: func(c int64) reader.FactExtractV1 {
			return reader.FactExtractV1{
				Entities: []reader.ExtractEntityV1{ent("Doktor", factengine.EntityKindPerson), ent("Kadıköy", factengine.EntityKindPlace)},
				Facts:    []reader.ExtractFactV1{triple(c, "Doktor", "lives_in", "Kadıköy")},
			}
		}},
	}
	f, g, eps := runLaneLinkNotes(t, marker, factengine.ProvenanceExternalDoc, notes)
	if f.reader.call(0).forceSecret || !f.reader.call(1).forceSecret || f.reader.call(2).forceSecret {
		t.Fatal("scenario: only the secret-content note may run locally")
	}
	if n := f.count(`SELECT count(*) FROM facts WHERE predicate = 'lives_in'`); n != 1 {
		t.Fatalf("lives_in rows = %d, want the one shared row", n)
	}

	assertCloudLookupNeverPairs(t, g, "doktor", "Ayşe Kaya")
	assertCloudLookupNeverPairs(t, g, "ayşe", "Doktor")
	assertDisjoint(t, cloudFactEntities(t, g, "lives_in"), cloudFactEntities(t, g, "works_at"), "lives_in vs works_at")
	assertNoPredicate(t, "cloud <olgular> for the neighbour note", walkCloud(t, g, "komşu ne dedi", []int64{eps["inbox/komsu.md"]}), "works_at")
	assertNoPredicate(t, "cloud <olgular> for \"Ayşe Kaya\"", walkCloud(t, g, "Ayşe Kaya nerede", nil), "lives_in")
}

// TestLocalLaneKeepsResolvingASplitNameToItsOwnEntity (FIX-MEM-1/MEM-4
// review): once the cloud lane has its own twin of a name only a secret
// note gave an entity, later secret notes still resolve the name to the
// secret note's entity - lane-scoped identity never turns every local
// sighting into another provisional namesake.
func TestLocalLaneKeepsResolvingASplitNameToItsOwnEntity(t *testing.T) {
	secretAnswer := func(c int64) reader.FactExtractV1 {
		return reader.FactExtractV1{
			Entities: []reader.ExtractEntityV1{ent("Ayşe Kaya", factengine.EntityKindPerson, "Doktor")},
			Facts:    []reader.ExtractFactV1{triple(c, "Doktor", "prescribes", "ilaç")},
		}
	}
	notes := []laneLinkNote{
		{path: "finans-doktor.md", text: "Doktorum Ayşe Kaya.", answer: secretAnswer},
		{path: "people/ayse.md", text: "Ayşe Kaya ile Doktor kahve içti.", answer: func(c int64) reader.FactExtractV1 {
			return reader.FactExtractV1{
				Entities: []reader.ExtractEntityV1{ent("Ayşe Kaya", factengine.EntityKindPerson), ent("Doktor", factengine.EntityKindPerson)},
				Facts:    []reader.ExtractFactV1{triple(c, "Ayşe Kaya", "had_coffee_with", "Doktor")},
			}
		}},
		{path: "finans-doktor-2.md", text: "Doktorum Ayşe Kaya yine aradı.", answer: secretAnswer},
		{path: "finans-doktor-3.md", text: "Doktorum Ayşe Kaya üçüncü kez.", answer: secretAnswer},
	}
	f, _, _ := runLaneLinkNotes(t, "", factengine.ProvenanceExternalDoc, notes)
	if n := f.count(`SELECT count(*) FROM entities WHERE provisional = 1`); n != 0 {
		t.Fatalf("provisional entities = %d, want 0 - a split name is no namesake", n)
	}
	if n := f.count(`SELECT count(*) FROM entities`); n != 3 {
		t.Fatalf("entities = %d, want 3 (the secret note's Ayşe Kaya, the cloud lane's Ayşe Kaya and Doktor)", n)
	}
	if n := f.count(`SELECT count(DISTINCT subject_entity_id) FROM facts WHERE predicate = 'prescribes'`); n != 1 {
		t.Fatalf("prescribes subject entities = %d, want the one secret-note entity", n)
	}
}
