// controlauth_test.go covers G03 / B-3's UDS caller-identity gate
// (controlauth.go): the per-boot control secret, the route
// classification, and that a worker-plane caller (same socket, same uid,
// no secret) can reach neither the control routes nor - in particular -
// /policy/promote or a W3 approval decision.
package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/ritual"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/taint"
	"kahya/kahyad/internal/task"
)

// doRaw sends one request with an explicit trace id and (optionally) an
// explicit X-Kahya-Control-Secret value, over a NO-secret client, and
// returns the status plus the decoded {"error": ...} body (if any).
func doRaw(t *testing.T, client *http.Client, method, path, traceID, secret, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://kahyad"+path, rdr)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("X-Kahya-Trace-Id", traceID)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if secret != "" {
		req.Header.Set(config.ControlSecretHeader, secret)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	return resp.StatusCode, e.Error
}

// controlRejectCount counts control_auth_rejected ledger rows for traceID
// with the given payload.reason.
func controlRejectCount(t *testing.T, f taskTestFixture, traceID, reason string) int {
	t.Helper()
	var n int
	if err := f.store.DB().QueryRow(
		`SELECT count(*) FROM events WHERE kind = 'control_auth_rejected' AND trace_id = ? AND json_extract(payload, '$.reason') = ?`,
		traceID, reason,
	).Scan(&n); err != nil {
		t.Fatalf("count control_auth_rejected: %v", err)
	}
	return n
}

func readSecretFile(t *testing.T, sock string) string {
	t.Helper()
	s, err := config.ReadControlSecret(sock)
	if err != nil || s == "" {
		t.Fatalf("ReadControlSecret(%s) = %q, %v; want a 64-hex secret", sock, s, err)
	}
	return s
}

// TestControlSecretMintedNextToSocket: Prepare writes <socket>.secret
// (0600, 64 lowercase hex) holding exactly the secret the server enforces;
// Shutdown removes it; the next boot on the same socket rotates it.
func TestControlSecretMintedNextToSocket(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "k.sock")
	secretPath := config.ControlSecretPath(sock)

	srv := New(testConfig(sock), testLogger(t), "v-ctrl", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck

	fi, err := os.Stat(secretPath)
	if err != nil {
		t.Fatalf("stat %s: %v", secretPath, err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("secret file mode = %o, want 600", mode)
	}
	raw, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("read secret: %v", err)
	}
	first := strings.TrimSpace(string(raw))
	if len(first) != 64 || strings.ToLower(first) != first {
		t.Fatalf("secret = %q, want 64 lowercase hex chars", first)
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("secret %q is not hex: %v", first, err)
	}
	if first != srv.controlSecret {
		t.Fatalf("file secret %q != enforced secret %q", first, srv.controlSecret)
	}
	if _, err := os.Stat(secretPath + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file %s.tmp left behind (err=%v)", secretPath, err)
	}

	// Wait until the Serve goroutine is really serving, so this test is the
	// "served, then shut down" order every time (the other order - Shutdown
	// before Serve ran - is TestLateServeNeverUnlinksASuccessorsSocket).
	waitServing(t, sock)
	if err := srv.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := os.Stat(secretPath); !os.IsNotExist(err) {
		t.Fatalf("secret file still present after Shutdown (err=%v)", err)
	}

	srv2 := New(testConfig(sock), testLogger(t), "v-ctrl", healthyDB)
	if err := srv2.Prepare(); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	go srv2.Serve() //nolint:errcheck
	t.Cleanup(func() { srv2.Shutdown() })
	second := readSecretFile(t, sock)
	if second == first {
		t.Fatalf("secret not rotated across boots: %q", second)
	}
}

// TestPrepareFailsClosedWhenControlSecretCannotBeMinted: if the secret
// cannot be written, Prepare returns an error and no listener is left
// serving (no secret => no server, never a secret-less one).
func TestPrepareFailsClosedWhenControlSecretCannotBeMinted(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "k.sock")
	// A non-empty directory where the secret file must go: the final
	// rename onto it fails.
	blocker := config.ControlSecretPath(sock)
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o700); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}

	srv := New(testConfig(sock), testLogger(t), "v-ctrl", healthyDB)
	if err := srv.Prepare(); err == nil {
		srv.Shutdown()
		t.Fatalf("Prepare succeeded with an unwritable control secret; want fail-closed error")
	}
	if probeHealth(sock) {
		t.Fatalf("a listener still answers /health after Prepare failed")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket file left behind after failed Prepare (err=%v)", err)
	}
	// The startup flock must have been released: a second Prepare (once
	// the blocker is gone) succeeds.
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatalf("remove blocker: %v", err)
	}
	srv2 := New(testConfig(sock), testLogger(t), "v-ctrl", healthyDB)
	if err := srv2.Prepare(); err != nil {
		t.Fatalf("Prepare after removing blocker: %v", err)
	}
	srv2.Shutdown()
}

// routePath turns a ServeMux pattern into a concrete request path for
// classification: drop a "METHOD " prefix and substitute every {wildcard}
// segment with "x".
func routePath(pattern string) string {
	p := pattern
	if i := strings.Index(p, " "); i >= 0 {
		p = p[i+1:]
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			segs[i] = "x"
		}
	}
	return strings.Join(segs, "/")
}

// TestRouteTableClassifiesEveryMuxRoute is the structural guard: every
// route Prepare mounts is classified, and EXACTLY /health is open and
// EXACTLY the four documented worker routes are worker - everything else
// (and every unknown/unclean path) is control. A route added to
// routeTable without a deliberate openPaths/workerPaths entry is
// therefore control-plane by construction.
func TestRouteTableClassifiesEveryMuxRoute(t *testing.T) {
	srv := New(testConfig(filepath.Join(shortSocketDir(t), "k.sock")), testLogger(t), "v-ctrl", healthyDB)
	wantNonControl := map[string]routeClass{
		"/health":               routeOpen,
		"/policy/check":         routeWorker,
		"/v1/memory/search":     routeWorker,
		"/v1/mcp":               routeWorker,
		"/policy/consume-token": routeWorker,
	}
	seen := map[string]bool{}
	for _, rt := range srv.routeTable() {
		path := routePath(rt.pattern)
		seen[path] = true
		got := classifyRoute(path)
		want, special := wantNonControl[path]
		if !special {
			want = routeControl
		}
		if got != want {
			t.Errorf("classifyRoute(%q) (pattern %q) = %s, want %s", path, rt.pattern, got, want)
		}
		if rt.handler == nil {
			t.Errorf("route %q has a nil handler", rt.pattern)
		}
	}
	for path := range wantNonControl {
		if !seen[path] {
			t.Errorf("non-control path %q is not mounted by routeTable (stale classification)", path)
		}
	}
	if len(openPaths)+len(workerPaths) != len(wantNonControl) {
		t.Errorf("openPaths+workerPaths = %d entries, want exactly %d (%v)", len(openPaths)+len(workerPaths), len(wantNonControl), wantNonControl)
	}
	for _, p := range []string{"/nope", "", "/", "//policy/check", "/policy/check/", "/x/../policy/check", "/v1/mcp/x", "/HEALTH"} {
		if got := classifyRoute(p); got != routeControl {
			t.Errorf("classifyRoute(%q) = %s, want control (fail-closed default)", p, got)
		}
	}
}

// controlProbe is one control route TestControlRoutesRequireSecret hits.
type controlProbe struct {
	method, path, body string
}

var controlProbes = []controlProbe{
	{http.MethodPost, "/policy/promote", `{"tool":"fs_write","class":"W1","scope":"global"}`},
	{http.MethodPost, "/policy/feedback", `{"kind":"deny","pending_approval_id":"x"}`},
	{http.MethodPost, "/policy/undo", `{"trace_id":"x"}`},
	{http.MethodPost, "/approvals/x/decision", `{"approve":false}`},
	{http.MethodGet, "/approvals/pending", ""},
	{http.MethodGet, "/policy/approvals", ""},
	{http.MethodGet, "/policy/state", ""},
	{http.MethodPost, "/halt", `{}`},
	{http.MethodPost, "/v1/task", `{`},
	{http.MethodPost, "/jobs/trigger/x", ""},
	{http.MethodGet, "/v1/task/status?id=x", ""},
	{http.MethodPost, "/v1/task/resolve", `{}`},
	{http.MethodPost, "/session/sensitive-read", `{}`},
	{http.MethodGet, "/v1/log?trace_id=x", ""},
	{http.MethodPost, "/v1/reindex", `{}`},
	{http.MethodGet, "/metrics", ""},
	{http.MethodGet, "/readiness", ""},
	{http.MethodPost, "/debug/emit-approval", `{}`},
	{http.MethodPost, "/v1/remembered", `{}`},
	{http.MethodPost, "/v1/memory/extract", `{}`},
	{http.MethodGet, "/v1/entity/list", ""},
	{http.MethodGet, "/v1/entity/show/1", ""},
	{http.MethodGet, "/v1/fact/query?predicate=works_at", ""},
	{http.MethodGet, "/v1/fact/list?quarantined=1", ""},
	{http.MethodPost, "/v1/fact/deny", `{"fact_id":1}`},
	{http.MethodPost, "/v1/fact/confirm", `{"fact_ids":[1]}`},
	{http.MethodPost, "/v1/memory/confirm", `{"file":"inbox/x.md"}`},
	{http.MethodPost, "/v1/ingest", `{"content_b64":"eA=="}`},
	{http.MethodGet, "/nope", ""},
}

// TestControlRoutesRequireSecret: every control route (and an unknown
// path) answers 403 + MsgControlSecretRejected to a caller with no or a
// wrong secret - ledgered as control_auth_rejected with reason
// missing_secret / bad_secret - and is served normally with the right one.
func TestControlRoutesRequireSecret(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)
	noSecret := unixHTTPClientNoSecret(f.srv.cfg.Socket)
	secret := readSecretFile(t, f.srv.cfg.Socket)

	for i, p := range controlProbes {
		missTrace := fmt.Sprintf("trace-ctrl-miss-%d", i)
		status, errBody := doRaw(t, noSecret, p.method, p.path, missTrace, "", p.body)
		if status != http.StatusForbidden || errBody != MsgControlSecretRejected {
			t.Errorf("%s %s without secret = %d %q, want 403 %q", p.method, p.path, status, errBody, MsgControlSecretRejected)
		}
		if n := controlRejectCount(t, f, missTrace, controlRejectMissingSecret); n != 1 {
			t.Errorf("%s %s without secret: control_auth_rejected(missing_secret) rows = %d, want 1", p.method, p.path, n)
		}

		badTrace := fmt.Sprintf("trace-ctrl-bad-%d", i)
		status, errBody = doRaw(t, noSecret, p.method, p.path, badTrace, "deadbeef", p.body)
		if status != http.StatusForbidden || errBody != MsgControlSecretRejected {
			t.Errorf("%s %s with a wrong secret = %d %q, want 403 %q", p.method, p.path, status, errBody, MsgControlSecretRejected)
		}
		if n := controlRejectCount(t, f, badTrace, controlRejectBadSecret); n != 1 {
			t.Errorf("%s %s with a wrong secret: control_auth_rejected(bad_secret) rows = %d, want 1", p.method, p.path, n)
		}

		okTrace := fmt.Sprintf("trace-ctrl-ok-%d", i)
		status, errBody = doRaw(t, noSecret, p.method, p.path, okTrace, secret, p.body)
		if errBody == MsgControlSecretRejected {
			t.Errorf("%s %s with the right secret was still rejected (status %d)", p.method, p.path, status)
		}
		if n := controlRejectCount(t, f, okTrace, controlRejectMissingSecret) + controlRejectCount(t, f, okTrace, controlRejectBadSecret); n != 0 {
			t.Errorf("%s %s with the right secret ledgered %d control_auth_rejected rows, want 0", p.method, p.path, n)
		}
	}

	// A different boot's secret (right shape, wrong value) is rejected too.
	other := strings.Repeat("ab", 32)
	if status, _ := doRaw(t, noSecret, http.MethodGet, "/policy/state", "trace-ctrl-other", other, ""); status != http.StatusForbidden {
		t.Errorf("GET /policy/state with another boot's secret = %d, want 403", status)
	}
}

// TestOpenRoutesDoNotRequireSecret: /health is open - no secret, no
// attribution (every script's readiness loop and probeHealth depend on
// it).
func TestOpenRoutesDoNotRequireSecret(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)
	noSecret := unixHTTPClientNoSecret(f.srv.cfg.Socket)

	if status, _ := doRaw(t, noSecret, http.MethodGet, "/health", "trace-open-health", "", ""); status != http.StatusOK {
		t.Fatalf("GET /health without secret = %d, want 200", status)
	}
	if status, _ := doRaw(t, noSecret, http.MethodGet, "/health", "trace-open-health-bad", "deadbeef", ""); status != http.StatusOK {
		t.Fatalf("GET /health with a wrong secret = %d, want 200 (open route ignores the header)", status)
	}
}

// TestWorkerRouteUnattributableRequiresSecret (G03 / B-14): a worker-route
// caller kahyad cannot attribute to a live worker it spawned (here: no
// live registry wired at all, so nobody is attributable) is served only
// with the control secret - without it, 403 + MsgControlSecretRejected
// and control_auth_rejected reason=unattributable_worker_route.
func TestWorkerRouteUnattributableRequiresSecret(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)
	noSecret := unixHTTPClientNoSecret(f.srv.cfg.Socket)
	secret := readSecretFile(t, f.srv.cfg.Socket)

	probes := []controlProbe{
		{http.MethodPost, "/policy/check", `{"trace_id":"x","task_id":"t1","tool_name":"memory_search","tool_input":{"query":"x"}}`},
		{http.MethodPost, "/v1/memory/search", `{"query":"x"}`},
		{http.MethodPost, "/policy/consume-token", `{}`},
		{http.MethodPost, "/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`},
	}
	for i, p := range probes {
		trace := fmt.Sprintf("trace-unattr-%d", i)
		status, errBody := doRaw(t, noSecret, p.method, p.path, trace, "", p.body)
		if status != http.StatusForbidden || errBody != MsgControlSecretRejected {
			t.Errorf("%s %s unattributed, no secret = %d %q, want 403 %q", p.method, p.path, status, errBody, MsgControlSecretRejected)
		}
		if n := controlRejectCount(t, f, trace, controlRejectUnattributable); n != 1 {
			t.Errorf("%s %s: control_auth_rejected(unattributable_worker_route) rows = %d, want 1", p.method, p.path, n)
		}
		okTrace := fmt.Sprintf("trace-unattr-ok-%d", i)
		if status, errBody := doRaw(t, noSecret, p.method, p.path, okTrace, secret, p.body); status == http.StatusForbidden || errBody == MsgControlSecretRejected {
			t.Errorf("%s %s unattributed WITH the secret = %d %q, want served", p.method, p.path, status, errBody)
		}
	}

	// /v1/mcp served end to end for the secret-carrying, unattributed
	// caller (a CLI/script).
	mf := newMCPTestFixture(t)
	mresp := postMCP(t, unixHTTPClient(mf.srv.cfg.Socket), `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	defer mresp.Body.Close()
	if mresp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(mresp.Body)
		t.Fatalf("POST /v1/mcp tools/list with secret = %d %s, want 200", mresp.StatusCode, b)
	}
	nresp := postMCP(t, unixHTTPClientNoSecret(mf.srv.cfg.Socket), `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	defer nresp.Body.Close()
	if nresp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /v1/mcp tools/list unattributed, no secret = %d, want 403", nresp.StatusCode)
	}
}

// seedTaintedAndCleanTasks wires a real taint tracker + server-side
// session resolver onto f's engine and seeds two running tasks: t-clean
// (session sess-clean, clean) and t-dirty (session sess-dirty, tainted) -
// the B-14 repro's exact setup.
func seedTaintedAndCleanTasks(t *testing.T, st *store.Store, eng *policy.Engine) {
	t.Helper()
	ctx := context.Background()
	tr := taint.New(st.Queries, st)
	eng.SetTaintChecker(tr)
	eng.SetSessionResolver(policy.NewStoreSessionResolver(st.Queries))
	for _, row := range [][3]string{{"t-clean", "trace-clean", "sess-clean"}, {"t-dirty", "trace-dirty", "sess-dirty"}} {
		if _, err := st.DB().ExecContext(ctx,
			`INSERT INTO tasks (id, trace_id, session_id, state, updated_at, created_at, lane) VALUES (?, ?, ?, 'running', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'normal')`,
			row[0], row[1], row[2]); err != nil {
			t.Fatalf("seed task %s: %v", row[0], err)
		}
	}
	if err := tr.InsertClean(ctx, "trace-clean", "sess-clean"); err != nil {
		t.Fatalf("InsertClean: %v", err)
	}
	if err := tr.Raise(ctx, "trace-dirty", "sess-dirty", "untrusted_output:web_read"); err != nil {
		t.Fatalf("Raise: %v", err)
	}
}

// identityOverrideCount counts worker_identity_override rows ledgered
// under traceID for path/source/claimed_task_id.
func identityOverrideCount(t *testing.T, st *store.Store, traceID, path, source, claimedTask string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(
		`SELECT count(*) FROM events WHERE kind = 'worker_identity_override' AND trace_id = ?
		   AND json_extract(payload, '$.path') = ? AND json_extract(payload, '$.source') = ?
		   AND json_extract(payload, '$.claimed_task_id') = ?`,
		traceID, path, source, claimedTask,
	).Scan(&n); err != nil {
		t.Fatalf("count worker_identity_override: %v", err)
	}
	return n
}

// TestWorkerIdentityPinnedToLiveWorker is G03 / B-14's core regression
// test: a TAINTED live worker (this test process, registered under
// t-dirty by its own session id - the key handleTask registers a spawned
// worker under) that names the CLEAN task's ids on /policy/check is
// still taint-denied, because kahyad pins the caller to its attributed
// task; the override is ledgered under the worker's own trace. The same
// pinning applies to /policy/consume-token (extra finding E-1). Before the
// fix the laundered call fell through to the ladder (needs_approval).
func TestWorkerIdentityPinnedToLiveWorker(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)
	reg := task.NewLiveRegistry()
	f.srv.SetTaskLiveRegistry(reg)
	seedTaintedAndCleanTasks(t, f.store, f.srv.policyEngine)
	reg.RegisterWorker("t-dirty", "trace-dirty", os.Getpid(), testProcessSID(t))
	worker := unixHTTPClientNoSecret(f.srv.cfg.Socket) // a worker never has the secret

	fsWrite := func(task, trace string) map[string]any {
		return map[string]any{
			"trace_id": trace, "task_id": task, "tool_name": "fs_write",
			"tool_input": map[string]any{"path": "x", "content_base64": "eA=="},
		}
	}

	t.Run("policy_check", func(t *testing.T) {
		var resp policyCheckResponse
		r := postJSON(t, worker, "/policy/check", fsWrite("t-clean", "trace-clean"), &resp)
		if r.StatusCode != http.StatusOK || resp.Decision != policy.ResultDeny || resp.Rule != policy.RuleTaintedSessionV1 {
			t.Fatalf("laundered fs_write = %d %+v, want deny rule=%s (pinned to the tainted worker's own task)", r.StatusCode, resp, policy.RuleTaintedSessionV1)
		}
		if n := identityOverrideCount(t, f.store, "trace-dirty", "/policy/check", "body", "t-clean"); n != 1 {
			t.Fatalf("worker_identity_override(body, claimed t-clean) under trace-dirty = %d, want 1", n)
		}
		var cleanDecisions int
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE kind = 'policy_decision' AND trace_id = 'trace-clean'`).Scan(&cleanDecisions); err != nil {
			t.Fatalf("count: %v", err)
		}
		if cleanDecisions != 0 {
			t.Fatalf("policy_decision rows ledgered under the CLEAN task's trace = %d, want 0 (forensic trail must name the real caller)", cleanDecisions)
		}

		// Its OWN ids are of course denied the same way, and naming them
		// is no override.
		r = postJSON(t, worker, "/policy/check", fsWrite("t-dirty", "trace-dirty"), &resp)
		if resp.Rule != policy.RuleTaintedSessionV1 {
			t.Fatalf("own-ids fs_write = %+v, want tainted_session deny", resp)
		}
		if n := identityOverrideCount(t, f.store, "trace-dirty", "/policy/check", "body", "t-dirty"); n != 0 {
			t.Fatalf("worker naming its own ids ledgered %d overrides, want 0", n)
		}
	})

	t.Run("consume_token", func(t *testing.T) {
		var resp policyConsumeTokenResponse
		r := postJSON(t, worker, "/policy/consume-token", map[string]any{
			"trace_id": "trace-clean", "task_id": "t-clean", "tool": "fs_write", "class": "W1", "scope": "global",
			"token": strings.Repeat("0", 64), "tool_input": map[string]any{"path": "x"},
		}, &resp)
		if r.StatusCode != http.StatusOK || resp.OK {
			t.Fatalf("consume-token naming t-clean = %d %+v, want 200 ok:false", r.StatusCode, resp)
		}
		if n := identityOverrideCount(t, f.store, "trace-dirty", "/policy/consume-token", "body", "t-clean"); n != 1 {
			t.Fatalf("worker_identity_override(consume-token, claimed t-clean) under trace-dirty = %d, want 1", n)
		}
	})

	t.Run("unattributed_secret_caller_keeps_declaration", func(t *testing.T) {
		// Once the worker is gone, the same process is just a CLI/script:
		// with the control secret its declared ids stand (no pinning), so
		// naming t-clean resolves the CLEAN session and reaches the ladder.
		reg.Unregister("t-dirty")
		var resp policyCheckResponse
		postJSON(t, f.client, "/policy/check", fsWrite("t-clean", "trace-clean"), &resp)
		if resp.Rule == policy.RuleTaintedSessionV1 || resp.Decision != "needs_approval" {
			t.Fatalf("secret-carrying unattributed fs_write(t-clean) = %+v, want needs_approval via the ladder", resp)
		}
	})
}

// TestWorkerAttributionIsPerRequest: attribution is resolved on every
// request, not once per connection - a kept-alive connection opened
// BEFORE the worker registered is attributed once it is, and stops being
// attributed (fail-closed: 403 without a secret) once it unregisters.
func TestWorkerAttributionIsPerRequest(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)
	reg := task.NewLiveRegistry()
	f.srv.SetTaskLiveRegistry(reg)
	worker := unixHTTPClientNoSecret(f.srv.cfg.Socket)
	body := `{"trace_id":"x","task_id":"t1","tool_name":"memory_search","tool_input":{"query":"x"}}`

	if status, _ := doRaw(t, worker, http.MethodPost, "/policy/check", "trace-pr-1", "", body); status != http.StatusForbidden {
		t.Fatalf("before registration = %d, want 403", status)
	}
	reg.RegisterWorker("t-live", "trace-live", os.Getpid(), testProcessSID(t))
	if status, _ := doRaw(t, worker, http.MethodPost, "/policy/check", "trace-pr-2", "", body); status != http.StatusOK {
		t.Fatalf("after registration (same keep-alive client) = %d, want 200", status)
	}
	if n := identityOverrideCount(t, f.store, "trace-live", "/policy/check", "header", ""); n != 1 {
		t.Fatalf("header override (declared trace trace-pr-2) under trace-live = %d, want 1", n)
	}
	reg.Unregister("t-live")
	if status, _ := doRaw(t, worker, http.MethodPost, "/policy/check", "trace-pr-3", "", body); status != http.StatusForbidden {
		t.Fatalf("after Unregister (same keep-alive client) = %d, want 403", status)
	}
}

// TestControlRouteRefusesLiveWorkerEvenWithSecret (G03 / B-14): a caller
// inside a live worker's session is refused on control routes even
// if it read <socket>.secret - reason live_worker_on_control_route.
func TestControlRouteRefusesLiveWorkerEvenWithSecret(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)
	reg := task.NewLiveRegistry()
	f.srv.SetTaskLiveRegistry(reg)
	reg.RegisterWorker("t-live", "trace-live", os.Getpid(), testProcessSID(t))
	secret := readSecretFile(t, f.srv.cfg.Socket)
	worker := unixHTTPClientNoSecret(f.srv.cfg.Socket)

	for i, p := range controlProbes {
		trace := fmt.Sprintf("trace-wctl-%d", i)
		status, errBody := doRaw(t, worker, p.method, p.path, trace, secret, p.body)
		if status != http.StatusForbidden || errBody != MsgControlSecretRejected {
			t.Errorf("%s %s from a live worker WITH the secret = %d %q, want 403", p.method, p.path, status, errBody)
		}
		if n := controlRejectCount(t, f, trace, controlRejectWorkerOnControl); n != 1 {
			t.Errorf("%s %s: control_auth_rejected(live_worker_on_control_route) rows = %d, want 1", p.method, p.path, n)
		}
	}
	reg.Unregister("t-live")
	if status, _ := doRaw(t, worker, http.MethodGet, "/policy/state", "trace-wctl-after", secret, ""); status != http.StatusOK {
		t.Fatalf("GET /policy/state with the secret after the worker exited = %d, want 200", status)
	}
}

// TestAttributedWorkerWithoutTraceFailsClosed: a live entry registered
// via the legacy pid-only Register (no trace_id) cannot be pinned
// completely, so its worker-route calls are refused rather than served
// half-pinned.
func TestAttributedWorkerWithoutTraceFailsClosed(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)
	reg := task.NewLiveRegistry()
	f.srv.SetTaskLiveRegistry(reg)
	reg.Register("t-legacy", testProcessSID(t))
	worker := unixHTTPClientNoSecret(f.srv.cfg.Socket)

	status, errBody := doRaw(t, worker, http.MethodPost, "/policy/check", "trace-legacy", "",
		`{"trace_id":"x","task_id":"t-legacy","tool_name":"memory_search","tool_input":{"query":"x"}}`)
	if status != http.StatusForbidden || errBody != MsgControlSecretRejected {
		t.Fatalf("legacy-registered worker /policy/check = %d %q, want 403", status, errBody)
	}
	if n := controlRejectCount(t, f, "trace-legacy", controlRejectNoTrace); n != 1 {
		t.Fatalf("control_auth_rejected(attributed_worker_without_trace) rows = %d, want 1", n)
	}
}

// TestPolicyPromoteRequiresControlSecret: the B-3 repro - a caller that
// only knows the socket (a worker) cannot raise the autonomy ladder; the
// CLI's secret-carrying call still can.
func TestPolicyPromoteRequiresControlSecret(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)
	noSecret := unixHTTPClientNoSecret(f.srv.cfg.Socket)
	body := map[string]any{"tool": "fs_write", "class": "W1", "scope": "global"}

	var denied struct {
		Level int    `json:"level"`
		Error string `json:"error"`
	}
	resp := postJSON(t, noSecret, "/policy/promote", body, &denied)
	if resp.StatusCode != http.StatusForbidden || denied.Error != MsgControlSecretRejected || denied.Level != 0 {
		t.Fatalf("promote without secret = %d %+v, want 403 %q", resp.StatusCode, denied, MsgControlSecretRejected)
	}

	var state policyStateResponse
	getJSON(t, f.client, "/policy/state", &state)
	if len(state.States) != 0 {
		t.Fatalf("autonomy_state after a rejected promote = %+v, want empty", state.States)
	}

	var ok policyPromoteResponse
	resp = postJSON(t, f.client, "/policy/promote", body, &ok)
	if resp.StatusCode != http.StatusOK || ok.Level != 1 {
		t.Fatalf("promote with secret = %d %+v, want 200 level 1", resp.StatusCode, ok)
	}
}

// TestMemorySearchHafizaInjectedPinnedToAttributedWorker (G03 / B-14):
// the <hafiza> injection ledger row (HANDOFF §5 safety #4 forensic trail)
// is written against the task that actually received the block - the
// attributed live worker's - never the task_id/trace_id the caller
// declared in the body.
func TestMemorySearchHafizaInjectedPinnedToAttributedWorker(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fs := &fakeSearcher{hits: []search.Hit{
		{ChunkID: 5, EpisodeID: 1, Path: "properties/kadikoy.md", Seq: 2, Text: "Kadıköy'de iki daire gezdik", Score: 0.9, SourceTier: "user_asserted"},
	}}
	led := &fakeEventLogger{}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(fs)
	srv.SetEventLogger(led)
	reg := task.NewLiveRegistry()
	srv.SetTaskLiveRegistry(reg)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve() //nolint:errcheck
	defer srv.Shutdown()
	reg.RegisterWorker("t-live", "trace-live", os.Getpid(), testProcessSID(t))

	resp := postMemorySearch(t, unixHTTPClientNoSecret(socketPath), `{"query":"kadikoy","for_injection":true,"task_id":"task-forged","trace_id":"tid-forged"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if fs.lastTID != "trace-live" {
		t.Errorf("search ran under trace %q, want the attributed trace-live", fs.lastTID)
	}
	events := led.eventsOfKind("hafiza_injected")
	if len(events) != 1 {
		t.Fatalf("hafiza_injected events = %d, want 1", len(events))
	}
	if events[0].traceID != "trace-live" || events[0].payload["task_id"] != "t-live" {
		t.Fatalf("hafiza_injected ledgered under trace %q task %v, want trace-live / t-live", events[0].traceID, events[0].payload["task_id"])
	}
	overrides := led.eventsOfKind("worker_identity_override")
	if len(overrides) != 1 || overrides[0].traceID != "trace-live" || overrides[0].payload["claimed_task_id"] != "task-forged" || overrides[0].payload["source"] != "body" {
		t.Fatalf("worker_identity_override events = %+v, want one body override claiming task-forged under trace-live", overrides)
	}
}

// TestWorkerRawMemorySearchRefused (FIX-FINAL worker-raw-memory-search-
// for-injection-false): /v1/memory/search is a worker route, but its
// for_injection flag came from the body, so a live worker posting
// for_injection:false got the unfiltered local results - secret-glob
// notes, content-secret chunks and quarantined episodes - with no mark,
// escalation or ledger row. An attributed live worker asking for the raw
// surface is now refused (403, ledgered control_auth_rejected) while its
// for_injection:true request still gets only the cloud-eligible hit.
func TestWorkerRawMemorySearchRefused(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fs := &fakeSearcher{hits: []search.Hit{
		{ChunkID: 1, EpisodeID: 1, Path: "finans-2026.md", Seq: 0, Text: "Maas hesabi bakiyesi", Score: 0.9, SourceTier: "user_asserted"},
		{ChunkID: 2, EpisodeID: 2, Path: "gizli/notes/saglik.md", Seq: 0, Text: "Tahlil sonucu: HbA1c 7.9", Score: 0.8, SourceTier: "agent_derived"},
		{ChunkID: 3, EpisodeID: 3, Path: "notlar/kira.md", Seq: 0, Text: contentSecretIBANText, Score: 0.7, SourceTier: "user_asserted"},
		{ChunkID: 4, EpisodeID: 4, Path: "properties/kadikoy.md", Seq: 2, Text: "Kadıköy'de iki daire gezdik", Score: 0.6, SourceTier: "user_asserted"},
	}}
	led := &fakeEventLogger{}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(fs)
	srv.SetEventLogger(led)
	srv.SetMemoryLane(&memlane.Classifier{MemoryDir: "/m", SecretGlobs: []string{"/m/gizli/**", "/m/finans*.md"}, Matcher: ritual.PolicyGlobMatcher{}})
	reg := task.NewLiveRegistry()
	srv.SetTaskLiveRegistry(reg)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve() //nolint:errcheck
	defer srv.Shutdown()
	reg.RegisterWorker("t-live", "trace-live", os.Getpid(), testProcessSID(t))

	for _, body := range []string{
		`{"query":"hesap","for_injection":false}`,
		`{"query":"hesap"}`,
	} {
		resp := postMemorySearch(t, unixHTTPClientNoSecret(socketPath), body)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403 (body %s)", body, resp.StatusCode, raw)
		}
		if strings.Contains(string(raw), "Maas") || strings.Contains(string(raw), "HbA1c") || strings.Contains(string(raw), "TR33") {
			t.Fatalf("%s: refusal body carries memory content: %s", body, raw)
		}
	}
	rejected := led.eventsOfKind("control_auth_rejected")
	if len(rejected) != 2 || rejected[0].payload["reason"] != "worker_raw_memory_search" || rejected[0].payload["task_id"] != "t-live" {
		t.Fatalf("control_auth_rejected = %+v, want two worker_raw_memory_search rows for t-live", rejected)
	}

	resp := postMemorySearch(t, unixHTTPClientNoSecret(socketPath), `{"query":"hesap","for_injection":true}`)
	var out struct {
		Results []struct {
			ChunkID int64 `json:"chunk_id"`
		} `json:"results"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(out.Results) != 1 || out.Results[0].ChunkID != 4 {
		t.Fatalf("for_injection:true = %d %+v, want 200 with only the clean chunk 4", resp.StatusCode, out.Results)
	}
}

// waitServing blocks until a daemon answers /health on sock.
func waitServing(t *testing.T, sock string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !probeHealth(sock) {
		if time.Now().After(deadline) {
			t.Fatalf("no daemon answered /health on %s within 5s", sock)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestLateServeNeverUnlinksASuccessorsSocket (FIX-FINAL flake-r2-control-
// secret-late-serve-unlinks-socket): the order that made
// TestControlSecretMintedNextToSocket flaky, forced. Shutdown runs before
// the first server's Serve ever does; a successor then binds the same
// path; only now does the first server's Serve run. http.Server.Serve
// closes the listener it is handed, and net.UnixListener used to unlink its
// path on Close - deleting the successor's live socket ("second Prepare:
// chmod socket ...: no such file or directory", or a successor left
// serving a socket nobody can dial).
func TestLateServeNeverUnlinksASuccessorsSocket(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "k.sock")

	srv := New(testConfig(sock), testLogger(t), "v-late", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	firstListener := srv.ln
	if err := srv.Shutdown(); err != nil {
		t.Fatalf("Shutdown before Serve: %v", err)
	}
	// Shutdown closed the listener Serve never registered: Accept on it
	// fails at once. (On a listener left open it would block, so it runs
	// beside a timer.)
	accepted := make(chan error, 1)
	go func() {
		c, err := firstListener.Accept()
		if err == nil {
			c.Close()
		}
		accepted <- err
	}()
	select {
	case err := <-accepted:
		if err == nil {
			t.Fatal("the first server's listener still accepts after Shutdown")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first server's listener is still open after Shutdown (Accept blocks)")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket still present after Shutdown (err=%v)", err)
	}

	srv2 := New(testConfig(sock), testLogger(t), "v-late", healthyDB)
	if err := srv2.Prepare(); err != nil {
		t.Fatalf("successor Prepare: %v", err)
	}
	go srv2.Serve() //nolint:errcheck
	t.Cleanup(func() { srv2.Shutdown() })
	waitServing(t, sock)
	successorSecret := readSecretFile(t, sock)

	// The first server's Serve goroutine finally runs.
	if err := srv.Serve(); err != nil {
		t.Fatalf("late Serve = %v, want nil (server already shut down)", err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("the late Serve unlinked the successor's socket: %v", err)
	}
	if !probeHealth(sock) {
		t.Fatal("the successor no longer answers /health after the first server's late Serve")
	}

	// A repeated Shutdown of the first server (t.Cleanup in other tests)
	// no longer holds the flock: it must not unlink the successor's socket
	// or secret either.
	if err := srv.Shutdown(); err != nil {
		t.Fatalf("repeated Shutdown: %v", err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("a repeated Shutdown unlinked the successor's socket: %v", err)
	}
	if got := readSecretFile(t, sock); got != successorSecret {
		t.Fatalf("a repeated Shutdown changed the successor's secret: %q != %q", got, successorSecret)
	}
	if !probeHealth(sock) {
		t.Fatal("the successor no longer answers /health after the first server's repeated Shutdown")
	}
}
