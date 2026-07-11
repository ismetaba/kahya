package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/policy"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/store"
	"kahya/mcp/memory"
)

// ---- fixture helpers: a real store.Store + indexer.Indexer + a git
// fixture memory repo, wired into a real kahyad Server exactly the way
// main.go wires production. Used by every test in this file - the W12-05
// gate test and the direct-invocation tests both need REAL search/reindex
// behavior, not fakes, to prove the write/search round-trip actually
// works end to end.

func runGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// mcpTestFixture bundles everything a test in this file needs: a running
// kahyad Server (real store/search/indexer wired, per production), the
// git fixture repo memoryDir lives under, and the *store.Store itself
// (for direct ledger/table assertions).
type mcpTestFixture struct {
	srv       *Server
	client    *http.Client
	memoryDir string
	repoRoot  string
	store     *store.Store
}

func newMCPTestFixture(t *testing.T) mcpTestFixture {
	t.Helper()
	repoRoot := t.TempDir()
	memDir := filepath.Join(repoRoot, "memory")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatalf("mkdir memory dir: %v", err)
	}
	runGitCmd(t, repoRoot, "init", "-q")

	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db"), MemoryDir: memDir}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	log := testLogger(t)
	idx := indexer.New(st.DB(), memDir, log)
	searcher := search.New(st.DB(), log, search.DefaultConfig())

	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), log, "v-mcp-test", healthyDB)
	srv.SetSearcher(searcher)
	srv.SetEventLogger(st)
	srv.SetMCPMemory(memDir, idx)
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })

	return mcpTestFixture{
		srv: srv, client: unixHTTPClient(socketPath),
		memoryDir: memDir, repoRoot: repoRoot, store: st,
	}
}

// postMCP POSTs a raw JSON-RPC body to /v1/mcp with the headers the MCP
// streamable-HTTP spec requires (Content-Type AND an Accept that lists
// both application/json and text/event-stream - the SDK's handler
// rejects a request missing either with a plain-text 400, which is not
// itself a JSON-RPC message).
func postMCP(t *testing.T, client *http.Client, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://kahyad/v1/mcp", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/mcp: %v", err)
	}
	return resp
}

type jsonrpcToolCallResult struct {
	Result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

// ---- W12-05 acceptance: tools/list over /v1/mcp returns exactly three
// tools. ----

func TestMCPToolsListReturnsExactlyThreeTools(t *testing.T) {
	f := newMCPTestFixture(t)

	resp := postMCP(t, f.client, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var parsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body)
	}
	if len(parsed.Result.Tools) != 3 {
		t.Fatalf("tools = %+v, want exactly 3", parsed.Result.Tools)
	}
	names := map[string]bool{}
	for _, tool := range parsed.Result.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"memory_search", "memory_write", "memory_forget"} {
		if !names[want] {
			t.Errorf("missing tool %q; got %+v", want, parsed.Result.Tools)
		}
	}
}

// newMCPTestFixtureDenyAll is newMCPTestFixture's twin, but with
// SetDenyAll called before Prepare - simulating a policy.yaml load
// failure at boot (W3-01).
func newMCPTestFixtureDenyAll(t *testing.T) mcpTestFixture {
	t.Helper()
	repoRoot := t.TempDir()
	memDir := filepath.Join(repoRoot, "memory")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		t.Fatalf("mkdir memory dir: %v", err)
	}
	runGitCmd(t, repoRoot, "init", "-q")

	cfg := config.Config{DBPath: filepath.Join(t.TempDir(), "brain.db"), MemoryDir: memDir}
	st, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	log := testLogger(t)
	idx := indexer.New(st.DB(), memDir, log)
	searcher := search.New(st.DB(), log, search.DefaultConfig())

	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	srv := New(testConfig(socketPath), log, "v-mcp-denyall-test", healthyDB)
	srv.SetSearcher(searcher)
	srv.SetEventLogger(st)
	srv.SetMCPMemory(memDir, idx)
	srv.SetDenyAll()
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })

	return mcpTestFixture{
		srv: srv, client: unixHTTPClient(socketPath),
		memoryDir: memDir, repoRoot: repoRoot, store: st,
	}
}

// TestMCPGateDenyAllModeDeniesEvenMemorySearch is W3-01's deny-all
// acceptance criterion applied to the /v1/mcp mount point: while deny-all
// mode is active, even memory_search - the ONE tool the interim static
// table itself allows - is denied, with policy.RuleDenyAllV1 as its rule.
func TestMCPGateDenyAllModeDeniesEvenMemorySearch(t *testing.T) {
	f := newMCPTestFixtureDenyAll(t)

	resp := postMCP(t, f.client, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"memory_search","arguments":{"query":"test"}}}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var parsed jsonrpcToolCallResult
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body)
	}
	if !parsed.Result.IsError {
		t.Fatalf("memory_search under deny-all: result.isError = false, want true; body=%s", body)
	}
	if len(parsed.Result.Content) == 0 || parsed.Result.Content[0].Text != policy.ReasonDenyAll {
		t.Fatalf("deny-all message = %+v, want %q", parsed.Result.Content, policy.ReasonDenyAll)
	}
}

// ---- W12-05 GATE TEST: the binding boundary is kahyad-side. ----

// TestMCPGateDeniesMemoryWriteWithTurkishReasonAndLedgers is the first
// half of the step-9 gate test: a memory_write tools/call THROUGH
// /v1/mcp is denied with the interim policy's exact Turkish reason, no
// file/commit is created, and a policy_decision ledger row is added.
func TestMCPGateDeniesMemoryWriteWithTurkishReasonAndLedgers(t *testing.T) {
	f := newMCPTestFixture(t)

	reqBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory_write","arguments":{"content":"gizli yazi","file":"inbox/hack.md"}}}`
	resp := postMCP(t, f.client, reqBody)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var parsed jsonrpcToolCallResult
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body)
	}
	if !parsed.Result.IsError {
		t.Fatalf("result.isError = false, want true (denied); body=%s", body)
	}
	wantReason := "W3 politika altyapısı gelene dek yalnız hafıza araması (memory_search) açık."
	if len(parsed.Result.Content) == 0 || parsed.Result.Content[0].Text != wantReason {
		t.Fatalf("deny message = %+v, want %q", parsed.Result.Content, wantReason)
	}

	if _, err := os.Stat(filepath.Join(f.memoryDir, "inbox", "hack.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file was created despite the deny (stat err=%v)", err)
	}

	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE kind='policy_decision'`).Scan(&count); err != nil {
		t.Fatalf("count policy_decision events: %v", err)
	}
	if count != 1 {
		t.Fatalf("policy_decision event count = %d, want 1", count)
	}

	var writeCount int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE kind='memory_write'`).Scan(&writeCount); err != nil {
		t.Fatalf("count memory_write events: %v", err)
	}
	if writeCount != 0 {
		t.Fatalf("memory_write event count = %d, want 0 (the write must never have executed)", writeCount)
	}
}

// TestMCPGateDeniesMemoryForgetAndUnknownTool is a second, lighter deny
// case for coverage: memory_forget denied with the same interim reason,
// and a made-up tool name denied with the DISTINCT unknown-tool reason.
func TestMCPGateDeniesMemoryForgetAndUnknownTool(t *testing.T) {
	f := newMCPTestFixture(t)

	resp := postMCP(t, f.client, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory_forget","arguments":{"file":"x.md"}}}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var parsed jsonrpcToolCallResult
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body)
	}
	if !parsed.Result.IsError {
		t.Fatalf("memory_forget: result.isError = false, want true; body=%s", body)
	}

	resp2 := postMCP(t, f.client, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"some_unknown_tool","arguments":{}}}`)
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	var parsed2 jsonrpcToolCallResult
	if err := json.Unmarshal(body2, &parsed2); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body2)
	}
	if !parsed2.Result.IsError {
		t.Fatalf("unknown tool: result.isError = false, want true; body=%s", body2)
	}
	wantUnknown := "Tanınmayan araç reddedildi (fail-closed)."
	if len(parsed2.Result.Content) == 0 || parsed2.Result.Content[0].Text != wantUnknown {
		t.Fatalf("unknown-tool deny message = %+v, want %q", parsed2.Result.Content, wantUnknown)
	}
}

// TestMCPGateAllowsMemorySearchThroughHTTP is the allow-side complement:
// memory_search is not denied, and it too gets ledgered as a
// policy_decision (allow), matching /policy/check's "one ledger insert
// per decision" convention regardless of outcome.
func TestMCPGateAllowsMemorySearchThroughHTTP(t *testing.T) {
	f := newMCPTestFixture(t)

	resp := postMCP(t, f.client, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"memory_search","arguments":{"query":"test"}}}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var parsed jsonrpcToolCallResult
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body)
	}
	if parsed.Result.IsError {
		t.Fatalf("memory_search: result.isError = true, want false (allowed); body=%s", body)
	}

	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE kind='policy_decision'`).Scan(&count); err != nil {
		t.Fatalf("count policy_decision events: %v", err)
	}
	if count != 1 {
		t.Fatalf("policy_decision event count = %d, want 1 (allow decisions are ledgered too)", count)
	}
}

// TestMCPGateBoundaryIsKahyadSideBelowGatePerformsWrite is the second
// half of the step-9 gate test: the SAME handler logic invoked DIRECTLY
// (no HTTP, no gate) performs the write - proving the enforcement
// boundary is kahyad's /v1/mcp dispatcher, not can_use_tool, and not
// something baked into mcp/memory.Server's handlers themselves.
func TestMCPGateBoundaryIsKahyadSideBelowGatePerformsWrite(t *testing.T) {
	f := newMCPTestFixture(t)

	// Through /v1/mcp: denied (re-confirm the boundary for THIS test's
	// own fixture instance, independent of the dedicated deny test above).
	resp := postMCP(t, f.client, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"memory_write","arguments":{"content":"a","file":"inbox/gate.md"}}}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var parsed jsonrpcToolCallResult
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, body)
	}
	if !parsed.Result.IsError {
		t.Fatalf("expected deny via /v1/mcp, got body=%s", body)
	}
	if _, err := os.Stat(filepath.Join(f.memoryDir, "inbox", "gate.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file was created via /v1/mcp despite the deny")
	}

	// Directly below the gate: build the identical mcp/memory.Server
	// wiring (same memoryDir/indexer/ledger) and call HandleWrite
	// straight - no MCP dispatch, no policy gate in the way at all.
	log := testLogger(t)
	idx := indexer.New(f.store.DB(), f.memoryDir, log)
	searcher := search.New(f.store.DB(), log, search.DefaultConfig())
	memSrv := memory.New(f.memoryDir, mcpSearchAdapter{searcher}, idx, f.store, log)

	out, err := memSrv.HandleWrite(context.Background(), "tid-direct", memory.MemoryWriteArgs{
		Content: "dogrudan yazim", File: "inbox/gate.md",
	})
	if err != nil {
		t.Fatalf("direct HandleWrite (below the gate): %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(f.memoryDir, "inbox", "gate.md"))
	if err != nil {
		t.Fatalf("direct write did not create the file: %v", err)
	}
	if !strings.Contains(string(raw), "dogrudan yazim") {
		t.Errorf("written file missing expected content: %q", raw)
	}
	if out.CommitSHA == "" {
		t.Errorf("direct write returned no commit sha")
	}
}

// ---- direct-invocation tests against a real fixture memory repo:
// write -> front matter -> git author -> search finds it; forget(heading)
// / forget(file); for_injection quarantine (see server_test.go for the
// /v1/memory/search-level version of the quarantine test). ----

func TestMCPDirectHandlerWriteEndToEndFindableBySearch(t *testing.T) {
	f := newMCPTestFixture(t)
	log := testLogger(t)
	idx := indexer.New(f.store.DB(), f.memoryDir, log)
	searcher := search.New(f.store.DB(), log, search.DefaultConfig())
	memSrv := memory.New(f.memoryDir, mcpSearchAdapter{searcher}, idx, f.store, log)

	_, err := memSrv.HandleWrite(context.Background(), "tid-direct", memory.MemoryWriteArgs{
		Content: "Kadıköy'de iki daire gezdik", File: "notes/kadikoy.md",
	})
	if err != nil {
		t.Fatalf("HandleWrite: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(f.memoryDir, "notes", "kadikoy.md"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !strings.Contains(string(raw), "kahya_source_tier: agent_derived") {
		t.Fatalf("written file missing agent_derived front matter: %q", raw)
	}

	author := runGitCmd(t, f.repoRoot, "log", "-1", "--format=%an")
	if author != "kahyad" {
		t.Fatalf("git log author = %q, want kahyad", author)
	}

	hits, err := searcher.Search(context.Background(), "tid-direct", "Kadıköy", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, h := range hits {
		if strings.Contains(h.Text, "Kadıköy'de iki daire gezdik") {
			found = true
		}
	}
	if !found {
		t.Fatalf("search did not find the newly written text; hits=%+v", hits)
	}

	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE kind='memory_write'`).Scan(&count); err != nil {
		t.Fatalf("count memory_write events: %v", err)
	}
	if count != 1 {
		t.Fatalf("memory_write event count = %d, want 1", count)
	}
}

func TestMCPDirectHandlerForgetHeadingRemovesOnlyThatSection(t *testing.T) {
	f := newMCPTestFixture(t)
	seed := "# Notlar\n\n## Birinci\nBirinci icerik.\n\n## Ikinci\nSaklanacak.\n\n## Ucuncu\nUcuncu icerik.\n"
	if err := os.WriteFile(filepath.Join(f.memoryDir, "notes.md"), []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runGitCmd(t, f.repoRoot, "add", "-A")
	runGitCmd(t, f.repoRoot, "-c", "user.email=seed@example.com", "-c", "user.name=Seed", "commit", "-q", "-m", "seed")

	log := testLogger(t)
	idx := indexer.New(f.store.DB(), f.memoryDir, log)
	searcher := search.New(f.store.DB(), log, search.DefaultConfig())
	memSrv := memory.New(f.memoryDir, mcpSearchAdapter{searcher}, idx, f.store, log)

	if _, err := idx.ReindexFile(context.Background(), "tid-seed", "notes.md"); err != nil {
		t.Fatalf("seed reindex: %v", err)
	}

	if _, err := memSrv.HandleForget(context.Background(), "tid-forget", memory.MemoryForgetArgs{File: "notes.md", Heading: "Ikinci"}); err != nil {
		t.Fatalf("HandleForget: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(f.memoryDir, "notes.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), "Saklanacak") {
		t.Errorf("removed section still present: %q", raw)
	}
	if !strings.Contains(string(raw), "Birinci icerik.") || !strings.Contains(string(raw), "Ucuncu icerik.") {
		t.Errorf("unrelated sections affected: %q", raw)
	}

	hits, err := searcher.Search(context.Background(), "tid-forget", "Saklanacak", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hits {
		if strings.Contains(h.Text, "Saklanacak") {
			t.Errorf("removed section is still searchable: %+v", h)
		}
	}
}

func TestMCPDirectHandlerForgetFileMakesTextUnsearchableButKeepsGitHistory(t *testing.T) {
	f := newMCPTestFixture(t)
	if err := os.WriteFile(filepath.Join(f.memoryDir, "notes.md"), []byte("essiz bir cumle burada"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runGitCmd(t, f.repoRoot, "add", "-A")
	runGitCmd(t, f.repoRoot, "-c", "user.email=seed@example.com", "-c", "user.name=Seed", "commit", "-q", "-m", "seed")

	log := testLogger(t)
	idx := indexer.New(f.store.DB(), f.memoryDir, log)
	searcher := search.New(f.store.DB(), log, search.DefaultConfig())
	memSrv := memory.New(f.memoryDir, mcpSearchAdapter{searcher}, idx, f.store, log)

	if _, err := idx.ReindexFile(context.Background(), "tid-seed", "notes.md"); err != nil {
		t.Fatalf("seed reindex: %v", err)
	}
	hitsBefore, err := searcher.Search(context.Background(), "t", "essiz", 5)
	if err != nil || len(hitsBefore) == 0 {
		t.Fatalf("sanity: text should be searchable before forget; hits=%+v err=%v", hitsBefore, err)
	}

	if _, err := memSrv.HandleForget(context.Background(), "tid-forget", memory.MemoryForgetArgs{File: "notes.md"}); err != nil {
		t.Fatalf("HandleForget: %v", err)
	}

	if _, err := os.Stat(filepath.Join(f.memoryDir, "notes.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("original file still present (err=%v)", err)
	}
	entries, err := os.ReadDir(filepath.Join(f.memoryDir, ".trash"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly one .trash entry, got %v (err=%v)", entries, err)
	}

	hitsAfter, err := searcher.Search(context.Background(), "t", "essiz", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hitsAfter {
		if strings.Contains(h.Text, "essiz") {
			t.Errorf("forgotten text is still searchable: %+v", h)
		}
	}

	logOut := runGitCmd(t, f.repoRoot, "log", "--all", "--oneline")
	if !strings.Contains(logOut, "seed") {
		t.Errorf("git history missing the seed commit: %s", logOut)
	}
}
