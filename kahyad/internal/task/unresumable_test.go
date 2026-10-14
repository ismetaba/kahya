package task

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/store/sqlcgen"
)

// FIX-I-14 review: local answers and the briefing now walk the status
// machine, so the resume scan, the cloud-retry park and the budget pause
// meet task rows the outbox can never redispatch. These tests pin how the
// task package tells them apart and keeps them out of a worker.

// insertTaskWithoutEnvelope inserts a tasks row with no stored envelope -
// the shape of the briefing's row and of a locally answered task's.
func insertTaskWithoutEnvelope(t *testing.T, st *store.Store, id, lane string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := st.Queries.InsertTask(context.Background(), sqlcgen.InsertTaskParams{
		ID: id, TraceID: "trace-" + id, State: "running", UpdatedAt: now, CreatedAt: now, Lane: lane,
	}); err != nil {
		t.Fatalf("InsertTask: %v", err)
	}
}

// TestLaneSecretMatchesSpawn pins the task package's own copy of the
// secret-lane value to the envelope's (the "duplicate rather than import"
// convention spawn.LaneSecret itself follows).
func TestLaneSecretMatchesSpawn(t *testing.T) {
	if laneSecret != spawn.LaneSecret {
		t.Fatalf("laneSecret = %q, spawn.LaneSecret = %q", laneSecret, spawn.LaneSecret)
	}
}

func TestResumableNeedsStoredEnvelopeAndNonSecretLane(t *testing.T) {
	env := sql.NullString{String: `{"schema_version":1}`, Valid: true}
	for _, tc := range []struct {
		name string
		t    sqlcgen.Task
		want bool
	}{
		{"cloud_task", sqlcgen.Task{Envelope: env, Lane: "normal"}, true},
		{"legacy_empty_lane", sqlcgen.Task{Envelope: env}, true},
		{"no_envelope", sqlcgen.Task{Lane: "normal"}, false},
		{"empty_envelope", sqlcgen.Task{Envelope: sql.NullString{Valid: true}, Lane: "normal"}, false},
		{"secret_lane_with_envelope", sqlcgen.Task{Envelope: env, Lane: "secret"}, false},
	} {
		if got := Resumable(tc.t); got != tc.want {
			t.Errorf("%s: Resumable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestParkOrGiveUpFailsTaskWithoutStoredEnvelope: an envelope-less task
// (the briefing's - its worker's proxy calls OnCloudUnreachable on an
// offline morning) is never parked: nothing could ever resume it, so a park
// would leave it in bekliyor-yeniden-deneme forever, promise the user
// "Ağ dönünce otomatik devam edecek" and be swept by the next halt-all.
// It fails instead - one task.failed row, no outbox row, no notice - and a
// repeat report of the same outage changes nothing.
func TestParkOrGiveUpFailsTaskWithoutStoredEnvelope(t *testing.T) {
	st, m, cr, notifier := newCloudRetryFixture(t, []time.Duration{time.Minute}, 24*time.Hour)
	ctx := context.Background()
	insertTaskWithoutEnvelope(t, st, "b1", "normal")
	transitionToExecuting(t, m, "trace-b1", "b1")

	for i := 0; i < 2; i++ {
		if err := cr.ParkOrGiveUp(ctx, "trace-b1", "b1"); err != nil {
			t.Fatalf("ParkOrGiveUp #%d: %v", i, err)
		}
	}

	if got := taskStatus(t, st, "b1"); got != StatusFailed {
		t.Errorf("status = %q, want failed", got)
	}
	var outboxRows int
	if err := st.DB().QueryRow(`SELECT count(*) FROM outbox`).Scan(&outboxRows); err != nil {
		t.Fatal(err)
	}
	if outboxRows != 0 {
		t.Errorf("outbox rows = %d, want 0 (nothing can resume it)", outboxRows)
	}
	if n := countEventsOfKind(t, st, EventCloudWaitingRetry); n != 0 {
		t.Errorf("%s rows = %d, want 0", EventCloudWaitingRetry, n)
	}
	if n := countEventsOfKind(t, st, EventCloudTaskFailed); n != 1 {
		t.Errorf("%s rows = %d, want 1", EventCloudTaskFailed, n)
	}
	var reason string
	if err := st.DB().QueryRow(`SELECT json_extract(payload, '$.reason') FROM events WHERE kind = ?`, EventCloudTaskFailed).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != ReasonNotResumable {
		t.Errorf("task.failed reason = %q, want %q", reason, ReasonNotResumable)
	}
	if n := countEventsOfKind(t, st, "task.illegal_transition"); n != 0 {
		t.Errorf("task.illegal_transition rows = %d, want 0", n)
	}
	if len(notifier.calls) != 0 {
		t.Errorf("notifications = %+v, want none (the park notice would be a false promise)", notifier.calls)
	}
}

// TestParkOrGiveUpStillParksResumableTask is the control: a task that
// stored its envelope (every POST /v1/task cloud task) is parked as before.
func TestParkOrGiveUpStillParksResumableTask(t *testing.T) {
	st, m, cr, notifier := newCloudRetryFixture(t, []time.Duration{time.Minute}, 24*time.Hour)
	insertTask(t, st, "t1")
	transitionToExecuting(t, m, "trace-t1", "t1")
	if err := cr.ParkOrGiveUp(context.Background(), "trace-t1", "t1"); err != nil {
		t.Fatalf("ParkOrGiveUp: %v", err)
	}
	if got := taskStatus(t, st, "t1"); got != StatusRetryWait {
		t.Errorf("status = %q, want %q", got, StatusRetryWait)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].message != MsgCloudParked {
		t.Errorf("notifications = %+v, want exactly the park notice", notifier.calls)
	}
}

// TestResumeScanSkipsInProcessSession: a task whose session runs inside
// kahyad (LiveRegistry.BeginInProcess - a local answer, a briefing run) is
// live for the scan; once the session ended (after its terminal status) it
// is no longer a candidate at all.
func TestResumeScanSkipsInProcessSession(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	m := NewMachine(st.Queries, st)
	live := NewLiveRegistry()
	resume := NewResume(st.Queries, st.Queries, st.Queries, m, nil, AnyLive(live), 3)

	insertTaskWithoutEnvelope(t, st, "l1", "secret")
	end := live.BeginInProcess("l1")
	transitionToExecuting(t, m, "trace-l1", "l1")

	if n, err := resume.Scan(ctx); err != nil || n != 0 {
		t.Fatalf("Scan during the in-process session = (%d, %v), want (0, nil)", n, err)
	}
	var outboxRows int
	if err := st.DB().QueryRow(`SELECT count(*) FROM outbox`).Scan(&outboxRows); err != nil {
		t.Fatal(err)
	}
	if outboxRows != 0 {
		t.Fatalf("outbox rows = %d, want 0", outboxRows)
	}
	if err := m.Transition(ctx, "trace-l1", "l1", StatusDone); err != nil {
		t.Fatal(err)
	}
	end()
	if n, err := resume.Scan(ctx); err != nil || n != 0 {
		t.Fatalf("Scan after the session = (%d, %v), want (0, nil)", n, err)
	}
	if got := taskStatus(t, st, "l1"); got != StatusDone {
		t.Errorf("status = %q, want done", got)
	}
}
