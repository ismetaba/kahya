// nocloud_fallback_test.go is the W4-03 no-cloud-fallback regression test
// (task spec step 8 / HANDOFF §4 memory-pressure ⚑): a secret-lane
// Reader job - whether it succeeds on the local model, or fails closed
// because the local model is unavailable - must NEVER cause a single
// byte to reach the W12-08 forward-proxy. Rather than a bare in-package
// counting fake, this spins up a REAL kahyad/internal/anthproxy.Proxy
// (the exact chokepoint HANDOFF names) in front of a real httptest
// upstream, and wires the test's CloudModel double to actually POST
// through it when (and only when) invoked - so RequestCount()==0
// afterward proves Run's own branching logic structurally never reached
// the cloud lane, not merely that a inert fake was never called.
package reader

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/mlx"
	"kahya/kahyad/internal/secretlane"
)

// newTestProxy starts a real anthproxy.Proxy in front of a no-op upstream
// httptest.Server, returning the proxy (for RequestCount()), its base URL,
// and its local auth token - everything a CloudModelFunc double needs to
// make a real inbound request to it.
func newTestProxy(t *testing.T) (proxy *anthproxy.Proxy, baseURL, token string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"{}"}]}`))
	}))
	t.Cleanup(upstream.Close)

	governor := anthproxy.NewGovernor(anthproxy.Limits{
		DailyBudgetUSD: 1000, MonthlyBudgetUSD: 10000, TaskTokenCeiling: 500000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)

	token = "kahya-task-00000000000000000000000000000000"
	p, err := anthproxy.New(anthproxy.ProxyConfig{
		TaskID: "t1", TraceID: "trace-nocloud", Token: token,
		UpstreamURL: upstream.URL, CredentialMode: anthproxy.CredentialModePassthrough,
		Governor: governor,
	})
	if err != nil {
		t.Fatalf("anthproxy.New: %v", err)
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatalf("Proxy.Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, addr, token
}

// realProxyCloudModel is a CloudModel that, if actually invoked, makes a
// real HTTP POST through the given proxy (mimicking what
// WorkerCloudModel's own worker-spawned HTTP traffic would eventually do)
// - so the proxy's own RequestCount() is a meaningful, not merely
// tautological, zero.
func realProxyCloudModel(baseURL, token string) CloudModel {
	return CloudModelFunc(func(ctx context.Context, jobType string, rawBytes []byte, traceID string) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(rawBytes))
		if err != nil {
			return "", err
		}
		req.Header.Set("x-api-key", token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		return `{}`, nil
	})
}

// TestNoCloudFallbackSecretLaneFixtureNeverHitsProxy is the step-8 test,
// verbatim: "secret-lane-classified fixture run => zero requests observed
// at the W12-08 forward-proxy".
func TestNoCloudFallbackSecretLaneFixtureNeverHitsProxy(t *testing.T) {
	proxy, baseURL, token := newTestProxy(t)
	fixture := loadFixture(t)

	local := &fakeLocalModel{response: validMailJSON}
	classifier := secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
		return secretlane.Verdict{SecretLane: true, Category: secretlane.CategoryFinans}, nil
	}))
	r := NewRunner(classifier, local, realProxyCloudModel(baseURL, token), nil, nil)

	if _, err := r.Run(context.Background(), JobTypeMailSummary, fixture, "trace-nocloud-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := proxy.RequestCount(); n != 0 {
		t.Fatalf("proxy.RequestCount() = %d, want 0 (a secret-lane job must never reach the forward-proxy)", n)
	}
}

// TestNoCloudFallbackLocalUnavailableNeverHitsProxy is the step-8 test,
// verbatim: "secret-lane fixture with the local model unavailable ... =>
// Reader job fails with reader.local_unavailable AND the proxy counter is
// still zero (no cloud fallback)".
func TestNoCloudFallbackLocalUnavailableNeverHitsProxy(t *testing.T) {
	proxy, baseURL, token := newTestProxy(t)
	fixture := loadFixture(t)

	local := &fakeLocalModel{err: mlx.ErrLocalModelUnavailable}
	classifier := secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
		return secretlane.Verdict{SecretLane: true, Category: secretlane.CategoryFinans}, nil
	}))
	ledger := &fakeLedger{}
	r := NewRunner(classifier, local, realProxyCloudModel(baseURL, token), nil, ledger)

	_, err := r.Run(context.Background(), JobTypeMailSummary, fixture, "trace-nocloud-2")
	if err == nil {
		t.Fatal("Run: expected an error (local model unavailable)")
	}
	if ledger.count(EventLocalUnavailable) != 1 {
		t.Fatalf("%s events = %d, want 1", EventLocalUnavailable, ledger.count(EventLocalUnavailable))
	}
	if n := proxy.RequestCount(); n != 0 {
		t.Fatalf("proxy.RequestCount() = %d, want 0 (local-unavailable must never fall back to the cloud forward-proxy)", n)
	}
}

// TestCloudLaneActuallyReachesTheProxy is a sanity/control test proving
// realProxyCloudModel (and the proxy fixture itself) is NOT a tautological
// always-zero setup: a genuinely non-secret-lane job DOES increment
// RequestCount(), so the two zero-count tests above are proving something
// real.
func TestCloudLaneActuallyReachesTheProxy(t *testing.T) {
	proxy, baseURL, token := newTestProxy(t)

	classifier := secretlane.NewClassifier(secretlane.QwenClassifierFunc(func(ctx context.Context, text string) (secretlane.Verdict, error) {
		return secretlane.Verdict{SecretLane: false, Category: secretlane.CategoryNone}, nil
	}))
	r := NewRunner(classifier, nil, realProxyCloudModel(baseURL, token), nil, nil)

	// The realProxyCloudModel double returns literal "{}" regardless of
	// what the (fake) upstream actually said, which fails mail_summary_v1
	// validation (missing required fields) - that is fine and expected
	// here; this test only cares whether the PROXY was reached, not
	// whether validation succeeded afterward.
	_, _ = r.Run(context.Background(), JobTypeMailSummary, []byte("herhangi bir metin"), "trace-control")

	if n := proxy.RequestCount(); n != 1 {
		t.Fatalf("proxy.RequestCount() = %d, want 1 (control case: a genuine cloud-lane call must reach the proxy)", n)
	}
}
