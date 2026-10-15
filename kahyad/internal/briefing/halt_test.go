package briefing

import (
	"context"
	"testing"
	"time"

	"kahya/kahyad/internal/halt"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/task"
)

type haltingSpawner struct {
	exec    *halt.Executor
	halted  int
	ctxDone bool
}

func (h *haltingSpawner) Spawn(ctx context.Context, env spawn.Envelope) (string, error) {
	h.halted, _ = h.exec.HaltAll(context.Background()) // ⌥⎋ while the briefing worker runs
	select {
	case <-ctx.Done():
		h.ctxDone = true
		return "", ctx.Err()
	case <-time.After(5 * time.Second):
	}
	return `{"lines":["ozet"]}`, nil
}

// TestHaltedBriefingIsNeverDelivered (FIX-FINAL halt-does-not-stop-
// inprocess-sessions): the briefing's run has no registered worker pid, so
// HaltAll moved its row to user_halted while the run went on to redact and
// deliver the summary to Telegram/Hammerspoon. The halt now cancels the
// worker run's context, and a halted briefing is never delivered.
func TestHaltedBriefingIsNeverDelivered(t *testing.T) {
	st := newTestStore(t)
	m := task.NewMachine(st.Queries, st)
	live := task.NewLiveRegistry()
	sp := &haltingSpawner{exec: halt.NewExecutor(st.Queries, m, live, nil, nil, st)}
	del := &fakeDelivery{Sent: true}
	o := &Orchestrator{
		Classifier: permissiveClassifier(), Spawner: sp, Delivery: del,
		TaskStore: st.Queries, Machine: m, Live: live,
		Now: fixedNow(time.Date(2026, 7, 12, 8, 30, 0, 0, time.UTC)),
	}
	res, err := o.Run(context.Background(), "trace-briefing-halt")
	if sp.halted != 1 {
		t.Fatalf("fixture: HaltAll halted %d tasks, want 1", sp.halted)
	}
	if !sp.ctxDone {
		t.Error("the halt did not cancel the briefing worker's context")
	}
	if err == nil {
		t.Error("Run of a halted briefing returned no error")
	}
	if len(del.Calls) != 0 || res.Delivered {
		t.Errorf("a halted briefing was delivered (%d sends)", len(del.Calls))
	}
	row, gerr := st.Queries.GetTaskByID(context.Background(), res.TaskID)
	if gerr != nil || row.Status != task.StatusUserHalted {
		t.Errorf("task status = %q (%v), want user_halted", row.Status, gerr)
	}
}
