package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/eval"
	"kahya/kahyad/internal/logx"
	"kahya/kahyad/internal/search"
)

// recencyTestLogger is a real JSONL logger in a temp dir plus a reader of
// its decoded lines.
func recencyTestLogger(t *testing.T) (*logx.Logger, func() []map[string]any) {
	t.Helper()
	dir := t.TempDir()
	log, err := logx.New(dir, "boot-recency-00000000000000000000")
	if err != nil {
		t.Fatalf("logx.New: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	return log, func() []map[string]any {
		f, err := os.Open(filepath.Join(dir, "kahyad.jsonl"))
		if err != nil {
			t.Fatalf("open log: %v", err)
		}
		defer f.Close()
		var out []map[string]any
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var rec map[string]any
			if err := json.Unmarshal(sc.Bytes(), &rec); err == nil {
				out = append(out, rec)
			}
		}
		return out
	}
}

func eventsNamedIn(recs []map[string]any, name string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["event"] == name {
			out = append(out, r)
		}
	}
	return out
}

// ledgerReader serves fixed eval.retrieval.result rows to the real gate.
type ledgerReader struct{ rows []eval.EventRow }

func (r ledgerReader) ListEventsByKind(_ context.Context, kind string) ([]eval.EventRow, error) {
	if kind != eval.EventRetrievalResult {
		return nil, nil
	}
	return r.rows, nil
}

func greenRetrievalRow(t *testing.T, datasetSHA, modelVer, fusionSHA string) eval.EventRow {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"precision": 0.9, "dataset_sha256": datasetSHA, "model_ver": modelVer, "fusion_sha256": fusionSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	return eval.EventRow{ID: 1, Payload: string(b), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
}

// recencyGate is the production gate shape main.go builds, over a fixed
// ledger.
func recencyGate(rows ...eval.EventRow) eval.FusionActivationGate {
	return eval.FusionActivationGate{
		Gate:          eval.EvalGate{Reader: ledgerReader{rows: rows}},
		MaxAge:        24 * time.Hour,
		DatasetSHA256: "ds-1",
		ModelVer:      "mv-1",
	}
}

func recencyCandidateConfig(weight float64, halfLife int) search.Config {
	c := search.DefaultConfig()
	c.RecencyWeight = weight
	c.RecencyHalfLifeDays = halfLife
	return c
}

func newCandidateSearcher(c search.Config) *search.Searcher { return search.New(nil, nil, c) }

// TestSearchRecencyNotActivatedWithoutGreenEval (FIX-MEM-8): a configured
// search_recency_weight with no green retrieval eval for that exact config
// leaves the searcher on the default config (its fusion identity unchanged),
// logs search_recency_not_activated with the gate's Turkish reason, and
// hands the eval runner a candidate under the recency config's own sha.
func TestSearchRecencyNotActivatedWithoutGreenEval(t *testing.T) {
	log, lines := recencyTestLogger(t)
	searcher := search.New(nil, nil, search.DefaultConfig())
	before := searcher.FusionConfigSHA256()
	// A green result exists - but only for the DEFAULT config.
	gate := recencyGate(greenRetrievalRow(t, "ds-1", "mv-1", before))

	cand := activateSearchRecency(searcher, 0.2, 90, gate, newCandidateSearcher, log)
	if searcher.FusionConfigSHA256() != before {
		t.Fatal("recency must not activate without a green eval for its own config")
	}
	wantSHA := search.ConfigSHA256(recencyCandidateConfig(0.2, 90))
	if cand == nil || cand.FusionSHA256 != wantSHA {
		t.Fatalf("candidate = %+v, want the recency config's sha %s", cand, wantSHA)
	}
	if s, ok := cand.Searcher.(*search.Searcher); !ok || s.FusionConfigSHA256() != wantSHA {
		t.Fatalf("candidate searcher = %#v, want one running the recency config", cand.Searcher)
	}
	recs := eventsNamedIn(lines(), "search_recency_not_activated")
	if len(recs) != 1 {
		t.Fatalf("search_recency_not_activated lines = %d, want 1", len(recs))
	}
	if reason, _ := recs[0]["reason"].(string); !strings.Contains(reason, eval.GateRefusalReason) {
		t.Fatalf("reason = %q, want the gate's Turkish refusal", reason)
	}
	if recs[0]["candidate_fusion_sha256"] != wantSHA || recs[0]["level"] != "WARN" {
		t.Fatalf("log line = %v", recs[0])
	}
	if len(eventsNamedIn(lines(), "search_recency_activated")) != 0 {
		t.Fatal("no search_recency_activated line may appear on a refusal")
	}

	// A nil gate is a refusal too (fail-closed).
	if c := activateSearchRecency(searcher, 0.2, 90, nil, newCandidateSearcher, log); c == nil || searcher.FusionConfigSHA256() != before {
		t.Fatal("a nil gate must refuse and keep the default config")
	}
}

// TestSearchRecencyActivatedAfterGreenEval (FIX-MEM-8): once a green
// eval.retrieval.result for the recency config's fusion_sha256 exists (what
// the candidate pass of `kahya eval retrieval` ledgers), the next boot
// activates it: the searcher's identity becomes the recency config's, no
// candidate remains, search_recency_activated is logged.
func TestSearchRecencyActivatedAfterGreenEval(t *testing.T) {
	log, lines := recencyTestLogger(t)
	searcher := search.New(nil, nil, search.DefaultConfig())
	wantSHA := search.ConfigSHA256(recencyCandidateConfig(0.2, 30))
	gate := recencyGate(greenRetrievalRow(t, "ds-1", "mv-1", wantSHA))

	if cand := activateSearchRecency(searcher, 0.2, 30, gate, newCandidateSearcher, log); cand != nil {
		t.Fatalf("candidate = %+v, want nil once recency is active", cand)
	}
	if searcher.FusionConfigSHA256() != wantSHA {
		t.Fatalf("active fusion sha = %s, want the recency config's %s", searcher.FusionConfigSHA256(), wantSHA)
	}
	recs := eventsNamedIn(lines(), "search_recency_activated")
	if len(recs) != 1 || recs[0]["fusion_sha256"] != wantSHA {
		t.Fatalf("search_recency_activated lines = %v", recs)
	}
}

// countingGate counts gate consultations.
type countingGate struct{ calls int }

func (g *countingGate) CheckFusionActivation(string) (bool, string) { g.calls++; return true, "" }

// TestSearchRecencyOffIsNoop (FIX-MEM-8): weight 0 (the default) never
// consults the gate, never changes the fusion identity, logs nothing.
func TestSearchRecencyOffIsNoop(t *testing.T) {
	log, lines := recencyTestLogger(t)
	searcher := search.New(nil, nil, search.DefaultConfig())
	before := searcher.FusionConfigSHA256()
	g := &countingGate{}
	if cand := activateSearchRecency(searcher, 0, 90, g, newCandidateSearcher, log); cand != nil {
		t.Fatalf("candidate = %+v, want nil", cand)
	}
	if g.calls != 0 || searcher.FusionConfigSHA256() != before {
		t.Fatalf("gate calls = %d, sha changed = %v; want 0, false", g.calls, searcher.FusionConfigSHA256() != before)
	}
	for _, r := range lines() {
		if ev, _ := r["event"].(string); strings.HasPrefix(ev, "search_recency") {
			t.Fatalf("unexpected log line %v", r)
		}
	}
}

// TestSearchRecencyDefaultsMatchSearchPackage: config's default half-life is
// search's.
func TestSearchRecencyDefaultsMatchSearchPackage(t *testing.T) {
	if config.DefaultSearchRecencyHalfLifeDays != search.DefaultRecencyHalfLifeDays {
		t.Fatalf("config default half-life %d != search default %d", config.DefaultSearchRecencyHalfLifeDays, search.DefaultRecencyHalfLifeDays)
	}
}

// TestSearchRecencyActivatedBeforeFusionSHAIsRead (FIX-MEM-8): main.go
// activates recency from the config keys, through the real eval gate, BEFORE
// it reads searcher.FusionConfigSHA256() - the identity every other gate and
// the retrieval eval record - and hands the pending candidate to the
// retrieval runner.
func TestSearchRecencyActivatedBeforeFusionSHAIsRead(t *testing.T) {
	_, _, src := mainSource(t)
	act := onlyCallSite(t, "", "activateSearchRecency", "")
	if !strings.Contains(act.args, "cfg.SearchRecencyWeight") || !strings.Contains(act.args, "cfg.SearchRecencyHalfLifeDays") {
		t.Fatalf("activateSearchRecency(%s) must take the two config keys", act.args)
	}
	sha := onlyCallSite(t, "searcher", "FusionConfigSHA256", "")
	if act.offset > sha.offset {
		t.Fatal("recency must be activated before main.go reads searcher.FusionConfigSHA256()")
	}
	fusionGate := onlyCompositeLit(t, "eval", "FusionActivationGate")
	for k, want := range map[string]string{
		"Gate": "evalGate", "DatasetSHA256": "retrievalDatasetSHA",
		"ModelVer": "cfg.ActiveEmbedModelVer", "MaxAge": "consolidation.DefaultEvalGateMaxAge",
	} {
		if fusionGate[k] != want {
			t.Errorf("eval.FusionActivationGate.%s = %q, want %q", k, fusionGate[k], want)
		}
	}
	runner := onlyCompositeLit(t, "eval", "RetrievalRunner")
	if runner["Candidate"] != "recencyCandidate" || runner["FusionSHA256"] != "fusionSHA" {
		t.Errorf("eval.RetrievalRunner = %v, want FusionSHA256: fusionSHA and Candidate: recencyCandidate", runner)
	}
	if !strings.Contains(string(src[:sha.offset]), "recencyCandidate := activateSearchRecency(") {
		t.Error("main.go must keep activateSearchRecency's candidate as recencyCandidate")
	}
}
