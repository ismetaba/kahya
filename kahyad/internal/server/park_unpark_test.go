package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
)

// TestHandleTaskUnparksWhenWorkerCLIRetrySucceeds (FIX-FINAL park-then-
// cli-retry-success-reruns-task): once the inline retries are exhausted the
// proxy parks the task, but the worker's CLI retries the 503 itself and the
// proxy forwards it. When that retry succeeded and the worker finished
// cleanly, the task was still reported parked ("Bulut servisine
// ulaşılamıyor...") after the full answer, with a pending resume row that
// re-ran the finished task at next_retry_at. It now ends done with the
// result event.
func TestHandleTaskUnparksWhenWorkerCLIRetrySucceeds(t *testing.T) {
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"blip"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`))
	}))
	t.Cleanup(up.Close)
	pre := func(srv *Server, st *store.Store) {
		wireDurabilityWithCloudRetry([]time.Duration{time.Minute})(srv, st)
		srv.cfg.CloudRetryMaxInline = 1
	}
	worker := filepath.Join("testdata", "retrying_cloud_worker.py")
	f := newTaskTestFixtureWithOptions(t, []string{"python3", worker}, 5, up.URL, generousTestGovernor(), pre)
	traceID := "trace-park-unpark-000000000000001"
	frames := readAllSSE(t, postTask(t, longClient(f.srv.cfg.Socket), traceID, "bir soru"))
	ev, msg := lastSSEMessage(t, frames)
	_, status, state := taskRowByTrace(t, f.store, traceID)
	if n.Load() < 2 {
		t.Fatalf("upstream saw %d requests, want the 503 and the CLI's successful retry", n.Load())
	}
	if status != task.StatusDone || state != "done" || ev != "result" {
		t.Fatalf("status=%q state=%q last SSE=%s %q; want done/done/result (a clean success after a park un-parks)", status, state, ev, msg)
	}
	var unparked int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE trace_id = ? AND kind = ?`, traceID, task.EventUnparkedOnSuccess).Scan(&unparked); err != nil || unparked != 1 {
		t.Fatalf("%s rows = %d (%v), want 1", task.EventUnparkedOnSuccess, unparked, err)
	}
}
