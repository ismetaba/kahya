package store

import (
	"context"
	"database/sql"
	"errors"
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
// RetractFact decides the kind itself, atomically with the denial check
// (FIX-FINAL fact-history-ended-vs-corrected, review): an 'ended' request
// is recorded ended (valid_to = ended_valid_to) only while the fact has no
// negative evidence row; otherwise it is recorded corrected (valid_to = the
// retraction time). RETURNING is the recorded kind.
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
	retract := func(id int64, kind string) (string, error) {
		t.Helper()
		got, err := q.RetractFact(ctx, sqlcgen.RetractFactParams{
			RequestedKind: kind, EndedValidTo: "2026-05-01T00:00:00Z", RetractedAt: "2026-06-01T10:00:00Z",
			UpdatedAt: "2026-06-01T10:00:01Z", ID: id,
		})
		return got.String, err
	}
	ended, corrected, denied := insert("Acme"), insert("Beta"), insert("Delta")
	if got, err := retract(ended, "ended"); err != nil || got != "ended" {
		t.Fatalf("RetractFact(active, ended) = %q, %v; want ended", got, err)
	}
	if _, err := retract(ended, "corrected"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("RetractFact(already retracted) err = %v, want sql.ErrNoRows", err)
	}
	if got, err := retract(corrected, "corrected"); err != nil || got != "corrected" {
		t.Fatalf("RetractFact(active, corrected) = %q, %v", got, err)
	}
	// A user denial on file: an ended request is recorded corrected.
	if _, err := q.InsertEvidence(ctx, sqlcgen.InsertEvidenceParams{FactID: denied, SessionID: sql.NullString{String: "ritual-1", Valid: true},
		Polarity: -1, Weight: -2.94, CreatedAt: "2026-03-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if got, err := retract(denied, "ended"); err != nil || got != "corrected" {
		t.Fatalf("RetractFact(denied, ended) = %q, %v; want corrected", got, err)
	}
	for id, want := range map[int64][2]string{
		ended:     {"ended", "2026-05-01T00:00:00Z"},
		corrected: {"corrected", "2026-06-01T10:00:00Z"},
		denied:    {"corrected", "2026-06-01T10:00:00Z"},
	} {
		f, err := q.GetFact(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if f.Status != "retracted" || f.RetractionKind.String != want[0] || f.RetractedAt.String != "2026-06-01T10:00:00Z" ||
			f.ValidTo.String != want[1] || f.UpdatedAt != "2026-06-01T10:00:01Z" {
			t.Fatalf("retracted row %d = %+v, want kind %s valid_to %s", id, f, want[0], want[1])
		}
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
