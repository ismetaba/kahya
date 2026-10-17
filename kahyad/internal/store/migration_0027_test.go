package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"

	"kahya/kahyad/internal/store/sqlcgen"
)

var migration0027Columns = []string{"retraction_kind", "retracted_at"}

// TestMigration0027LeavesEveryPreExistingFactWithoutAKind (FIX-FINAL
// fact-history-ended-vs-corrected): 0027 adds facts.retraction_kind and
// facts.retracted_at. A pre-0027 retraction's kind is unknown, so it stays
// NULL - read as corrected (never shown as history) - and an active fact
// has none either; the column refuses any value but ended/corrected.
func TestMigration0027LeavesEveryPreExistingFactWithoutAKind(t *testing.T) {
	cfg := testCfg(t)
	rawDB := openRawAt(t, cfg.DBPath, 26)
	for _, stmt := range []string{
		`INSERT INTO facts (id, subject, predicate, object, source_tier, evidentiality, confidence, importance, status, valid_to, updated_at, created_at) VALUES (1, 'Emre', 'works_at', 'Acme', 'user_asserted', 'witnessed', 0, 0, 'retracted', '2026-05-01T00:00:00Z', '2026-05-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO facts (id, subject, predicate, object, source_tier, evidentiality, confidence, importance, status, updated_at, created_at) VALUES (2, 'Emre', 'lives_in', 'Moda', 'user_asserted', 'witnessed', 2.9, 0, 'active', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := rawDB.Exec(stmt); err != nil {
			t.Fatalf("seed pre-0027 row: %v\n%s", err, stmt)
		}
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	got := columnNames(t, s.DB(), "facts")
	for _, c := range migration0027Columns {
		if !got[c] {
			t.Errorf("facts.%s missing after 0027", c)
		}
	}
	for _, id := range []int64{1, 2} {
		f, err := s.Queries.GetFact(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if f.RetractionKind.Valid || f.RetractedAt.Valid {
			t.Errorf("pre-0027 fact %d (status %s) = kind %v, retracted_at %v; want both NULL", id, f.Status, f.RetractionKind, f.RetractedAt)
		}
	}
	if _, err := s.DB().Exec(`UPDATE facts SET retraction_kind = 'gone' WHERE id = 1`); !isConstraintViolation(err) {
		t.Fatalf("retraction_kind 'gone' = %v, want a CHECK violation", err)
	}
	for _, k := range []string{"ended", "corrected"} {
		if _, err := s.DB().Exec(`UPDATE facts SET retraction_kind = ? WHERE id = 1`, k); err != nil {
			t.Fatalf("retraction_kind %q = %v, want accepted", k, err)
		}
	}
}

// TestMigration0027DownRestoresPreviousSchema: 0027's Down removes exactly
// its two columns.
func TestMigration0027DownRestoresPreviousSchema(t *testing.T) {
	cfg := testCfg(t)
	rawDB := openRawAt(t, cfg.DBPath, 27)
	defer rawDB.Close()
	if err := goose.DownTo(rawDB, ".", 26); err != nil {
		t.Fatalf("goose.DownTo(26): %v", err)
	}
	got := columnNames(t, rawDB, "facts")
	for _, c := range migration0027Columns {
		if got[c] {
			t.Errorf("facts.%s still present after 0027 Down", c)
		}
	}
	if !got["object_link_cloud"] {
		t.Error("0027 Down dropped a 0025 column")
	}
}

// TestRetractionKindQueriesBindEveryParameter: RetractFact and
// MarkFactRetractionCorrected run against the real schema with every
// parameter their generated methods bind (sqlc v1.30.0 can silently drop
// one), close only an active fact, and flip only an ended retraction.
func TestRetractionKindQueriesBindEveryParameter(t *testing.T) {
	ctx := context.Background()
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	q := s.Queries
	insert := func(obj string) int64 {
		t.Helper()
		f, err := q.InsertFact(ctx, sqlcgen.InsertFactParams{
			Subject: "Emre", Predicate: "works_at", Object: obj, SourceTier: "user_asserted", Evidentiality: "witnessed",
			Status: "active", UpdatedAt: "2026-01-01T00:00:00Z", CreatedAt: "2026-01-01T00:00:00Z",
		})
		if err != nil {
			t.Fatal(err)
		}
		if f.RetractionKind.Valid || f.RetractedAt.Valid {
			t.Fatalf("InsertFact RETURNING = kind %v retracted_at %v, want NULLs", f.RetractionKind, f.RetractedAt)
		}
		return f.ID
	}
	ns := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	retract := func(id int64, kind string) int64 {
		t.Helper()
		n, err := q.RetractFact(ctx, sqlcgen.RetractFactParams{
			RetractionKind: ns(kind), RetractedAt: ns("2026-06-01T10:00:00Z"), ValidTo: ns("2026-05-01T00:00:00Z"),
			UpdatedAt: "2026-06-01T10:00:01Z", ID: id,
		})
		if err != nil {
			t.Fatalf("RetractFact(%d, %s): %v", id, kind, err)
		}
		return n
	}
	ended, corrected := insert("Acme"), insert("Beta")
	if n := retract(ended, "ended"); n != 1 {
		t.Fatalf("RetractFact(active) rows = %d, want 1", n)
	}
	if n := retract(ended, "corrected"); n != 0 {
		t.Fatalf("RetractFact(already retracted) rows = %d, want 0", n)
	}
	retract(corrected, "corrected")
	f, err := q.GetFact(ctx, ended)
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != "retracted" || f.RetractionKind.String != "ended" || f.RetractedAt.String != "2026-06-01T10:00:00Z" ||
		f.ValidTo.String != "2026-05-01T00:00:00Z" || f.UpdatedAt != "2026-06-01T10:00:01Z" {
		t.Fatalf("retracted row = %+v", f)
	}

	mark := func(id int64) int64 {
		t.Helper()
		n, err := q.MarkFactRetractionCorrected(ctx, sqlcgen.MarkFactRetractionCorrectedParams{UpdatedAt: "2026-07-01T00:00:00Z", ID: id})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	active := insert("Gamma")
	if n := mark(active); n != 0 {
		t.Fatalf("MarkFactRetractionCorrected(active) rows = %d, want 0", n)
	}
	if n := mark(corrected); n != 0 {
		t.Fatalf("MarkFactRetractionCorrected(corrected) rows = %d, want 0", n)
	}
	if n := mark(ended); n != 1 {
		t.Fatalf("MarkFactRetractionCorrected(ended) rows = %d, want 1", n)
	}
	if f, err := q.GetFact(ctx, ended); err != nil || f.RetractionKind.String != "corrected" || f.UpdatedAt != "2026-07-01T00:00:00Z" {
		t.Fatalf("after mark = %+v, %v; want corrected", f, err)
	}
}
