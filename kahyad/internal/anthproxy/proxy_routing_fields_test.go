package anthproxy

import (
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/router"
)

// FIX-FINAL proxy-fallbacks-bypass-allowlist-and-pricing.

// TestProxyRejectsUnallowlistedRoutingFields: the model allowlist checked
// only the top-level "model", so a non-Fable body's own "fallbacks"
// (naming any model) and a "speed":"fast" (a 2x price the table does not
// carry) were forwarded unchanged. They are refused with a 400 before any
// reservation, ledgered proxy.model_rejected{field}, and never forwarded.
func TestProxyRejectsUnallowlistedRoutingFields(t *testing.T) {
	for _, tc := range []struct{ name, body, field string }{
		{"sonnet_fallbacks_array", `{"model":"claude-sonnet-5-5","max_tokens":1,"fallbacks":[{"model":"claude-opus-4-1"}]}`, "fallbacks"},
		{"opus_fallbacks_default", `{"model":"claude-opus-5-5","max_tokens":1,"fallbacks":"default"}`, "fallbacks"},
		{"opus_speed_fast", `{"model":"claude-opus-5-5","max_tokens":1,"speed":"fast"}`, "speed"},
		{"fable_speed_fast", `{"model":"` + router.ModelFable + `","max_tokens":1,"speed":"fast"}`, "speed"},
		// FIX-FINAL proxy-inference-geo-unpriced-routing-field: "us" data
		// residency is a 1.1x premium the pricing table does not carry.
		{"opus_inference_geo_us", `{"model":"claude-opus-5-5","max_tokens":1,"inference_geo":"us"}`, "inference_geo"},
		{"sonnet_inference_geo_null", `{"model":"claude-sonnet-5-5","max_tokens":1,"Inference_Geo":null}`, "inference_geo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newJSONUpstream(t, 200, `{"usage":{}}`)
			ledger := &fakeLedger{}
			baseURL, _ := startTestProxy(t, testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil))
			status, body := doAndDrain(t, newAuthedRequest(t, "POST", baseURL+"/v1/messages", tc.body))
			if status != http.StatusBadRequest || !strings.Contains(body, MsgRoutingFieldNotAllowed) {
				t.Fatalf("status=%d body=%s, want 400 with %q", status, body, MsgRoutingFieldNotAllowed)
			}
			if upstream.hits() != 0 {
				t.Fatalf("upstream hits = %d, want 0", upstream.hits())
			}
			if got := ledger.countKind(EventModelRejected); got != 1 {
				t.Fatalf("%s = %d, want 1", EventModelRejected, got)
			}
			if p := lastPayload(t, ledger, EventModelRejected); p["field"] != tc.field {
				t.Errorf("payload field = %v, want %q", p["field"], tc.field)
			}
		})
	}
}

// TestProxyAllowsStandardSpeedAndShapedFableFallbacks: "speed":"standard"
// changes nothing, and a Fable body's own fallbacks are still accepted and
// overwritten with the owner-pinned claude-opus-4-8 array (G12-6).
func TestProxyAllowsStandardSpeedAndShapedFableFallbacks(t *testing.T) {
	for _, body := range []string{
		`{"model":"claude-sonnet-5-5","max_tokens":1,"speed":"standard"}`,
		`{"model":"claude-sonnet-5-5","max_tokens":1,"inference_geo":"global"}`,
		`{"model":"` + router.ModelFable + `","max_tokens":1,"fallbacks":[{"model":"claude-opus-4-1"}]}`,
	} {
		upstream := newJSONUpstream(t, 200, `{"usage":{}}`)
		ledger := &fakeLedger{}
		baseURL, _ := startTestProxy(t, testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil))
		if status, resp := doAndDrain(t, newAuthedRequest(t, "POST", baseURL+"/v1/messages", body)); status != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s, want 200", body, status, resp)
		}
		if upstream.hits() != 1 {
			t.Fatalf("%s: upstream hits = %d, want 1", body, upstream.hits())
		}
		if strings.Contains(upstream.last().body, "claude-opus-4-1") {
			t.Errorf("%s: forwarded body still names the client fallback: %s", body, upstream.last().body)
		}
	}
}

// TestProxyBillsEveryFallbackIteration: a Fable call refused mid-stream and
// re-served by claude-opus-4-8 reports both attempts in usage.iterations
// while top-level usage covers only the served one. The model_call must be
// billed at each attempt's own row, and the tokens of both attempts must
// count against the per-task ceiling.
func TestProxyBillsEveryFallbackIteration(t *testing.T) {
	const fableIn, fableOut, opusIn, opusOut = 200000, 4000, 200000, 6000
	iters := `[{"type":"message","model":"` + router.ModelFable + `","input_tokens":200000,"output_tokens":4000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},` +
		`{"type":"fallback_message","model":"claude-opus-4-8","input_tokens":200000,"output_tokens":6000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}]`
	now := time.Now()
	fableRow, err := PriceFor(router.ModelFable, now)
	if err != nil {
		t.Fatal(err)
	}
	opusRow, err := PriceFor("claude-opus-4-8", now)
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{InputTokens: fableIn, OutputTokens: fableOut}.CostUSD(fableRow) +
		Usage{InputTokens: opusIn, OutputTokens: opusOut}.CostUSD(opusRow)

	for _, sse := range []bool{false, true} {
		var upstream *recordingUpstream
		body := `{"model":"` + router.ModelFable + `","max_tokens":10000}`
		if sse {
			body = `{"model":"` + router.ModelFable + `","max_tokens":10000,"stream":true}`
			upstream = newSSEUpstream(t, []string{
				`data: {"type":"message_start","message":{"model":"claude-opus-4-8","usage":{"input_tokens":200000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
				``,
				`data: {"type":"message_delta","usage":{"output_tokens":6000,"iterations":` + iters + `}}`,
				``,
				`data: {"type":"message_stop"}`,
				``,
			}, 0)
		} else {
			upstream = newJSONUpstream(t, 200, `{"model":"claude-opus-4-8","usage":{"input_tokens":200000,"output_tokens":6000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"iterations":`+iters+`}}`)
		}
		ledger := &fakeLedger{}
		gov := generousGovernor()
		baseURL, _ := startTestProxy(t, testProxyConfig(t, upstream.srv.URL, gov, ledger, nil))
		if status, resp := doAndDrain(t, newAuthedRequest(t, "POST", baseURL+"/v1/messages", body)); status != http.StatusOK {
			t.Fatalf("sse=%v: status=%d body=%s", sse, status, resp)
		}
		if !waitForCondition(t, 2*time.Second, func() bool { return ledger.countKind(EventModelCall) == 1 }) {
			t.Fatalf("sse=%v: model_call rows = %d, want 1", sse, ledger.countKind(EventModelCall))
		}
		p := lastPayload(t, ledger, EventModelCall)
		if usd, _ := p["usd"].(float64); math.Abs(usd-want) > 1e-9 {
			t.Errorf("sse=%v: model_call usd = %v, want %v (each attempt at its own row)", sse, p["usd"], want)
		}
		gov.mu.Lock()
		got := gov.perTask["t_test"]
		gov.mu.Unlock()
		if wantTok := int64(fableIn + fableOut + opusIn + opusOut); got != wantTok {
			t.Errorf("sse=%v: per-task tokens = %d, want %d (both attempts count)", sse, got, wantTok)
		}
	}
}
