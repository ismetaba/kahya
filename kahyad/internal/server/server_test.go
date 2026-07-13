package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/store"
)

// testPolicyDoc is a small, hand-built policy.Policy covering every tool
// name this package's tests exercise - memory_search/memory_write/
// memory_forget (R/W1/W1) plus fs_write (W1) and mail_send (W3) for
// engine-wiring coverage. Built directly (not via policy.Load) so tests
// don't depend on the repo-root policy.yaml's exact contents.
func testPolicyDoc() policy.Policy {
	tools := []policy.ToolRule{
		{Name: "memory_search", Class: policy.ClassR, ScopeKey: "global"},
		{Name: "memory_write", Class: policy.ClassW1, ScopeKey: "global"},
		{Name: "memory_forget", Class: policy.ClassW1, ScopeKey: "global"},
		{Name: "fs_write", Class: policy.ClassW1, ScopeKey: "global"},
		{Name: "mail_send", Class: policy.ClassW3, ScopeKey: "global"},
	}
	byName := make(map[string]policy.ToolRule, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	return policy.Policy{Tools: tools, ToolsByName: byName}
}

// seedAutonomyState directly inserts an autonomy_state row (bypassing the
// engine's own Promote path) so a test can exercise an ALREADY-earned
// ladder level without going through 20 real approvals + a promote call.
func seedAutonomyState(t *testing.T, st *store.Store, tool, class, scope string, level int) {
	t.Helper()
	_, err := st.DB().Exec(
		`INSERT INTO autonomy_state (tool, class, scope, level, consecutive_approvals, updated_at) VALUES (?, ?, ?, ?, 0, ?)`,
		tool, class, scope, level, "2026-01-01T00:00:00Z",
	)
	if err != nil {
		t.Fatalf("seed autonomy_state: %v", err)
	}
}

// shortSocketDir returns a short-path temp dir suitable for unix sockets.
// macOS unix socket paths are capped around ~104 bytes; t.TempDir() nests
// deep enough (e.g. /private/var/folders/.../TestName/001/002) to blow
// past that, so tests bind sockets under a short os.MkdirTemp dir instead
// and clean it up themselves.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "k")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func testLogger(t *testing.T) *logx.Logger {
	t.Helper()
	l, err := logx.New(t.TempDir(), "test0000000000000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func testConfig(socketPath string) config.Config {
	return config.Config{Socket: socketPath}
}

// fakeDB is a minimal DBHealth stand-in so server tests don't need a real
// brain.db (and don't pull the sqlite/cgo driver into this package's tests).
type fakeDB struct {
	ok      bool
	version int64
	err     error
}

func (f fakeDB) Health(context.Context) (bool, int64, error) { return f.ok, f.version, f.err }

var healthyDB = fakeDB{ok: true, version: 1}

// fakeSearcher is a minimal Searcher stand-in so server tests can exercise
// /v1/memory/search without a real brain.db.
type fakeSearcher struct {
	hits    []search.Hit
	err     error
	lastQ   string
	lastK   int
	lastTID string
}

func (f *fakeSearcher) Search(_ context.Context, traceID, q string, k int) ([]search.Hit, error) {
	f.lastTID, f.lastQ, f.lastK = traceID, q, k
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

// loggingFakeSearcher is like fakeSearcher but also emits an
// event=memory_search JSONL line via log, scoped to the traceID it was
// called with - mirroring the real search.Searcher.Search's own logging
// pattern - without needing a real brain.db. It lets
// TestMemorySearchCorrelatesTraceIDWithHTTPRequestLine (MINOR 6) assert
// that the memory_search and http_request lines share one trace_id.
type loggingFakeSearcher struct {
	log     *logx.Logger
	lastTID string
}

func (f *loggingFakeSearcher) Search(_ context.Context, traceID, q string, _ int) ([]search.Hit, error) {
	f.lastTID = traceID
	f.log.With(traceID).Info("memory_search", "query_len", len(q))
	return nil, nil
}

// unixHTTPClient returns an http.Client that dials socketPath for every
// request, matching how the real kahya CLI talks to kahyad.
func unixHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

func TestHealthEndpoint(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test-123", healthyDB)

	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()

	client := unixHTTPClient(socketPath)
	resp, err := client.Get("http://kahyad/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v, want ok", body["status"])
	}
	if body["version"] != "v-test-123" {
		t.Errorf("version field = %v, want v-test-123", body["version"])
	}
	if _, ok := body["pid"]; !ok {
		t.Error("pid field missing")
	}
	if _, ok := body["uptime_s"]; !ok {
		t.Error("uptime_s field missing")
	}
	if body["db"] != "ok" {
		t.Errorf("db field = %v, want ok", body["db"])
	}
	if body["schema_version"] != float64(1) {
		t.Errorf("schema_version field = %v, want 1", body["schema_version"])
	}
	if body["embed"] != "disabled" {
		t.Errorf("embed field = %v, want disabled (no EmbedHealth wired)", body["embed"])
	}

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket perm = %o, want 600", perm)
	}

	if err := srv.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after Shutdown: err=%v", err)
	}
}

// TestHealthEndpointReportsDBError guards the fail-closed reporting rule in
// handleHealth: a failing DB ping must surface as "db":"error", never as
// "ok" (HANDOFF §4/§5 fail-closed posture applied to health reporting).
func TestHealthEndpointReportsDBError(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	unhealthyDB := fakeDB{ok: false, version: 1, err: errors.New("ping failed")}
	srv := New(testConfig(socketPath), testLogger(t), "v-test-db-down", unhealthyDB)

	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp, err := unixHTTPClient(socketPath).Get("http://kahyad/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["db"] != "error" {
		t.Errorf("db field = %v, want error", body["db"])
	}
}

func TestStaleSocketTakeover(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")

	// Simulate a daemon that crashed without cleaning up: bind a real unix
	// listener, then close it WITHOUT unlinking, leaving an orphaned socket
	// file that nothing is listening on.
	staleLn, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("create stale listener: %v", err)
	}
	if ul, ok := staleLn.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if err := staleLn.Close(); err != nil {
		t.Fatalf("close stale listener: %v", err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("expected stale socket file to remain on disk: %v", err)
	}

	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() over stale socket should succeed, got: %v", err)
	}
	go srv.Serve()

	client := unixHTTPClient(socketPath)
	resp, err := client.Get("http://kahyad/health")
	if err != nil {
		t.Fatalf("GET /health after takeover: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if err := srv.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestSecondInstanceRefused(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")

	first := New(testConfig(socketPath), testLogger(t), "v-first", healthyDB)
	if err := first.Prepare(); err != nil {
		t.Fatalf("first Prepare() error = %v", err)
	}
	go first.Serve()
	defer first.Shutdown()

	// Give the first instance's listener a moment to be dial-able (bind
	// already happened synchronously in Prepare, but be defensive).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := unixHTTPClient(socketPath).Get("http://kahyad/health"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first instance never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}

	second := New(testConfig(socketPath), testLogger(t), "v-second", healthyDB)
	err := second.Prepare()
	if err == nil {
		t.Fatal("second Prepare() error = nil, want ErrAlreadyRunning")
	}
	if err != ErrAlreadyRunning {
		t.Fatalf("second Prepare() error = %v, want ErrAlreadyRunning", err)
	}
}

func TestRunGracefulShutdownOnContextCancel(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-run", healthyDB)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := unixHTTPClient(socketPath).Get("http://kahyad/health"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() returned error after cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after Run() shutdown: err=%v", err)
	}
}

// TestTakeoverRaceSingleWinner reproduces the TOCTOU hazard the startup
// flock exists to prevent: N concurrent startups against one stale socket
// file must yield exactly one bound listener; every loser must see
// ErrAlreadyRunning, and the winner's listener must be reachable at the
// canonical path (not orphaned by a later remove+rebind).
func TestTakeoverRaceSingleWinner(t *testing.T) {
	dir := shortSocketDir(t)
	sock := filepath.Join(dir, "s.sock")

	// Seed a stale socket file (no listener behind it).
	staleLn, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	// Close without unlinking: simulates a SIGKILLed daemon's leftover.
	staleLn.(*net.UnixListener).SetUnlinkOnClose(false)
	staleLn.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket file missing after seed: %v", err)
	}

	const n = 8
	type result struct {
		ln   net.Listener
		lock *os.File
		err  error
	}
	results := make(chan result, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			ln, lock, err := prepareListener(sock, nil)
			results <- result{ln, lock, err}
		}()
	}
	close(start)

	var winners []result
	for i := 0; i < n; i++ {
		r := <-results
		if r.err == nil {
			winners = append(winners, r)
		} else if r.err != ErrAlreadyRunning {
			t.Errorf("loser returned unexpected error: %v", r.err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("want exactly 1 winner, got %d", len(winners))
	}
	w := winners[0]
	defer w.ln.Close()
	defer w.lock.Close()

	// The winner's listener must be the one reachable at the path: accept a
	// connection dialed against the canonical socket path.
	done := make(chan error, 1)
	go func() {
		conn, err := w.ln.Accept()
		if err == nil {
			conn.Close()
		}
		done <- err
	}()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial winner: %v", err)
	}
	conn.Close()
	if err := <-done; err != nil {
		t.Fatalf("winner accept: %v", err)
	}
}

// TestSocketDirPermsTightenedWhenPreexisting guards against the MkdirAll
// no-op: a pre-existing 0755 socket dir must be chmod'd to 0700 at startup.
func TestSocketDirPermsTightenedWhenPreexisting(t *testing.T) {
	dir := shortSocketDir(t)
	sockDir := filepath.Join(dir, "d")
	if err := os.Mkdir(sockDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sock := filepath.Join(sockDir, "s.sock")
	ln, lock, err := prepareListener(sock, nil)
	if err != nil {
		t.Fatalf("prepareListener: %v", err)
	}
	defer ln.Close()
	defer lock.Close()

	fi, err := os.Stat(sockDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("socket dir perms = %o, want 0700", got)
	}
}

// postMemorySearch is a small helper for hitting POST /v1/memory/search
// over the unix socket client with a raw JSON body.
func postMemorySearch(t *testing.T, client *http.Client, body string) *http.Response {
	t.Helper()
	resp, err := client.Post("http://kahyad/v1/memory/search", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST /v1/memory/search: %v", err)
	}
	return resp
}

// TestMemorySearchDenyAllReturns403 guards the W3-01 deny-all fail-closed
// fix: when policy.yaml failed to load, /v1/memory/search must NOT serve
// memory content (it feeds the <hafiza> block reaching the cloud model),
// even though a Searcher is wired.
func TestMemorySearchDenyAllReturns403(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fs := &fakeSearcher{hits: []search.Hit{
		{ChunkID: 7, EpisodeID: 3, Path: "note-a.md", Text: "gizli", Score: 0.9, SourceTier: "user_asserted"},
	}}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(fs)
	srv.SetDenyAll() // simulate a policy.yaml load failure at boot.
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	client := unixHTTPClient(socketPath)
	resp := postMemorySearch(t, client, `{"query":"gizli","k":3,"for_injection":true,"task_id":"t1"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (deny-all must gate memory search)", resp.StatusCode)
	}
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	if strings.Contains(buf.String(), "gizli") {
		t.Fatalf("deny-all response leaked memory content: %s", buf.String())
	}
}

// TestMemorySearchEndpointReturnsResults guards W12-03 step 4's happy path:
// a valid request reaches the wired Searcher and its hits round-trip as
// JSON with every documented field.
func TestMemorySearchEndpointReturnsResults(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fs := &fakeSearcher{hits: []search.Hit{
		{ChunkID: 7, EpisodeID: 3, Path: "note-a.md", Text: "ev bakiyoruz", Score: 0.42, SourceTier: "user_asserted"},
	}}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(fs)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	client := unixHTTPClient(socketPath)
	resp := postMemorySearch(t, client, `{"query":"evlerimizden","k":3,"trace_id":"tid-1"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Results []struct {
			ChunkID    int64   `json:"chunk_id"`
			EpisodeID  int64   `json:"episode_id"`
			Path       string  `json:"path"`
			Text       string  `json:"text"`
			Score      float64 `json:"score"`
			SourceTier string  `json:"source_tier"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(body.Results))
	}
	got := body.Results[0]
	if got.ChunkID != 7 || got.EpisodeID != 3 || got.Path != "note-a.md" || got.Text != "ev bakiyoruz" || got.SourceTier != "user_asserted" {
		t.Errorf("result = %+v, want chunk_id=7 episode_id=3 path=note-a.md text=%q source_tier=user_asserted", got, "ev bakiyoruz")
	}
	if got.Score != 0.42 {
		t.Errorf("result.score = %v, want 0.42", got.Score)
	}

	if fs.lastQ != "evlerimizden" || fs.lastK != 3 || fs.lastTID != "tid-1" {
		t.Errorf("Searcher.Search called with (traceID=%q, q=%q, k=%d), want (tid-1, evlerimizden, 3)", fs.lastTID, fs.lastQ, fs.lastK)
	}
}

// TestMemorySearchEndpointEmptyQueryIs400 guards step 4: an empty query
// must be a clean 400, never a panic (and never reach the Searcher at all).
func TestMemorySearchEndpointEmptyQueryIs400(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fs := &fakeSearcher{}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(fs)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	client := unixHTTPClient(socketPath)
	for _, body := range []string{`{"query":""}`, `{"query":"   "}`, `{}`} {
		resp := postMemorySearch(t, client, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, resp.StatusCode)
		}
		var errBody map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
			t.Errorf("body %s: decode error response: %v", body, err)
		}
		resp.Body.Close()
		if _, ok := errBody["error"]; !ok {
			t.Errorf("body %s: error response missing \"error\" field: %v", body, errBody)
		}
	}
	if fs.lastQ != "" {
		t.Errorf("Searcher.Search should never have been called for an empty query, got q=%q", fs.lastQ)
	}
}

// TestMemorySearchEndpointMalformedJSONIs400 guards against a panic on a
// non-JSON body.
func TestMemorySearchEndpointMalformedJSONIs400(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(&fakeSearcher{})
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postMemorySearch(t, unixHTTPClient(socketPath), `not json`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestMemorySearchEndpointNoSearcherIs503 guards the pre-wiring state: if
// SetSearcher is never called, the route must fail closed with 503, not
// panic on a nil s.search.
func TestMemorySearchEndpointNoSearcherIs503(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postMemorySearch(t, unixHTTPClient(socketPath), `{"query":"hello"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestMemorySearchCorrelatesTraceIDWithHTTPRequestLine is MINOR 6's
// regression test: when the POST body omits trace_id, handleMemorySearch
// must fall back to withTraceLogging's own resolved trace id (the request's
// context, set from X-Kahya-Trace-Id or freshly minted) rather than letting
// the Searcher mint an unrelated one - otherwise the event=memory_search
// JSONL line can never be correlated with this request's event=http_request
// line.
func TestMemorySearchCorrelatesTraceIDWithHTTPRequestLine(t *testing.T) {
	logDir := t.TempDir()
	log, err := logx.New(logDir, "boot0000000000000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	defer log.Close()

	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fs := &loggingFakeSearcher{log: log}
	srv := New(testConfig(socketPath), log, "v-test", healthyDB)
	srv.SetSearcher(fs)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	// No trace_id in the body: this is the case the middleware's own
	// resolved id must cover.
	resp := postMemorySearch(t, unixHTTPClient(socketPath), `{"query":"hello"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	data, err := os.ReadFile(filepath.Join(logDir, "kahyad.jsonl"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}

	var httpTID, memTID string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		switch rec["event"] {
		case "http_request":
			httpTID, _ = rec["trace_id"].(string)
		case "memory_search":
			memTID, _ = rec["trace_id"].(string)
		}
	}
	if httpTID == "" {
		t.Fatal("no event=http_request log line found")
	}
	if memTID == "" {
		t.Fatal("no event=memory_search log line found")
	}
	if httpTID != memTID {
		t.Errorf("http_request trace_id = %q, memory_search trace_id = %q, want equal (MINOR 6)", httpTID, memTID)
	}
	if fs.lastTID != httpTID {
		t.Errorf("Searcher.Search was called with traceID = %q, want the middleware's context trace id %q", fs.lastTID, httpTID)
	}
}

// TestMemorySearchEndpointSearcherErrorIs400 guards a Searcher error (e.g.
// search.ErrEmptyQuery bubbling up, or any other internal failure)
// surfacing as a clean 400 with an error body, never a 500 stack dump.
func TestMemorySearchEndpointSearcherErrorIs400(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(&fakeSearcher{err: search.ErrEmptyQuery})
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postMemorySearch(t, unixHTTPClient(socketPath), `{"query":"hello"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// fakeEmbedHealth is a minimal EmbedHealth stand-in (W12-11 step 2).
type fakeEmbedHealth struct{ state string }

func (f fakeEmbedHealth) State() string { return f.state }

// TestHealthEndpointReportsEmbedState guards SetEmbedHealth's wiring
// (W12-11 step 2): /health's "embed" field must reflect whatever the
// wired EmbedHealth currently reports, verbatim.
func TestHealthEndpointReportsEmbedState(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test-embed", healthyDB)
	srv.SetEmbedHealth(fakeEmbedHealth{state: "starting"})

	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp, err := unixHTTPClient(socketPath).Get("http://kahyad/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["embed"] != "starting" {
		t.Errorf("embed field = %v, want starting", body["embed"])
	}
}

// fakeReindexer is a minimal Reindexer stand-in (W12-04 step 5) so server
// tests can exercise POST /v1/reindex without a real brain.db or
// memory-dir corpus.
type fakeReindexer struct {
	res         indexer.Result
	err         error
	lastFull    bool
	lastReEmbed bool
	lastTID     string
}

func (f *fakeReindexer) Reindex(_ context.Context, traceID string, full, reEmbed bool) (indexer.Result, error) {
	f.lastTID, f.lastFull, f.lastReEmbed = traceID, full, reEmbed
	if f.err != nil {
		return indexer.Result{}, f.err
	}
	return f.res, nil
}

func postReindex(t *testing.T, client *http.Client, body string) *http.Response {
	t.Helper()
	resp, err := client.Post("http://kahyad/v1/reindex", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST /v1/reindex: %v", err)
	}
	return resp
}

// TestReindexEndpointReturnsSummary guards W12-04 step 5's happy path: a
// valid request reaches the wired Reindexer and its Result round-trips as
// the fixed six-key JSON schema (files_errored added in W78-05 so the restore
// drill's no-op assertion can see a skipped file).
func TestReindexEndpointReturnsSummary(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fr := &fakeReindexer{res: indexer.Result{
		FilesIndexed: 3, FilesUnchanged: 11, FilesRemoved: 1, FilesErrored: 2, Chunks: 9, DurationMs: 42,
	}}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetReindexer(fr)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postReindex(t, unixHTTPClient(socketPath), `{"full":true,"trace_id":"tid-reindex-1"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	want := map[string]float64{
		"files_indexed":   3,
		"files_unchanged": 11,
		"files_removed":   1,
		"files_errored":   2,
		"chunks":          9,
		"duration_ms":     42,
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%q] = %v, want %v", k, body[k], v)
		}
	}
	if len(body) != 6 {
		t.Errorf("body has %d keys (%v), want exactly 6", len(body), body)
	}

	if !fr.lastFull || fr.lastTID != "tid-reindex-1" {
		t.Errorf("Reindexer.Reindex called with (traceID=%q, full=%v), want (tid-reindex-1, true)", fr.lastTID, fr.lastFull)
	}
	if fr.lastReEmbed {
		t.Errorf("Reindexer.Reindex called with reEmbed=true, want false (request body omitted re_embed)")
	}
}

// TestReindexEndpointReEmbedFlagReachesReindexer guards the W12-11 step 5
// wiring: {"re_embed":true} in the request body must reach Reindexer.
// Reindex's reEmbed parameter.
func TestReindexEndpointReEmbedFlagReachesReindexer(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fr := &fakeReindexer{}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetReindexer(fr)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postReindex(t, unixHTTPClient(socketPath), `{"full":false,"re_embed":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !fr.lastReEmbed {
		t.Error("Reindexer.Reindex called with reEmbed=false, want true")
	}
	if fr.lastFull {
		t.Error("Reindexer.Reindex called with full=true, want false (re_embed is independent of full)")
	}
}

// TestReindexEndpointEmptyBodyDefaultsFullFalse guards the documented
// default: an empty POST body (no JSON at all) must behave exactly like
// {"full": false}, never error.
func TestReindexEndpointEmptyBodyDefaultsFullFalse(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fr := &fakeReindexer{}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetReindexer(fr)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	for _, body := range []string{``, `{}`} {
		resp := postReindex(t, unixHTTPClient(socketPath), body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("body=%q: status = %d, want 200", body, resp.StatusCode)
		}
		resp.Body.Close()
		if fr.lastFull {
			t.Errorf("body=%q: Reindex called with full=true, want false", body)
		}
	}
}

// TestReindexEndpointMalformedJSONIs400 guards against a panic on a
// non-JSON body.
func TestReindexEndpointMalformedJSONIs400(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetReindexer(&fakeReindexer{})
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postReindex(t, unixHTTPClient(socketPath), `not json`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestReindexEndpointNoReindexerIs503 guards the pre-wiring state: if
// SetReindexer is never called, the route must fail closed with 503, not
// panic on a nil s.reindex.
func TestReindexEndpointNoReindexerIs503(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postReindex(t, unixHTTPClient(socketPath), `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestReindexEndpointConflictIs409WithTurkishBody guards the task spec's
// exact concurrent-call contract: a Reindexer already running (surfaced as
// indexer.ErrReindexInProgress) answers 409 with the byte-exact Turkish
// error body the CLI (W12-06) surfaces verbatim.
func TestReindexEndpointConflictIs409WithTurkishBody(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetReindexer(&fakeReindexer{err: indexer.ErrReindexInProgress})
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postReindex(t, unixHTTPClient(socketPath), `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "reindex zaten çalışıyor" {
		t.Errorf(`body["error"] = %v, want "reindex zaten çalışıyor"`, body["error"])
	}
}

// TestReindexEndpointGenericErrorIs500 guards a non-conflict Reindexer
// error (e.g. a real filesystem walk failure) surfacing as a 500, not a
// misleading 409.
func TestReindexEndpointGenericErrorIs500(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetReindexer(&fakeReindexer{err: errors.New("boom")})
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postReindex(t, unixHTTPClient(socketPath), `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// writeJSONL writes lines (each already a complete JSON object) to path,
// one per line, for /v1/log test fixtures.
func writeJSONL(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestLogEndpointFiltersOrdersAndTagsProc guards GET /v1/log's kahyad-side
// contract (W12-06 deliverable): only lines whose trace_id matches the
// query parameter are returned, ordered by ts ascending across every
// *.jsonl file in log_dir, each tagged with "proc" (source file's basename
// minus ".jsonl"). The worker.jsonl fixture line also carries the task
// spec's byte-exact Turkish payload ("Kadıköy randevusu") to prove it
// survives the read-decode-reencode round trip untouched.
func TestLogEndpointFiltersOrdersAndTagsProc(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	logDir := t.TempDir()

	writeJSONL(t, filepath.Join(logDir, "kahyad.jsonl"), []string{
		`{"ts":"2026-07-10T09:00:02.000Z","level":"INFO","event":"second","trace_id":"target"}`,
		`{"ts":"2026-07-10T09:00:01.000Z","level":"INFO","event":"first","trace_id":"target"}`,
		`{"ts":"2026-07-10T09:00:03.000Z","level":"INFO","event":"other-trace","trace_id":"not-target"}`,
	})
	writeJSONL(t, filepath.Join(logDir, "worker.jsonl"), []string{
		`{"ts":"2026-07-10T09:00:01.500Z","level":"INFO","event":"worker-line","trace_id":"target","text":"Kadıköy randevusu"}`,
	})

	cfg := testConfig(socketPath)
	cfg.LogDir = logDir
	srv := New(cfg, testLogger(t), "v-test", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp, err := unixHTTPClient(socketPath).Get("http://kahyad/v1/log?trace_id=target")
	if err != nil {
		t.Fatalf("GET /v1/log: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Lines []map[string]any `json:"lines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Lines) != 3 {
		t.Fatalf("got %d lines, want 3 (only trace_id=target lines): %+v", len(body.Lines), body.Lines)
	}
	if body.Lines[0]["event"] != "first" || body.Lines[1]["event"] != "worker-line" || body.Lines[2]["event"] != "second" {
		t.Fatalf("lines not ordered by ts ascending: %+v", body.Lines)
	}
	if body.Lines[0]["proc"] != "kahyad" {
		t.Errorf(`lines[0]["proc"] = %v, want "kahyad"`, body.Lines[0]["proc"])
	}
	if body.Lines[1]["proc"] != "worker" {
		t.Errorf(`lines[1]["proc"] = %v, want "worker"`, body.Lines[1]["proc"])
	}
	if body.Lines[1]["text"] != "Kadıköy randevusu" {
		t.Errorf(`lines[1]["text"] = %v, want byte-exact "Kadıköy randevusu"`, body.Lines[1]["text"])
	}
}

// TestLogEndpointSkipsOversizedMalformedLineAndKeepsLaterMatches guards
// BLOCKER 3: readLogLines used to scan each file with a bufio.Scanner capped
// at 1MB, so one oversized (or merely malformed) line made the scan look
// like EOF and silently dropped every later line in that file - including
// real matches. A 2MB non-JSON line sandwiched between two real matches
// must not hide the second match.
func TestLogEndpointSkipsOversizedMalformedLineAndKeepsLaterMatches(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	logDir := t.TempDir()

	hugeGarbage := strings.Repeat("x", 2*1024*1024) // 2MB, not valid JSON, over the old 1MB cap
	writeJSONL(t, filepath.Join(logDir, "kahyad.jsonl"), []string{
		`{"ts":"2026-07-10T09:00:01.000Z","level":"INFO","event":"first","trace_id":"target"}`,
		hugeGarbage,
		`{"ts":"2026-07-10T09:00:02.000Z","level":"INFO","event":"second","trace_id":"target"}`,
	})

	cfg := testConfig(socketPath)
	cfg.LogDir = logDir
	srv := New(cfg, testLogger(t), "v-test", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp, err := unixHTTPClient(socketPath).Get("http://kahyad/v1/log?trace_id=target")
	if err != nil {
		t.Fatalf("GET /v1/log: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Lines []map[string]any `json:"lines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Lines) != 2 {
		t.Fatalf("got %d lines, want 2 (both real matches survive the oversized garbage line)", len(body.Lines))
	}
	if body.Lines[0]["event"] != "first" || body.Lines[1]["event"] != "second" {
		t.Errorf("lines = %+v, want [first, second]", body.Lines)
	}
}

// TestLogEndpointMissingTsSortsLastPreservingOrder guards MINOR 7: a line
// with a missing/unparseable "ts" used to parse to time.Time's zero value
// and sort BEFORE every real timestamp. It must instead sort AFTER every
// valid-ts line, in its original read order relative to any other
// missing-ts lines.
func TestLogEndpointMissingTsSortsLastPreservingOrder(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	logDir := t.TempDir()
	writeJSONL(t, filepath.Join(logDir, "kahyad.jsonl"), []string{
		`{"ts":"2026-07-10T09:00:01.000Z","level":"INFO","event":"first","trace_id":"target"}`,
		`{"ts":"2026-07-10T09:00:02.000Z","level":"INFO","event":"second","trace_id":"target"}`,
		`{"level":"INFO","event":"no-ts","trace_id":"target"}`,
	})

	cfg := testConfig(socketPath)
	cfg.LogDir = logDir
	srv := New(cfg, testLogger(t), "v-test", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp, err := unixHTTPClient(socketPath).Get("http://kahyad/v1/log?trace_id=target")
	if err != nil {
		t.Fatalf("GET /v1/log: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Lines []map[string]any `json:"lines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(body.Lines))
	}
	if body.Lines[0]["event"] != "first" || body.Lines[1]["event"] != "second" || body.Lines[2]["event"] != "no-ts" {
		t.Errorf("lines = %+v, want [first, second, no-ts] (missing ts sorts last, original order preserved)", body.Lines)
	}
}

// TestLogEndpointNoMatchesReturnsEmptyLines guards the "bogus trace_id"
// path the kahya CLI's log --trace relies on to print its not-found string:
// zero matches is a 200 with an empty "lines" array, not an error.
func TestLogEndpointNoMatchesReturnsEmptyLines(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	logDir := t.TempDir()
	writeJSONL(t, filepath.Join(logDir, "kahyad.jsonl"), []string{
		`{"ts":"2026-07-10T09:00:01.000Z","level":"INFO","event":"e","trace_id":"other"}`,
	})
	cfg := testConfig(socketPath)
	cfg.LogDir = logDir
	srv := New(cfg, testLogger(t), "v-test", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp, err := unixHTTPClient(socketPath).Get("http://kahyad/v1/log?trace_id=bogus")
	if err != nil {
		t.Fatalf("GET /v1/log: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Lines []map[string]any `json:"lines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Lines) != 0 {
		t.Errorf("got %d lines, want 0", len(body.Lines))
	}
}

// TestLogEndpointEmptyTraceIDIs400 guards malformed-input rejection: a
// missing/empty trace_id query parameter is a 400, never a panic or a
// whole-log dump.
func TestLogEndpointEmptyTraceIDIs400(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp, err := unixHTTPClient(socketPath).Get("http://kahyad/v1/log")
	if err != nil {
		t.Fatalf("GET /v1/log: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ---- W12-05: /v1/memory/search's for_injection extension ----

// fakeEventLogger is a minimal EventLogger stand-in (W12-05) so server
// tests can assert on ledgered events (policy_decision, hafiza_injected)
// without a real brain.db.
type fakeEventLogger struct {
	events []loggedEvent
}

type loggedEvent struct {
	traceID string
	kind    string
	payload map[string]any
}

func (f *fakeEventLogger) LogEvent(_ context.Context, traceID, kind string, payload map[string]any) error {
	f.events = append(f.events, loggedEvent{traceID: traceID, kind: kind, payload: payload})
	return nil
}

func (f *fakeEventLogger) eventsOfKind(kind string) []loggedEvent {
	var out []loggedEvent
	for _, e := range f.events {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// TestMemorySearchForInjectionExcludesAgentDerived is the W12-05 quarantine
// test (HANDOFF §5 memory #1, permanent regression coverage feeding
// W78-03): a chunk from an agent_derived episode is present in the
// results with for_injection:false, and absent with for_injection:true.
func TestMemorySearchForInjectionExcludesAgentDerived(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fs := &fakeSearcher{hits: []search.Hit{
		{ChunkID: 1, EpisodeID: 1, Path: "trusted.md", Seq: 0, Text: "guvenilir not", Score: 0.9, SourceTier: "user_asserted"},
		{ChunkID: 2, EpisodeID: 2, Path: "inbox/agent.md", Seq: 0, Text: "ajan notu", Score: 0.5, SourceTier: "agent_derived"},
	}}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(fs)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	client := unixHTTPClient(socketPath)

	// for_injection:false - both hits present.
	resp := postMemorySearch(t, client, `{"query":"not","for_injection":false}`)
	var bodyFalse struct {
		Results []struct {
			Path       string `json:"path"`
			SourceTier string `json:"source_tier"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&bodyFalse); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if len(bodyFalse.Results) != 2 {
		t.Fatalf("for_injection:false results = %d, want 2 (agent_derived must be included)", len(bodyFalse.Results))
	}

	// for_injection:true - agent_derived excluded.
	resp2 := postMemorySearch(t, client, `{"query":"not","for_injection":true,"task_id":"t1"}`)
	var bodyTrue struct {
		Results []struct {
			Path       string `json:"path"`
			SourceTier string `json:"source_tier"`
		} `json:"results"`
		HafizaBlock string `json:"hafiza_block"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&bodyTrue); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp2.Body.Close()
	if len(bodyTrue.Results) != 1 || bodyTrue.Results[0].Path != "trusted.md" {
		t.Fatalf("for_injection:true results = %+v, want only trusted.md", bodyTrue.Results)
	}
	for _, r := range bodyTrue.Results {
		if r.SourceTier == "agent_derived" {
			t.Errorf("agent_derived chunk leaked into for_injection:true results: %+v", r)
		}
	}
	if !strings.HasPrefix(bodyTrue.HafizaBlock, "<hafiza>") {
		t.Errorf("hafiza_block = %q, want it to start with <hafiza>", bodyTrue.HafizaBlock)
	}
	if strings.Contains(bodyTrue.HafizaBlock, "ajan notu") {
		t.Errorf("hafiza_block contains agent_derived text: %q", bodyTrue.HafizaBlock)
	}
}

// TestMemorySearchForInjectionLedgersHafizaInjected guards HANDOFF §5
// safety #4 (forensic poisoning traceability): a for_injection:true call
// ledgers a hafiza_injected event whose payload.block is byte-identical
// to the HTTP response's hafiza_block, and whose block_sha256 matches
// sha256(block).
func TestMemorySearchForInjectionLedgersHafizaInjected(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fs := &fakeSearcher{hits: []search.Hit{
		{ChunkID: 5, EpisodeID: 1, Path: "properties/kadikoy.md", Seq: 2, Text: "Kadıköy'de iki daire gezdik", Score: 0.9, SourceTier: "user_asserted"},
	}}
	led := &fakeEventLogger{}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(fs)
	srv.SetEventLogger(led)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postMemorySearch(t, unixHTTPClient(socketPath), `{"query":"kadikoy","for_injection":true,"task_id":"task-42","trace_id":"tid-hafiza"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		HafizaBlock string `json:"hafiza_block"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantBlock := "<hafiza>\n- [properties/kadikoy.md#2] Kadıköy'de iki daire gezdik\n</hafiza>"
	if body.HafizaBlock != wantBlock {
		t.Fatalf("hafiza_block = %q, want %q", body.HafizaBlock, wantBlock)
	}

	events := led.eventsOfKind("hafiza_injected")
	if len(events) != 1 {
		t.Fatalf("hafiza_injected events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.traceID != "tid-hafiza" {
		t.Errorf("ledger trace_id = %q, want tid-hafiza", ev.traceID)
	}
	if ev.payload["task_id"] != "task-42" {
		t.Errorf("ledger task_id = %v, want task-42", ev.payload["task_id"])
	}
	gotBlock, _ := ev.payload["block"].(string)
	if gotBlock != body.HafizaBlock {
		t.Fatalf("ledger payload.block = %q, want byte-identical to response hafiza_block %q", gotBlock, body.HafizaBlock)
	}
	sum := sha256.Sum256([]byte(body.HafizaBlock))
	wantSHA := hex.EncodeToString(sum[:])
	if ev.payload["block_sha256"] != wantSHA {
		t.Errorf("ledger block_sha256 = %v, want %s", ev.payload["block_sha256"], wantSHA)
	}
}

// TestMemorySearchForInjectionEmptyHitsIsEmptyBlock guards the renderer's
// "empty results -> empty string" behavior end to end through the HTTP
// handler (no hafiza_injected ledger row when there is nothing to inject).
func TestMemorySearchForInjectionEmptyHitsIsEmptyBlock(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fs := &fakeSearcher{hits: []search.Hit{
		{ChunkID: 1, EpisodeID: 1, Path: "only-agent.md", Seq: 0, Text: "ajan notu", Score: 0.5, SourceTier: "agent_derived"},
	}}
	led := &fakeEventLogger{}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(fs)
	srv.SetEventLogger(led)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()

	resp := postMemorySearch(t, unixHTTPClient(socketPath), `{"query":"ajan","for_injection":true,"task_id":"t1"}`)
	defer resp.Body.Close()
	var body struct {
		Results     []any  `json:"results"`
		HafizaBlock string `json:"hafiza_block"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Results) != 0 {
		t.Errorf("results = %+v, want empty (only hit was agent_derived)", body.Results)
	}
	if body.HafizaBlock != "" {
		t.Errorf("hafiza_block = %q, want empty string", body.HafizaBlock)
	}
	events := led.eventsOfKind("hafiza_injected")
	if len(events) != 1 {
		t.Fatalf("hafiza_injected events = %d, want 1 (still ledgered, even though block is empty)", len(events))
	}
	if events[0].payload["block"] != "" {
		t.Errorf("ledger payload.block = %v, want empty string", events[0].payload["block"])
	}
}
