package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
)

// TestHandleTaskReportsParkedWhenKahyadParkedIt (FIX-I-7): when kahyad's own
// proxy parked the task mid-run (inline retries exhausted ->
// CloudRetry.ParkOrGiveUp), kahyad is the authority on what the user sees,
// whatever the worker's terminal line said. The SDK delivers the proxy's
// 503 as ResultMessage(is_error) and the real worker prints an ordinary
// error line; before the fix that made tasks.state 'error' and the SSE say
// "Model çağrısı başarısız oldu" while tasks.status was already
// bekliyor-yeniden-deneme. cloud_call_worker.py prints exactly such an error
// line on the 503.
func TestHandleTaskReportsParkedWhenKahyadParkedIt(t *testing.T) {
	upstream503 := newRecordingUpstreamWithStatus(t, http.StatusServiceUnavailable)
	pre := func(srv *Server, st *store.Store) {
		wireDurabilityWithCloudRetry([]time.Duration{time.Minute})(srv, st)
		srv.cfg.CloudRetryMaxInline = 1 // one upstream attempt per call: no real backoff sleep
	}
	f := newTaskTestFixtureWithOptions(t, []string{"python3", "../outbox/testdata/cloud_call_worker.py"}, 5, upstream503.srv.URL, generousTestGovernor(), pre)

	traceID := "trace-i7-parked-00000000000000001"
	resp := postTask(t, longClient(f.srv.cfg.Socket), traceID, "çevrimdışı bir soru")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	ev, msg := lastSSEMessage(t, frames)
	if ev != "error" || msg != task.MsgCloudParked {
		t.Errorf("last SSE = %s %q, want error %q", ev, msg, task.MsgCloudParked)
	}

	_, status, state := taskRowByTrace(t, f.store, traceID)
	if status != task.StatusRetryWait {
		t.Errorf("tasks.status = %q, want %q", status, task.StatusRetryWait)
	}
	if state != "waiting_retry" {
		t.Errorf("tasks.state = %q, want waiting_retry", state)
	}
	if n := countEventsByTraceKind(t, f.store, traceID, "task_waiting_retry"); n != 1 {
		t.Errorf("task_waiting_retry rows = %d, want 1", n)
	}
	if n := countEventsByTraceKind(t, f.store, traceID, task.EventCloudWaitingRetry); n != 1 {
		t.Errorf("%s rows = %d, want 1", task.EventCloudWaitingRetry, n)
	}
}

// newRecordingUpstreamWithStatus is newRecordingUpstream answering every
// request with status (and an Anthropic-shaped error body for >= 400).
func newRecordingUpstreamWithStatus(t *testing.T, status int) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, string(body))
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 400 {
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"scripted failure"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}
