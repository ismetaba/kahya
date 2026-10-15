package anthproxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// FIX-I-1: the per-task local token travels in X-Kahya-Task-Token (set by
// spawn.BuildEnv through ANTHROPIC_CUSTOM_HEADERS); in passthrough mode
// ANTHROPIC_API_KEY is unset, so the worker's bundled CLI authenticates with
// its own Claude Code login and sends Authorization itself. These tests pin
// the request shapes the pinned CLI (claude-agent-sdk 0.2.111, bundled CLI
// 2.1.202) was observed to send by the hermetic header-capture probe
// (docs/ipc.md W12-08 note).

// newAuthedRequest builds the request shape the real worker's CLI sends in
// passthrough mode after FIX-I-1: the per-task token in X-Kahya-Task-Token
// plus the CLI's OWN upstream credential (a placeholder here - the proxy
// never reads it, it only checks that one exists).
func newAuthedRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set(TaskTokenHeader, testToken)
	req.Header.Set("Authorization", "Bearer test-upstream-credential")
	return req
}

// nonRetryableRecorder records every OnNonRetryableFailure call.
type nonRetryableRecorder struct {
	mu      sync.Mutex
	reasons []string
	taskIDs []string
}

func (r *nonRetryableRecorder) hook(_ context.Context, taskID, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.taskIDs = append(r.taskIDs, taskID)
	r.reasons = append(r.reasons, reason)
	return nil
}

func (r *nonRetryableRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reasons...)
}

func doAndDrain(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestProxyPassthroughRealCLIShapeOnlyAPIKeyHeader is the I-1 red test: with
// ANTHROPIC_API_KEY set, the real CLI sends ONLY x-api-key: <token> (no
// Authorization at all - its single credential source is the API key). The
// pre-fix proxy stripped that header and forwarded a credential-less request
// upstream (an upstream 401 and a byte leaving the machine for nothing). After
// the fix, passthrough mode refuses it locally: 401 + MsgNoUpstreamCredential,
// zero upstream hits, and the task fails exactly once.
func TestProxyPassthroughRealCLIShapeOnlyAPIKeyHeader(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	ledger := &fakeLedger{}
	rec := &nonRetryableRecorder{}
	cfg := testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil)
	cfg.CredentialMode = CredentialModePassthrough
	cfg.OnNonRetryableFailure = rec.hook
	baseURL, _ := startTestProxy(t, cfg)

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5-5"}`))
		req.Header.Set("x-api-key", testToken)
		status, body := doAndDrain(t, req)
		if status != http.StatusUnauthorized {
			t.Fatalf("request %d: status = %d, want 401 (no upstream credential in passthrough)", i, status)
		}
		if !strings.Contains(body, MsgNoUpstreamCredential) {
			t.Errorf("request %d: body = %s, want it to contain %q", i, body, MsgNoUpstreamCredential)
		}
	}
	if got := upstream.hits(); got != 0 {
		t.Fatalf("upstream hits = %d, want 0 - a credential-less request must never leave the machine", got)
	}
	if got := ledger.countKind(EventNoUpstreamCredential); got != 1 {
		t.Errorf("%s ledgered %d times across 2 requests, want exactly 1", EventNoUpstreamCredential, got)
	}
	if got := ledger.countKind(EventProxyAuthReject); got != 0 {
		t.Errorf("proxy_auth_reject ledgered %d times, want 0 (the local token itself was valid)", got)
	}
	if got := rec.snapshot(); len(got) != 1 || got[0] != ReasonNoUpstreamCredential {
		t.Errorf("OnNonRetryableFailure reasons = %v, want exactly [%s]", got, ReasonNoUpstreamCredential)
	}
}

// TestProxyPassthroughForwardsWorkerAuthUnchanged: the post-fix passthrough
// shape (token in X-Kahya-Task-Token, the CLI's own Authorization) reaches the
// upstream with Authorization byte-identical and the local token stripped
// from every header.
func TestProxyPassthroughForwardsWorkerAuthUnchanged(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{"input_tokens":7,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	ledger := &fakeLedger{}
	cfg := testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil)
	cfg.CredentialMode = CredentialModePassthrough
	baseURL, _ := startTestProxy(t, cfg)

	req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5-5"}`))
	req.Header.Set(TaskTokenHeader, testToken)
	req.Header.Set("Authorization", "Bearer worker-own-session-credential")
	if status, body := doAndDrain(t, req); status != 200 {
		t.Fatalf("status = %d, want 200 (body %s)", status, body)
	}

	if upstream.hits() != 1 {
		t.Fatalf("upstream hits = %d, want 1", upstream.hits())
	}
	got := upstream.last().headers
	if got.Get("Authorization") != "Bearer worker-own-session-credential" {
		t.Errorf("upstream Authorization = %q, want the worker's own credential forwarded unchanged", got.Get("Authorization"))
	}
	if v := got.Get(TaskTokenHeader); v != "" {
		t.Errorf("upstream %s = %q, want empty - the local token must never reach the upstream", TaskTokenHeader, v)
	}
	if v := got.Get("x-api-key"); v != "" {
		t.Errorf("upstream x-api-key = %q, want empty", v)
	}
	if !waitForCondition(t, time.Second, func() bool { return ledger.countKind(EventModelCall) == 1 }) {
		t.Fatal("model_call never ledgered in passthrough mode - metering must still happen")
	}
	if got := ledger.countKind(EventNoUpstreamCredential); got != 0 {
		t.Errorf("%s ledgered %d times, want 0", EventNoUpstreamCredential, got)
	}
}

// TestProxyKeychainModeAcceptsTaskTokenHeader: keychain mode authenticates
// the new carrier too, and the Director strips it along with every inbound
// auth header before injecting the real key.
func TestProxyKeychainModeAcceptsTaskTokenHeader(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	cfg := testProxyConfig(t, upstream.srv.URL, generousGovernor(), &fakeLedger{}, nil)
	cfg.CredentialMode = CredentialModeKeychain
	cfg.Credential = NewKeychainCredentialSource(&fakeKeychainReader{key: "sk-ant-fake"}, "prod", nil)
	baseURL, _ := startTestProxy(t, cfg)

	// The exact keychain-mode shape BuildEnv produces: ANTHROPIC_API_KEY=<token>
	// (x-api-key) AND ANTHROPIC_CUSTOM_HEADERS (X-Kahya-Task-Token).
	for _, legacyToo := range []bool{false, true} {
		req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5-5"}`))
		req.Header.Set(TaskTokenHeader, testToken)
		if legacyToo {
			req.Header.Set("x-api-key", testToken)
		}
		if status, body := doAndDrain(t, req); status != 200 {
			t.Fatalf("legacyToo=%v: status = %d, want 200 (body %s)", legacyToo, status, body)
		}
		got := upstream.last().headers
		if got.Get("x-api-key") != "sk-ant-fake" {
			t.Errorf("legacyToo=%v: upstream x-api-key = %q, want the injected keychain key", legacyToo, got.Get("x-api-key"))
		}
		if v := got.Get(TaskTokenHeader); v != "" {
			t.Errorf("legacyToo=%v: upstream %s = %q, want empty", legacyToo, TaskTokenHeader, v)
		}
		if v := got.Get("Authorization"); v != "" {
			t.Errorf("legacyToo=%v: upstream Authorization = %q, want empty", legacyToo, v)
		}
	}
}

// TestProxyRejectsWrongTaskTokenHeader: a wrong token in the new carrier is a
// local-auth failure even when a (correct-looking) legacy header is present -
// the dedicated header, when present, is the one that decides.
func TestProxyRejectsWrongTaskTokenHeader(t *testing.T) {
	for _, mode := range []string{CredentialModePassthrough, CredentialModeKeychain} {
		t.Run(mode, func(t *testing.T) {
			upstream := newJSONUpstream(t, 200, `{"usage":{}}`)
			ledger := &fakeLedger{}
			cfg := testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil)
			cfg.CredentialMode = mode
			cfg.Credential = NewKeychainCredentialSource(&fakeKeychainReader{key: "sk-ant-fake"}, "prod", nil)
			baseURL, _ := startTestProxy(t, cfg)

			req, _ := http.NewRequest("POST", baseURL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5-5"}`))
			req.Header.Set(TaskTokenHeader, "kahya-task-wrongwrongwrongwrongwrongwrong")
			req.Header.Set("x-api-key", testToken)
			req.Header.Set("Authorization", "Bearer some-credential")
			if status, _ := doAndDrain(t, req); status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", status)
			}
			if upstream.hits() != 0 {
				t.Errorf("upstream hits = %d, want 0", upstream.hits())
			}
			if got := ledger.countKind(EventProxyAuthReject); got != 1 {
				t.Errorf("proxy_auth_reject ledgered %d times, want 1", got)
			}
		})
	}
}

// TestTaskTokenHeaderLiteral pins the wire name (spawn.TaskTokenHeaderName,
// the worker and four test fixtures spell it independently).
func TestTaskTokenHeaderLiteral(t *testing.T) {
	if TaskTokenHeader != "X-Kahya-Task-Token" {
		t.Fatalf("TaskTokenHeader = %q, want X-Kahya-Task-Token", TaskTokenHeader)
	}
}
