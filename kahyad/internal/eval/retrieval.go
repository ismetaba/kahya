// retrieval.go implements W78-01's full retrieval-QA eval: a Turkish/mixed-
// language question set with human labels, a deterministic runner that
// scores precision INCLUDING abstention, and the eval.retrieval.result
// ledger event the §5-Memory-#5 pre-change gate (gate.go) consults before
// any consolidation/embedding/fusion change.
//
// Unlike mini.go's ~20-question sanity baseline (kept intact for W5-05),
// this runner queries through the SAME exported search.Searcher.Search that
// <hafiza> injection (server.handleMemorySearch) and the memory_search MCP
// tool call - never a parallel search implementation (task spec: "the eval
// MUST query through the same code path used for <hafiza> injection"). It
// therefore takes the FULL search.Hit (Score + SourceTier), which the
// scorer needs to reproduce the injection filter, rather than mini.go's
// text-only Hit.
package eval

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/search"
)

// EventRetrievalResult is the ledger event this runner appends on every run
// (green or red). Its payload is the gate's whole input surface: precision,
// total, correct, dataset_sha256, model_ver, fusion_sha256, trace_id.
const EventRetrievalResult = "eval.retrieval.result"

// MinPrecision is the green threshold (HANDOFF §6 W7-8 acceptance:
// "retrieval QA precision >= %80, çekimserlik dahil"). A run at or above
// this precision is green; below it is red and the gate refuses.
const MinPrecision = 0.80

// DefaultRetrievalK is the per-item top-K the injected set is taken from
// when an item does not set its own k. It matches the live chunk-injection
// path's memory.DefaultTopK (6) so the eval scores the same window the
// <hafiza> renderer would actually inject.
const DefaultRetrievalK = 6

// ExpectedRef is one piece of expected evidence. For an ANSWERABLE item it is
// the evidence that SHOULD be injected (correct = it appears). For an
// UNANSWERABLE item it is the specific (corpus-absent) answer that must NOT be
// confidently injected (correct = it does NOT appear) - so an unanswerable
// item MUST carry a non-empty Substring (the false answer to guard against),
// otherwise it can never be scored wrong and is a vacuous pass. A hit matches
// an ExpectedRef when its Path matches File AND its Text contains Substring.
type ExpectedRef struct {
	File      string `json:"file"`
	Substring string `json:"substring"`
}

// RetrievalItem is one dataset line (task spec Deliverables shape). Turkish
// Query text is byte-exact (real inflected morphology) and must never be
// ASCII-folded when copied.
type RetrievalItem struct {
	ID          string        `json:"id"`
	Query       string        `json:"query"`
	Lang        string        `json:"lang"`
	Answerable  bool          `json:"answerable"`
	Expected    []ExpectedRef `json:"expected"`
	LabelSource string        `json:"label_source"`
	AddedAt     string        `json:"added_at"`
	// K is the optional per-item top-K (DefaultRetrievalK when unset/non-positive).
	K int `json:"k,omitempty"`
}

// RetrievalDataset is a parsed dataset plus the SHA-256 of the exact file
// bytes it was loaded from (SHA256 is what the runner records and the gate
// matches - the dataset's identity, so a silently edited dataset can never
// satisfy a gate keyed to the old bytes).
type RetrievalDataset struct {
	Items  []RetrievalItem
	SHA256 string
}

// ParseRetrievalDataset reads the JSONL dataset (one RetrievalItem per
// non-blank line) from r, in file order. A blank line is skipped; any
// non-blank line that fails to decode is a hard error.
func ParseRetrievalDataset(r io.Reader) ([]RetrievalItem, error) {
	var out []RetrievalItem
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var it RetrievalItem
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, fmt.Errorf("eval: parse retrieval dataset line %d: %w", lineNo, err)
		}
		out = append(out, it)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("eval: read retrieval dataset: %w", err)
	}
	return out, nil
}

// LoadRetrievalDataset reads path, parses it, and computes SHA256 over the
// exact file bytes (dataset_sha256).
func LoadRetrievalDataset(path string) (RetrievalDataset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return RetrievalDataset{}, fmt.Errorf("eval: open retrieval dataset %s: %w", path, err)
	}
	items, err := ParseRetrievalDataset(strings.NewReader(string(b)))
	if err != nil {
		return RetrievalDataset{}, fmt.Errorf("eval: %s: %w", path, err)
	}
	sum := sha256.Sum256(b)
	return RetrievalDataset{Items: items, SHA256: hex.EncodeToString(sum[:])}, nil
}

// RetrievalSearcher is the EXACT memory_search surface the retrieval eval
// runs each query through. *kahyad/internal/search.Searcher satisfies it
// DIRECTLY (no adapter), which is the compile-level proof that this eval and
// <hafiza> injection (server.handleMemorySearch, which calls the same
// *search.Searcher.Search) traverse one function - see retrieval_test.go's
// `var _ RetrievalSearcher = (*search.Searcher)(nil)` assertion.
type RetrievalSearcher interface {
	Search(ctx context.Context, traceID, query string, k int) ([]search.Hit, error)
}

// injectedSet reproduces the <hafiza> chunk-injection filter for scoring
// EXACTLY: tier-eligible (factengine.TierInjectionEligible, confirmed=false -
// a chunk carries no per-item human confirmation, exactly as
// server.handleMemorySearch applies it), taking the top-k. There is
// deliberately NO numeric score floor: the live chunk-injection path applies
// none (it filters by tier, then memory.RenderKept caps at top-6 / a
// 4000-rune budget), and - critically - the fused search.Hit.Score is
// min-max normalized per query, so the top hit is ALWAYS ~1.0 regardless of
// absolute relevance (the vector leg always returns neighbors normalized to
// 1.0). A floor on that normalized score can therefore never distinguish a
// relevant match from a nearest-but-irrelevant one, so abstention is scored
// by whether the EXPECTED EVIDENCE appears in this injected set (scoreItem),
// not by an unreachable "empty set". hits arrive already ranked by fused
// score desc, so this preserves that order.
//
// A relevance-calibrated abstention floor for the hybrid vector path (an
// absolute cosine/BM25 signal, not the min-max-normalized fused score) is
// future reranker work (HANDOFF §8, "only if eval precision falls short");
// this eval measures exactly what the live path injects today.
func injectedSet(hits []search.Hit, k int) []search.Hit {
	if k <= 0 {
		k = DefaultRetrievalK
	}
	out := make([]search.Hit, 0, k)
	for _, h := range hits {
		if !factengine.TierInjectionEligible(h.SourceTier, false) {
			continue
		}
		out = append(out, h)
		if len(out) >= k {
			break
		}
	}
	return out
}

// expectedAppears reports whether any expected ref of the item is present in
// the injected set (some hit's Path matches File AND Text contains Substring).
func expectedAppears(expected []ExpectedRef, injected []search.Hit) bool {
	for _, h := range injected {
		for _, exp := range expected {
			if matchesFile(h.Path, exp.File) && strings.Contains(h.Text, exp.Substring) {
				return true
			}
		}
	}
	return false
}

// scoreItem applies the abstention-aware scoring semantics (task spec §5-
// memory-#5 "precision, çekimserlik dahil") to one item's injected set,
// SYMMETRICALLY around whether the expected evidence surfaced:
//   - answerable   -> correct iff the expected evidence APPEARS in the
//     injected set (retrieval surfaced the answer it should have).
//   - unanswerable -> correct iff the expected (corpus-absent) answer does
//     NOT appear (retrieval correctly declined to surface a false answer).
//
// This mirrors the live injection (tier-eligible top-K, no numeric floor):
// answerable=fail-to-surface and unanswerable=falsely-surface both score
// wrong. abstained (surfaced in the API response) reports whether the
// expected evidence was NOT surfaced - the good outcome for an unanswerable
// item, the bad one for an answerable item.
func scoreItem(it RetrievalItem, injected []search.Hit) (correct, abstained bool) {
	appears := expectedAppears(it.Expected, injected)
	abstained = !appears
	if it.Answerable {
		return appears, abstained
	}
	return !appears, abstained
}

// matchesFile reports whether a hit's Path satisfies an expected File
// reference: exact match, or Path ends with "/"+File (so a dataset can name
// just the relative file - e.g. "notes/ev.md" - and match a hit whose Path
// carries a longer absolute or repo-rooted prefix, WITHOUT a spurious match
// where the boundary is mid-segment, e.g. expected "ev.md" must not match
// "yeni-dev.md"). An empty expected File matches any path (the substring
// alone then decides).
func matchesFile(path, file string) bool {
	if file == "" {
		return true
	}
	return path == file || strings.HasSuffix(path, "/"+file)
}

// ItemResult is one item's per-run outcome, surfaced by the runner and the
// HTTP response.
type ItemResult struct {
	ID         string `json:"id"`
	Answerable bool   `json:"answerable"`
	Correct    bool   `json:"correct"`
	Abstained  bool   `json:"abstained"`
}

// RetrievalReport is one full run's scored outcome.
type RetrievalReport struct {
	Total     int          `json:"total"`
	Correct   int          `json:"correct"`
	Precision float64      `json:"precision"`
	Items     []ItemResult `json:"items"`
}

// RunRetrieval runs every item through searcher and scores it. A Search
// error on an item is fatal to the run (fail-closed: a run that could not
// even query search must never be recorded as a green result that unlocks a
// change) - unlike the mini baseline, where a per-question search error is
// merely that question's failure.
func RunRetrieval(ctx context.Context, searcher RetrievalSearcher, traceID string, items []RetrievalItem) (RetrievalReport, error) {
	if searcher == nil {
		return RetrievalReport{}, fmt.Errorf("eval: RunRetrieval: searcher is nil")
	}
	rep := RetrievalReport{Total: len(items), Items: make([]ItemResult, 0, len(items))}
	for _, it := range items {
		hits, err := searcher.Search(ctx, traceID, it.Query, it.K)
		if err != nil {
			return RetrievalReport{}, fmt.Errorf("eval: retrieval search %q: %w", it.ID, err)
		}
		correct, abstained := scoreItem(it, injectedSet(hits, it.K))
		if correct {
			rep.Correct++
		}
		rep.Items = append(rep.Items, ItemResult{ID: it.ID, Answerable: it.Answerable, Correct: correct, Abstained: abstained})
	}
	rep.Precision = precision(rep.Correct, rep.Total)
	return rep, nil
}

// precision = correct/total; an empty dataset is 0 (never a divide-by-zero,
// and an empty dataset must never read as green).
func precision(correct, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(correct) / float64(total)
}

// RetrievalRunner is the W78-01 retrieval-eval runner kahyad's own
// POST /v1/eval/retrieval handler calls. It loads the dataset, runs it
// against the SAME searcher /v1/memory/search calls, and ledgers exactly one
// eval.retrieval.result event carrying everything the gate needs.
type RetrievalRunner struct {
	// DatasetPath is the JSONL dataset (cfg.EvalRetrievalDatasetPath).
	// Loaded fresh each run so its SHA-256 always reflects the file on disk.
	DatasetPath string
	// ModelVer is the active embedding model_ver (cfg.ActiveEmbedModelVer) -
	// recorded so the gate can require a run against the current index state.
	ModelVer string
	// FusionSHA256 is the active fusion config's identity
	// (search.Searcher.FusionConfigSHA256()).
	FusionSHA256 string

	Searcher    RetrievalSearcher
	EventLogger EventLogger
}

// RetrievalOutcome is Run's full result.
type RetrievalOutcome struct {
	Report        RetrievalReport
	DatasetSHA256 string
	ModelVer      string
	FusionSHA256  string
}

// Run executes one full retrieval-eval pass: load the dataset, run+score it,
// and ledger one eval.retrieval.result event {precision, total, correct,
// dataset_sha256, model_ver, fusion_sha256, trace_id}. The event is written
// even on a red (< MinPrecision) run - the gate reads precision from the
// payload; a red run simply never satisfies it.
func (r *RetrievalRunner) Run(ctx context.Context, traceID string) (RetrievalOutcome, error) {
	if r.DatasetPath == "" {
		return RetrievalOutcome{}, fmt.Errorf("eval: RetrievalRunner.Run: no DatasetPath configured")
	}
	ds, err := LoadRetrievalDataset(r.DatasetPath)
	if err != nil {
		return RetrievalOutcome{}, err
	}
	rep, err := RunRetrieval(ctx, r.Searcher, traceID, ds.Items)
	if err != nil {
		return RetrievalOutcome{}, err
	}

	if r.EventLogger != nil {
		payload := map[string]any{
			"trace_id":       traceID,
			"precision":      rep.Precision,
			"total":          rep.Total,
			"correct":        rep.Correct,
			"dataset_sha256": ds.SHA256,
			"model_ver":      r.ModelVer,
			"fusion_sha256":  r.FusionSHA256,
		}
		if err := r.EventLogger.LogEvent(ctx, traceID, EventRetrievalResult, payload); err != nil {
			return RetrievalOutcome{}, fmt.Errorf("eval: ledger %s event: %w", EventRetrievalResult, err)
		}
	}

	return RetrievalOutcome{Report: rep, DatasetSHA256: ds.SHA256, ModelVer: r.ModelVer, FusionSHA256: r.FusionSHA256}, nil
}
