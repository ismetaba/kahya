package task

import (
	"context"
	"testing"
	"time"
)

// TestFailNonRetryableSkipsTaskNoLongerExecuting (FIX-I-6 step 4): a
// NonRetryable callback arriving after the task already left 'executing'
// (a halt won the race, a callback already failed it) must not attempt an
// illegal transition, must not notify a second time, and leaves the status
// alone. One task.cloud_failure_skipped row records it.
func TestFailNonRetryableSkipsTaskNoLongerExecuting(t *testing.T) {
	ctx := context.Background()
	for _, terminal := range []string{StatusUserHalted, StatusFailed} {
		t.Run(terminal, func(t *testing.T) {
			st, m, cr, notifier := newCloudRetryFixture(t, nil, 0)
			insertTask(t, st, "t1")
			transitionToExecuting(t, m, "trace-t1", "t1")
			mustTransition(t, m, ctx, "t1", terminal)

			if err := cr.FailNonRetryable(ctx, "trace-t1", "t1", "transport_error"); err != nil {
				t.Fatalf("FailNonRetryable: %v", err)
			}
			if got := taskStatus(t, st, "t1"); got != terminal {
				t.Errorf("status = %q, want %q (left untouched)", got, terminal)
			}
			if n := countEventsOfKind(t, st, EventIllegalTransition); n != 0 {
				t.Errorf("%s rows = %d, want 0", EventIllegalTransition, n)
			}
			if n := countEventsOfKind(t, st, EventCloudTaskFailed); n != 0 {
				t.Errorf("%s rows = %d, want 0", EventCloudTaskFailed, n)
			}
			if n := countEventsOfKind(t, st, EventCloudFailureSkipped); n != 1 {
				t.Errorf("%s rows = %d, want 1", EventCloudFailureSkipped, n)
			}
			if len(notifier.calls) != 0 {
				t.Errorf("Notify called %d times, want 0", len(notifier.calls))
			}
		})
	}
}

// TestParkIsIdempotentWhileAlreadyParked (FIX-I-7): the worker CLI retries
// a 503 on its own, so one cloud-unreachable episode can reach ParkOrGiveUp
// several times. Only the first parks: one outbox row, one notification,
// next_retry_at keeps its first value; the repeats are ledgered
// task.waiting_retry_dup.
func TestParkIsIdempotentWhileAlreadyParked(t *testing.T) {
	ctx := context.Background()
	st, m, cr, notifier := newCloudRetryFixture(t, []time.Duration{time.Minute}, 24*time.Hour)
	insertTask(t, st, "t1")
	mustTransition(t, m, ctx, "t1", StatusExecuting)

	var firstNextRetry string
	for i := 0; i < 3; i++ {
		if err := cr.ParkOrGiveUp(ctx, "trace-t1", "t1"); err != nil {
			t.Fatalf("ParkOrGiveUp #%d: %v", i, err)
		}
		row, err := st.Queries.GetTaskByID(ctx, "t1")
		if err != nil {
			t.Fatalf("GetTaskByID: %v", err)
		}
		if i == 0 {
			firstNextRetry = row.NextRetryAt.String
		} else if row.NextRetryAt.String != firstNextRetry {
			t.Errorf("next_retry_at after call #%d = %q, want the first park's %q", i, row.NextRetryAt.String, firstNextRetry)
		}
		time.Sleep(2 * time.Millisecond) // a re-park would compute a later next_retry_at
	}
	if got := taskStatus(t, st, "t1"); got != StatusRetryWait {
		t.Errorf("status = %q, want %q", got, StatusRetryWait)
	}
	if n := countOutboxResumeRows(t, st); n != 1 {
		t.Errorf("outbox rows = %d, want 1", n)
	}
	if len(notifier.calls) != 1 {
		t.Errorf("notify calls = %d, want 1", len(notifier.calls))
	}
	if n := countEventsOfKind(t, st, EventCloudWaitingRetry); n != 1 {
		t.Errorf("%s rows = %d, want 1", EventCloudWaitingRetry, n)
	}
	if n := countEventsOfKind(t, st, EventCloudWaitingRetryDup); n != 2 {
		t.Errorf("%s rows = %d, want 2", EventCloudWaitingRetryDup, n)
	}
}
