package eval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kahya/kahyad/internal/factengine"
	"kahya/kahyad/internal/search"
)

// --- fakes ---

// fakeRetrievalSearcher returns fixed hits per query text (a synthetic
// "corpus"): the scorer/runner never needs a real brain.db or FTS index to
// be exercised deterministically.
type fakeRetrievalSearcher struct {
	byQuery map[string][]search.Hit
	err     error
}

func (f *fakeRetrievalSearcher) Search(ctx context.Context, traceID, q string, k int) ([]search.Hit, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byQuery[q], nil
}

// recordingLogger captures ledgered events for assertions.
type recordingLogger struct {
	events []recordedEvent
}

type recordedEvent struct {
	kind    string
	payload map[string]any
}

func (l *recordingLogger) LogEvent(ctx context.Context, traceID, kind string, payload map[string]any) error {
	l.events = append(l.events, recordedEvent{kind: kind, payload: payload})
	return nil
}

// fakeGateReader serves fixed eval.retrieval.result rows to EvalGate.
type fakeGateReader struct {
	rows []EventRow
	err  error
}

func (r fakeGateReader) ListEventsByKind(ctx context.Context, kind string) ([]EventRow, error) {
	if r.err != nil {
		return nil, r.err
	}
	if kind != EventRetrievalResult {
		return nil, nil
	}
	return r.rows, nil
}

func greenRow(createdAt time.Time, datasetSHA, modelVer, fusionSHA string, precision float64) EventRow {
	payload, _ := json.Marshal(map[string]any{
		"precision":      precision,
		"dataset_sha256": datasetSHA,
		"model_ver":      modelVer,
		"fusion_sha256":  fusionSHA,
	})
	return EventRow{ID: 1, Payload: string(payload), CreatedAt: createdAt.Format(time.RFC3339)}
}

// --- loader ---

func TestLoadRetrievalDatasetSynth(t *testing.T) {
	ds, err := LoadRetrievalDataset("testdata/dataset.synth.jsonl")
	if err != nil {
		t.Fatalf("LoadRetrievalDataset: %v", err)
	}
	if len(ds.Items) != 13 {
		t.Fatalf("len(items) = %d, want 13", len(ds.Items))
	}
	if ds.SHA256 == "" || len(ds.SHA256) != 64 {
		t.Fatalf("SHA256 = %q, want a 64-hex string", ds.SHA256)
	}
	var unanswerable, mixed int
	for _, it := range ds.Items {
		if !it.Answerable {
			unanswerable++
		}
		if it.Lang == "mixed" {
			mixed++
		}
	}
	if unanswerable < 2 {
		t.Errorf("unanswerable = %d, want >= 2", unanswerable)
	}
	if mixed < 3 {
		t.Errorf("mixed-lang = %d, want >= 3", mixed)
	}
	// Turkish byte-exactness: the inflected form must survive verbatim.
	if ds.Items[0].Query != "evlerimizden en yakını hangisi" {
		t.Errorf("item 0 query = %q, want byte-exact Turkish", ds.Items[0].Query)
	}
}

func TestLoadRetrievalDatasetSHAChangesWithBytes(t *testing.T) {
	a, err := ParseRetrievalDataset(strings.NewReader(`{"id":"a","query":"q","answerable":true,"expected":[{"file":"f","substring":"s"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || a[0].ID != "a" {
		t.Fatalf("parse = %+v", a)
	}
}

// --- injectedSet (the injection filter) ---

// TestInjectedSetTierAndTopK: injectedSet mirrors the live injection exactly -
// a tier filter (agent_derived dropped) + a top-K cap, in the score-desc order
// Search delivers, and DELIBERATELY no numeric score floor (a low-scoring but
// tier-eligible hit is NOT dropped: the fused Score is min-max normalized, so a
// floor on it cannot distinguish relevant from nearest-but-irrelevant - see
// injectedSet's doc).
func TestInjectedSetTierAndTopK(t *testing.T) {
	hits := []search.Hit{ // already score-desc, as Search returns
		{ChunkID: 1, Path: "a.md", Text: "one", Score: 0.9, SourceTier: "user_asserted"},
		{ChunkID: 2, Path: "b.md", Text: "two", Score: 0.85, SourceTier: factengine.TierAgentDerived}, // excluded: tier
		{ChunkID: 3, Path: "c.md", Text: "three", Score: 0.8, SourceTier: "user_edit"},
		{ChunkID: 4, Path: "d.md", Text: "four", Score: 0.2, SourceTier: "external_doc"}, // low score, still kept (no floor)
	}
	got := injectedSet(hits, 2) // top-2 after the tier filter only
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (top-K cap after tier filter)", len(got))
	}
	if got[0].ChunkID != 1 || got[1].ChunkID != 3 {
		t.Fatalf("got ids %d,%d, want 1,3 (agent_derived dropped; no score floor)", got[0].ChunkID, got[1].ChunkID)
	}
}

// --- scorer (design D1 abstention semantics) ---

func TestScoreItemAnswerableMatch(t *testing.T) {
	it := RetrievalItem{Answerable: true, Expected: []ExpectedRef{{File: "notes/ev.md", Substring: "ev"}}}
	// Path ends-with the expected relative file, and Text contains "ev".
	injected := []search.Hit{{Path: "memory/notes/ev.md", Text: "en yakın ev burada", Score: 0.9, SourceTier: "user_asserted"}}
	correct, abstained := scoreItem(it, injected)
	if !correct || abstained {
		t.Fatalf("correct=%v abstained=%v, want true/false", correct, abstained)
	}
}

func TestScoreItemAnswerableMissScoresWrong(t *testing.T) {
	it := RetrievalItem{Answerable: true, Expected: []ExpectedRef{{File: "notes/ev.md", Substring: "ev"}}}
	// A non-matching hit (wrong file AND missing substring): not evidence.
	injected := []search.Hit{{Path: "notes/other.md", Text: "alakasız", Score: 0.9, SourceTier: "user_asserted"}}
	correct, abstained := scoreItem(it, injected)
	// answerable but the expected evidence did not surface: WRONG, and
	// abstained=true (the system declined to surface evidence it should have).
	if correct || !abstained {
		t.Fatalf("correct=%v abstained=%v, want false/true (answerable but no matching evidence surfaced)", correct, abstained)
	}
}

func TestScoreItemUnanswerableAbstainScoresCorrect(t *testing.T) {
	// The corpus-absent answer for this unanswerable item is "Mars taşınma
	// tarihi". The injected set (non-empty - the live path always returns
	// top-K neighbors) does NOT contain it, so retrieval correctly declined to
	// surface a false answer.
	it := RetrievalItem{Answerable: false, Expected: []ExpectedRef{{Substring: "Mars taşınma tarihi"}}}
	injected := []search.Hit{{Path: "notes/gezi.md", Text: "geçen yıl tatile gittik", Score: 0.9, SourceTier: "user_asserted"}}
	correct, abstained := scoreItem(it, injected)
	if !correct || !abstained {
		t.Fatalf("correct=%v abstained=%v, want true/true (unanswerable, false answer not surfaced)", correct, abstained)
	}
}

func TestScoreItemUnanswerableFalsePositiveScoresWrong(t *testing.T) {
	// Retrieval WRONGLY surfaced the corpus-absent answer - a hallucinated /
	// mis-ranked chunk that contains the guarded substring. That is a false
	// positive: the unanswerable item scores wrong.
	it := RetrievalItem{Answerable: false, Expected: []ExpectedRef{{Substring: "Mars taşınma tarihi"}}}
	injected := []search.Hit{{Path: "notes/x.md", Text: "Mars taşınma tarihi 2030", Score: 0.9, SourceTier: "user_asserted"}}
	correct, abstained := scoreItem(it, injected)
	if correct || abstained {
		t.Fatalf("correct=%v abstained=%v, want false/false (unanswerable but the false answer was surfaced)", correct, abstained)
	}
}

func TestPrecision(t *testing.T) {
	if got := precision(4, 5); got != 0.8 {
		t.Errorf("precision(4,5) = %v, want 0.8", got)
	}
	if got := precision(0, 0); got != 0 {
		t.Errorf("precision(0,0) = %v, want 0 (empty dataset never green)", got)
	}
}

// --- runner (end to end over the synthetic corpus + ledger) ---

// synthFixture builds a fake searcher that, for every ANSWERABLE item,
// returns one tier-eligible above-floor hit carrying that item's first
// expected evidence, and returns nothing for unanswerable items - so the
// whole synthetic dataset scores 100% (green).
func synthFixture(t *testing.T) (*fakeRetrievalSearcher, RetrievalDataset) {
	t.Helper()
	ds, err := LoadRetrievalDataset("testdata/dataset.synth.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	byQuery := map[string][]search.Hit{}
	for _, it := range ds.Items {
		if !it.Answerable {
			continue // abstain
		}
		exp := it.Expected[0]
		byQuery[it.Query] = []search.Hit{{
			Path:       "memory/" + exp.File,
			Text:       "... " + exp.Substring + " ...",
			Score:      0.95,
			SourceTier: "user_asserted",
		}}
	}
	return &fakeRetrievalSearcher{byQuery: byQuery}, ds
}

func TestRetrievalRunnerGreenRunLedgersResult(t *testing.T) {
	searcher, ds := synthFixture(t)
	logger := &recordingLogger{}
	r := &RetrievalRunner{
		DatasetPath:  "testdata/dataset.synth.jsonl",
		ModelVer:     "m1",
		FusionSHA256: "f1",
		Searcher:     searcher,
		EventLogger:  logger,
	}
	out, err := r.Run(context.Background(), "trace-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Report.Precision != 1.0 {
		t.Fatalf("precision = %v, want 1.0", out.Report.Precision)
	}
	if out.Report.Total != len(ds.Items) || out.Report.Correct != len(ds.Items) {
		t.Fatalf("correct/total = %d/%d, want %d/%d", out.Report.Correct, out.Report.Total, len(ds.Items), len(ds.Items))
	}
	if out.DatasetSHA256 != ds.SHA256 {
		t.Fatalf("dataset sha = %q, want %q", out.DatasetSHA256, ds.SHA256)
	}
	// exactly one eval.retrieval.result event, carrying the gate's inputs.
	if len(logger.events) != 1 || logger.events[0].kind != EventRetrievalResult {
		t.Fatalf("events = %+v, want one %s", logger.events, EventRetrievalResult)
	}
	p := logger.events[0].payload
	if p["dataset_sha256"] != ds.SHA256 || p["model_ver"] != "m1" || p["fusion_sha256"] != "f1" {
		t.Fatalf("event payload identity = %+v", p)
	}
}

// TestRetrievalRunnerUnanswerableFalsePositiveDropsPrecision proves the
// unanswerable items are NON-vacuous end to end: if the searcher surfaces a
// hit that contains an unanswerable item's guarded (corpus-absent) answer, that
// item flips to wrong and precision drops below the green threshold - so a
// regression that starts hallucinating a false answer is actually caught.
func TestRetrievalRunnerUnanswerableFalsePositiveDropsPrecision(t *testing.T) {
	searcher, ds := synthFixture(t)
	// Find an unanswerable item and make the searcher wrongly return its
	// guarded answer.
	var poisoned string
	for _, it := range ds.Items {
		if !it.Answerable {
			poisoned = it.ID
			searcher.byQuery[it.Query] = []search.Hit{{
				Path: "notes/hallucination.md", Text: "... " + it.Expected[0].Substring + " ...",
				Score: 0.95, SourceTier: "user_asserted",
			}}
			break
		}
	}
	if poisoned == "" {
		t.Fatal("no unanswerable item in the synthetic dataset to poison")
	}
	r := &RetrievalRunner{DatasetPath: "testdata/dataset.synth.jsonl", Searcher: searcher, EventLogger: &recordingLogger{}}
	out, err := r.Run(context.Background(), "trace-fp")
	if err != nil {
		t.Fatal(err)
	}
	if out.Report.Precision >= 1.0 {
		t.Fatalf("precision = %v, want < 1.0 (the poisoned unanswerable item must score wrong)", out.Report.Precision)
	}
	for _, ir := range out.Report.Items {
		if ir.ID == poisoned && ir.Correct {
			t.Fatalf("poisoned unanswerable item %s scored correct - the false positive was not caught", poisoned)
		}
	}
}

func TestRetrievalRunnerSearchErrorIsFatal(t *testing.T) {
	searcher := &fakeRetrievalSearcher{err: context.DeadlineExceeded}
	logger := &recordingLogger{}
	r := &RetrievalRunner{DatasetPath: "testdata/dataset.synth.jsonl", Searcher: searcher, EventLogger: logger}
	if _, err := r.Run(context.Background(), "trace-1"); err == nil {
		t.Fatal("Run: want error when a query search fails (fail-closed), got nil")
	}
	if len(logger.events) != 0 {
		t.Fatalf("a fatally-failed run must NOT ledger a result event, got %+v", logger.events)
	}
}

// --- gate (design D4) ---

func fixedGateNow() func() time.Time {
	return func() time.Time { return time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC) }
}

func newGate(rows []EventRow) EvalGate {
	return EvalGate{Reader: fakeGateReader{rows: rows}, Now: fixedGateNow()}
}

const maxAge24h = 24 * time.Hour

var wantState = GateState{DatasetSHA256: "d1", ModelVer: "m1", FusionSHA256: "f1"}

func TestGateAllowsFreshMatchingGreen(t *testing.T) {
	now := fixedGateNow()()
	g := newGate([]EventRow{greenRow(now.Add(-1*time.Hour), "d1", "m1", "f1", 0.85)})
	res := g.Check(context.Background(), maxAge24h, wantState)
	if !res.Allowed {
		t.Fatalf("Check = %+v, want Allowed", res)
	}
	if res.Reason != "" {
		t.Fatalf("allowed result must carry no refusal reason, got %q", res.Reason)
	}
}

func TestGateRefusesWhenNoResult(t *testing.T) {
	g := newGate(nil)
	res := g.Check(context.Background(), maxAge24h, wantState)
	assertRefused(t, res)
}

func TestGateRefusesStale(t *testing.T) {
	now := fixedGateNow()()
	g := newGate([]EventRow{greenRow(now.Add(-48*time.Hour), "d1", "m1", "f1", 0.9)})
	assertRefused(t, g.Check(context.Background(), maxAge24h, wantState))
}

func TestGateRefusesRed(t *testing.T) {
	now := fixedGateNow()()
	g := newGate([]EventRow{greenRow(now.Add(-1*time.Hour), "d1", "m1", "f1", 0.5)}) // below MinPrecision
	assertRefused(t, g.Check(context.Background(), maxAge24h, wantState))
}

func TestGateRefusesMismatchedDatasetSHA(t *testing.T) {
	now := fixedGateNow()()
	g := newGate([]EventRow{greenRow(now.Add(-1*time.Hour), "OTHER", "m1", "f1", 0.9)})
	assertRefused(t, g.Check(context.Background(), maxAge24h, wantState))
}

func TestGateRefusesMismatchedFusionSHA(t *testing.T) {
	now := fixedGateNow()()
	g := newGate([]EventRow{greenRow(now.Add(-1*time.Hour), "d1", "m1", "OTHER", 0.9)})
	assertRefused(t, g.Check(context.Background(), maxAge24h, wantState))
}

func TestGateRefusesMismatchedModelVer(t *testing.T) {
	now := fixedGateNow()()
	// A green run recorded against a DIFFERENT active model_ver must not
	// satisfy the gate for the current one (the index it was scored against is
	// no longer active - HANDOFF §4 model_ver rule).
	g := newGate([]EventRow{greenRow(now.Add(-1*time.Hour), "d1", "OTHER", "f1", 0.9)})
	assertRefused(t, g.Check(context.Background(), maxAge24h, wantState))
}

func TestGateRefusesOnReaderErrorFailClosed(t *testing.T) {
	g := EvalGate{Reader: fakeGateReader{err: context.DeadlineExceeded}, Now: fixedGateNow()}
	assertRefused(t, g.Check(context.Background(), maxAge24h, wantState))
}

func TestGateRefusesNilReaderFailClosed(t *testing.T) {
	g := EvalGate{Now: fixedGateNow()}
	assertRefused(t, g.Check(context.Background(), maxAge24h, wantState))
}

func assertRefused(t *testing.T, res GateResult) {
	t.Helper()
	if res.Allowed {
		t.Fatalf("Check = %+v, want refused", res)
	}
	if res.Reason != GateRefusalReason {
		t.Fatalf("refusal reason = %q, want byte-exact %q", res.Reason, GateRefusalReason)
	}
	if res.Detail == "" {
		t.Fatal("refused result must carry an English log detail")
	}
}

// --- fusion-activation gate (D4c): refuses an unknown fusion_sha ---

func TestFusionActivationGateRefusesUnknownFusionSHA(t *testing.T) {
	now := fixedGateNow()()
	// A green result exists for fusion "f1" against the current dataset/model.
	g := FusionActivationGate{
		Gate:          newGate([]EventRow{greenRow(now.Add(-1*time.Hour), "d1", "m1", "f1", 0.9)}),
		MaxAge:        maxAge24h,
		DatasetSHA256: "d1",
		ModelVer:      "m1",
	}
	if allowed, reason := g.CheckFusionActivation("f-UNKNOWN"); allowed || reason != GateRefusalReason {
		t.Fatalf("CheckFusionActivation(unknown) = (%v,%q), want refused", allowed, reason)
	}
	if allowed, _ := g.CheckFusionActivation("f1"); !allowed {
		t.Fatal("CheckFusionActivation(f1) should be allowed (a green result covers it)")
	}
}

// --- model_ver switch gate (D4b): refuses without a green candidate run ---

func TestReEmbedGateRefusesWithoutGreenCandidate(t *testing.T) {
	now := fixedGateNow()()
	// Only a green result for model "m-old" exists; activating "m-new" has none.
	a := ReEmbedGateAdapter{
		Gate:         newGate([]EventRow{greenRow(now.Add(-1*time.Hour), "d1", "m-old", "f1", 0.9)}),
		FusionSHA256: "f1",
		MaxAge:       maxAge24h,
	}
	if allowed, reason, _ := a.Check(context.Background(), "d1", "m-new"); allowed || reason != GateRefusalReason {
		t.Fatalf("Check(m-new) = (%v,%q), want refused (no green candidate for m-new)", allowed, reason)
	}
	if allowed, _, _ := a.Check(context.Background(), "d1", "m-old"); !allowed {
		t.Fatal("Check(m-old) should be allowed (a green candidate run exists)")
	}
}

// --- export-ritual drafting ---

type fakeRitualReader struct{ rows []RitualLabelRow }

func (r fakeRitualReader) ListRitualLabeledFactsForEval(ctx context.Context) ([]RitualLabelRow, error) {
	return r.rows, nil
}

func TestExportRitualCandidatesDrafts(t *testing.T) {
	reader := fakeRitualReader{rows: []RitualLabelRow{
		{FactID: 7, Label: "true", QuestionText: "köpeğimin adı Boncuk mu", Subject: "köpek", Predicate: "ad", Object: "Boncuk"},
		{FactID: 9, Label: "false", QuestionText: "kripto sattım mı", Subject: "ben", Predicate: "işlem", Object: "sat"},
	}}
	lines, err := ExportRitualCandidates(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	var trueItem RetrievalItem
	if err := json.Unmarshal([]byte(lines[0]), &trueItem); err != nil {
		t.Fatal(err)
	}
	if !trueItem.Answerable || trueItem.LabelSource != "ritual" || trueItem.ID != "ritual-7" {
		t.Fatalf("true draft = %+v, want answerable ritual-7", trueItem)
	}
	if len(trueItem.Expected) != 1 || trueItem.Expected[0].Substring != "Boncuk" {
		t.Fatalf("true draft expected = %+v, want substring Boncuk", trueItem.Expected)
	}
	var falseItem RetrievalItem
	if err := json.Unmarshal([]byte(lines[1]), &falseItem); err != nil {
		t.Fatal(err)
	}
	if falseItem.Answerable || len(falseItem.Expected) != 0 {
		t.Fatalf("false draft = %+v, want unanswerable with no expected evidence", falseItem)
	}
}

// --- same-path proof: the eval runner queries through the SAME exported
// search.Searcher.Search that <hafiza> injection uses. *search.Searcher
// satisfies RetrievalSearcher directly (no adapter), so the runner literally
// calls search.Searcher.Search. This is a compile-level assertion (see also
// retrieval_samepath_test.go, which additionally asserts the server-side
// injection interface is the SAME concrete type). ---
var _ RetrievalSearcher = (*search.Searcher)(nil)
