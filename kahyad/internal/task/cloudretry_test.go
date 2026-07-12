package task

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// transitionToExecuting drives id from its current status to 'executing'
// via the legal path for that status (intent->executing directly;
// bekliyor-yeniden-deneme/blocked_user->executing directly too) - each
// call bumps tasks.attempts by one (Machine.Transition's own doc
// comment), which is exactly how a real resume/redispatch accumulates
// attempts.
func transitionToExecuting(t *testing.T, m *Machine, traceID, id string) {
	t.Helper()
	if err := m.Transition(context.Background(), traceID, id, StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
}

// simulateNCloudFailures drives id through N full executing->bekliyor-
// yeniden-deneme->executing cycles (bumping attempts by N total),
// leaving it in 'executing' with tasks.attempts==n - a realistic
// precondition for testing park()'s schedule-index selection at a given
// attempts count.
func simulateNCloudFailures(t *testing.T, m *Machine, traceID, id string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		transitionToExecuting(t, m, traceID, id)
		if i < n-1 {
			if err := m.Transition(context.Background(), traceID, id, StatusRetryWait); err != nil {
				t.Fatalf("Transition(->bekliyor-yeniden-deneme): %v", err)
			}
		}
	}
}

func newCloudRetryFixture(t *testing.T, schedule []time.Duration, giveUpAfter time.Duration) (*store.Store, *Machine, *CloudRetry, *fakeNotifier) {
	t.Helper()
	st := testStore(t)
	m := NewMachine(st.Queries, st)
	notifier := &fakeNotifier{}
	cr := NewCloudRetry(st.Queries, st.Queries, m, st, notifier, schedule, giveUpAfter)
	return st, m, cr, notifier
}

// --- byte-exact Turkish strings (CLAUDE.md language policy) ---

func TestCloudRetryTurkishStringsByteExact(t *testing.T) {
	if MsgCloudParked != "Bulut servisine ulaşılamıyor; görev bekliyor-yeniden-deneme durumunda. Ağ dönünce otomatik devam edecek." {
		t.Errorf("MsgCloudParked = %q, not byte-exact", MsgCloudParked)
	}
	if MsgCloudNonRetryableFmt != "Bulut çağrısı kalıcı hatayla reddedildi (%s). Görev durduruldu." {
		t.Errorf("MsgCloudNonRetryableFmt = %q, not byte-exact", MsgCloudNonRetryableFmt)
	}
	if MsgCloudGiveUpFmt != "Yeniden deneme süresi doldu (24 sa). Görev kapatıldı: %s." {
		t.Errorf("MsgCloudGiveUpFmt = %q, not byte-exact", MsgCloudGiveUpFmt)
	}
}

// --- park: bekliyor-yeniden-deneme, next_retry_at, outbox row, ledger ---

func TestParkOrGiveUpParksWithNextRetryAtFromSchedule(t *testing.T) {
	schedule := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
	st, m, cr, notifier := newCloudRetryFixture(t, schedule, 24*time.Hour)
	fixedNow := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	cr.SetClock(func() time.Time { return fixedNow })
	m.SetClock(func() time.Time { return fixedNow })

	insertTask(t, st, "t1")
	simulateNCloudFailures(t, m, "trace-t1", "t1", 1) // attempts==1 -> schedule[0]==1m

	if err := cr.ParkOrGiveUp(context.Background(), "trace-t1", "t1"); err != nil {
		t.Fatalf("ParkOrGiveUp: %v", err)
	}

	got, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != StatusRetryWait {
		t.Errorf("status = %q, want %q", got.Status, StatusRetryWait)
	}
	if !got.NextRetryAt.Valid {
		t.Fatal("next_retry_at is NULL, want set")
	}
	nextRetryAt, err := time.Parse(fixedNanoRFC3339Layout, got.NextRetryAt.String)
	if err != nil {
		t.Fatalf("parse next_retry_at %q: %v", got.NextRetryAt.String, err)
	}
	want := fixedNow.Add(time.Minute)
	if nextRetryAt.Sub(want).Abs() > time.Second {
		t.Errorf("next_retry_at = %v, want ~%v (attempts=1 -> schedule[0]=1m)", nextRetryAt, want)
	}

	// The outbox resume row must be available exactly at next_retry_at -
	// kahyad/internal/outbox.Dispatcher's own ListDueOutboxRows query is
	// what actually redelivers it (no new mechanism, task spec step 3).
	var availableAt sql.NullString
	if err := st.DB().QueryRow(`SELECT available_at FROM outbox WHERE kind = ? LIMIT 1`, OutboxKindTaskResume).Scan(&availableAt); err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if !availableAt.Valid || availableAt.String != got.NextRetryAt.String {
		t.Errorf("outbox available_at = %+v, want it to equal next_retry_at %q", availableAt, got.NextRetryAt.String)
	}

	if n := countEventsOfKind(t, st, EventCloudWaitingRetry); n != 1 {
		t.Errorf("task.waiting_retry ledgered %d times, want 1", n)
	}

	if len(notifier.calls) != 1 {
		t.Fatalf("Notify called %d times, want 1", len(notifier.calls))
	}
	if notifier.calls[0].message != MsgCloudParked {
		t.Errorf("notify message = %q, want the exact parked string", notifier.calls[0].message)
	}
}

// TestParkOrGiveUpScheduleIndexClampsToLastEntry proves attempts beyond
// the schedule's length keep reusing the LAST entry ("then hourly" when
// that entry is itself 60m, per the task spec).
func TestParkOrGiveUpScheduleIndexClampsToLastEntry(t *testing.T) {
	schedule := []time.Duration{time.Minute, 5 * time.Minute}
	st, m, cr, _ := newCloudRetryFixture(t, schedule, 24*time.Hour)
	fixedNow := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	cr.SetClock(func() time.Time { return fixedNow })
	m.SetClock(func() time.Time { return fixedNow })

	insertTask(t, st, "t1")
	simulateNCloudFailures(t, m, "trace-t1", "t1", 5) // attempts==5, way past len(schedule)==2

	if err := cr.ParkOrGiveUp(context.Background(), "trace-t1", "t1"); err != nil {
		t.Fatalf("ParkOrGiveUp: %v", err)
	}

	got, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	nextRetryAt, err := time.Parse(fixedNanoRFC3339Layout, got.NextRetryAt.String)
	if err != nil {
		t.Fatalf("parse next_retry_at: %v", err)
	}
	want := fixedNow.Add(5 * time.Minute) // clamped to schedule's LAST entry
	if nextRetryAt.Sub(want).Abs() > time.Second {
		t.Errorf("next_retry_at = %v, want ~%v (clamped to last schedule entry)", nextRetryAt, want)
	}
}

// --- give-up: past give_up_after, task -> failed + give-up string ---

func TestParkOrGiveUpGivesUpPastGiveUpAfter(t *testing.T) {
	schedule := []time.Duration{time.Minute}
	st, m, cr, notifier := newCloudRetryFixture(t, schedule, 24*time.Hour)

	created := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC) // 2+ days before "now" below
	st2 := st
	ctx := context.Background()
	if _, err := st2.Queries.InsertTask(ctx, sqlcgen.InsertTaskParams{
		ID: "t1", TraceID: "trace-t1", SessionID: sql.NullString{}, State: "running", TaintTier: "untrusted",
		UpdatedAt: created.Format(time.RFC3339), CreatedAt: created.Format(time.RFC3339), Lane: "normal",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	laterNow := created.Add(25 * time.Hour) // past the 24h give_up_after
	m.SetClock(func() time.Time { return laterNow })
	cr.SetClock(func() time.Time { return laterNow })

	simulateNCloudFailures(t, m, "trace-t1", "t1", 1)

	if err := cr.ParkOrGiveUp(ctx, "trace-t1", "t1"); err != nil {
		t.Fatalf("ParkOrGiveUp: %v", err)
	}

	got, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %q, want %q (give-up)", got.Status, StatusFailed)
	}
	if n := countEventsOfKind(t, st, EventCloudTaskFailed); n != 1 {
		t.Errorf("task.failed ledgered %d times, want 1", n)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("Notify called %d times, want 1", len(notifier.calls))
	}
	want := "Yeniden deneme süresi doldu (24 sa). Görev kapatıldı: t1."
	if notifier.calls[0].message != want {
		t.Errorf("notify message = %q, want %q", notifier.calls[0].message, want)
	}
}

// TestParkOrGiveUpStillParksJustBeforeGiveUpAfter proves the give-up
// check is a >= boundary, not off-by-one in the wrong direction.
func TestParkOrGiveUpStillParksJustBeforeGiveUpAfter(t *testing.T) {
	schedule := []time.Duration{time.Minute}
	st, m, cr, _ := newCloudRetryFixture(t, schedule, 24*time.Hour)

	created := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if _, err := st.Queries.InsertTask(ctx, sqlcgen.InsertTaskParams{
		ID: "t1", TraceID: "trace-t1", SessionID: sql.NullString{}, State: "running", TaintTier: "untrusted",
		UpdatedAt: created.Format(time.RFC3339), CreatedAt: created.Format(time.RFC3339), Lane: "normal",
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	almostThere := created.Add(23 * time.Hour) // still within 24h
	m.SetClock(func() time.Time { return almostThere })
	cr.SetClock(func() time.Time { return almostThere })

	simulateNCloudFailures(t, m, "trace-t1", "t1", 1)

	if err := cr.ParkOrGiveUp(ctx, "trace-t1", "t1"); err != nil {
		t.Fatalf("ParkOrGiveUp: %v", err)
	}

	got, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != StatusRetryWait {
		t.Errorf("status = %q, want %q (must still park, not give up yet)", got.Status, StatusRetryWait)
	}
}

// --- FailNonRetryable: immediate failure, exact Turkish string ---

func TestFailNonRetryableTransitionsAndNotifies(t *testing.T) {
	st, m, cr, notifier := newCloudRetryFixture(t, nil, 0)
	insertTask(t, st, "t1")
	transitionToExecuting(t, m, "trace-t1", "t1")

	if err := cr.FailNonRetryable(context.Background(), "trace-t1", "t1", "authentication_error"); err != nil {
		t.Fatalf("FailNonRetryable: %v", err)
	}

	got, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %q, want %q", got.Status, StatusFailed)
	}
	if n := countEventsOfKind(t, st, EventCloudTaskFailed); n != 1 {
		t.Errorf("task.failed ledgered %d times, want 1", n)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("Notify called %d times, want 1", len(notifier.calls))
	}
	want := "Bulut çağrısı kalıcı hatayla reddedildi (authentication_error). Görev durduruldu."
	if notifier.calls[0].message != want {
		t.Errorf("notify message = %q, want %q", notifier.calls[0].message, want)
	}
}

// --- constructor defaults ---

func TestNewCloudRetryDefaultsScheduleAndGiveUpAfter(t *testing.T) {
	st := testStore(t)
	m := NewMachine(st.Queries, st)
	cr := NewCloudRetry(st.Queries, st.Queries, m, st, nil, nil, 0)
	if len(cr.schedule) != 4 {
		t.Fatalf("default schedule length = %d, want 4", len(cr.schedule))
	}
	if cr.schedule[len(cr.schedule)-1] != time.Hour {
		t.Errorf("default schedule's last entry = %v, want 1h (\"then hourly\")", cr.schedule[len(cr.schedule)-1])
	}
	if cr.giveUpAfter != 24*time.Hour {
		t.Errorf("default giveUpAfter = %v, want 24h", cr.giveUpAfter)
	}
}
