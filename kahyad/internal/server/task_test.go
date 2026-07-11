package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store"
)

// ---- mapTaskOutcome: pure-function unit tests (no real process/timeout
// needed - see that function's own doc comment for why). ----

func TestMapTaskOutcomeSuccess(t *testing.T) {
	m := mapTaskOutcome(nil, spawn.Outcome{Status: spawn.StatusOK, SessionID: "sess-1"}, "trace1", "task1", 30)
	if m.finalState != "done" || m.ledgerKind != "task_done" || m.sseEvent != "result" {
		t.Fatalf("got %+v", m)
	}
	payload, ok := m.ssePayload.(map[string]any)
	if !ok {
		t.Fatalf("ssePayload type = %T, want map[string]any", m.ssePayload)
	}
	if payload["status"] != "ok" || payload["task_id"] != "task1" || payload["session_id"] != "sess-1" {
		t.Errorf("ssePayload = %+v", payload)
	}
}

func TestMapTaskOutcomeSpawnError(t *testing.T) {
	m := mapTaskOutcome(fmt.Errorf("boom"), spawn.Outcome{}, "trace1", "task1", 30)
	if m.finalState != "error" || m.ledgerKind != "task_error" || m.sseEvent != "error" {
		t.Fatalf("got %+v", m)
	}
	payload := m.ssePayload.(map[string]string)
	want := fmt.Sprintf(MsgTaskUnexpectedExit, "trace1")
	if payload["message"] != want {
		t.Errorf("message = %q, want %q", payload["message"], want)
	}
}

func TestMapTaskOutcomeTimeout(t *testing.T) {
	m := mapTaskOutcome(nil, spawn.Outcome{Status: spawn.StatusTimeout}, "trace1", "task1", 7)
	if m.finalState != "error" || m.ledgerKind != "task_timeout" || m.sseEvent != "error" {
		t.Fatalf("got %+v", m)
	}
	payload := m.ssePayload.(map[string]string)
	want := fmt.Sprintf(MsgTaskTimeout, 7)
	if payload["message"] != want {
		t.Errorf("message = %q, want %q", payload["message"], want)
	}
}

func TestMapTaskOutcomeErrorWithWorkerMessage(t *testing.T) {
	m := mapTaskOutcome(nil, spawn.Outcome{Status: spawn.StatusError, ErrMsg: "ozel hata"}, "trace1", "task1", 30)
	if m.finalState != "error" || m.ledgerKind != "task_error" || m.sseEvent != "error" {
		t.Fatalf("got %+v", m)
	}
	payload := m.ssePayload.(map[string]string)
	if payload["message"] != "ozel hata" {
		t.Errorf("message = %q, want %q", payload["message"], "ozel hata")
	}
}

func TestMapTaskOutcomeErrorWithoutWorkerMessage(t *testing.T) {
	m := mapTaskOutcome(nil, spawn.Outcome{Status: spawn.StatusError, ErrMsg: ""}, "trace1", "task1", 30)
	payload := m.ssePayload.(map[string]string)
	want := fmt.Sprintf(MsgTaskUnexpectedExit, "trace1")
	if payload["message"] != want {
		t.Errorf("message = %q, want %q", payload["message"], want)
	}
}

// ---- /v1/task and /policy/check: real end-to-end fixture ----

// spawnTestdataDir resolves kahyad/internal/spawn/testdata relative to
// this package's own source directory (go test's working directory is
// always the package under test's directory) - this file reuses that
// package's fake worker scripts rather than forking a second copy.
func spawnTestdataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "spawn", "testdata")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("spawn testdata dir not found at %s: %v", dir, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	return abs
}

type taskTestFixture struct {
	srv    *Server
	client *http.Client
	store  *store.Store
}

func newTaskTestFixture(t *testing.T, workerCmd []string, timeoutMin int) taskTestFixture {
	t.Helper()
	cfg := config.Config{
		DBPath:               filepath.Join(t.TempDir(), "brain.db"),
		MemoryDir:            t.TempDir(),
		Socket:               filepath.Join(shortSocketDir(t), "k.sock"),
		LogDir:               t.TempDir(),
		AnthropicUpstreamURL: "https://upstream.invalid",
		DefaultModel:         "claude-sonnet-5",
		TaskTimeoutMin:       timeoutMin,
		WorkerCmd:            workerCmd,
	}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	log := testLogger(t)
	srv := New(cfg, log, "v-task-test", healthyDB)
	srv.SetEventLogger(st)
	srv.SetTaskStore(st.Queries)
	// W12-08: handleTask now requires an anthproxy Governor to be wired
	// (same "unwired dependency -> 503" posture as taskStore). Generous
	// limits so none of these pre-existing W12-07 fixtures ever trip a
	// budget/ceiling block; passthrough mode needs no Keychain/real key.
	srv.SetAnthproxy(anthproxy.NewGovernor(anthproxy.Limits{
		DailyBudgetUSD:         1000,
		MonthlyBudgetUSD:       10000,
		TaskTokenCeiling:       500000,
		DowngradeAtRatio:       0.8,
		CacheHitAlarmThreshold: 0.5,
	}, nil, nil), nil, anthproxy.NewPassthroughCredentialSource(), nil)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })

	return taskTestFixture{srv: srv, client: unixHTTPClient(cfg.Socket), store: st}
}

// sseFrame is one parsed "event: X\ndata: Y" frame from a raw SSE body.
type sseFrame struct {
	event string
	data  string
}

// readAllSSE fully drains an SSE response body into its frames, in order.
func readAllSSE(t *testing.T, body *http.Response) []sseFrame {
	t.Helper()
	defer body.Body.Close()
	var frames []sseFrame
	var evType string
	var dataLines []string
	scanner := bufio.NewScanner(body.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	flush := func() {
		if evType == "" && len(dataLines) == 0 {
			return
		}
		frames = append(frames, sseFrame{event: evType, data: strings.Join(dataLines, "\n")})
		evType, dataLines = "", nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event:"):
			evType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE body: %v", err)
	}
	return frames
}

func postTask(t *testing.T, client *http.Client, traceID, prompt string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"prompt": prompt, "trace_id": traceID})
	req, err := http.NewRequest(http.MethodPost, "http://kahyad/v1/task", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kahya-Trace-Id", traceID)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/task: %v", err)
	}
	return resp
}

// TestTaskEndToEndSuccessEchoWorker drives the full path through a real
// worker process (the spawn package's echo fake): SSE deltas arrive, the
// terminal event is "result" status=ok, the tasks row ends up state=done,
// and the ledger carries task_spawned + task_done under the SAME trace_id
// the request used (W12-07 acceptance criteria).
func TestTaskEndToEndSuccessEchoWorker(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "echo_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-echo-0000000000000000000001"
	resp := postTask(t, f.client, traceID, "test sorusu")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	frames := readAllSSE(t, resp)
	if len(frames) == 0 {
		t.Fatal("no SSE frames received")
	}
	last := frames[len(frames)-1]
	if last.event != "result" {
		t.Fatalf("last frame event = %q, want %q; frames=%+v", last.event, "result", frames)
	}
	var result struct {
		Status    string `json:"status"`
		TaskID    string `json:"task_id"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(last.data), &result); err != nil {
		t.Fatalf("decode result frame: %v (data=%s)", err, last.data)
	}
	if result.Status != "ok" {
		t.Errorf("result.status = %q, want ok", result.Status)
	}
	if result.TaskID == "" {
		t.Error("result.task_id is empty")
	}

	// At least one delta frame should have echoed the prompt back (the
	// envelope's own bytes, per echo_worker.py).
	sawPrompt := false
	for _, fr := range frames {
		if fr.event == "delta" && strings.Contains(fr.data, "test sorusu") {
			sawPrompt = true
		}
	}
	if !sawPrompt {
		t.Errorf("no delta frame echoed the prompt; frames=%+v", frames)
	}

	// tasks row ends up state=done. QueryRow (not Query+Next+deferred
	// Close) matters here: store.Open caps the sqlite pool at exactly one
	// connection (see that package's SetMaxOpenConns(1) comment), and a
	// deferred Close only runs at THIS FUNCTION's return, not after this
	// block - an open *sql.Rows would hold that one connection checked out
	// through the assertLedgerHasKind calls below, deadlocking them.
	var state string
	if err := f.store.DB().QueryRow(`SELECT state FROM tasks WHERE id = ?`, result.TaskID).Scan(&state); err != nil {
		t.Fatalf("query tasks: %v", err)
	}
	if state != "done" {
		t.Errorf("tasks.state = %q, want done", state)
	}

	// ledger has task_spawned and task_done under the same trace_id.
	assertLedgerHasKind(t, f.store, traceID, "task_spawned")
	assertLedgerHasKind(t, f.store, traceID, "task_done")
}

// TestTaskEndToEndUnexpectedExitBecomesTaskError drives the exit3 fake
// worker (delta then exit 3, no terminal result/error line) and asserts
// the SSE stream ends with the generic Turkish "unexpected exit" error,
// the tasks row is state=error, and the ledger carries task_error.
func TestTaskEndToEndUnexpectedExitBecomesTaskError(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "exit3_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-exit3-000000000000000000001"
	resp := postTask(t, f.client, traceID, "bir soru")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	frames := readAllSSE(t, resp)
	last := frames[len(frames)-1]
	if last.event != "error" {
		t.Fatalf("last frame event = %q, want error; frames=%+v", last.event, frames)
	}
	var errResp struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(last.data), &errResp); err != nil {
		t.Fatalf("decode error frame: %v", err)
	}
	if !strings.Contains(errResp.Message, "kahya log --trace") || !strings.Contains(errResp.Message, traceID) {
		t.Errorf("error message = %q, want it to reference `kahya log --trace %s`", errResp.Message, traceID)
	}

	var state string
	if err := f.store.DB().QueryRow(`SELECT state FROM tasks ORDER BY created_at DESC LIMIT 1`).Scan(&state); err != nil {
		t.Fatalf("query tasks: %v", err)
	}
	if state != "error" {
		t.Errorf("tasks.state = %q, want error", state)
	}
	assertLedgerHasKind(t, f.store, traceID, "task_error")
}

// TestTaskSurvivesClientDisconnectAndRecordsOutcome is a regression test
// for a bug caught during this task's own manual verification: the CLI's
// 30s idle-read timeout can close the connection BEFORE
// cfg.task_timeout_min elapses server-side. Since taskCtx is derived from
// the request's own context, that disconnect cancels taskCtx early too
// (desirable - it stops the orphaned worker promptly) - but the
// tasks-table/ledger writes that RECORD the resulting outcome must still
// succeed even though the request context is now also cancelled. Uses the
// hang fake with a generous 5-minute server-side timeout specifically so
// the only thing that can end this task early is the client disconnect
// itself, not a real task_timeout_min elapsing.
func TestTaskSurvivesClientDisconnectAndRecordsOutcome(t *testing.T) {
	script := filepath.Join(spawnTestdataDir(t), "hang_worker.py")
	f := newTaskTestFixture(t, []string{"python3", script}, 5)

	traceID := "trace-disconnect-00000000000000001"
	body, _ := json.Marshal(map[string]string{"prompt": "test", "trace_id": traceID})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://kahyad/v1/task", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kahya-Trace-Id", traceID)

	// Do() itself returns as soon as the SSE response HEADERS arrive (the
	// handler writes those before spawning anything) - it does not wait for
	// the body, so it will NOT error just because the context later
	// expires. The body must actually be read (blocking on the still-open,
	// forever-silent hang-worker stream) for the 300ms context deadline to
	// abort the connection and surface as a read error - this is exactly
	// what a real client (kahya's CLI) does when its own idle-read timeout
	// gives up.
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("Do(): unexpected error before the body was even read: %v", err)
	}
	if _, readErr := io.ReadAll(resp.Body); readErr == nil {
		resp.Body.Close()
		t.Fatal("expected reading the SSE body to fail once the client context deadline fired, got nil")
	}
	resp.Body.Close()

	// The server keeps running after the client gave up: poll (bounded)
	// until the tasks row leaves "running" - it must not get stuck there
	// just because the client that started it disappeared.
	deadline := time.Now().Add(5 * time.Second)
	var state string
	for {
		_ = f.store.DB().QueryRow(`SELECT state FROM tasks WHERE trace_id = ?`, traceID).Scan(&state)
		if state != "" && state != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tasks.state never left \"running\" after client disconnect (last=%q)", state)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if state != "error" {
		t.Errorf("tasks.state = %q, want error", state)
	}
	assertLedgerHasKind(t, f.store, traceID, "task_timeout")
}

// TestTaskEmptyPromptRejected covers the local validation path (no worker
// spawned at all).
func TestTaskEmptyPromptRejected(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)
	resp := postTask(t, f.client, "trace-empty", "   ")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func assertLedgerHasKind(t *testing.T, st *store.Store, traceID, kind string) {
	t.Helper()
	rows, err := st.DB().Query(`SELECT kind FROM events WHERE trace_id = ?`, traceID)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan kind: %v", err)
		}
		kinds = append(kinds, k)
		if k == kind {
			return
		}
	}
	t.Fatalf("no events row with kind=%q trace_id=%q; got kinds=%v", kind, traceID, kinds)
}

// ---- POST /policy/check ----

func TestPolicyCheckTableDriven(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)

	cases := []struct {
		name     string
		toolName string
		want     string
	}{
		{"memory_search allowed", "memory_search", "allow"},
		{"mcp-prefixed memory_search allowed", "mcp__kahya_memory__memory_search", "allow"},
		{"memory_write denied", "memory_write", "deny"},
		{"Read denied (ordering invariant)", "Read", "deny"},
		{"unknown tool denied fail-closed", "some_future_tool", "deny"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			traceID := "trace-policy-" + c.toolName
			body, _ := json.Marshal(map[string]any{
				"trace_id": traceID, "task_id": "t1", "tool_name": c.toolName, "tool_input": map[string]any{},
			})
			resp, err := f.client.Post("http://kahyad/policy/check", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST /policy/check: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var pr policyCheckResponse
			if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if pr.Decision != c.want {
				t.Errorf("decision = %q, want %q (reason=%q)", pr.Decision, c.want, pr.Reason)
			}
			if pr.Rule == "" {
				t.Error("rule is empty")
			}
			if c.want == "deny" && pr.Reason == "" {
				t.Error("deny decision has empty reason")
			}

			// Every decision writes a policy_decision ledger row carrying
			// the request's own trace_id.
			assertLedgerHasKind(t, f.store, traceID, "policy_decision")
		})
	}
}

// TestPolicyCheckDenyAllModeDeniesEvenMemorySearch is W3-01's deny-all
// acceptance criterion applied to /policy/check: SetDenyAll (simulating a
// policy.yaml load failure at boot) overrides the interim static table
// entirely - even memory_search, the one tool it itself allows - for
// every tool name, with policy.RuleDenyAllV1 as the returned rule.
func TestPolicyCheckDenyAllModeDeniesEvenMemorySearch(t *testing.T) {
	cfg := config.Config{
		DBPath:               filepath.Join(t.TempDir(), "brain.db"),
		MemoryDir:            t.TempDir(),
		Socket:               filepath.Join(shortSocketDir(t), "k.sock"),
		LogDir:               t.TempDir(),
		AnthropicUpstreamURL: "https://upstream.invalid",
		DefaultModel:         "claude-sonnet-5",
		TaskTimeoutMin:       5,
		WorkerCmd:            []string{"python3", "-c", "pass"},
	}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	log := testLogger(t)
	srv := New(cfg, log, "v-denyall-test", healthyDB)
	srv.SetEventLogger(st)
	srv.SetTaskStore(st.Queries)
	srv.SetAnthproxy(anthproxy.NewGovernor(anthproxy.Limits{
		DailyBudgetUSD:         1000,
		MonthlyBudgetUSD:       10000,
		TaskTokenCeiling:       500000,
		DowngradeAtRatio:       0.8,
		CacheHitAlarmThreshold: 0.5,
	}, nil, nil), nil, anthproxy.NewPassthroughCredentialSource(), nil)
	srv.SetDenyAll() // simulate a policy.yaml load failure at boot.
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })

	client := unixHTTPClient(cfg.Socket)

	for _, tool := range []string{"memory_search", "Read", "some_future_tool"} {
		t.Run(tool, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"trace_id": "trace-denyall-" + tool, "task_id": "t1", "tool_name": tool, "tool_input": map[string]any{},
			})
			resp, err := client.Post("http://kahyad/policy/check", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST /policy/check: %v", err)
			}
			defer resp.Body.Close()
			var pr policyCheckResponse
			if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if pr.Decision != "deny" {
				t.Errorf("decision = %q, want deny (deny-all mode must override even memory_search)", pr.Decision)
			}
			if pr.Rule != policy.RuleDenyAllV1 {
				t.Errorf("rule = %q, want %q", pr.Rule, policy.RuleDenyAllV1)
			}
			if pr.Reason != policy.ReasonDenyAll {
				t.Errorf("reason = %q, want %q", pr.Reason, policy.ReasonDenyAll)
			}
		})
	}
}

func TestPolicyCheckMalformedBodyDeniesWith400(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)

	resp, err := f.client.Post("http://kahyad/policy/check", "application/json", strings.NewReader("not-json"))
	if err != nil {
		t.Fatalf("POST /policy/check: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var pr policyCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pr.Decision != "deny" {
		t.Errorf("decision = %q, want deny", pr.Decision)
	}
}

// TestPolicyCheckOversizedBodyDeniesWithinBudget is BLOCKER 3's regression
// test: a 64MB /policy/check body must be rejected fail-closed (deny) well
// under the documented 5s caller-side budget - decoding 64MB alone would
// already blow past that budget without a body size cap in front of
// json.Decode - and the ledger must still record the deny under the
// trace_id from the request header (independent of the oversized body,
// which never gets far enough to be parsed).
func TestPolicyCheckOversizedBodyDeniesWithinBudget(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)

	traceID := "trace-oversized-00000000000000001"
	padding := strings.Repeat("A", 64<<20) // 64MB, comfortably over policyCheckMaxBody (1 MiB).
	body := `{"trace_id":"` + traceID + `","task_id":"t1","tool_name":"Read","tool_input":{"padding":"` + padding + `"}}`

	req, err := http.NewRequest(http.MethodPost, "http://kahyad/policy/check", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kahya-Trace-Id", traceID)

	start := time.Now()
	resp, err := f.client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("POST /policy/check: %v", err)
	}
	defer resp.Body.Close()

	if elapsed > time.Second {
		t.Errorf("policy/check with a 64MB body took %v, want well under 1s (well inside the 5s fail-closed budget)", elapsed)
	}
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 400 or 413", resp.StatusCode)
	}
	var pr policyCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pr.Decision != "deny" {
		t.Errorf("decision = %q, want deny", pr.Decision)
	}

	// The ledger still records the fail-closed deny, under the header's
	// trace_id, even though the oversized body itself was never parsed.
	assertLedgerHasKind(t, f.store, traceID, "policy_decision")
}

// TestTaskOversizedBodyRejected covers /v1/task's own body size cap
// (BLOCKER 3): an oversized body must be rejected before a worker is ever
// spawned, not left to exhaust memory decoding it.
func TestTaskOversizedBodyRejected(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)

	body := `{"prompt":"` + strings.Repeat("A", 9<<20) + `","trace_id":"trace-oversized-task"}`
	resp, err := f.client.Post("http://kahyad/v1/task", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/task: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 400 or 413", resp.StatusCode)
	}
}

func TestPolicyCheckAnswersWellUnder5Seconds(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)
	body, _ := json.Marshal(map[string]any{"trace_id": "trace-speed", "task_id": "t1", "tool_name": "memory_search"})

	start := time.Now()
	resp, err := f.client.Post("http://kahyad/policy/check", "application/json", bytes.NewReader(body))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("POST /policy/check: %v", err)
	}
	resp.Body.Close()
	if elapsed > time.Second {
		t.Errorf("policy/check took %v, want well under the 5s budget", elapsed)
	}
}
