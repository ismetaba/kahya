package task

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"kahya/kahyad/internal/store/sqlcgen"
)

// TestResolverAbortMovesBlockedUserToFailed is the acceptance test (b)'s
// own final step: "kahya task resolve <id> --abort -> failed".
func TestResolverAbortMovesBlockedUserToFailed(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)
	if err := m.Transition(ctx, "trace", "t1", StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
	if err := m.Transition(ctx, "trace", "t1", StatusBlockedUser); err != nil {
		t.Fatalf("Transition(->blocked_user): %v", err)
	}

	rs := NewResolver(st.Queries, st.Queries, m)
	if err := rs.Abort(ctx, "trace", "t1"); err != nil {
		t.Fatalf("Abort() error = %v", err)
	}
	if got := taskStatus(t, st, "t1"); got != StatusFailed {
		t.Errorf("status = %q, want %q", got, StatusFailed)
	}
}

func TestResolverAbortRejectsTerminalTask(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)
	if err := m.Transition(ctx, "trace", "t1", StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
	if err := m.Transition(ctx, "trace", "t1", StatusDone); err != nil {
		t.Fatalf("Transition(->done): %v", err)
	}

	rs := NewResolver(st.Queries, st.Queries, m)
	err := rs.Abort(ctx, "trace", "t1")
	if !errors.Is(err, ErrTaskNotResolvable) {
		t.Fatalf("Abort() error = %v, want ErrTaskNotResolvable", err)
	}
	if got := taskStatus(t, st, "t1"); got != StatusDone {
		t.Errorf("status = %q, want unchanged %q", got, StatusDone)
	}
}

func TestResolverRetryMovesBlockedUserBackToExecutingAndRequeues(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)
	if err := m.Transition(ctx, "trace", "t1", StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
	if err := m.Transition(ctx, "trace", "t1", StatusBlockedUser); err != nil {
		t.Fatalf("Transition(->blocked_user): %v", err)
	}
	attemptsBefore := taskAttempts(t, st, "t1")

	rs := NewResolver(st.Queries, st.Queries, m)
	if err := rs.Retry(ctx, "trace", "t1"); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	if got := taskStatus(t, st, "t1"); got != StatusExecuting {
		t.Errorf("status = %q, want %q", got, StatusExecuting)
	}
	// Exactly one bump: Machine.Transition(->executing) bumps attempts
	// once for this fresh dispatch; writeOutboxResumeRow itself must not
	// double-count it.
	if got := taskAttempts(t, st, "t1"); got != attemptsBefore+1 {
		t.Errorf("attempts = %d, want %d", got, attemptsBefore+1)
	}
	if n := countOutboxResumeRows(t, st); n != 1 {
		t.Errorf("outbox task_resume rows = %d, want 1", n)
	}
}

// TestResolverRetryCancelsPendingRowsAndClearsNextRetryAt (FIX-I-8): `kahya
// task resolve <id> --retry` on a parked task means "now": the park's own
// future row (and any other pending one) is canceled, next_retry_at is
// cleared, and exactly one fresh row is left.
func TestResolverRetryCancelsPendingRowsAndClearsNextRetryAt(t *testing.T) {
	st, m, cr, _ := newCloudRetryFixture(t, []time.Duration{time.Hour}, 30*24*time.Hour)
	ctx := context.Background()
	insertTask(t, st, "t1")
	mustTransition(t, m, ctx, "t1", StatusExecuting)
	if err := cr.ParkOrGiveUp(ctx, "trace-t1", "t1"); err != nil {
		t.Fatal(err)
	}
	var parkRowID int64
	if err := st.DB().QueryRow(`SELECT id FROM outbox WHERE task_id = 't1'`).Scan(&parkRowID); err != nil {
		t.Fatalf("park row: %v", err)
	}

	rs := NewResolver(st.Queries, st.Queries, m)
	if err := rs.Retry(ctx, "trace-t1", "t1"); err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	if got := taskStatus(t, st, "t1"); got != StatusExecuting {
		t.Errorf("status = %q, want %q", got, StatusExecuting)
	}
	var canceledAt sql.NullString
	if err := st.DB().QueryRow(`SELECT canceled_at FROM outbox WHERE id = ?`, parkRowID).Scan(&canceledAt); err != nil {
		t.Fatal(err)
	}
	if !canceledAt.Valid {
		t.Error("the park's row must be canceled - it would redispatch the task a second time at next_retry_at")
	}
	if n := pendingOutboxRows(t, st, "t1"); n != 1 {
		t.Errorf("pending rows = %d, want exactly the fresh one", n)
	}
	row, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.NextRetryAt.Valid {
		t.Errorf("next_retry_at = %q, want NULL", row.NextRetryAt.String)
	}
}

// scanningOutbox runs a resume scan once inside the named call, before it
// executes - a scan tick landing at that point of Resolver.Retry.
type scanningOutbox struct {
	OutboxEnqueuer
	at   string
	scan func()
	done bool
}

func (o *scanningOutbox) hit(name string) {
	if !o.done && o.at == name {
		o.done = true
		o.scan()
	}
}

func (o *scanningOutbox) CancelOutboxRowsByTask(ctx context.Context, arg sqlcgen.CancelOutboxRowsByTaskParams) (int64, error) {
	o.hit("cancel")
	return o.OutboxEnqueuer.CancelOutboxRowsByTask(ctx, arg)
}

func (o *scanningOutbox) CountPendingOutboxRowsByTask(ctx context.Context, taskID sql.NullString) (int64, error) {
	o.hit("count")
	return o.OutboxEnqueuer.CountPendingOutboxRowsByTask(ctx, taskID)
}

// TestResolverRetryRacingResumeScanDoesNotReblock (FIX-FINAL resume-scan-
// reblocks-during-task-retry): Retry moved blocked_user -> executing first
// and only then reset crash_resumes and wrote its row, so a resume-scan
// tick in between saw an executing, not-live task still at the cap,
// re-blocked it with a second cap notice, and Retry "succeeded" for a task
// that never ran. A scan landing anywhere in Retry now either skips the
// task or enqueues the single row itself; Retry always leaves it executing
// with exactly one pending row and no second notice.
func TestResolverRetryRacingResumeScanDoesNotReblock(t *testing.T) {
	for _, at := range []string{"cancel", "count"} {
		t.Run(at, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			insertTask(t, st, "t1")
			m := NewMachine(st.Queries, st)
			mustTransition(t, m, ctx, "t1", StatusExecuting)
			mustTransition(t, m, ctx, "t1", StatusBlockedUser)
			if _, err := st.DB().Exec(`UPDATE tasks SET crash_resumes = 3 WHERE id = 't1'`); err != nil {
				t.Fatal(err)
			}
			notifier := &fakeNotifier{}
			r := NewResume(st.Queries, st.Queries, st.Queries, m, notifier, nil, 3)
			r.SetMaxCrashResumes(3)
			ob := &scanningOutbox{OutboxEnqueuer: st.Queries, at: at, scan: func() {
				if _, err := r.Scan(ctx); err != nil {
					t.Errorf("scan inside Retry: %v", err)
				}
			}}
			if err := NewResolver(st.Queries, ob, m).Retry(ctx, "trace", "t1"); err != nil {
				t.Fatalf("Retry: %v", err)
			}
			if !ob.done {
				t.Fatalf("the scan hook at %q never ran", at)
			}
			if got := taskStatus(t, st, "t1"); got != StatusExecuting {
				t.Fatalf("status = %q, want executing (a racing scan must not re-block a retried task)", got)
			}
			if n := pendingOutboxRows(t, st, "t1"); n != 1 {
				t.Errorf("pending resume rows = %d, want exactly 1", n)
			}
			if len(notifier.calls) != 0 {
				t.Errorf("notices = %+v, want none", notifier.calls)
			}
		})
	}
}
