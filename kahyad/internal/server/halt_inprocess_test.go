package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"kahya/kahyad/internal/halt"
	"kahya/kahyad/internal/notify"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
)

type countingSpeaker struct{ n atomic.Int32 }

func (c *countingSpeaker) Speak(ctx context.Context, req notify.SpeakRequest) { c.n.Add(1) }

// TestHaltDuringLocalAnswerCancelsAndNeverDeliversOrSpeaks (FIX-FINAL
// halt-does-not-stop-inprocess-sessions): a secret-lane local answer has
// no worker pid, so HaltAll used to move the row to user_halted and count
// it while nothing was cancelled - the answer then streamed as a 'result'
// (state done) and was spoken after the halt had silenced speech. The halt
// now cancels the answer's context, and its output is dropped.
func TestHaltDuringLocalAnswerCancelsAndNeverDeliversOrSpeaks(t *testing.T) {
	var exec *halt.Executor
	var halted int
	var ctxErr error
	sp := &countingSpeaker{}
	answerer := secretlane.AnswererFunc(func(ctx context.Context, prompt string) (string, error) {
		halted, _ = exec.HaltAll(context.Background())
		select {
		case <-ctx.Done():
			ctxErr = ctx.Err()
		case <-time.After(5 * time.Second):
		}
		return "Yerelde işlendi.", nil
	})
	f := newTaskTestFixtureWithOptions(t, []string{"/nonexistent/worker"}, 5, "https://upstream.invalid", generousTestGovernor(), func(srv *Server, st *store.Store) {
		secretLanePre(answerer)(srv, st)
		live := task.NewLiveRegistry()
		srv.SetTaskLiveRegistry(live)
		srv.SetSpeaker(sp)
		exec = halt.NewExecutor(st.Queries, task.NewMachine(st.Queries, st), live, nil, nil, st)
	})
	traceID := "trace-sl-halt-000000000000000001"
	resp := postTask(t, f.client, traceID, "kredi kartı ekstresi ekte, özetler misin")
	frames := readAllSSE(t, resp)
	if halted != 1 {
		t.Fatalf("fixture: HaltAll halted %d tasks, want 1", halted)
	}
	if ctxErr == nil {
		t.Error("the halt did not cancel the local answer's context")
	}
	for _, fr := range frames {
		if fr.event == "result" || fr.event == "delta" {
			t.Errorf("a halted local answer was still delivered: %s %s", fr.event, fr.data)
		}
	}
	if n := sp.n.Load(); n != 0 {
		t.Errorf("a halted local answer was spoken %d time(s)", n)
	}
	if _, status, state := taskRowByTrace(t, f.store, traceID); status != task.StatusUserHalted || state == "done" {
		t.Errorf("task status=%s state=%s, want user_halted and never done", status, state)
	}
}
