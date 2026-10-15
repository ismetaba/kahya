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
	"testing"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/graph"
	"kahya/kahyad/internal/indexer"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/memlane"
	"kahya/kahyad/internal/ritual"
	"kahya/kahyad/internal/search"
	"kahya/kahyad/internal/store"
	"kahya/kahyad/internal/taint"
	"kahya/mcp/memory"
)

// graphFixture is newMCPTestFixture (real brain.db, searcher, indexer) with
// the REAL graph layer wired exactly as kahyad/main.go wires it - a
// graph.Graph over the store with the memory lane classifier - and the
// graph section switched on (config default, decision MEM-1.a).
type graphFixture struct {
	mcpTestFixture
	fe  *factengine.Engine
	idx *indexer.Indexer
}

func newGraphFixture(t *testing.T) graphFixture {
	t.Helper()
	var lane memlane.Classifier
	f := newMCPTestFixtureWith(t, testPolicyDoc(), func(srv *Server, st *store.Store) {
		lane = memlane.Classifier{Store: st.Queries, MemoryDir: srv.mcpMemoryDir, SecretGlobs: []string{filepath.Join(srv.mcpMemoryDir, "finans*.md")}, Matcher: ritual.PolicyGlobMatcher{}}
		srv.SetMemoryLane(&lane)
		srv.SetGraph(&graph.Graph{Store: st.Queries, Lane: lane})
		srv.cfg.HafizaGraphEnabled = true
		srv.cfg.HafizaGraphHops = 1
	})
	return graphFixture{
		mcpTestFixture: f,
		fe:             factengine.New(f.store.Queries, taint.New(f.store.Queries, f.store), f.store),
		idx:            indexer.New(f.store.DB(), f.memoryDir, testLogger(t)),
	}
}

// note writes relPath under the memory dir and indexes it, returning its
// episode id.
func (f graphFixture) note(t *testing.T, relPath, body string) int64 {
	t.Helper()
	abs := filepath.Join(f.memoryDir, filepath.FromSlash(relPath))
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

func (f graphFixture) entity(t *testing.T, name, kind string) int64 {
	t.Helper()
	id, _, err := f.fe.ResolveOrCreateEntity(context.Background(), name, kind)
	if err != nil {
		t.Fatalf("ResolveOrCreateEntity(%s): %v", name, err)
	}
	return id
}

// fact writes an external_doc (injection-eligible) fact citing episode ep.
func (f graphFixture) fact(t *testing.T, subj string, subjID int64, pred, obj string, objID, ep int64) int64 {
	t.Helper()
	id, err := f.fe.WriteFact(context.Background(), "tr-seed", factengine.Candidate{
		Subject: subj, Predicate: pred, Object: obj, Provenance: factengine.ProvenanceExternalDoc,
		EpisodeID: ep, SubjectEntityID: subjID, ObjectEntityID: objID, ExtractorVer: "test_v1",
		ValidFrom: "2026-05-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("WriteFact: %v", err)
	}
	return id
}

type injectionBody struct {
	HafizaBlock string `json:"hafiza_block"`
	Facts       []struct {
		FactID      int64   `json:"fact_id"`
		Subject     string  `json:"subject"`
		ConfidenceP float64 `json:"confidence_p"`
	} `json:"facts"`
}

func postInjection(t *testing.T, client *http.Client, body string) injectionBody {
	t.Helper()
	resp := postMemorySearch(t, client, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out injectionBody
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// lastHafizaPayload reads the newest hafiza_injected row of the real ledger.
func lastHafizaPayload(t *testing.T, st *store.Store) map[string]any {
	t.Helper()
	var raw string
	if err := st.DB().QueryRow(`SELECT payload FROM events WHERE kind = 'hafiza_injected' ORDER BY id DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatalf("read hafiza_injected: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return p
}

func jsonIDs(v any) []int64 {
	out := []int64{}
	arr, _ := v.([]any)
	for _, x := range arr {
		if n, ok := x.(float64); ok {
			out = append(out, int64(n))
		}
	}
	return out
}

// TestMemorySearchForInjectionIncludesEligibleFacts (FIX-MEM-1) over the
// real store, indexer, searcher, factengine and graph: a query naming an
// entity gets the eligible facts about it in the block's <olgular> section,
// and a fact citing a secret_lane_globs note (finans*.md) never enters
// that cloud-bound block nor its ledger row.
func TestMemorySearchForInjectionIncludesEligibleFacts(t *testing.T) {
	f := newGraphFixture(t)
	ep := f.note(t, "people/emre.md", "Emre Acme'de çalışıyor.\n")
	secretEp := f.note(t, "finans-emre.md", "Emre'nin kredi notu.\n")
	emre := f.entity(t, "Emre", "person")
	acme := f.entity(t, "Acme", "org")
	worksAt := f.fact(t, "Emre", emre, "works_at", "Acme", acme, ep)
	secretFact := f.fact(t, "Emre", emre, "has_loan_at", "BankX", 0, secretEp)

	got := postInjection(t, f.client, `{"query":"Emre nerede çalışıyor","for_injection":true,"task_id":"t1","trace_id":"tid-graph"}`)
	wantLine := "- [olgu#" + itoa(worksAt) + "] Emre | works_at | Acme (p=0.80, external_doc, 2026-05-01→)"
	if !strings.Contains(got.HafizaBlock, "<olgular>\n"+wantLine+"\n</olgular>") {
		t.Fatalf("hafiza_block lacks the facts section %q:\n%s", wantLine, got.HafizaBlock)
	}
	if !strings.Contains(got.HafizaBlock, "- [people/emre.md#0] Emre Acme'de çalışıyor.") {
		t.Fatalf("hafiza_block lost its chunk line:\n%s", got.HafizaBlock)
	}
	if strings.Contains(got.HafizaBlock, "BankX") || len(got.Facts) != 1 || got.Facts[0].FactID != worksAt {
		t.Fatalf("the secret-path fact %d reached the cloud block: %+v\n%s", secretFact, got.Facts, got.HafizaBlock)
	}

	p := lastHafizaPayload(t, f.store)
	if p["block"] != got.HafizaBlock || !reflect.DeepEqual(jsonIDs(p["fact_ids"]), []int64{worksAt}) {
		t.Fatalf("ledger payload = %v, want block byte-identical and fact_ids [%d]", p, worksAt)
	}
	if ents := jsonIDs(p["entity_ids"]); !reflect.DeepEqual(ents, []int64{emre, acme}) {
		t.Fatalf("entity_ids = %v, want [%d %d]", ents, emre, acme)
	}
}

// fakeGraph records Neighbourhood calls and returns a fixed answer.
type fakeGraph struct {
	lines []graph.FactLine
	ents  []int64
	err   error
	calls int
	opts  graph.Options
	eps   []int64
	q     string
}

func (g *fakeGraph) Neighbourhood(_ context.Context, _, q string, eps []int64, opts graph.Options) ([]graph.FactLine, []int64, error) {
	g.calls++
	g.q, g.eps, g.opts = q, eps, opts
	return g.lines, g.ents, g.err
}

func graphLines(n int) []graph.FactLine {
	out := make([]graph.FactLine, n)
	for i := range out {
		out[i] = graph.FactLine{FactID: int64(40 + i), Subject: "Emre", Predicate: "knows", Object: "Kişi" + itoa(int64(i)),
			SourceTier: factengine.TierUserAsserted, Evidentiality: factengine.Witnessed, ConfidenceP: 0.95}
	}
	return out
}

var graphTestHits = []search.Hit{
	{ChunkID: 5, EpisodeID: 9, Path: "people/emre.md", Seq: 0, Text: "Emre ile konuştuk", Score: 0.9, SourceTier: "user_asserted"},
	{ChunkID: 6, EpisodeID: 10, Path: "inbox/ajan.md", Seq: 0, Text: "ajan notu", Score: 0.8, SourceTier: "agent_derived"},
}

// newGraphFakeServer is a fake-searcher server with g as its graph.
func newGraphFakeServer(t *testing.T, g GraphReader, enabled bool, log *logx.Logger) (*http.Client, *fakeEventLogger, *Server) {
	t.Helper()
	socketPath := filepath.Join(shortSocketDir(t), "k.sock")
	led := &fakeEventLogger{}
	if log == nil {
		log = testLogger(t)
	}
	srv := New(testConfig(socketPath), log, "v-test", healthyDB)
	srv.SetSearcher(&fakeSearcher{hits: graphTestHits})
	srv.SetEventLogger(led)
	srv.SetGraph(g)
	srv.cfg.HafizaGraphEnabled = enabled
	srv.cfg.HafizaGraphHops = 1
	srv.cfg.HafizaFactsMaxLines = 2
	srv.cfg.HafizaFactsMaxRunes = 1200
	if err := srv.Prepare(); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Shutdown() })
	return unixHTTPClient(socketPath), led, srv
}

// TestMemorySearchLedgersFactIDsAndEntityIDs (FIX-MEM-1, HANDOFF §5 safety
// #4): the hafiza_injected row carries exactly the facts the block carries
// (fact_ids, after the hafiza_facts_max_lines cut), the walk's entity_ids,
// and block_sha256 over the whole block, <olgular> included.
func TestMemorySearchLedgersFactIDsAndEntityIDs(t *testing.T) {
	g := &fakeGraph{lines: graphLines(3), ents: []int64{3, 8}}
	client, led, _ := newGraphFakeServer(t, g, true, nil)

	got := postInjection(t, client, `{"query":"Emre kimleri tanıyor","for_injection":true,"task_id":"t-led","trace_id":"tid-led"}`)
	if !strings.Contains(got.HafizaBlock, "<olgular>") || strings.Contains(got.HafizaBlock, "[olgu#42]") {
		t.Fatalf("block = %q, want the first 2 facts only", got.HafizaBlock)
	}
	events := led.eventsOfKind("hafiza_injected")
	if len(events) != 1 {
		t.Fatalf("hafiza_injected events = %d, want 1", len(events))
	}
	p := events[0].payload
	sum := sha256.Sum256([]byte(got.HafizaBlock))
	if p["block"] != got.HafizaBlock || p["block_sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("ledger block/sha256 do not match the injected block")
	}
	if !reflect.DeepEqual(p["fact_ids"], []int64{40, 41}) || !reflect.DeepEqual(p["entity_ids"], []int64{3, 8}) {
		t.Fatalf("fact_ids = %v entity_ids = %v, want [40 41] [3 8]", p["fact_ids"], p["entity_ids"])
	}
	if len(got.Facts) != 2 || got.Facts[0].FactID != 40 || got.Facts[1].FactID != 41 {
		t.Fatalf("response facts = %+v, want the two kept facts", got.Facts)
	}
}

// TestMemorySearchGraphGetsCloudLaneAndConfigBounds: the walk is told the
// block is cloud-bound, gets the config bounds, and is seeded only with the
// episodes of the hits that survived the quarantine filter. A
// for_injection:false search never reads the graph.
func TestMemorySearchGraphGetsCloudLaneAndConfigBounds(t *testing.T) {
	g := &fakeGraph{}
	client, _, _ := newGraphFakeServer(t, g, true, nil)

	postInjection(t, client, `{"query":"Emre","for_injection":true,"task_id":"t1"}`)
	want := graph.Options{Hops: 1, MaxFacts: 2, PerHopLimit: hafizaGraphPerHopLimit, CloudLane: true}
	if g.calls != 1 || g.opts != want || g.q != "Emre" || !reflect.DeepEqual(g.eps, []int64{9}) {
		t.Fatalf("graph called %d times with q=%q eps=%v opts=%+v; want once, q=Emre, eps=[9], %+v", g.calls, g.q, g.eps, g.opts, want)
	}

	resp := postMemorySearch(t, client, `{"query":"Emre"}`)
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["facts"]; ok || g.calls != 1 {
		t.Fatalf("for_injection:false read the graph (calls %d) or returned facts: %v", g.calls, raw)
	}
}

// TestMemorySearchGraphFailureStillInjectsChunks: the graph is an
// enrichment - a walk error injects the chunk-only block (logged
// hafiza_graph_failed), never fails the request.
func TestMemorySearchGraphFailureStillInjectsChunks(t *testing.T) {
	logDir := t.TempDir()
	log, err := logx.New(logDir, "boot0000000000000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	g := &fakeGraph{err: errors.New("graph read failed")}
	client, led, _ := newGraphFakeServer(t, g, true, log)

	got := postInjection(t, client, `{"query":"Emre","for_injection":true,"task_id":"t1","trace_id":"tid-graph-fail"}`)
	want := memory.Render([]memory.Hit{{Path: "people/emre.md", Seq: 0, Text: "Emre ile konuştuk", Score: 0.9}}, memory.DefaultTopK)
	if got.HafizaBlock != want || len(got.Facts) != 0 {
		t.Fatalf("block = %q, want the chunk-only block %q", got.HafizaBlock, want)
	}
	if n := countJSONLEventLines(t, filepath.Join(logDir, "kahyad.jsonl"), "tid-graph-fail", "hafiza_graph_failed"); n != 1 {
		t.Fatalf("hafiza_graph_failed lines = %d, want 1", n)
	}
	if ev := led.eventsOfKind("hafiza_injected"); len(ev) != 1 || len(ev[0].payload["fact_ids"].([]int64)) != 0 {
		t.Fatalf("hafiza_injected = %+v, want one row with empty fact_ids", ev)
	}
}

// TestMemorySearchGraphDisabledBlockIsByteIdentical: hafiza_graph_enabled:
// false is the regression switch - the graph is never read and the block,
// response and ledger row are exactly the chunk-only ones (no fact_ids /
// entity_ids keys).
func TestMemorySearchGraphDisabledBlockIsByteIdentical(t *testing.T) {
	g := &fakeGraph{lines: graphLines(2), ents: []int64{1}}
	client, led, _ := newGraphFakeServer(t, g, false, nil)

	got := postInjection(t, client, `{"query":"Emre","for_injection":true,"task_id":"t1"}`)
	want := memory.Render([]memory.Hit{{Path: "people/emre.md", Seq: 0, Text: "Emre ile konuştuk", Score: 0.9}}, memory.DefaultTopK)
	if g.calls != 0 || got.HafizaBlock != want || got.Facts != nil {
		t.Fatalf("graph calls %d, block %q, facts %+v; want 0 calls and the chunk-only block %q", g.calls, got.HafizaBlock, got.Facts, want)
	}
	p := led.eventsOfKind("hafiza_injected")[0].payload
	if _, ok := p["fact_ids"]; ok {
		t.Fatalf("payload carries fact_ids with the graph off: %v", p)
	}
	if _, ok := p["entity_ids"]; ok {
		t.Fatalf("payload carries entity_ids with the graph off: %v", p)
	}
}
