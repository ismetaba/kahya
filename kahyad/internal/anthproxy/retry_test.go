package anthproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kahya/kahyad/internal/cloudretry"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/secretlane"
)

// --- test doubles specific to the W4-04 retry/shaping suite ---

// statusSequenceUpstream answers with a scripted sequence of statuses (one
// per hit; the last entry repeats for any hit beyond the sequence's
// length) and records every inbound request's body + Retry-After.
type statusSequenceUpstream struct {
	mu         sync.Mutex
	requests   []string // recorded bodies
	statuses   []int
	retryAfter string // sent on every non-2xx response, if non-empty
	srv        *httptest.Server
}

func newStatusSequenceUpstream(t *testing.T, statuses []int) *statusSequenceUpstream {
	t.Helper()
	u := &statusSequenceUpstream{statuses: statuses}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		idx := len(u.requests)
		u.requests = append(u.requests, string(body))
		status := u.statuses[len(u.statuses)-1]
		if idx < len(u.statuses) {
			status = u.statuses[idx]
		}
		retryAfter := u.retryAfter
		u.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if status >= 400 && retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		if status < 400 {
			_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"scripted failure"}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *statusSequenceUpstream) hits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func (u *statusSequenceUpstream) bodyAt(i int) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[i]
}

// fakeSleeper records every requested delay instead of actually sleeping
// - the "injectable clock" the task spec's own step-8 tests require, so
// backoff/retry-after assertions run instantly instead of taking several
// real seconds.
type fakeSleeper struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (f *fakeSleeper) sleep(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delays = append(f.delays, d)
}

func (f *fakeSleeper) recorded() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, len(f.delays))
	copy(out, f.delays)
	return out
}

// fixedRand always returns 0.5 - zero jitter, exact 1s/2s/4s backoff.
func fixedRand() float64 { return 0.5 }

func retryCfg(t *testing.T, upstreamURL string, governor *Governor, ledger EventLedger, sleeper *fakeSleeper) ProxyConfig {
	t.Helper()
	cfg := testProxyConfig(t, upstreamURL, governor, ledger, nil)
	cfg.MaxInlineRetries = 3
	cfg.Backoff = cloudretry.Backoff{Base: time.Second, Factor: 2, JitterFrac: 0.2, Rand: fixedRand}
	if sleeper != nil {
		cfg.Sleep = sleeper.sleep
	}
	return cfg
}

// --- 429x2 then 200: one logical success, exactly 3 upstream hits ---

func TestRetrySucceedsAfterTwoRetryableFailures(t *testing.T) {
	upstream := newStatusSequenceUpstream(t, []int{429, 429, 200})
	ledger := &fakeLedger{}
	governor := generousGovernor()
	sleeper := &fakeSleeper{}
	cfg := retryCfg(t, upstream.srv.URL, governor, ledger, sleeper)
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5"}`))
	req.Header.Set("x-api-key", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, body)
	}
	if got := upstream.hits(); got != 3 {
		t.Fatalf("upstream hits = %d, want exactly 3", got)
	}

	// Cost-governor counters count the logical call ONCE, not per retry.
	if !waitForCondition(t, time.Second, func() bool { return ledger.countKind(EventModelCall) == 1 }) {
		t.Fatalf("model_call ledgered %d times, want exactly 1", ledger.countKind(EventModelCall))
	}

	// Backoff delays observed: 1s then 2s (fixedRand => zero jitter).
	delays := sleeper.recorded()
	if len(delays) != 2 {
		t.Fatalf("recorded backoff delays = %v, want exactly 2 (between attempts 1->2 and 2->3)", delays)
	}
	if delays[0] != time.Second {
		t.Errorf("delays[0] = %v, want 1s", delays[0])
	}
	if delays[1] != 2*time.Second {
		t.Errorf("delays[1] = %v, want 2s", delays[1])
	}
}

// TestRetryAttemptsLoggedAsJSONLWithTraceID is the offline-smoke
// acceptance criterion's own JSONL half: "JSONL proxy log for the smoke
// run shows attempts 1..3 with the same trace_id as the task's ledger
// events" - proven directly here rather than via a real `kahya` CLI
// invocation (out of scope for this task - the W4-07 gate script reuses
// this exact mechanism end-to-end).
func TestRetryAttemptsLoggedAsJSONLWithTraceID(t *testing.T) {
	upstream := newStatusSequenceUpstream(t, []int{429, 429, 200})
	ledger := &fakeLedger{}
	governor := generousGovernor()
	logDir := t.TempDir()
	jsonlLog, err := logx.New(logDir, "boot0123456789abcdef0123456789ab")
	if err != nil {
		t.Fatalf("logx.New() error = %v", err)
	}
	t.Cleanup(func() { _ = jsonlLog.Close() })

	cfg := retryCfg(t, upstream.srv.URL, governor, ledger, &fakeSleeper{})
	cfg.JSONLLog = jsonlLog
	cfg.TraceID = "trace_jsonl_test"
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5"}`))
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

	raw, err := os.ReadFile(filepath.Join(logDir, "kahyad.jsonl"))
	if err != nil {
		t.Fatalf("read kahyad.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var attempts []int
	for _, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode JSONL line %q: %v", line, err)
		}
		if entry["event"] != "proxy.cloud_call_attempt" {
			continue
		}
		if entry["trace_id"] != "trace_jsonl_test" {
			t.Errorf("line %q: trace_id = %v, want trace_jsonl_test", line, entry["trace_id"])
		}
		attempts = append(attempts, int(entry["attempt"].(float64)))
	}
	if len(attempts) != 3 {
		t.Fatalf("proxy.cloud_call_attempt lines = %d, want 3 (attempts 1..3), got attempts=%v", len(attempts), attempts)
	}
	for i, want := range []int{1, 2, 3} {
		if attempts[i] != want {
			t.Errorf("attempts[%d] = %d, want %d", i, attempts[i], want)
		}
	}
}

// --- always-503: retries exhausted -> typed error + OnCloudUnreachable ---

func TestRetryExhaustionInvokesOnCloudUnreachable(t *testing.T) {
	upstream := newStatusSequenceUpstream(t, []int{503, 503, 503})
	ledger := &fakeLedger{}
	governor := generousGovernor()
	sleeper := &fakeSleeper{}
	cfg := retryCfg(t, upstream.srv.URL, governor, ledger, sleeper)

	var calledWith string
	var callCount int32
	cfg.OnCloudUnreachable = func(_ context.Context, taskID string) error {
		atomic.AddInt32(&callCount, 1)
		calledWith = taskID
		return nil
	}
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
	if !strings.Contains(string(body), MsgCloudUnreachableMarker) {
		t.Errorf("body = %s, want it to contain %q", body, MsgCloudUnreachableMarker)
	}
	if got := upstream.hits(); got != 3 {
		t.Fatalf("upstream hits = %d, want exactly 3 (max_inline)", got)
	}
	if atomic.LoadInt32(&callCount) != 1 {
		t.Fatalf("OnCloudUnreachable called %d times, want exactly 1", callCount)
	}
	if calledWith != "t_test" {
		t.Errorf("OnCloudUnreachable called with task_id=%q, want t_test", calledWith)
	}
	if got := ledger.countKind(EventCloudUnreachable); got != 1 {
		t.Errorf("proxy.cloud_unreachable ledgered %d times, want 1", got)
	}
	// The exhausted path never records usage against the governor (no
	// real response was ever usable) - model_call must never have fired.
	if got := ledger.countKind(EventModelCall); got != 0 {
		t.Errorf("model_call ledgered %d times, want 0 (exhausted call must not be billed)", got)
	}
}

// --- 401: zero retries, non-retryable, OnNonRetryableFailure fires once ---

func TestNonRetryableStatusNeverRetriesAndInvokesCallback(t *testing.T) {
	upstream := newStatusSequenceUpstream(t, []int{401})
	ledger := &fakeLedger{}
	governor := generousGovernor()
	sleeper := &fakeSleeper{}
	cfg := retryCfg(t, upstream.srv.URL, governor, ledger, sleeper)

	var reasonID string
	var callCount int32
	cfg.OnNonRetryableFailure = func(_ context.Context, taskID, reason string) error {
		atomic.AddInt32(&callCount, 1)
		reasonID = reason
		return nil
	}
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5"}`))
	req.Header.Set("x-api-key", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (forwarded unchanged)", resp.StatusCode)
	}
	if got := upstream.hits(); got != 1 {
		t.Fatalf("upstream hits = %d, want exactly 1 (never retried)", got)
	}
	if len(sleeper.recorded()) != 0 {
		t.Errorf("backoff delays recorded = %v, want none (non-retryable, never waits)", sleeper.recorded())
	}
	if !waitForCondition(t, time.Second, func() bool { return atomic.LoadInt32(&callCount) == 1 }) {
		t.Fatalf("OnNonRetryableFailure called %d times, want exactly 1", callCount)
	}
	if reasonID != "authentication_error" {
		t.Errorf("reasonID = %q, want authentication_error", reasonID)
	}
}

// --- retry-after honored ---

func TestRetryAfterHonoredOverBackoff(t *testing.T) {
	upstream := newStatusSequenceUpstream(t, []int{429, 200})
	upstream.retryAfter = "2"
	ledger := &fakeLedger{}
	governor := generousGovernor()
	sleeper := &fakeSleeper{}
	// A backoff base far bigger than 2s would prove Retry-After (not
	// backoff) was actually used, if we asserted the WRONG thing were
	// used - assert the recorded delay is exactly 2s, not the backoff's
	// own 1s first step.
	cfg := retryCfg(t, upstream.srv.URL, governor, ledger, sleeper)
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5"}`))
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
	if got := upstream.hits(); got != 2 {
		t.Fatalf("upstream hits = %d, want 2", got)
	}
	delays := sleeper.recorded()
	if len(delays) != 1 {
		t.Fatalf("recorded delays = %v, want exactly 1", delays)
	}
	if delays[0] != 2*time.Second {
		t.Errorf("delay = %v, want 2s (Retry-After honored, not the 1s backoff default)", delays[0])
	}
}

// --- Fable-5 request shaping ---

func TestFable5ShapingInjectsBetaAndFallback(t *testing.T) {
	upstream := newStatusSequenceUpstream(t, []int{200})
	ledger := &fakeLedger{}
	governor := generousGovernor()
	cfg := retryCfg(t, upstream.srv.URL, governor, ledger, nil)
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-fable-5","messages":[]}`))
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

	var seen struct {
		Model     string   `json:"model"`
		Betas     []string `json:"betas"`
		Fallbacks []struct {
			Model string `json:"model"`
		} `json:"fallbacks"`
	}
	if err := json.Unmarshal([]byte(upstream.bodyAt(0)), &seen); err != nil {
		t.Fatalf("decode upstream body: %v (%s)", err, upstream.bodyAt(0))
	}
	if seen.Model != "claude-fable-5" {
		t.Errorf("upstream model = %q, want claude-fable-5", seen.Model)
	}
	found := false
	for _, b := range seen.Betas {
		if b == "server-side-fallback-2026-06-01" {
			found = true
		}
	}
	if !found {
		t.Errorf("upstream betas = %v, want it to contain server-side-fallback-2026-06-01", seen.Betas)
	}
	if len(seen.Fallbacks) != 1 || seen.Fallbacks[0].Model != "claude-opus-4-8" {
		t.Errorf("upstream fallbacks = %v, want exactly [{model:claude-opus-4-8}]", seen.Fallbacks)
	}

	if got := ledger.countKind(EventFable5Shaped); got != 1 {
		t.Errorf("proxy.fable5_shaped ledgered %d times, want 1", got)
	}
}

func TestFable5ShapingNeverAppliesToOtherModels(t *testing.T) {
	upstream := newStatusSequenceUpstream(t, []int{200})
	ledger := &fakeLedger{}
	governor := generousGovernor()
	cfg := retryCfg(t, upstream.srv.URL, governor, ledger, nil)
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	req.Header.Set("x-api-key", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if strings.Contains(upstream.bodyAt(0), "fallbacks") {
		t.Errorf("upstream body = %s, want no fallbacks field injected for a non-Fable-5 model", upstream.bodyAt(0))
	}
	if got := ledger.countKind(EventFable5Shaped); got != 0 {
		t.Errorf("proxy.fable5_shaped ledgered %d times, want 0 (never for a non-Fable-5 model)", got)
	}
}

// --- secret-lane marker: fail-closed, zero upstream hits, even on the
// retry path (marker + 503 must not retry into the cloud either) ---

// fakeLaneLookup is a minimal secretlane.LaneLookup fake - one task_id is
// pinned lane="secret" (as W3-08's own router.go would have persisted it
// at task-creation time), completely independent of anything the request
// itself carries.
type fakeLaneLookup struct {
	lane string
}

func (f *fakeLaneLookup) GetTaskLane(_ context.Context, _ string) (lane, category string, found bool, err error) {
	if f.lane == "" {
		return "", "", false, nil
	}
	return f.lane, "finans", true, nil
}

func TestSecretLaneTaskNeverRetriesIntoCloud(t *testing.T) {
	// An upstream that would otherwise be retried 3 times (always 503) -
	// proving the block happens BEFORE the retry transport (indeed before
	// ANYTHING upstream-facing) ever runs, not merely that the retry loop
	// itself gives up quickly.
	upstream := newStatusSequenceUpstream(t, []int{503, 503, 503})
	ledger := &fakeLedger{}
	governor := generousGovernor()
	cfg := retryCfg(t, upstream.srv.URL, governor, ledger, &fakeSleeper{})

	lookup := &fakeLaneLookup{lane: secretlane.LaneSecret}
	backstop := secretlane.NewProxyBackstopHook(lookup, ledger)
	cfg.EgressGate = backstop("t_secret_task", "trace_secret")
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5"}`))
	req.Header.Set("x-api-key", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("status = %d, want a 4xx rejection", resp.StatusCode)
	}
	if !strings.Contains(string(body), secretlane.MsgSecretLaneCloudBlocked) {
		t.Errorf("body = %s, want the secret-lane blocked message", body)
	}
	if got := upstream.hits(); got != 0 {
		t.Fatalf("upstream hits = %d, want exactly 0 - a secret-lane task must NEVER reach the fake upstream, including via retry", got)
	}
	if got := ledger.countKind(secretlane.EventSecretLaneCloudBlocked); got != 1 {
		t.Errorf("secretlane_cloud_blocked ledgered %d times, want 1", got)
	}
}
