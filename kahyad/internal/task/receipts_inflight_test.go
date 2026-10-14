package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

// FIX-I-12 companion: with the effect no longer pinning brain.db's single
// connection, the periodic resume scan runs WHILE an effect is in flight.
// It must treat a task whose side effect is still executing inside kahyad
// as live - otherwise W4-07 scenario A (worker killed mid-call, effect
// still completing on kahyad's own goroutine) blocks a W2 task whose
// receipt is about to land.

type staticLive map[string]bool

func (s staticLive) IsLive(taskID string) bool { return s[taskID] }

func TestAnyLiveIsLiveWhenAnyCheckerIs(t *testing.T) {
	live := AnyLive(nil, staticLive{"a": true}, staticLive{"b": true})
	if !live.IsLive("a") || !live.IsLive("b") {
		t.Fatal("AnyLive must report a task live when any checker does")
	}
	if live.IsLive("c") {
		t.Fatal("AnyLive reported an unknown task live")
	}
	if AnyLive().IsLive("a") || AnyLive(nil).IsLive("a") {
		t.Fatal("an empty AnyLive must report nothing live")
	}
}

func TestReceiptsIsLiveOnlyWhileEffectInFlight(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	insertTask(t, st, "t2")
	r := newReceipts(st)
	if r.IsLive("t1") {
		t.Fatal("IsLive(t1) before any call = true")
	}
	inEffect := make(chan bool, 2)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, err := r.Execute(context.Background(), ExecuteInput{
			TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW2, ArgsHash: HashArgs([]byte("live")),
		}, func(ctx context.Context, _ *sql.Tx) (json.RawMessage, error) {
			inEffect <- r.IsLive("t1")
			inEffect <- r.IsLive("t2")
			<-release
			return json.RawMessage(`{"ok":true}`), nil
		})
		done <- err
	}()
	if !<-inEffect {
		t.Fatal("IsLive(t1) during its own effect = false, want true")
	}
	if <-inEffect {
		t.Fatal("IsLive(t2) during t1's effect = true, want false (per task)")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.IsLive("t1") {
		t.Fatal("IsLive(t1) after Execute returned = true, want false")
	}
	// A failing effect releases the mark too.
	_, _, _ = r.ExecuteInTx(context.Background(), ExecuteInput{
		TaskID: "t1", TraceID: "trace-1", ToolName: "stub_write", Class: ClassW1, ArgsHash: HashArgs([]byte("fail")),
	}, (&stubTool{}).failingEffect("boom"))
	if r.IsLive("t1") {
		t.Fatal("IsLive(t1) after a failed ExecuteInTx = true, want false")
	}
}

// TestResumeScanLeavesTaskWithEffectInFlightAlone is W4-07 scenario A at
// unit level: the worker is gone (no worker registry entry), a W2 effect
// is still running inside kahyad. The scan must neither fail the row nor
// block the task; once the receipt lands, the next scan resumes it.
func TestResumeScanLeavesTaskWithEffectInFlightAlone(t *testing.T) {
	st := testStore(t)
	insertTask(t, st, "t1")
	m := NewMachine(st.Queries, st)
	if err := m.Transition(context.Background(), "trace-t1", "t1", StatusExecuting); err != nil {
		t.Fatalf("Transition(->executing): %v", err)
	}
	r := newReceipts(st)
	notifier := &fakeNotifier{}
	resume := NewResume(st.Queries, st.Queries, st.Queries, m, notifier, AnyLive(NewLiveRegistry(), r), 3)

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, err := r.Execute(context.Background(), ExecuteInput{
			TaskID: "t1", TraceID: "trace-t1", ToolName: "w2_slow_stub", Class: ClassW2, ArgsHash: HashArgs([]byte("scenario-a")),
		}, func(ctx context.Context, _ *sql.Tx) (json.RawMessage, error) {
			close(started)
			<-release
			return json.RawMessage(`{"ok":true}`), nil
		})
		done <- err
	}()
	<-started

	n, err := resume.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan during effect: %v", err)
	}
	if n != 0 {
		t.Fatalf("Scan examined %d tasks during the in-flight effect, want 0 (the task is live)", n)
	}
	if got := taskStatus(t, st, "t1"); got != StatusExecuting {
		t.Fatalf("task status during effect = %q, want executing", got)
	}
	if got := countToolCalls(t, st, "t1", CallStatusExecuting); got != 1 {
		t.Fatalf("executing tool_calls during effect = %d, want 1 (the scan must not fail an in-flight row)", got)
	}
	if len(notifier.calls) != 0 {
		t.Fatalf("notifications during effect = %+v, want none", notifier.calls)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n, err := resume.Scan(context.Background()); err != nil || n != 1 {
		t.Fatalf("Scan after the receipt = (%d, %v), want (1, nil)", n, err)
	}
	if got := countOutboxResumeRows(t, st); got != 1 {
		t.Fatalf("outbox resume rows = %d, want 1 (receipt landed: plain resume, replay on redispatch)", got)
	}
	if got := taskStatus(t, st, "t1"); got != StatusExecuting {
		t.Fatalf("task status after scan = %q, want executing (resumed, not blocked)", got)
	}
	if got := countToolCalls(t, st, "t1", CallStatusReceipt); got != 1 {
		t.Fatalf("receipt tool_calls = %d, want 1", got)
	}
}
