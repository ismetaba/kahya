package factengine

import (
	"context"
	"testing"

	"kahya/kahyad/internal/store"
)

func resolveIn(t *testing.T, e *Engine, name, kind string, cloudLane bool) (int64, bool, bool) {
	t.Helper()
	id, created, provisional, err := e.ResolveEntityForExtraction(context.Background(), "trace-lane", name, kind, cloudLane)
	if err != nil {
		t.Fatalf("ResolveEntityForExtraction(%q, cloud=%v): %v", name, cloudLane, err)
	}
	return id, created, provisional
}

// aliasLanes returns entityID's aliases mapped to their cloud_lane flag.
func aliasLanes(t *testing.T, st *store.Store, entityID int64) map[string]int64 {
	t.Helper()
	rows, err := st.Queries.ListEntityAliasesByEntity(context.Background(), entityID)
	if err != nil {
		t.Fatalf("ListEntityAliasesByEntity(%d): %v", entityID, err)
	}
	out := map[string]int64{}
	for _, a := range rows {
		out[a.Alias] = a.CloudLane
	}
	return out
}

// TestResolveEntityForExtractionCloudLaneIgnoresLocalOnlyAliases (FIX-MEM-1/
// MEM-4 review, migration 0025): a secret note extracted locally gives Ayşe
// Kaya the alias "Doktor". An ordinary note's "Doktor" - and even its "Ayşe
// Kaya" - resolved on the cloud lane never lands on that entity: the cloud
// view holds no alias a local-lane call added, so the cloud call creates
// its own non-provisional entities (nothing a cloud model sees says the
// name is taken). The local lane still sees every alias and prefers the
// entities a local-lane call named: a later local "Doktor" or "Ayşe Kaya"
// is the secret note's entity again (no provisional twin per sighting),
// and a name only a cloud-lane call gave resolves to that cloud entity.
func TestResolveEntityForExtractionCloudLaneIgnoresLocalOnlyAliases(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	secretAyse, created, _ := resolveIn(t, e, "Ayşe Kaya", EntityKindPerson, false)
	if !created {
		t.Fatal("first local sighting must create the entity")
	}
	if added, err := e.AddAlias(ctx, "trace-lane", secretAyse, "Doktor", false); err != nil || !added {
		t.Fatalf("local AddAlias(Doktor) = %v, %v", added, err)
	}
	if lanes := aliasLanes(t, st, secretAyse); len(lanes) != 2 || lanes["Ayşe Kaya"] != 0 || lanes["Doktor"] != 0 {
		t.Fatalf("local-lane aliases = %v, want both local-only (cloud_lane 0)", lanes)
	}

	doktor, created, provisional := resolveIn(t, e, "Doktor", EntityKindPerson, true)
	if doktor == secretAyse || !created || provisional {
		t.Fatalf("cloud Doktor = (%d, created %v, provisional %v), want a NEW non-provisional entity, never the secret note's %d", doktor, created, provisional, secretAyse)
	}
	cloudAyse, created, provisional := resolveIn(t, e, "Ayşe Kaya", EntityKindPerson, true)
	if cloudAyse == secretAyse || cloudAyse == doktor || !created || provisional {
		t.Fatalf("cloud Ayşe Kaya = (%d, created %v, provisional %v), want a NEW non-provisional entity", cloudAyse, created, provisional)
	}
	if lanes := aliasLanes(t, st, doktor); lanes["Doktor"] != 1 {
		t.Fatalf("cloud-lane entity aliases = %v, want its first alias in the cloud view", lanes)
	}
	// The cloud view reuses its own entities.
	if again, created, _ := resolveIn(t, e, "doktor", EntityKindPerson, true); again != doktor || created {
		t.Fatalf("second cloud Doktor = (%d, created %v), want reuse of %d", again, created, doktor)
	}
	// The local view prefers the names a local-lane call gave: the secret
	// note's entity again, never a provisional twin per sighting.
	for _, name := range []string{"Doktor", "AYŞE KAYA"} {
		if got, created, _ := resolveIn(t, e, name, EntityKindPerson, false); got != secretAyse || created {
			t.Fatalf("local %s = (%d, created %v), want the secret note's entity %d", name, got, created, secretAyse)
		}
	}
	// A name only a cloud-lane call gave resolves to that entity locally.
	acme, _, _ := resolveIn(t, e, "Acme", EntityKindOrg, true)
	if got, created, _ := resolveIn(t, e, "acme", EntityKindOrg, false); got != acme || created {
		t.Fatalf("local acme = (%d, created %v), want the cloud entity %d", got, created, acme)
	}
	if n := countEntities(t, st); n != 4 {
		t.Fatalf("entities = %d, want 4 (secret Ayşe Kaya, cloud Doktor, cloud Ayşe Kaya, Acme)", n)
	}
	rows := eventPayloads(t, st, EventEntityCreated)
	if len(rows) != 4 || rows[1]["reason"] != EntityCreatedFirstSighting || rows[1]["cloud_lane"] != true || rows[0]["cloud_lane"] != false {
		t.Fatalf("entity_created = %v, want the lane on every row and first_sighting for the cloud Doktor", rows)
	}
}

// TestAddAliasCloudLaneUsesTheCloudView (FIX-MEM-1/MEM-4 review): a
// cloud-lane AddAlias checks collisions against cloud-view aliases only (a
// local-only owner never refuses it - the refusal would itself reveal the
// secret alias), refuses one another entity holds in the cloud view, and
// moves the entity's own local-only row into the cloud view when the cloud
// bytes state it too. A local-lane AddAlias keeps checking every alias.
func TestAddAliasCloudLaneUsesTheCloudView(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	secret, _, _ := resolveIn(t, e, "Ayşe Kaya", EntityKindPerson, false)
	if _, err := e.AddAlias(ctx, "trace-lane", secret, "Doktor", false); err != nil {
		t.Fatal(err)
	}
	emre, _, _ := resolveIn(t, e, "Emre", EntityKindPerson, true)
	acme, _, _ := resolveIn(t, e, "Acme", EntityKindOrg, true)

	// A local-only owner does not refuse a cloud-lane alias.
	if added, err := e.AddAlias(ctx, "trace-lane", emre, "Doktor", true); err != nil || !added {
		t.Fatalf("cloud AddAlias(emre, Doktor) = %v, %v; want added (the secret note's alias is not in the cloud view)", added, err)
	}
	if again, _, _ := resolveIn(t, e, "Doktor", EntityKindPerson, true); again != emre {
		t.Fatalf("cloud Doktor resolves to %d, want %d", again, emre)
	}
	// A cloud-view owner does.
	if added, err := e.AddAlias(ctx, "trace-lane", acme, "doktor", true); err != nil || added {
		t.Fatalf("cloud AddAlias(acme, doktor) = %v, %v; want refused (emre holds it in the cloud view)", added, err)
	}
	// A local-lane AddAlias sees every alias.
	if added, err := e.AddAlias(ctx, "trace-lane", acme, "DOKTOR", false); err != nil || added {
		t.Fatalf("local AddAlias(acme, DOKTOR) = %v, %v; want refused", added, err)
	}

	// The entity's own local-only row joins the cloud view when a cloud call
	// states it.
	if _, err := e.AddAlias(ctx, "trace-lane", emre, "Emre Bey", false); err != nil {
		t.Fatal(err)
	}
	if lanes := aliasLanes(t, st, emre); lanes["Emre Bey"] != 0 {
		t.Fatalf("local alias lanes = %v, want Emre Bey local-only", lanes)
	}
	if added, err := e.AddAlias(ctx, "trace-lane", emre, "Emre Bey", true); err != nil || added {
		t.Fatalf("cloud AddAlias(emre, Emre Bey) = %v, %v; want not added (the row exists)", added, err)
	}
	if lanes := aliasLanes(t, st, emre); lanes["Emre Bey"] != 1 || len(lanes) != 3 {
		t.Fatalf("alias lanes = %v, want Emre Bey moved into the cloud view (no second row)", lanes)
	}
	// ...unless another entity holds it in the cloud view.
	if _, err := e.AddAlias(ctx, "trace-lane", acme, "Acme Ltd", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddAlias(ctx, "trace-lane", emre, "Acme Ltd", true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddAlias(ctx, "trace-lane", acme, "Acme Ltd", true); err != nil {
		t.Fatal(err)
	}
	if lanes := aliasLanes(t, st, acme); lanes["Acme Ltd"] != 0 {
		t.Fatalf("acme alias lanes = %v, want Acme Ltd kept local-only (emre holds it in the cloud view)", lanes)
	}
	// The secret note's entity is untouched throughout.
	if lanes := aliasLanes(t, st, secret); lanes["Doktor"] != 0 || lanes["Ayşe Kaya"] != 0 {
		t.Fatalf("secret entity aliases = %v, want local-only", lanes)
	}
}

// TestWriteFactLinkLaneFollowsTheLinkWriter (FIX-MEM-1/MEM-4 review,
// migration 0025): a side's link-lane flag is 1 only when a cloud-lane
// candidate (LinksCloudLane) wrote that link, and it is written together
// with the link: a link a local-lane call wrote first stays local-only when
// a cloud-lane call re-asserts the triple (the first link wins), while the
// side the local call left unlinked takes the cloud call's link and flag.
func TestWriteFactLinkLaneFollowsTheLinkWriter(t *testing.T) {
	e, st := newTestEngine(t)
	ctx := context.Background()
	ayse, _, _ := resolveIn(t, e, "Ayşe Kaya", EntityKindPerson, false)
	doktor, _, _ := resolveIn(t, e, "Doktor", EntityKindPerson, true)
	kadikoy, _, _ := resolveIn(t, e, "Kadıköy", EntityKindPlace, true)

	local := Candidate{Subject: "Doktor", Predicate: "lives_in", Object: "Kadıköy", Provenance: ProvenanceExternalDoc,
		SessionID: "s-local", SubjectEntityID: ayse, ExtractorVer: "test_v1"}
	id, err := e.WriteFact(ctx, "trace-lane", local)
	if err != nil {
		t.Fatal(err)
	}
	f, err := st.Queries.GetFact(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.SubjectEntityID.Int64 != ayse || f.SubjectLinkCloud != 0 || f.ObjectEntityID.Valid || f.ObjectLinkCloud != 0 {
		t.Fatalf("local insert = %+v, want a local-only subject link and no object link", f)
	}

	cloud := Candidate{Subject: "Doktor", Predicate: "lives_in", Object: "Kadıköy", Provenance: ProvenanceExternalDoc,
		SessionID: "s-cloud", SubjectEntityID: doktor, ObjectEntityID: kadikoy, ExtractorVer: "test_v1", LinksCloudLane: true}
	if got, err := e.WriteFact(ctx, "trace-lane", cloud); err != nil || got != id {
		t.Fatalf("cloud re-assertion = %d, %v; want the same row %d", got, err, id)
	}
	f, err = st.Queries.GetFact(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.SubjectEntityID.Int64 != ayse || f.SubjectLinkCloud != 0 {
		t.Fatalf("subject after the cloud re-assertion = %d (cloud flag %d), want the local link kept local-only", f.SubjectEntityID.Int64, f.SubjectLinkCloud)
	}
	if f.ObjectEntityID.Int64 != kadikoy || f.ObjectLinkCloud != 1 {
		t.Fatalf("object after the cloud re-assertion = %d (cloud flag %d), want the cloud call's link, cloud-lane", f.ObjectEntityID.Int64, f.ObjectLinkCloud)
	}

	// A cloud-lane insert flags exactly the sides it links.
	half, err := e.WriteFact(ctx, "trace-lane", Candidate{Subject: "Doktor", Predicate: "drinks", Object: "kahve", Provenance: ProvenanceExternalDoc,
		SessionID: "s-cloud", SubjectEntityID: doktor, ExtractorVer: "test_v1", LinksCloudLane: true})
	if err != nil {
		t.Fatal(err)
	}
	if f, err = st.Queries.GetFact(ctx, half); err != nil || f.SubjectLinkCloud != 1 || f.ObjectLinkCloud != 0 {
		t.Fatalf("cloud insert = %+v, %v; want only the linked side flagged", f, err)
	}
}
