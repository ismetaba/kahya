// Package store owns brain.db: it opens the single SQLite database, runs
// goose migrations embedded in the kahyad binary, applies the operational
// pragmas (HANDOFF §4 stack row: WAL + busy_timeout + checkpoint policy +
// PRAGMA user_version), and exposes the sqlc-generated typed query layer
// (kahyad/internal/store/sqlcgen). kahyad is the ONLY writer of brain.db
// (HANDOFF §4/§5) — every other process reaches memory through the memory
// MCP tools, never through this package directly.
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3" // registers the "sqlite3" driver
	"github.com/pressly/goose/v3"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/migrations"
)

// dsnPragmas are applied by the mattn/go-sqlite3 driver on every new
// connection via DSN query parameters (HANDOFF §4 stack; W12-02 step 2):
// WAL journal, a 5s busy timeout so concurrent readers/writers back off
// instead of failing immediately, foreign key enforcement on, and NORMAL
// synchronous (safe under WAL). recursive_triggers must be ON or the
// implicit row-delete performed by INSERT OR REPLACE skips the events
// append-only DELETE trigger, silently overwriting ledger rows (§5 #4).
const dsnPragmas = "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on&_synchronous=NORMAL&_recursive_triggers=on"

// walAutocheckpointPages matches the step-2 checkpoint policy: run an
// automatic passive checkpoint every 1000 WAL pages so the WAL file never
// grows unbounded between the explicit TRUNCATE checkpoint on shutdown.
const walAutocheckpointPages = 1000

// Store is a handle on brain.db: the raw *sql.DB (pragmas applied, schema
// migrated) plus the sqlc-generated typed query layer.
type Store struct {
	db      *sql.DB
	Queries *sqlcgen.Queries

	// schemaVersion is the latest applied goose migration version, mirrored
	// into PRAGMA user_version as a cheap external/CLI-visible check
	// (`sqlite3 brain.db "PRAGMA user_version;"`) that doesn't require
	// knowing about goose's own tracking table.
	schemaVersion int64
}

// Open opens (creating if necessary) the SQLite database at cfg.DBPath,
// applies the WAL/busy_timeout/foreign_keys/synchronous pragmas, runs every
// pending goose migration embedded in the binary, and sets
// PRAGMA user_version to the resulting schema version. It fails closed: any
// error here — including a migration failure — must stop the caller from
// serving (HANDOFF §4 ⚑: no serving on a half-migrated DB).
func Open(cfg config.Config) (*Store, error) {
	db, err := sql.Open("sqlite3", cfg.DBPath+dsnPragmas)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", cfg.DBPath, err)
	}

	// SQLite (even in WAL mode) allows only one writer at a time, and the
	// mattn/go-sqlite3 driver hands database/sql's connection pool a
	// separate OS-level SQLite connection per pool slot. Capping the pool
	// at 1 connection serializes all access through the busy_timeout logic
	// above instead of letting Go's pool open a second connection that
	// SQLITE_BUSYs immediately outside that timeout's protection.
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	version, err := goose.GetDBVersion(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store: read schema version: %w", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: set user_version: %w", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA wal_autocheckpoint = %d", walAutocheckpointPages)); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: set wal_autocheckpoint: %w", err)
	}

	return &Store{db: db, Queries: sqlcgen.New(db), schemaVersion: version}, nil
}

// migrate runs every pending goose migration from the embedded FS against
// db. Safe to call against an already-migrated database (idempotent — goose
// tracks applied versions in its own table and skips them). goose.SetBaseFS
// and goose.SetDialect are process-global; that is fine here because
// kahyad only ever migrates one database (migrations.FS, dialect sqlite3)
// for its entire lifetime.
func migrate(db *sql.DB) error {
	// goose's default logger writes plain (non-JSONL) lines straight to
	// stdout via the stdlib "log" package, which would violate the "every
	// log line is JSONL with a trace_id" invariant (HANDOFF §4 ⚑). Silence
	// it; the caller logs its own JSONL "migrations_applied" event once
	// this returns successfully. NopLogger also makes goose's Fatalf a
	// no-op instead of an os.Exit, keeping migration failures inside our
	// own fail-closed error return path rather than a hard process exit.
	goose.SetLogger(goose.NopLogger())
	goose.SetBaseFS(migrations.FS)

	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("store: goose set dialect: %w", err)
	}
	if err := goose.Up(db, "."); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// DB returns the underlying *sql.DB for callers that need raw access (e.g.
// ad hoc queries not yet in the sqlc query set, or tests inspecting
// sqlite_master directly).
func (s *Store) DB() *sql.DB { return s.db }

// SchemaVersion returns the schema version recorded at Open time (the same
// value written to PRAGMA user_version).
func (s *Store) SchemaVersion() int64 { return s.schemaVersion }

// Health reports whether brain.db is reachable and the schema version it is
// running, for the /health endpoint (W12-02 step "extend /health").
func (s *Store) Health(ctx context.Context) (ok bool, schemaVersion int64, err error) {
	if err := s.db.PingContext(ctx); err != nil {
		return false, s.schemaVersion, err
	}
	return true, s.schemaVersion, nil
}

// Close checkpoints the WAL into the main database file (TRUNCATE mode, so
// the WAL/SHM files shrink back to empty) and then closes the underlying
// connection. main.go calls this during graceful shutdown (HANDOFF §4
// stack: WAL + checkpoint policy).
func (s *Store) Close() error {
	_, ckErr := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	closeErr := s.db.Close()
	if ckErr != nil {
		return fmt.Errorf("store: checkpoint on close: %w", ckErr)
	}
	return closeErr
}
