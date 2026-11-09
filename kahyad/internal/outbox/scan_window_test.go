package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store/sqlcgen"
	"kahya/kahyad/internal/task"
)

type countingNotifier struct {
	mu    sync.Mutex
	kinds []string
}

func (c *countingNotifier) Notify(_ context.Context, _, kind, _ string, _ map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kinds = append(c.kinds, kind)
	return nil
}

// TestResumeScanInRedispatchWindowNeverBlocksLastResume (FIX-FINAL
// resume-scan-blocks-redispatch-in-not-live-window): processResume moves
// the task to executing and only later registers the worker; a resume scan
// tick in that window found it executing-and-not-live with crash_resumes
// at the cap, moved it to blocked_user with the cap notice - and the
// dispatcher spawned the worker anyway, whose done transition was then
// refused. Here the tick lands inside the anthproxy opener (exactly that
// window): the task is live in-process and its claimed row still pending,
// so the scan leaves it alone and the redispatched worker ends it done.
func TestResumeScanInRedispatchWindowNeverBlocksLastResume(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	if _, err := st.DB().Exec(`UPDATE tasks SET crash_resumes = 3 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(task.OutboxTaskResumePayload{TaskID: "t1"})
	now := task.FixedNanoRFC3339(time.Now())
	if _, err := st.Queries.InsertOutboxRow(ctx, sqlcgen.InsertOutboxRowParams{
		TraceID: "trace-t1", Kind: task.OutboxKindTaskResume, Payload: string(payload),
		CreatedAt: now, AvailableAt: sql.NullString{String: now, Valid: true},
		TaskID: sql.NullString{String: "t1", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	m := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	notifier := &countingNotifier{}
	d := NewDispatcher(st.Queries, st, m, spawn.Config{Cmd: []string{"python3", "testdata/echo_session_worker.py"}}, live)
	r := task.NewResume(st.Queries, st.Queries, st.Queries, m, notifier, task.AnyLive(live), 3)
	var once sync.Once
	d.SetAnthproxyOpener(func(ctx context.Context, taskID, traceID string) (string, string, func() error, error) {
		once.Do(func() {
			if _, err := r.Scan(context.Background()); err != nil {
				t.Errorf("scan inside the redispatch window: %v", err)
			}
		})
		return "http://127.0.0.1:1", "k", func() error { return nil }, nil
	})
	if _, err := d.ClaimAndDispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := st.Queries.GetTaskByID(ctx, "t1"); cur.Status != task.StatusDone {
		t.Fatalf("final status = %q, want done (the scan must not block a task mid-dispatch)", cur.Status)
	}
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.kinds) != 0 {
		t.Fatalf("notices = %v, want none (no cap notice for a task that finished)", notifier.kinds)
	}
}

// TestRedispatchReparkedThenSucceededEndsDone (FIX-FINAL park-then-cli-
// retry-success-reruns-task, dispatcher half): a redispatched run that is
// re-parked mid-run (the proxy's park) and then finishes cleanly anyway
// tried retry-wait -> done, an illegal edge: the row was acked and the task
// stayed parked, to be re-run again. It now goes back to executing and
// ends done.
func TestRedispatchReparkedThenSucceededEndsDone(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	payload, _ := json.Marshal(task.OutboxTaskResumePayload{TaskID: "t1"})
	now := task.FixedNanoRFC3339(time.Now())
	if _, err := st.Queries.InsertOutboxRow(ctx, sqlcgen.InsertOutboxRowParams{
		TraceID: "trace-t1", Kind: task.OutboxKindTaskResume, Payload: string(payload),
		CreatedAt: now, AvailableAt: sql.NullString{String: now, Valid: true},
		TaskID: sql.NullString{String: "t1", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	m := task.NewMachine(st.Queries, st)
	d := NewDispatcher(st.Queries, st, m, spawn.Config{Cmd: []string{"python3", "testdata/echo_session_worker.py"}}, task.NewLiveRegistry())
	d.SetAnthproxyOpener(func(ctx context.Context, taskID, traceID string) (string, string, func() error, error) {
		// the proxy parks the redispatched task mid-run
		if err := m.Transition(ctx, traceID, taskID, task.StatusRetryWait); err != nil {
			t.Errorf("mid-run park: %v", err)
		}
		return "http://127.0.0.1:1", "k", func() error { return nil }, nil
	})
	if _, err := d.ClaimAndDispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := st.Queries.GetTaskByID(ctx, "t1"); cur.Status != task.StatusDone {
		t.Fatalf("final status = %q, want done", cur.Status)
	}
	var unparked int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, task.EventUnparkedOnSuccess).Scan(&unparked); err != nil || unparked != 1 {
		t.Fatalf("%s rows = %d (%v), want 1", task.EventUnparkedOnSuccess, unparked, err)
	}
}

// TestRedispatchMarksTheTaskLiveInProcess (FIX-FINAL flake-r2-dispatcher-
// inprocess-mark-untested): 69ad940 made two changes for the redispatch
// window and the test above passes with either one alone - the claimed,
// undelivered row already makes the resume scan return early. This pins
// the other one, processResume's in-process mark: inside the anthproxy
// opener (after the task went executing, before the worker's OnStart
// registers it) the task is live in the registry, which only the
// dispatcher's BeginInProcess can have done; once the dispatch returns the
// mark is gone. It fails when that block is removed - including the silent
// way, a d.live that no longer satisfies the BeginInProcess interface
// assertion.
func TestRedispatchMarksTheTaskLiveInProcess(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	insertTaskWithEnvelope(t, st, "t1", "sess-1")
	payload, _ := json.Marshal(task.OutboxTaskResumePayload{TaskID: "t1"})
	now := task.FixedNanoRFC3339(time.Now())
	if _, err := st.Queries.InsertOutboxRow(ctx, sqlcgen.InsertOutboxRowParams{
		TraceID: "trace-t1", Kind: task.OutboxKindTaskResume, Payload: string(payload),
		CreatedAt: now, AvailableAt: sql.NullString{String: now, Valid: true},
		TaskID: sql.NullString{String: "t1", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	m := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	if live.IsLive("t1") {
		t.Fatal("fixture: t1 is live before any dispatch")
	}
	d := NewDispatcher(st.Queries, st, m, spawn.Config{Cmd: []string{"python3", "testdata/echo_session_worker.py"}}, live)
	openerCalls, liveInWindow := 0, false
	var statusInWindow string
	d.SetAnthproxyOpener(func(ctx context.Context, taskID, traceID string) (string, string, func() error, error) {
		openerCalls++
		liveInWindow = live.IsLive(taskID)
		if cur, err := st.Queries.GetTaskByID(ctx, taskID); err == nil {
			statusInWindow = cur.Status
		}
		return "http://127.0.0.1:1", "k", func() error { return nil }, nil
	})
	if _, err := d.ClaimAndDispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if openerCalls != 1 {
		t.Fatalf("anthproxy opener calls = %d, want 1 (the window was never entered)", openerCalls)
	}
	if statusInWindow != task.StatusExecuting {
		t.Fatalf("status inside the opener = %q, want executing (not the redispatch window)", statusInWindow)
	}
	if !liveInWindow {
		t.Fatal("the task is executing but NOT live inside the redispatch window: processResume's BeginInProcess mark is missing")
	}
	if live.IsLive("t1") {
		t.Fatal("the task is still live after its dispatch returned: the in-process mark was never ended")
	}
	if cur, _ := st.Queries.GetTaskByID(ctx, "t1"); cur.Status != task.StatusDone {
		t.Fatalf("final status = %q, want done", cur.Status)
	}
}
