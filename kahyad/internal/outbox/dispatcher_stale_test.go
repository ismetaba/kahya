package outbox

import (
	"context"
	"testing"
	"time"

	"kahya/kahyad/internal/task"
)

// TestStaleRowBeforeNextRetryAtIsDroppedNotDispatched (FIX-I-8): a parked
// task's next attempt belongs to the row CloudRetry.park enqueued at
// next_retry_at. Any other due row for it - a resume-scan or `resolve
// --retry` row written before the park, a row stuck behind a blocked
// dispatcher - used to move it back to 'executing' and spawn it at once,
// bypassing the 1m/5m/15m/60m schedule. The stale row is now dropped
// (acked, outbox.stale_row_dropped) and the task stays parked.
func TestStaleRowBeforeNextRetryAtIsDroppedNotDispatched(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTaskWithEnvelope(t, st, "t1", "sess-1") // executing, attempts=1
	m := task.NewMachine(st.Queries, st)
	cr := task.NewCloudRetry(st.Queries, st.Queries, m, st, nil, []time.Duration{time.Hour}, 30*24*time.Hour)
	if err := cr.ParkOrGiveUp(ctx, "trace-t1", "t1"); err != nil { // the park's own row, at +1h
		t.Fatal(err)
	}
	stale := enqueueResumeRow(t, st, "t1") // available NOW: a stale resume-scan row
	marker := t.TempDir() + "/spawned"
	d := newTestDispatcher(st, m, markerWorker(marker))

	claimed, err := d.ClaimAndDispatch(ctx)
	if err != nil || claimed != 1 {
		t.Fatalf("ClaimAndDispatch() = %d, %v; want 1, nil", claimed, err)
	}
	assertNoWorkerSpawned(t, marker)
	if got := taskStatusOfStore(t, st, "t1"); got != task.StatusRetryWait {
		t.Fatalf("status = %q, want %q (backoff bypassed)", got, task.StatusRetryWait)
	}
	if !rowAcked(t, st, stale.ID) {
		t.Error("stale row must be acked (dropped), not left for re-claim")
	}
	if n := countEvents(t, st, EventStaleRowDropped); n != 1 {
		t.Errorf("%s rows = %d, want 1", EventStaleRowDropped, n)
	}
	// The park's own row is untouched: still pending, still owning the
	// next attempt.
	if n := countRows(t, st, `SELECT count(*) FROM outbox WHERE task_id = 't1' AND dispatched_at IS NULL AND canceled_at IS NULL`); n != 1 {
		t.Errorf("pending rows = %d, want 1 (the park's row at next_retry_at)", n)
	}
}
