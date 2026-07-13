package metrics

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/store"
)

// windowNow is the fixed [since, until] upper bound every test computes
// against, so calendar-day grouping and the 14-day default window are fully
// deterministic (never dependent on the wall clock).
var windowNow = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

func windowSince() time.Time { return windowNow.Add(-14 * 24 * time.Hour) }

// testEnv opens a fresh brain.db via the real store (schema + digest), returns
// its path plus the open store for seeding.
func testEnv(t *testing.T) (string, *store.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "brain.db")
	st, err := store.Open(config.Config{DBPath: dbPath})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return dbPath, st
}

// seed appends one event at a specific ts through the sole ledger writer
// (store.InsertEventWithDigest), so tests control calendar days and latency
// deltas exactly.
func seed(t *testing.T, st *store.Store, traceID, kind string, ts time.Time, payload string) {
	t.Helper()
	if _, err := store.InsertEventWithDigest(context.Background(), st.DB(), traceID, kind, []byte(payload), ts); err != nil {
		t.Fatalf("seed %s: %v", kind, err)
	}
}

// newReader opens the dedicated query_only handle and wraps it in a Reader,
// closing the handle at test end.
func newReader(t *testing.T, dbPath string) *Reader {
	t.Helper()
	db, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

func compute(t *testing.T, r *Reader) Metrics {
	t.Helper()
	m, err := r.Compute(context.Background(), windowSince(), windowNow)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return m
}

// TestCommandsPerDay proves task_spawned counts group per UTC calendar day and
// sum to the window total.
func TestCommandsPerDay(t *testing.T) {
	t.Setenv("TZ", "UTC") // deterministic local-day bucketing regardless of the runner's timezone
	dbPath, st := testEnv(t)
	day1 := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 11, 15, 0, 0, 0, time.UTC)
	seed(t, st, "t1", kindTaskSpawned, day1, `{"task_id":"a"}`)
	seed(t, st, "t2", kindTaskSpawned, day1.Add(2*time.Hour), `{"task_id":"b"}`)
	seed(t, st, "t3", kindTaskSpawned, day2, `{"task_id":"c"}`)
	// An out-of-window event (older than 14d) must NOT be counted.
	seed(t, st, "t0", kindTaskSpawned, windowSince().Add(-48*time.Hour), `{"task_id":"old"}`)

	m := compute(t, newReader(t, dbPath))
	if m.CommandsTotal != 3 {
		t.Fatalf("CommandsTotal = %d, want 3", m.CommandsTotal)
	}
	want := map[string]int{"2026-07-10": 2, "2026-07-11": 1}
	got := map[string]int{}
	for _, d := range m.CommandsPerDay {
		got[d.Day] = d.Count
	}
	for day, c := range want {
		if got[day] != c {
			t.Errorf("commands on %s = %d, want %d", day, got[day], c)
		}
	}
	if len(m.CommandsPerDay) != 2 {
		t.Errorf("CommandsPerDay len = %d, want 2 (%v)", len(m.CommandsPerDay), m.CommandsPerDay)
	}
}

// TestPaletteToFirstTokenP50IsMedian proves the p50 is the MEDIAN of the
// per-trace deltas (not the mean), for both an odd and an even count.
func TestPaletteToFirstTokenP50IsMedian(t *testing.T) {
	base := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

	// Odd count: deltas 100, 200, 300 ms -> median 200 (== mean here, still a
	// valid median check).
	t.Run("odd", func(t *testing.T) {
		dbPath, st := testEnv(t)
		deltas := []int64{100, 300, 200}
		for i, d := range deltas {
			trace := string(rune('a' + i))
			seed(t, st, trace, kindPaletteOpen, base, `{}`)
			seed(t, st, trace, kindFirstToken, base.Add(time.Duration(d)*time.Millisecond), `{}`)
		}
		m := compute(t, newReader(t, dbPath))
		if m.PaletteToFirstTokenP50Ms == nil || *m.PaletteToFirstTokenP50Ms != 200 {
			t.Fatalf("p50 = %v, want 200", m.PaletteToFirstTokenP50Ms)
		}
	})

	// Even count: deltas 100, 200, 300, 1000 -> median (200+300)/2 = 250,
	// which is DISTINCT from the mean (400). Proves median, not mean.
	t.Run("even", func(t *testing.T) {
		dbPath, st := testEnv(t)
		deltas := []int64{100, 200, 300, 1000}
		for i, d := range deltas {
			trace := string(rune('a' + i))
			seed(t, st, trace, kindPaletteOpen, base, `{}`)
			seed(t, st, trace, kindFirstToken, base.Add(time.Duration(d)*time.Millisecond), `{}`)
		}
		m := compute(t, newReader(t, dbPath))
		if m.PaletteToFirstTokenP50Ms == nil || *m.PaletteToFirstTokenP50Ms != 250 {
			t.Fatalf("p50 = %v, want 250 (median, not mean 400)", m.PaletteToFirstTokenP50Ms)
		}
	})

	// Multiple first_token rows for one trace: the EARLIEST is used.
	t.Run("earliest_first_token", func(t *testing.T) {
		dbPath, st := testEnv(t)
		seed(t, st, "x", kindPaletteOpen, base, `{}`)
		seed(t, st, "x", kindFirstToken, base.Add(500*time.Millisecond), `{}`)
		seed(t, st, "x", kindFirstToken, base.Add(900*time.Millisecond), `{}`)
		m := compute(t, newReader(t, dbPath))
		if m.PaletteToFirstTokenP50Ms == nil || *m.PaletteToFirstTokenP50Ms != 500 {
			t.Fatalf("p50 = %v, want 500 (earliest first_token)", m.PaletteToFirstTokenP50Ms)
		}
	})
}

// TestRememberedMoments proves remembered_moment events are counted and the
// distinct duplicate kind is excluded.
func TestRememberedMoments(t *testing.T) {
	dbPath, st := testEnv(t)
	base := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	seed(t, st, "r1", kindRememberedMoment, base, `{"channel":"cli"}`)
	seed(t, st, "r2", kindRememberedMoment, base.Add(time.Hour), `{"channel":"telegram"}`)
	seed(t, st, "r3", "remembered_moment.duplicate", base.Add(2*time.Hour), `{}`)
	m := compute(t, newReader(t, dbPath))
	if m.RememberedMoments != 2 {
		t.Fatalf("RememberedMoments = %d, want 2 (duplicate excluded)", m.RememberedMoments)
	}
}

// TestCacheHitRate proves the rate = cache_read / (input + cache_read).
func TestCacheHitRate(t *testing.T) {
	dbPath, st := testEnv(t)
	base := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	seed(t, st, "m1", kindModelCall, base, `{"input_tokens":100,"cache_read_input_tokens":100,"usd":0.5}`)
	seed(t, st, "m2", kindModelCall, base.Add(time.Hour), `{"input_tokens":300,"cache_read_input_tokens":100,"usd":1.5}`)
	m := compute(t, newReader(t, dbPath))
	if m.CacheHitRate == nil {
		t.Fatalf("CacheHitRate nil, want ~0.3333")
	}
	// sum cache_read = 200; sum input = 400; 200 / (400+200) = 0.3333...
	if got := *m.CacheHitRate; got < 0.3333 || got > 0.3334 {
		t.Fatalf("CacheHitRate = %v, want ~0.3333", got)
	}
}

// TestDailySpend proves usd is summed per UTC day and across the window.
func TestDailySpend(t *testing.T) {
	t.Setenv("TZ", "UTC") // deterministic local-day bucketing regardless of the runner's timezone
	dbPath, st := testEnv(t)
	day1 := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	seed(t, st, "m1", kindModelCall, day1, `{"usd":1.5}`)
	seed(t, st, "m2", kindModelCall, day1.Add(time.Hour), `{"usd":0.5}`)
	seed(t, st, "m3", kindModelCall, day2, `{"usd":3.0}`)
	m := compute(t, newReader(t, dbPath))

	if m.DailySpendTotalUSD < 4.999 || m.DailySpendTotalUSD > 5.001 {
		t.Fatalf("DailySpendTotalUSD = %v, want 5.00", m.DailySpendTotalUSD)
	}
	got := map[string]float64{}
	for _, d := range m.DailySpendUSD {
		got[d.Day] = d.USD
	}
	if got["2026-07-10"] < 1.999 || got["2026-07-10"] > 2.001 {
		t.Errorf("2026-07-10 spend = %v, want 2.00", got["2026-07-10"])
	}
	if got["2026-07-11"] < 2.999 || got["2026-07-11"] > 3.001 {
		t.Errorf("2026-07-11 spend = %v, want 3.00", got["2026-07-11"])
	}
}

// TestClarificationTurnRateVeriYok proves the metric is nil (veri-yok) when no
// clarification-turn event kind is emitted - the current, grep-confirmed
// state - even with commands present.
func TestClarificationTurnRateVeriYok(t *testing.T) {
	dbPath, st := testEnv(t)
	base := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	seed(t, st, "t1", kindTaskSpawned, base, `{}`)
	seed(t, st, "t2", kindTaskSpawned, base.Add(time.Hour), `{}`)
	m := compute(t, newReader(t, dbPath))
	if m.ClarificationTurnRate != nil {
		t.Fatalf("ClarificationTurnRate = %v, want nil (veri-yok: kind not emitted)", *m.ClarificationTurnRate)
	}
}

// TestClarificationTurnRateComputes proves that IF a clarification-turn event
// kind is present, the ratio computes as clarified-command-traces / total.
func TestClarificationTurnRateComputes(t *testing.T) {
	dbPath, st := testEnv(t)
	base := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	// Two command traces; trace "a" also has a clarification event.
	seed(t, st, "a", kindTaskSpawned, base, `{}`)
	seed(t, st, "b", kindTaskSpawned, base.Add(time.Hour), `{}`)
	seed(t, st, "a", "clarification", base.Add(time.Minute), `{}`)
	m := compute(t, newReader(t, dbPath))
	if m.ClarificationTurnRate == nil {
		t.Fatalf("ClarificationTurnRate nil, want 0.5")
	}
	if got := *m.ClarificationTurnRate; got < 0.4999 || got > 0.5001 {
		t.Fatalf("ClarificationTurnRate = %v, want 0.5", got)
	}
}

// TestVeriYokOnEmpty proves every nullable metric is nil and the counts are
// zero on an empty ledger (graceful, no error).
func TestVeriYokOnEmpty(t *testing.T) {
	dbPath, _ := testEnv(t)
	m := compute(t, newReader(t, dbPath))
	if m.ClarificationTurnRate != nil {
		t.Errorf("ClarificationTurnRate = %v, want nil", *m.ClarificationTurnRate)
	}
	if m.PaletteToFirstTokenP50Ms != nil {
		t.Errorf("PaletteToFirstTokenP50Ms = %v, want nil", *m.PaletteToFirstTokenP50Ms)
	}
	if m.CacheHitRate != nil {
		t.Errorf("CacheHitRate = %v, want nil", *m.CacheHitRate)
	}
	if m.CommandsTotal != 0 || len(m.CommandsPerDay) != 0 {
		t.Errorf("commands = %d/%v, want 0/empty", m.CommandsTotal, m.CommandsPerDay)
	}
	if m.RememberedMoments != 0 {
		t.Errorf("RememberedMoments = %d, want 0", m.RememberedMoments)
	}
	if m.DailySpendTotalUSD != 0 {
		t.Errorf("DailySpendTotalUSD = %v, want 0", m.DailySpendTotalUSD)
	}
}

// TestQueryOnlyRejectsWrite proves the metrics connection is read-only: an
// INSERT errors, and a full Compute leaves the events row count unchanged
// (zero writes).
func TestQueryOnlyRejectsWrite(t *testing.T) {
	dbPath, st := testEnv(t)
	base := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	seed(t, st, "t1", kindTaskSpawned, base, `{}`)
	seed(t, st, "m1", kindModelCall, base, `{"input_tokens":10,"cache_read_input_tokens":5,"usd":0.1}`)

	db, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer db.Close()

	// A direct INSERT on the query_only handle must error.
	if _, err := db.Exec(
		`INSERT INTO events (trace_id, ts, kind, payload, created_at) VALUES ('x','2026-07-12T10:00:00Z','k','{}','2026-07-12T10:00:00Z')`,
	); err == nil {
		t.Fatal("INSERT on query_only handle succeeded, want error")
	}

	before := eventCount(t, db)
	r := New(db)
	if _, err := r.Compute(context.Background(), windowSince(), windowNow); err != nil {
		t.Fatalf("Compute: %v", err)
	}
	after := eventCount(t, db)
	if before != after {
		t.Fatalf("events row count changed by Compute: before=%d after=%d", before, after)
	}
}

func eventCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}
