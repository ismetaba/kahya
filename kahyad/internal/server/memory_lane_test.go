package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/ritual"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/task"
	"kahya/mcp/memory"
)

// FIX-U-3: memory_write classifies its content locally at ingest; a
// secret-lane hit lands under gizli/ and escalates the CALLING task's lane
// (attribution-pinned, G00 C-9), and the cloud-facing chunk paths never
// return a secret-glob note.

// u3IBAN carries a real (checksum-valid) IBAN the deterministic pre-pass
// flags as finans.
const u3IBAN = "Kira için hesap: TR33 0006 1005 1978 6457 8413 26, aylık 42.000 TL"

// secretLaneMemoryGlobs is the fixture's secret_lane_globs: the gizli/
// subtree of the fixture memory dir (policy.yaml's ~/Kahya/memory/gizli/**
// shape, ~-expanded).
func secretLaneMemoryGlobs(memDir string) []string {
	return []string{filepath.Join(memDir, "gizli", "**")}
}

// newMCPTestFixtureSecretLane is newMCPTestFixture with the FIX-U-3 wiring
// main.go uses (minus the egress-gate sensitive marker: the mark half is
// proven in mcp/memory's tests).
func newMCPTestFixtureSecretLane(t *testing.T) mcpTestFixture {
	t.Helper()
	return newMCPTestFixtureWith(t, testPolicyDoc(), func(srv *Server, st *store.Store) {
		globs := secretLaneMemoryGlobs(srv.mcpMemoryDir)
		srv.SetMemoryLane(&memlane.Classifier{Store: st.Queries, MemoryDir: srv.mcpMemoryDir, SecretGlobs: globs, Matcher: ritual.PolicyGlobMatcher{}})
		srv.SetMCPMemorySecretLane(memory.SecretLaneSeam{
			Classifier: NewMemoryContentClassifier(),
			Escalator:  NewMemoryLaneEscalator(st.Queries, st),
			Dir:        "gizli",
			Globs:      globs,
			Matcher:    ritual.PolicyGlobMatcher{},
		})
	})
}

func secretMemoryWriteCall(id int, content, file string) string {
	args, _ := json.Marshal(map[string]string{"content": content, "file": file})
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"memory_write","arguments":%s}}`, id, args)
}

// postToolCallAs posts body to /v1/mcp with headers and decodes the tool
// result, returning it with the raw body.
func postToolCallAs(t *testing.T, client *http.Client, body string, headers map[string]string) (jsonrpcToolCallResult, string) {
	t.Helper()
	resp := postMCPWithHeaders(t, client, body, headers)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var parsed jsonrpcToolCallResult
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode tools/call: %v\nbody=%s", err, raw)
	}
	return parsed, string(raw)
}

// countEventsWhere counts events rows of kind whose payload satisfies the
// extra json_extract predicate (may be "").
func countEventsWhere(t *testing.T, st *store.Store, kind, pred string, args ...any) int {
	t.Helper()
	q := `SELECT count(*) FROM events WHERE kind = ?`
	if pred != "" {
		q += " AND " + pred
	}
	var n int
	if err := st.DB().QueryRow(q, append([]any{kind}, args...)...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", kind, err)
	}
	return n
}

func TestMemoryLaneEscalatorPersistsLaneAndLedgersSource(t *testing.T) {
	st := openEscalatorStore(t)
	insertLaneTask(t, st, "task-mw-1", "trace-mw-1", "normal")

	if err := NewMemoryLaneEscalator(st.Queries, st).EscalateSecretLane(context.Background(), "task-mw-1", "trace-mw-1", "kimlik"); err != nil {
		t.Fatalf("EscalateSecretLane: %v", err)
	}
	if lane, cat := taskLane(t, st, "task-mw-1"); lane != "secret" || cat != "kimlik" {
		t.Fatalf("lane/category = %q/%q, want secret/kimlik", lane, cat)
	}
	rows := kindPayloads(t, st, EventSecretLaneEscalated)
	if len(rows) != 1 || rows[0]["source"] != "memory_write" || rows[0]["task_id"] != "task-mw-1" || rows[0]["attribution"] != "declared" {
		t.Fatalf("%s rows = %v, want one for task-mw-1 (source memory_write, declared)", EventSecretLaneEscalated, rows)
	}
}

// TestMemoryLaneEscalatorUsesAttributedTaskOverDeclared is G00 C-9 for
// memory_write: the live worker's own task is escalated, never the one it
// names; a live worker with no tasks row fails closed.
func TestMemoryLaneEscalatorUsesAttributedTaskOverDeclared(t *testing.T) {
	st := openEscalatorStore(t)
	insertLaneTask(t, st, "task-real", "trace-real", "normal")
	insertLaneTask(t, st, "task-other", "trace-other", "normal")
	esc := NewMemoryLaneEscalator(st.Queries, st)

	if err := esc.EscalateSecretLane(withAttribution("task-real", "trace-real"), "task-other", "trace-real", "finans"); err != nil {
		t.Fatalf("EscalateSecretLane: %v", err)
	}
	if lane, _ := taskLane(t, st, "task-real"); lane != "secret" {
		t.Fatalf("attributed task lane = %q, want secret", lane)
	}
	if lane, _ := taskLane(t, st, "task-other"); lane != "normal" {
		t.Fatalf("declared task lane = %q, want normal - never escalated on a declaration", lane)
	}
	if err := esc.EscalateSecretLane(withAttribution("task-gone", "trace-gone"), "task-gone", "trace-gone", "finans"); err == nil {
		t.Fatal("EscalateSecretLane for a live worker with no tasks row = nil, want an error (fail-closed)")
	}
	rows := kindPayloads(t, st, EventSecretLaneEscalateNoTask)
	if len(rows) != 0 {
		t.Fatalf("%s rows = %v, want none", EventSecretLaneEscalateNoTask, rows)
	}
	if err := esc.EscalateSecretLane(context.Background(), "", "trace-cli", "finans"); err != nil {
		t.Fatalf("EscalateSecretLane(unattributed, no task) = %v, want nil", err)
	}
	rows = kindPayloads(t, st, EventSecretLaneEscalateNoTask)
	if len(rows) != 1 || rows[0]["source"] != "memory_write" || rows[0]["reason"] != "no_task_id" {
		t.Fatalf("%s rows = %v, want one (memory_write, no_task_id)", EventSecretLaneEscalateNoTask, rows)
	}
}

func TestMemoryContentClassifierMatchesFSAdapter(t *testing.T) {
	for _, text := range []string{"TC kimlik numaram 10000000146", u3IBAN, "Dünkü toplantı notları", "Kadıköy'de toplantı"} {
		ms, mc := NewMemoryContentClassifier().ClassifySecretLane(text)
		fs, fc := NewFSContentClassifier().ClassifySecretLane(text)
		if ms != fs || mc != fc {
			t.Errorf("memory classifier(%q) = (%v, %q), fs classifier = (%v, %q); want identical", text, ms, mc, fs, fc)
		}
	}
	if secret, cat := NewMemoryContentClassifier().ClassifySecretLane("TC kimlik numaram 10000000146"); !secret || cat != "kimlik" {
		t.Fatalf("TCKN classified (%v, %q), want (true, kimlik)", secret, cat)
	}
}

// TestMCPMemoryWriteSecretContentRedirectsAndEscalatesTask drives the
// production wiring over /v1/mcp (control-secret caller declaring its task):
// the IBAN note lands under gizli/notes/, the task's lane is secret/finans,
// and the ledger carries the escalation (source memory_write), the redirect
// and the memory_write row (secret_lane true).
func TestMCPMemoryWriteSecretContentRedirectsAndEscalatesTask(t *testing.T) {
	f := newMCPTestFixtureSecretLane(t)
	insertLaneTask(t, f.store, "task-u3", "trace-u3", "normal")
	seedAutonomyState(t, f.store, "memory_write", "W1", "global", 2)

	parsed, body := postToolCallAs(t, f.client, secretMemoryWriteCall(1, u3IBAN, "inbox/u3.md"),
		map[string]string{traceHeader: "trace-u3", taskHeader: "task-u3"})
	if parsed.Result.IsError || !strings.Contains(strings.ReplaceAll(body, `\"`, `"`), `"file":"gizli/notes/inbox/u3.md"`) {
		t.Fatalf("memory_write = %s, want success redirected to gizli/notes/inbox/u3.md", body)
	}
	if _, err := os.Stat(filepath.Join(f.memoryDir, "gizli", "notes", "inbox", "u3.md")); err != nil {
		t.Fatalf("redirected note: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.memoryDir, "inbox", "u3.md")); !os.IsNotExist(err) {
		t.Fatalf("normal-lane inbox/u3.md exists (err %v), want none", err)
	}
	if lane, cat := taskLane(t, f.store, "task-u3"); lane != "secret" || cat != "finans" {
		t.Fatalf("task lane/category = %q/%q, want secret/finans", lane, cat)
	}
	if n := countEventsWhere(t, f.store, EventSecretLaneEscalated, `json_extract(payload, '$.source') = 'memory_write' AND trace_id = ?`, "trace-u3"); n != 1 {
		t.Fatalf("%s (source memory_write, trace-u3) rows = %d, want 1", EventSecretLaneEscalated, n)
	}
	if n := countEventsWhere(t, f.store, memory.EventMemoryWriteSecretLaneRedirected, `json_extract(payload, '$.file') = 'gizli/notes/inbox/u3.md'`); n != 1 {
		t.Fatalf("%s rows = %d, want 1", memory.EventMemoryWriteSecretLaneRedirected, n)
	}
	if n := countEventsWhere(t, f.store, "memory_write", `json_extract(payload, '$.secret_lane') = 1 AND json_extract(payload, '$.task_id') = 'task-u3'`); n != 1 {
		t.Fatalf("memory_write (secret_lane true, task-u3) rows = %d, want 1", n)
	}
}

// TestMCPMemoryWriteSecretContentLiveWorkerEscalatesOwnTask: this test
// process, registered as the live worker of task-real, writes an IBAN note
// with NO control secret while declaring task-forged. The worker's OWN task
// is escalated (attribution live_worker), the forged one never is.
func TestMCPMemoryWriteSecretContentLiveWorkerEscalatesOwnTask(t *testing.T) {
	f := newMCPTestFixtureSecretLane(t)
	insertLaneTask(t, f.store, "task-real", "trace-real", "normal")
	insertLaneTask(t, f.store, "task-forged", "trace-forged", "normal")
	seedAutonomyState(t, f.store, "memory_write", "W1", "global", 2)
	reg := task.NewLiveRegistry()
	f.srv.SetTaskLiveRegistry(reg)
	reg.RegisterWorker("task-real", "trace-real", os.Getpid(), testProcessSID(t))
	defer reg.Unregister("task-real")

	for _, headers := range []map[string]string{
		{taskHeader: "task-forged", traceHeader: "trace-forged"},
		{}, // no task header at all: the attribution still names the task
	} {
		parsed, body := postToolCallAs(t, unixHTTPClientNoSecret(f.sock), secretMemoryWriteCall(2, u3IBAN, "inbox/w.md"), headers)
		if parsed.Result.IsError {
			t.Fatalf("live worker's memory_write (headers %v) = %s, want success", headers, body)
		}
	}
	if lane, cat := taskLane(t, f.store, "task-real"); lane != "secret" || cat != "finans" {
		t.Fatalf("worker's own task lane/category = %q/%q, want secret/finans", lane, cat)
	}
	if lane, _ := taskLane(t, f.store, "task-forged"); lane != "normal" {
		t.Fatalf("forged task lane = %q, want normal", lane)
	}
	if n := countEventsWhere(t, f.store, EventSecretLaneEscalated,
		`json_extract(payload, '$.task_id') = 'task-real' AND json_extract(payload, '$.attribution') = 'live_worker' AND trace_id = 'trace-real'`); n != 2 {
		t.Fatalf("%s rows for task-real (live_worker, trace-real) = %d, want 2", EventSecretLaneEscalated, n)
	}
	if n := countEventsWhere(t, f.store, EventSecretLaneEscalated, `json_extract(payload, '$.task_id') = 'task-forged'`); n != 0 {
		t.Fatalf("%s rows for task-forged = %d, want 0", EventSecretLaneEscalated, n)
	}
	if _, err := os.Stat(filepath.Join(f.memoryDir, "gizli", "notes", "inbox", "w.md")); err != nil {
		t.Fatalf("redirected note: %v", err)
	}
}

// TestMCPMemoryWriteSecretContentWithoutTaskHeaderStillLandsUnderGizli: an
// unattributed (control-secret) caller with no task id has no task proxy
// to cut off - the escalation is ledgered secret_lane_escalate_no_task, and
// the note still lands under gizli/ (the redirect does not depend on a
// task). Deviation from the recipe's "resolves by trace" variant: G00 C-9
// takes the task from caller attribution only, never a trace_id lookup.
func TestMCPMemoryWriteSecretContentWithoutTaskHeaderStillLandsUnderGizli(t *testing.T) {
	f := newMCPTestFixtureSecretLane(t)
	insertLaneTask(t, f.store, "task-u3", "trace-u3", "normal")
	seedAutonomyState(t, f.store, "memory_write", "W1", "global", 2)

	parsed, body := postToolCallAs(t, f.client, secretMemoryWriteCall(3, u3IBAN, "inbox/nt.md"), map[string]string{traceHeader: "trace-u3"})
	if parsed.Result.IsError {
		t.Fatalf("memory_write = %s, want success", body)
	}
	if _, err := os.Stat(filepath.Join(f.memoryDir, "gizli", "notes", "inbox", "nt.md")); err != nil {
		t.Fatalf("redirected note: %v", err)
	}
	if n := countEventsWhere(t, f.store, EventSecretLaneEscalateNoTask, `json_extract(payload, '$.source') = 'memory_write' AND json_extract(payload, '$.reason') = 'no_task_id'`); n != 1 {
		t.Fatalf("%s (memory_write, no_task_id) rows = %d, want 1", EventSecretLaneEscalateNoTask, n)
	}
	if lane, _ := taskLane(t, f.store, "task-u3"); lane != "normal" {
		t.Fatalf("task-u3 lane = %q, want normal (no task id was declared or attributed)", lane)
	}
}

// TestMCPMemoryWriteSecretGlobMissingRefusesOverMCP: wired seam, but no
// secret_lane_globs entry covers gizli/ - the write is refused with the
// Turkish tool error, nothing lands on disk, and the task is escalated
// anyway (the agent already holds the bytes).
func TestMCPMemoryWriteSecretGlobMissingRefusesOverMCP(t *testing.T) {
	f := newMCPTestFixtureWith(t, testPolicyDoc(), func(srv *Server, st *store.Store) {
		srv.SetMCPMemorySecretLane(memory.SecretLaneSeam{
			Classifier: NewMemoryContentClassifier(),
			Escalator:  NewMemoryLaneEscalator(st.Queries, st),
			Dir:        "gizli",
			Matcher:    ritual.PolicyGlobMatcher{},
		})
	})
	insertLaneTask(t, f.store, "task-g", "trace-g", "normal")
	seedAutonomyState(t, f.store, "memory_write", "W1", "global", 2)

	parsed, body := postToolCallAs(t, f.client, secretMemoryWriteCall(4, u3IBAN, "inbox/g.md"), map[string]string{traceHeader: "trace-g", taskHeader: "task-g"})
	if !parsed.Result.IsError || len(parsed.Result.Content) == 0 || parsed.Result.Content[0].Text != memory.MsgMemoryWriteSecretGlobMissing {
		t.Fatalf("memory_write = %s, want IsError with %q", body, memory.MsgMemoryWriteSecretGlobMissing)
	}
	for _, p := range []string{filepath.Join(f.memoryDir, "inbox", "g.md"), filepath.Join(f.memoryDir, "gizli", "notes", "inbox", "g.md")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s exists (err %v), want nothing written", p, err)
		}
	}
	if lane, _ := taskLane(t, f.store, "task-g"); lane != "secret" {
		t.Fatalf("task lane = %q, want secret (escalated before the glob check)", lane)
	}
	if n := countEventsWhere(t, f.store, "memory_write", ""); n != 0 {
		t.Fatalf("memory_write rows = %d, want 0", n)
	}
}

// TestMCPMemorySearchNeverReturnsSecretPathChunks: the memory_search tool
// (a worker's - cloud - view) never returns a chunk of a note under a
// secret-lane glob path, whatever its tier.
func TestMCPMemorySearchNeverReturnsSecretPathChunks(t *testing.T) {
	f := newMCPTestFixtureSecretLane(t)
	seedAutonomyState(t, f.store, "memory_search", "R", "global", 1)

	// Write the two notes directly below the gate, with no seam (the point
	// is only to create one secret-path and one normal note).
	log := testLogger(t)
	idx := indexer.New(f.store.DB(), f.memoryDir, log)
	searcher := search.New(f.store.DB(), log, search.DefaultConfig())
	direct := memory.New(f.memoryDir, mcpSearchAdapter{inner: searcher}, idx, f.store, log)
	for file, content := range map[string]string{
		"gizli/notes/inbox/s.md": "Kadıköy kira sözleşmesi imzalandı",
		"notes/n.md":             "Kadıköy'de toplantı",
	} {
		if _, err := direct.HandleWrite(context.Background(), "trace-seed", memory.MemoryWriteArgs{Content: content, File: file}); err != nil {
			t.Fatalf("seed %s: %v", file, err)
		}
	}

	// Sanity: without the lane filter both notes are found.
	raw, err := direct.HandleSearch(context.Background(), "trace-raw", memory.MemorySearchArgs{Query: "Kadıköy", K: 8})
	if err != nil {
		t.Fatalf("HandleSearch: %v", err)
	}
	if !hasResultPath(raw.Results, "gizli/notes/inbox/s.md") || !hasResultPath(raw.Results, "notes/n.md") {
		t.Fatalf("unfiltered search = %+v, want both notes (fixture sanity)", raw.Results)
	}

	parsed, body := postToolCallAs(t, f.client,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"memory_search","arguments":{"query":"Kadıköy","k":8}}}`,
		map[string]string{traceHeader: "trace-search"})
	if parsed.Result.IsError {
		t.Fatalf("memory_search = %s, want success", body)
	}
	unescaped := strings.ReplaceAll(body, `\"`, `"`)
	if strings.Contains(unescaped, "gizli/") {
		t.Fatalf("memory_search tool result = %s, want no gizli/ chunk", body)
	}
	if !strings.Contains(unescaped, `"path":"notes/n.md"`) {
		t.Fatalf("memory_search tool result = %s, want notes/n.md", body)
	}
}

func hasResultPath(results []memory.MemorySearchResultItem, p string) bool {
	for _, r := range results {
		if r.Path == p {
			return true
		}
	}
	return false
}

// TestMemorySearchForInjectionExcludesSecretPathChunks: the
// for_injection:true <hafiza> block (what a cloud model receives) never
// carries a chunk of a secret-glob note - even a user_asserted one - and
// the ledgered block/chunk_ids match; the raw local surface
// (for_injection:false) is not filtered.
func TestMemorySearchForInjectionExcludesSecretPathChunks(t *testing.T) {
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	fs := &fakeSearcher{hits: []search.Hit{
		{ChunkID: 1, EpisodeID: 1, Path: "gizli/notes/inbox/a.md", Seq: 0, Text: "gizli not", Score: 0.9, SourceTier: "user_asserted"},
		{ChunkID: 2, EpisodeID: 2, Path: "notes/b.md", Seq: 0, Text: "normal not", Score: 0.8, SourceTier: "user_asserted"},
		{ChunkID: 3, EpisodeID: 3, Path: "finans-2026.md", Seq: 0, Text: "finans not", Score: 0.7, SourceTier: "user_asserted"},
	}}
	led := &fakeEventLogger{}
	srv := New(testConfig(socketPath), testLogger(t), "v-test", healthyDB)
	srv.SetSearcher(fs)
	srv.SetEventLogger(led)
	srv.SetMemoryLane(&memlane.Classifier{MemoryDir: "/m", SecretGlobs: []string{"/m/gizli/**", "/m/finans*.md"}, Matcher: ritual.PolicyGlobMatcher{}})
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve()
	defer srv.Shutdown()
	client := unixHTTPClient(socketPath)

	type searchBody struct {
		Results []struct {
			ChunkID int64  `json:"chunk_id"`
			Path    string `json:"path"`
		} `json:"results"`
		HafizaBlock string `json:"hafiza_block"`
	}
	decode := func(body string) searchBody {
		t.Helper()
		resp := postMemorySearch(t, client, body)
		defer resp.Body.Close()
		var out searchBody
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	got := decode(`{"query":"not","for_injection":true,"trace_id":"t-u3"}`)
	if len(got.Results) != 1 || got.Results[0].Path != "notes/b.md" {
		t.Fatalf("for_injection:true results = %+v, want only notes/b.md", got.Results)
	}
	if strings.Contains(got.HafizaBlock, "gizli") || strings.Contains(got.HafizaBlock, "finans") || !strings.Contains(got.HafizaBlock, "normal not") {
		t.Fatalf("hafiza_block = %q, want only the normal note", got.HafizaBlock)
	}
	injected := led.eventsOfKind("hafiza_injected")
	if len(injected) != 1 {
		t.Fatalf("hafiza_injected rows = %d, want 1", len(injected))
	}
	if ids, _ := json.Marshal(injected[0].payload["chunk_ids"]); string(ids) != "[2]" {
		t.Fatalf("hafiza_injected chunk_ids = %s, want [2]", ids)
	}
	if injected[0].payload["block"] != got.HafizaBlock {
		t.Fatalf("ledgered block %q != served block %q", injected[0].payload["block"], got.HafizaBlock)
	}

	if raw := decode(`{"query":"not","for_injection":false}`); len(raw.Results) != 3 {
		t.Fatalf("for_injection:false results = %+v, want all 3 (the local surface is not filtered)", raw.Results)
	}
}
