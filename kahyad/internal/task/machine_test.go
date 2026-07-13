package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// testStore builds a Machine against a real temp-file brain.db (the same
// store.Open path production uses - the same rationale
// kahyad/internal/policy/engine_test.go's testEngine gives for using a
// real store rather than a Go-level fake).
func testStore(t *testing.T) *store.Store {
	t.Helper()
	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db")}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// insertTask inserts a minimal tasks row (status defaults to 'intent' -
// migrations/0007_task_durability.sql's DEFAULT) and returns its id.
func insertTask(t *testing.T, st *store.Store, id string) sqlcgen.Task {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	row, err := st.Queries.InsertTask(context.Background(), sqlcgen.InsertTaskParams{
		ID: id, TraceID: "trace-" + id, SessionID: sql.NullString{},
		State: "running", UpdatedAt: now, CreatedAt: now,
		Lane: "normal",
	})
	if err != nil {
		t.Fatalf("insertTask: %v", err)
	}
	return row
}

func countEventsOfKind(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", kind, err)
	}
	return n
}

func taskStatus(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	row, err := st.Queries.GetTaskByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	return row.Status
}

func taskAttempts(t *testing.T, st *store.Store, id string) int64 {
	t.Helper()
	row, err := st.Queries.GetTaskByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	return row.Attempts
}

func TestNewTaskDefaultsToIntent(t *testing.T) {
	st := testStore(t)
	row := insertTask(t, st, "t_intent")
	if row.Status != StatusIntent {
		t.Errorf("Status = %q, want %q", row.Status, StatusIntent)
	}
	if row.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0", row.Attempts)
	}
}

// --- Legal transitions ---

func TestTransitionIntentToExecutingLegalAndBumpsAttempts(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)

	if err := m.Transition(context.Background(), "trace-1", "t1", StatusExecuting); err != nil {
		t.Fatalf("Transition() error = %v", err)
	}
	if got := taskStatus(t, st, "t1"); got != StatusExecuting {
		t.Errorf("status = %q, want %q", got, StatusExecuting)
	}
	if got := taskAttempts(t, st, "t1"); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
	if n := countEventsOfKind(t, st, EventTransition); n != 1 {
		t.Errorf("task.transition ledger events = %d, want 1", n)
	}
}

func TestLegalTransitionsFromExecuting(t *testing.T) {
	for _, to := range []string{StatusDone, StatusFailed, StatusBlockedUser, StatusRetryWait, StatusUserHalted} {
		to := to
		t.Run(to, func(t *testing.T) {
			st := testStore(t)
			id := "t-exec-" + to
			insertTask(t, st, id)
			m := NewMachine(st.Queries, st)
			if err := m.Transition(context.Background(), "trace", id, StatusExecuting); err != nil {
				t.Fatalf("Transition(->executing) error = %v", err)
			}
			if err := m.Transition(context.Background(), "trace", id, to); err != nil {
				t.Fatalf("Transition(executing->%s) error = %v", to, err)
			}
			if got := taskStatus(t, st, id); got != to {
				t.Errorf("status = %q, want %q", got, to)
			}
		})
	}
}

// TestBekliyorToUserHaltedLegal is the spec's explicit "incl.
// bekliyor-yeniden-deneme->user_halted legal" acceptance case (W6-03's
// ⌥⎋ must be able to reach a task that is merely PARKED waiting for a
// retry, not only one actively executing).
func TestBekliyorToUserHaltedLegal(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t2")
	m := NewMachine(st.Queries, st)
	ctx := context.Background()

	mustTransition(t, m, ctx, "t2", StatusExecuting)
	mustTransition(t, m, ctx, "t2", StatusRetryWait)

	if err := m.Transition(ctx, "trace", "t2", StatusUserHalted); err != nil {
		t.Fatalf("Transition(bekliyor-yeniden-deneme->user_halted) error = %v, want nil (legal)", err)
	}
	if got := taskStatus(t, st, "t2"); got != StatusUserHalted {
		t.Errorf("status = %q, want %q", got, StatusUserHalted)
	}
}

func TestBlockedUserToExecutingLegal(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t3")
	m := NewMachine(st.Queries, st)
	ctx := context.Background()

	mustTransition(t, m, ctx, "t3", StatusExecuting)
	mustTransition(t, m, ctx, "t3", StatusBlockedUser)

	if err := m.Transition(ctx, "trace", "t3", StatusExecuting); err != nil {
		t.Fatalf("Transition(blocked_user->executing) error = %v, want nil (legal)", err)
	}
	// A second dispatch into 'executing' is a second attempt.
	if got := taskAttempts(t, st, "t3"); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// --- Illegal transitions ---

// TestUserHaltedToExecutingIllegal is the spec's explicit
// "user_halted->executing illegal" acceptance case: a halted task is
// PERMANENTLY excluded from resume/retry (W6-03), never re-enterable via
// this state machine.
func TestUserHaltedToExecutingIllegal(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t4")
	m := NewMachine(st.Queries, st)
	ctx := context.Background()

	mustTransition(t, m, ctx, "t4", StatusExecuting)
	mustTransition(t, m, ctx, "t4", StatusUserHalted)

	err := m.Transition(ctx, "trace-illegal", "t4", StatusExecuting)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Transition(user_halted->executing) error = %v, want ErrIllegalTransition", err)
	}
	// status must NOT have moved.
	if got := taskStatus(t, st, "t4"); got != StatusUserHalted {
		t.Errorf("status = %q after illegal transition attempt, want unchanged %q", got, StatusUserHalted)
	}
	if n := countEventsOfKind(t, st, EventIllegalTransition); n != 1 {
		t.Errorf("task.illegal_transition ledger events = %d, want 1", n)
	}
}

func TestIntentToDoneIllegal(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t5")
	m := NewMachine(st.Queries, st)

	err := m.Transition(context.Background(), "trace", "t5", StatusDone)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Transition(intent->done) error = %v, want ErrIllegalTransition", err)
	}
}

func TestDoneIsTerminal(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t6")
	m := NewMachine(st.Queries, st)
	ctx := context.Background()
	mustTransition(t, m, ctx, "t6", StatusExecuting)
	mustTransition(t, m, ctx, "t6", StatusDone)

	for _, to := range []string{StatusExecuting, StatusFailed, StatusIntent, StatusBlockedUser} {
		if err := m.Transition(ctx, "trace", "t6", to); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("Transition(done->%s) error = %v, want ErrIllegalTransition", to, err)
		}
	}
}

// TestTransitionToSameStatusIsNoop proves re-affirming the current status
// is neither illegal nor itself ledgered as a transition (Transition's own
// doc comment) - this matters for the resume scan's within-cap W1
// receipt-less retry path, which re-dispatches a task that never left
// 'executing' at all.
func TestTransitionToSameStatusIsNoop(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t7")
	m := NewMachine(st.Queries, st)
	ctx := context.Background()
	mustTransition(t, m, ctx, "t7", StatusExecuting)

	before := countEventsOfKind(t, st, EventTransition)
	if err := m.Transition(ctx, "trace", "t7", StatusExecuting); err != nil {
		t.Fatalf("Transition(executing->executing) error = %v, want nil", err)
	}
	if got := taskAttempts(t, st, "t7"); got != 1 {
		t.Errorf("attempts = %d, want unchanged 1 (no-op transition must not bump it)", got)
	}
	if after := countEventsOfKind(t, st, EventTransition); after != before {
		t.Errorf("task.transition ledger events changed (%d -> %d), want no-op", before, after)
	}
}

// TestEveryTransitionEventJSONLAndLedgerAgree is the task spec's own
// "grep test: every task/tool state transition ledger event carries the
// task's trace_id (JSONL log + events rows agree)" acceptance criterion:
// a real *logx.Logger writes kahyad.jsonl under a temp log dir, and both
// a legal transition (task.transition) and an illegal one
// (task.illegal_transition) must appear in BOTH the JSONL file and the DB
// events table, under the SAME trace_id, for the SAME task_id/from/to.
func TestEveryTransitionEventJSONLAndLedgerAgree(t *testing.T) {
	st := testStore(t)
	logDir := t.TempDir()
	jsonl, err := logx.New(logDir, "boot-trace")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}

	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)
	m.SetJSONLLogger(jsonl)
	ctx := context.Background()
	const traceID = "trace-grep-agree"

	if err := m.Transition(ctx, traceID, "t1", StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
	if err := m.Transition(ctx, traceID, "t1", StatusIntent); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Transition(executing->intent) error = %v, want ErrIllegalTransition", err)
	}

	// DB side: both events rows exist under traceID.
	dbKinds := map[string]bool{}
	rows, err := st.DB().QueryContext(ctx, `SELECT kind FROM events WHERE trace_id = ? AND kind IN (?, ?)`, traceID, EventTransition, EventIllegalTransition)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan: %v", err)
		}
		dbKinds[kind] = true
	}
	rows.Close()
	if !dbKinds[EventTransition] || !dbKinds[EventIllegalTransition] {
		t.Fatalf("events table missing rows for trace_id=%s: got %v", traceID, dbKinds)
	}

	// JSONL side: grep kahyad.jsonl for both event kinds under the SAME
	// trace_id.
	raw, err := os.ReadFile(filepath.Join(logDir, "kahyad.jsonl"))
	if err != nil {
		t.Fatalf("read kahyad.jsonl: %v", err)
	}
	jsonlKinds := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("unmarshal jsonl line %q: %v", line, err)
		}
		event, _ := m["event"].(string)
		if event != EventTransition && event != EventIllegalTransition {
			continue
		}
		if got, _ := m["trace_id"].(string); got != traceID {
			t.Errorf("jsonl line event=%s trace_id = %q, want %q", event, got, traceID)
			continue
		}
		jsonlKinds[event] = true
	}
	if !jsonlKinds[EventTransition] || !jsonlKinds[EventIllegalTransition] {
		t.Fatalf("kahyad.jsonl missing lines for trace_id=%s: got %v", traceID, jsonlKinds)
	}

	// Agreement: the JSONL and DB sides observed the exact same set of
	// event kinds under the exact same trace_id.
	if len(dbKinds) != len(jsonlKinds) {
		t.Errorf("DB kinds %v and JSONL kinds %v disagree in count", dbKinds, jsonlKinds)
	}
	for kind := range dbKinds {
		if !jsonlKinds[kind] {
			t.Errorf("kind %q present in DB events but not in kahyad.jsonl", kind)
		}
	}
}

func TestEveryTransitionLedgerEventCarriesTraceID(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t8")
	m := NewMachine(st.Queries, st)
	ctx := context.Background()
	const traceID = "trace-t8-grep-me"

	if err := m.Transition(ctx, traceID, "t8", StatusExecuting); err != nil {
		t.Fatalf("Transition() error = %v", err)
	}
	_ = m.Transition(ctx, traceID, "t8", StatusExecuting) // illegal: not reachable from itself via a real edge but exercise anyway
	if err := m.Transition(ctx, traceID, "t8", StatusUserHalted); err != nil {
		t.Fatalf("Transition() error = %v", err)
	}
	_ = m.Transition(ctx, traceID, "t8", StatusExecuting) // illegal from user_halted

	rows, err := st.DB().QueryContext(ctx, `SELECT trace_id FROM events WHERE kind IN (?, ?)`, EventTransition, EventIllegalTransition)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		var got string
		if err := rows.Scan(&got); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if got != traceID {
			t.Errorf("event trace_id = %q, want %q", got, traceID)
		}
	}
	if n == 0 {
		t.Fatal("no task.transition/task.illegal_transition events found")
	}
}

func mustTransition(t *testing.T, m *Machine, ctx context.Context, taskID, to string) {
	t.Helper()
	if err := m.Transition(ctx, "trace", taskID, to); err != nil {
		t.Fatalf("Transition(-> %s) error = %v", to, err)
	}
}

// TestConcurrentTransitionsFromSameStatusExactlyOneWins is the BLOCKER 3
// regression test: Machine.Transition used to read tasks.status via
// GetTaskByID, validate against that possibly-stale read, then blindly
// `UPDATE tasks SET status=? WHERE id=?` with no WHERE-clause guard on the
// from-status and no rows-affected check - two concurrent LEGAL
// transitions racing from the SAME 'from' status could both "succeed"
// with no error to either caller, the second silently overwriting the
// first (last-write-wins, torn result). SetTaskStatus is now an atomic
// compare-and-set (`UPDATE ... WHERE id=? AND status=<from>`): of two
// concurrent Transition calls from 'executing' (one to 'done', one to
// 'user_halted'), exactly one must ever succeed; the loser must get an
// error (ErrLostTransitionRace in the interleaving this test is built to
// exercise - its own GetTaskByID read raced the winner's write; a rarer,
// still-correct interleaving can instead read the ALREADY-written status
// and get ErrIllegalTransition, which this test also accepts as
// non-torn); and tasks.status must land on EXACTLY one of the two
// destinations, never a blend of both and never silently discarded with
// no signal at all. Run 20 times under -race, since which interleaving
// actually happens is inherently timing-dependent - across 20 runs this
// asserts ErrLostTransitionRace (the bug's own failure mode) was actually
// observed at least once, so the test is not merely exercising the
// benign path by accident.
func TestConcurrentTransitionsFromSameStatusExactlyOneWins(t *testing.T) {
	sawLostRace := false
	for run := 0; run < 20; run++ {
		st := testStore(t)
		const id = "t-race"
		insertTask(t, st, id)
		m := NewMachine(st.Queries, st)
		ctx := context.Background()
		mustTransition(t, m, ctx, id, StatusExecuting)

		dests := [2]string{StatusDone, StatusUserHalted}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = m.Transition(ctx, "trace-race", id, dests[i])
			}()
		}
		wg.Wait()

		successCount := 0
		for i, err := range errs {
			switch {
			case err == nil:
				successCount++
			case errors.Is(err, ErrLostTransitionRace):
				sawLostRace = true
			case errors.Is(err, ErrIllegalTransition):
				// The rarer benign interleaving: this goroutine's
				// GetTaskByID read happened AFTER the other's write had
				// already fully committed, so its own (from, to) pair was
				// never legal to begin with - still not a torn write.
			default:
				t.Fatalf("run %d: Transition()[%d] (-> %s) error = %v, want nil, ErrLostTransitionRace, or ErrIllegalTransition",
					run, i, dests[i], err)
			}
		}
		if successCount != 1 {
			t.Fatalf("run %d: successCount = %d, want exactly 1 (never 0, never 2 - no torn/last-write-wins result)", run, successCount)
		}

		got := taskStatus(t, st, id)
		if got != StatusDone && got != StatusUserHalted {
			t.Fatalf("run %d: status = %q, want exactly one of %q/%q", run, got, StatusDone, StatusUserHalted)
		}
	}
	if !sawLostRace {
		t.Error("ErrLostTransitionRace was never observed across 20 runs - the compare-and-set race window may not be exercised at all")
	}
}
