package anthproxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// FIX-I-11 (review): the usage parser reads the upstream body as plain
// JSON/SSE. The pinned worker CLI sends "Accept-Encoding: gzip, deflate, br,
// zstd"; forwarded unchanged, an upstream that honours it answers with a
// compressed body, the parser sees compressed bytes and every such call was
// ledgered as model_call {input_tokens:0, output_tokens:0, usd:0, status:ok}
// - the cost governor went blind. The proxy now asks the upstream for
// "identity", and an upstream that compresses anyway is billed fail-closed
// at the governor's conservative reservation estimate, never $0.

// cliAcceptEncoding is the header the pinned claude CLI really sends
// (observed on the hermetic e2e gate's mock upstream).
const cliAcceptEncoding = "gzip, deflate, br, zstd"

const (
	encodingJSONUsage = `{"usage":{"input_tokens":100000,"output_tokens":50000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
	encodingSSEStart  = `data: {"type":"message_start","message":{"usage":{"input_tokens":100000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
	encodingSSEDelta  = `data: {"type":"message_delta","delta":{},"usage":{"output_tokens":50000}}`
)

// encodingUpstream is a /v1/messages stand-in that gzips its reply - either
// only when the request's Accept-Encoding allows gzip (a well-behaved
// server, as api.anthropic.com may be) or always (a misbehaving one that
// ignores "identity"). It records the Accept-Encoding of every request.
type encodingUpstream struct {
	mu   sync.Mutex
	seen []string
	srv  *httptest.Server
}

func (u *encodingUpstream) acceptEncodings() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.seen...)
}

func newEncodingUpstream(t *testing.T, alwaysGzip, sse bool, status int) *encodingUpstream {
	t.Helper()
	u := &encodingUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		ae := r.Header.Get("Accept-Encoding")
		u.mu.Lock()
		u.seen = append(u.seen, ae)
		u.mu.Unlock()
		compress := alwaysGzip || strings.Contains(ae, "gzip")
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		if compress {
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.WriteHeader(status)
		var out io.Writer = w
		var gz *gzip.Writer
		if compress {
			gz = gzip.NewWriter(w)
			out = gz
		}
		flush := func() {
			if gz != nil {
				_ = gz.Flush()
			}
			w.(http.Flusher).Flush()
		}
		switch {
		case status >= 400:
			_, _ = io.WriteString(out, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
		case sse:
			for _, e := range []string{encodingSSEStart, encodingSSEDelta, `data: {"type":"message_stop"}`} {
				_, _ = fmt.Fprintf(out, "%s\n\n", e)
				flush()
			}
		default:
			_, _ = io.WriteString(out, encodingJSONUsage)
		}
		if gz != nil {
			_ = gz.Close()
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// doEncodingRequest sends body through the proxy as the worker CLI would:
// with its own Accept-Encoding and no transparent decompression on the
// client side, so the test sees exactly the bytes the worker would.
func doEncodingRequest(t *testing.T, baseURL, body, acceptEncoding string) (*http.Response, []byte) {
	t.Helper()
	req := newAuthedRequest(t, "POST", baseURL+"/v1/messages", body)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	client := &http.Client{Transport: &http.Transport{DisableCompression: true, DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, raw
}

func lastPayload(t *testing.T, ledger *fakeLedger, kind string) map[string]any {
	t.Helper()
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	var payload map[string]any
	for _, c := range ledger.calls {
		if c.kind == kind {
			payload = c.payload
		}
	}
	if payload == nil {
		t.Fatalf("no %s row ledgered", kind)
	}
	return payload
}

// TestProxyAsksUpstreamForIdentityEncoding: whatever Accept-Encoding the
// worker sends (the real CLI's list, a lone gzip, or none), in either
// credential mode, the upstream is asked for an uncompressed body.
func TestProxyAsksUpstreamForIdentityEncoding(t *testing.T) {
	for _, mode := range []string{CredentialModePassthrough, CredentialModeKeychain} {
		for _, ae := range []string{cliAcceptEncoding, "gzip", ""} {
			t.Run(fmt.Sprintf("%s/%q", mode, ae), func(t *testing.T) {
				upstream := newJSONUpstream(t, 200, encodingJSONUsage)
				cfg := testProxyConfig(t, upstream.srv.URL, generousGovernor(), &fakeLedger{}, nil)
				cfg.CredentialMode = mode
				if mode == CredentialModeKeychain {
					cfg.Credential = NewKeychainCredentialSource(&fakeKeychainReader{key: "sk-ant-real-keychain-key"}, "prod", nil)
				}
				baseURL, _ := startTestProxy(t, cfg)
				resp, _ := doEncodingRequest(t, baseURL, `{"model":"claude-sonnet-5-5","max_tokens":1}`, ae)
				if resp.StatusCode != 200 {
					t.Fatalf("status = %d, want 200", resp.StatusCode)
				}
				if upstream.hits() != 1 {
					t.Fatalf("upstream hits = %d, want 1", upstream.hits())
				}
				if got := upstream.last().headers.Values("Accept-Encoding"); len(got) != 1 || got[0] != "identity" {
					t.Errorf("upstream Accept-Encoding = %q, want exactly [identity] (the usage parser reads plain bytes only)", got)
				}
			})
		}
	}
}

// TestProxyBillsRealUsageWhenWorkerAcceptsCompression is the reviewer's
// probe made permanent: an upstream that compresses whenever the request
// allows gzip, a worker that sends the real CLI's Accept-Encoding. The
// call must be billed with its real usage (red before the fix:
// input_tokens=0 output_tokens=0 usd=0 status=ok).
func TestProxyBillsRealUsageWhenWorkerAcceptsCompression(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			upstream := newEncodingUpstream(t, false, sse, 200)
			ledger := &fakeLedger{}
			baseURL, _ := startTestProxy(t, testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil))
			body := `{"model":"claude-sonnet-5-5","max_tokens":60000}`
			if sse {
				body = `{"model":"claude-sonnet-5-5","max_tokens":60000,"stream":true}`
			}
			resp, raw := doEncodingRequest(t, baseURL, body, cliAcceptEncoding)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ce := resp.Header.Get("Content-Encoding"); ce != "" {
				t.Errorf("worker got Content-Encoding %q, want an uncompressed body", ce)
			}
			if !bytes.Contains(raw, []byte(`"input_tokens":100000`)) {
				t.Errorf("worker body is not the plain upstream reply: %q", raw)
			}
			if !waitForCondition(t, 2*time.Second, func() bool { return ledger.countKind(EventModelCall) == 1 }) {
				t.Fatalf("model_call rows = %d, want 1", ledger.countKind(EventModelCall))
			}
			p := lastPayload(t, ledger, EventModelCall)
			if p["input_tokens"] != int64(100000) || p["output_tokens"] != int64(50000) {
				t.Errorf("model_call input_tokens=%v output_tokens=%v, want 100000/50000 (a compressed reply was billed as zero usage)", p["input_tokens"], p["output_tokens"])
			}
			row, err := PriceFor("claude-sonnet-5-5", time.Now())
			if err != nil {
				t.Fatalf("PriceFor: %v", err)
			}
			want := Usage{InputTokens: 100000, OutputTokens: 50000}.CostUSD(row)
			if usd, _ := p["usd"].(float64); math.Abs(usd-want) > 1e-9 {
				t.Errorf("model_call usd = %v, want %v", p["usd"], want)
			}
			if p["status"] != "ok" {
				t.Errorf("model_call status = %v, want ok", p["status"])
			}
			if n := ledger.countKind(EventEncodedBody); n != 0 {
				t.Errorf("%s ledgered %d times for a plain body, want 0", EventEncodedBody, n)
			}
		})
	}
}

// TestProxyBillsEncodedBodyFailClosed: an upstream that compresses even
// though it was asked for identity leaves a body the proxy cannot read.
// The call is billed at the governor's conservative reservation estimate
// (input side at the 1h cache-write rate, max_tokens at the output rate),
// ledgered proxy.encoded_body, and the reply still reaches the worker
// unchanged.
func TestProxyBillsEncodedBodyFailClosed(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			upstream := newEncodingUpstream(t, true, sse, 200)
			ledger := &fakeLedger{}
			baseURL, _ := startTestProxy(t, testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil))
			body := `{"model":"claude-sonnet-5-5","max_tokens":60000}`
			if sse {
				body = `{"model":"claude-sonnet-5-5","max_tokens":60000,"stream":true}`
			}
			resp, raw := doEncodingRequest(t, baseURL, body, cliAcceptEncoding)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ce := resp.Header.Get("Content-Encoding"); ce != "gzip" {
				t.Errorf("worker got Content-Encoding %q, want the upstream's gzip forwarded unchanged", ce)
			}
			zr, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("worker body is not the upstream's gzip stream: %v", err)
			}
			plain, err := io.ReadAll(zr)
			if err != nil || !bytes.Contains(plain, []byte(`"input_tokens":100000`)) {
				t.Errorf("worker body does not decode to the upstream reply: %q (err %v)", plain, err)
			}

			if !waitForCondition(t, 2*time.Second, func() bool { return ledger.countKind(EventModelCall) == 1 }) {
				t.Fatalf("model_call rows = %d, want 1", ledger.countKind(EventModelCall))
			}
			p := lastPayload(t, ledger, EventModelCall)
			row, err := PriceFor("claude-sonnet-5-5", time.Now())
			if err != nil {
				t.Fatalf("PriceFor: %v", err)
			}
			inEst := int64(len(body)) / estRequestBytesPerToken
			wantUSD := float64(inEst)*row.USDPerMTokCacheWrite1h/1_000_000 + float64(60000)*row.USDPerMTokOut/1_000_000
			usd, _ := p["usd"].(float64)
			if usd <= 0 || math.Abs(usd-wantUSD) > 1e-9 {
				t.Errorf("model_call usd = %v, want the reservation estimate %v (an unreadable body is never billed $0)", p["usd"], wantUSD)
			}
			if p["output_tokens"] != int64(60000) || p["cache_creation_input_tokens"] != inEst || p["input_tokens"] != int64(0) {
				t.Errorf("model_call tokens in/out/cache_creation = %v/%v/%v, want 0/60000/%d (the estimate's split)",
					p["input_tokens"], p["output_tokens"], p["cache_creation_input_tokens"], inEst)
			}
			if p["status"] != "ok" {
				t.Errorf("model_call status = %v, want ok", p["status"])
			}
			if n := ledger.countKind(EventEncodedBody); n != 1 {
				t.Fatalf("%s ledgered %d times, want 1", EventEncodedBody, n)
			}
			ev := lastPayload(t, ledger, EventEncodedBody)
			if ev["content_encoding"] != "gzip" || ev["task_id"] != "t_test" || ev["model"] != "claude-sonnet-5-5" {
				t.Errorf("%s payload = %v, want task_id/model/content_encoding=gzip", EventEncodedBody, ev)
			}
			if billed, _ := ev["billed_usd"].(float64); math.Abs(billed-wantUSD) > 1e-9 {
				t.Errorf("%s billed_usd = %v, want %v", EventEncodedBody, ev["billed_usd"], wantUSD)
			}
		})
	}
}

// TestProxyEncodedErrorResponseIsNotBilledAtEstimate: an error status
// carries no usage (Anthropic does not bill it) whether or not its body is
// readable, so an encoded error reply is ledgered like a plain one (usd 0,
// status error) and is not reported as an unreadable billable body.
func TestProxyEncodedErrorResponseIsNotBilledAtEstimate(t *testing.T) {
	upstream := newEncodingUpstream(t, true, false, http.StatusBadRequest)
	ledger := &fakeLedger{}
	baseURL, _ := startTestProxy(t, testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil))
	resp, _ := doEncodingRequest(t, baseURL, `{"model":"claude-sonnet-5-5","max_tokens":60000}`, cliAcceptEncoding)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !waitForCondition(t, 2*time.Second, func() bool { return ledger.countKind(EventModelCall) == 1 }) {
		t.Fatalf("model_call rows = %d, want 1", ledger.countKind(EventModelCall))
	}
	p := lastPayload(t, ledger, EventModelCall)
	if p["usd"] != float64(0) || p["status"] != "error" {
		t.Errorf("model_call usd=%v status=%v, want 0/error", p["usd"], p["status"])
	}
	if n := ledger.countKind(EventEncodedBody); n != 0 {
		t.Errorf("%s ledgered %d times for an error reply, want 0", EventEncodedBody, n)
	}
}
