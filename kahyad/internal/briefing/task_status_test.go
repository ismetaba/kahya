package briefing

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/task"
)

// recordingMachine records every Transition and applies it through the real
// task.Machine (FIX-I-14).
type recordingMachine struct {
	inner *task.Machine
	mu    sync.Mutex
	calls []string
	ids   []string
}

func (m *recordingMachine) Transition(ctx context.Context, traceID, taskID, to string) error {
	m.mu.Lock()
	m.calls = append(m.calls, to)
	m.ids = append(m.ids, taskID)
	m.mu.Unlock()
	return m.inner.Transition(ctx, traceID, taskID, to)
}

// TestRunDrivesTaskStatusExecutingThenDone (FIX-I-14): the briefing's own
// task row used to stay at status 'intent' (and state 'running') forever -
// every ⌥⎋ then "halted" it. Run now walks it intent -> executing -> done
// (or failed) through the status machine and writes the matching state.
func TestRunDrivesTaskStatusExecutingThenDone(t *testing.T) {
	cases := []struct {
		name      string
		spawner   *fakeWorkerSpawner
		wantErr   bool
		wantCalls []string
		wantState string
	}{
		{"success", &fakeWorkerSpawner{RawJSON: `{"lines":["ozet"]}`}, false, []string{"executing", "done"}, "done"},
		{"spawn_error", &fakeWorkerSpawner{Err: errors.New("worker exited 1")}, true, []string{"executing", "failed"}, "error"},
		{"invalid_summary", &fakeWorkerSpawner{RawJSON: `not json`}, true, []string{"executing", "failed"}, "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newTestStore(t)
			machine := &recordingMachine{inner: task.NewMachine(st.Queries, st)}
			o := &Orchestrator{
				Classifier: permissiveClassifier(),
				Spawner:    c.spawner,
				Delivery:   &fakeDelivery{Sent: true},
				TaskStore:  st.Queries,
				Machine:    machine,
				Now:        fixedNow(time.Date(2026, 7, 12, 8, 30, 0, 0, time.UTC)),
			}
			result, err := o.Run(context.Background(), "trace-briefing-status")
			if (err != nil) != c.wantErr {
				t.Fatalf("Run error = %v, wantErr %v", err, c.wantErr)
			}
			if !reflect.DeepEqual(machine.calls, c.wantCalls) {
				t.Errorf("transitions = %v, want %v", machine.calls, c.wantCalls)
			}
			for _, id := range machine.ids {
				if id != result.TaskID {
					t.Errorf("transition for task %q, want %q", id, result.TaskID)
				}
			}
			row, err := st.Queries.GetTaskByID(context.Background(), result.TaskID)
			if err != nil {
				t.Fatalf("GetTaskByID: %v", err)
			}
			if row.State != c.wantState {
				t.Errorf("tasks.state = %q, want %q", row.State, c.wantState)
			}
			if want := c.wantCalls[len(c.wantCalls)-1]; row.Status != want {
				t.Errorf("tasks.status = %q, want %q", row.Status, want)
			}
		})
	}
}
