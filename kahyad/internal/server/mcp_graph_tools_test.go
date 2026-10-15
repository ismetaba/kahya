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
	if _, err := f.fe.AddAlias(t.Context(), "tr", emre, "Patron", false); err != nil {
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

// TestWorkerGraphReadsNeverPairNamesThroughASecretNoteAlias (FIX-MEM-1/MEM-4
// review): the worker-facing paths - /v1/mcp entity_lookup, fact_query and
// memory_search entity_id (through the policy gate) and the cloud
// for_injection <hafiza> block - never tie "Doktor" to "Ayşe Kaya" when
// only a secret-path note (finans-doktor.md, extracted locally) says they
// are the same person. The extraction steps are the lane-aware factengine
// calls kahyad/internal/extract makes (its own end-to-end test drives the
// real runner).
func TestWorkerGraphReadsNeverPairNamesThroughASecretNoteAlias(t *testing.T) {
	f := newGraphFixture(t)
	ctx := t.Context()
	for _, tool := range []string{"entity_lookup", "fact_query", "memory_search"} {
		seedAutonomyState(t, f.store, tool, "R", "global", 1)
	}
	secretEp := f.note(t, "finans-doktor.md", "Doktorum Ayşe Kaya, muayene ücreti 2000 TL.\n")
	coffeeEp := f.note(t, "inbox/2026-07-10.md", "Doktor ile kahve içtim.\n")
	peopleEp := f.note(t, "people/ayse.md", "Ayşe Kaya Kadıköy'de yaşıyor.\n")
	resolve := func(name, kind string, cloud bool) int64 {
		t.Helper()
		id, _, _, err := f.fe.ResolveEntityForExtraction(ctx, "tr-x", name, kind, cloud)
		if err != nil {
			t.Fatalf("ResolveEntityForExtraction(%s): %v", name, err)
		}
		return id
	}
	write := func(c factengine.Candidate) int64 {
		t.Helper()
		c.Provenance, c.ExtractorVer = factengine.ProvenanceExternalDoc, "fact_extract_v1"
		id, err := f.fe.WriteFact(ctx, "tr-x", c)
		if err != nil {
			t.Fatalf("WriteFact: %v", err)
		}
		// G13 MEM-6.a: confirmed, or the external_doc fact is quarantined.
		if err := f.fe.ConfirmFact(ctx, "tr-x", id); err != nil {
			t.Fatalf("ConfirmFact: %v", err)
		}
		return id
	}
	// Local-lane extraction of the secret note.
	secretAyse := resolve("Ayşe Kaya", factengine.EntityKindPerson, false)
	if _, err := f.fe.AddAlias(ctx, "tr-x", secretAyse, "Doktor", false); err != nil {
		t.Fatal(err)
	}
	write(factengine.Candidate{Subject: "Ayşe Kaya", Predicate: "is", Object: "doktorum", EpisodeID: secretEp, SubjectEntityID: secretAyse})
	// Cloud-lane extraction of the two ordinary notes.
	doktor := resolve("Doktor", factengine.EntityKindPerson, true)
	user := resolve("Kullanıcı", factengine.EntityKindPerson, true)
	coffee := write(factengine.Candidate{Subject: "Kullanıcı", Predicate: "had_coffee_with", Object: "Doktor", EpisodeID: coffeeEp,
		SubjectEntityID: user, ObjectEntityID: doktor, LinksCloudLane: true})
	cloudAyse := resolve("Ayşe Kaya", factengine.EntityKindPerson, true)
	livesIn := write(factengine.Candidate{Subject: "Ayşe Kaya", Predicate: "lives_in", Object: "Kadıköy", EpisodeID: peopleEp,
		SubjectEntityID: cloudAyse, LinksCloudLane: true})
	if doktor == secretAyse || cloudAyse == secretAyse || cloudAyse == doktor {
		t.Fatalf("cloud-lane resolution reused the secret note's entity: doktor=%d cloudAyse=%d secret=%d", doktor, cloudAyse, secretAyse)
	}

	r := callTool(t, f.mcpTestFixture, "entity_lookup", map[string]any{"name": "doktor"}, "tr-el", "")
	if r.Result.IsError || strings.Contains(string(r.Result.StructuredContent), "Ayşe") || !strings.Contains(string(r.Result.StructuredContent), `"canonical_name":"Doktor"`) {
		t.Fatalf("worker entity_lookup(doktor) = %s, want the coffee note's Doktor and no Ayşe Kaya", r.Result.StructuredContent)
	}
	r = callTool(t, f.mcpTestFixture, "entity_lookup", map[string]any{"name": "ayşe"}, "tr-el", "")
	if r.Result.IsError || strings.Contains(string(r.Result.StructuredContent), "Doktor") {
		t.Fatalf("worker entity_lookup(ayşe) = %s, want no Doktor", r.Result.StructuredContent)
	}

	r = callTool(t, f.mcpTestFixture, "fact_query", map[string]any{"include_quarantined": true}, "tr-fq", "")
	var fq memory.FactQueryOutput
	if r.Result.IsError || json.Unmarshal(r.Result.StructuredContent, &fq) != nil {
		t.Fatalf("worker fact_query = %+v", r.Result)
	}
	ents := map[int64][]int64{}
	for _, it := range fq.Facts {
		ents[it.FactID] = []int64{it.SubjectEntityID, it.ObjectEntityID}
	}
	for _, a := range ents[coffee] {
		for _, b := range ents[livesIn] {
			if a != 0 && a == b {
				t.Fatalf("worker fact_query: coffee %v and lives_in %v share entity %d", ents[coffee], ents[livesIn], a)
			}
		}
	}

	for _, ent := range []int64{secretAyse, cloudAyse, doktor} {
		r = callTool(t, f.mcpTestFixture, "memory_search", map[string]any{"query": "Doktor Ayşe kahve Kadıköy", "entity_id": ent}, "tr-ms", "")
		var ms memory.MemorySearchOutput
		if r.Result.IsError || json.Unmarshal(r.Result.StructuredContent, &ms) != nil {
			t.Fatalf("worker memory_search entity %d = %+v", ent, r.Result)
		}
		paths := map[string]bool{}
		for _, res := range ms.Results {
			paths[res.Path] = true
		}
		if paths["inbox/2026-07-10.md"] && paths["people/ayse.md"] {
			t.Fatalf("worker memory_search entity_id=%d returned both notes %v: the entity filter paired them", ent, paths)
		}
		if ent == secretAyse && len(paths) != 0 {
			t.Fatalf("worker memory_search entity_id=%d (the secret note's entity, local-lane links only) = %v, want nothing", ent, paths)
		}
		if ent == cloudAyse && !paths["people/ayse.md"] {
			t.Fatalf("worker memory_search entity_id=%d = %v, want its own cloud-linked note", ent, paths)
		}
	}

	body := postInjection(t, f.client, `{"query":"dün kahve içtim","for_injection":true,"task_id":"t-pair"}`)
	if !strings.Contains(body.HafizaBlock, "had_coffee_with") || strings.Contains(body.HafizaBlock, "lives_in") || strings.Contains(body.HafizaBlock, "doktorum") {
		t.Fatalf("cloud hafiza_block pairs the names:\n%s", body.HafizaBlock)
	}
}
