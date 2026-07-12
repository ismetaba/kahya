package store

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"kahya/kahyad/internal/ledgerdigest"
)

// TestGenesisDigestSeededByMigration proves migrations/0010_ledger_anchor.sql
// seeds the single ledger_digest_state row exactly as the task spec
// requires: id=1, last_event_id=0, digest=32 zero bytes (ledgerdigest.
// Genesis()) - the fixed starting point every fresh brain.db's first
// ledger append chains from.
func TestGenesisDigestSeededByMigration(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	state, err := s.Queries.GetLedgerDigestState(context.Background())
	if err != nil {
		t.Fatalf("GetLedgerDigestState: %v", err)
	}
	if state.LastEventID != 0 {
		t.Errorf("genesis last_event_id = %d, want 0", state.LastEventID)
	}
	if len(state.Digest) != ledgerdigest.Size {
		t.Fatalf("genesis digest length = %d, want %d", len(state.Digest), ledgerdigest.Size)
	}
	for i, b := range state.Digest {
		if b != 0 {
			t.Fatalf("genesis digest[%d] = %d, want 0", i, b)
		}
	}
}

// TestLogEventAdvancesDigestInSameTransaction is the W4-05 correctness
// requirement's own regression test: every store.Store.LogEvent call must
// advance ledger_digest_state to
// (last_event_id=<new event id>, digest=ledgerdigest.Next(prev, id,
// <exact stored payload bytes>)) - computed independently here against the
// SAME payload bytes LogEvent itself stores, so a future accidental change
// to what gets hashed (e.g. hashing the caller's map instead of the
// on-disk JSON string) fails this test.
func TestLogEventAdvancesDigestInSameTransaction(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	if err := s.LogEvent(ctx, "trace-1", "test.one", map[string]any{"n": float64(1)}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	rows, err := s.Queries.ListAllEvents(ctx)
	if err != nil {
		t.Fatalf("ListAllEvents: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(rows))
	}
	ev := rows[0]

	wantDigest := ledgerdigest.Next(ledgerdigest.Genesis(), ev.ID, []byte(ev.Payload))

	state, err := s.Queries.GetLedgerDigestState(ctx)
	if err != nil {
		t.Fatalf("GetLedgerDigestState: %v", err)
	}
	if state.LastEventID != ev.ID {
		t.Errorf("last_event_id = %d, want %d", state.LastEventID, ev.ID)
	}
	if hex.EncodeToString(state.Digest) != hex.EncodeToString(wantDigest[:]) {
		t.Errorf("digest = %x, want %x", state.Digest, wantDigest)
	}
}

// TestLogEventDigestChainsAcrossMultipleEvents proves the running digest
// keeps chaining correctly across many LogEvent calls (not just the very
// first one) - each step's prev_digest is the PREVIOUS step's stored
// digest, not genesis every time.
func TestLogEventDigestChainsAcrossMultipleEvents(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	kinds := []string{"test.a", "test.b", "test.c"}
	for i, k := range kinds {
		if err := s.LogEvent(ctx, "trace-chain", k, map[string]any{"i": float64(i)}); err != nil {
			t.Fatalf("LogEvent(%s): %v", k, err)
		}
	}

	rows, err := s.Queries.ListAllEvents(ctx)
	if err != nil {
		t.Fatalf("ListAllEvents: %v", err)
	}
	if len(rows) != len(kinds) {
		t.Fatalf("len(events) = %d, want %d", len(rows), len(kinds))
	}

	digest := ledgerdigest.Genesis()
	for _, ev := range rows {
		next := ledgerdigest.Next(digest, ev.ID, []byte(ev.Payload))
		digest = next[:]
	}

	state, err := s.Queries.GetLedgerDigestState(ctx)
	if err != nil {
		t.Fatalf("GetLedgerDigestState: %v", err)
	}
	if state.LastEventID != rows[len(rows)-1].ID {
		t.Errorf("last_event_id = %d, want %d", state.LastEventID, rows[len(rows)-1].ID)
	}
	if hex.EncodeToString(state.Digest) != hex.EncodeToString(digest) {
		t.Errorf("final digest = %x, want %x", state.Digest, digest)
	}
}

// TestInsertEventWithDigestRollsBackEventOnDigestFailure is the "never an
// event without its digest step" half of the task spec's own correctness
// requirement: if the digest-state half of the transaction cannot proceed
// (simulated here by dropping ledger_digest_state out from under a
// perfectly healthy events table), the events INSERT must roll back too -
// there must be no way to observe a ledger row with no corresponding
// digest advance.
func TestInsertEventWithDigestRollsBackEventOnDigestFailure(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	if _, err := s.DB().ExecContext(ctx, `DROP TABLE ledger_digest_state`); err != nil {
		t.Fatalf("drop ledger_digest_state (test setup): %v", err)
	}

	if _, err := InsertEventWithDigest(ctx, s.DB(), "trace-fail", "test.willfail", []byte(`{}`), time.Now()); err == nil {
		t.Fatal("InsertEventWithDigest() error = nil, want an error once ledger_digest_state is unreachable")
	}

	rows, err := s.Queries.ListAllEvents(ctx)
	if err != nil {
		t.Fatalf("ListAllEvents: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("events after a failed InsertEventWithDigest = %d, want 0 (the INSERT must have rolled back)", len(rows))
	}
}

// TestLogEventRegressionManyCallersStillPass is a broad smoke test
// standing in for "the many existing store.LogEvent callers still pass"
// (task spec's own regression note): a handful of ledger writes with
// different kinds/payload shapes, back to back, must all succeed and each
// advance last_event_id by exactly one.
func TestLogEventRegressionManyCallersStillPass(t *testing.T) {
	s, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	payloads := []map[string]any{
		{"kind": "policy_decision", "allow": true},
		{"kind": "hafiza_injected", "chars": float64(42)},
		{}, // an empty payload must still marshal/insert cleanly
		{"nested": map[string]any{"a": float64(1), "b": "two"}},
	}
	for i, p := range payloads {
		if err := s.LogEvent(ctx, "trace-regress", "regress.kind", p); err != nil {
			t.Fatalf("LogEvent[%d]: %v", i, err)
		}
	}

	state, err := s.Queries.GetLedgerDigestState(ctx)
	if err != nil {
		t.Fatalf("GetLedgerDigestState: %v", err)
	}
	if state.LastEventID != int64(len(payloads)) {
		t.Errorf("last_event_id = %d, want %d", state.LastEventID, len(payloads))
	}
}
