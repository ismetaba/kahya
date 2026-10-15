package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"kahya/kahyad/internal/factengine"
	"kahya/mcp/memory"
)

// toolCallResult is a tools/call answer with its structured output.
type toolCallResult struct {
	Result struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

// callTool posts one tools/call to /v1/mcp (control-secret client: the
// declared trace/task headers are honored) and decodes the answer.
func callTool(t *testing.T, f mcpTestFixture, name string, args any, traceID, taskID string) toolCallResult {
	t.Helper()
	argJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	headers := map[string]string{}
	if traceID != "" {
		headers[traceHeader] = traceID
	}
	if taskID != "" {
		headers[taskHeader] = taskID
	}
	resp := postMCPWithHeaders(t, f.client, fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, argJSON), headers)
	defer resp.Body.Close()
	var out toolCallResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return out
}

func factQueryIDs(t *testing.T, r toolCallResult) []int64 {
	t.Helper()
	if r.Result.IsError {
		t.Fatalf("fact_query failed: %+v", r.Result.Content)
	}
	var out memory.FactQueryOutput
	if err := json.Unmarshal(r.Result.StructuredContent, &out); err != nil {
		t.Fatalf("decode fact_query output: %v (%s)", err, r.Result.StructuredContent)
	}
	ids := []int64{}
	for _, f := range out.Facts {
		ids = append(ids, f.FactID)
	}
	return ids
}

// emreWithSecretFact seeds Emre with one ordinary fact and one citing a
// finans*.md note (the graph fixture's secret glob).
func emreWithSecretFact(t *testing.T, f graphFixture) (emre, visible, secret int64) {
	t.Helper()
	ep := f.note(t, "people/emre.md", "Emre Acme'de çalışıyor.\n")
	secretEp := f.note(t, "finans-emre.md", "Emre'nin kredi notu.\n")
	emre = f.entity(t, "Emre", "person")
	visible = f.fact(t, "Emre", emre, "works_at", "Acme", 0, ep)
	secret = f.fact(t, "Emre", emre, "has_loan_at", "BankX", 0, secretEp)
	return
}

// TestMCPGateAllowsFactQueryAsClassR (FIX-MEM-4): fact_query is class R -
// allowed at L1 (the bootstrap level) with no approval token minted or
// consumed, ledgered policy_decision allow; at L0 the ladder still asks
// (a read is not exempt from the autonomy ladder; autonomy_bootstrap_levels
// seeds L1 on a fresh install).
func TestMCPGateAllowsFactQueryAsClassR(t *testing.T) {
	f := newGraphFixture(t)
	emre, visible, _ := emreWithSecretFact(t, f)

	if r := callTool(t, f.mcpTestFixture, "fact_query", map[string]any{"entity_id": emre}, "tr-l0", ""); !r.Result.IsError {
		t.Fatalf("fact_query at L0 = %+v, want the ladder's approval refusal", r.Result)
	}
	seedAutonomyState(t, f.store, "fact_query", "R", "global", 1)
	r := callTool(t, f.mcpTestFixture, "fact_query", map[string]any{"entity_id": emre}, "tr-l1", "")
	if got := factQueryIDs(t, r); !reflect.DeepEqual(got, []int64{visible}) {
		t.Fatalf("fact_query = %v, want [%d]", got, visible)
	}
	if n := countEventsWhere(t, f.store, "policy_decision", "trace_id = ? AND json_extract(payload, '$.tool') = 'fact_query' AND json_extract(payload, '$.decision') = 'allow'", "tr-l1"); n != 1 {
		t.Fatalf("policy_decision allow rows = %d, want 1", n)
	}
	var tokens int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM approval_tokens WHERE tool = 'fact_query'`).Scan(&tokens); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if tokens != 0 {
		t.Fatalf("approval_tokens for fact_query = %d, want 0 (class R consumes no token)", tokens)
	}
}

// TestFactQueryToolDropsSecretPathFactsForNormalLaneTask (FIX-MEM-4, HANDOFF
// §5 product principle): the R-tool answer goes to a cloud session, so a
// fact citing a secret_lane_globs note never comes back - even with
// include_quarantined.
func TestFactQueryToolDropsSecretPathFactsForNormalLaneTask(t *testing.T) {
	f := newGraphFixture(t)
	seedAutonomyState(t, f.store, "fact_query", "R", "global", 1)
	emre, visible, secret := emreWithSecretFact(t, f)
	insertLaneTask(t, f.store, "task-normal", "tr-normal", "normal")

	for _, args := range []map[string]any{
		{"entity_id": emre},
		{"subject": "Emre", "include_quarantined": true},
		{"predicate": "has_loan_at"},
	} {
		got := factQueryIDs(t, callTool(t, f.mcpTestFixture, "fact_query", args, "tr-normal", "task-normal"))
		for _, id := range got {
			if id == secret {
				t.Fatalf("args %v: the secret-path fact %d came back: %v", args, secret, got)
			}
		}
		if args["predicate"] == nil && !reflect.DeepEqual(got, []int64{visible}) {
			t.Fatalf("args %v = %v, want [%d]", args, got, visible)
		}
	}
}

// TestFactQueryToolUnresolvedLaneIsFailClosed: a call whose task cannot be
// resolved (no tasks row) gets the cloud filter too.
func TestFactQueryToolUnresolvedLaneIsFailClosed(t *testing.T) {
	f := newGraphFixture(t)
	seedAutonomyState(t, f.store, "fact_query", "R", "global", 1)
	emre, visible, _ := emreWithSecretFact(t, f)
	got := factQueryIDs(t, callTool(t, f.mcpTestFixture, "fact_query", map[string]any{"entity_id": emre}, "tr-no-task", "task-missing"))
	if !reflect.DeepEqual(got, []int64{visible}) {
		t.Fatalf("unresolved lane = %v, want only %d", got, visible)
	}
}

// TestGraphToolsFilterEvenWhenTaskLaneIsSecret pins the deviation from the
// recipe's lane-by-trace switch: the /v1/mcp graph tools answer a WORKER (a
// cloud session) whatever its task's lane, so even a task escalated to the
// secret lane gets no secret-path fact and no secret-only alias.
func TestGraphToolsFilterEvenWhenTaskLaneIsSecret(t *testing.T) {
	f := newGraphFixture(t)
	seedAutonomyState(t, f.store, "fact_query", "R", "global", 1)
	seedAutonomyState(t, f.store, "entity_lookup", "R", "global", 1)
	emre, visible, _ := emreWithSecretFact(t, f)
	if _, err := f.fe.AddAlias(t.Context(), "tr", emre, "Patron"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	insertLaneTask(t, f.store, "task-secret", "tr-secret", "secret")

	got := factQueryIDs(t, callTool(t, f.mcpTestFixture, "fact_query", map[string]any{"entity_id": emre, "include_quarantined": true}, "tr-secret", "task-secret"))
	if !reflect.DeepEqual(got, []int64{visible}) {
		t.Fatalf("secret-lane task fact_query = %v, want only %d", got, visible)
	}
	r := callTool(t, f.mcpTestFixture, "entity_lookup", map[string]any{"name": "Patron"}, "tr-secret", "task-secret")
	if r.Result.IsError || strings.Contains(string(r.Result.StructuredContent), "Patron") || strings.Contains(string(r.Result.StructuredContent), `"entity_id"`) {
		t.Fatalf("entity_lookup by a secret-only alias = %s, want no entity", r.Result.StructuredContent)
	}
}

// TestMCPEntityLookupToolReturnsVisibleEntity: entity_lookup over /v1/mcp
// returns the entity with its visible aliases, cloud-eligible fact count
// and top facts.
func TestMCPEntityLookupToolReturnsVisibleEntity(t *testing.T) {
	f := newGraphFixture(t)
	seedAutonomyState(t, f.store, "entity_lookup", "R", "global", 1)
	emre, visible, _ := emreWithSecretFact(t, f)

	r := callTool(t, f.mcpTestFixture, "entity_lookup", map[string]any{"name": "emre'nin"}, "tr-lookup", "")
	if r.Result.IsError {
		t.Fatalf("entity_lookup failed: %+v", r.Result.Content)
	}
	var out memory.EntityLookupOutput
	if err := json.Unmarshal(r.Result.StructuredContent, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Entities) != 1 {
		t.Fatalf("entities = %+v, want Emre", out.Entities)
	}
	e := out.Entities[0]
	if e.EntityID != emre || e.CanonicalName != "Emre" || e.FactCount != 1 || len(e.TopFacts) != 1 || e.TopFacts[0].FactID != visible {
		t.Fatalf("entity = %+v, want Emre with only the visible fact %d", e, visible)
	}
}

// TestMCPGraphToolsNilGraphReturnTurkishError: with no graph wired both
// tools are still listed and allowed, and answer the byte-exact Turkish
// tool error.
func TestMCPGraphToolsNilGraphReturnTurkishError(t *testing.T) {
	f := newMCPTestFixture(t)
	seedAutonomyState(t, f.store, "fact_query", "R", "global", 1)
	seedAutonomyState(t, f.store, "entity_lookup", "R", "global", 1)
	for name, args := range map[string]any{"fact_query": map[string]any{"predicate": "works_at"}, "entity_lookup": map[string]any{"name": "Emre"}} {
		r := callTool(t, f, name, args, "tr-nil", "")
		if !r.Result.IsError || len(r.Result.Content) == 0 || r.Result.Content[0].Text != "graf katmanı bu kurulumda kullanılamıyor" {
			t.Fatalf("%s without a graph = %+v, want the Turkish graph-unavailable error", name, r.Result)
		}
	}
}

// TestMCPMemorySearchToolAppliesFilterArgs (FIX-MEM-4): memory_search's
// filter arguments reach the real searcher (path_prefix, since), a
// non-RFC3339 since is refused, and a secret-path note never comes back
// even when the prefix names it.
func TestMCPMemorySearchToolAppliesFilterArgs(t *testing.T) {
	f := newGraphFixture(t)
	seedAutonomyState(t, f.store, "memory_search", "R", "global", 1)
	f.note(t, "people/emre.md", "Toplantı notu: Emre.\n")
	f.note(t, "inbox/gun.md", "Toplantı notu: gün.\n")
	f.note(t, "finans-plan.md", "Toplantı notu: bütçe.\n")

	paths := func(args map[string]any) []string {
		t.Helper()
		r := callTool(t, f.mcpTestFixture, "memory_search", args, "tr-ms", "")
		if r.Result.IsError {
			t.Fatalf("memory_search %v failed: %+v", args, r.Result.Content)
		}
		var out memory.MemorySearchOutput
		if err := json.Unmarshal(r.Result.StructuredContent, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var ps []string
		for _, res := range out.Results {
			ps = append(ps, res.Path)
			if res.Source != "memory_file" || res.EpisodeCreatedAt == "" {
				t.Fatalf("result %+v lacks source / episode_created_at", res)
			}
		}
		return ps
	}
	if got := paths(map[string]any{"query": "Toplantı", "path_prefix": "people/"}); !reflect.DeepEqual(got, []string{"people/emre.md"}) {
		t.Fatalf("path_prefix people/ = %v", got)
	}
	if got := paths(map[string]any{"query": "Toplantı", "path_prefix": "finans"}); len(got) != 0 {
		t.Fatalf("a secret-path note came back through the filter: %v", got)
	}
	if got := paths(map[string]any{"query": "Toplantı", "since": "2999-01-01T00:00:00Z"}); len(got) != 0 {
		t.Fatalf("since in the future = %v, want none", got)
	}
	r := callTool(t, f.mcpTestFixture, "memory_search", map[string]any{"query": "Toplantı", "since": "dün"}, "tr-ms", "")
	if !r.Result.IsError || r.Result.Content[0].Text != "memory_search: since/until RFC3339 olmalı" {
		t.Fatalf("non-RFC3339 since = %+v, want the Turkish error", r.Result)
	}
}

// getFactQuery GETs /v1/fact/query?<query> with the control-secret client.
func getFactQuery(t *testing.T, f mcpTestFixture, query string) (int, factQueryResponse, string) {
	t.Helper()
	resp, err := f.client.Get("http://kahyad/v1/fact/query?" + query)
	if err != nil {
		t.Fatalf("GET /v1/fact/query: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out factQueryResponse
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

// TestFactQueryRouteLocalLaneIncludesSecretAndQuarantined (FIX-MEM-4 phase
// 4): `kahya fact query` is the user's LOCAL surface - the secret-path fact
// is listed, and quarantined=1 adds the labelled quarantined one; an entity
// name resolves through the folded alias key.
func TestFactQueryRouteLocalLaneIncludesSecretAndQuarantined(t *testing.T) {
	f := newGraphFixture(t)
	emre, visible, secret := emreWithSecretFact(t, f)
	ep := f.note(t, "people/emre-2.md", "Emre Moda'da oturuyor.\n")
	quarantined, err := f.fe.WriteFact(t.Context(), "tr", factengine.Candidate{
		Subject: "Emre", Predicate: "lives_in", Object: "Moda", Provenance: factengine.ProvenanceAgentDerived,
		EpisodeID: ep, SubjectEntityID: emre, ExtractorVer: "t",
	})
	if err != nil {
		t.Fatalf("WriteFact: %v", err)
	}

	status, out, raw := getFactQuery(t, f.mcpTestFixture, "entity=Emre'nin")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	ids := map[int64]bool{}
	for _, it := range out.Facts {
		ids[it.FactID] = true
	}
	if len(out.Facts) != 2 || !ids[visible] || !ids[secret] || out.TraceID == "" {
		t.Fatalf("default = %s, want the eligible facts %d and %d (local lane)", raw, visible, secret)
	}
	_, out, raw = getFactQuery(t, f.mcpTestFixture, "entity="+itoa(emre)+"&quarantined=1")
	labelled := false
	for _, it := range out.Facts {
		if it.FactID == quarantined && it.Quarantined {
			labelled = true
		}
	}
	if len(out.Facts) != 3 || !labelled {
		t.Fatalf("quarantined=1 = %s, want 3 facts with %d labelled", raw, quarantined)
	}
}

func TestFactQueryRouteRejectsBadParams(t *testing.T) {
	f := newGraphFixture(t)
	for _, q := range []string{"as_of=2026-01-01", "known_as_of=dun", "since=x", "until=2026", "limit=0", "limit=101", "limit=x", "min_tier=trusted", "entity=0"} {
		if status, _, raw := getFactQuery(t, f.mcpTestFixture, q); status != http.StatusBadRequest {
			t.Fatalf("%s: status %d (%s), want 400", q, status, raw)
		}
	}
	nog := newMCPTestFixture(t)
	if status, _, _ := getFactQuery(t, nog, "predicate=works_at"); status != http.StatusServiceUnavailable {
		t.Fatalf("no graph wired: status %d, want 503", status)
	}
}
