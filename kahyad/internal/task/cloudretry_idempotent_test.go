package task

import (
	"context"
	"testing"
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
