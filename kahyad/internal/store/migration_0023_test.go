package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"kahya/kahyad/migrations"
)

// openRawAt migrates a raw connection to exactly version v (bypassing
// Store.Open, which always migrates to the latest embedded version).
func openRawAt(t *testing.T, dbPath string, v int64) *sql.DB {
	t.Helper()
	rawDB, err := sql.Open("sqlite3", dbPath+dsnPragmas)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	goose.SetLogger(goose.NopLogger())
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("goose.SetDialect: %v", err)
	}
	if err := goose.UpTo(rawDB, ".", v); err != nil {
		t.Fatalf("goose.UpTo(%d): %v", v, err)
	}
	return rawDB
}

// TestMigration0023BackfillsPushedRowsLocalFallback (FIX-P-15): 0023 adds
// anchor_log.local_fallback_at. A row already 'pushed' under the pre-G07
// code had its line appended to the fallback right after the push, so it
// is backfilled as written (anchored_at); a 'pending' row is not.
func TestMigration0023BackfillsPushedRowsLocalFallback(t *testing.T) {
	cfg := testCfg(t)
	rawDB := openRawAt(t, cfg.DBPath, 10)
	if _, err := rawDB.Exec(`INSERT INTO anchor_log (event_id, digest_hex, anchored_at, remote_ref, status) VALUES
		(1, 'aa', '2026-01-01T00:00:00Z', 'file:///r', 'pushed'),
		(2, 'bb', '2026-01-01T06:00:00Z', NULL, 'pending')`); err != nil {
		t.Fatalf("insert pre-0023 anchor_log rows: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	read := func(eventID int64) sql.NullString {
		t.Helper()
		var v sql.NullString
		if err := s.DB().QueryRow(`SELECT local_fallback_at FROM anchor_log WHERE event_id = ?`, eventID).Scan(&v); err != nil {
			t.Fatalf("read local_fallback_at(event %d): %v", eventID, err)
		}
		return v
	}
	if got := read(1); !got.Valid || got.String != "2026-01-01T00:00:00Z" {
		t.Errorf("pushed row local_fallback_at = %+v, want backfilled '2026-01-01T00:00:00Z'", got)
	}
	if got := read(2); got.Valid {
		t.Errorf("pending row local_fallback_at = %+v, want NULL (written by the next Pusher.Run)", got)
	}
}

// TestMigration0023RecordsLegacyChainBase (G07-2 / S-3): events written
// before 0010 seeded ledger_digest_state were never folded into the running
// digest, so a from-event-1 recompute can never match it. 0023 records the
// last such legacy event id in ledger_chain_base; verify recomputes from
// the event after it. The production brain.db on the development Mac was
// at schema 6 with 54 events when this was written - the first subtest is
// exactly that shape.
func TestMigration0023RecordsLegacyChainBase(t *testing.T) {
	ctx := context.Background()
	insertRaw := func(t *testing.T, db *sql.DB, createdAt string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO events (trace_id, ts, kind, payload, created_at) VALUES ('tr', ?, 'legacy.event', '{}', ?)`, createdAt, createdAt); err != nil {
			t.Fatalf("insert legacy event: %v", err)
		}
	}
	chainBase := func(t *testing.T, s *Store) int64 {
		t.Helper()
		var n int64
		if err := s.DB().QueryRow(`SELECT base_event_id FROM ledger_chain_base WHERE id = 1`).Scan(&n); err != nil {
			t.Fatalf("read ledger_chain_base: %v", err)
		}
		return n
	}

	t.Run("pre-0010 database: every existing event is legacy", func(t *testing.T) {
		cfg := testCfg(t)
		rawDB := openRawAt(t, cfg.DBPath, 6)
		for i := 0; i < 3; i++ {
			insertRaw(t, rawDB, "2026-07-11T01:50:44.931902Z")
		}
		if err := rawDB.Close(); err != nil {
			t.Fatal(err)
		}
		s, err := Open(cfg)
		if err != nil {
			t.Fatalf("Open(): %v", err)
		}
		defer s.Close()
		if got := chainBase(t, s); got != 3 {
			t.Errorf("chain base = %d, want 3 (all pre-0010 events)", got)
		}
		if err := s.LogEvent(ctx, "tr-new", "test.chained", map[string]any{}); err != nil {
			t.Fatalf("LogEvent: %v", err)
		}
		if got := chainBase(t, s); got != 3 {
			t.Errorf("chain base after a new event = %d, want still 3 (written once, by the migration)", got)
		}
	})

	t.Run("post-0010 database with legacy and chained events", func(t *testing.T) {
		cfg := testCfg(t)
		rawDB := openRawAt(t, cfg.DBPath, 9)
		insertRaw(t, rawDB, "2026-01-01T00:00:00Z")
		insertRaw(t, rawDB, "2026-01-01T00:00:01.5Z")
		if err := goose.UpTo(rawDB, ".", 10); err != nil {
			t.Fatalf("goose.UpTo(10): %v", err)
		}
		for i := 0; i < 2; i++ {
			if _, err := InsertEventWithDigest(ctx, rawDB, "tr-chained", "test.chained", []byte("{}"), time.Now()); err != nil {
				t.Fatalf("InsertEventWithDigest: %v", err)
			}
		}
		if err := goose.UpTo(rawDB, ".", 22); err != nil {
			t.Fatalf("goose.UpTo(22): %v", err)
		}
		if err := rawDB.Close(); err != nil {
			t.Fatal(err)
		}
		s, err := Open(cfg)
		if err != nil {
			t.Fatalf("Open(): %v", err)
		}
		defer s.Close()
		if got := chainBase(t, s); got != 2 {
			t.Errorf("chain base = %d, want 2 (the events created before 0010 was applied)", got)
		}
	})

	t.Run("post-0010 database without legacy events", func(t *testing.T) {
		cfg := testCfg(t)
		rawDB := openRawAt(t, cfg.DBPath, 22)
		for i := 0; i < 2; i++ {
			if _, err := InsertEventWithDigest(ctx, rawDB, "tr-chained", "test.chained", []byte("{}"), time.Now()); err != nil {
				t.Fatalf("InsertEventWithDigest: %v", err)
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
		if got := chainBase(t, s); got != 0 {
			t.Errorf("chain base = %d, want 0 (every event is chained)", got)
		}
	})

	t.Run("fresh database", func(t *testing.T) {
		s, err := Open(testCfg(t))
		if err != nil {
			t.Fatalf("Open(): %v", err)
		}
		defer s.Close()
		if got := chainBase(t, s); got != 0 {
			t.Errorf("chain base = %d, want 0", got)
		}
	})
}
