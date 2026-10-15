package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"

	"kahya/kahyad/internal/store/sqlcgen"
)

func columnNames(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out[n] = true
	}
	return out
}

func indexNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'index'`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		out[n] = true
	}
	return out
}

var (
	migration0024Columns = map[string][]string{
		"facts":          {"subject_entity_id", "object_entity_id", "quote"},
		"entity_aliases": {"alias_folded"},
		"episodes":       {"extracted_at", "extractor_ver"},
	}
	migration0024Indexes = []string{
		"idx_facts_subject_entity", "idx_facts_object_entity", "idx_facts_predicate", "idx_facts_status_tier",
		"idx_entity_aliases_alias", "idx_entity_aliases_folded", "idx_evidence_episode_id", "idx_episodes_extracted_at",
	}
)

// TestMigration0024AddsGraphColumnsAndBackfillsFoldedAliases (FIX-MEM-2
// phase 0): 0024 adds the G13 graph columns and indexes; a pre-0024 alias row
// gets alias_folded = lower(alias), and every pre-0024 fact/episode row keeps
// NULL links and NULL extraction markers (nothing pending is invented).
func TestMigration0024AddsGraphColumnsAndBackfillsFoldedAliases(t *testing.T) {
	cfg := testCfg(t)
	rawDB := openRawAt(t, cfg.DBPath, 23)
	for _, stmt := range []string{
		`INSERT INTO entities (id, canonical_name, kind, status, provisional, created_at) VALUES (1, 'EMRE', 'person', 'active', 0, '2026-01-01T00:00:00Z')`,
		`INSERT INTO entity_aliases (entity_id, alias, created_at) VALUES (1, 'EMRE', '2026-01-01T00:00:00Z')`,
		`INSERT INTO facts (subject, predicate, object, source_tier, evidentiality, confidence, importance, status, updated_at, created_at) VALUES ('Emre', 'sever', 'kahve', 'agent_derived', 'inferred', 0, 0, 'active', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO episodes (source, source_path, source_tier, status, created_at) VALUES ('memory_file', 'notes/a.md', 'user_asserted', 'active', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := rawDB.Exec(stmt); err != nil {
			t.Fatalf("seed pre-0024 row: %v\n%s", err, stmt)
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
	for table, cols := range migration0024Columns {
		got := columnNames(t, s.DB(), table)
		for _, c := range cols {
			if !got[c] {
				t.Errorf("%s.%s missing after 0024", table, c)
			}
		}
	}
	idx := indexNames(t, s.DB())
	for _, n := range migration0024Indexes {
		if !idx[n] {
			t.Errorf("index %s missing after 0024", n)
		}
	}

	var folded sql.NullString
	if err := s.DB().QueryRow(`SELECT alias_folded FROM entity_aliases WHERE alias = 'EMRE'`).Scan(&folded); err != nil {
		t.Fatalf("read alias_folded: %v", err)
	}
	if !folded.Valid || folded.String != "emre" {
		t.Errorf("backfilled alias_folded = %+v, want 'emre'", folded)
	}
	var subj, obj sql.NullInt64
	var quote sql.NullString
	if err := s.DB().QueryRow(`SELECT subject_entity_id, object_entity_id, quote FROM facts`).Scan(&subj, &obj, &quote); err != nil {
		t.Fatalf("read fact links: %v", err)
	}
	if subj.Valid || obj.Valid || quote.Valid {
		t.Errorf("pre-0024 fact links = %+v %+v %+v, want all NULL", subj, obj, quote)
	}
	var extractedAt, extractorVer sql.NullString
	if err := s.DB().QueryRow(`SELECT extracted_at, extractor_ver FROM episodes`).Scan(&extractedAt, &extractorVer); err != nil {
		t.Fatalf("read extraction markers: %v", err)
	}
	if extractedAt.Valid || extractorVer.Valid {
		t.Errorf("pre-0024 episode markers = %+v %+v, want NULL (pending)", extractedAt, extractorVer)
	}
}

// TestMigration0024DownRestoresPreviousSchema (FIX-MEM-2 phase 0): 0024's
// Down section runs on the embedded SQLite and removes exactly its columns
// and indexes.
func TestMigration0024DownRestoresPreviousSchema(t *testing.T) {
	cfg := testCfg(t)
	rawDB := openRawAt(t, cfg.DBPath, 24)
	defer rawDB.Close()
	if err := goose.DownTo(rawDB, ".", 23); err != nil {
		t.Fatalf("goose.DownTo(23): %v", err)
	}
	for table, cols := range migration0024Columns {
		got := columnNames(t, rawDB, table)
		for _, c := range cols {
			if got[c] {
				t.Errorf("%s.%s still present after 0024 Down", table, c)
			}
		}
	}
	idx := indexNames(t, rawDB)
	for _, n := range migration0024Indexes {
		if idx[n] {
			t.Errorf("index %s still present after 0024 Down", n)
		}
	}
}

// TestMemoryV2QueriesBindEveryParameter (FIX-MEM-2 phase 0): every G13
// query runs against the real schema with the arguments its generated method
// binds - sqlc v1.30.0 silently drops a parameter it cannot parse (it did
// for "extractor_ver IS NOT ?"), which only a real call catches - and returns
// what its comment promises.
func TestMemoryV2QueriesBindEveryParameter(t *testing.T) {
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

	emre, err := q.InsertEntity(ctx, sqlcgen.InsertEntityParams{CanonicalName: "Emre", Kind: ns("person"), Status: "active", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := q.InsertEntity(ctx, sqlcgen.InsertEntityParams{CanonicalName: "Emre", Kind: ns("person"), Status: "merged", Provisional: 1, CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct {
		id    int64
		alias string
	}{{emre.ID, "Emre"}, {merged.ID, "Emre"}} {
		if _, err := q.InsertEntityAlias(ctx, sqlcgen.InsertEntityAliasParams{EntityID: a.id, Alias: a.alias, AliasFolded: ns("emre"), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := q.ListEntityIDsByAliasFolded(ctx, ns("emre"))
	if err != nil || len(all) != 2 {
		t.Fatalf("ListEntityIDsByAliasFolded = %v, %v; want both entities", all, err)
	}
	active, err := q.ListActiveEntityIDsByAliasFolded(ctx, ns("emre"))
	if err != nil || len(active) != 1 || active[0] != emre.ID {
		t.Fatalf("ListActiveEntityIDsByAliasFolded = %v, %v; want only the active entity", active, err)
	}
	if a, err := q.GetEntityAliasByEntityAndAlias(ctx, sqlcgen.GetEntityAliasByEntityAndAliasParams{EntityID: emre.ID, Alias: "Emre"}); err != nil || a.AliasFolded.String != "emre" {
		t.Fatalf("GetEntityAliasByEntityAndAlias = %+v, %v", a, err)
	}
	if ents, err := q.ListEntitiesByFolded(ctx, sqlcgen.ListEntitiesByFoldedParams{AliasFolded: ns("em%"), Limit: 10}); err != nil || len(ents) != 1 || ents[0].ID != emre.ID {
		t.Fatalf("ListEntitiesByFolded = %+v, %v", ents, err)
	}

	f, err := q.InsertFact(ctx, sqlcgen.InsertFactParams{
		Subject: "Emre", Predicate: "çalışır", Object: "gold-token", SourceTier: "agent_derived", Evidentiality: "inferred",
		Status: "active", UpdatedAt: now, CreatedAt: now, SubjectEntityID: ni(emre.ID), Quote: ns("Emre gold-token'da çalışıyor"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.SubjectEntityID.Valid || f.SubjectEntityID.Int64 != emre.ID || f.ObjectEntityID.Valid || f.Quote.String == "" {
		t.Fatalf("InsertFact RETURNING = %+v", f)
	}
	if err := q.UpdateFactLinksAndValidity(ctx, sqlcgen.UpdateFactLinksAndValidityParams{
		SubjectEntityID: ni(merged.ID), ObjectEntityID: ni(emre.ID), Quote: ns("başka"), ValidFrom: ns(now), UpdatedAt: now, ID: f.ID,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := q.GetFact(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubjectEntityID.Int64 != emre.ID || got.ObjectEntityID.Int64 != emre.ID || got.Quote.String != "Emre gold-token'da çalışıyor" || got.ValidFrom.String != now {
		t.Fatalf("after UpdateFactLinksAndValidity = %+v, want existing values kept (COALESCE) and NULLs filled", got)
	}
	if n, err := q.CountActiveFactsForEntity(ctx, sqlcgen.CountActiveFactsForEntityParams{SubjectEntityID: ni(emre.ID), ObjectEntityID: ni(emre.ID)}); err != nil || n != 1 {
		t.Fatalf("CountActiveFactsForEntity = %d, %v; want 1", n, err)
	}
	if hop, err := q.ListFactsByEntityHop(ctx, sqlcgen.ListFactsByEntityHopParams{SubjectEntityID: ni(emre.ID), ObjectEntityID: ni(emre.ID), Limit: 10}); err != nil || len(hop) != 1 || hop[0].ID != f.ID {
		t.Fatalf("ListFactsByEntityHop = %+v, %v", hop, err)
	}
	if qf, err := q.ListQuarantinedFacts(ctx, 10); err != nil || len(qf) != 1 {
		t.Fatalf("ListQuarantinedFacts = %+v, %v", qf, err)
	}

	ep, err := q.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{Source: "memory_file", SourcePath: ns("notes/ev.md"), SourceTier: "user_asserted", Status: "active", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	noPath, err := q.InsertEpisode(ctx, sqlcgen.InsertEpisodeParams{Source: "chat", SourceTier: "user_asserted", Status: "active", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, epID := range []int64{ep.ID, ep.ID, noPath.ID} {
		if _, err := s.DB().Exec(`INSERT INTO evidence (fact_id, episode_id, polarity, weight, created_at) VALUES (?, ?, 1, 0.5, ?)`, f.ID, epID, now); err != nil {
			t.Fatal(err)
		}
	}
	if paths, err := q.ListEpisodePathsForFact(ctx, f.ID); err != nil || len(paths) != 1 || paths[0].String != "notes/ev.md" {
		t.Fatalf("ListEpisodePathsForFact = %+v, %v; want the one distinct non-NULL path", paths, err)
	}
	if ids, err := q.ListFactIDsByEpisode(ctx, ni(ep.ID)); err != nil || len(ids) != 1 || ids[0] != f.ID {
		t.Fatalf("ListFactIDsByEpisode = %v, %v", ids, err)
	}
	if rows, err := q.ListFactsCitingEpisodes(ctx, []sql.NullInt64{ni(ep.ID), ni(noPath.ID)}); err != nil || len(rows) != 1 || rows[0].ID != f.ID {
		t.Fatalf("ListFactsCitingEpisodes = %+v, %v", rows, err)
	}
	if rows, err := q.ListFactsCitingEpisodes(ctx, nil); err != nil || len(rows) != 0 {
		t.Fatalf("ListFactsCitingEpisodes(nil) = %+v, %v; want none", rows, err)
	}

	pending, err := q.ListUnextractedEpisodes(ctx, sqlcgen.ListUnextractedEpisodesParams{ExtractorVer: ns("v1"), RowLimit: 10})
	if err != nil || len(pending) != 2 {
		t.Fatalf("ListUnextractedEpisodes = %+v, %v; want both episodes pending", pending, err)
	}
	if err := q.MarkEpisodeExtracted(ctx, sqlcgen.MarkEpisodeExtractedParams{ExtractedAt: ns(now), ExtractorVer: ns("v1"), ID: ep.ID}); err != nil {
		t.Fatal(err)
	}
	if pending, err = q.ListUnextractedEpisodes(ctx, sqlcgen.ListUnextractedEpisodesParams{ExtractorVer: ns("v1"), RowLimit: 10}); err != nil || len(pending) != 1 || pending[0].ID != noPath.ID {
		t.Fatalf("after MarkEpisodeExtracted(v1) = %+v, %v; want only the other episode", pending, err)
	}
	if pending, err = q.ListUnextractedEpisodes(ctx, sqlcgen.ListUnextractedEpisodesParams{ExtractorVer: ns("v2"), RowLimit: 10}); err != nil || len(pending) != 2 {
		t.Fatalf("under a new extractor version = %+v, %v; want both pending again", pending, err)
	}
	// FIX-MEM-3: after_id is the extractor's rolling start point and
	// row_limit the batch cap - both must bind.
	lo, hi := min(ep.ID, noPath.ID), max(ep.ID, noPath.ID)
	if pending, err = q.ListUnextractedEpisodes(ctx, sqlcgen.ListUnextractedEpisodesParams{ExtractorVer: ns("v2"), AfterID: lo, RowLimit: 10}); err != nil || len(pending) != 1 || pending[0].ID != hi {
		t.Fatalf("after_id=%d = %+v, %v; want only episode %d", lo, pending, err, hi)
	}
	if pending, err = q.ListUnextractedEpisodes(ctx, sqlcgen.ListUnextractedEpisodesParams{ExtractorVer: ns("v2"), RowLimit: 1}); err != nil || len(pending) != 1 || pending[0].ID != lo {
		t.Fatalf("row_limit=1 = %+v, %v; want only episode %d", pending, err, lo)
	}
	if err := q.ResetEpisodeExtraction(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	if e, err := q.GetEpisodeByID(ctx, ep.ID); err != nil || e.ExtractedAt.Valid || e.ExtractorVer.Valid {
		t.Fatalf("after ResetEpisodeExtraction = %+v, %v; want NULL markers", e, err)
	}
	if err := q.MarkEpisodeExtracted(ctx, sqlcgen.MarkEpisodeExtractedParams{ExtractedAt: ns(now), ExtractorVer: ns("v1"), ID: noPath.ID}); err != nil {
		t.Fatal(err)
	}
	if err := q.ResetAllEpisodeExtraction(ctx); err != nil {
		t.Fatal(err)
	}
	if e, err := q.GetEpisodeByID(ctx, noPath.ID); err != nil || e.ExtractedAt.Valid || e.ExtractorVer.Valid {
		t.Fatalf("after ResetAllEpisodeExtraction = %+v, %v; want NULL markers", e, err)
	}

	if _, err := s.DB().Exec(`INSERT INTO tasks (id, trace_id, state, lane, updated_at, created_at) VALUES ('t1', 'tr-1', 'done', 'secret', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if lane, err := q.GetTaskLaneByTrace(ctx, "tr-1"); err != nil || lane != "secret" {
		t.Fatalf("GetTaskLaneByTrace = %q, %v", lane, err)
	}
	if _, err := q.GetTaskLaneByTrace(ctx, "no-such-trace"); err != sql.ErrNoRows {
		t.Fatalf("GetTaskLaneByTrace(unknown) err = %v, want sql.ErrNoRows (caller fails closed)", err)
	}
}
