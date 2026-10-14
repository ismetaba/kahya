package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
)

// FIX-U-4 end to end: a cloud-lane task whose model call the proxy's
// secret-lane backstop refuses (403) ends 'failed' through the proxy's own
// callback - once, with one user notice - whatever the worker prints.

// alwaysSecretLaneLookup stands in for main.go's NewSecretLaneStoreAdapter:
// the task row is created lane=normal and the worker calls immediately, so
// flipping tasks.lane in time is racy - pin the verdict instead.
type alwaysSecretLaneLookup struct{}

func (alwaysSecretLaneLookup) GetTaskLane(context.Context, string) (lane, category string, found bool, err error) {
	return secretlane.LaneSecret, "finans", true, nil
}

// recordingNotices records Notify (CloudRetry) and Alarm (the secret-lane
// notice) calls.
type recordingNotices struct {
	mu     sync.Mutex
	notify []string
	alarms []string
}

func (r *recordingNotices) Notify(_ context.Context, _, kind, _ string, _ map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notify = append(r.notify, kind)
	return nil
}

func (r *recordingNotices) Alarm(_ context.Context, _, kind, _ string, _ map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alarms = append(r.alarms, kind)
	return nil
}

func (r *recordingNotices) counts() (notify, alarms int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.notify), len(r.alarms)
}

// wireDurabilityWithSecretLaneEgress is the production wiring of this path:
// a real CloudRetry (its notifier recorded), and the egress gate main.go
// builds - secretlane.ChainEgressGates(backstop, allowlist, notices).
func wireDurabilityWithSecretLaneEgress(rec *recordingNotices) func(srv *Server, st *store.Store) {
	return func(srv *Server, st *store.Store) {
		m := task.NewMachine(st.Queries, st)
		srv.SetTaskCloudRetry(m, task.NewCloudRetry(st.Queries, st.Queries, m, st, rec, nil, 0))
		srv.SetTaskDurability(task.NewResolver(st.Queries, st.Queries, m), st.Queries, nil)
		allowAll := func(string, string) func(*http.Request) error {
			return func(*http.Request) error { return nil }
		}
		gate := secretlane.ChainEgressGates(secretlane.NewProxyBackstopHook(alwaysSecretLaneLookup{}, st), allowAll, secretlane.NewCloudBlockedNotices(rec))
		srv.SetAnthproxy(serverTestGovernor(), nil, anthproxy.NewPassthroughCredentialSource(), gate)
	}
}

func serverTestGovernor() *anthproxy.Governor {
	return anthproxy.NewGovernor(anthproxy.Limits{
		DailyBudgetUSD: 1000, MonthlyBudgetUSD: 10000, TaskTokenCeiling: 500000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
}

func countingUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func taskFailedPayload(t *testing.T, st *store.Store, traceID string) map[string]any {
	t.Helper()
	var raw string
	if err := st.DB().QueryRow(`SELECT payload FROM events WHERE trace_id = ? AND kind = ?`, traceID, task.EventCloudTaskFailed).Scan(&raw); err != nil {
		t.Fatalf("read task.failed payload: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return p
}

func assertEgressTerminalLedger(t *testing.T, f taskTestFixture, traceID string, rec *recordingNotices, upstreamHits int32) {
	t.Helper()
	if n := countEventsByTraceKind(t, f.store, traceID, secretlane.EventSecretLaneCloudBlocked); n < 1 {
		t.Fatalf("secretlane_cloud_blocked = %d, want >= 1", n)
	}
	if n := countEventsByTraceKind(t, f.store, traceID, anthproxy.EventEgressBlockedTerminal); n != 1 {
		t.Fatalf("%s = %d, want exactly 1", anthproxy.EventEgressBlockedTerminal, n)
	}
	if n := countEventsByTraceKind(t, f.store, traceID, task.EventCloudTaskFailed); n != 1 {
		t.Fatalf("task.failed = %d, want exactly 1", n)
	}
	p := taskFailedPayload(t, f.store, traceID)
	if p["reason"] != anthproxy.ReasonSecretLaneCloudBlocked || p["notified_by"] != task.NotifiedByEgressGate {
		t.Fatalf("task.failed payload = %v, want reason %s + notified_by %s", p, anthproxy.ReasonSecretLaneCloudBlocked, task.NotifiedByEgressGate)
	}
	notifyCalls, alarms := rec.counts()
	if notifyCalls != 0 {
		t.Fatalf("CloudRetry notifications = %d, want 0 (the secret-lane notice is the one message)", notifyCalls)
	}
	if alarms != 1 {
		t.Fatalf("secret-lane notices = %d, want exactly 1", alarms)
	}
	if upstreamHits != 0 {
		t.Fatalf("upstream hits = %d, want 0", upstreamHits)
	}
}

func TestTaskEgress403MovesStatusToFailedWithReason(t *testing.T) {
	upstream, hits := countingUpstream(t)
	rec := &recordingNotices{}
	f := newTaskTestFixtureWithOptions(t, modelCloudWorkerScript(t), 5, upstream.URL, serverTestGovernor(), wireDurabilityWithSecretLaneEgress(rec))
	const traceID = "u4egress0000000000000000000000a1"

	frames := readAllSSE(t, postTask(t, f.client, traceID, "merhaba, bugun hava nasil?"))
	_, status, state := taskRowByTrace(t, f.store, traceID)
	if status != task.StatusFailed || state != "error" {
		t.Fatalf("task status/state = %s/%s, want failed/error", status, state)
	}
	if ev, msg := lastSSEMessage(t, frames); ev != "error" || msg != "cloud call failed with status 403" {
		t.Fatalf("last SSE = %s %q, want the worker's error line", ev, msg)
	}
	assertEgressTerminalLedger(t, f, traceID, rec, hits.Load())

	// No second spawn: a failed task is never a resume candidate.
	m := task.NewMachine(f.store.Queries, f.store)
	n, err := task.NewResume(f.store.Queries, f.store.Queries, f.store.Queries, m, nil, nil, 3).Scan(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("resume scan = (%d, %v), want (0, nil)", n, err)
	}
}

// TestTaskEgress403IsTerminalEvenIfWorkerReportsOK: a worker that ignores
// the 403 and prints "result ok" cannot turn the task done - kahyad's own
// verdict decides tasks.status; the worker line only sets the free-text
// state/SSE (decidePostSpawn leaves 'failed' untouched).
func TestTaskEgress403IsTerminalEvenIfWorkerReportsOK(t *testing.T) {
	upstream, hits := countingUpstream(t)
	rec := &recordingNotices{}
	worker := []string{"python3", filepath.Join("testdata", "egress_blocked_ok_worker.py")}
	f := newTaskTestFixtureWithOptions(t, worker, 5, upstream.URL, serverTestGovernor(), wireDurabilityWithSecretLaneEgress(rec))
	const traceID = "u4egress0000000000000000000000b2"

	frames := readAllSSE(t, postTask(t, f.client, traceID, "merhaba, bugun hava nasil?"))
	_, status, state := taskRowByTrace(t, f.store, traceID)
	if status != task.StatusFailed {
		t.Fatalf("task status = %s, want failed (the worker's 'result ok' must not make it done)", status)
	}
	if state != "done" {
		t.Fatalf("task state = %s, want done (the free-text state mirrors the worker's line)", state)
	}
	if ev, _ := lastSSEMessage(t, frames); ev != "result" {
		t.Fatalf("last SSE event = %s, want result", ev)
	}
	assertEgressTerminalLedger(t, f, traceID, rec, hits.Load())
}
