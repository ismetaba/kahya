package anthproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/notify"
)

// --- test doubles ---

type recordingUpstream struct {
	mu       sync.Mutex
	requests []recordedRequest
	srv      *httptest.Server
}

type recordedRequest struct {
	headers http.Header
	body    string
}

func (u *recordingUpstream) hits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func (u *recordingUpstream) last() recordedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[len(u.requests)-1]
}

// newJSONUpstream answers every request with a fixed non-stream
// /v1/messages JSON response and records every inbound request it saw.
func newJSONUpstream(t *testing.T, statusCode int, jsonBody string) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, recordedRequest{headers: r.Header.Clone(), body: string(body)})
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(jsonBody))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// newSSEUpstream answers with an SSE stream, flushing between each event
// with a short sleep - used to prove unbuffered relay.
func newSSEUpstream(t *testing.T, events []string, delay time.Duration) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, recordedRequest{headers: r.Header.Clone(), body: string(body)})
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, e := range events {
			_, _ = fmt.Fprintf(w, "%s\n", e)
			flusher.Flush()
			if delay > 0 {
				time.Sleep(delay)
			}
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

const testToken = "kahya-task-testtoken0000000000abcd"

func testProxyConfig(t *testing.T, upstreamURL string, governor *Governor, ledger EventLedger, notifier notify.Notifier) ProxyConfig {
	t.Helper()
	return ProxyConfig{
		TaskID:      "t_test",
		TraceID:     "trace_test",
		Token:       testToken,
		UpstreamURL: upstreamURL,
		Governor:    governor,
		EventLedger: ledger,
		Notifier:    notifier,
	}
}

func startTestProxy(t *testing.T, cfg ProxyConfig) (baseURL string, proxy *Proxy) {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return addr, p
}

func generousGovernor() *Governor {
	return NewGovernor(Limits{
		DailyBudgetUSD:         1000,
		MonthlyBudgetUSD:       10000,
		TaskTokenCeiling:       500_000,
		DowngradeAtRatio:       0.8,
		CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
}

// waitForCondition polls until cond() is true or the deadline passes.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// --- (a) keychain mode: local token stripped, real key injected ---

type fakeKeychainReader struct {
	key string
	err error
}

func (f *fakeKeychainReader) Read() (string, error) { return f.key, f.err }

func TestProxyKeychainModeStripsTokenAndInjectsKey(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	ledger := &fakeLedger{}
	governor := generousGovernor()

	cfg := testProxyConfig(t, upstream.srv.URL, governor, ledger, nil)
	cfg.CredentialMode = CredentialModeKeychain
	cfg.Credential = NewKeychainCredentialSource(&fakeKeychainReader{key: "sk-ant-real-keychain-key"}, "prod", nil)
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	req.Header.Set("x-api-key", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if upstream.hits() != 1 {
		t.Fatalf("upstream hits = %d, want 1", upstream.hits())
	}
	got := upstream.last().headers
	if got.Get("x-api-key") != "sk-ant-real-keychain-key" {
		t.Errorf("upstream x-api-key = %q, want the real keychain key", got.Get("x-api-key"))
	}
	if got.Get("x-api-key") == testToken {
		t.Error("the local per-task token reached the upstream unchanged - it must always be replaced in keychain mode")
	}
}

// --- (b) wrong/missing token -> 401 + proxy_auth_reject, zero upstream hits ---

func TestProxyRejectsWrongToken(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{}}`)
	ledger := &fakeLedger{}
	governor := generousGovernor()
	cfg := testProxyConfig(t, upstream.srv.URL, governor, ledger, nil)
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"type":"error"`) {
		t.Errorf("body = %s, want an Anthropic-shaped error", body)
	}

	if upstream.hits() != 0 {
		t.Errorf("upstream hits = %d, want 0 (a rejected request must never be forwarded)", upstream.hits())
	}
	if got := ledger.countKind(EventProxyAuthReject); got != 1 {
		t.Errorf("proxy_auth_reject ledgered %d times, want 1", got)
	}
}

func TestProxyRejectsMissingToken(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{}}`)
	governor := generousGovernor()
	cfg := testProxyConfig(t, upstream.srv.URL, governor, nil, nil)
	baseURL, _ := startTestProxy(t, cfg)

	resp, err := http.Post(baseURL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Post() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if upstream.hits() != 0 {
		t.Errorf("upstream hits = %d, want 0", upstream.hits())
	}
}

// --- (c) SSE streams unbuffered ---

func TestProxySSEStreamsUnbufferedAndRecordsUsage(t *testing.T) {
	events := []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":40,"cache_creation_input_tokens":0,"cache_read_input_tokens":10}}}`,
		``,
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":22}}`,
		``,
	}
	upstream := newSSEUpstream(t, events, 40*time.Millisecond)
	ledger := &fakeLedger{}
	governor := generousGovernor()
	cfg := testProxyConfig(t, upstream.srv.URL, governor, ledger, nil)
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","stream":true}`))
	req.Header.Set("x-api-key", testToken)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()

	var firstLineAt, lastLineAt time.Time
	scanner := bufio.NewScanner(resp.Body)
	lines := 0
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		if firstLineAt.IsZero() {
			firstLineAt = time.Now()
		}
		lastLineAt = time.Now()
		lines++
	}
	if lines != 2 {
		t.Fatalf("got %d non-blank SSE lines, want 2", lines)
	}
	// Unbuffered relay: the gap between the first and last line arriving
	// at the client must reflect the upstream's own inter-event delay,
	// not be ~0 (which would mean the whole body was buffered and
	// delivered in one shot at the end).
	if gap := lastLineAt.Sub(firstLineAt); gap < 20*time.Millisecond {
		t.Errorf("gap between first/last SSE line = %v, want >= ~40ms (proves streaming, not buffering)", gap)
	}
	_ = start

	if !waitForCondition(t, time.Second, func() bool { return ledger.countKind(EventModelCall) == 1 }) {
		t.Fatal("model_call never ledgered for the completed SSE call")
	}
	call := ledger.calls[len(ledger.calls)-1]
	if call.payload["input_tokens"] != int64(40) {
		t.Errorf("model_call input_tokens = %v, want 40", call.payload["input_tokens"])
	}
	if call.payload["output_tokens"] != int64(22) {
		t.Errorf("model_call output_tokens = %v, want 22", call.payload["output_tokens"])
	}
	if call.payload["cache_read_input_tokens"] != int64(10) {
		t.Errorf("model_call cache_read_input_tokens = %v, want 10", call.payload["cache_read_input_tokens"])
	}
}

// --- (d)/(e) governor blocks BEFORE forwarding ---

func TestProxyBlocksAtTaskCeilingBeforeForwarding(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{}}`)
	ledger := &fakeLedger{}
	governor := NewGovernor(Limits{
		DailyBudgetUSD: 1000, MonthlyBudgetUSD: 10000, TaskTokenCeiling: 500_000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
	// Pre-seed the ceiling via a prior recorded call - the NEXT request
	// must be blocked before it is ever forwarded.
	governor.RecordUsage(t.Context(), 0, ledger, "trace1", "t_test", "claude-sonnet-5",
		Usage{InputTokens: 500_000}, 1.0, "ok", 10, "")
	ledger.calls = nil // reset so the test only counts calls from THIS request

	cfg := testProxyConfig(t, upstream.srv.URL, governor, ledger, nil)
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5"}`))
	req.Header.Set("x-api-key", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(body), MsgTaskCeiling) {
		t.Errorf("body = %s, want it to contain %q", body, MsgTaskCeiling)
	}
	if upstream.hits() != 0 {
		t.Errorf("upstream hits = %d, want 0 - a ceiling-blocked request must never reach the upstream", upstream.hits())
	}
	if got := ledger.countKind(EventTaskPausedBudget); got != 1 {
		t.Errorf("task_paused_budget ledgered %d times, want 1", got)
	}
}

func TestProxyBlocksAtDailyBudgetWithExactTurkishMessage(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{}}`)
	ledger := &fakeLedger{}
	governor := NewGovernor(Limits{
		DailyBudgetUSD: 0.01, MonthlyBudgetUSD: 10000, TaskTokenCeiling: 500_000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
	governor.RecordUsage(context.Background(), 0, ledger, "trace1", "t_test", "claude-sonnet-5",
		Usage{InputTokens: 1000}, 0.02, "ok", 10, "")
	ledger.calls = nil

	var pauseCalledWith string
	var pauseMu sync.Mutex
	cfg := testProxyConfig(t, upstream.srv.URL, governor, ledger, nil)
	cfg.PauseBudget = func(_ context.Context, taskID string) error {
		pauseMu.Lock()
		pauseCalledWith = taskID
		pauseMu.Unlock()
		return nil
	}
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(body), MsgDailyBudgetBlock) {
		t.Errorf("body = %s, want it to contain %q", body, MsgDailyBudgetBlock)
	}
	if upstream.hits() != 0 {
		t.Errorf("upstream hits = %d, want 0", upstream.hits())
	}
	pauseMu.Lock()
	got := pauseCalledWith
	pauseMu.Unlock()
	if got != "t_test" {
		t.Errorf("PauseBudget callback called with task_id=%q, want t_test", got)
	}
}

// --- (f) passthrough mode forwards the worker's own auth unchanged ---

func TestProxyPassthroughForwardsWorkerAuthUnchanged(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{"input_tokens":7,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	ledger := &fakeLedger{}
	governor := generousGovernor()
	cfg := testProxyConfig(t, upstream.srv.URL, governor, ledger, nil)
	cfg.CredentialMode = CredentialModePassthrough
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5"}`))
	req.Header.Set("x-api-key", testToken)                                  // local-loopback proof only
	req.Header.Set("Authorization", "Bearer worker-own-session-credential") // the worker's real upstream auth
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got := upstream.last().headers
	if got.Get("Authorization") != "Bearer worker-own-session-credential" {
		t.Errorf("upstream Authorization = %q, want the worker's own credential forwarded unchanged", got.Get("Authorization"))
	}
	if got.Get("x-api-key") != "" {
		t.Errorf("upstream x-api-key = %q, want empty - the local token must never reach the upstream", got.Get("x-api-key"))
	}

	if !waitForCondition(t, time.Second, func() bool { return ledger.countKind(EventModelCall) == 1 }) {
		t.Fatal("model_call never ledgered in passthrough mode - metering must still happen")
	}
}

// --- (g) per-task listener closes on task end ---

func TestProxyListenerClosesOnTaskEnd(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{}}`)
	governor := generousGovernor()
	cfg := testProxyConfig(t, upstream.srv.URL, governor, nil, nil)

	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	baseURL, err := p.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", testToken)
	if _, err := http.DefaultClient.Do(req); err != nil {
		t.Fatalf("request before Close() failed: %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	req2, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{}`))
	req2.Header.Set("x-api-key", testToken)
	if _, err := client.Do(req2); err == nil {
		t.Fatal("request after Close() succeeded, want connection refused/failed")
	}
}

// --- (h) KAHYA_ANTHROPIC_KEY_OVERRIDE honored only in dev ---

func TestKeyOverrideHonoredOnlyInDev(t *testing.T) {
	t.Setenv(keyOverrideEnvVar, "sk-ant-override-value")

	devSrc := NewKeychainCredentialSource(&fakeKeychainReader{err: fmt.Errorf("keychain locked")}, "dev", nil)
	name, value, err := devSrc.UpstreamAuth(t.Context())
	if err != nil {
		t.Fatalf("dev-mode UpstreamAuth() error = %v, want the override to be honored", err)
	}
	if name != "x-api-key" || value != "sk-ant-override-value" {
		t.Errorf("dev-mode UpstreamAuth() = (%q, %q), want (x-api-key, sk-ant-override-value)", name, value)
	}

	var warned bool
	prodSrc := NewKeychainCredentialSource(&fakeKeychainReader{err: fmt.Errorf("keychain locked")}, "prod", func() { warned = true })
	if _, _, err := prodSrc.UpstreamAuth(t.Context()); err == nil {
		t.Fatal("prod-mode UpstreamAuth() error = nil, want an error - the override must be IGNORED outside dev, falling through to the (failing) real Keychain read")
	}
	if !warned {
		t.Error("warnOverrideIgnored was never called in prod mode with the override env var set")
	}
}

func TestKeyOverrideIgnoredWhenUnset(t *testing.T) {
	src := NewKeychainCredentialSource(&fakeKeychainReader{key: "sk-ant-real"}, "prod", nil)
	name, value, err := src.UpstreamAuth(t.Context())
	if err != nil {
		t.Fatalf("UpstreamAuth() error = %v", err)
	}
	if name != "x-api-key" || value != "sk-ant-real" {
		t.Errorf("UpstreamAuth() = (%q, %q), want (x-api-key, sk-ant-real)", name, value)
	}
}

// --- keychain unavailable -> 503 + single notify ---

func TestProxyKeychainUnavailableRespondsAndNotifiesOnce(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{}}`)
	governor := generousGovernor()
	notifier := &fakeNotifier{}
	cfg := testProxyConfig(t, upstream.srv.URL, governor, nil, notifier)
	cfg.CredentialMode = CredentialModeKeychain
	cfg.Credential = NewKeychainCredentialSource(&fakeKeychainReader{err: fmt.Errorf("errSecInteractionNotAllowed")}, "prod", nil)
	baseURL, _ := startTestProxy(t, cfg)

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("x-api-key", testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do() error = %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
		if !strings.Contains(string(body), MsgKeychainUnavailable) {
			t.Errorf("body = %s, want it to contain %q", body, MsgKeychainUnavailable)
		}
	}
	if upstream.hits() != 0 {
		t.Errorf("upstream hits = %d, want 0", upstream.hits())
	}
	if got := len(notifier.notified); got != 1 {
		t.Errorf("Notifier.Notify called %d times across 2 failed requests, want exactly 1 (notify once)", got)
	}
}
