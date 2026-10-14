package task

import (
	"context"
	"fmt"
	"testing"
	"time"

	"kahya/kahyad/internal/store"
)

// markAllOutboxDelivered simulates the outbox dispatcher having consumed
// every pending resume row.
func markAllOutboxDelivered(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.DB().Exec(`UPDATE outbox SET dispatched_at = '2026-01-01T00:00:00.000000000Z' WHERE dispatched_at IS NULL`); err != nil {
		t.Fatalf("mark outbox delivered: %v", err)
	}
}

func taskCrashResumes(t *testing.T, st *store.Store, id string) int64 {
	t.Helper()
	row, err := st.Queries.GetTaskByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	return row.CrashResumes
}

// TestResumeScanCapsCrashResumes (FIX-I-2): a tool-less task whose worker
// keeps dying (no terminal line, timeout, a budget 503 loop) used to be
// re-enqueued on every scan tick forever. The resume scan's own
// re-dispatches are now counted in tasks.crash_resumes; at
// task_max_crash_resumes (default 3) the task moves to blocked_user with the
// byte-exact Turkish notice, once.
func TestResumeScanCapsCrashResumes(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)
	mustTransition(t, m, ctx, "t1", StatusExecuting) // attempts=1
	notifier := &fakeNotifier{}
	r := NewResume(st.Queries, st.Queries, st.Queries, m, notifier, nil, 3)
	r.SetMaxCrashResumes(3)

	for i := 0; i < 4; i++ {
		row, err := st.Queries.GetTaskByID(ctx, "t1")
		if err != nil {
			t.Fatalf("GetTaskByID: %v", err)
		}
		if err := r.ProcessTask(ctx, row); err != nil {
			t.Fatalf("ProcessTask #%d: %v", i, err)
		}
		markAllOutboxDelivered(t, st) // the dispatcher consumed this attempt's row
	}
	if got := taskStatus(t, st, "t1"); got != StatusBlockedUser {
		t.Fatalf("status = %q, want %q", got, StatusBlockedUser)
	}
	if n := countOutboxResumeRows(t, st); n != 3 {
		t.Errorf("outbox rows = %d, want 3", n)
	}
	if got := taskCrashResumes(t, st, "t1"); got != 3 {
		t.Errorf("crash_resumes = %d, want 3", got)
	}
	want := fmt.Sprintf(MsgResumeCapExceededFmt, "t1", 3, "t1")
	if len(notifier.calls) != 1 || notifier.calls[0].message != want || notifier.calls[0].kind != EventResumeCapExceeded {
		t.Fatalf("notify = %+v, want one %s call with %q", notifier.calls, EventResumeCapExceeded, want)
	}

	// A blocked task handed to ProcessTask again (it is never a Scan
	// candidate, but ProcessTask is exported) neither notifies again nor
	// enqueues another row.
	row, _ := st.Queries.GetTaskByID(ctx, "t1")
	_ = r.ProcessTask(ctx, row)
	if len(notifier.calls) != 1 {
		t.Errorf("notify calls after re-processing a blocked task = %d, want 1", len(notifier.calls))
	}
}

// TestResumeCapExceededMessageByteExact pins the Turkish notice.
func TestResumeCapExceededMessageByteExact(t *testing.T) {
	if MsgResumeCapExceededFmt != "Görev %s: işçi süreci %d kez yarıda kesildi, otomatik devam limiti doldu — 'kahya task resolve %s' ile karar ver." {
		t.Errorf("MsgResumeCapExceededFmt = %q, not byte-exact", MsgResumeCapExceededFmt)
	}
}

// TestCloudParkRedispatchDoesNotConsumeCrashBudget: cloud-park redispatches
// (bekliyor-yeniden-deneme -> executing) count in tasks.attempts, never in
// crash_resumes - a parked task legitimately retries for up to
// cloud_retry_give_up_after.
func TestCloudParkRedispatchDoesNotConsumeCrashBudget(t *testing.T) {
	ctx := context.Background()
	st, m, cr, _ := newCloudRetryFixture(t, []time.Duration{time.Minute}, 24*time.Hour)
	insertTask(t, st, "t1")
	mustTransition(t, m, ctx, "t1", StatusExecuting)
	for i := 0; i < 5; i++ {
		if err := cr.ParkOrGiveUp(ctx, "trace-t1", "t1"); err != nil {
			t.Fatalf("ParkOrGiveUp #%d: %v", i, err)
		}
		mustTransition(t, m, ctx, "t1", StatusExecuting)
	}
	if got := taskAttempts(t, st, "t1"); got != 6 {
		t.Errorf("attempts = %d, want 6", got)
	}
	if got := taskCrashResumes(t, st, "t1"); got != 0 {
		t.Errorf("crash_resumes = %d, want 0 (cloud parks are not crash resumes)", got)
	}
}

// TestResolverRetryResetsCrashResumes: `kahya task resolve <id> --retry` is
// an explicit human decision and starts a fresh crash budget.
func TestResolverRetryResetsCrashResumes(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)
	mustTransition(t, m, ctx, "t1", StatusExecuting)
	r := NewResume(st.Queries, st.Queries, st.Queries, m, &fakeNotifier{}, nil, 3)
	for i := 0; i < 4; i++ {
		row, _ := st.Queries.GetTaskByID(ctx, "t1")
		if err := r.ProcessTask(ctx, row); err != nil {
			t.Fatalf("ProcessTask #%d: %v", i, err)
		}
		markAllOutboxDelivered(t, st)
	}
	if got := taskStatus(t, st, "t1"); got != StatusBlockedUser {
		t.Fatalf("precondition: status = %q, want blocked_user", got)
	}

	if err := NewResolver(st.Queries, st.Queries, m).Retry(ctx, "trace-t1", "t1"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if got := taskStatus(t, st, "t1"); got != StatusExecuting {
		t.Errorf("status = %q, want executing", got)
	}
	if got := taskCrashResumes(t, st, "t1"); got != 0 {
		t.Errorf("crash_resumes = %d, want 0 after --retry", got)
	}
	var pending int
	if err := st.DB().QueryRow(`SELECT count(*) FROM outbox WHERE dispatched_at IS NULL`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Errorf("pending outbox rows after --retry = %d, want 1", pending)
	}
}

// TestSetMaxCrashResumesNonPositiveRestoresDefault.
func TestSetMaxCrashResumesNonPositiveRestoresDefault(t *testing.T) {
	st := testStore(t)
	r := NewResume(st.Queries, st.Queries, st.Queries, NewMachine(st.Queries, st), nil, nil, 3)
	r.SetMaxCrashResumes(0)
	if r.maxCrashResumes != defaultMaxCrashResumes {
		t.Errorf("maxCrashResumes = %d, want %d", r.maxCrashResumes, defaultMaxCrashResumes)
	}
	r.SetMaxCrashResumes(5)
	if r.maxCrashResumes != 5 {
		t.Errorf("maxCrashResumes = %d, want 5", r.maxCrashResumes)
	}
}
