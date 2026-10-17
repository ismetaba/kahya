package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/taint"
	"kahya/mcp/memory"
)

// fact_history_test.go covers FIX-FINAL fact-history-ended-vs-corrected
// (owner decision 2026-10-16) at kahyad's surfaces: POST /v1/fact/retract
// records the kind, and fact_query (MCP, cloud lane) / GET /v1/fact/query
// (CLI, local lane) answer ended facts at a point in time - labelled - and
// never corrected ones.

// retractAt retracts a graph-fixture fact as kind at the given instant.
func (f graphFixture) retractAt(t *testing.T, subj, pred, obj string, kind factengine.RetractionKind, at string) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	f.fe.SetClock(func() time.Time { return ts })
	defer f.fe.SetClock(time.Now)
	if _, err := f.fe.RetractFact(context.Background(), "tr-retract", subj, pred, obj, "", kind); err != nil {
		t.Fatalf("RetractFact(%s %s %s): %v", subj, pred, obj, err)
	}
}

// emreHistory seeds Emre with an active fact, an ENDED one and a CORRECTED
// one (all valid from 2026-05-01, the graph fixture's fact default; both
// retractions on 2026-08-01).
func emreHistory(t *testing.T, f graphFixture) (emre, active, ended, corrected int64) {
	t.Helper()
	ep := f.note(t, "people/emre.md", "Emre Acme'de çalışıyor, Moda'da oturuyor.\n")
	emre = f.entity(t, "Emre", "person")
	active = f.fact(t, "Emre", emre, "works_at", "Beta", 0, ep)
	ended = f.fact(t, "Emre", emre, "works_at", "Acme", 0, ep)
	corrected = f.fact(t, "Emre", emre, "lives_in", "Moda", 0, ep)
	f.retractAt(t, "Emre", "works_at", "Acme", factengine.RetractionEnded, "2026-08-01T00:00:00Z")
	f.retractAt(t, "Emre", "lives_in", "Moda", factengine.RetractionCorrected, "2026-08-01T00:00:00Z")
	return
}

// TestFactQueryToolLabelsEndedFactsSonaErdi: the worker's fact_query with
// as_of inside the ended fact's window returns it with ended:true and the
// byte-exact label "sona erdi"; the corrected one never comes back; without
// as_of/known_as_of the answer is the active fact only.
func TestFactQueryToolLabelsEndedFactsSonaErdi(t *testing.T) {
	f := newGraphFixture(t)
	seedAutonomyState(t, f.store, "fact_query", "R", "global", 1)
	emre, active, ended, corrected := emreHistory(t, f)

	r := callTool(t, f.mcpTestFixture, "fact_query", map[string]any{"entity_id": emre, "as_of": "2026-06-01T00:00:00Z"}, "tr-hist", "")
	if r.Result.IsError {
		t.Fatalf("fact_query failed: %+v", r.Result.Content)
	}
	var out memory.FactQueryOutput
	if err := json.Unmarshal(r.Result.StructuredContent, &out); err != nil {
		t.Fatal(err)
	}
	byID := map[int64]memory.FactRecord{}
	for _, rec := range out.Facts {
		byID[rec.FactID] = rec
	}
	if len(out.Facts) != 2 || byID[active].Ended || byID[active].Label != "" {
		t.Fatalf("as_of = %s, want the active %d unlabelled and the ended %d (never the corrected %d)", r.Result.StructuredContent, active, ended, corrected)
	}
	got := byID[ended]
	if !got.Ended || got.Label != "sona erdi" || got.RetractedAt != "2026-08-01T00:00:00Z" || got.ValidTo != "2026-08-01T00:00:00Z" {
		t.Fatalf("ended record = %+v, want ended, label \"sona erdi\", retracted/valid_to 2026-08-01", got)
	}
	if !strings.Contains(string(r.Result.StructuredContent), `"label":"sona erdi"`) {
		t.Fatalf("structured output lacks the byte-exact label: %s", r.Result.StructuredContent)
	}

	// The facts were written on the real clock: known_as_of an hour from now
	// is after both retractions.
	later := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if ids := factQueryIDs(t, callTool(t, f.mcpTestFixture, "fact_query", map[string]any{"entity_id": emre, "known_as_of": later}, "tr-hist2", "")); len(ids) != 1 || ids[0] != active {
		t.Fatalf("known_as_of after both retractions = %v, want only the active %d", ids, active)
	}
	if ids := factQueryIDs(t, callTool(t, f.mcpTestFixture, "fact_query", map[string]any{"entity_id": emre, "include_quarantined": true}, "tr-now", "")); len(ids) != 1 || ids[0] != active {
		t.Fatalf("no point in time = %v, want only the active %d", ids, active)
	}
}

// TestFactQueryRouteMarksEndedFacts: `kahya fact query --as-of` (GET
// /v1/fact/query, local lane) marks the ended fact ended:true with its
// retraction time; the corrected one is absent.
func TestFactQueryRouteMarksEndedFacts(t *testing.T) {
	f := newGraphFixture(t)
	emre, active, ended, corrected := emreHistory(t, f)
	status, out, raw := getFactQuery(t, f.mcpTestFixture, "entity="+itoa(emre)+"&as_of=2026-06-01T00:00:00Z")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, raw)
	}
	marks := map[int64]factQueryItem{}
	for _, it := range out.Facts {
		marks[it.FactID] = it
	}
	if len(out.Facts) != 2 || marks[active].Ended || !marks[ended].Ended || marks[ended].RetractedAt != "2026-08-01T00:00:00Z" {
		t.Fatalf("as_of = %s, want active %d plain + ended %d marked (never corrected %d)", raw, active, ended, corrected)
	}
	if _, out, raw := getFactQuery(t, f.mcpTestFixture, "entity="+itoa(emre)); len(out.Facts) != 1 || out.Facts[0].FactID != active || strings.Contains(raw, `"ended"`) {
		t.Fatalf("no as_of = %s, want only the active %d with no ended field", raw, active)
	}
}

// TestFactRetractRouteRecordsTheKind: POST /v1/fact/retract defaults to a
// correction (an older client, `kahya fact retract`), records "ended" when
// asked and answers the kind it recorded; an unknown kind is a 400 that
// touches nothing; an ended request on a denied fact is recorded corrected.
func TestFactRetractRouteRecordsTheKind(t *testing.T) {
	client, fe, st := newEntityRouteFixture(t, true)
	ctx := context.Background()
	if err := taint.New(st.Queries, st).InsertClean(ctx, "tr", "clean"); err != nil {
		t.Fatal(err)
	}
	write := func(obj string) int64 {
		t.Helper()
		id, err := fe.WriteFact(ctx, "tr", factengine.Candidate{
			Subject: "user", Predicate: "likes", Object: obj, Provenance: factengine.ProvenanceUserAsserted,
			SessionID: "clean", ExtractorVer: "user_direct_v1",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	kindOf := func(id int64) string {
		t.Helper()
		f, err := fe.GetFact(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return f.RetractionKind.String
	}
	retract := func(obj, kind string) (int, map[string]any) {
		t.Helper()
		body := fmt.Sprintf(`{"subject":"user","predicate":"likes","object":%q}`, obj)
		if kind != "" {
			body = fmt.Sprintf(`{"subject":"user","predicate":"likes","object":%q,"kind":%q}`, obj, kind)
		}
		var out map[string]any
		return postFactJSON(t, client, "/v1/fact/retract", body, &out), out
	}

	plain, ended, bogus, denied := write("kahve"), write("çay"), write("su"), write("ayran")
	if status, out := retract("kahve", ""); status != http.StatusOK || out["kind"] != "corrected" || out["id"] != float64(plain) || kindOf(plain) != "corrected" {
		t.Fatalf("default retract = %d %v (stored %q), want corrected", status, out, kindOf(plain))
	}
	if status, out := retract("çay", "ended"); status != http.StatusOK || out["kind"] != "ended" || kindOf(ended) != "ended" {
		t.Fatalf("ended retract = %d %v (stored %q), want ended", status, out, kindOf(ended))
	}
	if status, out := retract("su", "sona-erdi"); status != http.StatusBadRequest {
		t.Fatalf("unknown kind = %d %v, want 400", status, out)
	}
	if f, _ := fe.GetFact(ctx, bogus); f.Status != "active" {
		t.Fatalf("fact after a refused kind = %q, want untouched", f.Status)
	}
	if err := fe.DenyFact(ctx, "tr", denied, "cli:deny"); err != nil {
		t.Fatal(err)
	}
	if status, out := retract("ayran", "ended"); status != http.StatusOK || out["kind"] != "corrected" || kindOf(denied) != "corrected" {
		t.Fatalf("ended retract of a denied fact = %d %v (stored %q), want recorded corrected", status, out, kindOf(denied))
	}
}

// TestMCPFactQueryToolDescriptionCarriesTheHistoryRule: tools/list shows
// fact_query with memory.FactQueryToolDescription and the as_of /
// known_as_of argument descriptions naming the "sona erdi" label.
func TestMCPFactQueryToolDescriptionCarriesTheHistoryRule(t *testing.T) {
	f := newMCPTestFixture(t)
	resp := postMCP(t, f.client, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	defer resp.Body.Close()
	var parsed struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				InputSchema struct {
					Properties map[string]struct {
						Description string `json:"description"`
					} `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	for _, tool := range parsed.Result.Tools {
		if tool.Name != "fact_query" {
			continue
		}
		if tool.Description != memory.FactQueryToolDescription {
			t.Fatalf("fact_query description = %q", tool.Description)
		}
		for _, arg := range []string{"as_of", "known_as_of"} {
			if d := tool.InputSchema.Properties[arg].Description; !strings.Contains(d, "'sona erdi'") {
				t.Errorf("%s description = %q, want it to name the 'sona erdi' label", arg, d)
			}
		}
		return
	}
	t.Fatal("fact_query not listed")
}
