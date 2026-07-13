package remembered

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/store"
)

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

func testLogger(t *testing.T) *logx.Logger {
	t.Helper()
	log, err := logx.New(t.TempDir(), "boot0123456789abcdef0123456789ab")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

func countRememberedRows(t *testing.T, st *store.Store, traceID string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE trace_id = ? AND kind = 'remembered_moment'`, traceID).Scan(&n); err != nil {
		t.Fatalf("count remembered_moment rows: %v", err)
	}
	return n
}

// TestMarkUnknownTraceFailsClosed: a trace_id with no events row at all
// (no task/ritual run ever existed under it) is rejected - ErrUnknownTrace,
// zero rows inserted.
func TestMarkUnknownTraceFailsClosed(t *testing.T) {
	st := testStore(t)
	m := New(st.Queries, st, testLogger(t))

	dup, err := m.Mark(context.Background(), "no-such-trace-00000000000000000", "local")
	if !errors.Is(err, ErrUnknownTrace) {
		t.Fatalf("Mark() err = %v, want ErrUnknownTrace", err)
	}
	if dup {
		t.Fatal("Mark() duplicate = true on an unknown trace, want false")
	}
	if n := countRememberedRows(t, st, "no-such-trace-00000000000000000"); n != 0 {
		t.Fatalf("remembered_moment rows = %d, want 0", n)
	}
}

// TestMarkEmptyTraceRejected: a blank trace_id is rejected locally,
// before any DB read.
func TestMarkEmptyTraceRejected(t *testing.T) {
	st := testStore(t)
	m := New(st.Queries, st, testLogger(t))

	if _, err := m.Mark(context.Background(), "   ", "local"); !errors.Is(err, ErrEmptyTrace) {
		t.Fatalf("Mark() err = %v, want ErrEmptyTrace", err)
	}
}

// TestMarkKnownTraceInsertsExactlyOneRowThenIdempotent is the core W5-03
// acceptance criterion: a real trace (an existing events row under it)
// marks cleanly once; a second Mark for the SAME trace_id inserts nothing
// more (duplicate=true, no error) - exactly one remembered_moment row
// ever exists for that trace_id, enforced by the partial UNIQUE index
// (migrations/0013_eval_labels.sql), not merely an application-level
// check.
func TestMarkKnownTraceInsertsExactlyOneRowThenIdempotent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	traceID := "abcd0000000000000000000000000001"

	// Seed: some ordinary event under this trace_id (a completed task, in
	// production) - Mark's own existence check reads this.
	if err := st.LogEvent(ctx, traceID, "task_done", map[string]any{}); err != nil {
		t.Fatalf("seed LogEvent: %v", err)
	}

	m := New(st.Queries, st, testLogger(t))

	dup1, err := m.Mark(ctx, traceID, "remote")
	if err != nil {
		t.Fatalf("first Mark() error = %v", err)
	}
	if dup1 {
		t.Fatal("first Mark() duplicate = true, want false")
	}
	if n := countRememberedRows(t, st, traceID); n != 1 {
		t.Fatalf("remembered_moment rows after first Mark = %d, want 1", n)
	}

	dup2, err := m.Mark(ctx, traceID, "remote")
	if err != nil {
		t.Fatalf("second Mark() error = %v", err)
	}
	if !dup2 {
		t.Fatal("second Mark() duplicate = false, want true")
	}
	if n := countRememberedRows(t, st, traceID); n != 1 {
		t.Fatalf("remembered_moment rows after second Mark = %d, want still 1", n)
	}
}

// TestMarkChannelLocalVsRemote: the marked row's payload carries whichever
// channel the caller passed (CLI="local", Telegram="remote") - the W5-03
// deliverable's own "CLI marks channel=local, Telegram marks channel=remote"
// requirement.
func TestMarkChannelLocalVsRemote(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	traceID := "abcd0000000000000000000000000002"
	if err := st.LogEvent(ctx, traceID, "task_done", map[string]any{}); err != nil {
		t.Fatalf("seed LogEvent: %v", err)
	}

	m := New(st.Queries, st, testLogger(t))
	if _, err := m.Mark(ctx, traceID, "local"); err != nil {
		t.Fatalf("Mark() error = %v", err)
	}

	var payload string
	if err := st.DB().QueryRow(`SELECT payload FROM events WHERE trace_id = ? AND kind = 'remembered_moment'`, traceID).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !contains(payload, `"channel":"local"`) {
		t.Fatalf("payload = %s, want it to carry channel=local", payload)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// TestFiveMarkedMomentsPerWeekCountQuery is the S9 "haftada >=5 hatirladi
// ani" gate's own measurability criterion: a fixture week containing 5
// marked moments counts as 5 via the exact query W78-04 uses.
func TestFiveMarkedMomentsPerWeekCountQuery(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	m := New(st.Queries, st, testLogger(t))

	traces := []string{
		"1111000000000000000000000000000a",
		"1111000000000000000000000000000b",
		"1111000000000000000000000000000c",
		"1111000000000000000000000000000d",
		"1111000000000000000000000000000e",
	}
	for _, tr := range traces {
		if err := st.LogEvent(ctx, tr, "task_done", map[string]any{}); err != nil {
			t.Fatalf("seed LogEvent(%s): %v", tr, err)
		}
		if _, err := m.Mark(ctx, tr, "remote"); err != nil {
			t.Fatalf("Mark(%s): %v", tr, err)
		}
	}

	var n int
	weekStart := "2000-01-01T00:00:00Z" // every fixture row is well after this
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind='remembered_moment' AND ts >= ?`, weekStart).Scan(&n); err != nil {
		t.Fatalf("W78-04 count query: %v", err)
	}
	if n != 5 {
		t.Fatalf("remembered_moment count = %d, want 5", n)
	}
}
