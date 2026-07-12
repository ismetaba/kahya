package anthproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRoundTripper is a base http.RoundTripper stand-in that always returns
// a fixed (resp, err) and counts how many times it was called - so a test
// can inject a genuine TRANSPORT-level failure (resp==nil, err!=nil) that
// the real http.DefaultTransport would only produce against an actual
// broken connection, and assert exactly how many attempts the retry loop
// made.
type fakeRoundTripper struct {
	mu    sync.Mutex
	calls int
	resp  *http.Response
	err   error
}

func (f *fakeRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.resp, f.err
}

func (f *fakeRoundTripper) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestNonRetryableTransportErrorWrappedAsErrNonRetryable is the unit half
// of the W4-04 review's BLOCKER 2 regression: a transport-level failure
// (no HTTP response at all) that cloudretry.Classify verdicts NonRetryable
// must be returned wrapped in ErrNonRetryable - NOT retried, and NOT
// returned as a bare error that handleUpstreamError would mistake for a
// generic 502. The bare error used here (a TLS-handshake-shaped message) is
// neither *net.OpError nor a timeout, so Classify(0, err) is NonRetryable.
func TestNonRetryableTransportErrorWrappedAsErrNonRetryable(t *testing.T) {
	base := &fakeRoundTripper{err: errors.New("tls: first record does not look like a TLS handshake")}
	rt := &retryTransport{base: base, maxInline: 3, sleep: func(time.Duration) {}}

	req, _ := http.NewRequest("POST", "http://upstream.invalid/v1/messages", strings.NewReader("{}"))
	data := &reqData{}
	req = req.WithContext(context.WithValue(req.Context(), reqDataCtxKey{}, data))

	resp, err := rt.RoundTrip(req)
	if resp != nil {
		t.Fatalf("resp = %v, want nil for a transport-level failure", resp)
	}
	if !errors.Is(err, ErrNonRetryable) {
		t.Fatalf("err = %v, want it to wrap ErrNonRetryable", err)
	}
	if errors.Is(err, ErrRetriesExhausted) {
		t.Fatal("err wraps ErrRetriesExhausted, want ErrNonRetryable (this is NOT the exhausted path)")
	}
	if base.count() != 1 {
		t.Fatalf("base RoundTrip calls = %d, want exactly 1 (a NonRetryable transport error is never retried)", base.count())
	}
	if data.NonRetryableReason != "transport_error" {
		t.Errorf("NonRetryableReason = %q, want transport_error", data.NonRetryableReason)
	}
	if data.Exhausted {
		t.Error("Exhausted = true, want false (a single NonRetryable attempt is not the exhausted path)")
	}
}

// TestNonRetryableTransportFailureInvokesCallback is the end-to-end half:
// through the FULL proxy (ServeHTTP -> retryTransport -> ErrorHandler), a
// transport-level NonRetryable failure must fire OnNonRetryableFailure
// exactly once (so kahyad/internal/task.CloudRetry.FailNonRetryable moves
// the task to 'failed') and return a bounded 502 to the worker - NOT leave
// the task stuck in 'executing' with no callback, which (via the outbox
// redispatch loop) would re-dispatch it forever. Before the fix this path
// fell through handleUpstreamError to a plain 502 and fired NO callback.
//
// The base transport is swapped for a deterministic failing one (the real
// http.DefaultTransport only produces this against a genuinely broken
// connection); everything downstream of it is the real proxy.
func TestNonRetryableTransportFailureInvokesCallback(t *testing.T) {
	ledger := &fakeLedger{}
	governor := generousGovernor()
	cfg := retryCfg(t, "http://upstream.invalid/", governor, ledger, &fakeSleeper{})

	var reasonID string
	var callCount int32
	cfg.OnNonRetryableFailure = func(_ context.Context, _ /*taskID*/, reason string) error {
		atomic.AddInt32(&callCount, 1)
		reasonID = reason
		return nil
	}
	var cloudUnreachableCount int32
	cfg.OnCloudUnreachable = func(context.Context, string) error {
		atomic.AddInt32(&cloudUnreachableCount, 1)
		return nil
	}

	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// Swap the base transport BEFORE Start() (no server goroutine yet, so no
	// request can be in flight) for one that fails NonRetryably at transport
	// level.
	base := &fakeRoundTripper{err: errors.New("tls: first record does not look like a TLS handshake")}
	p.rp.Transport.(*retryTransport).base = base
	baseURL, err := p.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5"}`))
	req.Header.Set("x-api-key", testToken)

	done := make(chan struct{})
	var resp *http.Response
	var doErr error
	go func() {
		resp, doErr = http.DefaultClient.Do(req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not return within 5s - transport NonRetryable failure must never hang")
	}
	if doErr != nil {
		t.Fatalf("Do() error = %v", doErr)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a transport-level failure", resp.StatusCode)
	}
	if base.count() != 1 {
		t.Fatalf("base attempts = %d, want exactly 1 (NonRetryable, never retried)", base.count())
	}
	if !waitForCondition(t, time.Second, func() bool { return atomic.LoadInt32(&callCount) == 1 }) {
		t.Fatalf("OnNonRetryableFailure fired %d times, want exactly 1", atomic.LoadInt32(&callCount))
	}
	if reasonID != "transport_error" {
		t.Errorf("reasonID = %q, want transport_error", reasonID)
	}
	if got := atomic.LoadInt32(&cloudUnreachableCount); got != 0 {
		t.Errorf("OnCloudUnreachable fired %d times, want 0 (a NonRetryable failure must not be treated as retry-exhaustion)", got)
	}
}
