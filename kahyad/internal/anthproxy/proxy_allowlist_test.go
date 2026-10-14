package anthproxy

import (
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/router"
)

// FIX-I-10: the proxy forwards only allowlisted models to allowlisted
// endpoints, and never bills a forwarded call at $0.

// unknownModelID is a model id that is not, and never will be, in HANDOFF
// §9 - so this file keeps holding across model-id bumps (G12 tail).
const unknownModelID = "claude-unknown-9"

// TestProxyUnknownModelIsNotBilledAtZero: before FIX-I-10 an unknown model
// was forwarded and billed $0 (PriceFor's error swallowed), so it was
// invisible to the daily/monthly USD budget. Now it is refused outright.
func TestProxyUnknownModelIsNotBilledAtZero(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{"input_tokens":1000000,"output_tokens":1000000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	ledger := &fakeLedger{}
	gov := NewGovernor(Limits{DailyBudgetUSD: 10, MonthlyBudgetUSD: 150, TaskTokenCeiling: 5_000_000}, nil, nil)
	cfg := testProxyConfig(t, upstream.srv.URL, gov, ledger, nil)
	baseURL, _ := startTestProxy(t, cfg)

	status, body := doAndDrain(t, newAuthedRequest(t, "POST", baseURL+"/v1/messages", `{"model":"`+unknownModelID+`","max_tokens":1}`))
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (model outside the allowlist); body %s", status, body)
	}
	if !strings.Contains(body, MsgModelNotAllowed) {
		t.Errorf("body = %s, want it to contain %q", body, MsgModelNotAllowed)
	}
	if upstream.hits() != 0 {
		t.Fatalf("upstream hits = %d, want 0", upstream.hits())
	}
	if got := ledger.countKind(EventModelRejected); got != 1 {
		t.Fatalf("%s ledgered %d times, want 1", EventModelRejected, got)
	}
	if got := ledger.countKind(EventModelCall); got != 0 {
		t.Errorf("model_call ledgered %d times, want 0", got)
	}
}

// TestProxyRejectsModelOutsideAllowlist: a REAL Anthropic id that HANDOFF
// §9 does not approve, and a body with no model at all, are both refused
// before any reservation; the payload names the requested model.
func TestProxyRejectsModelOutsideAllowlist(t *testing.T) {
	for _, tc := range []struct{ name, body, model string }{
		{"unapproved_real_id", `{"model":"claude-opus-4-1","max_tokens":1}`, "claude-opus-4-1"},
		{"no_model_field", `{"max_tokens":1}`, ""},
		{"malformed_body", `not json`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newJSONUpstream(t, 200, `{"usage":{}}`)
			ledger := &fakeLedger{}
			cfg := testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil)
			baseURL, p := startTestProxy(t, cfg)

			if status, body := doAndDrain(t, newAuthedRequest(t, "POST", baseURL+"/v1/messages", tc.body)); status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", status, body)
			}
			if upstream.hits() != 0 {
				t.Fatalf("upstream hits = %d, want 0", upstream.hits())
			}
			ledger.mu.Lock()
			var payloadModel any = "<none>"
			for _, c := range ledger.calls {
				if c.kind == EventModelRejected {
					payloadModel = c.payload["model"]
				}
			}
			ledger.mu.Unlock()
			if payloadModel != tc.model {
				t.Errorf("%s payload model = %v, want %q", EventModelRejected, payloadModel, tc.model)
			}
			g := p.cfg.Governor
			g.mu.Lock()
			reserved := g.nextReservationID
			g.mu.Unlock()
			if reserved != 0 {
				t.Errorf("governor made %d reservation(s) for a rejected model, want 0 (rejected before reserving)", reserved)
			}
		})
	}
}

// TestProxyAliasesDatedCLIAuxiliaryHaiku (owner decision G11-3 (i)): the
// CLI's dated auxiliary Haiku id is allowlisted and priced as the HANDOFF
// id it is a snapshot of, and forwarded with its body unchanged.
func TestProxyAliasesDatedCLIAuxiliaryHaiku(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{"input_tokens":1000,"output_tokens":1000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	ledger := &fakeLedger{}
	cfg := testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil)
	baseURL, _ := startTestProxy(t, cfg)

	const dated = "claude-haiku-4-5-20251001"
	target := CanonicalModel(dated)
	if target == dated {
		t.Fatalf("CanonicalModel(%q) = itself, want its HANDOFF alias target", dated)
	}
	if status, body := doAndDrain(t, newAuthedRequest(t, "POST", baseURL+"/v1/messages", `{"model":"`+dated+`","max_tokens":8}`)); status != 200 {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if !strings.Contains(upstream.last().body, dated) {
		t.Errorf("upstream body = %s, want the dated id forwarded unchanged", upstream.last().body)
	}
	if !waitForCondition(t, time.Second, func() bool { return ledger.countKind(EventModelCall) == 1 }) {
		t.Fatal("model_call never ledgered")
	}
	row, err := PriceFor(target, time.Now())
	if err != nil {
		t.Fatalf("PriceFor(%q): %v", target, err)
	}
	call := ledger.calls[len(ledger.calls)-1]
	if call.payload["model"] != target {
		t.Errorf("model_call model = %v, want the alias target %q", call.payload["model"], target)
	}
	want := Usage{InputTokens: 1000, OutputTokens: 1000}.CostUSD(row)
	if got, _ := call.payload["usd"].(float64); math.Abs(got-want) > 1e-12 || got <= 0 {
		t.Errorf("model_call usd = %v, want %v (priced as %s)", call.payload["usd"], want, target)
	}
	if got := ledger.countKind(EventModelRejected) + ledger.countKind(EventPriceUnknown); got != 0 {
		t.Errorf("model_rejected/price_unknown ledgered %d times, want 0", got)
	}
}

// TestProxyRejectsUnknownPath (owner decision G11-4): only /v1/messages and
// /v1/messages/count_tokens reach the upstream.
func TestProxyRejectsUnknownPath(t *testing.T) {
	for _, path := range []string{"/v1/files", "/v1/messages/batches", "/v1/models", "/v1/messages/", "/api/oauth/profile"} {
		t.Run(strings.ReplaceAll(strings.Trim(path, "/"), "/", "_"), func(t *testing.T) {
			upstream := newJSONUpstream(t, 200, `{"usage":{}}`)
			ledger := &fakeLedger{}
			cfg := testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil)
			baseURL, _ := startTestProxy(t, cfg)

			status, body := doAndDrain(t, newAuthedRequest(t, "POST", baseURL+path, `{"model":"claude-sonnet-5"}`))
			if status != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body %s", status, body)
			}
			if !strings.Contains(body, MsgPathNotAllowed) {
				t.Errorf("body = %s, want it to contain %q", body, MsgPathNotAllowed)
			}
			if upstream.hits() != 0 {
				t.Fatalf("upstream hits = %d, want 0", upstream.hits())
			}
			if got := ledger.countKind(EventPathRejected); got != 1 {
				t.Errorf("%s ledgered %d times, want 1", EventPathRejected, got)
			}
		})
	}
}

// TestProxyPathCheckRunsAfterLocalAuth: an unauthenticated caller learns
// nothing about the allowlist - it gets the local-auth 401, not a 404.
func TestProxyPathCheckRunsAfterLocalAuth(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{}}`)
	ledger := &fakeLedger{}
	baseURL, _ := startTestProxy(t, testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil))
	req, _ := http.NewRequest("POST", baseURL+"/v1/files", strings.NewReader(`{}`))
	if status, _ := doAndDrain(t, req); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if got := ledger.countKind(EventPathRejected); got != 0 {
		t.Errorf("%s ledgered %d times for an unauthenticated request, want 0", EventPathRejected, got)
	}
}

// TestProxyAllowsCountTokensPath: the second allowlisted endpoint (with the
// CLI's ?beta=true query) is forwarded.
func TestProxyAllowsCountTokensPath(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"input_tokens":12}`)
	ledger := &fakeLedger{}
	baseURL, _ := startTestProxy(t, testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil))
	if status, body := doAndDrain(t, newAuthedRequest(t, "POST", baseURL+"/v1/messages/count_tokens?beta=true", `{"model":"claude-sonnet-5","messages":[]}`)); status != 200 {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if upstream.hits() != 1 {
		t.Fatalf("upstream hits = %d, want 1", upstream.hits())
	}
	if got := ledger.countKind(EventPathRejected); got != 0 {
		t.Errorf("%s ledgered %d times, want 0", EventPathRejected, got)
	}
}

// TestProxyPricesUnpriceableModelAtMaxRate: if an allowlisted model somehow
// has no pricing row (here: a test allowlist that admits one), the call is
// billed at MaxPriceRow and ledgered proxy.price_unknown - never $0.
func TestProxyPricesUnpriceableModelAtMaxRate(t *testing.T) {
	upstream := newJSONUpstream(t, 200, `{"usage":{"input_tokens":1000,"output_tokens":1000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
	ledger := &fakeLedger{}
	cfg := testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil)
	cfg.AllowedModels = map[string]bool{"claude-mystery-1": true}
	baseURL, _ := startTestProxy(t, cfg)

	if status, body := doAndDrain(t, newAuthedRequest(t, "POST", baseURL+"/v1/messages", `{"model":"claude-mystery-1","max_tokens":8}`)); status != 200 {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if !waitForCondition(t, time.Second, func() bool { return ledger.countKind(EventModelCall) == 1 }) {
		t.Fatal("model_call never ledgered")
	}
	want := Usage{InputTokens: 1000, OutputTokens: 1000}.CostUSD(MaxPriceRow(time.Now()))
	var usd float64
	ledger.mu.Lock()
	var billedAs any
	for _, c := range ledger.calls {
		switch c.kind {
		case EventModelCall:
			usd, _ = c.payload["usd"].(float64)
		case EventPriceUnknown:
			billedAs = c.payload["billed_as"]
		}
	}
	ledger.mu.Unlock()
	if want <= 0 || math.Abs(usd-want) > 1e-12 {
		t.Errorf("model_call usd = %v, want %v (MaxPriceRow, never $0)", usd, want)
	}
	if got := ledger.countKind(EventPriceUnknown); got != 1 {
		t.Errorf("%s ledgered %d times, want 1", EventPriceUnknown, got)
	}
	if billedAs != MaxPriceRow(time.Now()).Model {
		t.Errorf("%s billed_as = %v, want %q", EventPriceUnknown, billedAs, MaxPriceRow(time.Now()).Model)
	}
}

// TestProxyNilAllowedModelsDefaultsToPricedSet: New() with no allowlist uses
// DefaultAllowedModels, and an empty non-nil map refuses everything.
func TestProxyNilAllowedModelsDefaultsToPricedSet(t *testing.T) {
	p, err := New(testProxyConfig(t, "http://127.0.0.1:1", generousGovernor(), nil, nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for m := range DefaultAllowedModels() {
		if !p.cfg.AllowedModels[m] {
			t.Errorf("default allowlist is missing priced model %q", m)
		}
	}
	cfg := testProxyConfig(t, "http://127.0.0.1:1", generousGovernor(), nil, nil)
	cfg.AllowedModels = map[string]bool{}
	p2, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(p2.cfg.AllowedModels) != 0 {
		t.Errorf("an explicit empty allowlist was replaced: %v", p2.cfg.AllowedModels)
	}
}

// TestProxyRejectsCaseVariantOrDuplicateModelKey (FIX-I-10, review): the
// proxy reads the body's model exactly as the upstream does - the exact,
// case-sensitive "model" member. Before this fix probeRequest used
// encoding/json's case-insensitive, last-match-wins struct decoding, so
// {"model":<not allowlisted>,"MODEL":<allowlisted>} was allowlisted and
// priced as the second model while the upstream ran the first one. A body
// whose top-level member names collide under case folding (exact
// duplicates included) is now refused before any reservation, with zero
// upstream bytes - and Fable-5 shaping never "launders" it into a clean
// single-model body first.
func TestProxyRejectsCaseVariantOrDuplicateModelKey(t *testing.T) {
	allowed := router.ModelHaiku
	for _, tc := range []struct{ name, body string }{
		{"case_variant_key", `{"model":"` + unknownModelID + `","MODEL":"` + allowed + `","max_tokens":1}`},
		{"case_variant_key_first", `{"MODEL":"` + allowed + `","model":"` + unknownModelID + `","max_tokens":1}`},
		{"duplicate_exact_key", `{"model":"` + unknownModelID + `","model":"` + allowed + `","max_tokens":1}`},
		{"lone_case_variant_key", `{"Model":"` + allowed + `","max_tokens":1}`},
		{"duplicate_key_ending_in_fable", `{"model":"` + allowed + `","model":"` + fable5Model + `","max_tokens":1}`},
		{"other_member_collision", `{"model":"` + allowed + `","max_tokens":1,"MAX_TOKENS":64000}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newJSONUpstream(t, 200, `{"usage":{"input_tokens":1000,"output_tokens":1000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`)
			ledger := &fakeLedger{}
			cfg := testProxyConfig(t, upstream.srv.URL, generousGovernor(), ledger, nil)
			baseURL, p := startTestProxy(t, cfg)

			status, body := doAndDrain(t, newAuthedRequest(t, "POST", baseURL+"/v1/messages", tc.body))
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (ambiguous body); body %s", status, body)
			}
			if !strings.Contains(body, MsgModelNotAllowed) {
				t.Errorf("body = %s, want it to contain %q", body, MsgModelNotAllowed)
			}
			if upstream.hits() != 0 {
				t.Fatalf("upstream hits = %d, want 0; forwarded body %s", upstream.hits(), upstream.last().body)
			}
			if got := ledger.countKind(EventModelRejected); got != 1 {
				t.Errorf("%s ledgered %d times, want 1", EventModelRejected, got)
			}
			if got := ledger.countKind(EventFable5Shaped) + ledger.countKind(EventModelCall); got != 0 {
				t.Errorf("fable5_shaped/model_call ledgered %d times, want 0", got)
			}
			g := p.cfg.Governor
			g.mu.Lock()
			reserved := g.nextReservationID
			g.mu.Unlock()
			if reserved != 0 {
				t.Errorf("governor made %d reservation(s) for an ambiguous body, want 0", reserved)
			}
		})
	}
}

// TestTopLevelFieldsReadsExactKeysAndRefusesAmbiguity pins the one body
// parser every proxy decision uses: exact member names (escapes decoded,
// as any JSON parser does), and ok=false for anything but exactly one
// well-formed object with fold-distinct member names - folded the way
// encoding/json folds them (the Kelvin sign and the long s collide too).
func TestTopLevelFieldsReadsExactKeysAndRefusesAmbiguity(t *testing.T) {
	fields, ok := topLevelFields([]byte(`{"model":"m","system":[{"text":"s"}],"Other":1}`))
	if !ok {
		t.Fatal("a well-formed, unambiguous object was refused")
	}
	if string(fields["model"]) != `"m"` || string(fields["Other"]) != `1` || len(fields) != 3 {
		t.Errorf("fields = %v, want exact keys model/system/Other", fields)
	}
	if _, folded := fields["other"]; folded {
		t.Error("a member was returned under a case-folded name")
	}
	for _, body := range []string{
		``,
		`not json`,
		`[]`,
		`"model"`,
		`null`,
		`{"model":"a","model":"b"}`,
		`{"model":"a","MODEL":"b"}`,
		`{"a":1,"A":2}`,
		`{"k":1,"K":2}`,
		`{"s":1,"ſ":2}`,
		`{"model":"a"} {"model":"b"}`,
		`{"model":"a"}}`,
	} {
		if _, ok := topLevelFields([]byte(body)); ok {
			t.Errorf("topLevelFields(%q) ok = true, want false", body)
		}
	}
}
