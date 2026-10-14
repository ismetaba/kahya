package outbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/task"
)

// TestCrashedRedispatchRowIsAckedSoResumeScanCountsNextAttempt (G10-E1,
// landed with FIX-I-3): a resumed worker that exits without a terminal line
// used to leave its row unacknowledged, so lease expiry re-claimed it and
// the dispatcher re-spawned the task forever outside the resume scan - the
// only path that counts tasks.crash_resumes (FIX-I-2's cap). Now the row is
// acked and the next attempt is the scan's own, counted one.
func TestCrashedRedispatchRowIsAckedSoResumeScanCountsNextAttempt(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	stale := enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	d := newTestDispatcher(st, m, []string{"python3", "testdata/fail_no_terminal_worker.py"}) // exit 1, no terminal line
	if n, _ := d.ClaimAndDispatch(context.Background()); n != 1 {
		t.Fatalf("claimed = %d, want 1", n)
	}
	if !rowAcked(t, st, stale.ID) {
		t.Fatal("crashed redispatch row left unacknowledged: lease expiry would re-claim it outside the counted resume-scan path (crash_resumes never increments)")
	}
	if got := taskStatusOfStore(t, st, "t1"); got != task.StatusExecuting {
		t.Fatalf("status = %q, want %q (a crash never ends the task)", got, task.StatusExecuting)
	}
	// One meaningful ledger row records the acked crash (G10-E1, decision
	// G10-6): the spawned worker's outcome, no shutdown involved.
	if n := countEvents(t, st, EventRedispatchCrashed); n != 1 {
		t.Fatalf("%s rows = %d, want 1", EventRedispatchCrashed, n)
	}
	p := redispatchCrashedPayload(t, st)
	if p["status"] != spawn.StatusError || p["outbox_id"] != float64(stale.ID) || p["shutdown"] != false {
		t.Errorf("%s payload = %v, want status %q, outbox_id %d, shutdown false", EventRedispatchCrashed, p, spawn.StatusError, stale.ID)
	}
	if _, spawned := p["spawned"]; spawned {
		t.Errorf("%s payload carries \"spawned\" for a worker that did run: %v", EventRedispatchCrashed, p)
	}

	// ...and the resume scan's COUNTED path stays open.
	r := task.NewResume(st.Queries, st.Queries, st.Queries, m, nil, nil, 3)
	if n, err := r.Scan(context.Background()); err != nil || n != 1 {
		t.Fatalf("Scan() = %d, %v; want 1, nil", n, err)
	}
	row, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.CrashResumes != 1 {
		t.Fatalf("crash_resumes = %d, want 1", row.CrashResumes)
	}
	if n := countRows(t, st, `SELECT count(*) FROM outbox WHERE task_id = 't1' AND dispatched_at IS NULL`); n != 1 {
		t.Errorf("pending resume rows = %d, want exactly the scan's fresh one", n)
	}
}

// TestCrashingResumedWorkerEndsBlockedUserAtCrashResumeCap (G10-E1
// acceptance, FIX-I-2's cap end to end with a real crashing worker): a task
// whose worker dies without a terminal line on every attempt - its first
// spawn and every redispatch - is redispatched only by the resume scan's
// counted path, task_max_crash_resumes (3) times, and on the 4th crash the
// next scan moves it to blocked_user with the one byte-exact notice. No
// lease-expiry re-claim ever redispatches it uncounted, and the loop ends:
// further scan/dispatch ticks spawn nothing. Before G10-E1 the crashed row
// stayed claimed, the enqueue dedupe (FIX-I-8) counted it as pending, and
// the dispatcher re-spawned the task every lease period forever with
// crash_resumes stuck at 1.
func TestCrashingResumedWorkerEndsBlockedUserAtCrashResumeCap(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	// The first spawn crashed: 'executing', no worker, no pending row.
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	m := task.NewMachine(st.Queries, st)
	notifier := &recordingNotifier{}
	r := task.NewResume(st.Queries, st.Queries, st.Queries, m, notifier, nil, 3)
	r.SetMaxCrashResumes(3)                                                                   // the config default
	d := newTestDispatcher(st, m, []string{"python3", "testdata/fail_no_terminal_worker.py"}) // lease 50ms

	// Twice as many ticks as the cap needs, each outlasting the 50ms lease
	// so an unacked row WOULD be re-claimed.
	for round := 0; round < 8; round++ {
		if _, err := r.Scan(ctx); err != nil {
			t.Fatalf("round %d Scan: %v", round, err)
		}
		if _, err := d.ClaimAndDispatch(ctx); err != nil {
			t.Fatalf("round %d ClaimAndDispatch: %v", round, err)
		}
		time.Sleep(70 * time.Millisecond)
	}

	got, err := st.Queries.GetTaskByID(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusBlockedUser {
		t.Fatalf("status = %q (crash_resumes=%d, %d redispatches), want %q: the crash-resume cap never ended the loop",
			got.Status, got.CrashResumes, countEvents(t, st, EventResumeDispatched), task.StatusBlockedUser)
	}
	if got.CrashResumes != 3 {
		t.Errorf("crash_resumes = %d, want 3 (the cap)", got.CrashResumes)
	}
	if n := countEvents(t, st, EventResumeDispatched); n != 3 {
		t.Errorf("redispatches = %d, want 3 (one per counted resume, never an uncounted lease re-claim)", n)
	}
	if n := countEvents(t, st, EventRedispatchCrashed); n != 3 {
		t.Errorf("%s rows = %d, want 3", EventRedispatchCrashed, n)
	}
	if n := countRows(t, st, `SELECT count(*) FROM outbox WHERE task_id = 't1' AND dispatched_at IS NULL AND canceled_at IS NULL`); n != 0 {
		t.Errorf("pending resume rows = %d, want 0", n)
	}
	want := fmt.Sprintf(task.MsgResumeCapExceededFmt, "t1", 3, "t1")
	if len(notifier.kinds) != 1 || notifier.kinds[0] != task.EventResumeCapExceeded || notifier.messages[0] != want {
		t.Errorf("notices = %v %q, want one %s with %q", notifier.kinds, notifier.messages, task.EventResumeCapExceeded, want)
	}
	// ...and the loop is really over: more ticks spawn nothing.
	for round := 0; round < 2; round++ {
		if _, err := r.Scan(ctx); err != nil {
			t.Fatalf("post-cap Scan: %v", err)
		}
		if n, err := d.ClaimAndDispatch(ctx); err != nil || n != 0 {
			t.Fatalf("post-cap ClaimAndDispatch() = %d, %v; want 0, nil", n, err)
		}
	}
	if n := countEvents(t, st, EventResumeDispatched); n != 3 {
		t.Errorf("redispatches after the cap = %d, want still 3", n)
	}
}

// TestDispatcherFailsTaskOnExplicitWorkerErrorLine (FIX-I-3, mirrors
// FIX-I-2's first-spawn rule in server.decidePostSpawn): a resumed worker
// that reports its OWN terminal {"type":"error"} line has nothing left to
// resume - the task fails and the row is acked, instead of the old
// unacknowledged row that lease expiry re-claimed (re-running the same
// failing session, and its cloud spend, every lease period).
func TestDispatcherFailsTaskOnExplicitWorkerErrorLine(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	row := enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	d := newTestDispatcher(st, m, []string{"python3", "-c",
		`import sys; sys.stdin.buffer.read(); print('{"type": "error", "message": "model hata"}', flush=True); sys.exit(1)`})
	if n, _ := d.ClaimAndDispatch(context.Background()); n != 1 {
		t.Fatalf("claimed = %d, want 1", n)
	}
	if got := taskStatusOfStore(t, st, "t1"); got != task.StatusFailed {
		t.Errorf("status = %q, want %q (the worker itself reported a terminal error)", got, task.StatusFailed)
	}
	if !rowAcked(t, st, row.ID) {
		t.Error("row must be acked once the task failed")
	}
	if n := countEvents(t, st, "outbox.redispatch_worker_error"); n != 1 {
		t.Errorf("outbox.redispatch_worker_error rows = %d, want 1", n)
	}
	if n := countEvents(t, st, "outbox.redispatch_crashed"); n != 0 {
		t.Errorf("outbox.redispatch_crashed rows = %d, want 0 (an explicit error line is not a crash)", n)
	}
}

// TestProcessResumeClearsWorkerPGIDOnExit (FIX-I-13): the redispatch path's
// OnStart persists the worker's pgid; once spawn.Run returned it is cleared,
// so no later halt can SIGKILL an unrelated group that reused the pid.
func TestProcessResumeClearsWorkerPGIDOnExit(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	enqueueResumeRow(t, st, "t1")
	m := task.NewMachine(st.Queries, st)
	d := newTestDispatcher(st, m, []string{"python3", "testdata/echo_session_worker.py"})
	if n, _ := d.ClaimAndDispatch(context.Background()); n != 1 {
		t.Fatalf("claimed = %d, want 1", n)
	}
	row, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != task.StatusDone {
		t.Fatalf("status = %q, want done", row.Status)
	}
	if row.WorkerPgid.Valid {
		t.Fatalf("worker_pgid = %d after the redispatched worker exited, want NULL", row.WorkerPgid.Int64)
	}
}
