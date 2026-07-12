package outbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/task"
)

// TestDispatcherRetriesParkedTaskAfterCloudHeals is the step-8 "always-503
// ... dispatcher (fake clock) retries; upstream healed ⇒ task done" test:
// a task already parked in bekliyor-yeniden-deneme by
// kahyad/internal/task.CloudRetry.park (simulating the SYNCHRONOUS
// first-dispatch path's own inline-retry-exhaustion, proven independently
// by kahyad/internal/anthproxy's own TestRetryExhaustionInvokesOnCloud
// Unreachable) is picked up by the SAME outbox redelivery mechanism
// (kahyad/internal/task.OutboxKindTaskResume + this package's
// ClaimAndDispatch - no new mechanism, task spec step 3) once its
// next_retry_at has passed (a fake dispatcher clock, so this test never
// actually sleeps), opens a FRESH per-redispatch anthproxy.Proxy
// (AnthproxyOpener, this task's own fix for the W4-02-documented gap),
// and completes 'done' once the fake upstream has healed.
func TestDispatcherRetriesParkedTaskAfterCloudHeals(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1") // status=executing, attempts=1

	t0 := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	m := task.NewMachine(st.Queries, st)
	m.SetClock(func() time.Time { return t0 })
	cr := task.NewCloudRetry(st.Queries, st.Queries, m, st, nil,
		[]time.Duration{time.Minute}, 24*time.Hour)
	cr.SetClock(func() time.Time { return t0 })

	// Simulates the synchronous first-dispatch path's own proxy callback
	// (kahyad/internal/anthproxy.ProxyConfig.OnCloudUnreachable) firing
	// once inline retries were exhausted against an always-503 upstream -
	// proven independently at the anthproxy layer.
	if err := cr.ParkOrGiveUp(context.Background(), "trace-t1", "t1"); err != nil {
		t.Fatalf("ParkOrGiveUp: %v", err)
	}

	parked, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if parked.Status != task.StatusRetryWait {
		t.Fatalf("status after park = %q, want %q", parked.Status, task.StatusRetryWait)
	}
	if !parked.NextRetryAt.Valid {
		t.Fatal("next_retry_at is NULL, want set")
	}

	// The fake upstream is already "healed" (always 200) - this test's
	// own point is that the DISPATCHER successfully reaches it again via
	// a freshly-opened proxy, not that the upstream itself flips
	// mid-test (that half is anthproxy's own retry-loop test).
	healedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`))
	}))
	t.Cleanup(healedUpstream.Close)

	governor := anthproxy.NewGovernor(anthproxy.Limits{
		DailyBudgetUSD: 1000, MonthlyBudgetUSD: 10000, TaskTokenCeiling: 500_000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)

	opener := func(_ context.Context, taskID, traceID string) (string, string, func() error, error) {
		token := "kahya-task-" + "cafebabecafebabecafebabecafebab1"
		p, err := anthproxy.New(anthproxy.ProxyConfig{
			TaskID: taskID, TraceID: traceID, Token: token,
			UpstreamURL: healedUpstream.URL, Governor: governor, EventLedger: st,
		})
		if err != nil {
			return "", "", nil, err
		}
		baseURL, err := p.Start()
		if err != nil {
			return "", "", nil, err
		}
		return baseURL, token, p.Close, nil
	}

	d := NewDispatcher(st.Queries, st, m, spawn.Config{Cmd: []string{"python3", "testdata/cloud_call_worker.py"}}, nil)
	d.SetAnthproxyOpener(opener)
	// Fake clock strictly past next_retry_at (~t0+1m) - proves the
	// dispatcher's OWN due-row check, not a real elapsed sleep, is what
	// makes the row claimable ("dispatcher (fake clock) retries").
	d.SetClock(func() time.Time { return t0.Add(2 * time.Minute) })

	claimed, err := d.ClaimAndDispatch(context.Background())
	if err != nil {
		t.Fatalf("ClaimAndDispatch() error = %v", err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1 (the parked task's own retry row must be due)", claimed)
	}

	got, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status after healed redispatch = %q, want %q", got.Status, task.StatusDone)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (1 for the original dispatch, 1 for this healed redispatch)", got.Attempts)
	}
}
