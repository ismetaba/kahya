package anthproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kahya/kahyad/internal/cloudretry"
	"kahya/kahyad/internal/logx"
)

// FIX-I-6: when the INBOUND request (the worker's) is cancelled - the
// worker died, was killed by a task timeout/halt/shutdown, or hung up -
// while an upstream attempt is in flight, httputil.ReverseProxy's
// Transport.RoundTrip returns a bare context.Canceled. Before the fix the
// taxonomy classified that NonRetryable and the proxy failed the task
// ("transport_error"), racing a halt's user_halted and turning a timeout
// that the resume scan should own into a terminal failure.

// TestInboundCancelDoesNotFireNonRetryableCallback drives the full proxy:
// the upstream holds the request open, the client cancels it after 200ms,
// and neither task callback may fire. The JSONL line proxy.inbound_canceled
// records what happened.
func TestInboundCancelDoesNotFireNonRetryableCallback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	logDir := t.TempDir()
	jsonl, err := logx.New(logDir, "trace_test")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { jsonl.Close() })

	var nonRetryable, unreachable atomic.Int32
	cfg := testProxyConfig(t, upstream.URL, generousGovernor(), &fakeLedger{}, nil)
	cfg.MaxInlineRetries = 3
	cfg.Sleep = func(time.Duration) {}
	cfg.JSONLLog = jsonl
	cfg.OnNonRetryableFailure = func(context.Context, string, string) error { nonRetryable.Add(1); return nil }
	cfg.OnCloudUnreachable = func(context.Context, string) error { unreachable.Add(1); return nil }
	baseURL, _ := startTestProxy(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := newAuthedRequest(t, "POST", baseURL+"/v1/messages", `{"model":"claude-sonnet-5","messages":[]}`)
	req = req.WithContext(ctx)
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}

	// The ErrorHandler runs on the proxy's own goroutine after the
	// client has already gone; wait until it has decided one way or the
	// other.
	jsonlPath := filepath.Join(logDir, "kahyad.jsonl")
	waitForCondition(t, 2*time.Second, func() bool {
		return countJSONLEvents(t, jsonlPath, EventInboundCanceled) > 0 || nonRetryable.Load() > 0 || unreachable.Load() > 0
	})
	if nonRetryable.Load() != 0 || unreachable.Load() != 0 {
		t.Fatalf("callbacks fired on inbound cancel: nonRetryable=%d unreachable=%d", nonRetryable.Load(), unreachable.Load())
	}
	if n := countJSONLEvents(t, jsonlPath, EventInboundCanceled); n != 1 {
		t.Fatalf("JSONL %s lines = %d, want 1", EventInboundCanceled, n)
	}
}

// TestRetryTransportReturnsErrInboundCanceled is the unit half: an
// attempt that fails because the inbound context is already cancelled is
// ErrInboundCanceled - never classified, never retried, never a
// NonRetryable reason.
func TestRetryTransportReturnsErrInboundCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	base := &cancelOnRoundTrip{cancel: cancel}
	rt := &retryTransport{base: base, maxInline: 3, sleep: func(time.Duration) {}}

	data := &reqData{}
	req, _ := http.NewRequestWithContext(context.WithValue(ctx, reqDataCtxKey{}, data), "POST", "http://upstream.invalid/v1/messages", strings.NewReader("{}"))

	resp, err := rt.RoundTrip(req)
	if resp != nil {
		t.Fatalf("resp = %v, want nil", resp)
	}
	if !errors.Is(err, ErrInboundCanceled) {
		t.Fatalf("err = %v, want it to wrap ErrInboundCanceled", err)
	}
	if errors.Is(err, ErrNonRetryable) || errors.Is(err, ErrRetriesExhausted) {
		t.Fatalf("err = %v, must be neither ErrNonRetryable nor ErrRetriesExhausted", err)
	}
	if got := base.calls.Load(); got != 1 {
		t.Fatalf("base RoundTrip calls = %d, want exactly 1 (an inbound cancel is never retried)", got)
	}
	if data.NonRetryableReason != "" {
		t.Errorf("NonRetryableReason = %q, want empty (an inbound cancel is not an upstream verdict)", data.NonRetryableReason)
	}
	if data.Exhausted {
		t.Error("Exhausted = true, want false")
	}
}

// TestRetrySleepAbortsOnInboundCancel: the real (unhooked) backoff sleep
// must end when the inbound request is cancelled. The upstream answers
// 503 + Retry-After: 30; before the fix the loop slept the full 30s.
func TestRetrySleepAbortsOnInboundCancel(t *testing.T) {
	base := &fakeRoundTripper{resp: &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{"Retry-After": []string{"30"}},
		Body:       http.NoBody,
	}}
	rt := &retryTransport{base: base, maxInline: 3, backoff: cloudretry.DefaultBackoff(), sleep: nil}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data := &reqData{}
	req, _ := http.NewRequestWithContext(context.WithValue(ctx, reqDataCtxKey{}, data), "POST", "http://upstream.invalid/v1/messages", strings.NewReader("{}"))

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := rt.RoundTrip(req)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInboundCanceled) {
			t.Fatalf("err = %v, want ErrInboundCanceled", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("RoundTrip took %v, want < 1s (the backoff sleep must honor the inbound context)", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RoundTrip still sleeping 5s after the inbound cancel (Retry-After: 30 ignored the context)")
	}
	if got := base.count(); got != 1 {
		t.Errorf("base attempts = %d, want 1 (cancelled during the first backoff)", got)
	}
}

// cancelOnRoundTrip cancels the request context and answers the way
// net/http does for a cancelled request: no response, a context error.
type cancelOnRoundTrip struct {
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (c *cancelOnRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	c.cancel()
	<-r.Context().Done()
	return nil, r.Context().Err()
}

// countJSONLEvents counts JSONL lines whose "event" is kind.
func countJSONLEvents(t *testing.T, path, kind string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var line map[string]any
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if line["event"] == kind {
			if tr, _ := line["trace_id"].(string); tr == "" {
				t.Errorf("JSONL %s line without trace_id: %s", kind, sc.Text())
			}
			n++
		}
	}
	return n
}
