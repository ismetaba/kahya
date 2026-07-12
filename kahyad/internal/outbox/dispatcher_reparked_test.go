package outbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/task"
)

// TestReparkedTaskRowMarkedDeliveredNotPrematurelyReclaimed is the
// regression test for the W4-04 review's BLOCKER: when a redispatched
// task's cloud call exhausts inline retries AGAIN, the anthproxy
// OnCloudUnreachable callback (wired to task.CloudRetry.ParkOrGiveUp) fires
// synchronously mid-spawn.Run and enqueues a FRESH, correctly-scheduled
// outbox row (R2, available_at = next_retry_at). The CURRENTLY-claimed row
// (R1) used to be left unacknowledged with only its short 2-minute
// claim-lease governing it - so the dispatcher re-claimed R1 (and thus the
// task) far EARLIER than the park's own 5m/15m/60m schedule, bypassing the
// backoff entirely and spawning a new stale row on every failure.
//
// The fix marks R1 delivered whenever the post-run task status is a fresh
// park (bekliyor-yeniden-deneme) or terminal, so ONLY R2's schedule governs
// the next attempt. This test proves the task is NOT re-claimable in the
// window AFTER R1's old lease would have expired but BEFORE R2's
// next_retry_at - the exact window the reviewer reclaimed R1 in.
func TestReparkedTaskRowMarkedDeliveredNotPrematurelyReclaimed(t *testing.T) {
	st := testStore(t)
	insertTaskWithEnvelope(t, st, "t1", "sess-1") // status=executing, attempts=1

	t0 := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	cur := t0
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return cur }
	setNow := func(v time.Time) { mu.Lock(); cur = v; mu.Unlock() }

	m := task.NewMachine(st.Queries, st)
	m.SetClock(clock)
	// schedule[0]=1m (first park), schedule[1]=5m (the re-park after the
	// failed redispatch); give-up far in the future so it always parks.
	cr := task.NewCloudRetry(st.Queries, st.Queries, m, st, nil,
		[]time.Duration{time.Minute, 5 * time.Minute}, 30*24*time.Hour)
	cr.SetClock(clock)

	// Set up R1: park the task once (as the synchronous first-dispatch
	// exhaustion would), at t0 -> R1 available_at = t0+1m.
	if err := cr.ParkOrGiveUp(context.Background(), "trace-t1", "t1"); err != nil {
		t.Fatalf("initial ParkOrGiveUp: %v", err)
	}

	// An upstream that is ALWAYS 503: every redispatch's cloud call exhausts
	// inline retries and re-parks. MaxInlineRetries=1 + no-op Sleep so it
	// exhausts instantly with no real backoff wait.
	deadUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(deadUpstream.Close)

	governor := anthproxy.NewGovernor(anthproxy.Limits{
		DailyBudgetUSD: 1000, MonthlyBudgetUSD: 10000, TaskTokenCeiling: 500_000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)

	opener := func(_ context.Context, taskID, traceID string) (string, string, func() error, error) {
		token := "kahya-task-" + "cafebabecafebabecafebabecafebab2"
		p, err := anthproxy.New(anthproxy.ProxyConfig{
			TaskID: taskID, TraceID: traceID, Token: token,
			UpstreamURL: deadUpstream.URL, Governor: governor, EventLedger: st,
			MaxInlineRetries: 1,
			Sleep:            func(time.Duration) {},
			// The synchronous exhaustion callback: re-park exactly as the
			// first-dispatch path does (main.go wires this identically).
			OnCloudUnreachable: func(ctx context.Context, tid string) error {
				return cr.ParkOrGiveUp(ctx, traceID, tid)
			},
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
	d.SetLeaseDuration(2 * time.Minute)
	d.SetClock(clock)

	// --- First claim pass at t0+2m: R1 (due at t0+1m) is claimed, the
	// redispatch's cloud call fails, OnCloudUnreachable re-parks -> R2 at
	// next_retry_at (attempts=2 -> schedule[1]=5m -> t0+7m). ---
	setNow(t0.Add(2 * time.Minute))
	claimed, err := d.ClaimAndDispatch(context.Background())
	if err != nil {
		t.Fatalf("first ClaimAndDispatch: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("first claim = %d, want 1 (R1 was due)", claimed)
	}

	reparked, err := st.Queries.GetTaskByID(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskByID after re-park: %v", err)
	}
	if reparked.Status != task.StatusRetryWait {
		t.Fatalf("status after failed redispatch = %q, want %q", reparked.Status, task.StatusRetryWait)
	}
	if !reparked.NextRetryAt.Valid {
		t.Fatal("next_retry_at is NULL after re-park, want set")
	}
	nextRetryAt, perr := time.Parse(time.RFC3339Nano, reparked.NextRetryAt.String)
	if perr != nil {
		t.Fatalf("parse next_retry_at %q: %v", reparked.NextRetryAt.String, perr)
	}
	wantNext := t0.Add(7 * time.Minute) // t0+2m + schedule[1]=5m
	if !nextRetryAt.Equal(wantNext) {
		t.Fatalf("next_retry_at = %v, want %v (t0+2m + 5m schedule)", nextRetryAt, wantNext)
	}

	// --- THE REGRESSION ASSERTION: at t0+4m30s - well past R1's old
	// claim-lease (t0+2m + 2m = t0+4m) but BEFORE R2's next_retry_at
	// (t0+7m) - NOTHING is due. Before the fix, R1 sat unacknowledged with
	// dispatched_at=NULL and lease_until=t0+4m, so it was re-claimed here,
	// three-plus minutes early. ---
	setNow(t0.Add(4*time.Minute + 30*time.Second))
	claimed, err = d.ClaimAndDispatch(context.Background())
	if err != nil {
		t.Fatalf("intermediate ClaimAndDispatch: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("intermediate claim = %d, want 0 (R1 must be delivered; R2 not yet due) - premature reclaim regression", claimed)
	}

	// --- Sanity: once R2's next_retry_at HAS passed (t0+7m30s), the task
	// is claimable again exactly once (via R2, not a stale R1). ---
	setNow(t0.Add(7*time.Minute + 30*time.Second))
	claimed, err = d.ClaimAndDispatch(context.Background())
	if err != nil {
		t.Fatalf("final ClaimAndDispatch: %v", err)
	}
	if claimed != 1 {
		t.Fatalf("final claim = %d, want 1 (R2 now due)", claimed)
	}
}
