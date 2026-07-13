// clarification_test.go covers W78-07's clarification_turn ledger event -
// the EMISSION side of the W78-04 north-star "açıklama-turu oranı" metric
// (HANDOFF §6 ⚑). It drives handleTask against a real worker process (the
// spawn package's fakes, same harness as task_test.go/palette_events_test.go)
// and asserts that:
//   - a worker that emits the non-terminal {"event":"clarification_turn"}
//     line makes kahyad ledger exactly one kind="clarification_turn" row
//     under the task's own trace_id, ALONGSIDE the task_spawned row the
//     metric divides by - and the task still completes with a normal "ok"
//     result (the signal is non-terminal);
//   - an ordinary worker that never emits that line writes NO
//     clarification_turn row (so the metric only ever counts real ones).
package server

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"kahya/kahyad/internal/metrics"
)

// TestClarificationTurnEventLedgered proves the worker's clarification
// signal becomes a real events row that both (a) shares the task's trace_id
// and (b) coexists with task_spawned, which together are exactly what
// metrics.clarificationTurnRate needs to compute a non-veri-yok rate.
func TestClarificationTurnEventLedgered(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "clarification_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-clarification-000000000001"
	resp := postTask(t, f.client, traceID, "faturayı öde")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The clarification signal is NON-terminal: the stream still ends with a
	// normal result frame, not an error.
	frames := readAllSSE(t, resp)
	if len(frames) == 0 {
		t.Fatal("no SSE frames received")
	}
	if last := frames[len(frames)-1]; last.event != "result" {
		t.Fatalf("terminal SSE event = %q, want %q (clarification is non-terminal)", last.event, "result")
	}

	assertLedgerHasKind(t, f.store, traceID, "task_spawned")
	assertLedgerHasKind(t, f.store, traceID, "clarification_turn")

	// Exactly one clarification_turn row (the worker emits the line once).
	var n int
	if err := f.store.DB().QueryRow(
		`SELECT count(*) FROM events WHERE trace_id = ? AND kind = 'clarification_turn'`, traceID,
	).Scan(&n); err != nil {
		t.Fatalf("count clarification_turn events: %v", err)
	}
	if n != 1 {
		t.Errorf("clarification_turn event count = %d, want 1", n)
	}
}

// TestNoClarificationSignalNoEvent proves an ordinary task whose worker
// never emits the clarification line writes NO clarification_turn row - the
// metric must only ever count turns the worker actually flagged.
func TestNoClarificationSignalNoEvent(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "echo_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-no-clarification-00000001"
	resp := postTask(t, f.client, traceID, "test sorusu")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	readAllSSE(t, resp)

	var n int
	if err := f.store.DB().QueryRow(
		`SELECT count(*) FROM events WHERE trace_id = ? AND kind = 'clarification_turn'`, traceID,
	).Scan(&n); err != nil {
		t.Fatalf("count clarification_turn events: %v", err)
	}
	if n != 0 {
		t.Errorf("clarification_turn event count = %d, want 0 (worker emitted no signal)", n)
	}
}

// TestClarificationTurnRateComputesFromEmittedEvent closes the loop end to
// end: one REAL clarified task (driven through handleTask, so its
// clarification_turn row is emitted by the production code path, not
// hand-seeded) plus one ordinary task_spawned trace in the SAME ledger make
// the read-only metrics reader (the W78-04 code the `kahya metrics` CLI
// serves, left deliberately unchanged by W78-07) compute a real rate of 1/2
// - not the veri-yok (nil) it returned when nothing emitted the event.
func TestClarificationTurnRateComputesFromEmittedEvent(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "clarification_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)
	ctx := context.Background()

	// One clarified command, emitted for real by the worker->kahyad path.
	resp := postTask(t, f.client, "trace-metric-clarified-0000001", "faturayı öde")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clarified task status = %d, want 200", resp.StatusCode)
	}
	readAllSSE(t, resp)

	// One ordinary command in the same ledger, seeded directly (a second
	// distinct task_spawned trace with NO clarification) - the metric's
	// denominator. Seeded rather than run through a second worker to keep
	// this test free of a mid-flight cfg.WorkerCmd swap (which the handler
	// goroutine reads concurrently).
	if err := f.store.LogEvent(ctx, "trace-metric-plain-000000001", "task_spawned", map[string]any{
		"task_id": "t_plain", "model": "claude-sonnet-5", "lane": "normal",
	}); err != nil {
		t.Fatalf("seed plain task_spawned: %v", err)
	}

	// A wall-clock window straddling now (events are stamped at write time).
	since := time.Now().Add(-time.Hour)
	until := time.Now().Add(time.Hour)
	m, err := metrics.New(f.store.DB()).Compute(ctx, since, until)
	if err != nil {
		t.Fatalf("metrics.Compute: %v", err)
	}
	if m.ClarificationTurnRate == nil {
		t.Fatal("ClarificationTurnRate = nil (veri-yok), want a real rate now that the event is emitted")
	}
	if got := *m.ClarificationTurnRate; got != 0.5 {
		t.Errorf("ClarificationTurnRate = %v, want 0.5 (1 clarified of 2 commands)", got)
	}
}
