// Package metrics computes the W78-04 MVP north-star and supporting metrics
// directly from the append-only events ledger (brain.db). Every function
// here is READ-ONLY: the Reader runs on a DEDICATED SQLite connection opened
// with PRAGMA query_only=ON, so a bug anywhere in this package can never
// write to brain.db (kahyad remains its sole writer - HANDOFF §5 #4, and
// the W78-04 gate: "add NO writers"). The `kahya metrics` CLI reaches these
// aggregates only over kahyad's UDS GET /metrics endpoint; it never opens
// brain.db itself (§4 locked decision: one db-access path).
//
// Metric -> event kind map (verified against the emitters in
// kahyad/internal/server/task.go, kahyad/internal/anthproxy/governor.go,
// and kahyad/internal/remembered/remembered.go):
//   - commands/day        = count(kind='task_spawned') grouped by UTC day
//   - clarification rate   = distinct task_spawned traces that ALSO have a
//     clarification-turn event / distinct task_spawned traces. Since W78-07
//     kahyad emits kind="clarification_turn" (server/task.go's
//     logClarificationTurn, driven by the worker's açıklama-turu stdout
//     signal) - so this metric now computes a real rate; it renders
//     veri-yok (nil) only when the ledger carries no clarification-turn
//     event at all (a brand-new/empty db), never as a permanent gap.
//   - palette->first-token p50 = MEDIAN of (first_token.ts - palette_open.ts)
//     per trace_id that carries both (earliest first_token if several).
//   - remembered moments   = count(kind='remembered_moment') in the window.
//   - cache-hit rate       = sum(cache_read_input_tokens) /
//     (sum(input_tokens) + sum(cache_read_input_tokens)) over model_call.
//   - daily spend          = sum(payload.usd) grouped by UTC day, plus a
//     window total, over model_call.
package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	// Blank-import the SQLite driver so metrics.OpenReadOnly can
	// sql.Open("sqlite3", ...) even when this package is exercised on its own
	// (its _test.go, `go test ./kahyad/internal/metrics/...`). The driver's
	// Register runs once process-wide, so co-existing with store's own import
	// is harmless.
	_ "github.com/mattn/go-sqlite3"
)

// Event-kind constants this package reads. These mirror the string literals /
// exported constants at the emission sites; they are duplicated here (rather
// than imported) to keep the read-only aggregation package free of the heavy
// server/anthproxy dependency graph. Any drift is caught by metrics_test.go's
// fixtures, which seed exactly these kinds.
const (
	// kindTaskSpawned is emitted once per task entry (server/task.go).
	kindTaskSpawned = "task_spawned"
	// kindPaletteOpen / kindFirstToken bracket the palette->first-token
	// latency, sharing one trace_id (server/task.go, W6-01/W6-04).
	kindPaletteOpen = "palette_open"
	kindFirstToken  = "first_token"
	// kindRememberedMoment is the user-marked "it remembered" ledger row
	// (remembered.EventRememberedMoment, W5-03). The distinct
	// "remembered_moment.duplicate" kind is deliberately NOT counted.
	kindRememberedMoment = "remembered_moment"
	// kindModelCall carries the per-call token/cost payload
	// (anthproxy.EventModelCall, W12-08).
	kindModelCall = "model_call"
)

// clarificationKinds are the event kinds that mark an "açıklama-turu" (a
// turn where the assistant asked the user a question before acting, HANDOFF
// §6 ⚑). As of W78-07 kahyad emits "clarification_turn" (server/task.go's
// logClarificationTurn); the other two are accepted as forward-compatible
// aliases so a future emitter/import path need not touch this reader. Any of
// them present makes ClarificationTurnRate compute a real ratio; none
// present (an empty/brand-new ledger) still renders veri-yok (nil), the same
// safe fallback as before the emitter existed.
var clarificationKinds = []string{"clarification", "clarification_turn", "acikla"}

// DayCount is one calendar day's command count (commands/day).
type DayCount struct {
	Day   string `json:"day"`   // UTC calendar day, "YYYY-MM-DD"
	Count int    `json:"count"` // task_spawned events on that day
}

// DaySpend is one calendar day's model spend in USD (daily spend).
type DaySpend struct {
	Day string  `json:"day"` // UTC calendar day, "YYYY-MM-DD"
	USD float64 `json:"usd"` // sum(payload.usd) for model_call on that day
}

// Metrics is the full result set over one [Since, Until] window. Pointer
// fields are nil exactly when the metric is veri-yok (data absent), which the
// CLI renders as "— (veri yok)" rather than a misleading 0.
type Metrics struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`

	CommandsPerDay []DayCount `json:"commands_per_day"`
	CommandsTotal  int        `json:"commands_total"`

	// ClarificationTurnRate is clarified-commands / total-commands in [0,1];
	// nil = veri-yok (no clarification-turn event kind is emitted yet, or no
	// commands in the window).
	ClarificationTurnRate *float64 `json:"clarification_turn_rate"`

	// PaletteToFirstTokenP50Ms is the median palette-open->first-token delta
	// in milliseconds; nil = veri-yok (no trace carries both events).
	PaletteToFirstTokenP50Ms *int64 `json:"palette_to_first_token_p50_ms"`

	RememberedMoments int `json:"remembered_moments"`

	// CacheHitRate is cache-read tokens / (input + cache-read) tokens in
	// [0,1]; nil = veri-yok (no model_call token accounting in the window).
	CacheHitRate *float64 `json:"cache_hit_rate"`

	DailySpendUSD      []DaySpend `json:"daily_spend_usd"`
	DailySpendTotalUSD float64    `json:"daily_spend_total_usd"`
}

// Reader computes metrics over a single read-only brain.db connection. Build
// one with New over a handle from OpenReadOnly; it is safe for concurrent use
// (every method only SELECTs).
type Reader struct {
	db *sql.DB
}

// New wraps an already-open read-only *sql.DB (see OpenReadOnly). The caller
// owns db's lifecycle (main.go closes it on shutdown).
func New(db *sql.DB) *Reader {
	return &Reader{db: db}
}

// OpenReadOnly opens a DEDICATED connection to the brain.db at dbPath with
// PRAGMA query_only=ON, so any write statement issued on it errors instead of
// mutating the ledger. A second read-only handle to the same WAL database is
// safe alongside kahyad's main read/write store handle. The pool is capped at
// one connection to mirror the store's single-connection posture.
func OpenReadOnly(dbPath string) (*sql.DB, error) {
	// _query_only=on -> PRAGMA query_only=ON on every pooled connection;
	// _busy_timeout backs off instead of failing under the writer's lock.
	db, err := sql.Open("sqlite3", dbPath+"?_query_only=on&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("metrics: open %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Compute runs every metric over the [since, until] window and returns the
// assembled result. since/until are compared against events.ts (UTC
// RFC3339Nano) via SQLite's julianday(), which parses the ISO-8601 'Z' form
// robustly (sqlite >= 3.45, asserted at store boot).
func (r *Reader) Compute(ctx context.Context, since, until time.Time) (Metrics, error) {
	m := Metrics{Since: since, Until: until}

	sinceS := since.UTC().Format(time.RFC3339Nano)
	untilS := until.UTC().Format(time.RFC3339Nano)

	perDay, total, err := r.commandsPerDay(ctx, sinceS, untilS)
	if err != nil {
		return Metrics{}, err
	}
	m.CommandsPerDay = perDay
	m.CommandsTotal = total

	rate, err := r.clarificationTurnRate(ctx, sinceS, untilS)
	if err != nil {
		return Metrics{}, err
	}
	m.ClarificationTurnRate = rate

	p50, err := r.paletteToFirstTokenP50Ms(ctx, sinceS, untilS)
	if err != nil {
		return Metrics{}, err
	}
	m.PaletteToFirstTokenP50Ms = p50

	remembered, err := r.rememberedMoments(ctx, sinceS, untilS)
	if err != nil {
		return Metrics{}, err
	}
	m.RememberedMoments = remembered

	cacheHit, err := r.cacheHitRate(ctx, sinceS, untilS)
	if err != nil {
		return Metrics{}, err
	}
	m.CacheHitRate = cacheHit

	spend, spendTotal, err := r.dailySpend(ctx, sinceS, untilS)
	if err != nil {
		return Metrics{}, err
	}
	m.DailySpendUSD = spend
	m.DailySpendTotalUSD = spendTotal

	return m, nil
}

// windowClause is the shared [since, until] predicate on events.ts. Uses
// julianday() so the comparison is numeric/timezone-aware, never a fragile
// RFC3339Nano string compare (fractional-second digits vary).
const windowClause = `julianday(ts) >= julianday(?) AND julianday(ts) <= julianday(?)`

// commandsPerDay counts task_spawned events grouped by the daemon-local calendar day, plus
// the window total.
func (r *Reader) commandsPerDay(ctx context.Context, since, until string) ([]DayCount, int, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT date(ts, 'localtime') AS day, count(*) AS c
		 FROM events
		 WHERE kind = ? AND `+windowClause+`
		 GROUP BY day ORDER BY day`,
		kindTaskSpawned, since, until)
	if err != nil {
		return nil, 0, fmt.Errorf("metrics: commands/day: %w", err)
	}
	defer rows.Close()

	var out []DayCount
	total := 0
	for rows.Next() {
		var d DayCount
		if err := rows.Scan(&d.Day, &d.Count); err != nil {
			return nil, 0, err
		}
		total += d.Count
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// clarificationTurnRate = distinct task_spawned traces that ALSO carry a
// clarification-turn event / distinct task_spawned traces. Returns nil
// (veri-yok) when there are no commands in the window OR when no
// clarification-turn event exists anywhere in the ledger (an empty/brand-new
// db - since W78-07 the kind IS emitted, so this is no longer the steady
// state).
func (r *Reader) clarificationTurnRate(ctx context.Context, since, until string) (*float64, error) {
	// Fast-out: if no clarification-turn event exists anywhere in the ledger,
	// the metric is not measurable -> veri-yok. This distinguishes an
	// empty/brand-new db from "measurable, zero clarifications this window".
	anyClar, err := r.anyClarificationEvent(ctx)
	if err != nil {
		return nil, err
	}
	if !anyClar {
		return nil, nil
	}

	totalTraces, err := r.distinctTraceCount(ctx, since, until, []string{kindTaskSpawned})
	if err != nil {
		return nil, err
	}
	if totalTraces == 0 {
		return nil, nil
	}

	// Clarified command traces: a task_spawned trace that also has a
	// clarification-turn event within the window.
	placeholders, args := inClause(clarificationKinds)
	q := `SELECT count(DISTINCT e.trace_id)
	      FROM events e
	      WHERE e.kind IN (` + placeholders + `) AND ` + windowClause + `
	        AND e.trace_id IN (
	          SELECT trace_id FROM events WHERE kind = ? AND ` + windowClause + `)`
	qargs := append([]any{}, args...)
	qargs = append(qargs, since, until, kindTaskSpawned, since, until)
	var clarified int
	if err := r.db.QueryRowContext(ctx, q, qargs...).Scan(&clarified); err != nil {
		return nil, fmt.Errorf("metrics: clarification rate: %w", err)
	}
	rate := float64(clarified) / float64(totalTraces)
	return &rate, nil
}

// anyClarificationEvent reports whether ANY clarification-turn event exists in
// the whole ledger (unwindowed) - the gap detector for clarificationTurnRate.
func (r *Reader) anyClarificationEvent(ctx context.Context) (bool, error) {
	placeholders, args := inClause(clarificationKinds)
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE kind IN (`+placeholders+`)`,
		args...).Scan(&n); err != nil {
		return false, fmt.Errorf("metrics: clarification presence: %w", err)
	}
	return n > 0, nil
}

// distinctTraceCount counts distinct trace_ids carrying any of kinds in the
// window.
func (r *Reader) distinctTraceCount(ctx context.Context, since, until string, kinds []string) (int, error) {
	placeholders, args := inClause(kinds)
	args = append(args, since, until)
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(DISTINCT trace_id) FROM events WHERE kind IN (`+placeholders+`) AND `+windowClause,
		args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("metrics: distinct trace count: %w", err)
	}
	return n, nil
}

// paletteToFirstTokenP50Ms returns the MEDIAN of per-trace
// (first_token.ts - palette_open.ts) deltas over the window, in milliseconds.
// A trace qualifies only if it carries BOTH events; the earliest ts of each
// kind is used. nil (veri-yok) when no trace carries both.
func (r *Reader) paletteToFirstTokenP50Ms(ctx context.Context, since, until string) (*int64, error) {
	// Per trace_id, the earliest palette_open and earliest first_token ts. An
	// INNER JOIN drops traces missing either kind. Delta is computed in Go
	// (parsing RFC3339Nano) rather than in SQL so the arithmetic is exact and
	// obviously the "earliest of each" the metric definition requires.
	rows, err := r.db.QueryContext(ctx,
		`SELECT p.trace_id, p.pmin, f.fmin FROM
		   (SELECT trace_id, min(ts) AS pmin FROM events
		      WHERE kind = ? AND `+windowClause+` GROUP BY trace_id) p
		 JOIN
		   (SELECT trace_id, min(ts) AS fmin FROM events
		      WHERE kind = ? AND `+windowClause+` GROUP BY trace_id) f
		 ON p.trace_id = f.trace_id`,
		kindPaletteOpen, since, until, kindFirstToken, since, until)
	if err != nil {
		return nil, fmt.Errorf("metrics: palette->first-token: %w", err)
	}
	defer rows.Close()

	var deltas []int64
	for rows.Next() {
		var traceID, pminS, fminS string
		if err := rows.Scan(&traceID, &pminS, &fminS); err != nil {
			return nil, err
		}
		pmin, err := time.Parse(time.RFC3339Nano, pminS)
		if err != nil {
			continue // unparseable ts: skip this trace, don't fail the metric
		}
		fmin, err := time.Parse(time.RFC3339Nano, fminS)
		if err != nil {
			continue
		}
		delta := fmin.Sub(pmin).Milliseconds()
		if delta < 0 {
			// first_token before palette_open is a clock/logging anomaly; the
			// metric definition treats the two as ordered by construction, so
			// clamp rather than emit a negative latency.
			delta = 0
		}
		deltas = append(deltas, delta)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(deltas) == 0 {
		return nil, nil
	}
	p50 := medianInt64(deltas)
	return &p50, nil
}

// medianInt64 returns the median of xs (mutates order via sort). For an even
// count it averages the two central values (integer floor) - this is the true
// median, distinct from the mean.
func medianInt64(xs []int64) int64 {
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

// rememberedMoments counts remembered_moment events in the window (the
// user-marked "it remembered" moments, W5-03).
func (r *Reader) rememberedMoments(ctx context.Context, since, until string) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE kind = ? AND `+windowClause,
		kindRememberedMoment, since, until).Scan(&n); err != nil {
		return 0, fmt.Errorf("metrics: remembered moments: %w", err)
	}
	return n, nil
}

// cacheHitRate = sum(cache_read_input_tokens) / (sum(input_tokens) +
// sum(cache_read_input_tokens)) over model_call events in the window - the
// fraction of input tokens served from the prompt cache. nil (veri-yok) when
// the denominator is 0 (no token accounting in the window).
func (r *Reader) cacheHitRate(ctx context.Context, since, until string) (*float64, error) {
	var input, cacheRead sql.NullFloat64
	if err := r.db.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(json_extract(payload, '$.input_tokens')), 0),
		   COALESCE(SUM(json_extract(payload, '$.cache_read_input_tokens')), 0)
		 FROM events WHERE kind = ? AND `+windowClause,
		kindModelCall, since, until).Scan(&input, &cacheRead); err != nil {
		return nil, fmt.Errorf("metrics: cache-hit rate: %w", err)
	}
	denom := input.Float64 + cacheRead.Float64
	if denom == 0 {
		return nil, nil
	}
	rate := cacheRead.Float64 / denom
	return &rate, nil
}

// dailySpend sums payload.usd over model_call events grouped by daemon-local calendar
// day, plus the window total.
func (r *Reader) dailySpend(ctx context.Context, since, until string) ([]DaySpend, float64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT date(ts, 'localtime') AS day, COALESCE(SUM(json_extract(payload, '$.usd')), 0) AS usd
		 FROM events
		 WHERE kind = ? AND `+windowClause+`
		 GROUP BY day ORDER BY day`,
		kindModelCall, since, until)
	if err != nil {
		return nil, 0, fmt.Errorf("metrics: daily spend: %w", err)
	}
	defer rows.Close()

	var out []DaySpend
	total := 0.0
	for rows.Next() {
		var d DaySpend
		if err := rows.Scan(&d.Day, &d.USD); err != nil {
			return nil, 0, err
		}
		total += d.USD
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// inClause builds a "?,?,?" placeholder list and matching []any args for a
// SQL IN (...) over string values.
func inClause(vals []string) (string, []any) {
	if len(vals) == 0 {
		return "NULL", nil
	}
	ph := make([]byte, 0, len(vals)*2)
	args := make([]any, len(vals))
	for i, v := range vals {
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
		args[i] = v
	}
	return string(ph), args
}
