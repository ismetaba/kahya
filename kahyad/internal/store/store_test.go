package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/migrations"
)

// wantTables are the ten §5-mandated tables that must exist from day 1
// (HANDOFF §5 ⚑ schema block), even though most start empty.
var wantTables = []string{
	"episodes", "chunks", "facts", "entities", "entity_aliases",
	"evidence", "merge_ledger", "tasks", "events", "outbox",
}

// ftsVecTables are the three W12-03 virtual tables (FTS5 dual index +
// sqlite-vec) that must both fully tear down on goose.DownTo(0) and be
// fully recreated - dimension CHECK included - by the next goose.Up
// (MINOR 8 regression coverage; see TestGooseDownUpRoundTrip).
var ftsVecTables = []string{"chunks_fts_tri", "chunks_fts_uni", "chunk_vec"}

func testCfg(t *testing.T) config.Config {
	t.Helper()
	return config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")}
}

func tableNames(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()

	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan sqlite_master row: %v", err)
		}
		got[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	return got
}

func TestOpenCreatesAllMandatedTables(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	got := tableNames(t, s.DB())
	for _, want := range wantTables {
		if !got[want] {
			t.Errorf("table %q missing after Open(); sqlite_master tables = %v", want, got)
		}
	}
}

func TestOpenReopenIsIdempotent(t *testing.T) {
	cfg := testCfg(t)

	s1, err := Open(cfg)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	v1 := s1.SchemaVersion()
	if err := s1.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	s2, err := Open(cfg)
	if err != nil {
		t.Fatalf("second Open() on existing brain.db error = %v", err)
	}
	defer s2.Close()

	if got := s2.SchemaVersion(); got != v1 {
		t.Errorf("schema version after reopen = %d, want %d (unchanged)", got, v1)
	}
	got := tableNames(t, s2.DB())
	for _, want := range wantTables {
		if !got[want] {
			t.Errorf("table %q missing after reopen; tables = %v", want, got)
		}
	}
}

func TestJournalModeIsWAL(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	var mode string
	if err := s.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestUserVersionMatchesLatestMigration(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	var userVersion int64
	if err := s.DB().QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if userVersion == 0 {
		t.Fatal("PRAGMA user_version = 0, want > 0 after migrating")
	}
	if userVersion != s.SchemaVersion() {
		t.Errorf("PRAGMA user_version = %d, want %d (Store.SchemaVersion())", userVersion, s.SchemaVersion())
	}

	// Cross-check against goose's own bookkeeping table so this test fails
	// loudly if a future migration forgets to keep user_version in sync.
	gooseVersion, err := goose.GetDBVersion(s.DB())
	if err != nil {
		t.Fatalf("goose.GetDBVersion: %v", err)
	}
	if userVersion != gooseVersion {
		t.Errorf("PRAGMA user_version = %d, want goose version %d", userVersion, gooseVersion)
	}
}

func TestEventsIsAppendOnly(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	ev, err := s.Queries.InsertEvent(ctx, sqlcgen.InsertEventParams{
		TraceID:   "t-append-only",
		Ts:        "2026-07-10T00:00:00Z",
		Kind:      "test_event",
		Payload:   "{}",
		CreatedAt: "2026-07-10T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	const wantErrSubstring = "ledger is append-only"

	if _, err := s.DB().ExecContext(ctx, `UPDATE events SET payload = 'x' WHERE id = ?`, ev.ID); err == nil {
		t.Error("UPDATE events succeeded, want failure (ledger is append-only)")
	} else if !strings.Contains(err.Error(), wantErrSubstring) {
		t.Errorf("UPDATE events error = %q, want it to contain %q", err.Error(), wantErrSubstring)
	}

	if _, err := s.DB().ExecContext(ctx, `DELETE FROM events WHERE id = ?`, ev.ID); err == nil {
		t.Error("DELETE events succeeded, want failure (ledger is append-only)")
	} else if !strings.Contains(err.Error(), wantErrSubstring) {
		t.Errorf("DELETE events error = %q, want it to contain %q", err.Error(), wantErrSubstring)
	}
}

func TestGooseDownUpRoundTrip(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("goose.SetDialect: %v", err)
	}

	if err := goose.DownTo(s.DB(), ".", 0); err != nil {
		t.Fatalf("goose.DownTo(0): %v", err)
	}
	got := tableNames(t, s.DB())
	for _, name := range wantTables {
		if got[name] {
			t.Errorf("table %q still present after migrating all the way down", name)
		}
	}
	// MINOR 8: the W12-03 FTS5/vec virtual tables must also be torn down
	// by the down migrations, not just the §5 base tables checked above.
	for _, name := range ftsVecTables {
		if got[name] {
			t.Errorf("virtual table %q still present after migrating all the way down", name)
		}
	}

	if err := goose.Up(s.DB(), "."); err != nil {
		t.Fatalf("goose.Up after down: %v", err)
	}
	got = tableNames(t, s.DB())
	for _, want := range wantTables {
		if !got[want] {
			t.Errorf("table %q missing after down/up round-trip; tables = %v", want, got)
		}
	}
	for _, want := range ftsVecTables {
		if !got[want] {
			t.Errorf("virtual table %q missing after down/up round-trip; tables = %v", want, got)
		}
	}

	// MINOR 8: chunk_vec's 512-float dimension CHECK must survive the
	// down/up round trip too - a re-created chunk_vec that silently
	// dropped its dimension enforcement would accept a malformed
	// embedding without error.
	blob511 := make([]byte, 511*4)
	if _, err := s.DB().Exec(
		`INSERT INTO chunk_vec(chunk_id, embedding, model_ver) VALUES (?, ?, ?)`,
		1, blob511, "qwen3-embedding-0.6b:512:v1"); err == nil {
		t.Error("insert of a 511-float embedding succeeded after down/up round-trip, want a dimension-mismatch error")
	}
}

// TestFactsRejectsInvalidEvidentiality is the §5-mandated CHECK-enum
// regression test (task file step 5): 'sanılan' ("assumed", Turkish) is not
// one of witnessed|reported|inferred and must be rejected by the CHECK
// constraint, not silently accepted.
func TestFactsRejectsInvalidEvidentiality(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	_, err = s.DB().Exec(`
		INSERT INTO facts (subject, predicate, object, source_tier, evidentiality, confidence, updated_at, created_at)
		VALUES ('ben', 'yasiyorum', 'Istanbul', 'user_asserted', 'sanılan', 0, '2026-07-10T00:00:00Z', '2026-07-10T00:00:00Z')
	`)
	if err == nil {
		t.Fatal("INSERT with evidentiality='sanılan' succeeded, want CHECK constraint failure")
	}
}

func TestHealthReportsSchemaVersion(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	ok, version, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if !ok {
		t.Error("Health() ok = false, want true")
	}
	if version != s.SchemaVersion() {
		t.Errorf("Health() schemaVersion = %d, want %d", version, s.SchemaVersion())
	}
}

// TestMigrationFromV1UpgradesToV2 is the W12-03 acceptance-criterion
// regression test: a brain.db that only ever saw migration 0001 (as every
// pre-W12-03 database did) must migrate cleanly up to the latest embedded
// schema version (chunks_fts_tri/chunks_fts_uni/chunk_vec from 0002, and -
// since W3-02 - autonomy_state/approval_tokens/undo_windows from 0003) the
// next time kahyad boots. The expected latest version is asserted as 3
// (0001+0002+0003); bump this literal, deliberately, the next time a new
// migration file is added - this is the one place in the test suite that
// pins "the latest goose version kahyad ships" as a number, so a forgotten
// migration file never silently passes this gate.
func TestMigrationFromV1UpgradesToV2(t *testing.T) {
	cfg := testCfg(t)

	// Simulate a pre-existing v1-only database by migrating a raw
	// connection up to version 1 ONLY, bypassing Store.Open (which always
	// migrates to the latest embedded version).
	rawDB, err := sql.Open("sqlite3", cfg.DBPath+dsnPragmas)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	goose.SetLogger(goose.NopLogger())
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("goose.SetDialect: %v", err)
	}
	if err := goose.UpTo(rawDB, ".", 1); err != nil {
		t.Fatalf("goose.UpTo(1): %v", err)
	}

	got := tableNames(t, rawDB)
	for _, name := range []string{"chunks_fts_tri", "chunks_fts_uni", "chunk_vec"} {
		if got[name] {
			t.Fatalf("table %q unexpectedly present on a v1-only db", name)
		}
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	// Boot through the real path: Store.Open must migrate the existing v1
	// db cleanly up to v2 (and pass the sqlite_feature_missing assertion).
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() on v1-only db error = %v", err)
	}
	defer s.Close()

	const latestVersion = 3
	if s.SchemaVersion() != latestVersion {
		t.Errorf("SchemaVersion() after upgrade = %d, want %d", s.SchemaVersion(), latestVersion)
	}
	var userVersion int64
	if err := s.DB().QueryRow(`PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if userVersion != latestVersion {
		t.Errorf("PRAGMA user_version after upgrade = %d, want %d", userVersion, latestVersion)
	}

	got = tableNames(t, s.DB())
	for _, name := range []string{"chunks_fts_tri", "chunks_fts_uni", "chunk_vec", "autonomy_state", "approval_tokens", "undo_windows"} {
		if !got[name] {
			t.Errorf("table %q missing after v1->latest upgrade; tables = %v", name, got)
		}
	}
}

// TestAssertSQLiteFeaturesPassesOnOpen guards the W12-03 startup assertion
// indirectly: every successful Open() above already proves
// assertSQLiteFeatures did not fail-close a healthy database. This test
// makes that assertion explicit and checks vec_version()/sqlite_version()
// are independently queryable post-Open, the same way assertSQLiteFeatures
// checks them.
func TestAssertSQLiteFeaturesPassesOnOpen(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	var sqliteVersion, vecVersion string
	if err := s.DB().QueryRow(`SELECT sqlite_version()`).Scan(&sqliteVersion); err != nil {
		t.Fatalf("sqlite_version(): %v", err)
	}
	if err := s.DB().QueryRow(`SELECT vec_version()`).Scan(&vecVersion); err != nil {
		t.Fatalf("vec_version(): %v (sqlite-vec extension not loaded)", err)
	}
	if major, minor, err := parseSQLiteVersion(sqliteVersion); err != nil {
		t.Errorf("parseSQLiteVersion(%q): %v", sqliteVersion, err)
	} else if major < minSQLiteMajor || (major == minSQLiteMajor && minor < minSQLiteMinor) {
		t.Errorf("sqlite_version() = %s, want >= %d.%d", sqliteVersion, minSQLiteMajor, minSQLiteMinor)
	}
}

// TestParseSQLiteVersion is a pure unit test of the major.minor parser used
// by assertSQLiteFeatures's >= 3.45 floor check.
func TestParseSQLiteVersion(t *testing.T) {
	cases := []struct {
		in        string
		wantMajor int
		wantMinor int
		wantErr   bool
	}{
		{in: "3.45.0", wantMajor: 3, wantMinor: 45},
		{in: "3.53.2", wantMajor: 3, wantMinor: 53},
		{in: "4.0.0", wantMajor: 4, wantMinor: 0},
		{in: "3.44.9", wantMajor: 3, wantMinor: 44},
		{in: "garbage", wantErr: true},
		{in: "3", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		major, minor, err := parseSQLiteVersion(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSQLiteVersion(%q) error = nil, want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSQLiteVersion(%q) error = %v, want nil", c.in, err)
			continue
		}
		if major != c.wantMajor || minor != c.wantMinor {
			t.Errorf("parseSQLiteVersion(%q) = (%d, %d), want (%d, %d)", c.in, major, minor, c.wantMajor, c.wantMinor)
		}
	}
}

// TestEventsReplaceCannotBypassAppendOnly guards the recursive_triggers
// pragma: with it OFF, INSERT OR REPLACE's implicit row-delete on a PK
// conflict skips the events DELETE trigger and silently overwrites a
// ledger row (§5 safety #4 violation). With _recursive_triggers=on in the
// DSN, the implicit delete fires events_no_delete and the REPLACE fails.
func TestEventsReplaceCannotBypassAppendOnly(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	ev, err := s.Queries.InsertEvent(ctx, sqlcgen.InsertEventParams{
		TraceID:   "t-replace-bypass",
		Ts:        "2026-07-10T00:00:00Z",
		Kind:      "test_event",
		Payload:   `{"a":1}`,
		CreatedAt: "2026-07-10T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	// Sanity: the pragma must actually be on for this connection pool.
	var rt int
	if err := s.DB().QueryRowContext(ctx, `PRAGMA recursive_triggers`).Scan(&rt); err != nil {
		t.Fatalf("PRAGMA recursive_triggers: %v", err)
	}
	if rt != 1 {
		t.Fatalf("recursive_triggers = %d, want 1 (DSN must set _recursive_triggers=on)", rt)
	}

	_, err = s.DB().ExecContext(ctx,
		`INSERT OR REPLACE INTO events (id, trace_id, ts, kind, payload, created_at)
		 VALUES (?, 'TAMPERED', '2099-01-01T00:00:00Z', 'k', '{"a":999}', '2099-01-01T00:00:00Z')`,
		ev.ID)
	if err == nil {
		t.Fatal("INSERT OR REPLACE on events succeeded, want failure (ledger is append-only)")
	}
	if !strings.Contains(err.Error(), "ledger is append-only") {
		t.Errorf("INSERT OR REPLACE error = %q, want it to contain %q", err.Error(), "ledger is append-only")
	}

	// The original row must be intact.
	var traceID string
	if err := s.DB().QueryRowContext(ctx, `SELECT trace_id FROM events WHERE id = ?`, ev.ID).Scan(&traceID); err != nil {
		t.Fatalf("re-read event: %v", err)
	}
	if traceID != "t-replace-bypass" {
		t.Errorf("event trace_id = %q after failed REPLACE, want original %q", traceID, "t-replace-bypass")
	}
}
