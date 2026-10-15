package anthproxy

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kahya/kahyad/internal/logx"
)

// TestCloudUnreachableCallbackFiresOncePerProxy (FIX-I-7): the worker CLI
// retries a 503 on its own, and every retry is a NEW inbound request with
// its own inline-retry loop. One per-task proxy must park the task once:
// OnCloudUnreachable and the proxy.cloud_unreachable ledger row fire once,
// the repeats are JSONL proxy.cloud_unreachable_repeat, and every response
// still carries the 503 + marker the worker keys on.
func TestCloudUnreachableCallbackFiresOncePerProxy(t *testing.T) {
	upstream := newStatusSequenceUpstream(t, []int{503})
	ledger := &fakeLedger{}
	logDir := t.TempDir()
	jsonl, err := logx.New(logDir, "trace_test")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { jsonl.Close() })

	var unreachable atomic.Int32
	cfg := retryCfg(t, upstream.srv.URL, generousGovernor(), ledger, &fakeSleeper{})
	cfg.MaxInlineRetries = 1
	cfg.JSONLLog = jsonl
	cfg.OnCloudUnreachable = func(context.Context, string) error { unreachable.Add(1); return nil }
	baseURL, _ := startTestProxy(t, cfg)

	for i := 0; i < 3; i++ {
		status, body := doAndDrain(t, newAuthedRequest(t, "POST", baseURL+"/v1/messages", `{"model":"claude-sonnet-5-5"}`))
		if status != http.StatusServiceUnavailable {
			t.Fatalf("request #%d status = %d, want 503", i, status)
		}
		if !strings.Contains(body, MsgCloudUnreachableMarker) {
			t.Fatalf("request #%d body = %s, want the cloud-unreachable marker", i, body)
		}
	}

	if got := unreachable.Load(); got != 1 {
		t.Errorf("OnCloudUnreachable fired %d times, want 1 (one park per proxy)", got)
	}
	if n := ledger.countKind(EventCloudUnreachable); n != 1 {
		t.Errorf("%s ledger rows = %d, want 1", EventCloudUnreachable, n)
	}
	jsonlPath := filepath.Join(logDir, "kahyad.jsonl")
	if !waitForCondition(t, time.Second, func() bool { return countJSONLEvents(t, jsonlPath, EventCloudUnreachableRepeat) == 2 }) {
		t.Errorf("JSONL %s lines = %d, want 2", EventCloudUnreachableRepeat, countJSONLEvents(t, jsonlPath, EventCloudUnreachableRepeat))
	}
}
