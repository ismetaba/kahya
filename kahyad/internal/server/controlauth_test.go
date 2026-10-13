// controlauth_test.go covers G03 / B-3's UDS caller-identity gate
// (controlauth.go): the per-boot control secret, the route
// classification, and that a worker-plane caller (same socket, same uid,
// no secret) can reach neither the control routes nor - in particular -
// /policy/promote or a W3 approval decision.
package server

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
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

// TestOpenRoutesDoNotRequireSecret: /health is open, and (B-3) the four
// worker routes are reachable without the control secret - a worker never
// has it.
func TestOpenRoutesDoNotRequireSecret(t *testing.T) {
	f := newTaskTestFixture(t, []string{"python3", "-c", "pass"}, 5)
	noSecret := unixHTTPClientNoSecret(f.srv.cfg.Socket)

	if status, _ := doRaw(t, noSecret, http.MethodGet, "/health", "trace-open-health", "", ""); status != http.StatusOK {
		t.Fatalf("GET /health without secret = %d, want 200", status)
	}

	var check policyCheckResponse
	resp := postJSON(t, noSecret, "/policy/check", map[string]any{
		"trace_id": "trace-open-check", "task_id": "t1", "tool_name": "memory_search",
		"tool_input": map[string]any{"query": "x"},
	}, &check)
	if resp.StatusCode != http.StatusOK || check.Decision == "" {
		t.Fatalf("POST /policy/check without secret = %d %+v, want 200 with a decision", resp.StatusCode, check)
	}

	// newTaskTestFixture wires no Searcher, so this answers 503 - the point
	// is only that the gate did not answer 403.
	if status, errBody := doRaw(t, noSecret, http.MethodPost, "/v1/memory/search", "trace-open-search", "", `{"query":"x"}`); status == http.StatusForbidden || errBody == MsgControlSecretRejected {
		t.Fatalf("POST /v1/memory/search without secret = %d %q, want not 403", status, errBody)
	}

	var consume policyConsumeTokenResponse
	resp = postJSON(t, noSecret, "/policy/consume-token", map[string]any{}, &consume)
	if resp.StatusCode != http.StatusOK || consume.OK {
		t.Fatalf("POST /policy/consume-token without secret = %d %+v, want 200 ok:false", resp.StatusCode, consume)
	}

	mf := newMCPTestFixture(t)
	mresp := postMCP(t, unixHTTPClientNoSecret(mf.srv.cfg.Socket), `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	defer mresp.Body.Close()
	if mresp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(mresp.Body)
		t.Fatalf("POST /v1/mcp tools/list without secret = %d %s, want 200", mresp.StatusCode, b)
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
