package store

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"kahya/kahyad/migrations"
)

// TestUndoWindowsOpIDUniquePartialIndex (FIX-B-8): 0022 adds
// undo_windows.op_id/scope. op_id is the consumed token's token_hash, so a
// second row with the same non-empty op_id must be refused by
// idx_undo_windows_op_id; legacy rows (empty op_id) never collide with each
// other. A row that existed before 0022 reads back an empty op_id and scope
// global, and Down drops both columns again.
func TestUndoWindowsOpIDUniquePartialIndex(t *testing.T) {
	cfg := testCfg(t)
	rawDB, err := sql.Open("sqlite3", cfg.DBPath+dsnPragmas)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer rawDB.Close()
	goose.SetLogger(goose.NopLogger())
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("goose.SetDialect: %v", err)
	}
	if err := goose.UpTo(rawDB, ".", 21); err != nil {
		t.Fatalf("goose.UpTo(21): %v", err)
	}
	const legacyInsert = `INSERT INTO undo_windows (task_id, tool, trace_id, opened_at, deadline) VALUES ('t0', 'fs_write', 'tr0', '2026-01-01T00:00:00Z', '2026-01-01T00:05:00Z')`
	if _, err := rawDB.Exec(legacyInsert); err != nil {
		t.Fatalf("insert pre-0022 row: %v", err)
	}
	if err := goose.UpTo(rawDB, ".", 22); err != nil {
		t.Fatalf("goose.UpTo(22): %v", err)
	}

	var opID, scope string
	if err := rawDB.QueryRow(`SELECT op_id, scope FROM undo_windows WHERE task_id = 't0'`).Scan(&opID, &scope); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if opID != "" || scope != "global" {
		t.Fatalf("legacy row after 0022: op_id=%q scope=%q, want '' / 'global'", opID, scope)
	}

	insert := func(opID string) error {
		_, err := rawDB.Exec(`INSERT INTO undo_windows (task_id, tool, trace_id, opened_at, deadline, op_id, scope) VALUES ('t1', 'fs_write', 'tr1', '2026-01-01T00:00:00Z', '2026-01-01T00:05:00Z', ?, 'global')`, opID)
		return err
	}
	if err := insert("abc"); err != nil {
		t.Fatalf("first op_id=abc insert: %v", err)
	}
	if err := insert("abc"); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("second op_id=abc insert: err = %v, want UNIQUE constraint failed", err)
	}
	if err := insert(""); err != nil {
		t.Fatalf("first legacy-shaped (op_id='') insert: %v", err)
	}
	if err := insert(""); err != nil {
		t.Fatalf("second legacy-shaped (op_id='') insert must not collide: %v", err)
	}

	if err := goose.DownTo(rawDB, ".", 21); err != nil {
		t.Fatalf("goose.DownTo(21): %v", err)
	}
	if _, err := rawDB.Exec(`SELECT op_id FROM undo_windows`); err == nil {
		t.Fatal("undo_windows.op_id still exists after 0022 Down")
	}
	var n int
	if err := rawDB.QueryRow(`SELECT count(*) FROM undo_windows`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("undo_windows rows after Down = %d, want 4 (Down drops columns, never rows)", n)
	}
}
