// router_task_test.go covers W4-08's wiring of kahyad/internal/router into
// POST /v1/task: the "derin düşün" opt-in (both forms) selecting
// claude-fable-5 with the real per-task anthproxy.Proxy's own Fable-5
// shaping applied end to end, the cost-governor 80% downgrade rung routing
// a Sonnet-class chat task to the local lane, secret-lane pinning
// outranking --derin with zero bytes reaching the fake upstream, and the
// intent_classified -> routing_decision -> model_call ledger ordering.
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/store"
)

// recordingUpstream is a minimal fake Anthropic upstream - mirrors
// kahyad/internal/anthproxy's own W4-04 fake-upstream harness pattern
// (statusSequenceUpstream in that package's retry_test.go), driven here
// from a real POST /v1/task through model_cloud_worker.py instead of a raw
// call directly against the proxy.
type recordingUpstream struct {
	mu     sync.Mutex
	bodies []string
	srv    *httptest.Server
}

func newRecordingUpstream(t *testing.T) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, string(body))
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *recordingUpstream) hits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

func (u *recordingUpstream) bodyAt(i int) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[i]
}

func generousTestGovernor() *anthproxy.Governor {
	return anthproxy.NewGovernor(anthproxy.Limits{
		DailyBudgetUSD: 1000, MonthlyBudgetUSD: 10000, TaskTokenCeiling: 500000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
}

// modelCloudWorkerScript resolves this package's own testdata fixture that
// echoes the envelope's OWN model field into a real cloud call (unlike
// kahyad/internal/outbox/testdata/cloud_call_worker.py, which hardcodes
// claude-sonnet-5 - see that file's own doc comment for why this task
// needs a different fixture).
func modelCloudWorkerScript(t *testing.T) []string {
	t.Helper()
	return []string{"python3", filepath.Join("testdata", "model_cloud_worker.py")}
}

// assertUpstreamSawFable5Shaping decodes body and asserts it names
// claude-fable-5 with the mandatory server-side-fallback beta and Opus
// fallback (HANDOFF §4, verbatim) - the SAME assertion shape
// kahyad/internal/anthproxy's own TestFable5ShapingInjectsBetaAndFallback
// uses, reused here to prove the router's own opt-in entry point drives a
// REAL request all the way through that shaping.
func assertUpstreamSawFable5Shaping(t *testing.T, body string) {
	t.Helper()
	var seen struct {
		Model     string   `json:"model"`
		Betas     []string `json:"betas"`
		Fallbacks []struct {
			Model string `json:"model"`
		} `json:"fallbacks"`
	}
	if err := json.Unmarshal([]byte(body), &seen); err != nil {
		t.Fatalf("decode upstream body: %v (%s)", err, body)
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
}

// TestDerinFlagSelectsFable5WithProxyShaping proves `kahya ask --derin`
// (taskRequest.DeepThink) produces envelope model=claude-fable-5 and that
// the real per-task anthproxy.Proxy applies the mandatory Fable-5 shaping
// to the actual outbound request.
func TestDerinFlagSelectsFable5WithProxyShaping(t *testing.T) {
	upstream := newRecordingUpstream(t)
	f := newTaskTestFixtureWithGovernor(t, modelCloudWorkerScript(t), 5, upstream.srv.URL, generousTestGovernor())

	traceID := "trace-derin-flag-0000000000000001"
	resp := postTaskFull(t, f.client, traceID, "şu mimariyi değerlendir", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	last := frames[len(frames)-1]
	if last.event != "result" {
		t.Fatalf("last frame event = %q, want result; frames=%+v", last.event, frames)
	}

	if got := upstream.hits(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
	assertUpstreamSawFable5Shaping(t, upstream.bodyAt(0))
}

// TestDerinDusunPrefixSelectsFable5AndStripsPrefix proves the byte-exact
// Turkish prompt prefix "derin düşün: şu mimariyi değerlendir" ALSO
// produces envelope model=claude-fable-5 (with the same proxy shaping),
// and that the prefix itself is stripped before the worker ever sees the
// prompt (model_cloud_worker.py doesn't echo the prompt, so this is proven
// via the classifier/envelope path instead - see
// TestDerinDusunPrefixStrippedFromEnvelopePrompt below for the direct
// byte-level proof via echo_worker.py).
func TestDerinDusunPrefixSelectsFable5AndStripsPrefix(t *testing.T) {
	upstream := newRecordingUpstream(t)
	f := newTaskTestFixtureWithGovernor(t, modelCloudWorkerScript(t), 5, upstream.srv.URL, generousTestGovernor())

	traceID := "trace-derin-prefix-000000000000001"
	resp := postTaskFull(t, f.client, traceID, "derin düşün: şu mimariyi değerlendir", false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	last := frames[len(frames)-1]
	if last.event != "result" {
		t.Fatalf("last frame event = %q, want result; frames=%+v", last.event, frames)
	}

	if got := upstream.hits(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
	assertUpstreamSawFable5Shaping(t, upstream.bodyAt(0))
}

// TestDerinDusunPrefixStrippedFromEnvelopePrompt proves the prefix itself
// never reaches the worker's prompt (echo_worker.py echoes the envelope
// byte-exact, so its presence/absence in the delta is directly
// observable).
func TestDerinDusunPrefixStrippedFromEnvelopePrompt(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "echo_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-derin-strip-0000000000000001"
	resp := postTaskFull(t, f.client, traceID, "derin düşün: şu mimariyi değerlendir", false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)

	var echoed string
	for _, fr := range frames {
		if fr.event == "delta" {
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(fr.data), &d) == nil && strings.Contains(d.Text, "task_id") {
				echoed = d.Text // the FIRST delta is the raw envelope JSON, echoed byte-exact
				break
			}
		}
	}
	if echoed == "" {
		t.Fatal("no envelope-echo delta frame observed")
	}
	if strings.Contains(echoed, "derin düşün:") {
		t.Errorf("echoed envelope = %s, want the \"derin düşün:\" prefix STRIPPED from the prompt", echoed)
	}
	if !strings.Contains(echoed, "şu mimariyi değerlendir") {
		t.Errorf("echoed envelope = %s, want the remaining prompt content preserved", echoed)
	}
	if !strings.Contains(echoed, `"model":"claude-fable-5"`) {
		t.Errorf("echoed envelope = %s, want model:claude-fable-5", echoed)
	}
	if !strings.Contains(echoed, `"deep_think":true`) {
		t.Errorf("echoed envelope = %s, want deep_think:true", echoed)
	}
}

// TestDerinDusunPrefixAloneRejectedAsEmptyPrompt proves a prompt consisting
// of ONLY the "derin düşün:" prefix (nothing left after stripping) is
// rejected with the ordinary clean 400 "prompt must not be empty" - not
// envelope.Validate()'s generic 500 - a post-review fix (the initial
// empty-prompt check runs on the RAW pre-strip prompt, so a bare-prefix
// prompt would otherwise slip past it).
func TestDerinDusunPrefixAloneRejectedAsEmptyPrompt(t *testing.T) {
	f := newTaskTestFixture(t, []string{"/nonexistent/worker/binary/should/never/run"}, 5)

	resp := postTask(t, f.client, "trace-derin-empty-000000000000001", "derin düşün:   ")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestDowngradeRoutesSonnetClassChatTaskToLocalLane proves the ⚑ cost-
// governor's Sonnet->yerel rung: with Downgraded()==true (fixture spend at
// exactly 80% of daily_budget_usd, seeded via anthproxy.Boot - no ledger
// side effect of its own), an ordinary chat task (cfg.default_model =
// claude-sonnet-5) is answered ENTIRELY locally - the worker (a
// nonexistent binary) is never spawned - and its persisted tasks.lane
// stays "normal" (the CONTENT was never secret; only the ROUTE was local).
func TestDowngradeRoutesSonnetClassChatTaskToLocalLane(t *testing.T) {
	seedNow := time.Now().UTC()
	governor := anthproxy.Boot([]anthproxy.BootEvent{
		{Ts: seedNow, Record: anthproxy.ModelCallRecord{TaskID: "seed", Model: "claude-opus-4-8", USD: 8.0}},
	}, anthproxy.Limits{
		DailyBudgetUSD: 10, MonthlyBudgetUSD: 1000, TaskTokenCeiling: 500000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
	if !governor.Downgraded() {
		t.Fatal("test setup: governor.Downgraded() = false after seeding 80% spend, want true")
	}

	f := newTaskTestFixtureWithGovernor(t, []string{"/nonexistent/worker/binary/should/never/run"}, 5, "https://upstream.invalid", governor)
	f.srv.SetSecretLane(secretlane.NewClassifier(nil), secretlane.AnswererFunc(func(ctx context.Context, prompt string) (string, error) {
		return "Yerelde (indirgeme) işlendi: " + prompt, nil
	}), nil)

	traceID := "trace-downgrade-local-0000000000001"
	resp := postTask(t, f.client, traceID, "bugün hava çok güzel, parkta yürüyüş yaptım")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	var result map[string]any
	for _, fr := range frames {
		switch fr.event {
		case "result":
			if err := json.Unmarshal([]byte(fr.data), &result); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
		case "error":
			t.Fatalf("got an error SSE frame (worker path may have run instead of the local lane): %s", fr.data)
		}
	}
	if result == nil {
		t.Fatal("no result frame received")
	}
	if result["processed_locally"] != true {
		t.Errorf("result[processed_locally] = %v, want true (Sonnet-class chat task must route locally under downgrade)", result["processed_locally"])
	}

	taskID, _ := result["task_id"].(string)
	row, err := f.store.Queries.GetTaskLane(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTaskLane: %v", err)
	}
	if row.Lane != secretlane.LaneNormal {
		t.Errorf("persisted lane = %q, want %q (downgrade-forced local routing must never mislabel ordinary content as secret)", row.Lane, secretlane.LaneNormal)
	}

	assertLedgerHasKind(t, f.store, traceID, "routing_decision")
	assertLedgerPayloadContains(t, f.store, traceID, "routing_decision", "downgrade_sonnet_to_local")
	assertLedgerNeverHasKind(t, f.store, "budget_downgrade_unavailable")
}

// TestSecretLaneWithDerinPinnedLocalZeroUpstreamRequests proves the ⚑
// ordering invariant's strongest form under W4-08: a secret-lane prompt
// submitted WITH --derin is pinned to the local lane, with ZERO requests
// ever reaching the (fake) Anthropic upstream - the secret-lane pin
// outranks the derin opt-in entirely, exactly as
// kahyad/internal/router.SelectModel's own matrix test proves in
// isolation.
func TestSecretLaneWithDerinPinnedLocalZeroUpstreamRequests(t *testing.T) {
	upstream := newRecordingUpstream(t)
	f := newTaskTestFixtureWithGovernor(t, []string{"/nonexistent/worker/binary/should/never/run"}, 5, upstream.srv.URL, generousTestGovernor())
	f.srv.SetSecretLane(secretlane.NewClassifier(nil), secretlane.AnswererFunc(func(ctx context.Context, prompt string) (string, error) {
		return "Yerelde işlendi: " + prompt, nil
	}), nil)

	traceID := "trace-secret-derin-00000000000001"
	resp := postTaskFull(t, f.client, traceID, "kredi kartı ekstresi ekte, özetler misin", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	var result map[string]any
	for _, fr := range frames {
		switch fr.event {
		case "result":
			_ = json.Unmarshal([]byte(fr.data), &result)
		case "error":
			t.Fatalf("got an error SSE frame: %s", fr.data)
		}
	}
	if result == nil || result["processed_locally"] != true {
		t.Fatalf("result = %+v, want processed_locally=true", result)
	}
	if got := upstream.hits(); got != 0 {
		t.Fatalf("upstream hits = %d, want exactly 0 - lane==secret must outrank --derin unconditionally", got)
	}

	taskID, _ := result["task_id"].(string)
	row, err := f.store.Queries.GetTaskLane(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTaskLane: %v", err)
	}
	if row.Lane != secretlane.LaneSecret {
		t.Errorf("persisted lane = %q, want %q", row.Lane, secretlane.LaneSecret)
	}
}

// TestLedgerOrderingIntentRoutingModelCall proves the W4-08 ledger-ordering
// acceptance criterion: for one ordinary cloud-routed task, under a SINGLE
// trace_id, intent_classified precedes routing_decision precedes
// model_call.
func TestLedgerOrderingIntentRoutingModelCall(t *testing.T) {
	upstream := newRecordingUpstream(t)
	f := newTaskTestFixtureWithGovernor(t, modelCloudWorkerScript(t), 5, upstream.srv.URL, generousTestGovernor())

	traceID := "trace-ledger-order-00000000000001"
	resp := postTask(t, f.client, traceID, "bugün hava çok güzel")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	readAllSSE(t, resp)

	rows, err := f.store.DB().Query(`SELECT kind FROM events WHERE trace_id = ? ORDER BY id ASC`, traceID)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		kinds = append(kinds, k)
	}

	idx := func(kind string) int {
		for i, k := range kinds {
			if k == kind {
				return i
			}
		}
		t.Fatalf("no %q event found under trace_id=%q; got kinds=%v", kind, traceID, kinds)
		return -1
	}
	iIntent := idx("intent_classified")
	iRouting := idx("routing_decision")
	iModelCall := idx("model_call")
	if !(iIntent < iRouting && iRouting < iModelCall) {
		t.Errorf("ledger order = %v (intent_classified=%d, routing_decision=%d, model_call=%d), want intent_classified < routing_decision < model_call",
			kinds, iIntent, iRouting, iModelCall)
	}
}

// assertLedgerPayloadContains asserts SOME row with trace_id/kind has a
// payload containing substr (a simple, dependency-free way to check one
// field of a JSON payload without a full struct/schema for every event
// kind this test cares about).
func assertLedgerPayloadContains(t *testing.T, st *store.Store, traceID, kind, substr string) {
	t.Helper()
	rows, err := st.DB().Query(`SELECT payload FROM events WHERE trace_id = ? AND kind = ?`, traceID, kind)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var payloads []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan payload: %v", err)
		}
		payloads = append(payloads, p)
		if strings.Contains(p, substr) {
			return
		}
	}
	t.Fatalf("no kind=%q row under trace_id=%q has a payload containing %q; got payloads=%v", kind, traceID, substr, payloads)
}

// assertLedgerNeverHasKind asserts NO row anywhere in st's events table
// (regardless of trace_id) has this kind - used for
// EventBudgetDowngradeUnavail's retirement: no production code path can
// produce it any more, so it must never appear at all, not merely "not
// under this one trace".
func assertLedgerNeverHasKind(t *testing.T, st *store.Store, kind string) {
	t.Helper()
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`, kind).Scan(&count); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if count != 0 {
		t.Errorf("events with kind=%q = %d, want 0 (retired)", kind, count)
	}
}
