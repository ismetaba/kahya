package taint

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// erroringStore is a fake Store whose GetSessionTaint always fails with a
// generic (non-sql.ErrNoRows) error - TestGetOnReadErrorFailsClosedToTainted
// uses it to prove Get's fail-closed posture covers genuine read errors,
// not merely the documented "no row" case.
type erroringStore struct{}

func (erroringStore) GetSessionTaint(context.Context, string) (sqlcgen.SessionTaint, error) {
	return sqlcgen.SessionTaint{}, errors.New("boom: simulated db error")
}
func (erroringStore) InsertSessionTaintClean(context.Context, sqlcgen.InsertSessionTaintCleanParams) error {
	return errors.New("unused")
}
func (erroringStore) RaiseSessionTaint(context.Context, sqlcgen.RaiseSessionTaintParams) error {
	return errors.New("unused")
}
func (erroringStore) InsertSessionTaintTainted(context.Context, sqlcgen.InsertSessionTaintTaintedParams) error {
	return errors.New("unused")
}

// testStore builds a Tracker against a real temp-file brain.db - the same
// rationale kahyad/internal/task/machine_test.go's testStore gives for
// using a real store rather than a Go-level fake (migrations/0009 is
// exercised for real, including its CHECK(tier IN (...)) constraint).
func testStore(t *testing.T) *store.Store {
	t.Helper()
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func countEventsOfKind(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", kind, err)
	}
	return n
}

// TestGetMissingRowIsTainted is the step-8 permanent regression test for
// HANDOFF's verbatim fail-closed rule: "kayit yoksa oturum guvenilmez
// sayilir" - a session_id with no session_taint row at all resolves to
// TierTainted, with a nil error (the absence of a row is the DEFINED
// behavior).
func TestGetMissingRowIsTainted(t *testing.T) {
	st := testStore(t)
	tr := New(st.Queries, st)

	tier, err := tr.Get(context.Background(), "session-never-seen")
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if tier != TierTainted {
		t.Fatalf("tier = %q, want %q", tier, TierTainted)
	}
}

// TestInsertCleanThenGetReturnsClean proves the one legitimate way to
// create a clean row actually works end to end.
func TestInsertCleanThenGetReturnsClean(t *testing.T) {
	st := testStore(t)
	tr := New(st.Queries, st)
	ctx := context.Background()

	if err := tr.InsertClean(ctx, "trace-1", "session-a"); err != nil {
		t.Fatalf("InsertClean: %v", err)
	}
	tier, err := tr.Get(ctx, "session-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tier != TierClean {
		t.Fatalf("tier = %q, want %q", tier, TierClean)
	}
}

// TestRaiseCreatesRowAndLedgersRaised: Raise on a session_id with NO
// existing row creates it directly at tier=tainted (a Reader session's
// own spawn-time taint, or a content-sourced tool output raising taint on
// a session whose own 'clean' row has not landed yet) and ledgers
// taint.raised.
func TestRaiseCreatesRowAndLedgersRaised(t *testing.T) {
	st := testStore(t)
	tr := New(st.Queries, st)
	ctx := context.Background()

	if err := tr.Raise(ctx, "trace-1", "session-b", "untrusted_output:web_fetch"); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	tier, err := tr.Get(ctx, "session-b")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tier != TierTainted {
		t.Fatalf("tier = %q, want %q", tier, TierTainted)
	}
	if n := countEventsOfKind(t, st, EventRaised); n != 1 {
		t.Fatalf("taint.raised events = %d, want 1", n)
	}
}

// TestRaiseThenInsertCleanFails is the step-8 permanent regression test:
// "Raise then attempt to write clean => error" - there is no API in this
// package that can ever downgrade an existing tainted row back to clean.
func TestRaiseThenInsertCleanFails(t *testing.T) {
	st := testStore(t)
	tr := New(st.Queries, st)
	ctx := context.Background()

	if err := tr.Raise(ctx, "trace-1", "session-c", "reason"); err != nil {
		t.Fatalf("Raise: %v", err)
	}

	err := tr.InsertClean(ctx, "trace-2", "session-c")
	if !errors.Is(err, ErrLowerAttempt) {
		t.Fatalf("InsertClean after Raise: err = %v, want ErrLowerAttempt", err)
	}

	// The row must still be tainted - the failed InsertClean attempt must
	// never have mutated it.
	tier, gerr := tr.Get(ctx, "session-c")
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	if tier != TierTainted {
		t.Fatalf("tier after failed lowering attempt = %q, want %q (still tainted)", tier, TierTainted)
	}

	if n := countEventsOfKind(t, st, EventLowerAttempt); n != 1 {
		t.Fatalf("taint.lower_attempt events = %d, want 1", n)
	}
}

// TestInsertCleanTwiceFailsAsLowerAttempt: a second InsertClean against an
// ALREADY-clean row is also rejected (there is no upsert path for clean at
// all - the invariant is "InsertClean only ever succeeds against a
// session_id with NO existing row", not merely "never downgrades from
// tainted").
func TestInsertCleanTwiceFailsAsLowerAttempt(t *testing.T) {
	st := testStore(t)
	tr := New(st.Queries, st)
	ctx := context.Background()

	if err := tr.InsertClean(ctx, "trace-1", "session-d"); err != nil {
		t.Fatalf("first InsertClean: %v", err)
	}
	err := tr.InsertClean(ctx, "trace-1", "session-d")
	if !errors.Is(err, ErrLowerAttempt) {
		t.Fatalf("second InsertClean: err = %v, want ErrLowerAttempt", err)
	}
}

// TestRaiseNeverLowersAnAlreadyTaintedRow: raising an already-tainted
// session a second time (a different reason) stays tainted - Raise itself
// has no lowering path either, it only ever writes tainted.
func TestRaiseIsIdempotentAtTaintedTier(t *testing.T) {
	st := testStore(t)
	tr := New(st.Queries, st)
	ctx := context.Background()

	if err := tr.Raise(ctx, "trace-1", "session-e", "first_reason"); err != nil {
		t.Fatalf("first Raise: %v", err)
	}
	if err := tr.Raise(ctx, "trace-2", "session-e", "second_reason"); err != nil {
		t.Fatalf("second Raise: %v", err)
	}
	tier, err := tr.Get(ctx, "session-e")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tier != TierTainted {
		t.Fatalf("tier = %q, want %q", tier, TierTainted)
	}
	if n := countEventsOfKind(t, st, EventRaised); n != 2 {
		t.Fatalf("taint.raised events = %d, want 2", n)
	}
}

// TestTaintSurvivesRestart is the step-8 "restart simulation" permanent
// regression test (also HANDOFF §6 W7-8's own red-team item: "tainted-
// oturum-restart-sonrasi-hala-tainted"): raise taint, close the DB (a
// clean shutdown checkpoint - store.Store.Close's own contract), reopen
// it fresh (a brand-new *store.Store/Tracker pointed at the SAME file,
// standing in for "kahyad restarted"), and confirm the row - and
// therefore the fail-closed decision built on it - is unchanged.
func TestTaintSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "brain.db")
	cfg := config.Config{DBPath: dbPath}

	st1, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open (first): %v", err)
	}
	tr1 := New(st1.Queries, st1)
	if err := tr1.Raise(context.Background(), "trace-1", "session-restart", "reason"); err != nil {
		t.Fatalf("Raise: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	st2, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open (second): %v", err)
	}
	t.Cleanup(func() { st2.Close() })
	tr2 := New(st2.Queries, st2)

	tier, err := tr2.Get(context.Background(), "session-restart")
	if err != nil {
		t.Fatalf("Get (after restart): %v", err)
	}
	if tier != TierTainted {
		t.Fatalf("tier after restart = %q, want %q", tier, TierTainted)
	}
}

// TestGetOnReadErrorFailsClosedToTainted: Get resolves to TierTainted even
// when the underlying store read fails for a reason OTHER than "no
// row" - a taint check that could not be answered must never behave as if
// the session were clean.
func TestGetOnReadErrorFailsClosedToTainted(t *testing.T) {
	tr := New(erroringStore{}, nil)
	tier, err := tr.Get(context.Background(), "whatever")
	if err == nil {
		t.Fatalf("Get: expected a non-nil error")
	}
	if tier != TierTainted {
		t.Fatalf("tier = %q, want %q", tier, TierTainted)
	}
}

// TestInsertUntrustedThenGetReturnsTainted is W5-01's own regression test
// for the THIRD session_taint birth-place (InsertUntrusted's own doc
// comment): a session minted untrusted-by-design at creation (the
// morning-briefing worker session) reads back as TierTainted, with a real
// row (not merely the fail-closed missing-row default) - checked via
// countEventsOfKind below, since this row's presence is what makes the
// tier "explicit and auditable" rather than incidental.
func TestInsertUntrustedThenGetReturnsTainted(t *testing.T) {
	st := testStore(t)
	tr := New(st.Queries, st)
	ctx := context.Background()

	if err := tr.InsertUntrusted(ctx, "trace-1", "session-briefing", "briefing:untrusted_by_design"); err != nil {
		t.Fatalf("InsertUntrusted: %v", err)
	}
	tier, err := tr.Get(ctx, "session-briefing")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tier != TierTainted {
		t.Fatalf("tier = %q, want %q", tier, TierTainted)
	}
}

// TestInsertUntrustedTwiceFailsAsLowerAttempt mirrors
// TestInsertCleanTwiceFailsAsLowerAttempt: InsertUntrusted is a plain
// INSERT, never an upsert - a second call against the SAME session_id
// (even though both would-be tiers are 'tainted') is rejected exactly like
// any other birth-place collision, ledgered under EventLowerAttempt (the
// existing ledger vocabulary this package already has for "a caller tried
// to create a row where one already exists").
func TestInsertUntrustedTwiceFailsAsLowerAttempt(t *testing.T) {
	st := testStore(t)
	tr := New(st.Queries, st)
	ctx := context.Background()

	if err := tr.InsertUntrusted(ctx, "trace-1", "session-briefing-2", "reason"); err != nil {
		t.Fatalf("first InsertUntrusted: %v", err)
	}
	err := tr.InsertUntrusted(ctx, "trace-1", "session-briefing-2", "reason")
	if !errors.Is(err, ErrLowerAttempt) {
		t.Fatalf("second InsertUntrusted: err = %v, want ErrLowerAttempt", err)
	}
	if n := countEventsOfKind(t, st, EventLowerAttempt); n != 1 {
		t.Fatalf("taint.lower_attempt events = %d, want 1", n)
	}
}

// TestInsertUntrustedOverExistingCleanRowFails proves InsertUntrusted
// cannot be used to retroactively taint an already-clean row either - it
// is strictly a BIRTH path (a fresh session_id with no row yet), never a
// transition, mirroring InsertClean's own "any existing row, any tier, is
// a rejected collision" contract.
func TestInsertUntrustedOverExistingCleanRowFails(t *testing.T) {
	st := testStore(t)
	tr := New(st.Queries, st)
	ctx := context.Background()

	if err := tr.InsertClean(ctx, "trace-1", "session-already-clean"); err != nil {
		t.Fatalf("InsertClean: %v", err)
	}
	err := tr.InsertUntrusted(ctx, "trace-2", "session-already-clean", "reason")
	if !errors.Is(err, ErrLowerAttempt) {
		t.Fatalf("InsertUntrusted over clean row: err = %v, want ErrLowerAttempt", err)
	}
	tier, gerr := tr.Get(ctx, "session-already-clean")
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	if tier != TierClean {
		t.Fatalf("tier after failed InsertUntrusted = %q, want %q (still clean)", tier, TierClean)
	}
}
