package store

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"

	"kahya/kahyad/migrations"
)

// TestMigration0021BackfillsIntentRows (FIX-I-14): before I-14, local and
// secret-lane (handleSecretLaneTask), briefing and reader task rows never
// entered the status machine and sat at status 'intent' forever, so every
// halt swept them. 0021 maps the rows whose free-form state already says
// how they ended (done -> done, error -> failed) and leaves genuinely
// unknown rows (state running) alone. Its Down is a no-op (data only).
func TestMigration0021BackfillsIntentRows(t *testing.T) {
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
	if err := goose.UpTo(rawDB, ".", 20); err != nil {
		t.Fatalf("goose.UpTo(20): %v", err)
	}

	rows := []struct{ id, status, state, want string }{
		{"local-done", "intent", "done", "done"},
		{"local-error", "intent", "error", "failed"},
		{"unknown-running", "intent", "running", "intent"},
		{"cloud-executing", "executing", "error", "executing"},
		{"cloud-done", "done", "done", "done"},
	}
	for _, r := range rows {
		if _, err := rawDB.Exec(`INSERT INTO tasks (id, trace_id, state, status, updated_at, created_at, lane) VALUES (?, ?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'normal')`,
			r.id, "trace-"+r.id, r.state, r.status); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	if err := goose.UpTo(rawDB, ".", 21); err != nil {
		t.Fatalf("goose.UpTo(21): %v", err)
	}
	for _, r := range rows {
		var got string
		if err := rawDB.QueryRow(`SELECT status FROM tasks WHERE id = ?`, r.id).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", r.id, err)
		}
		if got != r.want {
			t.Errorf("%s: status = %q, want %q", r.id, got, r.want)
		}
	}

	// Down is data-safe: rolling 0021 back keeps every row as it is.
	if err := goose.DownTo(rawDB, ".", 20); err != nil {
		t.Fatalf("goose.DownTo(20): %v", err)
	}
	var n int
	if err := rawDB.QueryRow(`SELECT count(*) FROM tasks WHERE status = 'done'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("done rows after Down = %d, want 2", n)
	}
}
