package factengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

func countEntities(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM entities`).Scan(&n); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	return n
}

// eventPayloads returns the decoded payloads of every events row of kind, in
// insertion order.
func eventPayloads(t *testing.T, st *store.Store, kind string) []map[string]any {
	t.Helper()
	rows, err := st.DB().Query(`SELECT payload FROM events WHERE kind = ? ORDER BY id`, kind)
	if err != nil {
		t.Fatalf("query %s events: %v", kind, err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("decode %s payload %q: %v", kind, raw, err)
		}
		out = append(out, p)
	}
	return out
}

func resolve(t *testing.T, e *Engine, name, kind string) (int64, bool, bool) {
	t.Helper()
	id, created, provisional, err := e.ResolveEntityForExtraction(context.Background(), "trace-resolve", name, kind, false)
	if err != nil {
		t.Fatalf("ResolveEntityForExtraction(%q, %q): %v", name, kind, err)
	}
	return id, created, provisional
}

// TestResolveEntityForExtractionReusesUnambiguousExactAlias (FIX-MEM-2 phase
// 0, decision MEM-2.a): the extractor seeing the same name again reuses the
// one non-provisional entity carrying it - identity resolution, not a merge.
// 100 sightings leave ONE row (ResolveOrCreateEntity would mint 100).
func TestResolveEntityForExtractionReusesUnambiguousExactAlias(t *testing.T) {
	e, st := newTestEngine(t)
	first, created, provisional := resolve(t, e, "Emre", "person")
	if !created || provisional {
		t.Fatalf("first sighting: created=%v provisional=%v, want true/false", created, provisional)
	}
	for i := 0; i < 100; i++ {
		id, created, provisional := resolve(t, e, "Emre", "person")
		if id != first || created || provisional {
			t.Fatalf("sighting %d = (%d, created %v, provisional %v), want (%d, false, false)", i+2, id, created, provisional, first)
		}
	}
	if n := countEntities(t, st); n != 1 {
		t.Fatalf("entities = %d after 101 sightings, want 1", n)
	}
	created1 := eventPayloads(t, st, EventEntityCreated)
	if len(created1) != 1 || created1[0]["reason"] != EntityCreatedFirstSighting || created1[0]["provisional"] != false {
		t.Fatalf("entity_created rows = %v, want one first_sighting", created1)
	}
	if got := countMergeLedgerRows(t, st); got != 0 {
		t.Fatalf("merge_ledger rows = %d, want 0 (reuse is not a merge)", got)
	}
}

// TestResolveEntityForExtractionFoldedAliasMatchesTurkishCase (FIX-MEM-2
// phase 0): the lookup key is Turkish I/i-folded and drops a suffix after an
// apostrophe - "EMRE", "emre", "Emre'nin" and "Emre’nin" are one entity; the
// stored alias keeps the surface form the extractor gave first.
func TestResolveEntityForExtractionFoldedAliasMatchesTurkishCase(t *testing.T) {
	e, st := newTestEngine(t)
	id, _, _ := resolve(t, e, "EMRE", "person")
	for _, name := range []string{"emre", "Emre'nin", "Emre’nin", "  Emre  "} {
		if got, created, prov := resolve(t, e, name, "person"); got != id || created || prov {
			t.Fatalf("%q resolved to (%d, %v, %v), want (%d, false, false)", name, got, created, prov, id)
		}
	}
	ismet, _, _ := resolve(t, e, "İSMET", "")
	for _, name := range []string{"ismet", "Ismet", "ısmet"} {
		if got, _, _ := resolve(t, e, name, ""); got != ismet {
			t.Fatalf("%q resolved to %d, want İSMET's %d (Turkish I/i folding)", name, got, ismet)
		}
	}
	aliases, err := st.Queries.ListEntityAliasesByEntity(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 1 || aliases[0].Alias != "EMRE" || aliases[0].AliasFolded.String != "emre" {
		t.Fatalf("aliases = %+v, want the surface form EMRE with key emre", aliases)
	}
	ent, err := st.Queries.GetEntity(context.Background(), id)
	if err != nil || ent.CanonicalName != "EMRE" {
		t.Fatalf("canonical_name = %q, %v; want the surface form", ent.CanonicalName, err)
	}
}

// TestResolveEntityForExtractionTwoMatchesCreatesProvisional (FIX-MEM-2
// phase 0, HANDOFF §5 memory #2): with two unmerged "Emre" namesakes on
// file, a third sighting can not pick one - it becomes a NEW provisional
// entity (reason ambiguous_alias); nothing is joined to anything.
func TestResolveEntityForExtractionTwoMatchesCreatesProvisional(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	a, _, err := e.ResolveOrCreateEntity(ctx, "Emre", "person")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := e.ResolveOrCreateEntity(ctx, "Emre", "person")
	if err != nil {
		t.Fatal(err)
	}
	id, created, provisional := resolve(t, e, "Emre", "person")
	if id == a || id == b || !created || !provisional {
		t.Fatalf("third Emre = (%d, created %v, provisional %v), want a new provisional entity (not %d/%d)", id, created, provisional, a, b)
	}
	rows := eventPayloads(t, st, EventEntityCreated)
	if len(rows) != 1 || rows[0]["reason"] != EntityCreatedAmbiguousAlias || rows[0]["provisional"] != true {
		t.Fatalf("entity_created = %v, want one ambiguous_alias row", rows)
	}
	if got := countMergeLedgerRows(t, st); got != 0 {
		t.Fatalf("merge_ledger rows = %d, want 0", got)
	}
}

// TestResolveEntityForExtractionProvisionalMatchNeverReused (FIX-MEM-2 phase
// 0): a single matching entity that is itself provisional (a suspected
// namesake) is never reused - another provisional entity is created
// (reason provisional_collision).
func TestResolveEntityForExtractionProvisionalMatchNeverReused(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	ent, err := st.Queries.InsertEntity(ctx, sqlcgen.InsertEntityParams{
		CanonicalName: "Ayşe", Kind: sql.NullString{String: "person", Valid: true}, Status: "active", Provisional: 1, CreatedAt: "2026-10-15T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Queries.InsertEntityAlias(ctx, sqlcgen.InsertEntityAliasParams{
		EntityID: ent.ID, Alias: "Ayşe", AliasFolded: sql.NullString{String: AliasKey("Ayşe"), Valid: true}, CreatedAt: "2026-10-15T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	id, created, provisional := resolve(t, e, "Ayşe", "person")
	if id == ent.ID || !created || !provisional {
		t.Fatalf("Ayşe = (%d, %v, %v), want a new provisional entity, never the provisional %d", id, created, provisional, ent.ID)
	}
	if rows := eventPayloads(t, st, EventEntityCreated); len(rows) != 1 || rows[0]["reason"] != EntityCreatedProvisionalCollision {
		t.Fatalf("entity_created = %v, want provisional_collision", rows)
	}
}

// TestResolveEntityForExtractionKindMismatchCreatesProvisional (FIX-MEM-2
// phase 0): "Mercedes" the person and "Mercedes" the company are not
// resolved into one another; an empty kind on either side is compatible.
func TestResolveEntityForExtractionKindMismatchCreatesProvisional(t *testing.T) {
	e, st := newTestEngine(t)
	person, _, _ := resolve(t, e, "Mercedes", "person")
	org, created, provisional := resolve(t, e, "Mercedes", "org")
	if org == person || !created || !provisional {
		t.Fatalf("Mercedes/org = (%d, %v, %v), want a new provisional entity", org, created, provisional)
	}
	rows := eventPayloads(t, st, EventEntityCreated)
	if len(rows) != 2 || rows[1]["reason"] != EntityCreatedKindMismatch || rows[1]["kind"] != "org" {
		t.Fatalf("entity_created = %v, want first_sighting then kind_mismatch", rows)
	}

	// Empty kind is compatible: a kindless sighting of a unique name reuses it.
	ankara, _, _ := resolve(t, e, "Ankara", "place")
	if got, created, _ := resolve(t, e, "Ankara", ""); got != ankara || created {
		t.Fatalf("kindless Ankara = (%d, %v), want reuse of %d", got, created, ankara)
	}
	// A kind outside the vocabulary folds to "other" (and is compatible
	// only with other/empty).
	if got := NormalizeEntityKind("  Person "); got != "person" {
		t.Fatalf("NormalizeEntityKind(Person) = %q", got)
	}
	if got := NormalizeEntityKind("galaxy"); got != EntityKindOther {
		t.Fatalf("NormalizeEntityKind(galaxy) = %q, want other", got)
	}
}

// TestAddAliasSkipsAliasOwnedByAnotherEntity (FIX-MEM-2 phase 0): an alias is
// added only to its own entity; a name another active entity already answers
// to is skipped and ledgered (factengine.alias_skipped_collision), never moved.
func TestAddAliasSkipsAliasOwnedByAnotherEntity(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	emre, _, _ := resolve(t, e, "Emre Yılmaz", "person")
	ahmet, _, _ := resolve(t, e, "Ahmet", "person")

	added, err := e.AddAlias(ctx, "trace-alias", emre, "Emre", false)
	if err != nil || !added {
		t.Fatalf("AddAlias(Emre) = %v, %v; want added", added, err)
	}
	if added, err := e.AddAlias(ctx, "trace-alias", emre, "Emre", false); err != nil || added {
		t.Fatalf("repeat AddAlias = %v, %v; want not added (already there)", added, err)
	}
	if got, created, _ := resolve(t, e, "emre", "person"); got != emre || created {
		t.Fatalf("emre resolves to %d (created %v), want %d through the new alias", got, created, emre)
	}

	added, err = e.AddAlias(ctx, "trace-alias", ahmet, "EMRE", false)
	if err != nil || added {
		t.Fatalf("AddAlias(ahmet, EMRE) = %v, %v; want skipped", added, err)
	}
	rows := eventPayloads(t, st, EventAliasSkippedCollision)
	if len(rows) != 1 || rows[0]["alias"] != "EMRE" || rows[0]["entity_id"] != float64(ahmet) || rows[0]["owner_entity_id"] != float64(emre) {
		t.Fatalf("alias_skipped_collision = %v", rows)
	}
	aliases, err := st.Queries.ListEntityAliasesByEntity(ctx, ahmet)
	if err != nil || len(aliases) != 1 {
		t.Fatalf("Ahmet aliases = %+v, %v; want only his own", aliases, err)
	}

	if _, err := e.AddAlias(ctx, "trace-alias", 9999, "x", false); err == nil {
		t.Fatal("AddAlias on an unknown entity must fail")
	}
	if _, err := e.AddAlias(ctx, "trace-alias", emre, "  ", false); err == nil {
		t.Fatal("AddAlias with an empty alias must fail")
	}
}

// TestResolveOrCreateEntityWritesFoldedAlias (FIX-MEM-2 phase 0): the
// existing resolver now writes alias_folded too, so the folded lookup sees
// every entity, whichever path created it.
func TestResolveOrCreateEntityWritesFoldedAlias(t *testing.T) {
	e, st := newTestEngine(t)
	id, _, err := e.ResolveOrCreateEntity(context.Background(), "İstanbul Ofisi", "place")
	if err != nil {
		t.Fatal(err)
	}
	aliases, err := st.Queries.ListEntityAliasesByEntity(context.Background(), id)
	if err != nil || len(aliases) != 1 || aliases[0].AliasFolded.String != "istanbul ofisi" {
		t.Fatalf("aliases = %+v, %v; want alias_folded 'istanbul ofisi'", aliases, err)
	}
}

func TestAliasKey(t *testing.T) {
	for in, want := range map[string]string{
		"Emre":         "emre",
		"EMRE":         "emre",
		"Emre'nin":     "emre",
		"Emre’nin":     "emre",
		" Emre'ye ":    "emre",
		"İSMET":        "ismet",
		"Işık":         "işik",
		"'Tırnak":      "'tirnak",
		"Kahya Ofisi":  "kahya ofisi",
		"":             "",
		"Ankara’da":    "ankara",
		"ŞEBNEM":       "şebnem",
		"Çağrı'yla":    "çağri",
		"Gökçe   Arda": "gökçe   arda",
	} {
		if got := AliasKey(in); got != want {
			t.Errorf("AliasKey(%q) = %q, want %q", in, got, want)
		}
	}
}
