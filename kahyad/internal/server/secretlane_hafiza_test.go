package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"kahya/kahyad/internal/anthproxy"
	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/graph"
	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/mlx"
	"kahya/kahyad/internal/ritual"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/secretlane"
	"kahya/kahyad/internal/spawn"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/taint"
	"kahya/mcp/memory"
)

// G13 MEM-7: a locally answered task (the secret lane, and the cost
// governor's local downgrade rung - EK-MEM-A) gets its own <hafiza> block,
// built in-process for the task's REAL lane, ledgered, and prefixed to the
// local answerer's prompt. Nothing of it reaches a worker or the cloud.

// neverSpawnWorker is a worker command that does not exist: a task that
// tried to spawn it would end in an SSE error frame instead of the local
// answer (the W3-08 proof method).
var neverSpawnWorker = []string{"/nonexistent/worker/binary/should/never/run"}

// promptRecorder is a secretlane.Answerer that records every prompt it gets
// and answers "Yerel yanıt", or err.
type promptRecorder struct {
	mu      sync.Mutex
	prompts []string
	err     error
}

func (r *promptRecorder) Answer(_ context.Context, prompt string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, prompt)
	if r.err != nil {
		return "", r.err
	}
	return "Yerel yanıt", nil
}

// only returns the one prompt the answerer received.
func (r *promptRecorder) only(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.prompts) != 1 {
		t.Fatalf("answerer prompts = %d, want 1", len(r.prompts))
	}
	return r.prompts[0]
}

// localAnswerFrames posts prompt as a task and returns its result payload
// (nil when none) and its error message ("" when none).
func localAnswerFrames(t *testing.T, client *http.Client, traceID, prompt string) (map[string]any, string) {
	t.Helper()
	resp := postTask(t, client, traceID, prompt)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result map[string]any
	var errMsg string
	for _, fr := range readAllSSE(t, resp) {
		switch fr.event {
		case "result":
			if err := json.Unmarshal([]byte(fr.data), &result); err != nil {
				t.Fatalf("decode result %q: %v", fr.data, err)
			}
		case "error":
			var e struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal([]byte(fr.data), &e)
			errMsg = e.Message
		}
	}
	return result, errMsg
}

// runLocalTask posts prompt as a task, fails unless it ended with a
// processed_locally result, and returns its task id.
func runLocalTask(t *testing.T, client *http.Client, traceID, prompt string) string {
	t.Helper()
	result, errMsg := localAnswerFrames(t, client, traceID, prompt)
	if errMsg != "" {
		t.Fatalf("got an error frame %q, want the local answer (a worker spawn would fail here)", errMsg)
	}
	if result == nil || result["status"] != "ok" || result["processed_locally"] != true {
		t.Fatalf("result = %v, want status ok and processed_locally true", result)
	}
	id, _ := result["task_id"].(string)
	return id
}

// hafizaRowsForTrace returns every hafiza_injected payload under traceID.
func hafizaRowsForTrace(t *testing.T, st *store.Store, traceID string) []map[string]any {
	t.Helper()
	rows, err := st.DB().Query(`SELECT payload FROM events WHERE trace_id = ? AND kind = 'hafiza_injected' ORDER BY id`, traceID)
	if err != nil {
		t.Fatalf("query hafiza_injected: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		out = append(out, p)
	}
	return out
}

// onlyHafizaRow returns the one hafiza_injected payload under traceID.
func onlyHafizaRow(t *testing.T, st *store.Store, traceID string) map[string]any {
	t.Helper()
	rows := hafizaRowsForTrace(t, st, traceID)
	if len(rows) != 1 {
		t.Fatalf("hafiza_injected rows under %s = %d, want 1", traceID, len(rows))
	}
	return rows[0]
}

// assertBlockLedgered checks a hafiza_injected payload against the block a
// task's prompt carried: block byte-identical, block_sha256 = sha256(block),
// lane and task id as given.
func assertBlockLedgered(t *testing.T, p map[string]any, block, lane, taskID string) {
	t.Helper()
	sum := sha256.Sum256([]byte(block))
	if p["block"] != block || p["block_sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("ledgered block/sha256 = %q / %v, want the injected block %q", p["block"], p["block_sha256"], block)
	}
	if p["lane"] != lane || p["task_id"] != taskID {
		t.Fatalf("ledgered lane %v task_id %v, want %s %s", p["lane"], p["task_id"], lane, taskID)
	}
}

// splitInjectedPrompt splits a prompt the local answerer got into its
// <hafiza> block and the user's original prompt ("<block>\n\n<prompt>").
func splitInjectedPrompt(t *testing.T, got, original string) string {
	t.Helper()
	if !strings.HasPrefix(got, "<hafiza>\n") || !strings.HasSuffix(got, "\n</hafiza>\n\n"+original) {
		t.Fatalf("answerer prompt = %q, want \"<hafiza>\\n...</hafiza>\\n\\n\" + the original prompt", got)
	}
	return strings.TrimSuffix(got, "\n\n"+original)
}

// injectionTestHits is a fake search answer: a trusted note of a saglik/
// path and a quarantined agent note (quarantine is lane-independent).
var injectionTestHits = []search.Hit{
	{ChunkID: 1, EpisodeID: 1, Path: "saglik/notlar.md", Seq: 0, Text: "Doktor her sabah bir tablet yazdı.", Score: 0.9, SourceTier: factengine.TierUserAsserted},
	{ChunkID: 2, EpisodeID: 2, Path: "inbox/ajan.md", Seq: 0, Text: "ajan notu onaysız", Score: 0.8, SourceTier: factengine.TierAgentDerived},
}

// newInjectionTaskFixture is a task fixture whose secret lane is wired with
// rec as the local answerer, s as the searcher and injection set to inject;
// extra runs last in the pre-Prepare hook.
func newInjectionTaskFixture(t *testing.T, upstreamURL string, governor *anthproxy.Governor, s Searcher, rec *promptRecorder, inject bool, extra func(*Server, *store.Store)) taskTestFixture {
	t.Helper()
	if governor == nil {
		governor = generousTestGovernor()
	}
	return newTaskTestFixtureWithOptions(t, neverSpawnWorker, 5, upstreamURL, governor, func(srv *Server, st *store.Store) {
		if s != nil {
			srv.SetSearcher(s)
		}
		srv.SetSecretLane(secretlane.NewClassifier(nil), rec, nil)
		srv.cfg.SecretLaneMemoryInjection = inject
		if extra != nil {
			extra(srv, st)
		}
	})
}

const secretInjectionPrompt = "kredi kartı ekstresi ekte, özetler misin"

// TestSecretLaneTaskInjectsLocalHafiza (FIX-MEM-7, HANDOFF §5 safety #4): with
// secret_lane_memory_injection on, a secret-lane task's local answerer gets
// the <hafiza> block as a prefix of its prompt - the quarantined agent note
// left out - and the block is ledgered byte-exact as hafiza_injected with
// lane=secret, block_sha256 = sha256(block) and the task's id. The search
// ran in-process with the hook's k and the task's trace.
func TestSecretLaneTaskInjectsLocalHafiza(t *testing.T) {
	fs := &fakeSearcher{hits: injectionTestHits}
	rec := &promptRecorder{}
	f := newInjectionTaskFixture(t, "https://upstream.invalid", nil, fs, rec, true, nil)

	traceID := "trace-mem7-inject-00000000000001"
	taskID := runLocalTask(t, f.client, traceID, secretInjectionPrompt)

	got := rec.only(t)
	if !strings.HasPrefix(got, "<hafiza>\n- [saglik/notlar.md#0] ") {
		t.Fatalf("answerer prompt = %q, want it to begin with the local <hafiza> block", got)
	}
	block := splitInjectedPrompt(t, got, secretInjectionPrompt)
	want := memory.Render([]memory.Hit{{Path: "saglik/notlar.md", Seq: 0, Text: "Doktor her sabah bir tablet yazdı.", Score: 0.9}}, memory.DefaultTopK)
	if block != want {
		t.Fatalf("injected block = %q, want %q (the quarantined agent note must stay out)", block, want)
	}
	if fs.lastQ != secretInjectionPrompt || fs.lastK != memory.DefaultTopK || fs.lastTID != traceID {
		t.Fatalf("search q=%q k=%d trace=%q, want the prompt, k=%d, the task trace", fs.lastQ, fs.lastK, fs.lastTID, memory.DefaultTopK)
	}
	p := onlyHafizaRow(t, f.store, traceID)
	assertBlockLedgered(t, p, block, spawn.LaneSecret, taskID)
	if ids := jsonIDs(p["chunk_ids"]); !reflect.DeepEqual(ids, []int64{1}) {
		t.Fatalf("chunk_ids = %v, want [1]", ids)
	}
	if n := countJSONLEventLines(t, f.jsonlPath, traceID, "secretlane_hafiza_injected"); n != 1 {
		t.Fatalf("secretlane_hafiza_injected lines = %d, want 1", n)
	}
}

// TestSecretLaneTaskInjectionOffWhenDisabled (FIX-MEM-7): with
// secret_lane_memory_injection false the local answer is exactly today's -
// the bare prompt, no search, no hafiza_injected row.
func TestSecretLaneTaskInjectionOffWhenDisabled(t *testing.T) {
	fs := &fakeSearcher{hits: injectionTestHits}
	rec := &promptRecorder{}
	f := newInjectionTaskFixture(t, "https://upstream.invalid", nil, fs, rec, false, nil)

	traceID := "trace-mem7-off-0000000000000001"
	runLocalTask(t, f.client, traceID, secretInjectionPrompt)
	if got := rec.only(t); got != secretInjectionPrompt {
		t.Fatalf("answerer prompt = %q, want the bare prompt", got)
	}
	if fs.lastQ != "" {
		t.Fatalf("memory was searched (q=%q) with injection off", fs.lastQ)
	}
	if rows := hafizaRowsForTrace(t, f.store, traceID); len(rows) != 0 {
		t.Fatalf("hafiza_injected rows = %v, want none", rows)
	}
}

// TestSecretLaneTaskNoWorkerSpawnWithInjection (FIX-MEM-7, product principle
// finans/saglik/kimlik local-only): with injection on, a secret-lane task is
// still answered with no worker spawned (a nonexistent worker binary would
// end the stream in an error frame) and zero requests to the Anthropic
// upstream - the memory block is built and consumed in kahyad only.
func TestSecretLaneTaskNoWorkerSpawnWithInjection(t *testing.T) {
	upstream := newRecordingUpstream(t)
	rec := &promptRecorder{}
	f := newInjectionTaskFixture(t, upstream.srv.URL, nil, &fakeSearcher{hits: injectionTestHits}, rec, true, nil)

	traceID := "trace-mem7-noworker-000000000001"
	taskID := runLocalTask(t, f.client, traceID, secretInjectionPrompt)
	if !strings.HasPrefix(rec.only(t), "<hafiza>\n") {
		t.Fatalf("answerer prompt = %q, want the injected block", rec.only(t))
	}
	if n := upstream.hits(); n != 0 {
		t.Fatalf("upstream hits = %d, want 0", n)
	}
	assertLedgerNeverHasKind(t, f.store, "model_call")
	if n := countJSONLEventLines(t, f.jsonlPath, traceID, "anthproxy_new_failed"); n != 0 {
		t.Fatalf("a task proxy was attempted (%d anthproxy_new_failed lines)", n)
	}
	row, err := f.store.Queries.GetTaskLane(context.Background(), taskID)
	if err != nil || row.Lane != spawn.LaneSecret {
		t.Fatalf("tasks.lane = %+v, %v; want secret", row, err)
	}
}

// TestSecretLaneTaskSearchErrorStillAnswers (FIX-MEM-7): memory is an
// enrichment - a search error answers the bare prompt (JSONL
// secretlane_hafiza_failed), with no hafiza_injected row.
func TestSecretLaneTaskSearchErrorStillAnswers(t *testing.T) {
	rec := &promptRecorder{}
	f := newInjectionTaskFixture(t, "https://upstream.invalid", nil, &fakeSearcher{err: errors.New("fts read failed")}, rec, true, nil)

	traceID := "trace-mem7-searcherr-00000000001"
	runLocalTask(t, f.client, traceID, secretInjectionPrompt)
	if got := rec.only(t); got != secretInjectionPrompt {
		t.Fatalf("answerer prompt = %q, want the bare prompt", got)
	}
	if rows := hafizaRowsForTrace(t, f.store, traceID); len(rows) != 0 {
		t.Fatalf("hafiza_injected rows = %v, want none", rows)
	}
	if n := countJSONLEventLines(t, f.jsonlPath, traceID, "secretlane_hafiza_failed"); n != 1 {
		t.Fatalf("secretlane_hafiza_failed lines = %d, want 1", n)
	}
}

// TestSecretLaneTaskInjectionMemoryPressureStillFailsClosed (FIX-MEM-7,
// HANDOFF §4 memory pressure FAIL-CLOSED): when the local model is refused
// for memory, the task still ends in the exact Turkish refusal - the block
// changes nothing about it and nothing goes to the cloud.
func TestSecretLaneTaskInjectionMemoryPressureStillFailsClosed(t *testing.T) {
	upstream := newRecordingUpstream(t)
	rec := &promptRecorder{err: mlx.ErrLocalModelUnavailable}
	f := newInjectionTaskFixture(t, upstream.srv.URL, nil, &fakeSearcher{hits: injectionTestHits}, rec, true, nil)

	result, errMsg := localAnswerFrames(t, f.client, "trace-mem7-nomemory-00000000001", "tahlil sonuçları ektedir")
	if result != nil {
		t.Fatalf("got a result frame %v, want the fail-closed refusal", result)
	}
	if want := mlx.MsgNoLocalMemory + " (" + mlx.MsgNoLocalMemoryGuidance + ")"; errMsg != want {
		t.Fatalf("error message = %q, want %q", errMsg, want)
	}
	if !strings.HasPrefix(rec.only(t), "<hafiza>\n") {
		t.Fatalf("the refused call did not carry the block: %q", rec.only(t))
	}
	if n := upstream.hits(); n != 0 {
		t.Fatalf("upstream hits = %d, want 0 (never a cloud fallback)", n)
	}
}

// TestSecretLaneTaskInjectionSkippedUnderDenyAll (FIX-MEM-7, fail-closed):
// while policy.yaml is untrusted (W3-01 deny-all) no memory is served -
// /v1/memory/search answers 403 then, and the secret_lane_globs the lane
// filter needs come from that policy - so the local answer gets the bare
// prompt (JSONL secretlane_hafiza_skipped reason deny_all).
func TestSecretLaneTaskInjectionSkippedUnderDenyAll(t *testing.T) {
	fs := &fakeSearcher{hits: injectionTestHits}
	rec := &promptRecorder{}
	f := newInjectionTaskFixture(t, "https://upstream.invalid", nil, fs, rec, true, func(srv *Server, _ *store.Store) {
		srv.SetDenyAll()
	})

	traceID := "trace-mem7-denyall-0000000000001"
	runLocalTask(t, f.client, traceID, secretInjectionPrompt)
	if got := rec.only(t); got != secretInjectionPrompt || fs.lastQ != "" {
		t.Fatalf("answerer prompt = %q (searched %q), want the bare prompt and no search", got, fs.lastQ)
	}
	if n := countJSONLEventLines(t, f.jsonlPath, traceID, "secretlane_hafiza_skipped"); n != 1 {
		t.Fatalf("secretlane_hafiza_skipped lines = %d, want 1", n)
	}
}

// failingKindLogger is an EventLogger whose writes of one kind fail.
type failingKindLogger struct {
	inner EventLogger
	kind  string
}

func (l failingKindLogger) LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error {
	if kind == l.kind {
		return errors.New("ledger write failed")
	}
	return l.inner.LogEvent(ctx, traceID, kind, payload)
}

// TestSecretLaneTaskInjectionNotInjectedWhenLedgerFails (FIX-MEM-7, HANDOFF
// §5 safety #4): every injected block is ledgered - a block whose
// hafiza_injected row could not be written is not injected; the answer goes
// on with the bare prompt.
func TestSecretLaneTaskInjectionNotInjectedWhenLedgerFails(t *testing.T) {
	rec := &promptRecorder{}
	f := newInjectionTaskFixture(t, "https://upstream.invalid", nil, &fakeSearcher{hits: injectionTestHits}, rec, true, func(srv *Server, st *store.Store) {
		srv.SetEventLogger(failingKindLogger{inner: st, kind: "hafiza_injected"})
	})

	traceID := "trace-mem7-ledgerfail-0000000001"
	runLocalTask(t, f.client, traceID, secretInjectionPrompt)
	if got := rec.only(t); got != secretInjectionPrompt {
		t.Fatalf("answerer prompt = %q, want the bare prompt (the block was never ledgered)", got)
	}
	if n := countJSONLEventLines(t, f.jsonlPath, traceID, "secretlane_hafiza_failed"); n != 1 {
		t.Fatalf("secretlane_hafiza_failed lines = %d, want 1", n)
	}
}

// TestMemorySearchForInjectionLedgersLaneNormal (FIX-MEM-7): the worker
// hook's cloud-bound block is ledgered with lane "normal"; block and
// block_sha256 are unchanged by the new field.
func TestMemorySearchForInjectionLedgersLaneNormal(t *testing.T) {
	client, led, _ := newGraphFakeServer(t, &fakeGraph{}, false, nil)
	got := postInjection(t, client, `{"query":"Emre","for_injection":true,"task_id":"t-lane","trace_id":"tid-lane"}`)
	events := led.eventsOfKind("hafiza_injected")
	if len(events) != 1 {
		t.Fatalf("hafiza_injected events = %d, want 1", len(events))
	}
	p := events[0].payload
	sum := sha256.Sum256([]byte(got.HafizaBlock))
	if p["lane"] != spawn.LaneNormal || p["block"] != got.HafizaBlock || p["block_sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("payload = %v, want lane normal and the block byte-identical", p)
	}
}

// ---- the real retrieval stack: store, indexer, searcher, factengine, graph ----

// localHafizaFixture is a task fixture with the REAL memory stack wired the
// way kahyad/main.go wires it - searcher over brain.db, the memory lane
// classifier (secret glob <memDir>/finans*.md), the graph layer with the
// <olgular> section on - and rec as the local answerer.
type localHafizaFixture struct {
	taskTestFixture
	memDir string
	fe     *factengine.Engine
	idx    *indexer.Indexer
	rec    *promptRecorder
}

func newLocalHafizaFixture(t *testing.T, governor *anthproxy.Governor) localHafizaFixture {
	t.Helper()
	rec := &promptRecorder{}
	var memDir string
	f := newInjectionTaskFixture(t, "https://upstream.invalid", governor, nil, rec, true, func(srv *Server, st *store.Store) {
		memDir = srv.cfg.MemoryDir
		lane := memlane.Classifier{Store: st.Queries, MemoryDir: memDir, SecretGlobs: []string{filepath.Join(memDir, "finans*.md")}, Matcher: ritual.PolicyGlobMatcher{}}
		srv.SetSearcher(search.New(st.DB(), srv.log, search.DefaultConfig()))
		srv.SetMemoryLane(&lane)
		srv.SetGraph(&graph.Graph{Store: st.Queries, DB: st.DB(), Lane: lane})
		srv.cfg.HafizaGraphEnabled = true
		srv.cfg.HafizaGraphHops = 1
	})
	return localHafizaFixture{
		taskTestFixture: f,
		memDir:          memDir,
		fe:              factengine.New(f.store.Queries, taint.New(f.store.Queries, f.store), f.store),
		idx:             indexer.New(f.store.DB(), memDir, testLogger(t)),
		rec:             rec,
	}
}

// note writes relPath under the memory dir and indexes it (episode id).
func (f localHafizaFixture) note(t *testing.T, relPath, body string) int64 {
	t.Helper()
	abs := filepath.Join(f.memDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}
	ep, err := f.idx.ReindexFile(context.Background(), "tr-seed", relPath)
	if err != nil || ep == 0 {
		t.Fatalf("ReindexFile(%s) = %d, %v", relPath, ep, err)
	}
	return ep
}

// fact writes a confirmed (injection-eligible) external_doc fact citing ep;
// cloudLinks says whether a cloud-lane extraction call linked it (a secret
// note's facts come from the LOCAL model, cloudLinks false).
func (f localHafizaFixture) fact(t *testing.T, subj string, subjID int64, pred, obj string, ep int64, cloudLinks bool) int64 {
	t.Helper()
	id, err := f.fe.WriteFact(context.Background(), "tr-seed", factengine.Candidate{
		Subject: subj, Predicate: pred, Object: obj, Provenance: factengine.ProvenanceExternalDoc,
		EpisodeID: ep, SubjectEntityID: subjID, ExtractorVer: "test_v1",
		ValidFrom: "2026-05-01T00:00:00Z", LinksCloudLane: cloudLinks,
	})
	if err != nil {
		t.Fatalf("WriteFact: %v", err)
	}
	if err := f.fe.ConfirmFact(context.Background(), "tr-seed", id); err != nil {
		t.Fatalf("ConfirmFact: %v", err)
	}
	return id
}

// emreMemory seeds the shared memory of the real-stack tests: an ordinary
// note and a secret-path note (finans-emre.md) about Emre, one fact citing
// each - the secret one extracted by the local model - and a fact from a
// content-secret extraction call (an ordinary path whose text the local
// classifier flagged: its evidence cites no episode, kahyad/internal/
// extract), reachable only through the alias "Emre".
func (f localHafizaFixture) emreMemory(t *testing.T) (worksAt, secretFact, contentSecretFact int64) {
	t.Helper()
	ep := f.note(t, "people/emre.md", "Emre Acme'de çalışıyor.\n")
	secretEp := f.note(t, "finans-emre.md", "Emre'nin BankX kredi borcu var.\n")
	emre, _, _, err := f.fe.ResolveEntityForExtraction(context.Background(), "tr-seed", "Emre", "person", true)
	if err != nil {
		t.Fatalf("ResolveEntityForExtraction: %v", err)
	}
	worksAt = f.fact(t, "Emre", emre, "works_at", "Acme", ep, true)
	secretFact = f.fact(t, "Emre", emre, "has_loan_at", "BankX", secretEp, false)
	contentSecretFact = f.fact(t, "Emre", emre, "has_diagnosis", "Migren", 0, false)
	return worksAt, secretFact, contentSecretFact
}

// downgradedGovernor is a governor at exactly 80% of its daily budget:
// Downgraded() is true, so an ordinary Sonnet-class chat task is answered
// on the local rung (W4-08).
func downgradedGovernor(t *testing.T) *anthproxy.Governor {
	t.Helper()
	g := anthproxy.Boot([]anthproxy.BootEvent{
		{Ts: time.Now().UTC(), Record: anthproxy.ModelCallRecord{TaskID: "seed", Model: "claude-opus-5-5", USD: 8.0}},
	}, anthproxy.Limits{
		DailyBudgetUSD: 10, MonthlyBudgetUSD: 1000, TaskTokenCeiling: 500000,
		DowngradeAtRatio: 0.8, CacheHitAlarmThreshold: 0.5,
	}, nil, nil)
	if !g.Downgraded() {
		t.Fatal("test setup: governor.Downgraded() = false, want true")
	}
	return g
}

// TestGovernorDowngradedTaskInjectsLocalHafizaWithoutSecretPathFacts
// (EK-MEM-A, FIX-MEM-7): the cost governor's local downgrade rung answers a
// NORMAL-lane task through the same local path, so it gets a local block
// too - built for its real lane: the ordinary note and its fact are in it,
// the secret-path note (finans-emre.md) and the fact citing it are not, and
// the row is ledgered with lane=normal. Over the real store, indexer,
// searcher, factengine and graph.
func TestGovernorDowngradedTaskInjectsLocalHafizaWithoutSecretPathFacts(t *testing.T) {
	f := newLocalHafizaFixture(t, downgradedGovernor(t))
	worksAt, secretFact, contentSecretFact := f.emreMemory(t)

	const prompt = "Emre nerede çalışıyor?"
	traceID := "trace-mem7-downgrade-00000000001"
	taskID := runLocalTask(t, f.client, traceID, prompt)
	block := splitInjectedPrompt(t, f.rec.only(t), prompt)
	if !strings.Contains(block, "[people/emre.md#0] Emre Acme'de çalışıyor.") || !strings.Contains(block, "] Emre | works_at | Acme (") {
		t.Fatalf("block lacks the ordinary note or its fact:\n%s", block)
	}
	if strings.Contains(block, "finans-emre.md") || strings.Contains(block, "BankX") || strings.Contains(block, "Migren") {
		t.Fatalf("a secret-path note/fact or a content-secret fact reached the downgraded normal task's block:\n%s", block)
	}
	p := onlyHafizaRow(t, f.store, traceID)
	assertBlockLedgered(t, p, block, spawn.LaneNormal, taskID)
	if ids := jsonIDs(p["fact_ids"]); !reflect.DeepEqual(ids, []int64{worksAt}) {
		t.Fatalf("fact_ids = %v, want [%d] (never the secret facts %d, %d)", ids, worksAt, secretFact, contentSecretFact)
	}
	row, err := f.store.Queries.GetTaskLane(context.Background(), taskID)
	if err != nil || row.Lane != spawn.LaneNormal {
		t.Fatalf("tasks.lane = %+v, %v; want normal", row, err)
	}
}

// TestSecretLaneTaskInjectionKeepsSecretPathChunksLocally (FIX-MEM-7): a
// secret-lane task's local block keeps a secret_lane_globs note
// (finans-emre.md) that the worker hook's cloud-bound block drops - the
// block never leaves the machine (decision MEM-7.a).
func TestSecretLaneTaskInjectionKeepsSecretPathChunksLocally(t *testing.T) {
	f := newLocalHafizaFixture(t, nil)
	f.emreMemory(t)

	const prompt = "Emre'nin banka hesabındaki kredi borcu ne kadar?"
	traceID := "trace-mem7-secretchunk-000000001"
	taskID := runLocalTask(t, f.client, traceID, prompt)
	block := splitInjectedPrompt(t, f.rec.only(t), prompt)
	if !strings.Contains(block, "[finans-emre.md#0] Emre'nin BankX kredi borcu var.") {
		t.Fatalf("the secret-lane block lacks the secret-path note:\n%s", block)
	}
	assertBlockLedgered(t, onlyHafizaRow(t, f.store, traceID), block, spawn.LaneSecret, taskID)

	cloud := postInjection(t, f.client, `{"query":"Emre'nin banka hesabındaki kredi borcu ne kadar?","for_injection":true,"task_id":"t-cloud","trace_id":"tid-mem7-cloud"}`)
	if !strings.HasPrefix(cloud.HafizaBlock, "<hafiza>\n") || strings.Contains(cloud.HafizaBlock, "finans-emre.md") || strings.Contains(cloud.HafizaBlock, "BankX") {
		t.Fatalf("the cloud-bound block = %q, want a block without the secret-path note", cloud.HafizaBlock)
	}
}

// TestSecretLaneTaskInjectionIncludesSecretPathFactsLocally (FIX-MEM-7 phase
// 2, decision MEM-7.a): a secret-lane task's local block walks the graph in
// the LOCAL lane (graph.Options.CloudLane false) - the fact citing the
// secret-path note and the content-secret extraction's fact (no episode
// citation, reached through the alias "Emre") are in its <olgular> section
// and its ledgered fact_ids. The same query's cloud-bound block (the worker
// hook) carries neither.
func TestSecretLaneTaskInjectionIncludesSecretPathFactsLocally(t *testing.T) {
	f := newLocalHafizaFixture(t, nil)
	worksAt, secretFact, contentSecretFact := f.emreMemory(t)

	const prompt = "Emre'nin banka hesabındaki kredi borcu ne kadar?"
	traceID := "trace-mem7-secretfacts-000000001"
	taskID := runLocalTask(t, f.client, traceID, prompt)
	block := splitInjectedPrompt(t, f.rec.only(t), prompt)
	for _, want := range []string{"] Emre | has_loan_at | BankX (", "] Emre | has_diagnosis | Migren (", "] Emre | works_at | Acme ("} {
		if !strings.Contains(block, want) {
			t.Fatalf("the secret-lane block lacks the fact %q:\n%s", want, block)
		}
	}
	p := onlyHafizaRow(t, f.store, traceID)
	assertBlockLedgered(t, p, block, spawn.LaneSecret, taskID)
	got := map[int64]bool{}
	for _, id := range jsonIDs(p["fact_ids"]) {
		got[id] = true
	}
	if len(got) != 3 || !got[worksAt] || !got[secretFact] || !got[contentSecretFact] {
		t.Fatalf("fact_ids = %v, want {%d %d %d}", jsonIDs(p["fact_ids"]), worksAt, secretFact, contentSecretFact)
	}

	cloud := postInjection(t, f.client, `{"query":"Emre'nin banka hesabındaki kredi borcu ne kadar?","for_injection":true,"task_id":"t-cloud","trace_id":"tid-mem7-cloud-facts"}`)
	if strings.Contains(cloud.HafizaBlock, "BankX") || strings.Contains(cloud.HafizaBlock, "Migren") {
		t.Fatalf("the cloud-bound block carries a secret fact:\n%s", cloud.HafizaBlock)
	}
	if len(cloud.Facts) != 1 || cloud.Facts[0].FactID != worksAt {
		t.Fatalf("cloud-bound facts = %+v, want only %d", cloud.Facts, worksAt)
	}
}

// TestLocalHafizaGraphLaneFollowsTaskLane (FIX-MEM-7 phase 2, EK-MEM-A): the
// graph walk of a local block is told the task's real lane - CloudLane false
// only for a secret-lane task; the governor's downgraded normal-lane task
// keeps CloudLane true (its answer may flow on to TTS/Telegram), with the
// same config bounds the hook's walk gets.
func TestLocalHafizaGraphLaneFollowsTaskLane(t *testing.T) {
	for _, tc := range []struct {
		name      string
		governor  func(*testing.T) *anthproxy.Governor
		prompt    string
		cloudLane bool
	}{
		{"secret_lane", func(*testing.T) *anthproxy.Governor { return generousTestGovernor() }, secretInjectionPrompt, false},
		{"downgraded_normal", downgradedGovernor, "Emre nerede çalışıyor?", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &fakeGraph{}
			rec := &promptRecorder{}
			f := newInjectionTaskFixture(t, "https://upstream.invalid", tc.governor(t), &fakeSearcher{hits: injectionTestHits}, rec, true, func(srv *Server, _ *store.Store) {
				srv.SetGraph(g)
				srv.cfg.HafizaGraphEnabled = true
				srv.cfg.HafizaGraphHops = 1
				srv.cfg.HafizaFactsMaxLines = 7
			})
			runLocalTask(t, f.client, "trace-mem7-graphlane-"+tc.name, tc.prompt)
			want := graph.Options{Hops: 1, MaxFacts: 7, PerHopLimit: hafizaGraphPerHopLimit, CloudLane: tc.cloudLane}
			if g.calls != 1 || g.opts != want || g.q != tc.prompt || !reflect.DeepEqual(g.eps, []int64{1}) {
				t.Fatalf("graph called %d times q=%q eps=%v opts=%+v; want once, the prompt, eps [1] (the quarantined hit's episode left out), %+v", g.calls, g.q, g.eps, g.opts, want)
			}
		})
	}
}
