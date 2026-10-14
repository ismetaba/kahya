package briefing

import (
	"context"
	"errors"
	"testing"
	"time"

	"kahya/kahyad/internal/outbox"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
)

// FIX-I-14 review: the briefing's task row is 'executing' while its worker
// runs inside Run, but that worker is never registered as a worker and the
// row stores no envelope. These tests run the production resume scan and
// outbox dispatcher against a running and an offline briefing, wired the
// way main.go wires them (one task.LiveRegistry for the scan, the
// dispatcher and Orchestrator.Live).

// midRunSpawner runs fn while the "worker" is running, then answers raw.
type midRunSpawner struct {
	fn  func(env spawn.Envelope)
	raw string
	err error
}

func (s *midRunSpawner) Spawn(ctx context.Context, env spawn.Envelope) (string, error) {
	s.fn(env)
	return s.raw, s.err
}

func countEventKind(t *testing.T, st *store.Store, kind string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count events %s: %v", kind, err)
	}
	return n
}

// TestRunInFlightBriefingIsNotFailedByResumeScan: a resume-scan tick and an
// outbox-dispatch tick that land while the briefing worker runs (the
// worker's model call usually outlasts the 30 s scan interval) leave the
// briefing alone, and a briefing that then delivers ends 'done'. Before the
// review fix the scan enqueued it (crash_resumes 1), the dispatcher failed
// it (no envelope, G10-E2) and the delivered briefing ended 'failed'.
func TestRunInFlightBriefingIsNotFailedByResumeScan(t *testing.T) {
	st := newTestStore(t)
	m := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	var scanned, claimed int
	var midStatus string
	sp := &midRunSpawner{raw: `{"lines":["ozet"]}`}
	sp.fn = func(env spawn.Envelope) {
		row, err := st.Queries.GetTaskByID(context.Background(), env.TaskID)
		if err != nil {
			t.Errorf("GetTaskByID mid-run: %v", err)
		}
		midStatus = row.Status
		n, err := task.NewResume(st.Queries, st.Queries, st.Queries, m, nil, task.AnyLive(live), 3).Scan(context.Background())
		if err != nil {
			t.Errorf("scan: %v", err)
		}
		scanned = n
		d := outbox.NewDispatcher(st.Queries, st, m, spawn.Config{Cmd: []string{"/nonexistent/worker"}}, live)
		c, err := d.ClaimAndDispatch(context.Background())
		if err != nil {
			t.Errorf("dispatch: %v", err)
		}
		claimed = c
	}
	o := &Orchestrator{
		Classifier: permissiveClassifier(),
		Spawner:    sp,
		Delivery:   &fakeDelivery{Sent: true},
		TaskStore:  st.Queries,
		Machine:    m,
		Live:       live,
		Ledger:     st,
		Now:        fixedNow(time.Date(2026, 7, 12, 8, 30, 0, 0, time.UTC)),
	}
	res, err := o.Run(context.Background(), "trace-briefing-inflight")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Delivered {
		t.Fatal("briefing not delivered")
	}
	if midStatus != task.StatusExecuting {
		t.Fatalf("status while the worker ran = %q, want executing (the test must exercise a scan candidate)", midStatus)
	}
	if scanned != 0 || claimed != 0 {
		t.Errorf("mid-run resume scan examined %d, dispatcher claimed %d; want 0, 0 (the briefing is live)", scanned, claimed)
	}
	row, err := st.Queries.GetTaskByID(context.Background(), res.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != task.StatusDone || row.State != "done" {
		t.Errorf("final status/state = %q/%q, want done/done", row.Status, row.State)
	}
	if row.CrashResumes != 0 {
		t.Errorf("crash_resumes = %d, want 0", row.CrashResumes)
	}
	if n := countEventKind(t, st, "outbox.envelope_invalid"); n != 0 {
		t.Errorf("outbox.envelope_invalid rows = %d, want 0", n)
	}
	if live.IsLive(res.TaskID) {
		t.Error("the briefing's task is still live after Run returned")
	}
}

// TestRunOfflineBriefingEndsFailedNotParked: on an offline morning the
// briefing worker's proxy reports the outage (OnCloudUnreachable ->
// CloudRetry.ParkOrGiveUp, as srv.NewTaskProxy wires it). An envelope-less
// task can never be resumed, so it is failed, not parked: no outbox row,
// no "Ağ dönünce otomatik devam edecek" notice, nothing non-terminal for
// the next halt-all, and the dispatcher finds nothing to claim. Before the
// review fix the row stayed in bekliyor-yeniden-deneme forever.
func TestRunOfflineBriefingEndsFailedNotParked(t *testing.T) {
	st := newTestStore(t)
	m := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	cr := task.NewCloudRetry(st.Queries, st.Queries, m, st, nil, []time.Duration{time.Millisecond}, 0)
	sp := &midRunSpawner{err: errors.New("worker exited 1")}
	sp.fn = func(env spawn.Envelope) {
		if err := cr.ParkOrGiveUp(context.Background(), env.TraceID, env.TaskID); err != nil {
			t.Errorf("ParkOrGiveUp: %v", err)
		}
	}
	o := &Orchestrator{
		Classifier: permissiveClassifier(),
		Spawner:    sp,
		Delivery:   &fakeDelivery{Sent: true},
		TaskStore:  st.Queries,
		Machine:    m,
		Live:       live,
		Ledger:     st,
	}
	res, err := o.Run(context.Background(), "trace-briefing-offline")
	if err == nil {
		t.Fatal("Run succeeded, want the worker failure")
	}
	row, gerr := st.Queries.GetTaskByID(context.Background(), res.TaskID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if row.Status != task.StatusFailed || row.State != "error" {
		t.Errorf("status/state = %q/%q, want failed/error", row.Status, row.State)
	}
	var outboxRows int
	if err := st.DB().QueryRow(`SELECT count(*) FROM outbox`).Scan(&outboxRows); err != nil {
		t.Fatal(err)
	}
	if outboxRows != 0 {
		t.Errorf("outbox rows = %d, want 0", outboxRows)
	}
	if n := countEventKind(t, st, task.EventCloudWaitingRetry); n != 0 {
		t.Errorf("%s rows = %d, want 0", task.EventCloudWaitingRetry, n)
	}
	if n := countEventKind(t, st, "task.illegal_transition"); n != 0 {
		t.Errorf("task.illegal_transition rows = %d, want 0", n)
	}
	time.Sleep(5 * time.Millisecond)
	d := outbox.NewDispatcher(st.Queries, st, m, spawn.Config{Cmd: []string{"/nonexistent/worker"}}, live)
	if c, err := d.ClaimAndDispatch(context.Background()); err != nil || c != 0 {
		t.Errorf("ClaimAndDispatch = (%d, %v), want (0, nil)", c, err)
	}
}
