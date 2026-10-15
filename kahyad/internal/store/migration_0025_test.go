package store

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/pressly/goose/v3"

	"kahya/kahyad/internal/store/sqlcgen"
)

var migration0025Columns = map[string][]string{
	"entity_aliases": {"cloud_lane"},
	"facts":          {"subject_link_cloud", "object_link_cloud"},
}

// TestMigration0025DefaultsEveryPreExistingRowToLocalOnly (FIX-MEM-1/MEM-4
// review): 0025 adds the lane columns of lane-scoped entity identity. A
// pre-0025 alias and a pre-0025 entity link have an unknown origin, so they
// are local-only (0) - fail-closed: no cloud-lane read follows them.
func TestMigration0025DefaultsEveryPreExistingRowToLocalOnly(t *testing.T) {
	cfg := testCfg(t)
	rawDB := openRawAt(t, cfg.DBPath, 24)
	for _, stmt := range []string{
		`INSERT INTO entities (id, canonical_name, kind, status, provisional, created_at) VALUES (1, 'Emre', 'person', 'active', 0, '2026-01-01T00:00:00Z')`,
		`INSERT INTO entity_aliases (entity_id, alias, created_at, alias_folded) VALUES (1, 'Emre', '2026-01-01T00:00:00Z', 'emre')`,
		`INSERT INTO facts (subject, predicate, object, source_tier, evidentiality, confidence, importance, status, updated_at, created_at, subject_entity_id, object_entity_id) VALUES ('Emre', 'sever', 'Emre', 'agent_derived', 'inferred', 0, 0, 'active', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 1, 1)`,
	} {
		if _, err := rawDB.Exec(stmt); err != nil {
			t.Fatalf("seed pre-0025 row: %v\n%s", err, stmt)
		}
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	for table, cols := range migration0025Columns {
		got := columnNames(t, s.DB(), table)
		for _, c := range cols {
			if !got[c] {
				t.Errorf("%s.%s missing after 0025", table, c)
			}
		}
	}
	if !indexNames(t, s.DB())["idx_entity_aliases_folded_cloud"] {
		t.Error("index idx_entity_aliases_folded_cloud missing after 0025")
	}
	var aliasLane, subjLane, objLane int64
	if err := s.DB().QueryRow(`SELECT cloud_lane FROM entity_aliases`).Scan(&aliasLane); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT subject_link_cloud, object_link_cloud FROM facts`).Scan(&subjLane, &objLane); err != nil {
		t.Fatal(err)
	}
	if aliasLane != 0 || subjLane != 0 || objLane != 0 {
		t.Fatalf("pre-0025 lanes = alias %d, links %d/%d; want all 0 (local-only)", aliasLane, subjLane, objLane)
	}
	if ids, err := s.Queries.ListActiveCloudEntityIDsByAliasFolded(context.Background(), sql.NullString{String: "emre", Valid: true}); err != nil || len(ids) != 0 {
		t.Fatalf("cloud view after 0025 = %v, %v; want no pre-0025 alias in it", ids, err)
	}
}

// TestMigration0025DownRestoresPreviousSchema: 0025's Down section removes
// exactly its columns and index.
func TestMigration0025DownRestoresPreviousSchema(t *testing.T) {
	cfg := testCfg(t)
	rawDB := openRawAt(t, cfg.DBPath, 25)
	defer rawDB.Close()
	if err := goose.DownTo(rawDB, ".", 24); err != nil {
		t.Fatalf("goose.DownTo(24): %v", err)
	}
	for table, cols := range migration0025Columns {
		got := columnNames(t, rawDB, table)
		for _, c := range cols {
			if got[c] {
				t.Errorf("%s.%s still present after 0025 Down", table, c)
			}
		}
	}
	if indexNames(t, rawDB)["idx_entity_aliases_folded_cloud"] {
		t.Error("index idx_entity_aliases_folded_cloud still present after 0025 Down")
	}
}

// TestLinkLaneQueriesBindEveryParameter: every 0025 query runs against the
// real schema with the arguments its generated method binds (sqlc v1.30.0
// can silently drop a parameter it cannot parse) and returns what its
// comment promises.
func TestLinkLaneQueriesBindEveryParameter(t *testing.T) {
	ctx := context.Background()
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	q := s.Queries
	const now = "2026-10-15T00:00:00Z"
	ns := func(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }
	ni := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }
	entity := func(name string, aliasLane int64) int64 {
		t.Helper()
		e, err := q.InsertEntity(ctx, sqlcgen.InsertEntityParams{CanonicalName: name, Kind: ns("person"), Status: "active", CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		a, err := q.InsertEntityAlias(ctx, sqlcgen.InsertEntityAliasParams{EntityID: e.ID, Alias: name, AliasFolded: ns("doktor"), CreatedAt: now, CloudLane: aliasLane})
		if err != nil || a.CloudLane != aliasLane {
			t.Fatalf("InsertEntityAlias RETURNING = %+v, %v; want cloud_lane %d", a, err, aliasLane)
		}
		return e.ID
	}
	local := entity("Doktor", 0)
	cloud := entity("doktor", 1)

	if ids, err := q.ListActiveCloudEntityIDsByAliasFolded(ctx, ns("doktor")); err != nil || !reflect.DeepEqual(ids, []int64{cloud}) {
		t.Fatalf("ListActiveCloudEntityIDsByAliasFolded = %v, %v; want only %d", ids, err, cloud)
	}
	if ids, err := q.ListActiveEntityIDsByAliasFolded(ctx, ns("doktor")); err != nil || len(ids) != 2 {
		t.Fatalf("ListActiveEntityIDsByAliasFolded = %v, %v; want both (the full view)", ids, err)
	}
	if ids, err := q.ListActiveLocalEntityIDsByAliasFolded(ctx, ns("doktor")); err != nil || !reflect.DeepEqual(ids, []int64{local}) {
		t.Fatalf("ListActiveLocalEntityIDsByAliasFolded = %v, %v; want only %d", ids, err, local)
	}
	if ids, err := q.ListCloudEntityIDsByAlias(ctx, "Doktor"); err != nil || len(ids) != 0 {
		t.Fatalf("ListCloudEntityIDsByAlias(Doktor) = %v, %v; want none (local-only row)", ids, err)
	}
	if rows, err := q.ListCloudEntitiesByFolded(ctx, sqlcgen.ListCloudEntitiesByFoldedParams{AliasFolded: ns("dok%"), Limit: 10}); err != nil || len(rows) != 1 || rows[0].ID != cloud {
		t.Fatalf("ListCloudEntitiesByFolded = %+v, %v; want only %d", rows, err, cloud)
	}
	alias, err := q.GetEntityAliasByEntityAndAlias(ctx, sqlcgen.GetEntityAliasByEntityAndAliasParams{EntityID: local, Alias: "Doktor"})
	if err != nil || alias.CloudLane != 0 {
		t.Fatalf("GetEntityAliasByEntityAndAlias = %+v, %v", alias, err)
	}
	if err := q.MarkEntityAliasCloud(ctx, alias.ID); err != nil {
		t.Fatal(err)
	}
	if ids, err := q.ListCloudEntityIDsByAlias(ctx, "Doktor"); err != nil || !reflect.DeepEqual(ids, []int64{local}) {
		t.Fatalf("after MarkEntityAliasCloud = %v, %v; want [%d]", ids, err, local)
	}
	if rows, err := q.ListEntityAliasesByEntity(ctx, local); err != nil || len(rows) != 1 || rows[0].CloudLane != 1 {
		t.Fatalf("ListEntityAliasesByEntity = %+v, %v", rows, err)
	}

	f, err := q.InsertFact(ctx, sqlcgen.InsertFactParams{
		Subject: "Doktor", Predicate: "lives_in", Object: "Kadıköy", SourceTier: "agent_derived", Evidentiality: "inferred",
		Status: "active", UpdatedAt: now, CreatedAt: now, SubjectEntityID: ni(local), SubjectLinkCloud: 0,
	})
	if err != nil || f.SubjectLinkCloud != 0 || f.ObjectLinkCloud != 0 {
		t.Fatalf("InsertFact RETURNING = %+v, %v", f, err)
	}
	// The flags fill only while their side is unlinked (old-row semantics).
	if err := q.UpdateFactLinksAndValidity(ctx, sqlcgen.UpdateFactLinksAndValidityParams{
		SubjectLinkCloud: 1, ObjectLinkCloud: 1, SubjectEntityID: ni(cloud), ObjectEntityID: ni(cloud), UpdatedAt: now, ID: f.ID,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := q.GetFact(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubjectEntityID.Int64 != local || got.SubjectLinkCloud != 0 || got.ObjectEntityID.Int64 != cloud || got.ObjectLinkCloud != 1 {
		t.Fatalf("after UpdateFactLinksAndValidity = %+v; want the subject's local link kept local-only, the object filled cloud-lane", got)
	}
	if hop, err := q.ListFactsByEntityCloudLinks(ctx, sqlcgen.ListFactsByEntityCloudLinksParams{SubjectEntityID: ni(local), ObjectEntityID: ni(local), Limit: 10}); err != nil || len(hop) != 0 {
		t.Fatalf("ListFactsByEntityCloudLinks(local) = %+v, %v; want none (a local-lane link)", hop, err)
	}
	if hop, err := q.ListFactsByEntityCloudLinks(ctx, sqlcgen.ListFactsByEntityCloudLinksParams{SubjectEntityID: ni(cloud), ObjectEntityID: ni(cloud), Limit: 10}); err != nil || len(hop) != 1 || hop[0].ObjectLinkCloud != 1 {
		t.Fatalf("ListFactsByEntityCloudLinks(cloud) = %+v, %v; want the fact through its cloud-lane object link", hop, err)
	}
	if hop, err := q.ListFactsByEntityCloudLinks(ctx, sqlcgen.ListFactsByEntityCloudLinksParams{SubjectEntityID: ni(cloud), ObjectEntityID: ni(cloud), Limit: 0}); err != nil || len(hop) != 0 {
		t.Fatalf("ListFactsByEntityCloudLinks limit 0 = %+v, %v; want the limit bound", hop, err)
	}
	ep, err := q.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{Source: "memory_file", SourcePath: ns("inbox/a.md"), SourceTier: "user_asserted", Status: "active", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO evidence (fact_id, episode_id, polarity, weight, created_at) VALUES (?, ?, 1, 0.5, ?)`, f.ID, ep.ID, now); err != nil {
		t.Fatal(err)
	}
	if rows, err := q.ListFactsCitingEpisodes(ctx, []sql.NullInt64{ni(ep.ID)}); err != nil || len(rows) != 1 || rows[0].SubjectLinkCloud != 0 || rows[0].ObjectLinkCloud != 1 {
		t.Fatalf("ListFactsCitingEpisodes = %+v, %v; want the link lanes", rows, err)
	}
}
