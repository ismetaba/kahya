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
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3" // registers the "sqlite3" driver
	"github.com/pressly/goose/v3"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/ledgerdigest"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/migrations"
)

// minSQLiteMajor/minSQLiteMinor is the floor asserted by assertSQLiteFeatures
// (HANDOFF §4 stack ⚑: FTS5 dual index / sqlite-vec need a modern SQLite).
const (
	minSQLiteMajor = 3
	minSQLiteMinor = 45
)

// ErrSQLiteFeatureMissing is returned by Open when the linked SQLite build
// lacks a feature kahyad's schema requires (too-old sqlite_version(), or no
// vec_version() at all because sqlite-vec did not load). The caller must
// fail-fast: main.go logs this distinctly as event=sqlite_feature_missing
// and exits 1 rather than serving on a database that silently can't run
// FTS5/vec0 queries (HANDOFF §4 stack ⚑, W12-03 step 1).
var ErrSQLiteFeatureMissing = fmt.Errorf("store: required sqlite feature missing")

// init registers the sqlite-vec extension as a process-wide SQLite
// "automatic extension" (calls C sqlite3_auto_extension under the hood)
// BEFORE any package in this process can call sql.Open("sqlite3", ...).
// Go guarantees every imported package's init() runs before any of that
// package's exported functions can be called, so this satisfies the W12-03
// requirement that ALL connections - including the goose migration
// connection inside Open below, which is what actually executes the
// CREATE VIRTUAL TABLE ... USING vec0(...) DDL in migrations/0002 - have
// the vec0 module available. Auto() is safe to have "run" even in test
// binaries that never touch a real vec0 table.
func init() {
	sqlite_vec.Auto()
}

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

	if err := assertSQLiteFeatures(db); err != nil {
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

// assertSQLiteFeatures fails closed (HANDOFF §4 ⚑) when the linked SQLite
// build is missing something kahyad's schema depends on: a modern-enough
// sqlite_version() (>= 3.45, needed by the FTS5/vec0 DDL in migrations/0002)
// or a working vec_version() (proves the sqlite-vec extension actually
// loaded on this connection, not just that init() ran). Both are cheap,
// read-only checks run once at boot, right after migration.
func assertSQLiteFeatures(db *sql.DB) error {
	var sqliteVersion string
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&sqliteVersion); err != nil {
		return fmt.Errorf("%w: sqlite_version() query failed: %v", ErrSQLiteFeatureMissing, err)
	}
	major, minor, err := parseSQLiteVersion(sqliteVersion)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSQLiteFeatureMissing, err)
	}
	if major < minSQLiteMajor || (major == minSQLiteMajor && minor < minSQLiteMinor) {
		return fmt.Errorf("%w: sqlite_version()=%s, need >= %d.%d", ErrSQLiteFeatureMissing, sqliteVersion, minSQLiteMajor, minSQLiteMinor)
	}

	var vecVersion string
	if err := db.QueryRow(`SELECT vec_version()`).Scan(&vecVersion); err != nil {
		return fmt.Errorf("%w: vec_version() query failed (sqlite-vec extension not loaded): %v", ErrSQLiteFeatureMissing, err)
	}

	return nil
}

// parseSQLiteVersion extracts the major/minor components from a
// sqlite_version() string ("3.45.0", "3.53.2", ...). It ignores the patch
// component entirely - only major.minor matters for the >= 3.45 floor.
func parseSQLiteVersion(v string) (major, minor int, err error) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("store: malformed sqlite_version() %q", v)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("store: malformed sqlite_version() %q: %w", v, err)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("store: malformed sqlite_version() %q: %w", v, err)
	}
	return major, minor, nil
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

// LogEvent appends one row to the append-only events ledger (HANDOFF §5
// safety #4). payload is JSON-marshaled here so every caller (W12-05's MCP
// gate/write/forget/injection ledgering, and any future caller) shares one
// marshaling/timestamp convention instead of hand-building the JSON string
// itself - mirroring how kahyad/internal/indexer.Indexer.Reindex already
// marshals its own ledger payload inline (via the SAME InsertEventWithDigest
// choke point - see its own doc comment). ts and created_at both use the
// current UTC time in RFC3339Nano, matching every other ledger writer in
// this codebase.
func (s *Store) LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("store: marshal event payload (kind=%s): %w", kind, err)
	}
	if _, err := InsertEventWithDigest(ctx, s.db, traceID, kind, b, time.Now()); err != nil {
		return err
	}
	return nil
}

// InsertEventWithDigest is THE single choke point (W4-05) that appends a
// row to the append-only events ledger: it inserts the row AND advances
// ledger_digest_state's running digest (kahyad/internal/ledgerdigest.Next -
// digest = SHA256(prev_digest || uint64_be(event_id) || event_payload_bytes),
// genesis prev_digest seeded as 32 zero bytes by migrations/
// 0010_ledger_anchor.sql) in the SAME SQLite transaction. Either both writes
// commit, or neither does - there is never an event without its digest
// step, and never a digest step without its event (task spec step 1's own
// correctness requirement). payload must be the EXACT bytes that get
// stored in the events.payload column (this function does not re-marshal
// it), since the digest must cover the ledger's actual on-disk bytes, not
// whatever in-memory value produced them.
//
// This is the ONLY function in this codebase that may execute
// "INSERT INTO events" - Store.LogEvent above (every ordinary ledger
// writer in this codebase - policy, tasks, egress, telegram, scheduler,
// backup, mcp/*, etc. - goes through LogEvent, never sqlcgen.InsertEvent
// directly) and kahyad/internal/indexer.Indexer's own reindex-summary
// ledger write (the ONE place in this codebase that used to call
// sqlcgen.Queries.InsertEvent directly, bypassing LogEvent, because it
// already held its own *sql.DB handle) both call this function instead.
// Grepping this repo for `InsertEvent(` or `INSERT INTO events` should
// never turn up a third call site outside this function and its own
// generated sqlcgen implementation.
func InsertEventWithDigest(ctx context.Context, db *sql.DB, traceID, kind string, payload []byte, now time.Time) (sqlcgen.Event, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return sqlcgen.Event{}, fmt.Errorf("store: begin ledger tx (kind=%s): %w", kind, err)
	}

	ts := now.UTC().Format(time.RFC3339Nano)
	txq := sqlcgen.New(tx)
	ev, err := txq.InsertEvent(ctx, sqlcgen.InsertEventParams{
		TraceID:   traceID,
		Ts:        ts,
		Kind:      kind,
		Payload:   string(payload),
		CreatedAt: ts,
	})
	if err != nil {
		_ = tx.Rollback()
		return sqlcgen.Event{}, fmt.Errorf("store: insert event (kind=%s): %w", kind, err)
	}

	state, err := txq.GetLedgerDigestState(ctx)
	if err != nil {
		_ = tx.Rollback()
		return sqlcgen.Event{}, fmt.Errorf("store: read ledger_digest_state (event=%d): %w", ev.ID, err)
	}

	next := ledgerdigest.Next(state.Digest, ev.ID, []byte(ev.Payload))
	if err := txq.AdvanceLedgerDigestState(ctx, sqlcgen.AdvanceLedgerDigestStateParams{
		LastEventID: ev.ID,
		Digest:      next[:],
	}); err != nil {
		_ = tx.Rollback()
		return sqlcgen.Event{}, fmt.Errorf("store: advance ledger_digest_state (event=%d): %w", ev.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return sqlcgen.Event{}, fmt.Errorf("store: commit ledger tx (kind=%s, event=%d): %w", kind, ev.ID, err)
	}
	return ev, nil
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
