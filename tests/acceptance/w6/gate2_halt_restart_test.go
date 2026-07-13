//go:build acceptance

package w6gate

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestW6Gate2HaltSurvivesDaemonRestart is HANDOFF §6 W6's second acceptance
// clause, at its STRONGEST framing: "uzun görev sırasında ⌥⎋ → daemon
// yeniden başlasa bile görev devam ETMİYOR ve retry edilmiyor" - after a
// halt, even a full daemon restart must not resume or retry the task, and
// every pending approval stays dead.
//
// The flow: a long task (hung fake worker) has called an UNPROMOTED W2 tool
// (w2_slow_stub) so a pending_approvals row exists; POST /halt drives the
// terminal user_halted transition + approval invalidation + worker
// process-group SIGKILL; then the daemon is fully STOPPED, a due task_resume
// outbox row is injected directly into brain.db while it is down (simulating
// a retry that WOULD fire), and a FRESH daemon is started on the SAME
// dbPath. The gate proves the injected retry is refused, no new worker is
// ever spawned, the task stays user_halted, and the approval remains dead.
//
// Refusal-of-retry proof: the production dispatcher carries TWO
// deliberately-redundant user_halted guards, and this gate exercises BOTH by
// injecting two due task_resume rows after the halt (while the daemon is
// stopped, so this test's sqlite connection is the sole writer):
//   - a LINKED row (outbox.task_id == the halted task) - filtered at the SQL
//     claim layer by ListDueOutboxRows' `t.status != 'user_halted'` guard, so
//     it is never even claimed (dispatched_at stays NULL);
//   - an UNLINKED row (outbox.task_id NULL, payload still names the task) -
//     which PASSES the SQL candidate filter (t.id IS NULL branch) so the
//     dispatcher genuinely claims it, then refuses it in processResume's own
//     in-Go guard with an "outbox.redelivery_guarded" event and no worker.
//
// The unlinked row's redelivery_guarded event is the positive control that
// the restarted dispatch loop actually RAN and scanned (so the "no resume was
// dispatched" assertions are meaningful, not vacuously true because nothing
// scanned). The refusal itself is proven by the absence of any NEW
// "outbox.resume_dispatched"/"outbox.resume_skipped_live" event for the trace
// - the correct signal for an outbox-driven resume (task_spawned is emitted
// ONLY by the first-spawn path, never by a resume, so it cannot detect this
// failure mode) and one that stays green under either guard's safe outcome.
func TestW6Gate2HaltSurvivesDaemonRestart(t *testing.T) {
	pythonBin := findPython3(t)
	workerScript := filepath.Join(fixturesDir(t), "halt_hang_worker.py")

	pidFile := filepath.Join(t.TempDir(), "worker.pid")
	counterFile := filepath.Join(t.TempDir(), "counter.txt")

	d := bootKahyad(t, daemonOpts{
		workerCmd: []string{pythonBin, workerScript},
		extraEnv: []string{
			"KAHYA_W6_PID_FILE=" + pidFile,
			"KAHYA_W2_STUB_DURATION_MS=200",
			"KAHYA_W2_STUB_COUNTER_FILE=" + counterFile,
		},
	})

	// Deliberately do NOT promote w2_slow_stub: the unpromoted W2 call is
	// exactly what makes kahyad mint a pending_approvals row for the task.

	traceID := newTraceID()
	resp := d.postTask(t, traceID, "uzun görev")
	drainSSEAsync(resp)

	db := d.openDB(t)

	// 1. Task exists, its pending approval is minted, and the worker pid is
	// written.
	taskID := waitForTaskID(t, db, traceID, 15*time.Second)
	approvalID := pendingApprovalID(t, db, taskID, 15*time.Second)
	workerPID := readPIDFile(t, pidFile, 10*time.Second)
	t.Logf("task=%s approval=%s worker_pid=%d", taskID, approvalID, workerPID)

	// 2. Task is in a live, non-terminal in-progress state before the halt
	// (read from the DB directly - GET /v1/task/status can block on kahyad's
	// single brain.db connection during an in-flight effect).
	preStatus := waitForTaskStatusDB(t, db, taskID, 15*time.Second, "executing", "blocked_user")
	t.Logf("pre-halt task status = %q", preStatus)

	// 3. Halt exactly this task.
	if hr := d.haltTask(t, taskID); hr.Halted != 1 {
		t.Fatalf("POST /halt halted=%d (err=%q), want 1\n%s", hr.Halted, hr.Error, dumpLogs(d.dirs.homeDir))
	}

	// 4. Pre-restart assertions.
	//   (a) task is terminal user_halted.
	haltedStatus := waitForTaskStatusDB(t, db, taskID, 10*time.Second, "user_halted")
	if haltedStatus != "user_halted" {
		t.Fatalf("post-halt task status = %q, want user_halted", haltedStatus)
	}
	//   (b) the approval is dead: an approve decision now returns ok=false.
	//   pendingApprovalID (step 1) already proved this exact id was an
	//   UNCONSUMED (live) approval immediately before the halt, and "onayla" is
	//   the codebase-canonical W3 typed word (a wrong word would fail every
	//   pre-existing W3 approval test), so ok=false here is specifically the
	//   halt's invalidation, not an unrelated rejection.
	if dec := d.decideApproval(t, approvalID, true); dec.OK {
		t.Fatalf("approve decision on invalidated approval %s returned ok=true (token=%q) - the approval survived the halt", approvalID, dec.Token)
	}
	//   (c) the worker process was killed by the halt's process-group SIGKILL.
	if !waitForPIDGone(t, workerPID, 8*time.Second) {
		t.Fatalf("worker pid %d still alive after halt - the process-group SIGKILL did not reach it", workerPID)
	}

	// 5. Snapshot outbox event counts BEFORE the restart so any resume the
	// restarted daemon performs is detectable as a DELTA. A wrongful
	// outbox-driven resume runs through the dispatcher's processResume, which
	// emits "outbox.resume_dispatched" (NOT task_spawned - that kind is only
	// ever emitted by the first-spawn path); "outbox.resume_skipped_live"
	// would mean it was claimed while a worker was live. Either appearing for
	// this trace after restart is a halt-safety regression.
	resumeDispatchedBefore := countEventsMatching(t, db, traceID, "outbox.resume_dispatched")
	resumeSkippedLiveBefore := countEventsMatching(t, db, traceID, "outbox.resume_skipped_live")
	guardedBefore := countEventsMatching(t, db, traceID, "outbox.redelivery_guarded")

	// 6. Stop the daemon. Now it is DOWN and this test's sqlite connection is
	// the SOLE writer: inject TWO due, non-canceled task_resume rows that each
	// simulate a retry that WOULD fire, exercising BOTH of the production
	// dispatcher's redundant user_halted guards:
	//   - LINKED (outbox.task_id == taskID): filtered by the SQL claim guard,
	//     never claimed (dispatched_at stays NULL).
	//   - UNLINKED (outbox.task_id NULL, payload names the task): passes the
	//     SQL guard as a candidate, so it is genuinely CLAIMED (proving the
	//     restarted dispatch loop is alive and scanning) and then refused by
	//     processResume's in-Go guard with a redelivery_guarded event.
	d.stop()

	injectDueResumeRowLinked(t, db, traceID, taskID)
	injectDueResumeRowUnlinked(t, db, traceID, taskID)

	// 7. Start a fresh daemon on the SAME dirs/dbPath (1s resume/dispatch
	// ticks - the harness default - so the guards run fast).
	d.opts = daemonOpts{
		workerCmd: []string{pythonBin, workerScript},
		extraEnv: []string{
			"KAHYA_W6_PID_FILE=" + pidFile,
			"KAHYA_W2_STUB_DURATION_MS=200",
			"KAHYA_W2_STUB_COUNTER_FILE=" + counterFile,
		},
	}
	d.start()

	// 8. Poll until the injected UNLINKED row has been claimed+refused (its
	// redelivery_guarded event appears) - that is the positive proof the
	// dispatch loop actually RAN and scanned the injected rows, so the
	// negative assertions below (no resume dispatched) are meaningful, not
	// vacuously true because nothing scanned. Fail loudly if the loop never
	// fired within a generous window of 1s ticks.
	if !waitForEventCountAbove(t, db, traceID, "outbox.redelivery_guarded", guardedBefore, 15*time.Second) {
		t.Fatalf("no NEW outbox.redelivery_guarded event after restart - the dispatch loop never claimed the injected unlinked row (dispatcher not scanning?), so the no-resume assertions would be vacuous\n%s", dumpLogs(d.dirs.homeDir))
	}
	// A little extra settle time so a WRONGFUL resume of the linked row (if the
	// SQL guard were broken) would also have had ticks to fire.
	time.Sleep(2 * time.Second)

	//   (a) task STILL user_halted (never went back to executing/blocked_user).
	if st := taskStatusDB(t, db, taskID); st != "user_halted" {
		t.Fatalf("after restart task status = %q, want user_halted (task resumed after a halt+restart)", st)
	}

	//   (b) the halted task was NEVER re-dispatched or re-spawned by the
	//   outbox across the restart - zero NEW resume_dispatched / resume_skipped_live
	//   events. This is the correct retry-refusal signal (safe under BOTH the
	//   SQL-guard and the in-Go-guard outcomes: neither emits resume_dispatched).
	if n := countEventsMatching(t, db, traceID, "outbox.resume_dispatched"); n != resumeDispatchedBefore {
		t.Fatalf("outbox.resume_dispatched events for trace went %d -> %d after restart - the halted task's retry row was ACTED ON as a resume", resumeDispatchedBefore, n)
	}
	if n := countEventsMatching(t, db, traceID, "outbox.resume_skipped_live"); n != resumeSkippedLiveBefore {
		t.Fatalf("outbox.resume_skipped_live events for trace went %d -> %d after restart - the dispatcher claimed the halted task's row as a live resume", resumeSkippedLiveBefore, n)
	}

	//   (c) the LINKED retry row was refused at the SQL layer: never claimed,
	//   dispatched_at stays NULL (the ListDueOutboxRows user_halted guard).
	if linkedRowDispatched(t, db, taskID) {
		t.Fatalf("the LINKED injected task_resume row was dispatched (dispatched_at set) - the SQL-level user_halted claim guard did not filter it")
	}

	//   (d) no FRESH worker: the pid file still holds the ORIGINAL (now-dead)
	//   worker pid, unchanged - a resumed task would have spawned a new worker
	//   that overwrote it with a different pid. (Comparing the recorded pid,
	//   not probing liveness, so a recycled pid can never false-pass.)
	if got, err := readOptionalPID(pidFile); err != nil {
		t.Fatalf("pid file unreadable after restart: %v", err)
	} else if got != workerPID {
		t.Fatalf("pid file changed %d -> %d after restart - a fresh worker was spawned for the halted task", workerPID, got)
	}

	//   (e) approvals still dead after restart.
	if dec := d.decideApproval(t, approvalID, true); dec.OK {
		t.Fatalf("approve decision on approval %s returned ok=true after restart - the approval came back to life", approvalID)
	}
}

// availablePast is a clearly-past, fixed-9-digit-nanosecond RFC3339Nano
// value so the dispatcher's lexicographic `available_at <= now` dueness
// comparison is unambiguously true regardless of nanosecond padding.
const availablePast = "2000-01-01T00:00:00.000000000Z"

// injectDueResumeRowLinked inserts one due, non-canceled task_resume outbox
// row whose outbox.task_id COLUMN points at taskID - the row the production
// dispatcher's ListDueOutboxRows claim query must filter out at the SQL layer
// (its `AND (t.id IS NULL OR t.status != 'user_halted')` guard resolves FALSE
// for a user_halted owner), so it is never even claimed (dispatched_at stays
// NULL). Only ever called while the daemon is stopped, when this test's
// connection is the sole writer. Columns match kahyad/internal/store/queries/
// queries.sql's InsertOutboxRow shape.
func injectDueResumeRowLinked(t *testing.T, db *sql.DB, traceID, taskID string) {
	t.Helper()
	insertResumeRow(t, db, traceID, `{"task_id":"`+taskID+`"}`, sql.NullString{String: taskID, Valid: true})
}

// injectDueResumeRowUnlinked inserts one due, non-canceled task_resume outbox
// row whose outbox.task_id COLUMN is NULL but whose PAYLOAD still names the
// halted taskID. Because task_id is NULL, the ListDueOutboxRows LEFT JOIN
// yields t.id IS NULL and the SQL guard PASSES it through as a candidate - so
// the dispatcher genuinely CLAIMS and processes it (proving the dispatch loop
// is alive in the restarted daemon), and only processResume's OWN in-Go
// second-line guard (dispatcher.go: t.Status == user_halted ->
// EventRedeliveryGuarded + markDelivered, no worker spawned) refuses it. This
// is the dispatcher-liveness positive control: a "outbox.redelivery_guarded"
// event MUST appear, and NO "outbox.resume_dispatched" event must.
func injectDueResumeRowUnlinked(t *testing.T, db *sql.DB, traceID, taskID string) {
	t.Helper()
	insertResumeRow(t, db, traceID, `{"task_id":"`+taskID+`"}`, sql.NullString{})
}

func insertResumeRow(t *testing.T, db *sql.DB, traceID, payload string, taskIDCol sql.NullString) {
	t.Helper()
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Exec(
		`INSERT INTO outbox (trace_id, kind, payload, dispatched_at, created_at, available_at, lease_until, attempts, task_id, canceled_at)
		 VALUES (?, 'task_resume', ?, NULL, ?, ?, NULL, 0, ?, NULL)`,
		traceID, payload, createdAt, availablePast, taskIDCol,
	)
	if err != nil {
		t.Fatalf("inject due task_resume outbox row (task_id_col=%v): %v", taskIDCol, err)
	}
}

// linkedRowDispatched reports whether the LINKED injected task_resume row
// (outbox.task_id == taskID) was ever claimed/dispatched. It must NOT be: the
// SQL-level user_halted guard filters it before any claim, so dispatched_at
// stays NULL. (The UNLINKED row is expected to be claimed+delivered via the
// in-Go guard, so it is asserted separately, by its redelivery_guarded event -
// never by dispatched_at, which for a safe Go-guard outcome is legitimately
// set by markDelivered.)
func linkedRowDispatched(t *testing.T, db *sql.DB, taskID string) bool {
	t.Helper()
	var dispatchedAt sql.NullString
	err := db.QueryRow(
		`SELECT dispatched_at FROM outbox WHERE task_id = ? AND kind = 'task_resume' ORDER BY id DESC LIMIT 1`,
		taskID,
	).Scan(&dispatchedAt)
	if err != nil {
		t.Fatalf("query linked injected outbox row for task_id=%s: %v", taskID, err)
	}
	return dispatchedAt.Valid
}
