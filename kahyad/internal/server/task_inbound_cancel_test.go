package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
)

// TestTaskTimeoutLeavesStatusExecutingNotFailed (FIX-I-6): a worker killed
// by its task deadline while its cloud call is still open through the
// per-task proxy must NOT be failed by the proxy. Before the fix the
// cancelled inbound request surfaced as a bare context.Canceled, the
// taxonomy called it NonRetryable, and OnNonRetryableFailure moved the task
// to 'failed' ("transport_error") - a terminal state the resume scan never
// looks at. After: the proxy logs proxy.inbound_canceled and fires nothing;
// the task stays 'executing' for the resume scan.
//
// The worker (model_cloud_worker.py) holds one /v1/messages call open
// (urlopen timeout 10s) against an upstream that never answers; the task
// deadline (2s) kills it first.
func TestTaskTimeoutLeavesStatusExecutingNotFailed(t *testing.T) {
	slowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body first: net/http only notices a client hang-up
		// (and cancels r.Context()) once the request body hit EOF.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	t.Cleanup(slowUpstream.Close)

	pre := func(srv *Server, st *store.Store) {
		wireDurabilityWithCloudRetry(nil)(srv, st)
		srv.SetTaskTimeoutForTests(2 * time.Second)
	}
	f := newTaskTestFixtureWithOptions(t, modelCloudWorkerScript(t), 5, slowUpstream.URL, generousTestGovernor(), pre)

	traceID := "trace-i6-timeout-00000000000000001"
	// The run takes the full 2s deadline: use a client that outlasts it.
	resp := postTask(t, longClient(f.srv.cfg.Socket), traceID, "uzun bir bulut görevi")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	if ev, _ := lastSSEMessage(t, frames); ev != "error" {
		t.Fatalf("last SSE event = %q, want error (timeout); frames=%+v", ev, frames)
	}
	assertLedgerHasKind(t, f.store, traceID, "task_timeout")

	// The proxy's ErrorHandler runs on its own goroutine; wait until it has
	// decided either way (the inbound-cancel JSONL line, or - the bug - a
	// task.failed row).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countJSONLEventLines(t, f.jsonlPath, traceID, anthproxy.EventInboundCanceled) > 0 ||
			countEventsByTraceKind(t, f.store, traceID, task.EventCloudTaskFailed) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	_, status, _ := taskRowByTrace(t, f.store, traceID)
	if status != task.StatusExecuting {
		t.Errorf("tasks.status = %q, want %q (a timeout belongs to the resume scan, not a terminal failure)", status, task.StatusExecuting)
	}
	if n := countEventsByTraceKind(t, f.store, traceID, task.EventCloudTaskFailed); n != 0 {
		t.Errorf("%s ledger rows = %d, want 0 (an inbound cancel is not a cloud failure)", task.EventCloudTaskFailed, n)
	}
	if n := countJSONLEventLines(t, f.jsonlPath, traceID, anthproxy.EventInboundCanceled); n != 1 {
		t.Errorf("JSONL %s lines = %d, want 1", anthproxy.EventInboundCanceled, n)
	}
}
