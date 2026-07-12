package factengine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/taint"
)

// newTestStore builds a *store.Store against a real temp-file brain.db -
// migrations/0001+0012's real CHECK constraints and column types are
// exercised for real (matching kahyad/internal/taint/taint_test.go's own
// testStore rationale), rather than hand-faking every sqlc query's SQL
// semantics (NULL != NULL, LIMIT 1 tie-breaking, ...) in a Go-level fake.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newTestEngine builds an Engine wired to a real temp store, a real
// taint.Tracker (over the SAME store), and the store itself as Ledger -
// every ledger event this package writes lands in that store's real
// events table, so tests can assert on it directly.
func newTestEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	tracker := taint.New(st.Queries, st)
	e := New(st.Queries, tracker, st)
	return e, st
}

// insertCleanSession marks sessionID clean in session_taint (the ONLY way
// a real caller's ProvenanceUserAsserted candidate ever survives the
// taint gate) via the real taint.Tracker, exactly the path
// kahyad/internal/server's OnSession callback and kahyad/internal/reader's
// actor-seed Spawn use in production.
func insertCleanSession(t *testing.T, st *store.Store, sessionID string) {
	t.Helper()
	tracker := taint.New(st.Queries, st)
	if err := tracker.InsertClean(context.Background(), "test-trace", sessionID); err != nil {
		t.Fatalf("insertCleanSession(%s): %v", sessionID, err)
	}
}

// insertTaintedSession marks sessionID tainted (kahyad/internal/taint.
// Tracker.Raise) - used by tests proving a candidate from an untrusted
// session never becomes user_asserted.
func insertTaintedSession(t *testing.T, st *store.Store, sessionID, reason string) {
	t.Helper()
	tracker := taint.New(st.Queries, st)
	if err := tracker.Raise(context.Background(), "test-trace", sessionID, reason); err != nil {
		t.Fatalf("insertTaintedSession(%s): %v", sessionID, err)
	}
}

func countEventsOfKind(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", kind, err)
	}
	return n
}

func countMergeLedgerRows(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM merge_ledger`).Scan(&n); err != nil {
		t.Fatalf("count merge_ledger rows: %v", err)
	}
	return n
}

func countEvidenceRows(t *testing.T, st *store.Store, factID int64) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM evidence WHERE fact_id = ?`, factID).Scan(&n); err != nil {
		t.Fatalf("count evidence rows for fact %d: %v", factID, err)
	}
	return n
}

// fixedClock pins Engine.now to a stable instant so RFC3339Nano
// timestamps in tests are deterministic.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}
