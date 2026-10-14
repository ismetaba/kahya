package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

// G10-E1 for a redispatch whose worker kahyad never started. Since FIX-I-8
// the resume scan's enqueue dedupe counts a claimed-but-unacked row as
// pending, so a processResume exit that leaves its claimed row unacked locks
// the scan - the one path that counts tasks.crash_resumes against
// task_max_crash_resumes (FIX-I-2) - out for good. The only retries left are
// the dispatcher's own uncounted lease-expiry re-claims, and the cap never
// fires. These tests pin that every such exit with the task left 'executing'
// acks its row, like a crashed run does.

// recordingNotifier is a task.Notifier that records every notice.
type recordingNotifier struct{ kinds, messages []string }

func (n *recordingNotifier) Notify(_ context.Context, _, kind, message string, _ map[string]any) error {
	n.kinds = append(n.kinds, kind)
	n.messages = append(n.messages, message)
	return nil
}

// attemptsFailStore is a task.Store whose IncrementTaskAttempts always
// fails: Machine.Transition then returns an error AFTER its status edge
// already landed.
type attemptsFailStore struct{ *sqlcgen.Queries }

func (attemptsFailStore) IncrementTaskAttempts(context.Context, sqlcgen.IncrementTaskAttemptsParams) (int64, error) {
	return 0, errors.New("disk I/O error")
}

// statusFailStore is a task.Store whose SetTaskStatus always fails: the task
// never leaves the status it had.
type statusFailStore struct{ *sqlcgen.Queries }

func (statusFailStore) SetTaskStatus(context.Context, sqlcgen.SetTaskStatusParams) (int64, error) {
	return 0, errors.New("disk I/O error")
}

// redispatchCrashedPayload returns the payload of the newest
// outbox.redispatch_crashed ledger row.
func redispatchCrashedPayload(t *testing.T, st *store.Store) map[string]any {
	t.Helper()
	var raw string
	if err := st.DB().QueryRow(`SELECT payload FROM events WHERE kind = ? ORDER BY id DESC LIMIT 1`, EventRedispatchCrashed).Scan(&raw); err != nil {
		t.Fatalf("read %s payload: %v", EventRedispatchCrashed, err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("decode %s payload %q: %v", EventRedispatchCrashed, raw, err)
	}
	return p
}

// parkTaskWithDueRow parks t1 (executing -> bekliyor-yeniden-deneme, the
// park's own row at next_retry_at = now+1h) and returns that row's id; the
// caller's dispatcher clock must run 2h ahead for the row to be due.
func parkTaskWithDueRow(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ctx := context.Background()
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	m := task.NewMachine(st.Queries, st)
	cr := task.NewCloudRetry(st.Queries, st.Queries, m, st, nil, []time.Duration{time.Hour}, 30*24*time.Hour)
	if err := cr.ParkOrGiveUp(ctx, "trace-t1", "t1"); err != nil {
		t.Fatal(err)
	}
	if got := taskStatusOfStore(t, st, "t1"); got != task.StatusRetryWait {
		t.Fatalf("status = %q, want %q", got, task.StatusRetryWait)
	}
	var id int64
	if err := st.DB().QueryRow(`SELECT id FROM outbox WHERE task_id = 't1' AND dispatched_at IS NULL`).Scan(&id); err != nil {
		t.Fatalf("park row: %v", err)
	}
	return id
}

// TestProxyOpenFailureRowIsAckedSoCrashResumeCapApplies: a redispatch whose
// per-task anthproxy listener cannot be opened (the task already moved to
// 'executing') acks its row with outbox.redispatch_crashed, so the next
// attempt is the resume scan's counted one and task_max_crash_resumes ends
// the loop in blocked_user with its notice. Before, the row stayed claimed
// and the dispatcher re-tried every lease period forever, uncounted.
func TestProxyOpenFailureRowIsAckedSoCrashResumeCapApplies(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	m := task.NewMachine(st.Queries, st)
	notifier := &recordingNotifier{}
	r := task.NewResume(st.Queries, st.Queries, st.Queries, m, notifier, nil, 0)
	r.SetMaxCrashResumes(2)
	marker := t.TempDir() + "/spawned"
	d := newTestDispatcher(st, m, markerWorker(marker)) // lease 50ms
	opens := 0
	d.SetAnthproxyOpener(func(context.Context, string, string) (string, string, func() error, error) {
		opens++
		return "", "", nil, errors.New("listen: address already in use")
	})

	// Round 1, checked step by step.
	if _, err := r.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if n, err := d.ClaimAndDispatch(ctx); err != nil || n != 1 {
		t.Fatalf("ClaimAndDispatch() = %d, %v; want 1, nil", n, err)
	}
	if n := countRows(t, st, `SELECT count(*) FROM outbox WHERE task_id = 't1' AND dispatched_at IS NULL`); n != 0 {
		t.Fatalf("unacked rows after a proxy-open failure = %d, want 0: the claimed row locks the counted resume scan out (FIX-I-8 dedupe) and only uncounted lease re-claims remain", n)
	}
	if got := taskStatusOfStore(t, st, "t1"); got != task.StatusExecuting {
		t.Fatalf("status = %q, want %q (kahyad failing to start the worker never ends the task)", got, task.StatusExecuting)
	}
	if n := countEvents(t, st, "outbox.anthproxy_open_failed"); n != 1 {
		t.Errorf("outbox.anthproxy_open_failed rows = %d, want 1", n)
	}
	p := redispatchCrashedPayload(t, st)
	if runErr, _ := p["run_err"].(string); !strings.Contains(runErr, "address already in use") {
		t.Errorf("%s run_err = %q, want the proxy open error", EventRedispatchCrashed, runErr)
	}
	if p["task_id"] != "t1" {
		t.Errorf("%s task_id = %v, want t1", EventRedispatchCrashed, p["task_id"])
	}

	// Rounds 2..6, each waiting out the 50ms lease: the cap must end it.
	for i := 0; i < 5; i++ {
		if _, err := r.Scan(ctx); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if _, err := d.ClaimAndDispatch(ctx); err != nil {
			t.Fatalf("ClaimAndDispatch: %v", err)
		}
		time.Sleep(70 * time.Millisecond)
	}
	got, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusBlockedUser {
		t.Fatalf("status = %q (crash_resumes=%d, %d proxy opens), want %q: the crash-resume cap never fired", got.Status, got.CrashResumes, opens, task.StatusBlockedUser)
	}
	if got.CrashResumes != 2 {
		t.Errorf("crash_resumes = %d, want 2 (the cap)", got.CrashResumes)
	}
	if opens != 2 {
		t.Errorf("proxy opens = %d, want 2 (one per counted resume, no uncounted re-claim)", opens)
	}
	if n := countEvents(t, st, EventRedispatchCrashed); n != 2 {
		t.Errorf("%s rows = %d, want 2", EventRedispatchCrashed, n)
	}
	if len(notifier.kinds) != 1 || notifier.kinds[0] != task.EventResumeCapExceeded {
		t.Errorf("notices = %v, want one %s", notifier.kinds, task.EventResumeCapExceeded)
	}
	assertNoWorkerSpawned(t, marker)
}

// TestPreSpawnTransitionFailureLeavingTaskExecutingAcksRow: the pre-spawn
// bekliyor-yeniden-deneme -> executing transition can fail AFTER its status
// edge landed (the attempts bump failed). The task is then 'executing' with
// no worker, which is the resume scan's to re-dispatch: the row is acked
// (outbox.redispatch_crashed) so that scan's counted enqueue is not locked
// out by the claimed row.
func TestPreSpawnTransitionFailureLeavingTaskExecutingAcksRow(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	parkRow := parkTaskWithDueRow(t, st)
	failing := task.NewMachine(attemptsFailStore{st.Queries}, st)
	marker := t.TempDir() + "/spawned"
	d := newTestDispatcher(st, failing, markerWorker(marker))
	d.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })

	if n, err := d.ClaimAndDispatch(ctx); err != nil || n != 1 {
		t.Fatalf("ClaimAndDispatch() = %d, %v; want 1, nil", n, err)
	}
	assertNoWorkerSpawned(t, marker)
	if got := taskStatusOfStore(t, st, "t1"); got != task.StatusExecuting {
		t.Fatalf("status = %q, want %q (the edge landed, only the attempts bump failed)", got, task.StatusExecuting)
	}
	if !rowAcked(t, st, parkRow) {
		t.Fatal("claimed row left unacked with the task 'executing': the resume scan's dedupe counts it as pending and only uncounted lease re-claims remain")
	}
	if n := countEvents(t, st, "outbox.transition_failed"); n != 1 {
		t.Errorf("outbox.transition_failed rows = %d, want 1", n)
	}
	if n := countEvents(t, st, EventRedispatchCrashed); n != 1 {
		t.Errorf("%s rows = %d, want 1", EventRedispatchCrashed, n)
	}

	// The counted path is open: the next scan enqueues and counts.
	m := task.NewMachine(st.Queries, st)
	r := task.NewResume(st.Queries, st.Queries, st.Queries, m, nil, nil, 3)
	if n, err := r.Scan(ctx); err != nil || n != 1 {
		t.Fatalf("Scan() = %d, %v; want 1, nil", n, err)
	}
	row, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.CrashResumes != 1 {
		t.Fatalf("crash_resumes = %d, want 1", row.CrashResumes)
	}
}

// TestPreSpawnTransitionFailureOnParkedTaskKeepsParkRow: when the pre-spawn
// transition fails with the task still parked, the claimed row is the park's
// own - the only row that will ever redispatch it (the resume scan never
// looks at a parked task). It is NOT acked; its lease expiry retries it.
func TestPreSpawnTransitionFailureOnParkedTaskKeepsParkRow(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	parkRow := parkTaskWithDueRow(t, st)
	failing := task.NewMachine(statusFailStore{st.Queries}, st)
	marker := t.TempDir() + "/spawned"
	d := newTestDispatcher(st, failing, markerWorker(marker))
	d.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })

	if n, err := d.ClaimAndDispatch(ctx); err != nil || n != 1 {
		t.Fatalf("ClaimAndDispatch() = %d, %v; want 1, nil", n, err)
	}
	assertNoWorkerSpawned(t, marker)
	if got := taskStatusOfStore(t, st, "t1"); got != task.StatusRetryWait {
		t.Fatalf("status = %q, want %q", got, task.StatusRetryWait)
	}
	if rowAcked(t, st, parkRow) {
		t.Fatal("the park's own row was acked while the task is still parked: nothing would ever redispatch it")
	}
	if n := countEvents(t, st, EventRedispatchCrashed); n != 0 {
		t.Errorf("%s rows = %d, want 0", EventRedispatchCrashed, n)
	}
}
